// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func stopEv(agentType, id string, extra map[string]any) map[string]any {
	e := map[string]any{"hook_event_name": "SubagentStop", "agent_type": agentType, "agent_id": id, "agent_transcript_path": "/nonexistent"}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func handbackEv(id, msg string) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "SubagentHandback", "agent_id": id,
		"tool_input": map[string]any{"message": msg}}
}

func spawnEv(from, to string, background bool) map[string]any {
	ti := map[string]any{"subagent_type": to}
	if background {
		ti["run_in_background"] = true
	}
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "agent_type": from, "agent_id": "orch1", "tool_input": ti}
}

func TestHandbackMessage(t *testing.T) {
	events := []map[string]any{handbackEv("a1", "one"), handbackEv("a1", "two"), handbackEv("", "anon")}
	if msg, ok := handbackMessage(events, "a1"); !ok || msg != "two" {
		t.Errorf("want the last handback of a1, got %q %v", msg, ok)
	}
	if _, ok := handbackMessage(events, "a2"); ok {
		t.Error("an agent that never handed back should not be found")
	}
	if _, ok := handbackMessage(events, ""); ok {
		t.Error("an empty id must never match")
	}
}

func TestStopReportProbe(t *testing.T) {
	good := []map[string]any{
		handbackEv("a1", "the word "+leafToken),
		stopEv("probe-leaf", "a1", map[string]any{"last_assistant_message": "done"}),
	}
	if !stopReportProbe(good) {
		t.Error("a handback holding the token should pass")
	}
	if !stopReportProbe([]map[string]any{stopEv("probe-leaf", "a1", map[string]any{"last_assistant_message": leafToken})}) {
		t.Error("last_assistant_message holding the token should pass without a handback")
	}
	for name, events := range map[string][]map[string]any{
		"no stop":                 {handbackEv("a1", leafToken)},
		"empty report":            {stopEv("probe-leaf", "a1", map[string]any{"last_assistant_message": ""})},
		"missing report":          {stopEv("probe-leaf", "a1", nil)},
		"closing text only":       {handbackEv("a1", "nothing"), stopEv("probe-leaf", "a1", map[string]any{"last_assistant_message": leafToken})},
		"handback of others":      {handbackEv("b2", leafToken), stopEv("probe-leaf", "a1", nil)},
		"empty ids match nothing": {handbackEv("", leafToken), stopEv("probe-leaf", "", nil)},
		"no transcript path":      {stopEv("probe-leaf", "a1", map[string]any{"last_assistant_message": leafToken, "agent_transcript_path": ""})},
	} {
		if stopReportProbe(events) {
			t.Errorf("%s: should fail", name)
		}
	}
}

func TestHandbackRedoProbe(t *testing.T) {
	good := []map[string]any{
		handbackEv("r1", redoFirst),
		handbackEv("r1", redoSecond),
		stopEv("probe-redo", "r1", nil),
	}
	if !handbackRedoProbe(good) {
		t.Error("a refused handback followed by a corrected one and a stop should pass")
	}
	for name, events := range map[string][]map[string]any{
		"no second handback":    {handbackEv("r1", redoFirst), stopEv("probe-redo", "r1", nil)},
		"other agent second":    {handbackEv("r1", redoFirst), handbackEv("r2", redoSecond), stopEv("probe-redo", "r1", nil)},
		"second before first":   {handbackEv("r1", redoSecond), handbackEv("r1", redoFirst), stopEv("probe-redo", "r1", nil)},
		"stop of another agent": {handbackEv("r1", redoFirst), handbackEv("r1", redoSecond), stopEv("probe-redo", "r2", nil)},
		"empty ids":             {handbackEv("", redoFirst), handbackEv("", redoSecond), stopEv("probe-redo", "", nil)},
		"no stop after":         {handbackEv("r1", redoFirst), handbackEv("r1", redoSecond)},
		"stop before second":    {handbackEv("r1", redoFirst), stopEv("probe-redo", "r1", nil), handbackEv("r1", redoSecond)},
	} {
		if handbackRedoProbe(events) {
			t.Errorf("%s: should fail", name)
		}
	}
}

func TestStopBlockProbe(t *testing.T) {
	ok, saw := stopBlockProbe([]map[string]any{
		stopEv("probe-stop", "s1", nil),
		stopEv("probe-stop", "s1", map[string]any{"stop_hook_active": true}),
	})
	if !ok || saw {
		t.Errorf("two stops, the second active: ok=%v saw=%v", ok, saw)
	}
	ok, _ = stopBlockProbe([]map[string]any{stopEv("probe-stop", "s1", nil)})
	if ok {
		t.Error("no second stop should fail")
	}
	ok, _ = stopBlockProbe([]map[string]any{
		stopEv("probe-stop", "s1", nil),
		stopEv("probe-stop", "s1", map[string]any{"stop_hook_active": false}),
	})
	if ok {
		t.Error("a second stop without stop_hook_active should fail")
	}
	ok, saw = stopBlockProbe([]map[string]any{
		handbackEv("s1", "done"),
		stopEv("probe-stop", "s1", nil),
		stopEv("probe-stop", "s1", map[string]any{"stop_hook_active": true}),
	})
	if ok || !saw {
		t.Errorf("a handback in the non-auto session must fail and be reported: ok=%v saw=%v", ok, saw)
	}
}

