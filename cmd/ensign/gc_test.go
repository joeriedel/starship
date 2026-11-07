// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	if n := dropDeadLinks(w.bridge).Links; n != 1 {
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
	for _, suf := range []string{".task", ".pin", ".env", ".lines", ".topic-a", ".topic-b"} {
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
	if n := dropDeadLinks(w.bridge).Links; n != 0 {
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
	go func() { done <- dropDeadLinks(w.bridge).Links }()
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
	if n := dropDeadLinks(w.bridge).Links; n != 1 || exists(link) {
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
	settleSession(dir, "s1", names(files), renamed, testSweep())
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
	settleSession(dir, "s1", names(files), renamed, testSweep())
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
	settleSession(dir, "s1", names(files), renamed, testSweep())
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
	for _, n := range []string{"old.id.lock", "old.id.sequence.lock", "old.id.tmp", "old.id.sequence.tmp"} {
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

func TestSessionLeftoversSequenceTmpOrphanSwept(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	oldFile(t, filepath.Join(dir, "s1.sequence.tmp"), oldAge)
	dropDeadLinks(w.bridge)
	for _, n := range []string{"s1.sequence.tmp", "s1.sequence.lock", "s1.lock", "s1.sequence.sequence.lock"} {
		if exists(filepath.Join(dir, n)) {
			t.Errorf("%s must not exist", n)
		}
	}
}

func TestSessionLeftoversSequenceTmpAndTmpBothSwept(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	var left []string
	for _, n := range []string{"s1.tmp", "s1.sequence.tmp"} {
		oldFile(t, filepath.Join(dir, n), oldAge)
		left = append(left, filepath.Join(dir, n))
	}
	dropDeadLinks(w.bridge)
	if !noneExist(files) || !noneExist(left) {
		t.Error("an old session loses both tmp files")
	}
}

func TestSessionLeftoversFreshSequenceTmpKept(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	tmp := filepath.Join(dir, "s1.sequence.tmp")
	lock := filepath.Join(dir, "s1.sequence.lock")
	write(t, tmp, "")
	oldFile(t, lock, oldAge)
	dropDeadLinks(w.bridge)
	for _, f := range files {
		if !exists(f) {
			t.Errorf("%s must stay: a fresh sequence tmp with no sequence file is a save in progress", f)
		}
	}
	if !exists(tmp) || !exists(lock) {
		t.Error("a fresh sequence tmp and its lock must stay")
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tmp, past, past); err != nil {
		t.Fatal(err)
	}
	dropDeadLinks(w.bridge)
	if !noneExist(files) {
		t.Error("old session files must go once the sequence tmp is a crash leftover")
	}
	if !exists(tmp) || !exists(lock) {
		t.Error("a sequence tmp younger than the sweep age and its lock must stay")
	}
}

func TestSessionLeftoversKeptForYoungSession(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	oldFile(t, filepath.Join(dir, "a.lock"), oldAge)
	oldFile(t, filepath.Join(dir, "a.sequence.lock"), oldAge)
	oldFile(t, filepath.Join(dir, "a.tmp"), oldAge)
	oldFile(t, filepath.Join(dir, "a.sequence.tmp"), oldAge)
	write(t, filepath.Join(dir, "a.task"), "x\n")
	oldFile(t, filepath.Join(dir, "b.lock"), oldAge)
	write(t, filepath.Join(dir, "b"), "20240101-000009-supernova\n")
	dropDeadLinks(w.bridge)
	for _, n := range []string{"a.lock", "a.sequence.lock", "a.tmp", "a.sequence.tmp", "b.lock"} {
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
		for _, n := range []string{".lock", ".sequence.lock", ".tmp", ".sequence.tmp"} {
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
		for _, n := range []string{"s1.lock", "s1.sequence.lock", "s1.tmp", "s1.sequence.tmp"} {
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
	keep := []string{".sweep.lock", "s1.task.tmp"}
	for _, n := range append([]string{"s1.writes.tmp"}, keep...) {
		oldFile(t, filepath.Join(dir, n), oldAge)
	}
	dropDeadLinks(w.bridge)
	for _, n := range keep {
		if !exists(filepath.Join(dir, n)) {
			t.Errorf("%s must stay", n)
		}
	}
	for _, n := range []string{"s1.writes.tmp", "s1.writes.lock", "s1.writes.writes.lock", "s1.sequence.lock", "s1.lock", "s1.sequence.sequence.lock"} {
		if exists(filepath.Join(dir, n)) {
			t.Errorf("%s must not exist", n)
		}
	}
}

// pipelineOnlyRun makes a run folder with an aged pipeline.json and no state.json.
func pipelineOnlyRun(t *testing.T, w *world, id string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(runsDir(w.bridge), id)
	oldFile(t, filepath.Join(dir, "pipeline.json"), age)
	return dir
}

func TestGCReclaimsOldPipelineOnlyFolderAndDropsLink(t *testing.T) {
	w := newWorld(t)
	old := pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
	young := pipelineOnlyRun(t, w, "20240101-000002-supernova", time.Minute)
	link := filepath.Join(w.bridge, ".starship", "sessions", "dead")
	write(t, link, "20240101-000001-supernova\n")
	res := gcRuns(w.bridge, 500, 0)
	if exists(old) || res.Removed != 1 {
		t.Fatalf("an old pipeline-only folder must go: %+v", res)
	}
	if !exists(young) {
		t.Error("a young pipeline-only folder must stay")
	}
	if exists(link) || res.Links != 1 {
		t.Errorf("the link naming the removed folder must be dropped in the same pass: %+v", res)
	}
}

func TestGCKeepsPipelineOnlyFolderWhenLockHeld(t *testing.T) {
	w := newWorld(t)
	dir := pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
	l := flock.New(filepath.Join(dir, "lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer l.Unlock()
	if res := gcRuns(w.bridge, 500, 0); !exists(dir) || res.Removed != 0 {
		t.Fatalf("a folder whose run lock is held must stay: %+v", res)
	}
}

func TestGCKeepsPipelineOnlyFolderHeldBySequence(t *testing.T) {
	w := newWorld(t)
	dir := pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
	sf := &seqFile{Sequences: []*sequence{{Steps: []*seqStep{{N: 1, Kind: "run", Run: "20240101-000001-supernova", Status: "active"}}}}}
	write(t, filepath.Join(w.bridge, ".starship", "sessions", "keep"), "")
	if err := writeJSON(seqPath(w.bridge, "s1"), sf); err != nil {
		t.Fatal(err)
	}
	if res := gcRuns(w.bridge, 500, 0); !exists(dir) || res.Removed != 0 {
		t.Fatalf("a folder held by a sequence must stay: %+v", res)
	}
}

func TestGCKeepsPipelineOnlyFolderWithCorruptState(t *testing.T) {
	w := newWorld(t)
	dir := pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
	write(t, filepath.Join(dir, "state.json"), "{not json")
	if res := gcRuns(w.bridge, 500, 0); !exists(dir) || res.Removed != 0 {
		t.Fatalf("a folder with a corrupt state.json must stay: %+v", res)
	}
}

func TestGCPipelineOnlyFoldersDoNotChangeKeptRuns(t *testing.T) {
	w := newWorld(t)
	old := fabricate(t, w, "20240101-000001-supernova", "success", true)
	newer := fabricate(t, w, "20240101-000002-supernova", "success", true)
	po := pipelineOnlyRun(t, w, "20240101-000003-supernova", pipelineOnlyAge+time.Minute)
	res := gcRuns(w.bridge, 1, 0)
	if exists(po) || exists(old) || !exists(newer) || res.Removed != 2 {
		t.Fatalf("keep-runs counts finished runs only: %+v", res)
	}
}

func TestGCPipelineOnlyFoldersCountTowardCap(t *testing.T) {
	w := newWorld(t)
	f1 := fabricate(t, w, "20240101-000001-supernova", "success", true)
	po1 := pipelineOnlyRun(t, w, "20240101-000002-supernova", pipelineOnlyAge+time.Minute)
	po2 := pipelineOnlyRun(t, w, "20240101-000003-supernova", pipelineOnlyAge+time.Minute)
	res := gcRuns(w.bridge, 0, 2)
	if exists(po1) || exists(po2) || !exists(f1) || res.Removed != 2 || !res.More {
		t.Fatalf("pipeline-only folders use the cap before finished runs: %+v", res)
	}
}

func TestGCLeavesNeitherFileFolderAndSpendsNoAttempt(t *testing.T) {
	w := newWorld(t)
	bare := filepath.Join(runsDir(w.bridge), "20240101-000001-supernova")
	write(t, filepath.Join(bare, "scratch", "note.txt"), "x")
	fin := fabricate(t, w, "20240101-000002-supernova", "success", true)
	res := gcRuns(w.bridge, 0, 1)
	if !exists(bare) {
		t.Errorf("a folder with neither file must survive")
	}
	if exists(fin) || res.Removed != 1 || res.More {
		t.Fatalf("the finished run must go and the bare folder use no attempt: %+v", res)
	}
}

func TestGCRemovesStateOnlyFolderAndSpendsAnAttempt(t *testing.T) {
	w := newWorld(t)
	stateOnly := fabricate(t, w, "20240101-000001-supernova", "success", true)
	if err := os.Remove(filepath.Join(stateOnly, "pipeline.json")); err != nil {
		t.Fatal(err)
	}
	newer := fabricate(t, w, "20240101-000002-supernova", "success", true)
	res := gcRuns(w.bridge, 0, 1)
	if exists(stateOnly) || !exists(newer) || res.Removed != 1 || !res.More {
		t.Fatalf("the state-only folder must go and spend the attempt, leaving the newer run for the next gc: %+v", res)
	}
}

func TestGCStateOnlyFolderTakesKeepSlot(t *testing.T) {
	w := newWorld(t)
	older := fabricate(t, w, "20240101-000001-supernova", "success", true)
	stateOnly := fabricate(t, w, "20240101-000002-supernova", "success", true)
	if err := os.Remove(filepath.Join(stateOnly, "pipeline.json")); err != nil {
		t.Fatal(err)
	}
	res := gcRuns(w.bridge, 1, 0)
	if exists(older) || !exists(stateOnly) || res.Removed != 1 {
		t.Fatalf("the state-only folder must hold the keep slot: %+v", res)
	}
}

func TestSessionFilesSweepsAnOldMovedMarker(t *testing.T) {
	w := newWorld(t)
	marker := movedPath(w.bridge, "old.id")
	write(t, marker, "new.id\n")
	past := time.Now().Add(-oldAge)
	os.Chtimes(marker, past, past)
	fresh := movedPath(w.bridge, "recent.id")
	write(t, fresh, "new.id\n")
	dropDeadLinks(w.bridge)
	if exists(marker) {
		t.Error("an old moved marker should be swept")
	}
	if !exists(fresh) {
		t.Error("a recent moved marker should stay")
	}
}

func TestSessionFilesKeepsAnOldMovedMarkerWhileItsSessionHasAnUnfinishedSequence(t *testing.T) {
	w := newWorld(t)
	files := sessFiles(t, w, "s1", oldAge, "")
	seq := seqPath(w.bridge, "s1")
	writeJSON(seq, &seqFile{Sequences: []*sequence{{Steps: []*seqStep{{N: 1, Kind: "run", Run: "r", Status: "pending"}}}}})
	marker := movedPath(w.bridge, "s1")
	write(t, marker, "s2\n")
	past := time.Now().Add(-oldAge)
	os.Chtimes(seq, past, past)
	os.Chtimes(marker, past, past)
	dropDeadLinks(w.bridge)
	if !exists(marker) || !allExist(append(files, seq)) {
		t.Error("the marker must stay while its session has an unfinished sequence")
	}
}

func TestGCEmptyOrMissingScratchIsNotAFailure(t *testing.T) {
	w := newWorld(t)
	empty := fabricate(t, w, "20240101-000001-supernova", "success", true)
	if err := os.Remove(filepath.Join(empty, "scratch", "note.txt")); err != nil {
		t.Fatal(err)
	}
	missing := fabricate(t, w, "20240101-000002-supernova", "success", true)
	if err := os.RemoveAll(filepath.Join(missing, "scratch")); err != nil {
		t.Fatal(err)
	}
	if res := gcRuns(w.bridge, 2, 0); res.ScratchFailed != 0 || res.Scratch != 0 {
		t.Fatalf("want no scratch work: %+v", res)
	}
}

func TestSessionStartNoFailuresSaysNothingFailed(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	fabricate(t, w, "20240101-000002-supernova", "success", true)
	show := sessionStartLine(t, w)
	if !strings.Contains(show, "cleaned up 1 old runs") || strings.Contains(show, "could not") {
		t.Errorf("start line: %q", show)
	}
}

func TestCmdGCNoFailuresSaysNothingFailed(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdGC([]string{"0"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "could not") {
		t.Errorf("output: %q", out.String())
	}
}

func sessionStartLine(t *testing.T, w *world) string {
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "keep-runs: 1\ncontrol-plane:\n  path: "+mustRoot(t)+"\n")
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "abc"})
	var out, errb bytes.Buffer
	if code := cmdHook("SessionStart", bytes.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	show, _ := sessionStartReply(t, out.Bytes())
	return show
}

// captureStderr returns what fn wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = wr
	fn()
	os.Stderr = old
	wr.Close()
	data, _ := io.ReadAll(r)
	r.Close()
	return string(data)
}

// runGC runs `ensign gc` from the world's bridge and returns what it printed and its error.
func runGC(t *testing.T, w *world, args ...string) (string, error) {
	t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	var err error
	captureStderr(t, func() { err = cmdGC(args, &out) })
	return out.String(), err
}

// failSeam replaces a gc file operation for one test.
func failSeam[T any](t *testing.T, seam *T, fake T) {
	t.Helper()
	old := *seam
	*seam = fake
	t.Cleanup(func() { *seam = old })
}

var errSeam = errors.New("seam failure")

func TestGCScratchItemThatCannotBeRemovedFails(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	failSeam(t, &gcRemoveAll, func(string) error { return errSeam })
	if show := sessionStartLine(t, w); !strings.Contains(show, "could not empty 1 scratch folders") {
		t.Errorf("start line: %q", show)
	}
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
	if res.ScratchFailed != 1 || res.Scratch != 0 || !strings.Contains(stderr, "warning: could not empty") {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
	out, err := runGC(t, w, "1")
	if err == nil || !strings.Contains(out, ", could not empty 1 scratch folders") {
		t.Errorf("err = %v, output = %q", err, out)
	}
}

func TestGCRunThatCannotBeDeletedFails(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	fabricate(t, w, "20240101-000002-supernova", "success", true)
	failSeam(t, &gcRemoveRun, func(string, time.Time) {})
	if res := gcRuns(w.bridge, 1, 0); res.Failed != 1 || res.Removed != 0 {
		t.Errorf("res = %+v", res)
	}
	out, err := runGC(t, w, "1")
	if err == nil || !strings.Contains(out, ", could not delete 1 runs") {
		t.Errorf("err = %v, output = %q", err, out)
	}
}

func TestGCReportsHeldSessionsThatCannotBeListed(t *testing.T) {
	w := newWorld(t)
	a := fabricate(t, w, "20240101-000001-supernova", "success", true)
	fabricate(t, w, "20240101-000002-supernova", "success", true)
	bad := seqPath(w.bridge, "bad")
	write(t, bad, "{not json")
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
	if !res.HeldFailed || res.Removed != 0 || !exists(a) {
		t.Errorf("res = %+v", res)
	}
	if !strings.Contains(stderr, "warning: could not list the held sessions, so no run was deleted and no scratch folder emptied: ") || !strings.Contains(stderr, bad) {
		t.Errorf("stderr = %q", stderr)
	}
	out, err := runGC(t, w, "1")
	if err == nil || !strings.Contains(out, ", could not list the held sessions") {
		t.Errorf("err = %v, output = %q", err, out)
	}
	if show := sessionStartLine(t, w); !strings.Contains(show, " · could not list the held sessions") {
		t.Errorf("start line: %q", show)
	}
}

func TestGCReportsSessionsFolderThatCannotBeListed(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "success", true)
	sess := filepath.Join(w.bridge, ".starship", "sessions")
	os.MkdirAll(sess, 0o755)
	failSeam(t, &gcReadDir, func(dir string) ([]os.DirEntry, error) {
		if dir == sess {
			return nil, errSeam
		}
		return os.ReadDir(dir)
	})
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 500, 0) })
	if !res.SessionsListFailed || !strings.Contains(stderr, "warning: could not list the session files in "+sess) {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
	out, err := runGC(t, w, "500")
	if err == nil || !strings.Contains(out, ", could not list the session files") {
		t.Errorf("err = %v, output = %q", err, out)
	}
	if show := sessionStartLine(t, w); !strings.Contains(show, " · could not list the session files") {
		t.Errorf("start line: %q", show)
	}
}

func TestGCMissingSessionsFolderReportsNothing(t *testing.T) {
	w := newWorld(t)
	os.RemoveAll(filepath.Join(w.bridge, ".starship", "sessions"))
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 500, 0) })
	if res.failed() || stderr != "" {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
}

func TestGCSessionFileThatCannotBeRemovedFails(t *testing.T) {
	sess := func(w *world) string { return filepath.Join(w.bridge, ".starship", "sessions") }
	cases := map[string]func(t *testing.T, w *world){
		"stale pid record": func(t *testing.T, w *world) {
			oldFile(t, filepath.Join(sess(w), "7.pid"), 2*pidRecordAge)
		},
		"stale gc file": func(t *testing.T, w *world) {
			oldFile(t, filepath.Join(sess(w), "old.gc"), time.Hour)
		},
		"leftover tmp": func(t *testing.T, w *world) {
			oldFile(t, filepath.Join(sess(w), "s1.tmp"), oldAge)
		},
		"leftover lock": func(t *testing.T, w *world) {
			oldFile(t, filepath.Join(sess(w), "s1.lock"), oldAge)
		},
		"dead link": func(t *testing.T, w *world) {
			write(t, filepath.Join(sess(w), "dead"), "20240101-000009-supernova\n")
		},
	}
	// Each surface gets a fresh world because a pass consumes the leftover it fails on or creates the lock beside it.
	for name, setup := range cases {
		failing := func(string) bool { return true }
		if name == "leftover tmp" {
			failing = func(path string) bool { return strings.HasSuffix(path, ".tmp") }
		}
		fresh := func(t *testing.T) *world {
			w := newWorld(t)
			setup(t, w)
			failSeam(t, &gcRemove, func(path string) error {
				if failing(path) {
					return errSeam
				}
				return os.Remove(path)
			})
			return w
		}
		t.Run(name, func(t *testing.T) {
			w := fresh(t)
			var res gcResult
			stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 500, 0) })
			if res.SessionFilesFailed != 1 || !strings.Contains(stderr, "warning: could not remove ") {
				t.Errorf("res = %+v, stderr = %q", res, stderr)
			}
			w = fresh(t)
			out, err := runGC(t, w, "500")
			if err == nil || !strings.Contains(out, ", could not remove 1 session files") {
				t.Errorf("err = %v, output = %q", err, out)
			}
			w = fresh(t)
			if show := sessionStartLine(t, w); !strings.Contains(show, " · could not remove 1 session files") {
				t.Errorf("start line: %q", show)
			}
		})
	}
}

