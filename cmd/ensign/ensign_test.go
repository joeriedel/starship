// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// world is a copy of the fixture bridge with a fake Claude Code session around it.
type world struct {
	t       *testing.T
	bridge  string
	session string // session transcript path; subagent transcripts are derived from it
	sid     string // Claude Code session id
	agents  int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root, _ := filepath.Abs("../..")
	tmp := t.TempDir()
	bridge := filepath.Join(tmp, "bridge")
	copyDir(t, filepath.Join(root, "testdata", "bridge"), bridge)
	ws := filepath.Join(tmp, "workspace")
	os.MkdirAll(ws, 0o755)
	write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+root+"\n")
	if _, err := setWorkspace(bridge, "supernova", ws); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", bridge)
	t.Setenv("STARSHIP_SESSION", "s1")
	return &world{t: t, bridge: bridge, session: filepath.Join(tmp, "session.jsonl"), sid: "s1"}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	filepath.Walk(from, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(from, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, _ := os.ReadFile(p)
		return os.WriteFile(filepath.Join(to, rel), data, 0o644)
	})
}

func write(t *testing.T, path, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *world) run(args ...string) string {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdRun(args, &out); err != nil {
		w.t.Fatalf("ensign run: %v\n%s", err, out.String())
	}
	return out.String()
}

// hook sends one event and returns the decoded output and the exit code.
func (w *world) hook(event string, in map[string]any) (map[string]any, int) {
	w.t.Helper()
	in["hook_event_name"] = event
	in["transcript_path"] = w.session
	in["session_id"] = w.sid
	data, _ := json.Marshal(in)
	var out, errb bytes.Buffer
	code := cmdHook(event, bytes.NewReader(data), &out, &errb)
	var res map[string]any
	if out.Len() > 0 {
		json.Unmarshal(out.Bytes(), &res)
	}
	if code != 0 {
		res = map[string]any{"stderr": errb.String()}
	}
	return res, code
}

func (w *world) approve() {
	w.t.Helper()
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	ctx := dig(res, "hookSpecificOutput", "additionalContext")
	if !strings.Contains(ctx, "approved") {
		w.t.Fatalf("go was not approved: %v", res)
	}
}

// spawn asks to start an agent. caller is "" for the session. It returns the new agent's
// id when allowed, or "" and the deny reason.
func (w *world) spawn(caller, callerType, role string) (string, string) {
	w.t.Helper()
	in := map[string]any{
		"tool_name":  "Agent",
		"tool_input": map[string]any{"subagent_type": role, "prompt": "do the thing", "description": "x"},
	}
	if caller != "" {
		in["agent_id"], in["agent_type"] = caller, callerType
	}
	res, code := w.hook("PreToolUse", in)
	if code != 0 {
		w.t.Fatalf("PreToolUse exited %d: %v", code, res)
	}
	if res == nil {
		w.t.Fatalf("PreToolUse for %s gave no decision", role)
	}
	if dig(res, "hookSpecificOutput", "permissionDecision") == "deny" {
		return "", dig(res, "hookSpecificOutput", "permissionDecisionReason")
	}
	w.agents++
	id := fmt.Sprintf("agent%02d", w.agents)
	prompt := res["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["prompt"].(string)
	rec, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
	write(w.t, agentTranscript(w.session, id), string(rec)+"\n")
	return id, ""
}

// handback is an agent's SubagentHandback call, which carries its report; it returns the
// reason the call was refused, or "".
func (w *world) handback(agent, agentType, message string) string {
	w.t.Helper()
	res, code := w.hook("PreToolUse", map[string]any{
		"agent_id": agent, "agent_type": agentType, "tool_name": "SubagentHandback",
		"tool_input": map[string]any{"message": message},
	})
	if code != 0 {
		w.t.Fatalf("SubagentHandback exited %d: %v", code, res)
	}
	if res != nil {
		return res["hookSpecificOutput"].(map[string]any)["permissionDecisionReason"].(string)
	}
	return ""
}

// stop ends an agent with a final message and no handback; it returns the block reason, or "".
func (w *world) stop(agent, agentType, message string, active bool) string {
	w.t.Helper()
	return w.stopWith(agent, agentType, message, active, nil)
}

// stopWith ends an agent's turn without a handback, listing the session's background tasks.
func (w *world) stopWith(agent, agentType, last string, active bool, tasks []map[string]any) string {
	w.t.Helper()
	res, code := w.hook("SubagentStop", map[string]any{
		"agent_id": agent, "agent_type": agentType, "last_assistant_message": last,
		"agent_transcript_path": agentTranscript(w.session, agent), "stop_hook_active": active,
		"background_tasks": tasks,
	})
	if code != 0 {
		w.t.Fatalf("SubagentStop exited %d: %v", code, res)
	}
	if res != nil && res["decision"] == "block" {
		return res["reason"].(string)
	}
	return ""
}

// launched is the PostToolUse of an agent that started a background task.
func (w *world) launched(agent, agentType, tool string, response map[string]any) {
	w.t.Helper()
	res, code := w.hook("PostToolUse", map[string]any{
		"agent_id": agent, "agent_type": agentType, "tool_name": tool, "tool_response": response,
	})
	if code != 0 || res != nil {
		w.t.Fatalf("PostToolUse should be silent, got %d: %v", code, res)
	}
}

func report(outcome string) string {
	return "Did it.\n\n```starship-report\noutcome: " + outcome + "\nsummary: build: ok; tests: 2 passed\n```\n"
}

// leaf spawns a stage agent from the lead and stops it with an outcome.
func (w *world) leaf(lead, role, outcome string) {
	w.t.Helper()
	id, why := w.spawn(lead, "orchestrator", role)
	if id == "" {
		w.t.Fatalf("spawning %s was refused: %s", role, why)
	}
	if r := w.stop(id, role, report(outcome), false); r != "" {
		w.t.Fatalf("%s stop was blocked: %s", role, r)
	}
}

func (w *world) state() *State {
	w.t.Helper()
	r, err := openSessionRun(w.bridge, w.sid)
	if err != nil || r == nil {
		w.t.Fatalf("no active run: %v", err)
	}
	defer r.close()
	return r.State
}

func dig(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

// startLead creates and approves a run, and spawns the orchestrator.
func (w *world) startLead(args ...string) string {
	w.t.Helper()
	w.run(append([]string{"supernova/auth-refactor"}, args...)...)
	w.approve()
	lead, why := w.spawn("", "", "orchestrator")
	if lead == "" {
		w.t.Fatalf("lead refused: %s", why)
	}
	return lead
}

func TestHelpAnswersWithoutTheModel(t *testing.T) {
	w := newWorld(t)
	res, code := w.hook("UserPromptSubmit", map[string]any{"prompt": "help"})
	if code != 0 || res["continue"] != false || res["decision"] != nil {
		t.Fatalf("help should stop the prompt with continue:false, got %d %v", code, res)
	}
	if r, _ := res["stopReason"].(string); !strings.HasPrefix(r, "\n") || !strings.Contains(r, "Getting around") {
		t.Fatalf("help text missing: %q", r)
	}
}

func TestMergeAndProvenance(t *testing.T) {
	w := newWorld(t)
	c, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if errs := validate(c); len(errs) > 0 {
		t.Fatalf("unexpected validation errors: %v", errs)
	}
	review, _, scope := c.Pipeline.find("review")
	if scope != "run" || strings.Join(review.Dimensions, ",") != "correctness,tests" {
		t.Fatalf("review: scope %q dims %v", scope, review.Dimensions)
	}
	if !strings.Contains(review.Source, "append by project:projects/supernova/project.yaml") {
		t.Errorf("review source does not record the project's append: %s", review.Source)
	}
	if c.Pipeline.Budgets["test"] != 2 || !strings.HasPrefix(c.Pipeline.Sources["budgets.test"], "project:") {
		t.Errorf("budgets.test = %d from %s", c.Pipeline.Budgets["test"], c.Pipeline.Sources["budgets.test"])
	}
	if c.Bindings["lint"].NotApplicable == "" {
		t.Errorf("lint should be declared not applicable")
	}
	out := w.run("supernova/auth-refactor", "--dry-run")
	if !strings.Contains(out, "dry run: nothing created") || !strings.Contains(out, "from project:") {
		t.Errorf("dry run output:\n%s", out)
	}
	if len(listRuns(w.bridge)) != 0 {
		t.Errorf("dry run created a run")
	}
}

func TestValidationCatchesMissingBinding(t *testing.T) {
	w := newWorld(t)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `  test:  { command: "./test.sh" }`+"\n", "", 1))
	c, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	errs := strings.Join(validate(c), "\n")
	if !strings.Contains(errs, "binding @test is required") {
		t.Fatalf("missing @test not caught: %s", errs)
	}
}

