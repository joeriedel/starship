// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease makes a control-plane repo from this one's config with release tags, and
// a folder of release attachments for them (STARSHIP_RELEASES), as GitHub would serve.
func fakeRelease(t *testing.T) string {
	t.Helper()
	needGit(t)
	repo := filepath.Join(t.TempDir(), "starship")
	root := repoRoot(t)
	for _, dir := range []string{"pipelines", "roles", "manual", "extensions", "briefs"} {
		copyDir(t, filepath.Join(root, dir), filepath.Join(repo, dir))
	}
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "release 0.1.0")
	runGit(t, repo, "tag", "v0.1.0")
	write(t, filepath.Join(repo, "roles", "coder.md"), "---\nname: coder\nmodel: sonnet\n---\nversion two\n")
	runGit(t, repo, "commit", "-q", "-am", "release 0.2.0")
	runGit(t, repo, "tag", "v0.2.0")
	write(t, filepath.Join(repo, "roles", "coder.md"), "---\nname: coder\nmodel: sonnet\n---\nunreleased\n")
	runGit(t, repo, "commit", "-q", "-am", "work in progress")

	releases := t.TempDir()
	t.Setenv("STARSHIP_RELEASES", releases)
	for _, tag := range []string{"v0.1.0", "v0.2.0"} {
		publishEnsign(t, tag)
	}
	t.Setenv("STARSHIP_TEST_ENSIGN", "1")
	return repo
}

// publishEnsign adds a release's attachments to STARSHIP_RELEASES.
func publishEnsign(t *testing.T, tag string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("STARSHIP_RELEASES"), tag)
	bin := releasedBinary(t)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, ensignAsset()), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "SHA256SUMS"), sha(bin)+"  "+ensignAsset()+"\n")
}

// releasedBinary is a real executable on every OS: this test binary, which TestMain turns into ensign when STARSHIP_TEST_ENSIGN is set. A bridge's copy records which release it is in its .tag file.
func releasedBinary(t *testing.T) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ensignTag(bridge string) string {
	tag, _ := os.ReadFile(ensignTagFile(bridge))
	return strings.TrimSpace(string(tag))
}

// pinnedBridge is a git bridge pinned to v0.1.0 of the fake release, with its changes committed.
func pinnedBridge(t *testing.T, repo string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("HOME", t.TempDir())
	bridge := filepath.Join(t.TempDir(), "work")
	var out bytes.Buffer
	if err := cmdInit([]string{bridge, "--remote", repo, "--tag", "v0.1.0"}, &out); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	t.Setenv("CLAUDE_PROJECT_DIR", bridge)
	runGit(t, bridge, "config", "user.email", "t@example.com")
	runGit(t, bridge, "config", "user.name", "t")
	runGit(t, bridge, "add", "-A")
	if staged, _ := gitOut(bridge, "diff", "--cached", "--name-only"); staged != "" {
		runGit(t, bridge, "commit", "-q", "-m", "bridge")
	}
	return bridge
}

// An update links with the ensign it fetched, so what that release generates exists at once.
func TestUpdateLinksWithTheNewEnsign(t *testing.T) {
	repo := fakeRelease(t)
	bridge := pinnedBridge(t, repo)
	write(t, filepath.Join(repo, "roles", "supernova.md"), "---\nname: supernova\nmodel: sonnet\n---\nnew in v0.3.0\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "release 0.3.0")
	runGit(t, repo, "tag", "v0.3.0")
	publishEnsign(t, "v0.3.0")

	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("STARSHIP_TEST_ENSIGN_LOG", log)
	var out bytes.Buffer
	if err := cmdUpdate([]string{"v0.3.0"}, &out); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}
	if calls, _ := os.ReadFile(log); string(calls) != "link --migrate\n" {
		t.Errorf("the fetched ensign should have been run as `link --migrate`; calls: %q", calls)
	}
	if _, err := os.Stat(filepath.Join(bridge, ".claude/agents/supernova.md")); err != nil {
		t.Errorf("the update did not generate the new release's role: %v", err)
	}
	if ensignTag(bridge) != "v0.3.0" {
		t.Errorf("ensign is %q, want v0.3.0", ensignTag(bridge))
	}
}

func TestUpdateWhenTheNewEnsignFailsToLink(t *testing.T) {
	repo := fakeRelease(t)
	bridge := pinnedBridge(t, repo)
	t.Setenv("STARSHIP_TEST_ENSIGN_FAIL", "1")
	var out bytes.Buffer
	err := cmdUpdate([]string{"v0.2.0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "linking failed") || !strings.Contains(err.Error(), "v0.2.0") {
		t.Fatalf("a failed link should be reported: %v", err)
	}
	if strings.Contains(out.String(), "updated") {
		t.Errorf("a failed update should not say it updated:\n%s", out.String())
	}
	if subj := runGit(t, bridge, "log", "--format=%s", "-1"); subj == "Update starship to v0.2.0" {
		t.Errorf("a failed update must not commit bridge.yaml")
	}
	if status := runGit(t, bridge, "status", "--porcelain", "--", "bridge.yaml"); status == "" {
		t.Errorf("bridge.yaml should be left changed for the user to commit")
	}
}

func ownEnsign(t *testing.T, bridge string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(bridge, filepath.FromSlash(ensignFile())))
	return string(data)
}

