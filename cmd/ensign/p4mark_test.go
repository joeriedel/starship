// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// changedFiles writes the files in dir with a modification time well after any run start, as a shell command running during the run leaves them.
func changedFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	setTimes(t, dir, time.Now().Add(time.Hour), names...)
}

// setTimes writes the files in dir, if they are not there, and sets their modification time.
func setTimes(t *testing.T, dir string, at time.Time, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err != nil {
			write(t, p, "package x\n")
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// quiet moves the run's mark to now, with a pause each side so files written before it have an earlier change time and files written after it a later one, whatever the clock's granularity.
func quiet(t *testing.T, r *RunFiles) {
	t.Helper()
	time.Sleep(60 * time.Millisecond)
	r.State.ReconcileMark = time.Now().UnixNano()
	time.Sleep(60 * time.Millisecond)
}

// recordDirs writes the set of folders the run recorded when its mark was set.
func recordDirs(t *testing.T, r *RunFiles, dirs ...string) {
	t.Helper()
	write(t, reconcileDirsFile(r), strings.Join(dirs, "\n")+"\n")
}

// candidatesOf runs p4Candidates from the run's mark and returns the result as newline-framed text, for Contains checks.
func candidatesOf(t *testing.T, r *RunFiles, v TaskVcs, env []string) string {
	t.Helper()
	mark, _ := reconcileMark(r)
	got, _, err := p4Candidates(r, env, v, mark)
	if err != nil {
		t.Fatal(err)
	}
	return "\n" + strings.Join(got, "\n") + "\n"
}

// p4Direct returns a run's files for the world, with the mark at an hour ago, so calls can be made without a run.
func p4Direct(t *testing.T, w *world) (*RunFiles, TaskVcs, []string) {
	t.Helper()
	r := &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Task: "supernova/auth-refactor", Workspace: w.workspace()}, State: &State{ReconcileMark: time.Now().Add(-time.Hour).UnixNano()}}
	v := TaskVcs{Kind: "p4", Client: "supernova-ws", Changelist: "101"}
	return r, v, p4Env(r, v, "")
}

func TestP4ReconcileSeesOnlyFilesChangedSinceTheMark(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_RECONCILE", "a.go:edit b.go:add old.go:edit c.go:edit")
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	setTimes(t, ws, time.Now().Add(-2*time.Hour), "old.go")
	quiet(t, r)
	setTimes(t, ws, time.Now().Add(-30*time.Minute), "a.go", "b.go")
	before := r.State.ReconcileMark
	opened, err := p4ReconcileRun(r, env, v)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opened, "\n"); !strings.Contains(got, "edit "+filepath.Join(ws, "a.go")) || !strings.Contains(got, "add "+filepath.Join(ws, "b.go")) || len(opened) != 2 {
		t.Errorf("opened %q: want a.go and b.go, not old.go", opened)
	}
	if r.State.ReconcileMark == before {
		t.Error("the mark did not move")
	}
	if _, err := os.Stat(reconcileDirsFile(r)); err != nil {
		t.Error("the pass did not record the folders:", err)
	}
	quiet(t, r)
	changedFiles(t, ws, "c.go")
	opened, err = p4ReconcileRun(r, env, v)
	if err != nil || len(opened) != 1 || !strings.HasPrefix(opened[0], "edit ") || !strings.HasSuffix(opened[0], "c.go") {
		t.Errorf("the second pass opened %q, %v: want c.go only", opened, err)
	}
}

func TestP4ReconcileOpensAShellDeletedFile(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_RECONCILE", "gone.go:delete")
	t.Setenv("STARSHIP_TEST_P4_MISSING", "gone.go")
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	opened, err := p4ReconcileRun(r, env, v)
	if err != nil || len(opened) != 1 || !strings.HasPrefix(opened[0], "delete ") {
		t.Errorf("opened %q, %v", opened, err)
	}
}

func TestP4ReconcileWithNothingChangedRunsNoReconcile(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_RECONCILE", "a.go:edit")
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	log := filepath.Join(t.TempDir(), "p4.log")
	t.Setenv("STARSHIP_TEST_P4_LOG", log)
	setTimes(t, w.workspace(), time.Now().Add(-2*time.Hour), "a.go")
	quiet(t, r)
	before := r.State.ReconcileMark
	if opened, err := p4ReconcileRun(r, env, v); err != nil || len(opened) != 0 {
		t.Fatalf("opened %q, %v", opened, err)
	}
	if strings.Contains(p4Log(t, log), "reconcile") {
		t.Errorf("p4 reconcile ran:\n%s", p4Log(t, log))
	}
	if r.State.ReconcileMark == before {
		t.Error("the mark did not move")
	}
}

