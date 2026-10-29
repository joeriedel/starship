package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HookInput is the part of Claude Code's hook payload that ensign reads.
type HookInput struct {
	Event               string         `json:"hook_event_name"`
	Cwd                 string         `json:"cwd"`
	ToolName            string         `json:"tool_name"`
	ToolInput           map[string]any `json:"tool_input"`
	AgentID             string         `json:"agent_id"`
	AgentType           string         `json:"agent_type"`
	TranscriptPath      string         `json:"transcript_path"`
	AgentTranscriptPath string         `json:"agent_transcript_path"`
	LastMessage         string         `json:"last_assistant_message"`
	StopHookActive      bool           `json:"stop_hook_active"`
	Prompt              string         `json:"prompt"`
}

var tagRe = regexp.MustCompile(`\[starship run=(\S+) dispatch=(\S+)`)

func tagLine(run string, d *Dispatch) string {
	t := fmt.Sprintf("[starship run=%s dispatch=%s stage=%s", run, d.ID, d.Stage)
	if d.Dim != "" {
		t += " dim=" + d.Dim
	}
	return t + "]"
}

// readTag finds the dispatch tag in the first user message of an agent's transcript.
func readTag(transcript string) (run, dispatch string) {
	f, err := os.Open(transcript)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "user" {
			continue
		}
		text := string(rec.Message.Content)
		var s string
		if json.Unmarshal(rec.Message.Content, &s) == nil {
			text = s
		}
		if m := tagRe.FindStringSubmatch(text); m != nil {
			return m[1], m[2]
		}
		return "", ""
	}
	return "", ""
}

// agentTranscript derives a subagent's transcript path from the session's.
func agentTranscript(sessionTranscript, agentID string) string {
	return filepath.Join(strings.TrimSuffix(sessionTranscript, ".jsonl"), "subagents", "agent-"+agentID+".jsonl")
}

func deny(r *RunFiles, stage, reason string) any {
	r.State.log("denied", stage, "", reason)
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": "starship: " + reason,
	}}
}

func block(reason string) any {
	return map[string]any{"decision": "block", "reason": "starship: " + reason}
}

// callerDispatch finds the dispatch of the agent making a call, binding its agent id if new.
func callerDispatch(r *RunFiles, in *HookInput) *Dispatch {
	if d := r.State.byAgent(in.AgentID); d != nil {
		return d
	}
	if run, id := readTag(agentTranscript(in.TranscriptPath, in.AgentID)); run == r.State.Run {
		if d := r.State.dispatch(id); d != nil {
			d.AgentID = in.AgentID
			return d
		}
	}
	// Fallback: exactly one unbound, unfinished dispatch of this role.
	var match *Dispatch
	for _, d := range r.State.Dispatches {
		if d.AgentID == "" && !d.Done && d.Role == in.AgentType {
			if match != nil {
				return nil
			}
			match = d
		}
	}
	if match != nil {
		match.AgentID = in.AgentID
	}
	return match
}

func hookPreToolUse(in *HookInput, r *RunFiles) (any, error) {
	if in.ToolName != "Agent" && in.ToolName != "Task" {
		return nil, nil
	}
	role, _ := in.ToolInput["subagent_type"].(string)
	m := &machine{snap: r.Snap, st: r.State}
	for _, fr := range r.Snap.Pipeline.FreeRoles {
		if role == fr {
			return nil, nil
		}
	}

	scope := ""
	var caller *Dispatch
	if in.AgentID != "" {
		caller = callerDispatch(r, in)
		if caller == nil {
			return deny(r, "", fmt.Sprintf("this agent is not part of run %s; only the run's lead dispatches its stages", r.State.Run)), nil
		}
		if s := m.stage(caller.Stage); s == nil || !s.isLead() {
			return deny(r, "", fmt.Sprintf("a %s cannot dispatch pipeline stages; only roles listed as free (%s) may be spawned here",
				caller.Role, strings.Join(r.Snap.Pipeline.FreeRoles, ", "))), nil
		}
		scope = caller.Stage
	}
	if r.State.terminal() {
		return deny(r, "", m.expect(scope)), nil
	}
	sc := r.State.Scopes[scope]
	if sc == nil || sc.Stage == "" {
		return deny(r, "", "nothing to dispatch from here. "+m.expect(scope)), nil
	}
	s := m.stage(sc.Stage)
	if role != s.Role {
		return deny(r, s.ID, fmt.Sprintf("refused %s: the next stage is %q, role %s. %s", role, s.ID, s.Role, m.expect(scope))), nil
	}
	if s.isLead() && !r.State.Approved {
		return deny(r, s.ID, "the run is not approved yet; the user must type go"), nil
	}

	d := &Dispatch{ID: fmt.Sprintf("d%d", len(r.State.Dispatches)+1), Stage: s.ID, Scope: scope, Role: role}
	if caller != nil {
		d.CallerAgent = caller.AgentID
	}
	if s.Parallel != nil {
		for _, dim := range s.Dimensions {
			if _, taken := sc.Branches[dim]; !taken {
				d.Dim = dim
				break
			}
		}
		if d.Dim == "" {
			return deny(r, s.ID, m.expect(scope)), nil
		}
		if sc.Branches == nil {
			sc.Branches = map[string]string{}
		}
		sc.Branches[d.Dim] = d.ID
	} else if m.running(scope, s.ID) > 0 {
		return deny(r, s.ID, m.expect(scope)), nil
	}
	r.State.Dispatches = append(r.State.Dispatches, d)
	r.State.log("dispatch", s.ID, d.ID, role+" "+d.Dim)

	original, _ := in.ToolInput["prompt"].(string)
	var prompt string
	if s.isLead() {
		first := s.Stages[0].ID
		if r.State.StartChild != "" {
			first = r.State.StartChild
		}
		m.enter(s.ID, first)
		prompt = leadPrompt(r, d, s, original)
	} else {
		prompt = stagePrompt(r, d, s, original)
	}
	updated := map[string]any{}
	for k, v := range in.ToolInput {
		updated[k] = v
	}
	updated["prompt"] = prompt
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":      "PreToolUse",
		"permissionDecision": "allow",
		"updatedInput":       updated,
	}}, nil
}

