// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// plainWorld is a world whose project marks @test plain, with the given test command.
func plainWorld(t *testing.T, test string) *world {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the commands here are POSIX shell")
	}
	w := newWorld(t)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	s := strings.Replace(string(data), `  build: { command: "./build.sh" }`, `  build: { command: "echo built", plain: true }`, 1)
	s = strings.Replace(s, `  test:  { command: "./test.sh" }`, `  test:  { command: "`+test+`", plain: true }`, 1)
	write(t, p, s)
	return w
}

// toTest takes a run through code, review and style, up to its test stage.
func (w *world) toTest(lead string) {
	w.t.Helper()
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
}

// runStage runs `ensign run-stage` for the world's run, as the lead's background command would.
func (w *world) runStage() (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	err := cmdRunStage([]string{"--bridge", w.bridge, "--run", w.state().Run}, &out)
	return out.String(), err
}

// next runs `ensign next` for the world's run.
func (w *world) next() string {
	w.t.Helper()
	var out bytes.Buffer
	if err := cmdNext([]string{"--bridge", w.bridge, "--run", w.state().Run}, &out); err != nil {
		w.t.Fatal(err)
	}
	return out.String()
}

func TestPlainTestRunsWithoutAnAgent(t *testing.T) {
	w := plainWorld(t, "echo 3 passed")
	lead := w.startLead()
	w.toTest(lead)
	why, err := w.runStage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(why, "Outcome: pass") || !strings.Contains(why, "run is finished") {
		t.Fatalf("the refusal should carry the result and the next step: %s", why)
	}
	st := w.state()
	if st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	d := st.Dispatches[len(st.Dispatches)-1]
	if d.Role != "tester" || d.Outcome != "pass" || !strings.Contains(d.Summary, "@test `echo 3 passed`: exit 0") {
		t.Fatalf("the dispatch should record the run: %+v", d)
	}
	log, _ := os.ReadFile(filepath.Join(w.bridge, ".starship", "runs", st.Run, "reports", d.ID+".log"))
	if !strings.Contains(string(log), "built") || !strings.Contains(string(log), "3 passed") {
		t.Fatalf("the full output should be kept: %q", log)
	}
}

func TestRunStageSaysWhenTheDispatchLockIsHeld(t *testing.T) {
	w := plainWorld(t, "echo 3 passed")
	lead := w.startLead()
	w.toTest(lead)
	st := w.state()
	id := "d" + strconv.Itoa(len(st.Dispatches)+1)
	lock := flock.New(filepath.Join(w.bridge, ".starship", "runs", st.Run, "dispatch-"+id+".lock"))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer lock.Close()
	_, err := w.runStage()
	if err == nil {
		t.Fatal("run-stage should fail while the dispatch lock is held")
	}
	if !strings.Contains(err.Error(), "the lock is already held") || strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("the error should say the lock is held: %v", err)
	}
}

func TestPlainTestFailureGoesBackToCode(t *testing.T) {
	w := plainWorld(t, "echo 'FAIL: TestLogin'; echo 'login_test.go:9: rejected'; exit 1")
	lead := w.startLead()
	w.toTest(lead)
	why, err := w.runStage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(why, "Outcome: fail") || !strings.Contains(why, "FAIL: TestLogin\nlogin_test.go:9: rejected") || !strings.Contains(why, `stage "code"`) {
		t.Fatalf("a failure should quote the output and send the run back to code: %s", why)
	}
	st := w.state()
	if st.Status != "running" || st.Spent["test"] != 1 {
		t.Fatalf("status %s, test spent %d", st.Status, st.Spent["test"])
	}
}

func TestPlainTestThatCannotRunIsAnError(t *testing.T) {
	w := plainWorld(t, "./no-such-runner")
	lead := w.startLead()
	w.toTest(lead)
	why, err := w.runStage()
	if err != nil || !strings.Contains(why, "Outcome: error") {
		t.Fatalf("a missing runner is an error, not a failure: %s", why)
	}
	if st := w.state(); st.Status != "error" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
}

func TestPlainExit125IsAnError(t *testing.T) {
	w := plainWorld(t, "exit 125")
	lead := w.startLead()
	w.toTest(lead)
	if why, err := w.runStage(); err != nil || !strings.Contains(why, "Outcome: error") {
		t.Fatalf("exit 125 means the command could not run: %s", why)
	}
}

func TestPlainNeedsACommand(t *testing.T) {
	w := newWorld(t)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `  test:  { command: "./test.sh" }`, `  test:  { not-applicable: "none", plain: true }`, 1))
	if _, err := loadConfig(w.bridge, "supernova/auth-refactor"); err == nil || !strings.Contains(err.Error(), "plain binding needs a command") {
		t.Fatalf("plain without a command should be refused: %v", err)
	}
}

func TestHookSettingsUpgrade(t *testing.T) {
	b := t.TempDir()
	f := filepath.Join(b, ".claude", "settings.json")
	for name, old := range map[string]string{
		"no timeouts":                                 hookSettingsWith(0, 0),
		"PreToolUse timeout":                          hookSettingsWith(hookTimeout, 0),
		"no timeouts, before SubagentHandback":        beforeHandback(hookSettingsWith(0, 0)),
		"PreToolUse timeout, before SubagentHandback": beforeHandback(hookSettingsWith(hookTimeout, 0)),
	} {
		write(t, f, old)
		_ = upgradeHookSettings(b, &BridgeFile{})
		if data, _ := os.ReadFile(f); string(data) != hookSettings() {
			t.Fatalf("settings with %s should be upgraded", name)
		}
	}
	write(t, f, strings.Replace(hookSettingsWith(0, 0), "ensign:*", "ensign:*\", \"Bash(make:*)", 1))
	_ = upgradeHookSettings(b, &BridgeFile{})
	if data, _ := os.ReadFile(f); strings.Contains(string(data), "timeout") {
		t.Fatal("an edited settings file must be left alone")
	}
}

