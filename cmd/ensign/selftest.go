// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

var agentIDLine = regexp.MustCompile(`(?m)^agentId:[ \t]*(\S+)`)

// reportReachedDispatcher reports whether the report of a subagentType dispatch reaches the dispatcher: either the token is in the tool result of the Agent call, or that result names an agentId and a message delivered from that agent carries the token. A tool result's content is a string or a list of text blocks. Deliveries are matched after all lines are read, since one can precede the result.
func reportReachedDispatcher(lines []string, subagentType, token string) bool {
	calls := map[string]bool{}
	agentIDs := map[string]bool{}
	delivered := map[string]bool{}
	for _, line := range lines {
		var rec struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Origin     *deliveryOrigin `json:"origin"`
			Attachment struct {
				Type   string          `json:"type"`
				Prompt string          `json:"prompt"`
				Origin *deliveryOrigin `json:"origin"`
			} `json:"attachment"`
		}
		if json.Unmarshal([]byte(strings.TrimSuffix(line, "\r")), &rec) != nil {
			continue
		}
		if o := rec.Origin; o != nil && o.Kind == "peer" && o.From != "" {
			var text string
			json.Unmarshal(rec.Message.Content, &text)
			if strings.Contains(o.Report+"\n"+text, token) {
				delivered[o.From] = true
			}
		}
		if o := rec.Attachment.Origin; rec.Attachment.Type == "queued_command" && o != nil && o.Kind == "peer" && o.From != "" {
			text := o.Report
			if text == "" {
				text = rec.Attachment.Prompt
			}
			if strings.Contains(text, token) {
				delivered[o.From] = true
			}
		}
		var blocks []struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input struct {
				SubagentType string `json:"subagent_type"`
			} `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rec.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			switch {
			case b.Type == "tool_use" && (b.Name == "Agent" || b.Name == "Task") && b.Input.SubagentType == subagentType:
				calls[b.ID] = true
			case b.Type == "tool_result" && calls[b.ToolUseID]:
				var text string
				var parts []struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(b.Content, &text) != nil && json.Unmarshal(b.Content, &parts) == nil {
					for _, p := range parts {
						text += p.Text + "\n"
					}
				}
				if strings.Contains(text, token) {
					return true
				}
				if m := agentIDLine.FindStringSubmatch(strings.ReplaceAll(text, "\r", "")); m != nil {
					agentIDs[m[1]] = true
				}
			}
		}
	}
	for id := range agentIDs {
		if delivered[id] {
			return true
		}
	}
	return false
}

// deliveryOrigin is the origin of a message one agent delivers to another. Report is the exact report text of a hand-back.
type deliveryOrigin struct {
	Kind   string `json:"kind"`
	From   string `json:"from"`
	Report string `json:"handbackReportText"`
}

// reportLines prints the checks and the advisory check, and returns an error when a counted check failed. An inconclusive check is neither a pass nor a failure and is not counted. A failed advisory check says nothing about enforcement, which runs on hooks.
func reportLines(out io.Writer, checks []selftestCheck, advisory selftestCheck, manualArrived bool, manualWhy string) error {
	failed, inconclusive := 0, 0
	for _, c := range checks {
		switch {
		case c.inconclusive != "":
			inconclusive++
			fmt.Fprintf(out, "  inconclusive %s\n", c.name)
			fmt.Fprintf(out, "       %s, so the probe says nothing either way; run the selftest again\n", c.inconclusive)
		case c.ok:
			fmt.Fprintf(out, "  ok   %s\n", c.name)
		default:
			failed++
			fmt.Fprintf(out, "  FAIL %s\n", c.name)
		}
	}
	if !manualArrived {
		fmt.Fprintf(out, "  the session transcript is the evidence for the SessionStart output check: %s\n", manualWhy)
	}
	if advisory.ok {
		fmt.Fprintf(out, "  ok   %s\n", advisory.name)
	} else {
		fmt.Fprintf(out, "  warn %s\n", advisory.name)
		fmt.Fprintln(out, "       this does not affect enforcement; a lead on this Claude Code build may not see agents' reports, and hooks still record them in the run's reports folder, which `ensign next` names")
	}
	unclear := ""
	if inconclusive > 0 {
		unclear = fmt.Sprintf("%d inconclusive", inconclusive)
	}
	if failed > 0 {
		if unclear != "" {
			unclear = " (" + unclear + ")"
		}
		return fmt.Errorf("%d of %d harness checks failed%s; starship's enforcement may not work on this Claude Code build", failed, len(checks)-inconclusive, unclear)
	}
	switch {
	case inconclusive > 0 && advisory.ok:
		fmt.Fprintf(out, "no harness check failed, but %s, so not every check ran to a verdict; run the selftest again\n", unclear)
	case inconclusive > 0:
		fmt.Fprintf(out, "no harness check failed, but %s and one advisory check did not pass; run the selftest again\n", unclear)
	case advisory.ok:
		fmt.Fprintln(out, "all harness checks passed")
	default:
		fmt.Fprintln(out, "harness checks passed; one advisory check did not")
	}
	return nil
}

// leafToken is the leaf probe's reply. Finding it in the result of the orchestrator's own
// Agent call proves the dispatch ran in the foreground.
const leafToken = "SELFTEST-LEAF-RESULT"

// cmdSelftest holds signals while the probes run, so that an interrupt kills the running session and still lets runSelftest remove its folders and their session records; the signal is then raised again so that ensign ends by it. SIGKILL can't be cleaned up after.
func cmdSelftest(out io.Writer) error {
	release := holdSignals()
	err := runSelftest(out)
	release()
	reraiseHeldSignal()
	return err
}

// errSelftestInterrupted is returned when a signal stopped the selftest.
func errSelftestInterrupted() error {
	return fmt.Errorf("interrupted by %s", caughtSignal())
}

func runSelftest(out io.Writer) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("claude is not on PATH")
	}
	fmt.Fprint(out, claudeVersionLine(checkClaudeVersion()))
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
	defer removeProbeDir(out, dir)
	events, _, _, err := runProbeSession(dir, `This is a self-test of this project's hooks, which the user asked for. `+
		`Use the Agent tool with subagent_type "probe-orch" and prompt "run the selftest". One of its steps is refused by a hook on purpose; `+
		`that is expected, not an error.`,
		"--allowedTools", "Agent,Bash", "--permission-mode", "auto")
	if caughtSignal() != nil {
		return errSelftestInterrupted()
	}
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
	defer removeProbeDir(out, stopDir)
	if caughtSignal() != nil {
		return errSelftestInterrupted()
	}
	stopEvents, _, _, err := runProbeSession(stopDir, `This is a self-test of this project's hooks, which the user asked for. `+
		`Use the Agent tool with subagent_type "probe-stop" and prompt "Reply with the word done." A hook asks the agent to continue once; that is expected. Then reply with the word finished.`,
		"--allowedTools", "Agent")
	if caughtSignal() != nil {
		return errSelftestInterrupted()
	}
	if err != nil {
		return err
	}
	stopContinues, stopSawHandback := stopBlockProbe(stopEvents)

	fmt.Fprintln(out, "running a restart probe: a session, then the same session resumed…")
	restart, err := runRestartProbe(out, self, token)
	if caughtSignal() != nil {
		return errSelftestInterrupted()
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "running a background-task probe for the Stop input…")
	stopBgEvents := runStopBackgroundProbe(self, token)
	if caughtSignal() != nil {
		return errSelftestInterrupted()
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
	// The probe session's Agent call is for probe-orch; the orchestrator's own transcript holds the result of its Agent call for probe-handback, which is the dispatcher's view of a tools: Read agent's report.
	dispatcherLines := transcriptLines(events)
	for _, e := range orchStops {
		t, _ := os.ReadFile(str(e, "agent_transcript_path"))
		dispatcherLines = append(dispatcherLines, strings.Split(strings.TrimSpace(string(t)), "\n")...)
	}
	manualLeaked := false
	for _, e := range append(leafStops, orchStops...) {
		t, _ := os.ReadFile(str(e, "agent_transcript_path"))
		manualLeaked = manualLeaked || strings.Contains(string(t), manualStart)
	}
	checks := []selftestCheck{
		{"SessionStart runs and gets CLAUDE_ENV_FILE", len(starts) > 0 && starts[0]["env_file"] == true, ""},
		{"SessionStart gets CLAUDE_PID, which links a session to its id after /clear", len(starts) > 0 && starts[0]["claude_pid"] == true, ""},
		{"PreToolUse fires for spawns inside a subagent, with agent_id", len(nested) > 0 && str(nested[0], "agent_id") != "", ""},
		{"PreToolUse updatedInput reaches the spawned agent", injected, ""},
		{"PreToolUse deny keeps an agent from starting", len(nested) >= 2 && len(deniedStops) == 0, ""},
		{"SubagentStop gives agent_type and agent_transcript_path; the report is the handback or last_assistant_message", stopReportProbe(events), ""},
		{"PreToolUse deny of a SubagentHandback keeps the agent working, and a corrected handback gets through", handbackRedoProbe(events), ""},
		{"SubagentStop block makes the agent continue (stop_hook_active on the next stop)", stopContinues, ""},
		{"session_id is the same for the session and its agents", len(sessions) == 1, ""},
		{fmt.Sprintf("SessionStart output of %d bytes reaches the session whole (from the transcript)", manualLimit), manualArrived, ""},
		{"SessionStart output stays out of subagents", len(leafStops) > 0 && !manualLeaked, ""},
		{"PreToolUse sees a subagent's SubagentHandback call, with its message", len(handbacks) > 0, ""},
		{"SubagentStop background_tasks lists a background command, with the id PostToolUse saw", backgroundListed(events), ""},
		{"an agent's Bash loads a setup file written after SessionStart", envLoaded(events), ""},
	}
	checks = append(checks, restartChecks(restart, out)...)
	checks = append(checks, selftestCheck{"Stop background_tasks lists a background command, with the id PostToolUse saw", stopListsBackground(stopBgEvents), ""})
	fmt.Fprintln(out, "running a connector probe against a stub MCP server…")
	mcp := runMCPProbe(self)
	if caughtSignal() != nil {
		return errSelftestInterrupted()
	}
	checks = append(checks, mcpChecks(mcp)...)
	if askedBackground {
		checks = append(checks, selftestCheck{"PreToolUse turns a background dispatch into a foreground one", foreground, ""})
	} else {
		fmt.Fprintln(out, "  note the probe never asked for a background dispatch, so the foreground rewrite was not exercised")
	}
	if stopSawHandback {
		fmt.Fprintln(out, "  note agents hand back outside auto mode too, so the SubagentStop block path cannot be checked")
	}
	advisory := selftestCheck{"the dispatcher receives a tools: Read agent's report, as the Agent tool result or a delivered message", reportReachedDispatcher(dispatcherLines, "probe-handback", handbackToken), ""}
	return reportLines(out, checks, advisory, manualArrived, manualWhy)
}

