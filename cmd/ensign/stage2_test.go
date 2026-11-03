// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	if st.Status != "running" || st.Scopes["run"].Stage != "code" || st.Spent["test"] != 2 || st.Spent["coder-loop"] != 1 || st.Extra["test"] != 1 {
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

func TestRunEntryListsEachChangedFileOnce(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	ws := w.gitWorkspace()
	w.project("events:\n  run.start: [{ require: \"echo pre > pre.txt && git add pre.txt && git commit -q -m pre\" }]\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	if r := w.stop(coder, "coder", coderReport("compiled", filepath.Join(ws, "lib.txt")), false); r != "" {
		t.Fatalf("blocked: %s", r)
	}
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md"))
	entry := string(data)
	if !strings.Contains(entry, "Changed: lib.txt. ") {
		t.Fatalf("the run's own file should be listed once, relative, and nothing else:\n%s", entry)
	}
	if strings.Contains(entry, "pre.txt") {
		t.Fatalf("a file committed by run.start is not the run's change:\n%s", entry)
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
	doc := "# t\n\n## Progress log\n\n- one.\n"
	// The entry carries today's date, so accept the entry as of either side of the call in case it straddles midnight.
	before := insertProgress(doc, runEntry(r))
	if err := logRunInJournal(r); err != nil {
		t.Fatal(err)
	}
	after := insertProgress(doc, runEntry(r))
	if _, err := os.Stat(filepath.Join(dir, "history.md")); err == nil {
		t.Errorf("history.md should not be created while task.md still has the log")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "task.md")); string(got) != before && string(got) != after {
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

func TestRunRefusesAProjectWithoutPlatforms(t *testing.T) {
	w := newWorld(t)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "platforms: [linux]\n", "", 1))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdRun([]string{"supernova/auth-refactor"}, &out)
	if err == nil || !strings.Contains(err.Error(), `project "supernova" declares no platforms`) || !strings.Contains(err.Error(), "platforms: [linux, macos, windows]") {
		t.Fatalf("a project without platforms should be refused with the line to add: %v", err)
	}
}

func TestConfigRejectsBadPlatforms(t *testing.T) {
	w := newWorld(t)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	for platforms, want := range map[string]string{"[linux, \"\"]": "empty", "[linux, linux]": "twice", "[linux, \" linux\"]": "twice", "[linux, Linux]": "twice"} {
		write(t, p, strings.Replace(string(data), "platforms: [linux]", "platforms: "+platforms, 1))
		c, err := loadConfig(w.bridge, "supernova/auth-refactor")
		if err != nil {
			t.Fatal(err)
		}
		if errs := strings.Join(validate(c), "\n"); !strings.Contains(errs, want) {
			t.Errorf("platforms %s should be rejected as %q: %s", platforms, want, errs)
		}
	}
}

