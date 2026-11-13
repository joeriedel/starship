package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// promptHook sends a prompt with the wait for locks shortened, and fails the test when the hook hangs.
func promptHook(w *world, prompt string) map[string]any {
	w.t.Helper()
	old := promptLockWait
	w.t.Cleanup(func() { promptLockWait = old })
	promptLockWait = 100 * time.Millisecond
	done := make(chan map[string]any, 1)
	go func() {
		res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": prompt})
		done <- res
	}()
	select {
	case res := <-done:
		return res
	case <-time.After(10 * time.Second):
		w.t.Fatalf("the %q prompt hook waited on a held lock", prompt)
		return nil
	}
}

func holdLock(t *testing.T, path string) {
	t.Helper()
	l := flock.New(path)
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Unlock() })
}

func pendingRunLock(w *world) string {
	w.t.Helper()
	w.run("supernova/auth-refactor")
	all, err := listRuns(w.bridge)
	if err != nil || len(all) != 1 {
		w.t.Fatalf("want one run: %v %v", all, err)
	}
	return filepath.Join(runsDir(w.bridge), all[0].id, "lock")
}

func TestPromptDoesNotWaitForTheRunLock(t *testing.T) {
	w := newWorld(t)
	holdLock(t, pendingRunLock(w))
	if res := promptHook(w, "what is next"); res != nil {
		t.Errorf("a plain prompt should add nothing: %v", res)
	}
}

func TestGoLeavesARunPendingWhenTheRunLockIsHeld(t *testing.T) {
	w := newWorld(t)
	holdLock(t, pendingRunLock(w))
	ctx := dig(promptHook(w, "go"), "hookSpecificOutput", "additionalContext")
	if !strings.Contains(ctx, "approved nothing") || !strings.Contains(ctx, "is busy") || !strings.Contains(ctx, "go again") {
		t.Errorf("go should say the run is busy: %q", ctx)
	}
	all, _ := listRuns(w.bridge)
	if all[0].state.Approved || all[0].state.Status != "pending" {
		t.Errorf("the run should still be pending: %+v", all[0].state)
	}
}

func TestGoLeavesARunPendingWhenTheFoldersLockIsHeld(t *testing.T) {
	w := newWorld(t)
	pendingRunLock(w)
	holdLock(t, filepath.Join(w.bridge, ".starship", "folders.lock"))
	ctx := dig(promptHook(w, "go"), "hookSpecificOutput", "additionalContext")
	if !strings.Contains(ctx, "approved nothing") || !strings.Contains(ctx, "go again") {
		t.Errorf("go should say it approved nothing: %q", ctx)
	}
	all, _ := listRuns(w.bridge)
	if all[0].state.Approved || all[0].state.Status != "pending" {
		t.Errorf("the run should still be pending: %+v", all[0].state)
	}
}

func TestPromptLeavesASequenceReportWhenTheSequenceLockIsHeld(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Report = "supernova update"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l := flock.New(seqPath(w.bridge, w.sid) + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	if res := promptHook(w, "what is next"); res != nil {
		t.Errorf("the report can't be taken while the lock is held: %v", res)
	}
	if readSeq(w.bridge, w.sid).Sequences[0].Report != "supernova update" {
		t.Fatal("the report should still be pending")
	}
	l.Unlock()
	if ctx := dig(promptHook(w, "what is next"), "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, "supernova update") {
		t.Errorf("a later prompt should deliver the report: %q", ctx)
	}
}

func TestGoApprovesNoSequenceWhenTheSequenceLockIsHeld(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	holdLock(t, seqPath(w.bridge, w.sid)+".lock")
	ctx := dig(promptHook(w, "go"), "hookSpecificOutput", "additionalContext")
	if !strings.Contains(ctx, "the sequence is busy") || !strings.Contains(ctx, "approved nothing") {
		t.Errorf("go should say the sequence is busy: %q", ctx)
	}
	if readSeq(w.bridge, w.sid).Sequences[0].Approved {
		t.Error("the sequence should not be approved")
	}
}
