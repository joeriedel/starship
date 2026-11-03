// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	out, err := runBinding(r, nil, r.Snap.Workspace, `echo "tool:$SUPERNOVA_TOOL"`, time.Now().Add(time.Minute))
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

// blockSetupFile puts a non-empty directory where the session's setup file goes, so writing and removing it both fail on every platform.
func blockSetupFile(t *testing.T, bridge, session string) string {
	t.Helper()
	path := setupFile(bridge, session)
	write(t, filepath.Join(path, "keep"), "x")
	return path
}

func TestBindSessionReportsSetupFileFailure(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	w.run("supernova/auth-refactor")
	ids, _ := sessionRuns(w.bridge, "s1")
	if len(ids) != 1 {
		t.Fatalf("runs %v", ids)
	}
	path := blockSetupFile(t, w.bridge, "s3")
	err := bindSession(w.bridge, "s3", ids[0])
	if !errors.Is(err, errSetupFile) || !strings.Contains(err.Error(), path) {
		t.Fatalf("write failure: %v", err)
	}
	if got, _ := sessionRuns(w.bridge, "s3"); !reflect.DeepEqual(got, ids) {
		t.Errorf("the run should still be bound: %v", got)
	}

	// With no environment the file is removed, and that failure is reported too.
	runEnv(t, "supernova", "SUPERNOVA_TOOL=")
	err = bindSession(w.bridge, "s3", ids[0])
	if !errors.Is(err, errSetupFile) || !strings.Contains(err.Error(), path) {
		t.Fatalf("remove failure: %v", err)
	}

	// A file that is already missing is not a failure.
	if err := bindSession(w.bridge, "s4", ids[0]); err != nil {
		t.Errorf("missing file on remove: %v", err)
	}
}

func TestCarryOverKeepsTheRunWhenTheSetupFileFails(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	w.run("supernova/auth-refactor")
	write(t, w.session, `{"sessionId":"s1"}`+"\n")
	blockSetupFile(t, w.bridge, "s2")
	err := carryOverSession(w.bridge, &HookInput{Source: "resume", SessionID: "s2", TranscriptPath: w.session})
	if !errors.Is(err, errSetupFile) {
		t.Fatalf("carry-over error: %v", err)
	}
	if ids, _ := sessionRuns(w.bridge, "s2"); len(ids) != 1 {
		t.Errorf("the run should be carried over: %v", ids)
	}
}

func TestCreateRunReportsSetupFileFailureAsWarning(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	w.run("supernova/auth-refactor")
	first, _ := sessionRuns(w.bridge, w.sid)
	if len(first) != 1 {
		t.Fatalf("runs %v", first)
	}
	path := blockSetupFile(t, w.bridge, w.sid)
	out := w.run("supernova/auth-refactor")
	if !strings.Contains(out, "created") || !strings.Contains(out, "waiting for go") || !strings.Contains(out, "replaced waiting run "+first[0]) {
		t.Errorf("created message missing: %s", out)
	}
	if !strings.Contains(out, "starship: ") || !strings.Contains(out, path) {
		t.Errorf("warning missing: %s", out)
	}
	if _, err := os.Stat(filepath.Join(runsDir(w.bridge), first[0])); !os.IsNotExist(err) {
		t.Errorf("the replaced run should be removed: %v", err)
	}
}

func TestStartRunReportsSetupFileFailureAsWarning(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	snap, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor"}, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := blockSetupFile(t, w.bridge, "s7")
	text, err := startRun(w.bridge, "s7", snap.Task, snap.Run)
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if !strings.Contains(text, "approved") || !strings.Contains(text, "starship: ") || !strings.Contains(text, path) {
		t.Errorf("text %q", text)
	}
	if st := w.runState(snap.Run); st.Status != "running" || st.Session != "s7" {
		t.Errorf("run is %s in session %q", st.Status, st.Session)
	}
}

func TestClaimRunSavesTheSessionWhenTheSetupFileFails(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	t.Setenv("STARSHIP_SESSION", "")
	w.run("supernova/auth-refactor")
	blockSetupFile(t, w.bridge, "s8")
	r, err := claimRun(w.bridge, "s8")
	if err != nil || r == nil || !errors.Is(r.warn, errSetupFile) {
		t.Fatalf("claimRun: %v, %v", r, err)
	}
	out, err := hookUserPromptSubmit(&HookInput{SessionID: "s8"}, r)
	if err != nil {
		t.Fatal(err)
	}
	ctx := out.(map[string]any)["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if !strings.Contains(ctx, "approved") || !strings.Contains(ctx, "starship: "+r.warn.Error()) {
		t.Errorf("the approval should carry the warning: %q", ctx)
	}
	r.close()
	ids, _ := sessionRuns(w.bridge, "s8")
	if len(ids) != 1 {
		t.Fatalf("runs %v", ids)
	}
	if st := w.runState(ids[0]); st.Status != "running" || st.Session != "s8" {
		t.Errorf("the claim should be saved: session %q", st.Session)
	}
}

func TestGitBashPath(t *testing.T) {
	d := t.TempDir()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	custom := filepath.Join(d, "custom-bash")
	write(t, custom, "x")
	t.Setenv("CLAUDE_CODE_GIT_BASH_PATH", custom)
	t.Setenv("PATH", t.TempDir())
	if got := gitBashPath(); got != custom {
		t.Errorf("env var: got %q, want %q", got, custom)
	}
	t.Setenv("CLAUDE_CODE_GIT_BASH_PATH", filepath.Join(d, "missing"))
	if got := gitBashPath(); got != "" {
		t.Errorf("missing env path with no git: got %q", got)
	}

	t.Setenv("CLAUDE_CODE_GIT_BASH_PATH", "")
	if got := gitBashPath(); got != "" {
		t.Errorf("no git: got %q", got)
	}
	bash := filepath.Join(d, "bin", "bash.exe")
	write(t, bash, "x")
	if err := os.MkdirAll(filepath.Join(d, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "cmd", "git"+exe), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(d, "cmd"))
	if got := gitBashPath(); got != bash {
		t.Errorf("git on PATH: got %q, want %q", got, bash)
	}
}
