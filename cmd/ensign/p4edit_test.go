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

func TestWithChangelist(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"flow add":         {"---\nvcs: { kind: p4, client: ws }\nx: 1\n---\nbody\n", "---\nvcs: { kind: p4, client: ws, changelist: 77 }\nx: 1\n---\nbody\n"},
		"flow replace":     {"---\nvcs: { kind: p4, changelist: '101', client: ws }\n---\n", "---\nvcs: { kind: p4, changelist: 77, client: ws }\n---\n"},
		"flow empty":       {"---\nvcs: {}\n---\n", "---\nvcs: { changelist: 77 }\n---\n"},
		"block add":        {"---\nvcs:\n  kind: p4\n  client: ws\npipeline: coding\n---\n", "---\nvcs:\n  kind: p4\n  client: ws\n  changelist: 77\npipeline: coding\n---\n"},
		"block replace":    {"---\nvcs:\n  kind: p4\n  changelist: \"101\"\n  client: ws\n---\n", "---\nvcs:\n  kind: p4\n  changelist: 77\n  client: ws\n---\n"},
		"crlf block add":   {"---\r\nvcs:\r\n  kind: p4\r\n---\r\nbody\r\n", "---\r\nvcs:\r\n  kind: p4\r\n  changelist: 77\r\n---\r\nbody\r\n"},
		"crlf flow add":    {"---\r\nvcs: { kind: p4 }\r\n---\r\n", "---\r\nvcs: { kind: p4, changelist: 77 }\r\n---\r\n"},
		"flow with before": {"---\npipeline: coding\nvcs: { kind: p4 }\n---\n", "---\npipeline: coding\nvcs: { kind: p4, changelist: 77 }\n---\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := withChangelist([]byte(c.in), "77")
			if err != nil || string(got) != c.want {
				t.Errorf("got %q, %v\nwant %q", got, err, c.want)
			}
		})
	}
	if _, err := withChangelist([]byte("---\npipeline: coding\n---\n"), "77"); err == nil {
		t.Error("no vcs: should fail")
	}
}

// p4Lead starts the world's run with the fake p4's commands logged, and returns its lead and the log.
func p4Lead(t *testing.T, w *world) (lead, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "p4.log")
	t.Setenv("STARSHIP_TEST_P4_LOG", log)
	lead, why := w.runStart()
	if lead == "" {
		t.Fatalf("run start refused: %s", why)
	}
	return
}

// p4Coder is p4Lead with a coder running under the lead.
func p4Coder(t *testing.T, w *world) (lead, coder, log string) {
	t.Helper()
	lead, log = p4Lead(t, w)
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	return
}

func p4Log(t *testing.T, log string) string {
	t.Helper()
	data, _ := os.ReadFile(log)
	return string(data)
}

func (w *world) postEdit(agent, agentType, tool, path string) map[string]any {
	w.t.Helper()
	res, code := w.hook("PostToolUse", map[string]any{"agent_id": agent, "agent_type": agentType,
		"tool_name": tool, "tool_input": map[string]any{"file_path": path}})
	if code != 0 {
		w.t.Fatalf("PostToolUse exited %d: %v", code, res)
	}
	return res
}

func TestP4TaskChangelistLifecycle(t *testing.T) {
	w := p4World(t, "{ kind: p4, client: supernova-ws }")
	log := filepath.Join(t.TempDir(), "p4.log")
	t.Setenv("STARSHIP_TEST_P4_LOG", log)
	r := &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Project: "supernova", Task: "supernova/auth-refactor", Workspace: w.workspace()}, State: &State{Run: "r1"}}
	taskMd := func() string {
		data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md"))
		return string(data)
	}
	state, err := runP4State(r)
	if err != nil || !strings.Contains(state, "changelist 4242 was created for the task") {
		t.Fatalf("first run: %q, %v", state, err)
	}
	if !strings.Contains(taskMd(), "vcs: { kind: p4, client: supernova-ws, changelist: 4242 }") {
		t.Errorf("task.md does not record it:\n%s", taskMd())
	}
	if !strings.Contains(p4Log(t, log), "change -i") {
		t.Error("no changelist was created")
	}

	os.Remove(log)
	t.Setenv("STARSHIP_TEST_P4_PENDING", "4242")
	state, err = runP4State(r)
	if err != nil || strings.Contains(state, "created") || !strings.Contains(state, "task changelist 4242 (0 opened files)") {
		t.Fatalf("second run: %q, %v", state, err)
	}
	if strings.Contains(p4Log(t, log), "change -i") {
		t.Error("the second run created another changelist")
	}

	t.Setenv("STARSHIP_TEST_P4_PENDING", "")
	t.Setenv("STARSHIP_TEST_P4_NEWCHANGE", "4300")
	state, err = runP4State(r)
	if err != nil || !strings.Contains(state, "recorded changelist 4242 is no longer pending (submitted or deleted), so changelist 4300 was created") {
		t.Fatalf("third run: %q, %v", state, err)
	}
	if !strings.Contains(taskMd(), "changelist: 4300 }") || strings.Contains(taskMd(), "4242") {
		t.Errorf("task.md not updated:\n%s", taskMd())
	}
}