func TestHookSettingsMatchBashAndUpgradeFromBefore(t *testing.T) {
	for _, s := range []string{hookSettings(), hookSettingsFor(true, []string{"mcp__supernova_mail__read_message"}), hookSettingsFor(true, nil)} {
		if !strings.Contains(s, `NotebookEdit|SubagentHandback|Bash`) {
			t.Fatalf("PreToolUse does not match Bash:\n%s", s)
		}
	}
	b := t.TempDir()
	f := filepath.Join(b, ".claude", "settings.json")
	forms := map[string]string{}
	for name, form := range map[string]string{"both timeouts": hookSettings(), "no timeouts": hookSettingsWith(0, 0), "PreToolUse timeout": hookSettingsWith(hookTimeout, 0)} {
		forms[name+", before Bash"] = withoutBash(form)
		forms[name+", before Bash and SubagentHandback"] = beforeHandback(withoutBash(form))
	}
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	declared := &BridgeFile{Connectors: map[string][]string{"reader": {"mcp__a__x"}}}
	for name, old := range forms {
		if old == hookSettings() || strings.Contains(old, "SubagentHandback|Bash\"") || strings.Contains(old, "NotebookEdit|Bash\"") {
			t.Fatalf("%s still matches Bash", name)
		}
		write(t, f, old)
		if err := upgradeHookSettings(b, &BridgeFile{}); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(f); string(data) != hookSettings() {
			t.Fatalf("settings %s should be upgraded", name)
		}
		write(t, f, crlf(old))
		if err := upgradeHookSettings(b, &BridgeFile{}); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(f); string(data) != hookSettings() || !strings.Contains(string(data), "|Bash") {
			t.Fatalf("an undeclared CRLF file %s should be upgraded to LF", name)
		}
		write(t, f, crlf(old))
		if err := upgradeHookSettings(b, declared); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(f); string(data) != hookSettingsFor(true, []string{"mcp__a__x"}) {
			t.Fatalf("a CRLF file %s on a declared bridge should be upgraded", name)
		}
		withConn := strings.Replace(old, `", "hooks"`, `|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool|Read|Grep", "hooks"`, 1)
		if withConn == old {
			t.Fatalf("%s: the connector parts were not added", name)
		}
		write(t, f, withConn)
		if err := upgradeHookSettings(b, declared); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(f); string(data) != hookSettingsFor(true, []string{"mcp__a__x"}) {
			t.Fatalf("settings %s with connectors should be upgraded", name)
		}
	}
	edited := strings.Replace(withoutBash(hookSettings()), "ensign:*", "ensign:*\", \"Bash(make:*)", 1)
	write(t, f, edited)
	_ = upgradeHookSettings(b, &BridgeFile{})
	if data, _ := os.ReadFile(f); string(data) != edited {
		t.Fatal("an edited settings file must be left alone")
	}
}