func TestBackgroundProbe(t *testing.T) {
	events := func(order ...map[string]any) []map[string]any { return order }
	withID := func(e map[string]any, id string) map[string]any { e["tool_use_id"] = id; return e }
	leafSpawn := withID(spawnEv("probe-orch", "probe-leaf", true), "t1")
	deniedSpawn := spawnEv("probe-orch", "probe-denied", false)
	leafStop := stopEv("probe-leaf", "a1", nil)
	post := func(id string) map[string]any {
		return map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Agent", "agent_type": "probe-orch", "tool_use_id": id}
	}
	handback := handbackEv("a1", leafToken)

	asked, fg := backgroundProbe(events(leafSpawn, handback, leafStop, post("t1"), deniedSpawn))
	if !asked || !fg {
		t.Errorf("PostToolUse after the leaf's stop: asked=%v fg=%v", asked, fg)
	}
	_, fg = backgroundProbe(events(leafSpawn, deniedSpawn, handback, leafStop, post("t1")))
	if !fg {
		t.Error("a parallel spawn before the leaf's stop must not fail a foreground run")
	}
	_, fg = backgroundProbe(events(leafSpawn, post("t1"), handback, leafStop, deniedSpawn))
	if fg {
		t.Error("a PostToolUse before the leaf's stop is a background launch")
	}
	_, fg = backgroundProbe(events(leafSpawn, handback, leafStop, deniedSpawn))
	if fg {
		t.Error("a missing PostToolUse should fail")
	}
	_, fg = backgroundProbe(events(leafSpawn, handback, leafStop, post("t2")))
	if fg {
		t.Error("another call's PostToolUse must not count")
	}
	_, fg = backgroundProbe(events(leafSpawn, handbackEv("a1", "other"), leafStop, post("t1")))
	if fg {
		t.Error("a handback without the token should fail")
	}
	_, fg = backgroundProbe(events(spawnEv("probe-orch", "probe-leaf", true), handbackEv("", leafToken), stopEv("probe-leaf", "", nil), post("")))
	if fg {
		t.Error("calls and handbacks with no id must not be matched")
	}
	_, fg = backgroundProbe(events(leafSpawn, handbackEv("a2", leafToken), leafStop, post("t1")))
	if fg {
		t.Error("another agent's handback must not count")
	}
	plainStop := stopEv("probe-leaf", "a1", nil)
	plainStop["last_assistant_message"] = leafToken
	_, fg = backgroundProbe(events(leafSpawn, plainStop, post("t1")))
	if !fg {
		t.Error("a plain-reply report holding the token should pass")
	}
	_, fg = backgroundProbe(events(leafSpawn, post("t1")))
	if fg {
		t.Error("no leaf stop should fail")
	}
	asked, _ = backgroundProbe(events(spawnEv("probe-orch", "probe-leaf", false), leafStop, deniedSpawn))
	if asked {
		t.Error("a foreground request is not an ask")
	}
}

func startEv(source string) map[string]any {
	e := map[string]any{"hook_event_name": "SessionStart"}
	if source != "" {
		e["source"] = source
	}
	return e
}

func TestSourcesReported(t *testing.T) {
	ev := func(s string) []map[string]any { return []map[string]any{startEv(s)} }
	if !sourcesReported(ev("startup"), ev("resume")) {
		t.Errorf("startup then resume should pass")
	}
	for name, c := range map[string][2][]map[string]any{
		"no source":       {ev(""), ev("")},
		"not resumed":     {ev("startup"), ev("startup")},
		"wrong first":     {ev("resume"), ev("resume")},
		"no SessionStart": {nil, nil},
	} {
		if sourcesReported(c[0], c[1]) {
			t.Errorf("%s should fail", name)
		}
	}
}

func TestRestartTied(t *testing.T) {
	old := `{"type":"user","sessionId":"s1"}`
	cur := `{"type":"user","sessionId":"s2"}`
	if !restartTied("s1", "s1", nil) {
		t.Errorf("an unchanged id should pass")
	}
	if !restartTied("s1", "s2", []string{old, cur, "not json"}) {
		t.Errorf("earlier records should tie a changed id")
	}
	for name, ok := range map[string]bool{
		"changed id, no earlier records": restartTied("s1", "s2", []string{cur}),
		"changed id, no transcript":      restartTied("s1", "s2", nil),
		"changed id, unreadable resumed": restartTied("s1", "s2", []string(nil)),
		"no first id":                    restartTied("", "", []string{old}),
		"no resumed id":                  restartTied("s1", "", []string{old}),
	} {
		if ok {
			t.Errorf("%s should fail", name)
		}
	}
}

func bashEv(id string) map[string]any {
	return map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "agent_id": id}
}

func TestAgentRanAgain(t *testing.T) {
	first := []map[string]any{stopEv("probe-restart", "a1", nil)}
	if !agentRanAgain(first, []map[string]any{bashEv("a1")}) {
		t.Errorf("the same agent id should pass")
	}
	for name, c := range map[string][2][]map[string]any{
		"different id":        {first, {bashEv("a2")}},
		"never ran":           {first, {startEv("resume")}},
		"no call, only stop":  {first, {stopEv("probe-restart", "a1", nil)}},
		"first never stopped": {nil, {bashEv("a1")}},
	} {
		if agentRanAgain(c[0], c[1]) {
			t.Errorf("%s should fail", name)
		}
	}
}

func TestAgentPathKept(t *testing.T) {
	at := func(id, path string) []map[string]any {
		return []map[string]any{stopEv("probe-restart", id, map[string]any{"agent_transcript_path": path})}
	}
	if !agentPathKept(at("a1", "/p/a1.jsonl"), at("a1", "/p/a1.jsonl")) {
		t.Errorf("the same path should pass")
	}
	for name, c := range map[string][2][]map[string]any{
		"different path":  {at("a1", "/p/a1.jsonl"), at("a1", "/q/a1.jsonl")},
		"no resumed stop": {at("a1", "/p/a1.jsonl"), nil},
		"other agent":     {at("a1", "/p/a1.jsonl"), at("a2", "/p/a1.jsonl")},
		"no first path":   {at("a1", ""), at("a1", "")},
		"no first agent":  {nil, at("a1", "/p/a1.jsonl")},
	} {
		if agentPathKept(c[0], c[1]) {
			t.Errorf("%s should fail", name)
		}
	}
}