func TestP4EditHook(t *testing.T) {
	const vcs = "{ kind: p4, client: supernova-ws, changelist: '101' }"
	setup := func(t *testing.T) (*world, string, string) {
		t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
		w := p4World(t, vcs)
		_, coder, log := p4Coder(t, w)
		return w, coder, log
	}
	t.Run("opens a tracked file for edit", func(t *testing.T) {
		w, coder, log := setup(t)
		t.Setenv("STARSHIP_TEST_P4_FILES", "main.go=head")
		path := filepath.Join(w.workspace(), "main.go")
		if refused, why := w.tryEdit(coder, "coder", path); refused {
			t.Fatalf("refused: %s", why)
		}
		if !strings.Contains(p4Log(t, log), "edit -c 101 "+path) {
			t.Errorf("no p4 edit:\n%s", p4Log(t, log))
		}
	})
	t.Run("already open in the task changelist", func(t *testing.T) {
		w, coder, log := setup(t)
		t.Setenv("STARSHIP_TEST_P4_FILES", "main.go=open:101")
		if refused, why := w.tryEdit(coder, "coder", filepath.Join(w.workspace(), "main.go")); refused {
			t.Fatalf("refused: %s", why)
		}
		if strings.Contains(p4Log(t, log), "edit -c") {
			t.Errorf("p4 edit ran:\n%s", p4Log(t, log))
		}
	})
	t.Run("open in another changelist", func(t *testing.T) {
		w, coder, log := setup(t)
		t.Setenv("STARSHIP_TEST_P4_FILES", "main.go=open:102")
		refused, why := w.tryEdit(coder, "coder", filepath.Join(w.workspace(), "main.go"))
		if !refused || !strings.Contains(why, "open in changelist 102") || !strings.Contains(why, "never moves files between changelists") {
			t.Fatalf("refused=%v: %s", refused, why)
		}
		if strings.Contains(p4Log(t, log), "edit -c") || strings.Contains(p4Log(t, log), "reopen") {
			t.Errorf("p4 moved or edited it:\n%s", p4Log(t, log))
		}
	})
	t.Run("p4 failure", func(t *testing.T) {
		w, coder, _ := setup(t)
		t.Setenv("STARSHIP_TEST_P4_FILES", "main.go=head")
		t.Setenv("STARSHIP_TEST_P4_OPEN_FAIL", "client is locked")
		refused, why := w.tryEdit(coder, "coder", filepath.Join(w.workspace(), "main.go"))
		if !refused || !strings.Contains(why, "p4 edit -c 101") || !strings.Contains(why, "client is locked") {
			t.Fatalf("refused=%v: %s", refused, why)
		}
	})
	t.Run("a new file is added after the write", func(t *testing.T) {
		w, coder, log := setup(t)
		path := filepath.Join(w.workspace(), "new.go")
		if refused, why := w.tryEdit(coder, "coder", path); refused {
			t.Fatalf("refused: %s", why)
		}
		if strings.Contains(p4Log(t, log), "add -c") {
			t.Fatal("added before the write")
		}
		write(t, path, "package supernova\n")
		if res := w.postEdit(coder, "coder", "Write", path); res != nil {
			t.Fatalf("post hook: %v", res)
		}
		if !strings.Contains(p4Log(t, log), "add -c 101 "+path) {
			t.Errorf("no p4 add:\n%s", p4Log(t, log))
		}
	})
	t.Run("a file in scratch is left alone", func(t *testing.T) {
		w, coder, log := setup(t)
		w.tryEdit(coder, "coder", filepath.Join(w.scratch(), "note.txt"))
		if strings.Contains(p4Log(t, log), "fstat") {
			t.Errorf("p4 was asked about scratch:\n%s", p4Log(t, log))
		}
	})
}

func TestP4EditBindings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bindings here are POSIX shell")
	}
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	marker := filepath.Join(t.TempDir(), "marker")
	for _, name := range []string{"checkout", "add", "reconcile"} {
		w.binding(name + ": { command: \"echo " + name + " $STARSHIP_CHANGELIST $STARSHIP_CLIENT $STARSHIP_FILE >> " + marker + "\" }")
	}
	_, coder, log := p4Coder(t, w)
	tracked := filepath.Join(w.workspace(), "main.go")
	t.Setenv("STARSHIP_TEST_P4_FILES", "main.go=head")
	if refused, why := w.tryEdit(coder, "coder", tracked); refused {
		t.Fatalf("refused: %s", why)
	}
	fresh := filepath.Join(w.workspace(), "new.go")
	write(t, fresh, "package supernova\n")
	w.postEdit(coder, "coder", "Write", fresh)
	got, _ := os.ReadFile(marker)
	for _, want := range []string{"checkout 101 supernova-ws " + tracked, "add 101 supernova-ws " + fresh} {
		if !strings.Contains(string(got), want) {
			t.Errorf("binding output lacks %q:\n%s", want, got)
		}
	}
	if l := p4Log(t, log); strings.Contains(l, "edit -c") || strings.Contains(l, "add -c") {
		t.Errorf("p4 ran instead of the bindings:\n%s", l)
	}
}