// gatedPlainRun starts a plain test stage whose command blocks until the returned release
// is called, and waits until it is running. wait returns the command's output.
func gatedPlainRun(t *testing.T) (w *world, run string, release func(), wait func() string) {
	t.Helper()
	dir := t.TempDir()
	started, gate := filepath.Join(dir, "started"), filepath.Join(dir, "gate")
	w = plainWorld(t, "touch "+started+"; i=0; while [ ! -f "+gate+" ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done; echo gated")
	lead := w.startLead()
	w.toTest(lead)
	run = w.state().Run
	done := make(chan string, 1)
	go func() {
		out, err := w.runStage()
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if i > 400 {
			t.Fatal("the test command never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	release = func() { os.WriteFile(gate, nil, 0o644) }
	wait = func() string {
		select {
		case res := <-done:
			return res
		case <-time.After(20 * time.Second):
			t.Fatal("run-stage did not finish")
			return ""
		}
	}
	return
}

// lockRun opens the run from outside run-stage, failing if its lock is still held.
func lockRun(t *testing.T, w *world, run string) *RunFiles {
	t.Helper()
	got := make(chan *RunFiles, 1)
	go func() {
		r, err := openRun(w.bridge, run)
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	select {
	case r := <-got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the run lock is held while the plain stage's commands run")
		return nil
	}
}

func TestPlainStageDoesNotHoldTheLockWhileRunning(t *testing.T) {
	w, run, release, wait := gatedPlainRun(t)
	r := lockRun(t, w, run)
	d := r.State.Dispatches[len(r.State.Dispatches)-1]
	if d.Role != "tester" || d.Done {
		t.Fatalf("the dispatch should be recorded as running: %+v", d)
	}
	r.State.note("left by another hook")
	r.save()
	r.close()

	release()
	res := wait()
	if !strings.Contains(res, "Outcome: pass") {
		t.Fatalf("unexpected decision: %v", res)
	}
	st := w.state()
	d = st.dispatch(d.ID)
	if st.Status != "success" || !d.Done || d.Outcome != "pass" {
		t.Fatalf("status %s, dispatch %+v", st.Status, d)
	}
	if !strings.Contains(strings.Join(st.Notes, "\n"), "left by another hook") {
		t.Fatalf("a change made while the commands ran was lost: %v", st.Notes)
	}
}

func TestPlainStageResultDoesNotChangeARunThatEndedMeanwhile(t *testing.T) {
	w, run, release, wait := gatedPlainRun(t)
	r := lockRun(t, w, run)
	r.State.Status, r.State.Reason = "error", "stopped by the user"
	r.State.Finished = true
	r.save()
	r.close()

	release()
	wait()
	st := w.state()
	d := st.Dispatches[len(st.Dispatches)-1]
	if st.Status != "error" || st.Reason != "stopped by the user" {
		t.Fatalf("the run's outcome changed: %s (%s)", st.Status, st.Reason)
	}
	if !d.Done || d.Outcome != "pass" {
		t.Fatalf("the result should still be recorded: %+v", d)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, ".starship", "runs", run, "reports", d.ID+".log")); err != nil {
		t.Fatal(err)
	}
}

func TestNextAtAPlainStageNamesRunStageAndRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	w := plainWorld(t, "touch "+marker)
	lead := w.startLead()
	w.toTest(lead)
	got := w.next()
	run := w.state().Run
	want := "ensign run-stage --bridge " + shellQuote(w.bridge) + " --run " + run
	if !strings.Contains(got, want) || !strings.Contains(got, "run_in_background") {
		t.Fatalf("next should give the run-stage command: %s", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("next ran the test command")
	}
}

func TestRunStageRefusesWhenTheDueStageIsNotPlain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	w := plainWorld(t, "touch "+marker)
	w.startLead()
	before := w.state()
	out, err := w.runStage()
	if err == nil || !strings.Contains(err.Error(), `"code"`) || out != "" {
		t.Fatalf("run-stage at code should be refused, naming it: %q, %v", out, err)
	}
	if after := w.state(); len(after.Dispatches) != len(before.Dispatches) || len(after.History) != len(before.History) {
		t.Fatal("a refused run-stage changed the run")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the test command ran")
	}
}

func TestSpawningAPlainTesterIsDenied(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	w := plainWorld(t, "touch "+marker)
	lead := w.startLead()
	w.toTest(lead)
	before := len(w.state().Dispatches)
	id, why := w.spawn(lead, "orchestrator", "tester")
	if id != "" || !strings.Contains(why, "ensign run-stage --bridge") {
		t.Fatalf("the spawn should be denied with the command: %q %s", id, why)
	}
	st := w.state()
	if len(st.Dispatches) != before || st.Spent["test"] != 0 {
		t.Fatalf("a denied spawn recorded a dispatch or spent budget: %d dispatches, spent %v", len(st.Dispatches), st.Spent)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the test command ran")
	}
}

func TestLeadWaitsOnItsRunStageCommand(t *testing.T) {
	w, _, release, wait := gatedPlainRun(t)
	lead := ""
	for _, d := range w.state().Dispatches {
		if d.Role == "orchestrator" {
			lead = d.AgentID
		}
	}
	w.launched(lead, "orchestrator", "Bash", map[string]any{"backgroundTaskId": "bash1"})
	shell := []map[string]any{{"id": "bash1", "type": "shell", "status": "running"}}
	if r := w.stopWith(lead, "orchestrator", "Waiting for the tests.", false, shell); r != "" {
		t.Fatalf("a lead waiting on its run-stage command should be let stop: %s", r)
	}
	if left := w.stopsLeft(lead); left != stopBudget {
		t.Fatalf("a wait must not spend the stop budget, got %d left", left)
	}
	release()
	wait()
}

func TestNextAfterRunStageReportsTheOutcome(t *testing.T) {
	w := plainWorld(t, "echo 'FAIL: TestLogin'; exit 1")
	lead := w.startLead()
	w.toTest(lead)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	got := w.next()
	if !strings.Contains(got, `Stage "test" (run by ensign) finished: fail.`) || !strings.Contains(got, ".md") || !strings.Contains(got, `"code"`) {
		t.Fatalf("next should report the outcome, its report and what is due: %s", got)
	}
}

func TestRunStageFailingBeforeHandlerFinishesTheRun(t *testing.T) {
	w := plainWorld(t, "echo 3 passed")
	w.project(`events:
  stage.before:test: [{ require: "exit 1" }]
  run.finish:error: [{ observe: "echo finished > finished.txt" }]
`)
	lead := w.startLead()
	w.toTest(lead)
	if _, err := w.runStage(); err == nil {
		t.Fatal("run-stage should report that the stage could not start")
	}
	st := w.state()
	if st.Status != "error" || !st.Finished {
		t.Fatalf("the run should be error and finished: %s finished=%v", st.Status, st.Finished)
	}
	ws := filepath.Join(filepath.Dir(w.bridge), "workspace")
	if _, err := os.Stat(filepath.Join(ws, "finished.txt")); err != nil {
		t.Error("run.finish:error did not run")
	}
}

func TestSpawningTestersForAParallelStageIsNotDenied(t *testing.T) {
	w := plainWorld(t, "echo 3 passed")
	lead := w.startLead()
	w.toTest(lead)
	r, err := openSessionRun(w.bridge, w.sid)
	if err != nil || r == nil {
		t.Fatalf("no active run: %v", err)
	}
	path := filepath.Join(r.Dir, "pipeline.json")
	r.close()
	var snap Snapshot
	if err := readJSON(path, &snap); err != nil {
		t.Fatal(err)
	}
	s, _, _ := snap.Pipeline.find("test")
	s.Parallel = &Parallel{Over: "dimensions", Join: "all"}
	s.Dimensions = []string{"unit", "integration"}
	data, _ := json.Marshal(&snap)
	write(t, path, string(data))
	if id, why := w.spawn(lead, "orchestrator", "tester"); id == "" {
		t.Fatalf("a parallel tester spawn should not be denied: %s", why)
	}
}

// diedRun takes a run to its plain test stage, whose `ensign run-stage` is a separate process that kills itself, leaving the dispatch running with its lock free.
func diedRun(t *testing.T) (w *world, lead string) {
	t.Helper()
	return diedRunIn(t, plainWorld(t, "kill -9 $PPID"))
}

// diedRunIn is diedRun for a world whose test command kills its run-stage.
func diedRunIn(t *testing.T, w *world) (*world, string) {
	t.Helper()
	lead := w.startLead()
	w.toTest(lead)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "run-stage", "--bridge", w.bridge, "--run", w.state().Run)
	cmd.Env = append(os.Environ(), "STARSHIP_TEST_ENSIGN=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("the run-stage process should have been killed")
	}
	st := w.state()
	if d := st.Dispatches[len(st.Dispatches)-1]; d.Stage != "test" || d.Done {
		t.Fatalf("the test dispatch should be left running: %+v", d)
	}
	return w, lead
}

func TestDeadRunStageIsReportedAndRecordedAsError(t *testing.T) {
	w, _ := diedRun(t)
	stateFile := filepath.Join(w.bridge, ".starship", "runs", w.state().Run, "state.json")
	before, _ := os.ReadFile(stateFile)
	if got := w.next(); !strings.Contains(got, "died before it finished") || !strings.Contains(got, "ensign run-stage") || strings.Contains(got, "still running") {
		t.Fatalf("next should report the dead stage: %q", got)
	}
	if after, _ := os.ReadFile(stateFile); string(after) != string(before) {
		t.Fatal("next changed the state")
	}
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "ran nothing") {
		t.Fatalf("run-stage should record the death: %q %v", out, err)
	}
	st := w.state()
	d := st.Dispatches[len(st.Dispatches)-1]
	if !d.Done || d.Outcome != "error" || !strings.Contains(d.Summary, "died before it finished") {
		t.Fatalf("dispatch %+v", d)
	}
	if st.Status != "error" || !st.Finished {
		t.Fatalf("the run should end with error: %s finished=%v", st.Status, st.Finished)
	}
}