func TestGCSessionFileAlreadyGoneIsNotAFailure(t *testing.T) {
	w := newWorld(t)
	sess := filepath.Join(w.bridge, ".starship", "sessions")
	oldFile(t, filepath.Join(sess, "7.pid"), 2*pidRecordAge)
	write(t, filepath.Join(sess, "dead"), "20240101-000009-supernova\n")
	failSeam(t, &gcRemove, func(string) error { return fs.ErrNotExist })
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 500, 0) })
	if res.failed() || res.Links != 0 || stderr != "" {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
}

func TestGCDeadLinkThatCannotBeRenamedFails(t *testing.T) {
	w := newWorld(t)
	link := filepath.Join(w.bridge, ".starship", "sessions", "dead")
	write(t, link, "20240101-000009-supernova\n")
	failSeam(t, &gcRename, func(string, string) error { return errSeam })
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 500, 0) })
	if res.SessionFilesFailed != 1 || res.Links != 0 || !exists(link) || !strings.Contains(stderr, "warning: could not rename "+link) {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
}

func TestGCRestoreThatFailsWarnsAndCountsOnce(t *testing.T) {
	w := newWorld(t)
	fabricate(t, w, "20240101-000001-supernova", "running", true)
	link := filepath.Join(w.bridge, ".starship", "sessions", "rebound")
	write(t, link, "20240101-000009-supernova\n")
	calls := 0
	failSeam(t, &gcRename, func(old, new string) error {
		calls++
		if calls > 1 {
			return errSeam
		}
		// The session rebinds to a live run right after the link is renamed away.
		err := os.Rename(old, new)
		write(t, new, "20240101-000001-supernova\n")
		return err
	})
	var sw linkSweep
	stderr := captureStderr(t, func() { sw = dropDeadLinks(w.bridge) })
	if sw.Failed != 1 || sw.Links != 0 || !strings.Contains(stderr, "warning: could not restore session link "+link) {
		t.Errorf("sweep = %+v, stderr = %q", sw, stderr)
	}
	if exists(link + ".gc") {
		t.Error("the renamed link must not be left behind")
	}
}

