// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Snapshot is the frozen input of a run. Hooks read only this, never the layers.
type Snapshot struct {
	Run          string               `json:"run"`
	Task         string               `json:"task"`
	Bridge       string               `json:"bridge"`
	ControlPlane string               `json:"control_plane"`
	Workspace    string               `json:"workspace"`
	Location     string               `json:"location,omitempty"`
	Subtask      string               `json:"subtask,omitempty"`
	Sides        []Side               `json:"sides,omitempty"`    // the locations the test stage covers, the edited one first
	Narrowed     bool                 `json:"narrowed,omitempty"` // Sides is fewer than the edited location and its dependents
	Cross        map[string]Binding   `json:"cross,omitempty"`    // the cross-location bindings, when there are several sides
	Dependencies []Dependency         `json:"dependencies,omitempty"`
	WorkItem     string               `json:"work_item"`
	Rules        []Rule               `json:"rules,omitempty"` // the project rules WorkItem carries
	Start        string               `json:"start,omitempty"`
	Pipeline     *Pipeline            `json:"pipeline"`
	Bindings     map[string]Binding   `json:"bindings"`
	Briefs       map[string]string    `json:"briefs"` // relative path -> contents, frozen with the run
	Events       map[string][]Handler `json:"events,omitempty"`
	Skills       []SkillDelivery      `json:"skills,omitempty"`
	KnownSkills  []string             `json:"known_skills,omitempty"` // every skill name a fact may be filed under
	Project      string               `json:"project,omitempty"`
	Platforms    []string             `json:"platforms,omitempty"`
	// Documentation is "off" when the documentation stage was turned off; older snapshots read empty, which is on.
	Documentation string   `json:"documentation,omitempty"`
	Extensions    []string `json:"extensions,omitempty"`
	Created       string   `json:"created"`
}

type Dispatch struct {
	ID          string `json:"id"`
	Stage       string `json:"stage"`
	Scope       string `json:"scope"`
	Dim         string `json:"dim,omitempty"`
	Role        string `json:"role"`
	CallerAgent string `json:"caller_agent,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"` // RFC3339: the agent's latest hook call
	Done        bool   `json:"done"`
	Locked      bool   `json:"locked,omitempty"` // run-stage holds the dispatch lock while it runs; unset for a dispatch from an ensign that took none
	Outcome     string `json:"outcome,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// Fields holds the report's other lines (files, rules, facts …).
	Fields map[string][]string `json:"fields,omitempty"`
	// StopBlocks is the legacy count of refused stops, read only to migrate old state.
	StopBlocks int `json:"stop_blocks,omitempty"`
	// StopsLeft is the refusals the agent has left before the run fails; nil only in state saved before it existed.
	StopsLeft *int   `json:"stops_left,omitempty"`
	Handback  string `json:"handback,omitempty"` // the message of the agent's SubagentHandback call
	Usage     *Usage `json:"usage,omitempty"`    // from the agent's transcript
	// DocsBase is a documentation dispatch's dirty paths with content hashes, taken when it was spawned; state saved before it existed reads empty.
	DocsBase map[string]string `json:"docs_base,omitempty"`
	// FinishedSeq is State.Clock when the dispatch's report was recorded; 0 while it runs.
	FinishedSeq int `json:"finished_seq,omitempty"`
	// Reconciled is what the Perforce reconcile before a test stage opened, for the lead; "" when it opened nothing.
	Reconciled string `json:"reconciled,omitempty"`
}

type ScopeState struct {
	Stage    string            `json:"stage"`              // current stage id; "" once the scope is finished
	Branches map[string]string `json:"branches,omitempty"` // dimension -> dispatch id, for a parallel stage
}

// ResumePoint is where an ask-user run picks up again.
type ResumePoint struct {
	Scope     string `json:"scope"`
	Stage     string `json:"stage"`
	Budget    string `json:"budget,omitempty"`     // the budget that ran out
	EdgeSpend bool   `json:"edge_spend,omitempty"` // it was spent by a transition, not by entering
	Reset     string `json:"reset,omitempty"`      // a reset still owed by that transition
}

type HistoryEntry struct {
	At       string `json:"at"`
	Kind     string `json:"kind"` // dispatch, denied, outcome, enter, finish, blocked
	Stage    string `json:"stage,omitempty"`
	Dispatch string `json:"dispatch,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type State struct {
	Run      string                 `json:"run"`
	Session  string                 `json:"session,omitempty"` // the Claude Code session that owns the run
	Status   string                 `json:"status"`            // pending, running, success, error, ask-user
	Reason   string                 `json:"reason,omitempty"`
	Approved bool                   `json:"approved"`
	Spent    map[string]int         `json:"spent"`
	Scopes   map[string]*ScopeState `json:"scopes"`
	// StartChild is the --start stage inside a lead, applied when the lead's scope opens.
	StartChild string         `json:"start_child,omitempty"`
	Started    bool           `json:"started,omitempty"`      // run.start has fired
	Sync       bool           `json:"sync,omitempty"`         // run @sync before run.start
	Base       string         `json:"base,omitempty"`         // git-catalog: HEAD when the run started
	Extra      map[string]int `json:"extra,omitempty"`        // budget added on resume
	Resume     *ResumePoint   `json:"resume,omitempty"`       // where an ask-user run continues
	Answers    []string       `json:"answers,omitempty"`      // the user's answers on resume
	Document   string         `json:"document,omitempty"`     // document-curate: the one document this run is narrowed to
	Rebuild    bool           `json:"rebuild,omitempty"`      // style-generate: rebuild from every source
	RetryOf    string         `json:"retry_of,omitempty"`     // the failed run this one retries; its leftover changes are kept
	Carried    []string       `json:"carried,omitempty"`      // a retry: the paths already dirty when it started, committed with the coder's files
	StaleOK    bool           `json:"stale_branch,omitempty"` // github-pr: run on a task branch that is behind the default branch
	StartHead  string         `json:"start_head,omitempty"`   // the workspace's commit when the run started (git)
	VcsState   string         `json:"vcs_state,omitempty"`    // the Perforce workspace's state when the run started
	Transcript string         `json:"transcript,omitempty"`   // the session's transcript; agents' sit beside it
	Notes      []string       `json:"notes,omitempty"`        // what handlers left for the report, e.g. a pull request
	Finished   bool           `json:"finished,omitempty"`     // the end-of-run steps have run
	FinishedAt int64          `json:"finished_at,omitempty"`  // unix nanoseconds when the run last ended, any outcome; check Status for a success
	Dispatches []*Dispatch    `json:"dispatches"`
	History    []HistoryEntry `json:"history"`

	Tasks map[string]string `json:"tasks,omitempty"` // background task id -> id of the agent that launched it

	Sequence bool `json:"sequence,omitempty"` // the run is a step of its session's sequence

	// Gate is the reason a passing pipeline gave, while the run waits at the closing gate for its lead to record whether the goal is met with `ensign run finish`; "" when the run is not there.
	Gate string `json:"gate,omitempty"`
	// Clock orders events within the run: it counts up as stage reports are recorded and as the lead calls ensign.
	Clock int `json:"clock,omitempty"`
	// LeadSeq is Clock at each lead scope's latest `ensign next` or `ensign run finish`.
	LeadSeq map[string]int `json:"lead_seq,omitempty"`
}

// tick advances the run's clock and returns it.
func (st *State) tick() int {
	st.Clock++
	return st.Clock
}

// atGate reports a run that waits at the closing gate.
func (st *State) atGate() bool { return st.Gate != "" && !st.terminal() }

// recordLeadCall notes that the lead of scope has just called ensign.
func (st *State) recordLeadCall(scope string) {
	if st.LeadSeq == nil {
		st.LeadSeq = map[string]int{}
	}
	st.LeadSeq[scope] = st.tick()
}

// reportedSinceLeadCall reports that a stage of scope finished after the scope's lead last called ensign, so the lead has not been told of it.
func (st *State) reportedSinceLeadCall(scope string) bool {
	last := 0
	for _, d := range st.Dispatches {
		if d.Scope == scope {
			last = max(last, d.FinishedSeq)
		}
	}
	return last > st.LeadSeq[scope]
}

// queued reports a sequence run that has not started: it is no session's waiting run.
func (st *State) queued() bool { return st.Sequence && st.Status == "pending" }

// stopBudget is how many refused stops and handbacks an agent gets before the run fails: the last one fails it.
const stopBudget = 3

// spendStop counts one refusal against the dispatch and reports whether the budget is now used up.
func (d *Dispatch) spendStop() bool {
	*d.StopsLeft--
	return *d.StopsLeft <= 0
}

func (st *State) terminal() bool {
	return st.Status == "success" || st.Status == "error" || st.Status == "ask-user"
}

// note keeps something for the run's report and the task's history.
func (st *State) note(s string) {
	st.Notes = append(st.Notes, s)
	st.log("note", "", "", s)
}

func (st *State) log(kind, stage, dispatch, detail string) {
	st.History = append(st.History, HistoryEntry{
		At: time.Now().Format(time.RFC3339), Kind: kind, Stage: stage, Dispatch: dispatch, Detail: detail,
	})
}