func TestSubagentStopCompletesADeadPlainStage(t *testing.T) {
	w, lead := diedRun(t)
	if r := w.stop(lead, "orchestrator", "waiting", false); r != "" {
		t.Fatalf("unexpected block: %s", r)
	}
	st := w.state()
	d := st.Dispatches[len(st.Dispatches)-1]
	if !d.Done || d.Outcome != "error" || st.Status != "error" {
		t.Fatalf("dispatch %+v, status %s", d, st.Status)
	}
}

func TestLivePlainStageIsNeverTakenOver(t *testing.T) {
	w, _, release, wait := gatedPlainRun(t)
	if got := w.next(); !strings.Contains(got, "still running") {
		t.Fatalf("next should say it is running: %q", got)
	}
	if _, err := w.runStage(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("run-stage should refuse: %v", err)
	}
	st := w.state()
	leadAgent := ""
	for _, d := range st.Dispatches {
		if d.Role == "orchestrator" {
			leadAgent = d.AgentID
		}
	}
	if r := w.stop(leadAgent, "orchestrator", "waiting", false); r != "" {
		t.Fatalf("the lead may wait: %s", r)
	}
	if d := w.state().Dispatches[len(st.Dispatches)-1]; d.Done {
		t.Fatalf("the live dispatch was taken over: %+v", d)
	}
	release()
	if res := wait(); !strings.Contains(res, "Outcome: pass") {
		t.Fatalf("unexpected: %v", res)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s", st.Status)
	}
}

func TestBindingTimeout(t *testing.T) {
	if defaultBindingTimeout != 60*time.Minute {
		t.Fatalf("default limit %s", defaultBindingTimeout)
	}
	w := plainWorld(t, "sleep 5")
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `command: "sleep 5", plain: true`, `command: "sleep 5", plain: true, timeout: 1s`, 1))
	cfg, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil || cfg.Bindings["test"].Timeout != time.Second {
		t.Fatalf("timeout not read: %v", err)
	}
	w.toTest(w.startLead())
	start := time.Now()
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "timed out") {
		t.Fatalf("a command over its timeout should be an error: %q %v", out, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("the timeout was not applied")
	}
	for _, bad := range []string{"soon", "0s", "-5m"} {
		write(t, p, strings.Replace(string(data), `command: "sleep 5", plain: true`, `command: "sleep 5", plain: true, timeout: `+bad, 1))
		if _, err := loadConfig(w.bridge, "supernova/auth-refactor"); err == nil || !strings.Contains(err.Error(), "timeout must be") {
			t.Fatalf("timeout %q should be refused: %v", bad, err)
		}
	}
}

// editLastDispatch rewrites the run's newest dispatch and state from outside any ensign command.
func editLastDispatch(t *testing.T, w *world, edit func(*State, *Dispatch)) {
	t.Helper()
	r := lockRun(t, w, w.state().Run)
	edit(r.State, r.State.Dispatches[len(r.State.Dispatches)-1])
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.close()
}

func TestUnlockedRunningPlainStageIsTreatedAsLive(t *testing.T) {
	w, lead := diedRun(t)
	editLastDispatch(t, w, func(_ *State, d *Dispatch) { d.Locked = false })
	if got := w.next(); strings.Contains(got, "died before it finished") || !strings.Contains(got, "still running") {
		t.Fatalf("next should treat it as running: %q", got)
	}
	if _, err := w.runStage(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("run-stage should refuse: %v", err)
	}
	if r := w.stop(lead, "orchestrator", "waiting", false); r != "" {
		t.Fatalf("the lead may wait: %s", r)
	}
	st := w.state()
	if d := st.Dispatches[len(st.Dispatches)-1]; d.Done || st.Finished {
		t.Fatalf("the dispatch was taken over: %+v", d)
	}
}

func TestNextOnATerminalRunDoesNotReportADeadOwner(t *testing.T) {
	w, _ := diedRun(t)
	editLastDispatch(t, w, func(st *State, _ *Dispatch) {
		st.Status, st.Reason, st.Finished = "error", "stopped by the user", true
	})
	if got := w.next(); strings.Contains(got, "died before it finished") || strings.Contains(got, "run-stage") {
		t.Fatalf("next on a terminal run: %q", got)
	}
}

// sidesPlainWorld is a locations world whose bindings are all plain and leave a marker file in the folder they ran in.
func sidesPlainWorld(t *testing.T, clientTest string) *world {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the commands here are POSIX shell")
	}
	w := locationsWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), `platforms: [linux]

bindings:
  build: { command: "touch built", plain: true }
  test:  { command: "touch tested", plain: true }
  lint:  { not-applicable: "no linter" }
  apply-dependency: { not-applicable: "these sides build from the sibling checkout" }

locations:
  models: {}
  client:
    depends-on: [models]
    bindings:
      test: { command: "`+clientTest+`", plain: true }
  server:
    depends-on: [models]

cross-location:
  bindings:
    test: { command: "touch cross", plain: true }
`)
	return w
}

