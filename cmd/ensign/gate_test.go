// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// passPipeline takes the lead's run through every stage but leaves it at the closing gate.
func (w *world) passPipeline(lead string) {
	w.t.Helper()
	w.holdGate = true
	defer func() { w.holdGate = false }()
	w.passRun(lead)
}

// finishGoal runs `ensign run finish` for the run with the given arguments.
func (w *world) finishGoal(run string, args ...string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	err := cmdRunFinish(append([]string{"--bridge", w.bridge, "--run", run}, args...), &out, &bytes.Buffer{})
	return out.String(), err
}

func TestFirstStopAfterAStageReportsIsFreeAndCarriesNext(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	left := w.stopsLeft(lead)
	r := w.stop(lead, "orchestrator", "waiting", false)
	if !strings.Contains(r, "the pipeline is not finished") || !strings.Contains(r, `stage "review"`) || !strings.Contains(r, "Budgets used") {
		t.Fatalf("the free stop should carry the text ensign next prints: %q", r)
	}
	if got := w.stopsLeft(lead); got != left {
		t.Fatalf("the first stop after a stage reported is free: %d left, was %d", got, left)
	}
	r = w.stop(lead, "orchestrator", "waiting", false)
	if !strings.Contains(r, "Budgets used") || w.stopsLeft(lead) != left-1 {
		t.Fatalf("a repeat with nothing new is charged, and carries the text: %q, %d left", r, w.stopsLeft(lead))
	}
	w.leaf(lead, "reviewer", "clean")
	left = w.stopsLeft(lead)
	w.stop(lead, "orchestrator", "waiting", false)
	if w.stopsLeft(lead) != left {
		t.Fatalf("the next stage's report makes the next stop free again")
	}
}

func TestEnsignNextCountsAsTheLeadsCall(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	left := w.stopsLeft(lead)
	w.next()
	w.stop(lead, "orchestrator", "waiting", false)
	if got := w.stopsLeft(lead); got != left-1 {
		t.Fatalf("a stop after ensign next has nothing new to read and is charged: %d left, was %d", got, left)
	}
}

func TestLeadStoppedStillEndsALeadThatStopsWorking(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	if r := w.stop(lead, "orchestrator", "waiting", false); r == "" {
		t.Fatal("the free stop is blocked")
	}
	for i := 0; i < stopBudget-1; i++ {
		if r := w.stop(lead, "orchestrator", "waiting", false); !strings.Contains(r, "the pipeline is not finished") {
			t.Fatalf("stop %d should be blocked: %q", i+1, r)
		}
	}
	if r := w.stop(lead, "orchestrator", "waiting", false); r != "" {
		t.Fatalf("the third charged stop passes: %s", r)
	}
	w.wantFailed("lead-stopped")
}

func TestPassingPipelineWaitsAtTheClosingGate(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	st := w.state()
	if st.Status != "running" || st.Finished || !st.atGate() {
		t.Fatalf("the run should wait at the gate: %s %+v", st.Status, st)
	}
	run := st.Run
	left := w.stopsLeft(lead)
	r := w.stop(lead, "orchestrator", "done", false)
	if !strings.Contains(r, "--goal met") {
		t.Fatalf("the first stop at the gate is free and says what to run: %q", r)
	}
	if w.stopsLeft(lead) != left {
		t.Fatalf("the free stop at the gate is not charged")
	}
	next := w.next()
	for _, want := range []string{"pipeline has passed", "ensign run finish --bridge " + shellQuote(w.bridge) + " --run " + run + " --goal met", "--goal not-met"} {
		if !strings.Contains(next, want) || !strings.Contains(r, want) {
			t.Errorf("next and the stop reply lack %q:\n%s\n%s", want, next, r)
		}
	}
	if r := w.handback(lead, "orchestrator", "All done."); !strings.Contains(r, "not finished") {
		t.Fatalf("a handback at the gate is refused: %q", r)
	}
	if _, why := w.spawn(lead, "orchestrator", "coder"); why == "" {
		t.Fatal("nothing can be dispatched at the gate")
	}
}

