// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
	for _, seq := range readSeq(w.bridge, w.sid).Sequences {
		if n <= len(seq.Steps) {
			return seq.Steps[n-1]
		}
		n -= len(seq.Steps)
	}
	w.t.Fatalf("no step %d", n)
	return nil
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

func TestSequenceRunPublishNoteOnlyWhenOriginKeepsCommitsWeLack(t *testing.T) {
	for _, keeps := range []bool{true, false} {
		t.Run(fmt.Sprintf("origin keeps commits=%v", keeps), func(t *testing.T) {
			w, ws, remote, _ := githubWorld(t, "success", localTest)
			task := "supernova/auth-refactor"
			w.mustSeq("run", task)
			w.mustSeq("publish", task)
			w.goContext()
			lead, why := w.spawn("", "", "orchestrator")
			if lead == "" {
				t.Fatalf("lead refused: %s", why)
			}
			if keeps {
				remoteTaskCommit(t, remote, "theirs.txt")
			}
			w.codeChange(lead, ws)
			if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
				t.Fatalf("the run should pass: %s", why)
			}
			reason := w.step(2).Reason
			if !strings.HasPrefix(reason, "pushed to main: ") {
				t.Fatalf("the publish should report its push: %q", reason)
			}
			if got := strings.Contains(reason, ". Note: ") && strings.Contains(reason, "left on origin"); got != keeps {
				t.Errorf("note present = %v, want %v: %q", got, keeps, reason)
			}
			if !keeps && strings.Contains(reason, "Note:") {
				t.Errorf("no note expected: %q", reason)
			}
			if publishRecorded(w.bridge, task) {
				t.Error("a single location writes no record")
			}
		})
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
	if r := w.step(2).Reason; !strings.Contains(r, "has moved since this task branched") || strings.HasPrefix(r, "landed ") {
		t.Errorf("the step should carry the operation's error as it is: %q", r)
	}
	if publishRecorded(w.bridge, task) {
		t.Error("a single location writes no record")
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
	for _, ri := range w.liveRuns() {
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
	w.wantStatus("failed", "skipped", "active", "pending")
	if r := w.step(1).Reason; strings.HasPrefix(r, skipPrefix) || w.step(1).SkippedAt == 0 {
		t.Errorf("the failed step keeps the run's reason and is marked skipped: %+v", w.step(1))
	}
	if r := w.step(2).Reason; !strings.Contains(r, "step 1.1 failed") {
		t.Errorf("the skip should name the failed step: %q", r)
	}
	if skipped := w.runState(w.step(2).Run); !skipped.terminal() {
		t.Errorf("the skipped run should be terminal: %s", skipped.Status)
	}
	reason := dig(w.stopHook(), "reason")
	if !strings.Contains(reason, "report") || strings.Contains(reason, "failed") || strings.Contains(reason, "retry") {
		t.Errorf("the hold is for the do step and says nothing of the failure:\n%s", reason)
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
	lead, _ := w.spawn("", "", "orchestrator")
	w.mustSeq("halt", "--reason", "not needed")
	w.wantStatus("active", "cancelled")
	if r := w.step(2).Reason; r != "not needed" {
		t.Errorf("reason %q", r)
	}
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
	if !strings.Contains(ctx, "update the changelog") || !strings.Contains(ctx, "ensign sequence done 1.1") {
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
	if out := w.mustSeq("--all"); !strings.Contains(out, "halted") {
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
	if !strings.Contains(err.Error(), "ensign sequence discard") || strings.Contains(err.Error(), "ensign sequence halt") {
		t.Fatalf("a sequence waiting for go should be cleared with discard, not halt: %v", err)
	}
}

func TestPlainRunRefusedWhileAnApprovedSequenceIsUnfinished(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "tell the user")
	w.approve()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdRun([]string{"supernova/auth-refactor"}, &out)
	if err == nil || !strings.Contains(err.Error(), "ensign sequence run") {
		t.Fatalf("a plain run should point at ensign sequence run: %v", err)
	}
	if !strings.Contains(err.Error(), "ensign sequence halt") || strings.Contains(err.Error(), "ensign sequence discard") {
		t.Fatalf("an approved sequence should be cleared with halt, not discard: %v", err)
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
	carryOverSession(w.bridge, &HookInput{Source: "clear", SessionID: "s2"}, "s1")
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
	if open, err := sequenceUnfinished(w.bridge, w.sid); err != nil || open {
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
	if len(w.liveRuns()) != 1 {
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
	for _, ri := range w.liveRuns() {
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
	if err := retireIf(w.bridge, id, "cancelled: halted", "pending", "ask-user"); err != nil {
		t.Fatal(err)
	}
	if st := w.runState(id); st.Status != "running" {
		t.Errorf("run is %s, want running", st.Status)
	}
}

func TestSequenceFailedRunWaitsForADecisionAndStopHoldsTheTurnOnce(t *testing.T) {
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
	for i := 0; i < 2; i++ {
		if res := w.stopHook(); res["decision"] == "block" {
			t.Errorf("stop %d: the session has been told, so the turn should end: %v", i+2, res)
		}
	}
}

func TestSequenceDecisionKeepsALaterSequencesStepFromHoldingTheTurn(t *testing.T) {
	for _, kind := range []string{"do", "release"} {
		for _, goBeforeFailure := range []bool{true, false} {
			name := kind + "/go after the decision"
			if goBeforeFailure {
				name = kind + "/go while step 1 is active"
			}
			t.Run(name, func(t *testing.T) {
				w := releaseWorld(t, 0)
				w.mustSeq("run", "supernova/auth-refactor")
				w.approve()
				lead, _ := w.spawn("", "", "orchestrator")
				if kind == "do" {
					w.mustSeq("do", "write to the user")
				} else {
					w.mustSeq("release", "supernova", "1.0.0")
				}
				if goBeforeFailure {
					w.goContext()
				}
				w.failRun(lead)
				w.wantStatus("decide", "pending")
				if !goBeforeFailure {
					w.goContext()
				}
				res := w.stopHook()
				reason, _ := res["reason"].(string)
				if res["decision"] != "block" || !strings.Contains(reason, "sequence retry 1") || !strings.Contains(reason, "sequence accept 1") {
					t.Fatalf("the Stop hook should hold the turn and name both commands: %v", res)
				}
				for i := 0; i < seqStopBudget+1; i++ {
					if res := w.stopHook(); res["decision"] == "block" {
						t.Errorf("stop %d: the session has been told, so the later sequence's step must not hold the turn: %v", i+2, res)
					}
				}
				w.wantStatus("decide", "pending")
				if st := w.step(2); st.Blocks != 0 {
					t.Errorf("the later step never started, so it spent %d blocks", st.Blocks)
				}
				if kind == "do" {
					w.mustSeq("accept", "1.1")
					w.wantStatus("failed", "active")
				}
			})
		}
	}
}

func TestSequenceRetriedStepThatFailsAgainIsHeldOnceMore(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	if res := w.stopHook(); res["decision"] != "block" {
		t.Fatalf("the first failure should be put to the session: %v", res)
	}
	w.retryStep("1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide")
	if res := w.stopHook(); res["decision"] != "block" || !strings.Contains(dig(res, "reason"), "sequence retry 1") {
		t.Fatalf("the second failure should be put to the session: %v", res)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the session has been told: %v", res)
	}
}

func TestSequenceHoldsSpentWhileActiveDoNotCountForTheDecision(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	for i := 0; i < seqStopBudget; i++ {
		if res := w.stopHook(); res["decision"] != "block" {
			t.Fatalf("stop %d should hold for the undispatched lead: %v", i+1, res)
		}
	}
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide")
	if res := w.stopHook(); res["decision"] != "block" || !strings.Contains(dig(res, "reason"), "sequence retry 1") {
		t.Fatalf("the decision should be put to the session once: %v", res)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the session has been told: %v", res)
	}
}

func TestSequenceCarriedDecisionIsPutToTheContinuingSession(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	if res := w.stopHook(); res["decision"] != "block" {
		t.Fatalf("the old session should be told: %v", res)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Fatalf("the old session has been told: %v", res)
	}
	carryOverSession(w.bridge, &HookInput{Source: "clear", SessionID: "s2"}, "s1")
	w.sid = "s2"
	w.wantStatus("decide")
	if res := w.stopHook(); res["decision"] != "block" || !strings.Contains(dig(res, "reason"), "sequence retry 1") {
		t.Fatalf("the continuing session should be told: %v", res)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the continuing session has been told: %v", res)
	}
}

func TestSequenceKeepGoingReportIsDeliveredOnce(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	write(t, filepath.Join(ws, "junk-report.txt"), "x\n")
	w.failRun(lead)
	w.wantStatus("failed", "skipped")
	if !readSeq(w.bridge, w.sid).Sequences[0].Closed {
		t.Fatal("the failure closes the sequence")
	}
	r, err := readRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	res := w.stopHook()
	reason, _ := res["reason"].(string)
	if res["decision"] != "block" {
		t.Fatalf("the report should be put to the session: %v", res)
	}
	for _, want := range []string{"step 1.1 (run supernova/auth-refactor) failed", firstLine(w.step(1).Reason), "step 1.2 (run supernova/auth-refactor) skipped: step 1.1 failed", r.Snap.Workspace, "junk-report.txt", "task/auth-refactor", "must not retry"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the report lacks %q:\n%s", want, reason)
		}
	}
	if strings.Contains(reason, "sequence skip") || strings.Contains(reason, "sequence retry") {
		t.Errorf("nothing is offered to decide:\n%s", reason)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the report has been delivered: %v", res)
	}
}

func TestSequenceKeepGoingReportCoversEveryKindOfFailure(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "chore for the supernova")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.mustSeq("fail", "3", "--reason", "the chore could not be done")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("failed", "skipped", "failed", "failed")
	reason := dig(w.stopHook(), "reason")
	for _, want := range []string{"step 1.1 (run supernova/auth-refactor) failed", "step 1.2 (run supernova/auth-refactor) skipped: step 1.1 failed", "step 1.3 (do chore for the supernova) failed: the chore could not be done", "step 1.4 (run supernova/other) failed"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the report lacks %q:\n%s", want, reason)
		}
	}
	if n := strings.Count(reason, "ran in keep-going mode"); n != 1 {
		t.Errorf("one report, got %d:\n%s", n, reason)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the report has been delivered: %v", res)
	}
}

func TestSequenceKeepGoingReportRidesOnALaterSequencesTodo(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.mustSeq("do", "next supernova chore")
	w.approve()
	w.failRun(lead)
	first := dig(w.stopHook(), "reason")
	if !strings.Contains(first, "next supernova chore") || strings.Count(first, "ran in keep-going mode") != 1 {
		t.Errorf("the report goes in the todo block once:\n%s", first)
	}
	for i := 0; i < 3; i++ {
		if again := dig(w.stopHook(), "reason"); strings.Contains(again, "ran in keep-going mode") {
			t.Errorf("stop %d repeats the report:\n%s", i+2, again)
		}
	}
}

func TestSequenceKeepGoingReportRidesOnAGoReply(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.mustSeq("do", "next supernova chore")
	if n := strings.Count(w.goContext(), "ran in keep-going mode"); n != 1 {
		t.Errorf("the go reply carries the report once, got %d", n)
	}
	for i := 0; i < 3; i++ {
		if again := dig(w.stopHook(), "reason"); strings.Contains(again, "ran in keep-going mode") {
			t.Errorf("stop %d repeats the report:\n%s", i+1, again)
		}
	}
}

func TestSequenceKeepGoingRefusesRetryAcceptAndSkip(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("failed", "active")
	for _, args := range [][]string{{"retry", "1.1", "--diagnosis", "why", "--state", "left"}, {"accept", "1.1"}, {"skip", "1.1"}} {
		if _, err := w.seq(args...); err == nil || !strings.Contains(err.Error(), "not waiting for a decision") {
			t.Errorf("%v should be refused: %v", args, err)
		}
	}
	w.wantStatus("failed", "active")
}

func TestSequenceKeepGoingEndsAStepAlreadyDeciding(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide", "pending", "pending")
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Mode = modeKeepGoing
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.stopHook()
	w.wantStatus("failed", "skipped", "active")
	if st := w.step(1); !st.skipped() || strings.HasPrefix(st.Reason, skipPrefix) {
		t.Errorf("the step ended as a keep-going failure: %+v", st)
	}
}

func TestSequenceKeepGoingRefusesAStepAlreadyDecidingAndEndsIt(t *testing.T) {
	for _, verb := range []string{"retry", "accept", "skip"} {
		w := newWorld(t)
		w.addTask("other")
		w.mustSeq("run", "supernova/auth-refactor")
		w.mustSeq("run", "supernova/auth-refactor")
		w.mustSeq("run", "supernova/other")
		w.approve()
		lead, _ := w.spawn("", "", "orchestrator")
		w.failRun(lead)
		w.wantStatus("decide", "pending", "pending")
		if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
			sf.Sequences[0].Mode = modeKeepGoing
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		args := []string{verb, "1.1"}
		if verb == "retry" {
			args = append(args, "--diagnosis", "why", "--state", "left")
		}
		if _, err := w.seq(args...); err == nil || !strings.Contains(err.Error(), "not waiting for a decision") {
			t.Errorf("%s should be refused: %v", verb, err)
		}
		w.wantStatus("failed", "skipped", "pending")
		w.stopHook()
		w.wantStatus("failed", "skipped", "active")
		if st := w.step(1); st.SkippedAt == 0 {
			t.Errorf("%s: the step has no SkippedAt: %+v", verb, st)
		}
	}
}

func TestSequenceKeepGoingRunAskingTheUserStillPauses(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "keep-going")
	w.approve()
	r, err := openRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "which database?"
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	w.stopHook()
	w.wantStatus("paused")
	if got := w.step(1).Reason; !strings.Contains(got, "which database?") {
		t.Errorf("the pause carries the question: %q", got)
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
	if !strings.Contains(listing, "attempt 2") || !strings.Contains(listing, "the fixture path was wrong") {
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
	w.wantStatus("decide", "pending")
	if sf := readSeq(w.bridge, w.sid); sf.Sequences[0].Halted {
		t.Error("the sequence should wait for the user, not halt")
	}
	w.mustSeq("accept", "1")
	w.wantStatus("failed", "cancelled")
	if sf := readSeq(w.bridge, w.sid); !sf.Sequences[0].Halted {
		t.Error("the sequence should be halted")
	}
}

func TestSequenceBadRetriesAreRefusedAndASecondRetryWorks(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("publish", "supernova/auth-refactor")
	w.mustSeq("do", "report")
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
	w.wantStatus("decide")
	out := w.mustSeq("retry", "1", "--diagnosis", "again", "--state", "left")
	if !strings.Contains(out, "attempt 3") {
		t.Errorf("a second retry should be the third attempt:\n%s", out)
	}
	w.wantStatus("active")
	r, err := readRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(r.Snap.WorkItem, "## Current state"); n != 1 {
		t.Errorf("the work item should have one retry section, has %d:\n%s", n, r.Snap.WorkItem)
	}
	if !strings.Contains(r.Snap.WorkItem, "again") || strings.Contains(r.Snap.WorkItem, "the fixture path was wrong") {
		t.Errorf("the retry section should carry the latest state only:\n%s", r.Snap.WorkItem)
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
	_, st := readSeq(w.bridge, w.sid).step(1, 1)
	if st.Started {
		t.Fatal("not started yet")
	}
	withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		_, st := sf.step(1, 1)
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
		_, st := sf.step(1, 1)
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
	if _, st := sf.step(1, 1); st == nil || st.Status != "done" {
		t.Fatalf("the new owner should hold the finished release: %+v", sf)
	}
	if _, st := sf.step(1, 2); st == nil || st.Status != "active" {
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
	if got := strings.Join(mustLive(t, w.bridge, w.sid), ","); got != b {
		t.Fatalf("live runs %q, want %q", got, b)
	}
	hr, err := openHookRun(w.bridge, &HookInput{SessionID: w.sid, AgentID: "agentB"})
	if err != nil || hr == nil || hr.State.Run != b {
		t.Fatalf("the live run's agent lost its run: %v", err)
	}
	hr.close()
}

func TestSequenceDropCancelsAMiddleStepAndKeepsTheRest(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.addTask("third")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("run", "supernova/third")
	w.approve()
	out := w.mustSeq("drop", "2", "--reason", "not now")
	w.wantStatus("active", "cancelled", "pending")
	if !strings.Contains(out, "[cancelled] not now") {
		t.Errorf("the listing should show the reason:\n%s", out)
	}
	if sf := readSeq(w.bridge, w.sid); sf.Sequences[0].Halted {
		t.Error("drop must not halt the sequence")
	}
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	w.wantStatus("done", "cancelled", "active")
}

func TestSequenceDropOfARunStepDropsItsPublishStep(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("publish", "supernova/other")
	w.approve()
	w.mustSeq("drop", "2")
	w.wantStatus("active", "cancelled", "cancelled")
	if r := w.step(2).Reason; r != "dropped by the user" {
		t.Errorf("reason %q", r)
	}
}

func TestSequenceDropOfAPublishStepLeavesItsRunStep(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("publish", "supernova/other")
	w.approve()
	w.mustSeq("drop", "3")
	w.wantStatus("active", "pending", "cancelled")
}

func TestSequenceDropSeveralSteps(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.addTask("third")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("do", "tell the user")
	w.mustSeq("run", "supernova/third")
	w.approve()
	w.mustSeq("drop", "2", "4", "--reason", "scope")
	w.wantStatus("active", "cancelled", "pending", "cancelled")
}

func TestSequenceDropRefusesWhatIsNotPending(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	for _, args := range [][]string{{"drop", "1"}, {"drop", "2", "1"}, {"drop", "9"}, {"drop"}} {
		if _, err := w.seq(args...); err == nil {
			t.Errorf("ensign sequence %v should be refused", args)
		}
	}
	w.wantStatus("active", "pending")
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	w.wantStatus("done", "active")
	if _, err := w.seq("drop", "1"); err == nil || !strings.Contains(err.Error(), "done") {
		t.Errorf("a done step can't be dropped: %v", err)
	}
	w.wantStatus("done", "active")
}

func (w *world) wantRetired(ids ...string) {
	w.t.Helper()
	for _, id := range ids {
		if r, err := readRun(w.bridge, id); err != nil || !r.State.terminal() {
			w.t.Errorf("run %s should be retired: %v", id, err)
		}
	}
}

func TestSequenceDropRemovesStepsOfTheSequenceWaitingForGo(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.addTask("third")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	runs := w.stepRuns(2)
	w.mustSeq("run", "supernova/third")
	out := w.mustSeq("drop", "2")
	if strings.Contains(out, "cancelled") || strings.Contains(out, "halted") || strings.Contains(out, "supernova/other") {
		t.Errorf("the dropped step should be gone from the listing:\n%s", out)
	}
	w.wantRetired(runs...)
	w.mustSeq("run", "supernova/other")
	sf := readSeq(w.bridge, w.sid)
	if len(sf.Sequences) != 1 || sf.Sequences[0].Halted || sf.Sequences[0].Closed {
		t.Fatalf("want one open sequence: %+v", sf.Sequences)
	}
	var tasks []string
	for i, st := range sf.Sequences[0].Steps {
		if st.N != i+1 || st.Status != "pending" {
			t.Errorf("step %d is numbered %d, %s", i+1, st.N, st.Status)
		}
		tasks = append(tasks, st.Task)
	}
	if got := strings.Join(tasks, ","); got != "supernova/auth-refactor,supernova/third,supernova/other" {
		t.Errorf("steps %s", got)
	}
}

func TestSequenceDropOfARunStepRemovesItsPublishStepBeforeGo(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("publish", "supernova/other")
	w.mustSeq("drop", "2")
	if n := len(readSeq(w.bridge, w.sid).Sequences[0].Steps); n != 1 {
		t.Errorf("%d steps left, want 1\n%s", n, sequenceListing(w.bridge, w.sid))
	}
	if _, err := w.seq("drop", "2"); err == nil {
		t.Error("step 2 is gone")
	}
}

func TestSequenceDropOfEveryStepRemovesTheSequence(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	runs := w.stepRuns(1)
	w.mustSeq("drop", "1")
	w.wantRetired(runs...)
	if sf := readSeq(w.bridge, w.sid); len(sf.Sequences) != 0 {
		t.Errorf("sequences left: %+v", sf.Sequences)
	}
	w.mustSeq("run", "supernova/auth-refactor")
	if sf := readSeq(w.bridge, w.sid); len(sf.Sequences) != 1 || sf.Sequences[0].N != 1 || len(sf.Sequences[0].Steps) != 1 {
		t.Errorf("want a fresh sequence 1 with one step: %+v", sf.Sequences)
	}
}

func TestSequenceDiscardThenRebuild(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	runs := append(w.stepRuns(1), w.stepRuns(2)...)
	if out := w.mustSeq("discard"); !strings.Contains(out, "discarded") {
		t.Errorf("discard output %q", out)
	}
	w.wantRetired(runs...)
	if sf := readSeq(w.bridge, w.sid); len(sf.Sequences) != 0 {
		t.Fatalf("sequences left: %+v", sf.Sequences)
	}
	if !seqExists(w.bridge, w.sid) {
		t.Error("the emptied file should have been rewritten, not left with the old content")
	}
	w.mustSeq("run", "supernova/other")
	out := w.mustSeq("--all")
	if strings.Contains(out, "halted") || strings.Contains(out, "auth-refactor") || !strings.Contains(out, "supernova/other") {
		t.Errorf("the rebuilt listing:\n%s", out)
	}
	if sf := readSeq(w.bridge, w.sid); len(sf.Sequences) != 1 || len(sf.Sequences[0].Steps) != 1 {
		t.Errorf("want one sequence of one step: %+v", sf.Sequences)
	}
}

func TestSequenceDiscardNeedsASequenceWaitingForGo(t *testing.T) {
	w := newWorld(t)
	if _, err := w.seq("discard"); err == nil {
		t.Error("discard with no sequence should fail")
	}
	if seqExists(w.bridge, w.sid) {
		t.Error("discard must not create a sequence file")
	}
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	if _, err := w.seq("discard"); err == nil || !strings.Contains(err.Error(), "halt") {
		t.Errorf("an approved sequence can't be discarded: %v", err)
	}
	w.wantStatus("active")
}

func TestSequenceDropOfAnApprovedSequenceCancelsAndKeepsTheStep(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	w.mustSeq("drop", "2")
	if n := len(readSeq(w.bridge, w.sid).Sequences[0].Steps); n != 2 {
		t.Errorf("%d steps, an approved sequence keeps a dropped step", n)
	}
	w.wantStatus("active", "cancelled")
}

func TestSequenceHaltRetiresAnApprovedRunWhoseLeadWasNeverDispatched(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	id := w.step(1).Run
	if st := w.runState(id); st.Status != "running" || !st.Approved || len(st.Dispatches) != 0 {
		t.Fatalf("setup: %s approved=%v", st.Status, st.Approved)
	}
	w.mustSeq("halt", "--reason", "changed my mind")
	w.wantStatus("cancelled")
	if st := w.runState(id); st.Status != "error" {
		t.Errorf("the run should be retired: %s", st.Status)
	}
	for i := 0; i <= seqStopBudget; i++ {
		if res := w.stopHook(); res["decision"] == "block" {
			t.Fatalf("a halted sequence should not demand a dispatch: %s", dig(res, "reason"))
		}
	}
}

func TestSequenceHaltOfAnUnapprovedSequenceSaysWhatItHalted(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "tell the user about supernova")
	out := w.mustSeq("halt", "--reason", "changed my mind")
	if !strings.Contains(out, "sequence 1 halted: changed my mind") {
		t.Errorf("halt should name the halted sequence and the reason:\n%q", out)
	}
}

func TestSequenceListingShowsOnlyOpenSequences(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 12; i++ {
		w.mustSeq("do", "tell the user about supernova")
		w.approve()
		w.mustSeq("done", strconv.Itoa(i)+".1")
	}
	w.mustSeq("do", "next supernova chore")
	ctx := w.goContext()
	if !strings.Contains(ctx, "next supernova chore") {
		t.Fatalf("the approval should list the new sequence:\n%s", ctx)
	}
	if strings.Contains(ctx, "sequence 1 (mode") || strings.Contains(ctx, "sequence 12 (mode") || len(ctx) > 5000 {
		t.Fatalf("the approval should not list finished sequences (%d chars):\n%s", len(ctx), ctx)
	}
	stop := dig(w.stopHook(), "reason")
	if !strings.Contains(stop, "waiting") || strings.Contains(stop, "sequence 1 (mode") || len(stop) > 5000 {
		t.Fatalf("the Stop text should not list finished sequences (%d chars):\n%s", len(stop), stop)
	}
	if out := w.mustSeq(); strings.Contains(out, "sequence 1 (mode") || !strings.Contains(out, "next supernova chore") {
		t.Fatalf("plain listing should show only the open sequence:\n%s", out)
	}
	if out := w.mustSeq("--all"); !strings.Contains(out, "sequence 1 (mode") || !strings.Contains(out, "sequence 12 (mode") {
		t.Fatalf("--all should list every sequence:\n%s", out)
	}
}

func TestSequenceListingNoneOpenPointsToAll(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "tell the user about supernova")
	w.approve()
	w.mustSeq("done", "1")
	out := w.mustSeq()
	if strings.Contains(out, "sequence 1 (mode") || !strings.Contains(out, "--all") {
		t.Fatalf("expected a pointer to --all:\n%s", out)
	}
}

func TestSequenceListingKeepsApprovedHalted(t *testing.T) {
	sf := &seqFile{Sequences: []*sequence{
		{N: 1, Mode: modeStop, Approved: true, Halted: true, Reason: "enough", Steps: []*seqStep{{N: 1, Kind: "do", Instruction: "first supernova chore", Status: "active"}}},
		{N: 2, Mode: modeStop, Approved: true, Halted: true, Reason: "enough", Closed: true, Steps: []*seqStep{{N: 2, Kind: "do", Instruction: "old supernova chore", Status: "cancelled"}}},
	}}
	out := sf.listing("")
	if !strings.Contains(out, "sequence 1 (mode") || !strings.Contains(out, "halted: enough") || strings.Contains(out, "sequence 2 (mode") {
		t.Fatalf("an approved halted sequence with open steps should be listed, a closed one not:\n%s", out)
	}
}

// bridgeRelease adds a @release to bridge.yaml that records its tag and $VERSION in the workspace.
func bridgeRelease(t *testing.T, w *world, tag string) {
	t.Helper()
	path := filepath.Join(w.bridge, "bridge.yaml")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(base)+"bindings:\n  release: { command: \"echo "+tag+" $VERSION >> released.txt\" }\n")
}

func TestSequenceReleaseFromTheBridge(t *testing.T) {
	w := newWorld(t)
	bridgeRelease(t, w, "bridge")
	w.mustSeq("release", "supernova", "1.0.0")
	w.goContext()
	w.mustSeq("release-run", "1")
	if got := w.released(); got != "bridge 1.0.0" {
		t.Errorf("the bridge's @release should run, got %q", got)
	}
}

func TestSequenceReleaseProjectOverridesBridge(t *testing.T) {
	w := releaseWorld(t, 0)
	bridgeRelease(t, w, "bridge")
	w.mustSeq("release", "supernova", "1.0.0")
	w.goContext()
	w.mustSeq("release-run", "1")
	if got := w.released(); got != "1.0.0" {
		t.Errorf("the project's @release should win over the bridge's, got %q", got)
	}
}

func TestSequenceReleaseFromAnExtension(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "extensions", "shipper", "extension.yaml"),
		"name: shipper\nbindings:\n  release: { command: \"echo shipper $VERSION >> released.txt\" }\n")
	path := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "use: [shipper]\n"+string(base))
	w.mustSeq("release", "supernova", "1.0.0")
	w.goContext()
	w.mustSeq("release-run", "1")
	if got := w.released(); got != "shipper 1.0.0" {
		t.Errorf("the extension's @release should run, got %q", got)
	}
}

func TestSequenceStepsNumberFromOneInEachSequence(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	w.approve()
	w.mustSeq("done", "1")
	out := w.mustSeq("do", "second supernova chore")
	if !strings.Contains(out, "sequence 2 (mode") || !strings.Contains(out, "  1  do") {
		t.Fatalf("the second sequence should number its first step 1:\n%s", out)
	}
	ctx := w.goContext()
	if !strings.Contains(ctx, "step 2.1:") || !strings.Contains(ctx, "ensign sequence done 2.1") {
		t.Fatalf("text outside the listing should name the step <seq>.<step>:\n%s", ctx)
	}
	w.mustSeq("done", "1") // the only open sequence is the second
	w.wantStatus("done", "done")
}

func TestSequenceStepAddressedBySequenceAndStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore", "--name", "hygiene fixes")
	w.mustSeq("do", "second supernova chore")
	w.approve()
	w.mustSeq("do", "third supernova chore", "--name", "docs pass")
	w.wantStatus("active", "pending", "pending")

	_, err := w.seq("done", "1")
	if err == nil || !strings.Contains(err.Error(), `sequence 1 "hygiene fixes"`) || !strings.Contains(err.Error(), `sequence 2 "docs pass"`) || !strings.Contains(err.Error(), "<seq>.<step>") {
		t.Fatalf("a plain number with two open sequences should be refused, naming them: %v", err)
	}
	w.wantStatus("active", "pending", "pending")
	if _, err := w.seq("done", "2.1"); err == nil {
		t.Error("a pending step can't be marked done")
	}
	if _, err := w.seq("done", "9.1"); err == nil || !strings.Contains(err.Error(), "no sequence 9") {
		t.Errorf("an unknown sequence should be named: %v", err)
	}
	if _, err := w.seq("done", "1.9"); err == nil || !strings.Contains(err.Error(), "no step 9") {
		t.Errorf("an unknown step should be named: %v", err)
	}
	if _, err := w.seq("done", "x.1"); err == nil {
		t.Error("a malformed step should be refused")
	}
	w.mustSeq("done", "1.1")
	w.wantStatus("done", "active", "pending")
}

func TestSequencePlainStepNeedsAnOpenSequence(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "supernova chore")
	w.approve()
	w.mustSeq("done", "1")
	_, err := w.seq("done", "1")
	if err == nil || !strings.Contains(err.Error(), "<seq>.<step>") {
		t.Errorf("with no open sequence a plain number should ask for <seq>.<step>: %v", err)
	}
}