func TestDeclaredPlatformsReachLeadAndReviewerOnly(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead("--start", "review")
	prompt := func(agent string) string {
		data, err := os.ReadFile(agentTranscript(w.session, agent))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	const line = "Declared platforms: linux"
	if !strings.Contains(prompt(lead), line) {
		t.Errorf("the lead prompt should carry the declared platforms")
	}
	reviewer, why := w.spawn(lead, "orchestrator", "reviewer")
	if reviewer == "" {
		t.Fatalf("reviewer refused: %s", why)
	}
	if !strings.Contains(prompt(reviewer), line) {
		t.Errorf("a reviewer prompt should carry the declared platforms")
	}

	w2 := newWorld(t)
	lead2 := w2.startLead("--start", "code")
	coder, why := w2.spawn(lead2, "orchestrator", "coder")
	if coder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	data, _ := os.ReadFile(agentTranscript(w2.session, coder))
	if strings.Contains(string(data), "Declared platforms") {
		t.Errorf("a coder prompt should not carry the platforms")
	}
}

// journalFixture makes the world's bridge a git repo pushed to a bare remote and returns the remote.
func journalFixture(t *testing.T, w *world) string {
	t.Helper()
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
	return remote
}

func cloneRemote(t *testing.T, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other")
	runGit(t, filepath.Dir(dir), "clone", "-q", remote, dir)
	return dir
}

func TestJournalCommitsRename(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	runGit(t, w.bridge, "mv", "projects/supernova/tasks/auth-refactor", "projects/supernova/tasks/auth-rework")
	var out bytes.Buffer
	if err := cmdJournal([]string{"commit", "-m", "Rename task"}, &out); err != nil {
		t.Fatalf("journal commit: %v\n%s", err, out.String())
	}
	files := runGit(t, remote, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(files, "tasks/auth-refactor/") || !strings.Contains(files, "tasks/auth-rework/task.md") {
		t.Fatalf("remote files:\n%s", files)
	}
	if st := runGit(t, w.bridge, "status", "--porcelain", "--", "projects"); st != "" {
		t.Fatalf("status not clean:\n%s", st)
	}
}

func TestJournalCommitsExplicitDeletions(t *testing.T) {
	needGit(t)
	task := "projects/supernova/tasks/auth-refactor/task.md"
	for name, remove := range map[string]func(t *testing.T, bridge string){
		"worktree": func(t *testing.T, bridge string) { os.Remove(filepath.Join(bridge, filepath.FromSlash(task))) },
		"staged":   func(t *testing.T, bridge string) { runGit(t, bridge, "rm", "-q", task) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			remote := journalFixture(t, w)
			remove(t, w.bridge)
			var out bytes.Buffer
			if err := cmdJournal([]string{"commit", "-m", "Drop task", task}, &out); err != nil {
				t.Fatalf("journal commit: %v\n%s", err, out.String())
			}
			if files := runGit(t, remote, "ls-tree", "-r", "--name-only", "HEAD"); strings.Contains(files, task) {
				t.Fatalf("deletion not recorded:\n%s", files)
			}
		})
	}
}

func TestJournalPushIgnoresUnrelatedChanges(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, "elsewhere.txt"), "remote change\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "other")
	runGit(t, other, "push", "-q")

	gi := filepath.Join(w.bridge, ".gitignore")
	write(t, gi, ".starship/\n# local edit\n")
	history := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md")
	write(t, history, "# history\n")
	var out bytes.Buffer
	if err := cmdJournal([]string{"commit", "-m", "Add history"}, &out); err != nil {
		t.Fatalf("journal commit: %v\n%s", err, out.String())
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "Add history" {
		t.Fatalf("remote has %q", got)
	}
	if data, _ := os.ReadFile(gi); !strings.Contains(string(data), "# local edit") {
		t.Fatalf("unstaged modification lost: %q", data)
	}
}

func TestJournalPushStashConflict(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, ".gitignore"), "theirs\n")
	runGit(t, other, "commit", "-q", "-am", "theirs")
	runGit(t, other, "push", "-q")

	write(t, filepath.Join(w.bridge, ".gitignore"), "mine\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md"), "# history\n")
	var out bytes.Buffer
	err := cmdJournal([]string{"commit", "-m", "Add history"}, &out)
	if err == nil || !strings.Contains(err.Error(), ".gitignore") || !strings.Contains(err.Error(), "git stash") {
		t.Fatalf("want a stash conflict naming .gitignore, got %v", err)
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "Add history" {
		t.Fatalf("remote has %q", got)
	}
	if got := runGit(t, w.bridge, "diff", "--name-only", "--diff-filter=U"); got != "" {
		t.Fatalf("files still unmerged:\n%s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(w.bridge, ".gitignore")); !strings.Contains(string(data), "<<<<<<<") {
		t.Fatalf("no conflict markers: %q", data)
	}
	if runGit(t, w.bridge, "stash", "list") == "" {
		t.Fatal("stash was not kept")
	}
	runGit(t, w.bridge, "checkout", "--", ".gitignore")
	out.Reset()
	if err := cmdJournal([]string{"push"}, &out); err != nil {
		t.Fatalf("push after recovery: %v\n%s", err, out.String())
	}
}

func TestJournalPushStashDeletedByPushedVersion(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	rel := "projects/supernova/tasks/auth-refactor/task.md"
	other := cloneRemote(t, remote)
	runGit(t, other, "rm", "-q", rel)
	runGit(t, other, "commit", "-q", "-m", "delete task")
	runGit(t, other, "push", "-q")

	file := filepath.Join(w.bridge, filepath.FromSlash(rel))
	write(t, file, "mine\n")
	var out bytes.Buffer
	err := cmdJournal([]string{"push"}, &out)
	if err == nil || !strings.Contains(err.Error(), "pushed version deleted") || !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), "conflict markers") {
		t.Fatalf("want a note that %s was deleted by the pushed version, got %v", rel, err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("deleted file was left in the worktree")
	}
	if runGit(t, w.bridge, "stash", "list") == "" {
		t.Fatal("stash was not kept")
	}
	if got := runGit(t, w.bridge, "diff", "--name-only", "--diff-filter=U"); got != "" {
		t.Fatalf("files still unmerged:\n%s", got)
	}
	changes, err := journalChanges(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if strings.Contains(c, "task.md") {
			t.Fatalf("journalChanges offers the deleted file: %v", changes)
		}
	}
}

