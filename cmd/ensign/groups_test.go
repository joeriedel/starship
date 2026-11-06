// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
	for _, ri := range w.listRuns() {
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

func TestSequenceRetryListingKeepsEveryGroupSubtask(t *testing.T) {
	w := groupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	w.failRun(w.leadOf(ids[0]))
	w.passLoc(w.leadOf(ids[1]))
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\npipeline: coding\nlocations: [models, client, server]\n---\n\n# t\n\n## Goal\n\nEdited goal.\n")
	w.retryStep("1")
	got := sequenceListing(w.bridge, w.sid)
	if !strings.Contains(got, "Edited goal. (subtasks 01-client, 02-server)") || strings.Contains(got, "Do it.") {
		t.Errorf("the line should show the new goal and both subtasks:\n%s", got)
	}
}

func TestSequenceRetryListingKeepsASingleSubtask(t *testing.T) {
	w := groupWorld(t, "01-client")
	w.mustSeq("run", "supernova/t")
	w.approve()
	w.failRun(w.leadOf(w.stepRuns(1)[0]))
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\npipeline: coding\nlocations: [models, client, server]\n---\n\n# t\n\n## Goal\n\nEdited goal.\n")
	w.retryStep("1")
	got := sequenceListing(w.bridge, w.sid)
	if !strings.Contains(got, "Edited goal. (subtask 01-client)") {
		t.Errorf("the line should show the new goal and the subtask:\n%s", got)
	}
}

// githubGroupWorld is a groupWorld whose project uses github-pr with a test binding that passes locally, and whose location folders are git repos with a bare origin each. It returns the remote of each location.
func githubGroupWorld(t *testing.T, names ...string) (*world, map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	w := groupWorld(t, names...)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	s := strings.NewReplacer(
		`build: { command: "./build.sh" }`, `build: { not-applicable: "none here" }`,
		`{ command: "./test.sh" }`, localTest,
		`{ command: "./models-test.sh" }`, localTest,
		`{ command: "./client-test.sh" }`, localTest,
		`{ command: "./cross-test.sh" }`, localTest,
	).Replace(string(data))
	write(t, p, "use: [github-pr]\n"+s)

	remotes := map[string]string{}
	for _, loc := range []string{"client", "server", "models"} {
		c, err := loadConfigAt(w.bridge, "supernova/t", loc)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, c.Workspace, "init", "-q", "-b", "main")
		write(t, filepath.Join(c.Workspace, "lib.txt"), "one\n")
		runGit(t, c.Workspace, "add", "lib.txt")
		runGit(t, c.Workspace, "commit", "-q", "-m", "init")
		remote := filepath.Join(t.TempDir(), "remote.git")
		runGit(t, ".", "init", "-q", "--bare", "-b", "main", remote)
		runGit(t, c.Workspace, "remote", "add", "origin", remote)
		runGit(t, c.Workspace, "push", "-q", "-u", "origin", "main")
		runGit(t, c.Workspace, "remote", "set-head", "origin", "main")
		remotes[loc] = remote
	}

	gh := &fakeGH{prs: map[string]bool{}, conclusion: "success"}
	oldGH, oldPoll := runGH, checkPoll
	runGH, checkPoll = gh.run, time.Millisecond
	t.Cleanup(func() { runGH, checkPoll = oldGH, oldPoll })
	return w, remotes
}

