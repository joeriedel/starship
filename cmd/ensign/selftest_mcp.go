// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// mcpNote is what the stub's read_note returns.
const mcpNote = "SELFTEST-NOTE-CONTENT"

// cmdSelftestMCP is a stub MCP server for the connector probe: newline-delimited JSON-RPC on stdin and stdout, one read tool and one write tool. Every tools/call it receives is appended to logPath, with the arguments exactly as they arrived, so the probe can tell whether a call reached the server and with what.
func cmdSelftestMCP(logPath string, stdin io.Reader, stdout io.Writer) int {
	reader := bufio.NewReaderSize(stdin, 1<<20)
	enc := json.NewEncoder(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(line, &req) == nil && req.Method != "" {
				if len(req.ID) > 0 {
					enc.Encode(mcpReply(req.ID, req.Method, req.Params, logPath))
				}
			}
		}
		if err != nil {
			return 0
		}
	}
}

func mcpReply(id json.RawMessage, method string, params json.RawMessage, logPath string) map[string]any {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2024-11-05"
		}
		resp["result"] = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "stub", "version": "1"},
		}
	case "ping":
		resp["result"] = map[string]any{}
	case "tools/list":
		resp["result"] = map[string]any{"tools": []any{
			map[string]any{"name": "read_note", "description": "Read the note.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			map[string]any{"name": "write_note", "description": "Write the note. The text argument is stored exactly as given.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}},
		}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(params, &p) != nil || (p.Name != "read_note" && p.Name != "write_note") {
			resp["error"] = map[string]any{"code": -32602, "message": "unknown tool"}
			break
		}
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			line, _ := json.Marshal(map[string]any{"name": p.Name, "arguments": p.Arguments})
			f.Write(append(line, '\n'))
			f.Close()
		}
		text := mcpNote
		if p.Name == "write_note" {
			var a struct {
				Text string `json:"text"`
			}
			json.Unmarshal(p.Arguments, &a)
			text = fmt.Sprintf("stored %d bytes", len(a.Text))
		}
		resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "method not found: " + method}
	}
	return resp
}

// The connector probe: a session and some agents call the stub MCP server while hooks watch. The server is launched by claude from a config file the probe writes, and is isolated from any connector the account has. Whether a call reached the server is decided only from the stub's log, never from a hook event: PreToolUse fires before the permission decision.
const (
	mcpDenyMark  = "SELFTEST-MCP-DENYME"
	mcpAfterMark = "SELFTEST-MCP-AFTER"
	mcpRoleMark  = "SELFTEST-MCP-ROLE-WRITE"
)

const selftestMCPAgent = `---
name: %s
description: Selftest probe that writes a note through the stub MCP server.
model: haiku
---
Call the write_note tool of the stub MCP server once, with the text your prompt names. Then reply with the word done.
`

const selftestMCPTry = `---
name: %s
description: Selftest probe that tries both tools of the stub MCP server.
tools: %s
model: haiku
---
Try the read_note tool of the stub MCP server, then the write_note tool with the text ` + mcpRoleMark + `. Try both before you reply, even if one is unavailable; a tool you do not have is simply unavailable. Use no other means. Reply with what happened in one line.
`

const selftestMCPOrch = `---
name: probe-mcp-orch
description: Selftest probe that spawns an agent which calls the stub MCP server.
tools: Agent
model: haiku
---
Use the Agent tool once with subagent_type "probe-mcp-nested", run_in_background false and prompt "write the note SELFTEST-MCP-NESTED". Then reply with the word done.
`

