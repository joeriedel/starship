// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workspace returns the fixture task's workspace, made a git repo with one commit.
func (w *world) gitWorkspace() string {
	w.t.Helper()
	ws := filepath.Join(filepath.Dir(w.bridge), "workspace")
	runGit(w.t, ws, "init", "-q", "-b", "main")
	runGit(w.t, ws, "config", "user.email", "t@example.com")
	runGit(w.t, ws, "config", "user.name", "t")
	write(w.t, filepath.Join(ws, "lib.txt"), "one\n")
	runGit(w.t, ws, "add", "lib.txt")
	runGit(w.t, ws, "commit", "-q", "-m", "init")
	return ws
}

// project appends YAML to the fixture project's config.
func (w *world) project(yaml string) {
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(w.t, p, string(data)+yaml)
}

func coderReport(outcome, files string) string {
	return "Done.\n\n```starship-report\noutcome: " + outcome + "\nsummary: added two\nfiles: " + files + "\n```\n"
}

func TestGitCatalog(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	lead := w.startLead()

	coder, _ := w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "one\ntwo\n")
	if r := w.stop(coder, "coder", report("compiled"), false); !strings.Contains(r, "no files line") {
		t.Fatalf("a coder report without files should be blocked: %q", r)
	}
	if r := w.stop(coder, "coder", coderReport("compiled", filepath.Join(ws, "lib.txt")), true); r != "" {
		t.Fatalf("blocked: %s", r)
	}
	log := runGit(t, ws, "log", "--format=%s", "-1")
	if !strings.HasPrefix(log, "wip(c1 p1): added two") {
		t.Fatalf("catalog commit subject: %q", log)
	}
	if body := runGit(t, ws, "log", "--format=%B", "-1"); strings.Contains(strings.ToLower(body), "co-authored") {
		t.Fatalf("attribution in a catalog commit: %q", body)
	}

	// The orchestrator's prompt carried the extension's augment.
	st := w.state()
	if st.Base == "" {
		t.Fatalf("no base recorded")
	}

	// Review finds something; the next pass leaves a stray file behind.
	w.leaf(lead, "reviewer", "findings")
	w.leaf(lead, "reviewer", "clean")
	coder, _ = w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "one\ntwo\nthree\n")
	write(t, filepath.Join(ws, "stray.txt"), "oops\n")
	w.stop(coder, "coder", coderReport("broken", "lib.txt"), false)
	if st := w.state(); st.Status != "error" || !strings.Contains(st.Reason, "stray.txt") {
		t.Fatalf("a stray file should end the run: %s %s", st.Status, st.Reason)
	}
}

func TestCatalogRefusesADirtyWorkspace(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	write(t, filepath.Join(ws, "lib.txt"), "someone's edit\n")
	w.run("supernova/auth-refactor")
	w.approve()
	if id, why := w.spawn("", "", "orchestrator"); id != "" || !strings.Contains(why, "uncommitted changes") {
		t.Fatalf("lead on a dirty workspace: %q %q", id, why)
	}
	if st := w.state(); st.Status != "error" {
		t.Fatalf("status %s", st.Status)
	}
}

func TestSquash(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	lead := w.startLead()
	for i, text := range []string{"two", "three"} {
		coder, _ := w.spawn(lead, "orchestrator", "coder")
		write(t, filepath.Join(ws, "lib.txt"), "one\n"+text+"\n")
		w.stop(coder, "coder", coderReport("broken", "lib.txt"), false)
		if st := w.state(); st.Scopes["run"].Stage != "code" {
			t.Fatalf("pass %d: at %s", i, st.Scopes["run"].Stage)
		}
	}
	coder, _ := w.spawn(lead, "orchestrator", "coder") // third broken pass ends the run
	w.stop(coder, "coder", coderReport("broken", "lib.txt"), false)
	var out bytes.Buffer
	if err := cmdCatalog([]string{"squash", "supernova/auth-refactor", "-m", "Add a line"}, &out); err != nil {
		t.Fatal(err)
	}
	if n := runGit(t, ws, "rev-list", "--count", "HEAD"); n != "2" {
		t.Fatalf("expected init plus one squashed commit, got %s", n)
	}
	if s := runGit(t, ws, "log", "--format=%s", "-1"); s != "Add a line" {
		t.Fatalf("squash subject %q", s)
	}
}