// codeChangeIn is codeChange for a run of the locations fixture, changing the location's folder ws.
func (w *world) codeChangeIn(lead, ws string) {
	w.t.Helper()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		w.t.Fatalf("coder refused: %s", why)
	}
	write(w.t, filepath.Join(ws, "lib.txt"), "two\n")
	w.stop(coder, "coder", report("compiled"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
}

// finishRun codes a change in the run's location and runs its test stage, returning the run's final state.
func (w *world) finishRun(run string) *State {
	w.t.Helper()
	var snap Snapshot
	if err := readJSON(filepath.Join(runsDir(w.bridge), run, "pipeline.json"), &snap); err != nil {
		w.t.Fatal(err)
	}
	c, err := loadConfigAt(w.bridge, "supernova/t", snap.Location)
	if err != nil {
		w.t.Fatal(err)
	}
	w.codeChangeIn(w.leadOf(run), c.Workspace)
	var out bytes.Buffer
	if err := cmdRunStage([]string{"--bridge", w.bridge, "--run", run}, &out); err != nil || !strings.Contains(out.String(), "Outcome: pass") {
		w.t.Fatalf("run %s should pass: %v\n%s", run, err, out.String())
	}
	return w.runState(run)
}

func TestPublishAfterAGroupLandsEveryLocationOfTheGroup(t *testing.T) {
	w, remotes := githubGroupWorld(t, "01-client", "02-server")
	w.mustSeq("run", "supernova/t")
	w.mustSeq("publish", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	if len(ids) != 2 {
		t.Fatalf("the group is client and server: %v", ids)
	}
	for _, id := range ids {
		st := w.finishRun(id)
		if notes := strings.Join(st.Notes, "\n"); !strings.Contains(notes, "A publish step in the session's sequence takes it from here") || strings.Contains(notes, "Nothing has been opened yet") {
			t.Errorf("each run of the group should name the publish step:\n%s", notes)
		}
	}
	w.wantStatus("done", "done")
	if got := w.step(2).Reason; !strings.HasPrefix(got, "landed client (pushed to main: ") || !strings.Contains(got, "), server (pushed to main: ") {
		t.Errorf("the publish should name both locations: %q", got)
	}
	for loc, remote := range remotes {
		want := "one"
		if loc != "models" {
			want = "two"
		}
		if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != want {
			t.Errorf("main of %s has %q, want %q", loc, got, want)
		}
	}
	if publishRecorded(w.bridge, "supernova/t") {
		t.Error("a publish that landed everything leaves no record")
	}
}

func TestPublishLandsTheLocationsOfEarlierGroupsInDependencyOrder(t *testing.T) {
	w, remotes := githubGroupWorld(t, "01-client", "02-server", "03-models")
	w.mustSeq("run", "supernova/t")
	w.mustSeq("publish", "supernova/t")
	w.approve()
	ids := w.stepRuns(1)
	if len(ids) != 2 {
		t.Fatalf("the first group is client and server: %v", ids)
	}
	for _, id := range ids {
		st := w.finishRun(id)
		if notes := strings.Join(st.Notes, "\n"); !strings.Contains(notes, "A publish step in the session's sequence takes it from here") {
			t.Errorf("a run followed by a further group still gets the publish note:\n%s", notes)
		}
	}
	for _, id := range w.stepRuns(1) {
		w.finishRun(id)
	}
	w.wantStatus("done", "done")
	reason := w.step(2).Reason
	models, client, server := strings.Index(reason, "models ("), strings.Index(reason, "client ("), strings.Index(reason, "server (")
	if !strings.HasPrefix(reason, "landed ") || models < 0 || models > client || client > server {
		t.Errorf("models lands before client and server, which keep their run order: %q", reason)
	}
	for loc, remote := range remotes {
		if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
			t.Errorf("main of %s has %q, want two", loc, got)
		}
	}
}

func TestPublishLandsALocationOnceWhenGroupsRevisitIt(t *testing.T) {
	w, remotes := githubGroupWorld(t, "01-client", "02-models", "03-client")
	w.mustSeq("run", "supernova/t")
	w.mustSeq("publish", "supernova/t")
	w.approve()
	for i := 0; i < 3; i++ {
		for _, id := range w.stepRuns(1) {
			w.finishRun(id)
		}
	}
	w.wantStatus("done", "done")
	reason := w.step(2).Reason
	if strings.Count(reason, "client (") != 1 || strings.Count(reason, "models (") != 1 || strings.Contains(reason, "server") {
		t.Errorf("client and models each land once: %q", reason)
	}
	for _, loc := range []string{"client", "models"} {
		if got := runGit(t, ".", "--git-dir", remotes[loc], "show", "main:lib.txt"); got != "two" {
			t.Errorf("main of %s has %q, want two", loc, got)
		}
	}
}

// failedPublish runs a sequence of models, then client and server together, then a publish, with client's tested marker lost so landing it fails. It returns the world, the remotes and a function that puts the marker back.
func failedPublish(t *testing.T) (*world, map[string]string, func()) {
	t.Helper()
	w, remotes := githubGroupWorld(t, "01-models", "02-client", "03-server")
	w.mustSeq("run", "supernova/t")
	w.mustSeq("publish", "supernova/t")
	w.approve()
	w.finishRun(w.stepRuns(1)[0])
	ids := w.stepRuns(1)
	if len(ids) != 2 {
		t.Fatalf("the second group is client and server: %v", ids)
	}
	w.finishRun(ids[0])
	marker := testedFile(w.bridge, "supernova/t", "client")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	w.finishRun(ids[1])
	w.wantStatus("done", "failed")
	return w, remotes, func() { write(t, marker, string(data)) }
}

func TestPublishStopsAtTheFirstFailureAndReportsWhatLanded(t *testing.T) {
	w, remotes, _ := failedPublish(t)
	reason := w.step(2).Reason
	if !strings.HasPrefix(reason, "landed models (pushed to main: ") || !strings.Contains(reason, "); never tried server; client failed: ") || !strings.Contains(reason, "no passing run") {
		t.Errorf("the failure should name what landed, what was never tried and what failed:\n%s", reason)
	}
	for loc, want := range map[string]string{"models": "two", "client": "one", "server": "one"} {
		if got := runGit(t, ".", "--git-dir", remotes[loc], "show", "main:lib.txt"); got != want {
			t.Errorf("main of %s has %q, want %q", loc, got, want)
		}
	}
	if got := readPublishRecord(w.bridge, "supernova/t"); !reflect.DeepEqual(got, []string{"client", "server"}) {
		t.Errorf("the record lists what is left: %v", got)
	}
}

func TestANewPublishLandsOnlyWhatIsLeft(t *testing.T) {
	w, remotes, restore := failedPublish(t)
	restore()
	w.mustSeq("publish", "supernova/t")
	w.approve()
	w.wantStatus("done", "failed", "done")
	if got := w.step(3).Reason; !strings.HasPrefix(got, "landed client (pushed to main: ") || !strings.Contains(got, "), server (pushed to main: ") || strings.Contains(got, "models") {
		t.Errorf("the retry lands client and server only: %q", got)
	}
	for loc, remote := range remotes {
		if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
			t.Errorf("main of %s has %q, want two", loc, got)
		}
	}
	if publishRecorded(w.bridge, "supernova/t") {
		t.Error("nothing is left, so the record is gone")
	}
}