func TestP4ReconcileMarkFallsBackToTheRunStart(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_RECONCILE", "a.go:edit old.go:edit")
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	r.State.ReconcileMark = 0
	r.Snap.Created = time.Now().Add(time.Hour).Format(time.RFC3339)
	setTimes(t, w.workspace(), time.Now().Add(-2*time.Hour), "old.go")
	setTimes(t, w.workspace(), time.Now().Add(2*time.Hour), "a.go")
	opened, err := p4ReconcileRun(r, env, v)
	if err != nil || len(opened) != 1 || !strings.HasSuffix(opened[0], "a.go") {
		t.Errorf("opened %q, %v", opened, err)
	}
	r.State.ReconcileMark = 0
	r.Snap.Created = ""
	if _, err := p4ReconcileRun(r, env, v); err == nil {
		t.Error("a run with no mark and no start time should fail, not reconcile everything")
	}
}

func TestP4CandidatesSkipsVersionControlFoldersAndSymlinks(t *testing.T) {
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	for _, d := range []string{".git", ".starship", "src"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	changedFiles(t, filepath.Join(ws, ".git"), "x.go")
	changedFiles(t, filepath.Join(ws, ".starship"), "y.go")
	changedFiles(t, filepath.Join(ws, "src"), "z.go")
	outside := t.TempDir()
	changedFiles(t, outside, "w.go")
	linked := os.Symlink(outside, filepath.Join(ws, "link")) == nil
	if !linked && runtime.GOOS != "windows" {
		t.Fatal("could not make a symlink")
	}
	got := candidatesOf(t, r, v, env)
	for _, bad := range []string{"x.go", "y.go", "w.go"} {
		if strings.Contains(got, bad) {
			t.Errorf("candidates %q: %s should not be walked", got, bad)
		}
	}
	if !strings.Contains(got, "z.go") {
		t.Errorf("candidates %q lack src/z.go", got)
	}
}

func TestP4ReviewDiffIsLimitedToFilesChangedSinceTheMark(t *testing.T) {
	w := p4World(t, p4Task)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_RECONCILE", "new.go:add old.go:edit")
	r, v, _ := p4Direct(t, w)
	setTimes(t, w.workspace(), time.Now().Add(-2*time.Hour), "old.go")
	quiet(t, r)
	setTimes(t, w.workspace(), time.Now().Add(-time.Minute), "new.go")
	before := r.State.ReconcileMark
	file := filepath.Join(r.Dir, "change.diff")
	if _, err := writeP4Change(r, v, file); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "new.go") || strings.Contains(string(data), "old.go") {
		t.Errorf("the diff:\n%s", data)
	}
	if r.State.ReconcileMark != before {
		t.Error("the preview moved the mark")
	}
	if _, err := os.Stat(reconcileDirsFile(r)); err == nil {
		t.Error("the preview recorded the folders")
	}
}

func TestP4CandidatesIncludeAFileMovedInWithAnOldTime(t *testing.T) {
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	old := time.Now().Add(-5 * time.Hour)
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	setTimes(t, filepath.Join(ws, "src"), old, "kept.go")
	setTimes(t, ws, old, "top.go")
	recordDirs(t, r, "src")
	staging := t.TempDir()
	setTimes(t, staging, old, "moved.go")
	quiet(t, r)
	if err := os.Rename(filepath.Join(staging, "moved.go"), filepath.Join(ws, "src", "moved.go")); err != nil {
		t.Fatal(err)
	}
	got := candidatesOf(t, r, v, env)
	if !strings.Contains(got, "\n"+filepath.Join(ws, "src", "moved.go")+"\n") {
		t.Errorf("candidates %q lack the moved file", got)
	}
	if strings.Contains(got, "top.go") || strings.Contains(got, "kept.go") {
		t.Errorf("candidates %q include a file nothing touched", got)
	}
}

func TestP4CandidatesIncludeAFileOverwrittenKeepingItsOldTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the change time cannot be read back portably")
	}
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	old := time.Now().Add(-5 * time.Hour)
	setTimes(t, ws, old, "kept.go", "over.go")
	quiet(t, r)
	write(t, filepath.Join(ws, "over.go"), "package y\n")
	if err := os.Chtimes(filepath.Join(ws, "over.go"), old, old); err != nil {
		t.Fatal(err)
	}
	got := candidatesOf(t, r, v, env)
	if !strings.Contains(got, "\n"+filepath.Join(ws, "over.go")+"\n") || strings.Contains(got, "kept.go") {
		t.Errorf("candidates %q: want over.go only", got)
	}
}