func TestHappyPath(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	if r := w.stop(lead, "orchestrator", "done", false); !strings.Contains(r, "not finished") {
		t.Fatalf("lead stopping early was not blocked: %q", r)
	}
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
	st := w.state()
	if st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := st.Dispatches[len(st.Dispatches)-1].Summary; got != "build: ok; tests: 2 passed" {
		t.Errorf("a summary containing colons was not kept whole: %q", got)
	}
	if r := w.stop(lead, "orchestrator", "done", false); r != "" {
		t.Fatalf("lead stop after success was blocked: %s", r)
	}
	dims := map[string]bool{}
	for _, d := range st.Dispatches {
		if d.Role == "reviewer" {
			dims[d.Dim] = true
		}
	}
	if !dims["correctness"] || !dims["tests"] {
		t.Errorf("reviewers were not assigned both dimensions: %v", dims)
	}
}

func TestOrderIsEnforced(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	if id, why := w.spawn("", "", "orchestrator"); id != "" || !strings.Contains(why, "not approved") {
		t.Fatalf("lead before go: id %q why %q", id, why)
	}
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	if id, why := w.spawn(lead, "orchestrator", "tester"); id != "" || !strings.Contains(why, `next stage is "code"`) {
		t.Fatalf("tester before code: id %q why %q", id, why)
	}
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	if id, why := w.spawn(coder, "coder", "coder"); id != "" || !strings.Contains(why, "cannot dispatch") {
		t.Fatalf("coder spawning a coder: id %q why %q", id, why)
	}
	res, code := w.hook("PreToolUse", map[string]any{"agent_id": coder, "agent_type": "coder",
		"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "explorer", "prompt": "look"}})
	if code != 0 || res != nil {
		t.Fatalf("free role should pass without a decision: %d %v", code, res)
	}
	if id, why := w.spawn(lead, "orchestrator", "coder"); id != "" || !strings.Contains(why, "still running") {
		t.Fatalf("second coder while one runs: id %q why %q", id, why)
	}
}

func TestStagesRunInTheForeground(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	res, code := w.hook("PreToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator",
		"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "coder", "prompt": "go", "run_in_background": true}})
	if code != 0 || dig(res, "hookSpecificOutput", "permissionDecision") != "allow" {
		t.Fatalf("coder dispatch: %d %v", code, res)
	}
	if bg := res["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["run_in_background"]; bg != false {
		t.Fatalf("run_in_background = %v, want false", bg)
	}

	// The lead ending its turn while that stage still runs is a wait, not giving up.
	for i := 0; i < 5; i++ {
		if r := w.stop(lead, "orchestrator", "Waiting for the coder.", i > 0); r != "" {
			t.Fatalf("waiting lead was blocked: %s", r)
		}
	}
	if st := w.state(); st.Status != "running" {
		t.Fatalf("status %s (%s), want running", st.Status, st.Reason)
	}
}

func TestForegroundDispatchAddsNoField(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	res, _ := w.hook("PreToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator",
		"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "coder", "prompt": "go"}})
	if _, ok := res["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["run_in_background"]; ok {
		t.Fatalf("run_in_background was added to an input that didn't have it")
	}
}

func TestFreeRolePassesThrough(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	res, code := w.hook("PreToolUse", map[string]any{
		"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "explorer", "prompt": "look"},
	})
	if code != 0 || res != nil {
		t.Fatalf("explorer: code %d res %v", code, res)
	}
}

func TestSessionSpawnOfNonStageRolePasses(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	pass := func(role string) {
		t.Helper()
		in := map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": role, "prompt": "look"}}
		if res, code := w.hook("PreToolUse", in); code != 0 || res != nil {
			t.Fatalf("%s: code %d res %v", role, code, res)
		}
	}
	for _, role := range []string{"general-purpose", "Explore", "claude-code-guide"} {
		pass(role)
	}
	if id, why := w.spawn("", "", "coder"); id != "" || !strings.Contains(why, "next stage is") {
		t.Fatalf("session coder: id %q why %q", id, why)
	}
	if id, why := w.spawn("", "", "orchestrator"); id != "" || !strings.Contains(why, "not approved") {
		t.Fatalf("lead before go: id %q why %q", id, why)
	}
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	for _, role := range []string{"general-purpose", "Explore", "claude-code-guide"} {
		pass(role)
	}
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	if id, why := w.spawn(coder, "coder", "general-purpose"); id != "" || !strings.Contains(why, "cannot dispatch") {
		t.Fatalf("coder spawning general-purpose: id %q why %q", id, why)
	}
}

func TestFindingsLoopAndJoinPrecedence(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "findings")
	st := w.state()
	if st.Scopes["run"].Stage != "code" || st.Spent["loop"] != 2 {
		t.Fatalf("after findings: stage %s loop %d", st.Scopes["run"].Stage, st.Spent["loop"])
	}
}

func TestLoopBudgetExhausted(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "broken")
	w.leaf(lead, "coder", "broken")
	w.leaf(lead, "coder", "broken")
	st := w.state()
	if st.Status != "error" || !strings.HasPrefix(st.Reason, "budget:loop") {
		t.Fatalf("status %s reason %s", st.Status, st.Reason)
	}
	if _, why := w.spawn(lead, "orchestrator", "coder"); !strings.Contains(why, "run is finished") {
		t.Fatalf("spawn after the run ended: %s", why)
	}
}

func TestTestFailureResetsLoopAndSpendsTest(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	cycle := func(testOutcome string) {
		w.leaf(lead, "coder", "compiled")
		w.leaf(lead, "reviewer", "clean")
		w.leaf(lead, "reviewer", "clean")
		w.leaf(lead, "style-reviewer", "done")
		w.leaf(lead, "tester", testOutcome)
	}
	cycle("fail")
	st := w.state()
	if st.Scopes["run"].Stage != "code" || st.Spent["loop"] != 1 || st.Spent["test"] != 1 {
		t.Fatalf("after one failure: stage %s loop %d test %d", st.Scopes["run"].Stage, st.Spent["loop"], st.Spent["test"])
	}
	cycle("fail")
	cycle("fail") // the project set test to 2
	st = w.state()
	if st.Status != "error" || !strings.HasPrefix(st.Reason, "budget:test") {
		t.Fatalf("status %s reason %s", st.Status, st.Reason)
	}
}

func TestBadReportIsBlockedThenFails(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	if r := w.stop(coder, "coder", "I did it, no report.", false); !strings.Contains(r, "no starship-report") {
		t.Fatalf("missing report: %q", r)
	}
	if r := w.stop(coder, "coder", report("finished"), true); !strings.Contains(r, `"finished" is not one of`) {
		t.Fatalf("wrong outcome: %q", r)
	}
	if r := w.stop(coder, "coder", "still nothing", true); r != "" {
		t.Fatalf("third bad report should end the run, not block: %q", r)
	}
	if st := w.state(); st.Status != "error" || !strings.HasPrefix(st.Reason, "bad-report") {
		t.Fatalf("status %s reason %s", st.Status, st.Reason)
	}
}

func TestStartAtTest(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead("--start", "test")
	if _, why := w.spawn(lead, "orchestrator", "coder"); !strings.Contains(why, `next stage is "test"`) {
		t.Fatalf("expected test first: %s", why)
	}
	w.leaf(lead, "tester", "fail")
	if st := w.state(); st.Scopes["run"].Stage != "code" || st.Spent["loop"] != 1 {
		t.Fatalf("after failing test: stage %s loop %d", st.Scopes["run"].Stage, st.Spent["loop"])
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	id, _ := os.ReadFile(sessionFile(w.bridge, w.sid))
	write(t, filepath.Join(runsDir(w.bridge), strings.TrimSpace(string(id)), "state.json"), "{not json")
	_, code := w.hook("PreToolUse", map[string]any{
		"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "orchestrator", "prompt": "x"},
	})
	if code != 2 {
		t.Fatalf("corrupt state should exit 2, got %d", code)
	}
	_, code = w.hook("SubagentStop", map[string]any{"agent_id": "a", "stop_hook_active": true})
	if code != 0 {
		t.Fatalf("a forced-continue stop should be let through, got %d", code)
	}
}

// addTask copies the fixture task under a new name with its own workspace.
func (w *world) addTask(name string) {
	w.t.Helper()
	ws := filepath.Join(filepath.Dir(w.bridge), "workspace-"+name)
	os.MkdirAll(ws, 0o755)
	write(w.t, filepath.Join(w.bridge, "projects/supernova/tasks", name, "task.md"),
		"---\npipeline: coding\n---\n\n## Goal\n\nDo "+name+".\n")
	if _, err := setWorkspace(w.bridge, "supernova/"+name, ws); err != nil {
		w.t.Fatal(err)
	}
}

func TestSessionsAreIndependent(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	leadA := w.startLead()

	b := *w
	b.sid = "s2"
	t.Setenv("STARSHIP_SESSION", "s2")
	b.run("supernova/other")
	b.approve()
	leadB, why := b.spawn("", "", "orchestrator")
	if leadB == "" {
		t.Fatalf("session 2's lead was refused: %s", why)
	}
	b.leaf(leadB, "coder", "compiled")

	t.Setenv("STARSHIP_SESSION", "s1")
	if st := w.state(); st.Scopes["run"].Stage != "code" {
		t.Fatalf("session 2's work moved session 1's run to %s", st.Scopes["run"].Stage)
	}
	w.leaf(leadA, "coder", "compiled")
	if st := w.state(); st.Scopes["run"].Stage != "review" {
		t.Fatalf("session 1's run is at %s", st.Scopes["run"].Stage)
	}
}

// runState reads a run's state from disk, whichever session it belongs to.
func (w *world) runState(id string) *State {
	w.t.Helper()
	r, err := openRun(w.bridge, id)
	if err != nil {
		w.t.Fatal(err)
	}
	defer r.close()
	return r.State
}

func TestNewRunReplacesTheSessionsWaitingRun(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.run("supernova/auth-refactor")
	old := w.state().Run
	out := w.run("supernova/other")
	cur := w.state()
	if cur.Run == old {
		t.Fatalf("session still bound to the old run %s", old)
	}
	if !strings.Contains(out, "replaced waiting run "+old) {
		t.Fatalf("output does not mention the replacement:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(runsDir(w.bridge), old)); !os.IsNotExist(err) {
		t.Fatalf("replaced run folder still exists: %v", err)
	}
}

func TestRunApprovedDuringSetupIsKeptAndTheNewRunDeleted(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.run("supernova/auth-refactor")
	old := w.state().Run
	held, err := openRun(w.bridge, old)
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- cmdRun([]string{"supernova/other"}, &out)
	}()
	// state.json of the new run is written before the replace loop blocks on the old run's lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(runsDir(w.bridge))
		found := false
		for _, e := range entries {
			if e.Name() == old {
				continue
			}
			if _, err := os.Stat(filepath.Join(runsDir(w.bridge), e.Name(), "state.json")); err == nil {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			held.close()
			t.Fatal("new run was never created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	held.State.Approved = true
	if err := held.save(); err != nil {
		t.Fatal(err)
	}
	held.close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "already has an active run") {
			t.Fatalf("cmdRun: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cmdRun did not return")
	}
	entries, _ := os.ReadDir(runsDir(w.bridge))
	if len(entries) != 1 || entries[0].Name() != old {
		t.Fatalf("runs left: %v", entries)
	}
	if st := w.state(); st.Run != old || !st.Approved {
		t.Fatalf("session run %s approved %v", st.Run, st.Approved)
	}
}

func TestHookWithABoundRunThatIsGoneDoesNotFail(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), w.state().Run)); err != nil {
		t.Fatal(err)
	}
	res, code := w.hook("UserPromptSubmit", map[string]any{"prompt": "hello"})
	if code != 0 {
		t.Fatalf("hook failed: %v", res)
	}
}

func TestSessionWithAnApprovedRunIsRefused(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.run("supernova/auth-refactor")
	w.approve()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdRun([]string{"supernova/other"}, &out); err == nil || !strings.Contains(err.Error(), "already has an active run") {
		t.Fatalf("second run in one session: %v", err)
	}
}

func TestOtherSessionsRunsOnTheSameWorkspaceAreUntouched(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	pending := w.state().Run

	b := *w
	b.sid = "s2"
	t.Setenv("STARSHIP_SESSION", "s2")
	b.run("supernova/auth-refactor")
	b.approve()
	running := b.state().Run

	t.Setenv("STARSHIP_SESSION", "s3")
	w.run("supernova/auth-refactor")
	if st := w.runState(pending); st.Status != "pending" {
		t.Fatalf("pending run of another session is now %s", st.Status)
	}
	if st := w.runState(running); st.Status != "running" {
		t.Fatalf("running run of another session is now %s", st.Status)
	}
}

func TestGoClaimsARunCreatedOutsideASession(t *testing.T) {
	w := newWorld(t)
	t.Setenv("STARSHIP_SESSION", "")
	w.run("supernova/auth-refactor")
	if r, _ := openSessionRun(w.bridge, w.sid); r != nil {
		t.Fatalf("a terminal-created run should not belong to a session yet")
	}
	w.approve()
	if st := w.state(); st.Session != w.sid || st.Status != "running" {
		t.Fatalf("after go: session %q status %s", st.Session, st.Status)
	}
}

func TestSessionStartRecordsTheSession(t *testing.T) {
	w := newWorld(t)
	env := filepath.Join(t.TempDir(), "env.sh")
	t.Setenv("CLAUDE_ENV_FILE", env)
	in, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "abc"})
	var out, errb bytes.Buffer
	if code := cmdHook("SessionStart", bytes.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	data, _ := os.ReadFile(env)
	wantEnv := "export STARSHIP_SESSION=abc\nexport PATH=" + shellQuote(filepath.ToSlash(filepath.Join(w.bridge, ".starship", "bin"))) + ":\"$PATH\"\n"
	show, ctx := sessionStartReply(t, out.Bytes())
	if string(data) != wantEnv || !strings.HasPrefix(ctx, "STARSHIP-READY") || !strings.HasPrefix(show, "Starship ready · bridge ") {
		t.Fatalf("env file %q, shown %q, context %q", data, show, ctx)
	}
	if strings.Contains(show, "ensign") {
		t.Errorf("the line shown to the user must not name ensign: %q", show)
	}
	_ = w
}

// sessionStartReply decodes SessionStart's JSON: the line shown to the user and the
// context Claude reads.
func sessionStartReply(t *testing.T, out []byte) (string, string) {
	t.Helper()
	var r struct {
		SystemMessage string `json:"systemMessage"`
		Specific      struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("SessionStart should reply with JSON: %v\n%s", err, out)
	}
	return r.SystemMessage, r.Specific.AdditionalContext
}

func TestSessionStartDeliversTheManual(t *testing.T) {
	w := newWorld(t)
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	git(w.bridge, "init", "-q")
	git(w.bridge, "-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A")
	write(t, filepath.Join(w.bridge, "CLAUDE.md"), "# Bridge\n\n"+sessionImport+"\n")
	git(w.bridge, "add", "CLAUDE.md")
	git(w.bridge, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "old bridge")

	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	in, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "abc"})
	var out, errb bytes.Buffer
	if code := cmdHook("SessionStart", bytes.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, ctx := sessionStartReply(t, out.Bytes()); !strings.HasPrefix(ctx, "STARSHIP-READY") || !strings.Contains(ctx, "# Starship: the session manual") {
		t.Fatalf("the manual did not follow the heartbeat:\n%s", ctx)
	}
	// Claude Code cuts additionalContext over 10,000 characters to a preview.
	if _, ctx := sessionStartReply(t, out.Bytes()); len(ctx) >= 10000 {
		t.Errorf("the session context is %d characters; over 10,000 it is cut to a preview", len(ctx))
	}
	if claude, _ := os.ReadFile(filepath.Join(w.bridge, "CLAUDE.md")); strings.Contains(string(claude), sessionImport) {
		t.Errorf("the old import should be removed")
	}
	if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", "CLAUDE.md"); dirty != "" {
		t.Errorf("removing the import should be committed: %q", dirty)
	}
}