func TestManualInTranscript(t *testing.T) {
	const token = "supernova"
	ctx := func(content string) string {
		b, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{
			"type": "hook_additional_context", "hookEvent": "SessionStart", "hookName": "SessionStart", "content": []string{content}}})
		return string(b)
	}
	whole := probeManual(token)
	success, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{
		"type": "hook_success", "hookEvent": "SessionStart", "stdout": whole}})
	preview := "<persisted-output>\nOutput too large (19.4KB). Full output saved to: /home/u/supernova/tool-results/hook-1-additionalContext.txt\n\nPreview (first 2KB):\n" + whole[:2000]
	tests := []struct {
		name  string
		lines []string
		ok    bool
		why   string
	}{
		{"whole", []string{"not json", ctx(whole)}, true, ""},
		{"crlf", []string{ctx(whole) + "\r"}, true, ""},
		{"persisted", []string{ctx(preview)}, false, "preview"},
		{"cut", []string{ctx(whole[:len(whole)/2])}, false, "cut"},
		{"dropped", []string{string(success)}, false, "dropped"},
	}
	for _, tt := range tests {
		ok, why := manualInTranscript(tt.lines, token)
		if ok != tt.ok || !strings.Contains(why, tt.why) {
			t.Errorf("%s: got %v, %q; want %v, %q", tt.name, ok, why, tt.ok, tt.why)
		}
	}
}

func postBgEv(agentType, session string, resp any) map[string]any {
	return map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "agent_type": agentType, "session_id": session, "tool_response": resp}
}

func tasksOf(ids ...string) []any {
	var out []any
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "type": "shell", "status": "running"})
	}
	return out
}

func TestBackgroundListed(t *testing.T) {
	post := func(id string) map[string]any {
		return postBgEv("probe-bg", "s1", map[string]any{"backgroundTaskId": id})
	}
	stop := func(agentType string, tasks any) map[string]any {
		return stopEv(agentType, "a1", map[string]any{"background_tasks": tasks})
	}
	if backgroundListed(nil) || backgroundListed([]map[string]any{}) {
		t.Error("no events should fail")
	}
	if !backgroundListed([]map[string]any{post("b1"), stop("probe-bg", tasksOf("b1"))}) {
		t.Error("a matching id should pass")
	}
	if backgroundListed([]map[string]any{post("b1")}) {
		t.Error("an id only in PostToolUse should fail")
	}
	if backgroundListed([]map[string]any{stop("probe-bg", tasksOf("b1"))}) {
		t.Error("an id only in SubagentStop should fail")
	}
	if backgroundListed([]map[string]any{post("b1"), stop("probe-bg", tasksOf("b2"))}) {
		t.Error("a mismatched id should fail")
	}
	if backgroundListed([]map[string]any{postBgEv("probe-leaf", "s1", map[string]any{"backgroundTaskId": "b1"}), stop("probe-bg", tasksOf("b1"))}) {
		t.Error("a PostToolUse from another agent_type should fail")
	}
	if backgroundListed([]map[string]any{post("b1"), stop("probe-leaf", tasksOf("b1"))}) {
		t.Error("a SubagentStop from another agent_type should fail")
	}
	if backgroundListed([]map[string]any{post(""), stop("probe-bg", tasksOf(""))}) {
		t.Error("an empty id should fail")
	}
	if backgroundListed([]map[string]any{post("b1"), stop("probe-bg", []any{map[string]any{"type": "shell"}})}) {
		t.Error("a task with no id should fail")
	}
	if backgroundListed([]map[string]any{postBgEv("probe-bg", "s1", "b1"), stop("probe-bg", tasksOf("b1"))}) {
		t.Error("a string tool_response should fail")
	}
	if backgroundListed([]map[string]any{post("b1"), stop("probe-bg", "b1"), stop("probe-bg", map[string]any{"id": "b1"})}) {
		t.Error("background_tasks of the wrong type should fail")
	}
	if !backgroundListed([]map[string]any{post("b2"), stop("probe-bg", tasksOf("b1", "b2", "b3"))}) {
		t.Error("one matching task among several should pass")
	}
	if !backgroundListed([]map[string]any{stop("probe-bg", tasksOf("b1")), post("b1")}) {
		t.Error("a SubagentStop logged before the PostToolUse should pass")
	}
}

func TestStopListsBackground(t *testing.T) {
	post := func(session, id string) map[string]any {
		return postBgEv("", session, map[string]any{"backgroundTaskId": id})
	}
	stop := func(session string, tasks any) map[string]any {
		return map[string]any{"hook_event_name": "Stop", "session_id": session, "background_tasks": tasks}
	}
	if stopListsBackground(nil) {
		t.Error("no events should fail")
	}
	if !stopListsBackground([]map[string]any{post("s1", "b1"), stop("s1", tasksOf("b1"))}) {
		t.Error("a matching id should pass")
	}
	if !stopListsBackground([]map[string]any{post("s1", "b1"), stop("s1", tasksOf("b1")), stop("s1", []any{})}) {
		t.Error("a later empty Stop should not undo it")
	}
	if stopListsBackground([]map[string]any{post("s1", "b1"), stop("s1", []any{})}) {
		t.Error("an empty list should fail")
	}
	if stopListsBackground([]map[string]any{post("s1", "b1")}) {
		t.Error("no Stop should fail")
	}
	if stopListsBackground([]map[string]any{stop("s1", tasksOf("b1"))}) {
		t.Error("no PostToolUse should fail")
	}
	if stopListsBackground([]map[string]any{post("s1", "b1"), stop("s1", tasksOf("b2"))}) {
		t.Error("a mismatched id should fail")
	}
	if stopListsBackground([]map[string]any{post("s1", "b1"), stop("s2", tasksOf("b1"))}) {
		t.Error("another session should fail")
	}
	if stopListsBackground([]map[string]any{post("s1", ""), stop("s1", tasksOf(""))}) {
		t.Error("an empty id should fail")
	}
	if stopListsBackground([]map[string]any{postBgEv("probe-bg", "s1", map[string]any{"backgroundTaskId": "b1"}), stop("s1", tasksOf("b1"))}) {
		t.Error("a subagent's PostToolUse should fail")
	}
	if stopListsBackground([]map[string]any{post("s1", "b1"), {"hook_event_name": "SubagentStop", "session_id": "s1", "background_tasks": tasksOf("b1")}}) {
		t.Error("a SubagentStop should fail")
	}
	if stopListsBackground([]map[string]any{post("s1", "b1"), stop("s1", "b1")}) {
		t.Error("background_tasks of the wrong type should fail")
	}
}

