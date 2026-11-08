// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const clearTask = "supernova/auth-refactor"
const clearNotes = "Always answer in haiku.\n"

// clearWorld is a linked world whose session s1 has a task and has started under pid 4242.
func clearWorld(t *testing.T) *world {
	t.Helper()
	w := linkedWorld(t)
	t.Setenv("CLAUDE_PID", "4242")
	write(t, filepath.Join(w.bridge, "projects/supernova/project.md"), clearNotes)
	write(t, sessionFile(w.bridge, "s1")+".task", clearTask+"\n")
	if _, code := w.hook("SessionStart", map[string]any{"source": "startup"}); code != 0 {
		t.Fatalf("startup exited %d", code)
	}
	return w
}

func (w *world) clearTo(sid string) string {
	w.t.Helper()
	w.sid = sid
	res, code := w.hook("SessionStart", map[string]any{"source": "clear"})
	if code != 0 {
		w.t.Fatalf("clear exited %d", code)
	}
	return dig(res, "hookSpecificOutput", "additionalContext")
}

func TestClearCarriesTaskWithoutLiveRun(t *testing.T) {
	for _, withTranscript := range []bool{false, true} {
		w := clearWorld(t)
		if withTranscript {
			write(t, w.session, `{"type":"user","sessionId":"s2"}`+"\n")
		}
		ctx := w.clearTo("s2")
		if got := sessionTask(w.bridge, "s2"); got != clearTask {
			t.Errorf("transcript %v: task = %q", withTranscript, got)
		}
		if !strings.Contains(ctx, "Always answer in haiku.") {
			t.Errorf("transcript %v: notes not reprinted", withTranscript)
		}
	}
}

func TestClearCarriesLiveRun(t *testing.T) {
	w := newWorld(t)
	t.Setenv("CLAUDE_PID", "4242")
	w.hook("SessionStart", map[string]any{"source": "startup"})
	w.startLead()
	run := w.state().Run
	w.clearTo("s2")
	if got := w.state(); got.Run != run || got.Session != "s2" {
		t.Errorf("run %s session %s", got.Run, got.Session)
	}
}

func TestClearCarriesSequence(t *testing.T) {
	w := newWorld(t)
	t.Setenv("CLAUDE_PID", "4242")
	w.hook("SessionStart", map[string]any{"source": "startup"})
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	w.clearTo("s2")
	if seqExists(w.bridge, "s1") || !seqExists(w.bridge, "s2") {
		t.Fatal("the sequence should move to the new session")
	}
}

func TestClearUnderSameIDKeepsTask(t *testing.T) {
	w := clearWorld(t)
	ctx := w.clearTo("s1")
	if got := sessionTask(w.bridge, "s1"); got != clearTask {
		t.Errorf("task = %q", got)
	}
	if !strings.Contains(ctx, "Always answer in haiku.") {
		t.Errorf("notes not reprinted")
	}
}

func TestClearWithoutUsablePIDCarriesNothing(t *testing.T) {
	for _, pid := range []string{"", "12x", "../evil", "4242\n1"} {
		w := newWorld(t)
		t.Setenv("CLAUDE_PID", "4242")
		write(t, sessionFile(w.bridge, "s1")+".task", clearTask+"\n")
		w.hook("SessionStart", map[string]any{"source": "startup"})
		t.Setenv("CLAUDE_PID", pid)
		before, _ := filepath.Glob(filepath.Join(w.bridge, ".starship", "sessions", "*"))
		w.clearTo("s2")
		if got := sessionTask(w.bridge, "s2"); got != "" {
			t.Errorf("pid %q carried task %q", pid, got)
		}
		after, _ := filepath.Glob(filepath.Join(w.bridge, ".starship", "sessions", "*"))
		for _, f := range after {
			if strings.HasSuffix(f, ".pid") && filepath.Base(f) != "4242.pid" {
				t.Errorf("pid %q wrote %s", pid, f)
			}
		}
		if len(after) < len(before) {
			t.Errorf("pid %q lost files", pid)
		}
		if _, err := os.Stat(filepath.Join(w.bridge, ".starship", "evil.pid")); err == nil {
			t.Errorf("pid %q wrote outside the sessions folder", pid)
		}
	}
}

