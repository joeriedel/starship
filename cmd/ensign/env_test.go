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
	carryOverSession(w.bridge, &HookInput{Source: "resume", SessionID: "s2", TranscriptPath: w.session}, "")
	data, err := os.ReadFile(setupFile(w.bridge, "s2"))
	if err != nil || string(data) != "export SUPERNOVA_TOOL='hello'\n" {
		t.Fatalf("setup file %q (%v)", data, err)
	}
}

// blockSetupFile replaces any existing setup file with a non-empty directory, so writing and removing it both fail on every platform.
func blockSetupFile(t *testing.T, bridge, session string) string {
	t.Helper()
	path := setupFile(bridge, session)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
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
	err := carryOverSession(w.bridge, &HookInput{Source: "resume", SessionID: "s2", TranscriptPath: w.session}, "")
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

// agentOnNewSession starts a run with a live lead on s1 and moves the session to s2, which has no live runs, so the lead's next hook rebinds the run. Edits are allowed only in the returned folder.
func agentOnNewSession(t *testing.T) (w *world, lead, run, allowed string) {
	t.Helper()
	w = newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	lead = w.startLead()
	run = w.state().Run
	w.sid = "s2"
	allowed = t.TempDir()
	old := editTempFolders
	editTempFolders = func() []string { return []string{allowed} }
	t.Cleanup(func() { editTempFolders = old })
	return
}

func editHook(w *world, lead, target string) map[string]any {
	w.t.Helper()
	res, code := w.hook("PreToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator",
		"tool_name": "Edit", "tool_input": map[string]any{"file_path": target}})
	if code != 0 {
		w.t.Fatalf("PreToolUse exited %d: %v", code, res)
	}
	return res
}

func countRebound(st *State) int {
	n := 0
	for _, h := range st.History {
		if h.Kind == "rebound" {
			n++
		}
	}
	return n
}

func TestEditGuardSavesTheRebind(t *testing.T) {
	w, lead, run, allowed := agentOnNewSession(t)
	target := filepath.Join(allowed, "x.txt")
	for i := 0; i < 2; i++ {
		if res := editHook(w, lead, target); res != nil {
			t.Fatalf("edit %d: %v", i, res)
		}
		st := w.runState(run)
		if st.Session != "s2" || countRebound(st) != 1 {
			t.Fatalf("edit %d: session %q, rebound entries %d; want s2 and one", i, st.Session, countRebound(st))
		}
	}
}

func TestEditGuardWritesNothingWithoutARebind(t *testing.T) {
	for _, refused := range []bool{false, true} {
		w, lead, run, allowed := agentOnNewSession(t)
		if res := editHook(w, lead, filepath.Join(allowed, "x.txt")); res != nil {
			t.Fatalf("first edit: %v", res)
		}
		statePath := filepath.Join(runsDir(w.bridge), run, "state.json")
		before, err := os.Stat(statePath)
		if err != nil {
			t.Fatal(err)
		}
		beforeBytes, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(allowed, "y.txt")
		if refused {
			target = filepath.Join(t.TempDir(), "y.txt")
		}
		res := editHook(w, lead, target)
		if got := dig(res, "hookSpecificOutput", "permissionDecision") == "deny"; got != refused {
			t.Fatalf("refused %v: %v", refused, res)
		}
		after, err := os.Stat(statePath)
		if err != nil {
			t.Fatal(err)
		}
		afterBytes, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) || !bytes.Equal(beforeBytes, afterBytes) {
			t.Errorf("refused %v: the edit rewrote state.json", refused)
		}
	}
}