func TestGoalMetFinishesTheRunAsSuccess(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	run := w.state().Run
	out, err := w.finishGoal(run, "--goal", "met")
	if err != nil || !strings.Contains(out, "success") {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	st := w.runState(run)
	if st.Status != "success" || !st.Finished {
		t.Fatalf("status %s finished=%v", st.Status, st.Finished)
	}
	if _, err := w.finishGoal(run, "--goal", "met"); err == nil || !strings.Contains(err.Error(), "finished") {
		t.Fatalf("a finished run refuses a second call: %v", err)
	}
	if r := w.handback(lead, "orchestrator", "All done."); r != "" {
		t.Fatalf("the lead's handback works after the finish: %s", r)
	}
}

func TestGoalNotMetFinishesTheRunAsAnError(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	run := w.state().Run
	if _, err := w.finishGoal(run, "--goal", "not-met"); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("not-met needs a why: %v", err)
	}
	if !w.runState(run).atGate() {
		t.Fatal("a refused call leaves the run at the gate")
	}
	if _, err := w.finishGoal(run, "--goal", "not-met", "the", "retry", "path", "is", "unfixed"); err != nil {
		t.Fatal(err)
	}
	st := w.runState(run)
	if st.Status != "error" || st.Reason != "goal-not-met: the retry path is unfixed" {
		t.Fatalf("status %s reason %q", st.Status, st.Reason)
	}
}

func TestRunFinishIsRefusedOutsideTheGate(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	if _, err := w.finishGoal(run, "--goal", "met"); err == nil || !strings.Contains(err.Error(), "not at its closing gate") {
		t.Fatalf("a running run is refused: %v", err)
	}
	w.passPipeline(lead)
	if _, err := w.finishGoal(run); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("a missing goal is refused: %v", err)
	}
	if _, err := w.finishGoal(run, "--goal", "maybe"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("an unknown goal is refused: %v", err)
	}
}

func TestSequenceAdvancesOnlyWhenTheLeadFinishesTheRun(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.goContext()
	lead := w.leadOf(w.stepRuns(1)[0])
	w.passPipeline(lead)
	if st := w.step(1); st.Status != "active" {
		t.Fatalf("the step stays active at the gate: %+v", st)
	}
	if st := w.step(2); st.Status == "active" {
		t.Fatalf("the next step must not start at the gate: %+v", st)
	}
	if _, err := w.finishGoal(w.stepRuns(1)[0], "--goal", "met"); err != nil {
		t.Fatal(err)
	}
	if st := w.step(1); st.Status != "done" {
		t.Fatalf("the step is done once the lead finishes: %+v", st)
	}
	if st := w.step(2); st.Status != "active" {
		t.Fatalf("the next step starts once the lead finishes: %+v", st)
	}
}

func (w *world) tool(agent, agentType, tool string, input map[string]any) map[string]any {
	w.t.Helper()
	res, code := w.hook("PreToolUse", map[string]any{"agent_id": agent, "agent_type": agentType, "tool_name": tool, "tool_input": input})
	if code != 0 {
		w.t.Fatalf("PreToolUse exited %d: %v", code, res)
	}
	return res
}

func TestAgentsOfAFinishedRunAreDeniedEveryTool(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	coder := "agent02"
	w.leaf(lead, "coder", "compiled")
	calls := map[string]map[string]any{
		"Bash": {"command": "ls"}, "Read": {"file_path": filepath.Join(w.workspace(), "a.go")},
		"Grep": {"pattern": "x"}, "Glob": {"pattern": "*"},
	}
	for tool, input := range calls {
		if res := w.tool(lead, "orchestrator", tool, input); dig(res, "hookSpecificOutput", "permissionDecision") == "deny" {
			t.Fatalf("%s of a live run's lead is denied: %v", tool, res)
		}
	}
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
	run := w.state().Run
	check := func(when string) {
		t.Helper()
		for tool, input := range calls {
			for agent, typ := range map[string]string{lead: "orchestrator", coder: "coder"} {
				res := w.tool(agent, typ, tool, input)
				why := dig(res, "hookSpecificOutput", "permissionDecisionReason")
				if dig(res, "hookSpecificOutput", "permissionDecision") != "deny" || !strings.Contains(why, run) || !strings.Contains(why, "success") || !strings.Contains(why, "Stop") {
					t.Errorf("%s: %s by %s should be denied naming the run and its status: %v", when, tool, typ, res)
				}
			}
		}
		if res := w.tool("", "", "Bash", map[string]any{"command": "ls"}); res != nil {
			t.Errorf("%s: the main session is not affected: %v", when, res)
		}
	}
	check("after finish")
	if r := w.handback(lead, "orchestrator", "All done."); r != "" {
		t.Fatalf("the lead's handback still works: %s", r)
	}
	// The session moves on to a retry; the old run's agents are still refused.
	w.run("supernova/auth-refactor")
	if cur := w.state().Run; cur == run {
		t.Fatalf("the session should have moved to a new run")
	}
	check("after the session moved to another run")
}

