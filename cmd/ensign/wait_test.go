// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type waitResult struct {
	out string
	err error
}

// startWait runs `ensign wait` for the run in the background and returns its result channel.
func (w *world) startWait(run string) chan waitResult {
	w.t.Helper()
	old := waitPoll
	waitPoll = 5 * time.Millisecond
	w.t.Cleanup(func() { waitPoll = old })
	done := make(chan waitResult, 1)
	go func() {
		var out bytes.Buffer
		err := cmdWait([]string{"--bridge", w.bridge, "--run", run}, &out)
		done <- waitResult{out.String(), err}
	}()
	return done
}

func (w *world) wantWaiting(done chan waitResult, why string) {
	w.t.Helper()
	select {
	case r := <-done:
		w.t.Fatalf("ensign wait returned while %s: %q %v", why, r.out, r.err)
	case <-time.After(100 * time.Millisecond):
	}
}

func (w *world) wantWaitEnds(done chan waitResult, want string) {
	w.t.Helper()
	select {
	case r := <-done:
		if r.err != nil || !strings.Contains(r.out, want) || !strings.Contains(r.out, "ensign next") {
			w.t.Fatalf("ensign wait ended with %q, %v; want %q", r.out, r.err, want)
		}
	case <-time.After(5 * time.Second):
		w.t.Fatal("ensign wait did not end")
	}
}

// helper records a background helper agent the lead started.
func (w *world) helper(lead, id string) {
	w.t.Helper()
	w.launched(lead, "orchestrator", "Agent", map[string]any{"status": "async_launched", "agentId": id})
}

func TestWaitWithNothingRunningEndsAtOnce(t *testing.T) {
	w := newWorld(t)
	w.startLead()
	w.wantWaitEnds(w.startWait(w.state().Run), "No agent you started is still running")
}

