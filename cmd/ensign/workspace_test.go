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

func TestWorkspaceMissingOnThisMachine(t *testing.T) {
	w := newWorld(t)
	os.Remove(machineFile(w.bridge))
	var out bytes.Buffer
	if err := cmdRun([]string{"supernova/auth-refactor"}, &out); err == nil || !strings.Contains(err.Error(), "no folder recorded for project supernova") {
		t.Fatalf("a run needs to know where the code is on this machine: %v", err)
	}
	out.Reset()
	if err := openTask(w.bridge, "supernova/auth-refactor", filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "workspace  STOP: this machine has no folder recorded") {
		t.Errorf("opening the task should say the folder is unknown here:\n%s", out.String())
	}
	ws := t.TempDir()
	if err := cmdWorkspace([]string{"supernova", filepath.Join(ws, "nope")}, &out); err == nil {
		t.Error("a folder that isn't there can't be recorded")
	}
	if err := cmdWorkspace([]string{"supernova", ws}, &out); err != nil {
		t.Fatal(err)
	}
	if got, err := workspaceFor(w.bridge, "supernova", "auth-refactor"); err != nil || got != ws {
		t.Errorf("recorded, the task finds it: %q %v", got, err)
	}
}

func TestMigrateWorkspaces(t *testing.T) {
	b := t.TempDir()
	root := t.TempDir()
	other := t.TempDir()
	write(t, filepath.Join(b, ".gitignore"), ".starship/\n")
	write(t, filepath.Join(b, ".starship/machine.yaml"), "projects: "+filepath.ToSlash(root)+"\n")
	write(t, filepath.Join(b, "projects/app/project.yaml"), "# Project: app\n\n# Where this project's code is. New tasks use it unless they name a different location.\nworkspace: $PROJECTS/app\n\nbindings:\n  test: { command: \"true\" }\n")
	write(t, filepath.Join(b, "projects/app/tasks/same/task.md"), "---\npipeline: coding\nworkspace: $PROJECTS/app\n---\n\n# same\n")
	write(t, filepath.Join(b, "projects/app/tasks/own/task.md"), "---\npipeline: coding\nworkspace: "+filepath.ToSlash(other)+"\n---\n\n# own\n")
	write(t, filepath.Join(b, "projects/app/style/sources.yaml"), "sources:\n  - id: s1\n    trust: exemplar\n    ref: $PROJECTS/app/internal/session\n")

	note := migrateWorkspaces(b)
	if !strings.Contains(note, "projects/app/project.yaml") {
		t.Fatalf("the moved paths should be reported: %q", note)
	}
	app := filepath.ToSlash(filepath.Join(root, "app"))
	m := loadMachine(b)
	if m.Workspaces["app"] != app || m.Workspaces["app/own"] != filepath.ToSlash(other) || m.Workspaces["app/same"] != "" {
		t.Errorf("machine.yaml workspaces: %v", m.Workspaces)
	}
	for _, f := range []string{"projects/app/project.yaml", "projects/app/tasks/same/task.md", "projects/app/tasks/own/task.md"} {
		data, _ := os.ReadFile(filepath.Join(b, f))
		if strings.Contains(string(data), "workspace:") || strings.Contains(string(data), "Where this project's code is") {
			t.Errorf("%s still has its workspace line:\n%s", f, data)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(b, "projects/app/project.yaml")); !strings.Contains(string(data), "test: { command:") {
		t.Errorf("the rest of project.yaml should be untouched:\n%s", data)
	}
	if src := loadSources(b, "app"); src.Sources[0].Ref != "$WORKSPACE/internal/session" {
		t.Errorf("a source in the project's code should be relative to it: %q", src.Sources[0].Ref)
	}
	if got := resolveRef(filepath.FromSlash(app), "$WORKSPACE/internal/session"); got != app+"/internal/session" {
		t.Errorf("resolved %q", got)
	}
	if _, err := os.Stat(filepath.Join(b, ".starship/machine.yaml")); err == nil {
		t.Error("the old machine file should be gone")
	}
	if data, _ := os.ReadFile(filepath.Join(b, ".gitignore")); !strings.Contains(string(data), "\nmachine.yaml\n") {
		t.Errorf("machine.yaml must be gitignored:\n%s", data)
	}
	if migrateWorkspaces(b) != "" {
		t.Error("a second pass has nothing to do")
	}
}

func TestMigrateWorkspacesWithoutTheOldRoot(t *testing.T) {
	b := t.TempDir()
	write(t, filepath.Join(b, ".gitignore"), ".starship/\n")
	pf := filepath.Join(b, "projects/app/project.yaml")
	write(t, pf, "workspace: $PROJECTS/app\n")
	if note := migrateWorkspaces(b); strings.Contains(note, "moved") {
		t.Errorf("a $PROJECTS path this machine can't resolve stays put: %q", note)
	}
	if data, _ := os.ReadFile(pf); !strings.Contains(string(data), "workspace:") {
		t.Error("nothing should be lost")
	}
	// Once the user says where it is, the old line goes.
	ws := t.TempDir()
	if _, err := setWorkspace(b, "app", ws); err != nil {
		t.Fatal(err)
	}
	migrateWorkspaces(b)
	if data, _ := os.ReadFile(pf); strings.Contains(string(data), "workspace:") {
		t.Errorf("the line should go once machine.yaml knows the folder:\n%s", data)
	}
	if got, _ := workspaceFor(b, "app", ""); got != ws {
		t.Errorf("the user's answer stands: %q", got)
	}
}

func TestMigrateWorkspacesCommitsItsOwnEdits(t *testing.T) {
	b := t.TempDir()
	runGit(t, b, "init", "-q", "-b", "main")
	runGit(t, b, "config", "user.email", "t@example.com")
	runGit(t, b, "config", "user.name", "t")
	ws := t.TempDir()
	write(t, filepath.Join(b, ".gitignore"), ".starship/\n")
	write(t, filepath.Join(b, "projects/app/project.yaml"), "workspace: "+filepath.ToSlash(ws)+"\n")
	write(t, filepath.Join(b, "projects/app/tasks/one/task.md"), "---\nworkspace: "+filepath.ToSlash(ws)+"\n---\n\n# one\n")
	runGit(t, b, "add", "-A")
	runGit(t, b, "commit", "-q", "-m", "old")
	// The user's own unfinished edit to a task stays theirs to commit.
	write(t, filepath.Join(b, "projects/app/tasks/one/task.md"), "---\nworkspace: "+filepath.ToSlash(ws)+"\n---\n\n# one\n\nhalf written\n")

	note := migrateWorkspaces(b)
	if !strings.Contains(note, "Commit these with the journal: projects/app/tasks/one/task.md.") {
		t.Errorf("only the file with the user's edits is left: %q", note)
	}
	if got := runGit(t, b, "show", "--stat", "--format=%s", "HEAD"); !strings.Contains(got, "machine.yaml") || !strings.Contains(got, ".gitignore") || !strings.Contains(got, "project.yaml") || strings.Contains(got, "task.md") {
		t.Errorf("the migration's own commit:\n%s", got)
	}
	if st := runGit(t, b, "status", "--porcelain"); st != "M projects/app/tasks/one/task.md" {
		t.Errorf("machine.yaml should be ignored and only the user's file left: %q", st)
	}
}