func TestJournalPushStashConflictNameWithSpace(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	name := "my notes.txt"
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, name), "base\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "notes")
	runGit(t, other, "push", "-q")
	runGit(t, w.bridge, "pull", "-q", "--ff-only")
	write(t, filepath.Join(other, name), "theirs\n")
	runGit(t, other, "commit", "-q", "-am", "theirs")
	runGit(t, other, "push", "-q")

	write(t, filepath.Join(w.bridge, name), "mine\n")
	var out bytes.Buffer
	err := cmdJournal([]string{"push"}, &out)
	if err == nil || !strings.Contains(err.Error(), "("+name+")") || !strings.Contains(err.Error(), "conflict markers") {
		t.Fatalf("want a stash conflict naming %q, got %v", name, err)
	}
	if got := runGit(t, w.bridge, "diff", "--name-only", "--diff-filter=U"); got != "" {
		t.Fatalf("files still unmerged:\n%s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(w.bridge, name)); !strings.Contains(string(data), "<<<<<<<") {
		t.Fatalf("no conflict markers: %q", data)
	}
}

func TestJournalPushRefusesUnmergedFiles(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	journalFixture(t, w)
	gi := filepath.Join(w.bridge, ".gitignore")
	runGit(t, w.bridge, "checkout", "-q", "-b", "side")
	write(t, gi, "side\n")
	runGit(t, w.bridge, "commit", "-q", "-am", "side")
	runGit(t, w.bridge, "checkout", "-q", "main")
	write(t, gi, "main\n")
	runGit(t, w.bridge, "commit", "-q", "-am", "main")
	merge := exec.Command("git", "-C", w.bridge, "merge", "side")
	if err := merge.Run(); err == nil {
		t.Fatal("merge should have conflicted")
	}
	var out bytes.Buffer
	err := cmdJournal([]string{"push"}, &out)
	if err == nil || !strings.Contains(err.Error(), ".gitignore") || strings.Contains(err.Error(), "another machine") {
		t.Fatalf("want an unmerged-files error naming .gitignore, got %v", err)
	}
}

func TestJournalPushRefusesUnmergedFilesWithoutRemoteBranch(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	journalFixture(t, w)
	runGit(t, w.bridge, "checkout", "-q", "-b", "fresh")
	notes := filepath.Join(w.bridge, "notes.md")
	runGit(t, w.bridge, "checkout", "-q", "-b", "side")
	write(t, notes, "side\n")
	runGit(t, w.bridge, "add", "notes.md")
	runGit(t, w.bridge, "commit", "-q", "-m", "side")
	runGit(t, w.bridge, "checkout", "-q", "fresh")
	write(t, notes, "fresh\n")
	runGit(t, w.bridge, "add", "notes.md")
	runGit(t, w.bridge, "commit", "-q", "-m", "fresh")
	merge := exec.Command("git", "-C", w.bridge, "merge", "side")
	if err := merge.Run(); err == nil {
		t.Fatal("merge should have conflicted")
	}
	var out bytes.Buffer
	err := cmdJournal([]string{"push"}, &out)
	if err == nil || !strings.Contains(err.Error(), "unmerged files") || !strings.Contains(err.Error(), "notes.md") {
		t.Fatalf("want an unmerged-files error naming notes.md, got %v", err)
	}
	if b, rerr := os.ReadFile(notes); rerr != nil || !strings.Contains(string(b), "fresh") {
		t.Fatalf("notes.md should keep its content, got %q, %v", b, rerr)
	}
	if got, _ := gitOut(w.bridge, "ls-remote", remoteName(w.bridge), "refs/heads/fresh"); got != "" {
		t.Fatalf("nothing should have been pushed, remote has %q", got)
	}
}

