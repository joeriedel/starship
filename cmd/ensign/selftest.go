// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// `ensign selftest` re-checks the Claude Code behaviour starship depends on, with a
// throwaway project and a cheap model. Harness behaviour changes between builds; run it
// after every Claude Code upgrade. It costs a few cents.

const selftestOrch = `---
name: probe-orch
description: Selftest probe that spawns five agents.
tools: Agent
model: haiku
---
You are a harness selftest. Do exactly these steps, then stop:
1. Use the Agent tool with subagent_type "probe-leaf", run_in_background set to true, and
   prompt "Reply with the word ` + leafToken + `."
2. Use the Agent tool with subagent_type "probe-denied" and prompt "Reply with the word done."
3. Use the Agent tool with subagent_type "probe-handback" and prompt "Hand back."
4. Use the Agent tool with subagent_type "probe-redo" and prompt "Hand back."
5. Use the Agent tool with subagent_type "probe-bg" and prompt "Start the background command."
6. Reply with one line saying what happened to each.
`

const selftestHandback = `---
name: probe-handback
description: Selftest probe that hands back a report.
tools: Read
model: haiku
---
Your only step is to call the SubagentHandback tool with the message ` + handbackToken + `.
`

const selftestRedo = `---
name: probe-redo
description: Selftest probe whose first handback is refused by a hook.
tools: Read
model: haiku
---
Your only step is to call the SubagentHandback tool with the message ` + redoFirst + `.
If the call is refused, do what the refusal says and call SubagentHandback again.
`

// The redo probe's first handback is denied by the hook; the denial asks for redoSecond.
const (
	redoFirst  = "SELFTEST-REDO-FIRST"
	redoSecond = "SELFTEST-REDO-SECOND"
)

const selftestBg = `---
name: probe-bg
description: Selftest probe that leaves a command running in the background.
tools: Bash
model: haiku
---
You are a harness selftest. Use the Bash tool once, with command "sleep 20" and run_in_background
set to true. When the tool returns, end your turn at once with the word started. Do not wait for it.
`

// handbackToken is the report the handback probe gives; PreToolUse must see it in the call's input.
const handbackToken = "SELFTEST-HANDBACK-REPORT"

const selftestLeaf = `---
name: %s
description: Selftest probe.
tools: Read
model: haiku
---
Reply with exactly what your prompt asks.
`

// The session manual reaches the session through SessionStart's output. The probe sends
// a block as large as the manual is allowed to be (manualLimit), between two markers.
const (
	manualStart = "SELFTEST-MANUAL-START"
	manualEnd   = "SELFTEST-MANUAL-END"
	manualLimit = 9000
)

// probeManual spreads ten marker lines evenly through the block, each carrying a token
// unique to this selftest, so only a model shown all of it can repeat every marker.
// (Long output is sometimes cut to its beginning and end.)
func probeManual(token string) string {
	const marks = 10
	filler := "This line stands in for the session manual, which only the session should see.\n"
	markLen := len(manualEnd) + len(token) + 8
	per := (manualLimit - len(manualStart) - 1 - marks*markLen) / marks / len(filler)
	var b strings.Builder
	b.WriteString(manualStart + "\n")
	for i := 1; i <= marks; i++ {
		b.WriteString(strings.Repeat(filler, per))
		fmt.Fprintf(&b, "%s %d-%s\n", manualEnd, i, token)
	}
	return b.String()
}

// sawWholeManual reports whether a reply repeats every marker of probeManual.
func sawWholeManual(reply, token string) bool {
	for i := 1; i <= 10; i++ {
		if !strings.Contains(reply, fmt.Sprintf("%d-%s", i, token)) {
			return false
		}
	}
	return true
}

// leafToken is the leaf probe's reply. Finding it in the result of the orchestrator's own
// Agent call proves the dispatch ran in the foreground.
const leafToken = "SELFTEST-LEAF-RESULT"

