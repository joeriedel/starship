// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import "testing"

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