// newMCPProbeDir makes a throwaway project whose settings hook every MCP call and spawn, and returns it with the path of the MCP config to hand to claude.
func newMCPProbeDir(self string) (dir, config string, err error) {
	dir, err = os.MkdirTemp("", "ensign-selftest-mcp-")
	if err != nil {
		return "", "", err
	}
	// The hook command runs through a shell, which mangles a backslashed or spaced Windows path.
	hookSelf := strconv.Quote(filepath.ToSlash(self))
	hook := func(matcher, ev string) []any {
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookSelf + " selftest-hook " + ev}}}
		if matcher != "" {
			group["matcher"] = matcher
		}
		return []any{group}
	}
	settings := map[string]any{"hooks": map[string]any{
		"PreToolUse":   hook("mcp__.*|Agent", "PreToolUse"),
		"SubagentStop": hook("", "SubagentStop"),
	}}
	config = filepath.Join(dir, "mcp.json")
	servers := map[string]any{"mcpServers": map[string]any{"stub": map[string]any{
		"command": self, "args": []string{"selftest-mcp", filepath.Join(dir, "mcp-stub.jsonl")}}}}
	files := map[string]any{
		filepath.Join(dir, ".claude", "settings.json"): settings,
		config: servers,
	}
	for path, v := range files {
		data, err := json.Marshal(v)
		if err == nil {
			err = writeFile(path, string(data))
		}
		if err != nil {
			os.RemoveAll(dir)
			return "", "", err
		}
	}
	agents := map[string]string{
		"probe-mcp-open":   fmt.Sprintf(selftestMCPAgent, "probe-mcp-open"),
		"probe-mcp-nested": fmt.Sprintf(selftestMCPAgent, "probe-mcp-nested"),
		"probe-mcp-read":   fmt.Sprintf(selftestMCPTry, "probe-mcp-read", "mcp__stub__read_note"),
		"probe-mcp-none":   fmt.Sprintf(selftestMCPTry, "probe-mcp-none", "Read"),
		"probe-mcp-orch":   selftestMCPOrch,
	}
	for name, content := range agents {
		if err := writeFile(filepath.Join(dir, ".claude", "agents", name+".md"), content); err != nil {
			os.RemoveAll(dir)
			return "", "", err
		}
	}
	git(dir, "init", "-q")
	return dir, config, nil
}

// mcpRun is what the connector probe left behind: the hook events and the calls the stub server received.
type mcpRun struct {
	events []map[string]any
	calls  []map[string]any
}

// runMCPProbe runs the connector probe session. A failure is noted on stderr and returns an empty run, so the checks fail without aborting the others.
func runMCPProbe(self string) mcpRun {
	dir, config, err := newMCPProbeDir(self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connector probe: %v\n", err)
		return mcpRun{}
	}
	defer os.RemoveAll(dir)
	prompt := `This is a self-test of this project's hooks, which the user asked for. Do these steps in order, each exactly once. Make one tool call per message and wait for its result before the next step: a later step depends on how an earlier one turned out. A hook refuses some calls on purpose; that is expected, not an error.
1. Call the stub MCP server's write_note tool with the text "` + mcpDenyMark + ` session".
2. Only after you have seen the result of step 1, call write_note with the text "` + mcpAfterMark + `".
3. Use the Agent tool with subagent_type "probe-mcp-open", run_in_background false and prompt "write the note ` + mcpDenyMark + ` sub".
4. Use the Agent tool with subagent_type "probe-mcp-read", run_in_background false and prompt "go".
5. Use the Agent tool with subagent_type "probe-mcp-none", run_in_background false and prompt "go".
6. Use the Agent tool with subagent_type "probe-mcp-orch", run_in_background false and prompt "go".
When all steps are done, reply with the word done.
`
	events, _, _, err := runProbeSession(dir, prompt, "--mcp-config", config, "--strict-mcp-config", "--allowedTools", "mcp__stub,Agent")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connector probe: %v\n", err)
		return mcpRun{}
	}
	return mcpRun{events: events, calls: readJSONLines(filepath.Join(dir, "mcp-stub.jsonl"))}
}

// readJSONLines reads a file of JSON objects, one per line.
func readJSONLines(path string) []map[string]any {
	var out []map[string]any
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// mcpText is the text argument of a hook event's tool_input or of a stub log entry.
func mcpText(m map[string]any) string {
	args, ok := m["tool_input"].(map[string]any)
	if !ok {
		args, _ = m["arguments"].(map[string]any)
	}
	return eventStr(args, "text")
}

// mcpHookCalls are the PreToolUse events for the stub server's tool, in order.
func mcpHookCalls(events []map[string]any, tool string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if eventStr(e, "hook_event_name") == "PreToolUse" && eventStr(e, "tool_name") == "mcp__stub__"+tool {
			out = append(out, e)
		}
	}
	return out
}

