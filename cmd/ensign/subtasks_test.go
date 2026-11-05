// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func (w *world) subtaskFile(task, name, content string) {
	w.t.Helper()
	write(w.t, filepath.Join(w.bridge, "projects/supernova/tasks", task, "subtasks", name+".md"), content)
}

// subtaskTask adds a task with the named subtasks, each with a goal of its own.
func (w *world) subtaskTask(task string, names ...string) {
	w.t.Helper()
	w.addTask(task)
	for _, n := range names {
		w.subtaskFile(task, n, "## Goal\n\nGoal of "+n+".\n\n## Scope\n\nScope of "+n+".\n")
	}
}

func (w *world) taskCmd(args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdTask(args, &out)
	return out.String(), err
}

func (w *world) snapOf(task string, o runOpts) (*Snapshot, error) {
	w.t.Helper()
	o.task = "supernova/" + task
	return createRun(w.bridge, o, true, io.Discard)
}

func (w *world) subtaskMarks(task string) map[string]subtaskMark {
	w.t.Helper()
	subs, err := loadSubtasks(taskDirOf(w.bridge, "supernova/"+task))
	if err != nil {
		w.t.Fatal(err)
	}
	marks, err := subtaskMarks(w.bridge, "supernova/"+task, subs)
	if err != nil {
		w.t.Fatal(err)
	}
	return marks
}

func TestSubtasksAreReadInOrderWithOrWithoutFrontmatter(t *testing.T) {
	w := newWorld(t)
	w.addTask("t")
	w.subtaskFile("t", "10-last", "## Goal\n\nlast\n")
	w.subtaskFile("t", "02-second", "---\nlocation: server\n---\n\n## Goal\n\nsecond\n")
	w.subtaskFile("t", "01-first", "---\r\nlocation: client\r\n---\r\n\r\n## Scope\r\n\r\nfirst scope\r\n")
	w.subtaskFile("t", "extra", "## Goal\n\nno number\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/subtasks/notes.txt"), "ignored")
	subs, err := loadSubtasks(filepath.Join(w.bridge, "projects/supernova/tasks/t"))
	if err != nil {
		t.Fatal(err)
	}
	var names, locs []string
	for _, s := range subs {
		names, locs = append(names, s.Name), append(locs, s.Location)
	}
	if want := []string{"01-first", "02-second", "10-last", "extra"}; !reflect.DeepEqual(names, want) {
		t.Errorf("order %v, want %v", names, want)
	}
	if want := []string{"client", "server", "", ""}; !reflect.DeepEqual(locs, want) {
		t.Errorf("locations %v, want %v", locs, want)
	}
	if subs[0].Scope != "first scope" || subs[0].Goal != "" || subs[2].Goal != "last" {
		t.Errorf("sections: %+v", subs)
	}
	w.addTask("plain")
	if subs, err := loadSubtasks(filepath.Join(w.bridge, "projects/supernova/tasks/plain")); err != nil || len(subs) != 0 {
		t.Errorf("a task without a subtasks folder has none: %v %v", subs, err)
	}
}