func (st *State) dispatch(id string) *Dispatch {
	for _, d := range st.Dispatches {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (st *State) byAgent(agentID string) *Dispatch {
	for _, d := range st.Dispatches {
		if agentID != "" && d.AgentID == agentID {
			return d
		}
	}
	return nil
}

func runsDir(bridge string) string { return filepath.Join(bridge, ".starship", "runs") }

func sessionFile(bridge, session string) string {
	return filepath.Join(bridge, ".starship", "sessions", session)
}

// RunFiles is an open run: its snapshot, its state, and the lock held while using them.
type RunFiles struct {
	Dir   string
	Snap  *Snapshot
	State *State
	lock  *flock.Flock
	warn  error // The session's setup file could not be written; the run is bound and the hook proceeds.
	// The open rebound the run, so it has state to save.
	rebound bool
}

// openRun locks and loads one run. A run whose folder is gone gives an error matching fs.ErrNotExist.
func openRun(bridge, id string) (*RunFiles, error) {
	dir := filepath.Join(runsDir(bridge), id)
	// flock creates its file, so a missing folder is caught first.
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	l := flock.New(filepath.Join(dir, "lock"))
	if err := l.Lock(); err != nil {
		return nil, fmt.Errorf("locking run %s: %w", id, err)
	}
	r := &RunFiles{Dir: dir, lock: l, Snap: &Snapshot{}, State: &State{}}
	if err := readRunFiles(dir, r.Snap, r.State); err != nil {
		l.Unlock()
		return nil, err
	}
	for _, d := range r.State.Dispatches {
		if d.StopsLeft == nil {
			left := max(stopBudget-d.StopBlocks, 1)
			d.StopsLeft, d.StopBlocks = &left, 0
		}
	}
	return r, nil
}

// readRunFiles reads a run's snapshot and state, folding the old budget name loop into coder-loop in both.
func readRunFiles(dir string, snap *Snapshot, st *State) error {
	err1 := readJSON(filepath.Join(dir, "pipeline.json"), snap)
	if err := errors.Join(err1, readJSON(filepath.Join(dir, "state.json"), st)); err != nil {
		return err
	}
	if snap.Pipeline != nil {
		if err := snap.Pipeline.canonicalizeBudgets(); err != nil {
			return err
		}
	}
	st.canonicalizeBudgets()
	return nil
}

// sessionRuns lists the ids of a session's runs, oldest first. An older session file holds one id. A session without a file has none; any other read error is returned.
func sessionRuns(bridge, session string) ([]string, error) {
	if session == "" {
		return nil, nil
	}
	data, err := readFileRetry(sessionFile(bridge, session))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the runs of session %s: %w", session, err)
	}
	var ids []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ids = append(ids, l)
		}
	}
	return ids, nil
}

// liveSessionRuns lists the ids of a session's runs that have not finished.
func liveSessionRuns(bridge, session string) ([]string, error) {
	var live []string
	ids, err := sessionRuns(bridge, session)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if runIsLive(bridge, id) {
			live = append(live, id)
		}
	}
	return live, nil
}

// runIsLive judges a run by state.json alone, retrying a not-found read only when pipeline.json shows the run exists, so a deleted run fails at once and a state.json mid-save is retried.
func runIsLive(bridge, id string) bool {
	var st State
	err := readRunState(filepath.Join(runsDir(bridge), id), &st)
	return err == nil && !st.terminal()
}

// readRunState reads a run folder's state.json, retrying a not-found read only when pipeline.json shows the run exists.
func readRunState(dir string, st *State) error {
	err := readJSON(filepath.Join(dir, "state.json"), st)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(filepath.Join(dir, "pipeline.json")); serr == nil {
			err = readExistingJSON(filepath.Join(dir, "state.json"), st)
		}
	}
	return err
}

// writeSessionRuns replaces a session's run ids; an empty list removes the file.
func writeSessionRuns(bridge, session string, ids []string) error {
	f := sessionFile(bridge, session)
	if len(ids) == 0 {
		err := retry(retryWindow, isTransientFileError, func() error { return os.Remove(f) })
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return renameRetry(tmp, f)
}

// editSessionRuns changes a session's run ids under the session file's lock.
func editSessionRuns(bridge, session string, edit func(ids []string) []string) error {
	f := sessionFile(bridge, session)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	l := flock.New(f + ".lock")
	if err := l.Lock(); err != nil {
		return err
	}
	defer l.Unlock()
	ids, err := sessionRuns(bridge, session)
	if err != nil {
		return err
	}
	return writeSessionRuns(bridge, session, edit(ids))
}

// unbindSession removes a run from a session's run set.
func unbindSession(bridge, session, run string) error {
	return editSessionRuns(bridge, session, func(ids []string) []string {
		return slices.DeleteFunc(ids, func(id string) bool { return id == run })
	})
}

// sessionRunID names the run a call from the session means: its one live run, else its latest. When several are live, it returns them in several.
func sessionRunID(bridge, session string) (id string, several []string, err error) {
	live, err := liveSessionRuns(bridge, session)
	if err != nil {
		return "", nil, err
	}
	if len(live) > 1 {
		return "", live, nil
	}
	if len(live) == 1 {
		return live[0], nil, nil
	}
	ids, err := sessionRuns(bridge, session)
	if err != nil {
		return "", nil, err
	}
	if len(ids) > 0 {
		return ids[len(ids)-1], nil, nil
	}
	return "", nil, nil
}

// openSessionRun opens the run a command from the session means when it names none with --run. It returns nil, nil when the session has none, and an error when several are live.
func openSessionRun(bridge, session string) (*RunFiles, error) {
	id, several, err := sessionRunID(bridge, session)
	if err != nil {
		return nil, err
	}
	if len(several) > 0 {
		return nil, fmt.Errorf("this session has several live runs (%s); pass --run <id> to name one", strings.Join(several, ", "))
	}
	return openRunIfExists(bridge, id)
}

// openSoleSessionRun is openSessionRun for hooks: with several live runs the session has no one run, so it returns nil, nil.
func openSoleSessionRun(bridge, session string) (*RunFiles, error) {
	id, _, err := sessionRunID(bridge, session)
	if err != nil {
		return nil, err
	}
	return openRunIfExists(bridge, id)
}

// openRunIfExists opens a run, or returns nil, nil when there is none to open: its folder, state.json or pipeline.json is gone, or it is a terminal run (success, error or ask-user) whose pipeline can't be used. A link to such a terminal run must not fail its session's hooks; the session's next bind drops it.
func openRunIfExists(bridge, id string) (*RunFiles, error) {
	if id == "" {
		return nil, nil
	}
	r, err := openRun(bridge, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil && finishedWithBadPipeline(filepath.Join(runsDir(bridge), id)) {
		return nil, nil
	}
	return r, err
}

// finishedWithBadPipeline reports whether a run's state reads and is terminal while its files do not read as a whole.
func finishedWithBadPipeline(dir string) bool {
	var st State
	if readRunState(dir, &st) != nil || !st.terminal() {
		return false
	}
	var snap Snapshot
	var st2 State
	return readRunFiles(dir, &snap, &st2) != nil
}

// findAgentRun finds the live run an agent belongs to: the run its transcript's dispatch tag names, then any live run with a dispatch bound to its agent id. It returns nil, nil when there is none. Each candidate is opened and closed in turn, so no two run locks are held.
func findAgentRun(bridge string, in *HookInput) (*RunFiles, error) {
	if in.AgentID == "" {
		return nil, nil
	}
	try := func(id string, match func(*State) bool) (*RunFiles, error) {
		r, err := openRun(bridge, id)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if r.State.terminal() || !match(r.State) {
			r.close()
			return nil, nil
		}
		return r, nil
	}
	var r *RunFiles
	var err error
	if run, d := agentTag(in); run != "" {
		r, err = try(run, func(st *State) bool { return st.dispatch(d) != nil })
	}
	if r == nil && err == nil {
		var live []runInfo
		if live, err = liveRuns(bridge); err != nil {
			return nil, err
		}
		for _, ri := range live {
			if ri.state.byAgent(in.AgentID) == nil {
				continue
			}
			if r, err = try(ri.id, func(st *State) bool { return st.byAgent(in.AgentID) != nil }); r != nil || err != nil {
				break
			}
		}
	}
	return r, err
}

// agentTag reads the dispatch tag from the calling agent's transcript.
func agentTag(in *HookInput) (run, dispatch string) {
	tp := in.AgentTranscriptPath
	if tp == "" {
		tp = agentTranscript(in.TranscriptPath, in.AgentID)
	}
	return readTag(tp)
}

// openAgentRun finds the live run an agent belongs to and rebinds the session to it. It returns nil, nil when there is none.
func openAgentRun(bridge string, in *HookInput) (*RunFiles, error) {
	if in.AgentID == "" || in.SessionID == "" {
		return nil, nil
	}
	r, err := findAgentRun(bridge, in)
	if r == nil || err != nil {
		return nil, err
	}
	if r.State.Session == in.SessionID {
		return r, nil
	}
	live, err := liveSessionRuns(bridge, in.SessionID)
	if err != nil {
		r.close()
		return nil, err
	}
	if len(live) > 0 {
		return r, nil
	}
	if err := rebindRun(bridge, r, in.SessionID, fmt.Sprintf("found through agent %s", in.AgentID)); err != nil {
		return nil, err
	}
	return r, nil
}

// reboundPrefix begins the detail of every "rebound" history entry, so a run's earlier owners can be read back from its history.
func reboundPrefix(old string) string {
	return "session " + old + " -> "
}

// reboundAwayFrom reports whether the run's history shows it was rebound away from the session.
func (st *State) reboundAwayFrom(session string) bool {
	prefix := reboundPrefix(session)
	for _, h := range st.History {
		if h.Kind == "rebound" && strings.HasPrefix(h.Detail, prefix) {
			return true
		}
	}
	return false
}

// rebindRun binds the session to the open run r and makes it the run's owner. A setup file error is tolerated and left in r.warn; any other error closes r.
func rebindRun(bridge string, r *RunFiles, session, how string) error {
	bindErr := bindSession(bridge, session, r.State.Run)
	if bindErr != nil && !errors.Is(bindErr, errSetupFile) {
		r.close()
		return bindErr
	}
	finishRebind(bridge, r, session, how)
	r.warn = bindErr
	return nil
}

// finishRebind makes the session the owner of the open run r, which the session's file already lists. It does not touch the session's file or setup file.
func finishRebind(bridge string, r *RunFiles, session, how string) {
	copySessionTask(bridge, r.State.Session, session)
	r.State.log("rebound", "", "", fmt.Sprintf("%s%s, %s", reboundPrefix(r.State.Session), session, how))
	r.State.Session = session
	r.rebound = true
}

// openSpawnRun finds the run the session's own spawn of a lead belongs to: the one named by the tag the prompt begins with, when it is one of the session's live runs. It returns nil, nil when there is none.
func openSpawnRun(bridge string, in *HookInput) (*RunFiles, error) {
	if in.ToolName != "Agent" && in.ToolName != "Task" {
		return nil, nil
	}
	prompt, _ := in.ToolInput["prompt"].(string)
	m := spawnTagRe.FindStringSubmatch(prompt)
	if m == nil {
		return nil, nil
	}
	live, err := liveSessionRuns(bridge, in.SessionID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(live, m[1]) {
		return nil, nil
	}
	r, err := openRun(bridge, m[1])
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.State.terminal() {
		r.close()
		return nil, nil
	}
	return r, nil
}

// openHookRun opens the run a hook call is checked against. An agent's own live run comes first, even when its session is bound to another run, and that binding is left alone; the session is rebound when it has no live run, or when its one live run has the agent but is owned by another session that the history doesn't show it was rebound away from. When the session has several live runs, the agent's run is likewise given the session as owner if the session's file lists it, without touching the session's file or setup file. Otherwise it is the session's run, when it has just one live run. The session's run is closed before another is opened, so no two run locks are held.
func openHookRun(bridge string, in *HookInput) (*RunFiles, error) {
	live, err := liveSessionRuns(bridge, in.SessionID)
	if err != nil {
		return nil, err
	}
	if len(live) > 1 {
		if in.AgentID == "" {
			return openSpawnRun(bridge, in)
		}
		r, err := findAgentRun(bridge, in)
		if r == nil || err != nil {
			return r, err
		}
		if slices.Contains(live, r.State.Run) && r.State.Session != in.SessionID && !r.State.reboundAwayFrom(in.SessionID) {
			finishRebind(bridge, r, in.SessionID, fmt.Sprintf("found through agent %s", in.AgentID))
		}
		return r, nil
	}
	r, err := openSoleSessionRun(bridge, in.SessionID)
	if err != nil || in.AgentID == "" {
		return r, err
	}
	if r == nil {
		return openAgentRun(bridge, in)
	}
	if run, d := agentTag(in); (run == r.State.Run && r.State.dispatch(d) != nil) || r.State.byAgent(in.AgentID) != nil {
		if r.State.Session != in.SessionID && !r.State.terminal() && !r.State.reboundAwayFrom(in.SessionID) {
			if err := rebindRun(bridge, r, in.SessionID, fmt.Sprintf("found through agent %s", in.AgentID)); err != nil {
				return nil, err
			}
		}
		return r, nil
	}
	r.close()
	ar, err := findAgentRun(bridge, in)
	if ar != nil || err != nil {
		return ar, err
	}
	return openSoleSessionRun(bridge, in.SessionID)
}

// markRetired marks an open run as error and releases it. The lock is released before any
// delete, since a folder with an open file cannot be removed on Windows.
func markRetired(r *RunFiles, reason string) error {
	if !r.State.terminal() {
		r.State.FinishedAt = time.Now().UnixNano()
	}
	r.State.Status, r.State.Reason = "error", reason
	err := r.save()
	r.close()
	return err
}

// removeRun deletes a retired run's folder. A failed delete leaves a run that is already
// terminal, so it is only a warning.
func removeRun(dir string) {
	removeRunBy(dir, time.Now().Add(retryWindow))
}

// removeRunBy is removeRun with a retry deadline the caller sets, so gcRuns can build each delete's deadline from a budget that only failed deletes spend.
func removeRunBy(dir string, deadline time.Time) {
	if err := retry(time.Until(deadline), isTransientFileError, func() error { return os.RemoveAll(dir) }); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete run folder %s: %v\n", dir, err)
	}
}

// retireRun marks an open run as error and deletes its folder.
func retireRun(r *RunFiles, reason string) error {
	dir := r.Dir
	if err := markRetired(r, reason); err != nil {
		return err
	}
	removeRun(dir)
	return nil
}

// copySessionTask gives a session without a task file the task of the session it replaces,
// since the status header reads it. The old session's files stay in place.
func copySessionTask(bridge, from, to string) {
	dst := sessionFile(bridge, to) + ".task"
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if data, err := os.ReadFile(sessionFile(bridge, from) + ".task"); err == nil {
		os.WriteFile(dst, data, 0o644)
	}
}

// errSetupFile marks a bindSession error where the run is bound to the session but its setup file could not be written.
var errSetupFile = errors.New("setup file")

// bindSession records that a run belongs to a session, and writes the setup file with the run's project environment for the session's agents. Runs of the session that have finished are dropped from its set. A setup-file failure is returned, wrapping errSetupFile, after the run is bound.
func bindSession(bridge, session, run string) error {
	err := editSessionRuns(bridge, session, func(ids []string) []string {
		keep := []string{}
		for _, id := range ids {
			if id != run && runIsLive(bridge, id) {
				keep = append(keep, id)
			}
		}
		return append(keep, run)
	})
	if err != nil {
		return err
	}
	var snap Snapshot
	if err := readJSON(filepath.Join(runsDir(bridge), run, "pipeline.json"), &snap); err != nil {
		return fmt.Errorf("%w: the session's setup file was not written because the run's pipeline could not be read: %w", errSetupFile, err)
	}
	if err := writeSetupFile(bridge, session, snap.Project); err != nil {
		return fmt.Errorf("%w: %w", errSetupFile, err)
	}
	return nil
}

// busyError is a refusal because something is under way that will end by itself: a publish step or a landing waits for it instead of failing.
type busyError struct{ err error }

func (e *busyError) Error() string { return e.err.Error() }

func (e *busyError) Unwrap() error { return e.err }

// folderBusyError is a run that can't start because another live run holds a code folder it needs, or because a landing or an update is under way there (landing or update, with no run named). held says how the other run holds it: it works in the folder, or it only reads it (applies a branch from it); wanted says the same for the run that can't start.
type folderBusyError struct {
	run, status, folder string
	held, wanted        folderMode
	otherSession        bool
	landing             bool
	update              bool
}

type folderMode int

const (
	folderWorks folderMode = iota // builds, tests or writes in the folder
	folderReads                   // applies a branch from the folder and does not change it
)

func (e *folderBusyError) Error() string {
	var msg string
	switch {
	case e.landing:
		return fmt.Sprintf("a landing is under way in the code folder %s; start this once it has finished", e.folder)
	case e.update:
		return fmt.Sprintf("an update is under way in the code folder %s; start this once it has finished", e.folder)
	case e.held == folderWorks && e.wanted == folderWorks:
		msg = fmt.Sprintf("run %s (%s) already holds the code folder %s; one run at a time works in a folder, so start this once that run has finished", e.run, e.status, e.folder)
	case e.held == folderWorks:
		msg = fmt.Sprintf("run %s (%s) works in the code folder %s, which this run applies a branch from; start this once that run has finished", e.run, e.status, e.folder)
	default:
		msg = fmt.Sprintf("run %s (%s) applies a branch from the code folder %s, which this run works in; start this once that run has finished", e.run, e.status, e.folder)
	}
	if e.otherSession {
		msg += "; another session is doing work there"
	}
	return msg
}

// lockFolders serializes the check that a folder is free with the approval that takes it. Take a run's lock or a publish lock before this one, never the reverse.
func lockFolders(bridge string) (*flock.Flock, error) {
	dir := filepath.Join(bridge, ".starship")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, "folders.lock"))
	if err := l.Lock(); err != nil {
		return nil, err
	}
	return l, nil
}