func cmdSelftest(out io.Writer) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("claude is not on PATH")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	token := fmt.Sprintf("%x", time.Now().UnixNano())
	fmt.Fprintln(out, "running a probe session with haiku (a few cents)…")
	events, result, err := runProbeSession(self, token, map[string]string{
		".claude/agents/probe-orch.md":     selftestOrch,
		".claude/agents/probe-leaf.md":     fmt.Sprintf(selftestLeaf, "probe-leaf"),
		".claude/agents/probe-denied.md":   fmt.Sprintf(selftestLeaf, "probe-denied"),
		".claude/agents/probe-handback.md": selftestHandback,
		".claude/agents/probe-redo.md":     selftestRedo,
		".claude/agents/probe-bg.md":       selftestBg,
	}, `This is a self-test of this project's hooks, which the user asked for. `+
		`Use the Agent tool with subagent_type "probe-orch" and prompt "run the selftest". One of its steps is refused by a hook on purpose; `+
		`that is expected, not an error. When it finishes: this project's SessionStart hook added a block to your context at startup, `+
		`beginning with the line `+manualStart+`. Reply with only its lines that start with `+manualEnd+`, exactly as written, in order.`,
		"--allowedTools", "Agent,Bash", "--permission-mode", "auto")
	if err != nil {
		return err
	}
	session := struct{ Result string }{result}

	// SubagentHandback ends every agent in auto mode, so the block has to be probed in a
	// session that is not in auto mode.
	fmt.Fprintln(out, "running a second probe session, outside auto mode…")
	stopEvents, _, err := runProbeSession(self, token, map[string]string{
		".claude/agents/probe-stop.md": fmt.Sprintf(selftestLeaf, "probe-stop"),
	}, `This is a self-test of this project's hooks, which the user asked for. `+
		`Use the Agent tool with subagent_type "probe-stop" and prompt "Reply with the word done." A hook asks the agent to continue once; that is expected. Then reply with the word finished.`,
		"--allowedTools", "Agent")
	if err != nil {
		return err
	}
	stopContinues, stopSawHandback := stopBlockProbe(stopEvents)

	str := func(e map[string]any, k string) string { s, _ := e[k].(string); return s }
	find := func(match func(map[string]any) bool) []map[string]any {
		var out []map[string]any
		for _, e := range events {
			if match(e) {
				out = append(out, e)
			}
		}
		return out
	}
	nested := find(func(e map[string]any) bool {
		return str(e, "hook_event_name") == "PreToolUse" && str(e, "agent_type") == "probe-orch"
	})
	leafStops := find(func(e map[string]any) bool {
		return str(e, "hook_event_name") == "SubagentStop" && str(e, "agent_type") == "probe-leaf"
	})
	deniedStops := find(func(e map[string]any) bool {
		return str(e, "hook_event_name") == "SubagentStop" && str(e, "agent_type") == "probe-denied"
	})
	orchStops := find(func(e map[string]any) bool {
		return str(e, "hook_event_name") == "SubagentStop" && str(e, "agent_type") == "probe-orch"
	})
	handbacks := find(func(e map[string]any) bool {
		ti, _ := e["tool_input"].(map[string]any)
		msg, _ := ti["message"].(string)
		return str(e, "hook_event_name") == "PreToolUse" && str(e, "tool_name") == "SubagentHandback" && strings.Contains(msg, handbackToken)
	})
	starts := find(func(e map[string]any) bool { return str(e, "hook_event_name") == "SessionStart" })

	injected := false
	if len(leafStops) > 0 {
		t, _ := os.ReadFile(str(leafStops[0], "agent_transcript_path"))
		injected = strings.Contains(string(t), "SELFTEST-INJECTED")
	}
	sessions := map[string]bool{}
	for _, e := range events {
		sessions[str(e, "session_id")] = true
	}
	askedBackground, foreground := backgroundProbe(events)
	// The transcript keeps the whole output even when the model is shown only part of
	// it, so the model itself has to repeat the markers.
	manualArrived, manualLeaked := sawWholeManual(session.Result, token), false
	for _, e := range append(leafStops, orchStops...) {
		t, _ := os.ReadFile(str(e, "agent_transcript_path"))
		manualLeaked = manualLeaked || strings.Contains(string(t), manualStart)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"SessionStart runs and gets CLAUDE_ENV_FILE", len(starts) > 0 && starts[0]["env_file"] == true},
		{"PreToolUse fires for spawns inside a subagent, with agent_id", len(nested) > 0 && str(nested[0], "agent_id") != ""},
		{"PreToolUse updatedInput reaches the spawned agent", injected},
		{"PreToolUse deny keeps an agent from starting", len(nested) >= 2 && len(deniedStops) == 0},
		{"SubagentStop gives agent_type and agent_transcript_path; the report is the handback or last_assistant_message", stopReportProbe(events)},
		{"PreToolUse deny of a SubagentHandback keeps the agent working, and a corrected handback gets through", handbackRedoProbe(events)},
		{"SubagentStop block makes the agent continue (stop_hook_active on the next stop)", stopContinues},
		{"session_id is the same for the session and its agents", len(sessions) == 1},
		{fmt.Sprintf("SessionStart output of %d bytes reaches the session whole", manualLimit), manualArrived},
		{"SessionStart output stays out of subagents", len(leafStops) > 0 && !manualLeaked},
		{"PreToolUse sees a subagent's SubagentHandback call, with its message", len(handbacks) > 0},
		{"SubagentStop background_tasks lists a background command, with the id PostToolUse saw", backgroundListed(events)},
	}
	if askedBackground {
		checks = append(checks, struct {
			name string
			ok   bool
		}{"PreToolUse turns a background dispatch into a foreground one", foreground})
	} else {
		fmt.Fprintln(out, "  note the probe never asked for a background dispatch, so the foreground rewrite was not exercised")
	}
	if stopSawHandback {
		fmt.Fprintln(out, "  note agents hand back outside auto mode too, so the SubagentStop block path cannot be checked")
	}
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.ok {
			mark, failed = "FAIL", failed+1
		}
		fmt.Fprintf(out, "  %s %s\n", mark, c.name)
	}
	if !manualArrived {
		fmt.Fprintf(out, "  the session's reply, which should list every marker:\n%s\n", truncate(session.Result, 1500))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d harness checks failed; starship's enforcement may not work on this Claude Code build", failed, len(checks))
	}
	fmt.Fprintln(out, "all harness checks passed")
	return nil
}