func TestSequenceFileWithSessionWideStepNumbersStillWorks(t *testing.T) {
	w := newWorld(t)
	old := `{"next_step":4,"sequences":[` +
		`{"n":1,"mode":"stop","approved":true,"closed":true,"steps":[` +
		`{"n":1,"kind":"do","instruction":"old supernova chore","status":"done"},` +
		`{"n":2,"kind":"do","instruction":"older supernova chore","status":"done"}]},` +
		`{"n":2,"mode":"stop","approved":true,"steps":[` +
		`{"n":3,"kind":"do","instruction":"current supernova chore","status":"active"},` +
		`{"n":4,"kind":"do","instruction":"later supernova chore","status":"pending"}]}]}`
	write(t, seqPath(w.bridge, w.sid), old)
	out := w.mustSeq()
	if !strings.Contains(out, "  1  do      current supernova chore") || !strings.Contains(out, "  2  do      later supernova chore") {
		t.Fatalf("the listing should renumber the steps from 1:\n%s", out)
	}
	if _, err := w.seq("done", "2.2"); err == nil {
		t.Error("a pending step can't be marked done")
	}
	w.mustSeq("done", "2.1")
	w.wantStatus("done", "done", "done", "active")
	data, err := os.ReadFile(seqPath(w.bridge, w.sid))
	if err != nil || strings.Contains(string(data), "next_step") {
		t.Errorf("the rewritten file should not carry next_step: %v\n%s", err, data)
	}
}

func TestSequenceNameShowsWhereTheSequenceNumberDoes(t *testing.T) {
	w := newWorld(t)
	out := w.mustSeq("do", "first supernova chore", "--name", "  hygiene fixes ")
	if !strings.Contains(out, `sequence 1 "hygiene fixes" (mode stop, not approved yet)`) {
		t.Fatalf("the listing header should carry the name:\n%s", out)
	}
	if ctx := w.goContext(); !strings.Contains(ctx, `sequence 1 "hygiene fixes" approved (1 steps).`) {
		t.Fatalf("the approval should carry the name:\n%s", ctx)
	}
	out = w.mustSeq("done", "1")
	if !strings.Contains(out, `sequence 1 "hygiene fixes" finished.`) {
		t.Errorf("the finish message should carry the name:\n%s", out)
	}
	w.mustSeq("do", "second supernova chore", "--name", "docs pass")
	w.approve()
	if out := w.mustSeq("halt", "--reason", "enough"); !strings.Contains(out, `sequence 2 "docs pass" halted: enough`) {
		t.Errorf("the halt message should carry the name:\n%s", out)
	}
	out = w.mustSeq("do", "third supernova chore")
	if !strings.Contains(out, "sequence 3 (mode stop, not approved yet)") || strings.Contains(out, `sequence 3 "`) {
		t.Errorf("an unnamed sequence should read as before:\n%s", out)
	}
	if ctx := w.goContext(); !strings.Contains(ctx, "sequence 3 approved (1 steps).") {
		t.Errorf("an unnamed approval should read as before:\n%s", ctx)
	}
}

func TestSequenceNameBelongsToTheFirstStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	_, err := w.seq("do", "second supernova chore", "--name", "hygiene fixes")
	if err == nil || !strings.Contains(err.Error(), "first step") {
		t.Fatalf("a name on a step that joins a sequence should be refused: %v", err)
	}
	if sf := readSeq(w.bridge, w.sid); len(sf.Sequences[0].Steps) != 1 || sf.Sequences[0].Name != "" {
		t.Errorf("the refused step must not be added: %+v", sf.Sequences[0])
	}
}

func TestSequenceNameIsValidated(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"", "   ", `say "hi"`, "two\nlines", strings.Repeat("x", 61)} {
		if _, err := w.seq("do", "supernova chore", "--name", name); err == nil {
			t.Errorf("name %q should be refused", name)
		}
	}
	if _, err := w.seq("do", "supernova chore", "--name"); err == nil {
		t.Error("--name without a value should be refused")
	}
	if sf := readSeq(w.bridge, w.sid); len(sf.Sequences) != 0 {
		t.Errorf("a refused name must not start a sequence: %+v", sf)
	}
	w.mustSeq("do", "supernova chore", "--name", strings.Repeat("x", 60))
}

func TestSequenceHaltOfAGroupWithNoLeadStartedCancelsTheStep(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	w.mustSeq("halt", "--reason", "changed my mind")
	w.wantStatus("cancelled")
	for _, id := range ids {
		if st := w.runState(id); st.Status != "error" || st.Reason != "cancelled: changed my mind" {
			t.Errorf("run %s: %s %q", id, st.Status, st.Reason)
		}
	}
	if l := readSeq(w.bridge, w.sid).listingAll(w.bridge); !strings.Contains(l, "[cancelled] changed my mind") || strings.Contains(l, "[failed]") {
		t.Errorf("listing:\n%s", l)
	}
}

func TestSequenceHaltOfAGroupWithOneRunGoingCancelsTheStepWhenItPasses(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	lead := w.leadOf(ids[1])
	w.mustSeq("halt", "--reason", "changed my mind")
	w.wantStatus("active")
	if st := w.runState(ids[0]); st.Status != "error" || st.Reason != "cancelled: changed my mind" {
		t.Fatalf("the run without a lead should be retired: %s %q", st.Status, st.Reason)
	}
	w.passLoc(lead)
	w.wantStatus("cancelled")
	if r := w.step(1).Reason; r != "changed my mind" {
		t.Errorf("reason %q", r)
	}
	if l := readSeq(w.bridge, w.sid).listingAll(w.bridge); !strings.Contains(l, "[cancelled] changed my mind") || strings.Contains(l, "[failed]") {
		t.Errorf("listing:\n%s", l)
	}
}

func TestSequenceHaltOfAGroupWithOneRunGoingFailsTheStepWithTheRealReason(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	lead := w.leadOf(ids[1])
	w.mustSeq("halt", "--reason", "changed my mind")
	w.wantStatus("active")
	w.failRun(lead)
	w.wantStatus("failed")
	real := w.runState(ids[1]).Reason
	if r := w.step(1).Reason; r == "" || r != real || strings.HasPrefix(r, haltPrefix) {
		t.Errorf("reason %q, want the failed run's %q", r, real)
	}
	if l := readSeq(w.bridge, w.sid).listingAll(w.bridge); !strings.Contains(l, "[failed] "+real) {
		t.Errorf("listing should show the real failure %q:\n%s", real, l)
	}
}

func TestSequenceHaltOfAPausedGroupCancelsTheStepWhenTheRestFinish(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	lead := w.leadOf(ids[1])
	r, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "another cycle?"
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	w.stopHook()
	w.wantStatus("paused")
	w.mustSeq("halt", "--reason", "changed my mind")
	w.wantStatus("active")
	if st := w.runState(ids[0]); st.Status != "error" || st.Reason != "cancelled: changed my mind" {
		t.Fatalf("the paused run should be retired: %s %q", st.Status, st.Reason)
	}
	w.passLoc(lead)
	w.wantStatus("cancelled")
	if l := readSeq(w.bridge, w.sid).listingAll(w.bridge); !strings.Contains(l, "[cancelled] changed my mind") || strings.Contains(l, "[failed]") {
		t.Errorf("listing:\n%s", l)
	}
}

func TestSequenceDropOfAPendingGroupStepInAnApprovedSequenceRetiresItsRuns(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	locTask(t, w, "other", "models")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(2)
	if len(ids) != 2 {
		t.Fatalf("a pending group step should hold its queued runs: %v", ids)
	}
	out := w.mustSeq("drop", "2", "--reason", "not now")
	w.wantStatus("active", "cancelled")
	if !strings.Contains(out, "[cancelled] not now") {
		t.Errorf("the listing should show the reason:\n%s", out)
	}
	for _, id := range ids {
		if st := w.runState(id); st.Status != "error" || st.Reason != "cancelled: not now" {
			t.Errorf("run %s: %s %q", id, st.Status, st.Reason)
		}
	}
}

func TestSequenceDropOfAGroupStepInTheOpenSequenceRemovesItAndRetiresItsRuns(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	locTask(t, w, "other", "models")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("run", "supernova/t")
	ids := w.stepRuns(2)
	if len(ids) != 2 {
		t.Fatalf("a group step should hold its queued runs: %v", ids)
	}
	w.mustSeq("drop", "2", "--reason", "not now")
	if n := len(readSeq(w.bridge, w.sid).Sequences[0].Steps); n != 1 {
		t.Errorf("%d steps, the dropped step should be gone", n)
	}
	for _, id := range ids {
		if st := w.runState(id); st.Status != "error" || st.Reason != "dropped: not now" {
			t.Errorf("run %s: %s %q", id, st.Status, st.Reason)
		}
	}
}

