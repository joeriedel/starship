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

// runEntryReported is runEntry for a run whose one dispatch reported these files in its report text.
func runEntryReported(t *testing.T, ws, head, filesLine string) string {
	t.Helper()
	stage := &Stage{Outcomes: Outcomes{{Name: "compiled"}}}
	_, _, fields, problem := parseReport("```starship-report\noutcome: compiled\nfiles: "+filesLine+"\n```", stage)
	if problem != "" {
		t.Fatal(problem)
	}
	r := &RunFiles{Snap: &Snapshot{Workspace: ws, Pipeline: &Pipeline{Name: "x"}}, State: &State{StartHead: head, Dispatches: []*Dispatch{{Role: "coder", Done: true, Fields: fields}}}}
	return runEntry(r)
}

func TestRunEntryReportedCommaNameNotSplit(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	head := runGit(t, ws, "rev-parse", "HEAD")
	write(t, filepath.Join(ws, "a,b.txt"), "c\n")
	want := ` Changed: "a,b.txt".`
	if got := runEntryReported(t, ws, head, "a,b.txt"); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestRunEntryReportedDeletedFileStaysListed(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	write(t, filepath.Join(ws, "old.txt"), "a\n")
	runGit(t, ws, "add", ".")
	runGit(t, ws, "commit", "-q", "-m", "more")
	head := runGit(t, ws, "rev-parse", "HEAD")
	runGit(t, ws, "rm", "-q", "old.txt")
	runGit(t, ws, "commit", "-q", "-m", "drop")
	want := " Changed: old.txt."
	if got := runEntryReported(t, ws, head, "old.txt"); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestRunEntryReportedRenamedFileBothNamesStayListed(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	write(t, filepath.Join(ws, "nova.txt"), strings.Repeat("a star collapses under its own weight\n", 20))
	runGit(t, ws, "add", ".")
	runGit(t, ws, "commit", "-q", "-m", "more")
	head := runGit(t, ws, "rev-parse", "HEAD")
	runGit(t, ws, "mv", "nova.txt", "remnant.txt")
	runGit(t, ws, "commit", "-q", "-m", "rename")
	got := runEntryReported(t, ws, head, "nova.txt, remnant.txt")
	for _, name := range []string{"nova.txt", "remnant.txt"} {
		if !strings.Contains(got, name) {
			t.Errorf("got %q, want it to contain %q", got, name)
		}
	}
}

func TestRunEntryReportedIgnoredFileStaysListed(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	write(t, filepath.Join(ws, ".gitignore"), "*.log\n")
	runGit(t, ws, "add", ".")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	head := runGit(t, ws, "rev-parse", "HEAD")
	write(t, filepath.Join(ws, "nova.log"), "a\n")
	want := " Changed: nova.log."
	if got := runEntryReported(t, ws, head, "nova.log"); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestRunEntryReportedMissingNameKeptWithoutGit(t *testing.T) {
	needGit(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	want := " Changed: a, b.txt."
	if got := runEntryReported(t, ws, "", "a,b.txt"); !strings.Contains(got, want) {
		t.Errorf("no StartHead: got %q, want it to contain %q", got, want)
	}
	if got := runEntryReported(t, ws, "0123456789abcdef0123456789abcdef01234567", "a,b.txt"); !strings.Contains(got, want) {
		t.Errorf("bad StartHead: got %q, want it to contain %q", got, want)
	}
}

func TestWorkspaceRelative(t *testing.T) {
	for _, c := range []struct{ prefix, name, want string }{
		{"", "a/b.txt", "a/b.txt"},
		{"sub/", "sub/a.txt", "a.txt"},
		{"sub/", "sub/d/a.txt", "d/a.txt"},
		{"sub/", "top.txt", "../top.txt"},
		{"sub/", "subway/x", "../subway/x"},
		{"sub/", "other/x", "../other/x"},
		{"a/b/", "a/b/c.txt", "c.txt"},
		{"a/b/", "a/c.txt", "../c.txt"},
		{"a/b/", "a/bc/c.txt", "../bc/c.txt"},
		{"a/b/", "top.txt", "../../top.txt"},
		{"a/b/", "x/y.txt", "../../x/y.txt"},
	} {
		if got := workspaceRelative(c.prefix, c.name); got != c.want {
			t.Errorf("workspaceRelative(%q, %q) = %q, want %q", c.prefix, c.name, got, c.want)
		}
	}
}

func runEntrySubfolder(t *testing.T, relative bool) {
	needGit(t)
	repo := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	ws := filepath.Join(repo, "sub")
	write(t, filepath.Join(ws, "a.txt"), "a\n")
	write(t, filepath.Join(ws, "gone.txt"), "a\n")
	write(t, filepath.Join(repo, "top.txt"), "a\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "more")
	head := runGit(t, repo, "rev-parse", "HEAD")
	if relative {
		runGit(t, repo, "config", "diff.relative", "true")
	}
	write(t, filepath.Join(ws, "a.txt"), "b\n")
	write(t, filepath.Join(repo, "top.txt"), "b\n")
	write(t, filepath.Join(ws, "new.txt"), "c\n")
	runGit(t, repo, "rm", "-q", "sub/gone.txt")
	runGit(t, repo, "commit", "-q", "-am", "drop")
	want := " Changed: ../top.txt, a.txt, gone.txt, new.txt."
	if got := runEntryReported(t, ws, head, "gone.txt, a.txt"); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestRunEntrySubfolderWorkspaceListsWorkspaceRelativeNames(t *testing.T) {
	runEntrySubfolder(t, false)
}

func TestRunEntrySubfolderWorkspaceIgnoresDiffRelativeConfig(t *testing.T) {
	runEntrySubfolder(t, true)
}
