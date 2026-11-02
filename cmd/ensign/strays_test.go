package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func strayRun(t *testing.T) (*RunFiles, string) {
	t.Helper()
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	runGit(t, ws, "checkout", "-q", "-b", "task/dep")
	return unitRun(t, ws), ws
}

func refused(t *testing.T, r *RunFiles, ws string, want ...string) string {
	t.Helper()
	err := githubCommit(r)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("message lacks %q: %v", w, err)
		}
	}
	if runGit(t, ws, "rev-list", "--count", "HEAD") != "1" {
		t.Error("nothing may be committed on a refusal")
	}
	return err.Error()
}

func committed(t *testing.T, r *RunFiles, ws, file string) {
	t.Helper()
	if err := githubCommit(r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runGit(t, ws, "show", "--name-only", "--format=", "HEAD"), file) {
		t.Errorf("%s should be committed", file)
	}
}

func TestStrayBinaryRefused(t *testing.T) {
	r, ws := strayRun(t)
	write(t, filepath.Join(ws, "nova.dat"), "ab\x00cd")
	refused(t, r, ws, "nova.dat", "binary")
}

func TestStrayExecutableRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no exec bits on Windows")
	}
	r, ws := strayRun(t)
	write(t, filepath.Join(ws, "nova.sh"), "echo hi\n")
	os.Chmod(filepath.Join(ws, "nova.sh"), 0o755)
	refused(t, r, ws, "nova.sh", "executable")
}

func TestStrayExecutableBitIgnoredWithoutFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no exec bits on Windows")
	}
	r, ws := strayRun(t)
	runGit(t, ws, "config", "core.fileMode", "false")
	write(t, filepath.Join(ws, "nova.sh"), "echo hi\n")
	os.Chmod(filepath.Join(ws, "nova.sh"), 0o755)
	committed(t, r, ws, "nova.sh")
}

func TestStrayOversizeRefused(t *testing.T) {
	r, ws := strayRun(t)
	write(t, filepath.Join(ws, "big.txt"), strings.Repeat("a", strayMaxSize+1))
	refused(t, r, ws, "big.txt", "1048577 bytes", "1048576 byte limit")
}

func TestStrayReportedByCoderIsCommitted(t *testing.T) {
	r, ws := strayRun(t)
	write(t, filepath.Join(ws, "nova.dat"), "ab\x00cd")
	r.State.Dispatches = []*Dispatch{{Role: "coder", Done: true, Fields: map[string][]string{"files": {"nova.dat"}}}}
	committed(t, r, ws, "nova.dat")
}

func TestStraySmallTextIsCommitted(t *testing.T) {
	r, ws := strayRun(t)
	write(t, filepath.Join(ws, "nova.txt"), "plain\n")
	committed(t, r, ws, "nova.txt")
}

func TestStrayAttribution(t *testing.T) {
	r, ws := strayRun(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	at := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }
	r.State.History = []HistoryEntry{
		{At: at(10 * time.Minute), Kind: "dispatch", Stage: "code", Dispatch: "d1"},
		{At: at(30 * time.Minute), Kind: "dispatch", Stage: "test", Dispatch: "d2"},
	}
	mid, old := filepath.Join(ws, "mid.dat"), filepath.Join(ws, "old.dat")
	write(t, mid, "a\x00")
	write(t, old, "a\x00")
	os.Chtimes(mid, base.Add(20*time.Minute), base.Add(20*time.Minute))
	os.Chtimes(old, base, base)
	msg := refused(t, r, ws, "mid.dat", "old.dat")
	if !strings.Contains(msg, "stage code, dispatch d1") {
		t.Errorf("mid.dat should be attributed to the earlier stage: %s", msg)
	}
	if !strings.Contains(msg, "predates every dispatch") {
		t.Errorf("old.dat should predate every dispatch: %s", msg)
	}
}
