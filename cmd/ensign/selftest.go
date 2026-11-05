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
description: Selftest probe that spawns six agents.
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
6. Use the Agent tool with subagent_type "probe-env" and prompt "Run the command."
7. Reply with one line saying what happened to each.
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

// The env probe's setup file is written after SessionStart, by the hook for the Agent call that spawns it.
const envToken = "SELFTEST-ENV-TOKEN"

const selftestEnv = `---
name: probe-env
description: Selftest probe whose shell should load a setup file written after the session started.
tools: Bash
model: haiku
---
You are a harness selftest. Use the Bash tool once, with command "echo env:$STARSHIP_PROBE_ENV".
When the tool returns, end your turn at once with the word done.
`

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
	manualLimit = 8000
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

// manualInTranscript decides from the session transcript whether the SessionStart output reached the session whole. When it is too large Claude Code records only a preview of it in the hook_additional_context record.
func manualInTranscript(lines []string, token string) (ok bool, why string) {
	var content strings.Builder
	found := false
	for _, line := range lines {
		var rec struct {
			Attachment struct {
				Type      string   `json:"type"`
				HookEvent string   `json:"hookEvent"`
				Content   []string `json:"content"`
			} `json:"attachment"`
		}
		if json.Unmarshal([]byte(strings.TrimSuffix(line, "\r")), &rec) != nil {
			continue
		}
		if a := rec.Attachment; a.Type == "hook_additional_context" && a.HookEvent == "SessionStart" {
			found = true
			content.WriteString(strings.Join(a.Content, "\n"))
		}
	}
	text := content.String()
	switch {
	case !found:
		return false, "no SessionStart context was recorded, so it was dropped"
	case strings.Contains(text, "<persisted-output>"):
		return false, "Claude Code saved the output to a file and showed the session only a preview"
	case !sawWholeManual(text, token):
		n := 0
		for i := 1; i <= 10; i++ {
			if strings.Contains(text, fmt.Sprintf("%d-%s", i, token)) {
				n++
			}
		}
		return false, fmt.Sprintf("the output was cut: %d of 10 markers found", n)
	}
	return true, ""
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
	dir, err := newProbeDir(self, token, map[string]string{
		".claude/agents/probe-orch.md":     selftestOrch,
		".claude/agents/probe-leaf.md":     fmt.Sprintf(selftestLeaf, "probe-leaf"),
		".claude/agents/probe-denied.md":   fmt.Sprintf(selftestLeaf, "probe-denied"),
		".claude/agents/probe-handback.md": selftestHandback,
		".claude/agents/probe-redo.md":     selftestRedo,
		".claude/agents/probe-bg.md":       selftestBg,
		".claude/agents/probe-env.md":      selftestEnv,
	})
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	events, _, _, err := runProbeSession(dir, `This is a self-test of this project's hooks, which the user asked for. `+
		`Use the Agent tool with subagent_type "probe-orch" and prompt "run the selftest". One of its steps is refused by a hook on purpose; `+
		`that is expected, not an error.`,
		"--allowedTools", "Agent,Bash", "--permission-mode", "auto")
	if err != nil {
		return err
	}

	// SubagentHandback ends every agent in auto mode, so the block has to be probed in a
	// session that is not in auto mode.
	fmt.Fprintln(out, "running a second probe session, outside auto mode…")
	stopDir, err := newProbeDir(self, token, map[string]string{
		".claude/agents/probe-stop.md": fmt.Sprintf(selftestLeaf, "probe-stop"),
	})
	if err != nil {
		return err
	}
	defer os.RemoveAll(stopDir)
	stopEvents, _, _, err := runProbeSession(stopDir, `This is a self-test of this project's hooks, which the user asked for. `+
		`Use the Agent tool with subagent_type "probe-stop" and prompt "Reply with the word done." A hook asks the agent to continue once; that is expected. Then reply with the word finished.`,
		"--allowedTools", "Agent")
	if err != nil {
		return err
	}
	stopContinues, stopSawHandback := stopBlockProbe(stopEvents)

	fmt.Fprintln(out, "running a restart probe: a session, then the same session resumed…")
	restart, err := runRestartProbe(self, token)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "running a background-task probe for the Stop input…")
	stopBgEvents := runStopBackgroundProbe(self, token)

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
	// The transcript's hook_additional_context record holds what the session was shown, which is a preview when the output is too large. The model's own willingness to repeat the markers is not reliable.
	manualArrived, manualWhy := manualInTranscript(transcriptLines(events), token)
	manualLeaked := false
	for _, e := range append(leafStops, orchStops...) {
		t, _ := os.ReadFile(str(e, "agent_transcript_path"))
		manualLeaked = manualLeaked || strings.Contains(string(t), manualStart)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"SessionStart runs and gets CLAUDE_ENV_FILE", len(starts) > 0 && starts[0]["env_file"] == true},
		{"SessionStart gets CLAUDE_PID, which links a session to its id after /clear", len(starts) > 0 && starts[0]["claude_pid"] == true},
		{"PreToolUse fires for spawns inside a subagent, with agent_id", len(nested) > 0 && str(nested[0], "agent_id") != ""},
		{"PreToolUse updatedInput reaches the spawned agent", injected},
		{"PreToolUse deny keeps an agent from starting", len(nested) >= 2 && len(deniedStops) == 0},
		{"SubagentStop gives agent_type and agent_transcript_path; the report is the handback or last_assistant_message", stopReportProbe(events)},
		{"PreToolUse deny of a SubagentHandback keeps the agent working, and a corrected handback gets through", handbackRedoProbe(events)},
		{"SubagentStop block makes the agent continue (stop_hook_active on the next stop)", stopContinues},
		{"session_id is the same for the session and its agents", len(sessions) == 1},
		{fmt.Sprintf("SessionStart output of %d bytes reaches the session whole (from the transcript)", manualLimit), manualArrived},
		{"SessionStart output stays out of subagents", len(leafStops) > 0 && !manualLeaked},
		{"PreToolUse sees a subagent's SubagentHandback call, with its message", len(handbacks) > 0},
		{"SubagentStop background_tasks lists a background command, with the id PostToolUse saw", backgroundListed(events)},
		{"an agent's Bash loads a setup file written after SessionStart", envLoaded(events)},
	}
	checks = append(checks, restartChecks(restart, out)...)
	checks = append(checks, selftestCheck{"Stop background_tasks lists a background command, with the id PostToolUse saw", stopListsBackground(stopBgEvents)})
	fmt.Fprintln(out, "running a connector probe against a stub MCP server…")
	checks = append(checks, mcpChecks(runMCPProbe(self))...)
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
		fmt.Fprintf(out, "  the session transcript is the evidence for the SessionStart output check: %s\n", manualWhy)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d harness checks failed; starship's enforcement may not work on this Claude Code build", failed, len(checks))
	}
	fmt.Fprintln(out, "all harness checks passed")
	return nil
}