func TestSubtaskRunPicksNextPendingAndFreezesItsWorkItem(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-models-schema", "02-server-api", "03-client-login")
	snap, err := w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Subtask != "01-models-schema" {
		t.Fatalf("subtask %q", snap.Subtask)
	}
	for _, want := range []string{"Do t.", "## Subtask 01-models-schema", "### Goal\n\nGoal of 01-models-schema.", "### Scope\n\nScope of 01-models-schema."} {
		if !strings.Contains(snap.WorkItem, want) {
			t.Errorf("work item lacks %q:\n%s", want, snap.WorkItem)
		}
	}
	if strings.Contains(snap.WorkItem, "Already on the branch") || strings.Contains(snap.WorkItem, "02-server-api") {
		t.Errorf("nothing is on the branch yet and later subtasks stay out:\n%s", snap.WorkItem)
	}

	if _, err := w.taskCmd("subtask", "done", "supernova/t", "01-models-schema"); err != nil {
		t.Fatal(err)
	}
	snap, err = w.snapOf("t", runOpts{})
	if err != nil || snap.Subtask != "02-server-api" {
		t.Fatalf("next pending: %v %+v", err, snap)
	}
	if want := "## Already on the branch\n\n- 01-models-schema: Goal of 01-models-schema."; !strings.Contains(snap.WorkItem, want) {
		t.Errorf("work item lacks %q:\n%s", want, snap.WorkItem)
	}
	if strings.Contains(snap.WorkItem, "03-client-login") {
		t.Errorf("a later subtask is named:\n%s", snap.WorkItem)
	}

	snap, err = w.snapOf("t", runOpts{subtask: "03-client-login"})
	if err != nil || snap.Subtask != "03-client-login" {
		t.Fatalf("named: %v %+v", err, snap)
	}
	if !strings.Contains(snap.WorkItem, "- 01-models-schema:") || strings.Contains(snap.WorkItem, "02-server-api") {
		t.Errorf("only the done earlier subtask is listed:\n%s", snap.WorkItem)
	}
	if _, err := w.taskCmd("subtask", "drop", "supernova/t", "02-server-api"); err != nil {
		t.Fatal(err)
	}
	snap, _ = w.snapOf("t", runOpts{})
	if !strings.Contains(snap.WorkItem, "- 02-server-api: dropped, not on the branch") {
		t.Errorf("a dropped subtask is listed as such:\n%s", snap.WorkItem)
	}
	if _, err := w.snapOf("t", runOpts{subtask: "02-server-api"}); err == nil || !strings.Contains(err.Error(), "already dropped") {
		t.Errorf("a dropped subtask is not pending: %v", err)
	}
	if _, err := w.snapOf("t", runOpts{subtask: "nope"}); err == nil || !strings.Contains(err.Error(), "no subtask") {
		t.Errorf("unknown subtask: %v", err)
	}
}

func TestSubtaskRunFlagAndSummary(t *testing.T) {
	o, err := parseRunArgs("ensign run", []string{"supernova/t", "--subtask", "02-x"})
	if err != nil || o.subtask != "02-x" {
		t.Fatalf("%v %+v", err, o)
	}
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	if out := w.run("supernova/t", "--subtask", "02-b"); !strings.Contains(out, "subtask    02-b") {
		t.Errorf("the summary names the subtask:\n%s", out)
	}
}