func TestHeartbeatMigration(t *testing.T) {
	claudePath := func(w *world) string { return filepath.Join(w.bridge, "CLAUDE.md") }
	read := func(w *world) string {
		b, _ := os.ReadFile(claudePath(w))
		return string(b)
	}
	setup := func(t *testing.T, content, autocrlf string) *world {
		w := newWorld(t)
		git(w.bridge, "init", "-q")
		git(w.bridge, "config", "core.autocrlf", autocrlf)
		write(t, claudePath(w), content)
		git(w.bridge, "add", "CLAUDE.md")
		git(w.bridge, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "old bridge")
		t.Setenv("GIT_AUTHOR_NAME", "t")
		t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
		t.Setenv("GIT_COMMITTER_NAME", "t")
		t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
		return w
	}
	old := "# Bridge\n\n## The heartbeat\n\n" + oldHeartbeat + "\n"

	t.Run("old paragraph is replaced and committed", func(t *testing.T) {
		w := setup(t, old, "false")
		updateHeartbeat(w.bridge)
		got := read(w)
		if !strings.Contains(got, heartbeat) || strings.Contains(got, oldHeartbeat) {
			t.Errorf("heartbeat not replaced:\n%s", got)
		}
		if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", "CLAUDE.md"); dirty != "" {
			t.Errorf("the change should be committed: %q", dirty)
		}
		updateHeartbeat(w.bridge)
		if read(w) != got {
			t.Errorf("a second run should change nothing")
		}
	})
	t.Run("dirty file is left alone", func(t *testing.T) {
		w := setup(t, old, "false")
		dirty := old + "my edit\n"
		write(t, claudePath(w), dirty)
		updateHeartbeat(w.bridge)
		if read(w) != dirty {
			t.Errorf("a dirty CLAUDE.md should be unchanged")
		}
	})
	t.Run("file without the old paragraph is left alone", func(t *testing.T) {
		other := "# Bridge\n\nSomething else.\n"
		w := setup(t, other, "false")
		updateHeartbeat(w.bridge)
		if read(w) != other {
			t.Errorf("CLAUDE.md should be unchanged")
		}
	})
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	wantCRLF := crlf("# Bridge\n\n## The heartbeat\n\n" + heartbeat + "\n")
	t.Run("CRLF endings are kept", func(t *testing.T) {
		w := setup(t, crlf(old), "false")
		updateHeartbeat(w.bridge)
		if got := read(w); got != wantCRLF {
			t.Errorf("CRLF heartbeat not migrated cleanly:\n%q", got)
		}
		if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", "CLAUDE.md"); dirty != "" {
			t.Errorf("the change should be committed: %q", dirty)
		}
	})
	t.Run("CRLF working tree with autocrlf true is migrated and committed", func(t *testing.T) {
		w := setup(t, crlf(old), "true")
		if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", "CLAUDE.md"); dirty != "" {
			t.Fatalf("setup left CLAUDE.md dirty: %q", dirty)
		}
		updateHeartbeat(w.bridge)
		if got := read(w); got != wantCRLF {
			t.Errorf("CRLF heartbeat not migrated cleanly:\n%q", got)
		}
		if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", "CLAUDE.md"); dirty != "" {
			t.Errorf("the change should be committed: %q", dirty)
		}
		blob, _ := gitOut(w.bridge, "show", "HEAD:CLAUDE.md")
		if !strings.Contains(blob, heartbeat) || strings.Contains(blob, oldHeartbeat) {
			t.Errorf("committed heartbeat not replaced:\n%s", blob)
		}
	})
	t.Run("the init template carries the new paragraph", func(t *testing.T) {
		if tmpl := fmt.Sprintf(bridgeClaudeMD, "x"); !strings.Contains(tmpl, heartbeat) || strings.Contains(tmpl, oldHeartbeat) {
			t.Errorf("template heartbeat is wrong:\n%s", tmpl)
		}
	})
}

