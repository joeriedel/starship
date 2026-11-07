// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// inBridge runs f with the bridge as the working directory.
func (w *world) inBridge(f func() error) error {
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	return f()
}

func (w *world) runClear(args ...string) (string, error) {
	var out bytes.Buffer
	err := w.inBridge(func() error { return cmdRunClear(args, &out) })
	return out.String(), err
}

// runFiles reads every file in a run folder except the lock files, which flock creates.
func runFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "lock") {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		files[e.Name()] = string(data)
	}
	return files
}

const clearID = "20240101-000002-supernova"

func TestRunClearEndsAnUnreadableRun(t *testing.T) {
	cases := []struct {
		name      string
		plant     func(t *testing.T, w *world, dir string)
		corrupt   bool // state.json is kept as state.json.corrupt
		openAfter bool // pipeline.json is good, so openRun reads the run
	}{
		{"corrupt state", func(t *testing.T, w *world, dir string) {
			write(t, filepath.Join(dir, "state.json"), "{not json")
		}, true, true},
		{"live state, corrupt pipeline", func(t *testing.T, w *world, dir string) {
			write(t, filepath.Join(dir, "pipeline.json"), "{not json")
		}, false, false},
		{"live state, missing pipeline", func(t *testing.T, w *world, dir string) {
			os.Remove(filepath.Join(dir, "pipeline.json"))
		}, false, false},
		{"both bad", func(t *testing.T, w *world, dir string) {
			write(t, filepath.Join(dir, "pipeline.json"), "{not json")
			write(t, filepath.Join(dir, "state.json"), "{not json")
		}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			t.Setenv("STARSHIP_SESSION", "")
			dir := plantRun(t, w.bridge, clearID, "running")
			writeJSON(filepath.Join(dir, "state.json"), &State{Run: clearID, Session: "s1", Status: "running", Approved: true})
			if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, "s1")), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeSessionRuns(w.bridge, "s1", []string{clearID}); err != nil {
				t.Fatal(err)
			}
			c.plant(t, w, dir)
			if _, err := listRuns(w.bridge); err == nil {
				t.Fatal("the planted run should block listRuns")
			}
			out, err := w.runClear(clearID)
			if err != nil {
				t.Fatalf("clear: %v", err)
			}
			for _, want := range []string{"Cleared run " + clearID, dir, "code folders", "not stopped", "must pass again"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if _, err := listRuns(w.bridge); err != nil {
				t.Errorf("listRuns still fails: %v", err)
			}
			if err := link(w.bridge, true, nil); err != nil {
				t.Errorf("link still fails: %v", err)
			}
			var st State
			if err := readRunState(dir, &st); err != nil {
				t.Fatal(err)
			}
			if st.Status != "error" || st.Reason != clearedReason || st.FinishedAt == 0 || !st.terminal() {
				t.Errorf("state %+v", st)
			}
			if retiredWithoutFault(&st) {
				t.Error("a cleared run must count as a failure")
			}
			if c.corrupt && (!st.Started || st.Run != clearID) {
				t.Errorf("a rebuilt state must be Started with its id: %+v", st)
			}
			kept, kerr := os.ReadFile(filepath.Join(dir, "state.json.corrupt"))
			if c.corrupt != (kerr == nil) || (c.corrupt && string(kept) != "{not json") {
				t.Errorf("state.json.corrupt = %q, %v", kept, kerr)
			}
			ids, _ := sessionRuns(w.bridge, "s1")
			if len(ids) != 0 {
				t.Errorf("session links %v", ids)
			}
			if c.openAfter {
				r, err := openRun(w.bridge, clearID)
				if err != nil {
					t.Fatalf("openRun: %v", err)
				}
				r.close()
			}
		})
	}
}

