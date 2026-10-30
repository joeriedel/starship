// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Distribution: each bridge runs a pinned release of the control plane, cloned into
// .starship/control-plane, and keeps that release's ensign as .starship/bin/ensign.
// Nothing is installed for the machine.

func cpCloneDir(bridge string) string { return filepath.Join(bridge, ".starship", "control-plane") }

// pinRef is what a bridge pins: a release tag, or (for development) any ref.
func pinRef(bf *BridgeFile) string {
	if bf.ControlPlane.Tag != "" {
		return bf.ControlPlane.Tag
	}
	return bf.ControlPlane.Ref
}

// fetchControlPlane makes the bridge's clone match its pin, replacing any other checkout.
func fetchControlPlane(bridge string, bf *BridgeFile) error {
	remote, ref := bf.ControlPlane.Remote, pinRef(bf)
	if remote == "" || ref == "" {
		return fmt.Errorf("bridge.yaml: control-plane needs a remote and a tag (or ref)")
	}
	dir := cpCloneDir(bridge)
	if cur, err := gitOut(dir, "rev-parse", "HEAD"); err == nil {
		if want, err := gitOut(dir, "rev-parse", ref+"^{commit}"); err == nil && want == cur {
			return nil
		}
	}
	tmp := dir + ".new"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	steps := [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}}
	if bf.ControlPlane.Tag != "" {
		steps = append(steps, []string{"fetch", "-q", "--depth", "1", "origin", "refs/tags/" + ref + ":refs/tags/" + ref},
			[]string{"checkout", "-q", "--detach", ref})
	} else {
		steps = append(steps, []string{"fetch", "-q", "origin", ref}, []string{"checkout", "-q", "--detach", "FETCH_HEAD"})
	}
	for _, s := range steps {
		if err := git(tmp, s...); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("fetching the control plane %s at %s: %v", remote, ref, err)
		}
	}
	os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

// pinnedCommit is the control plane commit a bridge is running, or "" in development.
func pinnedCommit(bridge string) string {
	bf, err := loadBridgeFile(bridge)
	if err != nil || bf.ControlPlane.Path != "" {
		return ""
	}
	sha, _ := gitOut(cpCloneDir(bridge), "rev-parse", "HEAD")
	return sha
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "ensign.exe"
	}
	return "ensign"
}

// Release binaries are attached to each GitHub release, never committed. A bridge
// downloads the one for its pin and platform, checks it against the release's
// SHA256SUMS, and keeps it as .starship/bin/ensign, with the tag it came from beside it.

// ensignAsset is the release attachment for this platform.
func ensignAsset() string { return "ensign-" + platformKey() + filepath.Ext(exeName()) }

var githubRemoteRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// releaseAsset fetches one attachment of a release. STARSHIP_RELEASES names a local
// folder of <tag>/<name> files instead (tests, or a mirror).
func releaseAsset(remote, tag, name string) ([]byte, error) {
	if dir := os.Getenv("STARSHIP_RELEASES"); dir != "" {
		return os.ReadFile(filepath.Join(dir, tag, name))
	}
	m := githubRemoteRe.FindStringSubmatch(remote)
	if m == nil {
		return nil, fmt.Errorf("can't find releases for %s: only GitHub remotes have them", remote)
	}
	url := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", m[1], m[2], tag, name)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func ensignTagFile(bridge string) string {
	return filepath.Join(bridge, filepath.FromSlash(ensignFile())+".tag")
}

// fetchEnsign makes the bridge's .starship/bin/ensign its pinned release's, checked
// against the release's SHA256SUMS. A binary already there for that tag is kept.
func fetchEnsign(bridge string, bf *BridgeFile) error {
	remote, tag := bf.ControlPlane.Remote, bf.ControlPlane.Tag
	if remote == "" || tag == "" {
		return nil
	}
	dest := filepath.Join(bridge, filepath.FromSlash(ensignFile()))
	if have, _ := os.ReadFile(ensignTagFile(bridge)); strings.TrimSpace(string(have)) == tag {
		if _, err := os.Stat(dest); err == nil {
			return nil
		}
	}
	name := ensignAsset()
	sums, err := releaseAsset(remote, tag, "SHA256SUMS")
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("release %s has no ensign for %s", tag, platformKey())
	}
	// The ensign that set the bridge up may already be the right one (downloaded by hand).
	if cur, err := os.ReadFile(dest); err == nil && sha(cur) == want {
		return os.WriteFile(ensignTagFile(bridge), []byte(tag+"\n"), 0o644)
	}
	bin, err := releaseAsset(remote, tag, name)
	if err != nil {
		return err
	}
	if sha(bin) != want {
		return fmt.Errorf("the downloaded %s does not match release %s's SHA256SUMS; not using it", name, tag)
	}
	if err := replaceFile(dest, bin, true); err != nil {
		return err
	}
	return os.WriteFile(ensignTagFile(bridge), []byte(tag+"\n"), 0o644)
}

