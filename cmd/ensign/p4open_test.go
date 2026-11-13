// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const p4OpenVcs = "{ kind: p4, client: supernova-ws, changelist: '101' }"

// p4Open runs `ensign p4 open` for the world's run.
func (w *world) p4Open(paths ...string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	err := cmdP4(append([]string{"open", "--bridge", w.bridge, "--run", w.state().Run}, paths...), &out)
	return out.String(), err
}

func TestP4OpenEditsATrackedFile(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "a.go=head")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	file := filepath.Join(w.workspace(), "a.go")
	write(t, file, "package a\n")
	out, err := w.p4Open(file)
	if err != nil || !strings.Contains(out, "opened for edit") {
		t.Fatalf("%q, %v", out, err)
	}
	if !strings.Contains(p4Log(t, log), "edit -c 101 ") {
		t.Errorf("no edit:\n%s", p4Log(t, log))
	}
}

func TestP4OpenAddsANewFile(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	write(t, filepath.Join(w.workspace(), "new.go"), "package n\n")
	out, err := w.p4Open(filepath.Join(w.workspace(), "new.go"))
	if err != nil || !strings.Contains(out, "added") {
		t.Fatalf("%q, %v", out, err)
	}
	if !strings.Contains(p4Log(t, log), "add -c 101 ") {
		t.Errorf("no add:\n%s", p4Log(t, log))
	}
}

func TestP4OpenDeletesATrackedFileMissingOnDisk(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "gone.go=head")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	out, err := w.p4Open(filepath.Join(w.workspace(), "gone.go"))
	if err != nil || !strings.Contains(out, "opened for delete") {
		t.Fatalf("%q, %v", out, err)
	}
	if !strings.Contains(p4Log(t, log), "delete -c 101 ") {
		t.Errorf("no delete:\n%s", p4Log(t, log))
	}
}

func TestP4OpenLeavesAFileOpenInTheChangelistAlone(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "a.go=open:101")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	write(t, filepath.Join(w.workspace(), "a.go"), "package a\n")
	out, err := w.p4Open(filepath.Join(w.workspace(), "a.go"))
	if err != nil || !strings.Contains(out, "already open in changelist 101") {
		t.Fatalf("%q, %v", out, err)
	}
	if strings.Contains(p4Log(t, log), "edit") {
		t.Errorf("p4 edit ran:\n%s", p4Log(t, log))
	}
}

func TestP4OpenRefusesAFileOpenInAnotherChangelist(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "a.go=open:102 b.go=head")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	write(t, filepath.Join(w.workspace(), "a.go"), "package a\n")
	write(t, filepath.Join(w.workspace(), "b.go"), "package b\n")
	out, err := w.p4Open(filepath.Join(w.workspace(), "a.go"), filepath.Join(w.workspace(), "b.go"))
	if err == nil || !strings.Contains(out, "changelist 102") || !strings.Contains(out, "refused") {
		t.Fatalf("%q, %v", out, err)
	}
	if !strings.Contains(out, "opened for edit") {
		t.Errorf("the other path was not processed:\n%s", out)
	}
	if strings.Contains(p4Log(t, log), "reopen") || strings.Contains(p4Log(t, log), "edit -c 101 "+filepath.Join(w.workspace(), "a.go")) {
		t.Errorf("the file was moved or edited:\n%s", p4Log(t, log))
	}
}

func TestP4OpenRefusesPathsOutsideTheWorkspace(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "a.go=head")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	outside := filepath.Join(t.TempDir(), "a.go")
	write(t, outside, "package a\n")
	write(t, filepath.Join(w.scratch(), "a.go"), "package a\n")
	for _, p := range []string{outside, filepath.Join(w.scratch(), "a.go")} {
		if out, err := w.p4Open(p); err == nil || !strings.Contains(out, "refused") {
			t.Errorf("%s: %q, %v", p, out, err)
		}
	}
	if strings.Contains(p4Log(t, log), "edit") || strings.Contains(p4Log(t, log), "fstat") {
		t.Errorf("p4 was asked about a file outside the workspace:\n%s", p4Log(t, log))
	}
}