func testSweep() *sessionSweep { return &sessionSweep{deadline: time.Now().Add(time.Second)} }

func TestReapSessionRenameFailureWarnsAndCounts(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	failSeam(t, &gcRename, func(string, string) error { return errSeam })
	s := testSweep()
	stderr := captureStderr(t, func() { reapSession(dir, "s1", []string{filepath.Base(files[0])}, s) })
	if s.failed != 1 || !strings.Contains(stderr, "warning: could not rename") || !exists(files[0]) || exists(files[0]+".gc") {
		t.Errorf("failed = %d, stderr = %q", s.failed, stderr)
	}
	failSeam(t, &gcRename, func(string, string) error { return fs.ErrNotExist })
	s = testSweep()
	stderr = captureStderr(t, func() { reapSession(dir, "s1", []string{filepath.Base(files[0])}, s) })
	if s.failed != 0 || stderr != "" {
		t.Errorf("a vanished file is no failure: failed = %d, stderr = %q", s.failed, stderr)
	}
}

func TestSettleSessionStaleRemoveFailure(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	files := sessFiles(t, w, "s1", oldAge, "")
	if err := os.Rename(files[0], files[0]+".gc"); err != nil {
		t.Fatal(err)
	}
	failSeam(t, &gcRemove, func(string) error { return errSeam })
	s := testSweep()
	stderr := captureStderr(t, func() { settleSession(dir, "s1", []string{filepath.Base(files[0])}, []string{files[0]}, s) })
	if s.failed != 1 || !strings.Contains(stderr, "warning: could not remove") {
		t.Errorf("failed = %d, stderr = %q", s.failed, stderr)
	}
	failSeam(t, &gcRemove, func(string) error { return fs.ErrNotExist })
	s = testSweep()
	stderr = captureStderr(t, func() { settleSession(dir, "s1", []string{filepath.Base(files[0])}, []string{files[0]}, s) })
	if s.failed != 0 || stderr != "" {
		t.Errorf("failed = %d, stderr = %q", s.failed, stderr)
	}
}

