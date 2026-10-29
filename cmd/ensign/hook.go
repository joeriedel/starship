package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// HookInput is the part of Claude Code's hook payload that ensign reads.
type HookInput struct {
	Event               string         `json:"hook_event_name"`
	SessionID           string         `json:"session_id"`
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

	d := &Dispatch{ID: fmt.Sprintf("d%d", len(r.State.Dispatches)+1), Stage: s.ID, Scope: scope, Role: role,
		Fields: map[string][]string{}}
	if caller != nil {
		d.CallerAgent = caller.AgentID
	}
	firstOfStage := m.running(scope, s.ID) == 0 && len(sc.Branches) == 0
	if s.isLead() && !r.State.Started {
		r.State.Started = true
		if r.State.Sync {
			if out, err := runHandler(r, Handler{Kind: "require", Command: "@sync", Source: "--sync"}, nil); err != nil {
				m.fail("handler", fmt.Sprintf("@sync failed: %v: %s", err, lastLines(out, 5)))
				return deny(r, s.ID, "the run could not start: "+r.State.Reason), nil
			}
		}
		if err := fire(r, "run.start", nil); err != nil {
			m.fail("handler", err.Error())
			return deny(r, s.ID, "the run could not start: "+r.State.Reason), nil
		}
	}
	if firstOfStage && !(s.isLead() && r.State.Scopes[s.ID] != nil && r.State.Scopes[s.ID].Stage != "") {
		if err := fire(r, "stage.before:"+s.ID, d); err != nil {
			m.fail("handler", err.Error())
			return deny(r, s.ID, "stage "+s.ID+" could not start: "+r.State.Reason), nil
		}
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
	extra := augments(r, "stage.before:"+s.ID, d)
	if s.isLead() {
		if child := r.State.Scopes[s.ID]; child == nil || child.Stage == "" {
			first := s.Stages[0].ID
			if r.State.StartChild != "" {
				first = r.State.StartChild
			}
			m.enter(s.ID, first)
		}
		prompt = leadPrompt(r, d, s, original, extra)
	} else {
		prompt = stagePrompt(r, d, s, original, extra)
	}
	updated := map[string]any{}
	for k, v := range in.ToolInput {
		updated[k] = v
	}
	updated["prompt"] = prompt
	// A stage runs in the foreground, so the lead never has a turn spent waiting on it.
	// Waiting turns are where agents poll, send filler and invent results.
	if bg, _ := updated["run_in_background"].(bool); bg {
		updated["run_in_background"] = false
	}
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
		if m.runningIn(s.ID) != "" {
			// Stages run in the foreground, so this means one ran in the background anyway.
			// Ending the turn is how the lead waits for it; blocking would force filler.
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

	outcome, summary, fields, problem := parseReport(in.LastMessage, s)
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
	if s.Curates != "" {
		d.Outcome = outcome
		if problem := applyCuration(r, s, d, in.LastMessage); problem != "" {
			d.Outcome = ""
			d.StopBlocks++
			if d.StopBlocks > 2 {
				d.Done = true
				m.fail("bad-report", fmt.Sprintf("%s (%s) never produced a valid rewrite: %s", s.ID, d.ID, problem))
				return nil, nil
			}
			r.State.log("blocked", s.ID, d.ID, problem)
			return block(problem), nil
		}
	}
	if d.Fields == nil {
		d.Fields = map[string][]string{}
	}
	for k, v := range fields {
		d.Fields[k] = v
	}
	d.Done, d.Outcome, d.Summary = true, outcome, summary
	recordFacts(r, d)
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
	if err := fire(r, "stage.after:"+s.ID, d); err != nil {
		m.fail("handler", err.Error())
		return nil, nil
	}
	m.apply(d.Scope, s, outcome)
	return nil, nil
}

// claimRun binds the newest pending run that no session owns to this session, and opens it.
// That is how a run created outside a session (from a terminal) gets approved.
func claimRun(bridge, session string) (*RunFiles, error) {
	for _, ri := range listRuns(bridge) {
		if ri.state.Status != "pending" || ri.state.Session != "" {
			continue
		}
		r, err := openRun(bridge, ri.id)
		if err != nil {
			return nil, err
		}
		if r.State.Session != "" { // claimed meanwhile
			r.close()
			continue
		}
		r.State.Session = session
		if err := bindSession(bridge, session, ri.id); err != nil {
			r.close()
			return nil, err
		}
		return r, nil
	}
	return nil, nil
}

func hookUserPromptSubmit(in *HookInput, r *RunFiles) (any, error) {
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
	if !lead.isLead() {
		m := &machine{snap: r.Snap, st: r.State}
		return ctx(fmt.Sprintf("run %s approved. You dispatch its stages yourself: %s After each stage finishes, run `ensign next --run %s` and do what it says.",
			r.State.Run, m.expect(""), r.State.Run)), nil
	}
	return ctx(fmt.Sprintf("run %s approved. Dispatch its lead now: use the Agent tool with subagent_type %q, "+
		"and pass this work item as the prompt:\n\n%s", r.State.Run, lead.Role, r.Snap.WorkItem)), nil
}

var reportRe = regexp.MustCompile("(?s)```starship-report\\s*\\n(.*?)```")

// parseReport extracts the outcome, summary and other fields from the last report block
// in an agent's final message. Each line is "key: value", split at the first colon, so a
// summary may contain colons. A key may repeat; files is a comma-separated list.
func parseReport(msg string, s *Stage) (outcome, summary string, fields map[string][]string, problem string) {
	all := reportRe.FindAllStringSubmatch(msg, -1)
	if len(all) == 0 {
		return "", "", nil, "Your final message has no starship-report block."
	}
	fields = map[string][]string{}
	for _, line := range strings.Split(all[len(all)-1][1], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimLeft(key, "- "))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "outcome":
			outcome = val
		case "summary":
			summary = val
		case "files":
			for _, f := range strings.Split(strings.Trim(val, "[]"), ",") {
				if f = strings.Trim(strings.TrimSpace(f), `"'`); f != "" {
					fields[key] = append(fields[key], f)
				}
			}
		default:
			if val != "" {
				fields[key] = append(fields[key], val)
			}
		}
	}
	if _, ok := s.Outcomes.find(outcome); !ok {
		return "", "", nil, fmt.Sprintf("Your report's outcome %q is not one of this stage's outcomes.", outcome)
	}
	for _, f := range s.ReportFields {
		if len(fields[f]) == 0 && !(f == "files" && outcome != "compiled" && outcome != "broken") {
			return "", "", nil, fmt.Sprintf("Your report has no %s line, which this stage requires.", f)
		}
	}
	return outcome, summary, fields, ""
}