func TestSequenceHaltOfAPausedGroupKeepsARealFailure(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	w.failRun(w.leadOf(ids[1]))
	r, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "another cycle?"
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	w.stopHook()
	w.wantStatus("paused")
	w.mustSeq("halt", "--reason", "changed my mind")
	w.wantStatus("failed")
	real := w.runState(ids[1]).Reason
	if r := w.step(1).Reason; r == "" || r != real || strings.HasPrefix(r, haltPrefix) {
		t.Errorf("reason %q, want the failed run's %q", r, real)
	}
	if l := readSeq(w.bridge, w.sid).listingAll(w.bridge); !strings.Contains(l, "[failed] "+real) {
		t.Errorf("listing should show the real failure %q:\n%s", real, l)
	}
}

func TestSequenceSkipContinuesAndCancelsThePublishStep(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("publish", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide", "pending", "pending")
	out := w.mustSeq("skip", "1")
	if !strings.Contains(out, "has no uncommitted changes") {
		t.Errorf("skip should tell the state of the workspace even with nothing affected:\n%s", out)
	}
	w.wantStatus("failed", "cancelled", "active")
	if sf := readSeq(w.bridge, w.sid); sf.Sequences[0].Halted {
		t.Error("skip must not halt the sequence")
	}
	if !strings.Contains(out, "skipped by the user") {
		t.Errorf("the listing should show the skip:\n%s", out)
	}
}

func TestSequenceSkipRefusesWhatIsNotDeciding(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "report")
	w.approve()
	for _, n := range []string{"1", "2", "3"} {
		if _, err := w.seq("skip", n); err == nil {
			t.Errorf("skip %s should be refused", n)
		}
	}
	w.wantStatus("active", "pending", "pending")

	k := newWorld(t)
	k.mustSeq("run", "supernova/auth-refactor")
	k.mustSeq("mode", "keep-going")
	k.approve()
	lead, _ := k.spawn("", "", "orchestrator")
	k.failRun(lead)
	k.wantStatus("failed")
	if _, err := k.seq("skip", "1.1"); err == nil || !strings.Contains(err.Error(), "not waiting for a decision") {
		t.Errorf("skip in keep-going has nothing to decide: %v", err)
	}
}

func TestSequenceSkipNamesTheStepsItMayAffect(t *testing.T) {
	w := releaseWorld(t, 0)
	w.addTask("other")
	w.addTask("auth-refactor-2")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("release", "supernova", "1.2.3")
	w.mustSeq("do", "update the docs for supernova/auth-refactor")
	w.mustSeq("do", "look at auth-refactor-2")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	res := w.stopHook()
	reason, _ := res["reason"].(string)
	check := func(where, text string) {
		t.Helper()
		for _, want := range []string{"step 1.2, release: supernova 1.2.3", "step 1.3, do: update the docs", "step 1.6, run: supernova/auth-refactor"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s should name %q:\n%s", where, want, text)
			}
		}
		for _, bad := range []string{"1.4", "1.5", "supernova/other"} {
			if strings.Contains(text, "step "+bad) || strings.Contains(text, bad+",") {
				t.Errorf("%s should not name %s:\n%s", where, bad, text)
			}
		}
	}
	check("the Stop hook", reason)
	check("skip", w.mustSeq("skip", "1"))
}

func TestSequenceSkipNamesStepsOfLaterSequences(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	path := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Replace(string(base), "bindings:\n", "bindings:\n  release: { command: \"true\" }\n", 1))
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("publish", "supernova/auth-refactor")
	w.approve()
	w.mustSeq("release", "supernova", "1.2.3", "--name", "later")
	w.mustSeq("do", "update the docs for supernova/auth-refactor")
	w.mustSeq("do", "look at other")
	w.mustSeq("publish", "supernova/auth-refactor")
	w.mustSeq("publish", "supernova/other")
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	reason, _ := w.stopHook()["reason"].(string)
	check := func(where, text string) {
		t.Helper()
		if !strings.Contains(text, "no uncommitted changes") {
			t.Errorf("%s should carry the workspace state:\n%s", where, text)
		}
		for _, want := range []string{"step 2.1, release: supernova 1.2.3", "step 2.2, do: update the docs", "step 2.4, publish: supernova/auth-refactor (cancelled when reached, unless the task runs again and passes first in that location)"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s should name %q:\n%s", where, want, text)
			}
		}
		for _, bad := range []string{"step 1.2", "step 2.3", "step 2.5"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s should not name %s:\n%s", where, bad, text)
			}
		}
	}
	check("the Stop hook", reason)
	check("skip", w.mustSeq("skip", "1.1"))
}

func TestNamesTask(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"fix supernova/auth-refactor now", true},
		{"fix Auth-Refactor.", true},
		{"auth-refactor-2", false},
		{"pre-auth-refactor", false},
		{"other/auth-refactor", false},
		{"task/auth-refactor", true},
		{"my-task/auth-refactor", false},
		{"other/auth-refactor and auth-refactor", true},
		{"supernova/auth-refactor-2", false},
		{"unrelated", false},
	}
	for _, c := range cases {
		if got := namesTask(c.text, "supernova/auth-refactor"); got != c.want {
			t.Errorf("namesTask(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestSequenceStopOffersSkipOnlyInStopMode(t *testing.T) {
	for _, mode := range []string{"stop", "keep-going"} {
		w := newWorld(t)
		w.mustSeq("run", "supernova/auth-refactor")
		w.mustSeq("mode", mode)
		w.approve()
		lead, _ := w.spawn("", "", "orchestrator")
		w.failRun(lead)
		reason, _ := w.stopHook()["reason"].(string)
		for _, offer := range []string{"sequence skip 1", "sequence accept 1", "sequence retry 1"} {
			if got := strings.Contains(reason, offer); got != (mode == "stop") {
				t.Errorf("%s mode: offers %q = %v:\n%s", mode, offer, got, reason)
			}
		}
		if !strings.Contains(reason, "uncommitted") {
			t.Errorf("%s mode: the workspace text is missing:\n%s", mode, reason)
		}
	}
}

// haltWithRunLockHeld runs halt while the test holds the lock of a run, as a finishing hook does, and checks halt does not hold the sequence lock while it waits for that run. It returns once halt has finished.
func (w *world) haltWithRunLockHeld(run string) {
	w.t.Helper()
	l := flock.New(filepath.Join(runsDir(w.bridge), run, "lock"))
	if err := l.Lock(); err != nil {
		w.t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			l.Unlock()
		}
	}
	defer release()
	halted := make(chan error, 1)
	go func() {
		_, err := w.seq("halt", "--reason", "stop")
		halted <- err
	}()
	saved := make(chan bool, 1)
	go func() {
		for {
			var h bool
			withSeq(w.bridge, w.sid, func(sf *seqFile) error {
				h = len(sf.Sequences) > 0 && sf.Sequences[0].Halted
				return nil
			})
			if h {
				saved <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case <-saved:
	case <-time.After(10 * time.Second):
		release()
		<-halted
		w.t.Fatal("halt kept the sequence lock while waiting for a run lock")
	}
	release()
	select {
	case err := <-halted:
		if err != nil {
			w.t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		w.t.Fatal("halt did not finish after the run lock was released")
	}
}

func TestSequenceHaltOfAnActiveRunStepTakesNoRunLockUnderTheSequenceLock(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	ids := w.stepRuns(1)
	w.haltWithRunLockHeld(ids[0])
	if st := w.runState(ids[0]); st.Status != "error" || st.Reason != "cancelled: stop" {
		t.Errorf("the unstarted run should be retired: %s %q", st.Status, st.Reason)
	}
	w.wantStatus("cancelled")
	if st := w.step(1); st.Reason != "stop" {
		t.Errorf("reason %q", st.Reason)
	}
}

func TestSequenceHaltOfADecidingRunStepTakesNoRunLockUnderTheSequenceLock(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	ids := w.stepRuns(1)
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("decide")
	w.haltWithRunLockHeld(ids[0])
	w.wantStatus("cancelled")
	if st := w.step(1); st.Reason != "stop" {
		t.Errorf("reason %q", st.Reason)
	}
}

func TestSequenceHaltOfAPausedGroupRetiresAnApprovedRunWithNoLead(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	r, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "another cycle?"
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	w.stopHook()
	w.wantStatus("paused")
	w.mustSeq("halt", "--reason", "stop")
	for _, id := range ids {
		if st := w.runState(id); st.Status != "error" || st.Reason != "cancelled: stop" {
			t.Errorf("run %s: %s %q", id, st.Status, st.Reason)
		}
	}
	w.wantStatus("cancelled")
	if !readSeq(w.bridge, w.sid).Sequences[0].Closed {
		t.Error("the sequence should be closed")
	}
}

func TestSequenceDiscardOfAHaltedSequenceWithAStepStillActiveSaysSo(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	w.leadOf(ids[1])
	w.mustSeq("halt")
	w.wantStatus("active")
	_, err := w.seq("discard")
	if err == nil || !strings.Contains(err.Error(), readSeq(w.bridge, w.sid).Sequences[0].label()) || strings.Contains(err.Error(), "ensign sequence halt") {
		t.Errorf("discard error: %v", err)
	}
}

func TestSequenceHaltSaysSoWhenARunCouldNotBeRetired(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	w.leadOf(ids[1])
	block := filepath.Join(runsDir(w.bridge), ids[0], "state.json.tmp")
	if err := os.Mkdir(block, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := w.seq("halt", "--reason", "stop")
	if err == nil || !strings.Contains(err.Error(), ids[0]) || !strings.Contains(err.Error(), "ensign sequence halt again") {
		t.Fatalf("halt error: %v", err)
	}
	if strings.Contains(err.Error(), ids[1]) {
		t.Errorf("the run with a lead was left alone and should not be named: %v", err)
	}
	if !readSeq(w.bridge, w.sid).Sequences[0].Halted {
		t.Error("the sequence should still be saved halted")
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	w.mustSeq("halt", "--reason", "stop")
	if st := w.runState(ids[0]); st.Status != "error" || st.Reason != "cancelled: stop" {
		t.Errorf("the run should be retired by the second halt: %s %q", st.Status, st.Reason)
	}
}

func TestSequenceHaltOfARetriedPendingGroupStepRetiresOnlyTheFreshRun(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		st := sf.Sequences[0].Steps[0]
		st.Status = "pending"
		st.runs()[0].Status = "error"
		st.runs()[1].Status = "pending"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.haltWithRunLockHeld(ids[0])
	if st := w.runState(ids[1]); st.Status != "error" || st.Reason != "cancelled: stop" {
		t.Errorf("the fresh run should be retired: %s %q", st.Status, st.Reason)
	}
	if st := w.runState(ids[0]); st.Reason == "cancelled: stop" {
		t.Errorf("the kept run should not be touched: %s %q", st.Status, st.Reason)
	}
	w.wantStatus("cancelled")
}

func TestSequenceAttemptCountsFromTheStepsFields(t *testing.T) {
	for _, c := range []struct {
		st   seqStep
		want int
	}{{seqStep{}, 1}, {seqStep{Prev: "r1"}, 2}, {seqStep{Prev: "r2", Retries: 1}, 2}, {seqStep{Prev: "r3", Retries: 2}, 3}} {
		if got := c.st.attempt(); got != c.want {
			t.Errorf("%+v: attempt %d, want %d", c.st, got, c.want)
		}
	}
}

func TestSequenceEveryFailedAttemptWaitsForTheUser(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.approve()
	var runs []string
	for attempt := 1; attempt <= 4; attempt++ {
		lead, _ := w.spawn("", "", "orchestrator")
		runs = append(runs, w.step(1).Run)
		w.failRun(lead)
		w.wantStatus("decide", "pending")
		if sf := readSeq(w.bridge, w.sid); sf.Sequences[0].Halted {
			t.Fatalf("attempt %d halted the sequence", attempt)
		}
		if l := w.mustSeq(); !strings.Contains(l, fmt.Sprintf("(attempt %d", attempt)) {
			t.Errorf("the listing should show attempt %d:\n%s", attempt, l)
		}
		if attempt < 4 {
			w.retryStep("1")
		}
	}
	for i := 1; i < len(runs); i++ {
		if got := w.runState(runs[i]).RetryOf; got != runs[i-1] {
			t.Errorf("run %d retries %q, want %q", i+1, got, runs[i-1])
		}
	}
	if st := w.step(1); st.Retries != 3 {
		t.Errorf("retries %d, want 3", st.Retries)
	}
}

func TestSequenceSkipAndAcceptWorkAfterRetries(t *testing.T) {
	for _, cmd := range []string{"skip", "accept"} {
		w := newWorld(t)
		w.mustSeq("run", "supernova/auth-refactor")
		w.mustSeq("do", "later")
		w.approve()
		for i := 0; i < 2; i++ {
			lead, _ := w.spawn("", "", "orchestrator")
			w.failRun(lead)
			w.retryStep("1")
		}
		lead, _ := w.spawn("", "", "orchestrator")
		w.failRun(lead)
		w.wantStatus("decide", "pending")
		w.mustSeq(cmd, "1")
		if cmd == "skip" {
			w.wantStatus("failed", "active")
			if !w.step(1).skipped() {
				t.Error("the step should be skipped")
			}
		} else {
			w.wantStatus("failed", "cancelled")
			if w.step(1).skipped() {
				t.Error("an accepted step is not skipped")
			}
		}
	}
}

func TestSequenceDecideTextOffersRetrySkipHaltInOrder(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "stop")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	text, _ := w.stopHook()["reason"].(string)
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "- ") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "- retry: `ensign sequence retry 1.1 --diagnosis") || !strings.HasPrefix(lines[1], "- skip: ") || !strings.HasPrefix(lines[2], "- halt: ") {
		t.Fatalf("want retry, skip, halt in that order:\n%s", text)
	}
	wantSkip, wantHalt := "`ensign sequence skip 1.1`", "`ensign sequence accept 1.1`"
	if !strings.Contains(lines[1], wantSkip) || !strings.Contains(lines[2], wantHalt) {
		t.Errorf("wrong commands:\n%s", text)
	}
	bare := text
	for {
		i := strings.Index(bare, "`")
		if i < 0 {
			break
		}
		j := strings.Index(bare[i+1:], "`")
		bare = bare[:i] + bare[i+1+j+1:]
	}
	if strings.Contains(bare, "accept") {
		t.Errorf("the word accept is said outside a command:\n%s", text)
	}
}

func TestSequenceDecideTextRequiresAFullBriefEveryTime(t *testing.T) {
	var briefs []string
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "stop")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	text, _ := w.stopHook()["reason"].(string)
	for _, want := range []string{
		"full brief, every time, even if an earlier message already said the same thing",
		"what failed and why, in plain words",
		"what state the work is in",
		"what each of retry, skip and halt would do",
		"every decision a retry needs, with each option spelled out and a recommendation",
		"Never refer to an option by letter or by pointing at an earlier message",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "full brief") {
			t.Errorf("the brief must be a plain paragraph: %s", l)
		}
		if strings.Contains(l, "full brief") {
			briefs = append(briefs, l)
		}
	}
	if len(briefs) != 1 || briefs[0] != decideBriefText[:len(decideBriefText)-1] {
		t.Errorf("the brief is not the shared text: %q", briefs)
	}
}

func TestSequenceKeepGoingHaltByHandWithAFailedRunGivesTheReportOnce(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "later")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.wantStatus("failed", "active")
	if out := w.mustSeq("halt", "--reason", "enough"); strings.Count(out, "halted: enough") != 1 {
		t.Errorf("the halt output should say halted: enough once:\n%s", out)
	}
	w.wantStatus("failed", "cancelled")
	reason := dig(w.stopHook(), "reason")
	for _, want := range []string{"ran in keep-going mode", "step 1.1 (run supernova/auth-refactor) failed: ", "  folder: ", "cancelled: enough"} {
		if strings.Count(reason, want) != 1 {
			t.Errorf("the close should have %q once:\n%s", want, reason)
		}
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "ran in keep-going mode") {
		t.Errorf("the report has been delivered:\n%s", reason)
	}
}

func TestSequenceKeepGoingHaltByHandWithNothingFailedGivesNoFailureReport(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("publish", "supernova/auth-refactor")
	w.mustSeq("mode", "keep-going")
	w.approve()
	w.mustSeq("halt", "--reason", "enough")
	w.wantStatus("cancelled", "cancelled")
	seq := readSeq(w.bridge, w.sid).Sequences[0]
	if !seq.Closed {
		t.Fatalf("the halted sequence should be closed: %+v", seq)
	}
	if strings.Contains(seq.Report, "ran in keep-going mode") {
		t.Errorf("a halt with nothing failed gets no failure report:\n%s", seq.Report)
	}
	line := w.step(2).publishLine("cancelled", w.step(2).Reason)
	reason := dig(w.stopHook(), "reason")
	if strings.Contains(reason, "ran in keep-going mode") || strings.Count(reason, line) != 1 {
		t.Errorf("want the cancelled publish line %q once and no failure report:\n%s", line, reason)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, line) {
		t.Errorf("the publish line has been delivered:\n%s", reason)
	}
}

// laterPublish adds an approved sequence holding one pending publish of the task after the session's open ones, closing those, and returns the publish.
func (w *world) laterPublish(task string) *seqStep {
	w.t.Helper()
	pub := &seqStep{N: 1, Kind: "publish", Task: task, Status: "pending"}
	err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		for _, seq := range sf.Sequences {
			seq.Closed = true
		}
		sf.Sequences = append(sf.Sequences, &sequence{N: len(sf.Sequences) + 1, Mode: modeKeepGoing, Approved: true, Steps: []*seqStep{pub}})
		_, err := sf.advanceParts(w.bridge, w.sid)
		pub.Status = sf.Sequences[len(sf.Sequences)-1].Steps[0].Status
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return pub
}

func TestSequenceKeepGoingLostRunIsSkippedAndCancelsALaterPublish(t *testing.T) {
	for _, mode := range []string{"keep-going", "stop"} {
		w := newWorld(t)
		task := "supernova/auth-refactor"
		w.mustSeq("run", task)
		w.mustSeq("mode", mode)
		w.approve()
		w.spawn("", "", "orchestrator")
		if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), w.step(1).Run)); err != nil {
			t.Fatal(err)
		}
		if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error { sf.reconcile(w.bridge); return nil }); err != nil {
			t.Fatal(err)
		}
		st := w.step(1)
		if st.Status != "failed" {
			t.Fatalf("%s: the lost run's step is %s", mode, st.Status)
		}
		if got := st.SkippedAt != 0; got != (mode == "keep-going") {
			t.Errorf("%s: SkippedAt set is %v: %+v", mode, got, st)
		}
		if mode == "keep-going" {
			if pub := w.laterPublish(task); pub.Status != "cancelled" {
				t.Errorf("a later publish of the lost run's task is %s, want cancelled", pub.Status)
			}
		}
	}
}

func TestSequenceKeepGoingRunThatCannotStartIsSkippedAndCancelsALaterPublish(t *testing.T) {
	for _, mode := range []string{"keep-going", "stop"} {
		w := newWorld(t)
		task := "supernova/auth-refactor"
		w.mustSeq("run", task)
		w.mustSeq("mode", mode)
		if err := retireIf(w.bridge, w.step(1).Run, "gone", "pending"); err != nil {
			t.Fatal(err)
		}
		w.goContext()
		st := w.step(1)
		if st.Status != "failed" {
			t.Fatalf("%s: the step is %s", mode, st.Status)
		}
		if got := st.SkippedAt != 0; got != (mode == "keep-going") {
			t.Errorf("%s: SkippedAt set is %v: %+v", mode, got, st)
		}
		if mode == "keep-going" {
			if pub := w.laterPublish(task); pub.Status != "cancelled" {
				t.Errorf("a later publish of the task is %s, want cancelled", pub.Status)
			}
		}
	}
}