func TestWaitForAMissingRunIsAnError(t *testing.T) {
	w := newWorld(t)
	if err := cmdWait([]string{"--bridge", w.bridge, "--run", "no-such-run"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a missing run should be an error")
	}
}

func TestWaitEndsWhenTheStageAgentFinishes(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	done := w.startWait(run)
	w.wantWaiting(done, "the coder runs")
	if r := w.stop(coder, "coder", report("compiled"), false); r != "" {
		t.Fatal(r)
	}
	w.wantWaitEnds(done, "No agent you started is running any more")
}

func TestWaitForAParallelStageNeedsEveryBranch(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	w.leaf(lead, "coder", "compiled")
	var revs []string
	for i := 0; i < 2; i++ {
		id, why := w.spawn(lead, "orchestrator", "reviewer")
		if id == "" {
			t.Fatal(why)
		}
		revs = append(revs, id)
	}
	done := w.startWait(run)
	w.wantWaiting(done, "both reviewers run")
	w.stop(revs[0], "reviewer", report("clean"), false)
	w.wantWaiting(done, "one reviewer runs")
	w.stop(revs[1], "reviewer", report("clean"), false)
	w.wantWaitEnds(done, "No agent you started is running any more")
}

func TestWaitEndsWhenTheRunIsFinished(t *testing.T) {
	w := newWorld(t)
	run, _, _ := w.inFlight()
	done := w.startWait(run)
	w.wantWaiting(done, "reviewers run")
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	w.wantWaitEnds(done, "The run is finished")
}

func TestWaitHoldsNoLockBetweenPolls(t *testing.T) {
	w := newWorld(t)
	run, _, _ := w.inFlight()
	done := w.startWait(run)
	w.wantWaiting(done, "reviewers run")
	r, err := openRun(w.bridge, run)
	if err != nil {
		t.Fatalf("the run's lock should be free while ensign wait polls: %v", err)
	}
	r.close()
	w.wantWaiting(done, "reviewers run")
	if _, err := w.stopRun(run); err != nil {
		t.Fatal(err)
	}
	w.wantWaitEnds(done, "The run is finished")
}

func TestWaitGivesUpOnStateThatStaysUnreadable(t *testing.T) {
	w := newWorld(t)
	run, _, _ := w.inFlight()
	old := waitUnreadable
	waitUnreadable = 50 * time.Millisecond
	t.Cleanup(func() { waitUnreadable = old })
	done := w.startWait(run)
	w.wantWaiting(done, "reviewers run")
	if err := os.WriteFile(filepath.Join(runsDir(w.bridge), run, "state.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err == nil || !strings.Contains(r.err.Error(), "could not be read") {
			t.Fatalf("want an unreadable-state error, got %q %v", r.out, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ensign wait did not give up")
	}
}

func TestHelperFinishesByHandbackStopOrTaskStop(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	w.helper(lead, "h1")
	if !w.state().Helpers["h1"] {
		t.Fatalf("a launched helper is tracked: %v", w.state().Helpers)
	}
	done := w.startWait(run)
	w.wantWaiting(done, "a helper runs")

	if r := w.handback("h1", "general-purpose", "found it"); r != "" {
		t.Fatalf("a helper's handback is let through: %s", r)
	}
	w.wantWaitEnds(done, "No agent you started is running any more")

	w.helper(lead, "h2")
	if r := w.stopWith("h2", "general-purpose", "done", false, nil); r != "" {
		t.Fatal(r)
	}
	if len(w.state().Helpers) != 0 {
		t.Fatalf("a helper that stopped is finished: %v", w.state().Helpers)
	}

	w.helper(lead, "h3")
	w.hookPost(lead, "orchestrator", "TaskStop", map[string]any{"task_id": "h3"})
	if len(w.state().Helpers) != 0 {
		t.Fatalf("a helper the lead stopped is finished: %v", w.state().Helpers)
	}
}

func TestPausedHelperIsNotFinished(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	w.helper(lead, "h1")
	w.launched("h1", "general-purpose", "Bash", map[string]any{"backgroundTaskId": "hb1"})
	st := w.state()
	if st.Tasks["hb1"] != "h1" || !st.Shells["hb1"] || len(st.TaskFiles) != 0 {
		t.Fatalf("a helper's launch is recorded under its id and leaves no task file: %v %v %v", st.Tasks, st.Shells, st.TaskFiles)
	}
	paused := []map[string]any{{"id": "hb1", "type": "shell", "status": "running"}}
	if r := w.stopWith("h1", "general-purpose", "waiting", false, paused); r != "" {
		t.Fatal(r)
	}
	done := w.startWait(run)
	w.wantWaiting(done, "a helper is paused")
	if r := w.stopWith("h1", "general-purpose", "done", false, nil); r != "" {
		t.Fatal(r)
	}
	w.wantWaitEnds(done, "No agent you started is running any more")
}

func TestPausedHelperThatHandsBackIsFinished(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	w.launched("h1", "general-purpose", "Bash", map[string]any{"backgroundTaskId": "hb1"})
	paused := []map[string]any{{"id": "hb1", "type": "shell", "status": "running"}}
	w.stopWith("h1", "general-purpose", "waiting", false, paused)
	if !w.state().Helpers["h1"] {
		t.Fatal("a paused helper is not finished")
	}
	w.handback("h1", "general-purpose", "done")
	if len(w.state().Helpers) != 0 {
		t.Fatalf("a handback finishes a paused helper: %v", w.state().Helpers)
	}
}

func TestHelperTrackingWithSeveralLiveRuns(t *testing.T) {
	tmp := useTempDir(t)
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	w.helper(lead, "h1")
	w.helper(lead, "explorer1")
	w.liveStepRun(filepath.Join(tmp, "other-folder"), "")
	if n := len(mustLive(t, w.bridge, w.sid)); n < 2 {
		t.Fatalf("want two live runs, got %d", n)
	}
	w.launched("h1", "general-purpose", "Bash", map[string]any{"backgroundTaskId": "hb1"})
	if st := w.runState(run); st.Tasks["hb1"] != "h1" {
		t.Fatalf("a helper's launch is recorded in the run that started it: %v", st.Tasks)
	}
	paused := []map[string]any{{"id": "hb1", "type": "shell", "status": "running"}}
	w.stopWith("h1", "general-purpose", "waiting", false, paused)
	if !w.runState(run).Helpers["h1"] {
		t.Fatal("a paused helper is not finished")
	}
	w.stopWith("h1", "general-purpose", "done", false, nil)
	if w.runState(run).Helpers["h1"] {
		t.Fatalf("a helper that stopped is finished: %v", w.runState(run).Helpers)
	}
	if r := w.handback("explorer1", "explorer", "found it"); r != "" {
		t.Fatalf("a free-role helper's handback is let through: %s", r)
	}
	if st := w.runState(run); len(st.Helpers) != 0 {
		t.Fatalf("a free-role helper that handed back is finished: %v", st.Helpers)
	}
}

func TestLeadWithAnAgentRunningNeedsItsOwnWait(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	agent := []map[string]any{{"id": "h1", "type": "subagent", "status": "running"}}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "ensign wait --bridge") || !strings.Contains(r, "run_in_background") {
		t.Fatalf("the block should name the command: %q", r)
	}
	if left := w.stopsLeft(lead); left != stopBudget {
		t.Fatalf("the first block is free: %d left", left)
	}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "ensign wait --bridge") {
		t.Fatalf("a repeat is blocked too: %q", r)
	}
	if left := w.stopsLeft(lead); left != stopBudget-1 {
		t.Fatalf("a repeat is charged: %d left", left)
	}
	w.helper(lead, "h2")
	agent = append(agent, map[string]any{"id": "h2", "type": "subagent", "status": "running"})
	w.stopWith(lead, "orchestrator", "waiting", false, agent)
	if left := w.stopsLeft(lead); left != stopBudget-1 {
		t.Fatalf("a new launch makes the next block free again: %d left", left)
	}
	w.stopWith(lead, "orchestrator", "waiting", false, agent)
	if left := w.stopsLeft(lead); left != stopBudget-2 {
		t.Fatalf("the repeat is charged: %d left", left)
	}
}

func TestLeadWithAnAgentRunningFailsWhenTheBudgetIsSpent(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	agent := []map[string]any{{"id": "h1", "type": "subagent", "status": "running"}}
	for i := 0; i < stopBudget; i++ {
		if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "ensign wait") {
			t.Fatalf("stop %d should be blocked: %q", i, r)
		}
	}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); r != "" {
		t.Fatalf("the stop that spends the budget passes: %s", r)
	}
	w.wantFailed("lead-stopped")
}