func TestWaitOnOwnBackgroundTask(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	// The lead waits on a background explorer while no stage is running.
	w.launched(lead, "orchestrator", "Agent", map[string]any{"status": "async_launched", "agentId": "explorer1"})
	task := []map[string]any{{"id": "explorer1", "type": "subagent", "status": "running"}}
	if r := w.stopWith(lead, "orchestrator", "Waiting for the explorer.", false, task); r != "" {
		t.Fatalf("a lead waiting on its own background agent should be let stop: %s", r)
	}
	if st := w.state(); st.byAgent(lead).StopBlocks != 0 {
		t.Fatalf("a wait must not spend the stop budget, got %d", st.byAgent(lead).StopBlocks)
	}

	// A stage waits on a background Bash command.
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	w.launched(coder, "coder", "Bash", map[string]any{"backgroundTaskId": "bash1"})
	w.launched(coder, "coder", "Bash", map[string]any{"stdout": "foreground call"})
	shell := []map[string]any{{"id": "bash1", "type": "shell", "status": "running"}}
	if r := w.stopWith(coder, "coder", "Waiting for the build.", false, shell); r != "" {
		t.Fatalf("a stage waiting on its own task should be let stop: %s", r)
	}
	st := w.state()
	if d := st.byAgent(coder); d.Done || d.StopBlocks != 0 {
		t.Fatalf("a wait is not a report: done=%v blocks=%d", d.Done, d.StopBlocks)
	}

	// Someone else's task does not excuse the stop, and neither does a finished one.
	other := []map[string]any{{"id": "explorer1", "type": "subagent", "status": "running"}}
	if r := w.stopWith(coder, "coder", "Still waiting.", false, other); !strings.Contains(r, "no starship-report") {
		t.Fatalf("a task the stage does not own should not excuse it: %q", r)
	}
	if r := w.stopWith(coder, "coder", "The build finished.", true, nil); !strings.Contains(r, "no starship-report") {
		t.Fatalf("a task no longer in flight should not excuse it: %q", r)
	}
}

