// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const authTask = "supernova/auth-refactor"

// remoteMain is the commit the remote's default branch holds.
func remoteMain(t *testing.T, remote string) string {
	t.Helper()
	return runGit(t, ".", "--git-dir", remote, "rev-parse", "main")
}

// wantLands checks the tested work reaches the remote's default branch.
func (w *world) wantLands(remote string) {
	w.t.Helper()
	if out, err := w.land("merge"); err != nil {
		w.t.Fatalf("merge should land: %q %v", out, err)
	}
	if got := runGit(w.t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		w.t.Errorf("the work did not land, main:lib.txt is %q", got)
	}
}

// wantRefused checks merge and pull-request both refuse with the latest-run message, and nothing is pushed.
func (w *world) wantRefused(remote, mainBefore string) {
	w.t.Helper()
	for _, step := range []string{"merge", "pull-request"} {
		if _, err := w.land(step); err == nil || !strings.Contains(err.Error(), "did not pass") {
			w.t.Errorf("%s should refuse after a failed run: %v", step, err)
		}
	}
	if got := remoteMain(w.t, remote); got != mainBefore {
		w.t.Errorf("nothing may be pushed to main: %s", got)
	}
}

func TestStartingARunClearsTheMarkerEvenWhenSyncFails(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), "bindings:\n", "bindings:\n  sync: { command: \"git no-such-subcommand\" }\n", 1))
	w.passingRun(ws, gh)
	marker := testedFile(w.bridge, authTask, "")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a passing run should leave its marker: %v", err)
	}
	mainBefore := remoteMain(t, remote)
	w.run(authTask, "--sync")
	w.approve()
	if lead, _ := w.spawn("", "", "orchestrator"); lead != "" {
		t.Fatal("the lead should be refused when @sync fails")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the marker should be gone once the run started: %v", err)
	}
	if _, err := w.land("merge"); err == nil {
		t.Error("merge should refuse")
	}
	if got := remoteMain(t, remote); got != mainBefore {
		t.Errorf("nothing may be pushed to main: %s", got)
	}
}

func TestMergeRefusesAfterAFailedLatestRun(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	mainBefore := remoteMain(t, remote)
	w.run(authTask)
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	write(t, filepath.Join(ws, "lib.txt"), "two\n")
	runGit(t, ws, "commit", "-q", "-am", "work")
	w.failRun(lead)
	w.staleMarker(ws)
	w.wantRefused(remote, mainBefore)

	lead = w.startLead()
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the later run should pass: %s", why)
	}
	w.wantLands(remote)
}

func TestSequencePublishAfterAcceptOrHaltOfAFailedRunFails(t *testing.T) {
	for _, how := range []string{"accept", "halt"} {
		w, ws, remote, _ := githubWorld(t, "success", localTest)
		mainBefore := remoteMain(t, remote)
		w.mustSeq("run", authTask)
		w.approve()
		lead, _ := w.spawn("", "", "orchestrator")
		write(t, filepath.Join(ws, "lib.txt"), "two\n")
		runGit(t, ws, "commit", "-q", "-am", "work")
		w.failRun(lead)
		w.staleMarker(ws)
		runGit(t, ws, "checkout", "-q", "task/auth-refactor")
		if how == "accept" {
			w.mustSeq("accept", "1")
		} else {
			w.mustSeq("halt")
		}
		w.mustSeq("publish", authTask)
		w.goContext()
		if got := w.step(2).Status; got != "failed" {
			t.Errorf("%s: the publish step is %s, want failed\n%s", how, got, sequenceListing(w.bridge, w.sid))
		}
		if got := remoteMain(t, remote); got != mainBefore {
			t.Errorf("%s: nothing may be pushed to main: %s", how, got)
		}
	}
}

func TestMergeIgnoresARunHaltCancelledBeforeItStarted(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	w.mustSeq("do", "report")
	w.mustSeq("run", authTask)
	w.approve()
	w.mustSeq("halt")
	if st := w.runState(w.step(2).Run); st.Status != "error" || !strings.HasPrefix(st.Reason, haltPrefix) || st.Started {
		t.Fatalf("the queued run should be retired by halt: %+v", st)
	}
	w.wantLands(remote)
}