// folderLockFile is the lock file of a kind of operation ("landings" or "updates") in a code folder, named by the folder as sameFolder compares it. These files are never removed: unlinking a flock file can give two holders. There is one per folder ever landed in, updated or probed, so their number is bounded by the number of folders.
func folderLockFile(bridge, kind, folder string) string {
	sum := sha256.Sum256([]byte(foldPath(resolveEditFolder(folder, runtime.GOOS), runtime.GOOS)))
	return filepath.Join(bridge, ".starship", kind, hex.EncodeToString(sum[:])+".lock")
}

func landingLockFile(bridge, folder string) string { return folderLockFile(bridge, "landings", folder) }

func updateLockFile(bridge, folder string) string { return folderLockFile(bridge, "updates", folder) }

// recordLockFolder writes the folder a lock file belongs to beside it, as <lock>.folder, so landingInside can find the locks held inside a folder. The folder file is plain, never locked, and written under lockFolders.
func recordLockFolder(lock, folder string) error {
	return os.WriteFile(lock+".folder", []byte(resolveEditFolder(folder, runtime.GOOS)), 0o644)
}

// landingInside reports the first folder strictly inside folder that a landing or an update holds, and which ("a landing" or "an update"); "" when none does. The caller holds lockFolders.
func landingInside(bridge, folder string) (string, string, error) {
	top := resolveEditFolder(folder, runtime.GOOS)
	for _, kind := range []struct{ dir, what string }{{"landings", "a landing"}, {"updates", "an update"}} {
		entries, err := readDirIfAny(filepath.Join(bridge, ".starship", kind.dir))
		if err != nil {
			return "", "", err
		}
		for _, e := range entries {
			rec, ok := strings.CutSuffix(e.Name(), ".lock.folder")
			if !ok {
				continue
			}
			data, err := os.ReadFile(filepath.Join(bridge, ".starship", kind.dir, e.Name()))
			if err != nil {
				continue
			}
			inner := string(data)
			if inner == "" || !pathWithin(inner, top, runtime.GOOS) || sameFolder(inner, top, runtime.GOOS) {
				continue
			}
			if held, err := lockHeld(filepath.Join(bridge, ".starship", kind.dir, rec+".lock"), kind.what, inner); err != nil || held {
				return inner, kind.what, err
			}
		}
	}
	return "", "", nil
}

