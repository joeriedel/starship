package main

import (
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