func reportInstructions(s *Stage) string {
	var extra string
	for _, f := range s.ReportFields {
		if f == "files" {
			extra += "files: <every file you changed, comma-separated>\n"
		} else {
			extra += f + ": <" + f + ">\n"
		}
	}
	return fmt.Sprintf("End your final message with a report block:\n\n```starship-report\noutcome: <one of: %s>\nsummary: <one line>\n%s```",
		strings.Join(s.Outcomes.names(), ", "), extra)
}

func workspaceSection(r *RunFiles) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Workspace\n\n%s\n\nScratch folder for logs and notes (never the workspace): %s\n\nThe project's bindings, as the exact commands to run:\n\n",
		r.Snap.Workspace, filepath.Join(r.Dir, "scratch"))
	for _, k := range sortedKeys(r.Snap.Bindings) {
		bd := r.Snap.Bindings[k]
		switch {
		case bd.Command != "":
			fmt.Fprintf(&b, "- @%s: `cd %s && %s`\n", k, shellQuote(r.Snap.Workspace), bd.Command)
		case bd.Guidance != "":
			fmt.Fprintf(&b, "- @%s: %s\n", k, bd.Guidance)
		default:
			fmt.Fprintf(&b, "- @%s: not applicable (%s)\n", k, bd.NotApplicable)
		}
	}
	return b.String()
}

func leadPrompt(r *RunFiles, d *Dispatch, s *Stage, original string, extra []string) string {
	var b strings.Builder
	b.WriteString(tagLine(r.State.Run, d) + "\n\n")
	if len(r.State.Answers) > 0 {
		fmt.Fprintf(&b, "## Resuming\n\nThis run stopped to ask the user a question and is continuing with their answer. "+
			"Earlier stages' reports are in %s.\n\n%s\n\n", filepath.Join(r.Dir, "reports"), strings.Join(r.State.Answers, "\n\n"))
	}
	for _, a := range extra {
		b.WriteString(strings.TrimSpace(a) + "\n\n")
	}
	b.WriteString("## The stages you lead\n\n")
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
	fmt.Fprintf(&b, "\n## What to dispatch next\n\nAt the start, and after every stage finishes, run this and do exactly what it says:\n\n    ensign next --bridge %s --run %s\n\n",
		shellQuote(r.Snap.Bridge), r.State.Run)
	if sk := skillSections(r, s.Role); sk != "" {
		b.WriteString("\n" + sk + "\n\n")
	}
	b.WriteString(workspaceSection(r))
	b.WriteString("\n## Work item\n\n" + original + "\n")
	return b.String()
}

