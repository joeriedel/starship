// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// callMCP sends the requests to the stub, as lines on its stdin, and returns the replies in order. The stub exits when its stdin closes, so the whole exchange is one run.
func callMCP(t *testing.T, logPath string, requests ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if code := cmdSelftestMCP(logPath, strings.NewReader(strings.Join(requests, "\n")+"\n"), &out); code != 0 {
		t.Fatalf("stub exited %d", code)
	}
	var replies []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("reply is not JSON: %q", sc.Text())
		}
		replies = append(replies, m)
	}
	return replies
}

func TestSelftestMCPStub(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	text := "line \"one\"\nback\\slash café 日本語"
	args, _ := json.Marshal(map[string]any{"text": text})
	replies := callMCP(t, log,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":"four","method":"tools/call","params":{"name":"write_note","arguments":`+string(args)+`}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read_note","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"delete_note","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
	)
	if len(replies) != 7 {
		t.Fatalf("a notification must get no reply; want 7 replies, got %d", len(replies))
	}
	if r := replies[0]["result"].(map[string]any); r["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize should echo the client's protocol version: %v", r)
	}
	tools := replies[2]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "read_note" || tools[1].(map[string]any)["name"] != "write_note" {
		t.Errorf("want read_note and write_note: %v", tools)
	}
	if replies[3]["id"] != "four" {
		t.Errorf("the reply must carry the request's id: %v", replies[3]["id"])
	}
	if got := replies[3]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != fmt.Sprintf("stored %d bytes", len(text)) {
		t.Errorf("write_note result = %v", got)
	}
	if got := replies[4]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != mcpNote {
		t.Errorf("read_note result = %v", got)
	}
	for i, want := range map[int]float64{5: -32602, 6: -32601} {
		if e, _ := replies[i]["error"].(map[string]any); e == nil || e["code"] != want {
			t.Errorf("reply %d: want error %v, got %v", i, want, replies[i])
		}
	}
	calls := readJSONLines(log)
	if len(calls) != 2 {
		t.Fatalf("only the two known tools/call requests are logged, got %d", len(calls))
	}
	if calls[0]["name"] != "write_note" || mcpText(calls[0]) != text {
		t.Errorf("the log must hold the arguments exactly as received: %v", calls[0])
	}
	if calls[1]["name"] != "read_note" {
		t.Errorf("second call = %v", calls[1])
	}
}

func TestSelftestMCPStubExitsOnEOF(t *testing.T) {
	log := filepath.Join(t.TempDir(), "none.jsonl")
	if len(callMCP(t, log)) != 0 {
		t.Error("an empty stdin gets no reply")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("nothing is logged without a tools/call")
	}
}

func mcpEv(tool, id, typ, text string) map[string]any {
	e := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__stub__" + tool, "tool_input": map[string]any{}}
	if id != "" {
		e["agent_id"], e["agent_type"] = id, typ
	}
	if text != "" {
		e["tool_input"] = map[string]any{"text": text}
	}
	return e
}

func mcpCall(tool, text string) map[string]any {
	return map[string]any{"name": tool, "arguments": map[string]any{"text": text}}
}

func TestMCPIdentityProbe(t *testing.T) {
	good := []map[string]any{
		mcpEv("write_note", "", "", mcpAfterMark),
		mcpEv("write_note", "a1", "probe-mcp-open", "x"),
		spawnEv("probe-mcp-orch", "probe-mcp-nested", false),
		mcpEv("write_note", "n1", "probe-mcp-nested", "x"),
	}
	good[2]["agent_id"] = "o1"
	if !mcpIdentityProbe(good) {
		t.Error("session, agent and nested agent with their own identity should pass")
	}
	for name, mutate := range map[string]func([]map[string]any){
		"session call carries an id":         func(e []map[string]any) { e[0]["agent_id"] = "s" },
		"agent has no id":                    func(e []map[string]any) { delete(e[1], "agent_id") },
		"nested reports its parent":          func(e []map[string]any) { e[3]["agent_type"] = "probe-mcp-orch" },
		"nested shares the parent's id":      func(e []map[string]any) { e[3]["agent_id"] = "o1" },
		"no orchestrator event":              func(e []map[string]any) { e[2]["agent_type"] = "other" },
		"session call is not the marked one": func(e []map[string]any) { e[0]["tool_input"] = map[string]any{"text": "short"} },
	} {
		events := make([]map[string]any, len(good))
		for i, e := range good {
			c := map[string]any{}
			for k, v := range e {
				c[k] = v
			}
			events[i] = c
		}
		mutate(events)
		if mcpIdentityProbe(events) {
			t.Errorf("%s: should fail", name)
		}
	}
}