func TestSequenceKeepGoingRunThatFailsInAHaltedSequenceIsSkipped(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Halted, sf.Sequences[0].Reason = true, "enough"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.failRun(lead)
	if st := w.step(1); st.Status != "failed" || st.SkippedAt == 0 {
		t.Errorf("a run that fails for real in a halted keep-going sequence is skipped: %+v", st)
	}
}

func TestSkipWorkspaceTextNamesTheFoldersState(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	r, err := readRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	folder := r.Snap.Workspace

	text := skipWorkspaceText(w.bridge, w.step(1))
	if !strings.Contains(text, folder) || !strings.Contains(text, "has no uncommitted changes") || strings.Contains(text, "committed or stashed") {
		t.Errorf("a clean folder:\n%s", text)
	}

	for i := 0; i < 12; i++ {
		write(t, filepath.Join(ws, fmt.Sprintf("junk-%02d.txt", i)), "x\n")
	}
	text = skipWorkspaceText(w.bridge, w.step(1))
	for _, want := range []string{folder, "task/auth-refactor", "junk-00.txt", "junk-09.txt", "and 2 more", "committed or stashed"} {
		if !strings.Contains(text, want) {
			t.Errorf("a dirty folder should say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "junk-10.txt") || strings.Contains(text, "failed run's") {
		t.Errorf("the list is capped and doesn't claim whose work it is:\n%s", text)
	}
	if out := w.mustSeq("skip", "1"); !strings.Contains(out, "and 2 more") {
		t.Errorf("skip should carry the warning:\n%s", out)
	}
}

func TestSkipWorkspaceTextFallsBackWhenTheStateCantBeRead(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	if text := skipWorkspaceText(w.bridge, w.step(1)); !strings.Contains(text, skipWorkspaceUnknown) || !strings.Contains(text, "could not be read") {
		t.Errorf("a folder that is not a git repository:\n%s", text)
	}
	os.Remove(filepath.Join(runsDir(w.bridge), w.step(1).Run, "state.json"))
	if text := skipWorkspaceText(w.bridge, w.step(1)); text != skipWorkspaceUnknown {
		t.Errorf("an unreadable run:\n%s", text)
	}
}

// staleMarker writes the passing-run marker of the task as an earlier passing run left it, matching the task branch's head.
func (w *world) staleMarker(ws string) {
	w.t.Helper()
	sha := runGit(w.t, ws, "rev-parse", "task/auth-refactor")
	write(w.t, testedFile(w.bridge, "supernova/auth-refactor", ""), sha+"\n")
}

func TestSequencePublishOfASkippedTaskIsCancelledInALaterSequence(t *testing.T) {
	task := "supernova/auth-refactor"
	for _, queuedFirst := range []bool{true, false} {
		w, ws, remote, _ := githubWorld(t, "success", localTest)
		mainBefore := runGit(t, ".", "--git-dir", remote, "rev-parse", "main")
		w.mustSeq("run", task)
		w.approve()
		if queuedFirst {
			w.mustSeq("publish", task, "--name", "later")
			w.approve()
		}
		lead, _ := w.spawn("", "", "orchestrator")
		write(t, filepath.Join(ws, "lib.txt"), "two\n")
		runGit(t, ws, "commit", "-q", "-am", "work")
		w.failRun(lead)
		w.staleMarker(ws)
		runGit(t, ws, "checkout", "-q", "task/auth-refactor")
		w.mustSeq("skip", "1.1")
		var out string
		if queuedFirst {
			out = dig(w.stopHook(), "reason")
		} else {
			w.mustSeq("publish", task)
			out = w.goContext()
		}
		if !strings.Contains(out, "step 2.1 (publish supernova/auth-refactor) cancelled: step 1.1 was skipped") || strings.Contains(out, "Step 0/") {
			t.Errorf("queuedFirst=%v: the session should be told of the cancel, with no position:\n%s", queuedFirst, out)
		}
		w.wantStatus("failed", "cancelled")
		if got := runGit(t, ".", "--git-dir", remote, "rev-parse", "main"); got != mainBefore {
			t.Errorf("queuedFirst=%v: nothing may be pushed to main: %s", queuedFirst, got)
		}
		if sf := readSeq(w.bridge, w.sid); sf.Sequences[1].Halted {
			t.Errorf("queuedFirst=%v: the cancel must not halt the later sequence", queuedFirst)
		}
	}
}

func TestSequencePublishOfASkippedTaskProceedsWhenTheTaskRanAgain(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.approve()
	w.mustSeq("run", task, "--name", "again")
	w.mustSeq("publish", task)
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.mustSeq("skip", "1.1")
	w.approve()
	lead, _ = w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the second run should pass: %s", why)
	}
	w.wantStatus("failed", "done", "done")
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the publish should land the rerun's work, got %q", got)
	}
}

func TestSequenceClosingSaysWhatWasSkipped(t *testing.T) {
	for _, c := range []struct {
		skips int
		want  string
	}{
		{0, "sequence 1 finished."},
		{1, "sequence 1 finished with a skipped step: step 1.1."},
		{2, "sequence 1 finished with 2 skipped steps: 1.1, 1.2."},
	} {
		w := newWorld(t)
		for i := 0; i < c.skips; i++ {
			w.mustSeq("run", "supernova/auth-refactor")
		}
		w.mustSeq("do", "report")
		w.approve()
		for i := 0; i < c.skips; i++ {
			lead, _ := w.spawn("", "", "orchestrator")
			w.failRun(lead)
			w.mustSeq("skip", fmt.Sprintf("1.%d", i+1))
		}
		out := w.mustSeq("done", strconv.Itoa(c.skips+1))
		if !strings.Contains(out, c.want) {
			t.Errorf("%d skips: want %q in\n%s", c.skips, c.want, out)
		}
	}
}

func TestSequenceKeepGoingFailureIsASkipForLaterPublishes(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	mainBefore := runGit(t, ".", "--git-dir", remote, "rev-parse", "main")
	w.mustSeq("run", task)
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-q", "-am", "work")
	w.failRun(lead)
	w.staleMarker(ws)
	runGit(t, ws, "checkout", "-q", "task/auth-refactor")
	if r := w.step(1).Reason; strings.HasPrefix(r, skipPrefix) || !w.step(1).skipped() {
		t.Errorf("the failed step keeps the run's reason and counts as skipped: %q", r)
	}
	w.mustSeq("publish", task)
	out := w.goContext()
	if !strings.Contains(out, "step 2.1 (publish supernova/auth-refactor) cancelled: step 1.1 was skipped") {
		t.Errorf("the later publish should be cancelled:\n%s", out)
	}
	if r := w.step(2).Reason; r != "step 1.1 was skipped" {
		t.Errorf("the cancel reason: %q", r)
	}
	if got := runGit(t, ".", "--git-dir", remote, "rev-parse", "main"); got != mainBefore {
		t.Errorf("nothing may be pushed to main: %s", got)
	}
}

func TestSequenceSkippedBeforeTracksLocationAndLaterRuns(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "skiploc", "client, server")
	var out bytes.Buffer
	newRun := func(loc, status string) string {
		t.Helper()
		time.Sleep(2 * time.Millisecond)
		r, err := createRun(w.bridge, runOpts{task: task, location: loc}, true, &out)
		if err != nil {
			t.Fatal(err)
		}
		rf, err := readRun(w.bridge, r.Run)
		if err != nil {
			t.Fatal(err)
		}
		rf.State.Status = status
		if status == "success" {
			rf.State.FinishedAt = time.Now().UnixNano()
		}
		if err := rf.save(); err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	step := func(n int, kind, status, reason string, runs ...string) *seqStep {
		st := &seqStep{N: n, Kind: kind, Task: task, Status: status, Reason: reason}
		var rs []*seqRun
		for _, r := range runs {
			rs = append(rs, &seqRun{Run: r})
		}
		st.setRuns(rs)
		return st
	}
	skip := skipPrefix + "broken"
	newRun("client", "success")
	failed := newRun("client", "error")
	skipped := step(1, "run", "failed", skip, failed)
	skipped.SkippedAt = time.Now().UnixNano()
	// The run step before the publish pins its location to the client.
	pinned := newRun("client", "pending")
	pub := step(2, "publish", "pending", "")
	seq1 := &sequence{N: 1, Steps: []*seqStep{skipped}}
	seq2 := &sequence{N: 2, Steps: []*seqStep{step(1, "run", "pending", "", pinned), pub}}
	sf := &seqFile{Sequences: []*sequence{seq1, seq2}}

	if got, _ := sf.skippedBefore(w.bridge, seq2, pub); got != skipped {
		t.Fatalf("a publish alone in a later sequence should see the skip in its location: %v", got)
	}

	cancelled := newRun("client", "error")
	seq1.Steps = append(seq1.Steps, step(2, "run", "cancelled", "dropped", cancelled))
	if got, _ := sf.skippedBefore(w.bridge, seq2, pub); got != skipped {
		t.Errorf("a cancelled later run step must not clear the skip: %v", got)
	}

	newRun("server", "success")
	if got, _ := sf.skippedBefore(w.bridge, seq2, pub); got != skipped {
		t.Errorf("a passing run in another location must not clear the skip: %v", got)
	}

	newRun("client", "success")
	if got, _ := sf.skippedBefore(w.bridge, seq2, pub); got != nil {
		t.Errorf("a plain passing run after the skip should clear it: %v", got)
	}
}

func TestSequenceSkippedBeforeComparesEveryRunOfAGroup(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "skipgroup", "client, server")
	var out bytes.Buffer
	var runs []*seqRun
	for _, loc := range []string{"client", "server"} {
		r, err := createRun(w.bridge, runOpts{task: task, location: loc}, true, &out)
		if err != nil {
			t.Fatal(err)
		}
		rf, err := readRun(w.bridge, r.Run)
		if err != nil {
			t.Fatal(err)
		}
		rf.State.Status = "success"
		rf.State.FinishedAt = time.Now().UnixNano()
		if loc == "client" {
			rf.State.Status = "error"
		}
		if err := rf.save(); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, &seqRun{Run: r.Run})
	}
	skipped := &seqStep{N: 1, Kind: "run", Task: task, Status: "failed", Reason: skipPrefix + "broken"}
	skipped.setRuns(runs)
	pub := &seqStep{N: 2, Kind: "publish", Task: task, Status: "pending"}
	seq := &sequence{N: 1, Steps: []*seqStep{skipped, pub}}
	sf := &seqFile{Sequences: []*sequence{seq}}
	// The publish acts in the group's last run's location, the server; the skipped step has a run there, though its failed run was the client's.
	if got, _ := sf.skippedBefore(w.bridge, seq, pub); got != skipped {
		t.Errorf("a skipped group step with a run in the publish's location should stand: %v", got)
	}
}

func TestSequenceSkippedBeforeWithoutAResolvableLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "skipnoloc", "client, server")
	var out bytes.Buffer
	newRun := func(status string) {
		t.Helper()
		time.Sleep(2 * time.Millisecond)
		r, err := createRun(w.bridge, runOpts{task: task, location: "client"}, true, &out)
		if err != nil {
			t.Fatal(err)
		}
		rf, err := readRun(w.bridge, r.Run)
		if err != nil {
			t.Fatal(err)
		}
		rf.State.Status = status
		if status == "success" {
			rf.State.FinishedAt = time.Now().UnixNano()
		}
		if err := rf.save(); err != nil {
			t.Fatal(err)
		}
	}
	newRun("error")
	failed := w.listRuns()[0].id
	skipped := &seqStep{N: 1, Kind: "run", Task: task, Status: "failed", Reason: skipPrefix + "broken", SkippedAt: time.Now().UnixNano()}
	skipped.setRuns([]*seqRun{{Run: failed}})
	pub := &seqStep{N: 1, Kind: "publish", Task: task, Status: "pending"}
	seq1 := &sequence{N: 1, Steps: []*seqStep{skipped}}
	seq2 := &sequence{N: 2, Steps: []*seqStep{pub}}
	sf := &seqFile{Sequences: []*sequence{seq1, seq2}}
	if _, err := loadConfigAt(w.bridge, task, ""); err == nil {
		t.Fatal("the task has no success to follow and two locations; its location should not resolve")
	}
	if got, _ := sf.skippedBefore(w.bridge, seq2, pub); got != skipped {
		t.Errorf("the skip should apply when the location can't be resolved: %v", got)
	}
	newRun("success")
	if got, _ := sf.skippedBefore(w.bridge, seq2, pub); got != nil {
		t.Errorf("a passing run after the skip should clear it: %v", got)
	}
}

func TestSequenceSkippedBeforeAStandingSkipWithoutATime(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "skipold", "client")
	var out bytes.Buffer
	r, err := createRun(w.bridge, runOpts{task: task, location: "client"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	ok, err := createRun(w.bridge, runOpts{task: task, location: "client"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := readRun(w.bridge, ok.Run)
	if err != nil {
		t.Fatal(err)
	}
	rf.State.Status, rf.State.FinishedAt = "success", time.Now().UnixNano()
	if err := rf.save(); err != nil {
		t.Fatal(err)
	}
	skipped := &seqStep{N: 1, Kind: "run", Task: task, Status: "failed", Reason: skipPrefix + "broken"}
	skipped.setRuns([]*seqRun{{Run: r.Run}})
	pub := &seqStep{N: 2, Kind: "publish", Task: task, Status: "pending"}
	seq := &sequence{N: 1, Steps: []*seqStep{skipped, pub}}
	sf := &seqFile{Sequences: []*sequence{seq}}
	if got, _ := sf.skippedBefore(w.bridge, seq, pub); got != skipped {
		t.Errorf("a skip with no time stands: %v", got)
	}
}

func TestSequencePublishRunsWhenTheTaskRanAfterARetriedThenSkippedStep(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.approve()
	w.mustSeq("run", task, "--name", "again")
	w.mustSeq("publish", task)
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.retryStep("1.1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.mustSeq("skip", "1.1")
	w.approve()
	lead, _ = w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the second sequence's run should pass: %s", why)
	}
	w.wantStatus("failed", "done", "done")
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the publish should land the rerun's work, got %q", got)
	}
}

func TestSequencePublishIsCancelledWhenTheRetrySucceededBeforeALaterSkip(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	mainBefore := runGit(t, ".", "--git-dir", remote, "rev-parse", "main")
	w.mustSeq("run", task)
	w.approve()
	w.mustSeq("run", task, "--name", "second")
	w.approve()
	w.mustSeq("publish", task, "--name", "third")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.retryStep("1.1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the retry should pass: %s", why)
	}
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.mustSeq("skip", "2.1")
	w.wantStatus("done", "failed", "cancelled")
	if got := runGit(t, ".", "--git-dir", remote, "rev-parse", "main"); got != mainBefore {
		t.Errorf("nothing may be pushed to main: %s", got)
	}
}

func TestSequenceSkippedBeforeSurvivesADeletedRunFolder(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "skipgone", "client, server")
	var out bytes.Buffer
	newRun := func(loc string) string {
		t.Helper()
		r, err := createRun(w.bridge, runOpts{task: task, location: loc}, true, &out)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	for _, recorded := range []bool{true, false} {
		failed := newRun("client")
		ru := &seqRun{Run: failed}
		if recorded {
			ru.Location = "client"
		}
		skipped := &seqStep{N: 1, Kind: "run", Task: task, Status: "failed", Reason: skipPrefix + "broken", SkippedAt: time.Now().UnixNano()}
		skipped.setRuns([]*seqRun{ru})
		pub := &seqStep{N: 2, Kind: "publish", Task: task, Status: "pending"}
		seq := &sequence{N: 1, Steps: []*seqStep{skipped, pub}}
		sf := &seqFile{Sequences: []*sequence{seq}}
		if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), failed)); err != nil {
			t.Fatal(err)
		}
		if got, _ := sf.skippedBefore(w.bridge, seq, pub); got != skipped {
			t.Errorf("recorded=%v: a skip must stand when its run folder is gone: %v", recorded, got)
		}
	}
}

func TestSequenceDecideTextListsTheTasksOwnStepsInStopMode(t *testing.T) {
	w := releaseWorld(t, 0)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("release", "supernova", "1.2.3")
	w.mustSeq("mode", "stop")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	text, _ := w.stopHook()["reason"].(string)
	if !strings.Contains(text, "step 1.3, release") {
		t.Errorf("the release step should be listed:\n%s", text)
	}
	if !strings.Contains(text, "step 1.2, run") {
		t.Errorf("own pending run step should be listed:\n%s", text)
	}
}

