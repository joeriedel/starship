// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	w := plainWorld(t, "echo 'FAIL: TestLogin'; exit 1")
	lead := w.startLead()
	w.toTest(lead)
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: fail") || !strings.Contains(why, "FAIL: TestLogin") || !strings.Contains(why, `stage "code"`) {
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
	write(t, f, strings.Replace(hookSettingsWith(0), "ensign:*", "ensign:*\", \"Bash(make:*)", 1))
	upgradeHookSettings(b)
	if data, _ := os.ReadFile(f); strings.Contains(string(data), "timeout") {
		t.Fatal("an edited settings file must be left alone")
	}
}
