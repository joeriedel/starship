// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadRunMissingStateFailsFast(t *testing.T) {
	w := newWorld(t)
	id := "20240101-000001-supernova"
	dir := fabricate(t, w, id, "running", true)
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := readRun(w.bridge, id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if time.Since(start) > retryWindow/2 {
		t.Fatalf("missing state took %v", time.Since(start))
	}
}

func TestGCUndeletableRunsUseUpCap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	var stuck, free []string
	for i := 1; i <= 4; i++ {
		dir := fabricate(t, w, fmt.Sprintf("20240101-00000%d-supernova", i), "success", true)
		if i <= 2 {
			scratch := filepath.Join(dir, "scratch")
			if err := os.Chmod(scratch, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(scratch, 0o755) })
			stuck = append(stuck, dir)
		} else {
			free = append(free, dir)
		}
	}
	res := gcRuns(w.bridge, 0, 2)
	if res.Removed != 0 || !res.More {
		t.Fatalf("failed deletes must use the cap: %+v", res)
	}
	for _, d := range append(stuck, free...) {
		if !exists(d) {
			t.Errorf("%s must survive: the cap was spent on the older undeletable runs", d)
		}
	}
}

func TestGCFailedDeleteFallsOutOfGC(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stuck := fabricate(t, w, "20240101-000001-supernova", "success", true)
	newer := fabricate(t, w, "20240101-000002-supernova", "success", true)
	scratch := filepath.Join(stuck, "scratch")
	if err := os.Chmod(scratch, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(scratch, 0o755) })
	res := gcRuns(w.bridge, 0, 1)
	if res.Removed != 0 || !res.More {
		t.Fatalf("the failed delete must use the cap: %+v", res)
	}
	for _, f := range []string{"pipeline.json", "state.json"} {
		if exists(filepath.Join(stuck, f)) {
			t.Errorf("%s should have been removed from the stuck folder", f)
		}
	}
	if !exists(filepath.Join(scratch, "note.txt")) || !exists(newer) {
		t.Fatalf("the undeletable file and the newer run must remain")
	}
	res = gcRuns(w.bridge, 0, 1)
	if exists(newer) || res.Removed != 1 {
		t.Fatalf("the stuck folder must not use the cap again: %+v", res)
	}
}

func TestGCUnwritableRunFolderUsesCapEachStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stuck := fabricate(t, w, "20240101-000001-supernova", "success", true)
	newer := fabricate(t, w, "20240101-000002-supernova", "success", true)
	write(t, filepath.Join(stuck, "lock"), "")
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stuck, 0o755) })
	for i := 0; i < 2; i++ {
		res := gcRuns(w.bridge, 0, 1)
		if res.Removed != 0 || !res.More || !exists(newer) {
			t.Fatalf("call %d: the stuck run must use the cap: %+v", i+1, res)
		}
	}
}

func TestGCUnwritablePipelineOnlyFolderUsesCapEachStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stuck := pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
	newer := fabricate(t, w, "20240101-000002-supernova", "success", true)
	write(t, filepath.Join(stuck, "lock"), "")
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stuck, 0o755) })
	for i := 0; i < 2; i++ {
		res := gcRuns(w.bridge, 0, 1)
		if res.Removed != 0 || !res.More || !exists(newer) {
			t.Fatalf("call %d: the stuck pipeline-only folder must use the cap: %+v", i+1, res)
		}
	}
}

// stickRun makes a fabricated run's folder undeletable.
func stickRun(t *testing.T, dir string) {
	scratch := filepath.Join(dir, "scratch")
	if err := os.Chmod(scratch, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(scratch, 0o755) })
}

func TestSessionStartReportsFailedDeletes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	for i := 1; i <= 3; i++ {
		stickRun(t, fabricate(t, w, fmt.Sprintf("20240101-00000%d-supernova", i), "success", true))
	}
	fabricate(t, w, "20240101-000004-supernova", "success", true)
	show := sessionStartLine(t, w)
	if !strings.Contains(show, "could not delete 3 old runs") || strings.Contains(show, "cleaned up") {
		t.Errorf("start line: %q", show)
	}
}

func TestSessionStartPartialDeleteFailureReportsBoth(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stickRun(t, fabricate(t, w, "20240101-000001-supernova", "success", true))
	fabricate(t, w, "20240101-000002-supernova", "success", true)
	fabricate(t, w, "20240101-000003-supernova", "success", true)
	show := sessionStartLine(t, w)
	if !strings.Contains(show, "cleaned up 1 old runs") || !strings.Contains(show, "could not delete 1 old runs") {
		t.Errorf("start line: %q", show)
	}
}

func TestGCCountsFailedDeletes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stickRun(t, fabricate(t, w, "20240101-000001-supernova", "success", true))
	fabricate(t, w, "20240101-000002-supernova", "success", true)
	if res := gcRuns(w.bridge, 0, 0); res.Failed != 1 || res.Removed != 1 {
		t.Fatalf("want 1 failed and 1 removed: %+v", res)
	}
	w = newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	if res := gcRuns(w.bridge, 0, 0); res.Failed != 0 || res.Removed != 1 {
		t.Fatalf("want 0 failed: %+v", res)
	}
}

func TestGCCountsScratchFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stickRun(t, fabricate(t, w, "20240101-000001-supernova", "success", true))
	if res := gcRuns(w.bridge, 1, 0); res.ScratchFailed != 1 || res.Scratch != 0 || res.Failed != 0 {
		t.Fatalf("want 1 scratch failed only: %+v", res)
	}
}

func TestGCCountsUnreadableScratchFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	dir := fabricate(t, w, "20240101-000001-supernova", "success", true)
	scratch := filepath.Join(dir, "scratch")
	if err := os.Chmod(scratch, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(scratch, 0o755) })
	if res := gcRuns(w.bridge, 1, 0); res.ScratchFailed != 1 || res.Scratch != 0 || res.Failed != 0 {
		t.Fatalf("want 1 scratch failed only: %+v", res)
	}
}

func TestSessionStartReportsScratchFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stickRun(t, fabricate(t, w, "20240101-000001-supernova", "success", true))
	show := sessionStartLine(t, w)
	if !strings.Contains(show, "could not empty 1 scratch folders") || strings.Contains(show, "cleaned up") {
		t.Errorf("start line: %q", show)
	}
}

func TestSessionStartReportsScratchFailedAlongsideCleaned(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	stickRun(t, fabricate(t, w, "20240101-000002-supernova", "success", true))
	show := sessionStartLine(t, w)
	if !strings.Contains(show, "cleaned up 1 old runs") || !strings.Contains(show, "could not empty 1 scratch folders") {
		t.Errorf("start line: %q", show)
	}
}

func TestCmdGCReportsFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld(t)
	stickRun(t, fabricate(t, w, "20240101-000001-supernova", "success", true))
	stickRun(t, fabricate(t, w, "20240101-000002-supernova", "success", true))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdGC([]string{"1"}, &out); err == nil {
		t.Error("cmdGC must fail when something failed")
	}
	if !strings.Contains(out.String(), "could not delete 1 runs") || !strings.Contains(out.String(), "could not empty 1 scratch folders") {
		t.Errorf("output: %q", out.String())
	}
}