func TestParseProbeSession(t *testing.T) {
	result, id, err := parseProbeSession([]byte(`{"result": "done", "session_id": "abc"}`))
	if err != nil || result != "done" || id != "abc" {
		t.Errorf("valid JSON gave %q, %q, %v", result, id, err)
	}
	if _, _, err := parseProbeSession([]byte("Error: not logged in")); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("non-JSON should fail and show the output, got %v", err)
	}
	if _, _, err := parseProbeSession(nil); err == nil {
		t.Error("empty output should fail")
	}
}

func TestNewProbeDirQuotesTheHookCommands(t *testing.T) {
	self := `/opt/super nova/ensign`
	dir, err := (&probeHost{root: t.TempDir()}).newProbeDir(self, "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings are not JSON: %v\n%s", err, data)
	}
	if len(settings.Hooks) != 4 {
		t.Fatalf("want 4 hook events, got %d", len(settings.Hooks))
	}
	for ev, groups := range settings.Hooks {
		want := `"/opt/super nova/ensign" selftest-hook ` + ev
		if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != want {
			t.Errorf("%s: want command %q, got %+v", ev, want, groups)
		}
	}
}

func TestProbeStopSettingsQuotesTheHookCommand(t *testing.T) {
	var settings struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(probeStopSettings(`/opt/super nova/ensign`)), &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings.Hooks.Stop[0].Hooks[0].Command; got != `"/opt/super nova/ensign" selftest-hook Stop` {
		t.Errorf("command = %q", got)
	}
}

func TestProbeHookJSONIsValidForAnyPath(t *testing.T) {
	self := "/opt/super\x01nova\n/ensign\\"
	var groups []struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(probeHookGroups("Bash", probeHookCommand(self, "PreToolUse"))), &groups); err != nil {
		t.Fatal(err)
	}
	if got, want := groups[0].Hooks[0].Command, probeHookCommand(self, "PreToolUse"); got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func fakeClaude(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a POSIX shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunProbeSessionTimeoutKillsChildren(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "beat")
	fakeClaude(t, "echo started\n(while :; do echo x >> "+heartbeat+"; sleep 0.1; done) &\nwait\n")
	old := probeTimeout
	probeTimeout = time.Second
	defer func() { probeTimeout = old }()
	start := time.Now()
	_, _, _, err := runProbeSession(nil, t.TempDir(), "supernova")
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "started") {
		t.Fatalf("err = %v, want timed out with the output", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("took %s, want well under the wait delay plus limit", elapsed)
	}
	before, _ := os.Stat(heartbeat)
	time.Sleep(600 * time.Millisecond)
	after, _ := os.Stat(heartbeat)
	if before == nil || after == nil || before.Size() != after.Size() {
		t.Errorf("the child is still writing: %v then %v", before, after)
	}
}

func TestRunProbeSessionLeftoverPipeHolderIsSuccess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	fakeClaude(t, `echo '{"result":"supernova","session_id":"s1"}'`+"\nsleep 8 &\necho $! > "+pidFile+"\nexit 0\n")
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					p.Kill()
				}
			}
		}
	})
	_, result, sessionID, err := runProbeSession(nil, t.TempDir(), "supernova")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result != "supernova" || sessionID != "s1" {
		t.Errorf("result %q session %q", result, sessionID)
	}
}

