// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
	return id, w.stop(id, "documentation", strings.Replace(coderReport(outcome, files), "\n```\n", "\nchecked: cmd/ensign/hook.go parseReport\n```\n", 1), false)
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

func TestStartDocsRefusedWhenOff(t *testing.T) {
	w := newWorld(t)
	w.docsBinding(`{ paths: [docs] }`)
	w.project("documentation: off\n")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out strings.Builder
	if err := cmdRun([]string{"supernova/auth-refactor", "--start", "docs"}, &out); err == nil || !strings.Contains(err.Error(), "would be skipped") {
		t.Fatalf("--start docs with documentation off should be refused: %v", err)
	}
}

func TestHelpListsStartDocs(t *testing.T) {
	w := newWorld(t)
	extra, _ := extraHelp(w.bridge)
	for _, e := range extra["Doing the work"] {
		if e.Run == "ensign run <project>/<task> --start docs" {
			return
		}
	}
	t.Error("the help lacks the --start docs entry")
}

// markerSha reads the tested commit recorded for the fixture task, or "" when there is none.
func (w *world) markerSha() string {
	data, _ := os.ReadFile(testedFile(w.bridge, "supernova/auth-refactor", ""))
	return strings.TrimSpace(string(data))
}

func TestGithubPRDocsOnlyRunRecordsNoTestedCommit(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead("--start", "docs")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-qam", "never tested")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if sha := w.markerSha(); sha != "" {
		t.Errorf("a run that never tested must record no tested commit, got %s", sha)
	}
	if !w.hasNote("No test ran in this run") || !w.hasNote("--start test") {
		t.Errorf("the note should say nothing was tested and how to record it: %v", w.state().Notes)
	}
	if _, _, _, err := tested(w.bridge, "supernova/auth-refactor", ""); err == nil {
		t.Error("tested() should refuse a branch no run tested")
	}
}

func TestGithubPRFailedThenPassedTestRecordsHead(t *testing.T) {
	w, ws, _, gh := githubWorld(t, "failure", ciTest)
	w.docsBinding(`{ paths: [docs] }`)
	lead := w.startLead()
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: fail") {
		t.Fatalf("the first test should fail: %s", why)
	}
	gh.conclusion = "success"
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	write(t, filepath.Join(ws, "lib.txt"), "three\n")
	w.stop(coder, "coder", report("compiled"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the retried test should pass: %s", why)
	}
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got, head := w.markerSha(), runGit(t, ws, "rev-parse", "HEAD"); got != head {
		t.Errorf("the marker should hold HEAD %s, got %q", head, got)
	}
}

