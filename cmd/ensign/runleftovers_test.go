// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// plantBadRun writes a run folder of the auth task with the given status and reason and a pipeline.json of the given kind: "good", "missing", "unparseable" or "budgets" (parses, but canonicalizeBudgets refuses it).
func plantBadRun(t *testing.T, w *world, id, status, reason, kind string, finished int64) string {
	t.Helper()
	dir := filepath.Join(runsDir(w.bridge), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), State{Run: id, Status: status, Reason: reason, FinishedAt: finished}); err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{Run: id, Task: authTask}
	switch kind {
	case "unparseable":
		write(t, filepath.Join(dir, "pipeline.json"), "{not json")
		return dir
	case "missing":
		return dir
	case "budgets":
		snap.Pipeline = &Pipeline{Budgets: map[string]int{"loop": 1, "coder-loop": 2}}
	}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), snap); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHookIgnoresAFinishedRunWithABadPipeline(t *testing.T) {
	const id = "20240101-000001-supernova-auth-refactor"
	for _, kind := range []string{"missing", "unparseable", "budgets"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			plantBadRun(t, w, id, "error", "tests failed", kind, 1)
			if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, w.sid)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeSessionRuns(w.bridge, w.sid, []string{id}); err != nil {
				t.Fatal(err)
			}
			if res, code := w.hook("UserPromptSubmit", map[string]any{"prompt": "hello"}); code != 0 {
				t.Errorf("a finished run with a bad pipeline is no run: exit %d %v", code, res)
			}
		})
	}
	t.Run("live", func(t *testing.T) {
		w := newWorld(t)
		plantBadRun(t, w, id, "running", "", "unparseable", 0)
		if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, w.sid)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeSessionRuns(w.bridge, w.sid, []string{id}); err != nil {
			t.Fatal(err)
		}
		if res, code := w.hook("UserPromptSubmit", map[string]any{"prompt": "hello"}); code != 2 {
			t.Errorf("a live run with a bad pipeline must still fail the hook: exit %d %v", code, res)
		}
	})
}

func TestGCRemovesFinishedRunsWithABadPipelineBeyondKeep(t *testing.T) {
	w := newWorld(t)
	var dirs []string
	for i, c := range []struct{ status, reason, kind string }{
		{"success", "", "missing"},
		{"success", "", "unparseable"},
		{"success", "", "budgets"},
		{"error", "tests failed", "unparseable"},
		{"success", "", "unparseable"},
		{"error", "tests failed", "budgets"},
	} {
		dirs = append(dirs, plantBadRun(t, w, "20240101-00000"+string(rune('1'+i))+"-supernova-auth-refactor", c.status, c.reason, c.kind, 1))
	}
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 2, 0) })
	if res.Removed != 4 || res.failed() || stderr != "" {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
	for i, d := range dirs {
		if got := exists(d); got != (i >= 4) {
			t.Errorf("run %d: exists is %v", i+1, got)
		}
	}
}

func TestGCReportsARunWhoseLockCannotBeTaken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop creating the lock file on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	old := fabricate(t, w, "20240101-000001-supernova", "success", true)
	fabricate(t, w, "20240101-000002-supernova", "success", true)
	if err := os.Chmod(old, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(old, 0o755) })
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 1) })
	if res.OpenFailed != 1 || !res.failed() || res.Failed != 0 || res.Removed != 0 || !exists(old) || !strings.Contains(stderr, "warning: could not open run folder "+old) {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
	out, err := runGC(t, w, "1")
	if err == nil || !strings.Contains(out, ", could not open 1 old runs") {
		t.Errorf("err = %v, output = %q", err, out)
	}
	if show := sessionStartLine(t, w); !strings.Contains(show, " · could not open 1 old runs") {
		t.Errorf("start line: %q", show)
	}
}

const clearHeldID = "20240101-000001-supernova-auth-refactor"