func TestLeadOwningAShellMayStopFree(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	w.launched(lead, "orchestrator", "Bash", map[string]any{"backgroundTaskId": "wait1"})
	shell := []map[string]any{{"id": "wait1", "type": "shell", "status": "running"}}
	for i := 0; i < stopBudget+1; i++ {
		if r := w.stopWith(lead, "orchestrator", "waiting", false, shell); r != "" {
			t.Fatalf("a lead with its own shell running may stop: %s", r)
		}
	}
	if left := w.stopsLeft(lead); left != stopBudget {
		t.Fatalf("the stops are free: %d left", left)
	}
	// A shell of another agent does not count.
	other := []map[string]any{{"id": "bash9", "type": "shell", "status": "running"}}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, other); !strings.Contains(r, "ensign wait --bridge") {
		t.Fatalf("a shell the lead did not start does not excuse it: %q", r)
	}
}

func TestNextTellsTheLeadToRunEnsignWait(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	want := "ensign wait --bridge " + shellQuote(w.bridge) + " --run " + w.state().Run
	check := func(got, prefix string) {
		t.Helper()
		if !strings.HasPrefix(got, prefix) || !strings.Contains(got, want) || !strings.Contains(got, "run_in_background") {
			t.Fatalf("next should start with %q and give the wait command %q: %s", prefix, want, got)
		}
	}
	check(w.next(), `Dispatch stage "code": one coder agent.`)
	if id, why := w.spawn(lead, "orchestrator", "coder"); id == "" {
		t.Fatal(why)
	}
	check(w.next(), `Wait: stage "code" (coder) is still running.`)
}