func TestMergeIgnoresAFailedRunInAnotherLocation(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	other := filepath.Join(runsDir(w.bridge), "99999999-999999-other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(filepath.Join(other, "pipeline.json"), Snapshot{Run: "99999999-999999-other", Task: authTask, Location: "client"})
	writeJSON(filepath.Join(other, "state.json"), State{Status: "error", Started: true, Reason: "tests failed"})
	w.wantLands(remote)
}

// writeRun writes a terminal run of the auth task by hand, as run number n with the end time at.
func (w *world) writeRun(n, status string, started bool, reason, at string, finished int64) {
	w.t.Helper()
	id := "99999999-99999" + n + "-extra"
	dir := filepath.Join(runsDir(w.bridge), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	writeJSON(filepath.Join(dir, "pipeline.json"), Snapshot{Run: id, Task: authTask})
	st := State{Status: status, Started: started, Reason: reason, FinishedAt: finished}
	if at != "" {
		st.History = []HistoryEntry{{At: at, Kind: "finish"}}
	}
	writeJSON(filepath.Join(dir, "state.json"), st)
}

func TestMergeIgnoresRunsRetiredWithoutFault(t *testing.T) {
	for _, reason := range []string{"cancelled: x", "skipped: x", "discarded: x", "dropped: x", "withdrawn: x", "replaced by run x"} {
		w, ws, remote, gh := githubWorld(t, "success", localTest)
		w.passingRun(ws, gh)
		w.writeRun("1", "error", false, reason, "", 0)
		w.wantLands(remote)
	}
}

func TestMergeRefusesAnUnstartedRunWithAnotherReason(t *testing.T) {
	for _, reason := range []string{"failed: no seat", "ensign: broke", "retired at the user's request by session s1"} {
		w, ws, remote, gh := githubWorld(t, "success", localTest)
		w.passingRun(ws, gh)
		mainBefore := remoteMain(t, remote)
		w.writeRun("1", "error", false, reason, "", 0)
		w.wantRefused(remote, mainBefore)
	}
}

func TestMergeDecidesByWhenRunsEnded(t *testing.T) {
	passedAt := time.Now().Add(time.Hour)
	for _, c := range []struct {
		offset time.Duration
		refuse bool
	}{{-5 * time.Second, false}, {0, true}, {5 * time.Second, true}} {
		w, ws, remote, gh := githubWorld(t, "success", localTest)
		w.passingRun(ws, gh)
		r := w.runState(w.state().Run)
		r.FinishedAt = passedAt.UnixNano()
		writeJSON(filepath.Join(runsDir(w.bridge), r.Run, "state.json"), r)
		mainBefore := remoteMain(t, remote)
		w.writeRun("1", "error", true, "tests failed", passedAt.Add(c.offset).UTC().Format(time.RFC3339), 0)
		if c.refuse {
			w.wantRefused(remote, mainBefore)
		} else {
			w.wantLands(remote)
		}
	}
}

func TestMergeTreatsAPassWithoutFinishTimeAsUnknown(t *testing.T) {
	for _, c := range []struct {
		older  bool
		refuse bool
	}{{true, false}, {false, true}} {
		w, ws, remote, gh := githubWorld(t, "success", localTest)
		w.passingRun(ws, gh)
		r := w.runState(w.state().Run)
		r.FinishedAt = 0
		writeJSON(filepath.Join(runsDir(w.bridge), r.Run, "state.json"), r)
		mainBefore := remoteMain(t, remote)
		w.writeRun("1", "error", true, "tests failed", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), 0)
		if c.older {
			runs := runsDir(w.bridge)
			if err := os.Rename(filepath.Join(runs, "99999999-999991-extra"), filepath.Join(runs, "00000000-000000-extra")); err != nil {
				t.Fatal(err)
			}
		}
		if c.refuse {
			w.wantRefused(remote, mainBefore)
		} else {
			w.wantLands(remote)
		}
	}
}