func TestEditGuardWarnsWhenTheRebindSaveFails(t *testing.T) {
	for _, refused := range []bool{false, true} {
		w, lead, run, allowed := agentOnNewSession(t)
		if err := os.Mkdir(filepath.Join(runsDir(w.bridge), run, "state.json.tmp"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(allowed, "x.txt")
		if refused {
			target = filepath.Join(t.TempDir(), "x.txt")
		}
		res := editHook(w, lead, target)
		if msg := dig(res, "systemMessage"); !strings.HasPrefix(msg, "starship: ") || !strings.Contains(msg, "could not be saved") {
			t.Fatalf("refused %v: the reply should carry the warning: %v", refused, res)
		}
		if got := dig(res, "hookSpecificOutput", "permissionDecision") == "deny"; got != refused {
			t.Errorf("refused %v: %v", refused, res)
		}
	}
}

func TestEditGuardCarriesBothWarnings(t *testing.T) {
	w, lead, run, allowed := agentOnNewSession(t)
	path := blockSetupFile(t, w.bridge, "s2")
	if err := os.Mkdir(filepath.Join(runsDir(w.bridge), run, "state.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := editHook(w, lead, filepath.Join(allowed, "x.txt"))
	msg := dig(res, "systemMessage")
	if !strings.Contains(msg, path) || !strings.Contains(msg, "could not be saved") {
		t.Fatalf("the reply should carry both warnings: %v", res)
	}
}

func TestEditGuardWarnsWhenTheSetupFileFails(t *testing.T) {
	w, lead, run, allowed := agentOnNewSession(t)
	path := blockSetupFile(t, w.bridge, "s2")
	res := editHook(w, lead, filepath.Join(allowed, "x.txt"))
	if msg := dig(res, "systemMessage"); !strings.HasPrefix(msg, "starship: ") || !strings.Contains(msg, path) {
		t.Fatalf("the allowed edit should carry the warning: %v", res)
	}
	if _, ok := res["hookSpecificOutput"]; ok {
		t.Errorf("the edit should be allowed: %v", res)
	}
	if st := w.runState(run); st.Session != "s2" || countRebound(st) != 1 {
		t.Errorf("the rebind should be saved: session %q, rebound entries %d", st.Session, countRebound(st))
	}
}

func TestEditGuardRefusalCarriesTheWarning(t *testing.T) {
	w, lead, _, _ := agentOnNewSession(t)
	path := blockSetupFile(t, w.bridge, "s2")
	res := editHook(w, lead, filepath.Join(t.TempDir(), "x.txt"))
	if dig(res, "hookSpecificOutput", "permissionDecision") != "deny" {
		t.Fatalf("the edit should be refused: %v", res)
	}
	if msg := dig(res, "systemMessage"); !strings.HasPrefix(msg, "starship: ") || !strings.Contains(msg, path) {
		t.Fatalf("the refusal should carry the warning: %v", res)
	}
}

func TestAgentHookRebindWarnsWhenTheSetupFileFails(t *testing.T) {
	w, lead, run, _ := agentOnNewSession(t)
	path := blockSetupFile(t, w.bridge, "s2")
	res, code := w.hook("PostToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator",
		"tool_name": "Bash", "tool_response": map[string]any{"backgroundTaskId": "bash1"}})
	if code != 0 {
		t.Fatalf("PostToolUse exited %d: %v", code, res)
	}
	if msg := dig(res, "systemMessage"); !strings.HasPrefix(msg, "starship: ") || !strings.Contains(msg, path) {
		t.Fatalf("the reply should carry the warning: %v", res)
	}
	if st := w.runState(run); st.Session != "s2" {
		t.Errorf("the rebind should be saved: session %q", st.Session)
	}
}

func TestBindSessionReportsAnUnreadablePipeline(t *testing.T) {
	w := newWorld(t)
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	runEnv(t, "supernova", "SUPERNOVA_TOOL=hello")
	w.run("supernova/auth-refactor")
	ids, _ := sessionRuns(w.bridge, "s1")
	if len(ids) != 1 {
		t.Fatalf("runs %v", ids)
	}
	pipeline := filepath.Join(runsDir(w.bridge), ids[0], "pipeline.json")
	if err := os.Remove(pipeline); err != nil {
		t.Fatal(err)
	}
	if err := bindSession(w.bridge, "s5", ids[0]); !errors.Is(err, errSetupFile) || !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("missing pipeline: %v", err)
	}
	write(t, pipeline, "{not json")
	if err := bindSession(w.bridge, "s6", ids[0]); !errors.Is(err, errSetupFile) || !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("corrupt pipeline: %v", err)
	}
	for _, s := range []string{"s5", "s6"} {
		if got := mustLive(t, w.bridge, s); !reflect.DeepEqual(got, ids) {
			t.Errorf("%s: the run should still be bound: %v", s, got)
		}
	}
}

func mutateRun(t *testing.T, w *world, run string, f func(st *State)) {
	t.Helper()
	r, err := openRun(w.bridge, run)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	f(r.State)
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
}

func leadBash(w *world, lead string) {
	w.t.Helper()
	if res, code := w.hook("PostToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator",
		"tool_name": "Bash", "tool_response": map[string]any{"backgroundTaskId": "bash1"}}); code != 0 {
		w.t.Fatalf("PostToolUse exited %d: %v", code, res)
	}
}

