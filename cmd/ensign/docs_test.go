// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// docsBinding adds a @docs line to the fixture project's bindings.
func (w *world) docsBinding(line string) {
	w.t.Helper()
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(w.t, p, strings.Replace(string(data), "bindings:\n", "bindings:\n  docs: "+line+"\n", 1))
}

// toDocs takes an agent-path run through test, leaving its docs stage current.
func (w *world) toDocs(lead string) {
	w.t.Helper()
	w.toTest(lead)
	w.leaf(lead, "tester", "pass")
}

func (w *world) docsReport(lead, outcome, files string) (string, string) {
	w.t.Helper()
	return w.docsWith(lead, outcome, files, nil)
}

// docsWith spawns the documentation agent, runs edit as the agent's work (after the baseline is taken), and stops it with a report.
func (w *world) docsWith(lead, outcome, files string, edit func()) (string, string) {
	w.t.Helper()
	id, why := w.spawn(lead, "orchestrator", "documentation")
	if id == "" {
		w.t.Fatalf("documentation refused: %s", why)
	}
	if edit != nil {
		edit()
	}
	return id, w.stop(id, "documentation", coderReport(outcome, files), false)
}

func (w *world) hasNote(sub string) bool {
	for _, n := range w.state().Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestDocsStageRunsWhenDeclared(t *testing.T) {
	for outcome, want := range map[string]string{"updated": "success", "unchanged": "success", "blocked": "error"} {
		w := newWorld(t)
		w.docsBinding(`{ paths: [docs, README.md] }`)
		lead := w.startLead()
		w.toDocs(lead)
		if st := w.state(); st.Status == "success" || st.Scopes["run"].Stage != "docs" {
			t.Fatalf("%s: docs should follow test: %s %+v", outcome, st.Status, st.Scopes["run"])
		}
		w.docsReport(lead, outcome, "docs/a.md")
		if st := w.state(); st.Status != want {
			t.Errorf("%s: status %s (%s), want %s", outcome, st.Status, st.Reason, want)
		}
	}
}

func TestDocsStageSkipped(t *testing.T) {
	for name, line := range map[string]string{"nothing": "", "empty": `{ paths: [] }`, "not applicable": `{ not-applicable: "no docs" }`} {
		w := newWorld(t)
		if line != "" {
			w.docsBinding(line)
		}
		lead := w.startLead()
		w.toDocs(lead)
		if st := w.state(); st.Status != "success" || !w.hasNote("skipped") {
			t.Errorf("%s: status %s (%s), notes %v", name, st.Status, st.Reason, st.Notes)
		}
	}
	w := plainWorld(t, "echo 3 passed")
	lead := w.startLead()
	w.toTest(lead)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	if st := w.state(); st.Status != "success" || !w.hasNote("no @docs paths declared") {
		t.Errorf("plain: status %s (%s), notes %v", st.Status, st.Reason, st.Notes)
	}
}

func TestDocsStagePlainRunsWhenDeclared(t *testing.T) {
	w := plainWorld(t, "echo 3 passed")
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.toTest(lead)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	if st := w.state(); st.Status == "success" || st.Scopes["run"].Stage != "docs" {
		t.Fatalf("docs should follow a plain test: %s %+v", st.Status, st.Scopes["run"])
	}
}

func TestDocumentationSetting(t *testing.T) {
	setTask := func(w *world, v string) {
		write(t, filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md"),
			"---\npipeline: coding\ndocumentation: "+v+"\n---\n\n## Goal\n\nAdd multiply.\n")
	}
	cases := []struct {
		name, project, task string
		skipped             bool
	}{
		{"project off", "off", "", true},
		{"task off", "", "off", true},
		{"task on over project off", "off", "on", false},
		{"project on", "on", "", false},
	}
	for _, c := range cases {
		w := newWorld(t)
		w.docsBinding(`{ paths: [docs] }`)
		if c.project != "" {
			w.project("documentation: " + c.project + "\n")
		}
		if c.task != "" {
			setTask(w, c.task)
		}
		lead := w.startLead()
		w.toDocs(lead)
		st := w.state()
		if (st.Status == "success") != c.skipped {
			t.Errorf("%s: status %s, skipped want %v", c.name, st.Status, c.skipped)
		}
		if c.skipped && !w.hasNote("turned off") {
			t.Errorf("%s: no note: %v", c.name, st.Notes)
		}
	}
	w := newWorld(t)
	w.project("documentation: maybe\n")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out strings.Builder
	if err := cmdRun([]string{"supernova/auth-refactor"}, &out); err == nil || !strings.Contains(err.Error(), "on or off") {
		t.Errorf("an invalid value should be an error: %v", err)
	}
}

func TestDocsBindingParsing(t *testing.T) {
	parse := func(src string) (Binding, error) {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		return parseBinding(n.Content[0], "test")
	}
	b, err := parse("paths: [docs, README.md]")
	if err != nil || len(b.Paths) != 2 || b.Paths[1] != "README.md" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := parse("paths: [docs]\ncommand: x"); err == nil {
		t.Error("paths with command should be an error")
	}
	if _, err := parse("paths: [docs]\nguidance: x"); err == nil {
		t.Error("paths with guidance should be an error")
	}
	b, err = parse("linux: { paths: [a] }\ndarwin: { paths: [a] }\nwindows: { paths: [a] }")
	if err != nil || len(b.Paths) != 1 {
		t.Errorf("paths under a platform key: %+v %v", b, err)
	}
	data, _ := json.Marshal(b)
	var back Binding
	if err := json.Unmarshal(data, &back); err != nil || len(back.Paths) != 1 || back.Paths[0] != "a" {
		t.Errorf("round trip: %s %v", data, err)
	}
	if line := bindingLine("docs", b, "/ws"); !strings.Contains(line, "a") || strings.Contains(line, "not applicable") {
		t.Errorf("a path-only binding must render its paths: %s", line)
	}
}

func TestDocsPathProblems(t *testing.T) {
	for _, bad := range []string{"", " ", "/etc", `\x`, "C:docs", "C:/docs", "../x", "a/../b", `a\..\b`, ".", "./", "a/..", "docs/.."} {
		if docsPathProblem(bad) == "" {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, ok := range []string{"docs", "README.md", "docs/guide", "./docs", "a..b/c"} {
		if why := docsPathProblem(ok); why != "" {
			t.Errorf("%q: %s", ok, why)
		}
	}
	c := &Config{ControlPlane: repoRoot(t), Bindings: map[string]Binding{"docs": {Command: "x"}}, Pipeline: &Pipeline{
		Stages: []*Stage{{ID: "s", Role: "coder", Outcomes: Outcomes{{Name: "a", Then: Transition{Finish: "success"}}}, Skip: &StageSkip{Outcome: "nope"}}},
	}}
	got := strings.Join(validate(c), "\n")
	for _, want := range []string{"@docs must list paths", "skip needs unless-binding", `skip outcome "nope"`} {
		if !strings.Contains(got, want) {
			t.Errorf("validate lacks %q:\n%s", want, got)
		}
	}
}

func TestStartDocs(t *testing.T) {
	w := newWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out strings.Builder
	if err := cmdRun([]string{"supernova/auth-refactor", "--start", "docs"}, &out); err == nil || !strings.Contains(err.Error(), "would be skipped") {
		t.Fatalf("--start docs with nothing declared should be refused: %v", err)
	}
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead("--start", "docs")
	if lead == "" || w.state().Status == "success" {
		t.Fatal("--start docs should start a run when docs are declared")
	}
}

func TestDocumentationEditGuard(t *testing.T) {
	w, _ := newGuardWorld(t)
	w.docsBinding(`{ paths: [docs, README.md] }`)
	lead := w.startLead()
	w.toDocs(lead)
	ws := w.workspace()
	os.MkdirAll(filepath.Join(ws, "docs"), 0o755)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "docs", "link")); err != nil {
		t.Skip("no symlinks")
	}
	id, why := w.spawn(lead, "orchestrator", "documentation")
	if id == "" {
		t.Fatal(why)
	}
	w.expectEdit(id, "documentation", filepath.Join(ws, "docs", "guide.md"), false)
	w.expectEdit(id, "documentation", filepath.Join(ws, "README.md"), false)
	w.expectEdit(id, "documentation", filepath.Join(ws, "README.md.bak"), true)
	w.expectEdit(id, "documentation", filepath.Join(ws, "main.go"), true)
	w.expectEdit(id, "documentation", filepath.Join(ws, "docs", "link", "x.md"), true)
	if _, why := w.tryEdit(id, "documentation", filepath.Join(ws, "main.go")); !strings.Contains(why, filepath.Join(ws, "docs")) {
		t.Errorf("the refusal should name the allowed paths: %s", why)
	}
	w.stop(id, "documentation", coderReport("unchanged", ""), false)
	w.expectEdit(id, "documentation", filepath.Join(ws, "docs", "guide.md"), true)
}

func TestPathWithinCaseFolding(t *testing.T) {
	for goos, want := range map[string]bool{"windows": true, "darwin": true, "linux": false} {
		if got := pathWithin("/W/Docs/a.md", "/w/docs", goos); got != want {
			t.Errorf("%s: %v, want %v", goos, got, want)
		}
	}
}

func TestDocsBackstop(t *testing.T) {
	needGit(t)
	for _, tc := range []struct{ name, file string }{{"untracked", "other.txt"}, {"tracked", "lib.txt"}} {
		w := newWorld(t)
		ws := w.gitWorkspace()
		w.docsBinding(`{ paths: [docs] }`)
		lead := w.startLead()
		w.toDocs(lead)
		w.docsWith(lead, "updated", "docs/a.md", func() {
			write(t, filepath.Join(ws, "docs", "a.md"), "doc\n")
			write(t, filepath.Join(ws, tc.file), "changed\n")
		})
		st := w.state()
		if st.Status != "error" || !strings.Contains(st.Reason, tc.file) || strings.Contains(st.Reason, "a.md") {
			t.Errorf("%s: status %s (%s)", tc.name, st.Status, st.Reason)
		}
	}
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.toDocs(lead)
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Errorf("an in-scope change should pass: %s (%s)", st.Status, st.Reason)
	}
}