func TestRunClearRefusalsChangeNothing(t *testing.T) {
	corruptState := func(t *testing.T, dir string) { write(t, filepath.Join(dir, "state.json"), "{not json") }
	cases := []struct {
		name  string
		plant func(t *testing.T, w *world, dir string) func() // returns a release for held locks
		id    string
		want  string
	}{
		{"healthy live run", func(t *testing.T, w *world, dir string) func() { return nil }, clearID, "ensign run retire"},
		{"healthy finished run", func(t *testing.T, w *world, dir string) func() {
			writeJSON(filepath.Join(dir, "state.json"), &State{Run: clearID, Status: "success"})
			return nil
		}, clearID, "nothing to clear"},
		{"finished, corrupt pipeline", func(t *testing.T, w *world, dir string) func() {
			writeJSON(filepath.Join(dir, "state.json"), &State{Run: clearID, Status: "error"})
			write(t, filepath.Join(dir, "pipeline.json"), "{not json")
			return nil
		}, clearID, "lifts its hold"},
		{"finished with a fault, bad budgets", func(t *testing.T, w *world, dir string) func() {
			plantBadRun(t, w, clearID, "error", "tests failed", "budgets", 1)
			return nil
		}, clearID, "lifts its hold"},
		{"success, corrupt pipeline", func(t *testing.T, w *world, dir string) func() {
			w.plantUnreadablePipelineRun(clearID, "success", true, 1, false)
			return nil
		}, clearID, "keep-runs"},
		{"readable ask-user", func(t *testing.T, w *world, dir string) func() {
			plantBadRun(t, w, clearID, "ask-user", "which?", "good", 1)
			return nil
		}, clearID, "waiting on a question"},
		{"pipeline only", func(t *testing.T, w *world, dir string) func() {
			os.Remove(filepath.Join(dir, "state.json"))
			return nil
		}, clearID, "5 minutes old"},
		{"neither file", func(t *testing.T, w *world, dir string) func() {
			os.Remove(filepath.Join(dir, "state.json"))
			os.Remove(filepath.Join(dir, "pipeline.json"))
			return nil
		}, clearID, "deleted by hand"},
		{"run lock held", func(t *testing.T, w *world, dir string) func() {
			corruptState(t, dir)
			l := flock.New(filepath.Join(dir, "lock"))
			if ok, err := l.TryLock(); err != nil || !ok {
				t.Fatal("could not take the lock")
			}
			return func() { l.Unlock() }
		}, clearID, "still open"},
		{"dispatch lock held", func(t *testing.T, w *world, dir string) func() {
			corruptState(t, dir)
			l := flock.New(filepath.Join(dir, "dispatch-d1.lock"))
			if ok, err := l.TryLock(); err != nil || !ok {
				t.Fatal("could not take the lock")
			}
			return func() { l.Unlock() }
		}, clearID, "still open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			t.Setenv("STARSHIP_SESSION", "")
			dir := plantRun(t, w.bridge, clearID, "running")
			release := c.plant(t, w, dir)
			if release != nil {
				defer release()
			}
			before := runFiles(t, dir)
			_, err := w.runClear(c.id)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want a refusal with %q, got %v", c.want, err)
			} else {
				if strings.Contains(c.name, "success") && strings.Contains(err.Error(), "lifts its hold") {
					t.Errorf("a success holds nothing: %v", err)
				}
				if strings.Contains(c.name, "readable ask-user") && strings.Contains(err.Error(), "has finished") {
					t.Errorf("an ask-user run has not finished: %v", err)
				}
			}
			after := runFiles(t, dir)
			if len(before) != len(after) {
				t.Errorf("files changed: %v -> %v", before, after)
			}
			for n, data := range before {
				if after[n] != data {
					t.Errorf("%s changed", n)
				}
			}
		})
	}
	w := newWorld(t)
	t.Setenv("STARSHIP_SESSION", "")
	if _, err := w.runClear("20240101-000009-supernova"); err == nil || !strings.Contains(err.Error(), "no run 20240101-000009-supernova") {
		t.Errorf("missing folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runsDir(w.bridge), "20240101-000009-supernova")); err == nil {
		t.Error("clear created the folder")
	}
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "-x"} {
		if _, err := w.runClear(id); err == nil {
			t.Errorf("id %q should be refused", id)
		}
	}
	if _, err := w.runClear(); err == nil {
		t.Error("no argument should be refused")
	}
}