func TestSubtaskRefusals(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a")
	w.subtaskFile("t", "02-empty", "# nothing here\n")
	if _, err := w.snapOf("t", runOpts{}); err == nil || !strings.Contains(err.Error(), "02-empty") || !strings.Contains(err.Error(), "neither") {
		t.Errorf("no goal or scope: %v", err)
	}
	os.Remove(filepath.Join(w.bridge, "projects/supernova/tasks/t/subtasks/02-empty.md"))
	if _, err := w.taskCmd("subtask", "done", "supernova/t", "nope"); err == nil || !strings.Contains(err.Error(), "no subtask") {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := w.taskCmd("subtask", "done", "supernova/t", "01-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.snapOf("t", runOpts{}); err == nil || !strings.Contains(err.Error(), "done or dropped") {
		t.Errorf("all done: %v", err)
	}
	w.addTask("plain")
	if _, err := w.snapOf("plain", runOpts{subtask: "01-a"}); err == nil || !strings.Contains(err.Error(), "no subtasks") {
		t.Errorf("--subtask on a plain task: %v", err)
	}
	if _, err := w.snapOf("plain", runOpts{}); err != nil {
		t.Errorf("a plain task runs as before: %v", err)
	}
	w.subtaskTask("s", "01-a")
	if _, err := w.seq("run", "supernova/s", "--subtask", "01-a"); err == nil || !strings.Contains(err.Error(), "never a subtask") {
		t.Errorf("sequence run --subtask: %v", err)
	}
}

func TestSubtaskLocations(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "multi", "client, server")
	dir := filepath.Join(w.bridge, "projects/supernova/tasks/multi")
	write(t, filepath.Join(dir, "subtasks/01-c.md"), "---\nlocation: client\n---\n\n## Goal\n\nclient part\n")
	write(t, filepath.Join(dir, "subtasks/02-s.md"), "---\nlocation: server\n---\n\n## Goal\n\nserver part\n")
	snap, err := createRun(w.bridge, runOpts{task: task, subtask: "02-s"}, true, io.Discard)
	if err != nil || snap.Location != "server" {
		t.Fatalf("the run takes the subtask's location: %v %+v", err, snap)
	}
	if _, err := createRun(w.bridge, runOpts{task: task, subtask: "02-s", location: "client"}, true, io.Discard); err == nil || !strings.Contains(err.Error(), "--location client") {
		t.Errorf("a different --location is refused: %v", err)
	}
	if _, err := createRun(w.bridge, runOpts{task: task, subtask: "02-s", location: "server"}, true, io.Discard); err != nil {
		t.Errorf("the same --location is fine: %v", err)
	}
	write(t, filepath.Join(dir, "subtasks/03-n.md"), "## Goal\n\nno location\n")
	if _, err := createRun(w.bridge, runOpts{task: task}, true, io.Discard); err == nil || !strings.Contains(err.Error(), "03-n must name its location") {
		t.Errorf("several locations need one on each subtask: %v", err)
	}
	write(t, filepath.Join(dir, "subtasks/03-n.md"), "---\nlocation: models\n---\n\n## Goal\n\nelsewhere\n")
	if _, err := createRun(w.bridge, runOpts{task: task}, true, io.Discard); err == nil || !strings.Contains(err.Error(), "does not use") {
		t.Errorf("a location outside the task's: %v", err)
	}
}

func TestSubtaskSuccessMarksItDoneAndJournalNamesIt(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.run("supernova/t")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	marks := w.subtaskMarks("t")
	if m := marks["01-a"]; m.Status != "done" || m.Run == "" {
		t.Errorf("marks %+v", marks)
	}
	if _, ok := marks["02-b"]; ok {
		t.Errorf("the other is still pending: %+v", marks)
	}
	history, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md"))
	if !strings.Contains(string(history), "), subtask 01-a: success.**") {
		t.Errorf("the journal line should name the subtask:\n%s", history)
	}
}

func TestSubtaskProgressInOpenAndListings(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b", "03-c")
	w.taskCmd("subtask", "done", "supernova/t", "01-a")
	out, err := w.taskCmd("subtask", "drop", "supernova/t", "02-b")
	if err != nil || !strings.Contains(out, "subtasks 2/3, next 03-c") {
		t.Fatalf("confirmation: %v %q", err, out)
	}
	if open, err := w.taskCmd("open", "supernova/t"); err != nil || !strings.Contains(open, "subtasks   2/3, next 03-c") {
		t.Errorf("task open: %v\n%s", err, open)
	}
	if got := projectSummary(w.bridge, "supernova"); !strings.Contains(got, "(subtasks 2/3, next 03-c)") {
		t.Errorf("project listing:\n%s", got)
	}
	if got := taskSummary(w.bridge, "supernova", "t"); !strings.Contains(got, "subtasks 2/3, next 03-c") {
		t.Errorf("task summary:\n%s", got)
	}
	if got := taskSummary(w.bridge, "supernova", "auth-refactor"); strings.Contains(got, "subtasks") {
		t.Errorf("a plain task shows no subtasks:\n%s", got)
	}
	w.taskCmd("subtask", "done", "supernova/t", "03-c")
	if got := taskSummary(w.bridge, "supernova", "t"); !strings.Contains(got, "subtasks 3/3\n") {
		t.Errorf("all done:\n%s", got)
	}
}

