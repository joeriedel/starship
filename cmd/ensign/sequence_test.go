// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seq runs `ensign sequence …` as the world's session.
func (w *world) seq(args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdSequence(args, &out)
	return out.String(), err
}

func (w *world) mustSeq(args ...string) string {
	w.t.Helper()
	out, err := w.seq(args...)
	if err != nil {
		w.t.Fatalf("ensign sequence %v: %v", args, err)
	}
	return out
}

func (w *world) step(n int) *seqStep {
	w.t.Helper()
	_, st := readSeq(w.bridge, w.sid).step(n)
	if st == nil {
		w.t.Fatalf("no step %d", n)
	}
	return st
}

func (w *world) wantStatus(want ...string) {
	w.t.Helper()
	for i, s := range want {
		if got := w.step(i + 1).Status; got != s {
			w.t.Errorf("step %d is %s, want %s\n%s", i+1, got, s, sequenceListing(w.bridge, w.sid))
		}
	}
}

func (w *world) failRun(lead string) {
	w.t.Helper()
	for i := 0; i < 3; i++ {
		w.leaf(lead, "coder", "broken")
	}
}

func (w *world) passRun(lead string) {
	w.t.Helper()
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
}

func (w *world) goContext() string {
	w.t.Helper()
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	return dig(res, "hookSpecificOutput", "additionalContext")
}

func (w *world) stopHook() map[string]any {
	w.t.Helper()
	res, code := w.hook("Stop", map[string]any{})
	if code != 0 {
		w.t.Fatalf("Stop exited %d: %v", code, res)
	}
	return res
}

func TestSequenceRunPublishRunInOrder(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	out := w.mustSeq("run", task)
	if !strings.Contains(out, "type go to approve the sequence") {
		t.Fatalf("the listing should end asking for go:\n%s", out)
	}
	if _, err := os.Stat(sessionFile(w.bridge, w.sid)); err == nil {
		t.Fatal("a queued run must not bind the session")
	}
	ctx := w.goContext()
	if !strings.Contains(ctx, "approved") || !strings.Contains(ctx, `"orchestrator"`) {
		t.Fatalf("go should approve and hand over the first lead: %s", ctx)
	}
	first := w.step(1).Run
	if st := w.state(); st.Run != first || !st.Approved || st.Status != "running" {
		t.Fatalf("first run is %s %s approved=%v", st.Run, st.Status, st.Approved)
	}
	lead, why := w.spawn("", "", "orchestrator")
	if lead == "" {
		t.Fatalf("lead refused: %s", why)
	}
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the run should pass: %s", why)
	}
	w.wantStatus("done", "done", "active")
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the publish should have put the work on main, got %q", got)
	}
	if st := w.state(); st.Run != w.step(3).Run || !st.Approved || st.Status != "running" {
		t.Errorf("the second run should be bound, approved and running: %s %s", st.Run, st.Status)
	}
}