func TestEvidenceFolderIsWritableAndRestOfReportsIsNot(t *testing.T) {
	w, _ := newGuardWorld(t)
	lead := w.startLead()
	run := w.state().Run
	evidence := filepath.Join(runsDir(w.bridge), run, "reports", "evidence")
	if fi, err := os.Stat(evidence); err != nil || !fi.IsDir() {
		t.Fatalf("a run is created with its evidence folder: %v", err)
	}
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	reports := filepath.Join(runsDir(w.bridge), run, "reports")
	for agent, typ := range map[string]string{lead: "orchestrator", coder: "coder"} {
		w.expectEdit(agent, typ, filepath.Join(evidence, "probe.log"), false)
		w.expectEdit(agent, typ, filepath.Join(evidence, "sub", "probe.log"), false)
		w.expectEdit(agent, typ, filepath.Join(reports, "d1.md"), true)
		w.expectEdit(agent, typ, filepath.Join(reports, "evidence-not.md"), true)
	}
}

func TestEvidenceSurvivesTheRunEnd(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	evidence := filepath.Join(runsDir(w.bridge), run, "reports", "evidence", "probe.log")
	scratch := filepath.Join(runsDir(w.bridge), run, "scratch", "note")
	write(t, evidence, "probe")
	write(t, scratch, "note")
	w.passRun(lead)
	if w.runState(run).Status != "success" {
		t.Fatal("the run should have finished")
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Errorf("evidence is kept: %v", err)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Errorf("scratch is emptied")
	}
}

func TestWithdrawFreshRetiresPendingRuns(t *testing.T) {
	w := newWorld(t)
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	err = withdrawFresh(w.bridge, []*Snapshot{snap}, os.ErrInvalid)
	if err != os.ErrInvalid {
		t.Fatalf("a clean withdrawal returns the error as it was: %v", err)
	}
	if st := w.runState(snap.Run); st.Status != "error" || !strings.HasPrefix(st.Reason, "withdrawn: ") {
		t.Fatalf("the fresh run should be withdrawn: %s %q", st.Status, st.Reason)
	}
}

func TestLeadHandbackAtTheGateRightAfterTheTesterReportsIsFree(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	left := w.stopsLeft(lead)
	r := w.handback(lead, "orchestrator", "All done.")
	if !strings.Contains(r, "the pipeline is not finished") || !strings.Contains(r, "--goal met") {
		t.Fatalf("the free handback should carry the text ensign next prints: %q", r)
	}
	if got := w.stopsLeft(lead); got != left {
		t.Fatalf("the first handback after a stage reported is free: %d left, was %d", got, left)
	}
	r = w.handback(lead, "orchestrator", "All done.")
	if !strings.Contains(r, "--goal met") || w.stopsLeft(lead) != left-1 {
		t.Fatalf("a repeat is charged and carries the text: %q, %d left", r, w.stopsLeft(lead))
	}
}

func TestExhaustedBudgetFinishingSuccessWaitsAtTheGate(t *testing.T) {
	w := newWorld(t)
	w.project(`  coding2: {}
`)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "      - set:    { budgets.test: 2 }\n",
		"      - set:    { budgets.test: 1 }\n      - set:    { on-exhausted.test: { finish: success } }\n", 1))
	lead := w.startLead("--start", "test")
	w.holdGate = true
	w.leaf(lead, "tester", "fail")
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "fail")
	st := w.state()
	if st.Status != "running" || !st.atGate() {
		t.Fatalf("an exhausted budget that finishes with success waits at the gate: %s %+v", st.Status, st)
	}
}