// runProbeSession runs one claude -p session in a fresh throwaway project holding the
// probe hooks and the given agent files, and returns the hook log and the session's reply.
func runProbeSession(self, token string, agents map[string]string, prompt string, claudeArgs ...string) ([]map[string]any, string, error) {
	dir, err := os.MkdirTemp("", "ensign-selftest-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	hook := func(ev string) string {
		return fmt.Sprintf(`[{"hooks": [{"type": "command", "command": %q}]}]`, self+" selftest-hook "+ev)
	}
	settings := `{"hooks": {
  "SessionStart": ` + hook("SessionStart") + `,
  "PreToolUse": [{"matcher": "Agent|Task|SubagentHandback", "hooks": [{"type": "command", "command": ` + fmt.Sprintf("%q", self+" selftest-hook PreToolUse") + `}]}],
  "PostToolUse": [{"matcher": "Bash|Agent|Task", "hooks": [{"type": "command", "command": ` + fmt.Sprintf("%q", self+" selftest-hook PostToolUse") + `}]}],
  "SubagentStop": ` + hook("SubagentStop") + `
}}`
	files := map[string]string{".claude/settings.json": settings}
	for rel, content := range agents {
		files[rel] = content
	}
	for rel, content := range files {
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(rel)), content); err != nil {
			return nil, "", err
		}
	}
	git(dir, "init", "-q")
	if err := writeFile(filepath.Join(dir, "selftest-token"), token); err != nil {
		return nil, "", err
	}
	cmd := exec.Command("claude", append([]string{"-p", "--model", "haiku", "--output-format", "json"}, claudeArgs...)...)
	cmd.Dir, cmd.Env = dir, cleanClaudeEnv()
	cmd.Stdin = strings.NewReader(prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("the probe session failed: %v: %s", err, lastLines(string(raw)+stderr.String(), 5))
	}
	var session struct {
		Result string `json:"result"`
	}
	json.Unmarshal(raw, &session)
	var events []map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, "selftest-log.jsonl"))
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil {
			events = append(events, e)
		}
	}
	return events, session.Result, nil
}