func TestSequencePublishThatIsNotAFastForwardFails(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("do", "tell the user")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	write(t, filepath.Join(other, "news.txt"), "meanwhile\n")
	runGit(t, other, "add", "news.txt")
	runGit(t, other, "commit", "-q", "-m", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
	w.runStage()
	_ = gh
	w.wantStatus("done", "failed", "cancelled")
	if r := w.step(2).Reason; !strings.Contains(r, "has moved since this task branched") {
		t.Errorf("the step should carry the operation's error: %q", r)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:news.txt"); got != "meanwhile" {
		t.Errorf("main should be untouched, got %q", got)
	}
}

func TestSequenceStepAddedAfterGoWaitsForItsOwnGo(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	out := w.mustSeq("do", "write to the user")
	if !strings.Contains(out, "sequence 2") || !strings.Contains(out, "type go to approve the sequence") {
		t.Fatalf("the step should open a second sequence:\n%s", out)
	}
	w.passRun(lead)
	w.wantStatus("done", "pending")
	if sf := readSeq(w.bridge, w.sid); sf.Sequences[1].Approved {
		t.Fatal("the second sequence must wait for its own go")
	}
	if ctx := w.goContext(); !strings.Contains(ctx, "write to the user") {
		t.Fatalf("second go should hand over the do step: %s", ctx)
	}
	w.wantStatus("done", "active")
}

func TestSequenceStopModeHaltsAndCancelledRunsAreNotWaiting(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.mustSeq("run", "supernova/other")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide", "pending", "pending")
	w.mustSeq("accept", "1")
	w.wantStatus("failed", "cancelled", "cancelled")
	if sf := readSeq(w.bridge, w.sid); !sf.Sequences[0].Halted {
		t.Error("the sequence should be halted")
	}
	cancelled := w.runState(w.step(3).Run)
	if !cancelled.terminal() || cancelled.queued() || !strings.HasPrefix(cancelled.Reason, "cancelled:") {
		t.Errorf("the cancelled run should be terminal: %s %q", cancelled.Status, cancelled.Reason)
	}
	for _, ri := range liveRuns(w.bridge) {
		t.Errorf("run %s is still live", ri.id)
	}
	w.run("supernova/other") // a plain run works again, and replaces nothing
	if st := w.state(); st.Status != "pending" || st.Sequence {
		t.Errorf("plain run: %s sequence=%v", st.Status, st.Sequence)
	}
}

func TestSequenceKeepGoingSkipsLaterStepsOfTheFailedTask(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "report")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide", "pending", "pending", "pending")
	w.mustSeq("accept", "1")
	w.wantStatus("failed", "skipped", "active", "pending")
	if r := w.step(2).Reason; !strings.Contains(r, "step 1 failed") {
		t.Errorf("the skip should name the failed step: %q", r)
	}
	if skipped := w.runState(w.step(2).Run); !skipped.terminal() {
		t.Errorf("the skipped run should be terminal: %s", skipped.Status)
	}
	w.mustSeq("done", "3")
	w.wantStatus("failed", "skipped", "done", "active")
	if st := w.state(); st.Run != w.step(4).Run || !st.Approved {
		t.Errorf("the other task's run should be under way: %s", st.Run)
	}
}

func TestSequenceHaltCancelsWhatHasNotStarted(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	w.mustSeq("halt", "--reason", "not needed")
	w.wantStatus("active", "cancelled")
	if r := w.step(2).Reason; r != "not needed" {
		t.Errorf("reason %q", r)
	}
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	w.wantStatus("done", "cancelled")
	if w.runState(w.step(2).Run).Status != "error" {
		t.Error("the cancelled run should be terminal")
	}
}

func TestSequenceDoStepWaitsForDoneOrFail(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "update the changelog")
	w.mustSeq("do", "tell the user")
	ctx := w.goContext()
	if !strings.Contains(ctx, "update the changelog") || !strings.Contains(ctx, "ensign sequence done 1") {
		t.Fatalf("go should hand over the do step: %s", ctx)
	}
	w.wantStatus("active", "pending")
	if _, err := w.seq("done", "2"); err == nil {
		t.Error("a pending step can't be marked done")
	}
	out := w.mustSeq("done", "1", "--note", "added")
	if !strings.Contains(out, "tell the user") {
		t.Errorf("done should hand over the next step: %s", out)
	}
	w.wantStatus("done", "active")
	w.mustSeq("fail", "2", "--reason", "no way to tell")
	w.wantStatus("done", "failed")
	if out := w.mustSeq(); !strings.Contains(out, "halted") {
		t.Errorf("a failed do step halts a stop-mode sequence:\n%s", out)
	}
	if _, err := w.seq("fail", "2"); err == nil {
		t.Error("fail needs a reason")
	}
}