// mcpReached reports whether the stub received a call to tool whose text contains mark.
func mcpReached(calls []map[string]any, tool, mark string) bool {
	for _, c := range calls {
		if eventStr(c, "name") == tool && strings.Contains(mcpText(c), mark) {
			return true
		}
	}
	return false
}

// mcpIdentityProbe checks the identity PreToolUse reports for an MCP call: none for the session, and for an agent its own agent_id and agent_type, also when a different agent (the orchestrator) spawned it.
func mcpIdentityProbe(events []map[string]any) bool {
	session, sub, nested := false, false, false
	orchID := ""
	for _, e := range events {
		if eventStr(e, "tool_name") == "Agent" && eventStr(e, "agent_type") == "probe-mcp-orch" {
			orchID = eventStr(e, "agent_id")
		}
	}
	for _, e := range mcpHookCalls(events, "write_note") {
		id, typ := eventStr(e, "agent_id"), eventStr(e, "agent_type")
		session = session || (id == "" && typ == "" && strings.Contains(mcpText(e), mcpAfterMark))
		sub = sub || (id != "" && typ == "probe-mcp-open")
		nested = nested || (id != "" && typ == "probe-mcp-nested" && orchID != "" && id != orchID)
	}
	return session && sub && nested
}

// mcpDenyProbe checks that a denied write from the session and one from an agent were seen by the hook and never reached the stub, and that the stub did receive the allowed write after them.
func mcpDenyProbe(events, calls []map[string]any) bool {
	session, sub := false, false
	for _, e := range mcpHookCalls(events, "write_note") {
		if strings.Contains(mcpText(e), mcpDenyMark) {
			if eventStr(e, "agent_id") == "" {
				session = true
			} else {
				sub = true
			}
		}
	}
	return session && sub && !mcpReached(calls, "write_note", mcpDenyMark) && mcpReached(calls, "write_note", mcpAfterMark)
}

// mcpRoleProbe checks what a role's tools: list grants. The role naming only read_note must reach read_note and never write_note; the role naming no MCP tool must reach neither. Both must have run to their SubagentStop, the read role must have called read_note, and the stub must have received a read_note but never the role probe's write. Known gap: a role that never tried a withheld tool passes.
func mcpRoleProbe(events, calls []map[string]any) bool {
	stopped := map[string]bool{}
	readByRole := false
	for _, e := range events {
		typ := eventStr(e, "agent_type")
		switch {
		case eventStr(e, "hook_event_name") == "SubagentStop":
			stopped[typ] = true
		case strings.HasPrefix(eventStr(e, "tool_name"), "mcp__stub__") && typ == "probe-mcp-none":
			return false
		case eventStr(e, "tool_name") == "mcp__stub__write_note" && typ == "probe-mcp-read":
			return false
		}
		if eventStr(e, "hook_event_name") == "PreToolUse" && eventStr(e, "tool_name") == "mcp__stub__read_note" && typ == "probe-mcp-read" {
			readByRole = true
		}
	}
	return readByRole && mcpReached(calls, "read_note", "") && stopped["probe-mcp-read"] && stopped["probe-mcp-none"] && !mcpReached(calls, "write_note", mcpRoleMark)
}

// mcpChecks are the connector probe's checks.
func mcpChecks(r mcpRun) []selftestCheck {
	return []selftestCheck{
		{"PreToolUse (matcher mcp__.*) sees an MCP call as mcp__<server>__<tool>, with agent_id and agent_type empty for the session and the agent's own for an agent, also a nested one", mcpIdentityProbe(r.events)},
		{"PreToolUse deny keeps an MCP call from the server, for the session and for an agent", mcpDenyProbe(r.events, r.calls)},
		{"an agent's tools: list grants the MCP tools it names and no others", mcpRoleProbe(r.events, r.calls)},
	}
}