// backgroundListed reports whether a probe-bg SubagentStop input lists a background task
// whose id is the one PostToolUse reported for that agent's background Bash call.
func backgroundListed(events []map[string]any) bool {
	ids := map[string]bool{}
	for _, e := range events {
		if e["hook_event_name"] == "PostToolUse" && e["agent_type"] == "probe-bg" {
			resp, _ := e["tool_response"].(map[string]any)
			if id, _ := resp["backgroundTaskId"].(string); id != "" {
				ids[id] = true
			}
		}
	}
	for _, e := range events {
		if e["hook_event_name"] != "SubagentStop" || e["agent_type"] != "probe-bg" {
			continue
		}
		tasks, _ := e["background_tasks"].([]any)
		for _, t := range tasks {
			task, _ := t.(map[string]any)
			if id, _ := task["id"].(string); ids[id] {
				return true
			}
		}
	}
	return false
}

// eventStr returns a string field of an event, or "" when it is missing or not a string.
func eventStr(e map[string]any, k string) string { s, _ := e[k].(string); return s }

// isHandback reports whether e is a PreToolUse for a SubagentHandback call.
func isHandback(e map[string]any) bool {
	return eventStr(e, "hook_event_name") == "PreToolUse" && eventStr(e, "tool_name") == "SubagentHandback"
}

// handbackMessage returns the message of the last handback the agent attempted. An empty
// agentID never matches, so two events with no id are not taken for the same agent.
func handbackMessage(events []map[string]any, agentID string) (string, bool) {
	if agentID == "" {
		return "", false
	}
	msg, found := "", false
	for _, e := range events {
		if isHandback(e) && eventStr(e, "agent_id") == agentID {
			ti, _ := e["tool_input"].(map[string]any)
			msg, found = eventStr(ti, "message"), true
		}
	}
	return msg, found
}

// stopReportProbe checks that the first probe-leaf SubagentStop carries agent_type and
// agent_transcript_path, and that the leaf's report (its handback message, else the stop's
// last_assistant_message) holds the leaf token.
func stopReportProbe(events []map[string]any) bool {
	for _, e := range events {
		if eventStr(e, "hook_event_name") != "SubagentStop" || eventStr(e, "agent_type") != "probe-leaf" {
			continue
		}
		if eventStr(e, "agent_transcript_path") == "" {
			return false
		}
		report, ok := handbackMessage(events, eventStr(e, "agent_id"))
		if !ok {
			report = eventStr(e, "last_assistant_message")
		}
		return strings.Contains(report, leafToken)
	}
	return false
}

// handbackRedoProbe checks that one agent's handback carrying redoFirst was followed by a
// later handback carrying redoSecond, and then by that agent's SubagentStop.
func handbackRedoProbe(events []map[string]any) bool {
	msgOf := func(e map[string]any) string {
		ti, _ := e["tool_input"].(map[string]any)
		return eventStr(ti, "message")
	}
	for i, first := range events {
		id := eventStr(first, "agent_id")
		if !isHandback(first) || id == "" || !strings.Contains(msgOf(first), redoFirst) {
			continue
		}
		for j := i + 1; j < len(events); j++ {
			second := events[j]
			if !isHandback(second) || eventStr(second, "agent_id") != id || !strings.Contains(msgOf(second), redoSecond) {
				continue
			}
			for _, stop := range events[j+1:] {
				if eventStr(stop, "hook_event_name") == "SubagentStop" && eventStr(stop, "agent_type") == "probe-redo" && eventStr(stop, "agent_id") == id {
					return true
				}
			}
		}
	}
	return false
}

// stopBlockProbe reads the second session's log: continues is whether the second
// probe-stop SubagentStop has stop_hook_active and no agent handed back; sawHandback is
// whether any agent handed back, which makes the result meaningless.
func stopBlockProbe(events []map[string]any) (continues, sawHandback bool) {
	stops := 0
	for _, e := range events {
		if isHandback(e) {
			sawHandback = true
		}
		if eventStr(e, "hook_event_name") == "SubagentStop" && eventStr(e, "agent_type") == "probe-stop" {
			stops++
			if stops == 2 && e["stop_hook_active"] == true {
				continues = true
			}
		}
	}
	return continues && !sawHandback, sawHandback
}

