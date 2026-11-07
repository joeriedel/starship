// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func mustLive(t *testing.T, bridge, session string) []string {
	t.Helper()
	ids, err := liveSessionRuns(bridge, session)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// holdingRun writes an approved running run with the snapshot, owned by session.
func holdingRun(t *testing.T, w *world, id, session string, snap *Snapshot) {
	t.Helper()
	dir := filepath.Join(runsDir(w.bridge), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), snap); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), &State{Run: id, Session: session, Status: "running", Approved: true, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}); err != nil {
		t.Fatal(err)
	}
}

type supernovaFolders struct{ client, server, models string }

// clientSnap is a run on client that applies a models branch; modelsSnap is a run on models that tests client and server.
func (f supernovaFolders) clientSnap() *Snapshot {
	return &Snapshot{Workspace: f.client, Location: "client", Sides: []Side{{Location: "client", Workspace: f.client}},
		Dependencies: []Dependency{{Location: "models", Branch: "models-fix", Path: f.models, Sides: []string{"client"}}}}
}

func (f supernovaFolders) serverSnap() *Snapshot {
	return &Snapshot{Workspace: f.server, Location: "server", Sides: []Side{{Location: "server", Workspace: f.server}},
		Dependencies: []Dependency{{Location: "models", Branch: "models-fix", Path: f.models, Sides: []string{"server"}, Auto: true}}}
}

func (f supernovaFolders) modelsSnap() *Snapshot {
	return &Snapshot{Workspace: f.models, Location: "models",
		Sides:        []Side{{Location: "models", Workspace: f.models}, {Location: "client", Workspace: f.client}, {Location: "server", Workspace: f.server}},
		Dependencies: []Dependency{{Location: "models", Path: f.models, Sides: []string{"client", "server"}}}}
}

func TestRunsThatOnlyReadAFolderShareIt(t *testing.T) {
	w := newWorld(t)
	f := supernovaFolders{t.TempDir(), t.TempDir(), t.TempDir()}
	holdingRun(t, w, "20260101-000001-client", "s1", f.clientSnap())
	if err := folderHolder(w.bridge, f.serverSnap(), "s2"); err != nil {
		t.Fatalf("a server run applying the same models branch should start: %v", err)
	}
	holdingRun(t, w, "20260101-000002-server", "s2", f.serverSnap())

	if err := folderHolder(w.bridge, f.modelsSnap(), "s3"); err == nil {
		t.Fatal("a models run that tests client and server should wait")
	}
	err := folderHolder(w.bridge, &Snapshot{Workspace: f.models, Location: "models"}, "s3")
	var busy *folderBusyError
	if !errors.As(err, &busy) || busy.folder != f.models || busy.held != folderReads || busy.wanted != folderWorks {
		t.Fatalf("a models run while client and server read models: %v", err)
	}
	if !strings.Contains(err.Error(), f.models) || !strings.Contains(err.Error(), "applies a branch") || !strings.Contains(err.Error(), "another session is doing work") {
		t.Errorf("refusal text: %v", err)
	}
	if err := folderHolder(w.bridge, f.modelsSnap(), "s1", "20260101-000001-client", "20260101-000002-server"); err != nil {
		t.Errorf("skipped runs hold nothing: %v", err)
	}
}

func TestAModelsRunExcludesTheRunsThatReadModels(t *testing.T) {
	w := newWorld(t)
	f := supernovaFolders{t.TempDir(), t.TempDir(), t.TempDir()}
	holdingRun(t, w, "20260101-000001-models", "s1", f.modelsSnap())
	for name, snap := range map[string]*Snapshot{"client": f.clientSnap(), "server": f.serverSnap()} {
		err := folderHolder(w.bridge, snap, "s1")
		var busy *folderBusyError
		if !errors.As(err, &busy) || busy.run != "20260101-000001-models" || busy.held != folderWorks {
			t.Fatalf("%s run while models run holds: %v", name, err)
		}
		if strings.Contains(err.Error(), "another session") {
			t.Errorf("the same session's run should not point at retiring: %v", err)
		}
	}
	other := &Snapshot{Workspace: f.client, Location: "client"}
	err := folderHolder(w.bridge, other, "s2")
	if err == nil || !strings.Contains(err.Error(), f.client) || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("a client run whose client folder the models run tests: %v", err)
	}
}