func TestSequenceStopHookHoldsTheTurnForAReadyStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "tell the user")
	w.approve()
	for i := 0; i < seqStopBudget; i++ {
		res := w.stopHook()
		if res["decision"] != "block" || !strings.Contains(dig(res, "reason"), "orchestrator") {
			t.Fatalf("stop %d should be held for the undispatched lead: %v", i+1, res)
		}
	}
	if res := w.stopHook(); res != nil {
		t.Fatalf("the budget is spent, the turn may end: %v", res)
	}

	w2 := newWorld(t)
	w2.mustSeq("run", "supernova/auth-refactor")
	w2.approve()
	w2.spawn("", "", "orchestrator")
	if res := w2.stopHook(); res != nil {
		t.Fatalf("the lead is dispatched, so nothing is waiting: %v", res)
	}

	w3 := newWorld(t)
	w3.mustSeq("do", "tell the user")
	w3.approve()
	for i := 0; i < seqStopBudget; i++ {
		if res := w3.stopHook(); res["decision"] != "block" || !strings.Contains(dig(res, "reason"), "tell the user") {
			t.Fatalf("stop %d should be held for the do step: %v", i+1, res)
		}
	}
	if res := w3.stopHook(); res != nil {
		t.Fatalf("the budget is spent: %v", res)
	}
}

func (w *world) stopHookWithBackground() map[string]any {
	w.t.Helper()
	res, code := w.hook("Stop", map[string]any{"background_tasks": []map[string]any{{"id": "bash1", "type": "shell", "status": "running"}}})
	if code != 0 {
		w.t.Fatalf("Stop exited %d: %v", code, res)
	}
	return res
}

func TestSequenceStopHookLetsTheTurnEndWhileADoStepWaitsOnBackgroundWork(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "tell the user")
	w.approve()
	for i := 0; i < seqStopBudget+2; i++ {
		if res := w.stopHookWithBackground(); res != nil {
			t.Fatalf("stop %d: background work is in flight, the turn may end: %v", i+1, res)
		}
	}
	if n := w.step(1).Blocks; n != 0 {
		t.Fatalf("no hold should be spent while waiting, got %d", n)
	}

	w2 := newWorld(t)
	w2.mustSeq("do", "tell the user")
	w2.approve()
	if res := w2.stopHook(); res["decision"] != "block" {
		t.Fatalf("nothing in flight, the do step holds the turn: %v", res)
	}
	if n := w2.step(1).Blocks; n != 1 {
		t.Fatalf("a hold should be spent, got %d", n)
	}

	w3 := newWorld(t)
	w3.mustSeq("run", "supernova/auth-refactor")
	w3.approve()
	if res := w3.stopHookWithBackground(); res["decision"] != "block" {
		t.Fatalf("a ready run step still holds the turn: %v", res)
	}
}

func TestPlainRunIsUnchangedWithoutASequence(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	w.approve()
	if st := w.state(); !st.Approved || st.Status != "running" || st.Sequence {
		t.Fatalf("a plain run: %s approved=%v", st.Status, st.Approved)
	}
}

func TestPlainRunRefusedWhileASequenceIsOpen(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "tell the user")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdRun([]string{"supernova/auth-refactor"}, &out)
	if err == nil || !strings.Contains(err.Error(), "ensign sequence run") {
		t.Fatalf("a plain run should point at ensign sequence run: %v", err)
	}
}

func TestWaitingPlainRunBecomesTheFirstStep(t *testing.T) {
	w := newWorld(t)
	w.run("supernova/auth-refactor")
	plain := w.state().Run
	out := w.mustSeq("do", "tell the user")
	if !strings.Contains(out, "was waiting for go") {
		t.Fatalf("the output should say so:\n%s", out)
	}
	if _, err := os.Stat(sessionFile(w.bridge, w.sid)); err == nil {
		t.Error("the session binding should be gone")
	}
	if w.step(1).Run != plain || w.step(1).Kind != "run" {
		t.Errorf("step 1 is %+v", w.step(1))
	}
	w.approve()
	if st := w.state(); st.Run != plain || !st.Approved {
		t.Errorf("go should start the absorbed run: %s", st.Run)
	}
}

func TestSequencePublishNeedsAnExtensionWithAPublishOperation(t *testing.T) {
	w := newWorld(t)
	_, err := w.seq("publish", "supernova/auth-refactor")
	if err == nil || !strings.Contains(err.Error(), "supernova/auth-refactor") || !strings.Contains(err.Error(), "no publish operation") {
		t.Fatalf("publish should be refused: %v", err)
	}
}

