// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mockReply struct {
	Content    []map[string]any
	StopReason string
	StopSeq    any
}

// postMock sends one request to the stand-in and decodes the reply, whether it came as a stream or as JSON.
func postMock(t *testing.T, m *messagesMock, req map[string]any) mockReply {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(m.URL+"/v1/messages?beta=true", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var r mockReply
	if req["stream"] != true {
		var msg struct {
			Content    []map[string]any `json:"content"`
			StopReason string           `json:"stop_reason"`
			StopSeq    any              `json:"stop_sequence"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("not JSON: %v: %s", err, raw)
		}
		return mockReply{msg.Content, msg.StopReason, msg.StopSeq}
	}
	for _, block := range strings.Split(string(raw), "\n\n") {
		_, data, ok := strings.Cut(block, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
			Delta        map[string]any `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("bad event %q: %v", data, err)
		}
		switch ev.Type {
		case "content_block_start":
			r.Content = append(r.Content, ev.ContentBlock)
		case "content_block_delta":
			b := r.Content[ev.Index]
			if ev.Delta["type"] == "text_delta" {
				b["text"] = fmt.Sprint(b["text"]) + fmt.Sprint(ev.Delta["text"])
			} else {
				var in map[string]any
				json.Unmarshal([]byte(fmt.Sprint(ev.Delta["partial_json"])), &in)
				b["input"] = in
			}
		case "message_delta":
			r.StopReason, r.StopSeq = fmt.Sprint(ev.Delta["stop_reason"]), ev.Delta["stop_sequence"]
		}
	}
	return r
}

func newTestMock(t *testing.T) *messagesMock {
	t.Helper()
	m, err := startMessagesMock(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if !strings.HasPrefix(m.URL, "http://127.0.0.1:") {
		t.Fatalf("the stand-in should listen on the loopback address, got %s", m.URL)
	}
	return m
}

func userText(s string) map[string]any {
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": s}}}
}

func assistantCall(id, name string, input map[string]any) map[string]any {
	return map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}
}

func toolResult(id, text string) map[string]any {
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": text}}}
}

func tools(names ...string) []any {
	var out []any
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func TestMessagesMockScriptsASession(t *testing.T) {
	m := newTestMock(t)
	for _, stream := range []bool{true, false} {
		first := postMock(t, m, map[string]any{"model": "claude-supernova", "stream": stream, "tools": tools("Agent"),
			"messages": []any{userText("go\nselftest-script: main-stop")}})
		if first.StopReason != "tool_use" || len(first.Content) != 1 || first.Content[0]["name"] != "Agent" {
			t.Fatalf("stream %v: first turn = %+v", stream, first)
		}
		in, _ := first.Content[0]["input"].(map[string]any)
		if in["subagent_type"] != "probe-stop" || in["prompt"] == "" {
			t.Errorf("stream %v: Agent input = %v", stream, in)
		}
		id, _ := first.Content[0]["id"].(string)
		second := postMock(t, m, map[string]any{"model": "claude-supernova", "stream": stream, "tools": tools("Agent"),
			"messages": []any{userText("go\nselftest-script: main-stop"), assistantCall(id, "Agent", in), toolResult(id, "done")}})
		if second.StopReason != "end_turn" || len(second.Content) != 1 || second.Content[0]["text"] != "finished" {
			t.Errorf("stream %v: second turn = %+v", stream, second)
		}
	}
}

func TestMessagesMockFollowsTheSubagentMarkerInTheSystemPrompt(t *testing.T) {
	m := newTestMock(t)
	system := []any{map[string]any{"type": "text", "text": "selftest-script: probe-redo\nYour only step is to hand back."}}
	first := postMock(t, m, map[string]any{"stream": true, "system": system, "tools": tools("Read", "SubagentHandback"), "messages": []any{userText("Hand back.")}})
	in, _ := first.Content[0]["input"].(map[string]any)
	if first.Content[0]["name"] != "SubagentHandback" || in["message"] != redoFirst {
		t.Fatalf("first handback = %+v", first)
	}
	second := postMock(t, m, map[string]any{"stream": true, "system": system, "tools": tools("Read", "SubagentHandback"), "messages": []any{
		userText("Hand back."), assistantCall("t1", "SubagentHandback", in), toolResult("t1", "denied")}})
	in, _ = second.Content[0]["input"].(map[string]any)
	if in["message"] != redoSecond {
		t.Errorf("second handback = %+v", second)
	}
}

func TestMessagesMockServesAutoModesCheck(t *testing.T) {
	m := newTestMock(t)
	system := "Grade the action.\n## Output Format\n\nOutput <severity>N</severity> where N is an integer."
	stage1 := postMock(t, m, map[string]any{"stream": false, "system": system, "stop_sequences": []string{"</severity>"},
		"messages": []any{userText("go\nselftest-script: main-orch")}})
	if stage1.StopReason != "stop_sequence" || stage1.StopSeq != "</severity>" || stage1.Content[0]["text"] != "<severity>0" {
		t.Errorf("stage 1 = %+v", stage1)
	}
	stage2 := postMock(t, m, map[string]any{"stream": true, "system": system, "messages": []any{userText("go\nselftest-script: main-orch")}})
	if stage2.StopReason != "end_turn" || stage2.Content[0]["text"] != "<severity>0</severity>" {
		t.Errorf("stage 2 = %+v", stage2)
	}
}

func TestMessagesMockLoadsADeferredToolFirst(t *testing.T) {
	m := newTestMock(t)
	prompt := "send it. Use the SendMessage tool to send the agent with id a1b2c3 the message x\nselftest-script: main-resume"
	first := postMock(t, m, map[string]any{"stream": true, "tools": tools("Bash", "ToolSearch"), "messages": []any{userText(prompt)}})
	in, _ := first.Content[0]["input"].(map[string]any)
	if first.Content[0]["name"] != "ToolSearch" || in["query"] != "select:SendMessage" {
		t.Fatalf("first turn = %+v", first)
	}
	second := postMock(t, m, map[string]any{"stream": true, "tools": tools("Bash", "ToolSearch", "SendMessage"), "messages": []any{
		userText(prompt), assistantCall("t1", "ToolSearch", in), toolResult("t1", "loaded")}})
	in, _ = second.Content[0]["input"].(map[string]any)
	if second.Content[0]["name"] != "SendMessage" || in["to"] != "a1b2c3" {
		t.Errorf("second turn = %+v", second)
	}
}

func TestMessagesMockResumedSessionFollowsItsOwnPrompt(t *testing.T) {
	m := newTestMock(t)
	msgs := []any{userText("one\nselftest-script: main-restart"), assistantCall("t1", "Agent", map[string]any{}), toolResult("t1", "launched"),
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "waiting for the agent"}}},
		userText("two. Use the SendMessage tool to send the agent with id zz9 the message x\nselftest-script: main-resume")}
	got := postMock(t, m, map[string]any{"stream": true, "tools": tools("SendMessage"), "messages": msgs})
	in, _ := got.Content[0]["input"].(map[string]any)
	if got.Content[0]["name"] != "SendMessage" || in["to"] != "zz9" {
		t.Errorf("resumed turn = %+v", got)
	}
}

