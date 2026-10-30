// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
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
	for _, tag := range []string{"v0.1.0", "v0.2.0"} {
		bin := releasedBinary(tag)
		write(t, filepath.Join(releases, tag, ensignAsset()), bin)
		write(t, filepath.Join(releases, tag, "SHA256SUMS"), sha([]byte(bin))+"  "+ensignAsset()+"\n")
	}
	t.Setenv("STARSHIP_RELEASES", releases)
	return repo
}

func releasedBinary(tag string) string { return "#!/bin/sh\necho released ensign " + tag + "\n" }

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
	if ownEnsign(t, bridge) != releasedBinary("v0.1.0") {
		t.Fatalf("init should download v0.1.0's ensign into the bridge")
	}

	out.Reset()
	projects := t.TempDir()
	if err := cmdSetup([]string{"--projects", projects}, &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	if m := loadMachine(bridge); m.Projects != filepath.ToSlash(projects) {
		t.Errorf("machine projects = %q", m.Projects)
	}
	if ws, _ := expandWorkspace(bridge, "$PROJECTS/app"); ws != filepath.Join(projects, "app") {
		t.Errorf("$PROJECTS expanded to %q", ws)
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
	if ownEnsign(t, bridge) != releasedBinary("v0.2.0") {
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
	if err := cmdProject([]string{"new", "app", "--workspace", ws}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"new", "app/one", "--workspace", ws}, &out); err != nil {
		t.Fatal(err)
	}
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
	write(t, filepath.Join(bridge, filepath.FromSlash(ensignFile())), releasedBinary("v0.1.0"))

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
	if ownEnsign(t, bridge) != releasedBinary("v0.1.0") {
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
