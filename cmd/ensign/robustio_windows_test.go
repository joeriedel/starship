// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// hold opens path with share mode 0 and releases it after 100ms.
func hold(t *testing.T, path string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		windows.CloseHandle(h)
	}()
}

func TestWriteJSONSucceedsWhileTargetHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeJSON(path, &State{Run: "supernova"}); err != nil {
		t.Fatal(err)
	}
	hold(t, path)
	if err := writeJSON(path, &State{Run: "supernova-2"}); err != nil {
		t.Fatal(err)
	}
	var got State
	if err := readJSON(path, &got); err != nil || got.Run != "supernova-2" {
		t.Fatalf("want the new state, got %+v err=%v", got, err)
	}
}

func TestReadJSONSucceedsWhileTargetHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeJSON(path, &State{Run: "supernova"}); err != nil {
		t.Fatal(err)
	}
	hold(t, path)
	var got State
	if err := readJSON(path, &got); err != nil || got.Run != "supernova" {
		t.Fatalf("want the state, got %+v err=%v", got, err)
	}
}

func TestReadRunSucceedsWhileStateHeld(t *testing.T) {
	w := newWorld(t)
	dir := fabricate(t, w, "20240101-000001-supernova", "running", true)
	hold(t, filepath.Join(dir, "state.json"))
	if _, err := readRun(w.bridge, "20240101-000001-supernova"); err != nil {
		t.Fatal(err)
	}
}

func TestIsTransientFileError(t *testing.T) {
	for _, e := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION} {
		if !isTransientFileError(&os.PathError{Op: "open", Path: "x", Err: e}) || !isTransientFileError(&os.LinkError{Op: "rename", Old: "a", New: "b", Err: e}) {
			t.Errorf("%v should be transient", e)
		}
	}
	if isTransientFileError(&os.PathError{Op: "open", Path: "x", Err: windows.ERROR_FILE_NOT_FOUND}) {
		t.Error("ERROR_FILE_NOT_FOUND must not be transient")
	}
}

func TestReadJSONMissingFileFailsFast(t *testing.T) {
	start := time.Now()
	err := readJSON(filepath.Join(t.TempDir(), "absent.json"), &State{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if time.Since(start) > retryWindow/2 {
		t.Fatalf("missing file took %v", time.Since(start))
	}
}

func TestIsTransientExistingFileError(t *testing.T) {
	for _, e := range []error{windows.ERROR_FILE_NOT_FOUND, windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION} {
		if !isTransientExistingFileError(&os.PathError{Op: "open", Path: "x", Err: e}) {
			t.Errorf("%v should be transient", e)
		}
	}
	if isTransientExistingFileError(&os.PathError{Op: "open", Path: "x", Err: windows.ERROR_PATH_NOT_FOUND}) {
		t.Error("ERROR_PATH_NOT_FOUND must not be transient")
	}
}

func TestReadRunSucceedsWhileStateAbsent(t *testing.T) {
	w := newWorld(t)
	id := "20240101-000001-supernova"
	state := filepath.Join(fabricate(t, w, id, "running", true), "state.json")
	var st State
	if err := readJSON(state, &st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeJSON(state, &st)
	}()
	if _, err := readRun(w.bridge, id); err != nil {
		t.Fatal(err)
	}
}

func TestReadRunMissingPipelineFailsFast(t *testing.T) {
	w := newWorld(t)
	id := "20240101-000001-supernova"
	dir := fabricate(t, w, id, "running", true)
	if err := os.Remove(filepath.Join(dir, "pipeline.json")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := readRun(w.bridge, id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if time.Since(start) > retryWindow/2 {
		t.Fatalf("missing pipeline took %v", time.Since(start))
	}
}

func TestLiveSessionRunsSucceedsWhileStateAbsent(t *testing.T) {
	w := newWorld(t)
	id := "20240101-000001-supernova"
	state := filepath.Join(fabricate(t, w, id, "running", true), "state.json")
	if err := bindSession(w.bridge, "s1", id); err != nil {
		t.Fatal(err)
	}
	var st State
	if err := readJSON(state, &st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeJSON(state, &st)
	}()
	ids, err := liveSessionRuns(w.bridge, "s1")
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("want the run kept, got %v err=%v", ids, err)
	}
}

func TestBindSessionSucceedsWhileStateAbsent(t *testing.T) {
	w := newWorld(t)
	first := "20240101-000001-supernova"
	second := "20240101-000002-supernova"
	state := filepath.Join(fabricate(t, w, first, "running", true), "state.json")
	fabricate(t, w, second, "running", true)
	if err := bindSession(w.bridge, "s1", first); err != nil {
		t.Fatal(err)
	}
	var st State
	if err := readJSON(state, &st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeJSON(state, &st)
	}()
	if err := bindSession(w.bridge, "s1", second); err != nil {
		t.Fatal(err)
	}
	ids, err := sessionRuns(w.bridge, "s1")
	if err != nil || len(ids) != 2 || ids[0] != first {
		t.Fatalf("want both runs bound, got %v err=%v", ids, err)
	}
}

func TestLiveSessionRunsFailsFastPastDeletedRun(t *testing.T) {
	w := newWorld(t)
	id := "20240101-000001-supernova"
	dir := fabricate(t, w, id, "running", true)
	if err := bindSession(w.bridge, "s1", id); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ids, err := liveSessionRuns(w.bridge, "s1")
	if err != nil || len(ids) != 0 {
		t.Fatalf("want no live runs, got %v err=%v", ids, err)
	}
	if time.Since(start) > retryWindow/2 {
		t.Fatalf("deleted run took %v", time.Since(start))
	}
}

func TestSessionRunsSucceedsWhileSessionFileHeld(t *testing.T) {
	w := newWorld(t)
	id := "20240101-000001-supernova"
	fabricate(t, w, id, "running", true)
	if err := bindSession(w.bridge, "s1", id); err != nil {
		t.Fatal(err)
	}
	hold(t, sessionFile(w.bridge, "s1"))
	ids, err := sessionRuns(w.bridge, "s1")
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("want the bound run, got %v err=%v", ids, err)
	}
}

func TestWriteSessionRunsSucceedsWhileSessionFileHeld(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, "s1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionRuns(w.bridge, "s1", []string{"20240101-000001-supernova"}); err != nil {
		t.Fatal(err)
	}
	hold(t, sessionFile(w.bridge, "s1"))
	if err := writeSessionRuns(w.bridge, "s1", []string{"20240101-000002-supernova"}); err != nil {
		t.Fatal(err)
	}
	ids, err := sessionRuns(w.bridge, "s1")
	if err != nil || len(ids) != 1 || ids[0] != "20240101-000002-supernova" {
		t.Fatalf("want the new run bound, got %v err=%v", ids, err)
	}
}

func TestWriteSessionRunsEmptySucceedsWhileSessionFileHeld(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, "s1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionRuns(w.bridge, "s1", []string{"20240101-000001-supernova"}); err != nil {
		t.Fatal(err)
	}
	hold(t, sessionFile(w.bridge, "s1"))
	if err := writeSessionRuns(w.bridge, "s1", nil); err != nil {
		t.Fatal(err)
	}
	if exists(sessionFile(w.bridge, "s1")) {
		t.Fatal("want the session file removed")
	}
}