func TestRetireEndsAnotherSessionsRun(t *testing.T) {
	w := newWorld(t)
	folder := t.TempDir()
	a := w.liveStepRun(folder, "")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer

	if err := cmdRun([]string{"retire", a}, &out); err == nil || !strings.Contains(err.Error(), "this session") {
		t.Fatalf("a run of the caller's own session: %v", err)
	}
	t.Setenv("STARSHIP_SESSION", "s2")
	if err := cmdRun([]string{"retire", "no-such-run"}, &out); err == nil || !strings.Contains(err.Error(), "no run") {
		t.Fatalf("an unknown run: %v", err)
	}
	if err := cmdRun([]string{"retire", a}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), a) || !strings.Contains(out.String(), folder) {
		t.Errorf("output should name the run and the freed folder: %s", out.String())
	}
	st := w.runState(a)
	if st.Status != "error" || !strings.Contains(st.Reason, "s2") {
		t.Fatalf("retired run is %s: %s", st.Status, st.Reason)
	}
	if ids, _ := sessionRuns(w.bridge, w.sid); len(ids) != 0 {
		t.Errorf("the owner's session still lists %v", ids)
	}
	if err := folderHolder(w.bridge, &Snapshot{Workspace: folder}, "s2"); err != nil {
		t.Errorf("the folder should be free: %v", err)
	}
	if err := cmdRun([]string{"retire", a}, &out); err == nil || !strings.Contains(err.Error(), "finished") {
		t.Fatalf("a finished run: %v", err)
	}
}