func TestMCPDenyProbe(t *testing.T) {
	events := []map[string]any{
		mcpEv("write_note", "", "", mcpDenyMark+" session"),
		mcpEv("write_note", "a1", "probe-mcp-open", mcpDenyMark+" sub"),
	}
	calls := []map[string]any{mcpCall("write_note", mcpAfterMark)}
	if !mcpDenyProbe(events, calls) {
		t.Error("denied calls that never reached the stub should pass")
	}
	if mcpDenyProbe(events, append(calls, mcpCall("write_note", mcpDenyMark+" sub"))) {
		t.Error("a denied call that reached the stub should fail")
	}
	if mcpDenyProbe(events[:1], calls) || mcpDenyProbe(events[1:], calls) {
		t.Error("both the session and the agent must have made a denied call")
	}
	if mcpDenyProbe(events, nil) {
		t.Error("with no allowed write reaching the stub, the stub may simply be broken")
	}
}

func TestMCPRoleProbe(t *testing.T) {
	good := []map[string]any{
		mcpEv("read_note", "r1", "probe-mcp-read", ""),
		stopEv("probe-mcp-read", "r1", nil),
		stopEv("probe-mcp-none", "n1", nil),
	}
	reached := []map[string]any{{"name": "read_note"}}
	if !mcpRoleProbe(good, reached) {
		t.Error("read only for the read role and nothing for the other should pass")
	}
	for name, events := range map[string][]map[string]any{
		"read role can write": append([]map[string]any{mcpEv("write_note", "r1", "probe-mcp-read", mcpRoleMark)}, good...),
		"none role can read":  append([]map[string]any{mcpEv("read_note", "n1", "probe-mcp-none", "")}, good...),
		"none role can write": append([]map[string]any{mcpEv("write_note", "n1", "probe-mcp-none", "")}, good...),
		"none role not run":   good[:2],
		"read role not run":   {good[0], good[2]},
		"read only by open":   {mcpEv("read_note", "o1", "probe-mcp-open", ""), good[1], good[2]},
	} {
		if mcpRoleProbe(events, reached) {
			t.Errorf("%s: should fail", name)
		}
	}
	if mcpRoleProbe(good, nil) {
		t.Error("no read reaching the stub should fail")
	}
	if mcpRoleProbe(good, append(reached, mcpCall("write_note", mcpRoleMark))) {
		t.Error("a role write that reached the stub should fail")
	}
}

func TestMCPChecksEmptyRun(t *testing.T) {
	checks := mcpChecks(mcpRun{})
	if len(checks) != 3 {
		t.Fatalf("want 3 connector checks, got %d", len(checks))
	}
	for _, c := range checks {
		if c.ok {
			t.Errorf("an empty run should fail %q", c.name)
		}
	}
}

func TestNewMCPProbeDir(t *testing.T) {
	self := `C:\Program Files\ensign\ensign.exe`
	dir, config, err := newMCPProbeDir(self)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("mcp config is not JSON: %v", err)
	}
	stub := cfg.MCPServers["stub"]
	if stub.Command != self || len(stub.Args) != 2 || stub.Args[0] != "selftest-mcp" || filepath.Dir(stub.Args[1]) != dir {
		t.Errorf("stub server = %+v", stub)
	}
	settings, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil || !json.Valid(settings) || !strings.Contains(string(settings), "mcp__.*") {
		t.Errorf("settings: %v %s", err, settings)
	}
	if want := strconv.Quote(filepath.ToSlash(self)) + " selftest-hook PreToolUse"; !strings.Contains(string(settings), strings.ReplaceAll(strings.ReplaceAll(want, `\`, `\\`), `"`, `\"`)) {
		t.Errorf("hook command should be the quoted slashed path, want %s in %s", want, settings)
	}
}

func TestSelftestHookDeniesMarkedWrite(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", t.TempDir())
	run := func(text string) string {
		in := fmt.Sprintf(`{"tool_name":"mcp__stub__write_note","tool_input":{"text":%q}}`, text)
		var out bytes.Buffer
		if code := cmdSelftestHook("PreToolUse", strings.NewReader(in), &out); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return out.String()
	}
	if got := run(mcpDenyMark + " x"); !strings.Contains(got, `"deny"`) {
		t.Errorf("a marked write should be denied, got %q", got)
	}
	if got := run(mcpAfterMark); strings.Contains(got, `"deny"`) {
		t.Errorf("an unmarked write should pass, got %q", got)
	}
}