// backgroundProbe reports whether the orchestrator probe asked to run its leaf in the
// background, and whether, after the hook's rewrite, it ran in the foreground anyway.
// A foreground Agent call returns only after the leaf finishes, so its PostToolUse, paired
// with the spawn by tool_use_id, comes after the leaf's SubagentStop; a background launch
// returns at once. The leaf's report (its handback message, else last_assistant_message)
// must hold leafToken.
func backgroundProbe(events []map[string]any) (asked, foreground bool) {
	leafStop, pre, post := -1, -1, -1
	for i, e := range events {
		ti, _ := e["tool_input"].(map[string]any)
		switch {
		case e["hook_event_name"] == "PreToolUse" && pre < 0:
			if ti["subagent_type"] == "probe-leaf" && ti["run_in_background"] == true {
				asked, pre = true, i
			}
		case e["hook_event_name"] == "SubagentStop" && e["agent_type"] == "probe-leaf" && leafStop < 0:
			leafStop = i
		}
	}
	if !asked || leafStop < 0 {
		return asked, false
	}
	id := eventStr(events[pre], "tool_use_id")
	for i, e := range events {
		if id != "" && e["hook_event_name"] == "PostToolUse" && eventStr(e, "tool_use_id") == id {
			post = i
			break
		}
	}
	if post < leafStop {
		return asked, false
	}
	report, ok := handbackMessage(events, eventStr(events[leafStop], "agent_id"))
	if !ok {
		report = eventStr(events[leafStop], "last_assistant_message")
	}
	return asked, strings.Contains(report, leafToken)
}

// cmdSelftestHook is the probe's hook: it logs every event and exercises each decision.
func cmdSelftestHook(event string, stdin io.Reader, stdout io.Writer) int {
	var in map[string]any
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return 0
	}
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		dir, _ = in["cwd"].(string)
	}
	if event == "SessionStart" {
		in["env_file"] = os.Getenv("CLAUDE_ENV_FILE") != ""
		token, _ := os.ReadFile(filepath.Join(dir, "selftest-token"))
		// Delivered the way ensign delivers the manual: as additionalContext.
		defer json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "SessionStart", "additionalContext": probeManual(string(token))}})
	}
	if line, err := json.Marshal(in); err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "selftest-log.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	reply := func(v any) int { json.NewEncoder(stdout).Encode(v); return 0 }
	switch event {
	case "PreToolUse":
		ti, _ := in["tool_input"].(map[string]any)
		if msg, _ := ti["message"].(string); in["tool_name"] == "SubagentHandback" && strings.Contains(msg, redoFirst) {
			return reply(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "deny",
				"permissionDecisionReason": "selftest deny: call SubagentHandback again with the message " + redoSecond}})
		}
		switch ti["subagent_type"] {
		case "probe-leaf":
			ti["prompt"] = fmt.Sprint(ti["prompt"]) + " SELFTEST-INJECTED"
			if bg, _ := ti["run_in_background"].(bool); bg {
				ti["run_in_background"] = false
			}
			return reply(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": ti}})
		case "probe-denied":
			return reply(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "selftest deny"}})
		}
	case "SubagentStop":
		if in["agent_type"] == "probe-stop" && in["stop_hook_active"] != true {
			return reply(map[string]any{"decision": "block", "reason": "selftest block: reply with one more line, then stop"})
		}
	}
	return 0
}

// cleanClaudeEnv is this process's environment without the variables Claude Code sets
// for its own commands. A claude started with them runs as a child of the calling
// session: it is told that session's project directory, and its prompt arrives with a
// "Claude" line in front, so `help` no longer matches.
// Only the variables a running session sets about itself are removed; configuration a
// user sets (a provider, a token, limits) is kept.
func cleanClaudeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !sessionVars[name] {
			env = append(env, kv)
		}
	}
	return env
}

var sessionVars = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_PROJECT_DIR": true, "CLAUDE_EFFORT": true,
	"CLAUDE_ENV_FILE": true, "CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ID": true,
	"CLAUDE_CODE_SESSION_ATTENDED": true, "CLAUDE_CODE_MESSAGING_SOCKET": true,
	"CLAUDE_CODE_MESSAGING_TOKEN": true, "CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_CODE_EXECPATH": true,
	"STARSHIP_SESSION": true,
}