func TestSequenceStepWorksThroughSubtasks(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.mustSeq("run", "supernova/t")
	w.approve()
	first := w.step(1).Run
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	st := w.step(1)
	if st.Status != "active" || st.Run == first {
		t.Fatalf("the step should move on to the next subtask's run: %+v", st)
	}
	r, err := readRun(w.bridge, st.Run)
	if err != nil || r.Snap.Subtask != "02-b" || !strings.Contains(r.Snap.WorkItem, "- 01-a:") {
		t.Fatalf("second run: %v %+v", err, r.Snap)
	}
	if cur := w.state(); cur.Run != st.Run || !cur.Approved {
		t.Errorf("the next run should be bound and approved: %+v", cur)
	}
	lead, _ = w.spawn("", "", "orchestrator")
	w.passRun(lead)
	w.wantStatus("done")
	if m := w.subtaskMarks("t"); m["01-a"].Status != "done" || m["02-b"].Status != "done" {
		t.Errorf("marks %+v", m)
	}
}

func TestSequenceRetryRerunsTheSameSubtask(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.mustSeq("run", "supernova/t")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	first := w.step(1).Run
	w.retryStep("1")
	st := w.step(1)
	if st.Run == first || st.Prev != first {
		t.Fatalf("retry: %+v", st)
	}
	r, err := readRun(w.bridge, st.Run)
	if err != nil || r.Snap.Subtask != "01-a" || !strings.Contains(r.Snap.WorkItem, "## Subtask 01-a") {
		t.Fatalf("the retry runs the same subtask: %v %+v", err, r.Snap)
	}
	if len(w.subtaskMarks("t")) != 0 {
		t.Errorf("a failed run marks nothing: %+v", w.subtaskMarks("t"))
	}
}

func TestSequenceAcceptEndsTheStepBeforeLaterSubtasks(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.mustSeq("run", "supernova/t")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.mustSeq("accept", "1")
	w.wantStatus("failed")
	if len(w.subtaskMarks("t")) != 0 {
		t.Errorf("nothing is done: %+v", w.subtaskMarks("t"))
	}
}

func TestSequenceHaltStopsTheSubtaskChain(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.mustSeq("run", "supernova/t")
	w.approve()
	first := w.step(1).Run
	lead, _ := w.spawn("", "", "orchestrator")
	w.mustSeq("halt", "--reason", "enough")
	w.passRun(lead)
	st := w.step(1)
	if st.Status != "done" || st.Run != first || !strings.Contains(st.Reason, "02-b") {
		t.Fatalf("the halted step ends with its run: %+v", st)
	}
	if m := w.subtaskMarks("t"); m["01-a"].Status != "done" || m["02-b"].Status != "" {
		t.Errorf("marks %+v", m)
	}
	if sf := readSeq(w.bridge, w.sid); !sf.Sequences[0].Halted || sequenceUnfinished(w.bridge, w.sid) {
		t.Errorf("the sequence should close halted: %+v", sf.Sequences[0])
	}
}

func TestSequenceChainedSubtaskRunKeepsStaleBranch(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.mustSeq("run", "supernova/t")
	w.approve()
	r, err := openRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	r.State.StaleOK = true
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	if st := w.runState(w.step(1).Run); !st.StaleOK {
		t.Errorf("the chained run should keep stale_branch: %+v", st)
	}
}

func TestSubtaskProgressSurvivesLosingMachineState(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.run("supernova/t")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.passRun(lead)
	if err := os.RemoveAll(filepath.Join(w.bridge, ".starship/tasks")); err != nil {
		t.Fatal(err)
	}
	ch, err := pickSubtask(w.bridge, runOpts{task: "supernova/t"})
	if err != nil || ch.sub.Name != "02-b" || len(ch.earlier) != 1 || !strings.HasPrefix(ch.earlier[0], "- 01-a: Goal of 01-a") {
		t.Fatalf("the next run should follow the journal: %v %+v", err, ch)
	}
}

func TestTaskSubtaskByHandWritesJournalLine(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b", "03-c")
	if _, err := w.taskCmd("subtask", "done", "supernova/t", "01-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.taskCmd("subtask", "drop", "supernova/t", "02-b"); err != nil {
		t.Fatal(err)
	}
	history, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md"))
	for _, want := range []string{", subtask 01-a: done by hand.**", ", subtask 02-b: dropped by hand.**"} {
		if !strings.Contains(string(history), want) {
			t.Errorf("history lacks %q:\n%s", want, history)
		}
	}
	m := w.subtaskMarks("t")
	if m["01-a"].Status != "done" || m["02-b"].Status != "dropped" || len(m) != 2 {
		t.Errorf("marks %+v", m)
	}
}