func TestGithubPRStartTestRecordsHead(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.startLead("--start", "test")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-qam", "untested so far")
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the test should pass: %s", why)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got, head := w.markerSha(), runGit(t, ws, "rev-parse", "HEAD"); got != head {
		t.Errorf("the marker should hold HEAD %s, got %q", head, got)
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
	w.stop(id, "documentation", "Done.\n\n```starship-report\noutcome: unchanged\nsummary: none\nchecked: none, no code bears on the docs\n```\n", false)
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
	if r := w.stop(id, "documentation", report("unchanged"), false); !strings.Contains(r, "no checked line") {
		t.Errorf("unchanged without checked should be refused: %q", r)
	}
	unchanged := "Done.\n\n```starship-report\noutcome: unchanged\nsummary: none\nchecked: none, no code bears on the docs\n```\n"
	if r := w.stop(id, "documentation", unchanged, false); r != "" {
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

func TestValidateCurates(t *testing.T) {
	for _, curates := range []string{"", "style", "skill", "document", "brag", "brag-feedback", "styles"} {
		c := &Config{ControlPlane: repoRoot(t), Pipeline: &Pipeline{
			Stages: []*Stage{{ID: "s", Role: "coder", Curates: curates, Outcomes: Outcomes{{Name: "a", Then: Transition{Finish: "success"}}}}},
		}}
		got := strings.Join(validate(c), "\n")
		if curates == "styles" {
			if !strings.Contains(got, `curates "styles" is not supported`) {
				t.Errorf("validate should refuse curates styles:\n%s", got)
			}
		} else if strings.Contains(got, "curates") {
			t.Errorf("curates %q should be accepted:\n%s", curates, got)
		}
	}
}

// testedWorld runs a task to a passing run and returns the world, its workspace, the remote and the tested commit.
func testedWorld(t *testing.T, docsLine string) (*world, string, string, string) {
	t.Helper()
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	if docsLine != "" {
		w.docsBinding(docsLine)
	}
	sha := w.markerSha()
	if sha == "" || sha != runGit(t, ws, "rev-parse", "HEAD") {
		t.Fatalf("a passing run should record its head, got %q", sha)
	}
	return w, ws, remote, sha
}

func TestDocsStartKeepsTheTestedMarker(t *testing.T) {
	w, ws, remote, sha := testedWorld(t, `{ paths: [docs] }`)
	lead := w.startLead("--start", "docs")
	if got := w.markerSha(); got != sha {
		t.Fatalf("a docs start must keep the marker: %q", got)
	}
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	head := runGit(t, ws, "rev-parse", "HEAD")
	if head == sha {
		t.Fatal("the docs run should have committed")
	}
	if got := w.markerSha(); got != sha {
		t.Errorf("the marker should still name the tested commit: %q", got)
	}
	if !w.hasNote("still tested") {
		t.Errorf("the note should say the branch is still tested: %v", w.state().Notes)
	}
	if _, _, got, err := tested(w.bridge, authTask, ""); err != nil || got != head {
		t.Errorf("tested() should accept and return the branch tip: %s %v", got, err)
	}
	if _, err := w.land("merge"); err != nil {
		t.Fatal(err)
	}
	if got := remoteMain(t, remote); got != head {
		t.Errorf("merge should land the tip with the docs commit: %s, want %s", got, head)
	}
}

func TestDocsOnlyHandCommitsStayTested(t *testing.T) {
	for name, tc := range map[string]struct{ line, file string }{
		"directory":   {`{ paths: [docs] }`, "docs/a.md"},
		"single file": {`{ paths: [README.md, docs] }`, "README.md"},
	} {
		t.Run(name, func(t *testing.T) {
			w, ws, _, _ := testedWorld(t, tc.line)
			write(t, filepath.Join(ws, filepath.FromSlash(tc.file)), "doc\n")
			runGit(t, ws, "add", "-A")
			runGit(t, ws, "commit", "-qm", "docs")
			if _, _, got, err := tested(w.bridge, authTask, ""); err != nil || got != runGit(t, ws, "rev-parse", "HEAD") {
				t.Errorf("a docs-only commit should stay tested: %s %v", got, err)
			}
		})
	}
}

func TestNonDocsCommitsLoseTheTestedState(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T, ws string){
		"code": func(t *testing.T, ws string) {
			write(t, filepath.Join(ws, "lib.txt"), "three\n")
		},
		"both": func(t *testing.T, ws string) {
			write(t, filepath.Join(ws, "lib.txt"), "three\n")
			write(t, filepath.Join(ws, "docs", "a.md"), "doc\n")
		},
		"rename into docs": func(t *testing.T, ws string) {
			runGit(t, ws, "mv", "lib.txt", "docs-moved.txt")
			if err := os.MkdirAll(filepath.Join(ws, "docs"), 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, ws, "mv", "docs-moved.txt", "docs/lib.txt")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, ws, _, _ := testedWorld(t, `{ paths: [docs] }`)
			edit(t, ws)
			runGit(t, ws, "add", "-A")
			runGit(t, ws, "commit", "-qm", "change")
			if _, _, _, err := tested(w.bridge, authTask, ""); err == nil || !strings.Contains(err.Error(), "has changed") {
				t.Errorf("a commit touching code must be refused: %v", err)
			}
		})
	}
}

func TestDocsDescendantNeedsADocsBinding(t *testing.T) {
	w, ws, _, _ := testedWorld(t, "")
	write(t, filepath.Join(ws, "docs", "a.md"), "doc\n")
	runGit(t, ws, "add", "-A")
	runGit(t, ws, "commit", "-qm", "docs")
	if _, _, _, err := tested(w.bridge, authTask, ""); err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Errorf("without a docs binding a descendant must be refused: %v", err)
	}
}

func TestRewrittenBranchLosesTheTestedState(t *testing.T) {
	w, ws, _, _ := testedWorld(t, `{ paths: [docs] }`)
	runGit(t, ws, "commit", "-q", "--amend", "-m", "reworded")
	if _, _, _, err := tested(w.bridge, authTask, ""); err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Errorf("a rewritten branch must be refused: %v", err)
	}
}