func TestJournalPushConflict(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	rel := "projects/supernova/tasks/auth-refactor/task.md"
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, filepath.FromSlash(rel)), "theirs\n")
	runGit(t, other, "commit", "-q", "-am", "theirs")
	runGit(t, other, "push", "-q")

	write(t, filepath.Join(w.bridge, filepath.FromSlash(rel)), "mine\n")
	var out bytes.Buffer
	err := cmdJournal([]string{"commit", "-m", "Mine"}, &out)
	if err == nil || !strings.Contains(err.Error(), "another machine changed the same files") || !strings.Contains(err.Error(), rel) {
		t.Fatalf("want a conflict naming %s, got %v", rel, err)
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(w.bridge, ".git", d)); err == nil {
			t.Fatalf("bridge left mid-rebase (%s)", d)
		}
	}
}

func TestJournalPushNonConflictFailure(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, "incoming.txt"), "theirs\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "incoming")
	runGit(t, other, "push", "-q")

	write(t, filepath.Join(w.bridge, "incoming.txt"), "untracked\n")
	history := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md")
	write(t, history, "# history\n")
	var out bytes.Buffer
	err := cmdJournal([]string{"commit", "-m", "Add history"}, &out)
	if err == nil || !strings.Contains(err.Error(), "incoming.txt") || strings.Contains(err.Error(), "another machine") {
		t.Fatalf("want git's own message without a conflict claim, got %v", err)
	}
}

// failUnmergedCheckAfter lets the first n unmerged-file checks run for real, then makes every later one fail.
func failUnmergedCheckAfter(t *testing.T, n int) {
	t.Helper()
	real := diffUnmerged
	calls := 0
	diffUnmerged = func(bridge string) (string, error) {
		calls++
		if calls > n {
			return "", fmt.Errorf("simulated diff failure")
		}
		return real(bridge)
	}
	t.Cleanup(func() { diffUnmerged = real })
}

func TestJournalPushUnmergedCheckFailureRefuses(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md"), "# history\n")
	runGit(t, w.bridge, "add", "-A")
	runGit(t, w.bridge, "commit", "-q", "-m", "Add history")
	corruptIndex(t, w.bridge)
	var out bytes.Buffer
	err := cmdJournal([]string{"push"}, &out)
	if err == nil || !strings.Contains(err.Error(), "couldn't check the bridge for unmerged files") || !strings.Contains(err.Error(), "git diff") {
		t.Fatalf("want a refusal naming the failed check, got %v", err)
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "bridge" {
		t.Fatalf("remote has %q", got)
	}
}

func TestJournalPushUnmergedCheckFailureAfterFailedPull(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	rel := "projects/supernova/tasks/auth-refactor/task.md"
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, filepath.FromSlash(rel)), "theirs\n")
	runGit(t, other, "commit", "-q", "-am", "theirs")
	runGit(t, other, "push", "-q")

	write(t, filepath.Join(w.bridge, filepath.FromSlash(rel)), "mine\n")
	failUnmergedCheckAfter(t, 1)
	var out bytes.Buffer
	err := cmdJournal([]string{"commit", "-m", "Mine"}, &out)
	if err == nil || !strings.Contains(err.Error(), "pulling origin/main failed") || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "simulated diff failure") || strings.Contains(err.Error(), "another machine") {
		t.Fatalf("want a pull error saying the conflict state is unknown, got %v", err)
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(w.bridge, ".git", d)); err == nil {
			t.Fatalf("bridge left mid-rebase (%s)", d)
		}
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "theirs" {
		t.Fatalf("remote has %q", got)
	}
}

