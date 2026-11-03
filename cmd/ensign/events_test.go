package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestJournalPathDotDotNames(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "supernova")
	for abs, want := range map[string]string{
		filepath.Join(ws, "..notes.md"):    "..notes.md",
		filepath.Join(ws, "..hidden", "x"): "..hidden/x",
	} {
		if got := journalPath(ws, abs); got != want {
			t.Errorf("journalPath(%q) = %q, want %q", abs, got, want)
		}
	}
	outside := filepath.Join(filepath.Dir(ws), "sibling", "x")
	if got, want := journalPath(ws, outside), filepath.ToSlash(outside); got != want {
		t.Errorf("outside path = %q, want %q", got, want)
	}
}

func runEntryChanged(t *testing.T, ws, head string) string {
	t.Helper()
	r := &RunFiles{Snap: &Snapshot{Workspace: ws, Pipeline: &Pipeline{Name: "x"}}, State: &State{StartHead: head}}
	return runEntry(r)
}

func TestRunEntryListsAwkwardNames(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	write(t, filepath.Join(ws, "old nova.txt"), "a\n")
	write(t, filepath.Join(ws, "ørsted.txt"), "a\n")
	write(t, filepath.Join(ws, "..notes.md"), "a\n")
	runGit(t, ws, "add", ".")
	runGit(t, ws, "commit", "-q", "-m", "more")
	head := runGit(t, ws, "rev-parse", "HEAD")
	write(t, filepath.Join(ws, "old nova.txt"), "b\n")
	write(t, filepath.Join(ws, "ørsted.txt"), "b\n")
	write(t, filepath.Join(ws, "..notes.md"), "b\n")
	write(t, filepath.Join(ws, "new nova.txt"), "c\n")
	write(t, filepath.Join(ws, "æther.txt"), "c\n")
	want := " Changed: ..notes.md, new nova.txt, old nova.txt, æther.txt, ørsted.txt."
	if got := runEntryChanged(t, ws, head); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestRunEntryListsNamesWithQuotesAndTabs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("quote and tab are not allowed in Windows file names")
	}
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	write(t, filepath.Join(ws, "tracked \"q\".txt"), "a\n")
	runGit(t, ws, "add", ".")
	runGit(t, ws, "commit", "-q", "-m", "more")
	head := runGit(t, ws, "rev-parse", "HEAD")
	write(t, filepath.Join(ws, "tracked \"q\".txt"), "b\n")
	write(t, filepath.Join(ws, "tab\there.txt"), "c\n")
	want := " Changed: tab\there.txt, tracked \"q\".txt."
	if got := runEntryChanged(t, ws, head); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}