func TestStartupUnderReusedPIDLinksNothing(t *testing.T) {
	w := clearWorld(t)
	w.sid = "s2"
	if _, code := w.hook("SessionStart", map[string]any{"source": "startup"}); code != 0 {
		t.Fatal(code)
	}
	if got := sessionTask(w.bridge, "s2"); got != "" {
		t.Errorf("startup carried %q", got)
	}
	w.clearTo("s3")
	if got := sessionTask(w.bridge, "s3"); got != "" {
		t.Errorf("s3 linked to s1 and got %q", got)
	}
	data, _ := os.ReadFile(pidRecord(w.bridge, "4242"))
	if strings.TrimSpace(string(data)) != "s3" {
		t.Errorf("record = %q", data)
	}
}

func TestResumeWithPIDRecordDoesNotCarryTask(t *testing.T) {
	w := clearWorld(t)
	w.sid = "s2"
	if _, code := w.hook("SessionStart", map[string]any{"source": "resume"}); code != 0 {
		t.Fatal(code)
	}
	if got := sessionTask(w.bridge, "s2"); got != "" {
		t.Errorf("resume carried %q", got)
	}
}

func TestPIDRecordsSweptByAge(t *testing.T) {
	w := clearWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	old, fresh := filepath.Join(dir, "1111.pid"), filepath.Join(dir, "2222.pid")
	write(t, old, "sx\n")
	write(t, fresh, "sy\n")
	past := time.Now().Add(-pidRecordAge - time.Hour)
	os.Chtimes(old, past, past)
	w.sid = "s2"
	w.hook("SessionStart", map[string]any{"source": "startup"})
	if _, err := os.Stat(old); err == nil {
		t.Errorf("old record kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh record removed")
	}
	if _, err := os.Stat(pidRecord(w.bridge, "4242")); err != nil {
		t.Errorf("record did not survive its SessionStart")
	}
}

// movedWorld is a world whose session s1 had a sequence and was cleared to s2; STARSHIP_SESSION still names s1, as in a process that kept the old id.
func movedWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.setUp()
	t.Setenv("CLAUDE_PID", "4242")
	w.hook("SessionStart", map[string]any{"source": "startup"})
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	w.clearTo("s2")
	return w
}