func TestHandbackIsTheReport(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	// A refused handback is refused at the call, with the reason, and can be resent.
	if r := w.handback(coder, "coder", "I did it, no report."); !strings.Contains(r, "no starship-report") {
		t.Fatalf("an invalid handback should be refused with the reason: %q", r)
	}
	if r := w.handback(coder, "coder", report("finished")); !strings.Contains(r, `"finished" is not one of`) {
		t.Fatalf("an unknown outcome should be refused: %q", r)
	}
	if d := w.state().byAgent(coder); d.Handback != "" || d.Done {
		t.Fatalf("a refused handback must not be recorded: %+v", d)
	}
	if r := w.handback(coder, "coder", report("compiled")); r != "" {
		t.Fatalf("a valid handback should be accepted: %s", r)
	}
	// The closing text is not the report.
	if r := w.stopWith(coder, "coder", "Closing text.", false, nil); r != "" {
		t.Fatalf("a handed-back stage should finish: %s", r)
	}
	if d := w.state().byAgent(coder); !d.Done || d.Outcome != "compiled" {
		t.Fatalf("the handback message should be the report: %+v", d)
	}
}

func TestReportInFinalMessageWithoutHandback(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	if d := w.state().byAgent("agent02"); d == nil || !d.Done || d.Outcome != "compiled" {
		t.Fatalf("the final message should be the report when there is no handback: %+v", d)
	}
}