func TestSubtaskMarksFromJournalText(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "01-a-b", "02-b", "03-c")
	hist := "# t: history\r\n\r\n" +
		"- **2026-01-01, run r1 (supernova), subtask 01-a: success.** 1 coder pass.\r\n\r\n" +
		"- **2026-01-02, run r2 (supernova), subtask 02-b: error (boom).**\r\n\r\n" +
		"- **2026-01-03, run r3 (supernova), subtask 03-c: paused to ask the user.**\r\n\r\n" +
		"- **2026-01-04, run r4 (supernova), subtask 09-gone: success.**\r\n"
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md"), hist)
	m := w.subtaskMarks("t")
	if len(m) != 1 || m["01-a"].Status != "done" || m["01-a"].Run != "r1" {
		t.Errorf("only the successful known subtask is marked: %+v", m)
	}
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md"), hist+"\r\n- **2026-01-05, subtask 01-a: dropped by hand.**\r\n")
	if m := w.subtaskMarks("t"); m["01-a"].Status != "dropped" {
		t.Errorf("the last line wins: %+v", m)
	}
}

func TestSubtaskMarksFromLegacyProgressLog(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	task := filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, string(data)+"\n## Progress log\n\n- **2026-01-01, run r1 (supernova), subtask 01-a: success.**\n")
	if m := w.subtaskMarks("t"); m["01-a"].Status != "done" || len(m) != 1 {
		t.Errorf("marks %+v", m)
	}
	if _, err := w.taskCmd("subtask", "done", "supernova/t", "02-b"); err != nil {
		t.Fatal(err)
	}
	if m := w.subtaskMarks("t"); m["02-b"].Status != "done" {
		t.Errorf("a hand mark goes in the progress log too: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md")); err == nil {
		t.Errorf("history.md should not be created while task.md keeps the log")
	}
}

func TestAddToJournalConcurrentLegacy(t *testing.T) {
	w := newWorld(t)
	names := []string{"01-a", "02-b", "03-c", "04-d", "05-e", "06-f"}
	w.subtaskTask("t", names...)
	task := filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, string(data)+"\n## Progress log\n\n")
	var wg sync.WaitGroup
	subs, _ := loadSubtasks(taskDirOf(w.bridge, "supernova/t"))
	for range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := subtaskMarks(w.bridge, "supernova/t", subs); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := "- **2026-01-01, run r-" + n + " (supernova), subtask " + n + ": success.**"
			if err := addToJournal(w.bridge, "supernova/t", entry); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := os.ReadFile(task)
	for _, n := range names {
		if !strings.Contains(string(got), "subtask "+n+": success") {
			t.Errorf("line for %s lost:\n%s", n, got)
		}
	}
	if m := w.subtaskMarks("t"); len(m) != len(names) {
		t.Errorf("marks %+v", m)
	}
}

func TestSequenceRetryOfASubtaskRunTakesEditedSubtaskAndTaskGoals(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	w.mustSeq("run", "supernova/t")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.writeTaskFile("t", "## Goal\n\nEdited task goal.\n")
	w.subtaskFile("t", "01-a", "## Goal\n\nEdited subtask goal.\n")
	w.retryStep("1")
	wi := w.stepWorkItem(1)
	for _, want := range []string{"Edited task goal.", "Edited subtask goal."} {
		if !strings.Contains(wi, want) {
			t.Errorf("the work item lacks %q:\n%s", want, wi)
		}
	}
	if strings.Contains(wi, "Do t.") || strings.Contains(wi, "Goal of 01-a.") || strings.Count(wi, "## Subtask 01-a") != 1 {
		t.Errorf("stale text or a doubled subtask:\n%s", wi)
	}
}