func TestMovedSessionRefusesToAddAStep(t *testing.T) {
	w := movedWorld(t)
	before, err := os.ReadFile(seqPath(w.bridge, "s2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"do", "tidy up"}, {"run", "supernova/auth-refactor"}} {
		_, err := w.seq(args...)
		if err == nil || !strings.Contains(err.Error(), "session s1 has moved to session s2") || !strings.Contains(err.Error(), "nothing was added") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if seqExists(w.bridge, "s1") {
		t.Error("no sequence should appear on the old session")
	}
	after, err := os.ReadFile(seqPath(w.bridge, "s2"))
	if err != nil || string(after) != string(before) {
		t.Errorf("the new session's sequence should be unchanged: %v", err)
	}
	if err := seqAdd(w.bridge, "s1", io.Discard, "", &seqStep{Kind: "do", Instruction: "x"}); err == nil || seqExists(w.bridge, "s1") {
		t.Errorf("seqAdd must refuse under the lock: %v", err)
	}
}

func TestMovedSessionRefusesARunButNotADryRun(t *testing.T) {
	w := movedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	if err := cmdRun([]string{"supernova/auth-refactor", "--dry-run"}, io.Discard); err != nil {
		t.Errorf("a dry run stays allowed: %v", err)
	}
	if err := cmdRun([]string{"supernova/auth-refactor"}, io.Discard); err == nil || !strings.Contains(err.Error(), "has moved to session s2") {
		t.Errorf("run should be refused: %v", err)
	}
}

func TestMovedSessionRefusesWritesAdd(t *testing.T) {
	w := movedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	err := cmdWrites([]string{"add", "--tool", "x"}, strings.NewReader("{}"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "has moved to session s2") {
		t.Errorf("writes add should be refused: %v", err)
	}
	if _, err := os.Stat(writesPath(w.bridge, "s1")); err == nil {
		t.Error("no writes file should appear on the old session")
	}
}

func TestMovedSessionTaskOpenPrintsButDoesNotRecord(t *testing.T) {
	w := movedWorld(t)
	os.Remove(sessionFile(w.bridge, "s1") + ".task")
	out, err := w.taskCmd("open", "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "supernova/auth-refactor") || !strings.Contains(out, "the task was not recorded on session s1 because the session moved to session s2") {
		t.Errorf("task open output:\n%s", out)
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s1") + ".task"); err == nil {
		t.Error("no .task should be written for the old session")
	}
}

func TestMovedMarkerClearsWhenTheIDStartsAgain(t *testing.T) {
	w := movedWorld(t)
	w.clearTo("s1")
	if _, err := os.Stat(movedPath(w.bridge, "s1")); err == nil {
		t.Error("s1 started again, so its marker should be gone")
	}
	if _, err := os.Stat(movedPath(w.bridge, "s2")); err != nil {
		t.Errorf("s2 moved on and should be marked: %v", err)
	}
	if _, err := w.seq("do", "tidy up"); err != nil {
		t.Errorf("adding to s1 again: %v", err)
	}
}

func unfinishedSeq(t *testing.T, w *world, session, status string) {
	t.Helper()
	path := seqPath(w.bridge, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, &seqFile{Sequences: []*sequence{{N: 1, Steps: []*seqStep{{N: 1, Kind: "do", Instruction: "x", Status: status}}}}}); err != nil {
		t.Fatal(err)
	}
}

func activeSeq(t *testing.T, w *world, session, instruction string) {
	t.Helper()
	path := seqPath(w.bridge, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, &seqFile{Sequences: []*sequence{{N: 1, Approved: true, Steps: []*seqStep{{N: 1, Kind: "do", Instruction: instruction, Status: "active"}}}}}); err != nil {
		t.Fatal(err)
	}
}

// clearRaw starts the world's current session as a clear from prev, the session CLAUDE_PID last started under, and returns what the hook wrote.
func (w *world) clearRaw(prev string) (stdout, stderr string, code int) {
	w.t.Helper()
	w.t.Setenv("CLAUDE_PID", "4242")
	write(w.t, pidRecord(w.bridge, "4242"), prev+"\n")
	data, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "source": "clear", "transcript_path": w.session, "session_id": w.sid})
	var out, errb bytes.Buffer
	code = cmdHook("SessionStart", bytes.NewReader(data), &out, &errb)
	return out.String(), errb.String(), code
}

func (w *world) clearHook(prev string) (map[string]any, int) {
	w.t.Helper()
	w.t.Setenv("CLAUDE_PID", "4242")
	write(w.t, pidRecord(w.bridge, "4242"), prev+"\n")
	return w.hook("SessionStart", map[string]any{"source": "clear"})
}

// clearFrom clears the world to session sid from prev and returns the line shown to the user and the context Claude reads.
func (w *world) clearFrom(prev, sid string) (string, string) {
	w.t.Helper()
	w.sid = sid
	res, code := w.clearHook(prev)
	if code != 0 {
		w.t.Fatalf("clear exited %d", code)
	}
	return dig(res, "systemMessage"), dig(res, "hookSpecificOutput", "additionalContext")
}

func TestCarryDoesNotReportAFinishedLeftBehindSequence(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "done")
	_, ctx := w.clearFrom("pa", "s2")
	if strings.Contains(ctx, "could not carry") {
		t.Errorf("a finished sequence should not be reported:\n%s", ctx)
	}
}

