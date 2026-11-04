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
	Extensions   []string             `json:"extensions,omitempty"`
	Created      string               `json:"created"`
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
	Rebuild    bool           `json:"rebuild,omitempty"`      // style-generate: rebuild from every source
	RetryOf    string         `json:"retry_of,omitempty"`     // the failed run this one retries; its leftover changes are kept
	Carried    []string       `json:"carried,omitempty"`      // a retry: the paths already dirty when it started, committed with the coder's files
	StaleOK    bool           `json:"stale_branch,omitempty"` // github-pr: run on a task branch that is behind the default branch
	StartHead  string         `json:"start_head,omitempty"`   // the workspace's commit when the run started (git)
	Transcript string         `json:"transcript,omitempty"`   // the session's transcript; agents' sit beside it
	Notes      []string       `json:"notes,omitempty"`        // what handlers left for the report, e.g. a pull request
	Finished   bool           `json:"finished,omitempty"`     // the end-of-run steps have run
	FinishedAt int64          `json:"finished_at,omitempty"`  // unix nanoseconds when the run last ended, any outcome; check Status for a success
	Dispatches []*Dispatch    `json:"dispatches"`
	History    []HistoryEntry `json:"history"`

	Tasks map[string]string `json:"tasks,omitempty"` // background task id -> id of the agent that launched it

	Sequence bool `json:"sequence,omitempty"` // the run is a step of its session's sequence
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
	data, err := os.ReadFile(sessionFile(bridge, session))
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
		var st State
		if readJSON(filepath.Join(runsDir(bridge), id, "state.json"), &st) == nil && !st.terminal() {
			live = append(live, id)
		}
	}
	return live, nil
}