func TestNextTellsTheLeadOfAParallelStageToRunEnsignWait(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	want := "ensign wait --bridge " + shellQuote(w.bridge) + " --run " + w.state().Run
	got := w.next()
	if !strings.Contains(got, "Dispatch 2 reviewer agent(s)") || !strings.Contains(got, want) {
		t.Fatalf("next: %s", got)
	}
	for i := 0; i < 2; i++ {
		if id, why := w.spawn(lead, "orchestrator", "reviewer"); id == "" {
			t.Fatal(why)
		}
	}
	got = w.next()
	if !strings.HasPrefix(got, "Wait: stage ") || !strings.Contains(got, "still running.") || !strings.Contains(got, want) {
		t.Fatalf("next: %s", got)
	}
}

func TestHandbackAfterRunFinishIgnoresTheLeadsEnsignWait(t *testing.T) {
	tmp := useTempDir(t)
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	run := w.state().Run
	out := fakeTaskFile(t, tmp, "sess-1", "wait1", "waiting\n")
	res, code := w.hook("PostToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "ensign wait --bridge /b --run " + run},
		"tool_response": map[string]any{"backgroundTaskId": "wait1"}})
	if code != 0 || res != nil {
		t.Fatalf("PostToolUse: %d %v", code, res)
	}
	st := w.state()
	if !st.Waits["wait1"] || st.TaskFiles["wait1"] != out {
		t.Fatalf("the wait shell should be recorded: waits=%v files=%v", st.Waits, st.TaskFiles)
	}
	w.next()
	if r := w.handback(lead, "orchestrator", "All done."); !strings.Contains(r, "wait1") {
		t.Fatalf("a lead whose run is not finished is still refused for the running wait: %q", r)
	}
	if _, err := w.finishGoal(run, "--goal", "met"); err != nil {
		t.Fatal(err)
	}
	if r := w.handback(lead, "orchestrator", "All done."); r != "" {
		t.Fatalf("a finished run's lead is not held back by its ensign wait, whose exit line is not written yet: %s", r)
	}
	if d := w.runState(run).byAgent(lead); d.Handback != "All done." {
		t.Fatalf("the handback should be recorded: %+v", d)
	}
}

func TestNextDuringARunningPlainStageGivesNoWaitAdvice(t *testing.T) {
	w, _, release, wait := gatedPlainRunAfter(t, "")
	got := w.next()
	if !strings.Contains(got, "is still running.") || strings.Contains(got, "ensign wait") {
		t.Errorf("ensign runs this stage itself, so ensign wait would end at once: %q", got)
	}
	release()
	wait()
}

func TestDenialToTheSessionAboutARunningLeadGivesNoWaitAdvice(t *testing.T) {
	w := newWorld(t)
	w.startLead()
	id, why := w.spawn("", "", "orchestrator")
	if id != "" || !strings.Contains(why, "still running") {
		t.Fatalf("want a refusal naming the running lead, got %q %q", id, why)
	}
	if strings.Contains(why, "ensign wait") {
		t.Errorf("the session does not run ensign wait: %q", why)
	}
}

func TestDispatchWithoutAnAgentYetCountsAsRunning(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	if id, why := w.spawn(lead, "orchestrator", "coder"); id == "" {
		t.Fatal(why)
	}
	r, err := openSessionRun(w.bridge, w.sid)
	if err != nil || r == nil {
		t.Fatal(err)
	}
	defer r.close()
	r.State.Dispatches[len(r.State.Dispatches)-1].AgentID = ""
	if !agentsRunning(r.Snap, r.State, leadScope(r)) {
		t.Fatal("a dispatch whose agent has not bound yet is running")
	}
}