func TestSequenceStopMessageCarriesOnlyTheStepAndItsPosition(t *testing.T) {
	// The step the hook repeats through advance's text.
	w := newWorld(t)
	for _, what := range []string{"first supernova chore", "second supernova chore", "third supernova chore"} {
		w.mustSeq("do", what)
	}
	w.approve()
	w.mustSeq("done", "1.1")
	reason := dig(w.stopHook(), "reason")
	if !strings.Contains(reason, "second supernova chore") || strings.Count(reason, "Step 2/3") != 1 || strings.Contains(reason, "sequence 1 (mode") {
		t.Fatalf("the Stop message should carry Step 2/3 and no listing:\n%s", reason)
	}
	if strings.Contains(reason, "\n\n\n") {
		t.Errorf("the Stop message has a stray blank line:\n%q", reason)
	}

	// The step is started by the Stop hook's own advance, which repeats its text.
	w2 := newWorld(t)
	for _, what := range []string{"first supernova chore", "second supernova chore", "third supernova chore"} {
		w2.mustSeq("do", what)
	}
	w2.approve()
	err := withSeq(w2.bridge, w2.sid, func(sf *seqFile) error {
		sf.Sequences[0].Steps[0].Status = "done"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reason = dig(w2.stopHook(), "reason")
	if n := strings.Count(reason, "second supernova chore"); n != 1 {
		t.Errorf("the step text should appear once, got %d:\n%s", n, reason)
	}
	if strings.Count(reason, "Step 2/3") != 1 || strings.Contains(reason, "sequence 1 (mode") || strings.Contains(reason, "\n\n\n") {
		t.Fatalf("the Stop message should carry Step 2/3, no listing, no stray blank line:\n%q", reason)
	}
}

func TestSequenceGoMessageCarriesOnlyThePosition(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	w.mustSeq("do", "second supernova chore")
	ctx := w.goContext()
	if !strings.Contains(ctx, "first supernova chore") || strings.Count(ctx, "Step 1/2") != 1 || strings.Contains(ctx, "sequence 1 (mode") {
		t.Fatalf("go should start Step 1/2 and not list the sequence:\n%s", ctx)
	}

	// The first sequence still holds its step, so approving the second only queues it.
	w.mustSeq("do", "third supernova chore")
	w.mustSeq("do", "fourth supernova chore")
	ctx = w.goContext()
	if !strings.Contains(ctx, "approved (2 steps)") || strings.Count(ctx, "Step 1/4") != 1 || strings.Contains(ctx, "sequence 2 (mode") || strings.Contains(ctx, "\n\n\n") {
		t.Fatalf("go should name the position of the queued sequence and not list it:\n%q", ctx)
	}
}

func TestSequenceListingStaysWhereItIsKept(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	out := w.mustSeq("do", "second supernova chore")
	if !strings.Contains(out, "type go to approve the sequence") {
		t.Fatalf("adding should list the sequence:\n%s", out)
	}
	w.approve()
	if out := w.mustSeq(); !strings.Contains(out, "sequence 1 (mode") {
		t.Fatalf("the bare command should list the sequence:\n%s", out)
	}
	if out := w.mustSeq("done", "1.1"); !strings.Contains(out, "sequence 1 (mode") {
		t.Fatalf("done should still list the sequence:\n%s", out)
	}
}

func TestSequenceStopMessageLeavesAKeepGoingFailureToTheReport(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("do", "tell the supernova user")
	w.mustSeq("mode", "keep-going")
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
	w.wantStatus("done", "failed", "active")
	for i := 0; i < 2; i++ {
		reason := dig(w.stopHook(), "reason")
		if !strings.Contains(reason, "tell the supernova user") || strings.Count(reason, "Step 3/3") != 1 || strings.Contains(reason, "sequence 1 (mode") || strings.Contains(reason, "\n\n\n") {
			t.Fatalf("stop %d should carry the do step and its position, no listing:\n%q", i+1, reason)
		}
		if n := strings.Count(reason, "step 1.2 (publish "+task+") failed: "); n != 0 {
			t.Fatalf("stop %d should leave the failed publish to the report, got %d:\n%s", i+1, n, reason)
		}
	}
}

func TestSequenceStopThatAdvancesReportsAFailedPublishOnce(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	w.mustSeq("mode", "keep-going")
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
	w.wantStatus("done", "failed", "active")
	// Put the publish back so the Stop's own advance is what runs it and fails.
	err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		steps := sf.Sequences[0].Steps
		steps[1].Status, steps[1].Reason = "pending", ""
		steps[2].Status = "pending"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reason := dig(w.stopHook(), "reason")
	w.wantStatus("done", "failed", "active")
	if n := strings.Count(reason, "step 1.2 (publish "+task+") failed: "); n != 1 {
		t.Errorf("the failure should appear once, got %d:\n%s", n, reason)
	}
	if n := strings.Count(reason, "last supernova chore"); n != 1 {
		t.Errorf("the do step should appear once, got %d:\n%s", n, reason)
	}
	if strings.Contains(reason, "\n\n\n") || strings.Contains(reason, "sequence 1 (mode") {
		t.Errorf("stray blank line or listing:\n%q", reason)
	}
}

// divergeRemote pushes a commit to the remote's main from another clone, so that a publish of the world's task is refused.
func divergeRemote(t *testing.T, remote string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	write(t, filepath.Join(other, "news.txt"), "meanwhile\n")
	runGit(t, other, "add", "news.txt")
	runGit(t, other, "commit", "-q", "-m", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
}

// publishAdvancedByTheRun is a run, publish, do sequence whose run has passed, so that the run's finish advanced the publish and its text was discarded.
func publishAdvancedByTheRun(t *testing.T) (*world, string) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the run should pass: %s", why)
	}
	w.wantStatus("done", "done", "active")
	return w, task
}

func TestSequenceStopCarriesAPublishTheRunAdvancedWithItsPositionOnce(t *testing.T) {
	w, task := publishAdvancedByTheRun(t)
	line := "Step 2/3: step 1.2 (publish " + task + ") done: "
	reason := dig(w.stopHook(), "reason")
	if !strings.Contains(reason, "last supernova chore") || strings.Count(reason, line) != 1 {
		t.Errorf("the stop should carry the do step and the publish once:\n%s", reason)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "(publish ") {
		t.Errorf("the publish has been delivered:\n%s", reason)
	}
}

func TestSequencePromptDeliversAPublishTheRunAdvancedOnce(t *testing.T) {
	w, task := publishAdvancedByTheRun(t)
	line := "Step 2/3: step 1.2 (publish " + task + ") done: "
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "what happened?"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); strings.Count(ctx, line) != 1 {
		t.Errorf("the prompt should carry the publish once: %v", res)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "(publish ") {
		t.Errorf("the stop repeats the publish:\n%s", reason)
	}
	res, _ = w.hook("UserPromptSubmit", map[string]any{"prompt": "and now?"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); strings.Contains(ctx, "(publish ") {
		t.Errorf("the next prompt repeats the publish: %v", res)
	}
}

func TestSequenceSessionWordLeavesTheQueuedReportToTheNextStop(t *testing.T) {
	w, task := publishAdvancedByTheRun(t)
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "help"})
	if res["continue"] != false {
		t.Fatalf("help should be answered by the hook: %v", res)
	}
	if strings.Contains(fmt.Sprint(res), "(publish ") {
		t.Errorf("the session word's reply carries the report: %v", res)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Count(reason, "(publish "+task+") done: ") != 1 {
		t.Errorf("the stop should still deliver the publish:\n%s", reason)
	}
}

// failedPublishAdvancedByTheRun is a stop-mode sequence whose publish was refused when the run's finish advanced it.
func failedPublishAdvancedByTheRun(t *testing.T, then ...string) (*world, string) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	for _, c := range then {
		w.mustSeq("do", c)
	}
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	divergeRemote(t, remote)
	w.runStage()
	if got := w.step(2).Status; got != "failed" {
		t.Fatalf("the publish should fail, it is %s", got)
	}
	return w, task
}

func TestSequenceStopReportsAFailedPublishInTheLastStepOnce(t *testing.T) {
	w, task := failedPublishAdvancedByTheRun(t)
	res := w.stopHook()
	reason := dig(res, "reason")
	if res["decision"] != "block" || strings.Count(reason, "Step 2/2: step 1.2 (publish "+task+") failed: ") != 1 {
		t.Fatalf("the failed publish should reach the session once with its position: %v", res)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the failure has been delivered: %v", res)
	}
}

func TestSequencePromptReportsAFailedPublishInTheLastStepOnce(t *testing.T) {
	w, task := failedPublishAdvancedByTheRun(t)
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "what happened?"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); strings.Count(ctx, "Step 2/2: step 1.2 (publish "+task+") failed: ") != 1 {
		t.Fatalf("the failed publish should reach the session once with its position: %v", res)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the failure has been delivered: %v", res)
	}
}

// wantFailedPublishReportedOnce checks that sequence 1 is closed, that the next Stop or prompt carries the failure once, and that nothing repeats it.
func wantFailedPublishReportedOnce(t *testing.T, w *world, task string, atPrompt bool) {
	t.Helper()
	if !readSeq(w.bridge, w.sid).Sequences[0].Closed {
		t.Errorf("sequence 1 should be closed:\n%s", sequenceListing(w.bridge, w.sid))
	}
	line := "step 1.2 (publish " + task + ") failed: "
	if atPrompt {
		res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "what happened?"})
		if ctx := dig(res, "hookSpecificOutput", "additionalContext"); strings.Count(ctx, line) != 1 {
			t.Fatalf("the prompt should carry the failure once: %v", res)
		}
	} else {
		res := w.stopHook()
		if reason := dig(res, "reason"); res["decision"] != "block" || strings.Count(reason, line) != 1 {
			t.Fatalf("the stop should carry the failure once: %v", res)
		}
	}
	if res := w.stopHook(); strings.Contains(fmt.Sprint(res), line) {
		t.Errorf("a further stop repeats the failure: %v", res)
	}
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "and now?"})
	if strings.Contains(fmt.Sprint(res), line) {
		t.Errorf("a further prompt repeats the failure: %v", res)
	}
}

// failedPublishAdvancedByResume is a stop-mode run, publish sequence whose run passed with the publish still pending, so that `ensign resume` is the advance that runs the publish. The resumed run is saved as running before the advance, so the publish is refused because that run is still going.
func failedPublishAdvancedByResume(t *testing.T) (*world, string) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	w.runStage()
	if got := w.step(2).Status; got != "done" {
		t.Fatalf("the publish should land first, it is %s", got)
	}
	runID := w.state().Run
	err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		seq := sf.Sequences[0]
		seq.Closed, seq.Report = false, ""
		seq.Steps[1].Status, seq.Steps[1].Reason, seq.Steps[1].Queued = "pending", "", false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRun(w.bridge, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "which way?"
	r.State.Resume = &ResumePoint{Scope: "run", Stage: "code"}
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdResume([]string{"--answer", "left", "--run", runID}, &out); err != nil {
		t.Fatal(err)
	}
	if got := w.step(2).Status; got != "failed" {
		t.Fatalf("the resume's advance should fail the publish, it is %s", got)
	}
	if got := w.step(2).Reason; !strings.Contains(got, "still going") {
		t.Fatalf("the publish should be refused because the resumed run is live, its reason is %q", got)
	}
	return w, task
}

func TestSequenceStopReportsAFailedPublishAdvancedByResumeOnce(t *testing.T) {
	w, task := failedPublishAdvancedByResume(t)
	wantFailedPublishReportedOnce(t, w, task, false)
}

func TestSequencePromptReportsAFailedPublishAdvancedByResumeOnce(t *testing.T) {
	w, task := failedPublishAdvancedByResume(t)
	wantFailedPublishReportedOnce(t, w, task, true)
}

// failedPublishBesideAnOpenSequence sets up the same failed publish as failedPublishAdvancedByTheRun, with a second sequence added after the go, so that sequence 1 closes while sequence 2 stays open.
func failedPublishBesideAnOpenSequence(t *testing.T) (*world, string) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.approve()
	w.mustSeq("do", "later supernova chore")
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	divergeRemote(t, remote)
	w.runStage()
	if got := w.step(2).Status; got != "failed" {
		t.Fatalf("the publish should fail, it is %s", got)
	}
	return w, task
}

func TestSequenceStopReportsAFailedPublishWhenALaterSequenceIsOpenOnce(t *testing.T) {
	w, task := failedPublishBesideAnOpenSequence(t)
	wantFailedPublishReportedOnce(t, w, task, false)
	if seq := readSeq(w.bridge, w.sid).Sequences[1]; seq.Closed {
		t.Errorf("sequence 2 should stay open")
	}
}

func TestSequencePromptReportsAFailedPublishWhenALaterSequenceIsOpenOnce(t *testing.T) {
	w, task := failedPublishBesideAnOpenSequence(t)
	wantFailedPublishReportedOnce(t, w, task, true)
	if seq := readSeq(w.bridge, w.sid).Sequences[1]; seq.Closed {
		t.Errorf("sequence 2 should stay open")
	}
}

func TestSequenceFailedPublishWithALaterStepKeepsThePositionItWasShown(t *testing.T) {
	w, task := failedPublishAdvancedByTheRun(t, "last supernova chore")
	reason := dig(w.stopHook(), "reason")
	if strings.Count(reason, "Step 2/3: step 1.2 (publish "+task+") failed: ") != 1 || strings.Contains(reason, "Step 2/2") {
		t.Errorf("the position is the one shown when the earlier steps started:\n%s", reason)
	}
}

func TestSequenceKeepGoingReportHasAFailedPublishOnceAndKeepsAnEarlierPublish(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	w.runStage()
	w.wantStatus("done", "done", "active")
	w.mustSeq("fail", "3", "--reason", "the chore could not be done")
	if !readSeq(w.bridge, w.sid).Sequences[0].Closed {
		t.Fatal("the failure closes the sequence")
	}
	reason := dig(w.stopHook(), "reason")
	for _, want := range []string{"Step 2/3: step 1.2 (publish " + task + ") done: ", "step 1.3 (do last supernova chore) failed: the chore could not be done"} {
		if strings.Count(reason, want) != 1 {
			t.Errorf("the report should have %q once:\n%s", want, reason)
		}
	}
}

func TestSequenceKeepGoingReportListsAFailedPublishOnceWithItsPosition(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	divergeRemote(t, remote)
	w.runStage()
	w.wantStatus("done", "failed")
	reason := dig(w.stopHook(), "reason")
	if n := strings.Count(reason, "- Step 2/2: step 1.2 (publish "+task+") failed: "); n != 1 || strings.Count(reason, "ran in keep-going mode") != 1 {
		t.Errorf("the report should list the failed publish once with its position:\n%s", reason)
	}
	if res := w.stopHook(); res["decision"] == "block" {
		t.Errorf("the report has been delivered: %v", res)
	}
}

func TestSequenceCancelledPublishInAStopSequenceIsDeliveredOnce(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-q", "-am", "work")
	w.failRun(lead)
	w.staleMarker(ws)
	runGit(t, ws, "checkout", "-q", "task/auth-refactor")
	w.mustSeq("skip", "1.1")
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	got := w.goContext()
	for i := 0; i < 2; i++ {
		got += "\n" + dig(w.stopHook(), "reason")
	}
	if n := strings.Count(got, "(publish "+task+") cancelled: step 1.1 was skipped"); n != 1 || strings.Contains(got, "Step 0/") {
		t.Errorf("the cancelled publish should reach the session once, got %d:\n%s", n, got)
	}
}

func TestSequenceKeepGoingFailedPublishSurvivesAHaltByHand(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	w.addTask("other")
	if _, err := setWorkspace(w.bridge, "supernova/other", ws); err != nil {
		t.Fatal(err)
	}
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("run", "supernova/other")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	divergeRemote(t, remote)
	w.runStage()
	w.wantStatus("done", "failed", "active")
	w.spawn("", "", "orchestrator")
	w.mustSeq("halt", "--reason", "enough")
	w.mustSeq("do", "later supernova chore")
	line := "Step 2/3: step 1.2 (publish " + task + ") failed: "
	if ctx := w.goContext(); strings.Contains(ctx, "(publish ") {
		t.Errorf("go leaves the halted sequence's publish to the close:\n%s", ctx)
	}
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Steps[2].Status = "cancelled"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Count(reason, line) != 1 {
		t.Errorf("the close should deliver the publish once with its position:\n%s", reason)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "(publish ") {
		t.Errorf("the publish has been delivered:\n%s", reason)
	}
}

func TestSequenceKeepGoingHaltByHandDeliversAFailedPublishOnce(t *testing.T) {
	w, task := keepGoingFailedPublishWithAHeldDo(t)
	w.mustSeq("halt", "--reason", "enough")
	reason := dig(w.stopHook(), "reason")
	if n := strings.Count(reason, "Step 2/3: step 1.2 (publish "+task+") failed: "); n != 1 || strings.Count(reason, "ran in keep-going mode") != 1 {
		t.Errorf("the halted sequence should deliver the publish once, in the failure report, got %d:\n%s", n, reason)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "(publish ") {
		t.Errorf("the publish has been delivered:\n%s", reason)
	}
}

// keepGoingFailedPublishWithAHeldDo is a keep-going run, publish, do sequence whose publish was refused when the run's finish advanced it, leaving the do step active.
func keepGoingFailedPublishWithAHeldDo(t *testing.T) (*world, string) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	w.mustSeq("mode", "keep-going")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	divergeRemote(t, remote)
	w.runStage()
	w.wantStatus("done", "failed", "active")
	return w, task
}

func TestSequenceCLIAdvanceDoesNotPrintAQueuedPublish(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("do", "first supernova chore")
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	w.approve()
	out := w.mustSeq("done", "1")
	if strings.Contains(out, "(publish ") {
		t.Errorf("the CLI should leave the publish to the report:\n%s", out)
	}
	if n := strings.Count(dig(w.stopHook(), "reason"), "(publish "+task+") "); n != 1 {
		t.Errorf("the stop should deliver the publish once, got %d", n)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "(publish ") {
		t.Errorf("the publish has been delivered:\n%s", reason)
	}
}

func TestSequenceGoDeliversAFirstStepPublishOnce(t *testing.T) {
	w, _, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("publish", task)
	w.mustSeq("do", "last supernova chore")
	ctx := w.goContext()
	if n := strings.Count(ctx, "Step 1/2: step 1.1 (publish "+task+") "); n != 1 {
		t.Errorf("the go reply should carry the publish once, got %d:\n%s", n, ctx)
	}
	if reason := dig(w.stopHook(), "reason"); strings.Contains(reason, "(publish ") {
		t.Errorf("the stop repeats the publish:\n%s", reason)
	}
}

func TestSequenceApprovalNamesNoPositionWhileTheFirstStepWaitsOnAFolder(t *testing.T) {
	w := groupWorld(t, "01-client")
	w.mustSeq("run", "supernova/t")
	ids := w.stepRuns(1)
	run, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	folder := run.Snap.Workspace
	run.close()
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: "supernova/t"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	snap.Workspace, snap.Sides = folder, nil
	if err := writeJSON(filepath.Join(runsDir(w.bridge), snap.Run, "pipeline.json"), snap); err != nil {
		t.Fatal(err)
	}
	if _, err := startRun(w.bridge, "other-session", snap.Task, snap.Run); err != nil {
		t.Fatal(err)
	}
	ctx := w.goContext()
	if !strings.Contains(ctx, "can't start yet") || strings.Contains(ctx, "Now at") {
		t.Fatalf("go should say the step can't start yet and name no position:\n%s", ctx)
	}
}

func TestSequenceStopNotesACancelledStepAndNotADoneOne(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	w.mustSeq("do", "second\nsupernova chore")
	w.mustSeq("do", "third supernova chore")
	w.approve()
	err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		steps := sf.Sequences[0].Steps
		steps[0].Status, steps[0].Reason = "done", "fine"
		steps[1].Status, steps[1].Reason = "cancelled", "nothing left\nto do"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reason := dig(w.stopHook(), "reason")
	if !strings.Contains(reason, "step 1.2 (do second) cancelled: nothing left\n") || strings.Contains(reason, "to do") || strings.Contains(reason, "step 1.1 (") {
		t.Errorf("only the cancelled step should be noted, on one line:\n%s", reason)
	}
}

func TestSequenceGoReportsARunThatCannotStart(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("do", "report on the supernova")
	w.mustSeq("mode", "keep-going")
	if err := retireIf(w.bridge, w.step(1).Run, "gone", "pending"); err != nil {
		t.Fatal(err)
	}
	ctx := w.goContext()
	if n := strings.Count(ctx, "step 1.1 (run supernova/auth-refactor) failed: "); n != 1 {
		t.Errorf("the failure should be reported once, got %d:\n%s", n, ctx)
	}
	if strings.Count(ctx, "report on the supernova") != 1 || !strings.Contains(ctx, "Now at step 1.2 (Step 2/2).") {
		t.Errorf("go should dispatch the do step with its position:\n%s", ctx)
	}
}

func TestSequenceGoNamesTheHeldStepOfTheOlderSequence(t *testing.T) {
	w := newWorld(t)
	for _, what := range []string{"first supernova chore", "second supernova chore", "third supernova chore"} {
		w.mustSeq("do", what)
	}
	w.approve()
	w.mustSeq("done", "1.1")
	w.mustSeq("do", "later supernova chore")
	ctx := w.goContext()
	if !strings.Contains(ctx, "Now at step 1.2 (Step 2/4).") || strings.Contains(ctx, "Now at step 2.1") {
		t.Errorf("go should name the held step of the first sequence:\n%s", ctx)
	}

	w2 := newWorld(t)
	w2.mustSeq("run", "supernova/auth-refactor")
	w2.mustSeq("mode", "keep-going")
	retireIf(w2.bridge, w2.step(1).Run, "gone", "pending")
	if ctx := w2.goContext(); strings.Contains(ctx, "Now at") || !strings.Contains(ctx, "failed: ") {
		t.Errorf("go with no held step should print the failure and no position:\n%s", ctx)
	}
}

