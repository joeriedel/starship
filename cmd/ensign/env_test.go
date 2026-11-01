// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runEnv(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := cmdEnv(args, &out)
	return out.String(), err
}

func TestMachineEnvRoundTrip(t *testing.T) {
	w := newWorld(t)
	m := loadMachine(w.bridge)
	m.Env = map[string]ProjectEnv{
		"supernova": {Set: map[string]string{"GOROOT": "/home/me/sdk/go", "NOTE": "a: b"}, Path: []string{"/home/me/sdk/go/bin", "/opt/tools"}},
		"empty":     {},
	}
	if err := saveMachine(w.bridge, m); err != nil {
		t.Fatal(err)
	}
	got := loadMachine(w.bridge)
	if !reflect.DeepEqual(got.Env, map[string]ProjectEnv{"supernova": m.Env["supernova"]}) {
		t.Fatalf("env after a round trip: %+v", got.Env)
	}
	if got.Workspaces["supernova"] == "" {
		t.Errorf("workspaces lost: %+v", got.Workspaces)
	}
	// A file without the section loads, and saving it writes none.
	m.Env = nil
	saveMachine(w.bridge, m)
	data, _ := os.ReadFile(machineFile(w.bridge))
	if strings.Contains(string(data), "env:") || loadMachine(w.bridge).Env != nil {
		t.Errorf("no env expected:\n%s", data)
	}
}

func TestEnvCommand(t *testing.T) {
	w := newWorld(t)
	tools := t.TempDir()
	if _, err := runEnv(t, "supernova", "GOROOT=/sdk/go"); err != nil {
		t.Fatal(err)
	}
	if _, err := runEnv(t, "supernova", "--path", tools); err != nil {
		t.Fatal(err)
	}
	out, _ := runEnv(t, "supernova")
	if out != "GOROOT=/sdk/go\npath "+filepath.ToSlash(tools)+"\n" {
		t.Fatalf("shown: %q", out)
	}
	if _, err := runEnv(t, "supernova", "--path", tools); err == nil {
		t.Errorf("a folder already on the path should be refused")
	}
	if _, err := runEnv(t, "supernova", "--drop-path", tools); err != nil {
		t.Fatal(err)
	}
	if _, err := runEnv(t, "supernova", "--drop-path", tools); err == nil {
		t.Errorf("dropping a folder that isn't there should be refused")
	}
	if _, err := runEnv(t, "supernova", "GOROOT="); err != nil {
		t.Fatal(err)
	}
	if e := loadMachine(w.bridge).Env; len(e) != 0 {
		t.Errorf("an emptied project should leave no entry: %+v", e)
	}
	for _, args := range [][]string{
		{"supernova", "1BAD=x"},
		{"supernova", "NOEQUALS"},
		{"supernova", "PATH=/x"},
		{"supernova", "Path=/x"},
		{"supernova", "--path", filepath.Join(tools, "missing")},
		{"nova", "A=b"},
		{"supernova", "--bogus", "x"},
		{},
	} {
		if _, err := runEnv(t, args...); err == nil {
			t.Errorf("%v should be refused", args)
		}
	}
}

func TestApplyEnv(t *testing.T) {
	e := ProjectEnv{Set: map[string]string{"GOROOT": "/sdk"}, Path: []string{"/sdk/bin", "/opt"}}
	got := applyEnv([]string{"A=1", "GOROOT=/old", "PATH=/usr/bin"}, e, false, ":")
	linuxPath := filepath.FromSlash("/sdk/bin") + ":" + filepath.FromSlash("/opt")
	want := []string{"A=1", "PATH=" + linuxPath + ":/usr/bin", "GOROOT=/sdk"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := applyEnv([]string{"A=1"}, e, false, ":"); got[len(got)-1] != "PATH="+linuxPath {
		t.Errorf("no PATH to extend: %v", got)
	}
	// Windows: names match in any case, and Path keeps its spelling.
	got = applyEnv([]string{"Path=C:\\Windows", "goroot=C:\\old"}, ProjectEnv{Set: e.Set, Path: []string{"C:/sdk/bin"}}, true, ";")
	want = []string{"Path=" + filepath.FromSlash("C:/sdk/bin") + ";C:\\Windows", "GOROOT=/sdk"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("windows: got %v, want %v", got, want)
	}
	// On Linux names are case-sensitive.
	if got := applyEnv([]string{"goroot=x"}, ProjectEnv{Set: e.Set}, false, ":"); len(got) != 2 {
		t.Errorf("linux keeps goroot: %v", got)
	}
}

func TestProjectEnvReadsMachineYAMLLive(t *testing.T) {
	w := newWorld(t)
	if got := projectEnv(w.bridge, "supernova", []string{"A=1"}); len(got) != 1 {
		t.Fatalf("no env recorded: %v", got)
	}
	runEnv(t, "supernova", "TOOL_HOME=/x")
	got := projectEnv(w.bridge, "supernova", []string{"A=1"})
	if got[len(got)-1] != "TOOL_HOME=/x" {
		t.Errorf("recorded variable missing: %v", got)
	}
}

func TestBindingRunSeesProjectEnv(t *testing.T) {
	w := newWorld(t)
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	r := &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Project: "supernova", Workspace: t.TempDir()}, State: &State{Run: "r1"}}
	out, err := runBinding(r, nil, `echo "tool:$SUPERNOVA_TOOL"`, time.Now().Add(time.Minute))
	if err != nil || strings.TrimSpace(out) != "tool:hello" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestPosixPath(t *testing.T) {
	for _, c := range []struct {
		in      string
		windows bool
		want    string
	}{
		{"C:/Users/me/bin", true, "/c/Users/me/bin"},
		{"/home/me", true, "/home/me"},
		{"C:/Users/me/bin", false, "C:/Users/me/bin"},
	} {
		if got := posixPath(c.in, c.windows); got != c.want {
			t.Errorf("posixPath(%q, %v) = %q, want %q", c.in, c.windows, got, c.want)
		}
	}
	e := ProjectEnv{Set: map[string]string{"A": "it's"}, Path: []string{"C:/sdk/bin", "C:/opt"}}
	want := "export A='it'\\''s'\nexport PATH='/c/sdk/bin:/c/opt':\"$PATH\"\n"
	if got := setupScript(e, true); got != want {
		t.Errorf("setup script %q, want %q", got, want)
	}
}