func TestHelperOfAnEarlierLeadIsNotCounted(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	r, err := openSessionRun(w.bridge, w.sid)
	if err != nil || r == nil {
		t.Fatal(err)
	}
	defer r.close()
	if !agentsRunning(r.Snap, r.State, leadScope(r)) {
		t.Fatal("the lead's helper is running")
	}
	r.State.Tasks["h1"] = "an-earlier-lead"
	if agentsRunning(r.Snap, r.State, leadScope(r)) {
		t.Fatal("a helper an earlier lead started is not counted")
	}
}

func TestHelperFinishRecordedBeforeItsLaunch(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	if r := w.stopWith("h1", "general-purpose", "done", false, nil); r != "" {
		t.Fatal(r)
	}
	if r := w.handback("h2", "general-purpose", "done"); r != "" {
		t.Fatal(r)
	}
	// A stop with a task whose owner is unknown may be a pause, so it records nothing.
	w.stopWith("h3", "general-purpose", "waiting", false, []map[string]any{{"id": "t9", "type": "shell", "status": "running"}})
	// Nor does a stop with a task another agent launched.
	w.launched(lead, "orchestrator", "Bash", map[string]any{"backgroundTaskId": "wait1"})
	w.stopWith("h4", "general-purpose", "done", false, []map[string]any{{"id": "wait1", "type": "shell", "status": "running"}})
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		w.helper(lead, h)
	}
	got := w.state().Helpers
	if got["h1"] || got["h2"] || !got["h3"] || got["h4"] {
		t.Fatalf("only the helper that may be paused is tracked: %v", got)
	}
}

func TestEndedAgentsAreBounded(t *testing.T) {
	st := &State{}
	for i := 0; i < endedBound+10; i++ {
		st.agentEnded(fmt.Sprintf("a%d", i))
	}
	if len(st.Ended) != endedBound || st.Ended[0] != "a10" {
		t.Fatalf("the oldest are dropped: %d %v", len(st.Ended), st.Ended[:1])
	}
}

func TestLeadsTaskStopAttemptFinishesTheHelper(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	w.helper(lead, "h2")
	res, code := w.hook("PreToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator", "tool_name": "TaskStop", "tool_input": map[string]any{"task_id": "h1"}})
	if code != 0 || res != nil {
		t.Fatalf("the TaskStop is let through: %d %v", code, res)
	}
	res, code = w.hook("PreToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator", "tool_name": "TaskStop", "tool_input": map[string]any{"shell_id": "h2"}})
	if code != 0 || res != nil {
		t.Fatalf("the TaskStop is let through: %d %v", code, res)
	}
	if st := w.state(); len(st.Helpers) != 0 {
		t.Fatalf("a helper the lead tried to stop is finished: %v", st.Helpers)
	}
}

func TestHelperPausedOnAMonitorIsNotFinished(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	monitor := []map[string]any{{"id": "mon1", "type": "monitor", "status": "running"}}
	if r := w.stopWith("h1", "general-purpose", "watching", false, monitor); r != "" {
		t.Fatal(r)
	}
	if !w.state().Helpers["h1"] {
		t.Fatal("a helper with a Monitor of no recorded owner in flight is paused")
	}
	w.handback("h1", "general-purpose", "done")
	if len(w.state().Helpers) != 0 {
		t.Fatal("its handback finishes it")
	}
}

func TestLeadBlockNamesTheHelpersToTaskStop(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	r := w.stopWith(lead, "orchestrator", "waiting", false, nil)
	if !strings.Contains(r, "ensign wait --bridge") || !strings.Contains(r, "TaskStop") || !strings.Contains(r, "h1") {
		t.Fatalf("the block should name the wait command and the helper to stop: %q", r)
	}
}