func TestEveryTerminalOutcomeStampsWhenTheRunEnded(t *testing.T) {
	w := newWorld(t)
	for _, tr := range []Transition{{Error: "handler"}, {AskUser: "question"}, {Finish: "done"}} {
		w.mustSeq("run", "supernova/auth-refactor", "--name", "stamp")
		id := w.step(1).Run
		r, err := openRun(w.bridge, id)
		if err != nil {
			t.Fatal(err)
		}
		before := time.Now().UnixNano()
		(&machine{snap: r.Snap, st: r.State}).finish(tr, "why")
		if r.State.FinishedAt < before {
			t.Errorf("%+v: FinishedAt %d, want at least %d", tr, r.State.FinishedAt, before)
		}
		r.close()
		w.mustSeq("halt")
	}
}

func TestMarkRetiredStampsAnUnendedRunAndKeepsAnEndedOnesTime(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	id := w.step(1).Run
	r, err := openRun(w.bridge, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := markRetired(r, "cancelled: x"); err != nil {
		t.Fatal(err)
	}
	st := w.runState(id)
	if st.FinishedAt == 0 {
		t.Fatal("a retired run should record when it ended")
	}
	r, err = openRun(w.bridge, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := markRetired(r, "cancelled: again"); err != nil {
		t.Fatal(err)
	}
	if got := w.runState(id).FinishedAt; got != st.FinishedAt {
		t.Errorf("a run that had ended kept no end time: %d, want %d", got, st.FinishedAt)
	}
}

// whileRunLockHeld runs fn while the test holds a run's lock, as a finishing hook does, and fails if fn does not finish while the lock is held. fn runs on its own goroutine, so it must not fail the test.
func (w *world) whileRunLockHeld(run string, fn func()) {
	w.t.Helper()
	l := flock.New(filepath.Join(runsDir(w.bridge), run, "lock"))
	if err := l.Lock(); err != nil {
		w.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		l.Unlock()
	case <-time.After(10 * time.Second):
		l.Unlock()
		<-done
		w.t.Fatal("the command waited for a run lock")
	}
}

// retriedGroupStep leaves step 1 of a two-run group pending, as a retry does: the first run is kept as a finished one, the second is queued.
func (w *world) retriedGroupStep() []string {
	w.t.Helper()
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		st := sf.Sequences[0].Steps[0]
		st.Status = "pending"
		st.runs()[0].Status = "success"
		st.runs()[1].Status = "pending"
		return nil
	}); err != nil {
		w.t.Fatal(err)
	}
	return ids
}

func (w *world) blockSave(run string) {
	w.t.Helper()
	if err := os.Mkdir(filepath.Join(runsDir(w.bridge), run, "state.json.tmp"), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func TestSequenceDropOfARetriedPendingStepLeavesAKeptRunAlone(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	ids := w.retriedGroupStep()
	var err error
	w.whileRunLockHeld(ids[0], func() { _, err = w.seq("drop", "1", "--reason", "enough") })
	if err != nil {
		t.Fatal(err)
	}
	if st := w.runState(ids[1]); st.Status != "error" || st.Reason != "cancelled: enough" {
		t.Errorf("the queued run should be retired: %s %q", st.Status, st.Reason)
	}
	if st := w.runState(ids[0]); st.Status == "error" {
		t.Errorf("the kept run should not be touched: %s %q", st.Status, st.Reason)
	}
}

func TestSequenceFailedCascadeOverARetriedPendingStepLeavesAKeptRunAlone(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	ids := w.retriedGroupStep()
	var err error
	w.whileRunLockHeld(ids[0], func() {
		err = withSeq(w.bridge, w.sid, func(sf *seqFile) error {
			seq := sf.Sequences[0]
			sf.failed(w.bridge, seq, &seqStep{Kind: "do", N: 9, Seq: seq.N})
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if st := w.runState(ids[1]); st.Status != "error" {
		t.Errorf("the queued run should be retired: %s", st.Status)
	}
	if st := w.runState(ids[0]); st.Status == "error" {
		t.Errorf("the kept run should not be touched: %s", st.Status)
	}
}

func TestSequenceFailedCascadeReportsAFailedRetireToStderr(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	ids := w.retriedGroupStep()
	w.blockSave(ids[1])
	var stderr strings.Builder
	defer func(old io.Writer) { seqStderr = old }(seqStderr)
	seqStderr = &stderr
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		seq := sf.Sequences[0]
		sf.failed(w.bridge, seq, &seqStep{Kind: "do", N: 9, Seq: seq.N})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); !strings.HasPrefix(got, "ensign: ") || !strings.Contains(got, ids[1]) || !strings.Contains(got, "could not be retired") || strings.Count(got, "\n") != 1 {
		t.Errorf("stderr should name the run on one line: %q", got)
	}
	st := w.step(1)
	if st.Status != "cancelled" || !strings.Contains(st.Reason, ids[1]) {
		t.Errorf("the failure should also be in the reason: %s %q", st.Status, st.Reason)
	}
}

func TestSequenceStartFailureNamesARunThatCouldNotBeRetired(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	id := w.step(1).Run
	w.blockSave(id)
	writeFile(sessionFile(w.bridge, w.sid)+".pin", "not-the-pinned-commit\n")
	ctx := w.goContext()
	if !strings.Contains(ctx, "could not be retired") || !strings.Contains(ctx, id) {
		t.Errorf("the outcome should name the retire failure: %s", ctx)
	}
	if st := w.step(1); st.Status != "failed" || !strings.Contains(st.Reason, "could not be retired") || strings.Contains(st.Reason, "\n") {
		t.Errorf("the step's reason should carry it on one line: %s %q", st.Status, st.Reason)
	}
}

func TestSequenceStartFailureNamesEveryRunThatCouldNotBeRetired(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	ids := w.stepRuns(1)
	w.blockSave(ids[1])
	writeFile(sessionFile(w.bridge, w.sid)+".pin", "not-the-pinned-commit\n")
	var stderr strings.Builder
	defer func(old io.Writer) { seqStderr = old }(seqStderr)
	seqStderr = &stderr
	w.goContext()
	if got := stderr.String(); !strings.Contains(got, "ensign: run "+ids[1]+" could not be retired: ") {
		t.Errorf("stderr should name the second run: %q", got)
	}
	if st := w.runState(ids[0]); st.Status != "error" {
		t.Errorf("the first run should be retired: %s", st.Status)
	}
}

func TestSequenceAddDoesNotWaitForARunInTheSequence(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	var err error
	w.whileRunLockHeld(w.step(1).Run, func() { _, err = w.seq("do", "tell the user") })
	if err != nil {
		t.Fatal(err)
	}
}

func TestCarrySequenceDoesNotHoldTheSequenceLockWhileWaitingForARun(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	// Step 1 is deciding, so carrySequence resets its Blocks under the lock: readSeq sees that only after the closure has run.
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		st := sf.Sequences[0].Steps[0]
		st.Status, st.Blocks = "decide", 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	id := w.step(2).Run
	if st := w.runState(id); st.Session != w.sid {
		t.Fatalf("the queued run's session is %s, want %s", st.Session, w.sid)
	}
	l := flock.New(filepath.Join(runsDir(w.bridge), id, "lock"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	// Stat, never open: an open file would fail writeJSON's rename on Windows. Resetting Blocks (omitempty) shrinks the file.
	info, err := os.Stat(seqPath(w.bridge, w.sid))
	if err != nil {
		t.Fatal(err)
	}
	before := info.Size()
	carried := make(chan error, 1)
	go func() { carried <- carrySequence(w.bridge, w.sid, "s2") }()
	reset := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if info, err := os.Stat(seqPath(w.bridge, "s2")); err == nil && info.Size() != before {
			reset = true
			break
		}
	}
	if !reset {
		l.Unlock()
		<-carried
		t.Fatal("carrySequence never saved the sequence before waiting for the run")
	}
	taken := make(chan struct{})
	go func() {
		withSeq(w.bridge, "s2", func(sf *seqFile) error { return nil })
		close(taken)
	}()
	free := false
	select {
	case <-taken:
		free = true
	case <-time.After(10 * time.Second):
	}
	l.Unlock()
	<-taken
	if err := <-carried; err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("carrySequence kept the sequence lock while waiting for a run lock")
	}
	if st := w.runState(id); st.Session != "s2" {
		t.Errorf("the run's session is %s, want s2", st.Session)
	}
}

func TestSequenceHaltSaysSoWhenAPendingRunCouldNotBeRetired(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	w.spawn("", "", "orchestrator")
	pending := w.step(2).Run
	w.blockSave(pending)
	_, err := w.seq("halt", "--reason", "stop")
	if err == nil || !strings.Contains(err.Error(), pending) || !strings.Contains(err.Error(), "left pending") || strings.Contains(err.Error(), "halt again") {
		t.Fatalf("halt error: %v", err)
	}
	if !readSeq(w.bridge, w.sid).Sequences[0].Halted {
		t.Error("the sequence should still be saved halted")
	}
	w.wantStatus("active", "cancelled")
}

func TestSequenceHaltKeepsTheSuffixForTheRunsItIsTrueFor(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	active, pending := w.step(1).Run, w.step(2).Run
	w.blockSave(active)
	w.blockSave(pending)
	_, err := w.seq("halt", "--reason", "stop")
	if err == nil {
		t.Fatal("halt should fail")
	}
	msg := err.Error()
	i := strings.Index(msg, "halt again")
	if i < 0 || !strings.Contains(msg[:i], active) || strings.Contains(msg[:i], pending) || !strings.Contains(msg[i:], pending) {
		t.Errorf("the suffix should follow the active run and precede the pending one:\n%s", msg)
	}
}

func TestSequenceDropAndDiscardSaySoWhenARunCouldNotBeRetired(t *testing.T) {
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	second := w.step(2).Run
	w.blockSave(second)
	out, err := w.seq("drop", "2")
	if err == nil || !strings.Contains(err.Error(), second) || out == "" {
		t.Fatalf("drop: %q %v", out, err)
	}
	if n := len(readSeq(w.bridge, w.sid).Sequences[0].Steps); n != 1 {
		t.Errorf("the drop should still be saved, %d steps left", n)
	}
	first := w.step(1).Run
	w.blockSave(first)
	out, err = w.seq("discard")
	if err == nil || !strings.Contains(err.Error(), first) || !strings.Contains(out, "discarded") {
		t.Fatalf("discard: %q %v", out, err)
	}
	if n := len(readSeq(w.bridge, w.sid).Sequences); n != 0 {
		t.Errorf("the discard should still be saved, %d sequences left", n)
	}
}

// writeTaskFile replaces the task's task.md with body under its front matter.
func (w *world) writeTaskFile(task, body string) {
	w.t.Helper()
	write(w.t, filepath.Join(w.bridge, "projects/supernova/tasks", task, "task.md"), "---\npipeline: coding\n---\n\n"+body)
}

// stepWorkItem returns the work item of step n's run.
func (w *world) stepWorkItem(n int) string {
	w.t.Helper()
	r, err := readRun(w.bridge, w.step(n).Run)
	if err != nil {
		w.t.Fatal(err)
	}
	return r.Snap.WorkItem
}

func TestSequenceRetryTakesTheTasksCurrentGoalAndScope(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("auth-refactor", "## Goal\n\nNew goal text.\n\n## Scope\n\nNew scope text.\n")
	w.retryStep("1")
	wi := w.stepWorkItem(1)
	if !strings.Contains(wi, "New goal text.") || !strings.Contains(wi, "New scope text.") || strings.Contains(wi, "Add a multiply function") {
		t.Errorf("the retry should take the edited task:\n%s", wi)
	}
	if strings.Count(wi, "## Current state") != 1 || strings.Index(wi, "## Current state") < strings.Index(wi, "New scope text.") {
		t.Errorf("one current state, after the task text:\n%s", wi)
	}
}

func TestSequenceSecondRetryTakesTheNewestTextAndOnlyTheLatestState(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("auth-refactor", "## Goal\n\nSecond goal.\n")
	w.retryStep("1")
	lead, _ = w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("auth-refactor", "## Goal\n\nThird goal.\n")
	w.mustSeq("retry", "1", "--diagnosis", "the second diagnosis", "--state", "the second leftover")
	wi := w.stepWorkItem(1)
	if !strings.Contains(wi, "Third goal.") || strings.Contains(wi, "Second goal.") || strings.Contains(wi, "Add a multiply function") {
		t.Errorf("the second retry should take the newest text:\n%s", wi)
	}
	if strings.Count(wi, "## Current state") != 1 || !strings.Contains(wi, "the second diagnosis") || strings.Contains(wi, "the fixture path was wrong") {
		t.Errorf("only the latest state should be there:\n%s", wi)
	}
}

func TestSequenceRetryOfATaskWithoutGoalOrScopeIsRefused(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("auth-refactor", "Just some notes.\n")
	if _, err := w.seq("retry", "1", "--diagnosis", "why", "--state", "left"); err == nil || !strings.Contains(err.Error(), "no work item") {
		t.Fatalf("the retry should be refused: %v", err)
	}
	w.wantStatus("decide")
	w.writeTaskFile("auth-refactor", "## Goal\n\nFixed goal.\n")
	w.retryStep("1")
	w.wantStatus("active")
}

func TestSequenceRetryLeavesALaterUnstartedStepsWorkItemAlone(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	before := w.stepWorkItem(2)
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("auth-refactor", "## Goal\n\nEdited goal.\n")
	w.retryStep("1")
	after := w.stepWorkItem(2)
	if after != before || !strings.Contains(after, "Add a multiply function") || strings.Contains(after, "Edited goal.") {
		t.Errorf("step 2 changed:\nbefore: %s\nafter: %s", before, after)
	}
}

func TestSequencePositionCountsOnlyStepsThatRun(t *testing.T) {
	step := func(status string) *seqStep { return &seqStep{Status: status} }
	seq := func(approved, closed bool, steps ...*seqStep) *sequence {
		return &sequence{Approved: approved, Closed: closed, Steps: steps}
	}

	// A cancelled step before the current one lowers X and Y; a cancelled later step lowers Y only.
	a, cur, c, d := step("cancelled"), step("active"), step("cancelled"), step("pending")
	sf := &seqFile{Sequences: []*sequence{seq(true, false, a, cur, c, d)}}
	if got := sf.position(cur); got != "Step 1/2" {
		t.Errorf("cancelled steps should not count, got %s", got)
	}
	if got := sf.position(d); got != "Step 2/2" {
		t.Errorf("got %s", got)
	}

	// A plain skipped status is not counted; a failed step that the user skipped is.
	sk, ran := step("skipped"), &seqStep{Status: "failed", Reason: skipPrefix + "x"}
	cur = step("decide")
	sf = &seqFile{Sequences: []*sequence{seq(true, false, sk, ran, cur)}}
	if got := sf.position(cur); got != "Step 2/2" {
		t.Errorf("skipped status should not count but a failed skipped step should, got %s", got)
	}

	// Y spans queued approved sequences and X continues into the second after the first closes; an unapproved sequence adds nothing.
	f1, f2, s1, s2, u := step("done"), step("done"), step("active"), step("pending"), step("pending")
	sf = &seqFile{Sequences: []*sequence{seq(true, true, f1, f2), seq(true, false, s1, s2), seq(false, false, u)}}
	if got := sf.position(s1); got != "Step 3/4" {
		t.Errorf("got %s", got)
	}
	sf.Sequences[0].Closed = false
	if got := sf.position(f2); got != "Step 2/4" {
		t.Errorf("got %s", got)
	}

	// A marked sequence after a closed one starts a new queue.
	n1, n2 := step("pending"), step("pending")
	sf = &seqFile{Sequences: []*sequence{seq(true, true, f1, f2, s1), seq(true, false, n1, n2)}}
	sf.Sequences[1].QueueStart = true
	if got := sf.position(n1); got != "Step 1/2" {
		t.Errorf("a new queue should restart the count, got %s", got)
	}

	// A queue's Y leaves out a later queue, and a closed earlier queue's steps are not counted.
	sf.Sequences[0].QueueStart = true
	if got := sf.position(s1); got != "Step 3/3" {
		t.Errorf("Y should leave out the later queue, got %s", got)
	}
	if got := sf.position(n2); got != "Step 2/2" {
		t.Errorf("got %s", got)
	}

	// Without marks everything counts from the first approved sequence.
	sf.Sequences[0].QueueStart, sf.Sequences[1].QueueStart = false, false
	if got := sf.position(n2); got != "Step 5/5" {
		t.Errorf("an unmarked file should count from its first sequence, got %s", got)
	}
}

func TestSequenceQueueStartFollowsWhetherAnEarlierSequenceIsOpen(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	w.mustSeq("do", "second supernova chore")
	w.approve()
	w.mustSeq("do", "third supernova chore")
	w.mustSeq("do", "fourth supernova chore")
	ctx := w.goContext()
	if !strings.Contains(ctx, "Step 1/4") {
		t.Errorf("a sequence queued behind a held step should count on with it:\n%s", ctx)
	}
	withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		if !sf.Sequences[0].QueueStart || sf.Sequences[1].QueueStart {
			t.Errorf("only the first sequence should start the queue: %+v %+v", sf.Sequences[0], sf.Sequences[1])
		}
		return nil
	})

	w.mustSeq("done", "1.1")
	w.mustSeq("done", "1.2")
	w.mustSeq("done", "2.1")
	w.mustSeq("done", "2.2")
	w.mustSeq("do", "fifth supernova chore")
	w.mustSeq("do", "sixth supernova chore")
	ctx = w.goContext()
	if !strings.Contains(ctx, "Step 1/2") {
		t.Errorf("a sequence approved after the others closed should restart the count:\n%s", ctx)
	}
}

func TestSequenceDoneOnADoStepNamesTheNextRunStep(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "first supernova chore")
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	out := w.mustSeq("done", "1.1")
	if n := strings.Count(out, "Now at step 1.2 (Step 2/2)."); n != 1 {
		t.Errorf("done should name the step it started once, got %d:\n%s", n, out)
	}

	w2 := newWorld(t)
	w2.mustSeq("do", "only supernova chore")
	w2.approve()
	if out := w2.mustSeq("done", "1.1"); strings.Contains(out, "Now at") {
		t.Errorf("done on the last step should not name a step:\n%s", out)
	}
}

func TestSequenceRetryListingShowsTheNewGoal(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("auth-refactor", "## Goal\n\nBrand new goal.\n")
	w.retryStep("1")
	got := sequenceListing(w.bridge, w.sid)
	if !strings.Contains(got, "Brand new goal.") || strings.Contains(got, "Add a multiply function") {
		t.Errorf("the listing should show the retry's goal:\n%s", got)
	}
}

// strandedWorld halts a two-step sequence whose pending second run could not be retired, so that step 2 is cancelled with the run still pending. It returns that run.
func strandedWorld(t *testing.T) (*world, string) {
	t.Helper()
	w := newWorld(t)
	w.addTask("other")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.approve()
	w.spawn("", "", "orchestrator")
	pending := w.step(2).Run
	w.blockSave(pending)
	if _, err := w.seq("halt", "--reason", "stop"); err == nil {
		t.Fatal("halt should fail")
	}
	return w, pending
}

func (w *world) unblockSave(run string) {
	w.t.Helper()
	if err := os.Remove(filepath.Join(runsDir(w.bridge), run, "state.json.tmp")); err != nil {
		w.t.Fatal(err)
	}
}

func TestSequenceHaltNotesAFailedRetireOnTheStepAndRetriesItAtTheNextAdvance(t *testing.T) {
	w, pending := strandedWorld(t)
	st := w.step(2)
	if st.Status != "cancelled" || !strings.Contains(st.RetireNote, pending) || !strings.HasSuffix(st.Reason, "; "+st.RetireNote) {
		t.Fatalf("the step should carry the failure: %s %q %q", st.Status, st.Reason, st.RetireNote)
	}
	if out, _ := w.seq("--all"); !strings.Contains(out, "could not be retired") {
		t.Errorf("the listing should carry the failure:\n%s", out)
	}
	w.unblockSave(pending)
	takeSequenceReports(w.bridge, w.sid)
	if _, err := advanceSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if got := w.runState(pending); got.Status != "error" || got.Reason != "cancelled: stop" {
		t.Errorf("the run should be retired on retry: %s %q", got.Status, got.Reason)
	}
	if st := w.step(2); st.RetireNote != "" || st.Reason != "stop" {
		t.Errorf("the note should be cleared: %q %q", st.RetireNote, st.Reason)
	}
	if got := takeSequenceReports(w.bridge, w.sid); strings.Count(got, "was retired on retry") != 1 || !strings.Contains(got, pending) {
		t.Errorf("the retry should be reported once: %q", got)
	}
	if _, err := advanceSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if got := takeSequenceReports(w.bridge, w.sid); got != "" {
		t.Errorf("nothing more to report: %q", got)
	}
}

func TestSequenceRetryThatStillFailsIsReportedAtThePromptAndTheReasonDoesNotGrow(t *testing.T) {
	w, pending := strandedWorld(t)
	takeSequenceReports(w.bridge, w.sid)
	before := w.step(2).Reason
	for i := 0; i < 2; i++ {
		if _, err := advanceSequence(w.bridge, w.sid); err != nil {
			t.Fatal(err)
		}
		res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "hello"})
		if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, pending) || !strings.Contains(ctx, "could not be retired") {
			t.Errorf("prompt %d should report the failure: %v", i+1, res)
		}
	}
	if st := w.step(2); st.Reason != before || strings.Count(st.Reason, "could not be retired") != 1 {
		t.Errorf("the reason should not grow: %q, was %q", st.Reason, before)
	}
}

