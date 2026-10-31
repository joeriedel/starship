// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestPlainTestRunsWithoutAnAgent(t *testing.T) {
	w := plainWorld(t, "echo 3 passed")
	lead := w.startLead()
	w.toTest(lead)
	id, why := w.spawn(lead, "orchestrator", "tester")
	if id != "" {
		t.Fatal("a tester agent was started for a plain @test")
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

func TestPlainTestFailureGoesBackToCode(t *testing.T) {
	w := plainWorld(t, "echo 'FAIL: TestLogin'; echo 'login_test.go:9: rejected'; exit 1")
	lead := w.startLead()
	w.toTest(lead)
	_, why := w.spawn(lead, "orchestrator", "tester")
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
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: error") {
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
	if _, why := w.spawn(lead, "orchestrator", "tester"); !strings.Contains(why, "Outcome: error") {
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
	write(t, f, hookSettingsWith(0))
	if got := preToolUseTimeout(b); got.Seconds() != 600 {
		t.Fatalf("without a timeout the hook gets Claude Code's default, got %s", got)
	}
	upgradeHookSettings(b)
	if got := preToolUseTimeout(b); got.Seconds() != hookTimeout {
		t.Fatalf("the upgrade should set the timeout, got %s", got)
	}
	write(t, f, beforeHandback(hookSettings()))
	upgradeHookSettings(b)
	if data, _ := os.ReadFile(f); string(data) != hookSettings() {
		t.Fatal("settings from before SubagentHandback should be upgraded")
	}
	write(t, f, strings.Replace(hookSettingsWith(0), "ensign:*", "ensign:*\", \"Bash(make:*)", 1))
	upgradeHookSettings(b)
	if data, _ := os.ReadFile(f); strings.Contains(string(data), "timeout") {
		t.Fatal("an edited settings file must be left alone")
	}
}

// gatedPlainRun starts a plain test stage whose command blocks until the returned release
// is called, and waits until it is running. wait returns the hook's decision.
func gatedPlainRun(t *testing.T) (w *world, run string, release func(), wait func() map[string]any) {
	t.Helper()
	dir := t.TempDir()
	started, gate := filepath.Join(dir, "started"), filepath.Join(dir, "gate")
	w = plainWorld(t, "touch "+started+"; i=0; while [ ! -f "+gate+" ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done; echo gated")
	lead := w.startLead()
	w.toTest(lead)
	run = w.state().Run
	done := make(chan map[string]any, 1)
	go func() {
		res, _ := w.hook("PreToolUse", map[string]any{
			"tool_name":  "Agent",
			"tool_input": map[string]any{"subagent_type": "tester", "prompt": "x", "description": "x"},
			"agent_id":   lead, "agent_type": "orchestrator",
		})
		done <- res
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
	wait = func() map[string]any {
		select {
		case res := <-done:
			return res
		case <-time.After(20 * time.Second):
			t.Fatal("the hook did not finish")
			return nil
		}
	}
	return
}

// lockRun opens the run from outside the hook, failing if its lock is still held.
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
	if !strings.Contains(dig(res, "hookSpecificOutput", "permissionDecisionReason"), "Outcome: pass") {
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