func TestSequenceSurvivesSessionIDChange(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	first, second := w.step(1).Run, w.step(2).Run
	write(t, w.session, `{"sessionId":"s1"}`+"\n")
	carryOverSession(w.bridge, &HookInput{Source: "resume", SessionID: "s2", TranscriptPath: w.session})
	if seqExists(w.bridge, "s1") || !seqExists(w.bridge, "s2") {
		t.Fatal("the sequence should move to the new session")
	}
	if st := w.runState(second); st.Session != "s2" {
		t.Errorf("the queued run belongs to %q", st.Session)
	}
	w.sid = "s2"
	if st := w.state(); st.Run != first || st.Session != "s2" {
		t.Errorf("the active run should follow: %s %s", st.Run, st.Session)
	}
	if sf := readSeq(w.bridge, "s2"); len(sf.Sequences) != 1 || sf.Sequences[0].Steps[1].Run != second {
		t.Errorf("sequence content lost: %+v", sf)
	}
}

func TestSequenceHaltOfAnOpenSequenceLetsALaterOneStart(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first")
	w.mustSeq("halt")
	w.mustSeq("do", "second")
	ctx := w.goContext()
	if !strings.Contains(ctx, "second") {
		t.Fatalf("go should start the new sequence's step: %s", ctx)
	}
	if got := w.step(2).Status; got != "active" {
		t.Errorf("step 2 is %s, want active", got)
	}
}

func TestSequenceHaltCancelsAPausedRunStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	r, err := openRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "another cycle?"
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	w.mustSeq("halt")
	w.wantStatus("cancelled")
	if st := w.runState(w.step(1).Run); !st.terminal() {
		t.Errorf("the paused run should be terminal: %s", st.Status)
	}
	if sequenceUnfinished(w.bridge, w.sid) {
		t.Error("the halted sequence should be finished")
	}
	w.run("supernova/auth-refactor")
}

func TestSequenceHaltCancelsAnActiveDoStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "tell the user")
	w.approve()
	w.mustSeq("halt")
	w.wantStatus("cancelled")
	if res := w.stopHook(); res != nil {
		t.Fatalf("a halted do step must not hold the turn: %v", res)
	}
}

func TestLiveRunsCountsQueuedSequenceRuns(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	if len(liveRuns(w.bridge)) != 1 {
		t.Error("a queued sequence run should still be live")
	}
}

func TestSequenceStepRefusedByPinMismatchRetiresItsRun(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	id := w.step(1).Run
	writeFile(sessionFile(w.bridge, w.sid)+".pin", "not-the-pinned-commit\n")
	w.goContext()
	w.wantStatus("failed")
	if st := w.runState(id); !st.terminal() {
		t.Errorf("the refused step's run should be terminal: %s", st.Status)
	}
	for _, ri := range liveRuns(w.bridge) {
		if ri.id == id {
			t.Error("the refused step's run should not be live")
		}
	}
}

func TestRetireIfLeavesAResumedRunAlone(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	id := w.step(1).Run
	if retireIf(w.bridge, id, "cancelled: halted", "pending", "ask-user") {
		t.Fatal("a running run must not be retired")
	}
	if st := w.runState(id); st.Status != "running" {
		t.Errorf("run is %s, want running", st.Status)
	}
}

func TestSequenceFailedRunWaitsForADecisionAndStopHoldsTheTurn(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "report")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide", "pending")
	if sf := readSeq(w.bridge, w.sid); sf.Sequences[0].Halted {
		t.Error("the sequence should wait, not halt")
	}
	res := w.stopHook()
	reason, _ := res["reason"].(string)
	if res["decision"] != "block" || !strings.Contains(reason, "sequence retry 1") || !strings.Contains(reason, "sequence accept 1") {
		t.Fatalf("the Stop hook should hold the turn and name both commands: %v", res)
	}
	for i := 0; i < seqStopBudget; i++ {
		w.stopHook()
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the hold should run out: %v", res)
	}
}