func TestRunRetireAndStopPointAtClear(t *testing.T) {
	w := newWorld(t)
	dir := plantRun(t, w.bridge, clearID, "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	var out bytes.Buffer
	for name, f := range map[string]func(string) error{
		"retire": func(id string) error { return w.inBridge(func() error { return cmdRunRetire([]string{id}, &out) }) },
		"stop":   func(id string) error { return w.inBridge(func() error { return cmdRunStop([]string{id}, &out, &out) }) },
	} {
		if err := f(clearID); err == nil || !strings.Contains(err.Error(), "ensign run clear "+clearID) || !strings.Contains(err.Error(), "state.json") {
			t.Errorf("%s: %v", name, err)
		}
		if err := f("20240101-000009-supernova"); err == nil || !strings.Contains(err.Error(), "no run 20240101-000009-supernova") {
			t.Errorf("%s of a missing run: %v", name, err)
		}
	}
}

func TestRunRetireAndStopAdviceMatchesClear(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, w *world)
		clear bool // the message offers run clear, so clear must succeed
		want  string
	}{
		{"corrupt state", func(t *testing.T, w *world) {
			write(t, filepath.Join(plantRun(t, w.bridge, clearID, "running"), "state.json"), "{not json")
		}, true, "state.json"},
		{"corrupt pipeline, live state", func(t *testing.T, w *world) {
			write(t, filepath.Join(plantRun(t, w.bridge, clearID, "running"), "pipeline.json"), "{not json")
		}, true, "pipeline.json"},
		{"finished, corrupt pipeline", func(t *testing.T, w *world) {
			w.plantUnreadablePipelineRun(clearID, "error", true, 1, false)
		}, false, "keep-runs"},
		{"finished, missing pipeline", func(t *testing.T, w *world) {
			w.plantUnreadablePipelineRun(clearID, "error", true, 1, true)
		}, false, "has finished (error)"},
		{"ask-user, corrupt pipeline", func(t *testing.T, w *world) {
			w.plantUnreadablePipelineRun(clearID, "ask-user", true, 1, false)
		}, true, "waiting on a question"},
		{"ask-user, missing pipeline", func(t *testing.T, w *world) {
			w.plantUnreadablePipelineRun(clearID, "ask-user", true, 1, true)
		}, true, "waiting on a question"},
		{"ask-user, bad budgets", func(t *testing.T, w *world) {
			plantBadRun(t, w, clearID, "ask-user", "which?", "budgets", 1)
		}, true, "waiting on a question"},
		{"finished, bad budgets", func(t *testing.T, w *world) {
			plantBadRun(t, w, clearID, "error", "tests failed", "budgets", 1)
		}, false, "has finished (error)"},
		{"success, corrupt pipeline", func(t *testing.T, w *world) {
			w.plantUnreadablePipelineRun(clearID, "success", true, 1, false)
		}, false, "has finished (success)"},
		{"never started, no fault, corrupt pipeline", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, clearID, "error")
			if err := writeJSON(filepath.Join(dir, "state.json"), State{Run: clearID, Status: "error", Reason: noFaultPrefixes[1] + "by the sequence", FinishedAt: 1}); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "pipeline.json"), "{not json")
		}, false, "has finished (error)"},
		{"pipeline only", func(t *testing.T, w *world) {
			pipelineOnlyRun(t, w, clearID, time.Minute)
		}, false, "no state.json"},
		{"bad budgets", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, clearID, "running")
			write(t, filepath.Join(dir, "pipeline.json"), `{"pipeline":{"budgets":{"loop":1,"coder-loop":2}}}`)
		}, true, "ensign run clear"},
		{"plain file", func(t *testing.T, w *world) {
			write(t, filepath.Join(runsDir(w.bridge), clearID), "x")
		}, false, "no run " + clearID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.plant(t, w)
			var out bytes.Buffer
			for verb, f := range map[string]func() error{
				"retire": func() error { return w.inBridge(func() error { return cmdRunRetire([]string{clearID}, &out) }) },
				"stop":   func() error { return w.inBridge(func() error { return cmdRunStop([]string{clearID}, &out, &out) }) },
			} {
				err := f()
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("%s: %v, want %q", verb, err, c.want)
				}
				if strings.Contains(c.name, "ask-user") {
					for _, bad := range []string{"has finished", "blocks nothing", "keep-runs"} {
						if strings.Contains(err.Error(), bad) {
							t.Errorf("%s: says %q of an ask-user run: %v", verb, bad, err)
						}
					}
				}
				if strings.Contains(c.name, "success") && strings.Contains(err.Error(), "lifts its hold") {
					t.Errorf("%s: claims a success run holds a publish: %v", verb, err)
				}
				if strings.Contains(c.name, "no fault") && strings.Contains(err.Error(), "lifts its hold") {
					t.Errorf("%s: claims a no-fault run holds a publish: %v", verb, err)
				}
				if offers := strings.Contains(err.Error(), "ensign run clear"); offers != c.clear {
					t.Errorf("%s: offers clear %v, want %v: %v", verb, offers, c.clear, err)
				}
			}
			if _, err := w.runClear(clearID); (err == nil) != c.clear {
				t.Errorf("clear: %v, but the message offers it: %v", err, c.clear)
			}
		})
	}
}