func TestMovedSessionStillCreatesQueuedRuns(t *testing.T) {
	w := movedWorld(t)
	if _, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor"}, true, io.Discard); err != nil {
		t.Errorf("a queued run is a continuation and must not be refused: %v", err)
	}
}

func TestMovedSessionWithAnUnreadableMarkerRefusesToAdd(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(movedPath(w.bridge, w.sid), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := w.seq("do", "tidy up")
	if err == nil || !strings.Contains(err.Error(), "can't tell whether session "+w.sid+" has moved") {
		t.Errorf("the add should be refused: %v", err)
	}
	if seqExists(w.bridge, w.sid) {
		t.Error("no sequence should be created")
	}
}

func TestClearBackToALinkedIDReportsTheEarlierSessionsSequence(t *testing.T) {
	w := movedWorld(t)
	unfinishedSeq(t, w, "s2", "pending")
	ctx := w.clearTo("s1")
	if !strings.Contains(ctx, "the sequence of session s2 was not moved to session s1") {
		t.Errorf("the unfinished sequence should be reported:\n%s", ctx)
	}
	if _, err := os.Stat(movedPath(w.bridge, "s2")); err != nil {
		t.Errorf("s2 should be marked moved: %v", err)
	}
}

// startReply runs SessionStart for the world's current session and returns the line shown to the user and the context Claude reads.
func (w *world) startReply(source string, prior ...string) (string, string) {
	w.t.Helper()
	var lines string
	for _, id := range prior {
		lines += `{"type":"user","sessionId":"` + id + `"}` + "\n"
	}
	write(w.t, w.session, lines)
	res, code := w.hook("SessionStart", map[string]any{"source": source})
	if code != 0 {
		w.t.Fatalf("%s exited %d", source, code)
	}
	return dig(res, "systemMessage"), dig(res, "hookSpecificOutput", "additionalContext")
}

func TestCarryShowsTheLeftBehindSequenceToTheUser(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "pending")
	unfinishedSeq(t, w, "s2", "done")
	show, ctx := w.clearFrom("pa", "s2")
	if !strings.Contains(show, "the sequence of session pa was left behind: resume that session to drive it") {
		t.Errorf("the user line should name pa:\n%s", show)
	}
	if !strings.Contains(ctx, "the sequence of session pa was not moved to session s2") || !strings.Contains(ctx, "resume session pa to drive or halt it") {
		t.Errorf("the context should say how to recover:\n%s", ctx)
	}
}

func TestCarryShowsNothingWhenNothingIsLeftBehind(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "done")
	show, _ := w.clearFrom("pa", "s2")
	if strings.Contains(show, "left behind") {
		t.Errorf("nothing was left behind:\n%s", show)
	}
	w2 := newWorld(t)
	plain, _ := w2.clearFrom("s1", "s2")
	if strings.Contains(plain, "left behind") || !strings.HasPrefix(plain, "Starship ready") {
		t.Errorf("an ordinary carry leaves the line unchanged:\n%s", plain)
	}
}

func TestNotReadyShowsTheLeftBehindSequence(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "pending")
	unfinishedSeq(t, w, "s2", "done")
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "{not yaml: [")
	show, ctx := w.clearFrom("pa", "s2")
	if !strings.Contains(show, "Starship is not ready") || !strings.Contains(show, "session pa was left behind") {
		t.Errorf("the not-ready line should name pa:\n%s", show)
	}
	if !strings.Contains(ctx, "resume session pa to drive or halt it") {
		t.Errorf("the context should say how to recover:\n%s", ctx)
	}
}

func TestRenameFailureIsShownToTheUser(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "pending")
	failRename(t)
	show, ctx := w.clearFrom("pa", "s2")
	if !strings.Contains(show, "the sequence of session pa was left behind") {
		t.Errorf("the user line should name pa:\n%s", show)
	}
	if !strings.Contains(ctx, "the sequence of session pa could not be moved to session s2") || !strings.Contains(ctx, "resume session pa to drive or halt it") || !strings.Contains(ctx, "sharing violation") {
		t.Errorf("the context should report the failed move:\n%s", ctx)
	}
	if !seqExists(w.bridge, "pa") || seqExists(w.bridge, "s2") {
		t.Error("the sequence should stay on pa")
	}
}