// writeSessionRuns replaces a session's run ids; an empty list removes the file.
func writeSessionRuns(bridge, session string, ids []string) error {
	f := sessionFile(bridge, session)
	if len(ids) == 0 {
		err := os.Remove(f)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f)
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

func openRunIfExists(bridge, id string) (*RunFiles, error) {
	if id == "" {
		return nil, nil
	}
	r, err := openRun(bridge, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return r, err
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
		for _, ri := range liveRuns(bridge) {
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
	copySessionTask(bridge, r.State.Session, session)
	r.State.log("rebound", "", "", fmt.Sprintf("%s%s, %s", reboundPrefix(r.State.Session), session, how))
	r.State.Session = session
	r.warn = bindErr
	r.rebound = true
	return nil
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

// openHookRun opens the run a hook call is checked against. An agent's own live run comes first, even when its session is bound to another run, and that binding is left alone; the session is rebound when it has no live run, or when its one live run has the agent but is owned by another session that the history doesn't show it was rebound away from. Otherwise it is the session's run, when it has just one live run. The session's run is closed before another is opened, so no two run locks are held.
func openHookRun(bridge string, in *HookInput) (*RunFiles, error) {
	live, err := liveSessionRuns(bridge, in.SessionID)
	if err != nil {
		return nil, err
	}
	if len(live) > 1 {
		if in.AgentID == "" {
			return openSpawnRun(bridge, in)
		}
		return findAgentRun(bridge, in)
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
	if err := os.RemoveAll(dir); err != nil {
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
			var st State
			if id != run && readJSON(filepath.Join(runsDir(bridge), id, "state.json"), &st) == nil && !st.terminal() {
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

// folderBusyError is a run that can't start because another live run holds a code folder it needs. held says how the other run holds it: it works in the folder, or it only reads it (applies a branch from it); wanted says the same for the run that can't start.
type folderBusyError struct {
	run, status, folder string
	held, wanted        folderMode
	otherSession        bool
}

type folderMode int

const (
	folderWorks folderMode = iota // builds, tests or writes in the folder
	folderReads                   // applies a branch from the folder and does not change it
)

func (e *folderBusyError) Error() string {
	var msg string
	switch {
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

// lockFolders serializes the check that a folder is free with the approval that takes it. Take a run's lock before this one, never the reverse.
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

// sameFolder reports whether two code folders are one, as the edit guard compares paths.
func sameFolder(a, b, goos string) bool {
	return foldPath(resolveEditFolder(a, goos), goos) == foldPath(resolveEditFolder(b, goos), goos)
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

// folderHolder finds the run, other than those in skip, that holds a code folder the snapshot's run needs: an approved run that has not finished. A run that works in a folder excludes every other run that works in it or reads it; runs that only read a folder share it. It returns nil when the folders are free. session is the session of the run that wants them.
func folderHolder(bridge string, snap *Snapshot, session string, skip ...string) error {
	myWorks, myReads := folderHolds(snap)
	if len(myWorks) == 0 && len(myReads) == 0 {
		return nil
	}
	find := func(mine, theirs []string) string {
		for _, m := range mine {
			for _, t := range theirs {
				if sameFolder(m, t, runtime.GOOS) {
					return t
				}
			}
		}
		return ""
	}
	for _, ri := range liveRuns(bridge) {
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
	return nil
}

type runInfo struct {
	id    string
	snap  Snapshot
	state State
}

// listRuns reads every run in the bridge, newest first, without locking.
func listRuns(bridge string) []runInfo {
	entries, _ := os.ReadDir(runsDir(bridge))
	var out []runInfo
	for i := len(entries) - 1; i >= 0; i-- {
		dir := filepath.Join(runsDir(bridge), entries[i].Name())
		var ri runInfo
		ri.id = entries[i].Name()
		if readJSON(filepath.Join(dir, "pipeline.json"), &ri.snap) == nil &&
			readJSON(filepath.Join(dir, "state.json"), &ri.state) == nil {
			out = append(out, ri)
		}
	}
	return out
}

func (r *RunFiles) save() error {
	return writeJSON(filepath.Join(r.Dir, "state.json"), r.State)
}

func (r *RunFiles) close() { r.lock.Unlock() }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
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
	return os.Rename(tmp, path)
}

// runOpts is what `ensign run` and `ensign sequence run` accept.
type runOpts struct {
	task, start, pipeline, project string
	location, subtask              string
	sides, dependencies            []string
	dry, sync, rebuild, staleOK    bool
	// A retry inside a sequence: the failed run's id and base, and the work item to freeze instead of the task's.
	retryOf, retryBase, workItem string
	rules                        []Rule // the retried run's rules, which workItem already carries
}

// parseRunArgs reads the flags shared by `ensign run` and `ensign sequence run`; cmd names the command in the usage line.
func parseRunArgs(cmd string, args []string) (runOpts, error) {
	var o runOpts
	usage := fmt.Errorf("usage: %s (<project>/<task> | --pipeline <name> [--project <p>]) [--location <name>] [--subtask <name>] [--sides <a,b,...>] [--dependency <location>=<branch>] [--start <stage>] [--sync] [--rebuild] [--stale-branch] [--dry-run]", cmd)
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
		case "--pipeline", "--project", "--location", "--subtask", "--sides", "--dependency":
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

// cmdRun implements `ensign run <project>/<task> [--start <stage>] [--dry-run]`.
func cmdRun(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "retire" {
		return cmdRunRetire(args[1:], out)
	}
	o, err := parseRunArgs("ensign run", args)
	if err != nil {
		return err
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	_, err = createRun(bridge, o, false, out)
	return err
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
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no run %s", args[0])
	}
	if err != nil {
		return err
	}
	switch {
	case r.State.terminal():
		r.close()
		return fmt.Errorf("run %s has finished (%s); there is nothing to retire", args[0], r.State.Status)
	case r.State.Session == session:
		r.close()
		return fmt.Errorf("run %s belongs to this session; only a run of another session can be retired", args[0])
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

// createRun validates and freezes a run. A queued run (a sequence step) belongs to the session but is not bound to it and replaces nothing. It returns nil for a dry run.
func createRun(bridge string, o runOpts, queued bool, out io.Writer) (*Snapshot, error) {
	task, start, pipeline, project := o.task, o.start, o.pipeline, o.project
	dry, sync, rebuild, staleOK := o.dry, o.sync, o.rebuild, o.staleOK
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
	if o.workItem != "" {
		c.Rules = o.rules
	} else if len(c.Rules) > 0 {
		c.WorkItem += "\n\n## Project rules\n\n"
		for _, r := range c.Rules {
			c.WorkItem += "- " + r.Text + "\n"
		}
	}
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
	var resolve func(list []*Stage)
	resolve = func(list []*Stage) {
		for _, s := range list {
			if s.Parallel != nil && s.Parallel.Over != "dimensions" {
				items, err := generatorItems(c, s.Parallel.Over, rebuild)
				if err != nil {
					errs = append(errs, err.Error())
				}
				s.Dimensions = items
			}
			resolve(s.Stages)
		}
	}
	resolve(c.Pipeline.Stages)
	if start != "" {
		if s, _, _ := c.Pipeline.find(start); s == nil {
			errs = append(errs, fmt.Sprintf("--start: no stage %q", start))
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
	if !queued || dry {
		printSummary(out, c, start)
	}
	if dry {
		fmt.Fprintln(out, "\n(dry run: nothing created)")
		return nil, nil
	}

	session := os.Getenv("STARSHIP_SESSION")
	if session != "" {
		if data, err := os.ReadFile(sessionFile(bridge, session) + ".pin"); err == nil {
			if started := strings.TrimSpace(string(data)); started != pinnedCommit(bridge) {
				return nil, fmt.Errorf("the bridge was updated to a new starship release after this session started; restart claude before starting a run")
			}
		}
	}
	if !queued && session != "" && sequenceUnfinished(bridge, session) {
		if onlyWaitingSequenceUnfinished(bridge, session) {
			return nil, fmt.Errorf("this session has a sequence waiting for go; add the run with ensign sequence run, or ensign sequence discard first")
		}
		return nil, fmt.Errorf("this session has a sequence in progress; add the run with ensign sequence run, or ensign sequence halt first")
	}
	var replace []string
	for _, ri := range listRuns(bridge) {
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
		if err := folderHolder(bridge, want, session, replace...); err != nil {
			return nil, err
		}
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
		Run:          id,
		Task:         c.Task,
		Bridge:       bridge,
		ControlPlane: c.ControlPlane,
		Workspace:    c.Workspace,
		Location:     c.Location,
		Subtask:      c.Subtask,
		Sides:        c.Sides,
		Narrowed:     c.Narrowed,
		Cross:        c.Cross,
		Dependencies: c.Dependencies,
		WorkItem:     c.WorkItem,
		Rules:        c.Rules,
		Start:        start,
		Pipeline:     c.Pipeline,
		Bindings:     c.Bindings,
		Briefs:       map[string]string{},
		Events:       c.Events,
		Extensions:   c.Extensions,
		Skills:       c.Skills,
		KnownSkills:  knownSkills(bridge, c.ControlPlane, c.Project),
		Project:      c.Project,
		Platforms:    c.Platforms,
		Created:      time.Now().Format(time.RFC3339),
	}
	if o.workItem != "" {
		snap.WorkItem = o.workItem
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
		Sync: sync, Extra: map[string]int{}, Rebuild: rebuild, StaleOK: staleOK, Sequence: queued,
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
	if err := os.Mkdir(filepath.Join(dir, "reports"), 0o755); err != nil {
		return nil, err
	}
	if err := writeOnDemandSkills(dir, snap.Skills); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), snap); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), st); err != nil {
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