// newProbeDir makes a throwaway project holding the probe hooks and the given agent files, and returns its folder. The caller removes it once every session in it has finished.
func newProbeDir(self, token string, agents map[string]string) (string, error) {
	dir, err := os.MkdirTemp("", "ensign-selftest-")
	if err != nil {
		return "", err
	}
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
			os.RemoveAll(dir)
			return "", err
		}
	}
	git(dir, "init", "-q")
	if err := writeFile(filepath.Join(dir, "selftest-token"), token); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// readProbeLog reads the hook log of a probe folder.
func readProbeLog(dir string) []map[string]any {
	var events []map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, "selftest-log.jsonl"))
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil {
			events = append(events, e)
		}
	}
	return events
}

// runProbeSession runs one claude -p in a probe folder and returns the hook events it logged, the session's reply and its session id. Pass --resume in claudeArgs to continue an earlier session in the same folder.
func runProbeSession(dir, prompt string, claudeArgs ...string) ([]map[string]any, string, string, error) {
	before := len(readProbeLog(dir))
	cmd := exec.Command("claude", append([]string{"-p", "--model", "haiku", "--output-format", "json"}, claudeArgs...)...)
	cmd.Dir, cmd.Env = dir, cleanClaudeEnv()
	cmd.Stdin = strings.NewReader(prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("the probe session failed: %v: %s", err, lastLines(string(raw)+stderr.String(), 5))
	}
	result, sessionID, err := parseProbeSession(raw)
	if err != nil {
		return nil, "", "", err
	}
	return readProbeLog(dir)[before:], result, sessionID, nil
}