func TestProbeHookCommandSurvivesShellSpecialPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("such directory names are not valid on Windows")
	}
	dir := filepath.Join(t.TempDir(), "sp ace $HOME `x` \"q\" back\\$slash")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "args")
	script := filepath.Join(dir, "ensign")
	body := "#!/bin/sh\necho \"$@\" > '" + out + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("sh", "-c", probeHookCommand(script, "Stop")).CombinedOutput(); err != nil {
		t.Fatalf("hook command failed: %v: %s", err, b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "selftest-hook Stop" {
		t.Fatalf("script args = %q", got)
	}
}

func TestReportReachedDispatcher(t *testing.T) {
	line := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	call := func(id, typ string) string {
		return line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": "Agent", "input": map[string]any{"subagent_type": typ}}}}})
	}
	result := func(id string, content any) string {
		return line(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}}})
	}
	pointer := func(agent string) []any {
		return []any{map[string]any{"type": "text", "text": "  This agent's report was delivered to you as a message from \"" + agent + "\".\n  \nagentId: " + agent + "\n<usage>tool_uses: 1</usage>"}}
	}
	origin := func(agent string, report string) map[string]any {
		o := map[string]any{"kind": "peer", "from": agent, "senderTaskId": agent, "name": "probe-handback", "handback": true}
		if report != "" {
			o["handbackReportText"] = report
		}
		return o
	}
	attachment := func(agent, report, prompt string) string {
		return line(map[string]any{"type": "attachment", "attachment": map[string]any{
			"type": "queued_command", "prompt": prompt, "origin": origin(agent, report)}})
	}
	userDelivery := func(agent, report string) string {
		return line(map[string]any{"type": "user", "isMeta": true, "origin": origin(agent, report), "message": map[string]any{
			"role": "user", "content": "<agent-message from=\"" + agent + "\">\n  " + report + "\n</agent-message>"}})
	}
	blocks := []any{map[string]any{"type": "text", "text": "report: " + handbackToken}}
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"text blocks", []string{call("t1", "probe-handback"), result("t1", blocks)}, true},
		{"string", []string{call("t1", "probe-handback"), result("t1", "report: "+handbackToken)}, true},
		{"crlf and noise", []string{"not json", call("t1", "probe-handback") + "\r", result("t1", "report: "+handbackToken) + "\r"}, true},
		{"no token", []string{call("t1", "probe-handback"), result("t1", "Done.")}, false},
		{"other agent", []string{call("t1", "probe-leaf"), result("t1", blocks)}, false},
		{"result of another call", []string{call("t1", "probe-handback"), result("t2", blocks)}, false},
		{"no result", []string{call("t1", "probe-handback")}, false},
		{"pointer and attachment", []string{call("t1", "probe-handback"), result("t1", pointer("a1")), attachment("a1", handbackToken, "x")}, true},
		{"pointer and attachment with crlf", []string{call("t1", "probe-handback") + "\r", result("t1", pointer("a1")) + "\r", attachment("a1", handbackToken, "x") + "\r"}, true},
		{"pointer and user delivery", []string{call("t1", "probe-handback"), result("t1", pointer("a1")), userDelivery("a1", handbackToken)}, true},
		{"delivery before result", []string{attachment("a1", handbackToken, "x"), call("t1", "probe-handback"), result("t1", pointer("a1"))}, true},
		{"delivery from another agent", []string{call("t1", "probe-handback"), result("t1", pointer("a1")), attachment("a2", handbackToken, "x")}, false},
		{"delivery of another type", []string{call("t1", "probe-leaf"), result("t1", pointer("a1")), attachment("a1", handbackToken, "x")}, false},
		{"token only in prompt", []string{call("t1", "probe-handback"), result("t1", pointer("a1")), attachment("a1", "", "<agent-message>\n  "+handbackToken+"\n</agent-message>")}, true},
		{"pointer without delivery", []string{call("t1", "probe-handback"), result("t1", pointer("a1"))}, false},
		{"async stub without delivery", []string{call("t1", "probe-handback"), result("t1", "Async agent launched.\nagentId: a1\n")}, false},
		{"token in a stray line", []string{call("t1", "probe-handback"), result("t1", pointer("a1")), line(map[string]any{"type": "assistant", "note": handbackToken, "agentId": "a1"})}, false},
	}
	for _, tt := range tests {
		if got := reportReachedDispatcher(tt.lines, "probe-handback", handbackToken); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

func TestReportLines(t *testing.T) {
	pass := selftestCheck{"first", true, ""}
	fail := selftestCheck{"second", false, ""}
	unsure := selftestCheck{"third", false, "the cause is the third's own"}
	adv := func(ok bool) selftestCheck { return selftestCheck{"the advisory one", ok, ""} }
	var b strings.Builder
	if err := reportLines(&b, []selftestCheck{pass}, adv(true), true, ""); err != nil || !strings.Contains(b.String(), "all harness checks passed") || strings.Contains(b.String(), "warn") {
		t.Errorf("all pass: %v %q", err, b.String())
	}
	b.Reset()
	err := reportLines(&b, []selftestCheck{pass}, adv(false), true, "")
	if err != nil || strings.Contains(b.String(), "enforcement may not work") || strings.Contains(b.String(), "FAIL") ||
		!strings.Contains(b.String(), "warn the advisory one") || !strings.Contains(b.String(), "one advisory check did not") {
		t.Errorf("advisory fails: %v %q", err, b.String())
	}
	for _, a := range []bool{true, false} {
		b.Reset()
		err := reportLines(&b, []selftestCheck{pass, fail}, adv(a), false, "dropped")
		if err == nil || !strings.Contains(err.Error(), "1 of 2 harness checks failed") || !strings.Contains(b.String(), "FAIL second") {
			t.Errorf("counted fails (advisory %v): %v %q", a, err, b.String())
		}
		if i, j := strings.Index(b.String(), "FAIL second"), strings.Index(b.String(), "dropped"); j < i {
			t.Errorf("evidence line before checks: %q", b.String())
		}
	}
	for _, a := range []bool{true, false} {
		b.Reset()
		err := reportLines(&b, []selftestCheck{pass, unsure, unsure}, adv(a), true, "")
		got := b.String()
		if err != nil || strings.Contains(got, "all harness checks passed") || strings.Contains(got, "harness checks passed;") ||
			!strings.Contains(got, "inconclusive third") || !strings.Contains(got, "2 inconclusive") || !strings.Contains(got, "run the selftest again") ||
			strings.Contains(got, "FAIL") || strings.Contains(got, "one advisory check did not pass") != !a {
			t.Errorf("inconclusive (advisory %v): %v %q", a, err, got)
		}
		if i, j := strings.Index(got, "inconclusive third"), strings.Index(got, "the cause is the third's own, so the probe says nothing either way; run the selftest again"); j < i || j < 0 {
			t.Errorf("explanation should follow the check: %q", got)
		}
	}
	b.Reset()
	err = reportLines(&b, []selftestCheck{pass, fail, unsure}, adv(true), true, "")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 harness checks failed") || !strings.Contains(err.Error(), "1 inconclusive") ||
		strings.Contains(b.String(), "all harness checks passed") || !strings.Contains(b.String(), "inconclusive third") {
		t.Errorf("failed and inconclusive: %v %q", err, b.String())
	}
}

func TestRestartAgentVerdicts(t *testing.T) {
	first := []map[string]any{stopEv("probe-restart", "a1", map[string]any{"agent_transcript_path": "/t/a1"})}
	bashOther := bashEv("a2")
	noAgent, notRun := "never started", "did not reach the resumed session"
	tests := []struct {
		name    string
		first   []map[string]any
		resumed []map[string]any
		stage   restartStage
		id, p   verdict
		why     string
	}{
		{"no events", first, nil, resumeCompleted, verdictFail, verdictFail, notRun},
		{"only a session bash", first, []map[string]any{bashEv("")}, resumeCompleted, verdictFail, verdictFail, notRun},
		{"first agent id empty", []map[string]any{stopEv("probe-restart", "", nil)}, []map[string]any{bashEv("a1")}, resumeNotStarted, verdictInconclusive, verdictInconclusive, noAgent},
		{"no first stop", nil, []map[string]any{bashEv("a1")}, resumeNotStarted, verdictInconclusive, verdictInconclusive, noAgent},
		{"other id by bash", first, []map[string]any{bashOther}, resumeCompleted, verdictFail, verdictFail, ""},
		{"other id by stop", first, []map[string]any{stopEv("probe-restart", "a2", nil)}, resumeCompleted, verdictFail, verdictFail, ""},
		{"ran, never stopped", first, []map[string]any{bashEv("a1")}, resumeCompleted, verdictPass, verdictInconclusive, "its stop was not seen"},
		{"ran and stopped, same path", first, []map[string]any{bashEv("a1"), stopEv("probe-restart", "a1", map[string]any{"agent_transcript_path": "/t/a1"})}, resumeCompleted, verdictPass, verdictPass, ""},
		{"ran and stopped, other path", first, []map[string]any{bashEv("a1"), stopEv("probe-restart", "a1", map[string]any{"agent_transcript_path": "/t/other"})}, resumeCompleted, verdictPass, verdictFail, ""},
		{"stopped without a bash", first, []map[string]any{stopEv("probe-restart", "a1", map[string]any{"agent_transcript_path": "/t/a1"})}, resumeCompleted, verdictPass, verdictPass, ""},
		{"never started", first, nil, resumeNotStarted, verdictInconclusive, verdictInconclusive, "never started"},
		{"never started, no session id", first, nil, resumeNotStarted, verdictInconclusive, verdictInconclusive, "no session_id"},
		{"failed", first, []map[string]any{bashEv("a1")}, resumeFailed, verdictInconclusive, verdictInconclusive, "resumed session failed"},
	}
	for _, tt := range tests {
		if id, p, why := restartAgentVerdicts(tt.first, tt.resumed, tt.stage, "unknown"); id != tt.id || p != tt.p || !strings.Contains(why, tt.why) || (tt.why == "") != (why == "") {
			t.Errorf("%s: got %v, %v, %q; want %v, %v, %q", tt.name, id, p, why, tt.id, tt.p, tt.why)
		}
	}
	sentTests := []struct {
		name  string
		stage restartStage
		sent  string
		want  verdict
	}{
		{"sent, never ran", resumeCompleted, "true", verdictFail},
		{"not sent, never ran", resumeCompleted, "false", verdictFail},
		{"unknown, never ran", resumeCompleted, "unknown", verdictFail},
		{"sent, resume failed", resumeFailed, "true", verdictInconclusive},
		{"sent, first failed", firstFailed, "true", verdictInconclusive},
		{"sent, never started", resumeNotStarted, "true", verdictInconclusive},
	}
	for _, tt := range sentTests {
		if id, p, _ := restartAgentVerdicts(first, nil, tt.stage, tt.sent); id != tt.want || p != tt.want {
			t.Errorf("%s: got %v, %v; want %v", tt.name, id, p, tt.want)
		}
	}
}

func TestCalledSendMessage(t *testing.T) {
	line := func(typ string, blocks ...any) string {
		b, _ := json.Marshal(map[string]any{"type": typ, "message": map[string]any{"content": blocks}})
		return string(b)
	}
	use := func(name string) any {
		return map[string]any{"type": "tool_use", "name": name, "input": map[string]any{"to": "a1", "message": "go"}}
	}
	withID := func(b any, id string) any {
		m := map[string]any{"id": id}
		for k, v := range b.(map[string]any) {
			m[k] = v
		}
		return m
	}
	text, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": "SendMessage"}})
	tests := []struct {
		name  string
		lines []string
		want  bool
		skip  map[string]bool
	}{
		{"none", nil, false, nil},
		{"called", []string{"not json", line("assistant", map[string]any{"type": "text"}, use("ToolSearch")), line("assistant", use("SendMessage"))}, true, nil},
		{"other tools", []string{line("assistant", use("Agent"), use("Bash"))}, false, nil},
		{"a user record", []string{line("user", use("SendMessage"))}, false, nil},
		{"string content", []string{string(text)}, false, nil},
		{"other recipient", []string{line("assistant", map[string]any{"type": "tool_use", "name": "SendMessage", "input": map[string]any{"to": "a2"}})}, false, nil},
		{"no input", []string{line("assistant", map[string]any{"type": "tool_use", "name": "SendMessage"})}, false, nil},
		{"id in a nested field", []string{line("assistant", map[string]any{"type": "tool_use", "name": "SendMessage", "input": map[string]any{"target": map[string]any{"id": "a1"}}})}, true, nil},
		{"empty id", []string{line("assistant", use("SendMessage"))}, false, nil},
		{"skipped id", []string{line("assistant", withID(use("SendMessage"), "t1"))}, false, map[string]bool{"t1": true}},
		{"skipped id, another call", []string{line("assistant", withID(use("SendMessage"), "t1"), withID(use("SendMessage"), "t2"))}, true, map[string]bool{"t1": true}},
		{"call with no id is not skipped", []string{line("assistant", use("SendMessage"))}, true, map[string]bool{"": true}},
	}
	for _, tt := range tests {
		agent := "a1"
		if tt.name == "empty id" {
			agent = ""
		}
		if got := calledSendMessage(tt.lines, agent, tt.skip); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

func TestRestartChecks(t *testing.T) {
	stopped := stopEv("probe-restart", "a1", map[string]any{"agent_transcript_path": "/t/a1"})
	call := func(id, to string) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "SendMessage", "input": map[string]any{"to": to, "message": "go"}}}}})
		return string(b)
	}
	sent := call("t1", "a1")
	sentAgain := call("t2", "a1")
	before := map[string]bool{"t1": true}
	failedResult := `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"no agent a1"}]}}`
	probe := func(resumed []map[string]any, transcript ...string) *restartProbe {
		return &restartProbe{
			stage:   resumeCompleted,
			first:   restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1"},
			resumed: restartRun{events: append([]map[string]any{startEv("resume")}, resumed...), session: "s1", transcript: transcript},
		}
	}
	other, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Bash"}}}})
	state := func(cs []selftestCheck) string {
		var out []string
		for _, c := range cs {
			out = append(out, map[bool]string{true: "ok", false: "no"}[c.ok]+map[bool]string{true: "?", false: ""}[c.inconclusive != ""])
		}
		return strings.Join(out, " ")
	}
	tests := []struct {
		name string
		p    *restartProbe
		want string
		said string
		why  string
	}{
		{"never ran, never sent", probe(nil, string(other)), "ok ok no no", "did not reach the resumed session", ""},
		{"wrong recipient", probe(nil, call("t3", "a2")), "ok ok no no", "did not reach the resumed session", ""},
		{"call before the resume, same file", &restartProbe{stage: resumeCompleted, callsBefore: before, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1", transcript: []string{sent}}, resumed: restartRun{events: []map[string]any{startEv("resume")}, session: "s1", transcript: []string{sent}}}, "ok ok no no", "did not reach the resumed session", ""},
		{"old and new call, same file", &restartProbe{stage: resumeCompleted, callsBefore: before, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1", transcript: []string{sent, `{"type":"user"}`, sentAgain}}, resumed: restartRun{events: []map[string]any{startEv("resume")}, session: "s1", transcript: []string{sent, `{"type":"user"}`, sentAgain}}}, "ok ok no no", "SendMessage was called but the agent never ran", ""},
		{"call after the resume in the first file", &restartProbe{stage: resumeCompleted, callsBefore: before, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1", transcript: []string{sent, `{"type":"user"}`, sentAgain}}, resumed: restartRun{events: []map[string]any{startEv("resume")}, session: "s1"}}, "ok ok no no", "SendMessage was called but the agent never ran", ""},
		{"call to the right id that errored", probe(nil, sent, failedResult), "ok ok no no", "SendMessage was called but the agent never ran", ""},
		{"sent, never ran", probe(nil, sent), "ok ok no no", "SendMessage was called but the agent never ran", ""},
		{"no transcript", probe(nil), "ok ok no no", "did not reach the resumed session", ""},
		{"ran under another id", probe([]map[string]any{bashEv("a2")}), "ok ok no no", "\"none\" seen again", ""},
		{"ran, no stop", probe([]map[string]any{bashEv("a1")}), "ok ok ok no?", "", "the agent ran in the resumed session but its stop was not seen"},
		{"stopped, no bash", probe([]map[string]any{stopped}), "ok ok ok ok", "\"a1\" seen again", ""},
		{"kept everything", probe([]map[string]any{bashEv("a1"), stopped}), "ok ok ok ok", "", ""},
		{"no first agent", &restartProbe{first: restartRun{events: []map[string]any{startEv("startup")}}}, "no? no? no? no?", "", "the resumed session never started"},
		{"first session id empty", &restartProbe{first: restartRun{events: []map[string]any{startEv("startup"), stopped}}}, "no? no? no? no?", "messaged the agent: unknown", "no session_id, so the resumed session never started"},
		{"first session failed, first shows the call", &restartProbe{stage: firstFailed, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1", transcript: []string{sent}}}, "no? no? no? no?", "messaged the agent: true", "the first session failed, so the resumed session never started"},
		{"never started, first shows the call", &restartProbe{first: restartRun{events: []map[string]any{startEv("startup"), stopped}, transcript: []string{sent}}}, "no? no? no? no?", "messaged the agent: true", "no session_id, so the resumed session never started"},
		{"first session failed", &restartProbe{stage: firstFailed}, "no? no? no? no?", "messaged the agent: unknown", "the first session failed, so the resumed session never started"},
		{"resumed failed", &restartProbe{stage: resumeFailed, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1"}}, "no no no? no?", "messaged the agent: unknown", "the resumed session failed, so nothing is known about the agent"},
		{"resumed failed, first shows the call", &restartProbe{stage: resumeFailed, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1", transcript: []string{sent}}}, "no no no? no?", "messaged the agent: true", "resumed session failed"},
		{"new id, resumed transcript unreadable", &restartProbe{stage: resumeCompleted, first: restartRun{events: []map[string]any{startEv("startup"), stopped}, session: "s1", transcript: []string{`{"sessionId":"s1"}`}}, resumed: restartRun{events: []map[string]any{startEv("resume")}, session: "s2"}}, "ok no no no", "did not reach the resumed session", ""},
	}
	for _, tt := range tests {
		var b strings.Builder
		cs := restartChecks(tt.p, &b)
		if got := state(cs); got != tt.want || !strings.Contains(b.String(), tt.said) {
			t.Errorf("%s: got %q, want %q (%q)", tt.name, got, tt.want, b.String())
		}
		if got := cs[2].inconclusive + cs[3].inconclusive; tt.why == "" && got != "" || !strings.Contains(got, tt.why) {
			t.Errorf("%s: inconclusive reasons %q, want %q", tt.name, got, tt.why)
		}
		if strings.Contains(b.String(), "SendMessage was called but") != strings.Contains(tt.said, "SendMessage was called but") {
			t.Errorf("%s: the SendMessage line was printed wrongly: %q", tt.name, b.String())
		}
	}
	var b strings.Builder
	cs := restartChecks(probe(nil, sent), &b)
	if err := reportLines(&b, cs, selftestCheck{ok: true}, true, ""); err == nil {
		t.Errorf("a sent message with no agent run did not fail the selftest: %q", b.String())
	}
}

func TestSentMessage(t *testing.T) {
	call, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "SendMessage", "input": map[string]any{"to": "a1"}}}}})
	wrong, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "SendMessage", "input": map[string]any{"to": "a2"}}}}})
	none := []string{`{"type":"user"}`}
	yes := []string{string(call)}
	old, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "t0", "name": "SendMessage", "input": map[string]any{"to": "a1"}}}}})
	tests := []struct {
		name           string
		stage          restartStage
		first, resumed []string
		want           string
	}{
		{"resumed shows the call", resumeCompleted, none, yes, "true"},
		{"first shows the call, resumed unreadable", resumeCompleted, yes, nil, "true"},
		{"first shows the call, resumed failed", resumeFailed, yes, nil, "true"},
		{"resumed lines without the call", resumeCompleted, none, none, "false"},
		{"resumed lines without the call, first has none", resumeCompleted, nil, none, "false"},
		{"resumed unreadable, first without the call", resumeCompleted, none, nil, "unknown"},
		{"failed, no lines", resumeFailed, none, nil, "unknown"},
		{"failed, resumed lines without the call", resumeFailed, none, none, "unknown"},
		{"never started", resumeNotStarted, nil, nil, "unknown"},
		{"wrong recipient, resumed lines", resumeCompleted, none, []string{string(wrong)}, "false"},
		{"call made before the resume, in both files", resumeCompleted, []string{string(old)}, []string{string(old)}, "false"},
		{"wrong recipient, first lines", resumeCompleted, []string{string(wrong)}, nil, "unknown"},
	}
	for _, tt := range tests {
		if got := sentMessage(tt.stage, "a1", map[string]bool{"t0": true}, tt.first, tt.resumed); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

// clearHeldSignal forgets a signal a test noted, so that a later test doesn't see it as caught.
func clearHeldSignal() {
	heldSignals.Lock()
	heldSignals.sig = nil
	heldSignals.Unlock()
}

// isolateClaudeHome points the home and the Claude config dir at a temp folder, so no test can reach the real ~/.claude, and returns the config dir.
func isolateClaudeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return filepath.Join(home, ".claude")
}