// plantUnreadablePipelineRun writes a finished run of the auth task whose pipeline.json can't be read.
func (w *world) plantUnreadablePipelineRun(id, status string, started bool, finished int64, missing bool) {
	w.t.Helper()
	dir := filepath.Join(runsDir(w.bridge), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), State{Run: id, Status: status, Started: started, Reason: "tests failed", FinishedAt: finished}); err != nil {
		w.t.Fatal(err)
	}
	if missing {
		return
	}
	write(w.t, filepath.Join(dir, "pipeline.json"), "{not json")
}

func TestMergeRefusesAfterARunWithoutAPipelineFailed(t *testing.T) {
	const failedID = "99999999-999991-supernova-auth-refactor"
	later := time.Now().Add(time.Hour).UnixNano()
	cases := []struct {
		name     string
		id       string
		status   string
		finished int64
		missing  bool
		refuse   bool
	}{
		{"corrupt pipeline", failedID, "error", later, false, true},
		{"missing pipeline", failedID, "error", later, true, true},
		{"collision suffix", failedID + "-2", "error", later, false, true},
		{"another task", "99999999-999991-supernova-other", "error", later, false, false},
		{"a longer label", failedID + "x", "error", later, false, false},
		{"ended before the pass", failedID, "error", 1, false, false},
		{"a success is no failure", failedID, "success", later, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, ws, remote, gh := githubWorld(t, "success", localTest)
			w.passingRun(ws, gh)
			mainBefore := remoteMain(t, remote)
			if err := os.MkdirAll(filepath.Join(runsDir(w.bridge), c.id), 0o755); err != nil {
				t.Fatal(err)
			}
			w.plantUnreadablePipelineRun(c.id, c.status, true, c.finished, c.missing)
			if !c.refuse {
				w.wantLands(remote)
				return
			}
			w.wantRefused(remote, mainBefore)
			if _, err := w.land("merge"); err == nil || !strings.Contains(err.Error(), c.id) || !strings.Contains(err.Error(), "pipeline.json") {
				t.Errorf("the refusal should name the run: %v", err)
			}
			pass := w.runState(w.state().Run)
			pass.FinishedAt = c.finished + int64(time.Hour)
			writeJSON(filepath.Join(runsDir(w.bridge), pass.Run, "state.json"), pass)
			w.wantLands(remote)
		})
	}
}

func TestRunIDMayBelongTo(t *testing.T) {
	const stamp = "20240101-000001-"
	for id, want := range map[string]bool{
		stamp + "supernova-auth-refactor":     true,
		stamp + "supernova-auth-refactor-2":   true,
		stamp + "supernova-auth-refactor-99":  true,
		stamp + "supernova-auth-refactor-1":   false,
		stamp + "supernova-auth-refactor-100": false,
		stamp + "supernova-auth-refactor-x":   false,
		stamp + "supernova-auth-refactorx":    false,
		stamp + "supernova-other":             false,
		"supernova-auth-refactor":             false,
		"20240101-000001-supernova-auth":      false,
	} {
		if got := runIDMayBelongTo(id, "supernova/auth-refactor"); got != want {
			t.Errorf("%s: got %v, want %v", id, got, want)
		}
	}
}