func TestHandlersAndAugments(t *testing.T) {
	w := newWorld(t)
	ws := filepath.Join(filepath.Dir(w.bridge), "workspace")
	write(t, filepath.Join(w.bridge, "projects/supernova/note.md"), "PROJECT-NOTE\n")
	w.project(`events:
  stage.before:code: [{ augment: note.md }, { observe: "exit 3" }]
  stage.after:style: [{ require: "echo styled > styled.txt" }]
  run.finish:success: [{ observe: "echo ok > finished.txt" }]
`)
	lead := w.startLead()
	in := map[string]any{"tool_name": "Agent", "agent_id": lead, "agent_type": "orchestrator",
		"tool_input": map[string]any{"subagent_type": "coder", "prompt": "x"}}
	res, _ := w.hook("PreToolUse", in)
	prompt := res["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["prompt"].(string)
	if !strings.Contains(prompt, "PROJECT-NOTE") {
		t.Fatalf("augment missing from the coder's prompt")
	}
	w.agents++
	rec, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
	write(t, agentTranscript(w.session, "agent02"), string(rec)+"\n")
	w.stop("agent02", "coder", report("compiled"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	if _, err := os.Stat(filepath.Join(ws, "styled.txt")); err != nil {
		t.Errorf("stage.after:style require did not run")
	}
	w.leaf(lead, "tester", "pass")
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s %s", st.Status, st.Reason)
	}
	if _, err := os.Stat(filepath.Join(ws, "finished.txt")); err != nil {
		t.Errorf("run.finish:success observe did not run")
	}
	if hist := historyText(w.state()); !strings.Contains(hist, "observe stage.before:code: exit 3 failed") {
		t.Errorf("a failing observe should be logged:\n%s", hist)
	}
}

func historyText(st *State) string {
	var b strings.Builder
	for _, h := range st.History {
		b.WriteString(h.Kind + " " + h.Stage + " " + h.Detail + "\n")
	}
	return b.String()
}

func TestRequireFailureEndsTheRun(t *testing.T) {
	w := newWorld(t)
	w.project("events:\n  stage.before:review: [{ require: \"echo nope; exit 1\" }]\n")
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	if id, why := w.spawn(lead, "orchestrator", "reviewer"); id != "" || !strings.Contains(why, "could not start") {
		t.Fatalf("reviewer after a failed require: %q %q", id, why)
	}
	if st := w.state(); st.Status != "error" || !strings.HasPrefix(st.Reason, "handler") || !strings.Contains(st.Reason, "nope") {
		t.Fatalf("status %s reason %s", st.Status, st.Reason)
	}
}

func TestDoneCheck(t *testing.T) {
	w := newWorld(t)
	w.project("  done: { command: \"test -f DONE\" }\n")
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	// move the appended binding under bindings:
	s := strings.Replace(string(data), "  done: { command: \"test -f DONE\" }\n", "", 1)
	s = strings.Replace(s, "bindings:\n", "bindings:\n  done: { command: \"test -f DONE\" }\n", 1)
	write(t, p, s)
	lead := w.startLead("--start", "test")
	w.leaf(lead, "tester", "pass")
	if st := w.state(); st.Status != "error" || !strings.HasPrefix(st.Reason, "done-check") {
		t.Fatalf("a failing @done should turn success into an error: %s %s", st.Status, st.Reason)
	}
}

func TestAskUserAndResume(t *testing.T) {
	w := newWorld(t)
	w.project(`  coding2: {}
`)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "      - set:    { budgets.test: 2 }\n",
		"      - set:    { budgets.test: 1 }\n      - set:    { on-exhausted.test: { ask-user: \"another cycle?\" } }\n", 1))
	lead := w.startLead("--start", "test")
	w.leaf(lead, "tester", "fail") // spends test 1/1, back to code
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "fail") // test budget exhausted → ask
	st := w.state()
	if st.Status != "ask-user" || st.Reason != "another cycle?" || st.Resume == nil || st.Resume.Stage != "code" {
		t.Fatalf("expected ask-user resuming at code: %s %q %+v", st.Status, st.Reason, st.Resume)
	}
	if r := w.stop(lead, "orchestrator", "stopping", false); r != "" {
		t.Fatalf("the lead should be allowed to stop while the run waits: %s", r)
	}
	var out bytes.Buffer
	if err := cmdResume([]string{"--answer", "yes, one more"}, &out); err != nil {
		t.Fatal(err)
	}
	st = w.state()
	if st.Status != "running" || st.Scopes["run"].Stage != "code" || st.Spent["test"] != 2 || st.Spent["loop"] != 1 || st.Extra["test"] != 1 {
		t.Fatalf("after resume: %s at %s, spent %v extra %v", st.Status, st.Scopes["run"].Stage, st.Spent, st.Extra)
	}
	lead2, why := w.spawn("", "", "orchestrator")
	if lead2 == "" {
		t.Fatalf("a new lead after resume was refused: %s", why)
	}
	prompt, _ := os.ReadFile(agentTranscript(w.session, lead2))
	if !strings.Contains(string(prompt), "yes, one more") {
		t.Errorf("the resumed lead was not told the answer")
	}
	w.leaf(lead2, "coder", "compiled")
	if st := w.state(); st.Scopes["run"].Stage != "review" {
		t.Fatalf("resumed run did not continue: at %s", st.Scopes["run"].Stage)
	}
}

