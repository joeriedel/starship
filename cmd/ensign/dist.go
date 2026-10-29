package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Distribution: each bridge runs a pinned release of the control plane, cloned into
// .starship/control-plane, and a launcher on PATH hands off to that release's ensign.

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

// pinnedBinary is the ensign a bridge's release ships, if it exists on disk.
func pinnedBinary(bridge string) string {
	p := filepath.Join(cpCloneDir(bridge), "bin", platformKey(), exeName())
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// installDir is where setup puts the launcher. STARSHIP_BIN_DIR overrides it (tests).
func installDir() (string, error) {
	if d := os.Getenv("STARSHIP_BIN_DIR"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, "starship", "bin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func copyFile(from, to string, mode os.FileMode) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	tmp := to + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

func registryFile() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "starship", "bridges.yaml"), nil
}

type Registry struct {
	Bridges []string `yaml:"bridges"`
}

func loadRegistry() Registry {
	var r Registry
	if f, err := registryFile(); err == nil {
		readYAML(f, &r)
	}
	return r
}

func register(bridge string) error {
	f, err := registryFile()
	if err != nil {
		return err
	}
	r := loadRegistry()
	if !contains(r.Bridges, bridge) {
		r.Bridges = append(r.Bridges, bridge)
		sort.Strings(r.Bridges)
	}
	return writeYAML(f, r)
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
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return err
	}
	if bf.ControlPlane.Path == "" {
		if err := fetchControlPlane(bridge, bf); err != nil {
			return err
		}
		fmt.Fprintf(out, "control plane %s at %s\n", bf.ControlPlane.Remote, pinRef(bf))
	}

	src := pinnedBinary(bridge)
	if src == "" {
		src, _ = os.Executable()
	}
	dir, err := installDir()
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, exeName())
	if same, _ := sameFile(src, dest); !same {
		if err := copyFile(src, dest, 0o755); err != nil {
			return fmt.Errorf("installing ensign: %v", err)
		}
	}
	fmt.Fprintf(out, "ensign installed at %s\n", dest)
	if note, err := ensureOnPath(dir); err != nil {
		return err
	} else if note != "" {
		fmt.Fprintln(out, note)
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
	if err := register(bridge); err != nil {
		return err
	}
	if err := link(bridge, false, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "bridge ready — restart claude")
	return nil
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
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

// handoff runs the bridge's pinned ensign instead of this one, when there is one and
// this isn't it. It returns only if no handoff is needed.
func handoff() {
	if os.Getenv("STARSHIP_HANDOFF") != "" || len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "setup", "init", "version", "--version", "selftest", "selftest-hook":
		return
	}
	bridge, err := cwdBridge()
	if err != nil {
		return
	}
	target := pinnedBinary(bridge)
	if target == "" {
		return
	}
	self, _ := os.Executable()
	if same, _ := sameFile(self, target); same {
		return
	}
	os.Setenv("STARSHIP_HANDOFF", "1")
	execReplace(target, os.Args)
}