func TestSettleSessionNotStale(t *testing.T) {
	setup := func() (string, []string, string) {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		files := sessFiles(t, w, "s1", oldAge, "")
		if err := os.Rename(files[0], files[0]+".gc"); err != nil {
			t.Fatal(err)
		}
		write(t, files[1], "new\n")
		return dir, files, filepath.Base(files[0])
	}

	dir, files, name := setup()
	failSeam(t, &gcRename, func(string, string) error { return errSeam })
	s := testSweep()
	stderr := captureStderr(t, func() { settleSession(dir, "s1", []string{name}, []string{files[0]}, s) })
	if s.failed != 1 || !strings.Contains(stderr, "warning: could not rename") || !exists(files[0]+".gc") || exists(files[0]) {
		t.Errorf("a failed restore must keep the .gc: failed = %d, stderr = %q", s.failed, stderr)
	}

	dir, files, name = setup()
	write(t, files[0], "newer\n")
	s = testSweep()
	settleSession(dir, "s1", []string{name}, []string{files[0]}, s)
	if got, _ := os.ReadFile(files[0]); string(got) != "newer\n" || exists(files[0]+".gc") || s.failed != 0 {
		t.Errorf("a superseded file must lose its .gc: content %q, failed %d", got, s.failed)
	}

	gcRename = os.Rename
	dir, files, name = setup()
	s = testSweep()
	stderr = captureStderr(t, func() { settleSession(dir, "s1", []string{name}, []string{files[0]}, s) })
	if s.failed != 0 || stderr != "" || !exists(files[0]) || exists(files[0]+".gc") {
		t.Errorf("a restore must count nothing: failed %d, stderr %q", s.failed, stderr)
	}
}