func TestDocsReportFiles(t *testing.T) {
	w := newWorld(t)
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.toDocs(lead)
	id, why := w.spawn(lead, "orchestrator", "documentation")
	if id == "" {
		t.Fatal(why)
	}
	if r := w.stop(id, "documentation", report("updated"), false); !strings.Contains(r, "no files line") {
		t.Errorf("updated without files should be refused: %q", r)
	}
	if r := w.stop(id, "documentation", report("unchanged"), false); r != "" {
		t.Errorf("unchanged without files should be accepted: %q", r)
	}
}

func TestGithubPRCommitsDocs(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeChange(lead, ws)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if d, _ := dirty(ws); d {
		t.Error("the docs edits should be committed")
	}
	if _, _, sha, err := tested(w.bridge, "supernova/auth-refactor", ""); err != nil || sha != runGit(t, ws, "rev-parse", "HEAD") {
		t.Errorf("tested() should accept the docs commit: %s %v", sha, err)
	}
}

// codeToDocs runs a coder pass that edits lib.txt and reports it with coderMsg, then review, style and test by hand, leaving docs current.
func (w *world) codeToDocs(lead, ws, coderMsg string) {
	w.t.Helper()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		w.t.Fatalf("coder refused: %s", why)
	}
	write(w.t, filepath.Join(ws, "lib.txt"), "two\n")
	if r := w.stop(coder, "coder", coderMsg, false); r != "" {
		w.t.Fatalf("coder stop was blocked: %s", r)
	}
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
}