// parseProbeSession reads the result and session id from the JSON a probe session printed.
func parseProbeSession(raw []byte) (result, sessionID string, err error) {
	var session struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return "", "", fmt.Errorf("the probe session printed output that is not JSON: %v: %s", err, lastLines(string(raw), 5))
	}
	return session.Result, session.SessionID, nil
}

// envLoaded reports whether a probe-env Bash call saw the variable that the setup file, written after SessionStart, exports.
func envLoaded(events []map[string]any) bool {
	for _, e := range events {
		if e["hook_event_name"] == "PostToolUse" && e["agent_type"] == "probe-env" && strings.Contains(fmt.Sprint(e["tool_response"]), "env:"+envToken) {
			return true
		}
	}
	return false
}

// backgroundTaskIDs maps each non-empty tool_response.backgroundTaskId that PostToolUse reported for the given agent_type ("" for the main session) to the session it came from.
func backgroundTaskIDs(events []map[string]any, agentType string) map[string]string {
	ids := map[string]string{}
	for _, e := range events {
		if e["hook_event_name"] == "PostToolUse" && eventStr(e, "agent_type") == agentType {
			resp, _ := e["tool_response"].(map[string]any)
			if id, _ := resp["backgroundTaskId"].(string); id != "" {
				ids[id] = eventStr(e, "session_id")
			}
		}
	}
	return ids
}

// taskListed reports whether the event's background_tasks has an entry with an id in ids.
func taskListed(e map[string]any, ids map[string]string) (string, bool) {
	tasks, _ := e["background_tasks"].([]any)
	for _, t := range tasks {
		task, _ := t.(map[string]any)
		if id, _ := task["id"].(string); id != "" {
			if _, ok := ids[id]; ok {
				return id, true
			}
		}
	}
	return "", false
}

// backgroundListed reports whether a probe-bg SubagentStop input lists a background task
// whose id is the one PostToolUse reported for that agent's background Bash call.
func backgroundListed(events []map[string]any) bool {
	ids := backgroundTaskIDs(events, "probe-bg")
	for _, e := range events {
		if e["hook_event_name"] == "SubagentStop" && e["agent_type"] == "probe-bg" {
			if _, ok := taskListed(e, ids); ok {
				return true
			}
		}
	}
	return false
}

// stopListsBackground reports whether a Stop input lists the background task that PostToolUse reported for the main session's Bash call in the same session. A later Stop with an empty list does not undo it.
func stopListsBackground(events []map[string]any) bool {
	ids := backgroundTaskIDs(events, "")
	for _, e := range events {
		if e["hook_event_name"] == "Stop" {
			if id, ok := taskListed(e, ids); ok && ids[id] == eventStr(e, "session_id") {
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
		in["claude_pid"] = claudePID() != ""
		token, _ := os.ReadFile(filepath.Join(dir, "selftest-token"))
		if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" {
			if env, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				p := singleQuote(filepath.ToSlash(filepath.Join(dir, "probe-setup.sh")))
				fmt.Fprintf(env, "if [ -f %s ]; then . %s; fi\n", p, p)
				env.Close()
			}
		}
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
		if in["tool_name"] == "mcp__stub__write_note" && strings.Contains(eventStr(ti, "text"), mcpDenyMark) {
			return reply(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "deny",
				"permissionDecisionReason": "this write is refused on purpose by the selftest"}})
		}
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
		case "probe-env":
			os.WriteFile(filepath.Join(dir, "probe-setup.sh"), []byte("export STARSHIP_PROBE_ENV="+envToken+"\n"), 0o644)
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