// infoFailEntry is a directory entry whose Info fails.
type infoFailEntry struct {
	os.DirEntry
	err error
}

func (e infoFailEntry) Info() (fs.FileInfo, error) { return nil, e.err }

func TestDropDeadLinksEntryInfoFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{errSeam, 1}, {fs.ErrNotExist, 0}} {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		oldFile(t, filepath.Join(dir, "123.pid"), pidRecordAge+time.Hour)
		oldFile(t, filepath.Join(dir, "x.gc"), staleGCAge+time.Hour)
		failSeam(t, &gcReadDir, func(d string) ([]os.DirEntry, error) {
			entries, err := os.ReadDir(d)
			for i, e := range entries {
				if strings.HasSuffix(e.Name(), ".gc") || strings.HasSuffix(e.Name(), ".pid") {
					entries[i] = infoFailEntry{e, tc.err}
				}
			}
			return entries, err
		})
		var sw linkSweep
		stderr := captureStderr(t, func() { sw = dropDeadLinks(w.bridge) })
		if sw.Failed != 2*tc.want || (tc.want == 0) != (stderr == "") || (tc.want > 0 && !strings.Contains(stderr, "warning: could not read")) {
			t.Errorf("%v: failed = %d, stderr = %q", tc.err, sw.Failed, stderr)
		}
	}
}

func TestGCRemovesARunWithoutAPipelineInsteadOfReportingIt(t *testing.T) {
	setup := func() (*world, string) {
		w := newWorld(t)
		old := fabricate(t, w, "20240101-000001-supernova", "success", true)
		fabricate(t, w, "20240101-000002-supernova", "success", true)
		if err := os.Remove(filepath.Join(old, "pipeline.json")); err != nil {
			t.Fatal(err)
		}
		return w, old
	}
	w, old := setup()
	var res gcResult
	stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 1) })
	if res.OpenFailed != 0 || res.failed() || res.Failed != 0 || res.More || res.Removed != 1 || exists(old) || stderr != "" {
		t.Errorf("res = %+v, stderr = %q", res, stderr)
	}
	w, old = setup()
	out, err := runGC(t, w, "1")
	if err != nil || strings.Contains(out, "could not open") || exists(old) {
		t.Errorf("err = %v, output = %q", err, out)
	}
	w, _ = setup()
	if show := sessionStartLine(t, w); strings.Contains(show, "could not open") {
		t.Errorf("start line: %q", show)
	}
}

func TestGCRunDeleteBudgetIsSpentOnlyByFailedDeletes(t *testing.T) {
	const spent = 40 * time.Millisecond
	for _, tc := range []struct {
		name   string
		remove bool
	}{{"failed", false}, {"succeeded", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
			fabricate(t, w, "20240101-000002-supernova", "success", true)
			fabricate(t, w, "20240101-000003-supernova", "success", true)
			fabricate(t, w, "20240101-000004-supernova", "success", true)
			var left []time.Duration
			failSeam(t, &gcRemoveRun, func(dir string, d time.Time) {
				left = append(left, time.Until(d))
				time.Sleep(spent)
				if tc.remove {
					os.RemoveAll(dir)
				}
			})
			gcRuns(w.bridge, 1, 0)
			if len(left) != 3 {
				t.Fatalf("left = %v", left)
			}
			for i := 1; i < len(left); i++ {
				shrunk := left[i-1] - left[i]
				if tc.remove && shrunk > spent/2 {
					t.Errorf("a successful delete must not shrink the budget: %v", left)
				}
				if !tc.remove && shrunk < spent/2 {
					t.Errorf("a failed delete must shrink the budget: %v", left)
				}
			}
		})
	}
}

// removeSiblings is a gcRemoveRun that deletes its own folder and every other run folder, so the next candidate has vanished by the time gc reaches it.
func removeSiblings(dir string, _ time.Time) {
	parent := filepath.Dir(dir)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(parent, e.Name()))
	}
}

func TestGCVanishedCandidateIsNotReportedAsUnopenable(t *testing.T) {
	for _, name := range []string{"openRun", "TryLock"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if name == "openRun" {
				for i := 1; i <= 3; i++ {
					fabricate(t, w, fmt.Sprintf("2024010%d-000001-supernova", i), "success", true)
				}
			} else {
				pipelineOnlyRun(t, w, "20240101-000001-supernova", pipelineOnlyAge+time.Minute)
				pipelineOnlyRun(t, w, "20240102-000001-supernova", pipelineOnlyAge+time.Minute)
			}
			failSeam(t, &gcRemoveRun, removeSiblings)
			var res gcResult
			stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 1, 0) })
			if res.OpenFailed != 0 || res.failed() || stderr != "" {
				t.Errorf("res = %+v, stderr = %q", res, stderr)
			}
		})
	}
}

// statFailing fails a Stat for the paths with the suffix and passes the rest through.
func statFailing(suffix string, err error) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		if strings.HasSuffix(p, suffix) {
			return nil, err
		}
		return os.Stat(p)
	}
}