func TestSync(t *testing.T) {
	w := newWorld(t)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "bindings:\n", "bindings:\n  sync: { command: \"echo synced > synced.txt\" }\n", 1))
	w.startLead("--sync", "--start", "test")
	ws := filepath.Join(filepath.Dir(w.bridge), "workspace")
	if _, err := os.Stat(filepath.Join(ws, "synced.txt")); err != nil {
		t.Fatalf("@sync did not run at run start")
	}
}

func TestJournals(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, filepath.Dir(remote), "init", "-q", "--bare", "-b", "main", remote)
	runGit(t, w.bridge, "init", "-q", "-b", "main")
	runGit(t, w.bridge, "config", "user.email", "t@example.com")
	runGit(t, w.bridge, "config", "user.name", "t")
	write(t, filepath.Join(w.bridge, ".gitignore"), ".starship/\n")
	runGit(t, w.bridge, "add", "-A")
	runGit(t, w.bridge, "commit", "-q", "-m", "bridge")
	runGit(t, w.bridge, "remote", "add", "origin", remote)
	runGit(t, w.bridge, "push", "-q", "-u", "origin", "main")

	stop := func(active bool) string {
		res, code := w.hook("Stop", map[string]any{"stop_hook_active": active})
		if code != 0 {
			t.Fatalf("Stop exited %d", code)
		}
		if res == nil {
			return ""
		}
		return res["reason"].(string)
	}
	if r := stop(false); r != "" {
		t.Fatalf("a clean bridge should not block: %s", r)
	}
	task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, string(data)+"\n- progress\n")
	if r := stop(false); !strings.Contains(r, "task.md") {
		t.Fatalf("an uncommitted journal should block: %q", r)
	}
	if r := stop(true); r != "" {
		t.Fatalf("a second block would trap the session: %q", r)
	}
	var out bytes.Buffer
	if err := cmdJournal([]string{"commit", "-m", "Log progress"}, &out); err != nil {
		t.Fatalf("journal commit: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "pushed") {
		t.Fatalf("journal commit did not push:\n%s", out.String())
	}
	if r := stop(false); r != "" {
		t.Fatalf("after commit and push: %q", r)
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "Log progress" {
		t.Fatalf("remote has %q", got)
	}
	history := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md")
	write(t, history, "# auth-refactor: history\n\n- run.\n")
	if r := stop(false); !strings.Contains(r, "history.md") {
		t.Fatalf("an uncommitted history.md should block: %q", r)
	}
	if err := cmdJournal([]string{"commit", "-m", "Add history"}, &out); err != nil {
		t.Fatalf("journal commit: %v", err)
	}
	if r := stop(false); r != "" {
		t.Fatalf("after committing history.md: %q", r)
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "Add history" {
		t.Fatalf("remote has %q", got)
	}

	// A project can turn journal commits off.
	write(t, task, string(data)+"\n- more\n")
	w.project("journal: { commit: off, reason: kept elsewhere }\n")
	if r := stop(false); strings.Contains(r, "task.md") {
		t.Fatalf("journal commits are off for this project: %q", r)
	}
}

func TestJournalRefusesRunCitations(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	runGit(t, w.bridge, "init", "-q", "-b", "main")
	runGit(t, w.bridge, "config", "user.email", "t@example.com")
	runGit(t, w.bridge, "config", "user.name", "t")
	runGit(t, w.bridge, "config", "diff.external", "true")
	write(t, filepath.Join(w.bridge, ".gitignore"), ".starship/\n")
	runGit(t, w.bridge, "add", "-A")
	runGit(t, w.bridge, "commit", "-q", "-m", "bridge")
	head := runGit(t, w.bridge, "rev-parse", "HEAD")
	dir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	task := filepath.Join(dir, "task.md")
	history := filepath.Join(dir, "history.md")
	other := filepath.Join(w.bridge, "projects/supernova/notes.md")
	base, _ := os.ReadFile(task)

	for _, cite := range []string{
		"saw it in .starship/runs/20261030-115944-supernova-fix/reports/d3.log",
		"see `reports/d12.log`",
		"(checked, d6.log)",
		"see .starship\\runs\\20261030-115944-supernova-fix\\scratch",
	} {
		write(t, task, string(base)+"\n- fine\r\n- "+cite+"\r\n")
		write(t, other, "d9.log\n")
		var out bytes.Buffer
		err := cmdJournal([]string{"commit", "-m", "Cite"}, &out)
		if err == nil || !strings.Contains(err.Error(), "projects/supernova/tasks/auth-refactor/task.md:") {
			t.Fatalf("%q should be refused with file and line: %v", cite, err)
		}
		if strings.Contains(err.Error(), "notes.md") {
			t.Fatalf("a non-journal file was scanned: %v", err)
		}
		if got := runGit(t, w.bridge, "rev-parse", "HEAD"); got != head {
			t.Fatalf("a refused journal was committed")
		}
	}

	// A citation committed earlier is not re-flagged; only newly added lines count.
	old := string(base) + "\n- old see d12.log\n"
	write(t, task, old)
	runGit(t, w.bridge, "add", "-A")
	runGit(t, w.bridge, "commit", "-q", "-m", "old citation")
	write(t, task, old+"- unrelated\n")
	var out0 bytes.Buffer
	if err := cmdJournal([]string{"commit", "-m", "Unrelated"}, &out0); err != nil {
		t.Fatalf("an old citation should not block an unrelated line: %v", err)
	}
	head = runGit(t, w.bridge, "rev-parse", "HEAD")
	line := strings.Count(old+"- unrelated\n\n", "\n") + 1
	write(t, task, old+"- unrelated\n\n- new d3.log\n")
	err := cmdJournal([]string{"commit", "-m", "New"}, &out0)
	if want := fmt.Sprintf("task.md:%d)", line); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the appended citation should be refused at %s: %v", want, err)
	}

	// A directory argument is expanded to the files git would commit.
	err = cmdJournal([]string{"commit", "-m", "Dir", "projects/supernova/tasks/auth-refactor"}, &out0)
	if err == nil || !strings.Contains(err.Error(), "task.md:") {
		t.Fatalf("a directory argument should be scanned: %v", err)
	}
	if got := runGit(t, w.bridge, "rev-parse", "HEAD"); got != head {
		t.Fatalf("a refused directory commit was committed")
	}

	// An added line that looks like a "+++" header is still content.
	committed := old + "- unrelated\n"
	first := strings.Count(committed, "\n") + 1
	write(t, task, committed+"++ d12.log\nb d3.log\n")
	err = cmdJournal([]string{"commit", "-m", "Plus"}, &out0)
	for _, want := range []string{fmt.Sprintf("task.md:%d,", first), fmt.Sprintf("task.md:%d)", first+1)} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %s in the refusal: %v", want, err)
		}
	}

	write(t, task, string(base)+"\n- under .starship/runs/<id> and .starship/runs/, bad12.log; run 20261030-224308-supernova-fix\n")
	write(t, history, "# auth-refactor: history\n\n- **2026-10-30, run 20261030-224308-supernova-fix (default): success.**\n")
	var out bytes.Buffer
	if err := cmdJournal([]string{"commit", "-m", "Generic"}, &out); err != nil {
		t.Fatalf("generic text should commit: %v", err)
	}
	if got := runGit(t, w.bridge, "rev-parse", "HEAD"); got == head {
		t.Fatalf("nothing was committed")
	}
}