func TestFailedDocsRunRefusesPublishing(t *testing.T) {
	w, _, _, _ := testedWorld(t, `{ paths: [docs] }`)
	lead := w.startLead("--start", "docs")
	w.docsWith(lead, "blocked", "docs/a.md", nil)
	if st := w.state(); st.Status != "error" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if _, _, _, err := tested(w.bridge, authTask, ""); err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Errorf("a failed docs run should refuse publishing: %v", err)
	}
}

func TestOtherStartsStillClearTheMarker(t *testing.T) {
	w, ws, _, _ := testedWorld(t, `{ paths: [docs] }`)
	r := &RunFiles{Snap: &Snapshot{Bridge: w.bridge, Task: authTask, Workspace: ws, Start: "test"}, State: &State{}}
	if err := githubBranch(r); err != nil {
		t.Fatal(err)
	}
	if got := w.markerSha(); got != "" {
		t.Errorf("a non-docs start must clear the marker, got %s", got)
	}
}

func TestDocsSymlinkIntoDocsIsNotDocs(t *testing.T) {
	w, ws, _, _ := testedWorld(t, `{ paths: [docs] }`)
	write(t, filepath.Join(ws, "docs", "sample.yaml"), "a: 1\n")
	if err := os.Symlink(filepath.Join("docs", "sample.yaml"), filepath.Join(ws, "config.yaml")); err != nil {
		t.Skip("no symlinks")
	}
	runGit(t, ws, "add", "-A")
	runGit(t, ws, "commit", "-qm", "link")
	if _, _, _, err := tested(w.bridge, authTask, ""); err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Errorf("a committed symlink outside docs must be refused: %v", err)
	}
}