func lockFailing(suffix string, ok bool, err error) func(*flock.Flock) (bool, error) {
	return func(l *flock.Flock) (bool, error) {
		if strings.HasSuffix(l.Path(), suffix) {
			return ok, err
		}
		return l.TryLock()
	}
}

func readEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestDropDeadLinksLockErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		err  error
		want int
	}{{"error", false, errSeam, 1}, {"missing", false, fs.ErrNotExist, 0}, {"held", false, nil, 0}} {
		w := newWorld(t)
		failSeam(t, &gcTryLock, lockFailing(".sweep.lock", tc.ok, tc.err))
		var sw linkSweep
		stderr := captureStderr(t, func() { sw = dropDeadLinks(w.bridge) })
		if sw.Failed != tc.want || (tc.want == 0) != (stderr == "") || (tc.want > 0 && !strings.Contains(stderr, "could not lock")) {
			t.Errorf("%s: failed = %d, stderr = %q", tc.name, sw.Failed, stderr)
		}
	}
}

func TestDropDeadLinksMissingSessionsFolderIsSilent(t *testing.T) {
	w := newWorld(t)
	os.RemoveAll(filepath.Join(w.bridge, ".starship", "sessions"))
	var sw linkSweep
	stderr := captureStderr(t, func() { sw = dropDeadLinks(w.bridge) })
	if sw.Failed != 0 || sw.ListFailed || stderr != "" {
		t.Errorf("sw = %+v, stderr = %q", sw, stderr)
	}
}

func TestSweepSessionFilesLockErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		err  error
		want int
	}{{"error", false, errSeam, 1}, {"missing", false, fs.ErrNotExist, 0}, {"held", false, nil, 0}} {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		files := sessFiles(t, w, "s1", oldAge, "")
		oldFile(t, seqPath(w.bridge, "s1"), oldAge)
		oldFile(t, filepath.Join(dir, "s2.lock"), oldAge)
		failSeam(t, &gcTryLock, lockFailing(".lock", tc.ok, tc.err))
		s := testSweep()
		stderr := captureStderr(t, func() { sweepSessionFiles(w.bridge, readEntries(t, dir), s) })
		// One failure for the .sequence.lock of s1 and one for the .lock of s2.
		if s.failed != 2*tc.want || (tc.want == 0) != (stderr == "") || (tc.want > 0 && !strings.Contains(stderr, "could not lock")) {
			t.Errorf("%s: failed = %d, stderr = %q", tc.name, s.failed, stderr)
		}
		if !allExist(files) || !exists(filepath.Join(dir, "s2.lock")) {
			t.Errorf("%s: a session whose lock failed must be skipped", tc.name)
		}
	}
}

func TestSweepSessionFilesInfoFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{errSeam, 1}, {fs.ErrNotExist, 0}} {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		files := sessFiles(t, w, "s1", oldAge, "")
		oldFile(t, filepath.Join(dir, "s1.lock"), oldAge)
		entries := readEntries(t, dir)
		for i, e := range entries {
			if e.Name() == "s1.task" {
				entries[i] = infoFailEntry{e, tc.err}
			}
		}
		s := testSweep()
		stderr := captureStderr(t, func() { sweepSessionFiles(w.bridge, entries, s) })
		if s.failed != tc.want || (tc.want == 0) != (stderr == "") || (tc.want > 0 && !strings.Contains(stderr, "could not read")) {
			t.Errorf("%v: failed = %d, stderr = %q", tc.err, s.failed, stderr)
		}
		if !allExist(files) || !exists(filepath.Join(dir, "s1.lock")) {
			t.Errorf("%v: the group must be skipped", tc.err)
		}
	}
}

func TestLeftoverGroupsInfoFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{errSeam, 1}, {fs.ErrNotExist, 0}} {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		oldFile(t, filepath.Join(dir, "s1.lock"), oldAge)
		entries := readEntries(t, dir)
		for i, e := range entries {
			if e.Name() == "s1.lock" {
				entries[i] = infoFailEntry{e, tc.err}
			}
		}
		s := testSweep()
		var groups map[string]*sweptLeftovers
		stderr := captureStderr(t, func() { groups = leftoverGroups(dir, entries, s) })
		if s.failed != tc.want || (tc.want == 0) != (stderr == "") || !groups["s1"].failed {
			t.Errorf("%v: failed = %d, stderr = %q", tc.err, s.failed, stderr)
		}
		sweepLeftovers(w.bridge, groups, s, func() ([]runInfo, error) { return nil, nil })
		if !exists(filepath.Join(dir, "s1.lock")) {
			t.Errorf("%v: the group must be skipped", tc.err)
		}
	}
}

func TestSweepLeftoversLockErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		err  error
		want int
	}{{"error", false, errSeam, 1}, {"missing", false, fs.ErrNotExist, 0}, {"held", false, nil, 0}} {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		oldFile(t, filepath.Join(dir, "s1.lock"), oldAge)
		s := testSweep()
		groups := leftoverGroups(dir, readEntries(t, dir), s)
		failSeam(t, &gcTryLock, lockFailing(".lock", tc.ok, tc.err))
		stderr := captureStderr(t, func() {
			sweepLeftovers(w.bridge, groups, s, func() ([]runInfo, error) { return nil, nil })
		})
		if s.failed != tc.want || (tc.want == 0) != (stderr == "") || !exists(filepath.Join(dir, "s1.lock")) {
			t.Errorf("%s: failed = %d, stderr = %q", tc.name, s.failed, stderr)
		}
	}
}