func TestLeadBlockAtTheGateSendsItToStopHelpersAndFinish(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	w.helper(lead, "h1")
	r := w.stopWith(lead, "orchestrator", "waiting", false, nil)
	if strings.Contains(r, "ensign wait") || !strings.Contains(r, "TaskStop") || !strings.Contains(r, "h1") || !strings.Contains(r, "ensign run finish") {
		t.Fatalf("at the gate the lead is to stop the helper and finish the run: %q", r)
	}
}

// shell records a background shell command of the lead and returns the task list that shows it in flight.
func (w *world) shell(lead, id, command string) []map[string]any {
	w.t.Helper()
	res, code := w.hook("PostToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": command},
		"tool_response": map[string]any{"backgroundTaskId": id}})
	if code != 0 || res != nil {
		w.t.Fatalf("PostToolUse: %d %v", code, res)
	}
	return []map[string]any{{"id": id, "type": "shell", "status": "running"}}
}

func TestLeadWithAFailedStageAgentStopsItsWaitAndTheRunFails(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	if coder, why := w.spawn(lead, "orchestrator", "coder"); coder == "" {
		t.Fatal(why)
	}
	r := w.stopWith(lead, "orchestrator", "waiting", false, nil)
	if !strings.Contains(r, "TaskStop") || !strings.Contains(r, "will not report") {
		t.Fatalf("the block should say how to give up on a failed agent: %q", r)
	}
	if got := w.next(); !strings.Contains(got, "TaskStop") {
		t.Fatalf("next should say it too: %q", got)
	}
	for i := 0; i < stopBudget-1; i++ {
		if r := w.stopWith(lead, "orchestrator", "waiting", false, nil); !strings.Contains(r, "ensign wait") {
			t.Fatalf("stop %d should be blocked: %q", i, r)
		}
	}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, nil); r != "" {
		t.Fatalf("the stop that spends the budget passes: %s", r)
	}
	if st := w.runState(run); st.Status != "error" || !strings.Contains(st.Reason, "lead stopped") {
		t.Fatalf("the run should end as lead-stopped: %s %s", st.Status, st.Reason)
	}
}

func TestLeadWithOnlyAWaitAtTheGateIsBlocked(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.passPipeline(lead)
	w.helper(lead, "h1")
	tasks := w.shell(lead, "wait1", "ensign wait --bridge /b --run x")
	r := w.stopWith(lead, "orchestrator", "waiting", false, tasks)
	if !strings.Contains(r, "TaskStop") || !strings.Contains(r, "ensign run finish") {
		t.Fatalf("at the gate a wait does not excuse the stop: %q", r)
	}
}

func TestLeadWithOnlyAWaitAndAnUnreadResultGetsTheFreeBlock(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	tasks := w.shell(lead, "wait1", "ensign wait --bridge /b --run x")
	left := w.stopsLeft(lead)
	r := w.stopWith(lead, "orchestrator", "waiting", false, tasks)
	if !strings.Contains(r, "the pipeline is not finished") || !strings.Contains(r, `stage "review"`) {
		t.Fatalf("an unread result is not excused by a wait: %q", r)
	}
	if got := w.stopsLeft(lead); got != left {
		t.Fatalf("the block is free: %d left, was %d", got, left)
	}
}

func TestLeadWithAnotherShellAndAnUnreadResultMayStop(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	tasks := w.shell(lead, "rs1", "ensign run-stage --bridge /b --run x")
	if r := w.stopWith(lead, "orchestrator", "waiting", false, tasks); r != "" {
		t.Fatalf("a shell that is not a wait excuses the stop: %s", r)
	}
}