func TestPinnedBridge(t *testing.T) {
	repo := fakeRelease(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("HOME", t.TempDir())

	bridge := filepath.Join(t.TempDir(), "work")
	var out bytes.Buffer
	if err := cmdInit([]string{bridge, "--remote", repo, "--tag", "v0.1.0"}, &out); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	t.Setenv("CLAUDE_PROJECT_DIR", bridge)
	coder := filepath.Join(bridge, ".claude/agents/coder.md")
	if data, _ := os.ReadFile(coder); strings.Contains(string(data), "version two") {
		t.Fatalf("v0.1.0 should not have version two's coder")
	}
	if ensignTag(bridge) != "v0.1.0" {
		t.Fatalf("init should download v0.1.0's ensign into the bridge")
	}

	out.Reset()
	// An older bridge's CLAUDE.md still passes --projects; it is ignored.
	if err := cmdSetup([]string{"--projects", t.TempDir()}, &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}

	// A session that started on v0.1.0 can't start a run after the bridge moves on.
	t.Setenv("STARSHIP_SESSION", "s1")
	writeFile(sessionFile(bridge, "s1")+".pin", pinnedCommit(bridge)+"\n")
	runGit(t, bridge, "config", "user.email", "t@example.com")
	runGit(t, bridge, "config", "user.name", "t")
	runGit(t, bridge, "add", "-A")
	// init commits the bridge itself when git has an identity (macOS git makes one up).
	if staged, _ := gitOut(bridge, "diff", "--cached", "--name-only"); staged != "" {
		runGit(t, bridge, "commit", "-q", "-m", "bridge")
	}

	if tag, err := latestTag(repo); err != nil || tag != "v0.2.0" {
		t.Fatalf("latest tag %q %v", tag, err)
	}
	out.Reset()
	if err := cmdUpdate(nil, &out); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}
	if data, _ := os.ReadFile(coder); !strings.Contains(string(data), "version two") {
		t.Errorf("update did not relink the new release's roles")
	}
	if ensignTag(bridge) != "v0.2.0" {
		t.Errorf("update should download v0.2.0's ensign")
	}
	if data, _ := os.ReadFile(filepath.Join(bridge, "bridge.yaml")); !strings.Contains(string(data), "tag: v0.2.0") {
		t.Errorf("bridge.yaml not updated:\n%s", data)
	}
	if subj := runGit(t, bridge, "log", "--format=%s", "-1"); subj != "Update starship to v0.2.0" {
		t.Errorf("update commit: %q", subj)
	}

	// The project and task exist so that the only reason to refuse is the update.
	ws := t.TempDir()
	if err := cmdProject([]string{"new", "app", "--workspace", ws, "--platforms", "linux"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"new", "app/one", "--workspace", ws}, &out); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bridge, "projects/app/tasks/one/task.md"), "---\npipeline: coding\n---\n\n## Goal\n\nDo one.\n")
	p := filepath.Join(bridge, "projects/app/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "bindings:\n", "bindings:\n  build: { command: \"true\" }\n  test: { command: \"true\" }\n", 1))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(bridge)
	if err := cmdRun([]string{"app/one"}, &out); err == nil || !strings.Contains(err.Error(), "restart claude") {
		t.Fatalf("a run in a session from before the update should be refused: %v", err)
	}
}

// The flow the README gives Claude: download the release's ensign into the new bridge's
// folder, then run it with init and nothing else.
func TestInitFromARelease(t *testing.T) {
	repo := fakeRelease(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	defer func(r, v string) { releaseRepo, version = r, v }(releaseRepo, version)
	releaseRepo, version = "file://"+repo, "v0.1.0"

	bridge := filepath.Join(t.TempDir(), "home")
	write(t, filepath.Join(bridge, "notes.txt"), "the user's own file\n")
	if err := replaceFile(filepath.Join(bridge, filepath.FromSlash(ensignFile())), releasedBinary(t), true); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cmdInit([]string{bridge}, &out); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		t.Fatal(err)
	}
	if bf.ControlPlane.Remote != "file://"+repo || bf.ControlPlane.Tag != "v0.1.0" {
		t.Errorf("pinned %q at %q, want the release this ensign is", bf.ControlPlane.Remote, bf.ControlPlane.Tag)
	}
	if !bytes.Equal([]byte(ownEnsign(t, bridge)), releasedBinary(t)) {
		t.Errorf("the downloaded ensign should be kept")
	}
	if tag, _ := os.ReadFile(ensignTagFile(bridge)); string(tag) != "v0.1.0\n" {
		t.Errorf("the bridge should record which release its ensign is: %q", tag)
	}
	if settings, _ := os.ReadFile(filepath.Join(bridge, ".claude/settings.json")); !strings.Contains(string(settings), `$CLAUDE_PROJECT_DIR/.starship/bin/ensign`) {
		t.Errorf("hooks should run the bridge's own ensign:\n%s", settings)
	}
	if _, err := os.Stat(filepath.Join(bridge, ".claude/agents/coder.md")); err != nil {
		t.Errorf("the bridge was not linked")
	}
	if status, _ := gitOut(bridge, "status", "--porcelain"); status != "?? notes.txt" {
		t.Errorf("init should commit its own files and leave the user's alone; status %q", status)
	}
}