func TestJournalPushUnmergedCheckFailureAfterPull(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, "incoming.txt"), "theirs\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "incoming")
	runGit(t, other, "push", "-q")

	write(t, filepath.Join(w.bridge, ".gitignore"), "mine\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md"), "# history\n")
	failUnmergedCheckAfter(t, 1)
	var out bytes.Buffer
	err := cmdJournal([]string{"commit", "-m", "Add history"}, &out)
	if err == nil || !strings.Contains(err.Error(), "pulling origin/main succeeded") || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "simulated diff failure") {
		t.Fatalf("want an error saying the re-apply state is unknown, got %v", err)
	}
	if got := runGit(t, remote, "log", "--format=%s", "-1"); got != "incoming" {
		t.Fatalf("remote has %q", got)
	}
}

func corruptIndex(t *testing.T, dir string) {
	t.Helper()
	write(t, filepath.Join(dir, ".git", "index"), "not an index")
}

// failingBridge is a git bridge with a remote and one committed supernova task, whose index is then corrupted so git status fails.
func failingBridge(t *testing.T, files map[string]string) string {
	t.Helper()
	needGit(t)
	b := newRepo(t, filepath.Join(t.TempDir(), "bridge"))
	runGit(t, b, "remote", "add", "origin", filepath.ToSlash(t.TempDir()))
	for p, s := range files {
		write(t, filepath.Join(b, p), s)
	}
	runGit(t, b, "add", "-A")
	runGit(t, b, "commit", "-q", "-m", "old bridge")
	corruptIndex(t, b)
	return b
}

