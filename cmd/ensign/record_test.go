// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setRecord puts a record: line on the project, and on the task's frontmatter when task is not empty.
func (w *world) setRecord(project, task string) {
	w.t.Helper()
	if project != "" {
		w.project("record: " + project + "\n")
	}
	if task != "" {
		f := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
		data, _ := os.ReadFile(f)
		write(w.t, f, strings.Replace(string(data), "pipeline: coding\n", "pipeline: coding\nrecord: "+task+"\n", 1))
	}
}

func TestRecordSetting(t *testing.T) {
	const p4 = "{ kind: p4, client: supernova-ws }"
	cases := []struct {
		name, vcs, project, task string
		want                     string // the mode, or text the error contains
		fails                    bool
	}{
		{"git default", "", "", "", "commit", false},
		{"p4 default", p4, "", "", "shelve", false},
		{"project none", "", "none", "", "none", false},
		{"task overrides project", "", "none", "commit", "commit", false},
		{"task none over project commit", p4, "commit", "none", "none", false},
		{"p4 commit", p4, "commit", "", "commit", false},
		{"bad project value", "", "stash", "", filepath.Join("supernova", "project.yaml") + ": record must be commit, shelve or none", true},
		{"bad task value", "", "", "stash", "task.md: record must be commit, shelve or none", true},
		{"git shelve in project", "", "shelve", "", filepath.Join("supernova", "project.yaml") + ": record: shelve is not available for a git task", true},
		{"git shelve in task", "", "", "shelve", "task.md: record: shelve is not available for a git task", true},
		{"git shelve overridden", "", "shelve", "none", "none", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var w *world
			if c.vcs == "" {
				w = newWorld(t)
			} else {
				w = p4World(t, c.vcs)
			}
			w.setRecord(c.project, c.task)
			cfg, err := loadConfig(w.bridge, "supernova/auth-refactor")
			if c.fails {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("want an error with %q, got %v", c.want, err)
				}
				return
			}
			if err != nil || cfg.Record != c.want {
				t.Fatalf("record = %q, %v; want %q", cfg.Record, err, c.want)
			}
		})
	}
}

func TestRecordIsRefusedInBridgeYAML(t *testing.T) {
	w := newWorld(t)
	f := filepath.Join(w.bridge, "bridge.yaml")
	data, _ := os.ReadFile(f)
	write(t, f, string(data)+"record: none\n")
	_, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err == nil || !strings.Contains(err.Error(), "bridge.yaml: record: is a project setting") {
		t.Fatalf("got %v", err)
	}
}

func TestGitRecordNoneLeavesTheWorkUncommitted(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.setRecord("none", "")
	w.project("use: [git-catalog]\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "one\ntwo\n")
	write(t, filepath.Join(ws, "extra.txt"), "unreported\n")
	if r := w.stop(coder, "coder", coderReport("compiled", "lib.txt"), true); r != "" {
		t.Fatalf("blocked: %s", r)
	}
	if got := runGit(t, ws, "log", "--format=%s"); got != "init" {
		t.Errorf("record: none must commit nothing, log: %q", got)
	}
	if got := runGit(t, ws, "status", "--porcelain"); !strings.Contains(got, "lib.txt") {
		t.Errorf("the work should stay in the workspace: %q", got)
	}
	if st := w.state(); st.Status == "error" {
		t.Errorf("an unreported file must not fail a pass that commits nothing: %s", st.Reason)
	}
}

func TestGitRecordCommitStillCommits(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.setRecord("commit", "")
	w.project("use: [git-catalog]\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "one\ntwo\n")
	w.stop(coder, "coder", coderReport("compiled", "lib.txt"), true)
	if got := runGit(t, ws, "log", "--format=%s", "-1"); !strings.HasPrefix(got, "wip(c1 p1)") {
		t.Errorf("record: commit commits the pass: %q", got)
	}
}

func TestGithubRecordNoneWritesNoMarkerAndPublishRefuses(t *testing.T) {
	w, ws, _, gh := githubWorld(t, "success", localTest)
	w.setRecord("none", "")
	lead := w.startLead()
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the run should pass: %s", why)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := runGit(t, ws, "log", "--format=%s", "main..HEAD"); got != "" {
		t.Errorf("record: none commits nothing: %q", got)
	}
	if d, _ := dirty(ws); !d {
		t.Error("the work should be left uncommitted")
	}
	if _, err := os.Stat(testedFile(w.bridge, "supernova/auth-refactor", "")); err == nil {
		t.Error("no tested marker may be written")
	}
	for _, step := range []string{"merge", "pull-request"} {
		if _, err := w.land(step); err == nil || !strings.Contains(err.Error(), "record: none") {
			t.Errorf("%s should be refused for record: none: %v", step, err)
		}
	}
	if err := noRecordRefusal(w.bridge, "supernova/auth-refactor"); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("the publish step's check: %v", err)
	}
	if gh.index("pr create") >= 0 {
		t.Errorf("nothing may be opened: %v", gh.calls)
	}
}