// lockLanding marks a landing under way in a code folder until the returned func is called. The lock is shared, so landings in one folder (two tasks, or a publish command's child under its parent) don't refuse each other; a run starting there is refused by landingUnderWay, and a landing is refused while an update is under way there. Both the acquire and the probe run under lockFolders, so a probe never makes an acquire fail. Take it before checking that the landing's task has no live run.
func lockLanding(bridge, folder string) (func(), error) {
	fl, err := lockFolders(bridge)
	if err != nil {
		return nil, err
	}
	defer fl.Unlock()
	if updating, err := updateUnderWay(bridge, folder); err != nil {
		return nil, err
	} else if updating {
		return nil, &busyError{fmt.Errorf("an update is under way in the code folder %s; wait for it to finish", folder)}
	}
	lock := landingLockFile(bridge, folder)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	if err := recordLockFolder(lock, folder); err != nil {
		return nil, err
	}
	l := flock.New(lock)
	if ok, err := l.TryRLock(); err != nil {
		return nil, fmt.Errorf("locking the landing in %s: %w", folder, err)
	} else if !ok {
		return nil, &busyError{fmt.Errorf("the landing lock of %s is held exclusively", folder)}
	}
	return func() { l.Unlock() }, nil
}

// lockUpdate marks an update under way in a code folder until the returned func is called. It is exclusive, and refused while a landing or another update is under way there: the update switches the folder's branch, and a landing would read the wrong current branch. A run starting there is refused by folderHolder. The acquire and the probes run under lockFolders. Take it before checking that no run holds the folder.
func lockUpdate(bridge, folder string) (func(), error) {
	fl, err := lockFolders(bridge)
	if err != nil {
		return nil, err
	}
	defer fl.Unlock()
	if landing, err := landingUnderWay(bridge, folder); err != nil {
		return nil, err
	} else if landing {
		return nil, fmt.Errorf("a landing is under way in the code folder %s; wait for it to finish", folder)
	}
	lock := updateLockFile(bridge, folder)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	if err := recordLockFolder(lock, folder); err != nil {
		return nil, err
	}
	l := flock.New(lock)
	if ok, err := l.TryLock(); err != nil {
		return nil, fmt.Errorf("locking the update in %s: %w", folder, err)
	} else if !ok {
		return nil, fmt.Errorf("an update is already under way in the code folder %s; wait for it to finish", folder)
	}
	return func() { l.Unlock() }, nil
}

// landingUnderWay reports whether a landing holds a code folder. The caller holds lockFolders.
func landingUnderWay(bridge, folder string) (bool, error) {
	return lockHeld(landingLockFile(bridge, folder), "a landing", folder)
}

// updateUnderWay reports whether an update holds a code folder. The caller holds lockFolders.
func updateUnderWay(bridge, folder string) (bool, error) {
	return lockHeld(updateLockFile(bridge, folder), "an update", folder)
}

// lockHeld reports whether anyone holds the lock file, by taking it exclusively and letting go.
func lockHeld(lock, what, folder string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return false, err
	}
	l := flock.New(lock)
	ok, err := l.TryLock()
	if err != nil {
		return false, fmt.Errorf("checking for %s in %s: %w", what, folder, err)
	}
	if ok {
		l.Unlock()
	}
	return !ok, nil
}

// inRunsSpelling names the resolved folder around, which is f, inside f or around f, the way the run spelled f: f joined with the tail, or f trimmed by the components around lacks. It returns around when that spelling is not the same folder.
func inRunsSpelling(f, around string) string {
	resolved := resolveEditFolder(f, runtime.GOOS)
	if around == resolved {
		return f
	}
	candidate := around
	if rel, err := filepath.Rel(resolved, around); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		candidate = filepath.Join(f, rel)
	} else if rel, err := filepath.Rel(around, resolved); err == nil {
		candidate = filepath.Clean(f)
		for range strings.Split(rel, string(filepath.Separator)) {
			candidate = filepath.Dir(candidate)
		}
	}
	if sameFolder(candidate, around, runtime.GOOS) {
		return candidate
	}
	return around
}

// sameFolder reports whether two code folders are one, as the edit guard compares paths.
func sameFolder(a, b, goos string) bool {
	return foldPath(resolveEditFolder(a, goos), goos) == foldPath(resolveEditFolder(b, goos), goos)
}

// foldersOverlap reports whether two code folders are one, or one is inside the other, as the edit guard compares paths.
func foldersOverlap(a, b, goos string) bool {
	a, b = resolveEditFolder(a, goos), resolveEditFolder(b, goos)
	return pathWithin(a, b, goos) || pathWithin(b, a, goos)
}

// folderHolds are the code folders a run takes. It works in every folder it builds, tests or writes into: its workspace and each side's. It only reads the checkouts of the dependency branches it applies, unless it works in them too.
func folderHolds(snap *Snapshot) (works, reads []string) {
	for _, sd := range snap.sides() {
		if sd.Workspace != "" {
			works = append(works, sd.Workspace)
		}
	}
	for _, d := range snap.Dependencies {
		if d.Path != "" && !slices.ContainsFunc(works, func(w string) bool { return sameFolder(d.Path, w, runtime.GOOS) }) {
			reads = append(reads, d.Path)
		}
	}
	return works, reads
}

// folderHolder finds the run, other than those in skip, that holds a code folder the snapshot's run needs: an approved run that has not finished, in the same folder or one inside or around it. Failing that, a landing or an update under way in one of the folders, a folder around it or a folder inside it holds it, whether the run works in the folder or only reads it. The caller holds lockFolders. A run that works in a folder excludes every other run that works in it or reads it; runs that only read a folder share it. It returns nil when the folders are free. session is the session of the run that wants them.
func folderHolder(bridge string, snap *Snapshot, session string, skip ...string) error {
	myWorks, myReads := folderHolds(snap)
	if len(myWorks) == 0 && len(myReads) == 0 {
		return nil
	}
	find := func(mine, theirs []string) string {
		for _, m := range mine {
			for _, t := range theirs {
				if foldersOverlap(m, t, runtime.GOOS) {
					return t
				}
			}
		}
		return ""
	}
	live, err := liveRuns(bridge)
	if err != nil {
		return err
	}
	for _, ri := range live {
		if !ri.state.Approved || slices.Contains(skip, ri.id) || ri.snap.Workspace == "" {
			continue
		}
		theirWorks, theirReads := folderHolds(&ri.snap)
		other := ri.state.Session != "" && ri.state.Session != session
		busy := func(folder string, held, wanted folderMode) error {
			return &folderBusyError{run: ri.id, status: ri.state.Status, folder: folder, held: held, wanted: wanted, otherSession: other}
		}
		if f := find(myWorks, theirWorks); f != "" {
			return busy(f, folderWorks, folderWorks)
		}
		if f := find(myWorks, theirReads); f != "" {
			return busy(f, folderReads, folderWorks)
		}
		if f := find(myReads, theirWorks); f != "" {
			return busy(f, folderWorks, folderReads)
		}
	}
	for _, f := range append(slices.Clone(myWorks), myReads...) {
		around, what, err := landingAround(bridge, f)
		if err != nil {
			return err
		}
		if around == "" {
			around, what, err = landingInside(bridge, f)
			if err != nil {
				return err
			}
		}
		if around != "" {
			around = inRunsSpelling(f, around)
			return &folderBusyError{folder: around, landing: what == "a landing", update: what == "an update"}
		}
	}
	return nil
}

type runInfo struct {
	id    string
	snap  Snapshot
	state State
}

// runClass is how readRunInfo found a run folder.
type runClass int

const (
	runReadable           runClass = iota // both files read
	runNoState                            // no state.json: a creation gap or leftover, which gc owns
	runFinishedNoPipeline                 // a terminal state (success, error or ask-user) and a pipeline.json that can't be read
	runStateUnreadable                    // state.json exists and can't be read
	runPipelineUnreadable                 // a state that is not terminal and a pipeline.json that can't be read; state.json is written second, so this is no creation gap
)

// readRunInfo reads one run folder into ri and says which of the cases it is; the error is the read failure for the last two.
func readRunInfo(dir string, ri *runInfo) (runClass, error) {
	if err := readRunState(dir, &ri.state); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return runNoState, nil
		}
		return runStateUnreadable, err
	}
	if err := readJSON(filepath.Join(dir, "pipeline.json"), &ri.snap); err != nil {
		if ri.state.terminal() {
			return runFinishedNoPipeline, nil
		}
		return runPipelineUnreadable, err
	}
	return runReadable, nil
}

// readRunClass is readRunInfo for `ensign run clear` and the advice about it: a pipeline.json that parses but fails budget canonicalization, as openRun finds it, counts as one that can't be read.
func readRunClass(dir string, ri *runInfo) (runClass, error) {
	class, err := readRunInfo(dir, ri)
	if class != runReadable || ri.snap.Pipeline == nil {
		return class, err
	}
	if err := ri.snap.Pipeline.canonicalizeBudgets(); err != nil {
		if ri.state.terminal() {
			return runFinishedNoPipeline, err
		}
		return runPipelineUnreadable, err
	}
	return class, nil
}

// clearAccepts says whether `ensign run clear` ends a run of this class: one whose state.json can't be read, a live one whose pipeline.json can't, or an ask-user one, which can't resume without it. A run that has finished (success or error) holds nothing.
func clearAccepts(class runClass, st *State) bool {
	switch class {
	case runStateUnreadable, runPipelineUnreadable:
		return true
	case runFinishedNoPipeline:
		return st.Status == "ask-user"
	}
	return false
}

// finishedNoPipelineFacts is what a success or error run with a pipeline.json that can't be read means for the bridge, as the sentences openRunError and `ensign run clear` give.
func finishedNoPipelineFacts(st *State) string {
	msg := fmt.Sprintf("has finished (%s) and blocks nothing bridge-wide; only its pipeline.json can't be read.", st.Status)
	if finishedRun(st) {
		msg += " The cleanup removes it once it is past keep-runs."
	}
	if st.Status != "success" && !retiredWithoutFault(st) {
		msg += " A new passing run of its task lifts its hold on a publish."
	}
	return msg
}

