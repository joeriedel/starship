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

func TestJoinFileNames(t *testing.T) {
	for _, c := range []struct {
		name  string
		names []string
		want  string
	}{
		{"ordinary", []string{"a.txt"}, "a.txt"},
		{"space", []string{"old nova.txt"}, "old nova.txt"},
		{"non-ascii", []string{"ørsted.txt"}, "ørsted.txt"},
		{"tab", []string{"tab\there.txt"}, "tab\there.txt"},
		{"mid quote", []string{"a\"q\".txt"}, "a\"q\".txt"},
		{"comma", []string{"a,b.txt"}, `"a,b.txt"`},
		{"newline", []string{"a\nb.txt"}, `"a\nb.txt"`},
		{"cr", []string{"a\rb.txt"}, `"a\rb.txt"`},
		{"leading quote", []string{"\"q\".txt"}, `"\"q\".txt"`},
		{"mix", []string{"a.txt", "b,c.txt", "d e.txt", "f\ng.txt"}, `a.txt, "b,c.txt", d e.txt, "f\ng.txt"`},
		{"empty", nil, ""},
	} {
		if got := joinFileNames(c.names); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRunEntryQuotesCommaName(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	head := runGit(t, ws, "rev-parse", "HEAD")
	write(t, filepath.Join(ws, "a,b.txt"), "c\n")
	want := ` Changed: "a,b.txt".`
	if got := runEntryChanged(t, ws, head); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestRunEntryQuotesNewlineName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("newline is not allowed in Windows file names")
	}
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	head := runGit(t, ws, "rev-parse", "HEAD")
	write(t, filepath.Join(ws, "a\nb.txt"), "c\n")
	want := ` Changed: "a\nb.txt".`
	got := runEntryChanged(t, ws, head)
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("entry contains a newline: %q", got)
	}
}
