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

// stopRun calls `ensign run stop` from the bridge, as session sid, and returns its output and error.
func (w *world) stopRun(args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdRun(append([]string{"stop"}, args...), &out)
	return out.String(), err
}

func (w *world) journal() string {
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md"))
	return string(data)
}

// inFlight starts a run with a lead, a coder that has reported and two reviewers still running.
func (w *world) inFlight() (run, lead, rev string) {
	w.t.Helper()
	lead = w.startLead()
	run = w.state().Run
	w.leaf(lead, "coder", "compiled")
	rev, why := w.spawn(lead, "orchestrator", "reviewer")
	if rev == "" {
		w.t.Fatalf("reviewer refused: %s", why)
	}
	if id, why := w.spawn(lead, "orchestrator", "reviewer"); id == "" {
		w.t.Fatalf("reviewer refused: %s", why)
	}
	return run, lead, rev
}

func TestStopEndsTheSessionsOwnRun(t *testing.T) {
	w := newWorld(t)
	run, _, _ := w.inFlight()
	out, err := w.stopRun(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Stopped run " + run, "Free now: " + w.workspace(), "ignored"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	st := w.runState(run)
	if st.Status != "error" || !strings.HasPrefix(st.Reason, "cancelled: ") || !strings.Contains(st.Reason, "s1") || !st.Finished || st.FinishedAt == 0 {
		t.Fatalf("stopped run: %s %q finished=%v", st.Status, st.Reason, st.Finished)
	}
	for _, d := range st.Dispatches {
		if !d.Done {
			t.Errorf("dispatch %s is not done", d.ID)
		}
	}
	if ids, _ := sessionRuns(w.bridge, w.sid); len(ids) != 0 {
		t.Errorf("the session still lists %v", ids)
	}
	if err := folderHolder(w.bridge, &Snapshot{Workspace: w.workspace()}, "s2"); err != nil {
		t.Errorf("the folder should be free: %v", err)
	}
	if j := w.journal(); !strings.Contains(j, run) || !strings.Contains(j, "cancelled") {
		t.Errorf("the journal doesn't name the stopped run:\n%s", j)
	}
}

func TestStopRefusalsChangeNothing(t *testing.T) {
	w := newWorld(t)
	run, _, _ := w.inFlight()
	before := w.runState(run).Reason + w.runState(run).Status
	if _, err := w.stopRun(); err == nil || !strings.Contains(err.Error(), "usage: ensign run stop <run>") {
		t.Errorf("no argument: %v", err)
	}
	if _, err := w.stopRun("-x"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("a flag: %v", err)
	}
	if _, err := w.stopRun("no-such-run"); err == nil || !strings.Contains(err.Error(), "no run") {
		t.Errorf("unknown run: %v", err)
	}
	other := "other-run"
	holdingRun(t, w, other, "s2", &Snapshot{Workspace: t.TempDir()})
	if _, err := w.stopRun(other); err == nil || !strings.Contains(err.Error(), "another session") || strings.Contains(err.Error(), "retire") {
		t.Errorf("another session's run: %v", err)
	}
	if st := w.runState(other); st.Status != "running" || st.Finished {
		t.Errorf("another session's run changed: %s", st.Status)
	}
	queued := "queued-run"
	holdingRun(t, w, queued, "s1", &Snapshot{Workspace: t.TempDir()})
	r, err := openRun(w.bridge, queued)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Sequence, r.State.Status, r.State.Approved = true, "pending", false
	r.save()
	r.close()
	if _, err := w.stopRun(queued); err == nil || !strings.Contains(err.Error(), "sequence halt") {
		t.Errorf("queued run: %v", err)
	}
	if st := w.runState(queued); st.Status != "pending" {
		t.Errorf("queued run changed: %s", st.Status)
	}
	t.Setenv("STARSHIP_SESSION", "")
	if _, err := w.stopRun(run); err == nil || !strings.Contains(err.Error(), "STARSHIP_SESSION") {
		t.Errorf("no session: %v", err)
	}
	t.Setenv("STARSHIP_SESSION", "s1")
	if st := w.runState(run); st.Status+st.Reason != before || st.Finished {
		t.Errorf("a refusal changed the run: %s %s", st.Status, st.Reason)
	}
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	if _, err := w.stopRun(run); err == nil || !strings.Contains(err.Error(), "has finished") {
		t.Errorf("second stop: %v", err)
	}
}

func TestStopDropsLateReports(t *testing.T) {
	w := newWorld(t)
	run, lead, rev := w.inFlight()
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	want := w.runState(run)
	same := func(when string) {
		t.Helper()
		st := w.runState(run)
		if st.Status != want.Status || st.Reason != want.Reason || st.FinishedAt != want.FinishedAt {
			t.Errorf("%s changed the run: %s %q", when, st.Status, st.Reason)
		}
	}
	if r := w.stop(rev, "reviewer", "no report at all", false); r != "" {
		t.Errorf("a bad report via SubagentStop blocked: %s", r)
	}
	same("a bad report via SubagentStop")
	if r := w.handback(rev, "reviewer", "no report at all"); r != "" {
		t.Errorf("a bad report via hand-back was refused: %s", r)
	}
	same("a bad report via hand-back")
	if r := w.handback(rev, "reviewer", report("clean")); r != "" {
		t.Errorf("a valid report via hand-back was refused: %s", r)
	}
	if r := w.stop(rev, "reviewer", report("clean"), false); r != "" {
		t.Errorf("a valid report via SubagentStop blocked: %s", r)
	}
	same("a valid report")
	if r := w.handback(lead, "orchestrator", "done"); r != "" {
		t.Errorf("the lead's hand-back was refused: %s", r)
	}
	if r := w.stop(lead, "orchestrator", "done", false); r != "" {
		t.Errorf("the lead's SubagentStop blocked: %s", r)
	}
	same("the lead stopping")
}

func TestStopConfinesAStrayCoder(t *testing.T) {
	w, _ := newGuardWorld(t)
	lead := w.startLead()
	run := w.state().Run
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	main := filepath.Join(w.workspace(), "main.go")
	w.expectEdit(coder, "coder", main, false)
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	if refused, why := w.tryEdit(coder, "coder", main); !refused {
		t.Errorf("a stray coder edited the code folder: %s", why)
	}
}

// strayTesterWorld stops a run with its tester in flight, then brings a second run to its test stage. It returns the world, the stray tester, the second run and its real tester.
func strayTesterWorld(t *testing.T) (w *world, stray, runB, tester string) {
	t.Helper()
	w = newWorld(t)
	lead := w.startLead()
	runA := w.state().Run
	w.toTest(lead)
	stray, why := w.spawn(lead, "orchestrator", "tester")
	if stray == "" {
		t.Fatalf("tester refused: %s", why)
	}
	if _, err := w.stopRun(runA); err != nil {
		t.Fatal(err)
	}
	lead = w.startLead()
	runB = w.state().Run
	if runB == runA {
		t.Fatal("the second run reused the first run's id")
	}
	w.toTest(lead)
	tester, why = w.spawn(lead, "orchestrator", "tester")
	if tester == "" {
		t.Fatalf("second tester refused: %s", why)
	}
	return w, stray, runB, tester
}

func TestAStrayOfAStoppedRunDoesNotBindToTheNextRun(t *testing.T) {
	w, stray, runB, tester := strayTesterWorld(t)
	before := w.runState(runB)
	last := func(st *State) *Dispatch { return st.Dispatches[len(st.Dispatches)-1] }
	if d := last(before); d.Role != "tester" || d.AgentID != "" || d.Done {
		t.Fatalf("the second run's tester dispatch: %+v", d)
	}
	if r := w.handback(stray, "tester", report("pass")); r != "" {
		t.Errorf("the stray's hand-back was refused: %s", r)
	}
	if r := w.stop(stray, "tester", report("pass"), false); r != "" {
		t.Errorf("the stray's stop blocked: %s", r)
	}
	st := w.runState(runB)
	if d := last(st); d.AgentID != "" || d.Done {
		t.Errorf("the stray took the second run's dispatch: %+v", d)
	}
	if st.Status != before.Status || st.Reason != before.Reason || len(st.Dispatches) != len(before.Dispatches) {
		t.Errorf("the stray changed the second run: %s %q", st.Status, st.Reason)
	}
	if st.Scopes[""].Stage != before.Scopes[""].Stage {
		t.Errorf("the stray moved the second run to %q", st.Scopes[""].Stage)
	}
	if r := w.handback(tester, "tester", report("pass")); r != "" {
		t.Errorf("the real tester's hand-back was refused: %s", r)
	}
	if r := w.stop(tester, "tester", report("pass"), false); r != "" {
		t.Errorf("the real tester's stop blocked: %s", r)
	}
	if st := w.runState(runB); st.Status != "success" || !last(st).Done || last(st).AgentID != tester {
		t.Errorf("the real tester did not complete the run: %s %q %+v", st.Status, st.Reason, last(st))
	}
}

func TestAnAgentWithNoReadableTagBindsByRole(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.toTest(lead)
	tester, why := w.spawn(lead, "orchestrator", "tester")
	if tester == "" {
		t.Fatalf("tester refused: %s", why)
	}
	if err := os.Remove(agentTranscript(w.session, tester)); err != nil {
		t.Fatal(err)
	}
	if r := w.handback(tester, "tester", report("pass")); r != "" {
		t.Errorf("the hand-back was refused: %s", r)
	}
	st := w.state()
	if d := st.Dispatches[len(st.Dispatches)-1]; d.AgentID != tester || d.Handback == "" {
		t.Errorf("the untagged tester was not bound by role: %+v", d)
	}
}

func TestPlainStageFinishingAfterTheStopLeavesTheRunStopped(t *testing.T) {
	w, run, release, wait := gatedPlainRun(t)
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	want := w.runState(run)
	release()
	if res := wait(); !strings.Contains(res, "The run is finished: error (cancelled") {
		t.Errorf("the reply doesn't say the run was stopped: %v", res)
	}
	st := w.runState(run)
	if st.Status != want.Status || st.Reason != want.Reason || st.FinishedAt != want.FinishedAt || !strings.HasPrefix(st.Reason, "cancelled: ") {
		t.Errorf("the plain stage changed the stopped run: %s %q", st.Status, st.Reason)
	}
	if d := st.Dispatches[len(st.Dispatches)-1]; d.Outcome != "" {
		t.Errorf("the plain stage's outcome was applied: %+v", d)
	}
}

func TestStopWithAnOlderTerminalRunListed(t *testing.T) {
	w := newWorld(t)
	run, _, rev := w.inFlight()
	old := "old-run"
	holdingRun(t, w, old, "s1", &Snapshot{Workspace: t.TempDir()})
	r, err := openRun(w.bridge, old)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Finished = "success", true
	r.save()
	r.close()
	if err := editSessionRuns(w.bridge, w.sid, func(ids []string) []string { return append([]string{old}, ids...) }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	want := w.runState(run)
	if r := w.stop(rev, "reviewer", "no report", false); r != "" {
		t.Errorf("a late report blocked: %s", r)
	}
	if r := w.handback(rev, "reviewer", "no report"); r != "" {
		t.Errorf("a late hand-back was refused: %s", r)
	}
	if st := w.runState(run); st.Reason != want.Reason || st.FinishedAt != want.FinishedAt {
		t.Errorf("a late report changed the run: %q", st.Reason)
	}
}

func TestStopInAHaltedSequenceCancelsTheStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	w.spawn("", "", "orchestrator")
	run := w.step(1).Run
	w.mustSeq("halt", "--reason", "enough")
	w.wantStatus("active")
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	w.wantStatus("cancelled")
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("a closed sequence must not hold the turn: %v", res)
	}
}

func TestStopInAnUnhaltedSequenceLeavesTheStepToDecide(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	w.spawn("", "", "orchestrator")
	run := w.step(1).Run
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	w.wantStatus("decide")
}

func TestRetireRefusalPointsAtStop(t *testing.T) {
	w := newWorld(t)
	run, _, _ := w.inFlight()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	err := cmdRun([]string{"retire", run}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "this session") || !strings.Contains(err.Error(), "ensign run stop "+run) {
		t.Fatalf("retire of the session's own run: %v", err)
	}
}

func TestLeadStoppingWithNothingRunningFailsTheRun(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	for i := 0; i < stopBudget-1; i++ {
		if r := w.stop(lead, "orchestrator", "waiting", false); !strings.Contains(r, "the pipeline is not finished") {
			t.Fatalf("an early stop should be blocked: %q", r)
		}
	}
	if r := w.stop(lead, "orchestrator", "waiting", false); r != "" {
		t.Fatalf("the stop that spends the budget should pass: %s", r)
	}
	w.wantFailed("lead-stopped")
	for _, d := range w.state().Dispatches {
		if d.Role == "orchestrator" && !d.Done {
			t.Fatalf("the lead should be done: %+v", d)
		}
	}
}