func TestMergeRefusesAfterAClearedRun(t *testing.T) {
	// A corrupt state.json with a good pipeline.json, and a live state with a bad pipeline.json: both end as a failure after the earlier pass.
	for _, bad := range []string{"state", "pipeline"} {
		t.Run(bad, func(t *testing.T) {
			w, ws, remote, gh := githubWorld(t, "success", localTest)
			w.passingRun(ws, gh)
			mainBefore := remoteMain(t, remote)
			id := "99999999-999991-supernova-auth-refactor"
			dir := filepath.Join(runsDir(w.bridge), id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSON(filepath.Join(dir, "pipeline.json"), Snapshot{Run: id, Task: authTask})
			writeJSON(filepath.Join(dir, "state.json"), State{Run: id, Status: "running"})
			if bad == "state" {
				write(t, filepath.Join(dir, "state.json"), "{not json")
			} else {
				write(t, filepath.Join(dir, "pipeline.json"), "{not json")
			}
			t.Setenv("STARSHIP_SESSION", "")
			if _, err := w.runClear(id); err != nil {
				t.Fatal(err)
			}
			t.Setenv("STARSHIP_SESSION", w.sid)
			w.wantRefused(remote, mainBefore)

			lead := w.startLead()
			w.codeChange(lead, ws)
			if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
				t.Fatalf("the later run should pass: %s", why)
			}
			w.wantLands(remote)
		})
	}
}

func TestCorruptSequenceFileIsReported(t *testing.T) {
	w := newWorld(t)
	write(t, seqPath(w.bridge, w.sid), "{not json")
	var out bytes.Buffer
	if err := w.inBridge(func() error { return cmdRun([]string{authTask}, &out) }); err == nil || !strings.Contains(err.Error(), "ensign sequence clear") {
		t.Errorf("a run should be refused beside an unreadable sequence: %v", err)
	}
	if _, err := sequenceUnfinished(w.bridge, w.sid); err == nil {
		t.Error("sequenceUnfinished should fail")
	}
	if l := sequenceListing(w.bridge, w.sid); !strings.Contains(l, "can't be read") {
		t.Errorf("listing %q", l)
	}
	if note := retryStrandedRetires(w.bridge, w.sid); !strings.HasPrefix(note, "starship: ") || !strings.Contains(note, "can't be read") {
		t.Errorf("note %q", note)
	}
	if err := checkAddable(w.bridge, w.sid, "later"); err == nil || !strings.Contains(err.Error(), "ensign sequence clear") {
		t.Errorf("checkAddable: %v", err)
	}
	if err := withSeq(w.bridge, w.sid, func(*seqFile) error { return nil }); err == nil || !strings.Contains(err.Error(), "ensign sequence clear") {
		t.Errorf("withSeq: %v", err)
	}
}

func TestGithubReadyNotesAnUnreadableSequence(t *testing.T) {
	w, ws, _, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	r, err := openRun(w.bridge, w.state().Run)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	r.State.Sequence = true
	write(t, seqPath(w.bridge, w.sid), "{not json")
	if err := githubReady(r); err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(r.State.Notes, "\n")
	if !strings.Contains(notes, "sequence file can't be read") || !strings.Contains(notes, "before asking for a pull request") {
		t.Errorf("notes:\n%s", notes)
	}
	if _, err := os.Stat(testedFile(w.bridge, authTask, "")); err != nil {
		t.Errorf("the marker should stand: %v", err)
	}
}

func (w *world) seqClear(args ...string) (string, error) {
	var out bytes.Buffer
	err := w.inBridge(func() error { return cmdSequenceClear(args, &out) })
	return out.String(), err
}

func TestSequenceClearSetsACorruptFileAside(t *testing.T) {
	w := newWorld(t)
	path := seqPath(w.bridge, w.sid)
	write(t, path, "{not json")
	if err := withSeq(w.bridge, w.sid, func(*seqFile) error { return nil }); err == nil {
		t.Fatal("withSeq should refuse the damaged file")
	}
	if _, err := heldRuns(w.bridge); err == nil {
		t.Fatal("heldRuns should fail on the damaged file")
	}
	out, err := w.seqClear()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, path+".corrupt") || !strings.Contains(out, "ensign run retire") {
		t.Errorf("output:\n%s", out)
	}
	if kept, err := os.ReadFile(path + ".corrupt"); err != nil || string(kept) != "{not json" {
		t.Errorf("kept %q, %v", kept, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file should be gone: %v", err)
	}
	if err := withSeq(w.bridge, w.sid, func(*seqFile) error { return nil }); err != nil {
		t.Errorf("withSeq: %v", err)
	}
	if res := gcRuns(w.bridge, 10, 0); res.HeldFailed {
		t.Error("gc still cannot tell which runs are held")
	}
	if files, _ := filesIn(filepath.Dir(path), ".sequence"); len(files) != 0 {
		t.Errorf("the set-aside file is still listed as a sequence: %v", files)
	}
	if id, ok := sessionOfFile(w.sid + ".sequence.corrupt"); !ok || id != w.sid {
		t.Error("gc should sweep the set-aside file with the session's others")
	}
}