func TestDocsOnlySinceInASubfolder(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "sub", "code.txt"), "one\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	ws := filepath.Join(repo, "sub")

	write(t, filepath.Join(repo, "sub", "docs", "a.md"), "doc\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "sub docs")
	head := runGit(t, repo, "rev-parse", "HEAD")
	if !docsOnlySince(ws, base, head, []string{"docs"}) {
		t.Error("sub/docs/a.md should count as docs for a workspace in sub")
	}
	if docsOnlySince(ws, base, head, nil) {
		t.Error("no declared paths should never count")
	}

	write(t, filepath.Join(repo, "docs", "a.md"), "doc\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "root docs")
	if docsOnlySince(ws, head, runGit(t, repo, "rev-parse", "HEAD"), []string{"docs"}) {
		t.Error("docs/a.md at the repo root is outside the workspace's docs")
	}
}

func TestDocsOnlySinceIgnoresDiffConfigThatHidesPaths(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "diff.relative", "true")
	write(t, filepath.Join(repo, "sub", "code.txt"), "one\n")
	write(t, filepath.Join(repo, "other", "o"), "one\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	ws := filepath.Join(repo, "sub")

	write(t, filepath.Join(repo, "other", "o"), "two\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "outside")
	head := runGit(t, repo, "rev-parse", "HEAD")
	if docsOnlySince(ws, base, head, []string{"docs"}) {
		t.Error("a change outside the workspace must be refused under diff.relative")
	}

	write(t, filepath.Join(repo, "sub", "docs", "a.md"), "doc\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "docs")
	if !docsOnlySince(ws, head, runGit(t, repo, "rev-parse", "HEAD"), []string{"docs"}) {
		t.Error("sub/docs/a.md should count as docs under diff.relative")
	}
}

func TestDocsOnlySinceRefusesAGitlinkChangeUnderIgnoreAll(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	write(t, filepath.Join(repo, ".gitmodules"), "[submodule \"lib\"]\n\tpath = lib\n\turl = ./lib\n\tignore = all\n")
	runGit(t, repo, "add", ".gitmodules")
	runGit(t, repo, "commit", "-qm", "first")
	first := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+first+",lib")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")

	other := runGit(t, repo, "commit-tree", "-m", "other", "HEAD^{tree}")
	runGit(t, repo, "update-index", "--cacheinfo", "160000,"+other+",lib")
	runGit(t, repo, "commit", "-qm", "bump")
	head := runGit(t, repo, "rev-parse", "HEAD")
	if got := runGit(t, repo, "diff", "--name-only", base, head); got != "" {
		t.Fatalf("a plain diff should hide the gitlink bump under ignore = all, got %q", got)
	}
	if docsOnlySince(repo, base, head, []string{"docs"}) {
		t.Error("a gitlink change must be refused even when .gitmodules says ignore = all")
	}
}

func TestDocsRunThatChangesNothingKeepsTheTestedCommit(t *testing.T) {
	w, _, _, sha := testedWorld(t, `{ paths: [docs] }`)
	lead := w.startLead("--start", "docs")
	w.docsWith(lead, "unchanged", "", nil)
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if !w.hasNote("still the tested commit") || w.hasNote("only @docs paths changed") {
		t.Errorf("the note should say the branch is still the tested commit: %v", w.state().Notes)
	}
	if _, _, got, err := tested(w.bridge, authTask, ""); err != nil || got != sha {
		t.Errorf("the tested commit should be unchanged: %s %v", got, err)
	}
}

func TestCommitDocsPathsStagesOnlyLiteralNames(t *testing.T) {
	needGit(t)
	cases := []struct{ name, path, sibling string }{
		{"bracket", "a[b].md", "ab.md"},
		{"star", "a*.md", "ab.md"},
		{"colon", ":(glob)x.md", "x.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name != "bracket" && runtime.GOOS == "windows" {
				t.Skip("the name can't exist on Windows")
			}
			ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
			write(t, filepath.Join(ws, c.path), "old\n")
			write(t, filepath.Join(ws, c.sibling), "old\n")
			runGit(t, ws, "add", ".")
			runGit(t, ws, "commit", "-q", "-m", "more")
			write(t, filepath.Join(ws, c.path), "new\n")
			write(t, filepath.Join(ws, c.sibling), "new\n")
			if err := commitDocsPaths(ws, []string{c.path}, true, "-m", "docs"); err != nil {
				t.Fatal(err)
			}
			if got := runGit(t, ws, "-c", "core.quotepath=off", "show", "--name-only", "--format=", "HEAD"); got != c.path {
				t.Errorf("committed %q, want only %q", got, c.path)
			}
			if got := runGit(t, ws, "-c", "core.quotepath=off", "status", "--porcelain"); !strings.Contains(got, c.sibling) {
				t.Errorf("the sibling should stay uncommitted: %q", got)
			}

			// A tracked name deleted from disk is found through ls-files.
			if err := os.Remove(filepath.Join(ws, c.path)); err != nil {
				t.Fatal(err)
			}
			if err := commitDocsPaths(ws, []string{c.path}, true, "-m", "drop"); err != nil {
				t.Fatal(err)
			}
			if got := runGit(t, ws, "-c", "core.quotepath=off", "show", "--name-only", "--format=", "HEAD"); got != c.path {
				t.Errorf("deletion committed %q, want only %q", got, c.path)
			}
			if _, err := os.Stat(filepath.Join(ws, c.sibling)); err != nil {
				t.Errorf("the sibling should remain: %v", err)
			}
		})
	}
}

func TestCommitDocsPathsCommitsAPathOutsideTheRootUnderDiffRelative(t *testing.T) {
	repo := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	runGit(t, repo, "config", "diff.relative", "true")
	write(t, filepath.Join(repo, "sub", "code.txt"), "one\n")
	write(t, filepath.Join(repo, "other", "o.md"), "one\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "base")
	write(t, filepath.Join(repo, "other", "o.md"), "two\n")
	if err := commitDocsPaths(filepath.Join(repo, "sub"), []string{"../other/o.md"}, true, "-m", "docs"); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, repo, "show", "--name-only", "--format=", "HEAD"); got != "other/o.md" {
		t.Errorf("committed %q, want other/o.md", got)
	}
}

func TestCommitDocsPathsCommitsAStagedGitlinkHiddenByConfig(t *testing.T) {
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	write(t, filepath.Join(ws, ".gitmodules"), "[submodule \"lib\"]\n\tpath = lib\n\turl = ./lib\n\tignore = all\n")
	runGit(t, ws, "add", ".gitmodules")
	runGit(t, ws, "commit", "-qm", "modules")
	before := runGit(t, ws, "rev-parse", "HEAD")
	newRepo(t, filepath.Join(ws, "lib"))
	if err := commitDocsPaths(ws, []string{"lib"}, true, "-m", "docs"); err != nil {
		t.Fatal(err)
	}
	if after := runGit(t, ws, "rev-parse", "HEAD"); after == before {
		t.Error("the staged gitlink should have been committed")
	}
}