func TestMessagesMockHandsBackWhenAskedTo(t *testing.T) {
	m := newTestMock(t)
	system := []any{map[string]any{"type": "text", "text": "selftest-script: probe-leaf"}}
	got := postMock(t, m, map[string]any{"stream": true, "system": system, "tools": tools("Read", "SubagentHandback"), "messages": []any{
		userText("Reply with the word " + leafToken + "."),
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": leafToken}}},
		userText("[handback-send-enforce] Your report has not been delivered. Call SubagentHandback({message: <your full report>}) now."),
	}})
	in, _ := got.Content[0]["input"].(map[string]any)
	if got.Content[0]["name"] != "SubagentHandback" || in["message"] != leafToken {
		t.Errorf("handback = %+v", got)
	}
}

func TestMessagesMockAnswersOtherRequests(t *testing.T) {
	m := newTestMock(t)
	got := postMock(t, m, map[string]any{"stream": true, "messages": []any{userText("title this")}})
	if len(got.Content) != 1 || got.Content[0]["type"] != "text" {
		t.Errorf("unscripted request = %+v", got)
	}
	resp, err := http.Post(m.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "input_tokens") {
		t.Errorf("count_tokens = %s", b)
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("recorded %d messages requests, want 1", n)
	}
}

// Every marker the probes carry has a script.
func TestEveryProbeMarkerHasAScript(t *testing.T) {
	texts := []string{selftestOrch, selftestHandback, selftestRedo, selftestEnv, selftestBg, selftestRestart,
		fmt.Sprintf(selftestLeaf, "probe-leaf"), fmt.Sprintf(selftestLeaf, "probe-stop"), selftestMCPAgent, selftestMCPTry, selftestMCPOrch}
	for _, text := range texts {
		m := mockMarker.FindStringSubmatch(text)
		if m == nil || mockScripts[m[1]] == nil {
			t.Errorf("no script for the marker in %q (%v)", text[:40], m)
		}
	}
	for _, name := range []string{"main-orch", "main-stop", "main-restart", "main-resume", "main-stopbg", "main-live", "main-mcp"} {
		if mockScripts[name] == nil {
			t.Errorf("no script %s", name)
		}
	}
}