func hookSubagentStop(in *HookInput, r *RunFiles) (any, error) {
	d := r.State.byAgent(in.AgentID)
	if d == nil {
		run, id := readTag(in.AgentTranscriptPath)
		if run != r.State.Run {
			return nil, nil // not part of this run: a free role, or unrelated
		}
		if d = r.State.dispatch(id); d == nil {
			return nil, fmt.Errorf("dispatch %s from the transcript tag is not in run %s", id, run)
		}
		d.AgentID = in.AgentID
	}
	m := &machine{snap: r.Snap, st: r.State}
	s := m.stage(d.Stage)

	if s.isLead() {
		if r.State.terminal() {
			d.Done = true
			return nil, nil
		}
		d.StopBlocks++
		if d.StopBlocks > 3 {
			d.Done = true
			m.fail("lead-stopped", "the lead stopped before the pipeline finished")
			return nil, nil
		}
		r.State.log("blocked", s.ID, d.ID, "lead tried to stop early")
		return block("the pipeline is not finished. " + m.expect(s.ID)), nil
	}
	if d.Done {
		return nil, nil
	}

	outcome, summary, problem := parseReport(in.LastMessage, s)
	if problem != "" {
		d.StopBlocks++
		if d.StopBlocks > 2 {
			d.Done = true
			m.fail("bad-report", fmt.Sprintf("%s (%s) never produced a valid report: %s", s.ID, d.ID, problem))
			return nil, nil
		}
		r.State.log("blocked", s.ID, d.ID, problem)
		return block(problem + " " + reportInstructions(s)), nil
	}
	d.Done, d.Outcome, d.Summary = true, outcome, summary
	os.WriteFile(filepath.Join(r.Dir, "reports", d.ID+".md"), []byte(in.LastMessage), 0o644)
	r.State.log("report", s.ID, d.ID, outcome+": "+summary)
	if r.State.terminal() {
		return nil, nil
	}

	sc := r.State.Scopes[d.Scope]
	if sc == nil || sc.Stage != s.ID {
		return nil, nil // a stale stage; recorded, but it no longer drives the run
	}
	if s.Parallel != nil {
		var got []string
		for _, dim := range s.Dimensions {
			id, ok := sc.Branches[dim]
			if !ok {
				return nil, nil
			}
			b := r.State.dispatch(id)
			if !b.Done {
				return nil, nil
			}
			got = append(got, b.Outcome)
		}
		outcome = join(s, got)
	}
	m.apply(d.Scope, s, outcome)
	return nil, nil
}

func hookUserPromptSubmit(in *HookInput, r *RunFiles) (any, error) {
	if strings.TrimSpace(in.Prompt) != "go" {
		return nil, nil
	}
	ctx := func(s string) any {
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "UserPromptSubmit", "additionalContext": "starship: " + s,
		}}
	}
	if r == nil {
		return ctx("there is no run waiting for approval."), nil
	}
	if r.State.Status != "pending" {
		return ctx(fmt.Sprintf("run %s is %s, not waiting for approval.", r.State.Run, r.State.Status)), nil
	}
	r.State.Approved, r.State.Status = true, "running"
	r.State.log("approved", "", "", "")
	lead := r.Snap.Pipeline.Stages[0]
	return ctx(fmt.Sprintf("run %s approved. Dispatch its lead now: use the Agent tool with subagent_type %q, "+
		"and pass this work item as the prompt:\n\n%s", r.State.Run, lead.Role, r.Snap.WorkItem)), nil
}

