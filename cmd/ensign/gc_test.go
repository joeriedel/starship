package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fabricate writes a run folder with the given status and a scratch file.
func fabricate(t *testing.T, w *world, id, status string, approved bool) string {
	t.Helper()
	dir := filepath.Join(runsDir(w.bridge), id)
	write(t, filepath.Join(dir, "scratch", "note.txt"), "x")
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), &Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), &State{Run: id, Status: status, Approved: approved, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestGCKeepsNewestFinishedAndSkipsUnfinished(t *testing.T) {
	w := newWorld(t)
	old := fabricate(t, w, "20240101-000001-supernova", "success", true)
	mid := fabricate(t, w, "20240101-000002-supernova", "error", true)
	pend := fabricate(t, w, "20240101-000000-supernova", "pending", false)
	queued := fabricate(t, w, "20240101-000003-supernova", "pending", true)
	running := fabricate(t, w, "20240101-000004-supernova", "running", true)
	ask := fabricate(t, w, "20240101-000005-supernova", "ask-user", true)
	newest := fabricate(t, w, "20240101-000006-supernova", "success", true)
	res := gcRuns(w.bridge, 2, 0)
	if res.Removed != 1 || exists(old) {
		t.Fatalf("want the oldest finished run removed, got %+v", res)
	}
	for _, d := range []string{mid, pend, queued, running, ask, newest} {
		if !exists(d) {
			t.Errorf("%s should be kept", d)
		}
	}
	if res.Scratch != 2 || exists(filepath.Join(mid, "scratch", "note.txt")) || !exists(filepath.Join(mid, "scratch")) {
		t.Errorf("finished runs' scratch should be emptied: %+v", res)
	}
	if !exists(filepath.Join(running, "scratch", "note.txt")) || !exists(filepath.Join(ask, "scratch", "note.txt")) {
		t.Error("unfinished runs' scratch must stay")
	}
}

func TestGCZeroDeletesAllFinishedAndDropsDeadLinks(t *testing.T) {
	w := newWorld(t)
	a := fabricate(t, w, "20240101-000001-supernova", "success", true)
	fabricate(t, w, "20240101-000002-supernova", "error", true)
	live := fabricate(t, w, "20240101-000003-supernova", "running", true)
	sess := filepath.Join(w.bridge, ".starship", "sessions")
	write(t, filepath.Join(sess, "dead"), "20240101-000001-supernova\n")
	write(t, filepath.Join(sess, "alive"), "20240101-000003-supernova\n")
	write(t, filepath.Join(sess, "dead.pin"), "abc\n")
	write(t, filepath.Join(sess, "dead.task"), "supernova/x\n")
	var out bytes.Buffer
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	if err := cmdGC([]string{"0"}, &out); err != nil {
		t.Fatal(err)
	}
	if exists(a) || !exists(live) {
		t.Fatal("gc 0 should delete finished runs only")
	}
	if !strings.Contains(out.String(), "removed 2 runs") || !strings.Contains(out.String(), "dropped 1 session links") {
		t.Errorf("output: %q", out.String())
	}
	if exists(filepath.Join(sess, "dead")) || !exists(filepath.Join(sess, "alive")) ||
		!exists(filepath.Join(sess, "dead.pin")) || !exists(filepath.Join(sess, "dead.task")) {
		t.Error("only the dead bare link should go")
	}
}

func TestGCKeepsRunsHeldBySequenceSteps(t *testing.T) {
	w := newWorld(t)
	failed := fabricate(t, w, "20240101-000001-supernova", "error", true)
	active := fabricate(t, w, "20240101-000002-supernova", "success", true)
	gone := fabricate(t, w, "20240101-000003-supernova", "success", true)
	sf := &seqFile{Sequences: []*sequence{{Steps: []*seqStep{
		{N: 1, Kind: "run", Run: "20240101-000001-supernova", Status: "decide"},
		{N: 2, Kind: "run", Run: "20240101-000002-supernova", Status: "active"},
		{N: 3, Kind: "run", Run: "20240101-000003-supernova", Status: "done"},
	}}}}
	if err := writeJSON(seqPath(w.bridge, "s1"), sf); err != nil {
		t.Fatal(err)
	}
	res := gcRuns(w.bridge, 0, 0)
	if !exists(failed) || !exists(active) || exists(gone) || res.Removed != 1 {
		t.Fatalf("held runs must survive, others go: %+v", res)
	}
	if !exists(filepath.Join(failed, "scratch", "note.txt")) {
		t.Error("a held run's scratch must stay")
	}
}

func TestGCDeletesNothingWhenSequenceFileUnreadable(t *testing.T) {
	w := newWorld(t)
	a := fabricate(t, w, "20240101-000001-supernova", "success", true)
	write(t, seqPath(w.bridge, "bad"), "{not json")
	if res := gcRuns(w.bridge, 0, 0); res.Removed != 0 || !exists(a) {
		t.Fatalf("gc must be conservative: %+v", res)
	}
}

func TestDropDeadLinksKeepsLiveAndClearsLeftovers(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "running", true)
	sess := filepath.Join(w.bridge, ".starship", "sessions")
	write(t, filepath.Join(sess, "live"), "20240101-000001-supernova\n")
	write(t, filepath.Join(sess, "dead"), "20240101-000009-supernova\n")
	write(t, filepath.Join(sess, "old.gc"), "x\n")
	write(t, filepath.Join(sess, "fresh.gc"), "x\n")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(sess, "old.gc"), past, past); err != nil {
		t.Fatal(err)
	}
	liveInfo, err := os.Stat(filepath.Join(sess, "live"))
	if err != nil {
		t.Fatal(err)
	}
	if n := dropDeadLinks(w.bridge); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	if !exists(filepath.Join(sess, "live")) || exists(filepath.Join(sess, "live.gc")) ||
		exists(filepath.Join(sess, "dead")) || exists(filepath.Join(sess, "dead.gc")) || exists(filepath.Join(sess, "old.gc")) {
		t.Error("live link stays, dead link and old leftovers go")
	}
	if !exists(filepath.Join(sess, "fresh.gc")) {
		t.Error("a fresh .gc file belongs to a concurrent pass and must stay")
	}
	if info, err := os.Stat(filepath.Join(sess, "live")); err != nil || !os.SameFile(info, liveInfo) {
		t.Error("live link must never be renamed or replaced")
	}
}