func TestRemoveRunSucceedsWhilePipelineHeld(t *testing.T) {
	w := newWorld(t)
	dir := fabricate(t, w, "20240101-000001-supernova", "error", true)
	hold(t, filepath.Join(dir, "pipeline.json"))
	removeRun(dir)
	if exists(dir) {
		t.Fatal("want the run folder removed")
	}
}

func TestGCReclaimsPipelineOnlyFolderFast(t *testing.T) {
	w := newWorld(t)
	dir := pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
	start := time.Now()
	res := gcRuns(w.bridge, 500, 0)
	if time.Since(start) > retryWindow/2 {
		t.Fatalf("reclaiming took %v", time.Since(start))
	}
	if exists(dir) || res.Removed != 1 {
		t.Fatalf("the pipeline-only folder must be reclaimed: %+v", res)
	}
}

func TestEmptyScratchSucceedsWhileItemHeld(t *testing.T) {
	w := newWorld(t)
	dir := fabricate(t, w, "20240101-000001-supernova", "success", true)
	hold(t, filepath.Join(dir, "scratch", "note.txt"))
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
	if res.ScratchFailed != 0 || res.Scratch != 1 || stderr != "" {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
}

func TestEmptyScratchDeadlineIsShared(t *testing.T) {
	w := newWorld(t)
	const kept = 4
	for i := 1; i <= kept; i++ {
		dir := fabricate(t, w, fmt.Sprintf("2024010%d-000001-supernova", i), "success", true)
		p, err := windows.UTF16PtrFromString(filepath.Join(dir, "scratch", "note.txt"))
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { windows.CloseHandle(h) })
	}
	start := time.Now()
	var res gcResult
	captureStderr(t, func() { res = gcRuns(w.bridge, kept, 0) })
	if res.ScratchFailed != kept {
		t.Errorf("res = %+v, want %d scratch failed", res, kept)
	}
	if took := time.Since(start); took >= 3*retryWindow {
		t.Errorf("the pass took %v; the retry window must be shared", took)
	}
}

func TestGCRunDeletesShareTheRetryWindow(t *testing.T) {
	w := newWorld(t)
	const stuck = 4
	for i := 1; i <= stuck; i++ {
		dir := fabricate(t, w, fmt.Sprintf("2024010%d-000001-supernova", i), "success", true)
		p, err := windows.UTF16PtrFromString(filepath.Join(dir, "scratch", "note.txt"))
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { windows.CloseHandle(h) })
	}
	start := time.Now()
	var res gcResult
	captureStderr(t, func() { res = gcRuns(w.bridge, 0, 0) })
	if res.Failed != stuck {
		t.Errorf("res = %+v, want %d failed deletes", res, stuck)
	}
	if took := time.Since(start); took >= 3*retryWindow {
		t.Errorf("the pass took %v; the failed deletes must share one retry budget", took)
	}
}