func TestSequenceClearRefusals(t *testing.T) {
	w := newWorld(t)
	path := seqPath(w.bridge, w.sid)
	if _, err := w.seqClear(); err == nil || !strings.Contains(err.Error(), "no sequence file") {
		t.Errorf("absent: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, &seqFile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.seqClear(); err == nil || !strings.Contains(err.Error(), "ensign sequence discard") {
		t.Errorf("readable: %v", err)
	}
	write(t, path, "{not json")
	l := flock.New(path + ".lock")
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatal("could not take the lock")
	}
	if _, err := w.seqClear(); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("lock held: %v", err)
	}
	l.Unlock()
	if _, err := w.seqClear("a/b"); err == nil {
		t.Error("a session with a separator should be refused")
	}
	t.Setenv("STARSHIP_SESSION", "")
	if _, err := w.seqClear(); err == nil {
		t.Error("no session at all should be refused")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the refusals changed the file: %v", err)
	}
	if _, err := w.seqClear(w.sid); err != nil {
		t.Errorf("an explicit session works from a plain shell: %v", err)
	}
}

func TestReadSeqFileWaitsOutASave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.sequence")
	var sf seqFile

	write(t, path+".tmp", "{}")
	go func() {
		time.Sleep(150 * time.Millisecond)
		writeJSON(path, &seqFile{})
		os.Remove(path + ".tmp")
	}()
	if err := readSeqFile(path, &sf); err != nil {
		t.Errorf("a save that finishes: %v", err)
	}
	os.Remove(path)

	old := time.Now().Add(-time.Minute)
	write(t, path+".tmp", "{}")
	os.Chtimes(path+".tmp", old, old)
	start := time.Now()
	if err := readSeqFile(path, &sf); !errors.Is(err, fs.ErrNotExist) || time.Since(start) > retryWindow/2 {
		t.Errorf("a stale tmp is a crash leftover: %v after %v", err, time.Since(start))
	}
	os.Remove(path + ".tmp")

	if err := readSeqFile(path, &sf); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no tmp: %v", err)
	}
}

func TestReadSeqFileFailsClosedWhenASaveNeverFinishes(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Dir(seqPath(w.bridge, "s9"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling link is listed by name and reads as not found, like a file mid-replace on Windows.
	if err := os.Symlink(filepath.Join(dir, "nowhere"), seqPath(w.bridge, "s9")); err != nil {
		t.Skip("no symlinks here")
	}
	tmp := seqPath(w.bridge, "s9") + ".tmp"
	write(t, tmp, "{}")
	// Each fail-closed read waits out retryWindow, so the leftover would turn stale without a fresh mtime.
	fresh := func() {
		now := time.Now()
		if err := os.Chtimes(tmp, now, now); err != nil {
			t.Fatal(err)
		}
	}
	var sf seqFile
	fresh()
	if err := readSeqFile(seqPath(w.bridge, "s9"), &sf); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a save that never finishes must not read as absent: %v", err)
	}
	fresh()
	if _, err := heldRuns(w.bridge); err == nil {
		t.Error("heldRuns must fail closed")
	}
	fresh()
	if !hasOpenWork(w.bridge, "s9") {
		t.Error("hasOpenWork must count it as busy")
	}
	fresh()
	if !sessionBusy(w.bridge, "s9", nil) {
		t.Error("sessionBusy must count it as busy")
	}
}

func TestRunClearUnbindsABothCorruptRun(t *testing.T) {
	w := newWorld(t)
	dir := plantRun(t, w.bridge, clearID, "running")
	write(t, filepath.Join(dir, "pipeline.json"), "{not json")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, "s1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionRuns(w.bridge, "s1", []string{clearID}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.runClear(clearID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the session link should be gone: %v", err)
	}
	id, _, err := sessionRunID(w.bridge, "s1")
	if err != nil || id != "" {
		t.Errorf("sessionRunID = %q, %v", id, err)
	}
}