func TestLeadHandbackWaitsForTheFinishedRun(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	if r := w.handback(lead, "orchestrator", "All done."); !strings.Contains(r, "not finished") {
		t.Fatalf("a lead handback before the run is finished should be refused: %q", r)
	}
	if d := w.state().byAgent(lead); d.Handback != "" {
		t.Fatalf("a refused lead handback must not be recorded: %+v", d)
	}
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
	if r := w.handback(lead, "orchestrator", "All done."); r != "" {
		t.Fatalf("a lead handback after the run finished should be accepted: %s", r)
	}
	if d := w.state().byAgent(lead); d.Handback != "All done." {
		t.Fatalf("an accepted lead handback must be recorded: %+v", d)
	}
}

func TestRunRefusesATaskWithNoGoalOrScope(t *testing.T) {
	w := newWorld(t)
	task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
	write(t, task, "---\npipeline: coding\n---\n\n# auth-refactor\n\n## Goal\n\n## Notes\n\nSome notes.\n")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdRun([]string{"supernova/auth-refactor"}, &out)
	if err == nil || !strings.Contains(err.Error(), `neither a "## Goal" nor a "## Scope"`) || !strings.Contains(err.Error(), task) {
		t.Fatalf("a task with no Goal or Scope should be refused: %v", err)
	}

	write(t, task, "---\npipeline: coding\n---\n\n## Scope\n\nOnly the multiply function.\n")
	w.run("supernova/auth-refactor", "--dry-run")
}