func TestSequenceStopDoesNotQueueARetireThatKeepsFailing(t *testing.T) {
	w, _ := strandedWorld(t)
	takeSequenceReports(w.bridge, w.sid)
	for i := 0; i < 2; i++ {
		if res := w.stopHook(); res["decision"] != nil {
			t.Fatalf("stop %d should not block: %v", i+1, res)
		}
	}
	if got := takeSequenceReports(w.bridge, w.sid); got != "" {
		t.Errorf("a stop should queue nothing for a retire that still fails: %q", got)
	}
	if w.step(2).RetireNote == "" {
		t.Error("the note should stay")
	}
}

func TestSequenceStopReportsARetireThatSucceeds(t *testing.T) {
	w, pending := strandedWorld(t)
	takeSequenceReports(w.bridge, w.sid)
	w.unblockSave(pending)
	if reason := dig(w.stopHook(), "reason"); !strings.Contains(reason, "was retired on retry") {
		t.Errorf("the stop should report it: %q", reason)
	}
	if res := w.stopHook(); res["decision"] != nil {
		t.Errorf("the second stop should not block: %v", res)
	}
}

func TestSequenceRetryLeavesARunThatIsNotPendingOnDisk(t *testing.T) {
	w, pending := strandedWorld(t)
	w.unblockSave(pending)
	r, err := openRun(w.bridge, pending)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Approved = "running", true
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	if _, err := advanceSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if got := w.runState(pending); got.Status != "running" {
		t.Errorf("an approved run is not retired: %s", got.Status)
	}
	if st := w.step(2); st.RetireNote != "" || strings.Contains(st.Reason, "could not be retired") {
		t.Errorf("the note should be cleared: %q / %q", st.RetireNote, st.Reason)
	}
	report := ""
	for _, seq := range readSeq(w.bridge, w.sid).Sequences {
		report += seq.Report + "\n"
	}
	if n := strings.Count(report, "no longer stranded"); n != 1 {
		t.Errorf("the end of the failure should be reported once, got %d:\n%s", n, report)
	}
	if _, err := advanceSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	report = ""
	for _, seq := range readSeq(w.bridge, w.sid).Sequences {
		report += seq.Report + "\n"
	}
	if n := strings.Count(report, "no longer stranded"); n != 1 {
		t.Errorf("a step with no note stays silent, got %d:\n%s", n, report)
	}
}

func TestSessionStartRetriesAStrandedRunAndReportsAFailure(t *testing.T) {
	w, pending := strandedWorld(t)
	res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); code != 0 || !strings.Contains(ctx, pending) || !strings.Contains(ctx, "could not be retired") {
		t.Fatalf("the start should report the failure: %d %v", code, res)
	}
	w.unblockSave(pending)
	res, _ = w.hook("SessionStart", map[string]any{"source": "startup"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, "was retired on retry") {
		t.Errorf("the start should report the retry: %v", res)
	}
	if got := w.runState(pending); got.Status != "error" {
		t.Errorf("the run should be retired: %s", got.Status)
	}
}

func TestSessionStartWithNothingStrandedDoesNotTakeTheSequenceLock(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	l := flock.New(seqPath(w.bridge, w.sid) + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	done := make(chan string, 1)
	go func() { done <- retryStrandedRetires(w.bridge, w.sid) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("nothing to say: %q", got)
		}
	case <-time.After(10 * time.Second):
		l.Unlock()
		<-done
		t.Fatal("the start waited for the sequence lock")
	}
}

func TestSequenceCascadeRetireFailureAndAdvanceFailureReachTheNextPrompt(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	ids := w.retriedGroupStep()
	w.blockSave(ids[1])
	defer func(old io.Writer) { seqStderr = old }(seqStderr)
	seqStderr = io.Discard
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		seq := sf.Sequences[0]
		sf.failed(w.bridge, seq, &seqStep{Kind: "do", N: 9, Seq: seq.N})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "hello"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, ids[1]) || !strings.Contains(ctx, "could not be retired") {
		t.Errorf("the cascade failure should reach the prompt: %v", res)
	}
	if err := queueSequenceFailure(w.bridge, w.sid, ids[0], "could not advance the sequence: boom"); err != nil {
		t.Fatal(err)
	}
	res, _ = w.hook("UserPromptSubmit", map[string]any{"prompt": "hello"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, "could not advance the sequence: boom") {
		t.Errorf("the advance failure should reach the prompt: %v", res)
	}
}

func TestFinishIfTerminalWarnsWhenTheAdvanceFailureCannotBeQueued(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	r, err := openRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	(&machine{snap: r.Snap, st: r.State}).fail("handler", "boom")
	if err := os.Mkdir(seqPath(w.bridge, w.sid)+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := finishIfTerminal(r, false, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "could not advance the sequence") || r.warn == nil || !strings.Contains(r.warn.Error(), "could not advance the sequence") {
		t.Errorf("stderr %q, warn %v", stderr.String(), r.warn)
	}
}

func TestFinishIfTerminalDoesNotAdvanceAFinishedRunAgain(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	r, err := openRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	(&machine{snap: r.Snap, st: r.State}).fail("handler", "boom")
	if err := os.Mkdir(seqPath(w.bridge, w.sid)+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := finishIfTerminal(r, false, &stderr); err != nil || !strings.Contains(stderr.String(), "could not advance the sequence") {
		t.Fatalf("the first call should advance: %q %v", stderr.String(), err)
	}
	stderr.Reset()
	r.warn = nil
	if err := finishIfTerminal(r, false, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() > 0 || r.warn != nil {
		t.Errorf("the second call should not advance: %q %v", stderr.String(), r.warn)
	}
	// Resume clears Finished, so a resumed run that finishes again advances.
	r.State.Finished = false
	if err := finishIfTerminal(r, false, &stderr); err != nil || !strings.Contains(stderr.String(), "could not advance the sequence") {
		t.Errorf("a run finishing again after a resume should advance: %q %v", stderr.String(), err)
	}
}

// carryWorld has three queued runs; the second and third are not in the old session's list, as when an earlier carry left them to carrySequence. It returns their ids.
func carryWorld(t *testing.T) (*world, []string) {
	t.Helper()
	w := newWorld(t)
	w.addTask("other")
	w.addTask("third")
	w.mustSeq("run", "supernova/auth-refactor")
	w.mustSeq("run", "supernova/other")
	w.mustSeq("run", "supernova/third")
	w.approve()
	ids := []string{w.step(1).Run, w.step(2).Run, w.step(3).Run}
	if err := editSessionRuns(w.bridge, w.sid, func(all []string) []string {
		return slices.DeleteFunc(all, func(id string) bool { return id != ids[0] })
	}); err != nil {
		t.Fatal(err)
	}
	return w, ids
}

// Windows rename and permission behaviour differs, so these tests fail the rename and the save by seam and by a directory in the way, which behave alike on every platform.
func TestCarrySequenceQueuesARunItCouldNotRebindAndCarriesTheRest(t *testing.T) {
	w, ids := carryWorld(t)
	w.blockSave(ids[1])
	if err := carrySequence(w.bridge, w.sid, "s2"); err != nil {
		t.Fatalf("a queued failure is not also returned: %v", err)
	}
	if !seqExists(w.bridge, "s2") {
		t.Error("the sequence should have moved")
	}
	var report string
	for _, seq := range readSeq(w.bridge, "s2").Sequences {
		report += seq.Report
	}
	if !strings.Contains(report, ids[1]) || strings.Contains(report, ids[2]) || !strings.Contains(report, "stays on the earlier session") || !strings.Contains(report, "does not retry") {
		t.Errorf("the new session's report should name the second run only and say it is not retried:\n%s", report)
	}
	if got := w.runState(ids[2]).Session; got != "s2" {
		t.Errorf("the third run should be carried: %s", got)
	}
	if got := w.runState(ids[1]).Session; got != "s1" {
		t.Errorf("the second run stays on the old session: %s", got)
	}
}

func TestCarryOverReturnsASaveFailureWhenTheSessionHasNoSequenceAndRebindsTheRest(t *testing.T) {
	w, ids := carryWorld(t)
	if err := editSessionRuns(w.bridge, w.sid, func([]string) []string { return slices.Clone(ids) }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(seqPath(w.bridge, w.sid)); err != nil {
		t.Fatal(err)
	}
	w.blockSave(ids[0])
	in := &HookInput{Source: "clear", SessionID: "s2"}
	err := carryOverSession(w.bridge, in, "s1")
	if err == nil || !strings.Contains(err.Error(), ids[0]) || strings.Contains(err.Error(), ids[1]) || strings.Contains(err.Error(), ids[2]) {
		t.Fatalf("the error should name the first run only: %v", err)
	}
	if !strings.Contains(err.Error(), "can move it") || strings.Contains(err.Error(), "does not retry") {
		t.Errorf("a failed save is listed, so an agent hook can move it: %v", err)
	}
	for _, id := range ids[1:] {
		if got := w.runState(id).Session; got != "s2" {
			t.Errorf("run %s should be rebound: %s", id, got)
		}
	}
	if got := w.runState(ids[0]).Session; got != "s1" {
		t.Errorf("the run whose save failed stays on the old session: %s", got)
	}
}

func TestCarryOverReturnsASaveFailureWhenTheSessionHasAnEmptySequenceFile(t *testing.T) {
	w, ids := carryWorld(t)
	if err := editSessionRuns(w.bridge, w.sid, func([]string) []string { return slices.Clone(ids) }); err != nil {
		t.Fatal(err)
	}
	write(t, seqPath(w.bridge, w.sid), `{"sequences":[]}`)
	w.blockSave(ids[0])
	in := &HookInput{Source: "clear", SessionID: "s2"}
	err := carryOverSession(w.bridge, in, "s1")
	if err == nil || !strings.Contains(err.Error(), ids[0]) || strings.Contains(err.Error(), ids[1]) || strings.Contains(err.Error(), errNoSequence.Error()) {
		t.Fatalf("the error should name the first run only: %v", err)
	}
	if !strings.Contains(err.Error(), "can move it") || strings.Contains(err.Error(), "does not retry") {
		t.Errorf("a failed save is listed, so an agent hook can move it: %v", err)
	}
	if !seqExists(w.bridge, "s2") || len(readSeq(w.bridge, "s2").Sequences) != 0 {
		t.Error("the empty sequence file should have moved unchanged")
	}
	if got := w.runState(ids[1]).Session; got != "s2" {
		t.Errorf("run %s should be rebound: %s", ids[1], got)
	}
}

func (w *world) corruptRun(id string) {
	w.t.Helper()
	write(w.t, filepath.Join(runsDir(w.bridge), id, "state.json"), "{not json")
}

func (w *world) carryAllFromS1(ids []string) (*HookInput, error) {
	w.t.Helper()
	if err := editSessionRuns(w.bridge, w.sid, func([]string) []string { return slices.Clone(ids) }); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Remove(seqPath(w.bridge, w.sid)); err != nil {
		w.t.Fatal(err)
	}
	return &HookInput{Source: "clear", SessionID: "s2"}, nil
}

func TestCarryOverReportsAnUnreadableRunAndCarriesTheRest(t *testing.T) {
	w, ids := carryWorld(t)
	in, _ := w.carryAllFromS1(ids)
	w.corruptRun(ids[1])
	err := carryOverSession(w.bridge, in, "s1")
	if err == nil || !strings.Contains(err.Error(), ids[1]) || strings.Contains(err.Error(), ids[2]) || !strings.Contains(err.Error(), "nothing is sure to") || strings.Contains(err.Error(), "does not retry") {
		t.Fatalf("the error should name the unreadable run only, as one that may still move: %v", err)
	}
	for _, id := range []string{ids[0], ids[2]} {
		if got := w.runState(id).Session; got != "s2" {
			t.Errorf("run %s should be carried: %s", id, got)
		}
	}
}

func TestCarryOverReportsAnUnreadableBoundRunOnceAsMayMove(t *testing.T) {
	w, ids := carryWorld(t)
	if err := editSessionRuns(w.bridge, w.sid, func([]string) []string { return []string{ids[1]} }); err != nil {
		t.Fatal(err)
	}
	w.corruptRun(ids[1])
	in := &HookInput{Source: "clear", SessionID: "s2"}
	if err := carryOverSession(w.bridge, in, "s1"); err != nil {
		t.Fatalf("the queued report is not also returned: %v", err)
	}
	var report string
	for _, seq := range readSeq(w.bridge, "s2").Sequences {
		report += seq.Report
	}
	if n := strings.Count(report, "could not be moved to this session"); n != 1 || !strings.Contains(report, ids[1]) || !strings.Contains(report, "nothing is sure to") {
		t.Errorf("the unreadable run should be reported once, as may-move, got %d:\n%s", n, report)
	}
}

func TestCarryOverSkipsARunWhoseFolderIsGone(t *testing.T) {
	w, ids := carryWorld(t)
	in, _ := w.carryAllFromS1(ids)
	if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), ids[1])); err != nil {
		t.Fatal(err)
	}
	if err := carryOverSession(w.bridge, in, "s1"); err != nil {
		t.Fatalf("a run that no longer exists is not a failed move: %v", err)
	}
	if got := w.runState(ids[2]).Session; got != "s2" {
		t.Errorf("run %s should be carried: %s", ids[2], got)
	}
}

func TestCarryOverReportsAFailedBindAndCarriesTheRest(t *testing.T) {
	w, ids := carryWorld(t)
	in, _ := w.carryAllFromS1(ids)
	old := carryBind
	t.Cleanup(func() { carryBind = old })
	carryBind = func(bridge, session, run string) error {
		if run == ids[1] {
			return errors.New("session file busy")
		}
		return old(bridge, session, run)
	}
	err := carryOverSession(w.bridge, in, "s1")
	if err == nil || !strings.Contains(err.Error(), ids[1]) || strings.Contains(err.Error(), ids[2]) || !strings.Contains(err.Error(), "session file busy") || !strings.Contains(err.Error(), "nothing is sure to") || strings.Contains(err.Error(), "does not retry") {
		t.Fatalf("the error should name the unbound run only, as one that may still move: %v", err)
	}
	if got := w.runState(ids[1]).Session; got != "s1" {
		t.Errorf("the unbound run stays on the old session: %s", got)
	}
	for _, id := range []string{ids[0], ids[2]} {
		if got := w.runState(id).Session; got != "s2" {
			t.Errorf("run %s should be carried: %s", id, got)
		}
	}
}

func TestCarrySequenceReportsAnUnreadableQueuedRunAndCarriesTheRest(t *testing.T) {
	w, ids := carryWorld(t)
	w.corruptRun(ids[1])
	if err := carrySequence(w.bridge, w.sid, "s2"); err != nil {
		t.Fatalf("a queued failure is not also returned: %v", err)
	}
	var report string
	for _, seq := range readSeq(w.bridge, "s2").Sequences {
		report += seq.Report
	}
	if !strings.Contains(report, ids[1]) || strings.Contains(report, ids[2]) || !strings.Contains(report, "does not retry") {
		t.Errorf("the report should name the unreadable run only:\n%s", report)
	}
	if got := w.runState(ids[2]).Session; got != "s2" {
		t.Errorf("the third run should be carried: %s", got)
	}
}

func TestCarrySequenceSkipsAQueuedRunWhoseFolderIsGone(t *testing.T) {
	w, ids := carryWorld(t)
	if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), ids[1])); err != nil {
		t.Fatal(err)
	}
	if err := carrySequence(w.bridge, w.sid, "s2"); err != nil {
		t.Fatal(err)
	}
	for _, seq := range readSeq(w.bridge, "s2").Sequences {
		if strings.Contains(seq.Report, ids[1]) {
			t.Errorf("a run that no longer exists is not reported:\n%s", seq.Report)
		}
	}
	if got := w.runState(ids[2]).Session; got != "s2" {
		t.Errorf("the third run should be carried: %s", got)
	}
}

func TestCarrySequenceMovesTheSequenceUnderTheOldSessionLock(t *testing.T) {
	w, _ := carryWorld(t)
	old := carryLockWait
	t.Cleanup(func() { carryLockWait = old })
	carryLockWait = 30 * time.Second
	carried := make(chan error, 1)
	err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		go func() { carried <- carrySequence(w.bridge, w.sid, "s2") }()
		select {
		case err := <-carried:
			carried <- err
		case <-time.After(500 * time.Millisecond):
		}
		sf.Sequences[0].Report = "holder's update"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-carried:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("carrySequence did not finish after the lock was released")
	}
	if seqExists(w.bridge, w.sid) {
		t.Error("the old session should have no sequence file: the holder's save recreated it")
	}
	if got := readSeq(w.bridge, "s2").Sequences[0].Report; got != "holder's update" {
		t.Errorf("the new session's sequence should hold the holder's change: %q", got)
	}
}