func TestRunRetireOfOwnQueuedRun(t *testing.T) {
	const queuedID = "20240101-000008-supernova"
	for _, held := range []bool{false, true} {
		w := newWorld(t)
		dir := plantRun(t, w.bridge, queuedID, "pending")
		if err := writeJSON(filepath.Join(dir, "state.json"), &State{Run: queuedID, Session: "s1", Status: "pending", Sequence: true}); err != nil {
			t.Fatal(err)
		}
		sf := &seqFile{}
		if held {
			sf.Sequences = []*sequence{{N: 1, Steps: []*seqStep{{N: 1, Kind: "run", Runs: []*seqRun{{Run: queuedID}}}}}}
		}
		path := seqPath(w.bridge, "s1")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, sf); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := w.inBridge(func() error { return cmdRunRetire([]string{queuedID}, &out) })
		var st State
		if rerr := readRunState(dir, &st); rerr != nil {
			t.Fatal(rerr)
		}
		if held {
			if err == nil || !strings.Contains(err.Error(), "belongs to this session") || st.terminal() {
				t.Errorf("held: err %v, state %+v", err, st)
			}
		} else if err != nil || !st.terminal() {
			t.Errorf("not held: err %v, state %+v", err, st)
		}
	}
}

func TestRunClearKeepsTheLinkWhenStateCannotBeCopied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory at state.json does not fail the same way on Windows")
	}
	w := newWorld(t)
	dir := plantRun(t, w.bridge, clearID, "running")
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if class, _ := readRunInfo(dir, &runInfo{id: clearID}); class != runStateUnreadable {
		t.Fatalf("state.json as a directory is classed %v, not unreadable", class)
	}
	if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, "s1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionRuns(w.bridge, "s1", []string{clearID}); err != nil {
		t.Fatal(err)
	}
	_, err := w.runClear(clearID)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("want a refusal saying nothing was changed, got %v", err)
	}
	if ids, err := sessionRuns(w.bridge, "s1"); err != nil || len(ids) != 1 || ids[0] != clearID {
		t.Errorf("the session link should survive: %v, %v", ids, err)
	}
}

func TestRunClearEndsAnAskUserRunWithABadPipeline(t *testing.T) {
	for _, kind := range []string{"unparseable", "missing", "budgets"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			t.Setenv("STARSHIP_SESSION", "")
			dir := plantBadRun(t, w, clearID, "ask-user", "which?", kind, 42)
			st := State{Run: clearID, Session: "s1", Status: "ask-user", Reason: "which?", FinishedAt: 42, Resume: &ResumePoint{}, Scopes: map[string]*ScopeState{"main": {Stage: "code", Branches: map[string]string{"a": "b"}}}}
			writeJSON(filepath.Join(dir, "state.json"), st)
			if err := os.MkdirAll(filepath.Dir(sessionFile(w.bridge, "s1")), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeSessionRuns(w.bridge, "s1", []string{clearID}); err != nil {
				t.Fatal(err)
			}
			out, err := w.runClear(clearID)
			if err != nil {
				t.Fatalf("clear: %v", err)
			}
			if !strings.Contains(out, "Cleared run "+clearID) || strings.Contains(out, "must pass again") {
				t.Errorf("output: %s", out)
			}
			var got State
			if err := readRunState(dir, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != "error" || got.Reason != clearedReason || got.FinishedAt != 42 || got.Resume != nil {
				t.Errorf("state %+v", got)
			}
			if sc := got.Scopes["main"]; sc == nil || sc.Stage != "" || sc.Branches != nil {
				t.Errorf("scope %+v", sc)
			}
			if ids, _ := sessionRuns(w.bridge, "s1"); len(ids) != 0 {
				t.Errorf("session links %v", ids)
			}
		})
	}
}

func TestGCRemovesAClearedAskUserRunUnlessAMarkerNeedsIt(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprint("marked ", marked), func(t *testing.T) {
			w := newWorld(t)
			t.Setenv("STARSHIP_SESSION", "")
			dir := plantBadRun(t, w, clearHeldID, "ask-user", "which?", "unparseable", 200)
			other := plantRun(t, w.bridge, "20240101-000009-supernova-other", "success")
			write(t, filepath.Join(other, "scratch", "x"), "x")
			if res := gcRuns(w.bridge, 1, 0); res.Removed != 0 || !exists(dir) {
				t.Fatalf("an ask-user run is never removed: %+v", res)
			}
			if _, err := w.runClear(clearHeldID); err != nil {
				t.Fatal(err)
			}
			if marked {
				w.mark(t, authTask, "")
			}
			gcRuns(w.bridge, 1, 0)
			if exists(dir) == !marked {
				t.Errorf("marked %v, exists %v", marked, exists(dir))
			}
		})
	}
}