func TestGitStatusFailuresAreNotACleanTree(t *testing.T) {
	needGit(t)
	t.Run("squash", func(t *testing.T) {
		ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
		base := runGit(t, ws, "rev-parse", "HEAD")
		write(t, filepath.Join(ws, "lib.txt"), "one\n")
		runGit(t, ws, "add", "lib.txt")
		runGit(t, ws, "commit", "-q", "-m", "more")
		corruptIndex(t, ws)
		if _, err := squash(ws, base, "squashed"); err == nil || !strings.Contains(err.Error(), "git status") {
			t.Fatalf("got %v", err)
		}
		if n := runGit(t, ws, "rev-list", "--count", "HEAD"); n != "2" {
			t.Errorf("history must not be rewritten, %s commits", n)
		}
	})
	t.Run("githubCommit", func(t *testing.T) {
		r, ws := strayRun(t)
		write(t, filepath.Join(ws, "nova.txt"), "x\n")
		corruptIndex(t, ws)
		if err := githubCommit(r); err == nil || !strings.Contains(err.Error(), "git status") {
			t.Fatalf("got %v", err)
		}
		if n := runGit(t, ws, "rev-list", "--count", "HEAD"); n != "1" {
			t.Errorf("nothing may be committed, %s commits", n)
		}
	})
	t.Run("vcsState", func(t *testing.T) {
		ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
		corruptIndex(t, ws)
		got := vcsState(ws, TaskVcs{})
		if !strings.Contains(got, "status unknown") || strings.Contains(got, "0 changed files") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("vcsStateKeepsBranchStop", func(t *testing.T) {
		ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
		corruptIndex(t, ws)
		got := vcsState(ws, TaskVcs{Branch: "feature/supernova-refactor"})
		if !strings.Contains(got, "status unknown") || !strings.Contains(got, "STOP: the task expects branch feature/supernova-refactor") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("syncBridge", func(t *testing.T) {
		b := failingBridge(t, map[string]string{"projects/supernova/project.yaml": "x: 1\n"})
		got := syncBridge(b)
		if !strings.Contains(got, "could not be read") || !strings.Contains(got, "not updated") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("journal", func(t *testing.T) {
		b := failingBridge(t, map[string]string{"projects/supernova/project.yaml": "x: 1\n"})
		if changed, err := journalChanges(b); err == nil || changed != nil {
			t.Fatalf("got %v, %v", changed, err)
		}
		out := journalStop(b, &HookInput{})
		if out == nil || !strings.Contains(fmt.Sprint(out), "could not be read") {
			t.Errorf("the stop must be blocked: %v", out)
		}
		if out := journalStop(b, &HookInput{StopHookActive: true}); out != nil {
			t.Errorf("a second stop must not trap the session: %v", out)
		}
		var buf bytes.Buffer
		t.Chdir(b)
		write(t, filepath.Join(b, "bridge.yaml"), "name: supernova\n")
		if err := cmdJournal([]string{"commit", "-m", "x"}, &buf); err == nil || !strings.Contains(err.Error(), "git status") {
			t.Errorf("journal commit got %v", err)
		}
	})
	t.Run("session migrations", func(t *testing.T) {
		const task = "projects/supernova/tasks/auth-refactor/task.md"
		files := map[string]string{
			"CLAUDE.md": "# Bridge\n\n## The heartbeat\n\n" + oldHeartbeat + "\n" + sessionImport + "\n",
			task:        "---\npipeline: coding\n---\n\n# t\n\n## Progress log\n\n- one.\n",
		}
		for name, run := range map[string]func(b string){
			"addGitattributes":  addGitattributes,
			"updateHeartbeat":   updateHeartbeat,
			"dropSessionImport": dropSessionImport,
			"splitHistory":      func(b string) { splitHistory(b) },
		} {
			t.Run(name, func(t *testing.T) {
				b := failingBridge(t, files)
				head := runGit(t, b, "rev-parse", "HEAD")
				run(b)
				if got := runGit(t, b, "rev-parse", "HEAD"); got != head {
					t.Error("nothing may be committed")
				}
				for p, want := range files {
					if got, _ := os.ReadFile(filepath.Join(b, p)); string(got) != want {
						t.Errorf("%s changed:\n%s", p, got)
					}
				}
				if _, err := os.Stat(filepath.Join(b, ".gitattributes")); err == nil {
					t.Error(".gitattributes was written")
				}
				if _, err := os.Stat(filepath.Join(b, filepath.Dir(task), "history.md")); err == nil {
					t.Error("history.md was written")
				}
			})
		}
		b := failingBridge(t, files)
		if notes := splitHistory(b); !strings.Contains(notes, "could not read") {
			t.Errorf("splitHistory should say why: %q", notes)
		}
	})
}

func TestJournalChangesQuotedAndRenamedPaths(t *testing.T) {
	needGit(t)
	b := newRepo(t, filepath.Join(t.TempDir(), "bridge"))
	write(t, filepath.Join(b, "projects/supernova/notes/old name.md"), "a\n")
	runGit(t, b, "add", "-A")
	runGit(t, b, "commit", "-q", "-m", "old")
	runGit(t, b, "mv", "projects/supernova/notes/old name.md", "projects/supernova/notes/café au lait.md")
	write(t, filepath.Join(b, "skills/supernova/Zoë's skill.md"), "b\n")
	got, err := journalChanges(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"projects/supernova/notes/café au lait.md", "projects/supernova/notes/old name.md", "skills/supernova/Zoë's skill.md"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMigrateWorkspacesSkipsQuotedPreDirtyPathsAndFailedStatus(t *testing.T) {
	needGit(t)
	setup := func(t *testing.T) (string, string) {
		b := newRepo(t, filepath.Join(t.TempDir(), "bridge"))
		ws := filepath.ToSlash(t.TempDir())
		write(t, filepath.Join(b, ".gitignore"), ".starship/\n")
		write(t, filepath.Join(b, "projects/supernova/project.yaml"), "workspace: "+ws+"\n")
		write(t, filepath.Join(b, "projects/supernova/tasks/café one/task.md"), "---\nworkspace: "+ws+"\n---\n\n# one\n")
		runGit(t, b, "add", "-A")
		runGit(t, b, "commit", "-q", "-m", "old")
		return b, ws
	}
	t.Run("quoted path", func(t *testing.T) {
		b, ws := setup(t)
		write(t, filepath.Join(b, "projects/supernova/tasks/café one/task.md"), "---\nworkspace: "+ws+"\n---\n\n# one\n\nhalf written\n")
		note := migrateWorkspaces(b)
		if !strings.Contains(note, "café one/task.md") {
			t.Errorf("the user's edited file is left uncommitted: %q", note)
		}
		if got := runGit(t, b, "-c", "core.quotepath=off", "show", "--name-only", "--format=", "HEAD"); strings.Contains(got, "task.md") {
			t.Errorf("committed the user's file:\n%s", got)
		}
	})
	t.Run("failed status", func(t *testing.T) {
		b, _ := setup(t)
		head := runGit(t, b, "rev-parse", "HEAD")
		corruptIndex(t, b)
		note := migrateWorkspaces(b)
		if !strings.Contains(note, "could not be read") {
			t.Errorf("note: %q", note)
		}
		if got := runGit(t, b, "rev-parse", "HEAD"); got != head {
			t.Error("nothing may be committed")
		}
	})
}

func TestOldLoopBudgetNameIsAnAlias(t *testing.T) {
	w := newWorld(t)
	w.project(`  coding2: {}
`)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "      - set:    { budgets.test: 2 }\n",
		"      - set:    { budgets.test: 2 }\n      - set:    { budgets.loop: 1 }\n", 1))
	lead := w.startLead()
	r, err := openRun(w.bridge, w.state().Run)
	if err != nil {
		t.Fatal(err)
	}
	b := r.Snap.Pipeline.Budgets
	r.close()
	if _, old := b["loop"]; old || b["coder-loop"] != 1 {
		t.Fatalf("budgets.loop should set coder-loop: %v", b)
	}
	w.leaf(lead, "coder", "broken")
	if st := w.state(); st.Status != "error" || !strings.HasPrefix(st.Reason, "budget:coder-loop") {
		t.Fatalf("status %s reason %s", st.Status, st.Reason)
	}
}

func TestResumeBudgetOldLoopName(t *testing.T) {
	w := newWorld(t)
	w.project(`  coding2: {}
`)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "      - set:    { budgets.test: 2 }\n",
		"      - set:    { budgets.test: 1 }\n      - set:    { on-exhausted.test: { ask-user: \"another cycle?\" } }\n", 1))
	lead := w.startLead("--start", "test")
	w.leaf(lead, "tester", "fail")
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "fail")
	var out bytes.Buffer
	if err := cmdResume([]string{"--answer", "yes", "--budget", "test=1", "--budget", "loop=2"}, &out); err != nil {
		t.Fatal(err)
	}
	if st := w.state(); st.Extra["coder-loop"] != 2 || st.Extra["loop"] != 0 {
		t.Fatalf("extra %v", st.Extra)
	}
}

func TestSavedRunWithOldLoopName(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "broken")
	id := w.state().Run
	dir := filepath.Join(runsDir(w.bridge), id)
	for _, f := range []string{"pipeline.json", "state.json"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, f), strings.ReplaceAll(string(b), "coder-loop", "loop"))
	}
	if st := w.runState(id); st.Spent["coder-loop"] != 2 || st.Spent["loop"] != 0 {
		t.Fatalf("spent %v", st.Spent)
	}
	w.leaf(lead, "coder", "broken")
	if st := w.state(); st.Spent["coder-loop"] != 3 || st.Status != "running" {
		t.Fatalf("after another pass: %s %v", st.Status, st.Spent)
	}
}