func TestCommitDocsPathsHookDoesNotSeeLiteralPathspecs(t *testing.T) {
	needGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts need a POSIX shell, unreliable on Windows")
	}
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	marker := filepath.Join(t.TempDir(), "env.txt")
	hook := filepath.Join(ws, ".git", "hooks", "pre-commit")
	write(t, hook, "#!/bin/sh\necho \"[${GIT_LITERAL_PATHSPECS-unset}]\" > '"+marker+"'\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "a.md"), "new\n")
	if err := commitDocsPaths(ws, []string{"a.md"}, false, "-m", "docs"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the hook should have run: %v", err)
	}
	if strings.TrimSpace(string(got)) != "[unset]" {
		t.Errorf("hook saw GIT_LITERAL_PATHSPECS = %s", got)
	}
}

// noTestStage drops the test stage from the fixture's coding pipeline.
func (w *world) noTestStage() {
	w.t.Helper()
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	s := strings.Replace(string(data), "    ops:\n", "    ops:\n      - remove: test\n", 1)
	if s == string(data) {
		w.t.Fatal("the fixture's coding pipeline has no ops list")
	}
	write(w.t, p, s)
}

func TestDocsStartWithoutTestStageKeepsAnExistingMarker(t *testing.T) {
	w, ws, _, sha := testedWorld(t, `{ paths: [docs] }`)
	w.noTestStage()
	lead := w.startLead("--start", "docs")
	write(t, filepath.Join(ws, "lib.txt"), "three\n")
	runGit(t, ws, "commit", "-qam", "never tested")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := w.markerSha(); got != sha {
		t.Errorf("the marker must stay on the tested commit %s, got %s", sha, got)
	}
	if !w.hasNote("not recorded as tested") || w.hasNote("--start test") || !w.hasNote("ensign run supernova/auth-refactor`") {
		t.Errorf("the note should advise a full run, not --start test: %v", w.state().Notes)
	}
}

func TestDocsStartWithoutTestStageKeepsTheMarkerOverDocsCommits(t *testing.T) {
	w, ws, _, sha := testedWorld(t, `{ paths: [docs] }`)
	w.noTestStage()
	lead := w.startLead("--start", "docs")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := w.markerSha(); got != sha {
		t.Errorf("the marker must stay on the tested commit %s, got %s", sha, got)
	}
	if !w.hasNote("only @docs paths changed") {
		t.Errorf("the note should say only docs changed: %v", w.state().Notes)
	}
}

func TestDocsStartWithoutTestStageRecordsNothingForAMissingMarker(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs] }`)
	w.noTestStage()
	lead := w.startLead("--start", "docs")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := w.markerSha(); got != "" {
		t.Errorf("a docs start must not write a marker, got %q", got)
	}
	if !w.hasNote("not recorded as tested") || !w.hasNote("`ensign run "+authTask+"`") || w.hasNote("--start test") {
		t.Errorf("the note should advise a full run: %v", w.state().Notes)
	}
}

func TestDocsStartAfterAFailedRunWithoutTestStageCannotPublish(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	w.docsBinding(`{ paths: [docs] }`)
	w.noTestStage()
	mainBefore := remoteMain(t, remote)
	lead := w.startLead()
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-q", "-am", "work")
	w.failRun(lead)

	lead = w.startLead("--start", "docs")
	w.docsWith(lead, "updated", "docs/a.md", func() { write(t, filepath.Join(ws, "docs", "a.md"), "doc\n") })
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := w.markerSha(); got != "" {
		t.Fatalf("the docs run must not write a marker, got %q", got)
	}
	if _, _, _, err := tested(w.bridge, authTask, ""); err == nil {
		t.Error("tested() must not accept the failed run's commits")
	}
	for _, step := range []string{"merge", "pull-request"} {
		if _, err := w.land(step); err == nil {
			t.Errorf("%s should be refused", step)
		}
	}
	if got := remoteMain(t, remote); got != mainBefore {
		t.Errorf("nothing may be pushed to main: %s", got)
	}
}

func TestFullRunWithoutTestStageStillRecordsAndPublishes(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	w.noTestStage()
	lead := w.startLead()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-qam", "work")
	w.stop(coder, "coder", coderReport("compiled", "lib.txt"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got, head := w.markerSha(), runGit(t, ws, "rev-parse", "HEAD"); got != head {
		t.Errorf("a full run records HEAD %s, got %q", head, got)
	}
	w.wantLands(remote)
}