// probeHookCommand is the command line of a probe hook for the given event. It runs through a shell, which splits or mangles a spaced or backslashed path, so the path is forward-slashed and double-quoted, escaping only backslash, double quote, dollar and backtick: the four characters a POSIX shell treats specially inside double quotes. Go's strconv.Quote would leave $ and backtick live and emit \x escapes no shell reads.
func probeHookCommand(self, ev string) string {
	return `"` + hookPathQuoter.Replace(filepath.ToSlash(self)) + `" selftest-hook ` + ev
}

var hookPathQuoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// probeHookGroups is the JSON array holding one hook group that runs command, for the matcher if it is not empty.
func probeHookGroups(matcher, command string) string {
	group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}}
	if matcher != "" {
		group["matcher"] = matcher
	}
	data, _ := json.Marshal([]any{group})
	return string(data)
}

// probeStopSettings is the settings.local.json body that hooks the Stop event.
func probeStopSettings(self string) string {
	data, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": probeHookCommand(self, "Stop")}}}}}})
	return string(data)
}

// newProbeDir makes a throwaway project holding the probe hooks and the given agent files, and returns its folder. The caller removes it once every session in it has finished.
func newProbeDir(self, token string, agents map[string]string) (string, error) {
	dir, err := os.MkdirTemp("", "ensign-selftest-")
	if err != nil {
		return "", err
	}
	hook := func(matcher, ev string) string {
		return probeHookGroups(matcher, probeHookCommand(self, ev))
	}
	settings := `{"hooks": {
  "SessionStart": ` + hook("", "SessionStart") + `,
  "PreToolUse": ` + hook("Agent|Task|SubagentHandback", "PreToolUse") + `,
  "PostToolUse": ` + hook("Bash|Agent|Task", "PostToolUse") + `,
  "SubagentStop": ` + hook("", "SubagentStop") + `
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

// removeProbeDir removes a probe folder and the folder where Claude Code recorded its sessions. The record lives under the config dir, outside the probe folder, so it would stay behind for every run. Only folders that carry this probe folder's own random name are touched. A removal that fails is noted on out and changes nothing else. The probe log is read first, since it is in the folder being removed.
func removeProbeDir(out io.Writer, dir string) {
	candidates := map[string]bool{}
	for _, e := range readProbeLog(dir) {
		if path, _ := e["transcript_path"].(string); path != "" {
			candidates[filepath.Dir(path)] = true
		}
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		if home, err := os.UserHomeDir(); err == nil {
			config = filepath.Join(home, ".claude")
		}
	}
	if config != "" {
		candidates[filepath.Join(config, "projects", claudeProjectName(dir))] = true
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			candidates[filepath.Join(config, "projects", claudeProjectName(real))] = true
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(out, "could not remove the probe folder %s: %v\n", dir, err)
	}
	own := claudeProjectName(filepath.Base(dir))
	for c := range candidates {
		if filepath.Base(filepath.Dir(c)) != "projects" || !strings.HasSuffix(filepath.Base(c), own) {
			continue
		}
		if err := os.RemoveAll(c); err != nil {
			fmt.Fprintf(out, "could not remove the session records in %s: %v\n", c, err)
		}
	}
}

// claudeProjectName is the name of the folder where Claude Code records the sessions of a project at path: every character that isn't a letter or digit becomes '-'.
func claudeProjectName(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, path)
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

// probeTimeout bounds one probe session; a variable so a test can shorten it.
var probeTimeout = 10 * time.Minute

// runProbeSession runs one claude -p in a probe folder and returns the hook events it logged, the session's reply and its session id. Pass --resume in claudeArgs to continue an earlier session in the same folder.
func runProbeSession(dir, prompt string, claudeArgs ...string) ([]map[string]any, string, string, error) {
	if sig := caughtSignal(); sig != nil {
		return nil, "", "", fmt.Errorf("interrupted by %s", sig)
	}
	before := len(readProbeLog(dir))
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", append([]string{"-p", "--model", "haiku", "--output-format", "json"}, claudeArgs...)...)
	cmd.Dir, cmd.Env = dir, cleanClaudeEnv()
	cmd.Stdin = strings.NewReader(prompt)
	started, finish := killTreeOnCancel(cmd, cancel)
	cmd.WaitDelay = killWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Start()
	if err == nil {
		started()
		err = cmd.Wait()
	}
	sigErr := finish()
	raw := stdout.Bytes()
	switch {
	case sigErr != nil:
		err = sigErr
	case ctx.Err() != nil:
		err = fmt.Errorf("the probe session timed out after %s: %s", probeTimeout.Round(time.Second), lastLines(string(raw)+stderr.String(), 5))
	case errors.Is(err, exec.ErrWaitDelay):
		// The session exited 0; a background child only held the output pipe open.
		err = nil
	}
	if err != nil {
		if ctx.Err() != nil && sigErr == nil {
			return nil, "", "", err
		}
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

// restartStage is how far the restart probe got. The zero value is a resume that never started; firstFailed is a resume that never started because the first session failed.
type restartStage int

const (
	resumeNotStarted restartStage = iota
	firstFailed
	resumeFailed
	resumeCompleted
)

type restartProbe struct {
	first, resumed restartRun
	stage          restartStage
	// callsBefore holds the ids of the SendMessage calls the first session's transcript had before the resume started. Those calls were not the resumed turn's, however the resume files its transcript. Nil skips nothing.
	callsBefore map[string]bool
}

// runRestartProbe runs a session that starts a background agent, then resumes that session in the same folder and asks it to message the agent by id. It records the ids of the SendMessage calls in the first transcript before the resume, because --resume appends to that file on the builds seen, or may copy its history into a new one, so a call seen afterwards is the resumed turn's only if its id is not on that list. Why it stopped early is written to out. When a step fails it returns what was observed so far and how far it got: restartChecks leaves a check inconclusive when the resume never started, and fails the session and source checks when the resumed session was attempted and failed, since `claude --resume` failing is itself evidence. SIGKILL leaves the folder and its session records behind.
func runRestartProbe(out io.Writer, self, token string) (*restartProbe, error) {
	dir, err := newProbeDir(self, token, map[string]string{".claude/agents/probe-restart.md": selftestRestart})
	if err != nil {
		return nil, err
	}
	defer removeProbeDir(out, dir)
	var p restartProbe
	var result string
	p.first.events, result, p.first.session, err = runProbeSession(dir,
		`This is a self-test of this project's hooks, which the user asked for. `+
			`Use the Agent tool with subagent_type "probe-restart", run_in_background set to true, and prompt "Use Bash to run: echo restart-one". `+
			`When the agent has finished, reply with the word finished.`,
		"--allowedTools", "Agent,Bash", "--permission-mode", "auto")
	if caughtSignal() != nil {
		return &p, nil
	}
	if err != nil {
		fmt.Fprintf(out, "restart probe: the first session failed: %v\n", err)
		p.stage = firstFailed
		return &p, nil
	}
	agent := restartAgentID(p.first.events)
	if agent == "" {
		fmt.Fprintf(out, "restart probe: the agent never stopped, so there is nothing to message: %s\n", truncate(result, 300))
		return &p, nil
	}
	if p.first.session == "" {
		fmt.Fprintln(out, "restart probe: the first session reported no session_id")
		return &p, nil
	}
	p.callsBefore = sendMessageIDs(transcriptLines(p.first.events))
	p.resumed.events, _, p.resumed.session, err = runProbeSession(dir,
		`This is a self-test of this project's hooks, which the user asked for. `+
			`Use the SendMessage tool to send the agent with id `+agent+` the message "Use Bash to run: echo restart-two". `+
			`When the agent has finished, reply with the word finished.`,
		"--resume", p.first.session, "--allowedTools", "Agent,Bash,SendMessage", "--permission-mode", "auto")
	if caughtSignal() != nil {
		return &p, nil
	}
	if err != nil {
		fmt.Fprintf(out, "restart probe: the resumed session failed: %v\n", err)
		p.stage = resumeFailed
	} else {
		p.stage = resumeCompleted
	}
	p.first.transcript = transcriptLines(p.first.events)
	p.resumed.transcript = transcriptLines(p.resumed.events)
	return &p, nil
}