func TestP4OpenFindsTheBridgeFromTheBinaryOutsideIt(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "x.go=head")
	w := p4World(t, p4OpenVcs)
	_, log := p4Lead(t, w)
	write(t, filepath.Join(w.workspace(), "gen", "x.go"), "package gen\n")
	bin := filepath.Join(w.bridge, ".starship", "bin")
	write(t, filepath.Join(bin, "ensign"), "")
	old := p4Executable
	p4Executable = func() (string, error) { return filepath.Join(bin, "ensign"), nil }
	defer func() { p4Executable = old }()
	t.Chdir(w.workspace())
	var out bytes.Buffer
	err := cmdP4([]string{"open", "--run", w.state().Run, filepath.Join("gen", "x.go")}, &out)
	if err != nil || !strings.Contains(out.String(), "opened for edit") {
		t.Fatalf("%q, %v", out.String(), err)
	}
	if !strings.Contains(p4Log(t, log), "edit -c 101 "+filepath.Join(w.workspace(), "gen", "x.go")) {
		t.Errorf("no edit:\n%s", p4Log(t, log))
	}
}

func TestP4OpenWithoutABridgeSaysToPassIt(t *testing.T) {
	w := p4World(t, p4OpenVcs)
	old := p4Executable
	p4Executable = func() (string, error) { return filepath.Join(t.TempDir(), "ensign"), nil }
	defer func() { p4Executable = old }()
	t.Chdir(w.workspace())
	var out bytes.Buffer
	if err := cmdP4([]string{"open", "a.go"}, &out); err == nil || !strings.Contains(err.Error(), "--bridge") {
		t.Fatalf("%v", err)
	}
}

func TestP4OpenRefusesAMissingUntrackedFile(t *testing.T) {
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	w := p4World(t, p4OpenVcs)
	p4Lead(t, w)
	if out, err := w.p4Open(filepath.Join(w.workspace(), "nothing.go")); err == nil || !strings.Contains(out, "no such file") {
		t.Errorf("%q, %v", out, err)
	}
}

func TestP4OpenRefusesOutsideAP4Run(t *testing.T) {
	w := newWorld(t)
	w.startLead()
	if out, err := w.p4Open(filepath.Join(w.workspace(), "a.go")); err == nil || !strings.Contains(err.Error(), "only for Perforce tasks") {
		t.Errorf("git task: %q, %v", out, err)
	}
	var out bytes.Buffer
	if err := cmdP4([]string{"open", "--bridge", w.bridge, "--run", "no-such-run", "a.go"}, &out); err == nil || !strings.Contains(err.Error(), "no active run") {
		t.Errorf("no run: %q, %v", out.String(), err)
	}
	if err := cmdP4([]string{"open"}, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no paths: %v", err)
	}
}

func TestP4OpenUsesTheBindings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bindings here are POSIX shell")
	}
	t.Setenv("STARSHIP_TEST_P4_PENDING", "101")
	t.Setenv("STARSHIP_TEST_P4_FILES", "gone.go=head")
	w := p4World(t, p4OpenVcs)
	w.binding("delete: { command: \"echo deleted $STARSHIP_CHANGELIST > \\\"$STARSHIP_FILE.mark\\\"\" }")
	_, log := p4Lead(t, w)
	file := filepath.Join(w.workspace(), "gone.go")
	out, err := w.p4Open(file)
	if err != nil || !strings.Contains(out, "opened for delete") {
		t.Fatalf("%q, %v", out, err)
	}
	if data, _ := os.ReadFile(file + ".mark"); strings.TrimSpace(string(data)) != "deleted 101" {
		t.Errorf("the @delete binding did not run: %q", data)
	}
	if strings.Contains(p4Log(t, log), "delete -c") {
		t.Errorf("p4 delete ran instead of the binding:\n%s", p4Log(t, log))
	}
}

func TestOldStateWithAReconcileMarkStillLoads(t *testing.T) {
	w := newWorld(t)
	w.startLead()
	file := filepath.Join(runsDir(w.bridge), w.state().Run, "state.json")
	data, _ := os.ReadFile(file)
	write(t, file, strings.Replace(string(data), "{", "{\n  \"reconcile_mark\": 1700000000000000000,", 1))
	r, err := openRun(w.bridge, w.state().Run)
	if err != nil || r == nil {
		t.Fatalf("%v", err)
	}
	r.close()
}
