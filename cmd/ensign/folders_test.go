// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