func TestClearedAskUserRunKeepsItsPlaceAgainstAPass(t *testing.T) {
	const runID = "99999999-999991-supernova-auth-refactor"
	later := time.Now().Add(time.Hour).UnixNano()
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	mainBefore := remoteMain(t, remote)
	plantBadRun(t, w, runID, "ask-user", "which?", "unparseable", later)
	t.Setenv("STARSHIP_SESSION", "")
	if _, err := w.runClear(runID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STARSHIP_SESSION", w.sid)
	w.wantRefused(remote, mainBefore)
	pass := w.runState(w.state().Run)
	pass.FinishedAt = later + int64(time.Hour)
	writeJSON(filepath.Join(runsDir(w.bridge), pass.Run, "state.json"), pass)
	w.wantLands(remote)
}

func TestRunRetireAndStopOfAReadableAskUserRun(t *testing.T) {
	w := newWorld(t)
	plantRun(t, w.bridge, clearID, "ask-user")
	var out bytes.Buffer
	for verb, f := range map[string]func() error{
		"retire": func() error { return w.inBridge(func() error { return cmdRunRetire([]string{clearID}, &out) }) },
		"stop":   func() error { return w.inBridge(func() error { return cmdRunStop([]string{clearID}, &out, &out) }) },
	} {
		err := f()
		if err == nil || !strings.Contains(err.Error(), "waiting on a question") || !strings.Contains(err.Error(), "ensign resume --answer <text> --run "+clearID) || strings.Contains(err.Error(), "finished") {
			t.Errorf("%s: %v", verb, err)
		}
	}
	plantRun(t, w.bridge, "20240101-000009-supernova", "success")
	if err := w.inBridge(func() error { return cmdRunStop([]string{"20240101-000009-supernova"}, &out, &out) }); err == nil || !strings.Contains(err.Error(), "has finished (success)") {
		t.Errorf("a finished run keeps the finished text: %v", err)
	}
}

func TestResumeOfADamagedAskUserRunPointsAtClear(t *testing.T) {
	w := newWorld(t)
	w.plantUnreadablePipelineRun(clearID, "ask-user", true, 1, true)
	if err := os.MkdirAll(filepath.Join(w.bridge, ".starship", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionRuns(w.bridge, "s1", []string{clearID}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	resume := func(args ...string) error {
		return w.inBridge(func() error { return cmdResume(append([]string{"--answer", "yes"}, args...), &out) })
	}
	for name, args := range map[string][]string{"with --run": {"--run", clearID}, "from the session": nil} {
		if err := resume(args...); err == nil || !strings.Contains(err.Error(), "ensign run clear "+clearID) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := resume("--run", "20240101-000009-supernova"); err == nil || !strings.Contains(err.Error(), "no run 20240101-000009-supernova") {
		t.Errorf("a missing run: %v", err)
	}
}

func TestRetireForHaltWithAnUnreadablePipeline(t *testing.T) {
	w := newWorld(t)
	const pendingID, finishedID, goneID = "20240101-000001-supernova", "20240101-000002-supernova", "20240101-000003-supernova"
	os.Remove(filepath.Join(plantRun(t, w.bridge, pendingID, "pending"), "pipeline.json"))
	write(t, filepath.Join(plantRun(t, w.bridge, finishedID, "error"), "pipeline.json"), "{not json")
	if err := retireForHalt(w.bridge, pendingID, "halted", false); err == nil || !strings.Contains(err.Error(), "ensign run clear "+pendingID) {
		t.Errorf("a pending run that can't be opened is an error that names run clear: %v", err)
	}
	if err := retireForHalt(w.bridge, finishedID, "halted", false); err != nil {
		t.Errorf("a finished run needs no retiring: %v", err)
	}
	if err := retireForHalt(w.bridge, goneID, "halted", false); err != nil {
		t.Errorf("a run whose folder is gone is left alone: %v", err)
	}
}