const selftestRestart = `---
name: probe-restart
description: Selftest probe that makes a Bash call and can be messaged again later.
tools: Bash
model: haiku
---
You are a harness selftest. Use the Bash tool once, with the command the message asks for, then reply with the word done.
`

// restartRun is what one session of the restart probe left behind.
type restartRun struct {
	events     []map[string]any
	session    string
	transcript []string
}

type restartProbe struct{ first, resumed restartRun }

// runRestartProbe runs a session that starts a background agent, then resumes that session in the same folder and asks it to message the agent by id. When a step fails it returns what was observed so far, so restartChecks fails only the checks that needed the rest.
func runRestartProbe(self, token string) (*restartProbe, error) {
	dir, err := newProbeDir(self, token, map[string]string{".claude/agents/probe-restart.md": selftestRestart})
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var p restartProbe
	var result string
	p.first.events, result, p.first.session, err = runProbeSession(dir,
		`This is a self-test of this project's hooks, which the user asked for. `+
			`Use the Agent tool with subagent_type "probe-restart", run_in_background set to true, and prompt "Use Bash to run: echo restart-one". `+
			`When the agent has finished, reply with the word finished.`,
		"--allowedTools", "Agent,Bash", "--permission-mode", "auto")
	if err != nil {
		fmt.Fprintf(os.Stderr, "restart probe: the first session failed: %v\n", err)
		return &p, nil
	}
	agent := restartAgentID(p.first.events)
	if agent == "" {
		fmt.Fprintf(os.Stderr, "restart probe: the agent never stopped, so there is nothing to message: %s\n", truncate(result, 300))
		return &p, nil
	}
	if p.first.session == "" {
		fmt.Fprintln(os.Stderr, "restart probe: the first session reported no session_id")
		return &p, nil
	}
	p.resumed.events, _, p.resumed.session, err = runProbeSession(dir,
		`This is a self-test of this project's hooks, which the user asked for. `+
			`Use the SendMessage tool to send the agent with id `+agent+` the message "Use Bash to run: echo restart-two". `+
			`When the agent has finished, reply with the word finished.`,
		"--resume", p.first.session, "--allowedTools", "Agent,Bash,SendMessage", "--permission-mode", "auto")
	if err != nil {
		fmt.Fprintf(os.Stderr, "restart probe: the resumed session failed: %v\n", err)
	}
	p.resumed.transcript = transcriptLines(p.resumed.events)
	return &p, nil
}

// runStopBackgroundProbe runs a session whose main agent starts a background command and ends its turn, and returns the hook events. The Stop hook lives in settings.local.json so the other probe folders keep their settings. A failure is noted on stderr and returns no events, so the check always fails without aborting the others.
func runStopBackgroundProbe(self, token string) []map[string]any {
	stopHook := fmt.Sprintf(`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": %q}]}]}}`, self+" selftest-hook Stop")
	dir, err := newProbeDir(self, token, map[string]string{".claude/settings.local.json": stopHook})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop background probe: %v\n", err)
		return nil
	}
	defer os.RemoveAll(dir)
	events, _, _, err := runProbeSession(dir,
		`This is a self-test of this project's hooks, which the user asked for. `+
			`Use the Bash tool once, with command "sleep 30" and run_in_background set to true. Then reply with the word started at once. Do not wait for the command.`,
		"--allowedTools", "Bash", "--permission-mode", "auto")
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop background probe: the session failed: %v\n", err)
		return nil
	}
	return events
}

