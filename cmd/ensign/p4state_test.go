// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// p4World is the supernova fixture with a Perforce task, p4 standing in as the test binary's fake.
func p4World(t *testing.T, vcs string) *world {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := p4Program
	p4Program = exe
	t.Cleanup(func() { p4Program = old })
	t.Setenv("STARSHIP_TEST_FAKE_CMD", "p4")
	w := newWorld(t)
	task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, strings.Replace(string(data), "pipeline: coding\n", "pipeline: coding\nvcs: "+vcs+"\n", 1))
	w.enter()
	return w
}

// binding adds a binding to the project's bindings block.
func (w *world) binding(line string) {
	w.t.Helper()
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	write(w.t, p, strings.Replace(string(data), "bindings:\n", "bindings:\n  "+line+"\n", 1))
}

func (w *world) openVcsLine() string {
	w.t.Helper()
	var out bytes.Buffer
	if err := cmdTask([]string{"open", "supernova/auth-refactor"}, &out); err != nil {
		w.t.Fatal(err)
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if rest, ok := strings.CutPrefix(l, "vcs        "); ok {
			return rest
		}
	}
	w.t.Fatalf("no vcs line:\n%s", out.String())
	return ""
}

// runStart starts a run and the lead, returning why the lead was refused.
func (w *world) runStart() (string, string) {
	w.t.Helper()
	w.run("supernova/auth-refactor")
	w.approve()
	return w.spawn("", "", "orchestrator")
}

func TestP4State(t *testing.T) {
	const vcs = "{ kind: p4, client: supernova-ws, changelist: '101' }"
	t.Run("clean", func(t *testing.T) {
		w := p4World(t, "{ kind: p4, client: supernova-ws }")
		if got := w.openVcsLine(); got != "p4, client supernova-ws, no other pending changelists" {
			t.Errorf("got %q", got)
		}
		lead, why := w.runStart()
		if lead == "" {
			t.Fatalf("refused: %s", why)
		}
		if got := w.state().VcsState; got != "p4, client supernova-ws, no other pending changelists" {
			t.Errorf("run state %q", got)
		}
	})
	t.Run("task changelist and others", func(t *testing.T) {
		t.Setenv("STARSHIP_TEST_P4_PENDING", "101 102")
		t.Setenv("STARSHIP_TEST_P4_OPENED", "101 101 102 default")
		w := p4World(t, vcs)
		want := "p4, client supernova-ws, task changelist 101 (2 opened files), other changelists: 102 (1 opened files), default (1 opened files)"
		if got := w.openVcsLine(); got != want {
			t.Errorf("got %q", got)
		}
		if lead, why := w.runStart(); lead == "" {
			t.Fatalf("other changelists must not refuse: %s", why)
		}
		if got := w.state().VcsState; got != want {
			t.Errorf("run state %q", got)
		}
	})
	t.Run("task changelist missing", func(t *testing.T) {
		w := p4World(t, vcs)
		if got := w.openVcsLine(); !strings.Contains(got, "task changelist 101 is not among the client's pending changelists") {
			t.Errorf("got %q", got)
		}
		if lead, why := w.runStart(); lead == "" {
			t.Fatalf("refused: %s", why)
		}
	})
	t.Run("state override", func(t *testing.T) {
		w := p4World(t, vcs)
		w.binding("state: { command: \"echo supernova stands ready\" }")
		if got := w.openVcsLine(); got != "supernova stands ready" {
			t.Errorf("got %q", got)
		}
		if lead, why := w.runStart(); lead == "" {
			t.Fatalf("refused: %s", why)
		}
		if got := w.state().VcsState; got != "supernova stands ready" {
			t.Errorf("run state %q", got)
		}
	})
	t.Run("failing state override", func(t *testing.T) {
		w := p4World(t, vcs)
		w.binding("state: { command: \"echo boom; exit 1\" }")
		if got := w.openVcsLine(); !strings.Contains(got, "@state failed") || !strings.Contains(got, "boom") {
			t.Errorf("got %q", got)
		}
	})
	problems := map[string]struct{ vcs, env, val, want string }{
		"missing client": {"{ kind: p4 }", "", "", "p4: the task's vcs: has no client"},
		"p4 missing":     {"{ kind: p4, client: supernova-ws }", "", "", "p4: not found on PATH"},
		"not logged in":  {"{ kind: p4, client: supernova-ws }", "STARSHIP_TEST_P4_LOGIN", "fail", "run p4 login (p4 login -s: Perforce password (P4PASSWD) invalid or unset.)"},
		"unknown client": {"{ kind: p4, client: supernova-ws }", "STARSHIP_TEST_P4_CLIENT", "unknown", "p4: client supernova-ws is unknown to the server"},
	}
	for name, c := range problems {
		t.Run(name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv(c.env, c.val)
			}
			w := p4World(t, c.vcs)
			if name == "p4 missing" {
				p4Program = filepath.Join(t.TempDir(), "no-such-p4")
			}
			if got := w.openVcsLine(); !strings.Contains(got, c.want) {
				t.Errorf("task open: got %q, want %q", got, c.want)
			}
			lead, why := w.runStart()
			if lead != "" || !strings.Contains(why, "could not start") {
				t.Fatalf("run start was not refused: %q %q", lead, why)
			}
			if st := w.state(); st.Status != "error" || !strings.Contains(st.Reason, c.want) {
				t.Errorf("status %s reason %s", st.Status, st.Reason)
			}
		})
	}
}

