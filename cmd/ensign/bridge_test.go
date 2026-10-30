// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	root, _ := filepath.Abs("../..")
	return root
}

// newBridge runs ensign init against this repo as the control plane.
func newBridge(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	var out bytes.Buffer
	if err := cmdInit([]string{dir, "--control-plane", repoRoot(t)}, &out); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	return dir
}

func TestInitAndLink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	b := newBridge(t)
	if status, _ := gitOut(b, "status", "--porcelain"); status != "" {
		t.Errorf("init should commit the new bridge; uncommitted: %q", status)
	}
	if changed := journalChanges(b); len(changed) != 0 {
		t.Errorf("a new bridge should have no journal changes: %v", changed)
	}
	for _, f := range []string{"bridge.yaml", "CLAUDE.md", ".gitignore", ".claude/settings.json",
		".claude/agents/coder.md", ".claude/agents/orchestrator.md", ".starship/session.md", ".starship/link.yaml"} {
		if _, err := os.Stat(filepath.Join(b, f)); err != nil {
			t.Errorf("init did not create %s", f)
		}
	}
	var settings map[string]any
	data, _ := os.ReadFile(filepath.Join(b, ".claude/settings.json"))
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	if claude, _ := os.ReadFile(filepath.Join(b, "CLAUDE.md")); strings.Contains(string(claude), sessionImport) ||
		!strings.Contains(string(claude), "STARSHIP-READY") {
		t.Errorf("CLAUDE.md should carry the heartbeat rule and not import the manual (subagents would get it)")
	}
	if linkStale(b) {
		t.Errorf("link is stale right after init")
	}

	// A hand edit to a generated file is protected.
	coder := filepath.Join(b, ".claude/agents/coder.md")
	write(t, coder, "edited by hand\n")
	if err := link(b, false, nil); err == nil || !strings.Contains(err.Error(), "edited by hand") {
		t.Fatalf("link overwrote a hand edit: %v", err)
	}
	if err := link(b, true, nil); err != nil {
		t.Fatal(err)
	}

	// A bridge can change a role's model.
	bf, _ := os.ReadFile(filepath.Join(b, "bridge.yaml"))
	write(t, filepath.Join(b, "bridge.yaml"), string(bf)+"roles:\n  coder: { model: haiku }\n")
	if !linkStale(b) {
		t.Errorf("a role override should make link stale")
	}
	if err := link(b, false, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(coder); !strings.Contains(string(data), "model: haiku") {
		t.Errorf("model override not applied")
	}

	// A bridge role clashing with a control-plane role needs replaces.
	write(t, filepath.Join(b, "roles/coder.md"), "---\nname: coder\nmodel: sonnet\n---\nmine\n")
	if err := link(b, false, nil); err == nil || !strings.Contains(err.Error(), "replaces: coder") {
		t.Fatalf("clash not refused: %v", err)
	}
	write(t, filepath.Join(b, "roles/coder.md"), "---\nname: coder\nreplaces: coder\nmodel: sonnet\n---\nmine\n")
	if err := link(b, false, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(coder); !strings.Contains(string(data), "mine") {
		t.Errorf("bridge role did not replace the control plane's")
	}
}

func TestProjectAndTask(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	b := newBridge(t)
	ws := filepath.Join(t.TempDir(), "ws")
	os.MkdirAll(ws, 0o755)
	exec.Command("git", "-C", ws, "init", "-q", "-b", "main").Run()
	t.Setenv("STARSHIP_SESSION", "sess")
	var out bytes.Buffer
	if err := cmdTask([]string{"new", "nova/first", "--workspace", ws}, &out); err == nil {
		t.Fatalf("a task in a missing project should be refused")
	}
	if err := cmdProject([]string{"new", "nova"}, &out); err == nil {
		t.Fatalf("a project without its code's location should be refused")
	}
	if err := cmdProject([]string{"new", "nova", "--workspace", filepath.Join(ws, "missing")}, &out); err == nil || !strings.Contains(err.Error(), "never guess") {
		t.Fatalf("a project whose folder doesn't exist should be refused: %v", err)
	}
	if err := cmdProject([]string{"new", "nova", "--workspace", ws}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"new", "nova/first", "--branch", "feature"}, &out); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(b, "projects/nova/tasks/first/task.md")); !strings.Contains(string(data), "workspace: "+filepath.ToSlash(ws)+"\n") {
		t.Errorf("a new task should use the project's workspace:\n%s", data)
	}
	other := t.TempDir()
	if err := cmdTask([]string{"new", "nova/second", "--workspace", other}, &out); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(b, "projects/nova/tasks/second/task.md")); !strings.Contains(string(data), "workspace: "+filepath.ToSlash(other)+"\n") {
		t.Errorf("--workspace should override the project's:\n%s", data)
	}
	task := filepath.Join(b, "projects/nova/tasks/first/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, strings.Replace(string(data), "## Goal\n", "## Goal\n\nMake it faster.\n", 1))

	out.Reset()
	if err := cmdTask([]string{"open", "nova/first"}, &out); err != nil {
		t.Fatal(err)
	}
	o := out.String()
	if !strings.Contains(o, "the task expects branch feature") || !strings.Contains(o, "Make it faster.") {
		t.Errorf("task open output:\n%s", o)
	}
	if sessionTask(b, "sess") != "nova/first" {
		t.Errorf("task open did not record the session's task")
	}

	help := sessionWord(b, "sess", "help")
	for _, want := range []string{"Bridge: home · Project: nova · Task: first", "Getting around", "nova (2 tasks)"} {
		if !strings.Contains(help, want) {
			t.Errorf("help is missing %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "ensign") {
		t.Errorf("session help must not mention ensign:\n%s", help)
	}
	if st := sessionWord(b, "sess", "status"); !strings.Contains(st, "Make it faster.") || !strings.Contains(st, "No run") {
		t.Errorf("status:\n%s", st)
	}
	if h := sessionWord(b, "sess", "help nova"); !strings.Contains(h, "first") || !strings.Contains(h, "New.") {
		t.Errorf("help nova:\n%s", h)
	}
	if sessionWord(b, "sess", "help me with this") == "" {
		t.Logf("free-form help goes to the model")
	}
	if sessionWord(b, "sess", "please go") != "" {
		t.Errorf("only exact words are intercepted")
	}
}

func TestWorkItemIsGoalAndScope(t *testing.T) {
	body := "# t\n\n## Goal\n\nDo X.\n\n## Scope\n\nIn: a\n\n## Progress log\n\n- long history\n"
	w := workItem(body)
	if !strings.Contains(w, "Do X.") || !strings.Contains(w, "In: a") || strings.Contains(w, "long history") {
		t.Fatalf("work item:\n%s", w)
	}
}

func TestControlPlaneEditsAreRefused(t *testing.T) {
	b := newBridge(t)
	try := func(path string) map[string]any {
		in, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s",
			"tool_name": "Edit", "tool_input": map[string]any{"file_path": path}})
		var out, errb bytes.Buffer
		if code := cmdHook("PreToolUse", bytes.NewReader(in), &out, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		var res map[string]any
		json.Unmarshal(out.Bytes(), &res)
		return res
	}
	if res := try(filepath.Join(repoRoot(t), "roles/coder.md")); dig(res, "hookSpecificOutput", "permissionDecision") != "deny" {
		t.Errorf("an edit to the control plane was not refused: %v", res)
	}
	if res := try(filepath.Join(b, "projects/x/project.yaml")); res != nil {
		t.Errorf("an edit inside the bridge was refused: %v", res)
	}
}

// Starship ships its own project settings, so a bridge can work on it with nothing to
// fill in: adding it as a project gives a configuration that validates.
func TestShippedProjectSettings(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	b := newBridge(t)
	var out bytes.Buffer
	if err := cmdProject([]string{"new", "starship", "--workspace", repoRoot(t)}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "from the code's own .starship/project.yaml") {
		t.Errorf("project new should say where the settings came from: %s", out.String())
	}
	if err := cmdTask([]string{"new", "starship/tidy"}, &out); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(b, "starship/tidy")
	if err != nil {
		t.Fatal(err)
	}
	if errs := validate(c); len(errs) > 0 {
		t.Fatalf("the shipped settings should validate:\n%s", strings.Join(errs, "\n"))
	}
	if !c.Bindings["test"].Plain {
		t.Errorf("starship's tests run through CI as a plain @test")
	}

	// A task goes straight to master by replacing @branch and @test in its own settings.
	task := filepath.Join(b, "projects/starship/tasks/tidy/task.md")
	data, _ := os.ReadFile(task)
	direct := "bindings:\n" +
		"  branch: { command: 'sh \"$STARSHIP_WORKSPACE/.starship/direct.sh\" start' }\n" +
		"  test: { command: 'sh \"$STARSHIP_WORKSPACE/.starship/direct.sh\" test', plain: true }\n---\n"
	write(t, task, strings.Replace(string(data), "\n---\n", "\n"+direct, 1)) // before the frontmatter's closing line
	if c, err = loadConfig(b, "starship/tidy"); err != nil {
		t.Fatal(err)
	}
	if errs := validate(c); len(errs) > 0 {
		t.Fatalf("a direct task should validate:\n%s", strings.Join(errs, "\n"))
	}
	if !strings.Contains(c.Bindings["branch"].Command, "direct.sh\" start") || !strings.Contains(c.Bindings["test"].Command, "direct.sh\" test") || !c.Bindings["test"].Plain {
		t.Errorf("the task's bindings should replace the project's: %+v %+v", c.Bindings["branch"], c.Bindings["test"])
	}

	// A shipped file may not choose the workspace.
	ws := t.TempDir()
	write(t, filepath.Join(ws, ".starship", "project.yaml"), "workspace: /elsewhere\n")
	if err := cmdProject([]string{"new", "other", "--workspace", ws}, &out); err == nil {
		t.Fatal("a shipped workspace should be refused")
	}
}