// p4Recorded runs a Perforce task whose changelist 101 holds the given open files (OPENFILES syntax) to the end of a passing run, and returns the p4 commands it ran.
func p4Recorded(t *testing.T, w *world, open string) (*State, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "p4.log")
	t.Setenv("STARSHIP_TEST_P4_LOG", log)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_OPENFILES", open)
	lead, why := w.runStart()
	if lead == "" {
		t.Fatalf("a Perforce run must get past run.start: %s", why)
	}
	w.passRun(lead)
	data, _ := os.ReadFile(log)
	return w.state(), string(data)
}

const p4Task = "{ kind: p4, client: supernova-ws, changelist: '101' }"

func TestP4RecordModes(t *testing.T) {
	cases := []struct {
		name, record, want string
		notWant            []string
	}{
		{"default shelves", "", "shelve -f -c 101", []string{"submit"}},
		{"shelve", "shelve", "shelve -f -c 101", []string{"submit"}},
		{"commit", "commit", "submit -c 101", []string{"shelve"}},
		{"none", "none", "", []string{"submit", "shelve"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := p4World(t, p4Task)
			w.setRecord(c.record, "")
			st, log := p4Recorded(t, w, "a.go:101")
			if st.Status != "success" {
				t.Fatalf("status %s (%s)", st.Status, st.Reason)
			}
			if c.want != "" && !strings.Contains(log, c.want) {
				t.Errorf("p4 log lacks %q:\n%s", c.want, log)
			}
			for _, n := range c.notWant {
				if strings.Contains(log, n) {
					t.Errorf("p4 log has %q:\n%s", n, log)
				}
			}
		})
	}
}

func TestP4TaskWithGitCatalogGetsPastRunStartAndRecords(t *testing.T) {
	cases := []struct {
		name, record, want, notWant string
	}{
		{"default shelves", "", "shelve -f -c 101", "submit"},
		{"commit", "commit", "submit -c 101", "shelve"},
	}
	for _, dirty := range []bool{false, true} {
		for _, c := range cases {
			name := c.name + ", not a git repository"
			if dirty {
				name = c.name + ", dirty git repository"
			}
			t.Run(name, func(t *testing.T) {
				w := p4World(t, p4Task)
				if dirty {
					needGit(t)
					write(t, filepath.Join(dirtyWorkspace(t, w), "b.go"), "package b\n")
				}
				w.project("use: [git-catalog]\n")
				w.setRecord(c.record, "")
				log := filepath.Join(t.TempDir(), "p4.log")
				t.Setenv("STARSHIP_TEST_P4_LOG", log)
				t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
				t.Setenv("STARSHIP_TEST_P4_OPENFILES", "a.go:101")
				lead, why := w.runStart()
				if lead == "" {
					t.Fatalf("git-catalog must not refuse a Perforce run at run.start: %s", why)
				}
				coder, why := w.spawn(lead, "orchestrator", "coder")
				if coder == "" {
					t.Fatal(why)
				}
				if r := w.stop(coder, "coder", coderReport("compiled", "a.go"), false); r != "" {
					t.Fatalf("coder stop was blocked: %s", r)
				}
				w.gateMet(w.runOfAgent(lead))
				w.leaf(lead, "reviewer", "clean")
				w.leaf(lead, "reviewer", "clean")
				w.leaf(lead, "style-reviewer", "done")
				w.leaf(lead, "tester", "pass")
				if st := w.state(); st.Status != "success" {
					t.Fatalf("status %s (%s)", st.Status, st.Reason)
				}
				data, _ := os.ReadFile(log)
				if !strings.Contains(string(data), c.want) || strings.Contains(string(data), c.notWant) {
					t.Errorf("p4 log should have %q and not %q:\n%s", c.want, c.notWant, data)
				}
			})
		}
	}
}