func TestLiveSettingsVerdict(t *testing.T) {
	post := func(cmd string) map[string]any {
		return map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}
	}
	live := func(cmd string) map[string]any {
		return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "live_settings": true, "tool_input": map[string]any{"command": cmd}}
	}
	calls := []map[string]any{post("echo live-before"), post("cp live-settings.json .claude/settings.local.json"), post("sleep 5"), post("echo live-after")}
	tests := []struct {
		name         string
		events       []map[string]any
		ok           bool
		inconclusive bool
	}{
		{"the new hook fired on the later call", append(append([]map[string]any{}, calls...), live("echo live-after")), true, false},
		{"the later call ran and the hook did not fire", calls, false, false},
		{"the hook fired only on a call during the wait", append(append([]map[string]any{}, calls...), live("sleep 5")), false, false},
		{"the hook was there from the start", append(append([]map[string]any{}, calls...), live("echo live-before"), live("echo live-after")), false, true},
		{"the later call never ran", calls[:3], false, true},
		{"the settings were never copied", []map[string]any{calls[0], calls[3]}, false, true},
		{"no events", nil, false, true},
	}
	for _, tt := range tests {
		ok, why := liveSettingsVerdict(tt.events)
		if ok != tt.ok || (why != "") != tt.inconclusive {
			t.Errorf("%s: ok %v, inconclusive %q", tt.name, ok, why)
		}
	}
}

func TestSelftestHookMarksTheLiveSettingsEvent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	var out bytes.Buffer
	in := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo supernova"}}`
	if code := cmdSelftestHook("LivePreToolUse", strings.NewReader(in), &out); code != 0 || out.Len() > 0 {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
	events := readProbeLog(dir)
	if len(events) != 1 || events[0]["live_settings"] != true {
		t.Errorf("log = %v", events)
	}
}

func TestLiveSettingsFileHooksBash(t *testing.T) {
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(liveSettingsFile("/opt/super nova/ensign")), &settings); err != nil {
		t.Fatal(err)
	}
	g := settings.Hooks["PreToolUse"]
	if len(g) != 1 || g[0].Matcher != "Bash" || g[0].Hooks[0].Command != `"/opt/super nova/ensign" selftest-hook LivePreToolUse` {
		t.Errorf("settings = %+v", settings)
	}
}
