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
description: Selftest probe that spawns two agents.
tools: Agent
model: haiku
---
You are a harness selftest. Do exactly these steps, then stop:
1. Use the Agent tool with subagent_type "probe-leaf", run_in_background set to true, and
   prompt "Reply with the word ` + leafToken + `."
2. Use the Agent tool with subagent_type "probe-denied" and prompt "Reply with the word done."
3. Reply with one line saying what happened to each.
`

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
	dir, err := os.MkdirTemp("", "ensign-selftest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	hook := func(ev string) string {
		return fmt.Sprintf(`[{"hooks": [{"type": "command", "command": %q}]}]`, self+" selftest-hook "+ev)
	}
	settings := `{"hooks": {
  "SessionStart": ` + hook("SessionStart") + `,
  "PreToolUse": [{"matcher": "Agent|Task", "hooks": [{"type": "command", "command": ` + fmt.Sprintf("%q", self+" selftest-hook PreToolUse") + `}]}],
  "SubagentStop": ` + hook("SubagentStop") + `
}}`
	files := map[string]string{
		".claude/settings.json":          settings,
		".claude/agents/probe-orch.md":   selftestOrch,
		".claude/agents/probe-leaf.md":   fmt.Sprintf(selftestLeaf, "probe-leaf"),
		".claude/agents/probe-denied.md": fmt.Sprintf(selftestLeaf, "probe-denied"),
	}
	for rel, content := range files {
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(rel)), content); err != nil {
			return err
		}
	}
	git(dir, "init", "-q")
	token := fmt.Sprintf("%x", time.Now().UnixNano())
	if err := writeFile(filepath.Join(dir, "selftest-token"), token); err != nil {
		return err
	}
	fmt.Fprintln(out, "running a probe session with haiku (a few cents)…")
	cmd := exec.Command("claude", "-p", "--model", "haiku", "--allowedTools", "Agent", "--output-format", "json")
	cmd.Dir, cmd.Env = dir, cleanClaudeEnv()
	cmd.Stdin = strings.NewReader(`This is a self-test of this project's hooks, which the user asked for. ` +
		`Use the Agent tool with subagent_type "probe-orch" and prompt "run the selftest". One of its steps is refused by a hook on purpose; ` +
		`that is expected, not an error. When it finishes: this project's SessionStart hook added a block to your context at startup, ` +
		`beginning with the line ` + manualStart + `. Reply with only its lines that start with ` + manualEnd + `, exactly as written, in order.`)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("the probe session failed: %v: %s", err, lastLines(string(raw)+stderr.String(), 5))
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
	secondStopActive := len(orchStops) >= 2 && orchStops[1]["stop_hook_active"] == true
	askedBackground, foreground := backgroundProbe(events, orchStops)
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
		{"SubagentStop gives agent_type, agent_transcript_path, last_assistant_message",
			len(leafStops) > 0 && str(leafStops[0], "agent_transcript_path") != "" && leafStops[0]["last_assistant_message"] != nil},
		{"SubagentStop block makes the agent continue (stop_hook_active on the next stop)", secondStopActive},
		{"session_id is the same for the session and its agents", len(sessions) == 1},
		{fmt.Sprintf("SessionStart output of %d bytes reaches the session whole", manualLimit), manualArrived},
		{"SessionStart output stays out of subagents", len(leafStops) > 0 && !manualLeaked},
	}
	if askedBackground {
		checks = append(checks, struct {
			name string
			ok   bool
		}{"PreToolUse turns a background dispatch into a foreground one", foreground})
	} else {
		fmt.Fprintln(out, "  note the probe never asked for a background dispatch, so the foreground rewrite was not exercised")
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

// backgroundProbe reports whether the orchestrator probe asked to run its leaf in the
// background, and whether, after the hook's rewrite, it ran in the foreground anyway:
// the leaf's reply came back as the result of the Agent call, and the leaf finished before
// the orchestrator first stopped.
func backgroundProbe(events []map[string]any, orchStops []map[string]any) (asked, foreground bool) {
	leafStop, orchStop := -1, -1
	for i, e := range events {
		switch {
		case e["hook_event_name"] == "PreToolUse":
			ti, _ := e["tool_input"].(map[string]any)
			if ti["subagent_type"] == "probe-leaf" && ti["run_in_background"] == true {
				asked = true
			}
		case e["hook_event_name"] == "SubagentStop" && e["agent_type"] == "probe-leaf" && leafStop < 0:
			leafStop = i
		case e["hook_event_name"] == "SubagentStop" && e["agent_type"] == "probe-orch" && orchStop < 0:
			orchStop = i
		}
	}
	if !asked || len(orchStops) == 0 || leafStop < 0 || orchStop < leafStop {
		return asked, false
	}
	path, _ := orchStops[0]["agent_transcript_path"].(string)
	data, _ := os.ReadFile(path)
	return asked, toolResultContains(string(data), leafToken)
}

// toolResultContains reports whether any tool result in a transcript contains text.
func toolResultContains(transcript, text string) bool {
	for _, line := range strings.Split(transcript, "\n") {
		var rec struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		blocks, _ := rec.Message.Content.([]any)
		for _, b := range blocks {
			if m, _ := b.(map[string]any); m["type"] == "tool_result" {
				if raw, _ := json.Marshal(m["content"]); strings.Contains(string(raw), text) {
					return true
				}
			}
		}
	}
	return false
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
		if in["agent_type"] == "probe-orch" && in["stop_hook_active"] != true {
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
