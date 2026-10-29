package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
1. Use the Agent tool with subagent_type "probe-leaf" and prompt "Reply with the word done."
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
	fmt.Fprintln(out, "running a probe session with haiku (a few cents)…")
	cmd := exec.Command("claude", "-p", "--model", "haiku", "--allowedTools", "Agent", "--output-format", "json")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(`Use the Agent tool with subagent_type "probe-orch" and prompt "run the selftest". When it finishes, stop.`)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the probe session failed: %v: %s", err, lastLines(string(b), 5))
	}

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
	}
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.ok {
			mark, failed = "FAIL", failed+1
		}
		fmt.Fprintf(out, "  %s %s\n", mark, c.name)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d harness checks failed; starship's enforcement may not work on this Claude Code build", failed, len(checks))
	}
	fmt.Fprintln(out, "all harness checks passed")
	return nil
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