var reportRe = regexp.MustCompile("(?s)```starship-report\\s*\\n(.*?)```")

// parseReport extracts the outcome from the last report block in an agent's final message.
// Each line is "key: value", split at the first colon, so a summary may contain colons.
func parseReport(msg string, s *Stage) (outcome, summary, problem string) {
	all := reportRe.FindAllStringSubmatch(msg, -1)
	if len(all) == 0 {
		return "", "", "Your final message has no starship-report block."
	}
	for _, line := range strings.Split(all[len(all)-1][1], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "outcome":
			outcome = val
		case "summary":
			summary = val
		}
	}
	if _, ok := s.Outcomes.find(outcome); !ok {
		return "", "", fmt.Sprintf("Your report's outcome %q is not one of this stage's outcomes.", outcome)
	}
	return outcome, summary, ""
}

func reportInstructions(s *Stage) string {
	return fmt.Sprintf("End your final message with a report block:\n\n```starship-report\noutcome: <one of: %s>\nsummary: <one line>\n```",
		strings.Join(s.Outcomes.names(), ", "))
}

func workspaceSection(r *RunFiles) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Workspace\n\n%s\n\nRun commands from the workspace root. Bindings for this project:\n\n", r.Snap.Workspace)
	for _, k := range sortedKeys(r.Snap.Bindings) {
		bd := r.Snap.Bindings[k]
		switch {
		case bd.Command != "":
			fmt.Fprintf(&b, "- @%s: `%s`\n", k, bd.Command)
		case bd.Guidance != "":
			fmt.Fprintf(&b, "- @%s: %s\n", k, bd.Guidance)
		default:
			fmt.Fprintf(&b, "- @%s: not applicable (%s)\n", k, bd.NotApplicable)
		}
	}
	return b.String()
}

func leadPrompt(r *RunFiles, d *Dispatch, s *Stage, original string) string {
	var b strings.Builder
	b.WriteString(tagLine(r.State.Run, d) + "\n\n## The stages you lead\n\n")
	for _, c := range s.Stages {
		fmt.Fprintf(&b, "- %s — role %s", c.ID, c.Role)
		if c.Parallel != nil {
			fmt.Fprintf(&b, ", %d in parallel (%s)", len(c.Dimensions), strings.Join(c.Dimensions, ", "))
		}
		var outs []string
		for _, o := range c.Outcomes {
			outs = append(outs, o.Name+" → "+o.Then.String())
		}
		fmt.Fprintf(&b, "; outcomes: %s\n", strings.Join(outs, "; "))
	}
	exe, _ := os.Executable()
	fmt.Fprintf(&b, "\n## What to dispatch next\n\nAt the start, and after every stage finishes, run this and do exactly what it says:\n\n    %s next --bridge %s\n\n",
		exe, r.Snap.Bridge)
	b.WriteString(workspaceSection(r))
	b.WriteString("\n## Work item\n\n" + original + "\n")
	return b.String()
}

func stagePrompt(r *RunFiles, d *Dispatch, s *Stage, original string) string {
	var b strings.Builder
	b.WriteString(tagLine(r.State.Run, d) + "\n\n## Stage: " + s.ID + "\n\n")
	if d.Dim != "" {
		fmt.Fprintf(&b, "Your focus: **%s**.\n\n%s\n", d.Dim, r.Snap.Briefs["briefs/review/"+d.Dim+".md"])
	}
	if s.Brief != "" {
		b.WriteString(r.Snap.Briefs[s.Brief] + "\n")
	}
	b.WriteString(workspaceSection(r))
	b.WriteString("\n## Your brief\n\n" + original + "\n\n## Report\n\n" + reportInstructions(s) + "\n")
	return b.String()
}

// cmdNext prints what the run's lead should dispatch next.
func cmdNext(args []string) error {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--bridge" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	bridge, err := findBridge(dir)
	if err != nil {
		return err
	}
	r, err := openActiveRun(bridge)
	if err != nil {
		return err
	}
	if r == nil {
		fmt.Println("There is no active run.")
		return nil
	}
	defer r.close()
	m := &machine{snap: r.Snap, st: r.State}
	scope := ""
	if lead := r.Snap.Pipeline.Stages[0]; lead.isLead() {
		scope = lead.ID
	}
	fmt.Println(m.expect(scope))
	if !r.State.terminal() {
		fmt.Println("Budgets used: " + m.budgetLine())
	}
	return nil
}