func TestCarrySequenceLeavesTheSequenceWhenTheOldSessionLockIsHeldPastTheBound(t *testing.T) {
	w, ids := carryWorld(t)
	old := carryLockWait
	t.Cleanup(func() { carryLockWait = old })
	carryLockWait = 200 * time.Millisecond
	l := flock.New(seqPath(w.bridge, w.sid) + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	err := carrySequence(w.bridge, w.sid, "s2")
	if err == nil || !strings.Contains(err.Error(), "stays on the earlier session") || !strings.Contains(err.Error(), "the old session's sequence lock is held") {
		t.Fatalf("the held lock should be returned: %v", err)
	}
	if !seqExists(w.bridge, w.sid) || seqExists(w.bridge, "s2") {
		t.Error("the sequence should stay on the old session")
	}
	for _, id := range ids[1:] {
		if got := w.runState(id).Session; got != w.sid {
			t.Errorf("run %s should not be rebound: %s", id, got)
		}
	}
}

func TestCarrySequenceDoesNothingWhenTheNewSessionGainsASequenceDuringTheWait(t *testing.T) {
	w, _ := carryWorld(t)
	old := carryLockWait
	t.Cleanup(func() { carryLockWait = old })
	carryLockWait = 30 * time.Second
	l := flock.New(seqPath(w.bridge, w.sid) + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	before, err := os.ReadFile(seqPath(w.bridge, w.sid))
	if err != nil {
		t.Fatal(err)
	}
	carried := make(chan error, 1)
	go func() { carried <- carrySequence(w.bridge, w.sid, "s2") }()
	time.Sleep(200 * time.Millisecond)
	write(t, seqPath(w.bridge, "s2"), `{"sequences":[]}`)
	l.Unlock()
	select {
	case err := <-carried:
		if err == nil || !strings.Contains(err.Error(), "not driven from this one") {
			t.Fatalf("the sequence left behind should be reported: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("carrySequence did not finish after the lock was released")
	}
	after, err := os.ReadFile(seqPath(w.bridge, w.sid))
	if err != nil || string(after) != string(before) {
		t.Errorf("the old session's sequence should be unchanged: %v", err)
	}
	if got, err := os.ReadFile(seqPath(w.bridge, "s2")); err != nil || string(got) != `{"sequences":[]}` {
		t.Errorf("the new session's sequence should be unchanged: %q %v", got, err)
	}
}

func failRename(t *testing.T) {
	t.Helper()
	old := seqRename
	t.Cleanup(func() { seqRename = old })
	seqRename = func(string, string) error { return errors.New("sharing violation") }
}

func TestCarrySequenceReturnsAFailedRenameAndLeavesTheSequenceOnTheOldSession(t *testing.T) {
	w, _ := carryWorld(t)
	failRename(t)
	err := carrySequence(w.bridge, w.sid, "s2")
	if err == nil || !strings.Contains(err.Error(), "sharing violation") || !strings.Contains(err.Error(), w.sid) || !strings.Contains(err.Error(), "s2") {
		t.Fatalf("the failed move should be returned: %v", err)
	}
	if !seqExists(w.bridge, w.sid) || seqExists(w.bridge, "s2") {
		t.Error("the sequence should stay on the old session")
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s2") + ".sequence.carry"); err == nil {
		t.Error("nothing should be recorded for a retry")
	}
	entries, _ := filepath.Glob(sessionFile(w.bridge, "s2") + "*")
	entries = slices.DeleteFunc(entries, func(e string) bool { return e == seqPath(w.bridge, "s2")+".lock" })
	if len(entries) != 0 {
		t.Errorf("no file but the lock should exist for the new session: %v", entries)
	}
}

func TestCarrySequenceHoldsTheNewSessionLockWhileItRenames(t *testing.T) {
	w, _ := carryWorld(t)
	real := seqRename
	t.Cleanup(func() { seqRename = real })
	var tried error
	seqRename = func(from, to string) error {
		l := flock.New(seqPath(w.bridge, "s2") + ".lock")
		locked, err := l.TryLock()
		if locked {
			l.Unlock()
			tried = errors.New("the new session's lock was free during the rename")
		} else if err != nil {
			tried = err
		}
		return real(from, to)
	}
	if err := carrySequence(w.bridge, w.sid, "s2"); err != nil {
		t.Fatal(err)
	}
	if tried != nil {
		t.Error(tried)
	}
	if !seqExists(w.bridge, "s2") || seqExists(w.bridge, w.sid) {
		t.Error("the sequence should have moved")
	}
}

func TestCarrySequenceLeavesTheSequenceWhenTheNewSessionLockIsHeldPastTheBound(t *testing.T) {
	w, ids := carryWorld(t)
	old := carryLockWait
	t.Cleanup(func() { carryLockWait = old })
	carryLockWait = 200 * time.Millisecond
	l := flock.New(seqPath(w.bridge, "s2") + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	err := carrySequence(w.bridge, w.sid, "s2")
	if err == nil || !strings.Contains(err.Error(), "stays on the earlier session") || !strings.Contains(err.Error(), "the new session's sequence lock is held") {
		t.Fatalf("the held lock should be returned: %v", err)
	}
	if !seqExists(w.bridge, w.sid) || seqExists(w.bridge, "s2") {
		t.Error("the sequence should stay on the old session")
	}
	for _, id := range ids[1:] {
		if got := w.runState(id).Session; got != w.sid {
			t.Errorf("run %s should not be rebound: %s", id, got)
		}
	}
}

func TestCarrySequenceDoesNotOverwriteASequenceTheNewSessionGainsWhileItsLockIsHeld(t *testing.T) {
	w, _ := carryWorld(t)
	old := carryLockWait
	t.Cleanup(func() { carryLockWait = old })
	carryLockWait = 30 * time.Second
	l := flock.New(seqPath(w.bridge, "s2") + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	before, err := os.ReadFile(seqPath(w.bridge, w.sid))
	if err != nil {
		t.Fatal(err)
	}
	carried := make(chan error, 1)
	go func() { carried <- carrySequence(w.bridge, w.sid, "s2") }()
	time.Sleep(200 * time.Millisecond)
	write(t, seqPath(w.bridge, "s2"), `{"sequences":[]}`)
	l.Unlock()
	select {
	case err := <-carried:
		if err == nil || !strings.Contains(err.Error(), "not driven from this one") {
			t.Fatalf("the sequence left behind should be reported: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("carrySequence did not finish after the lock was released")
	}
	after, err := os.ReadFile(seqPath(w.bridge, w.sid))
	if err != nil || string(after) != string(before) {
		t.Errorf("the old session's sequence should be unchanged: %v", err)
	}
	if got, err := os.ReadFile(seqPath(w.bridge, "s2")); err != nil || string(got) != `{"sequences":[]}` {
		t.Errorf("the new session's sequence should be unchanged: %q %v", got, err)
	}
}

func queuedReports(bridge, session string) string {
	var all []string
	for _, seq := range readSeq(bridge, session).Sequences {
		all = append(all, seq.Report)
	}
	return strings.Join(all, "\n")
}

func TestCarrySequenceReportsAFailedSaveOfTheMovedFileAndRebindsTheRuns(t *testing.T) {
	w, ids := carryWorld(t)
	real := seqSave
	t.Cleanup(func() { seqSave = real })
	calls := 0
	seqSave = func(path string, v any) error {
		calls++
		if calls == 1 {
			return errors.New("disk full")
		}
		return real(path, v)
	}
	if err := carrySequence(w.bridge, w.sid, "s2"); err != nil {
		t.Fatalf("a queued report is the way the session hears of it: %v", err)
	}
	if !seqExists(w.bridge, "s2") || seqExists(w.bridge, w.sid) {
		t.Error("the sequence should be on the new session")
	}
	for _, id := range ids[1:] {
		if got := w.runState(id).Session; got != "s2" {
			t.Errorf("run %s should be rebound: %s", id, got)
		}
	}
	if got := queuedReports(w.bridge, "s2"); strings.Count(got, "could not be updated") != 1 || !strings.Contains(got, "disk full") || !strings.Contains(got, "does not retry") {
		t.Errorf("the failed save should be queued once:\n%s", got)
	}
}

func TestCarrySequenceReturnsAFailedSaveOfTheMovedFileOnceWhenItCannotBeQueued(t *testing.T) {
	w, ids := carryWorld(t)
	real := seqSave
	t.Cleanup(func() { seqSave = real })
	seqSave = func(string, any) error { return errors.New("disk full") }
	err := carrySequence(w.bridge, w.sid, "s2")
	if err == nil || strings.Count(err.Error(), "could not be updated") != 1 || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("the failed save should be returned once: %v", err)
	}
	for _, id := range ids[1:] {
		if got := w.runState(id).Session; got != "s2" {
			t.Errorf("run %s should be rebound: %s", id, got)
		}
	}
}

func TestCarrySequenceLeavesTheAdvanceOfARunThatFinishedInTheWindowToTheNextStop(t *testing.T) {
	w, ids := carryWorld(t)
	if err := renameSequenceLocked(w.bridge, w.sid, "s2"); err != nil {
		t.Fatal(err)
	}
	r, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status = "success"
	r.State.FinishedAt = time.Now().UnixNano()
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	if text, err := advanceSequence(w.bridge, w.sid); text != "" || err != nil {
		t.Errorf("the finishing hook's advance on the old session should do nothing: %q %v", text, err)
	}
	if seqExists(w.bridge, w.sid) {
		t.Error("the advance should not recreate the old session's sequence")
	}
	if got := readSeq(w.bridge, "s2").Sequences[0].Steps[0].Status; got != "active" {
		t.Fatalf("the step on the new session should still be active: %s", got)
	}
	sequenceStop(w.bridge, &HookInput{SessionID: "s2"})
	if got := readSeq(w.bridge, "s2").Sequences[0].Steps[0].Status; got != "done" {
		t.Errorf("the next Stop on the new session should reconcile the finished run: %s", got)
	}
}

func TestCarrySequenceTakesAnAlreadyMovedSequenceForDone(t *testing.T) {
	w, _ := carryWorld(t)
	old := seqRename
	t.Cleanup(func() { seqRename = old })
	seqRename = func(string, string) error { return os.ErrNotExist }
	if err := carrySequence(w.bridge, w.sid, "s2"); err != nil {
		t.Errorf("a sequence that is already gone is not a failure: %v", err)
	}
}

func TestRebindFailuresFallBackToTheReturnedErrorWhenTheQueueFails(t *testing.T) {
	w, _ := carryWorld(t)
	t.Setenv(lockHeldEnv, w.sid)
	err := reportRebindFailures(w.bridge, w.sid, []rebindFail{{"20260101-000000-supernova", errors.New("disk full"), rebindSaveFailed}})
	if err == nil || !strings.Contains(err.Error(), "20260101-000000-supernova") || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "sequence lock") {
		t.Errorf("the failure should come back with the queue's error: %v", err)
	}
}

func TestWithSeqRefusesForTheSessionWhoseLockAPublishHolds(t *testing.T) {
	w := newWorld(t)
	t.Setenv(lockHeldEnv, w.sid)
	called := false
	err := withSeq(w.bridge, w.sid, func(*seqFile) error { called = true; return nil })
	if err == nil || called || !strings.Contains(err.Error(), "publish command is holding this session's sequence lock") {
		t.Fatalf("withSeq should refuse at once: %v", err)
	}
	if err := withSeq(w.bridge, "s7", func(*seqFile) error { return nil }); err != nil {
		t.Errorf("another session is not refused: %v", err)
	}
}

func TestSequencePublishCommandSeesWhichSessionsLockItHolds(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "placeholder")
	command := "echo held=$STARSHIP_SEQUENCE_LOCK_HELD #"
	if runtime.GOOS == "windows" && gitBashPath() == "" {
		command = "echo held=%STARSHIP_SEQUENCE_LOCK_HELD% & rem"
	}
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Steps[0] = &seqStep{Kind: "publish", Task: "supernova/auth-refactor", Command: command, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.approve()
	if st := w.step(1); st.Status != "done" || !strings.Contains(st.Reason, "held="+w.sid) {
		t.Errorf("the publish command should see the session: %s %q", st.Status, st.Reason)
	}
}

func TestSequenceRunNoteNamesALaterPublishStep(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("publish", task)
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	w.runStage()
	notes := strings.Join(w.state().Notes, "\n")
	if !strings.Contains(notes, "A publish step in the session's sequence takes it from here") || strings.Contains(notes, "Nothing has been opened yet") {
		t.Errorf("the run's note should name the publish step:\n%s", notes)
	}
}

func TestSequenceRunNoteWithoutAPublishStepIsUnchanged(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	task := "supernova/auth-refactor"
	w.mustSeq("run", task)
	w.mustSeq("do", "tell the user")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.codeChange(lead, ws)
	w.runStage()
	notes := strings.Join(w.state().Notes, "\n")
	if !strings.Contains(notes, "Nothing has been opened yet") || strings.Contains(notes, "publish step") {
		t.Errorf("the note should be the usual one:\n%s", notes)
	}
}

func TestPublishStepAheadFollowsTheRun(t *testing.T) {
	const session, task = "s1", "supernova/auth-refactor"
	run := func(id, t string) *seqStep {
		return &seqStep{Kind: "run", Task: t, Status: "active", Runs: []*seqRun{{Run: id}}}
	}
	group := func(t string, ids ...string) *seqStep {
		st := &seqStep{Kind: "run", Task: t, Status: "active"}
		for _, id := range ids {
			st.Runs = append(st.Runs, &seqRun{Run: id, Subtask: "sub-" + id})
		}
		return st
	}
	publish := func(t, status string) *seqStep { return &seqStep{Kind: "publish", Task: t, Status: status} }
	cases := []struct {
		name  string
		steps []*seqStep
		subs  []string
		want  bool
	}{
		{"later publish", []*seqStep{run("r1", task), publish(task, "pending")}, nil, true},
		{"later publish after another task's step", []*seqStep{run("r1", task), run("r2", "supernova/other"), publish(task, "pending")}, nil, true},
		{"publish of another task", []*seqStep{run("r1", task), publish("supernova/other", "pending")}, nil, false},
		{"publish cancelled", []*seqStep{run("r1", task), publish(task, "cancelled")}, nil, false},
		{"another run of the task first", []*seqStep{run("r1", task), run("r2", task), publish(task, "pending")}, nil, false},
		{"publish already done before the run", []*seqStep{publish(task, "done"), run("r1", task)}, nil, false},
		{"publish pending before the run", []*seqStep{publish(task, "pending"), run("r1", task)}, nil, false},
		{"no publish", []*seqStep{run("r1", task)}, nil, false},
		{"run not last in its group", []*seqStep{group(task, "r1", "r2"), publish(task, "pending")}, []string{"sub-r1", "sub-r2"}, true},
		{"last run of a group", []*seqStep{group(task, "r0", "r1"), publish(task, "pending")}, []string{"sub-r0", "sub-r1"}, true},
		{"last run of a group with a further group", []*seqStep{group(task, "r0", "r1"), publish(task, "pending")}, []string{"sub-r0", "sub-r1", "sub-later"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bridge := t.TempDir()
			for _, name := range c.subs {
				write(t, filepath.Join(subtasksDir(taskDirOf(bridge, task)), name+".md"), "## Goal\n\nx\n")
			}
			sf := &seqFile{Sequences: []*sequence{{N: 1, Mode: "stop", Steps: c.steps}}}
			if err := os.MkdirAll(filepath.Dir(seqPath(bridge, session)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(seqPath(bridge, session), sf); err != nil {
				t.Fatal(err)
			}
			id := "r1"
			sub := ""
			if c.subs != nil {
				sub = "sub-r1"
			}
			r := &RunFiles{Snap: &Snapshot{Bridge: bridge, Task: task, Subtask: sub}, State: &State{Run: id, Session: session, Sequence: true}}
			if got, err := publishStepAhead(r); err != nil || got != c.want {
				t.Errorf("got %v, %v, want %v", got, err, c.want)
			}
		})
	}
}

func TestPublishRunsACustomCommandOncePerLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "t", "models, client, server")
	log := filepath.Join(t.TempDir(), "log")
	run := &seqStep{N: 1, Kind: "run", Task: task, Earlier: []string{"models"}, Runs: []*seqRun{{Run: "r1", Location: "client"}, {Run: "r2", Location: "server"}}}
	pub := &seqStep{N: 2, Kind: "publish", Task: task, Command: `echo "$(basename "$PWD")" | tee -a ` + shellQuote(log) + ` #`}
	out, err := runPublish(w.bridge, w.sid, &sequence{Steps: []*seqStep{run, pub}}, pub)
	if err != nil {
		t.Fatal(err)
	}
	if want := "landed models (models), client (client), server (server)"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
	if got, _ := os.ReadFile(log); string(got) != "models\nclient\nserver\n" {
		t.Errorf("the command should run once in each location's folder, dependencies first:\n%s", got)
	}
	if publishRecorded(w.bridge, task) {
		t.Error("everything landed, so no record is left")
	}
}

func TestPublishFailureOfACustomCommandNamesTheLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "t", "models, client, server")
	run := &seqStep{N: 1, Kind: "run", Task: task, Runs: []*seqRun{{Run: "r1", Location: "models"}, {Run: "r2", Location: "client"}, {Run: "r3", Location: "server"}}}
	pub := &seqStep{N: 2, Kind: "publish", Task: task, Command: `test "$(basename "$PWD")" != client #`}
	_, err := runPublish(w.bridge, w.sid, &sequence{Steps: []*seqStep{run, pub}}, pub)
	if err == nil || !strings.HasPrefix(err.Error(), "landed models (); never tried server; client failed: ") {
		t.Fatalf("got %v", err)
	}
	// A new publish step in another session, with no run step, lands what is left.
	pub.Command = `true #`
	again := &seqStep{N: 1, Kind: "publish", Task: task, Command: pub.Command}
	out, err := runPublish(w.bridge, "other-session", &sequence{Steps: []*seqStep{again}}, again)
	if err != nil || out != "landed client (), server ()" {
		t.Errorf("got %q, %v", out, err)
	}
	if publishRecorded(w.bridge, task) {
		t.Error("nothing is left, so the record is gone")
	}
}

func TestSkippedStepOfAnEarlierGroupBlocksAPublishThatLandsItsLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "t", "models, client, server")
	skipped := &seqStep{N: 1, Kind: "run", Task: task, Status: "failed", Reason: skipPrefix + "no", Earlier: []string{"models"}, Runs: []*seqRun{{Run: "r1", Location: "client"}}}
	run := &seqStep{N: 2, Kind: "run", Task: task, Status: "done", Runs: []*seqRun{{Run: "r2", Location: "server"}}}
	pub := &seqStep{N: 3, Kind: "publish", Task: task, Status: "pending"}
	seq := &sequence{N: 1, Steps: []*seqStep{skipped, run, pub}}
	sf := &seqFile{Sequences: []*sequence{seq}}
	if got, _ := sf.skippedBefore(w.bridge, seq, pub); got != nil {
		t.Errorf("the skipped step never worked in server: %v", got.ref())
	}
	if err := writePublishRecord(w.bridge, task, []string{"models"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := sf.skippedBefore(w.bridge, seq, pub); got != skipped {
		t.Errorf("models, from the skipped step's earlier group, is among the targets: %v", got)
	}
}

func TestAStepKeepsTheLocationsOfItsEarlierGroups(t *testing.T) {
	st := &seqStep{Kind: "run", Runs: []*seqRun{{Run: "r1", Location: "client"}, {Run: "r2", Location: "server"}}}
	st.Earlier = append(st.Earlier, st.runLocations("")...)
	st.setRuns([]*seqRun{{Run: "r3", Location: "client"}})
	if got := st.locations(""); !slices.Equal(got, []string{"client", "server"}) {
		t.Errorf("got %v", got)
	}
}

func TestSequencePublishCommandSeesWhichTaskPublishLockItHolds(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("do", "placeholder")
	command := "echo held=$STARSHIP_PUBLISH_LOCK_HELD #"
	if runtime.GOOS == "windows" && gitBashPath() == "" {
		command = "echo held=%STARSHIP_PUBLISH_LOCK_HELD% & rem"
	}
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Steps[0] = &seqStep{Kind: "publish", Task: "supernova/auth-refactor", Command: command, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.approve()
	if st := w.step(1); st.Status != "done" || !strings.Contains(st.Reason, "held=supernova/auth-refactor") {
		t.Errorf("the publish command should see the task: %s %q", st.Status, st.Reason)
	}
}