// toTestOneReview is toTest for a fixture whose project does not append review dimensions: the review stage takes one reviewer.
func (w *world) toTestOneReview(lead string) {
	w.t.Helper()
	w.leaf(lead, "coder", "compiled")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
}

func (w *world) sideFolder(loc string) string {
	return filepath.FromSlash(loadMachine(w.bridge).Locations["supernova"][loc])
}

func markerExists(parts ...string) bool {
	_, err := os.Stat(filepath.Join(parts...))
	return err == nil
}

func TestPlainStageRunsEverySideAndTheCrossTest(t *testing.T) {
	w := sidesPlainWorld(t, "touch tested")
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	lead, why := w.spawn("", "", "orchestrator")
	if lead == "" {
		t.Fatalf("lead refused: %s", why)
	}
	w.toTestOneReview(lead)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	for _, l := range []string{"models", "client", "server"} {
		if !markerExists(w.sideFolder(l), "built") || !markerExists(w.sideFolder(l), "tested") {
			t.Errorf("side %s should have been built and tested in its own folder", l)
		}
	}
	if !markerExists(w.sideFolder("models"), "cross") || markerExists(w.sideFolder("client"), "cross") {
		t.Error("the cross test should run once, in the edited location's folder")
	}
}

func TestPlainStageStopsAtTheFirstFailingSide(t *testing.T) {
	w := sidesPlainWorld(t, "exit 1")
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.toTestOneReview(lead)
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: fail") || !strings.Contains(out, "side client: @test") {
		t.Fatalf("the failing side should be named: %v\n%s", err, out)
	}
	if markerExists(w.sideFolder("server"), "built") || markerExists(w.sideFolder("models"), "cross") {
		t.Error("nothing should run after the failure")
	}
}

func TestPlainStageOfASingleSideRunsAsBefore(t *testing.T) {
	w := sidesPlainWorld(t, "touch tested")
	w.run(locTask(t, w, "c", "client"))
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.toTestOneReview(lead)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	if !markerExists(w.sideFolder("client"), "tested") || markerExists(w.sideFolder("models"), "tested") || markerExists(w.sideFolder("client"), "cross") {
		t.Error("a run of client alone tests client alone")
	}
}

// servicesWorld is a plain world whose build, test, up and down commands each append their name to an order file in the workspace.
func servicesWorld(t *testing.T, test, up, down string) *world {
	t.Helper()
	w := plainWorld(t, test)
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	s := strings.Replace(string(data), `command: "echo built"`, `command: "echo build >> order"`, 1)
	s = strings.Replace(s, "bindings:\n", "bindings:\n  services-up: { command: \""+up+"\" }\n  services-down: { command: \""+down+"\" }\n", 1)
	write(t, p, s)
	return w
}

func (w *world) order() string {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(w.bridge), "workspace", "order"))
	return strings.Join(strings.Fields(string(b)), " ")
}

func lastDispatch(w *world) *Dispatch {
	st := w.state()
	return st.Dispatches[len(st.Dispatches)-1]
}

func TestServicesRunAroundBuildAndTest(t *testing.T) {
	w := servicesWorld(t, "echo test >> order", "echo up >> order", "echo down >> order")
	w.toTest(w.startLead())
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: pass") {
		t.Fatalf("%q %v", out, err)
	}
	if got := w.order(); got != "up build test down" {
		t.Fatalf("order %q", got)
	}
	if d := lastDispatch(w); !strings.Contains(d.Summary, "@services-up `echo up >> order`: exit 0") || !strings.Contains(d.Summary, "@services-down `echo down >> order`: exit 0") {
		t.Fatalf("summary %q", d.Summary)
	}
}

func TestServicesStopAfterATestFailure(t *testing.T) {
	w := servicesWorld(t, "echo test >> order; exit 1", "echo up >> order", "echo down >> order")
	w.toTest(w.startLead())
	if out, err := w.runStage(); err != nil || !strings.Contains(out, "Outcome: fail") {
		t.Fatalf("%q %v", out, err)
	}
	if got := w.order(); got != "up build test down" {
		t.Fatalf("order %q", got)
	}
}

func TestServicesStopAfterATestTimeout(t *testing.T) {
	w := servicesWorld(t, "sleep 5", "echo up >> order", "echo down >> order")
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `command: "sleep 5", plain: true`, `command: "sleep 5", plain: true, timeout: 1s`, 1))
	w.toTest(w.startLead())
	if out, err := w.runStage(); err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "timed out") {
		t.Fatalf("%q %v", out, err)
	}
	if got := w.order(); got != "up build down" {
		t.Fatalf("order %q", got)
	}
}

func TestFailingServicesUpIsAnErrorAndStillRunsDown(t *testing.T) {
	w := servicesWorld(t, "echo test >> order", "echo up >> order; exit 1", "echo down >> order")
	w.toTest(w.startLead())
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "services did not start") {
		t.Fatalf("a failing up is an error, not a failure: %q %v", out, err)
	}
	if got := w.order(); got != "up down" {
		t.Fatalf("order %q", got)
	}
}

func TestFailingServicesDownIsAnError(t *testing.T) {
	w := servicesWorld(t, "echo test >> order", "echo up >> order", "echo down >> order; exit 3")
	w.toTest(w.startLead())
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "exit 3") || !strings.Contains(out, "the result without that was pass") {
		t.Fatalf("a failing down is an error and keeps the result: %q %v", out, err)
	}
	if got := w.order(); got != "up build test down" {
		t.Fatalf("order %q", got)
	}
}

func TestServicesWithGuidanceOnlyNeedATester(t *testing.T) {
	w := servicesWorld(t, "echo test >> order", "echo up >> order", "echo down >> order")
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `services-up: { command: "echo up >> order" }`, `services-up: { guidance: "start the stack by hand" }`, 1))
	w.toTest(w.startLead())
	if _, err := w.runStage(); err == nil || !strings.Contains(err.Error(), "not a plain stage") {
		t.Fatalf("run-stage should refuse: %v", err)
	}
}

