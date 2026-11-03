// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"strings"
	"testing"
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