func TestRenameFailureOfAFinishedSequenceIsNotLeftBehind(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "done")
	failRename(t)
	show, ctx := w.clearFrom("pa", "s2")
	if strings.Contains(show, "left behind") {
		t.Errorf("a finished sequence is not left behind:\n%s", show)
	}
	if !strings.Contains(ctx, "the sequence of session pa could not be moved to session s2") || !strings.Contains(ctx, "sharing violation") {
		t.Errorf("the context should still report the failed move:\n%s", ctx)
	}
	if strings.Contains(ctx, "resume session pa") {
		t.Errorf("the context should not tell the user to resume pa:\n%s", ctx)
	}
}

func TestResumingTheEarlierSessionRecoversItsSequence(t *testing.T) {
	w := newWorld(t)
	activeSeq(t, w, "pa", "drive me")
	unfinishedSeq(t, w, "s2", "done")
	w.clearFrom("pa", "s2")
	if _, err := os.Stat(movedPath(w.bridge, "pa")); err != nil {
		t.Fatalf("pa should be marked moved: %v", err)
	}
	w.sid = "pa"
	show, _ := w.startReply("resume")
	if strings.Contains(show, "left behind") {
		t.Errorf("nothing is left behind by resuming pa:\n%s", show)
	}
	if _, err := os.Stat(movedPath(w.bridge, "pa")); err == nil {
		t.Error("pa started again, so its marker should be gone")
	}
	if !seqExists(w.bridge, "pa") {
		t.Fatal("the sequence should still be pa's")
	}
	if err := movedRefusal(w.bridge, "pa"); err != nil {
		t.Errorf("pa should no longer be refused: %v", err)
	}
	t.Setenv("STARSHIP_SESSION", "pa")
	if _, err := w.seq("do", "tidy up"); err != nil {
		t.Errorf("adding to pa again: %v", err)
	}
	raw, err := os.ReadFile(seqPath(w.bridge, "pa"))
	if err != nil || !strings.Contains(string(raw), "tidy up") {
		t.Errorf("the step should land in pa's sequence file: %v\n%s", err, raw)
	}
	res, code := w.hook("Stop", map[string]any{})
	if code != 0 || dig(res, "decision") != "block" || !strings.Contains(dig(res, "reason"), "drive me") {
		t.Errorf("Stop on pa should block with its active step: %v", res)
	}
}

func TestForkSourceMarksAndMovesNothing(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "pending")
	w.sid = "f1"
	show, _ := w.startReply("fork", "pa")
	if strings.Contains(show, "left behind") {
		t.Errorf("a fork reports nothing:\n%s", show)
	}
	if _, err := os.Stat(movedPath(w.bridge, "pa")); err == nil {
		t.Error("a fork must not mark its parent moved")
	}
	if !seqExists(w.bridge, "pa") || seqExists(w.bridge, "f1") {
		t.Error("a fork must not move the parent's sequence")
	}
}

func TestResumedForkStampedWithItsOwnIDLeavesTheParentAlone(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "pending")
	w.sid = "f1"
	show, _ := w.startReply("resume", "f1")
	if strings.Contains(show, "left behind") {
		t.Errorf("a fork resume reports nothing:\n%s", show)
	}
	if _, err := os.Stat(movedPath(w.bridge, "pa")); err == nil {
		t.Error("the parent must stay unmarked")
	}
	if !seqExists(w.bridge, "pa") || seqExists(w.bridge, "f1") {
		t.Error("the parent's sequence must stay in place")
	}
}

