package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// world is a copy of the fixture bridge with a fake Claude Code session around it.
type world struct {
	t       *testing.T
	bridge  string
	session string // session transcript path; subagent transcripts are derived from it
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
	task := filepath.Join(bridge, "projects/supernova/tasks/auth-refactor/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, strings.Replace(string(data), "WORKSPACE", ws, 1))
	t.Setenv("CLAUDE_PROJECT_DIR", bridge)
	return &world{t: t, bridge: bridge, session: filepath.Join(tmp, "session.jsonl")}
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

// stop ends an agent with a final message; it returns the block reason, or "".
func (w *world) stop(agent, agentType, message string, active bool) string {
	w.t.Helper()
	res, code := w.hook("SubagentStop", map[string]any{
		"agent_id": agent, "agent_type": agentType, "last_assistant_message": message,
		"agent_transcript_path": agentTranscript(w.session, agent), "stop_hook_active": active,
	})
	if code != 0 {
		w.t.Fatalf("SubagentStop exited %d: %v", code, res)
	}
	if res != nil && res["decision"] == "block" {
		return res["reason"].(string)
	}
	return ""
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
	r, err := openActiveRun(w.bridge)
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
	if _, err := os.Stat(activeRunFile(w.bridge)); err == nil {
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
	id, _ := os.ReadFile(activeRunFile(w.bridge))
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