func TestSelftestInterruptedStopsAtOnce(t *testing.T) {
	isolateClaudeHome(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	runs := filepath.Join(t.TempDir(), "runs")
	fakeClaude(t, `[ "$1" = --version ] && { echo "2.1.319 (Claude Code)"; exit 0; }`+"\necho run >> "+runs+"\nsleep 30\n")
	release := holdSignals()
	t.Cleanup(clearHeldSignal)
	defer release()
	go func() {
		for {
			time.Sleep(100 * time.Millisecond)
			if b, _ := os.ReadFile(runs); len(b) > 0 {
				p, _ := os.FindProcess(os.Getpid())
				p.Signal(os.Interrupt)
				return
			}
		}
	}()
	var out bytes.Buffer
	start := time.Now()
	err := runSelftest(&out)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want interrupted", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("took %s, the session should have been killed", time.Since(start))
	}
	if b, _ := os.ReadFile(runs); strings.Count(string(b), "run") != 1 {
		t.Errorf("a later probe started: %q", b)
	}
	if strings.Contains(out.String(), "harness checks") || strings.Contains(strings.Replace(out.String(), "  ok   Claude Code", "", 1), "ok  ") || strings.Contains(out.String(), "FAIL") {
		t.Errorf("no verdict should print: %q", out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "ensign-selftest-*")); len(left) > 0 {
		t.Errorf("temp folders left behind: %v", left)
	}
}