func TestP4RecordBindingReplacesTheDefault(t *testing.T) {
	w := p4World(t, p4Task)
	w.setRecord("commit", "")
	out := filepath.Join(t.TempDir(), "recorded")
	w.binding(`record: { command: "echo $STARSHIP_RECORD $STARSHIP_CHANGELIST $STARSHIP_CLIENT > ` + filepath.ToSlash(out) + `" }`)
	st, log := p4Recorded(t, w, "a.go:101")
	if st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if strings.Contains(log, "submit") || strings.Contains(log, "shelve") {
		t.Errorf("the binding replaces p4 submit:\n%s", log)
	}
	if data, _ := os.ReadFile(out); strings.TrimSpace(string(data)) != "commit 101 supernova-ws" {
		t.Errorf("binding saw %q", data)
	}
}

func TestP4RecordWithNothingOpen(t *testing.T) {
	w := p4World(t, p4Task)
	w.setRecord("commit", "")
	st, log := p4Recorded(t, w, "")
	if st.Status != "success" || strings.Contains(log, "submit") {
		t.Fatalf("status %s (%s), log:\n%s", st.Status, st.Reason, log)
	}
	if !w.hasNote("has no open files, so nothing was recorded") {
		t.Errorf("notes: %v", st.Notes)
	}
}

func TestP4RecordFailureFailsTheRun(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_RECORD_FAIL", "no permission")
	w := p4World(t, p4Task)
	w.setRecord("commit", "")
	st, _ := p4Recorded(t, w, "a.go:101")
	if st.Status != "error" || !strings.Contains(st.Reason, "could not record changelist 101") || !strings.Contains(st.Reason, "no permission") {
		t.Errorf("status %s (%s)", st.Status, st.Reason)
	}
}

func TestP4ReviewDispatchNamesTheChangeFile(t *testing.T) {
	w := p4World(t, p4Task)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_OPENFILES", "a.go:101 b.go:101:add c.go:101:delete")
	write(t, filepath.Join(w.workspace(), "b.go"), "package b\n")
	lead, why := w.runStart()
	if lead == "" {
		t.Fatal(why)
	}
	w.leaf(lead, "coder", "compiled")
	id, why := w.spawn(lead, "orchestrator", "reviewer")
	if id == "" {
		t.Fatal(why)
	}
	prompt := w.decodedPromptOf(id)
	file := filepath.Join(runsDir(w.bridge), w.state().Run, "reports", "change-")
	i := strings.Index(prompt, file)
	if i < 0 {
		t.Fatalf("the review dispatch should name the change file:\n%s", prompt)
	}
	name := strings.Fields(prompt[i:])[0]
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+new", "+++ //supernova/main/b.go", "+package b", "deleted: //supernova/main/c.go"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the diff lacks %q:\n%s", want, data)
		}
	}
}

func TestP4ReviewDiffFallsBackToTheShelf(t *testing.T) {
	w := p4World(t, p4Task)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	r := &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Task: "supernova/auth-refactor", Workspace: w.workspace()}, State: &State{}}
	file := filepath.Join(r.Dir, "change.diff")
	what, err := writeP4Change(r, TaskVcs{Kind: "p4", Client: "supernova-ws", Changelist: "101"}, file)
	if err != nil || what != "the shelf" {
		t.Fatalf("%q, %v", what, err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "+shelved line") {
		t.Errorf("the shelf's diff: %s", data)
	}
}