func (w *world) docsCommitFiles(ws string) string {
	w.t.Helper()
	return runGit(w.t, ws, "show", "--name-only", "--format=", "HEAD")
}

func TestGitCatalogCommitsDocs(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeToDocs(lead, ws, coderReport("compiled", "lib.txt"))
	before := runGit(t, ws, "rev-parse", "HEAD")
	w.docsReport(lead, "unchanged", "")
	if st := w.state(); st.Status != "success" || runGit(t, ws, "rev-parse", "HEAD") != before {
		t.Errorf("a clean tree should be a no-op: %s (%s)", w.state().Status, w.state().Reason)
	}

	w = newWorld(t)
	ws = w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	w.docsBinding(`{ paths: [docs] }`)
	lead = w.startLead()
	w.codeToDocs(lead, ws, coderReport("compiled", "lib.txt"))
	write(t, filepath.Join(ws, "style.txt"), "style edit\n")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if subj := runGit(t, ws, "log", "--format=%s", "-1"); !strings.HasPrefix(subj, "docs") {
		t.Errorf("subject %q", subj)
	}
	if got := w.docsCommitFiles(ws); got != "docs/a.md" {
		t.Errorf("the docs commit holds %q", got)
	}
	if status := runGit(t, ws, "status", "--porcelain"); !strings.Contains(status, "style.txt") {
		t.Errorf("the style edit should stay dirty: %q", status)
	}
}

func TestDocsToleratesEarlierWork(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeToDocs(lead, ws, report("compiled"))
	w.docsReport(lead, "unchanged", "")
	if st := w.state(); st.Status != "success" {
		t.Errorf("uncommitted coder work is not the docs stage's: %s (%s)", st.Status, st.Reason)
	}
}

func TestGithubPRDocsLeavesLeftoversDirty(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeChange(lead, ws)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "coverage.out"), "mode: set\n")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := w.docsCommitFiles(ws); got != "docs/a.md" {
		t.Errorf("the docs commit holds %q", got)
	}
	if _, _, _, err := tested(w.bridge, "supernova/auth-refactor", ""); err == nil {
		t.Error("tested() should refuse the leftover coverage.out")
	}
}

