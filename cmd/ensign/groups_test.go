// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// groupWorld is a locations world whose task t has one subtask per name, "<number>-<location>" for the locations client, server and models.
func groupWorld(t *testing.T, names ...string) *world {
	t.Helper()
	w := locationsWorld(t)
	locTask(t, w, "t", "models, client, server")
	for _, n := range names {
		loc := n[strings.Index(n, "-")+1:]
		w.subtaskFile("t", n, "---\nlocation: "+loc+"\n---\n\n## Goal\n\nGoal of "+n+".\n")
	}
	return w
}

// leadOf spawns the lead of a run the way the session does with several runs live: with the tag its approval text names.
func (w *world) leadOf(run string) string {
	w.t.Helper()
	id, why := w.spawnPrompt("", "", "orchestrator", spawnTag(run)+"\n\ndo it")
	if id == "" {
		w.t.Fatalf("the lead of %s was refused: %s", run, why)
	}
	return id
}

// passLoc takes a run of the locations fixture through its stages to success; its review stage takes one reviewer.
func (w *world) passLoc(lead string) {
	w.t.Helper()
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
}

// stepRuns lists the run ids of a step.
func (w *world) stepRuns(n int) []string {
	w.t.Helper()
	var ids []string
	for _, ru := range w.step(n).runs() {
		ids = append(ids, ru.Run)
	}
	return ids
}

// names lists the names of a group's subtasks.
func names(group []subtask) []string {
	var out []string
	for _, s := range group {
		out = append(out, s.Name)
	}
	return out
}