// runStopBackgroundProbe runs a session whose main agent starts a background command and ends its turn, and returns the hook events. The Stop hook lives in settings.local.json so the other probe folders keep their settings. A failure is noted on stderr and returns no events, so the check always fails without aborting the others.
func runStopBackgroundProbe(self, token string) []map[string]any {
	dir, err := newProbeDir(self, token, map[string]string{".claude/settings.local.json": probeStopSettings(self)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop background probe: %v\n", err)
		return nil
	}
	defer removeProbeDir(os.Stderr, dir)
	events, _, _, err := runProbeSession(dir,
		`This is a self-test of this project's hooks, which the user asked for. `+
			`Use the Bash tool once, with command "sleep 30" and run_in_background set to true. Then reply with the word started at once. Do not wait for the command.`,
		"--allowedTools", "Bash", "--permission-mode", "auto")
	if err != nil && caughtSignal() == nil {
		fmt.Fprintf(os.Stderr, "stop background probe: the session failed: %v\n", err)
		return nil
	}
	return events
}

// transcriptLines reads the session transcript named by the first event that carries one. It returns nil when no event names one or the file can't be read.
func transcriptLines(events []map[string]any) []string {
	for _, e := range events {
		if path, _ := e["transcript_path"].(string); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}
	}
	return nil
}