// clearHeldWorld plants a cleared run of the auth task beyond keep 1 and a newer run of another task that fills keep.
func clearHeldWorld(t *testing.T, kind string) (*world, string) {
	t.Helper()
	w := newWorld(t)
	dir := plantBadRun(t, w, clearHeldID, "error", clearedReason, kind, 200)
	other := plantRun(t, w.bridge, "20240101-000009-supernova-other", "success")
	write(t, filepath.Join(other, "scratch", "x"), "x")
	return w, dir
}

func (w *world) plantPass(t *testing.T, id string, finished int64) {
	t.Helper()
	plantBadRun(t, w, id, "success", "", "good", finished)
}

func (w *world) mark(t *testing.T, task, location string) {
	t.Helper()
	write(t, testedFile(w.bridge, task, location), "abc\n")
}

func TestGCHoldsAClearedRunWhileAMarkerNeedsIt(t *testing.T) {
	for _, kind := range []string{"unparseable", "missing", "budgets", "good"} {
		for _, pass := range []string{"earlier pass", "no pass"} {
			t.Run(kind+"/"+pass, func(t *testing.T) {
				w, dir := clearHeldWorld(t, kind)
				if pass == "earlier pass" {
					w.plantPass(t, "20240101-000008-supernova-auth-refactor", 100)
				}
				w.mark(t, authTask, "")
				var res gcResult
				stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1+btoi(pass == "earlier pass"), 0) })
				if !exists(dir) || res.Removed != 0 || res.failed() || res.OpenFailed != 0 || stderr != "" {
					t.Errorf("res = %+v, stderr = %q, exists = %v", res, stderr, exists(dir))
				}
				if err := latestRunPassed(w.bridge, authTask, ""); err == nil {
					t.Errorf("landing should still refuse: %v", err)
				}
			})
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestGCRemovesAClearedRunWhenNoMarkerNeedsIt(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, w *world)
	}{
		{"later pass", func(t *testing.T, w *world) {
			w.plantPass(t, "20240101-000008-supernova-auth-refactor", 300)
			w.mark(t, authTask, "")
		}},
		{"marker gone", func(t *testing.T, w *world) {}},
		{"marker of another task", func(t *testing.T, w *world) { w.mark(t, "supernova/other", "") }},
		{"marker of another location", func(t *testing.T, w *world) {
			dir := filepath.Join(runsDir(w.bridge), clearHeldID)
			writeJSON(filepath.Join(dir, "pipeline.json"), Snapshot{Run: clearHeldID, Task: authTask, Location: "server"})
			w.mark(t, authTask, "client")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, dir := clearHeldWorld(t, "good")
			c.setup(t, w)
			keep := 1 + btoi(c.name == "later pass")
			var res gcResult
			stderr := captureStderr(t, func() { res = gcRuns(w.bridge, keep, 0) })
			if exists(dir) || res.Removed != 1 || res.failed() || stderr != "" {
				t.Errorf("res = %+v, stderr = %q, exists = %v", res, stderr, exists(dir))
			}
		})
	}
}

func TestGCRemovesAFailedRunThatWasNotCleared(t *testing.T) {
	w, _ := clearHeldWorld(t, "good")
	dir := plantBadRun(t, w, "20240101-000000-supernova-auth-refactor", "error", "tests failed", "unparseable", 200)
	w.mark(t, authTask, "")
	res := gcRuns(w.bridge, 2, 0)
	if exists(dir) || res.Removed != 1 || res.failed() {
		t.Errorf("res = %+v, exists = %v", res, exists(dir))
	}
}