func TestDownloadedEnsignMustMatchItsChecksum(t *testing.T) {
	repo := fakeRelease(t)
	releases := os.Getenv("STARSHIP_RELEASES")
	write(t, filepath.Join(releases, "v0.2.0", ensignAsset()), "#!/bin/sh\necho tampered\n")
	bridge := t.TempDir()
	write(t, filepath.Join(bridge, filepath.FromSlash(ensignFile())), "old\n")
	var bf BridgeFile
	bf.ControlPlane.Remote, bf.ControlPlane.Tag = repo, "v0.2.0"
	err := fetchEnsign(bridge, &bf)
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("a binary that doesn't match the checksums should be refused: %v", err)
	}
	if ownEnsign(t, bridge) != "old\n" {
		t.Errorf("a refused download must not replace the bridge's ensign")
	}
}

func TestGithubRemotes(t *testing.T) {
	for _, r := range []string{"https://github.com/joeriedel/starship.git", "https://github.com/joeriedel/starship",
		"git@github.com:joeriedel/starship.git", "ssh://git@github.com/joeriedel/starship.git"} {
		if m := githubRemoteRe.FindStringSubmatch(r); m == nil || m[1] != "joeriedel" || m[2] != "starship" {
			t.Errorf("%s: %v", r, m)
		}
	}
	if githubRemoteRe.MatchString("https://example.com/a/b.git") {
		t.Errorf("a non-GitHub remote should not match")
	}
}

func TestInitNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "CLAUDE.md"), "mine\n")
	var out bytes.Buffer
	err := cmdInit([]string{dir, "--control-plane", repoRoot(t)}, &out)
	if err == nil || !strings.Contains(err.Error(), "CLAUDE.md") {
		t.Fatalf("init over an existing CLAUDE.md: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(data) != "mine\n" {
		t.Errorf("CLAUDE.md was changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "bridge.yaml")); err == nil {
		t.Errorf("init should write nothing when it refuses")
	}
}

func TestSetupWarnsOnOldClaude(t *testing.T) {
	repo := fakeRelease(t)
	for _, c := range []struct {
		script string
		warns  int
	}{
		{`echo "2.1.316 (Claude Code)"`, 1},
		{`echo "2.1.317 (Claude Code)"`, 0},
		{`exit 1`, 0},
	} {
		fakeClaude(t, c.script+"\n")
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
		t.Setenv("HOME", t.TempDir())
		bridge := filepath.Join(t.TempDir(), "supernova")
		var out bytes.Buffer
		if err := cmdInit([]string{bridge, "--remote", repo, "--tag", "v0.1.0"}, &out); err != nil {
			t.Fatalf("init: %v\n%s", err, out.String())
		}
		if n := strings.Count(out.String(), "update Claude Code"); n != c.warns {
			t.Errorf("init %q: %d warnings in %q", c.script, n, out.String())
		}
		t.Setenv("CLAUDE_PROJECT_DIR", bridge)
		out.Reset()
		if err := cmdSetup(nil, &out); err != nil {
			t.Fatalf("setup: %v\n%s", err, out.String())
		}
		if n := strings.Count(out.String(), "update Claude Code"); n != c.warns {
			t.Errorf("setup %q: %d warnings in %q", c.script, n, out.String())
		}
	}
}

func TestSetupWithoutClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec.LookPath on Windows does not find an extensionless git symlink")
	}
	repo := fakeRelease(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	if err := os.Symlink(mustLookPath(t, "git"), filepath.Join(bin, "git")); err != nil {
		t.Skip(err)
	}
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	if err := cmdInit([]string{filepath.Join(t.TempDir(), "supernova"), "--remote", repo, "--tag", "v0.1.0"}, &out); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "Claude Code") {
		t.Errorf("output mentions Claude Code: %q", out.String())
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skip(name + " not on PATH")
	}
	return p
}