func TestGithubPRCommitDocsNoOps(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs, nothere] }`)
	lead := w.startLead()
	w.codeChange(lead, ws)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	before := runGit(t, ws, "rev-parse", "HEAD")
	w.docsReport(lead, "unchanged", "")
	if st := w.state(); st.Status != "success" || runGit(t, ws, "rev-parse", "HEAD") != before {
		t.Errorf("nothing changed should be a no-op: %s (%s)", st.Status, st.Reason)
	}

	w, ws, _, _ = githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs, nothere] }`)
	lead = w.startLead()
	w.codeChange(lead, ws)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Errorf("a declared path that doesn't exist should not fail: %s (%s)", st.Status, st.Reason)
	}
}

func TestDocsBaselineChangesAgain(t *testing.T) {
	needGit(t)
	for _, tc := range []struct {
		name, file string
		edit       func(ws string)
	}{
		{"changed", "lib.txt", func(ws string) { write(t, filepath.Join(ws, "lib.txt"), "three\n") }},
		{"deleted tracked", "lib.txt", func(ws string) { os.Remove(filepath.Join(ws, "lib.txt")) }},
		{"deleted untracked", "early.txt", func(ws string) { os.Remove(filepath.Join(ws, "early.txt")) }},
	} {
		w := newWorld(t)
		ws := w.gitWorkspace()
		w.docsBinding(`{ paths: [docs] }`)
		lead := w.startLead()
		w.codeToDocs(lead, ws, report("compiled"))
		write(t, filepath.Join(ws, "early.txt"), "early\n")
		w.docsWith(lead, "updated", "docs/a.md", func() {
			write(t, filepath.Join(ws, "docs", "a.md"), "doc\n")
			tc.edit(ws)
		})
		st := w.state()
		if st.Status != "error" || !strings.Contains(st.Reason, tc.file) || strings.Contains(st.Reason, "a.md") {
			t.Errorf("%s: status %s (%s)", tc.name, st.Status, st.Reason)
		}
	}
}

func failingHook(t *testing.T, ws string) {
	t.Helper()
	hook := filepath.Join(ws, ".git", "hooks", "pre-commit")
	write(t, hook, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDocsCommitHooks(t *testing.T) {
	needGit(t)
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeChange(lead, ws)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	failingHook(t, ws)
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "error" {
		t.Errorf("github-pr: a failing pre-commit hook should fail the docs commit: %s (%s)", st.Status, st.Reason)
	}

	w = newWorld(t)
	ws = w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	w.docsBinding(`{ paths: [docs] }`)
	lead = w.startLead()
	w.codeToDocs(lead, ws, coderReport("compiled", "lib.txt"))
	failingHook(t, ws)
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Errorf("git-catalog: the hook should not stop the docs commit: %s (%s)", st.Status, st.Reason)
	}
	if got := w.docsCommitFiles(ws); got != "docs/a.md" {
		t.Errorf("the docs commit holds %q", got)
	}
}

func TestDocsRemovedFileInDocs(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeToDocs(lead, ws, coderReport("compiled", "lib.txt"))
	write(t, filepath.Join(ws, "docs", "gen.md"), "generated\n")
	w.docsWith(lead, "updated", "docs/a.md", func() {
		write(t, filepath.Join(ws, "docs", "a.md"), "doc\n")
		os.Remove(filepath.Join(ws, "docs", "gen.md"))
	})
	if st := w.state(); st.Status != "success" {
		t.Fatalf("an untracked docs file removed in the stage: %s (%s)", st.Status, st.Reason)
	}
	if got := w.docsCommitFiles(ws); got != "docs/a.md" {
		t.Errorf("the docs commit holds %q", got)
	}

	w = newWorld(t)
	ws = w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	w.docsBinding(`{ paths: [docs] }`)
	write(t, filepath.Join(ws, "docs", "old.md"), "old\n")
	runGit(t, ws, "add", "docs/old.md")
	runGit(t, ws, "commit", "-q", "-m", "add old doc")
	lead = w.startLead()
	w.codeToDocs(lead, ws, coderReport("compiled", "lib.txt"))
	w.docsWith(lead, "updated", "docs/old.md", func() { os.Remove(filepath.Join(ws, "docs", "old.md")) })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := w.docsCommitFiles(ws); got != "docs/old.md" {
		t.Errorf("the deletion should be committed, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "docs", "old.md")); err == nil {
		t.Error("old.md should stay deleted")
	}
}