func TestSubtaskGroups(t *testing.T) {
	w := groupWorld(t)
	sub := func(name, loc string) subtask { return subtask{Name: name, Location: loc} }
	groupsOf := func(subs ...subtask) []string {
		var out []string
		for _, g := range subtaskGroups(w.bridge, "supernova/t", subs) {
			out = append(out, strings.Join(names(g), "+"))
		}
		return out
	}
	if got := strings.Join(groupsOf(sub("1", "models"), sub("2", "client"), sub("3", "server")), " "); got != "1 2+3" {
		t.Errorf("models, then client and server together: %s", got)
	}
	if got := strings.Join(groupsOf(sub("1", "client"), sub("2", "server"), sub("3", "models")), " "); got != "1+2 3" {
		t.Errorf("a location its earlier ones depend on starts a group: %s", got)
	}
	if got := strings.Join(groupsOf(sub("1", "models"), sub("2", "models"), sub("3", "client")), " "); got != "1 2 3" {
		t.Errorf("one location twice, and a dependent: %s", got)
	}
	if got := strings.Join(groupsOf(sub("1", ""), sub("2", "")), " "); got != "1 2" {
		t.Errorf("subtasks without a location: %s", got)
	}
	if _, err := setLocationPath(w.bridge, "supernova", "server", w.sideFolder("client")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(groupsOf(sub("1", "client"), sub("2", "server")), " "); got != "1 2" {
		t.Errorf("locations in one folder: %s", got)
	}
}

func TestSequenceGroupStartsEveryRunAndListsGroups(t *testing.T) {
	w := groupWorld(t, "01-models", "02-client", "03-server")
	out := w.mustSeq("run", "supernova/t")
	for _, want := range []string{"group 1: 01-models (models)\n", "group 2: 02-client (client), 03-server (server)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	w.approve()
	lead := w.leadOf(w.stepRuns(1)[0])
	w.passLoc(lead)
	st := w.step(1)
	if st.Status != "active" || len(st.Runs) != 2 || st.Group != 2 {
		t.Fatalf("the step should hold the second group's two runs: %+v", st)
	}
	for i, ru := range st.Runs {
		r := w.runState(ru.Run)
		if !r.Approved || r.Status != "running" {
			t.Errorf("run %d of the group should be under way: %+v", i, r)
		}
	}
	if st.Runs[0].Subtask != "02-client" || st.Runs[1].Subtask != "03-server" {
		t.Errorf("runs: %+v %+v", st.Runs[0], st.Runs[1])
	}
	if l := sequenceListing(w.bridge, w.sid); !strings.Contains(l, "group 2: 02-client (client) [running], 03-server (server) [running]") {
		t.Errorf("listing:\n%s", l)
	}
}

func TestSequenceGoDispatchesEveryRunOfTheGroup(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	ctx := w.goContext()
	for _, id := range w.stepRuns(1) {
		if !strings.Contains(ctx, spawnTag(id)) {
			t.Errorf("go should tell the session to dispatch %s:\n%s", id, ctx)
		}
	}
	if len(w.stepRuns(1)) != 2 {
		t.Fatalf("the first group is client and server: %v", w.stepRuns(1))
	}
}

func TestSequenceStopNamesEveryWaitingLead(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	res := w.stopHook()
	reason, _ := res["reason"].(string)
	for _, id := range w.stepRuns(1) {
		if !strings.Contains(reason, "run "+id+" approved") {
			t.Errorf("the Stop hook should name the lead of %s:\n%s", id, reason)
		}
	}
}

func TestSessionSpawnWithSeveralLiveRunsFollowsTheTag(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	if res, _ := w.hook("PreToolUse", map[string]any{"tool_name": "Agent",
		"tool_input": map[string]any{"subagent_type": "orchestrator", "prompt": "no tag", "description": "x"}}); res != nil {
		t.Fatalf("a spawn that names no run is left alone: %v", res)
	}
	lead := w.leadOf(ids[1])
	if n := len(w.runState(ids[1]).Dispatches); n != 1 {
		t.Errorf("the tagged run should have its lead dispatched, has %d dispatches", n)
	}
	if n := len(w.runState(ids[0]).Dispatches); n != 0 {
		t.Errorf("the other run should have none, has %d", n)
	}
	w.leaf(lead, "coder", "compiled")
	if st := w.runState(ids[1]); st.Scopes["run"].Stage != "review" {
		t.Errorf("the lead's own spawn should reach its run: %s", st.Scopes["run"].Stage)
	}
}

func TestSequenceGroupFailureWaitsForTheOtherRunThenDecides(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	clientLead, serverLead := w.leadOf(ids[0]), w.leadOf(ids[1])
	w.failRun(clientLead)
	if st := w.step(1); st.Status != "active" {
		t.Fatalf("the other run is still going, so the step is %s", st.Status)
	}
	w.passLoc(serverLead)
	st := w.step(1)
	if st.Status != "decide" || !strings.Contains(sequenceListing(w.bridge, w.sid), "01-client (client) [error]") {
		t.Fatalf("both runs are over, one failed: %+v\n%s", st, sequenceListing(w.bridge, w.sid))
	}

	w.retryStep("1")
	st = w.step(1)
	if len(st.Runs) != 2 || st.Runs[0].Run == ids[0] || st.Runs[1].Run != ids[1] {
		t.Fatalf("only the failed run is rerun: %+v", st.Runs)
	}
	if st.Runs[0].Prev != ids[0] || st.Prev != ids[0] || st.Runs[0].Subtask != "01-client" {
		t.Errorf("retry: %+v step prev %s", st.Runs[0], st.Prev)
	}
	if r := w.runState(st.Runs[0].Run); !r.Approved || r.Status != "running" || r.RetryOf != ids[0] {
		t.Errorf("the retry should be under way: %+v", r)
	}
	w.passLoc(w.leadOf(st.Runs[0].Run))
	w.wantStatus("done")
	if m := w.subtaskMarks("t"); m["01-client"].Status != "done" || m["02-server"].Status != "done" {
		t.Errorf("marks %+v", m)
	}
}

func TestSequenceAcceptEndsTheLaterGroups(t *testing.T) {
	w := groupWorld(t, "01-models", "02-client", "03-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	w.failRun(w.leadOf(w.stepRuns(1)[0]))
	w.mustSeq("accept", "1")
	w.wantStatus("failed")
	n := 0
	for _, ri := range listRuns(w.bridge) {
		if ri.snap.Task == "supernova/t" {
			n++
		}
	}
	if n != 1 || len(w.subtaskMarks("t")) != 0 {
		t.Errorf("later groups don't run and their subtasks stay pending: %d runs, marks %+v", n, w.subtaskMarks("t"))
	}
}

func TestSequenceNextGroupWaitsForTheWholeGroup(t *testing.T) {
	w := groupWorld(t, "01-models", "02-client", "03-server", "04-models")
	w.mustSeq("run", "supernova/t")
	w.approve()
	w.passLoc(w.leadOf(w.stepRuns(1)[0]))
	ids := w.stepRuns(1)
	if len(ids) != 2 {
		t.Fatalf("group 2 is client and server: %v", ids)
	}
	clientLead, serverLead := w.leadOf(ids[0]), w.leadOf(ids[1])
	w.passLoc(clientLead)
	if got := w.stepRuns(1); len(got) != 2 || got[0] != ids[0] || w.step(1).Status != "active" {
		t.Fatalf("one run of the group is still going: %v %s", got, w.step(1).Status)
	}
	w.passLoc(serverLead)
	st := w.step(1)
	if len(st.Runs) != 1 || st.Runs[0].Subtask != "04-models" || st.Group != 3 || st.Status != "active" {
		t.Fatalf("the third group follows the whole second: %+v", st)
	}
}

func TestSequenceWaitingRunShowsItsReasonAndPausedStepStillNamesItsLead(t *testing.T) {
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
	if l := sequenceListing(w.bridge, w.sid); !strings.Contains(l, "(server) [waiting: run "+snap.Run) {
		t.Errorf("the waiting run should show why:\n%s", l)
	}
	if got := w.runState(ids[1]).Status; got != "pending" {
		t.Fatalf("the server run should still wait: %s", got)
	}

	client, err := openRun(w.bridge, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	client.State.Status, client.State.Reason = "ask-user", "which way?"
	client.save()
	client.close()
	holder, err := openRun(w.bridge, snap.Run)
	if err != nil {
		t.Fatal(err)
	}
	holder.State.Status = "success"
	holder.save()
	holder.close()

	res := w.stopHook()
	reason, _ := res["reason"].(string)
	if !strings.Contains(reason, "run "+ids[1]+" approved") {
		t.Errorf("the Stop hook should name the lead of the run that just started:\n%v", res)
	}
	if st := w.step(1); st.Status != "paused" {
		t.Errorf("the step should be paused: %s", st.Status)
	}
}