func TestDeadRunStageStopsItsServices(t *testing.T) {
	w, _ := diedRunIn(t, servicesWorld(t, "kill -9 $PPID", "echo up >> order", "echo down >> order"))
	if got := w.order(); got != "up build" {
		t.Fatalf("order before recovery %q", got)
	}
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: error") {
		t.Fatalf("%q %v", out, err)
	}
	if got := w.order(); got != "up build down" {
		t.Fatalf("recovery should run down once: %q", got)
	}
	if d := lastDispatch(w); !d.Done || !strings.Contains(d.Summary, "@services-down") {
		t.Fatalf("dispatch %+v", d)
	}
	if !strings.Contains(out, "ran only its @services-down") || strings.Contains(out, "ran nothing") || !strings.Contains(out, "exit 0") {
		t.Fatalf("the reply should say the down ran and how it went: %q", out)
	}
}

func TestDeadRunStageReportsAFailedDown(t *testing.T) {
	w, _ := diedRunIn(t, servicesWorld(t, "kill -9 $PPID", "echo up >> order", "exit 3"))
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "@services-down") || !strings.Contains(out, "a service may still be running") {
		t.Fatalf("the reply should report the failed down: %q %v", out, err)
	}
}

func TestStopServicesIsCappedByTheDeadline(t *testing.T) {
	w := servicesWorld(t, "echo ok", "echo up >> order", "sleep 30")
	w.toTest(w.startLead())
	r := lockRun(t, w, w.state().Run)
	defer r.close()
	sides := r.Snap.sides()
	start := time.Now()
	steps, _, failed := stopServices(r, &Dispatch{ID: "d-cap"}, sides, false, start.Add(time.Second))
	if !failed || time.Since(start) > 10*time.Second || len(steps) != 1 || !strings.Contains(steps[0], "a service may still be running") {
		t.Fatalf("the down should be cut off and counted as failed: %v %v", failed, steps)
	}
}

func TestSecondSignalDoesNotCancelTheDown(t *testing.T) {
	w := servicesWorld(t, "touch started; sleep 30", "echo up >> order", "touch downstarted; sleep 2; echo down >> order")
	w.toTest(w.startLead())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "run-stage", "--bridge", w.bridge, "--run", w.state().Run)
	cmd.Env = append(os.Environ(), "STARSHIP_TEST_ENSIGN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := func(marker string) {
		path := filepath.Join(filepath.Dir(w.bridge), "workspace", marker)
		for i := 0; !markerExists(path); i++ {
			if i > 200 {
				cmd.Process.Kill()
				t.Fatalf("%s never appeared", marker)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	wait("started")
	cmd.Process.Signal(syscall.SIGTERM)
	wait("downstarted")
	cmd.Process.Signal(syscall.SIGINT)
	cmd.Wait()
	if got := w.order(); got != "up build down" {
		t.Fatalf("the down should finish despite a second signal: %q", got)
	}
	if d := lastDispatch(w); !d.Done || d.Outcome != "error" || strings.Contains(d.Summary, "a service may still be running") {
		t.Fatalf("dispatch %+v", d)
	}
}

func TestCancelledRunStageStopsServicesBeforeDying(t *testing.T) {
	w := servicesWorld(t, "touch started; sleep 30", "echo up >> order", "echo down >> order")
	w.toTest(w.startLead())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "run-stage", "--bridge", w.bridge, "--run", w.state().Run)
	cmd.Env = append(os.Environ(), "STARSHIP_TEST_ENSIGN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(filepath.Dir(w.bridge), "workspace", "started")
	for i := 0; !markerExists(started); i++ {
		if i > 200 {
			cmd.Process.Kill()
			t.Fatal("the test command never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("ensign should still die of the signal: %v", err)
	}
	if got := w.order(); got != "up build down" {
		t.Fatalf("order %q", got)
	}
	if d := lastDispatch(w); !d.Done || d.Outcome != "error" || !strings.Contains(d.Summary, "interrupted") {
		t.Fatalf("the interrupted stage should be recorded as an error: %+v", d)
	}
}

func TestServicesRunPerSideInTheirOwnFolders(t *testing.T) {
	w := sidesPlainWorld(t, "touch tested")
	shared := filepath.Join(t.TempDir(), "shared")
	p := filepath.Join(w.bridge, "projects", "supernova", "project.yaml")
	data, _ := os.ReadFile(p)
	svc := func(name string) string {
		return `  services-` + name + `: { command: "echo ` + name + `-$(basename $PWD) >> ` + shared + `; echo ` + name + ` >> order" }` + "\n"
	}
	write(t, p, strings.Replace(string(data), "bindings:\n", "bindings:\n"+svc("up")+svc("down"), 1))
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	lead, why := w.spawn("", "", "orchestrator")
	if lead == "" {
		t.Fatalf("lead refused: %s", why)
	}
	w.toTestOneReview(lead)
	if _, err := w.runStage(); err != nil {
		t.Fatal(err)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	b, _ := os.ReadFile(shared)
	lines := strings.Fields(string(b))
	if len(lines) != 6 {
		t.Fatalf("shared log %q", lines)
	}
	for i := 0; i < 3; i++ {
		up, down := strings.TrimPrefix(lines[i], "up-"), strings.TrimPrefix(lines[5-i], "down-")
		if up != down {
			t.Fatalf("downs should run in reverse order of ups: %q", lines)
		}
	}
	for _, l := range []string{"models", "client", "server"} {
		o, _ := os.ReadFile(filepath.Join(w.sideFolder(l), "order"))
		if strings.Join(strings.Fields(string(o)), " ") != "up down" {
			t.Errorf("side %s should have run its own up and down: %q", l, o)
		}
	}
}

// report is the text of the newest dispatch's report.
func (w *world) report() string {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.bridge, ".starship", "runs", w.state().Run, "reports", lastDispatch(w).ID+".md"))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

const noticesHeading = "Notices from the stage's commands."

// noticeStage runs the world's test stage and returns the run-stage output and the report.
func noticeStage(t *testing.T, w *world) (out, report string) {
	t.Helper()
	w.toTest(w.startLead())
	out, err := w.runStage()
	if err != nil {
		t.Fatal(err)
	}
	return out, w.report()
}

func TestNoticesFromEveryBindingReachTheReport(t *testing.T) {
	n := func(s string) string { return "echo " + s + " >> $STARSHIP_NOTICES" }
	w := servicesWorld(t, n("n-test"), n("n-up"), n("n-down"))
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `command: "echo build >> order"`, `command: "`+n("n-build")+`"`, 1))
	out, report := noticeStage(t, w)
	for name, text := range map[string]string{"run-stage output": out, "report": report} {
		if !strings.Contains(text, noticesHeading) || !strings.Contains(text, "- n-up\n- n-build\n- n-test\n- n-down\n") {
			t.Errorf("%s should carry the notices of all four bindings in order: %q", name, text)
		}
		if strings.Contains(text, "- side ") {
			t.Errorf("a single-side run has no labels: %q", text)
		}
	}
	if !strings.Contains(out, "Outcome: pass") {
		t.Errorf("outcome %q", out)
	}
}

func TestNoticesAreLabelledInAMultiSideRun(t *testing.T) {
	w := sidesPlainWorld(t, "echo c-note >> $STARSHIP_NOTICES")
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(data), `touch cross`, `echo x-note >> $STARSHIP_NOTICES`, 1))
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.toTestOneReview(lead)
	out, err := w.runStage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "- side client: c-note\n- cross-location: x-note\n") {
		t.Fatalf("notices should carry their side's label, then the cross-location one: %q", out)
	}
	if !strings.Contains(w.report(), "- side client: c-note\n- cross-location: x-note\n") {
		t.Fatalf("report %q", w.report())
	}
}