func TestLeadWithAWaitAndABranchStillRunningMayStop(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.leaf(lead, "coder", "compiled")
	var revs []string
	for i := 0; i < 2; i++ {
		id, why := w.spawn(lead, "orchestrator", "reviewer")
		if id == "" {
			t.Fatal(why)
		}
		revs = append(revs, id)
	}
	w.stop(revs[0], "reviewer", report("clean"), false)
	tasks := w.shell(lead, "wait1", "ensign wait --bridge /b --run x")
	if r := w.stopWith(lead, "orchestrator", "waiting", false, tasks); r != "" {
		t.Fatalf("a wait excuses the stop while a branch runs: %s", r)
	}
}

func TestLeadWithAWaitAndALiveHelperMayStop(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	tasks := w.shell(lead, "wait1", "ensign wait --bridge /b --run x")
	if r := w.stopWith(lead, "orchestrator", "waiting", false, tasks); r != "" {
		t.Fatalf("a wait excuses the stop with only a helper running: %s", r)
	}
}

func TestLeadWithAHelperAndAnUnreadResultReadsItBeforeTheWait(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	w.leaf(lead, "coder", "compiled")
	tasks := w.shell(lead, "wait1", "ensign wait --bridge /b --run x")
	left := w.stopsLeft(lead)
	r := w.stopWith(lead, "orchestrator", "waiting", false, tasks)
	if !strings.Contains(r, "the pipeline is not finished") || !strings.Contains(r, `stage "review"`) {
		t.Fatalf("the unread result is read first: %q", r)
	}
	if got := w.stopsLeft(lead); got != left {
		t.Fatalf("the block is free: %d left, was %d", got, left)
	}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, tasks); r != "" {
		t.Fatalf("the wait excuses the next stop: %s", r)
	}
}

func TestLeadWithAHelperAndAnUnreadResultAndNoWaitReadsItThenIsToldToWait(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.helper(lead, "h1")
	w.leaf(lead, "coder", "compiled")
	agent := []map[string]any{{"id": "h1", "type": "subagent", "status": "running"}}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "the pipeline is not finished") {
		t.Fatalf("the unread result is read first: %q", r)
	}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "ensign wait --bridge") {
		t.Fatalf("then the lead is told to wait: %q", r)
	}
}

func TestResumeClearsTheHelpers(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	run := w.state().Run
	w.helper(lead, "h1")
	r, err := openRun(w.bridge, run)
	if err != nil {
		t.Fatal(err)
	}
	if !r.State.Helpers["h1"] {
		t.Fatal("the helper should be recorded")
	}
	r.State.Status, r.State.Reason = "ask-user", "which way?"
	r.State.Resume = &ResumePoint{Scope: leadScope(r), Stage: r.Snap.Pipeline.Stages[0].ID}
	r.save()
	r.close()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	if err := cmdResume([]string{"--answer", "left", "--run", run}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if st := w.runState(run); len(st.Helpers) != 0 {
		t.Fatalf("a resumed run starts with no helpers: %v", st.Helpers)
	}
}

func TestLeadWithARunningPlainStageAndAHelperAndAnUnreadResultReadsItThenIsToldToWait(t *testing.T) {
	w, _, release, wait := gatedPlainRun(t)
	lead := ""
	for _, d := range w.state().Dispatches {
		if d.Role == "orchestrator" {
			lead = d.AgentID
		}
	}
	w.helper(lead, "h1")
	agent := []map[string]any{{"id": "h1", "type": "subagent", "status": "running"}}
	left := w.stopsLeft(lead)
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "the pipeline is not finished") {
		t.Fatalf("the unread result is read first, though a stage runs: %q", r)
	}
	if got := w.stopsLeft(lead); got != left {
		t.Fatalf("the block is free: %d left, was %d", got, left)
	}
	if r := w.stopWith(lead, "orchestrator", "waiting", false, agent); !strings.Contains(r, "ensign wait --bridge") {
		t.Fatalf("then the lead is told to wait: %q", r)
	}
	release()
	wait()
}
