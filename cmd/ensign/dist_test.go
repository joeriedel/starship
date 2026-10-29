package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseRepo makes a control-plane repo from this one's config, with release tags.
func releaseRepo(t *testing.T) string {
	t.Helper()
	needGit(t)
	repo := filepath.Join(t.TempDir(), "starship")
	root := repoRoot(t)
	for _, dir := range []string{"pipelines", "roles", "manual", "extensions", "briefs"} {
		copyDir(t, filepath.Join(root, dir), filepath.Join(repo, dir))
	}
	write(t, filepath.Join(repo, "bin", platformKey(), exeName()), "#!/bin/sh\necho released ensign\n")
	os.Chmod(filepath.Join(repo, "bin", platformKey(), exeName()), 0o755)
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "release 0.1.0")
	runGit(t, repo, "tag", "v0.1.0")
	write(t, filepath.Join(repo, "roles", "coder.md"), "---\nname: coder\nmodel: sonnet\n---\nversion two\n")
	runGit(t, repo, "commit", "-q", "-am", "release 0.2.0")
	runGit(t, repo, "tag", "v0.2.0")
	write(t, filepath.Join(repo, "roles", "coder.md"), "---\nname: coder\nmodel: sonnet\n---\nunreleased\n")
	runGit(t, repo, "commit", "-q", "-am", "work in progress")
	return repo
}

func TestPinnedBridge(t *testing.T) {
	repo := releaseRepo(t)
	t.Setenv("STARSHIP_BIN_DIR", filepath.Join(t.TempDir(), "bin"))
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
	if pinnedBinary(bridge) == "" {
		t.Fatalf("the release's binary was not found in the pinned clone")
	}

	out.Reset()
	if err := cmdSetup([]string{"--projects", "/srv/projects"}, &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	installed, _ := os.ReadFile(filepath.Join(os.Getenv("STARSHIP_BIN_DIR"), exeName()))
	if !strings.Contains(string(installed), "released ensign") {
		t.Errorf("setup should install the pinned release's ensign")
	}
	if m := loadMachine(bridge); m.Projects != "/srv/projects" {
		t.Errorf("machine projects = %q", m.Projects)
	}
	if ws, _ := expandWorkspace(bridge, "$PROJECTS/app"); ws != filepath.FromSlash("/srv/projects/app") {
		t.Errorf("$PROJECTS expanded to %q", ws)
	}
	if reg := loadRegistry(); !contains(reg.Bridges, bridge) {
		t.Errorf("bridge not registered: %v", reg.Bridges)
	}

	// A session that started on v0.1.0 can't start a run after the bridge moves on.
	t.Setenv("STARSHIP_SESSION", "s1")
	writeFile(sessionFile(bridge, "s1")+".pin", pinnedCommit(bridge)+"\n")
	runGit(t, bridge, "config", "user.email", "t@example.com")
	runGit(t, bridge, "config", "user.name", "t")
	runGit(t, bridge, "add", "-A")
	runGit(t, bridge, "commit", "-q", "-m", "bridge")

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
	if data, _ := os.ReadFile(filepath.Join(bridge, "bridge.yaml")); !strings.Contains(string(data), "tag: v0.2.0") {
		t.Errorf("bridge.yaml not updated:\n%s", data)
	}
	if subj := runGit(t, bridge, "log", "--format=%s", "-1"); subj != "Update starship to v0.2.0" {
		t.Errorf("update commit: %q", subj)
	}

	// The project and task exist so that the only reason to refuse is the update.
	if err := cmdProject([]string{"new", "app"}, &out); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
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