func (w *world) retryStep(n string) string {
	w.t.Helper()
	return w.mustSeq("retry", n, "--diagnosis", "the fixture path was wrong", "--state", "only the login test is left")
}

func TestSequenceRetryStartsWithoutGoAndCarriesTheState(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	first := w.step(1).Run
	w.retryStep("1")
	st := w.step(1)
	if st.Status != "active" || st.Run == first || st.Prev != first {
		t.Fatalf("the retry should be under way: %+v", st)
	}
	if old := w.runState(first); old.Status != "error" {
		t.Errorf("the old run should be left alone: %s", old.Status)
	}
	if cur := w.state(); cur.Run != st.Run || !cur.Approved || cur.RetryOf != first {
		t.Errorf("the retry run should be bound and approved: %+v", cur)
	}
	r, err := readRun(w.bridge, st.Run)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Current state", "the fixture path was wrong", "only the login test is left"} {
		if !strings.Contains(r.Snap.WorkItem, want) {
			t.Errorf("the work item lacks %q:\n%s", want, r.Snap.WorkItem)
		}
	}
	listing := w.mustSeq()
	if !strings.Contains(listing, "attempt 2/2") || !strings.Contains(listing, "the fixture path was wrong") {
		t.Errorf("the listing should show the attempt and diagnosis:\n%s", listing)
	}
}

func TestSequenceFailedRetryHaltsInStopMode(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.retryStep("1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("failed", "cancelled")
	if sf := readSeq(w.bridge, w.sid); !sf.Sequences[0].Halted {
		t.Error("the sequence should be halted")
	}
}

func TestSequenceFailedRetrySkipsTheTasksLaterStepsInKeepGoingMode(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.retryStep("1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("failed", "skipped", "active")
}

func TestSequenceSecondRetryAndBadRetriesAreRefused(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("publish", "supernova/auth-refactor")
	w.mustSeq("do", "report")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	if _, err := w.seq("retry", "1", "--diagnosis", "x", "--state", "y"); err == nil || !strings.Contains(err.Error(), "not waiting") {
		t.Errorf("an active step can't be retried: %v", err)
	}
	w.failRun(lead)
	for _, args := range [][]string{
		{"retry", "1", "--state", "left"},
		{"retry", "1", "--diagnosis", "why"},
		{"retry", "1", "--diagnosis", " ", "--state", "left"},
		{"retry", "2", "--diagnosis", "why", "--state", "left"},
		{"retry", "3", "--diagnosis", "why", "--state", "left"},
	} {
		if _, err := w.seq(args...); err == nil {
			t.Errorf("%v should be refused", args)
		}
	}
	w.wantStatus("decide")
	w.retryStep("1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	_, err := w.seq("retry", "1", "--diagnosis", "again", "--state", "left")
	if err == nil || !strings.Contains(err.Error(), "already retried") {
		t.Errorf("a second retry should be refused: %v", err)
	}
}

func TestSequenceRetryRefusesPublishAndDoSteps(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.mustSeq("publish", "supernova/auth-refactor")
	w.mustSeq("do", "report")
	for _, n := range []string{"1", "2"} {
		_, err := w.seq("retry", n, "--diagnosis", "x", "--state", "y")
		if err == nil || !strings.Contains(err.Error(), "only run steps") {
			t.Errorf("step %s: %v", n, err)
		}
	}
}

func TestSequenceAcceptNeedsAWaitingStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	if _, err := w.seq("accept", "1"); err == nil {
		t.Error("accept of an active step should be refused")
	}
}

func TestSequenceHaltCancelsAStepWaitingForADecision(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide", "pending")
	w.mustSeq("halt", "--reason", "enough")
	w.wantStatus("cancelled", "cancelled")
}

func TestSequenceHaltedSequenceDoesNotWaitOnALateFailure(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.mustSeq("halt", "--reason", "enough")
	w.wantStatus("active", "cancelled")
	w.failRun(lead)
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("a halted sequence must not hold the turn: %v", res)
	}
	w.wantStatus("failed", "cancelled")
	if _, err := w.seq("retry", "1", "--diagnosis", "x", "--state", "y"); err == nil {
		t.Error("retry on a halted sequence should be refused")
	}
}

// releaseWorld gives the fixture project a @release that records $VERSION in the workspace and exits with code.
func releaseWorld(t *testing.T, code int) *world {
	t.Helper()
	w := newWorld(t)
	path := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Replace(string(base), "bindings:\n",
		"bindings:\n  release: { command: \"echo $VERSION >> released.txt; echo boom; exit "+strconv.Itoa(code)+"\" }\n", 1))
	return w
}