func TestReadRunFilesFoldsOldLoopName(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "broken")
	dir := filepath.Join(runsDir(w.bridge), w.state().Run)
	for _, f := range []string{"pipeline.json", "state.json"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, f), strings.ReplaceAll(string(b), "coder-loop", "loop"))
	}
	snap, st := &Snapshot{}, &State{}
	if err := readRunFiles(dir, snap, st); err != nil {
		t.Fatal(err)
	}
	if _, old := st.Spent["loop"]; old || st.Spent["coder-loop"] != 2 {
		t.Fatalf("spent %v", st.Spent)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"loop"`) {
		t.Fatalf("snapshot still names loop: %s", b)
	}
}

func TestOpenRunIfExistsMissingState(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	id := w.state().Run
	if err := os.Remove(filepath.Join(runsDir(w.bridge), id, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := openRun(w.bridge, id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("openRun error = %v, want fs.ErrNotExist", err)
	}
	r, err := openRunIfExists(w.bridge, id)
	if r != nil || err != nil {
		t.Fatalf("openRunIfExists = %v, %v; want nil, nil", r, err)
	}
}

func TestConfigRefusesPlatformsOutsideProject(t *testing.T) {
	for _, value := range []string{"[linux]", "[]", "", "~"} {
		w := newWorld(t)
		bf := filepath.Join(w.bridge, "bridge.yaml")
		data, _ := os.ReadFile(bf)
		write(t, bf, string(data)+"platforms: "+value+"\n")
		_, err := loadConfig(w.bridge, "supernova/auth-refactor")
		if err == nil || !strings.Contains(err.Error(), "project.yaml") || !strings.Contains(err.Error(), bf) {
			t.Errorf("bridge.yaml with platforms: %s should be refused, naming project.yaml: %v", value, err)
		}

		w = newWorld(t)
		task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
		data, _ = os.ReadFile(task)
		write(t, task, strings.Replace(string(data), "pipeline: coding\n", "pipeline: coding\nplatforms: "+value+"\n", 1))
		_, err = loadConfig(w.bridge, "supernova/auth-refactor")
		if err == nil || !strings.Contains(err.Error(), "projects/supernova/project.yaml") || !strings.Contains(err.Error(), task) {
			t.Errorf("task with platforms: %s should be refused, naming project.yaml: %v", value, err)
		}
	}
}

func TestConfigRefusesPlatformsInUsedExtension(t *testing.T) {
	for _, value := range []string{"[linux]", "[]", "", "~"} {
		w := newWorld(t)
		ext := filepath.Join(w.bridge, "extensions", "house", "extension.yaml")
		write(t, ext, "name: house\nplatforms: "+value+"\n")
		pf := filepath.Join(w.bridge, "projects", "supernova", "project.yaml")
		data, _ := os.ReadFile(pf)

		// Not used: loads fine.
		if _, err := loadConfig(w.bridge, "supernova/auth-refactor"); err != nil {
			t.Errorf("an unused extension with platforms: %s should not be refused: %v", value, err)
		}
		if _, err := loadProjectBindings(w.bridge, "supernova", ""); err != nil {
			t.Errorf("an unused extension with platforms: %s should not be refused: %v", value, err)
		}

		write(t, pf, string(data)+"\nuse: [house]\n")
		_, err := loadConfig(w.bridge, "supernova/auth-refactor")
		if err == nil || !strings.Contains(err.Error(), "projects/supernova/project.yaml") || !strings.Contains(err.Error(), ext) {
			t.Errorf("used extension with platforms: %s should be refused: %v", value, err)
		}
		_, err = loadProjectBindings(w.bridge, "supernova", "")
		if err == nil || !strings.Contains(err.Error(), "projects/supernova/project.yaml") || !strings.Contains(err.Error(), ext) {
			t.Errorf("loadProjectBindings with platforms: %s should be refused: %v", value, err)
		}

		// A bridge-scope run has no project, so the message names a <project> placeholder.
		bf := filepath.Join(w.bridge, "bridge.yaml")
		bdata, _ := os.ReadFile(bf)
		write(t, bf, string(bdata)+"\nuse: [house]\n")
		_, err = loadTarget(w.bridge, Target{Pipeline: "skill-curate"})
		if err == nil || !strings.Contains(err.Error(), "projects/<project>/project.yaml") || !strings.Contains(err.Error(), ext) {
			t.Errorf("bridge-scope use with platforms: %s should be refused: %v", value, err)
		}
	}
}
