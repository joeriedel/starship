// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func setRenameAside(t *testing.T, v bool) {
	old := renameAside
	renameAside = v
	t.Cleanup(func() { renameAside = old })
}

func readString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReplaceFileRenamesTheOldExecutableAside(t *testing.T) {
	setRenameAside(t, true)
	p := filepath.Join(t.TempDir(), "supernova.exe")
	os.WriteFile(p, []byte("old"), 0o755)
	if err := replaceFile(p, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, p); got != "new" {
		t.Fatalf("p = %q, want new", got)
	}
	if got := readString(t, p+".old"); got != "old" {
		t.Fatalf(".old = %q, want old", got)
	}
	if exists(p + ".new") {
		t.Fatal(".new left behind")
	}
}

func TestReplaceFileReplacesAStaleOld(t *testing.T) {
	setRenameAside(t, true)
	p := filepath.Join(t.TempDir(), "supernova.exe")
	os.WriteFile(p, []byte("old"), 0o755)
	os.WriteFile(p+".old", []byte("stale"), 0o755)
	if err := replaceFile(p, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, p+".old"); got != "old" {
		t.Fatalf(".old = %q, want old", got)
	}
	if got := readString(t, p); got != "new" {
		t.Fatalf("p = %q, want new", got)
	}
}

func TestReplaceFileWithNothingToReplaceLeavesNoOld(t *testing.T) {
	setRenameAside(t, true)
	p := filepath.Join(t.TempDir(), "supernova.exe")
	if err := replaceFile(p, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, p); got != "new" {
		t.Fatalf("p = %q, want new", got)
	}
	if exists(p + ".old") {
		t.Fatal(".old created with nothing to replace")
	}
}

func TestReplaceFileKeepsNoOldWithoutRenameAside(t *testing.T) {
	setRenameAside(t, false)
	p := filepath.Join(t.TempDir(), "supernova.exe")
	os.WriteFile(p, []byte("old"), 0o755)
	if err := replaceFile(p, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if exists(p + ".old") {
		t.Fatal(".old created without renameAside")
	}
	if got := readString(t, p); got != "new" {
		t.Fatalf("p = %q, want new", got)
	}
}

func TestReplaceFileLeavesDataFilesAlone(t *testing.T) {
	setRenameAside(t, true)
	p := filepath.Join(t.TempDir(), "supernova.txt")
	os.WriteFile(p, []byte("old"), 0o644)
	if err := replaceFile(p, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	if exists(p + ".old") {
		t.Fatal(".old created for a non-executable")
	}
}

func TestRemoveOldExe(t *testing.T) {
	bridge := t.TempDir()
	removeOldExe(bridge)
	p := filepath.Join(bridge, filepath.FromSlash(ensignFile()))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("cur"), 0o755)
	os.WriteFile(p+".old", []byte("old"), 0o755)
	removeOldExe(bridge)
	if exists(p + ".old") {
		t.Fatal(".old not removed")
	}
	if !exists(p) {
		t.Fatal("current ensign removed")
	}
}