func TestGCRejectsBadArgs(t *testing.T) {
	w := newWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	for _, args := range [][]string{{"-1"}, {"x"}, {"1", "2"}} {
		if err := cmdGC(args, &bytes.Buffer{}); err == nil {
			t.Errorf("gc %v should be a usage error", args)
		}
	}
}

func TestVanishedRunIsNoRun(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	if err := bindSession(w.bridge, "gone", "20240101-000001-supernova"); err != nil {
		t.Fatal(err)
	}
	gcRuns(w.bridge, 0, 0)
	r, err := openSessionRun(w.bridge, "gone")
	if r != nil || err != nil {
		t.Fatalf("a deleted run should be no run: %v %v", r, err)
	}
}

func TestScratchEmptiedOnFinish(t *testing.T) {
	for status, want := range map[string]bool{"success": true, "error": true, "ask-user": false} {
		w := newWorld(t)
		dir := fabricate(t, w, "20240101-000001-supernova", status, true)
		r, err := openRun(w.bridge, "20240101-000001-supernova")
		if err != nil {
			t.Fatal(err)
		}
		r.State.Finished = false
		finishRun(r)
		r.close()
		if got := !exists(filepath.Join(dir, "scratch", "note.txt")); got != want || !exists(filepath.Join(dir, "scratch")) {
			t.Errorf("%s: scratch emptied = %v, want %v", status, got, want)
		}
	}
}

func TestSessionStartCleansWithCap(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "keep-runs: 1\ncontrol-plane:\n  path: "+mustRoot(t)+"\n")
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	start := func() string {
		in, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "abc"})
		var out, errb bytes.Buffer
		if code := cmdHook("SessionStart", bytes.NewReader(in), &out, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		show, _ := sessionStartReply(t, out.Bytes())
		return show
	}
	if show := start(); strings.Contains(show, "cleaned") {
		t.Errorf("nothing to clean, but: %q", show)
	}
	for i := 0; i < sessionStartGCCap+10; i++ {
		fabricate(t, w, fmt.Sprintf("20240101-%06d-supernova", i), "success", true)
	}
	show := start()
	if !strings.Contains(show, fmt.Sprintf("cleaned up %d old runs", sessionStartGCCap)) || !strings.Contains(show, "next session start") {
		t.Errorf("first start: %q", show)
	}
	show = start()
	if !strings.Contains(show, "cleaned up 10 old runs") || strings.Contains(show, "next session start") {
		t.Errorf("second start: %q", show)
	}
}

func mustRoot(t *testing.T) string {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
