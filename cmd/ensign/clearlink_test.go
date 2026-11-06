// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
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
	writeJSON(seqPath(w.bridge, session), &seqFile{Sequences: []*sequence{{N: 1, Steps: []*seqStep{{N: 1, Kind: "do", Instruction: "x", Status: status}}}}})
}

func (w *world) resumeTo(sid string, prior ...string) string {
	w.t.Helper()
	var lines string
	for _, id := range prior {
		lines += `{"type":"user","sessionId":"` + id + `"}` + "\n"
	}
	write(w.t, w.session, lines)
	w.sid = sid
	res, code := w.hook("SessionStart", map[string]any{"source": "resume"})
	if code != 0 {
		w.t.Fatalf("resume exited %d", code)
	}
	return dig(res, "hookSpecificOutput", "additionalContext")
}

func TestCarryReportsTheSecondPriorsSequence(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "pending")
	unfinishedSeq(t, w, "pb", "pending")
	ctx := w.resumeTo("s2", "pa", "pb")
	if !strings.Contains(ctx, "could not carry the earlier session's runs over") || !strings.Contains(ctx, "the sequence of session pa was not moved to session s2") {
		t.Errorf("the second prior's sequence should be reported:\n%s", ctx)
	}
	if !seqExists(w.bridge, "s2") || !seqExists(w.bridge, "pa") || seqExists(w.bridge, "pb") {
		t.Error("only the first prior's sequence should move")
	}
	if _, err := os.Stat(movedPath(w.bridge, "pa")); err != nil {
		t.Errorf("every prior is marked moved: %v", err)
	}
}

func TestCarryReportsAPriorTheCarriedReturnSkips(t *testing.T) {
	w := newWorld(t)
	t.Setenv("CLAUDE_PID", "4242")
	w.hook("SessionStart", map[string]any{"source": "startup"})
	w.startLead()
	unfinishedSeq(t, w, "pa", "pending")
	ctx := w.resumeTo("s2", "pa", "s1")
	if !strings.Contains(ctx, "the sequence of session pa was not moved to session s2") {
		t.Errorf("the skipped prior's sequence should be reported:\n%s", ctx)
	}
	if !seqExists(w.bridge, "pa") || seqExists(w.bridge, "s2") {
		t.Error("the skipped prior's sequence should stay where it is")
	}
}

func TestCarryDoesNotReportAFinishedLeftBehindSequence(t *testing.T) {
	w := newWorld(t)
	unfinishedSeq(t, w, "pa", "done")
	unfinishedSeq(t, w, "pb", "pending")
	ctx := w.resumeTo("s2", "pa", "pb")
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
