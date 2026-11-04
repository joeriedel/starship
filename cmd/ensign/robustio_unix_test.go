//go:build !windows

package main

import (
	"errors"
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