// scanRuns reads every run in the bridge, newest first, without locking. unreadable holds the finished runs whose pipeline.json can't be read, which listRuns leaves out; their snap is empty. A run that can't be ruled out as live fails the scan: one whose state.json can't be read, or a non-terminal one whose pipeline.json can't be read. Skipping it would hide it from the checks that refuse while a run is live.
func scanRuns(bridge string) (runs, unreadable []runInfo, err error) {
	entries, err := os.ReadDir(runsDir(bridge))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || plainFileOnPath(runsDir(bridge)) != "" {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading the runs: %w", err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].IsDir() {
			continue
		}
		dir := filepath.Join(runsDir(bridge), entries[i].Name())
		var ri runInfo
		ri.id = entries[i].Name()
		class, err := readRunInfo(dir, &ri)
		switch class {
		case runReadable:
			runs = append(runs, ri)
		case runFinishedNoPipeline:
			ri.snap = Snapshot{}
			unreadable = append(unreadable, ri)
		case runStateUnreadable, runPipelineUnreadable:
			return nil, nil, fmt.Errorf("reading the runs: run %s can't be ruled out as live: %v; `ensign run clear %s` clears it, or repair the folder %s by hand", ri.id, err, ri.id, dir)
		}
	}
	return runs, unreadable, nil
}

// listRuns is scanRuns without the finished runs that have no readable pipeline.json: a finished run holds nothing, and a folder with no state.json is skipped.
func listRuns(bridge string) ([]runInfo, error) {
	runs, _, err := scanRuns(bridge)
	return runs, err
}

func (r *RunFiles) save() error {
	return writeJSON(filepath.Join(r.Dir, "state.json"), r.State)
}

func (r *RunFiles) close() { r.lock.Unlock() }

func readJSON(path string, v any) error {
	data, err := readFileRetry(path)
	if err != nil {
		return err
	}
	return unmarshalFile(path, data, v)
}

func unmarshalFile(path string, data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// writeJSON replaces the file atomically, so a crash never leaves half a state file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return renameRetry(tmp, path)
}

// runOpts is what `ensign run` and `ensign sequence run` accept.
type runOpts struct {
	task, start, pipeline, project string
	location, subtask              string
	sides, dependencies            []string
	dry, sync, rebuild, staleOK    bool
	document                       string
	// A retry inside a sequence: the failed run's id and base, its rules (kept as frozen), and the "## Current state" section that ends the new work item.
	retryOf, retryBase, currentState string
	rules                            []Rule
}

// parseRunArgs reads the flags shared by `ensign run` and `ensign sequence run`; cmd names the command in the usage line.
func parseRunArgs(cmd string, args []string) (runOpts, error) {
	var o runOpts
	usage := fmt.Errorf("usage: %s (<project>/<task> | --pipeline <name> [--project <p>]) [--location <name>] [--subtask <name>] [--sides <a,b,...>] [--dependency <location>=<branch>] [--start <stage>] [--sync] [--rebuild] [--document <name>] [--stale-branch] [--dry-run]", cmd)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			o.dry = true
		case "--sync":
			o.sync = true
		case "--rebuild":
			o.rebuild = true
		case "--stale-branch":
			o.staleOK = true
		case "--pipeline", "--project", "--location", "--subtask", "--sides", "--dependency", "--document":
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s needs a value", args[i])
			}
			switch args[i] {
			case "--pipeline":
				o.pipeline = args[i+1]
			case "--project":
				o.project = args[i+1]
			case "--dependency":
				if _, _, err := parseDependencyFlag(args[i+1]); err != nil {
					return o, err
				}
				o.dependencies = append(o.dependencies, args[i+1])
			case "--sides":
				o.sides = nil
				for _, n := range strings.Split(args[i+1], ",") {
					if n = strings.TrimSpace(n); n != "" {
						o.sides = append(o.sides, n)
					}
				}
				if len(o.sides) == 0 {
					return o, fmt.Errorf("--sides needs location names, comma-separated")
				}
			case "--subtask":
				o.subtask = args[i+1]
			case "--document":
				o.document = args[i+1]
			default:
				o.location = args[i+1]
			}
			i++
		case "--start":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--start needs a stage")
			}
			i++
			o.start = args[i]
		default:
			if strings.HasPrefix(args[i], "-") || o.task != "" {
				return o, usage
			}
			o.task = args[i]
		}
	}
	if o.task == "" && o.pipeline == "" {
		return o, usage
	}
	return o, nil
}

// isDocumentOver reports whether a parallel stage's list is the declared documents.
func isDocumentOver(over string) bool {
	return over == "pending-documents"
}

// eachStage calls f on every stage in list, nested stages included.
func eachStage(list []*Stage, f func(*Stage)) {
	for _, s := range list {
		f(s)
		eachStage(s.Stages, f)
	}
}

// checkDocumentFlag is why --document cannot narrow this run: the document is not declared, or no stage of the pipeline is over documents.
func checkDocumentFlag(c *Config, name string) error {
	if _, err := findDeclaredDocument(c.Bridge, name); err != nil {
		return fmt.Errorf("--document: %v", err)
	}
	over := false
	eachStage(c.Pipeline.Stages, func(s *Stage) {
		if s.Parallel != nil && isDocumentOver(s.Parallel.Over) {
			over = true
		}
	})
	if !over {
		return fmt.Errorf("--document: pipeline %s has no stage over pending-documents", c.Pipeline.Name)
	}
	return nil
}

// curationAgentFiles checks that every curating stage's role has its agent file, which Claude Code needs to dispatch it. A missing file is linked when no run is live; a dry run only reports it.
func curationAgentFiles(bridge string, stages []*Stage, dry bool, out io.Writer) error {
	agentsDir := filepath.Join(bridge, ".claude", "agents")
	hasAgent := func(role string) bool {
		_, matches, _ := agentFilesNamed(bridge, role)
		return matches > 0
	}
	var missing string
	var broken error
	eachStage(stages, func(s *Stage) {
		if s.Curates == "" || s.Role == "" || missing != "" || broken != nil || hasAgent(s.Role) {
			return
		}
		own := filepath.Join(agentsDir, s.Role+".md")
		d, err := os.ReadFile(own)
		if err == nil {
			_, err = agentName(own, d)
		}
		if err != nil && !os.IsNotExist(err) {
			broken = fmt.Errorf("the agent file .claude/agents/%s.md for role %s is broken: %v", s.Role, s.Role, err)
			return
		}
		missing = s.Role
	})
	if broken != nil {
		return broken
	}
	if missing == "" {
		return nil
	}
	rel := ".claude/agents/" + missing + ".md"
	if dry {
		fmt.Fprintf(out, "%s is missing; a real run would run ensign link to create it\n", rel)
		return nil
	}
	_, statErr := os.Stat(agentsDir)
	live, err := liveRuns(bridge)
	if err == nil && len(live) > 0 {
		err = fmt.Errorf("run %s is active", live[0].id)
	}
	if err == nil {
		err = link(bridge, false, nil)
	}
	if err == nil {
		if !hasAgent(missing) {
			err = fmt.Errorf("link did not create it")
		}
	}
	if err != nil {
		return fmt.Errorf("the agent file %s for role %s is missing (%v); run ensign link once no run is live", rel, missing, err)
	}
	if statErr != nil {
		fmt.Fprintf(out, "linked the missing agent file %s; restart claude before dispatching, since .claude/agents did not exist\n", rel)
	} else {
		fmt.Fprintf(out, "linked the missing agent file %s; claude picks it up within a few seconds\n", rel)
	}
	return nil
}

// cmdRun implements `ensign run <project>/<task> [--start <stage>] [--dry-run]`.
func cmdRun(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "retire" {
		return cmdRunRetire(args[1:], out)
	}
	if len(args) > 0 && args[0] == "stop" {
		return cmdRunStop(args[1:], out, os.Stderr)
	}
	if len(args) > 0 && args[0] == "clear" {
		return cmdRunClear(args[1:], out)
	}
	if len(args) > 0 && args[0] == "finish" {
		return cmdRunFinish(args[1:], out, os.Stderr)
	}
	o, err := parseRunArgs("ensign run", args)
	if err != nil {
		return err
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if err := refuseNoCode(bridge, o.task); err != nil {
		return err
	}
	if !o.dry {
		if err := writesPendingRefusal(bridge, os.Getenv("STARSHIP_SESSION")); err != nil {
			return err
		}
	}
	_, err = createRun(bridge, o, false, out)
	return err
}

// cmdRunFinish is `ensign run finish --goal met|not-met [<why>]`: the lead's judgment at the closing gate. met finishes the run as a success, not-met as the error goal-not-met with the why as its reason.
func cmdRunFinish(args []string, out, stderr io.Writer) error {
	const usage = "ensign run finish [--bridge <dir>] [--run <id>] --goal met|not-met [<why>]"
	var flags, why []string
	goal := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--goal" && i+1 < len(args):
			goal = args[i+1]
			i++
		case (args[i] == "--bridge" || args[i] == "--run") && i+1 < len(args):
			flags = append(flags, args[i], args[i+1])
			i++
		case strings.HasPrefix(args[i], "--"):
			return fmt.Errorf("usage: %s", usage)
		default:
			why = append(why, args[i])
		}
	}
	reason := strings.TrimSpace(strings.Join(why, " "))
	if goal != "met" && goal != "not-met" {
		return fmt.Errorf("usage: %s", usage)
	}
	if goal == "not-met" && reason == "" {
		return errors.New("--goal not-met needs the reason the goal is not met: ensign run finish --goal not-met \"<why>\"")
	}
	r, err := openRunByFlags(flags, usage)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("there is no active run")
	}
	defer r.close()
	m := &machine{snap: r.Snap, st: r.State}
	if r.State.terminal() {
		return terminalRunError(r.State.Run, r.State.Status, "finish")
	}
	if !r.State.atGate() {
		return fmt.Errorf("run %s is not at its closing gate, so it can't be finished: %s", r.State.Run, m.expect(leadScope(r)))
	}
	r.State.recordLeadCall(leadScope(r))
	if goal == "met" {
		m.finish(Transition{Finish: "success"}, r.State.Gate)
	} else {
		m.finish(Transition{Error: "goal-not-met"}, reason)
	}
	// Saved first: finishing may read the run's state from disk, and a failure below must not lose the judgment.
	if err := r.save(); err != nil {
		return err
	}
	if err := finishIfTerminal(r, false, stderr); err != nil {
		fmt.Fprintln(stderr, "ensign: could not finish the run:", err)
	}
	if err := r.save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Run %s finished: %s (%s).", r.State.Run, r.State.Status, r.State.Reason)
	if len(r.State.Notes) > 0 {
		fmt.Fprint(out, " "+strings.Join(r.State.Notes, " ")+".")
	}
	fmt.Fprintln(out, " Stop and report this.")
	return nil
}

