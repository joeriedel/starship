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