func TestRunRefusesAFreshTask(t *testing.T) {
	w := newWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdTask([]string{"new", "supernova/fresh", "--workspace", t.TempDir()}, &out); err != nil {
		t.Fatal(err)
	}
	err := cmdRun([]string{"supernova/fresh"}, &out)
	if err == nil || !strings.Contains(err.Error(), "has no work item") {
		t.Fatalf("a fresh task should be refused: %v", err)
	}
}

func TestSplitHistory(t *testing.T) {
	const rel = "projects/supernova/tasks/auth-refactor/"
	doc := "---\npipeline: coding\n---\n\n# t\n\n## Status\n\nNew.\n\n## Progress log\n\n- one.\n- two.\n\n## Outstanding\n\n- x\n"
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	setup := func(t *testing.T, content, autocrlf string) *world {
		w := newWorld(t)
		git(w.bridge, "init", "-q")
		git(w.bridge, "config", "core.autocrlf", autocrlf)
		write(t, filepath.Join(w.bridge, rel, "task.md"), content)
		git(w.bridge, "add", "-A")
		git(w.bridge, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "old bridge")
		t.Setenv("GIT_AUTHOR_NAME", "t")
		t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
		t.Setenv("GIT_COMMITTER_NAME", "t")
		t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
		return w
	}
	read := func(w *world, name string) string {
		b, _ := os.ReadFile(filepath.Join(w.bridge, rel, name))
		return string(b)
	}
	clean := func(t *testing.T, w *world) {
		t.Helper()
		if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", rel); dirty != "" {
			t.Errorf("the change should be committed: %q", dirty)
		}
	}
	wantTask := "---\npipeline: coding\n---\n\n# t\n\n## Status\n\nNew.\n\n## Outstanding\n\n- x\n"
	wantHistory := "# auth-refactor: history\n\n- one.\n- two.\n"

	t.Run("progress log is split out and committed", func(t *testing.T) {
		w := setup(t, doc, "false")
		splitHistory(w.bridge)
		if got := read(w, "task.md"); got != wantTask {
			t.Errorf("task.md:\n%q", got)
		}
		if got := read(w, "history.md"); got != wantHistory {
			t.Errorf("history.md:\n%q", got)
		}
		clean(t, w)
		if log, _ := gitOut(w.bridge, "log", "--format=%s", "-1"); log != "Move each task's progress log into history.md" {
			t.Errorf("last commit: %q", log)
		}
		splitHistory(w.bridge)
		if read(w, "task.md") != wantTask {
			t.Errorf("a second run should change nothing")
		}
	})
	t.Run("dirty task.md is left alone", func(t *testing.T) {
		w := setup(t, doc, "false")
		dirty := doc + "my edit\n"
		write(t, filepath.Join(w.bridge, rel, "task.md"), dirty)
		splitHistory(w.bridge)
		if read(w, "task.md") != dirty {
			t.Errorf("task.md should be unchanged")
		}
		if _, err := os.Stat(filepath.Join(w.bridge, rel, "history.md")); err == nil {
			t.Errorf("history.md should not be created")
		}
	})
	t.Run("existing history.md is left alone", func(t *testing.T) {
		w := setup(t, doc, "false")
		write(t, filepath.Join(w.bridge, rel, "history.md"), "mine\n")
		git(w.bridge, "add", "-A")
		git(w.bridge, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "history")
		splitHistory(w.bridge)
		if read(w, "task.md") != doc || read(w, "history.md") != "mine\n" {
			t.Errorf("both files should be unchanged")
		}
	})
	t.Run("task without a progress log is left alone", func(t *testing.T) {
		other := "---\npipeline: coding\n---\n\n# t\n\n## Status\n\nNew.\n"
		w := setup(t, other, "false")
		splitHistory(w.bridge)
		if read(w, "task.md") != other {
			t.Errorf("task.md should be unchanged")
		}
		if _, err := os.Stat(filepath.Join(w.bridge, rel, "history.md")); err == nil {
			t.Errorf("history.md should not be created")
		}
	})
	t.Run("a bridge that is not git is split without a commit", func(t *testing.T) {
		w := newWorld(t)
		write(t, filepath.Join(w.bridge, rel, "task.md"), doc)
		if note := splitHistory(w.bridge); note != "" {
			t.Errorf("note: %q", note)
		}
		if read(w, "task.md") != wantTask || read(w, "history.md") != wantHistory {
			t.Errorf("task.md:\n%q\nhistory.md:\n%q", read(w, "task.md"), read(w, "history.md"))
		}
	})
	t.Run("a task with both files returns a note", func(t *testing.T) {
		w := setup(t, doc, "false")
		write(t, filepath.Join(w.bridge, rel, "history.md"), "mine\n")
		if note := splitHistory(w.bridge); !strings.Contains(note, "merge them by hand") {
			t.Errorf("note: %q", note)
		}
	})
	t.Run("CRLF endings are kept", func(t *testing.T) {
		w := setup(t, crlf(doc), "false")
		splitHistory(w.bridge)
		if got := read(w, "task.md"); got != crlf(wantTask) {
			t.Errorf("task.md:\n%q", got)
		}
		if got := read(w, "history.md"); got != crlf(wantHistory) {
			t.Errorf("history.md:\n%q", got)
		}
		clean(t, w)
	})
	t.Run("CRLF working tree with autocrlf true is split and committed", func(t *testing.T) {
		w := setup(t, crlf(doc), "true")
		if dirty, _ := gitOut(w.bridge, "status", "--porcelain", "--", rel); dirty != "" {
			t.Fatalf("setup left task.md dirty: %q", dirty)
		}
		splitHistory(w.bridge)
		if got := read(w, "task.md"); got != crlf(wantTask) {
			t.Errorf("task.md:\n%q", got)
		}
		if got := read(w, "history.md"); got != crlf(wantHistory) {
			t.Errorf("history.md:\n%q", got)
		}
		clean(t, w)
		if blob, _ := gitOut(w.bridge, "show", "HEAD:"+rel+"history.md"); !strings.Contains(blob, "- one.") {
			t.Errorf("history.md not committed:\n%s", blob)
		}
	})
}