func TestCreateRunWritesTheSetupFile(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	tools := t.TempDir()
	runEnv(t, "supernova", "--path", tools)
	w.run("supernova/auth-refactor")
	data, err := os.ReadFile(setupFile(w.bridge, "s1"))
	want := "export SUPERNOVA_TOOL='hello'\nexport PATH=" + singleQuote(posixPath(filepath.ToSlash(tools), runtime.GOOS == "windows")) + ":\"$PATH\"\n"
	if err != nil || string(data) != want {
		t.Fatalf("setup file %q (%v), want %q", data, err, want)
	}
	runEnv(t, "supernova", "SUPERNOVA_TOOL=")
	runEnv(t, "supernova", "--drop-path", tools)
	w.run("supernova/auth-refactor")
	if _, err := os.Stat(setupFile(w.bridge, "s1")); !os.IsNotExist(err) {
		t.Errorf("a project with no environment should leave no setup file: %v", err)
	}
}

func TestSessionStartWithoutGitBashWritesAssignments(t *testing.T) {
	w := newWorld(t)
	hostOS, gitBashFound = "windows", func() bool { return false }
	t.Cleanup(func() { hostOS, gitBashFound = runtime.GOOS, detectGitBash })
	m := loadMachine(w.bridge)
	m.Env = map[string]ProjectEnv{
		"supernova": {Set: map[string]string{"SHARED": "1", "ONLY_A": "a b"}, Path: []string{"C:/sdk/bin"}},
		"quasar":    {Set: map[string]string{"SHARED": "2"}, Path: []string{"C:/q/bin"}},
	}
	saveMachine(w.bridge, m)
	t.Setenv("PATH", "C:\\Windows")
	env := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("CLAUDE_ENV_FILE", env)
	in, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "abc"})
	var out, errb bytes.Buffer
	if code := cmdHook("SessionStart", bytes.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	data, _ := os.ReadFile(env)
	bin := filepath.FromSlash(filepath.ToSlash(filepath.Join(w.bridge, ".starship", "bin")))
	want := "STARSHIP_SESSION=abc\nPATH=" + bin + ";" + filepath.FromSlash("C:/q/bin") + ";" + filepath.FromSlash("C:/sdk/bin") + ";C:\\Windows\nONLY_A=a b\n"
	if string(data) != want {
		t.Errorf("env file %q, want %q", data, want)
	}
	_, ctx := sessionStartReply(t, out.Bytes())
	if !strings.Contains(ctx, "SHARED (set differently by quasar and supernova)") {
		t.Errorf("the conflict should be named:\n%s", ctx)
	}
	// Nothing is written for the run in this mode.
	w.run("supernova/auth-refactor")
	if _, err := os.Stat(setupFile(w.bridge, "s1")); !os.IsNotExist(err) {
		t.Errorf("no setup file expected: %v", err)
	}
}

func TestRestartedSessionGetsTheSetupFile(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	w.run("supernova/auth-refactor")
	write(t, w.session, `{"sessionId":"s1"}`+"\n")
	carryOverSession(w.bridge, &HookInput{Source: "resume", SessionID: "s2", TranscriptPath: w.session})
	data, err := os.ReadFile(setupFile(w.bridge, "s2"))
	if err != nil || string(data) != "export SUPERNOVA_TOOL='hello'\n" {
		t.Fatalf("setup file %q (%v)", data, err)
	}
}