func (w *world) released() string {
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(w.bridge), "workspace", "released.txt"))
	return strings.TrimSpace(string(data))
}

func TestSequenceReleaseRunsReleaseWithVersionAndAdvances(t *testing.T) {
	w := releaseWorld(t, 0)
	out := w.mustSeq("release", "supernova", "1.2.3")
	if !strings.Contains(out, "supernova 1.2.3") {
		t.Fatalf("the listing should show the version:\n%s", out)
	}
	w.mustSeq("do", "report")
	ctx := w.goContext()
	if !strings.Contains(ctx, "ensign sequence release-run 1") {
		t.Fatalf("go should name release-run: %s", ctx)
	}
	w.wantStatus("active", "pending")
	if w.released() != "" {
		t.Fatal("the release must not run inside the hook")
	}
	res := w.stopHook()
	if r := dig(res, "reason"); !strings.Contains(r, "ensign sequence release-run 1") {
		t.Fatalf("the Stop hook should name release-run: %v", res)
	}
	out = w.mustSeq("release-run", "1")
	if w.released() != "1.2.3" {
		t.Errorf("released.txt = %q", w.released())
	}
	w.wantStatus("done", "active")
	if !strings.Contains(out, "report") {
		t.Errorf("release-run should hand over the next step:\n%s", out)
	}
	if _, err := w.seq("release-run", "1"); err == nil {
		t.Error("a second release-run of a finished step should be refused")
	}
}

func TestSequenceStopHookDoesNotBlockOnceReleaseStarted(t *testing.T) {
	w := releaseWorld(t, 0)
	w.mustSeq("release", "supernova", "1.0.0")
	w.goContext()
	_, st := readSeq(w.bridge, w.sid).step(1)
	if st.Started {
		t.Fatal("not started yet")
	}
	withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		_, st := sf.step(1)
		st.Started = true
		return nil
	})
	if res := w.stopHook(); dig(res, "reason") != "" {
		t.Errorf("a started release should not block the turn: %v", res)
	}
	if _, err := w.seq("release-run", "1"); err == nil {
		t.Error("a started release can't be started again")
	}
}

func TestSequenceReleaseFailureHaltsInStopMode(t *testing.T) {
	w := releaseWorld(t, 1)
	w.mustSeq("release", "supernova", "2.0.0")
	w.mustSeq("do", "later")
	w.goContext()
	w.mustSeq("release-run", "1")
	w.wantStatus("failed", "cancelled")
	if r := w.step(1).Reason; !strings.Contains(r, "boom") {
		t.Errorf("the reason should carry the output: %q", r)
	}
	if !readSeq(w.bridge, w.sid).Sequences[0].Halted {
		t.Error("the sequence should be halted")
	}
}

func TestSequenceReleaseFailureSkipsSameProjectInKeepGoing(t *testing.T) {
	w := releaseWorld(t, 1)
	w.addTask("other")
	w.mustSeq("release", "supernova", "2.0.0")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("do", "report")
	w.mustSeq("mode", "keep-going")
	w.goContext()
	w.mustSeq("release-run", "1")
	w.wantStatus("failed", "skipped", "active")
}

func TestSequenceReleaseNeedsBindingAndVersion(t *testing.T) {
	w := newWorld(t)
	if _, err := w.seq("release", "supernova", "1.0.0"); err == nil || !strings.Contains(err.Error(), "no @release") {
		t.Errorf("a project without @release should be refused: %v", err)
	}
	w = releaseWorld(t, 0)
	if _, err := w.seq("release", "supernova", "  "); err == nil {
		t.Error("an empty version should be refused")
	}
}