func TestSessionRunsReportsReadErrors(t *testing.T) {
	w := newWorld(t)
	if ids, err := sessionRuns(w.bridge, "nobody"); err != nil || len(ids) != 0 {
		t.Fatalf("a session without a file has no runs: %v %v", ids, err)
	}
	if err := os.MkdirAll(sessionFile(w.bridge, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionRuns(w.bridge, "broken"); err == nil {
		t.Fatal("an unreadable session file is not an empty one")
	}
	if line := runLine(w.bridge, "broken"); !strings.Contains(line, "broken") {
		t.Errorf("the status line should carry the error: %q", line)
	}
}

func TestSequenceStopHoldsForAStepWaitingOnAnotherSessionsRun(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	ids := w.stepRuns(1)
	server, err := openRun(w.bridge, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	folder := server.Snap.Workspace
	server.close()
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: "supernova/t"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	snap.Workspace, snap.Sides = folder, nil
	if err := writeJSON(filepath.Join(runsDir(w.bridge), snap.Run, "pipeline.json"), snap); err != nil {
		t.Fatal(err)
	}
	if _, err := startRun(w.bridge, "other-session", snap.Task, snap.Run); err != nil {
		t.Fatal(err)
	}
	w.approve()

	res := w.stopHook()
	reason := dig(res, "reason")
	if res["decision"] != "block" || !strings.Contains(reason, "waiting: ") || !strings.Contains(reason, snap.Run) || !strings.Contains(reason, folder) {
		t.Fatalf("the step waiting on another session's run should hold the turn: %v", res)
	}
	if !strings.Contains(reason, "another session is doing work") {
		t.Errorf("the reason should say another session is working: %s", reason)
	}
}

func TestWaitingHoldsDoNotSpendTheLeadPromptsAllowance(t *testing.T) {
	w := groupWorld(t, "01-client")
	w.mustSeq("run", "supernova/t")
	ids := w.stepRuns(1)
	run, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	folder := run.Snap.Workspace
	run.close()
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: "supernova/t"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	snap.Workspace, snap.Sides = folder, nil
	if err := writeJSON(filepath.Join(runsDir(w.bridge), snap.Run, "pipeline.json"), snap); err != nil {
		t.Fatal(err)
	}
	if _, err := startRun(w.bridge, "other-session", snap.Task, snap.Run); err != nil {
		t.Fatal(err)
	}
	w.approve()

	for i := 0; i < seqStopBudget; i++ {
		res := w.stopHook()
		if res["decision"] != "block" || !strings.Contains(dig(res, "reason"), "waiting: ") {
			t.Fatalf("hold %d should name the wait: %v", i+1, res)
		}
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	if err := cmdRun([]string{"retire", snap.Run}, &out); err != nil {
		t.Fatal(err)
	}

	res := w.stopHook()

	reason := dig(res, "reason")
	if res["decision"] != "block" || strings.Contains(reason, "waiting: ") || !strings.Contains(reason, ids[0]) {
		t.Fatalf("the started step should hold the turn with its lead prompt: %v", res)
	}
	if strings.Contains(reason, "sequence 1 (mode") {
		t.Errorf("the listing should be absent: %s", reason)
	}
	if n := strings.Count(reason, "Step 1/1"); n != 1 {
		t.Errorf("the position line should appear once, got %d: %s", n, reason)
	}
	if n := strings.Count(reason, "[starship run="+ids[0]+" lead]"); n != 1 {
		t.Errorf("the lead spawn tag should appear once, got %d: %s", n, reason)
	}
}

func TestFolderBusyErrorSaysAnotherSessionIsDoingWork(t *testing.T) {
	modes := []struct{ held, wanted folderMode }{{folderWorks, folderWorks}, {folderWorks, folderReads}, {folderReads, folderWorks}}
	for _, m := range modes {
		e := &folderBusyError{run: "20260101-000001-client", status: "running", folder: "/work/supernova", held: m.held, wanted: m.wanted, otherSession: true}
		msg := e.Error()
		if !strings.Contains(msg, e.run) || !strings.Contains(msg, e.folder) || !strings.Contains(msg, "another session is doing work") {
			t.Errorf("held %v wanted %v: %s", m.held, m.wanted, msg)
		}
		if strings.Contains(msg, "retire") || strings.Contains(msg, "ensign help runs") {
			t.Errorf("held %v wanted %v should not point at retiring: %s", m.held, m.wanted, msg)
		}
		e.otherSession = false
		if msg := e.Error(); strings.Contains(msg, "another session") {
			t.Errorf("held %v wanted %v: the same session's refusal mentions another session: %s", m.held, m.wanted, msg)
		}
	}
}

// holdLanding takes the landing lock of a folder until the test ends.
func holdLanding(t *testing.T, w *world, folder string) func() {
	t.Helper()
	unlock, err := lockLanding(w.bridge, folder)
	if err != nil {
		t.Fatal(err)
	}
	var once bool
	release := func() {
		if !once {
			once = true
			unlock()
		}
	}
	t.Cleanup(release)
	return release
}

func wantLandingBusy(t *testing.T, err error, folder string) {
	t.Helper()
	var busy *folderBusyError
	if !errors.As(err, &busy) || !busy.landing || busy.run != "" {
		t.Fatalf("want a landing refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "a landing is under way in the code folder "+folder) || strings.Contains(err.Error(), "run ") {
		t.Errorf("refusal text: %v", err)
	}
}

func TestALandingHoldsItsFolderAgainstRuns(t *testing.T) {
	w := newWorld(t)
	c, err := loadConfigAt(w.bridge, "supernova/auth-refactor", "")
	if err != nil {
		t.Fatal(err)
	}
	ws := c.Workspace
	w.run("supernova/auth-refactor")
	release := holdLanding(t, w, ws)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer

	wantLandingBusy(t, cmdRun([]string{"supernova/auth-refactor"}, &out), ws)
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	ctx := dig(res, "hookSpecificOutput", "additionalContext")
	if !strings.Contains(ctx, "was not started") || !strings.Contains(ctx, "a landing is under way") {
		t.Errorf("go while a landing is under way: %v", res)
	}
	if all, err := listRuns(w.bridge); err != nil || len(all) != 1 || all[0].state.Status != "pending" {
		t.Errorf("the run should stay pending: %v %v", all, err)
	}
	wantLandingBusy(t, folderHolder(w.bridge, &Snapshot{Workspace: ws + "/./x/.."}, w.sid), ws+"/./x/..")
	if err := folderHolder(w.bridge, &Snapshot{Workspace: t.TempDir()}, w.sid); err != nil {
		t.Errorf("another folder is free: %v", err)
	}
	reads := &Snapshot{Workspace: t.TempDir(), Dependencies: []Dependency{{Location: "models", Path: ws}}}
	wantLandingBusy(t, folderHolder(w.bridge, reads, w.sid), ws)

	release()
	w.approve()
}

func TestALandingOrUpdateInsideHoldsAParentRun(t *testing.T) {
	w := newWorld(t)
	parent := t.TempDir()
	inner := filepath.Join(parent, "client")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{Workspace: parent}
	release := holdLanding(t, w, inner)
	wantLandingBusy(t, folderHolder(w.bridge, snap, w.sid), inner)
	reads := &Snapshot{Workspace: t.TempDir(), Dependencies: []Dependency{{Location: "models", Path: parent}}}
	wantLandingBusy(t, folderHolder(w.bridge, reads, w.sid), inner)
	release()
	if err := folderHolder(w.bridge, reads, w.sid); err != nil {
		t.Errorf("the landing is over, so a run that only reads the parent starts: %v", err)
	}
	if err := folderHolder(w.bridge, snap, w.sid); err != nil {
		t.Errorf("the landing is over: %v", err)
	}
	unlock, err := lockUpdate(w.bridge, inner)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var busy *folderBusyError
	if err := folderHolder(w.bridge, snap, w.sid); !errors.As(err, &busy) || !busy.update || busy.folder != inner {
		t.Errorf("want an update refusal in %s: %v", inner, err)
	}
}

func TestALandingHoldsItsFolderAgainstResume(t *testing.T) {
	w := newWorld(t)
	folder := t.TempDir()
	a := w.liveStepRun(folder, "")
	r, err := openRun(w.bridge, a)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status, r.State.Reason = "ask-user", "which way?"
	r.State.Resume = &ResumePoint{Scope: "run", Stage: "code"}
	r.save()
	r.close()
	release := holdLanding(t, w, folder)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	wantLandingBusy(t, cmdResume([]string{"--answer", "left", "--run", a}, &out), folder)
	if st := w.runState(a); st.Status != "ask-user" {
		t.Fatalf("the refused resume left the run %s", st.Status)
	}
	release()
	if err := cmdResume([]string{"--answer", "left", "--run", a}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestASequenceStepWaitsForALanding(t *testing.T) {
	w := groupWorld(t, "01-client")
	w.mustSeq("run", "supernova/t")
	run, err := openRun(w.bridge, w.stepRuns(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	folder := run.Snap.Workspace
	run.close()
	release := holdLanding(t, w, folder)
	w.approve()
	if st := w.step(1); st.Status != "pending" || !strings.Contains(st.Reason, "waiting: a landing is under way in the code folder "+folder) {
		t.Fatalf("the step should wait for the landing: %s %q", st.Status, st.Reason)
	}
	res := w.stopHook()
	reason := dig(res, "reason")
	if res["decision"] != "block" || !strings.Contains(reason, "a landing is under way") || !strings.Contains(reason, "once the folder is free") || strings.Contains(reason, "once that run") {
		t.Fatalf("the Stop hook should hold the turn and say what it waits for: %v", res)
	}
	for i := 1; i < seqStopBudget; i++ {
		w.stopHook()
	}
	if res := w.stopHook(); res != nil {
		t.Errorf("the holds are limited: %v", res)
	}
	release()
	w.stopHook()
	if st := w.step(1); st.Status != "active" {
		t.Errorf("the step should start once the landing is released: %s %q", st.Status, st.Reason)
	}
}

func TestLandingsInOneFolderShareTheLock(t *testing.T) {
	w := newWorld(t)
	folder := t.TempDir()
	a := holdLanding(t, w, folder)
	b := holdLanding(t, w, folder+"/./x/..")
	for _, f := range []string{folder, folder + "/./x/.."} {
		if on, err := landingUnderWay(w.bridge, f); err != nil || !on {
			t.Errorf("%s: %v %v", f, on, err)
		}
	}
	a()
	if on, _ := landingUnderWay(w.bridge, folder); !on {
		t.Error("the other landing still holds the folder")
	}
	b()
	if on, err := landingUnderWay(w.bridge, folder); err != nil || on {
		t.Errorf("the folder should be free: %v %v", on, err)
	}
	if err := folderHolder(w.bridge, &Snapshot{Workspace: folder}, "s1"); err != nil {
		t.Errorf("a run may start: %v", err)
	}
}

func TestAPublishCommandHoldsTheLandingLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is a sh loop")
	}
	w := newWorld(t)
	c, err := loadConfigAt(w.bridge, "supernova/auth-refactor", "")
	if err != nil {
		t.Fatal(err)
	}
	w.mustSeq("do", "placeholder")
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	command := "touch " + shellQuote(started) + "; while [ ! -f " + shellQuote(release) + " ]; do sleep 0.05; done #"
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Steps[0] = &seqStep{Kind: "publish", Task: "supernova/auth-refactor", Command: command, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := approveSequence(w.bridge, w.sid)
		done <- err
	}()
	for deadline := time.Now().Add(20 * time.Second); !exists(started); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the publish command never started")
		}
	}
	if on, err := landingUnderWay(w.bridge, c.Workspace); err != nil || !on {
		t.Errorf("the command runs under the landing lock: %v %v", on, err)
	}
	write(t, release, "x\n")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if on, err := landingUnderWay(w.bridge, c.Workspace); err != nil || on {
		t.Errorf("the lock should be free after the landing: %v %v", on, err)
	}
}

// holdUpdate takes the update lock of a folder until the test ends.
func holdUpdate(t *testing.T, w *world, folder string) func() {
	t.Helper()
	unlock, err := lockUpdate(w.bridge, folder)
	if err != nil {
		t.Fatal(err)
	}
	var once bool
	release := func() {
		if !once {
			once = true
			unlock()
		}
	}
	t.Cleanup(release)
	return release
}

func wantUpdateBusy(t *testing.T, err error, folder string) {
	t.Helper()
	var busy *folderBusyError
	if !errors.As(err, &busy) || !busy.update || busy.run != "" {
		t.Fatalf("want an update refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "an update is under way in the code folder "+folder) || strings.Contains(err.Error(), "run ") {
		t.Errorf("refusal text: %v", err)
	}
}

func TestAnUpdateHoldsItsFolderAgainstRuns(t *testing.T) {
	w := newWorld(t)
	c, err := loadConfigAt(w.bridge, "supernova/auth-refactor", "")
	if err != nil {
		t.Fatal(err)
	}
	ws := c.Workspace
	w.run("supernova/auth-refactor")
	release := holdUpdate(t, w, ws)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	wantUpdateBusy(t, cmdRun([]string{"supernova/auth-refactor"}, &out), ws)
	wantUpdateBusy(t, folderHolder(w.bridge, &Snapshot{Workspace: ws + "/./x/.."}, w.sid), ws+"/./x/..")
	if err := folderHolder(w.bridge, &Snapshot{Workspace: t.TempDir()}, w.sid); err != nil {
		t.Errorf("another folder is free: %v", err)
	}
	release()
	if err := folderHolder(w.bridge, &Snapshot{Workspace: ws}, w.sid); err != nil {
		t.Errorf("the folder is free once the update is done: %v", err)
	}
}

func TestAnUpdateAndALandingRefuseEachOther(t *testing.T) {
	w := newWorld(t)
	ws := t.TempDir()
	releaseLanding := holdLanding(t, w, ws)
	if _, err := lockUpdate(w.bridge, ws); err == nil || !strings.Contains(err.Error(), "a landing is under way") {
		t.Errorf("update during a landing: %v", err)
	}
	releaseLanding()
	releaseUpdate := holdUpdate(t, w, ws)
	if _, err := lockUpdate(w.bridge, ws); err == nil || !strings.Contains(err.Error(), "an update is already under way") {
		t.Errorf("update during an update: %v", err)
	}
	if _, err := lockLanding(w.bridge, ws); err == nil || !strings.Contains(err.Error(), "an update is under way") {
		t.Errorf("landing during an update: %v", err)
	}
	releaseUpdate()
	holdLanding(t, w, ws)
}

func TestASequenceStepWaitsForAnUpdate(t *testing.T) {
	w := groupWorld(t, "01-client")
	w.mustSeq("run", "supernova/t")
	run, err := openRun(w.bridge, w.stepRuns(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	folder := run.Snap.Workspace
	run.close()
	holdUpdate(t, w, folder)
	w.approve()
	if st := w.step(1); st.Status != "pending" || !strings.Contains(st.Reason, "waiting: an update is under way in the code folder "+folder) {
		t.Fatalf("the step should wait for the update: %s %q", st.Status, st.Reason)
	}
}

func TestFolderHolderSeesRunsInSubfoldersAndParents(t *testing.T) {
	w := newWorld(t)
	parent := t.TempDir()
	child := filepath.Join(parent, "client")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	holdingRun(t, w, "20260101-000001-sub", "s1", &Snapshot{Workspace: child})
	var busy *folderBusyError
	if err := folderHolder(w.bridge, &Snapshot{Workspace: parent}, "s2"); !errors.As(err, &busy) || busy.run != "20260101-000001-sub" {
		t.Fatalf("a run in a parent folder should wait for the one in its subfolder: %v", err)
	}
	if err := folderHolder(w.bridge, &Snapshot{Workspace: filepath.Join(parent, "client2")}, "s2"); err != nil {
		t.Fatalf("a sibling whose name starts the same is another folder: %v", err)
	}
	if h, err := runsHolding(w.bridge, parent); err != nil || len(h) != 1 {
		t.Fatalf("runsHolding should see the run in the subfolder: %v %v", h, err)
	}
	if err := os.RemoveAll(filepath.Join(runsDir(w.bridge), "20260101-000001-sub")); err != nil {
		t.Fatal(err)
	}
	holdingRun(t, w, "20260101-000002-parent", "s1", &Snapshot{Workspace: parent})
	if err := folderHolder(w.bridge, &Snapshot{Workspace: child}, "s2"); !errors.As(err, &busy) || busy.run != "20260101-000002-parent" {
		t.Fatalf("a run in a subfolder should wait for the one in its parent: %v", err)
	}
	if h, err := runsHolding(w.bridge, child); err != nil || len(h) != 1 {
		t.Fatalf("runsHolding should see the run in the parent: %v %v", h, err)
	}
}

func TestRunsThatOnlyReadShareAcrossContainment(t *testing.T) {
	w := newWorld(t)
	parent := t.TempDir()
	child := filepath.Join(parent, "models")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	dep := func(path, ws string) *Snapshot {
		return &Snapshot{Workspace: ws, Dependencies: []Dependency{{Location: "models", Branch: "b", Path: path}}}
	}
	holdingRun(t, w, "20260101-000001-a", "s1", dep(parent, t.TempDir()))
	if err := folderHolder(w.bridge, dep(child, t.TempDir()), "s2"); err != nil {
		t.Fatalf("reads against reads share: %v", err)
	}
	var busy *folderBusyError
	if err := folderHolder(w.bridge, &Snapshot{Workspace: child}, "s2"); !errors.As(err, &busy) || busy.held != folderReads || busy.wanted != folderWorks {
		t.Fatalf("a run working in a subfolder of a folder another reads is busy: %v", err)
	}
}

func TestALandingOrUpdateInAParentHoldsASubfolder(t *testing.T) {
	w := newWorld(t)
	parent := t.TempDir()
	child := filepath.Join(parent, "client")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	release := holdLanding(t, w, parent)
	var busy *folderBusyError
	if err := folderHolder(w.bridge, &Snapshot{Workspace: child}, "s1"); !errors.As(err, &busy) || !busy.landing || busy.folder != parent {
		t.Fatalf("a landing in the parent should hold the subfolder: %v", err)
	}
	release()
	holdUpdate(t, w, parent)
	if err := folderHolder(w.bridge, &Snapshot{Workspace: child}, "s1"); !errors.As(err, &busy) || !busy.update || busy.folder != parent {
		t.Fatalf("an update in the parent should hold the subfolder: %v", err)
	}
}

// publishStepWorld is a world whose first sequence is a single approved-on-go publish step running command in the task's workspace.
func publishStepWorld(t *testing.T, command string) (*world, string) {
	t.Helper()
	w := newWorld(t)
	c, err := loadConfigAt(w.bridge, "supernova/auth-refactor", "")
	if err != nil {
		t.Fatal(err)
	}
	w.mustSeq("do", "placeholder")
	if err := withSeq(w.bridge, w.sid, func(sf *seqFile) error {
		sf.Sequences[0].Steps[0] = &seqStep{Kind: "publish", Task: "supernova/auth-refactor", Command: command, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return w, c.Workspace
}

func TestAPublishStepWaitsForAnUpdateThenLands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is sh")
	}
	marker := filepath.Join(t.TempDir(), "landed")
	w, folder := publishStepWorld(t, "touch "+shellQuote(marker)+" #")
	release := holdUpdate(t, w, folder)
	if _, _, err := approveSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if st := w.step(1); st.Status != "pending" || !strings.Contains(st.Reason, "waiting: an update is under way in the code folder "+folder) || exists(marker) {
		t.Fatalf("the publish should wait for the update: %s %q", st.Status, st.Reason)
	}
	res := w.stopHook()
	if reason := dig(res, "reason"); res["decision"] != "block" || !strings.Contains(reason, "an update is under way") || !strings.Contains(reason, "once the folder is free") {
		t.Fatalf("the Stop hook should hold the turn and say what the publish waits for: %v", res)
	}
	for i := 1; i < seqStopBudget; i++ {
		w.stopHook()
	}
	if res := w.stopHook(); res != nil {
		t.Errorf("the holds are limited: %v", res)
	}
	release()
	w.stopHook()
	if st := w.step(1); st.Status != "done" || !exists(marker) {
		t.Errorf("the publish should land once the update is over: %s %q", st.Status, st.Reason)
	}
}

func TestACustomPublishWaitsForRunsInTheFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is sh")
	}
	marker := filepath.Join(t.TempDir(), "landed")
	w, folder := publishStepWorld(t, "touch "+shellQuote(marker)+" #")
	sub := filepath.Join(folder, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	holdingRun(t, w, "20260101-000001-other", "s2", &Snapshot{Task: "supernova/other", Workspace: sub})
	if _, _, err := approveSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if st := w.step(1); st.Status != "pending" || !strings.Contains(st.Reason, "waiting: run 20260101-000001-other") || exists(marker) {
		t.Fatalf("a run of another task in a subfolder holds the landing's folder: %s %q", st.Status, st.Reason)
	}
	r, err := openRun(w.bridge, "20260101-000001-other")
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status = "success"
	r.save()
	r.close()
	w.stopHook()
	if st := w.step(1); st.Status != "done" || !exists(marker) {
		t.Errorf("the publish should land once the folder is free: %s %q", st.Status, st.Reason)
	}
}

func TestACustomPublishWaitsForARunOfItsTask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is sh")
	}
	w, _ := publishStepWorld(t, "true #")
	holdingRun(t, w, "20260101-000001-own", "s2", &Snapshot{Task: "supernova/auth-refactor", Workspace: t.TempDir()})
	if _, _, err := approveSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if st := w.step(1); st.Status != "pending" || !strings.Contains(st.Reason, "waiting: a run of supernova/auth-refactor is still going") {
		t.Fatalf("the publish should wait for the run: %s %q", st.Status, st.Reason)
	}
}

func TestAPublishThatFailsForAnotherReasonStillFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is sh")
	}
	w, _ := publishStepWorld(t, "echo boom; exit 3 #")
	if _, _, err := approveSequence(w.bridge, w.sid); err != nil {
		t.Fatal(err)
	}
	if st := w.step(1); st.Status != "failed" || !strings.Contains(st.Reason, "boom") {
		t.Fatalf("a failing command fails the step: %s %q", st.Status, st.Reason)
	}
}