func TestP4CandidatesDoNotSweepUnrelatedFilesInAChangedFolder(t *testing.T) {
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	for _, d := range []string{"src", "docs", "lib"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setTimes(t, filepath.Join(ws, "src"), time.Now().Add(-5*time.Hour), "untracked.go")
	setTimes(t, filepath.Join(ws, "lib"), time.Now().Add(-5*time.Hour), "other.go")
	setTimes(t, filepath.Join(ws, "docs"), time.Now().Add(-5*time.Hour), "readme.go")
	recordDirs(t, r, "src", "docs", "lib")
	quiet(t, r)
	changedFiles(t, filepath.Join(ws, "src"), "new.go")
	got := candidatesOf(t, r, v, env)
	if !strings.Contains(got, "\n"+filepath.Join(ws, "src", "new.go")+"\n") {
		t.Errorf("candidates %q lack src/new.go", got)
	}
	for _, bad := range []string{"untracked.go", "other.go", "readme.go"} {
		if strings.Contains(got, bad) {
			t.Errorf("candidates %q include the unrelated %s", got, bad)
		}
	}
}

func TestP4CandidatesPassAFolderNewSinceTheMarkWhole(t *testing.T) {
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	old := time.Now().Add(-5 * time.Hour)
	if err := os.MkdirAll(filepath.Join(ws, "known"), 0o755); err != nil {
		t.Fatal(err)
	}
	setTimes(t, filepath.Join(ws, "known"), old, "a.go")
	recordDirs(t, r, "known")
	quiet(t, r)
	if err := os.MkdirAll(filepath.Join(ws, "fresh", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	setTimes(t, filepath.Join(ws, "fresh"), old, "a.go")
	setTimes(t, filepath.Join(ws, "fresh", "sub"), old, "b.go")
	got := candidatesOf(t, r, v, env)
	if !strings.Contains(got, "\n"+filepath.Join(ws, "fresh")+"/...\n") {
		t.Errorf("candidates %q lack the new folder", got)
	}
	if strings.Contains(got, "fresh/a.go") || strings.Contains(got, "sub") || strings.Contains(got, "known") {
		t.Errorf("candidates %q: something under the new folder, or the known one, was passed", got)
	}
}

func TestP4CandidatesTreatNoFolderAsNewWithoutARecordedSet(t *testing.T) {
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	old := time.Now().Add(-5 * time.Hour)
	if err := os.MkdirAll(filepath.Join(ws, "fresh"), 0o755); err != nil {
		t.Fatal(err)
	}
	setTimes(t, filepath.Join(ws, "fresh"), old, "a.go")
	quiet(t, r)
	if got := candidatesOf(t, r, v, env); strings.Contains(got, "...") || strings.Contains(got, "a.go") {
		t.Errorf("candidates %q: want nothing", got)
	}
}

func TestRecordStartDirsRecordsTheWorkspaceFolders(t *testing.T) {
	w := p4World(t, p4Task)
	r, _, _ := p4Direct(t, w)
	ws := w.workspace()
	for _, d := range []string{"src/deep", ".git/objects"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordStartDirs(r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(reconcileDirsFile(r))
	var paths []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		_, p, _ := strings.Cut(l, "\t")
		paths = append(paths, p)
	}
	if got := strings.Join(paths, ","); got != "src,src/deep" {
		t.Errorf("recorded %q", got)
	}
	os.Remove(reconcileDirsFile(r))
	r.Snap.Bindings = map[string]Binding{"reconcile": {Command: "true"}}
	if err := recordStartDirs(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reconcileDirsFile(r)); err == nil {
		t.Error("folders were recorded for a run with a @reconcile binding")
	}
}

func TestP4CandidatesWalkAWorkspaceThatIsALink(t *testing.T) {
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	real := w.workspace()
	changedFiles(t, real, "a.go")
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("cannot make a symlink:", err)
	}
	r.Snap.Workspace = link
	if got := candidatesOf(t, r, v, env); !strings.Contains(got, "\n"+filepath.Join(link, "a.go")+"\n") {
		t.Errorf("candidates %q lack a.go under the link path", got)
	}
}

func TestP4CandidatesPassAFolderReplacedByAnotherAtTheSamePathWhole(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("folder identity is checked on linux")
	}
	w := p4World(t, p4Task)
	r, v, env := p4Direct(t, w)
	ws := w.workspace()
	old := time.Now().Add(-5 * time.Hour)
	for _, d := range []string{"lib", "lib.new", "steady"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setTimes(t, filepath.Join(ws, "lib"), old, "a.go")
	setTimes(t, filepath.Join(ws, "lib.new"), old, "b.go")
	setTimes(t, filepath.Join(ws, "steady"), old, "c.go")
	dirs, err := walkWorkspace(ws, nil, time.Time{}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReconcileDirs(r, dirs); err != nil {
		t.Fatal(err)
	}
	quiet(t, r)
	if err := os.RemoveAll(filepath.Join(ws, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(ws, "lib.new"), filepath.Join(ws, "lib")); err != nil {
		t.Fatal(err)
	}
	changedFiles(t, filepath.Join(ws, "steady"), "d.go")
	got := candidatesOf(t, r, v, env)
	if !strings.Contains(got, "\n"+filepath.Join(ws, "lib")+"/...\n") {
		t.Errorf("candidates %q lack the replaced folder", got)
	}
	if strings.Contains(got, "lib/b.go") || strings.Contains(got, "steady/...") {
		t.Errorf("candidates %q: something under the replaced folder was passed singly, or the unchanged folder whole", got)
	}
	if !strings.Contains(got, "\n"+filepath.Join(ws, "steady", "d.go")+"\n") {
		t.Errorf("candidates %q lack the file added to the unchanged folder", got)
	}
}