func TestP4StateProjectEnv(t *testing.T) {
	const vcs = "{ kind: p4, client: supernova-ws }"
	const clean = "p4, client supernova-ws, no other pending changelists"
	t.Run("set variable", func(t *testing.T) {
		t.Setenv("STARSHIP_TEST_P4_NEED", "SUPERNOVA_P4PORT")
		t.Setenv("SUPERNOVA_P4PORT", "")
		w := p4World(t, vcs)
		if got := w.openVcsLine(); !strings.Contains(got, "SUPERNOVA_P4PORT is not set") {
			t.Fatalf("without the env: %q", got)
		}
		m := loadMachine(w.bridge)
		m.Env = map[string]ProjectEnv{"supernova": {Set: map[string]string{"SUPERNOVA_P4PORT": "ssl:supernova:1666"}}}
		if err := saveMachine(w.bridge, m); err != nil {
			t.Fatal(err)
		}
		if got := w.openVcsLine(); got != clean {
			t.Errorf("task open: %q", got)
		}
		if lead, why := w.runStart(); lead == "" {
			t.Fatalf("run start refused: %s", why)
		}
		if got := w.state().VcsState; got != clean {
			t.Errorf("run state %q", got)
		}
	})
	t.Run("path folder", func(t *testing.T) {
		if hostOS == "windows" {
			t.Skip("the fake p4 is not an .exe copy here")
		}
		w := p4World(t, vcs)
		exe, _ := os.Executable()
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		write(t, filepath.Join(dir, "p4"), string(data))
		if err := os.Chmod(filepath.Join(dir, "p4"), 0o755); err != nil {
			t.Fatal(err)
		}
		p4Program = "p4"
		t.Setenv("PATH", t.TempDir())
		if got := w.openVcsLine(); got != "p4: not found on PATH" {
			t.Fatalf("without the env: %q", got)
		}
		m := loadMachine(w.bridge)
		m.Env = map[string]ProjectEnv{"supernova": {Path: []string{dir}}}
		if err := saveMachine(w.bridge, m); err != nil {
			t.Fatal(err)
		}
		if got := w.openVcsLine(); got != clean {
			t.Errorf("task open: %q", got)
		}
	})
	t.Run("task state binding", func(t *testing.T) {
		w := p4World(t, vcs)
		task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
		data, _ := os.ReadFile(task)
		write(t, task, strings.Replace(string(data), "pipeline: coding\n", "pipeline: coding\nbindings:\n  state: { command: \"echo supernova task state\" }\n", 1))
		if got := w.openVcsLine(); got != "supernova task state" {
			t.Errorf("task open: %q", got)
		}
		if lead, why := w.runStart(); lead == "" {
			t.Fatalf("refused: %s", why)
		}
		if got := w.state().VcsState; got != "supernova task state" {
			t.Errorf("run state %q", got)
		}
	})
}

func TestRunP4StateNoTask(t *testing.T) {
	r := &RunFiles{Snap: &Snapshot{Bridge: t.TempDir()}}
	got, err := runP4State(r)
	if got != "" || err != nil {
		t.Errorf("runP4State with no task = %q, %v", got, err)
	}
}