// cmdSetup implements `ensign setup [--projects <path>]`, run in a bridge.
func cmdSetup(args []string, out io.Writer) error {
	var projects string
	for i := 0; i < len(args); i++ {
		if args[i] == "--projects" && i+1 < len(args) {
			projects, i = args[i+1], i+1
			continue
		}
		return fmt.Errorf("usage: ensign setup [--projects <path>]")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	return setupBridge(bridge, projects, out)
}

// setupBridge makes a bridge work on this machine: the pinned control plane, ensign on
// PATH, where this machine keeps its code, and the generated files.
func setupBridge(bridge, projects string, out io.Writer) error {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return err
	}
	if bf.ControlPlane.Path == "" {
		if err := fetchControlPlane(bridge, bf); err != nil {
			return err
		}
		if err := fetchEnsign(bridge, bf); err != nil {
			return err
		}
		fmt.Fprintf(out, "control plane %s at %s\n", bf.ControlPlane.Remote, pinRef(bf))
	}

	if projects != "" {
		abs, _ := filepath.Abs(projects)
		m := loadMachine(bridge)
		m.Projects = filepath.ToSlash(abs)
		if err := writeYAML(machineFile(bridge), m); err != nil {
			return err
		}
		fmt.Fprintf(out, "$PROJECTS is %s on this machine\n", m.Projects)
	}
	if err := link(bridge, false, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "bridge ready — restart claude")
	return nil
}

var semverRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

func newerTag(a, b string) bool {
	ma, mb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// latestTag asks the remote for its release tags and returns the newest.
func latestTag(remote string) (string, error) {
	out, err := exec.Command("git", "ls-remote", "--tags", "--refs", remote).Output()
	if err != nil {
		return "", fmt.Errorf("listing tags on %s: %v", remote, err)
	}
	best := ""
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		tag := strings.TrimPrefix(f[1], "refs/tags/")
		if semverRe.MatchString(tag) && (best == "" || newerTag(tag, best)) {
			best = tag
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s has no release tags", remote)
	}
	return best, nil
}

var tagLineRe = regexp.MustCompile(`(?m)^(\s+tag:\s*)\S+\s*$`)

// cmdUpdate implements `ensign update [<tag>]`: move the bridge's pin to a newer release.
func cmdUpdate(args []string, out io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: ensign update [<tag>]")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return err
	}
	if bf.ControlPlane.Remote == "" || bf.ControlPlane.Tag == "" {
		return fmt.Errorf("this bridge is not pinned to a release (bridge.yaml: control-plane remote and tag)")
	}
	if live := liveRuns(bridge); len(live) > 0 {
		return fmt.Errorf("run %s is active; update waits until no run is active", live[0].id)
	}
	tag := ""
	if len(args) == 1 {
		tag = args[0]
	} else if tag, err = latestTag(bf.ControlPlane.Remote); err != nil {
		return err
	}
	if tag == bf.ControlPlane.Tag {
		fmt.Fprintf(out, "already at %s\n", tag)
		return nil
	}
	path := filepath.Join(bridge, "bridge.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if n := len(tagLineRe.FindAllIndex(data, -1)); n != 1 {
		return fmt.Errorf("bridge.yaml: expected exactly one tag line, found %d", n)
	}
	old := bf.ControlPlane.Tag
	bf.ControlPlane.Tag = tag
	if err := fetchControlPlane(bridge, bf); err != nil {
		return err
	}
	if err := fetchEnsign(bridge, bf); err != nil {
		return err
	}
	data = tagLineRe.ReplaceAll(data, []byte("${1}"+tag))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	if err := link(bridge, false, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "updated %s → %s; restart claude to use it\n", old, tag)
	if isGit(bridge) {
		if err := git(bridge, "commit", "-q", "-m", "Update starship to "+tag, "--", "bridge.yaml"); err != nil {
			return err
		}
		return pushBridge(bridge, out)
	}
	return nil
}
