// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// HookInput is the part of Claude Code's hook payload that ensign reads.
type HookInput struct {
	Event               string         `json:"hook_event_name"`
	Source              string         `json:"source"` // SessionStart: startup, resume, clear or compact
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

	// rawToolInput is tool_input as received, set for connector calls so the input can be compared with its numbers intact.
	rawToolInput json.RawMessage

	// ToolResponse is a PostToolUse result: an object for most tools, but not guaranteed.
	ToolResponse    any              `json:"tool_response"`
	BackgroundTasks []BackgroundTask `json:"background_tasks"`
}

// BackgroundTask is an in-flight task listed on Stop and SubagentStop. On SubagentStop it
// is scoped to the parent session and names no owner.
type BackgroundTask struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Command   string `json:"command"`
	AgentType string `json:"agent_type"`
}

var tagRe = regexp.MustCompile(`\[starship run=(\S+) dispatch=(\S+)`)

var spawnTagRe = regexp.MustCompile(`\[starship run=(\S+) lead\]`)

// spawnTag begins the prompt the session passes when it spawns a run's lead, so with several live runs the spawn is tied to its run.
func spawnTag(run string) string { return "[starship run=" + run + " lead]" }

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
	return denial(reason)
}

func denial(reason string) any {
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
	run, id := agentTag(in)
	if run == r.State.Run {
		if d := r.State.dispatch(id); d != nil {
			d.AgentID = in.AgentID
			return d
		}
	}
	if run != "" && run != r.State.Run {
		return nil // a stray of another run, such as a stopped one, never takes a dispatch here
	}
	// Fallback, when the tag names no other run: exactly one unbound, unfinished dispatch of this role.
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

// newDispatch makes the next dispatch of a stage; the caller records it.
func newDispatch(r *RunFiles, scope string, s *Stage, role string) *Dispatch {
	stops := stopBudget
	return &Dispatch{ID: fmt.Sprintf("d%d", len(r.State.Dispatches)+1), Stage: s.ID, Scope: scope, Role: role,
		Fields: map[string][]string{}, StopsLeft: &stops}
}

func hookPreToolUse(in *HookInput, r *RunFiles) (any, error) {
	if in.ToolName == "SubagentHandback" {
		return hookHandback(in, r), nil
	}
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

	if in.AgentID == "" && !r.Snap.Pipeline.hasStageRole(role) {
		return nil, nil
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
		if !r.Snap.Pipeline.hasStageRole(role) {
			return nil, nil
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
		return deny(r, s.ID, fmt.Sprintf("refused %s: stages run in the pipeline's order, and only the next one may start. The next stage is %q, role %s. %s", role, s.ID, s.Role, m.expect(scope))), nil
	}
	if s.isLead() && !r.State.Approved {
		return deny(r, s.ID, "the run is not approved yet; the user must type go"), nil
	}

	if plainStage(r.Snap, s) {
		return deny(r, s.ID, "ensign runs this stage itself; do not spawn an agent for it. "+m.expect(scope)), nil
	}

	d := newDispatch(r, scope, s, role)
	if caller != nil {
		d.CallerAgent = caller.AgentID
	}
	firstOfStage := m.running(scope, s.ID) == 0 && len(sc.Branches) == 0
	if s.isLead() && !r.State.Started {
		r.State.Started = true
		// A run that starts has to pass again, even if it dies before run.start's @branch. A run started at docs is the exception: it runs nothing the test stage covers, and tested() checks what it committed.
		if !startsAtDocs(r) {
			if err := os.Remove(testedFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location)); err != nil && !os.IsNotExist(err) {
				m.fail("handler", fmt.Sprintf("could not clear the tested marker: %v", err))
				return deny(r, s.ID, "the run could not start: "+r.State.Reason), nil
			}
		}
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
		if isGit(r.Snap.Workspace) {
			r.State.StartHead, _ = gitOut(r.Snap.Workspace, "rev-parse", "HEAD")
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
	if d.Role == "documentation" {
		base, err := docsBaseline(r)
		if err != nil {
			m.fail("ensign", err.Error())
			return deny(r, s.ID, "stage "+s.ID+" could not start: "+r.State.Reason), nil
		}
		d.DocsBase = base
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
		var perr error
		if prompt, perr = stagePrompt(r, d, s, original, extra); perr != nil {
			m.fail("ensign", perr.Error())
			return deny(r, s.ID, "stage "+s.ID+" could not start: "+r.State.Reason), nil
		}
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

// hookHandback records the message of an agent's SubagentHandback call as its report. A
// stage's report is checked here and refused with the reason, because the call ends the
// agent and a block at SubagentStop would reach nobody. Recorded before the call runs,
// since the call ends the agent's run.
func hookHandback(in *HookInput, r *RunFiles) any {
	d := callerDispatch(r, in)
	if d == nil || in.AgentID == "" {
		return nil
	}
	msg, _ := in.ToolInput["message"].(string)
	m := &machine{snap: r.Snap, st: r.State}
	s := m.stage(d.Stage)
	// refuse counts a refused handback; the one that uses up the budget fails the run and is let through.
	refuse := func(reason, code, why string) any {
		if d.spendStop() {
			d.Done = true
			if code == "lead-stopped" {
				stopOrphanedServices(r, m, time.Now().Add(recoveryDownLimit))
			}
			m.fail(code, why)
			return nil
		}
		r.State.log("denied", s.ID, d.ID, reason)
		return denial(reason)
	}
	if s != nil && s.isLead() && !r.State.terminal() {
		return refuse("the pipeline is not finished. "+m.expect(s.ID), "lead-stopped", "the lead stopped before the pipeline finished")
	} else if s != nil && !s.isLead() && !d.Done {
		outcome, _, _, problem := parseReport(msg, s)
		if problem != "" {
			return refuse(problem+" "+reportInstructions(s), "bad-report", fmt.Sprintf("%s (%s) never produced a valid report: %s", s.ID, d.ID, problem))
		}
		if s.Curates != "" {
			d.Outcome = outcome
			if problem := applyCuration(r, s, d, msg); problem != "" {
				d.Outcome = ""
				return refuse(problem, "bad-report", fmt.Sprintf("%s (%s) never produced a valid rewrite: %s", s.ID, d.ID, problem))
			}
		}
	}
	d.Handback = msg
	return nil
}

// backgroundLaunch returns the id of the background task an agent's tool call started,
// or "" when the call started none or was not made by an agent.
func backgroundLaunch(in *HookInput) string {
	resp, _ := in.ToolResponse.(map[string]any)
	if in.AgentID == "" || resp == nil {
		return ""
	}
	var task string
	if in.ToolName == "Bash" {
		task, _ = resp["backgroundTaskId"].(string)
	} else if resp["status"] == "async_launched" {
		task, _ = resp["agentId"].(string)
	}
	return task
}

// hookPostToolUse records which run agent launched a background task, so a later
// SubagentStop can tell a wait on that task from an early stop.
func hookPostToolUse(in *HookInput, r *RunFiles) (any, error) {
	task := backgroundLaunch(in)
	if task == "" || callerDispatch(r, in) == nil {
		return nil, nil
	}
	if r.State.Tasks == nil {
		r.State.Tasks = map[string]string{}
	}
	r.State.Tasks[task] = in.AgentID
	return nil, nil
}

// ownsInFlight reports whether the agent launched a task that is still in flight.
func ownsInFlight(r *RunFiles, in *HookInput) bool {
	for _, t := range in.BackgroundTasks {
		if r.State.Tasks[t.ID] == in.AgentID {
			return true
		}
	}
	return false
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
	recordUsage(r, d)
	m := &machine{snap: r.Snap, st: r.State}
	s := m.stage(d.Stage)

	if s.isLead() {
		deadline := time.Now().Add(recoveryDownLimit)
		if d := deadPlainOwner(r, m, s.ID); d != nil && !r.State.terminal() {
			if _, err := completeDeadOwner(r, m, d, deadline); err != nil {
				return nil, err
			}
		}
		if r.State.terminal() {
			d.Done = true
			return nil, nil
		}
		if m.runningIn(s.ID) != "" {
			// Stages run in the foreground, so this means one ran in the background anyway.
			// Ending the turn is how the lead waits for it; blocking would force filler.
			return nil, nil
		}
		if d.Handback == "" && ownsInFlight(r, in) {
			return nil, nil // waiting on its own background task
		}
		if d.spendStop() {
			d.Done = true
			stopOrphanedServices(r, m, deadline)
			m.fail("lead-stopped", "the lead stopped before the pipeline finished")
			return nil, nil
		}
		r.State.log("blocked", s.ID, d.ID, "lead tried to stop early")
		return block("the pipeline is not finished. " + m.expect(s.ID)), nil
	}
	if d.Done {
		return nil, nil
	}

	if d.Handback == "" && ownsInFlight(r, in) {
		return nil, nil // waiting on its own background task
	}
	// A handed-back report was checked, and any curation applied, at the handback call.
	report, handedBack := d.Handback, d.Handback != ""
	if !handedBack {
		report = in.LastMessage
	}
	outcome, summary, fields, problem := parseReport(report, s)
	if problem != "" {
		if d.spendStop() {
			d.Done = true
			m.fail("bad-report", fmt.Sprintf("%s (%s) never produced a valid report: %s", s.ID, d.ID, problem))
			return nil, nil
		}
		r.State.log("blocked", s.ID, d.ID, problem)
		return block(problem + " " + reportInstructions(s)), nil
	}
	if s.Curates != "" && !handedBack {
		d.Outcome = outcome
		if problem := applyCuration(r, s, d, report); problem != "" {
			d.Outcome = ""
			if d.spendStop() {
				d.Done = true
				m.fail("bad-report", fmt.Sprintf("%s (%s) never produced a valid rewrite: %s", s.ID, d.ID, problem))
				return nil, nil
			}
			r.State.log("blocked", s.ID, d.ID, problem)
			return block(problem), nil
		}
	}
	completeDispatch(r, m, d, s, outcome, summary, fields, report)
	return nil, nil
}

// completeDispatch records a finished stage's report and, once its stage is complete,
// fires stage.after and takes the transition for the outcome.
func completeDispatch(r *RunFiles, m *machine, d *Dispatch, s *Stage, outcome, summary string, fields map[string][]string, report string) {
	if d.Fields == nil {
		d.Fields = map[string][]string{}
	}
	// A private document's summary and fields can quote it. The report file below stays in the run folder, which is not tracked.
	if s.Curates != "document" {
		for k, v := range fields {
			d.Fields[k] = v
		}
	}
	d.Done, d.Outcome = true, outcome
	detail := outcome
	if s.Curates != "document" {
		d.Summary = summary
		recordFacts(r, d)
		detail += ": " + summary
	}
	os.WriteFile(filepath.Join(r.Dir, "reports", d.ID+".md"), []byte(report), 0o644)
	r.State.log("report", s.ID, d.ID, detail)
	if r.State.terminal() {
		return
	}

	sc := r.State.Scopes[d.Scope]
	if sc == nil || sc.Stage != s.ID {
		return // a stale stage; recorded, but it no longer drives the run
	}
	if s.Parallel != nil {
		var got []string
		for _, dim := range s.Dimensions {
			id, ok := sc.Branches[dim]
			if !ok {
				return
			}
			b := r.State.dispatch(id)
			if !b.Done {
				return
			}
			got = append(got, b.Outcome)
		}
		outcome = join(s, got)
	}
	if d.Role == "documentation" {
		_, _, stray, err := docsChanged(r, d.DocsBase)
		if err != nil {
			m.fail("ensign", err.Error())
			return
		}
		if len(stray) > 0 {
			m.fail("docs-scope", "the documentation stage changed files outside the @docs paths: "+strings.Join(stray, ", "))
			return
		}
	}
	if err := fire(r, "stage.after:"+s.ID, d); err != nil {
		m.fail("handler", err.Error())
		return
	}
	m.apply(d.Scope, s, outcome)
}

// claimRun binds the newest pending run that no session owns to this session, and opens it.
// That is how a run created outside a session (from a terminal) gets approved.
func claimRun(bridge, session string) (*RunFiles, error) {
	all, err := listRuns(bridge)
	if err != nil {
		return nil, err
	}
	for _, ri := range all {
		if ri.state.Status != "pending" || ri.state.Session != "" {
			continue
		}
		r, err := openRun(bridge, ri.id)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.State.Session != "" { // claimed meanwhile
			r.close()
			continue
		}
		r.State.Session = session
		bindErr := bindSession(bridge, session, ri.id)
		if bindErr != nil && !errors.Is(bindErr, errSetupFile) {
			r.close()
			return nil, bindErr
		}
		r.warn = bindErr
		return r, nil
	}
	return nil, nil
}

func hookUserPromptSubmit(in *HookInput, r *RunFiles) (any, error) {
	ctx := func(s string) any {
		s = "starship: " + s
		if r != nil && r.warn != nil {
			s += "\n\nstarship: " + r.warn.Error()
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "UserPromptSubmit", "additionalContext": s,
		}}
	}
	if r == nil {
		return ctx("there is no run waiting for approval."), nil
	}
	if r.State.Status != "pending" {
		return ctx(fmt.Sprintf("run %s is %s, not waiting for approval.", r.State.Run, r.State.Status)), nil
	}
	l, err := lockFolders(r.Snap.Bridge)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	if err := folderHolder(r.Snap.Bridge, r.Snap, r.State.Session, r.State.Run); err != nil {
		return ctx(fmt.Sprintf("run %s was not started: %v.", r.State.Run, err)), nil
	}
	r.State.Approved, r.State.Status = true, "running"
	r.State.log("approved", "", "", "")
	if err := r.save(); err != nil {
		return nil, err
	}
	return ctx(approvalText(r)), nil
}

// approvalText tells the session what to do with a run that has just been approved.
func approvalText(r *RunFiles) string {
	lead := r.Snap.Pipeline.Stages[0]
	if !lead.isLead() {
		m := &machine{snap: r.Snap, st: r.State}
		return fmt.Sprintf("run %s approved. You dispatch its stages yourself: %s After each stage finishes, run `ensign next --run %s` and do what it says.",
			r.State.Run, m.expect(""), r.State.Run)
	}
	return fmt.Sprintf("run %s approved. Dispatch its lead now: use the Agent tool with subagent_type %q, "+
		"and pass this work item as the prompt, first line included:\n\n%s\n\n%s", r.State.Run, lead.Role, spawnTag(r.State.Run), r.Snap.WorkItem)
}

var reportRe = regexp.MustCompile("(?s)```starship-report\\s*\\n(.*?)```")

// parseReport extracts the outcome, summary and other fields from the last report block
// in an agent's report. Each line is "key: value", split at the first colon, so a
// summary may contain colons. A key may repeat (checked entries each take a line); files is a comma-separated list whose entries may be double-quoted (see parseFileNames). A required field may be missing for some outcomes (see reportFieldOptional).
func parseReport(msg string, s *Stage) (outcome, summary string, fields map[string][]string, problem string) {
	all := reportRe.FindAllStringSubmatch(msg, -1)
	if len(all) == 0 {
		return "", "", nil, "Your report has no starship-report block."
	}
	fields = map[string][]string{}
	for _, line := range strings.Split(all[len(all)-1][1], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimLeft(key, "- "))
		raw := strings.TrimSpace(val)
		val = strings.Trim(raw, `"'`)
		switch key {
		case "outcome":
			outcome = val
		case "summary":
			summary = val
		case "files":
			names, bad := parseFileNames(raw)
			if bad != "" {
				return "", "", nil, bad
			}
			fields[key] = append(fields[key], names...)
		default:
			if val != "" {
				fields[key] = append(fields[key], val)
			}
		}
	}
	if _, ok := s.Outcomes.find(outcome); !ok {
		if s.Curates == "document" {
			return "", "", nil, "Your report's outcome is not one of this stage's outcomes."
		}
		return "", "", nil, fmt.Sprintf("Your report's outcome %q is not one of this stage's outcomes.", outcome)
	}
	for _, f := range s.ReportFields {
		if len(fields[f]) == 0 && !reportFieldOptional(f, outcome) {
			return "", "", nil, fmt.Sprintf("Your report has no %s line, which this stage requires.", f)
		}
	}
	return outcome, summary, fields, ""
}

// reportFieldOptional reports whether a stage's required report field may be missing for this outcome. files is required only for compiled, broken and updated; checked is required except when blocked.
func reportFieldOptional(field, outcome string) bool {
	switch field {
	case "files":
		return outcome != "compiled" && outcome != "broken" && outcome != "updated"
	case "checked":
		return outcome == "blocked"
	}
	return false
}

// parseFileNames splits the value of a files line. An entry may be wrapped in double quotes, inside which a double quote is written doubled and every other byte is literal; backslash is not an escape because reported Windows paths contain backslashes. Other entries run to the next comma and lose surrounding quotes. Empty names are dropped.
func parseFileNames(val string) (names []string, problem string) {
	if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
		val = val[1 : len(val)-1]
	}
	for i := 0; i < len(val); {
		i = len(val) - len(strings.TrimLeftFunc(val[i:], unicode.IsSpace))
		if i == len(val) {
			break
		}
		if val[i] != '"' {
			end := strings.IndexByte(val[i:], ',')
			if end < 0 {
				end = len(val) - i
			}
			if f := strings.Trim(strings.TrimSpace(val[i:i+end]), `"'`); f != "" {
				names = append(names, f)
			}
			i += end + 1
			continue
		}
		var name strings.Builder
		i++
		closed := false
		for i < len(val) && !closed {
			switch {
			case val[i] == '"' && i+1 < len(val) && val[i+1] == '"':
				name.WriteByte('"')
				i += 2
			case val[i] == '"':
				closed = true
				i++
			default:
				name.WriteByte(val[i])
				i++
			}
		}
		if !closed {
			return nil, `Your report's files line has a double quote that is never closed. Wrap a file name in double quotes to include a comma, and write a double quote inside it as "".`
		}
		i = len(val) - len(strings.TrimLeftFunc(val[i:], unicode.IsSpace))
		if i < len(val) && val[i] != ',' {
			return nil, `Your report's files line has text after a closing double quote. A double-quoted file name must be followed by a comma or the end of the line; write a double quote inside it as "".`
		}
		i++
		if name.Len() > 0 {
			names = append(names, name.String())
		}
	}
	return names, ""
}

func reportInstructions(s *Stage) string {
	var extra string
	for _, f := range s.ReportFields {
		if f == "files" {
			extra += "files: <every file you changed, comma-separated; wrap a name that contains a comma in double quotes, writing a double quote inside it as \"\">\n"
		} else if f == "checked" {
			extra += "checked: <the code you read to confirm the docs, as path and function; repeat this line for each entry, each starting with checked:; or, when no code bears on the docs, one line saying why>\n"
		} else {
			extra += f + ": <" + f + ">\n"
		}
	}
	return fmt.Sprintf("End your report with a report block:\n\n```starship-report\noutcome: <one of: %s>\nsummary: <one line>\n%s```",
		strings.Join(s.Outcomes.names(), ", "), extra)
}

func workspaceSection(r *RunFiles) string {
	var b strings.Builder
	ws := r.Snap.Workspace
	if r.Snap.Location != "" {
		ws += " (the project's " + r.Snap.Location + " location)"
	}
	fmt.Fprintf(&b, "## Workspace\n\n%s\n\nScratch folder for logs and notes (never the workspace): %s\n\nThe project's bindings, as the exact commands to run:\n\n",
		ws, filepath.Join(r.Dir, "scratch"))
	for _, k := range sortedKeys(r.Snap.Bindings) {
		bd := r.Snap.Bindings[k]
		b.WriteString(bindingLine(k, bd, r.Snap.Workspace))
	}
	return b.String()
}

// bindingLine renders a binding as a command to run in dir, as guidance, or as not applicable.
func bindingLine(k string, bd Binding, dir string) string {
	switch {
	case bd.Command != "":
		return fmt.Sprintf("- @%s: `cd %s && %s`\n", k, shellQuote(dir), bd.Command)
	case bd.Guidance != "":
		return fmt.Sprintf("- @%s: %s\n", k, bd.Guidance)
	case len(bd.Paths) > 0:
		return fmt.Sprintf("- @%s: paths relative to %s: %s\n", k, shellQuote(dir), strings.Join(bd.Paths, ", "))
	default:
		return fmt.Sprintf("- @%s: not applicable (%s)\n", k, bd.NotApplicable)
	}
}

// sidesSection tells a tester to test every side of a run that has several, each with its own commands in its own folder.
func sidesSection(r *RunFiles) string {
	sides := r.Snap.sides()
	if len(sides) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Sides\n\nThis change affects several locations. Build and test each side with its own commands in its own folder, not the workspace's bindings above:\n")
	for _, sd := range sides {
		fmt.Fprintf(&b, "\nside %s: %s\n\n", sd.Location, sd.Workspace)
		for _, k := range []string{servicesUp, "build", "test", servicesDown} {
			if bd, ok := sd.Bindings[k]; ok {
				b.WriteString(bindingLine(k, bd, sd.Workspace))
			}
		}
	}
	if bd := r.Snap.crossTest(); bd.Command != "" || bd.Guidance != "" || bd.NotApplicable != "" {
		b.WriteString("\nThen the cross-location test:\n\n" + bindingLine("test", bd, r.Snap.Workspace))
	}
	return b.String()
}

// dependenciesSection tells an agent that builds or tests to apply the other locations' unlanded branches before @build and @test, and to restore before it finishes. A coder works in the edited location only.
func dependenciesSection(r *RunFiles, coder bool) string {
	if len(r.Snap.Dependencies) == 0 {
		return ""
	}
	where := fmt.Sprintf("--bridge %s --run %s", shellQuote(r.Snap.Bridge), r.State.Run)
	var b strings.Builder
	for _, sd := range r.Snap.sides() {
		if coder && sd.Location != r.Snap.Location {
			continue
		}
		p := planSide(sd, r.Snap.Dependencies)
		if len(p.apply) == 0 {
			continue
		}
		var names []string
		for _, dep := range p.apply {
			names = append(names, dep.Location+" @ "+dep.label())
		}
		fmt.Fprintf(&b, "- side %s (applies %s): `ensign dependency apply %s --side %s`, then @build and @test there, then `ensign dependency restore %s --side %s`\n",
			sd.Location, strings.Join(names, ", "), where, sd.Location, where, sd.Location)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n## Dependencies\n\nThis run builds and tests against another location's unlanded branch. In each side below, run the apply command before @build or @test there, and the restore command before you finish, so the wiring is never committed:\n\n" + b.String()
}

func platformsLine(r *RunFiles) string {
	if len(r.Snap.Platforms) == 0 {
		return ""
	}
	return "\nDeclared platforms: " + strings.Join(r.Snap.Platforms, ", ") + "\n"
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
	b.WriteString(platformsLine(r))
	b.WriteString("\n## Work item\n\n" + original + "\n")
	return b.String()
}

func stagePrompt(r *RunFiles, d *Dispatch, s *Stage, original string, extra []string) (string, error) {
	var b strings.Builder
	b.WriteString(tagLine(r.State.Run, d) + "\n\n## Stage: " + s.ID + "\n\n")
	if d.Dim != "" && s.Parallel != nil && s.Parallel.Over == "dimensions" {
		fmt.Fprintf(&b, "Your focus: **%s**.\n\n%s\n", d.Dim, r.Snap.Briefs["briefs/review/"+d.Dim+".md"])
	} else if d.Dim != "" {
		ctx, err := itemContext(r, s, d.Dim, d)
		if err != nil {
			return "", err
		}
		b.WriteString(ctx + "\n")
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
	if s.Role == "tester" {
		b.WriteString(sidesSection(r))
	}
	if s.Role == "tester" || s.Role == "coder" {
		b.WriteString(dependenciesSection(r, s.Role == "coder"))
	}
	if s.Role == "reviewer" {
		b.WriteString(platformsLine(r))
	}
	b.WriteString("\n## Your brief\n\n" + original + "\n\n## Report\n\n" + reportInstructions(s) + "\n")
	if len(r.Snap.KnownSkills) > 0 && s.Curates != "document" {
		b.WriteString("\nIf you had to establish a fact about a technology itself — true on any project using it, not just this one — " +
			"add a line per fact to the report block: `fact: <skill> | <the fact> | <path:line or how you verified it>`. " +
			"The skills are: " + strings.Join(r.Snap.KnownSkills, ", ") + ". " +
			"If none fits, name the technology anyway; the fact is held for the user to decide whether it gets a skill.\n")
	}
	return b.String(), nil
}

// openRunByFlags opens the run named by --run, or the session's, from pairwise --bridge/--run flags. It returns nil when there is no such run.
func openRunByFlags(args []string, usage string) (*RunFiles, error) {
	dir, run := "", ""
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--bridge":
			dir = args[i+1]
		case "--run":
			run = args[i+1]
		default:
			return nil, fmt.Errorf("usage: %s", usage)
		}
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	bridge, err := findBridge(dir)
	if err != nil {
		return nil, err
	}
	if run != "" {
		r, err := openRun(bridge, run)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return r, err
	}
	return openSessionRun(bridge, os.Getenv("STARSHIP_SESSION"))
}

// leadScope is the scope whose stages the session or lead dispatches.
func leadScope(r *RunFiles) string {
	if lead := r.Snap.Pipeline.Stages[0]; lead.isLead() {
		return lead.ID
	}
	return ""
}

// cmdNext prints what the run's lead should dispatch next.
func cmdNext(args []string, out io.Writer) error {
	r, err := openRunByFlags(args, "ensign next [--bridge <dir>] [--run <id>]")
	if err != nil {
		return err
	}
	if r == nil {
		fmt.Fprintln(out, "There is no active run.")
		return nil
	}
	defer r.close()
	m := &machine{snap: r.Snap, st: r.State}
	scope := leadScope(r)
	for i := len(r.State.Dispatches) - 1; i >= 0; i-- {
		d := r.State.Dispatches[i]
		if !d.Done {
			continue
		}
		if s := m.stage(d.Stage); s != nil && plainStage(r.Snap, s) {
			fmt.Fprintf(out, "Stage %q (run by ensign) finished: %s. Its report: %s\n", d.Stage, d.Outcome, filepath.Join(r.Dir, "reports", d.ID+".md"))
		}
		break
	}
	if d := deadPlainOwner(r, m, scope); d != nil && !r.State.terminal() {
		runsAgain := "nothing is run again"
		if hasServicesDown(r.Snap) {
			runsAgain = "only its @services-down is run again, and its result is reported"
		}
		fmt.Fprintf(out, "Stage %q (run by ensign) died before it finished: its `ensign run-stage` is gone (killed, or the session or machine stopped), and its effect on the run is unknown. Run `ensign run-stage --bridge %s --run %s` to record it as an error; %s. Then rule on it as an error and run `ensign next`.\n", d.Stage, shellQuote(r.Snap.Bridge), r.State.Run, runsAgain)
	} else {
		fmt.Fprintln(out, m.expect(scope))
	}
	if !r.State.terminal() {
		fmt.Fprintln(out, "Budgets used: "+m.budgetLine())
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
			extra[canonicalBudget(name)] += v
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
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
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
	l, err := lockFolders(bridge)
	if err != nil {
		return err
	}
	defer l.Unlock()
	if err := folderHolder(bridge, r.Snap, r.State.Session, r.State.Run); err != nil {
		return err
	}
	m := &machine{snap: r.Snap, st: r.State}
	if err := m.resume(answer, extra); err != nil {
		return err
	}
	if err := r.save(); err != nil {
		return err
	}
	l.Unlock()
	if r.State.Sequence {
		if _, err := advanceSequence(bridge, r.State.Session); err != nil {
			return err
		}
	}
	lead := r.Snap.Pipeline.Stages[0]
	fmt.Fprintf(out, "run %s resumed. Dispatch its lead now: use the Agent tool with subagent_type %q and this work item as the prompt, first line included:\n\n%s\n\n%s\n",
		r.State.Run, lead.Role, spawnTag(r.State.Run), r.Snap.WorkItem)
	return nil
}

// shellQuote quotes a path for a POSIX shell when it needs it.
func shellQuote(p string) string {
	if strings.ContainsAny(p, " '\"$`\\&;|<>()*?[]{}!#~") {
		return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return p
}

// priorSessions lists the session ids, other than current, that records of a session
// transcript carry, latest first. A resumed transcript keeps the earlier session's records.
func priorSessions(transcript, current string) []string {
	f, err := os.Open(transcript)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.SessionID == "" || rec.SessionID == current {
			continue
		}
		if len(out) == 0 || out[0] != rec.SessionID {
			out = append([]string{rec.SessionID}, out...)
		}
	}
	return out
}

// claudePID returns CLAUDE_PID, which Claude Code gives hook commands and keeps across /clear, or "" when it is missing or not all digits: it becomes a file name.
func claudePID() string {
	pid := os.Getenv("CLAUDE_PID")
	for _, c := range pid {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return pid
}

// pidRecord returns the path of the record that holds the id of the session last started under pid. The name needs its dot: dropDeadLinks treats every dotless file in the folder as a session link.
func pidRecord(bridge, pid string) string {
	return filepath.Join(bridge, ".starship", "sessions", pid+".pid")
}

// processBaseline returns the connector and agents signatures the claude process pid started with: those recorded by its first SessionStart, or the given current ones, which it records, when there is no record or the source is startup (a new process that reused an old pid). A new process that reuses an old pid and starts with source resume reads the stale record, so it gets a false restart note until its next restart; telling that apart from an in-process /resume needs a process-instance identity.
func processBaseline(bridge, pid, source, connectors, agents string) (string, string) {
	file := filepath.Join(bridge, ".starship", "sessions", pid+".connectors")
	var base struct{ Connectors, Agents string }
	if data, err := os.ReadFile(file); err == nil && source != "startup" && json.Unmarshal(data, &base) == nil {
		return base.Connectors, base.Agents
	}
	data, _ := json.Marshal(struct{ Connectors, Agents string }{connectors, agents})
	writeFile(file, string(data))
	return connectors, agents
}

// carryBind binds a run to the session that continues its own; a variable so a test can fail it.
var carryBind = bindSession

// carryOverSession binds a restarted session to the live run of the session it continues.
// It assumes a resumed or compacted session's transcript still holds records
// stamped with the earlier session's id; a cleared session's does not, see below. A failure to read the earlier session's runs is
// returned for SessionStart to report; SessionStart never fails over this.
// A run whose setup file could not be written is still carried over, and that error is returned too.
// A sequence that could not be moved is returned too, and the runs are still carried; a run that could not be opened or bound, or whose rebind could not be saved, stays on the earlier session and is reported to this one, see reportRebindFailures; a run whose folder is gone is skipped. A run that stays is moved again only if a later session start or an agent hook happens to reach it; a run already reported by an earlier prior in this call is skipped in the later ones.
// A clear leaves no records under the earlier id in the new transcript, so prev, the id the session's CLAUDE_PID last started under, is tried first; it counts only for a clear to a different id, and then the task is carried even with no live run.
func carryOverSession(bridge string, in *HookInput, prev string) error {
	switch in.Source {
	case "resume", "compact", "clear":
	default:
		return nil
	}
	if in.SessionID == "" {
		return nil
	}
	linked := in.Source == "clear" && prev != "" && prev != in.SessionID
	if in.TranscriptPath == "" && !linked {
		return nil
	}
	if linked {
		copySessionTask(bridge, prev, in.SessionID)
	}
	var priors []string
	if in.TranscriptPath != "" {
		priors = priorSessions(in.TranscriptPath, in.SessionID)
	}
	if linked {
		kept := []string{prev}
		for _, id := range priors {
			if id != prev {
				kept = append(kept, id)
			}
		}
		priors = kept
	}
	var errs []error
	for _, old := range priors {
		if err := markMoved(bridge, old, in.SessionID); err != nil {
			errs = append(errs, err)
		}
	}
	strand := func(rest []string) {
		for _, old := range rest {
			errs = append(errs, strandedSequence(bridge, old, in.SessionID))
		}
	}
	if _, err := os.Stat(sessionFile(bridge, in.SessionID)); err == nil {
		strand(priors)
		return errors.Join(errs...)
	}
	reported := map[string]bool{}
	for i, old := range priors {
		if err := carrySequence(bridge, old, in.SessionID, reported); err != nil {
			errs = append(errs, err)
		}
		carried := false
		var setupErr error
		ids, err := sessionRuns(bridge, old)
		if err != nil {
			errs = append(errs, err)
			strand(priors[i+1:])
			return errors.Join(errs...)
		}
		var failed []rebindFail
		for _, id := range ids {
			if reported[id] {
				continue
			}
			r, err := openRun(bridge, id)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					failed = append(failed, rebindFail{id, err, rebindMayMove})
					reported[id] = true
				}
				continue
			}
			switch {
			case r.State.terminal():
			case r.State.Session == in.SessionID:
				carried = true
			default:
				bindErr := carryBind(bridge, in.SessionID, id)
				if bindErr != nil && !errors.Is(bindErr, errSetupFile) {
					failed = append(failed, rebindFail{id, bindErr, rebindMayMove})
					reported[id] = true
					break
				}
				if bindErr != nil {
					setupErr = bindErr
				}
				carried = true
				copySessionTask(bridge, old, in.SessionID)
				if r.State.Session != old {
					r.State.log("rebound", "", "", fmt.Sprintf("%s%s (%s)", reboundPrefix(r.State.Session), in.SessionID, in.Source))
				}
				r.State.Session = in.SessionID
				r.State.log("rebound", "", "", fmt.Sprintf("%s%s (%s)", reboundPrefix(old), in.SessionID, in.Source))
				if err := r.save(); err != nil {
					failed = append(failed, rebindFail{id, err, rebindSaveFailed})
				}
			}
			r.close()
		}
		// The queue takes the sequence lock, so it is called with no run lock held.
		errs = append(errs, reportRebindFailures(bridge, in.SessionID, failed))
		if carried {
			strand(priors[i+1:])
			return errors.Join(append(errs, setupErr)...)
		}
	}
	return errors.Join(errs...)
}

// stageRoleAnywhere reports whether role is a stage role, not free, in a live run's
// pipeline or in a pipeline file of the bridge's control plane and extensions. It also returns what it could not read, one line each (the runs, pipeline files, folders, or all of them when the bridge's own files can't be read), which stop it ruling the role out. A bridge with no bridge.yaml has none, except the runs when they can't be listed.
func stageRoleAnywhere(bridge, role string) (bool, []string) {
	if role == "" {
		return false, nil
	}
	isStage := func(p *Pipeline) bool {
		for _, fr := range p.FreeRoles {
			if fr == role {
				return false
			}
		}
		return p.hasStageRole(role)
	}
	var unread []string
	live, err := liveRuns(bridge)
	if err != nil {
		unread = append(unread, flatLine("the runs: "+err.Error()))
	}
	for _, ri := range live {
		if ri.snap.Pipeline != nil && isStage(ri.snap.Pipeline) {
			return true, nil
		}
	}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, unread
		}
		return false, append(unread, flatLine("all extensions and pipelines: "+err.Error()))
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return false, append(unread, flatLine("all extensions and pipelines: "+err.Error()))
	}
	glob := func(dir string) []string {
		found, err := filesIn(dir, ".yaml")
		if err != nil {
			unread = append(unread, flatLine(fmt.Sprintf("%s: %v", dir, err)))
		}
		return found
	}
	files := glob(filepath.Join(cp, "pipelines"))
	dirs, derr := extensionDirs(bridge, cp)
	if derr != nil {
		unread = append(unread, flatLine("all extensions: "+derr.Error()))
	}
	for _, dir := range dirs {
		files = append(files, glob(filepath.Join(dir, "pipelines"))...)
	}
	for _, f := range files {
		var p Pipeline
		if err := readYAML(f, &p); err != nil {
			unread = append(unread, err.Error())
		} else if isStage(&p) {
			return true, nil
		}
	}
	return false, unread
}

// readOnlyRole reports whether the definition in the bridge's .claude/agents, the file Claude Code uses, of the one agent named role, grants only Read, Grep, Glob and Bash. It fails closed: no file, more than one, a file that can't be read, no tools line, an empty list or any other tool is not read-only.
func readOnlyRole(bridge, role string) bool {
	if role == "" || strings.ContainsAny(role, `/\:`) || strings.Contains(role, "..") {
		return false
	}
	files, err := filesIn(filepath.Join(bridge, ".claude", "agents"), ".md")
	if err != nil {
		return false
	}
	var data []byte
	matches := 0
	for _, f := range files {
		d, err := os.ReadFile(f)
		if err != nil {
			return false
		}
		name, err := agentName(f, d)
		if err != nil {
			return false
		}
		if name == role {
			data = d
			matches++
		}
	}
	if matches != 1 {
		return false
	}
	m := frontmatterToolsLoc(data)
	if m == nil {
		return false
	}
	value := strings.TrimSpace(string(data[m[4]:m[5]]))
	if value == "" {
		return false
	}
	// A continuation line would give the agent tools this line does not show.
	if parsed, err := frontmatterToolsValue(role, data); err != nil || parsed != value {
		return false
	}
	for _, tool := range strings.Split(value, ",") {
		switch strings.TrimSpace(tool) {
		case "Read", "Grep", "Glob", "Bash":
		default:
			return false
		}
	}
	return true
}