func TestSettleSessionStatFailure(t *testing.T) {
	setup := func(t *testing.T) (string, []string) {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		files := sessFiles(t, w, "s1", oldAge, "")
		if err := os.Rename(files[0], files[0]+".gc"); err != nil {
			t.Fatal(err)
		}
		return dir, files
	}
	dir, files := setup(t)
	failSeam(t, &gcStat, statFailing("s1.pin", errSeam))
	s := testSweep()
	stderr := captureStderr(t, func() { settleSession(dir, "s1", baseNames(files), []string{files[0]}, s) })
	if s.failed != 1 || !strings.Contains(stderr, "could not read") || !exists(files[1]) {
		t.Errorf("failed = %d, stderr = %q", s.failed, stderr)
	}
	if exists(files[0]+".gc") && !exists(files[0]) || !exists(files[0]) && !exists(files[0]+".gc") {
		t.Error("a failed read must lose no file")
	}

	dir, files = setup(t)
	failSeam(t, &gcStat, statFailing("s1.pin", fs.ErrNotExist))
	s = testSweep()
	stderr = captureStderr(t, func() { settleSession(dir, "s1", baseNames(files), []string{files[0]}, s) })
	if s.failed != 0 || stderr != "" || exists(files[0]+".gc") || exists(files[0]) {
		t.Errorf("a missing file is not fresh: failed = %d, stderr = %q", s.failed, stderr)
	}
}

func baseNames(files []string) []string {
	var ns []string
	for _, f := range files {
		ns = append(ns, filepath.Base(f))
	}
	return ns
}

func TestGCRunsStateStatFailureKeepsFolder(t *testing.T) {
	for _, tc := range []struct {
		err         error
		wantRemoved bool
	}{{errSeam, false}, {fs.ErrNotExist, true}, {nil, true}} {
		w := newWorld(t)
		dir := filepath.Join(runsDir(w.bridge), "20240101-000001-supernova")
		oldFile(t, filepath.Join(dir, "pipeline.json"), pipelineOnlyAge+time.Hour)
		if tc.err != nil {
			failSeam(t, &gcStat, statFailing("state.json", tc.err))
		}
		var res gcResult
		stderr := captureStderr(t, func() { res = gcRuns(w.bridge, 10, 0) })
		if tc.wantRemoved {
			if exists(dir) || res.OpenFailed != 0 || stderr != "" {
				t.Errorf("%v: res = %+v, stderr = %q", tc.err, res, stderr)
			}
		} else if !exists(dir) || res.OpenFailed != 1 || !strings.Contains(stderr, "could not open run folder") {
			t.Errorf("%v: res = %+v, stderr = %q", tc.err, res, stderr)
		}
	}
}

func TestKeptGCRetry(t *testing.T) {
	// Each case has a live session s1 (a fresh .pin) unless stale is set, and an old s1.task.gc.
	setup := func(t *testing.T, stale bool, gcAge time.Duration) (string, string) {
		w := newWorld(t)
		dir := filepath.Join(w.bridge, ".starship", "sessions")
		pin := filepath.Join(dir, "s1.pin")
		if stale {
			oldFile(t, pin, oldAge)
		} else {
			write(t, pin, "x\n")
		}
		gc := filepath.Join(dir, "s1.task.gc")
		write(t, gc, "kept\n")
		past := time.Now().Add(-gcAge)
		os.Chtimes(gc, past, past)
		return w.bridge, filepath.Join(dir, "s1.task")
	}
	sweep := func(bridge string) (linkSweep, string) {
		var sw linkSweep
		stderr := captureStderr(t, func() { sw = dropDeadLinks(bridge) })
		return sw, stderr
	}
	readTask := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	bridge, task := setup(t, false, oldAge)
	sw, stderr := sweep(bridge)
	if readTask(task) != "kept\n" || exists(task+".gc") || sw.Failed != 0 || stderr != "" {
		t.Errorf("restore: task %q, sw %+v, stderr %q", readTask(task), sw, stderr)
	}

	bridge, task = setup(t, false, oldAge)
	write(t, task, "newer\n")
	sw, stderr = sweep(bridge)
	if readTask(task) != "newer\n" || exists(task+".gc") || sw.Failed != 0 || stderr != "" {
		t.Errorf("recreated: task %q, sw %+v, stderr %q", readTask(task), sw, stderr)
	}

	bridge, task = setup(t, true, oldAge)
	sw, stderr = sweep(bridge)
	if exists(task) || exists(task+".gc") || sw.Failed != 0 {
		t.Errorf("stale: sw %+v, stderr %q", sw, stderr)
	}

	bridge, task = setup(t, false, oldAge)
	failSeam(t, &gcRename, func(string, string) error { return errSeam })
	sw, stderr = sweep(bridge)
	if !exists(task+".gc") || exists(task) || sw.Failed != 1 || !strings.Contains(stderr, "could not rename") {
		t.Errorf("failed retry: sw %+v, stderr %q", sw, stderr)
	}
	gcRename = os.Rename

	bridge, task = setup(t, false, oldAge)
	failSeam(t, &gcStat, func(string) (fs.FileInfo, error) { return nil, errSeam })
	sw, stderr = sweep(bridge)
	if !exists(task+".gc") || exists(task) || sw.Failed == 0 || !strings.Contains(stderr, "could not read") {
		t.Errorf("failed stat: sw %+v, stderr %q", sw, stderr)
	}
	gcStat = os.Stat

	bridge, task = setup(t, true, oldAge)
	oldFile(t, filepath.Join(filepath.Dir(task), "s1.sequence"), oldAge)
	failSeam(t, &gcTryLock, lockFailing(".sequence.lock", false, nil))
	sw, stderr = sweep(bridge)
	if !exists(task+".gc") || sw.Failed != 0 || stderr != "" {
		t.Errorf("held sequence lock: sw %+v, stderr %q", sw, stderr)
	}
	gcTryLock = func(l *flock.Flock) (bool, error) { return l.TryLock() }

	bridge, task = setup(t, false, oldAge)
	dead := filepath.Join(filepath.Dir(task), "deadid.gc")
	oldFile(t, dead, staleGCAge+time.Hour)
	sweep(bridge)
	if exists(dead) {
		t.Error("a dead-link .gc must still go by age")
	}

	bridge, task = setup(t, false, time.Minute)
	sweep(bridge)
	if !exists(task+".gc") || exists(task) {
		t.Error("a .gc under staleGCAge must be left alone")
	}
}