// sentMessage says whether the resumed turn sent a message to the agent with the given id: "true", "false" or "unknown". A call counts when a SendMessage tool_use carries that id in its input and its own id is not in callsBefore, the calls made before the resume. On the builds seen, --resume appends to the first session's file, so firstLines can show the call when the resumed session logged no events. It can prove true but never false: false comes only from the resumed session's own lines in a session that completed. "true" with no agent run in the resumed session is a failure; the other answers leave that inconclusive.
func sentMessage(stage restartStage, agent string, callsBefore map[string]bool, firstLines, resumedLines []string) string {
	switch {
	case calledSendMessage(resumedLines, agent, callsBefore) || calledSendMessage(firstLines, agent, callsBefore):
		return "true"
	case stage == resumeCompleted && len(resumedLines) > 0:
		return "false"
	}
	return "unknown"
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

// selftestCheck is one check and its verdict. A check with a reason in inconclusive has ok false, and counts as neither a pass nor a failure.
type selftestCheck struct {
	name         string
	ok           bool
	inconclusive string
}

type verdict int

const (
	verdictFail verdict = iota
	verdictPass
	verdictInconclusive
)

func (v verdict) check(name, why string) selftestCheck {
	if v == verdictInconclusive {
		return selftestCheck{name, false, why}
	}
	return selftestCheck{name, v == verdictPass, ""}
}

// agentRanResumed reports whether any agent ran in the resumed session: a Bash call with an agent_id, or the stop of the restart probe's agent.
func agentRanResumed(resumed []map[string]any) bool {
	for _, e := range resumed {
		if id, _ := e["agent_id"].(string); e["hook_event_name"] == "PostToolUse" && e["tool_name"] == "Bash" && id != "" {
			return true
		}
		if e["hook_event_name"] == "SubagentStop" && e["agent_type"] == "probe-restart" {
			return true
		}
	}
	return false
}

// agentStoppedAgain reports whether the resumed session saw a SubagentStop for the agent with this id.
func agentStoppedAgain(resumed []map[string]any, id string) bool {
	for _, e := range resumed {
		if e["hook_event_name"] == "SubagentStop" && e["agent_id"] == id {
			return true
		}
	}
	return false
}

// agentKeptID reports whether the resumed session shows the agent under its first id: a Bash call or a stop.
func agentKeptID(first, resumed []map[string]any) bool {
	return agentRanAgain(first, resumed) || agentStoppedAgain(resumed, restartAgentID(first))
}

// restartNeverStarted says why the resume never started, from what the first session left.
func restartNeverStarted(first []map[string]any) string {
	if restartAgentID(first) == "" {
		return "the first session ended no agent with an id, so the resumed session never started"
	}
	return "the first session reported no session_id, so the resumed session never started"
}

// restartAgentVerdicts judges whether the agent kept its agent_id and its transcript path across the restart, and says why when a check is inconclusive. When the resume never started, or the resumed session failed, the probe says nothing either way. When no agent ran in the resumed session, that is inconclusive unless sent says SendMessage was called: Claude Code was asked to wake the agent and did not, which is a real break, as is an agent that ran under another id or stopped without keeping its path.
func restartAgentVerdicts(first, resumed []map[string]any, stage restartStage, sent string) (idKept, pathKept verdict, why string) {
	id := restartAgentID(first)
	switch stage {
	case resumeNotStarted:
		return verdictInconclusive, verdictInconclusive, restartNeverStarted(first)
	case firstFailed:
		return verdictInconclusive, verdictInconclusive, "the first session failed, so the resumed session never started"
	case resumeFailed:
		return verdictInconclusive, verdictInconclusive, "the resumed session failed, so nothing is known about the agent"
	}
	if !agentRanResumed(resumed) {
		if sent == "true" {
			return verdictFail, verdictFail, ""
		}
		return verdictInconclusive, verdictInconclusive, "the session model did not get the agent to run in the resumed session"
	}
	kept, stopped := agentKeptID(first, resumed), agentStoppedAgain(resumed, id)
	switch {
	case !kept && !stopped:
		return verdictFail, verdictFail, ""
	case !stopped:
		return verdictPass, verdictInconclusive, "the agent ran in the resumed session but its stop was not seen"
	}
	idKept, pathKept = verdictFail, verdictFail
	if kept {
		idKept = verdictPass
	}
	if agentPathKept(first, resumed) {
		pathKept = verdictPass
	}
	return idKept, pathKept, ""
}

// toolUseBlock is a content block of an assistant record.
type toolUseBlock struct {
	ID    string          `json:"id"`
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// sendMessageCalls returns the SendMessage tool_use blocks of the assistant records in a transcript.
func sendMessageCalls(transcript []string) []toolUseBlock {
	var calls []toolUseBlock
	for _, line := range transcript {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Type != "assistant" {
			continue
		}
		var blocks []toolUseBlock
		if json.Unmarshal(rec.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.Name == "SendMessage" {
				calls = append(calls, b)
			}
		}
	}
	return calls
}

// sendMessageIDs is the set of tool_use ids of the SendMessage calls in a transcript. A call with no id is left out.
func sendMessageIDs(transcript []string) map[string]bool {
	ids := map[string]bool{}
	for _, c := range sendMessageCalls(transcript) {
		if c.ID != "" {
			ids[c.ID] = true
		}
	}
	return ids
}

// calledSendMessage reports whether a transcript holds a SendMessage tool call by the assistant whose input names agent anywhere, other than a call whose tool_use id is in skip. It deliberately ignores the call's tool_result: if Claude Code lost the agent over the restart, the likely symptom is a call to the right id that comes back as an error, and that must stay a failure. An empty agent matches nothing.
func calledSendMessage(transcript []string, agent string, skip map[string]bool) bool {
	if agent == "" {
		return false
	}
	for _, c := range sendMessageCalls(transcript) {
		if (c.ID == "" || !skip[c.ID]) && strings.Contains(string(c.Input), agent) {
			return true
		}
	}
	return false
}

// restartChecks prints what the restart probe observed and returns its checks.
func restartChecks(p *restartProbe, out io.Writer) []selftestCheck {
	id := restartAgentID(p.first.events)
	fmt.Fprintf(out, "  restart probe: SessionStart source %q then %q\n", startSource(p.first.events), startSource(p.resumed.events))
	fmt.Fprintf(out, "  restart probe: session ids %q then %q\n", p.first.session, p.resumed.session)
	sent := sentMessage(p.stage, id, p.callsBefore, p.first.transcript, p.resumed.transcript)
	fmt.Fprintf(out, "  restart probe: agent id %q, then %q seen again; transcript path %q then %q; the resumed turn messaged the agent: %s\n",
		id, map[bool]string{true: id, false: "none"}[agentKeptID(p.first.events, p.resumed.events)],
		stopPath(p.first.events, id), stopPath(p.resumed.events, id), sent)
	idKept, pathKept, why := restartAgentVerdicts(p.first.events, p.resumed.events, p.stage, sent)
	if p.stage == resumeCompleted && sent == "true" && !agentRanResumed(p.resumed.events) {
		fmt.Fprintln(out, "  restart probe: SendMessage was called but the agent never ran in the resumed session; the model did its part, so this points at Claude Code")
	}
	sources := selftestCheck{"SessionStart carries source: startup, then resume", sourcesReported(p.first.events, p.resumed.events), ""}
	tied := selftestCheck{"a resumed session keeps its id, or its transcript keeps records stamped with the first id", restartTied(p.first.session, p.resumed.session, p.resumed.transcript), ""}
	if p.stage == resumeNotStarted || p.stage == firstFailed {
		sources.ok, sources.inconclusive = false, why
		tied.ok, tied.inconclusive = false, why
	}
	return []selftestCheck{
		sources,
		tied,
		idKept.check("an agent resumed after a restart keeps its agent_id", why),
		pathKept.check("an agent's transcript path survives a restart", why),
	}
}