func TestADirectMergeShrinksTheRecordOfWhatIsLeft(t *testing.T) {
	w, _, restore := failedPublish(t)
	if _, _, _, err := githubMerge(w.bridge, "supernova/t", "server"); err != nil {
		t.Fatal(err)
	}
	if got := readPublishRecord(w.bridge, "supernova/t"); !reflect.DeepEqual(got, []string{"client"}) {
		t.Errorf("the record after landing server by hand: %v", got)
	}
	restore()
	w.mustSeq("publish", "supernova/t")
	w.approve()
	if got := w.step(3).Reason; !strings.HasPrefix(got, "pushed to main: ") {
		t.Errorf("one location is left, so the text is a single landing's: %q", got)
	}
	if publishRecorded(w.bridge, "supernova/t") {
		t.Error("nothing is left, so the record is gone")
	}
}

func holdPublishLock(t *testing.T, w *world, task string) {
	t.Helper()
	lock := taskStateFile(w.bridge, task, "", ".publish.lock")
	os.MkdirAll(filepath.Dir(lock), 0o755)
	l := flock.New(lock)
	if ok, err := l.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	t.Cleanup(func() { l.Unlock() })
}

func TestPublishRefusedWhileThePublishLockIsHeld(t *testing.T) {
	w, _, restore := failedPublish(t)
	restore()
	file := publishRecordFile(w.bridge, "supernova/t")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	holdPublishLock(t, w, "supernova/t")
	w.mustSeq("publish", "supernova/t")
	w.approve()
	w.wantStatus("done", "failed", "failed")
	if got := w.step(3).Reason; !strings.Contains(got, "a publish of supernova/t is already under way") {
		t.Errorf("the step should say why: %q", got)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("the record changed: %q -> %q", before, after)
	}
}

func TestDirectMergeAndThePublishLock(t *testing.T) {
	w, _, restore := failedPublish(t)
	restore()
	holdPublishLock(t, w, "supernova/t")
	merge := func() error {
		w.enter()
		var out bytes.Buffer
		return cmdGithubPR([]string{"merge", "supernova/t", "--location", "client"}, &out)
	}
	if err := merge(); err == nil || !strings.Contains(err.Error(), "already under way") {
		t.Errorf("merge with the lock held: %v", err)
	}
	t.Setenv(publishLockEnv, "supernova/other")
	if err := merge(); err == nil || !strings.Contains(err.Error(), "already under way") {
		t.Errorf("merge under another task's lock: %v", err)
	}
	t.Setenv(publishLockEnv, "supernova/t")
	if err := merge(); err != nil {
		t.Errorf("merge by the publish command's child: %v", err)
	}
}