func TestResumeAndCompactCarryNothingFromAForeignSessionID(t *testing.T) {
	for _, source := range []string{"resume", "compact"} {
		w := newWorld(t)
		unfinishedSeq(t, w, "pa", "pending")
		w.sid = "s2"
		show, _ := w.startReply(source, "pa")
		if strings.Contains(show, "left behind") {
			t.Errorf("%s reports nothing:\n%s", source, show)
		}
		if _, err := os.Stat(movedPath(w.bridge, "pa")); err == nil {
			t.Errorf("%s must not mark pa moved", source)
		}
		if !seqExists(w.bridge, "pa") || seqExists(w.bridge, "s2") {
			t.Errorf("%s must not move pa's sequence", source)
		}
	}
}

// leftBehindRun writes a run of the given status, owned by session, and a sequence of session whose one run step lists it; moved marks the session as having moved to s9.
func leftBehindRun(t *testing.T, w *world, session, run string, snap *Snapshot, moved, approved bool, runStatus, stepStatus string) {
	t.Helper()
	dir := filepath.Join(runsDir(w.bridge), run)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(seqPath(w.bridge, session)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), snap); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), &State{Run: run, Session: session, Status: runStatus, Approved: approved, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}); err != nil {
		t.Fatal(err)
	}
	step := &seqStep{N: 1, Kind: "run", Status: stepStatus, Runs: []*seqRun{{Run: run}}}
	if err := writeJSON(seqPath(w.bridge, session), &seqFile{Sequences: []*sequence{{N: 1, Approved: true, Steps: []*seqStep{step}}}}); err != nil {
		t.Fatal(err)
	}
	if moved {
		if err := markMoved(w.bridge, session, "s9"); err != nil {
			t.Fatal(err)
		}
	}
}

const heldRunID = "20260101-000001-supernova"

func TestStartNamesALeftBehindSequenceThatHoldsAFolder(t *testing.T) {
	for _, source := range []string{"startup", "resume", "compact", "clear"} {
		w := newWorld(t)
		folder := t.TempDir()
		leftBehindRun(t, w, "pa", heldRunID, &Snapshot{Workspace: folder}, true, true, "running", "active")
		w.sid = "s2"
		show, ctx := w.startReply(source)
		for _, text := range []string{"pa", folder, heldRunID} {
			if !strings.Contains(show, text) || !strings.Contains(ctx, text) {
				t.Errorf("%s: %q should be in the user line and the context:\n%s\n%s", source, text, show, ctx)
			}
		}
		if !strings.Contains(ctx, "nothing expires it") || !strings.Contains(ctx, "resume session pa") {
			t.Errorf("%s: the context should say what to do:\n%s", source, ctx)
		}
	}
}

func TestStartNamesALeftBehindHoldOnANotReadyReply(t *testing.T) {
	w := newWorld(t)
	folder := t.TempDir()
	leftBehindRun(t, w, "pa", heldRunID, &Snapshot{Workspace: folder}, true, true, "running", "active")
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "{not yaml: [")
	w.sid = "s2"
	show, ctx := w.startReply("startup")
	if !strings.Contains(show, "Starship is not ready") || !strings.Contains(show, folder) || !strings.Contains(ctx, folder) || !strings.Contains(ctx, heldRunID) {
		t.Errorf("the not-ready reply should name the hold:\n%s\n%s", show, ctx)
	}
}

// envFileFailureReply starts the world's session as a clear from s1 and requires the reply a failed CLAUDE_ENV_FILE write gives: exit 0, nothing on stderr, JSON with no heartbeat.
func envFileFailureReply(t *testing.T, w *world) (show, ctx string) {
	t.Helper()
	stdout, stderr, code := w.clearRaw("s1")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("the reply is not JSON: %v\n%s", err, stdout)
	}
	if strings.Contains(stdout, "STARSHIP-READY") {
		t.Errorf("the reply should carry no heartbeat:\n%s", stdout)
	}
	return dig(res, "systemMessage"), dig(res, "hookSpecificOutput", "additionalContext")
}