func blockStateSave(t *testing.T, w *world, run string) string {
	t.Helper()
	dir := filepath.Join(runsDir(w.bridge), run, "state.json.tmp")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFailedRebindSaveIsRetriedOnTheNextHook(t *testing.T) {
	for _, viaEdit := range []bool{true, false} {
		w, lead, run, allowed := agentOnNewSession(t)
		blocker := blockStateSave(t, w, run)
		if viaEdit {
			editHook(w, lead, filepath.Join(allowed, "x.txt"))
		} else if res, code := w.hook("PostToolUse", map[string]any{"agent_id": lead, "agent_type": "orchestrator",
			"tool_name": "Bash", "tool_response": map[string]any{"backgroundTaskId": "bash1"}}); code != 2 {
			t.Fatalf("PostToolUse with a failing save exited %d, want 2: %v", code, res)
		}
		if st := w.runState(run); st.Session != "s1" {
			t.Fatalf("edit %v: the save should have failed, session %q", viaEdit, st.Session)
		}
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
		hook := func() {
			if viaEdit {
				editHook(w, lead, filepath.Join(allowed, "x.txt"))
			} else {
				leadBash(w, lead)
			}
		}
		hook()
		st := w.runState(run)
		if st.Session != "s2" || countRebound(st) != 1 {
			t.Fatalf("edit %v: session %q, rebound entries %d; want s2 and one", viaEdit, st.Session, countRebound(st))
		}
		statePath := filepath.Join(runsDir(w.bridge), run, "state.json")
		before, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		hook()
		after, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if viaEdit && !bytes.Equal(before, after) {
			t.Errorf("a further edit hook rewrote state.json")
		}
		if st := w.runState(run); st.Session != "s2" || countRebound(st) != 1 {
			t.Errorf("edit %v: after a further hook, session %q, rebound entries %d; want s2 and one", viaEdit, st.Session, countRebound(st))
		}
	}
}

func TestAgentUnderTheOldSessionDoesNotTakeTheRunBack(t *testing.T) {
	for _, detail := range []string{"session s1 -> s2, found through agent a1", "session s1 -> s2 (clear)", ""} {
		w, lead, run, _ := agentOnNewSession(t)
		if detail == "" {
			leadBash(w, lead)
		} else {
			mutateRun(t, w, run, func(st *State) {
				st.Session = "s2"
				st.log("rebound", "", "", detail)
			})
		}
		w.sid = "s1"
		leadBash(w, lead)
		st := w.runState(run)
		if st.Session != "s2" || countRebound(st) != 1 {
			t.Errorf("detail %q: session %q, rebound entries %d; want s2 and one", detail, st.Session, countRebound(st))
		}
	}
}

func TestAgentUnderAnEarlierSessionDoesNotTakeTheRun(t *testing.T) {
	w, lead, run, _ := agentOnNewSession(t)
	leadBash(w, lead)
	w.sid = "s3"
	leadBash(w, lead)
	for _, sid := range []string{"s1", "s2"} {
		w.sid = sid
		leadBash(w, lead)
		st := w.runState(run)
		if st.Session != "s3" || countRebound(st) != 2 {
			t.Fatalf("from %s: session %q, rebound entries %d; want s3 and two", sid, st.Session, countRebound(st))
		}
	}
}

func TestMismatchedOwnerIsKeptWhenTheCallerIsNotItsAgent(t *testing.T) {
	cases := map[string]func(w *world){
		"session hook": func(w *world) {
			w.hook("PreToolUse", map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "coder", "prompt": "x"}})
		},
		"other agent": func(w *world) {
			w.hook("PostToolUse", map[string]any{"agent_id": "stranger", "agent_type": "orchestrator",
				"tool_name": "Bash", "tool_response": map[string]any{"backgroundTaskId": "bash1"}})
		},
	}
	for name, call := range cases {
		w, _, run, _ := agentOnNewSession(t)
		w.sid = "s1"
		mutateRun(t, w, run, func(st *State) { st.Session = "s9" })
		call(w)
		if st := w.runState(run); st.Session != "s9" || countRebound(st) != 0 {
			t.Errorf("%s: session %q, rebound entries %d; want s9 and none", name, st.Session, countRebound(st))
		}
	}
}

func TestCarryOverNamesTheLinkAndTheOwnerAsRebound(t *testing.T) {
	w, lead, run, _ := agentOnNewSession(t)
	mutateRun(t, w, run, func(st *State) { st.Session = "s9" })
	if err := bindSession(w.bridge, "s9", run); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "s1"})
	write(t, w.session, string(rec)+"\n")
	if _, code := w.hook("SessionStart", map[string]any{"source": "resume"}); code != 0 {
		t.Fatalf("SessionStart exited %d", code)
	}
	st := w.runState(run)
	if st.Session != "s2" {
		t.Fatalf("session %q after the carry, want s2", st.Session)
	}
	for _, old := range []string{"s1", "s9"} {
		if !st.reboundAwayFrom(old) {
			t.Errorf("history does not show a rebind away from %s", old)
		}
	}
	if n := countRebound(st); n != 2 {
		t.Errorf("rebound entries %d, want 2", n)
	}
	w.sid = "s9"
	leadBash(w, lead)
	if st := w.runState(run); st.Session != "s2" || countRebound(st) != 2 {
		t.Errorf("an agent under s9 took the run back: session %q, rebound entries %d", st.Session, countRebound(st))
	}
}

func TestFinishedRunWithAMismatchedOwnerIsNotRebound(t *testing.T) {
	w, lead, run, _ := agentOnNewSession(t)
	w.sid = "s1"
	mutateRun(t, w, run, func(st *State) {
		st.Session = "s9"
		st.Status = "success"
	})
	leadBash(w, lead)
	if st := w.runState(run); st.Session != "s9" || countRebound(st) != 0 {
		t.Errorf("session %q, rebound entries %d; want s9 and none", st.Session, countRebound(st))
	}
}
