package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const noCodeRefusal = "has no pipeline, so it can't run: it is a task without code (pipeline: none)"

// noCodeWorld is a world with no folder recorded on this machine, and a task per frontmatter pipeline line.
func noCodeWorld(t *testing.T, projectLine string) *world {
	t.Helper()
	w := newWorld(t)
	os.Remove(machineFile(w.bridge))
	if projectLine != "" {
		w.project(projectLine + "\n")
	}
	return w
}

// enter makes the bridge the working directory for the rest of the test.
func (w *world) enter() {
	wd, _ := os.Getwd()
	w.t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(w.bridge)
}

func (w *world) frontTask(name, front string) {
	w.t.Helper()
	if front == "" {
		front = "documentation: on\n"
	}
	write(w.t, filepath.Join(w.bridge, "projects/supernova/tasks", name, "task.md"),
		"---\n"+front+"---\n\n## Goal\n\nDo "+name+".\n\n## Status\n\nBegun.\n\n## Decisions\n\nKeep it small.\n")
}

func TestNoCodeResolution(t *testing.T) {
	cases := []struct {
		name, project, task string
		none                bool
		bad                 bool
	}{
		{"project none, task unset", "pipeline: none", "", true, false},
		{"project none, task coding", "pipeline: none", "pipeline: coding\n", false, false},
		{"project unset, task none", "", "pipeline: none\n", true, false},
		{"neither set", "", "", false, false},
		{"project invalid", "pipeline: coding", "", false, true},
	}
	for _, c := range cases {
		w := noCodeWorld(t, c.project)
		w.frontTask("t", c.task)
		got, err := taskHasNoCode(w.bridge, "supernova", "t")
		if (err != nil) != c.bad || got != c.none {
			t.Errorf("%s: got %v, %v; want %v, bad=%v", c.name, got, err, c.none, c.bad)
		}
	}
	w := noCodeWorld(t, "")
	if got, err := taskHasNoCode(w.bridge, "supernova", "missing"); got || err != nil {
		t.Errorf("a missing task is not a no-code task: %v %v", got, err)
	}
	w = noCodeWorld(t, "pipeline: none")
	if got, err := taskHasNoCode(w.bridge, "supernova", "missing"); got || err != nil {
		t.Errorf("a missing task in a default-none project is not a no-code task: %v %v", got, err)
	}
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/broken/task.md"), "---\npipeline: coding\n\n## Goal\n")
	if got, err := taskHasNoCode(w.bridge, "supernova", "broken"); got || err != nil {
		t.Errorf("a broken task in a default-none project is not a no-code task: %v %v", got, err)
	}
	var out bytes.Buffer
	w.enter()
	for _, ref := range []string{"supernova/missing", "supernova/broken"} {
		if err := cmdRun([]string{ref}, &out); err == nil || strings.Contains(err.Error(), noCodeRefusal) {
			t.Errorf("run %s should give the usual error: %v", ref, err)
		}
	}
	w = noCodeWorld(t, "pipeline: coding")
	w.frontTask("t", "documentation: on\n")
	w.enter()
	if err := cmdTask([]string{"open", "supernova/t"}, &out); err == nil || !strings.Contains(err.Error(), "only value") {
		t.Errorf("an invalid project value should be refused when the project loads: %v", err)
	}
}