func stagePrompt(r *RunFiles, d *Dispatch, s *Stage, original string, extra []string) string {
	var b strings.Builder
	b.WriteString(tagLine(r.State.Run, d) + "\n\n## Stage: " + s.ID + "\n\n")
	if d.Dim != "" && s.Parallel != nil && s.Parallel.Over == "dimensions" {
		fmt.Fprintf(&b, "Your focus: **%s**.\n\n%s\n", d.Dim, r.Snap.Briefs["briefs/review/"+d.Dim+".md"])
	} else if d.Dim != "" {
		b.WriteString(itemContext(r, s, d.Dim, d) + "\n")
	}
	if s.Brief != "" {
		b.WriteString(r.Snap.Briefs[s.Brief] + "\n")
	}
	for _, a := range extra {
		b.WriteString(strings.TrimSpace(a) + "\n\n")
	}
	if sk := skillSections(r, s.Role); sk != "" {
		b.WriteString(sk + "\n\n")
	}
	b.WriteString(workspaceSection(r))
	b.WriteString("\n## Your brief\n\n" + original + "\n\n## Report\n\n" + reportInstructions(s) + "\n")
	if len(r.Snap.Skills) > 0 {
		b.WriteString("\nIf you had to establish a fact about a technology itself — true on any project using it, not just this one — " +
			"add a line per fact to the report block: `fact: <skill> | <the fact> | <path:line or how you verified it>`.\n")
	}
	return b.String()
}

// cmdNext prints what the run's lead should dispatch next.
func cmdNext(args []string) error {
	dir, run := "", ""
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--bridge":
			dir = args[i+1]
		case "--run":
			run = args[i+1]
		default:
			return fmt.Errorf("usage: ensign next [--bridge <dir>] [--run <id>]")
		}
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	bridge, err := findBridge(dir)
	if err != nil {
		return err
	}
	var r *RunFiles
	if run != "" {
		r, err = openRun(bridge, run)
	} else {
		r, err = openSessionRun(bridge, os.Getenv("STARSHIP_SESSION"))
	}
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

// cmdResume implements `ensign resume --answer <text> [--budget <name>=<n>]… [--run <id>]`.
func cmdResume(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign resume --answer <text> [--budget <name>=<n>] [--run <id>]")
	var answer, run string
	extra := map[string]int{}
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--answer":
			answer = args[i+1]
		case "--run":
			run = args[i+1]
		case "--budget":
			name, n, ok := strings.Cut(args[i+1], "=")
			v, err := strconv.Atoi(strings.TrimPrefix(n, "+"))
			if !ok || err != nil || v < 1 {
				return usage
			}
			extra[name] += v
		default:
			return usage
		}
	}
	if answer == "" || len(args)%2 != 0 {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	var r *RunFiles
	if run != "" {
		r, err = openRun(bridge, run)
	} else {
		r, err = openSessionRun(bridge, os.Getenv("STARSHIP_SESSION"))
	}
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("no run in this session")
	}
	defer r.close()
	for b := range extra {
		if _, ok := r.Snap.Pipeline.Budgets[b]; !ok {
			return fmt.Errorf("no budget %q", b)
		}
	}
	m := &machine{snap: r.Snap, st: r.State}
	if err := m.resume(answer, extra); err != nil {
		return err
	}
	if err := r.save(); err != nil {
		return err
	}
	lead := r.Snap.Pipeline.Stages[0]
	fmt.Fprintf(out, "run %s resumed. Dispatch its lead now: use the Agent tool with subagent_type %q and this work item as the prompt:\n\n%s\n",
		r.State.Run, lead.Role, r.Snap.WorkItem)
	return nil
}

// shellQuote quotes a path for a POSIX shell when it needs it.
func shellQuote(p string) string {
	if strings.ContainsAny(p, " '\"$`\\&;|<>()*?[]{}!#~") {
		return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return p
}