func TestRunIsLoggedInTheJournal(t *testing.T) {
	w := newWorld(t)
	ws := w.gitWorkspace()
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	write(t, filepath.Join(ws, "new.txt"), "new\n")
	w.stop(coder, "coder", report("compiled"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
	st := w.state()
	if st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	dir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	data, _ := os.ReadFile(filepath.Join(dir, "history.md"))
	history := string(data)
	want := "run " + st.Run + " (coding): success.** 1 coder pass. Changed: lib.txt, new.txt. Started at "
	if !strings.Contains(history, want) || !strings.Contains(history, "nothing committed.") {
		t.Fatalf("history.md has no entry for the run:\n%s", history)
	}
	if !strings.HasPrefix(history, "# auth-refactor: history\n\n- ") {
		t.Errorf("a new history.md should start with its heading:\n%s", history)
	}
	if task, _ := os.ReadFile(filepath.Join(dir, "task.md")); strings.Contains(string(task), st.Run) {
		t.Errorf("task.md should not carry the run's line")
	}
}

func TestAppendHistory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tasks", "t", "history.md")
	if err := appendHistory(f, "t", "- new."); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "# t: history\n\n- new.\n" {
		t.Errorf("a missing file is created with its heading:\n%q", got)
	}
	write(t, f, "# t: history\n\n- old.\n")
	if err := appendHistory(f, "t", "- new."); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "# t: history\n\n- old.\n\n- new.\n" {
		t.Errorf("an LF file:\n%q", got)
	}
	write(t, f, "# t: history\r\n\r\n- old.\r\n")
	if err := appendHistory(f, "t", "- new."); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "# t: history\r\n\r\n- old.\r\n\r\n- new.\r\n" {
		t.Errorf("a CRLF file:\n%q", got)
	}
}

func TestInsertProgress(t *testing.T) {
	doc := "# t\n\n## Progress log\n\n- one.\n\n## Outstanding\n\n- x\n"
	if got, want := insertProgress(doc, "- two."), "# t\n\n## Progress log\n\n- one.\n- two.\n\n## Outstanding\n\n- x\n"; got != want {
		t.Errorf("LF:\n%q", got)
	}
	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if got, want := insertProgress(crlf, "- two."), strings.ReplaceAll("# t\n\n## Progress log\n\n- one.\n- two.\n\n## Outstanding\n\n- x\n", "\n", "\r\n"); got != want {
		t.Errorf("CRLF:\n%q", got)
	}
}

func TestLogRunBeforeSplit(t *testing.T) {
	bridge := t.TempDir()
	dir := filepath.Join(bridge, "projects", "p", "tasks", "t")
	write(t, filepath.Join(dir, "task.md"), "# t\n\n## Progress log\n\n- one.\n")
	r := &RunFiles{Snap: &Snapshot{Bridge: bridge, Task: "p/t", Pipeline: &Pipeline{Name: "x"}}, State: &State{}}
	if err := logRunInJournal(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "history.md")); err == nil {
		t.Errorf("history.md should not be created while task.md still has the log")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "task.md")); strings.Count(string(got), "\n- ") < 2 {
		t.Errorf("the run should be in task.md's log:\n%s", got)
	}
}

func TestCatalogRetryCommitsTheLeftoversItKept(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("use: [git-catalog]\n")
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "one\ntwo\n")
	write(t, filepath.Join(ws, "stray.txt"), "left\n")
	w.stop(coder, "coder", coderReport("broken", "lib.txt"), false)
	w.wantFailed("")
	w.retryStep("1")

	lead, _ = w.spawn("", "", "orchestrator")
	coder, _ = w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "other.txt"), "mine\n")
	if r := w.stop(coder, "coder", coderReport("compiled", "other.txt"), true); r != "" {
		t.Fatalf("the retry's first pass was blocked: %s", r)
	}
	if status := runGit(t, ws, "status", "--porcelain"); status != "" {
		t.Errorf("the leftovers should be committed: %q", status)
	}
}
