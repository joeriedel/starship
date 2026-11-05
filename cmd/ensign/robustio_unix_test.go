//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
