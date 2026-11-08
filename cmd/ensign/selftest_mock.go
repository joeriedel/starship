// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
)

// The selftest talks to a local stand-in for the Messages API instead of a real model. Each probe session's conversation is a script: the stand-in reads the request, works out which script it belongs to and how far along it is, and answers with the next scripted turn. Nothing in the selftest then depends on what a model would choose to do, and no credential or provider is involved.

// mockMarker names the script a request belongs to. A subagent carries it in its system prompt (the agent file's body); a main session carries it in the prompt it was given.
var mockMarker = regexp.MustCompile(`selftest-script: ([a-z-]+)`)

// mockOut is one block of a scripted reply: text, or a tool call when Tool is set.
type mockOut struct {
	Text  string
	Tool  string
	Input map[string]any
}

type mockMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type mockRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Stop     []string        `json:"stop_sequences"`
	System   json.RawMessage `json:"system"`
	Messages []mockMessage   `json:"messages"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// blocks returns a message's content blocks; a plain string is one text block.
func (m mockMessage) blocks() []map[string]any {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []map[string]any{{"type": "text", "text": s}}
	}
	var bs []map[string]any
	json.Unmarshal(m.Content, &bs)
	return bs
}

// text is the text of a message: its text blocks, and for a tool result its content.
func (m mockMessage) text() string {
	var sb strings.Builder
	for _, b := range m.blocks() {
		switch b["type"] {
		case "text":
			sb.WriteString(fmt.Sprint(b["text"]))
		case "tool_result":
			if s, ok := b["content"].(string); ok {
				sb.WriteString(s)
			} else if parts, ok := b["content"].([]any); ok {
				for _, p := range parts {
					if pm, ok := p.(map[string]any); ok {
						sb.WriteString(fmt.Sprint(pm["text"]))
					}
				}
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (m mockMessage) isToolResult() bool {
	for _, b := range m.blocks() {
		if b["type"] == "tool_result" {
			return true
		}
	}
	return false
}

func (r *mockRequest) systemText() string {
	var s string
	if json.Unmarshal(r.System, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(r.System, &parts)
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text + "\n")
	}
	return sb.String()
}

func (r *mockRequest) hasTool(name string) bool {
	for _, t := range r.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// isClassifier reports whether the request is one of auto mode's own safety checks, which Claude Code sends as an ordinary request when the gateway does not review actions server-side (CLAUDE_CODE_AUTO_MODE_SERVER=0). It asks for a severity, and below 50 the action is allowed.
func (r *mockRequest) isClassifier() bool {
	return strings.Contains(r.systemText(), "<severity>N</severity>")
}

// script is the name of the script the request belongs to and the index of the last user message that names it (-1 when the system prompt does). The last such message wins, so a resumed session follows the prompt it was resumed with.
func (r *mockRequest) script() (string, int) {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != "user" || r.Messages[i].isToolResult() {
			continue
		}
		if m := mockMarker.FindStringSubmatch(r.Messages[i].text()); m != nil {
			return m[1], i
		}
	}
	if m := mockMarker.FindStringSubmatch(r.systemText()); m != nil {
		return m[1], -1
	}
	return "", -1
}

// turn is how many assistant messages the script has given since its prompt, not counting the tool searches that only loaded a tool.
func (r *mockRequest) turn() int {
	_, from := r.script()
	n := 0
	for _, m := range r.Messages[from+1:] {
		if m.Role != "assistant" {
			continue
		}
		bs := m.blocks()
		if len(bs) == 1 && bs[0]["type"] == "tool_use" && bs[0]["name"] == "ToolSearch" {
			continue
		}
		n++
	}
	return n
}

// lastAssistantText is the text of the last assistant message that has any.
func (r *mockRequest) lastAssistantText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "assistant" {
			if t := strings.TrimSpace(r.Messages[i].text()); t != "" {
				return t
			}
		}
	}
	return ""
}

// lastUserText is the text of the last message when it is a user's own (not a tool result), else "".
func (r *mockRequest) lastUserText() string {
	if len(r.Messages) == 0 {
		return ""
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role != "user" || last.isToolResult() {
		return ""
	}
	return last.text()
}

// promptText is the text of the message that carries the script's marker, the session's own prompt for a main session.
func (r *mockRequest) promptText() string {
	if _, i := r.script(); i >= 0 {
		return r.Messages[i].text()
	}
	return r.firstText()
}

func (r *mockRequest) firstText() string {
	if len(r.Messages) == 0 {
		return ""
	}
	return r.Messages[0].text()
}

// agentCall is the Agent tool call scripts make.
func agentCall(subagent, prompt string, background bool) mockOut {
	return mockOut{Tool: "Agent", Input: map[string]any{
		"description": subagent, "subagent_type": subagent, "prompt": prompt, "run_in_background": background}}
}

func bashCall(command string, background bool) mockOut {
	in := map[string]any{"command": command, "description": "selftest command"}
	if background {
		in["run_in_background"] = true
	}
	return mockOut{Tool: "Bash", Input: in}
}

func textOut(s string) mockOut { return mockOut{Text: s} }

func handbackCall(message string) mockOut {
	return mockOut{Tool: "SubagentHandback", Input: map[string]any{"message": message}}
}

func mcpWrite(text string) mockOut {
	return mockOut{Tool: "mcp__stub__write_note", Input: map[string]any{"text": text}}
}

// steps answers turn i with steps[i], and with a closing line after the last.
func steps(steps ...mockOut) func(*mockRequest) []mockOut {
	return func(r *mockRequest) []mockOut {
		if t := r.turn(); t < len(steps) {
			return []mockOut{steps[t]}
		}
		return []mockOut{textOut("done")}
	}
}

var writeNote = regexp.MustCompile(`write the note ([^\n]+)`)
var restartCommand = regexp.MustCompile(`run: (.+)`)
var agentIDInPrompt = regexp.MustCompile(`agent with id (\S+) the message`)
var promptWord = regexp.MustCompile(`Reply with the word ([A-Za-z0-9-]+)\.`)

// mockScripts holds every probe conversation, by the name its agent file or prompt carries.
var mockScripts = map[string]func(*mockRequest) []mockOut{
	"main-orch": steps(agentCall("probe-orch", "run the selftest", false)),
	"probe-orch": steps(
		agentCall("probe-leaf", "Reply with the word "+leafToken+".", true),
		agentCall("probe-denied", "Reply with the word done.", false),
		agentCall("probe-handback", "Hand back.", false),
		agentCall("probe-redo", "Hand back.", false),
		agentCall("probe-bg", "Start the background command.", false),
		agentCall("probe-env", "Run the command.", false),
		textOut("the leaf ran, the denied agent was refused, the others finished"),
	),
	"probe-leaf": func(r *mockRequest) []mockOut {
		if m := promptWord.FindStringSubmatch(r.firstText()); m != nil {
			return []mockOut{textOut(m[1])}
		}
		return []mockOut{textOut("done")}
	},
	"probe-stop": func(r *mockRequest) []mockOut {
		if r.turn() == 0 {
			return []mockOut{textOut("done")}
		}
		return []mockOut{textOut("one more line")}
	},
	"probe-handback": steps(handbackCall(handbackToken)),
	"probe-redo":     steps(handbackCall(redoFirst), handbackCall(redoSecond)),
	"probe-bg":       steps(bashCall("sleep 20", true), textOut("started")),
	"probe-env":      steps(bashCall("echo env:$STARSHIP_PROBE_ENV", false), textOut("done")),
	"probe-restart": func(r *mockRequest) []mockOut {
		if m := restartCommand.FindStringSubmatch(r.lastUserText()); m != nil {
			return []mockOut{bashCall(strings.TrimSpace(m[1]), false)}
		}
		return []mockOut{textOut("done")}
	},
	"main-restart": steps(agentCall("probe-restart", "Use Bash to run: echo restart-one", true), textOut("waiting for the agent")),
	"main-resume": func(r *mockRequest) []mockOut {
		if r.turn() == 0 {
			id := ""
			if m := agentIDInPrompt.FindStringSubmatch(r.promptText()); m != nil {
				id = m[1]
			}
			return []mockOut{{Tool: "SendMessage", Input: map[string]any{"to": id, "message": "Use Bash to run: echo restart-two", "summary": "run a command"}}}
		}
		if r.turn() == 1 {
			return []mockOut{textOut("waiting for the agent")}
		}
		return []mockOut{textOut("finished")}
	},
	// observe-bash is for testdata/observations/observe.py: one Bash call that prints the variable a SessionStart hook exported.
	"observe-bash": steps(bashCall(`echo "supernova-env=$SUPERNOVA_ENV"`, false), textOut("done")),
	"main-stop":    steps(agentCall("probe-stop", "Reply with the word done.", false), textOut("finished")),
	"main-stopbg":  steps(bashCall("sleep 30", true), textOut("started")),
	"main-live": steps(
		bashCall("echo live-before", false),
		bashCall("cp live-settings.json .claude/settings.local.json", false),
		bashCall("sleep 5", false),
		bashCall("echo live-after", false),
		textOut("done"),
	),
	"main-mcp": steps(
		mcpWrite(mcpDenyMark+" session"),
		mcpWrite(mcpAfterMark),
		agentCall("probe-mcp-open", "write the note "+mcpDenyMark+" sub", false),
		agentCall("probe-mcp-read", "go", false),
		agentCall("probe-mcp-none", "go", false),
		agentCall("probe-mcp-orch", "go", false),
		textOut("done"),
	),
	"probe-mcp-write": func(r *mockRequest) []mockOut {
		if m := writeNote.FindStringSubmatch(r.firstText()); m != nil && r.turn() == 0 {
			return []mockOut{mcpWrite(strings.TrimSpace(m[1]))}
		}
		return []mockOut{textOut("done")}
	},
	// The try agents call every stub tool they were given, in the order read then write.
	"probe-mcp-try": func(r *mockRequest) []mockOut {
		var calls []mockOut
		if r.hasTool("mcp__stub__read_note") {
			calls = append(calls, mockOut{Tool: "mcp__stub__read_note", Input: map[string]any{}})
		}
		if r.hasTool("mcp__stub__write_note") {
			calls = append(calls, mcpWrite(mcpRoleMark))
		}
		calls = append(calls, textOut("one of the tools was unavailable"))
		return steps(calls...)(r)
	},
	"probe-mcp-orch": steps(agentCall("probe-mcp-nested", "write the note SELFTEST-MCP-NESTED", false), textOut("done")),
}

// messagesMock is the stand-in Messages API.
type messagesMock struct {
	URL      string
	srv      *http.Server
	mu       sync.Mutex
	requests []mockRequestRecord
	log      io.Writer
}

// mockRequestRecord is a request the stand-in received, for tests and for the raw log.
type mockRequestRecord struct {
	Path   string          `json:"path"`
	Script string          `json:"script"`
	Body   json.RawMessage `json:"body"`
}

// startMessagesMock listens on a random port of 127.0.0.1. When log is not nil every request is written to it as a JSON line.
func startMessagesMock(log io.Writer) (*messagesMock, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &messagesMock{URL: "http://" + ln.Addr().String(), log: log}
	m.srv = &http.Server{Handler: http.HandlerFunc(m.handle)}
	go m.srv.Serve(ln)
	return m, nil
}

func (m *messagesMock) Close() { m.srv.Close() }

// Requests returns the requests received so far.
func (m *messagesMock) Requests() []mockRequestRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockRequestRecord(nil), m.requests...)
}

func (m *messagesMock) handle(w http.ResponseWriter, hr *http.Request) {
	body, _ := io.ReadAll(hr.Body)
	path := hr.URL.Path
	switch {
	case hr.Method == http.MethodPost && path == "/v1/messages":
		var req mockRequest
		if json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name, _ := req.script()
		m.mu.Lock()
		m.requests = append(m.requests, mockRequestRecord{Path: hr.URL.RequestURI(), Script: name, Body: body})
		if m.log != nil {
			line, _ := json.Marshal(m.requests[len(m.requests)-1])
			m.log.Write(append(line, '\n'))
		}
		id := len(m.requests)
		m.mu.Unlock()
		var out []mockOut
		switch script := mockScripts[name]; {
		case req.isClassifier():
			out = []mockOut{textOut("<severity>0</severity>")}
		case strings.HasPrefix(strings.TrimSpace(req.lastUserText()), "[handback-send-enforce]") && req.hasTool("SubagentHandback"):
			// In auto mode Claude Code ends an agent only through SubagentHandback and asks for it when the agent stops without.
			out = []mockOut{handbackCall(req.lastAssistantText())}
		case script != nil:
			out = script(&req)
			// A tool that is not in the request is deferred: it is loaded by a ToolSearch first, as a model would.
			if len(out) == 1 && out[0].Tool != "" && !req.hasTool(out[0].Tool) && req.hasTool("ToolSearch") {
				out = []mockOut{{Tool: "ToolSearch", Input: map[string]any{"query": "select:" + out[0].Tool, "max_results": 1}}}
			}
		}
		if len(out) == 0 {
			out = []mockOut{textOut("ok")}
		}
		writeMockMessage(w, req, id, out)
	case hr.Method == http.MethodPost && path == "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":1}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}
}

// writeMockMessage answers with a message holding out, as a stream when the request asked for one.
func writeMockMessage(w http.ResponseWriter, req mockRequest, id int, out []mockOut) {
	stop, stopSeq := "end_turn", any(nil)
	var content []map[string]any
	for i, o := range out {
		if o.Tool != "" {
			stop = "tool_use"
			content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_selftest_%d_%d", id, i), "name": o.Tool, "input": o.Input})
		} else {
			text := o.Text
			for _, seq := range req.Stop {
				if before, _, found := strings.Cut(text, seq); found {
					text, stop, stopSeq = before, "stop_sequence", seq
				}
			}
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	}
	msgID := fmt.Sprintf("msg_selftest_%d", id)
	usage := map[string]any{"input_tokens": 1, "output_tokens": 1}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": msgID, "type": "message", "role": "assistant", "model": req.Model,
			"content": content, "stop_reason": stop, "stop_sequence": stopSeq, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage}})
	for i, c := range content {
		start, delta := map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text_delta", "text": c["text"]}
		if c["type"] == "tool_use" {
			input, _ := json.Marshal(c["input"])
			start = map[string]any{"type": "tool_use", "id": c["id"], "name": c["name"], "input": map[string]any{}}
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(input)}
		}
		event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": start})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": stopSeq}, "usage": map[string]any{"output_tokens": 1}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

// cmdSelftestMock runs the stand-in Messages API until it is killed, for reproducing a probe or an observation by hand: it prints the URL to give claude as ANTHROPIC_BASE_URL and, with a path, logs every request there.
func cmdSelftestMock(logPath string, stdout io.Writer) int {
	var log io.Writer
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ensign:", err)
			return 1
		}
		defer f.Close()
		log = f
	}
	m, err := startMessagesMock(log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ensign:", err)
		return 1
	}
	fmt.Fprintln(stdout, m.URL)
	select {}
}
