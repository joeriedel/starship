//go:build windows

package main

import (
	"errors"
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
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(state, data, 0o644)
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
