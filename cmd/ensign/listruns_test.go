// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func (w *world) listRuns() []runInfo {
	w.t.Helper()
	runs, err := listRuns(w.bridge)
	if err != nil {
		w.t.Fatal(err)
	}
	return runs
}

func (w *world) liveRuns() []runInfo {
	w.t.Helper()
	runs, err := liveRuns(w.bridge)
	if err != nil {
		w.t.Fatal(err)
	}
	return runs
}

func TestListRunsMissingFolderAndPlainFile(t *testing.T) {
	w := newWorld(t)
	dir := runsDir(w.bridge)
	check := func(what string) {
		t.Helper()
		if runs, err := listRuns(w.bridge); len(runs) != 0 || err != nil {
			t.Errorf("%s: listRuns = %v, %v", what, runs, err)
		}
		if runs, err := liveRuns(w.bridge); len(runs) != 0 || err != nil {
			t.Errorf("%s: liveRuns = %v, %v", what, runs, err)
		}
	}
	os.RemoveAll(dir)
	check("missing folder")
	write(t, dir, "not a folder")
	check("plain file")
	os.RemoveAll(filepath.Dir(dir))
	write(t, filepath.Dir(dir), "not a folder")
	check("plain file above")
}