func TestSelftestSignalBetweenSessions(t *testing.T) {
	isolateClaudeHome(t)
	runs := filepath.Join(t.TempDir(), "runs")
	fakeClaude(t, `[ "$1" = --version ] && { echo "2.1.319 (Claude Code)"; exit 0; }`+"\necho run >> "+runs+"\n")
	release := holdSignals()
	t.Cleanup(clearHeldSignal)
	defer release()
	noteSignal(os.Interrupt)
	var out bytes.Buffer
	if err := runSelftest(&out); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want interrupted", err)
	}
	if b, _ := os.ReadFile(runs); len(b) > 0 {
		t.Errorf("a session started after the signal: %q", b)
	}
}

func TestProbeHostCloseRemovesEverything(t *testing.T) {
	realConfig := isolateClaudeHome(t)
	realRecord := filepath.Join(realConfig, "projects", "-tmp-supernova")
	if err := os.MkdirAll(realRecord, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := newProbeHost()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := h.newProbeDir("/opt/supernova/ensign", "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A record folder of any name, such as the hashed name Claude Code gives a project path over 200 characters.
	hashed := filepath.Join(h.root, "home", ".claude", "projects", strings.Repeat("-supernova", 8)+"-1a2b3c")
	if err := os.MkdirAll(hashed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hashed, "s1.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	h.close(&out)
	for _, path := range []string{h.root, dir, hashed} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s was left behind", path)
		}
	}
	if _, err := os.Stat(realRecord); err != nil {
		t.Errorf("the real config was touched: %v", err)
	}
	if out.Len() > 0 {
		t.Errorf("unexpected output %q", out.String())
	}
}

func TestSelftestIsolatesTheSessionsFromTheRealConfig(t *testing.T) {
	realConfig := isolateClaudeHome(t)
	realHome := filepath.Dir(realConfig)
	if err := os.MkdirAll(realConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realConfig, ".credentials.json"), []byte("supernova-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("ANTHROPIC_API_KEY", "sk-supernova-real")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "supernova-token")
	envLog := filepath.Join(t.TempDir(), "env")
	fakeClaude(t, `[ "$1" = --version ] && { echo "2.1.319 (Claude Code)"; exit 0; }`+
		"\nenv > "+envLog+"\nmkdir -p \"$CLAUDE_CONFIG_DIR/projects/$(echo \"$PWD\" | tr -c 'A-Za-z0-9\\n' -)\"\nexit 1\n")
	before := dirListing(t, realHome)
	var out bytes.Buffer
	if err := runSelftest(&out); err == nil {
		t.Fatal("want the fake session's failure")
	}
	if after := dirListing(t, realHome); after != before {
		t.Errorf("the real home changed:\n%s\nbecame\n%s", before, after)
	}
	if b, err := os.ReadFile(filepath.Join(realConfig, ".credentials.json")); err != nil || string(b) != "supernova-secret" {
		t.Errorf("the real credentials changed: %q %v", b, err)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	for _, bad := range []string{"sk-supernova-real", "supernova-token", realHome} {
		if strings.Contains(env, bad) {
			t.Errorf("the session saw %q:\n%s", bad, env)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "ensign-selftest-*")); len(left) > 0 {
		t.Errorf("temp folders left behind: %v", left)
	}
}

// dirListing is the paths and sizes under dir, one per line.
func dirListing(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			fmt.Fprintf(&sb, "%s %d\n", path, info.Size())
		}
		return nil
	})
	return sb.String()
}

func TestRunProbeSessionEndsLeftoverProcesses(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "beat")
	fakeClaude(t, `echo '{"result":"supernova","session_id":"s1"}'`+"\n(while :; do echo x >> "+heartbeat+"; sleep 0.1; done) >/dev/null 2>&1 &\nexit 0\n")
	if _, _, _, err := runProbeSession(nil, t.TempDir(), "supernova"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	before, _ := os.Stat(heartbeat)
	time.Sleep(600 * time.Millisecond)
	after, _ := os.Stat(heartbeat)
	if before != nil && after != nil && before.Size() != after.Size() {
		t.Errorf("the background process is still writing: %d then %d", before.Size(), after.Size())
	}
}