// openRunError is the error a command gives when openRun fails. Its advice depends on how readRunClass classes the folder: a run is pointed at `ensign run clear` only when clearAccepts says it takes it: a state.json that can't be read, a live run whose pipeline.json can't be read or fails budget canonicalization, or an ask-user run in that case. A folder that is not there is no run, and a path that is not a folder is not a run either.
func openRunError(bridge, id string, err error) error {
	dir := filepath.Join(runsDir(bridge), id)
	fi, serr := os.Stat(dir)
	if serr != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no run %s", id)
		}
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("no run %s", id)
	}
	var ri runInfo
	class, _ := readRunClass(dir, &ri)
	switch {
	case class == runFinishedNoPipeline && ri.state.Status == "ask-user":
		return fmt.Errorf("run %s can't be opened (%v): it is waiting on a question and can't resume, because its pipeline.json can't be read; `ensign run clear %s` ends it as a failed run. A new passing run of its task lifts its hold on a publish.", id, err, id)
	case clearAccepts(class, &ri.state):
		return fmt.Errorf("run %s can't be opened (%w); if its files are damaged, `ensign run clear %s` ends it", id, err, id)
	case class == runFinishedNoPipeline:
		return fmt.Errorf("run %s can't be opened (%v): it %s", id, err, finishedNoPipelineFacts(&ri.state))
	case class == runNoState:
		return fmt.Errorf("run %s can't be opened (%w): it has no state.json and blocks nothing", id, err)
	}
	return fmt.Errorf("run %s can't be opened (%w)", id, err)
}

// clearedReason is the reason of a run ended by `ensign run clear`. It must not start with a noFaultPrefixes entry: the run counts as failed.
const clearedReason = "cleared with ensign run clear: its files could not be read"

// cmdRunClear is `ensign run clear <run>`: it ends a run whose files can't be read, the runs listRuns fails on, and an ask-user run that can't resume for want of its pipeline.json, as a failed run and keeps its folder. It needs no session, since a damaged run can keep a session from getting ready.
func cmdRunClear(args []string, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ensign run clear <run>")
	}
	id := args[0]
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("%q is not a run id", id)
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	dir := filepath.Join(runsDir(bridge), id)
	// flock creates its file, so a missing folder is caught first.
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no run %s", id)
		}
		return err
	}
	// run-stage drops the run lock while a stage runs and holds a dispatch lock instead.
	names := []string{"lock"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, "dispatch-") && strings.HasSuffix(n, ".lock") {
			names = append(names, n)
		}
	}
	for _, n := range names {
		l := flock.New(filepath.Join(dir, n))
		ok, err := l.TryLock()
		if err != nil {
			return fmt.Errorf("locking run %s: %w", id, err)
		}
		if !ok {
			return fmt.Errorf("run %s is still open (%s is held), so it was not changed", id, n)
		}
		defer l.Unlock()
	}
	ri := runInfo{id: id}
	class, cause := readRunClass(dir, &ri)
	if !clearAccepts(class, &ri.state) {
		switch class {
		case runNoState:
			if _, err := os.Stat(filepath.Join(dir, "pipeline.json")); err == nil {
				return fmt.Errorf("run %s has no state.json: it blocks nothing, and a cleanup removes the folder once its pipeline.json is %d minutes old", id, int(pipelineOnlyAge.Minutes()))
			}
			return fmt.Errorf("run %s has no state.json and no pipeline.json: it blocks nothing and no cleanup removes it; if nothing is creating it, the folder can be deleted by hand", id)
		case runReadable:
			if ri.state.Status == "ask-user" {
				return fmt.Errorf("run %s is waiting on a question and its files can be read; there is nothing to clear", id)
			}
			if ri.state.terminal() {
				return fmt.Errorf("run %s has finished (%s) and its files can be read; there is nothing to clear", id, ri.state.Status)
			}
			return fmt.Errorf("run %s is a live run with readable files; `ensign run retire %s` ends it for another session, `ensign run stop %s` for its own", id, id, id)
		}
		return fmt.Errorf("run %s %s", id, finishedNoPipelineFacts(&ri.state))
	}
	askUser := class == runFinishedNoPipeline
	statePath := filepath.Join(dir, "state.json")
	now := time.Now().UnixNano()
	if class == runStateUnreadable {
		bad, err := readFileRetry(statePath)
		if err != nil {
			return fmt.Errorf("run %s: could not keep a copy of state.json (%v) after %v; nothing was changed", id, err, cause)
		}
		if err := os.WriteFile(statePath+".corrupt", bad, 0o644); err != nil {
			return err
		}
	}
	// Whatever state.json says, no session may keep the id: a link to a run that can't be opened fails every hook of its session.
	links, err := readDirIfAny(filepath.Join(bridge, ".starship", "sessions"))
	if err != nil {
		return err
	}
	for _, e := range links {
		if e.IsDir() || strings.Contains(e.Name(), ".") {
			continue
		}
		ids, err := sessionRuns(bridge, e.Name())
		if err != nil {
			return err
		}
		if slices.Contains(ids, id) {
			if err := unbindSession(bridge, e.Name(), id); err != nil {
				return err
			}
		}
	}
	if class == runPipelineUnreadable || askUser {
		st := ri.state
		st.Status, st.Reason = "error", clearedReason
		if askUser {
			// It ended when it asked: a later FinishedAt would put it after a pass that already lifted its hold.
			st.resetPosition()
		} else {
			st.FinishedAt = now
		}
		if err := writeJSON(statePath, st); err != nil {
			return err
		}
	} else {
		st := State{Run: id, Status: "error", Reason: clearedReason, Started: true, FinishedAt: now, Spent: map[string]int{}, Scopes: map[string]*ScopeState{}, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}
		if err := writeJSON(statePath, st); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Cleared run %s: it is now over, as a failed run. Its folder is kept at %s", id, dir)
	if class == runStateUnreadable {
		fmt.Fprint(out, ", and the state.json it had is kept as state.json.corrupt there")
	}
	fmt.Fprintf(out, ". The code folders were left as they are. Anything the run started, such as services, was not stopped, because its files can't be read.")
	if askUser {
		fmt.Fprintf(out, " It counts as a failed run of its task from when it asked, so a passing run of the task after that lifts its hold on a publish.\n")
	} else {
		fmt.Fprintf(out, " It counts as a failed run of its task, so the task must pass again before it can publish.\n")
	}
	return nil
}

// terminalRunError is the refusal of a verb on a terminal run. An ask-user run has not finished: it waits on a question.
func terminalRunError(id, status, verb string) error {
	if status == "ask-user" {
		return fmt.Errorf("run %s is waiting on a question, so there is nothing to %s; the way on is `ensign resume --answer <text> --run %s`", id, verb, id)
	}
	return fmt.Errorf("run %s has finished (%s); there is nothing to %s", id, status, verb)
}

