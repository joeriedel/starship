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

	// The orchestrator's prompt carried the bundle's augment.
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

	// A project can turn journal commits off.
	write(t, task, string(data)+"\n- more\n")
	w.project("journal: { commit: off, reason: kept elsewhere }\n")
	if r := stop(false); strings.Contains(r, "task.md") {
		t.Fatalf("journal commits are off for this project: %q", r)
	}
}