func TestGCKeepsAClearedRunWhenItCannotCheckTheMarkers(t *testing.T) {
	for _, fail := range []string{"scan", "tasks folder"} {
		t.Run(fail, func(t *testing.T) {
			w, dir := clearHeldWorld(t, "unparseable")
			w.mark(t, authTask, "")
			if fail == "scan" {
				live := plantRun(t, w.bridge, "20240101-000005-supernova-live", "running")
				write(t, filepath.Join(live, "state.json"), "{not json")
			} else {
				tasks := filepath.Join(w.bridge, ".starship", "tasks")
				os.RemoveAll(tasks)
				write(t, tasks, "x")
			}
			var res gcResult
			stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
			if !exists(dir) || res.OpenFailed != 1 || !res.failed() || !strings.Contains(stderr, "warning: could not open run folder "+dir) {
				t.Errorf("res = %+v, stderr = %q, exists = %v", res, stderr, exists(dir))
			}
		})
	}
}

func TestListingFailureStopsLocationAndSkipChecks(t *testing.T) {
	w := newWorld(t)
	dir := plantRun(t, w.bridge, "20240101-000001-supernova-live", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	if _, err := latestRunLocation(w.bridge, authTask, true); err == nil {
		t.Error("latestRunLocation should report the failed listing")
	}
	if _, err := runLocation(w.bridge, authTask, ""); err == nil {
		t.Error("runLocation should report the failed listing")
	}
	if loc, err := runLocation(w.bridge, authTask, "client"); loc != "client" || err != nil {
		t.Errorf("a location asked for needs no listing: %q, %v", loc, err)
	}
	skipped := &seqStep{N: 1, Kind: "run", Task: authTask, Status: "failed", Reason: skipPrefix + "broken", Earlier: []string{""}, SkippedAt: time.Now().UnixNano()}
	pub := &seqStep{N: 2, Kind: "publish", Task: authTask, Status: "pending"}
	seq := &sequence{N: 1, Steps: []*seqStep{skipped, pub}}
	sf := &seqFile{Sequences: []*sequence{seq}}
	if got, err := sf.skippedBefore(w.bridge, seq, pub); err == nil || got != nil {
		t.Errorf("skippedBefore should return the listing error: %v, %v", got, err)
	}
}

func TestSequenceKeepGoingPublishFailsWhenTheRunListingFails(t *testing.T) {
	w := newWorld(t)
	w.mustSeq("run", authTask)
	w.mustSeq("mode", "keep-going")
	w.approve()
	w.spawn("", "", "orchestrator")
	if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), w.step(1).Run)); err != nil {
		t.Fatal(err)
	}
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error { sf.reconcile(w.bridge); return nil }); err != nil {
		t.Fatal(err)
	}
	if st := w.step(1); st.SkippedAt == 0 {
		t.Fatalf("the lost run's step should be skipped: %+v", st)
	}
	dir := plantRun(t, w.bridge, "20240101-000001-supernova-live", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	pub := w.laterPublish(authTask)
	if pub.Status != "failed" {
		t.Errorf("a publish whose skip check can't list the runs is %s, want failed", pub.Status)
	}
}

func TestGCKeepsAClearedRunWhenAPlainFileReadsAsNotExist(t *testing.T) {
	w, dir := clearHeldWorld(t, "unparseable")
	w.mark(t, authTask, "")
	tasks := filepath.Join(w.bridge, ".starship", "tasks")
	os.RemoveAll(tasks)
	write(t, tasks, "x")
	failSeam(t, &gcReadDir, func(p string) ([]os.DirEntry, error) {
		if p == tasks {
			return nil, fs.ErrNotExist
		}
		return os.ReadDir(p)
	})
	var res gcResult
	captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
	if !exists(dir) || res.OpenFailed != 1 {
		t.Errorf("res = %+v, exists = %v", res, exists(dir))
	}
}

func TestGCRemovesAClearedRunWhenTheTasksFolderIsMissing(t *testing.T) {
	w, dir := clearHeldWorld(t, "unparseable")
	os.RemoveAll(filepath.Join(w.bridge, ".starship", "tasks"))
	var res gcResult
	captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
	if exists(dir) || res.Removed != 1 || res.failed() {
		t.Errorf("res = %+v, exists = %v", res, exists(dir))
	}
}