func TestSequenceReleaseRunRefusesStepsNotDue(t *testing.T) {
	w := releaseWorld(t, 0)
	w.mustSeq("do", "first")
	w.mustSeq("release", "supernova", "1.0.0")
	if _, err := w.seq("release-run", "2"); err == nil {
		t.Error("a pending release step is not due")
	}
	w.goContext()
	if _, err := w.seq("release-run", "1"); err == nil {
		t.Error("a do step can't be release-run")
	}
	if w.released() != "" {
		t.Error("nothing should have been released")
	}
}

func TestSequenceRetryRefusesReleaseStep(t *testing.T) {
	w := releaseWorld(t, 0)
	w.mustSeq("release", "supernova", "1.0.0")
	w.goContext()
	_, err := w.seq("retry", "1", "--diagnosis", "x", "--state", "y")
	if err == nil || !strings.Contains(err.Error(), "release") {
		t.Errorf("retry should refuse a release step: %v", err)
	}
}

func TestSequenceFailEndsStartedRelease(t *testing.T) {
	w := releaseWorld(t, 0)
	w.mustSeq("release", "supernova", "1.0.0")
	w.mustSeq("do", "later")
	w.goContext()
	withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		_, st := sf.step(1)
		st.Started = true
		return nil
	})
	if _, err := w.seq("done", "1"); err == nil {
		t.Error("done must stay do-only")
	}
	w.mustSeq("fail", "1", "--reason", "runner died")
	w.wantStatus("failed", "cancelled")
	if !readSeq(w.bridge, w.sid).Sequences[0].Halted {
		t.Error("the sequence should be halted")
	}
	if _, err := w.seq("release-run", "1"); err == nil {
		t.Error("a failed release can't be run")
	}
}

func TestSequenceReleaseResultFollowsCarriedSequence(t *testing.T) {
	w := newWorld(t)
	moved := filepath.Join(w.bridge, ".starship", "sessions", "other.sequence")
	write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"),
		"platforms: [linux]\nbindings:\n  build: { command: \"./build.sh\" }\n  release: { command: \"mv '"+filepath.ToSlash(seqPath(w.bridge, w.sid))+"' '"+filepath.ToSlash(moved)+"'\" }\n")
	w.mustSeq("release", "supernova", "1.0.0")
	w.mustSeq("do", "report")
	w.goContext()
	w.mustSeq("release-run", "1")
	sf := readSeq(w.bridge, "other")
	if _, st := sf.step(1); st == nil || st.Status != "done" {
		t.Fatalf("the new owner should hold the finished release: %+v", sf)
	}
	if _, st := sf.step(2); st == nil || st.Status != "active" {
		t.Errorf("the new owner should have advanced: %+v", sf)
	}
}

func TestEachLiveRunFinishesOnItsOwn(t *testing.T) {
	w := newWorld(t)
	a := w.liveStepRun(t.TempDir(), "agentA")
	b := w.liveStepRun(t.TempDir(), "agentB")
	r, err := openRun(w.bridge, a)
	if err != nil {
		t.Fatal(err)
	}
	(&machine{snap: r.Snap, st: r.State}).fail("handler", "boom")
	if err := finishIfTerminal(r, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r.save()
	r.close()
	history, _ := os.ReadFile(filepath.Join(w.bridge, "projects", "supernova", "tasks", "auth-refactor", "history.md"))
	if !strings.Contains(string(history), "boom") {
		t.Fatalf("the finished run was not journaled:\n%s", history)
	}
	if st := w.runState(b); st.terminal() || st.Finished {
		t.Fatalf("the other run is %s", st.Status)
	}
	if got := strings.Join(liveSessionRuns(w.bridge, w.sid), ","); got != b {
		t.Fatalf("live runs %q, want %q", got, b)
	}
	hr, err := openHookRun(w.bridge, &HookInput{SessionID: w.sid, AgentID: "agentB"})
	if err != nil || hr == nil || hr.State.Run != b {
		t.Fatalf("the live run's agent lost its run: %v", err)
	}
	hr.close()
}