// seedRecords writes a task's publish records and the files a sweep must leave, and returns the records.
func seedRecords(t *testing.T, w *world, task string) (records, keepers []string) {
	t.Helper()
	for _, f := range []string{taskStateFile(w.bridge, task, "", ".publish"), taskStateFile(w.bridge, task, "", ".tested"), taskStateFile(w.bridge, task, "client", ".tested")} {
		write(t, f, "x\n")
		records = append(records, f)
	}
	for _, f := range []string{taskStateFile(w.bridge, task, "", ".publish.lock"), taskStateFile(w.bridge, task, "", ".base"), taskStateFile(w.bridge, task, "client", ".base")} {
		write(t, f, "x\n")
		keepers = append(keepers, f)
	}
	return records, keepers
}

func wantGone(t *testing.T, files []string, gone bool) {
	t.Helper()
	for _, f := range files {
		if exists(f) == gone {
			t.Errorf("%s: gone=%v, want %v", f, !exists(f), gone)
		}
	}
}

func closedTask(w *world, name string) { w.frontTask(name, "status: closed\n") }

func TestGCSweepsAClosedTasksPublishRecords(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "a")
	w.frontTask("o", "status: open\n")
	gone, keep := seedRecords(t, w, "supernova/a")
	open, _ := seedRecords(t, w, "supernova/o")
	res := gcRuns(w.bridge, 5, 0)
	if res.Records != 3 || res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, gone, true)
	wantGone(t, keep, false)
	wantGone(t, open, false)
}

func TestGCSweepNeverClaimsAnotherTasksEncoding(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "b")
	w.frontTask("b--c", "status: open\n")
	gone, _ := seedRecords(t, w, "supernova/b")
	peer, _ := seedRecords(t, w, "supernova/b--c")
	if res := gcRuns(w.bridge, 5, 0); res.Records != 3 || res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, gone, true)
	wantGone(t, peer, false)
}

func TestGCSweepKeepsAnEncodingAnOpenTaskShares(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "b--c")
	write(t, filepath.Join(w.bridge, "projects/supernova--b/tasks/c/task.md"), "---\nstatus: open\n---\n\n# c\n")
	records, _ := seedRecords(t, w, "supernova/b--c")
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, records, false)
}

func TestGCSweepLeavesRecordsOfTasksThatAreGone(t *testing.T) {
	w := newWorld(t)
	records, _ := seedRecords(t, w, "supernova/ghost")
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, records, false)
}

func TestGCSweepKeepsRecordsOfATaskWithALiveRun(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "a")
	records, _ := seedRecords(t, w, "supernova/a")
	dir := fabricate(t, w, "20240101-000001-supernova", "pending", true)
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), &Snapshot{Task: "supernova/a"}); err != nil {
		t.Fatal(err)
	}
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, records, false)
	if err := writeJSON(filepath.Join(dir, "state.json"), &State{Run: "20240101-000001-supernova", Status: "success", Dispatches: []*Dispatch{}, History: []HistoryEntry{}}); err != nil {
		t.Fatal(err)
	}
	if res := gcRuns(w.bridge, 5, 0); res.Records != 3 || res.failed() {
		t.Errorf("result after the run ended: %+v", res)
	}
	wantGone(t, records, true)
}

func TestGCSweepSkipsABusyPublishLockQuietly(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "a")
	records, _ := seedRecords(t, w, "supernova/a")
	holdPublishLock(t, w, "supernova/a")
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, records, false)
}

func TestGCSweepCountsALockError(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "a")
	records, _ := seedRecords(t, w, "supernova/a")
	failSeam(t, &gcTryLock, lockFailing(".publish.lock", false, errSeam))
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.RecordsFailed != 1 || !res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, records, false)
}

func TestGCSweepRetriesAFailedRemoval(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "a")
	records, _ := seedRecords(t, w, "supernova/a")
	old := gcRemove
	gcRemove = func(p string) error {
		if strings.HasSuffix(p, ".publish") {
			return errSeam
		}
		return old(p)
	}
	res := gcRuns(w.bridge, 5, 0)
	gcRemove = old
	if res.Records != 2 || res.RecordsFailed != 1 || !res.failed() || !exists(records[0]) {
		t.Errorf("result: %+v", res)
	}
	if res := gcRuns(w.bridge, 5, 0); res.Records != 1 || res.failed() {
		t.Errorf("next gc: %+v", res)
	}
	wantGone(t, records, true)
}

func TestGCSweepKeepsRecordsOfAnUnreadableTask(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(filepath.Join(w.bridge, "projects/supernova/tasks/a/task.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	records, _ := seedRecords(t, w, "supernova/a")
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.RecordsFailed != 1 || !res.failed() {
		t.Errorf("result: %+v", res)
	}
	wantGone(t, records, false)
}

func TestGCSweepListing(t *testing.T) {
	w := newWorld(t)
	closedTask(w, "a")
	records, _ := seedRecords(t, w, "supernova/a")
	old := gcReadDir
	gcReadDir = func(p string) ([]os.DirEntry, error) {
		if filepath.Base(p) == "tasks" && filepath.Base(filepath.Dir(p)) == ".starship" {
			return nil, errSeam
		}
		return old(p)
	}
	res := gcRuns(w.bridge, 5, 0)
	gcReadDir = old
	if res.Records != 0 || res.RecordsFailed != 1 || !res.failed() {
		t.Errorf("failed listing: %+v", res)
	}
	wantGone(t, records, false)
	if err := os.RemoveAll(filepath.Join(w.bridge, ".starship", "tasks")); err != nil {
		t.Fatal(err)
	}
	if res := gcRuns(w.bridge, 5, 0); res.Records != 0 || res.failed() {
		t.Errorf("missing folder: %+v", res)
	}
}

func TestSessionStartSweepsClosedTasksRecords(t *testing.T) {
	w := newWorld(t)
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	closedTask(w, "a")
	records, _ := seedRecords(t, w, "supernova/a")
	in, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "abc"})
	var out, errb bytes.Buffer
	if code := cmdHook("SessionStart", bytes.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	show, _ := sessionStartReply(t, out.Bytes())
	if !strings.Contains(show, "removed 3 publish records of closed tasks") {
		t.Errorf("summary: %q", show)
	}
	wantGone(t, records, true)
}