func TestP4EditNotApplicableBinding(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	w.binding("checkout: { not-applicable: \"the depot is checked out by a sync tool\" }")
	_, coder, log := p4Coder(t, w)
	t.Setenv("STARSHIP_TEST_P4_FILES", "main.go=head")
	if refused, why := w.tryEdit(coder, "coder", filepath.Join(w.workspace(), "main.go")); refused {
		t.Fatalf("refused: %s", why)
	}
	if strings.Contains(p4Log(t, log), "edit -c") {
		t.Errorf("p4 edit ran:\n%s", p4Log(t, log))
	}
}

func TestP4EditGitTaskUntouched(t *testing.T) {
	w := newWorld(t)
	exe, _ := os.Executable()
	old := p4Program
	p4Program = exe
	t.Cleanup(func() { p4Program = old })
	t.Setenv("STARSHIP_TEST_FAKE_CMD", "p4")
	log := filepath.Join(t.TempDir(), "p4.log")
	t.Setenv("STARSHIP_TEST_P4_LOG", log)
	lead, why := w.runStart()
	if lead == "" {
		t.Fatalf("run start refused: %s", why)
	}
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	path := filepath.Join(w.workspace(), "new.go")
	if refused, why := w.tryEdit(coder, "coder", path); refused {
		t.Fatalf("refused: %s", why)
	}
	write(t, path, "package supernova\n")
	if res := w.postEdit(coder, "coder", "Write", path); res != nil {
		t.Errorf("post hook: %v", res)
	}
	if _, err := os.Stat(log); err == nil {
		t.Errorf("p4 ran for a git task:\n%s", p4Log(t, log))
	}
}

func TestP4NoReconcileBeforeTest(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	lead, log := p4Lead(t, w)
	write(t, filepath.Join(w.workspace(), "a.go"), "package a\n")
	w.toTest(lead)
	tester, why := w.spawn(lead, "orchestrator", "tester")
	if tester == "" {
		t.Fatalf("tester refused: %s", why)
	}
	if _, err := os.Stat(filepath.Join(runsDir(w.bridge), w.state().Run, "reports", "reconcile.md")); err == nil {
		t.Error("reconcile.md was written with no @reconcile")
	}
	if strings.Contains(p4Log(t, log), "reconcile") {
		t.Errorf("p4 reconciled:\n%s", p4Log(t, log))
	}
}

func TestP4ReconcileBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bindings here are POSIX shell")
	}
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	w.binding("reconcile: { command: \"echo opened-by-binding $STARSHIP_CHANGELIST; echo second.go\" }")
	lead, log := p4Lead(t, w)
	w.toTest(lead)
	if tester, why := w.spawn(lead, "orchestrator", "tester"); tester == "" {
		t.Fatalf("tester refused: %s", why)
	}
	data, _ := os.ReadFile(filepath.Join(runsDir(w.bridge), w.state().Run, "reports", "reconcile.md"))
	if !strings.Contains(string(data), "- opened-by-binding 101") || !strings.Contains(string(data), "- second.go") {
		t.Errorf("reconcile.md:\n%s", data)
	}
	if strings.Contains(p4Log(t, log), "reconcile") {
		t.Errorf("p4 reconcile ran instead of the binding:\n%s", p4Log(t, log))
	}
}

func TestP4ReconcileFailureRefusesTheTestStage(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	w.binding("reconcile: { command: \"echo boom; exit 1\" }")
	lead, _ := p4Lead(t, w)
	w.toTest(lead)
	tester, why := w.spawn(lead, "orchestrator", "tester")
	if tester != "" || !strings.Contains(why, "could not start") {
		t.Fatalf("tester=%q why=%q", tester, why)
	}
	if st := w.state(); !strings.Contains(st.Reason, "reconcile") || !strings.Contains(st.Reason, "boom") {
		t.Errorf("reason %q", st.Reason)
	}
}

func TestP4NoReconcileBeforePlainTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the commands here are POSIX shell")
	}
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	s := strings.Replace(string(data), `  build: { command: "./build.sh" }`, `  build: { command: "echo built", plain: true }`, 1)
	write(t, p, strings.Replace(s, `  test:  { command: "./test.sh" }`, `  test:  { command: "echo 3 passed", plain: true }`, 1))
	lead, log := p4Lead(t, w)
	write(t, filepath.Join(w.workspace(), "a.go"), "package a\n")
	w.toTest(lead)
	out, err := w.runStage()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Reconcile before this stage") {
		t.Errorf("run-stage output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(runsDir(w.bridge), w.state().Run, "reports", "reconcile.md")); err == nil {
		t.Error("reconcile.md was written with no @reconcile")
	}
	if strings.Contains(p4Log(t, log), "reconcile") {
		t.Errorf("p4 reconciled:\n%s", p4Log(t, log))
	}
}