func TestRunSurvivesSessionIDChange(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	w.leaf(lead, "coder", "compiled")

	write(t, sessionFile(w.bridge, "s1")+".task", "proj/task\n")
	w.sid = "s2"
	w.leaf(lead, "reviewer", "clean")

	st := w.runState(run)
	if got := sessionTask(w.bridge, "s2"); got != "proj/task" {
		t.Errorf("task copied to s2 = %q", got)
	}
	if got := sessionTask(w.bridge, "s1"); got != "proj/task" {
		t.Errorf("s1 task file was disturbed: %q", got)
	}
	if st.Session != "s2" {
		t.Errorf("State.Session = %q, want s2", st.Session)
	}
	data, err := os.ReadFile(sessionFile(w.bridge, "s2"))
	if err != nil || strings.TrimSpace(string(data)) != run {
		t.Errorf("session s2 file = %q, %v; want %s", data, err, run)
	}
	rebound := 0
	for _, h := range st.History {
		if h.Kind == "rebound" && strings.Contains(h.Detail, "s1 -> s2") {
			rebound++
		}
	}
	if rebound != 1 {
		t.Errorf("want one rebound history entry, got %d: %v", rebound, st.History)
	}
	if got := w.state(); got.Run != run {
		t.Errorf("lookup by the new session found %s, want %s", got.Run, run)
	}

	// A concurrent hook already rebound the run: the binding file is gone here, but the run's
	// session is current, so no second entry is logged.
	os.Remove(sessionFile(w.bridge, "s2"))
	w.leaf(lead, "reviewer", "clean")
	rebound = 0
	for _, h := range w.runState(run).History {
		if h.Kind == "rebound" {
			rebound++
		}
	}
	if rebound != 1 {
		t.Errorf("want one rebound entry after a repeat, got %d", rebound)
	}
}

func TestUntiedAgentCannotSpawnStages(t *testing.T) {
	w := newWorld(t)
	w.startLead()
	w.sid = "s3"
	spawn := func(role string) (map[string]any, int) {
		return w.hook("PreToolUse", map[string]any{"agent_id": "ghost", "agent_type": "orchestrator",
			"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": role, "prompt": "x"}})
	}
	res, code := spawn("coder")
	if why := dig(res, "hookSpecificOutput", "permissionDecisionReason"); code != 0 || !strings.Contains(why, "can't be found") {
		t.Fatalf("stage spawn by an untied agent: %d %v", code, res)
	}
	if res, code := spawn("explorer"); code != 0 || res != nil {
		t.Fatalf("free role should pass: %d %v", code, res)
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s3")); err == nil {
		t.Errorf("an untied agent bound its session")
	}
}

func TestSessionStartCarriesBindingOver(t *testing.T) {
	w := newWorld(t)
	w.startLead()
	run := w.state().Run
	rec, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "s1"})
	write(t, w.session, string(rec)+"\n")

	w.sid = "s2"
	if _, code := w.hook("SessionStart", map[string]any{"source": "startup"}); code != 0 {
		t.Fatalf("SessionStart exited %d", code)
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s2")); err == nil {
		t.Fatalf("a fresh startup carried a binding over")
	}
	if _, code := w.hook("SessionStart", map[string]any{"source": "resume"}); code != 0 {
		t.Fatalf("SessionStart exited %d", code)
	}
	if got := w.state(); got.Run != run || got.Session != "s2" {
		t.Errorf("after resume: run %s session %s", got.Run, got.Session)
	}
}

func TestLastSeenAndStatus(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	if s := statusText(w.bridge, w.sid); !strings.Contains(s, "Lead last seen: not yet seen. Last activity: not yet seen.") {
		t.Errorf("status before any agent call:\n%s", s)
	}
	w.leaf(lead, "coder", "compiled")
	for _, d := range w.state().Dispatches {
		if d.Role == "orchestrator" && d.LastSeen == "" {
			t.Errorf("the lead's LastSeen was not set")
		}
	}
	s := statusText(w.bridge, w.sid)
	if !strings.Contains(s, "Lead last seen: ") || strings.Contains(s, "not yet seen") {
		t.Errorf("status after agent calls:\n%s", s)
	}
}