func TestNoticesDoNotChangeAFailure(t *testing.T) {
	w := plainWorld(t, "echo FAILTEXT; echo n-fail >> $STARSHIP_NOTICES; exit 1")
	out, report := noticeStage(t, w)
	if !strings.Contains(out, "Outcome: fail") || lastDispatch(w).Outcome != "fail" {
		t.Fatalf("outcome %q", out)
	}
	i, j := strings.Index(report, "- n-fail\n"), strings.Index(report, "The end of the failing command's output:\n\n```\nFAILTEXT\n```\n")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("notices should come before the unchanged failure output: %q", report)
	}
	if got := w.next(); !strings.Contains(got, "has notices") {
		t.Fatalf("next should mention the notices: %s", got)
	}
}

func TestNextMentionsNoticesOfAPassingStage(t *testing.T) {
	w := plainWorld(t, "echo ok; echo n-pass >> $STARSHIP_NOTICES")
	if out, _ := noticeStage(t, w); !strings.Contains(out, "Outcome: pass") {
		t.Fatalf("outcome %q", out)
	}
	if got := w.next(); !strings.Contains(got, "finished: pass") || !strings.Contains(got, "it has notices") {
		t.Fatalf("next should mention the notices: %s", got)
	}
}

func TestNoticesDoNotChangeAnError(t *testing.T) {
	for _, cmd := range []string{"echo n-err >> $STARSHIP_NOTICES; exit 127", "echo n-err >> $STARSHIP_NOTICES; sleep 5"} {
		w := plainWorld(t, cmd)
		p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
		data, _ := os.ReadFile(p)
		write(t, p, strings.Replace(string(data), `command: "`+cmd+`", plain: true`, `command: "`+cmd+`", plain: true, timeout: 1s`, 1))
		out, report := noticeStage(t, w)
		if !strings.Contains(out, "Outcome: error") || !strings.Contains(report, "- n-err\n") {
			t.Errorf("%s: %q", cmd, out)
		}
	}
}

func TestNoNoticesLeavesTheReportAlone(t *testing.T) {
	for _, cmd := range []string{"exit 1", ": > $STARSHIP_NOTICES; exit 1"} {
		w := plainWorld(t, cmd)
		out, report := noticeStage(t, w)
		if !strings.Contains(out, "Outcome: fail") || strings.Contains(out, noticesHeading) || strings.Contains(report, noticesHeading) {
			t.Errorf("%s: a missing or empty file means no notices: %q", cmd, out)
		}
		if got := w.next(); strings.Contains(got, "has notices") {
			t.Errorf("%s: next should not mention notices: %s", cmd, got)
		}
	}
}

func TestNoticesAreCarriedWhole(t *testing.T) {
	long := strings.Repeat("x", 5000)
	w := plainWorld(t, `i=0; while [ $i -lt 60 ]; do echo line$i >> $STARSHIP_NOTICES; i=$((i+1)); done; head -c 5000 /dev/zero | tr '\\0' x >> $STARSHIP_NOTICES; echo >> $STARSHIP_NOTICES`)
	out, report := noticeStage(t, w)
	for _, text := range []string{out, report} {
		if !strings.Contains(text, "- line0\n- line1\n") || !strings.Contains(text, "- line59\n- "+long+"\n") {
			t.Fatalf("every line should be carried whole: %d bytes", len(text))
		}
	}
}

func TestNoticesKeepBlankLinesAndStripCRLF(t *testing.T) {
	w := plainWorld(t, `printf 'a\\r\\n\\nb' >> $STARSHIP_NOTICES`)
	_, report := noticeStage(t, w)
	if !strings.Contains(report, "- a\n- \n- b\n") || strings.Contains(report, "\r") {
		t.Fatalf("report %q", report)
	}
}

func TestInheritedNoticesVariableDoesNotWin(t *testing.T) {
	t.Setenv("STARSHIP_NOTICES", filepath.Join(t.TempDir(), "elsewhere"))
	w := plainWorld(t, "echo n-own >> $STARSHIP_NOTICES")
	if _, report := noticeStage(t, w); !strings.Contains(report, "- n-own\n") {
		t.Fatalf("report %q", report)
	}
}