// cmdRunRetire is `ensign run retire <run>`: it ends a live run of another session at the user's request, so the code folders it holds are free. The run's folder and the code folder are left as they are.
func cmdRunRetire(args []string, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ensign run retire <run>")
	}
	session := os.Getenv("STARSHIP_SESSION")
	if session == "" {
		return errors.New("retiring a run needs the session it is called from, and STARSHIP_SESSION is not set")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	r, err := openRun(bridge, args[0])
	if err != nil {
		return openRunError(bridge, args[0], err)
	}
	switch {
	case r.State.terminal():
		r.close()
		return terminalRunError(args[0], r.State.Status, "retire")
	case r.State.Session == session && !(r.State.queued() && seqNoLongerHolds(bridge, session, r.State.Run)):
		r.close()
		return fmt.Errorf("run %s belongs to this session; only a run of another session can be retired. To end this session's own run, use `ensign run stop %s`", args[0], args[0])
	}
	owner, id := r.State.Session, r.State.Run
	works, _ := folderHolds(r.Snap)
	if !r.State.Approved {
		works = nil
	}
	if err := markRetired(r, fmt.Sprintf("retired at the user's request by session %s", session)); err != nil {
		return err
	}
	if owner != "" {
		if err := unbindSession(bridge, owner, id); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Retired run %s of session %s. Its work in the code folders was left as it is.\n", id, owner)
	for _, f := range works {
		fmt.Fprintf(out, "Free now: %s\n", f)
	}
	return nil
}

// seqNoLongerHolds reports whether the session's sequence file can be read and none of its steps names the run, as after `ensign sequence clear`.
func seqNoLongerHolds(bridge, session, run string) bool {
	sf, err := readSeqErr(bridge, session)
	if err != nil {
		return false
	}
	for _, seq := range sf.Sequences {
		for _, st := range seq.Steps {
			for _, sr := range st.runs() {
				if sr.Run == run {
					return false
				}
			}
		}
	}
	return true
}

// cmdRunStop is `ensign run stop <run>`: the session ends its own live run at once. Agents still working are ignored: every open dispatch is marked done so their late reports are dropped, and the session is unbound so their hooks find no run. The run's folder and the code folders are left as they are.
func cmdRunStop(args []string, out, stderr io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ensign run stop <run>")
	}
	session := os.Getenv("STARSHIP_SESSION")
	if session == "" {
		return errors.New("stopping a run needs the session it is called from, and STARSHIP_SESSION is not set")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	r, err := openRun(bridge, args[0])
	if err != nil {
		return openRunError(bridge, args[0], err)
	}
	defer r.close()
	switch {
	case r.State.terminal():
		return terminalRunError(args[0], r.State.Status, "stop")
	case r.State.Session != session:
		return fmt.Errorf("run %s belongs to another session; only this session's own run can be stopped", args[0])
	case r.State.queued():
		return fmt.Errorf("run %s has not started; `ensign sequence halt` or `ensign sequence drop` ends it", args[0])
	}
	id := r.State.Run
	works, _ := folderHolds(r.Snap)
	if !r.State.Approved {
		works = nil
	}
	m := &machine{snap: r.Snap, st: r.State}
	advisory := stopAdvisory(r, m)
	if !advisory {
		m.fail("cancelled", fmt.Sprintf("stopped at the user's request by session %s", session))
	}
	if err := r.save(); err != nil {
		return err
	}
	if err := unbindSession(bridge, session, id); err != nil {
		fmt.Fprintln(stderr, "ensign: could not unbind the session from the stopped run:", err)
	}
	// Before the dispatches are marked done: only a running dispatch has an owner to check.
	notices := stopOrphanedServices(r, m, time.Now().Add(recoveryDownLimit))
	for _, d := range r.State.Dispatches {
		d.Done = true
	}
	if err := finishIfTerminal(r, false, stderr); err != nil {
		fmt.Fprintln(stderr, "ensign: could not finish the stopped run:", err)
	}
	if err := r.save(); err != nil {
		return err
	}
	if advisory {
		fmt.Fprintf(out, "Finished run %s: %s %s.\n", id, r.State.Status, strings.Join(r.State.Notes, " "))
	} else {
		fmt.Fprintf(out, "Stopped run %s. Its work in the code folders was left as it is.\n", id)
	}
	fmt.Fprint(out, notices)
	for _, f := range works {
		fmt.Fprintf(out, "Free now: %s\n", f)
	}
	fmt.Fprintln(out, "Agents still working are ignored and their reports are dropped, but a stray agent may touch files briefly after the stop: check the workspace before the next run there.")
	return nil
}

// createRun validates and freezes a run. A queued run (a sequence step) belongs to the session but is not bound to it and replaces nothing. It returns nil for a dry run.
func createRun(bridge string, o runOpts, queued bool, out io.Writer) (*Snapshot, error) {
	task, start, pipeline, project := o.task, o.start, o.pipeline, o.project
	dry, sync, rebuild, staleOK := o.dry, o.sync, o.rebuild, o.staleOK
	if err := refuseNoCode(bridge, task); err != nil {
		return nil, err
	}
	pick, err := pickSubtask(bridge, o)
	if err != nil {
		return nil, err
	}
	location := o.location
	if pick != nil && pick.sub.Location != "" {
		location = pick.sub.Location
	}
	c, err := loadTarget(bridge, Target{Task: task, Project: project, Pipeline: pipeline, Location: location, Sides: o.sides, WithSides: true})
	if err != nil {
		return nil, err
	}
	if c.Task != "" {
		taskFile := filepath.Join(c.TaskDir, "task.md")
		_, body, err := splitFrontmatter(taskFile)
		if err != nil {
			return nil, fmt.Errorf("task %q: %v", c.Task, err)
		}
		if section(string(body), "Goal") == "" && section(string(body), "Scope") == "" {
			return nil, fmt.Errorf("task %s has no work item: its task file %s has neither a \"## Goal\" nor a \"## Scope\" section with text in it; add one and run again", c.Task, taskFile)
		}
	}
	if pick != nil {
		c.Subtask = pick.sub.Name
		c.WorkItem = pick.workItem(c.WorkItem)
	}
	if o.retryOf != "" {
		c.Rules = o.rules
	}
	if len(c.Rules) > 0 {
		c.WorkItem += "\n\n## Project rules\n\n"
		for _, r := range c.Rules {
			c.WorkItem += "- " + r.Text + "\n"
		}
	}
	c.WorkItem += o.currentState
	var auto []string
	if pick != nil {
		auto = c.automaticDependencies(bridge, pick.doneLocs, o.dependencies)
	}
	if err := c.resolveDependencies(bridge, o.dependencies, auto); err != nil {
		return nil, err
	}
	if err := c.Pipeline.canonicalizeBudgets(); err != nil {
		return nil, err
	}
	errs := validate(c)
	var docErr error
	if o.document != "" {
		docErr = checkDocumentFlag(c, o.document)
		if docErr != nil {
			errs = append(errs, docErr.Error())
		}
	}
	idle := errNothingToCurate
	if o.document != "" {
		idle = fmt.Errorf("document %s has nothing to curate: no pending proposals", o.document)
	}
	// A stage with an empty outcome treats "nothing to do" as no items; the run still needs one stage with something to do.
	var resolve func(list []*Stage)
	emptyStages, emptyOnly := 0, 0
	resolve = func(list []*Stage) {
		for _, s := range list {
			if s.Parallel != nil && s.Parallel.Over != "dimensions" {
				var items []string
				var err error
				if isDocumentOver(s.Parallel.Over) && o.document != "" {
					if docErr != nil {
						continue
					}
					items, err = documentItems(bridge, o.document)
					if errors.Is(err, errNothingToCurate) && s.Parallel.Empty == "" {
						err = idle
					}
				} else {
					items, err = generatorItems(c, s.Parallel.Over, rebuild)
				}
				if s.Parallel.Empty != "" {
					emptyStages++
					if errors.Is(err, errNothingToCurate) {
						err = nil
					}
					if err == nil && len(items) == 0 {
						emptyOnly++
					}
				}
				if err != nil {
					errs = append(errs, err.Error())
				}
				s.Dimensions = items
			}
			resolve(s.Stages)
		}
	}
	resolve(c.Pipeline.Stages)
	if emptyStages > 0 && emptyOnly == emptyStages {
		errs = append(errs, idle.Error())
	}
	if start != "" {
		if s, _, _ := c.Pipeline.find(start); s == nil {
			errs = append(errs, fmt.Sprintf("--start: no stage %q", start))
		} else if why := skipReason(s, c.Bindings, c.Documentation); why != "" {
			errs = append(errs, fmt.Sprintf("--start: stage %q would be skipped (%s), so the run would have nothing to do", start, why))
		} else if s.Parallel != nil && s.Parallel.Empty != "" && len(s.Dimensions) == 0 {
			errs = append(errs, fmt.Sprintf("--start: stage %q has no items, so the run would have nothing to do", start))
		}
	}
	if sync {
		if b, ok := c.Bindings["sync"]; !ok || (b.Command == "" && b.NotApplicable == "") {
			errs = append(errs, "--sync needs the @sync binding")
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("the run is not valid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	if err := curationAgentFiles(bridge, c.Pipeline.Stages, dry, out); err != nil {
		return nil, err
	}
	if !queued || dry {
		printSummary(out, c, start)
	}
	if dry {
		fmt.Fprintln(out, "\n(dry run: nothing created)")
		return nil, nil
	}

	session := os.Getenv("STARSHIP_SESSION")
	if !queued {
		if err := movedRefusal(bridge, session); err != nil {
			return nil, err
		}
	}
	if session != "" {
		if data, err := os.ReadFile(sessionFile(bridge, session) + ".pin"); err == nil {
			if started := strings.TrimSpace(string(data)); started != pinnedCommit(bridge) {
				return nil, fmt.Errorf("the bridge was updated to a new starship release after this session started; restart claude before starting a run")
			}
		}
	}
	unfinished := false
	if !queued && session != "" {
		var err error
		if unfinished, err = sequenceUnfinished(bridge, session); err != nil {
			return nil, err
		}
	}
	if unfinished {
		switch waiting, building := onlyWaitingSequenceUnfinished(bridge, session); {
		case building:
			return nil, fmt.Errorf("this session has a sequence waiting for go; add the run with ensign sequence run, or ensign sequence discard first")
		case waiting:
			return nil, fmt.Errorf("this session has a sequence waiting for go again; add the run with ensign sequence run, type go to approve it, or ensign sequence halt first")
		}
		return nil, fmt.Errorf("this session has a sequence in progress; add the run with ensign sequence run, or ensign sequence halt first")
	}
	var replace []string
	all, err := listRuns(bridge)
	if err != nil {
		return nil, err
	}
	for _, ri := range all {
		if queued || ri.state.terminal() || session == "" || ri.state.Session != session || ri.state.queued() {
			continue
		}
		if ri.state.Approved {
			return nil, fmt.Errorf("this session already has an active run, %s", ri.id)
		}
		replace = append(replace, ri.id)
	}

	if !queued {
		want := &Snapshot{Workspace: c.Workspace, Location: c.Location, Sides: c.Sides, Dependencies: c.Dependencies}
		l, err := lockFolders(bridge)
		if err != nil {
			return nil, err
		}
		err = folderHolder(bridge, want, session, replace...)
		l.Unlock()
		if err != nil {
			return nil, err
		}
		if o.retryOf == "" {
			if err := dirtyFolder(want); err != nil {
				return nil, err
			}
		}
	}

	known, err := knownSkills(bridge, c.ControlPlane, c.Project)
	if err != nil {
		return nil, err
	}
	label := strings.ReplaceAll(c.Task, "/", "-")
	if label == "" {
		label = strings.Trim(c.Project+"-"+c.Pipeline.Name, "-")
	}
	id, dir, err := newRunDir(bridge, time.Now().Format("20060102-150405")+"-"+label)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{
		Run:           id,
		Task:          c.Task,
		Bridge:        bridge,
		ControlPlane:  c.ControlPlane,
		Workspace:     c.Workspace,
		Location:      c.Location,
		Subtask:       c.Subtask,
		Sides:         c.Sides,
		Narrowed:      c.Narrowed,
		Cross:         c.Cross,
		Dependencies:  c.Dependencies,
		WorkItem:      c.WorkItem,
		Rules:         c.Rules,
		Start:         start,
		Pipeline:      c.Pipeline,
		Bindings:      c.Bindings,
		Briefs:        map[string]string{},
		Events:        c.Events,
		Extensions:    c.Extensions,
		Skills:        c.Skills,
		KnownSkills:   known,
		Project:       c.Project,
		Platforms:     c.Platforms,
		Documentation: c.Documentation,
		Created:       time.Now().Format(time.RFC3339),
	}
	for _, hs := range snap.Events {
		for i := range hs {
			if hs[i].Kind == "augment" && !strings.HasPrefix(hs[i].Text, "@") {
				f := filepath.Join(hs[i].Dir, filepath.FromSlash(hs[i].Text))
				if _, err := os.Stat(f); err != nil {
					f, _ = c.findFile(hs[i].Text)
				}
				data, err := os.ReadFile(f)
				if err != nil {
					return nil, err
				}
				hs[i].Text = string(data)
			}
		}
	}
	if err := freezeBriefs(c, snap); err != nil {
		return nil, err
	}
	st := &State{Run: snap.Run, Session: session, Status: "pending", Spent: map[string]int{}, Scopes: map[string]*ScopeState{},
		Sync: sync, Extra: map[string]int{}, Rebuild: rebuild, Document: o.document, StaleOK: staleOK, Sequence: queued,
		RetryOf: o.retryOf, Base: o.retryBase}
	first := c.Pipeline.Stages[0].ID
	if start != "" {
		_, _, scope := c.Pipeline.find(start)
		if scope == "" {
			first = start
		} else {
			first, st.StartChild = scope, start
		}
	}
	m := &machine{snap: snap, st: st}
	m.enter("", first)

	os.Mkdir(filepath.Join(dir, "scratch"), 0o755)
	if err := os.MkdirAll(filepath.Join(dir, "reports", "evidence"), 0o755); err != nil {
		return nil, err
	}
	if err := writeOnDemandSkills(dir, snap.Skills); err != nil {
		return nil, err
	}
	// The run's lock is held while its two files are written, so gc never takes a folder with only pipeline.json for a leftover while this creation is under way. It is released before anything below opens the run.
	created := flock.New(filepath.Join(dir, "lock"))
	if err := created.Lock(); err != nil {
		return nil, fmt.Errorf("locking run %s: %w", snap.Run, err)
	}
	err = writeJSON(filepath.Join(dir, "pipeline.json"), snap)
	if err == nil {
		err = writeJSON(filepath.Join(dir, "state.json"), st)
	}
	created.Unlock()
	if err != nil {
		return nil, err
	}
	var replaced []string
	for _, old := range replace {
		r, err := openRun(bridge, old)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.State.terminal() || r.State.Session != session {
			r.close()
			continue
		}
		if r.State.Approved {
			// Approved since the scan: leave it alone and withdraw the run just created.
			r.close()
			if nr, err := openRun(bridge, snap.Run); err == nil {
				retireRun(nr, "withdrawn: run "+old+" was approved while this run was being set up")
			}
			return nil, fmt.Errorf("this session already has an active run, %s", old)
		}
		if err := markRetired(r, "replaced by run "+snap.Run); err != nil {
			return nil, err
		}
		replaced = append(replaced, old)
	}
	var setupErr error
	if session != "" && !queued {
		if err := bindSession(bridge, session, snap.Run); errors.Is(err, errSetupFile) {
			setupErr = err
		} else if err != nil {
			return nil, err
		}
	}
	// Folders go only after the session is bound, so a "go" hook never finds it without a run.
	for _, old := range replaced {
		removeRun(filepath.Join(runsDir(bridge), old))
	}
	if queued {
		return snap, nil
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pipeline.json"))
	sum := sha256.Sum256(data)
	for _, old := range replaced {
		fmt.Fprintf(out, "\nreplaced waiting run %s\n", old)
	}
	fmt.Fprintf(out, "\nrun %s created (pipeline sha %s), waiting for go\n", snap.Run, hex.EncodeToString(sum[:])[:12])
	if setupErr != nil {
		fmt.Fprintf(out, "starship: %v\n", setupErr)
	}
	return snap, nil
}

// newRunDir creates a run's directory exclusively, adding -2, -3 … if the id is taken,
// so two runs started in the same second can never share one.
func newRunDir(bridge, base string) (string, string, error) {
	if err := os.MkdirAll(runsDir(bridge), 0o755); err != nil {
		return "", "", err
	}
	for n := 1; n < 100; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		dir := filepath.Join(runsDir(bridge), id)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return id, dir, nil
		}
		if !os.IsExist(err) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("could not allocate a run id for %s", base)
}

// freezeBriefs copies every brief the run can use into the snapshot.
func freezeBriefs(c *Config, snap *Snapshot) error {
	var walk func(list []*Stage) error
	walk = func(list []*Stage) error {
		for _, s := range list {
			paths := []string{}
			if s.Brief != "" {
				paths = append(paths, s.Brief)
			}
			if s.Parallel != nil && s.Parallel.Over == "dimensions" {
				for _, d := range s.Dimensions {
					paths = append(paths, "briefs/review/"+d+".md")
				}
			}
			for _, rel := range paths {
				f, err := c.findFile(rel)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				snap.Briefs[rel] = string(data)
			}
			if err := walk(s.Stages); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(c.Pipeline.Stages)
}

func printSummary(out io.Writer, c *Config, start string) {
	p := c.Pipeline
	fmt.Fprintf(out, "task       %s\nworkspace  %s\n", c.Task, c.Workspace)
	if c.Subtask != "" {
		fmt.Fprintf(out, "subtask    %s\n", c.Subtask)
	}
	if c.Location != "" {
		fmt.Fprintf(out, "location   %s\n", c.Location)
		if len(c.Sides) > 0 {
			names := sideNames(c.Sides)
			note := "default: " + c.Location + " and its dependents"
			if c.Narrowed {
				note = "narrowed"
			}
			fmt.Fprintf(out, "sides      %s (%s)\n", strings.Join(names, ", "), note)
		}
		for _, l := range dependencyLines(c.Sides, c.Dependencies) {
			fmt.Fprintf(out, "%s\n", l)
		}
	}
	fmt.Fprintf(out, "pipeline   %s — %s\n", p.Name, p.Summary)
	if start != "" {
		fmt.Fprintf(out, "start at   %s\n", start)
	}
	fmt.Fprintln(out, "\nstages")
	var walk func(list []*Stage, indent string)
	walk = func(list []*Stage, indent string) {
		for _, s := range list {
			extra := ""
			if s.Spend != "" {
				extra += " spend:" + s.Spend
			}
			if s.Parallel != nil {
				extra += fmt.Sprintf(" parallel over %v", s.Dimensions)
			}
			src := s.Source
			if !strings.HasPrefix(src, "inserted by") && !strings.HasPrefix(src, "replaced by") {
				src = "from " + src
			}
			fmt.Fprintf(out, "%s%-8s %-15s%s\n%s         %s\n", indent, s.ID, s.Role, extra, indent, src)
			for _, o := range s.Outcomes {
				fmt.Fprintf(out, "%s           %-9s → %s\n", indent, o.Name, o.Then)
			}
			walk(s.Stages, indent+"  ")
		}
	}
	walk(p.Stages, "  ")
	for k, v := range p.Sources {
		if strings.HasPrefix(k, "removed.") {
			fmt.Fprintf(out, "  (stage %s removed by %s)\n", strings.TrimPrefix(k, "removed."), v)
		}
	}

	fmt.Fprintln(out, "\nbudgets")
	for _, k := range sortedKeys(p.Budgets) {
		fmt.Fprintf(out, "  %-10s %d   from %s\n", k, p.Budgets[k], p.Sources["budgets."+k])
	}
	for _, k := range sortedKeys(p.OnExhausted) {
		fmt.Fprintf(out, "  on %s exhausted → %s   from %s\n", k, p.OnExhausted[k], p.Sources["on-exhausted."+k])
	}

	if len(c.Skills) > 0 {
		fmt.Fprintln(out, "\nskills")
		for _, sk := range c.Skills {
			fmt.Fprintf(out, "  %-12s %s, for %s\n", sk.Name, sk.Delivery, strings.Join(sk.Roles, ", "))
		}
	}
	fmt.Fprintln(out, "\nbindings")
	for _, k := range sortedKeys(c.Bindings) {
		b := c.Bindings[k]
		v := b.Command
		switch {
		case b.NotApplicable != "":
			v = "not applicable: " + b.NotApplicable
		case b.Guidance != "":
			v = "guidance: " + b.Guidance
		case len(b.Paths) > 0:
			v = "paths: " + strings.Join(b.Paths, ", ")
		}
		fmt.Fprintf(out, "  @%-8s %s   from %s\n", k, v, b.Source)
	}
	if len(c.Rules) > 0 {
		fmt.Fprintln(out, "\nrules")
		for _, r := range c.Rules {
			fmt.Fprintf(out, "  %s   from %s\n", r.Text, r.Source)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sideNames(sides []Side) []string {
	var names []string
	for _, sd := range sides {
		names = append(names, sd.Location)
	}
	return names
}

// canonicalizeBudgets folds the old budget name loop into coder-loop in a saved run's state.
func (st *State) canonicalizeBudgets() {
	for _, m := range []map[string]int{st.Spent, st.Extra} {
		if v, ok := m["loop"]; ok {
			delete(m, "loop")
			m["coder-loop"] += v
		}
	}
	if st.Resume != nil {
		st.Resume.Budget = canonicalBudget(st.Resume.Budget)
		st.Resume.Reset = canonicalBudget(st.Resume.Reset)
	}
}

// stopAdvisory finishes a run stopped during an advisory stage through the stage's advisory outcome, since the work before it is done and ensign cannot tell a hung agent from a slow one. Each document with no completed dispatch is noted as missing. It reports false when the current stage is not advisory.
func stopAdvisory(r *RunFiles, m *machine) bool {
	sc := r.State.Scopes[leadScope(r)]
	if sc == nil || sc.Stage == "" {
		return false
	}
	s := m.stage(sc.Stage)
	if s == nil || s.Advisory == "" || s.Parallel == nil {
		return false
	}
	for _, dim := range s.Dimensions {
		if id, ok := sc.Branches[dim]; ok {
			b := r.State.dispatch(id)
			if b.Done {
				continue
			}
			b.Done, b.Outcome = true, s.Advisory
		}
		r.State.noteOnce(missingNote(dim))
	}
	if t, _ := s.Outcomes.find(s.Advisory); t.Finish != "" {
		// A stop finishes the run: it does not park at the gate.
		r.State.log("outcome", s.ID, "", s.Advisory+" → "+t.String())
		m.finish(t, fmt.Sprintf("%s reported %s", s.ID, s.Advisory))
		return true
	}
	m.apply(leadScope(r), s, s.Advisory)
	return true
}
