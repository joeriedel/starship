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

	"github.com/gofrs/flock"
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
	if err := os.MkdirAll(filepath.Join(w.bridge, ".starship", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	dead := filepath.Join(w.bridge, ".starship", "sessions", "dead")
	write(t, dead, "20240101-000009-supernova\n")
	if res := gcRuns(w.bridge, 0, 0); res.Removed != 0 || !exists(a) {
		t.Fatalf("gc must be conservative: %+v", res)
	}
	if exists(dead) {
		t.Error("a dead link goes even when no run may be deleted")
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

func TestGCDropsDeadLinksWhenNoRunIsRemoved(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	sess := filepath.Join(w.bridge, ".starship", "sessions")
	write(t, filepath.Join(sess, "dead"), "20240101-000009-supernova\n")
	if res := gcRuns(w.bridge, 5, 0); res.Removed != 0 || res.Links != 1 || exists(filepath.Join(sess, "dead")) {
		t.Fatalf("a dead link goes even when no run does: %+v", res)
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
	if !strings.Contains(show, "cleaned up 9 old runs") || strings.Contains(show, "next session start") {
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

// sessFiles writes the swept file types for a session, all aged by age, and returns their paths.
func sessFiles(t *testing.T, w *world, id string, age time.Duration, link string) []string {
	t.Helper()
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	var paths []string
	for _, suf := range []string{".task", ".pin", ".env", ".topic-a", ".topic-b"} {
		paths = append(paths, filepath.Join(dir, id+suf))
	}
	if link != "" {
		paths = append(paths, filepath.Join(dir, id))
	}
	past := time.Now().Add(-age)
	for _, p := range paths {
		content := "x\n"
		if p == filepath.Join(dir, id) {
			content = link
		}
		write(t, p, content)
		os.Chtimes(p, past, past)
	}
	return paths
}

func allExist(ps []string) bool {
	for _, p := range ps {
		if !exists(p) {
			return false
		}
	}
	return true
}

func noneExist(ps []string) bool {
	for _, p := range ps {
		if exists(p) {
			return false
		}
	}
	return true
}

const oldAge = sessionFileAge + time.Hour

func TestSessionFilesSweptByAge(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	stale := sessFiles(t, w, "old.id", oldAge, "")
	seq := seqPath(w.bridge, "old.id")
	writeJSON(seq, &seqFile{Sequences: []*sequence{{Steps: []*seqStep{{N: 1, Kind: "run", Run: "r", Status: "done"}}}}})
	past := time.Now().Add(-oldAge)
	os.Chtimes(seq, past, past)
	write(t, seq+".lock", "")
	write(t, filepath.Join(dir, "old.id.lock"), "")
	fresh := sessFiles(t, w, "new", time.Hour, "")
	mixed := sessFiles(t, w, "mixed", oldAge, "")
	os.Remove(mixed[1])
	write(t, mixed[1], "x\n")
	if n := dropDeadLinks(w.bridge); n != 0 {
		t.Fatalf("dropped %d links", n)
	}
	if !noneExist(append(stale, seq)) {
		t.Error("stale session files must go")
	}
	if !allExist(fresh) {
		t.Error("fresh session files must stay")
	}
	if !exists(mixed[0]) || !exists(mixed[1]) {
		t.Error("a stale .task beside a fresh .pin must stay")
	}
	if !exists(seq+".lock") || !exists(filepath.Join(dir, "old.id.lock")) {
		t.Error("fresh lock files stay")
	}
}

func TestSessionFilesKeptForLiveRuns(t *testing.T) {
	for _, status := range []string{"running", "ask-user"} {
		w := newWorld(t)
		fabricate(t, w, "20240101-000001-supernova", status, true)
		files := sessFiles(t, w, "s1", oldAge, "20240101-000001-supernova\n")
		dropDeadLinks(w.bridge)
		if !allExist(files) {
			t.Errorf("%s run through the link must keep the session's files", status)
		}
	}
	w := newWorld(t)
	dir := fabricate(t, w, "20240101-000001-supernova", "running", true)
	var st State
	readJSON(filepath.Join(dir, "state.json"), &st)
	st.Session = "s1"
	writeJSON(filepath.Join(dir, "state.json"), &st)
	files := sessFiles(t, w, "s1", oldAge, "")
	dropDeadLinks(w.bridge)
	if !allExist(files) {
		t.Error("a run owned through State.Session must keep the files")
	}
	w = newWorld(t)
	dir = fabricate(t, w, "20240101-000001-supernova", "success", true)
	write(t, filepath.Join(dir, "state.json"), "{bad")
	files = sessFiles(t, w, "s1", oldAge, "20240101-000001-supernova\n")
	dropDeadLinks(w.bridge)
	if !allExist(files) {
		t.Error("a linked run with unreadable state must keep the files")
	}
	w = newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	files = sessFiles(t, w, "s1", oldAge, "20240101-000001-supernova\n")
	dropDeadLinks(w.bridge)
	if !noneExist(files[:5]) {
		t.Error("a finished run does not keep the files")
	}
}

func TestSessionFilesKeptForOpenSequence(t *testing.T) {
	open := &seqFile{Sequences: []*sequence{{Steps: []*seqStep{{N: 1, Kind: "run", Run: "r", Status: "pending"}}}}}
	for _, name := range []string{"open", "corrupt"} {
		w := newWorld(t)
		files := sessFiles(t, w, "s1", oldAge, "")
		seq := seqPath(w.bridge, "s1")
		if name == "open" {
			writeJSON(seq, open)
		} else {
			write(t, seq, "{bad")
		}
		past := time.Now().Add(-oldAge)
		os.Chtimes(seq, past, past)
		dropDeadLinks(w.bridge)
		if !allExist(append(files, seq)) {
			t.Errorf("%s sequence must keep the session's files", name)
		}
	}
}

func TestSessionFilesSkippedWhenSequenceLockHeld(t *testing.T) {
	w := newWorld(t)
	files := sessFiles(t, w, "s1", oldAge, "")
	seq := seqPath(w.bridge, "s1")
	writeJSON(seq, &seqFile{})
	past := time.Now().Add(-oldAge)
	os.Chtimes(seq, past, past)
	l := flock.New(seq + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	done := make(chan struct{})
	go func() { dropDeadLinks(w.bridge); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gc blocked on the sequence lock")
	}
	if !allExist(append(files, seq)) {
		t.Error("nothing may be removed while the sequence lock is held")
	}
}

func TestSweepLockHeldSkipsSessionFilesAndGC(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	write(t, filepath.Join(dir, "dead"), "20240101-000009-supernova\n")
	write(t, filepath.Join(dir, "old.gc"), "x\n")
	write(t, filepath.Join(dir, "7.pid"), "x\n")
	past := time.Now().Add(-oldAge)
	os.Chtimes(filepath.Join(dir, "old.gc"), past, past)
	os.Chtimes(filepath.Join(dir, "7.pid"), past, past)
	l := flock.New(filepath.Join(dir, ".sweep.lock"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int)
	go func() { done <- dropDeadLinks(w.bridge) }()
	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("dropped %d links, want 1", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gc blocked on the sweep lock")
	}
	if !allExist(files) || !exists(filepath.Join(dir, "old.gc")) {
		t.Error("session files and old .gc files must stay while another sweeper holds the lock")
	}
	if exists(filepath.Join(dir, "7.pid")) || exists(filepath.Join(dir, "dead")) {
		t.Error("a stale .pid and a dead link still go")
	}
	l.Unlock()
	dropDeadLinks(w.bridge)
	if !noneExist(files[:5]) || exists(filepath.Join(dir, "old.gc")) {
		t.Error("a sweep after the lock is released removes them")
	}
	if !exists(filepath.Join(dir, ".sweep.lock")) {
		t.Error("the sweep lock file is never removed")
	}
}

func TestSessionOfFileRejectsGC(t *testing.T) {
	for _, name := range []string{"s1.topic-a.gc", "s1.task.gc"} {
		if id, ok := sessionOfFile(name); ok {
			t.Errorf("%s grouped under %q", name, id)
		}
	}
}

func TestSessionFilesSweptInSessionStart(t *testing.T) {
	w := clearWorld(t)
	files := sessFiles(t, w, "gone", oldAge, "")
	w.sid = "s2"
	w.hook("SessionStart", map[string]any{"source": "startup"})
	if !noneExist(files) {
		t.Error("SessionStart must sweep a stale session")
	}
	if !exists(filepath.Join(w.bridge, ".starship", "sessions", "s2.pin")) {
		t.Error("the starting session's own files must stay")
	}
}

func TestDeadLinkWithSeveralRuns(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "running", true)
	b := fabricate(t, w, "20240101-000002-supernova", "running", true)
	link := filepath.Join(w.bridge, ".starship", "sessions", "multi")
	write(t, link, "20240101-000001-supernova\n20240101-000002-supernova\n")
	if deadLink(w.bridge, link) {
		t.Error("both runs live: not dead")
	}
	os.RemoveAll(b)
	if deadLink(w.bridge, link) {
		t.Error("one run live: not dead")
	}
	os.RemoveAll(filepath.Join(runsDir(w.bridge), "20240101-000001-supernova"))
	if !deadLink(w.bridge, link) {
		t.Error("both runs gone: dead")
	}
	if n := dropDeadLinks(w.bridge); n != 1 || exists(link) {
		t.Errorf("dead link not dropped: %d", n)
	}
}

func TestSettleSessionJudgesTheWholeSession(t *testing.T) {
	// rename mimics the first step of reapSession, so a test can act between it and the judgement.
	rename := func(files []string) []string {
		for _, f := range files {
			if err := os.Rename(f, f+".gc"); err != nil {
				t.Fatal(err)
			}
		}
		return files
	}
	names := func(files []string) []string {
		var ns []string
		for _, f := range files {
			ns = append(ns, filepath.Base(f))
		}
		return ns
	}
	noGC := func(files []string) bool {
		for _, f := range files {
			if exists(f + ".gc") {
				return false
			}
		}
		return true
	}
	// files: .task, .pin, .env, .topic-a, .topic-b
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	renamed := rename(files)
	write(t, files[1], "x\n")
	settleSession(dir, "s1", names(files), renamed)
	if !allExist(files) || !noGC(files) {
		t.Error("a .pin rewritten after the snapshot must keep the whole session")
	}

	w = newWorld(t)
	dir = filepath.Join(w.bridge, ".starship", "sessions")
	files = sessFiles(t, w, "s1", oldAge, "")
	renamed = rename(files)
	write(t, files[0], "new\n")
	old := time.Now().Add(-oldAge)
	os.Chtimes(files[0], old, old)
	settleSession(dir, "s1", names(files), renamed)
	if got, _ := os.ReadFile(files[0]); string(got) != "new\n" {
		t.Errorf("a recreated .task must keep its new content, got %q", got)
	}
	if !noGC(files) {
		t.Error("no .gc file may be left behind")
	}

	w = newWorld(t)
	dir = filepath.Join(w.bridge, ".starship", "sessions")
	files = sessFiles(t, w, "s1", oldAge, "")
	renamed = rename(files)
	settleSession(dir, "s1", names(files), renamed)
	if !noneExist(files) || !noGC(files) {
		t.Error("a session that is still stale must be gone, with no .gc left")
	}
}

func oldFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	write(t, path, "")
	past := time.Now().Add(-age)
	os.Chtimes(path, past, past)
}

func TestSessionLeftoversSweptWithOldSession(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "old.id", oldAge, "")
	var left []string
	for _, n := range []string{"old.id.lock", "old.id.sequence.lock", "old.id.tmp"} {
		oldFile(t, filepath.Join(dir, n), oldAge)
		left = append(left, filepath.Join(dir, n))
	}
	dropDeadLinks(w.bridge)
	if !noneExist(files) || !noneExist(left) {
		t.Error("an old session's files, locks and tmp must all go")
	}
}

func TestSessionLeftoversOrphansSwept(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	var left []string
	for _, n := range []string{"a.lock", "b.sequence.lock", "c.tmp", "d.lock", "d.sequence.lock"} {
		oldFile(t, filepath.Join(dir, n), oldAge)
		left = append(left, filepath.Join(dir, n))
	}
	dropDeadLinks(w.bridge)
	if !noneExist(left) {
		t.Error("orphaned old locks and tmp must go")
	}
	if exists(filepath.Join(dir, "c.lock")) {
		t.Error("a tmp without a lock must not leave a lock behind")
	}
}

func TestSessionLeftoversKeptForYoungSession(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	oldFile(t, filepath.Join(dir, "a.lock"), oldAge)
	oldFile(t, filepath.Join(dir, "a.sequence.lock"), oldAge)
	oldFile(t, filepath.Join(dir, "a.tmp"), oldAge)
	write(t, filepath.Join(dir, "a.task"), "x\n")
	oldFile(t, filepath.Join(dir, "b.lock"), oldAge)
	write(t, filepath.Join(dir, "b"), "20240101-000009-supernova\n")
	dropDeadLinks(w.bridge)
	for _, n := range []string{"a.lock", "a.sequence.lock", "a.tmp", "b.lock"} {
		if !exists(filepath.Join(dir, n)) {
			t.Errorf("%s must stay for a young session", n)
		}
	}
}

func TestSessionFreshLockDoesNotHoldBackFiles(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	write(t, filepath.Join(dir, "s1.lock"), "")
	write(t, filepath.Join(dir, "s1.tmp"), "")
	dropDeadLinks(w.bridge)
	if !noneExist(files) {
		t.Error("old session files must go despite a fresh lock")
	}
	if !exists(filepath.Join(dir, "s1.lock")) || !exists(filepath.Join(dir, "s1.tmp")) {
		t.Error("a fresh lock and tmp must stay")
	}
}

func TestSessionLeftoversKeptWhenBusy(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	fabricate(t, w, "20240101-000001-supernova", "running", true)
	files := sessFiles(t, w, "s1", oldAge, "20240101-000001-supernova\n")
	seq := seqPath(w.bridge, "s2")
	writeJSON(seq, &seqFile{Sequences: []*sequence{{Steps: []*seqStep{{N: 1, Kind: "run", Run: "r", Status: "pending"}}}}})
	oldFile(t, seq, oldAge)
	var left []string
	for _, id := range []string{"s1", "s2"} {
		for _, n := range []string{".lock", ".sequence.lock", ".tmp"} {
			oldFile(t, filepath.Join(dir, id+n), oldAge)
			left = append(left, filepath.Join(dir, id+n))
		}
	}
	dropDeadLinks(w.bridge)
	if !allExist(left) || !allExist(files) {
		t.Error("a busy session keeps its locks and tmp")
	}
}

func TestSessionLeftoversKeptWhenLockHeld(t *testing.T) {
	for _, held := range []string{"s1.lock", "s1.sequence.lock"} {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		var left []string
		for _, n := range []string{"s1.lock", "s1.sequence.lock", "s1.tmp"} {
			oldFile(t, filepath.Join(dir, n), oldAge)
			left = append(left, filepath.Join(dir, n))
		}
		l := flock.New(filepath.Join(dir, held))
		if err := l.Lock(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { dropDeadLinks(w.bridge); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			l.Unlock()
			t.Fatalf("gc blocked on %s", held)
		}
		l.Unlock()
		if !allExist(left) {
			t.Errorf("nothing may be removed while %s is held", held)
		}
	}
}

func TestSessionLeftoversCreateNoSequenceLock(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	oldFile(t, filepath.Join(dir, "s1.lock"), oldAge)
	oldFile(t, filepath.Join(dir, "s2.tmp"), oldAge)
	dropDeadLinks(w.bridge)
	for _, n := range []string{"s1.sequence.lock", "s2.sequence.lock", "s2.lock"} {
		if exists(filepath.Join(dir, n)) {
			t.Errorf("%s must not exist", n)
		}
	}
}

func TestSessionLeftoversSparesSweepLockAndSessionTmp(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	keep := []string{".sweep.lock", "s1.sequence.tmp", "s1.task.tmp"}
	for _, n := range keep {
		oldFile(t, filepath.Join(dir, n), oldAge)
	}
	dropDeadLinks(w.bridge)
	for _, n := range keep {
		if !exists(filepath.Join(dir, n)) {
			t.Errorf("%s must stay", n)
		}
	}
	for _, n := range []string{"s1.sequence.lock", "s1.lock", "s1.sequence.sequence.lock"} {
		if exists(filepath.Join(dir, n)) {
			t.Errorf("%s must not be created", n)
		}
	}
}