func TestUnreadableNoticesAreOneLine(t *testing.T) {
	w := plainWorld(t, "rm -f $STARSHIP_NOTICES; mkdir $STARSHIP_NOTICES")
	out, report := noticeStage(t, w)
	if !strings.Contains(out, "Outcome: pass") || strings.Count(report, "notices could not be read") != 1 {
		t.Fatalf("a directory should be one line and no change of outcome: %q", report)
	}
}

func TestDeadRunStageKeepsItsNotices(t *testing.T) {
	w, _ := diedRunIn(t, plainWorld(t, "echo n-before >> $STARSHIP_NOTICES; kill -9 $PPID"))
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "- n-before\n") {
		t.Fatalf("%q %v", out, err)
	}
	if report := w.report(); !strings.Contains(report, noticesHeading) || !strings.Contains(report, "- n-before\n") {
		t.Fatalf("report %q", report)
	}
}

func TestLeadStopKeepsNoticesOfADeadStageOnATerminalRun(t *testing.T) {
	w, lead := diedRunIn(t, plainWorld(t, "echo n-before >> $STARSHIP_NOTICES; kill -9 $PPID"))
	if r := w.stop(lead, "orchestrator", "waiting", false); !strings.Contains(r, "The run is finished: error") || !strings.Contains(r, "Stop and report this.\n\nNotices") || !strings.Contains(r, "- n-before\n") {
		t.Fatalf("the first stop should block with the notice: %q", r)
	}
	if st := w.state(); st.Status != "error" {
		t.Fatalf("status %s", st.Status)
	}
	if r := w.stop(lead, "orchestrator", "waiting", false); r != "" {
		t.Fatalf("the second stop should pass: %s", r)
	}
	for _, d := range w.state().Dispatches {
		if d.Role == "orchestrator" && !d.Done {
			t.Fatalf("the lead should be done: %+v", d)
		}
	}
}

func TestLeadStopKeepsNoticesOfADeadStageWhenTheRunGoesOn(t *testing.T) {
	pw := plainWorld(t, "echo n-before >> $STARSHIP_NOTICES; kill -9 $PPID")
	pf := filepath.Join(pw.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(pf)
	write(t, pf, strings.Replace(string(data), "      - set:    { budgets.test: 2 }", "      - set:    { budgets.test: 2 }\n      - set:    { stage: test, outcomes.error: { goto: code, spend: test } }", 1))
	w, lead := diedRunIn(t, pw)
	left := map[string]int{}
	for _, d := range w.state().Dispatches {
		if d.StopsLeft != nil {
			left[d.ID] = *d.StopsLeft
		}
	}
	r := w.stop(lead, "orchestrator", "waiting", false)
	if !strings.Contains(r, "Dispatch stage \"code\"") || !strings.Contains(r, "- n-before\n") {
		t.Fatalf("the stop should block with the notice and the next stage: %q", r)
	}
	st := w.state()
	if st.Finished || st.Status == "error" {
		t.Fatalf("the run should go on: %s finished=%v", st.Status, st.Finished)
	}
	for _, d := range st.Dispatches {
		if d.Role == "orchestrator" && (d.Done || *d.StopsLeft != left[d.ID]) {
			t.Fatalf("the block should not spend a stop: %+v", d)
		}
	}
}

func TestRunStopPrintsNoticesOfADeadStage(t *testing.T) {
	w, _ := diedRunIn(t, servicesWorld(t, "echo n-before >> $STARSHIP_NOTICES; kill -9 $PPID", "true", "echo n-down >> $STARSHIP_NOTICES"))
	out, err := w.stopRun(w.state().Run)
	if err != nil {
		t.Fatal(err)
	}
	i, j, k := strings.Index(out, "Stopped run"), strings.Index(out, noticesHeading), strings.Index(out, "- n-before\n- n-down\n")
	if i < 0 || j < i || k < j {
		t.Fatalf("the notices, with the down's line, should follow the stopped line: %q", out)
	}
}

func TestRunStopPrintsNoNoticesSectionWithoutNotices(t *testing.T) {
	w, _ := diedRun(t)
	out, err := w.stopRun(w.state().Run)
	if err != nil || !strings.Contains(out, "Stopped run") || strings.Contains(out, noticesHeading) {
		t.Fatalf("%q %v", out, err)
	}
}

func TestLastLeadHandbackIsDeniedWithNoticesOfADeadStage(t *testing.T) {
	w, lead := diedRunIn(t, plainWorld(t, "echo n-before >> $STARSHIP_NOTICES; kill -9 $PPID"))
	for i := 0; i < 2; i++ {
		if r := w.handback(lead, "orchestrator", "All done."); !strings.Contains(r, "not finished") || strings.Contains(r, noticesHeading) {
			t.Fatalf("an early handback is refused without notices: %q", r)
		}
	}
	r := w.handback(lead, "orchestrator", "All done.")
	if !strings.Contains(r, "The run is finished: error") || !strings.Contains(r, "- n-before\n") {
		t.Fatalf("the last handback should be denied with the notices: %q", r)
	}
	w.wantFailed("lead-stopped")
	if r := w.handback(lead, "orchestrator", "All done."); r != "" {
		t.Fatalf("the next handback should be let through: %s", r)
	}
	if d := w.state().byAgent(lead); d.Handback != "All done." {
		t.Fatalf("the handback should be recorded: %+v", d)
	}
	if r := w.stop(lead, "orchestrator", "done", false); r != "" {
		t.Fatalf("the stop should pass: %s", r)
	}
	if d := w.state().byAgent(lead); !d.Done {
		t.Fatalf("the lead should be done: %+v", d)
	}
}

func TestLastLeadHandbackIsLetThroughWithoutNotices(t *testing.T) {
	w, lead := diedRun(t)
	for i := 0; i < 2; i++ {
		w.handback(lead, "orchestrator", "All done.")
	}
	if r := w.handback(lead, "orchestrator", "All done."); r != "" {
		t.Fatalf("without notices the last handback is let through: %s", r)
	}
	w.wantFailed("lead-stopped")
}