func TestNoCodeOpen(t *testing.T) {
	w := noCodeWorld(t, "pipeline: none")
	w.enter()
	w.frontTask("t", "locations: [client]\n")
	w.addTask("code")
	os.Remove(machineFile(w.bridge))
	var out bytes.Buffer
	if err := cmdTask([]string{"open", "supernova/t"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task       supernova/t", "no workspace", "Do t.", "Begun.", "Keep it small."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	for _, bad := range []string{"STOP", "vcs ", "location "} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("unexpected %q:\n%s", bad, out.String())
		}
	}
	out.Reset()
	w.frontTask("c", "pipeline: coding\n")
	if err := cmdTask([]string{"open", "supernova/c"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "workspace  STOP") {
		t.Errorf("a code task with no workspace should stop:\n%s", out.String())
	}
}

func TestNoCodeProjectWithoutPlatforms(t *testing.T) {
	w := noCodeWorld(t, "")
	w.enter()
	var out bytes.Buffer
	if err := cmdProject([]string{"new", "ideas", "--pipeline", "none"}, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/ideas/project.yaml"))
	if !strings.Contains(string(data), "\npipeline: none\n") || strings.Contains(string(data), "\nplatforms:") {
		t.Errorf("project file:\n%s", data)
	}
	if err := cmdTask([]string{"new", "ideas/first"}, &out); err != nil {
		t.Fatal(err)
	}
	task, _ := os.ReadFile(filepath.Join(w.bridge, "projects/ideas/tasks/first/task.md"))
	if !strings.HasPrefix(string(task), "---\npipeline: none\n---\n") {
		t.Errorf("task file:\n%s", task)
	}
	out.Reset()
	if err := cmdTask([]string{"open", "ideas/first"}, &out); err != nil || strings.Contains(out.String(), "STOP") {
		t.Errorf("open: %v\n%s", err, out.String())
	}
	if err := cmdRun([]string{"ideas/first"}, &out); err == nil || !strings.Contains(err.Error(), noCodeRefusal) {
		t.Errorf("run: %v", err)
	}
	// A code task there still needs platforms, once it has a workspace.
	ws := filepath.Join(t.TempDir(), "ws")
	os.MkdirAll(ws, 0o755)
	write(t, filepath.Join(w.bridge, "projects/ideas/tasks/code/task.md"), "---\npipeline: coding\n---\n\n## Goal\n\nBuild.\n")
	if _, err := setWorkspace(w.bridge, "ideas", ws); err != nil {
		t.Fatal(err)
	}
	if err := cmdRun([]string{"ideas/code", "--dry-run"}, &out); err == nil || !strings.Contains(err.Error(), "declares no platforms") {
		t.Errorf("a code task needs platforms: %v", err)
	}
}

func TestNoCodeRunRefused(t *testing.T) {
	w := noCodeWorld(t, "pipeline: none")
	w.enter()
	w.frontTask("t", "")
	w.subtaskFile("t", "one", "## Goal\n\nGoal of one.\n\n## Scope\n\nScope of one.\n")
	for _, args := range [][]string{
		{"supernova/t"},
		{"supernova/t", "--dry-run"},
		{"supernova/t", "--pipeline", "coding"},
		{"supernova/t", "--subtask", "one"},
	} {
		var out bytes.Buffer
		if err := cmdRun(args, &out); err == nil || !strings.Contains(err.Error(), noCodeRefusal) {
			t.Errorf("run %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"run", "supernova/t"}, {"run", "supernova/t", "--dry-run"}, {"publish", "supernova/t"}} {
		var out bytes.Buffer
		if err := cmdSequence(args, &out); err == nil || !strings.Contains(err.Error(), noCodeRefusal) {
			t.Errorf("sequence %v: %v", args, err)
		}
	}
	if l := sequenceListing(w.bridge, "s1"); l != "" {
		t.Errorf("a refused request must leave no sequence:\n%s", l)
	}
}

func TestNoCodeTaskNew(t *testing.T) {
	w := noCodeWorld(t, "")
	w.enter()
	ws := t.TempDir()
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks", name, "task.md"))
		return string(data)
	}
	var out bytes.Buffer
	for _, args := range [][]string{
		{"new", "supernova/a", "--pipeline", "none", "--workspace", ws},
		{"new", "supernova/a", "--pipeline", "none", "--locations", "x"},
		{"new", "supernova/a", "--pipeline", "none", "--branch", "b"},
	} {
		if err := cmdTask(args, &out); err == nil || !strings.Contains(err.Error(), "has no meaning") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := cmdTask([]string{"new", "supernova/a", "--pipeline", "other"}, &out); err == nil {
		t.Errorf("an unknown pipeline should be refused")
	}
	if err := cmdTask([]string{"new", "supernova/a", "--pipeline", "none"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(read("a"), "---\npipeline: none\n---\n") {
		t.Errorf("task file:\n%s", read("a"))
	}
	if err := cmdTask([]string{"new", "supernova/b", "--workspace", ws}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(read("b"), "---\npipeline: coding\n---\n") {
		t.Errorf("a code project's default is unchanged:\n%s", read("b"))
	}
	w.project("pipeline: none\n")
	if err := cmdTask([]string{"new", "supernova/c", "--branch", "x"}, &out); err == nil || !strings.Contains(err.Error(), "has no meaning") {
		t.Errorf("default none refuses --branch: %v", err)
	}
	if err := cmdTask([]string{"new", "supernova/c"}, &out); err != nil || !strings.Contains(read("c"), "pipeline: none") {
		t.Errorf("default none: %v\n%s", err, read("c"))
	}
	if err := cmdTask([]string{"new", "supernova/d", "--pipeline", "coding", "--workspace", ws}, &out); err != nil || !strings.HasPrefix(read("d"), "---\npipeline: coding\n---\n") {
		t.Errorf("explicit coding: %v\n%s", err, read("d"))
	}
}

func TestNoCodeProjectNew(t *testing.T) {
	w := noCodeWorld(t, "")
	w.enter()
	ws := t.TempDir()
	var out bytes.Buffer
	for _, args := range [][]string{
		{"new", "p1", "--pipeline", "coding"},
		{"new", "p1", "--pipeline", "none", "--workspace", ws, "--locations", "a"},
		{"new", "p1", "--platforms", "linux"},
		{"new", "p1", "--workspace", ws},
	} {
		if err := cmdProject(args, &out); err == nil {
			t.Errorf("%v should be refused", args)
		}
	}
	if err := cmdProject([]string{"new", "p2", "--pipeline", "none", "--locations", "a,b"}, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/p2/project.yaml"))
	if !strings.Contains(string(data), "pipeline: none") || !strings.Contains(string(data), "  a: {}") || strings.Contains(string(data), "\nplatforms:") {
		t.Errorf("project file:\n%s", data)
	}
	if err := cmdProject([]string{"new", "p3", "--pipeline", "none", "--workspace", ws, "--platforms", "linux"}, &out); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(w.bridge, "projects/p3/project.yaml"))
	if !strings.Contains(string(data), "pipeline: none") || !strings.Contains(string(data), "platforms: [linux]") {
		t.Errorf("project file:\n%s", data)
	}
	if got, _ := workspaceFor(w.bridge, "p3", ""); got != ws {
		t.Errorf("workspace %q", got)
	}
	if err := cmdProject([]string{"new", "p4", "--workspace", ws, "--platforms", "linux"}, &out); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(w.bridge, "projects/p4/project.yaml"))
	if strings.Contains(string(data), "pipeline:") && !strings.Contains(string(data), "# pipeline") {
		t.Errorf("a code project has no pipeline key:\n%s", data)
	}
}

func TestNoCodeJournal(t *testing.T) {
	needGit(t)
	w := noCodeWorld(t, "pipeline: none")
	w.enter()
	w.frontTask("t", "")
	runGit(t, w.bridge, "init", "-q", "-b", "main")
	runGit(t, w.bridge, "config", "user.email", "t@example.com")
	runGit(t, w.bridge, "config", "user.name", "t")
	write(t, filepath.Join(w.bridge, ".gitignore"), ".starship/\n")
	runGit(t, w.bridge, "add", "-A")
	runGit(t, w.bridge, "commit", "-q", "-m", "bridge")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md"), "# t: history\n\n- 2026-11-04: met the user; agreed the goal.\n")
	var out bytes.Buffer
	if err := cmdJournal([]string{"commit", "-m", "Log"}, &out); err != nil && !strings.Contains(err.Error(), "push") {
		t.Fatalf("journal commit: %v\n%s", err, out.String())
	}
	if got := runGit(t, w.bridge, "log", "--format=%s", "-1"); got != "Log" {
		t.Errorf("last commit %q", got)
	}
}