func TestGitReviewDispatchNamesTheDiffCommand(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	lead := w.startLead()
	write(t, filepath.Join(ws, "a.txt"), "new\n")
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	if r := w.stop(coder, "coder", coderReport("compiled", "a.txt"), false); r != "" {
		t.Fatalf("coder stop was blocked: %s", r)
	}
	id, why := w.spawn(lead, "orchestrator", "reviewer")
	if id == "" {
		t.Fatal(why)
	}
	if prompt := w.decodedPromptOf(id); !strings.Contains(prompt, "`git diff "+w.state().Base+"`") {
		t.Errorf("the review dispatch should name the diff command:\n%s", prompt)
	}
}

func TestGitReviewLineListsUntrackedFiles(t *testing.T) {
	needGit(t)
	for _, record := range []string{"commit", "none"} {
		w := newWorld(t)
		ws := w.gitWorkspace()
		w.setRecord(record, "")
		w.project("use: [git-catalog]\n")
		lead := w.startLead()
		coder, why := w.spawn(lead, "orchestrator", "coder")
		if coder == "" {
			t.Fatal(why)
		}
		write(t, filepath.Join(ws, "lib.txt"), "one\n")
		if r := w.stop(coder, "coder", coderReport("compiled", "lib.txt"), false); r != "" {
			t.Fatalf("coder stop was blocked: %s", r)
		}
		write(t, filepath.Join(ws, "fresh.txt"), "new\n")
		id, why := w.spawn(lead, "orchestrator", "reviewer")
		if id == "" {
			t.Fatal(why)
		}
		if prompt := w.decodedPromptOf(id); !strings.Contains(prompt, "fresh.txt") || !strings.Contains(prompt, "untracked") {
			t.Errorf("record: %s: the review line should list the untracked file:\n%s", record, prompt)
		}
	}
}

func TestP4ReviewDiffHasNoReconcilePreview(t *testing.T) {
	w := p4World(t, p4Task)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_OPENFILES", "a.go:101")
	log := filepath.Join(t.TempDir(), "p4.log")
	t.Setenv("STARSHIP_TEST_P4_LOG", log)
	write(t, filepath.Join(w.workspace(), "a.go"), "package a\n")
	write(t, filepath.Join(w.workspace(), "shell.go"), "package shell\n")
	r := &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Task: "supernova/auth-refactor", Workspace: w.workspace()}, State: &State{}}
	file := filepath.Join(r.Dir, "change.diff")
	what, err := writeP4Change(r, TaskVcs{Kind: "p4", Client: "supernova-ws", Changelist: "101"}, file)
	if err != nil || what != "the open files" {
		t.Fatalf("%q, %v", what, err)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), "shell.go") {
		t.Errorf("a file nothing opened is not in the change:\n%s", data)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "diff -f") || strings.Contains(string(calls), "reconcile") {
		t.Errorf("the change file must not preview a reconcile:\n%s", calls)
	}
}

func TestP4ReviewDiffReadsAnEscapedDepotPathOnce(t *testing.T) {
	w := p4World(t, p4Task)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_OPENFILES", "a%40b.md:101:add")
	write(t, filepath.Join(w.workspace(), "a@b.md"), "hello\n")
	r := &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Task: "supernova/auth-refactor", Workspace: w.workspace()}, State: &State{}}
	file := filepath.Join(r.Dir, "change.diff")
	if _, err := writeP4Change(r, TaskVcs{Kind: "p4", Client: "supernova-ws", Changelist: "101"}, file); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "+hello") {
		t.Errorf("the diff: %s", data)
	}
}

func TestGithubCIRefusesRecordNone(t *testing.T) {
	r := &RunFiles{Snap: &Snapshot{Task: "supernova/auth-refactor", Record: "none"}}
	if _, err := githubCI(r, time.Now().Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "record: none") {
		t.Errorf("got %v", err)
	}
}