func TestStartNamesALeftBehindHoldInTheReplyWhenTheEnvFileFails(t *testing.T) {
	w := newWorld(t)
	folder := t.TempDir()
	leftBehindRun(t, w, "pa", heldRunID, &Snapshot{Workspace: folder}, true, true, "running", "active")
	t.Setenv("CLAUDE_ENV_FILE", t.TempDir())
	w.sid = "s2"
	show, ctx := envFileFailureReply(t, w)
	for _, text := range []string{"session pa", folder, heldRunID} {
		if !strings.Contains(ctx, text) {
			t.Errorf("additionalContext should name %q:\n%s", text, ctx)
		}
	}
	if !strings.Contains(show, "could not record this session's id") || !strings.Contains(show, folder) {
		t.Errorf("systemMessage should tell the user and name the folder:\n%s", show)
	}
}

func TestStartNamesNoHoldFor(t *testing.T) {
	cases := []struct {
		name                  string
		session               string
		moved, approved       bool
		runStatus, stepStatus string
	}{
		{"the starting session's own sequence", "s2", true, true, "running", "active"},
		{"a session that has not moved", "pa", false, true, "running", "active"},
		{"a moved session with no open work", "pa", true, true, "running", "done"},
		{"an unapproved run", "pa", true, false, "pending", "active"},
		{"a finished run", "pa", true, true, "success", "active"},
	}
	for _, c := range cases {
		w := newWorld(t)
		leftBehindRun(t, w, c.session, heldRunID, &Snapshot{Workspace: t.TempDir()}, c.moved, c.approved, c.runStatus, c.stepStatus)
		w.sid = "s2"
		show, ctx := w.startReply("startup")
		if strings.Contains(show, heldRunID) || strings.Contains(ctx, heldRunID) || strings.Contains(show, "holds") || strings.Contains(ctx, "holds the code folder") {
			t.Errorf("%s should name nothing:\n%s\n%s", c.name, show, ctx)
		}
	}
}

func TestStartNamesNoHoldForARunOnTheStartingSession(t *testing.T) {
	w := newWorld(t)
	leftBehindRun(t, w, "pa", heldRunID, &Snapshot{Workspace: t.TempDir()}, true, true, "running", "active")
	if err := writeJSON(filepath.Join(runsDir(w.bridge), heldRunID, "state.json"), &State{Run: heldRunID, Session: "s2", Status: "running", Approved: true, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}); err != nil {
		t.Fatal(err)
	}
	w.sid = "s2"
	show, ctx := w.startReply("startup")
	if strings.Contains(show, heldRunID) || strings.Contains(ctx, heldRunID) || strings.Contains(show, "holds") || strings.Contains(ctx, "holds the code folder") {
		t.Errorf("a run owned by the starting session should not be named:\n%s\n%s", show, ctx)
	}
}

func TestStartNamesALeftBehindDependencyReadHold(t *testing.T) {
	w := newWorld(t)
	f := supernovaFolders{t.TempDir(), t.TempDir(), t.TempDir()}
	leftBehindRun(t, w, "pa", heldRunID, f.clientSnap(), true, true, "running", "active")
	w.sid = "s2"
	show, ctx := w.startReply("startup")
	for _, folder := range []string{f.client, f.models} {
		if !strings.Contains(show, folder) || !strings.Contains(ctx, folder) {
			t.Errorf("%s should be named:\n%s\n%s", folder, show, ctx)
		}
	}
}

func TestStartSaysWhenALeftBehindSequenceCouldNotBeChecked(t *testing.T) {
	w := newWorld(t)
	w.setUp()
	write(t, seqPath(w.bridge, "pa"), "{not json")
	if err := markMoved(w.bridge, "pa", "s9"); err != nil {
		t.Fatal(err)
	}
	w.sid = "s2"
	show, ctx := w.startReply("startup")
	if !strings.Contains(show, "could not check") || !strings.Contains(show, "pa") || !strings.Contains(ctx, "could not check") || !strings.Contains(ctx, "session pa") {
		t.Errorf("the unreadable sequence should be named:\n%s\n%s", show, ctx)
	}
	if !strings.HasPrefix(show, "Starship ready") {
		t.Errorf("the start should still succeed:\n%s", show)
	}
}