// transcriptLines reads the session transcript named by the first event that carries one.
func transcriptLines(events []map[string]any) []string {
	for _, e := range events {
		if path, _ := e["transcript_path"].(string); path != "" {
			data, _ := os.ReadFile(path)
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}
	}
	return nil
}

// restartAgentID is the id of the restart probe's agent, from its SubagentStop.
func restartAgentID(events []map[string]any) string {
	for _, e := range events {
		if e["hook_event_name"] == "SubagentStop" && e["agent_type"] == "probe-restart" {
			id, _ := e["agent_id"].(string)
			return id
		}
	}
	return ""
}

// startSource is the source of the first SessionStart event.
func startSource(events []map[string]any) string {
	for _, e := range events {
		if e["hook_event_name"] == "SessionStart" {
			src, _ := e["source"].(string)
			return src
		}
	}
	return ""
}

// sourcesReported reports whether SessionStart carries source: startup, then resume.
func sourcesReported(first, resumed []map[string]any) bool {
	return startSource(first) == "startup" && startSource(resumed) == "resume"
}

// restartTied reports whether a resumed session can be tied to the first: it has the same
// id, or its transcript holds records stamped with the first session's id.
func restartTied(firstID, resumedID string, resumedTranscript []string) bool {
	if firstID == "" || resumedID == "" {
		return false
	}
	if firstID == resumedID {
		return true
	}
	for _, line := range resumedTranscript {
		var rec struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.SessionID == firstID {
			return true
		}
	}
	return false
}

// agentRanAgain reports whether the agent the first session ended made a Bash call in the
// resumed session under the same agent_id.
func agentRanAgain(first, resumed []map[string]any) bool {
	id := restartAgentID(first)
	if id == "" {
		return false
	}
	for _, e := range resumed {
		if e["hook_event_name"] == "PostToolUse" && e["tool_name"] == "Bash" && e["agent_id"] == id {
			return true
		}
	}
	return false
}

// stopPath is the agent_transcript_path of the SubagentStop of the agent with this id.
func stopPath(events []map[string]any, id string) string {
	for _, e := range events {
		if e["hook_event_name"] == "SubagentStop" && e["agent_id"] == id {
			p, _ := e["agent_transcript_path"].(string)
			return p
		}
	}
	return ""
}

// agentPathKept reports whether the agent's transcript path is the same in both sessions.
func agentPathKept(first, resumed []map[string]any) bool {
	id := restartAgentID(first)
	p := stopPath(first, id)
	return id != "" && p != "" && stopPath(resumed, id) == p
}

type selftestCheck = struct {
	name string
	ok   bool
}

// restartChecks prints what the restart probe observed and returns its checks.
func restartChecks(p *restartProbe, out io.Writer) []selftestCheck {
	id := restartAgentID(p.first.events)
	fmt.Fprintf(out, "  restart probe: SessionStart source %q then %q\n", startSource(p.first.events), startSource(p.resumed.events))
	fmt.Fprintf(out, "  restart probe: session ids %q then %q\n", p.first.session, p.resumed.session)
	fmt.Fprintf(out, "  restart probe: agent id %q, then %q ran again; transcript path %q then %q\n",
		id, map[bool]string{true: id, false: "none"}[agentRanAgain(p.first.events, p.resumed.events)],
		stopPath(p.first.events, id), stopPath(p.resumed.events, id))
	return []selftestCheck{
		{"SessionStart carries source: startup, then resume", sourcesReported(p.first.events, p.resumed.events)},
		{"a resumed session keeps its id, or its transcript keeps records stamped with the first id", restartTied(p.first.session, p.resumed.session, p.resumed.transcript)},
		{"an agent resumed after a restart keeps its agent_id", agentRanAgain(p.first.events, p.resumed.events)},
		{"an agent's transcript path survives a restart", agentPathKept(p.first.events, p.resumed.events)},
	}
}
