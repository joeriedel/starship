// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// A sequence is an ordered list of steps in one session, approved with a single go. The session's sequences live in <bridge>/.starship/sessions/<session>.sequence; every change is made under that file's lock.

const (
	modeStop      = "stop"
	modeKeepGoing = "keep-going"
	// seqStopBudget is how many times the Stop hook holds a turn open for one waiting step.
	seqStopBudget = 3
)

type seqStep struct {
	N           int       `json:"n"`
	Kind        string    `json:"kind"` // run, publish, do, release
	Task        string    `json:"task,omitempty"`
	Run         string    `json:"run,omitempty"`     // the first of Runs, kept so a sequence file written before groups still loads
	Runs        []*seqRun `json:"runs,omitempty"`    // a run step's live group: the runs of subtasks that work side by side
	Group       int       `json:"group,omitempty"`   // the number of that group
	Command     string    `json:"command,omitempty"` // publish: the extension's operation
	Instruction string    `json:"instruction,omitempty"`
	Version     string    `json:"version,omitempty"`  // release: the version to release, fixed when the step is added
	Location    string    `json:"location,omitempty"` // release: the project location whose @release runs
	Started     bool      `json:"started,omitempty"`  // release: release-run has begun the project's @release command
	Token       string    `json:"token,omitempty"`    // release: identifies this start, so the result finds the step if the sequence moved to another session
	Headline    string    `json:"headline,omitempty"`
	Status      string    `json:"status"` // pending, active, paused, decide, done, failed, skipped, cancelled
	Reason      string    `json:"reason,omitempty"`
	Blocks      int       `json:"blocks,omitempty"`      // stops held open for this step
	WaitBlocks  int       `json:"wait_blocks,omitempty"` // stops held open while this step waited for a busy folder
	Prev        string    `json:"prev_run,omitempty"`    // the first run this step's retry replaces; a step gets one retry for each group
	Diagnosis   string    `json:"diagnosis,omitempty"`   // why the previous run failed, as the session wrote it
}

// seqRun is one run of a run step's group.
type seqRun struct {
	Run      string `json:"run"`
	Subtask  string `json:"subtask,omitempty"`
	Location string `json:"location,omitempty"`
	Prev     string `json:"prev_run,omitempty"` // the failed run this one retries
	Status   string `json:"status,omitempty"`   // the run's own status as last seen: pending, running, ask-user, success, error
	Reason   string `json:"reason,omitempty"`
}

// runs lists the runs of a run step. A step saved before groups has only Run, which is taken as its one run.
func (s *seqStep) runs() []*seqRun {
	if len(s.Runs) == 0 && s.Run != "" {
		s.Runs = []*seqRun{{Run: s.Run, Prev: s.Prev}}
	}
	return s.Runs
}

func (s *seqStep) setRuns(rs []*seqRun) {
	s.Runs, s.Run = rs, ""
	if len(rs) > 0 {
		s.Run = rs[0].Run
	}
}

// newRunStep is a run step for the queued runs of one group.
func newRunStep(task string, group int, snaps []*Snapshot) *seqStep {
	st := &seqStep{Kind: "run", Task: task, Group: group, Headline: groupHeadline(snaps)}
	var rs []*seqRun
	for _, snap := range snaps {
		rs = append(rs, &seqRun{Run: snap.Run, Subtask: snap.Subtask, Location: snap.Location, Status: "pending"})
	}
	st.setRuns(rs)
	return st
}

type sequence struct {
	N        int        `json:"n"`
	Mode     string     `json:"mode"`
	Approved bool       `json:"approved"`
	Halted   bool       `json:"halted,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	Closed   bool       `json:"closed,omitempty"` // its end has been reported
	Steps    []*seqStep `json:"steps"`
}

type seqFile struct {
	NextStep  int         `json:"next_step"`
	Sequences []*sequence `json:"sequences"`
}

func seqPath(bridge, session string) string { return sessionFile(bridge, session) + ".sequence" }

func seqExists(bridge, session string) bool {
	if session == "" {
		return false
	}
	_, err := os.Stat(seqPath(bridge, session))
	return err == nil
}

// withSeq reads the session's sequences under their lock, runs fn, and saves them if fn succeeds.
func withSeq(bridge, session string, fn func(sf *seqFile) error) error {
	path := seqPath(bridge, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	l := flock.New(path + ".lock")
	if err := l.Lock(); err != nil {
		return fmt.Errorf("locking the sequence: %w", err)
	}
	defer l.Unlock()
	sf := &seqFile{}
	if err := readJSON(path, sf); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := fn(sf); err != nil {
		return err
	}
	if len(sf.Sequences) == 0 {
		return nil
	}
	return writeJSON(path, sf)
}

// readSeq reads the session's sequences without the lock, for display and checks.
func readSeq(bridge, session string) *seqFile {
	sf := &seqFile{}
	if session != "" {
		readJSON(seqPath(bridge, session), sf)
	}
	return sf
}

// sequenceUnfinished reports whether the session has a step that has not finished.
func sequenceUnfinished(bridge, session string) bool {
	for _, seq := range readSeq(bridge, session).Sequences {
		if seq.unfinished() {
			return true
		}
	}
	return false
}

func sequenceListing(bridge, session string) string {
	return readSeq(bridge, session).listing(bridge)
}

func (s *seqStep) open() bool {
	return s.Status == "pending" || s.Status == "active" || s.Status == "paused" || s.Status == "decide"
}

func (s *seqStep) holds() bool { return s.Status == "active" || s.Status == "paused" }

// deciding reports that a step's run failed and the sequence waits for the session to retry it or accept the failure.
func (s *seqStep) deciding() bool { return s.Status == "decide" }

func (s *sequence) unfinished() bool {
	for _, st := range s.Steps {
		if st.open() {
			return true
		}
	}
	return false
}

func (s *sequence) holding() bool {
	for _, st := range s.Steps {
		if st.holds() || st.deciding() {
			return true
		}
	}
	return false
}

func (s *sequence) nextPending() *seqStep {
	for _, st := range s.Steps {
		if st.Status == "pending" {
			return st
		}
	}
	return nil
}

// openSeq is the newest sequence when it still waits for go.
func (sf *seqFile) openSeq() *sequence {
	if n := len(sf.Sequences); n > 0 {
		if s := sf.Sequences[n-1]; !s.Approved && !s.Halted {
			return s
		}
	}
	return nil
}

func (sf *seqFile) step(n int) (*sequence, *seqStep) {
	for _, seq := range sf.Sequences {
		for _, st := range seq.Steps {
			if st.N == n {
				return seq, st
			}
		}
	}
	return nil, nil
}

// add appends a step to the open sequence, starting one if the newest is approved or halted.
func (sf *seqFile) add(st *seqStep) {
	seq := sf.openSeq()
	if seq == nil {
		seq = &sequence{N: len(sf.Sequences) + 1, Mode: modeStop}
		sf.Sequences = append(sf.Sequences, seq)
	}
	sf.NextStep++
	st.N, st.Status = sf.NextStep, "pending"
	seq.Steps = append(seq.Steps, st)
}

// isOpen reports that a sequence still matters to the session: it has unfinished steps and is either approved or the one awaiting go.
func (sf *seqFile) isOpen(seq *sequence) bool {
	return len(seq.Steps) > 0 && !seq.Closed && seq.unfinished() && (seq.Approved || seq == sf.openSeq())
}

// listing renders the open sequences only, so hook messages stay small however many sequences a session has run.
func (sf *seqFile) listing(bridge string) string { return sf.render(bridge, false) }

// listingAll renders every sequence, finished ones included.
func (sf *seqFile) listingAll(bridge string) string { return sf.render(bridge, true) }

func (sf *seqFile) render(bridge string, all bool) string {
	var b strings.Builder
	for _, seq := range sf.Sequences {
		if len(seq.Steps) == 0 || (!all && !sf.isOpen(seq)) {
			continue
		}
		state := "approved"
		if !seq.Approved {
			state = "not approved yet"
		}
		if seq.Halted {
			state += ", halted: " + seq.Reason
		}
		fmt.Fprintf(&b, "sequence %d (mode %s, %s)\n", seq.N, seq.Mode, state)
		for _, st := range seq.Steps {
			what := st.Task
			switch st.Kind {
			case "run":
				what += "  " + st.Headline
			case "do":
				what = st.Instruction
			case "release":
				what += " " + st.Version
			}
			line := fmt.Sprintf("  %d  %-7s %s  [%s]", st.N, st.Kind, strings.TrimSpace(what), st.Status)
			if st.Reason != "" {
				line += " " + firstLine(st.Reason)
			}
			if st.Kind == "run" && (st.Prev != "" || st.deciding()) {
				n := 1
				if st.Prev != "" {
					n = 2
				}
				line += fmt.Sprintf(" (attempt %d/2", n)
				if st.Diagnosis != "" {
					line += ", retried because: " + firstLine(st.Diagnosis)
				}
				line += ")"
			}
			b.WriteString(line + "\n")
			if st.Kind == "run" && st.open() {
				for _, g := range st.groupLines(bridge) {
					b.WriteString("       " + g + "\n")
				}
			}
		}
		if !seq.Approved && !seq.Halted {
			b.WriteString("type go to approve the sequence\n")
		}
	}
	return b.String()
}

// groupLines shows a subtask step's groups: its current runs, then the later groups the task's pending subtasks fall into.
func (s *seqStep) groupLines(bridge string) []string {
	var lines, names []string
	for _, ru := range s.runs() {
		if ru.Subtask == "" {
			return nil
		}
		names = append(names, ru.Subtask)
		item := ru.Subtask
		if ru.Location != "" {
			item += " (" + ru.Location + ")"
		}
		if ru.Status != "" && ru.Status != "pending" {
			item += " [" + ru.Status + "]"
		}
		if ru.Status == "pending" && ru.Reason != "" {
			item += " [" + firstLine(ru.Reason) + "]"
		}
		lines = append(lines, item)
	}
	n := max(s.Group, 1)
	out := []string{fmt.Sprintf("group %d: %s", n, strings.Join(lines, ", "))}
	pending, _, err := pendingSubtasks(bridge, s.Task)
	if err != nil {
		return out
	}
	pending = slices.DeleteFunc(pending, func(p subtask) bool { return slices.Contains(names, p.Name) })
	for i, g := range subtaskGroups(bridge, s.Task, pending) {
		var items []string
		for _, sub := range g {
			item := sub.Name
			if sub.Location != "" {
				item += " (" + sub.Location + ")"
			}
			items = append(items, item)
		}
		out = append(out, fmt.Sprintf("group %d: %s", n+1+i, strings.Join(items, ", ")))
	}
	return out
}

// headline is a work item's first line that is text, not a Markdown heading.
func headline(workItem string) string {
	for _, l := range strings.Split(workItem, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if r := []rune(l); len(r) > 80 {
			l = strings.TrimSpace(string(r[:80])) + "…"
		}
		return l
	}
	return ""
}

// readRun reads a run without locking it, for display and checks.
func readRun(bridge, id string) (*RunFiles, error) {
	dir := filepath.Join(runsDir(bridge), id)
	r := &RunFiles{Dir: dir, Snap: &Snapshot{}, State: &State{}}
	if err := readJSON(filepath.Join(dir, "pipeline.json"), r.Snap); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "state.json"), r.State); err != nil {
		return nil, err
	}
	return r, nil
}

// reconcile brings the steps of runs under way in line with their runs' saved states. A step with several runs waits until all of them are over before it succeeds or fails.
func (sf *seqFile) reconcile(bridge string) {
	for _, seq := range sf.Sequences {
		for _, st := range seq.Steps {
			if st.Kind != "run" || !st.holds() {
				continue
			}
			runs := st.runs()
			var snaps []*RunFiles
			over, failed, lost := 0, 0, false
			var reason, asked string
			for _, ru := range runs {
				r, err := readRun(bridge, ru.Run)
				if err != nil {
					ru.Status, ru.Reason, lost = "error", "its run can't be read: "+err.Error(), true
				} else {
					ru.Status = r.State.Status
					if ru.Status != "pending" {
						ru.Reason = ""
					}
					ru.Subtask, ru.Location = r.Snap.Subtask, r.Snap.Location
					if r.State.Status == "error" || r.State.Status == "ask-user" {
						ru.Reason = r.State.Reason
					}
					snaps = append(snaps, r)
				}
				switch ru.Status {
				case "success":
					over++
				case "error":
					over++
					failed++
					if reason == "" {
						reason = ru.Reason
					}
				case "ask-user":
					if asked == "" {
						asked = ru.Reason
					}
				}
			}
			switch {
			case over < len(runs) && asked != "":
				st.Status, st.Reason = "paused", asked
			case over < len(runs):
				st.Status, st.Reason = "active", ""
			case failed > 0:
				st.Reason = reason
				if !lost && st.Prev == "" && !seq.Halted {
					st.Status = "decide"
					continue
				}
				st.Status = "failed"
				sf.failed(bridge, seq, st)
			default:
				st.Status, st.Reason = "done", ""
				if snaps[0].Snap.Subtask != "" {
					sf.nextSubtaskGroup(bridge, seq, st, snaps)
				}
			}
		}
	}
}

// groupHeadline is the line a run step shows: its work item's headline, and the subtasks it works on.
func groupHeadline(snaps []*Snapshot) string {
	h := headline(snaps[0].WorkItem)
	var names []string
	for _, snap := range snaps {
		if snap.Subtask != "" {
			names = append(names, snap.Subtask)
		}
	}
	switch len(names) {
	case 0:
	case 1:
		h += " (subtask " + names[0] + ")"
	default:
		h += " (subtasks " + strings.Join(names, ", ") + ")"
	}
	return h
}

// subtaskGroups splits subtasks, in order, into groups of consecutive ones that can run side by side: their locations are unrelated (neither depends on the other, directly or through others) and their code folders differ. A subtask without a location works in the run's default workspace, so it is a group of its own.
func subtaskGroups(bridge, task string, subs []subtask) [][]subtask {
	project, _, _ := strings.Cut(task, "/")
	locs, err := loadLocations(bridge, project)
	conflict := func(a, b subtask) bool {
		if err != nil || a.Location == "" || b.Location == "" || locs.Related(a.Location, b.Location) {
			return true
		}
		pa, errA := locationPathFor(bridge, project, a.Location)
		pb, errB := locationPathFor(bridge, project, b.Location)
		return errA != nil || errB != nil || sameFolder(pa, pb, runtime.GOOS)
	}
	var groups [][]subtask
	for _, s := range subs {
		if n := len(groups); n > 0 && !slices.ContainsFunc(groups[n-1], func(m subtask) bool { return conflict(m, s) }) {
			groups[n-1] = append(groups[n-1], s)
			continue
		}
		groups = append(groups, []subtask{s})
	}
	return groups
}

// carriedOpts is the options for the queued run of a subtask that follows runs already made: it keeps their stale-branch choice, takes the sides of the one in its location, and the dependencies named with --dependency that its location can take.
func carriedOpts(bridge, task string, sub subtask, staleOK bool, carry []*Snapshot) runOpts {
	o := runOpts{task: task, subtask: sub.Name, location: sub.Location, staleOK: staleOK}
	project, _, _ := strings.Cut(task, "/")
	locs, _ := loadLocations(bridge, project)
	for i, p := range carry {
		if sub.Location == "" && i == 0 {
			o.location = p.Location
		}
		if p.Location == o.location && p.Narrowed && o.sides == nil {
			o.sides = sideNames(p.Sides)
		}
	}
	if o.location == "" || locs == nil {
		return o
	}
	for _, p := range carry {
		for _, dep := range p.Dependencies {
			if dep.Branch == "" || dep.Auto || dep.Location == o.location || !contains(locs.Dependents(dep.Location), o.location) {
				continue
			}
			if v := dep.Location + "=" + dep.Branch; !slices.Contains(o.dependencies, v) {
				o.dependencies = append(o.dependencies, v)
			}
		}
	}
	return o
}

// createGroupRuns creates the queued runs of a group of subtasks. The first run takes first when it is given. A run that can't be created withdraws the ones made before it.
func createGroupRuns(bridge, task string, group []subtask, carry []*Snapshot, staleOK bool, first *runOpts, out io.Writer) ([]*Snapshot, error) {
	var made []*Snapshot
	for i, sub := range group {
		var o runOpts
		if i == 0 && first != nil {
			o = *first
			o.subtask = sub.Name
		} else {
			o = carriedOpts(bridge, task, sub, staleOK, append(slices.Clone(carry), made...))
		}
		snap, err := createRun(bridge, o, true, out)
		if err == nil && snap == nil {
			return nil, nil
		}
		if err != nil {
			for _, m := range made {
				retireIf(bridge, m.Run, "withdrawn: "+err.Error(), "pending")
			}
			return nil, err
		}
		made = append(made, snap)
	}
	return made, nil
}

// nextSubtaskGroup is called when every run of a step's group has succeeded. While the task has pending subtasks the step is not done: it moves to queued runs for the next group, which advance then starts.
func (sf *seqFile) nextSubtaskGroup(bridge string, seq *sequence, st *seqStep, done []*RunFiles) {
	fail := func(err error) {
		st.Status, st.Reason = "failed", err.Error()
		sf.failed(bridge, seq, st)
	}
	task := done[0].Snap.Task
	pending, _, err := pendingSubtasks(bridge, task)
	if err != nil {
		fail(err)
		return
	}
	// The group's own subtasks are marked done when their runs finish; skip them in case a mark is not yet written.
	pending = slices.DeleteFunc(pending, func(s subtask) bool {
		return slices.ContainsFunc(done, func(r *RunFiles) bool { return r.Snap.Subtask == s.Name })
	})
	if len(pending) == 0 {
		return
	}
	next := subtaskGroups(bridge, task, pending)[0]
	if seq.Halted {
		st.Reason = "sequence halted; subtask " + next[0].Name + " and later were not run"
		return
	}
	staleOK := false
	var carry []*Snapshot
	for _, r := range done {
		staleOK = staleOK || r.State.StaleOK
		carry = append(carry, r.Snap)
	}
	snaps, err := createGroupRuns(bridge, task, next, carry, staleOK, nil, io.Discard)
	if err != nil {
		fail(err)
		return
	}
	fresh := newRunStep(task, max(st.Group, 1)+1, snaps)
	st.setRuns(fresh.Runs)
	st.Group, st.Prev, st.Diagnosis, st.Headline = fresh.Group, "", "", fresh.Headline
	st.Status, st.Reason, st.Blocks, st.WaitBlocks = "pending", "", 0, 0
}

// failed applies the failure rules for a step that has just failed.
func (sf *seqFile) failed(bridge string, seq *sequence, failed *seqStep) {
	why := fmt.Sprintf("step %d failed", failed.N)
	if seq.Mode == modeKeepGoing {
		for _, st := range seq.Steps {
			if st.Status == "pending" && failed.Task != "" && (st.Task == failed.Task ||
				(failed.Kind == "release" && strings.HasPrefix(st.Task, failed.Task+"/"))) {
				cancelStep(bridge, st, "skipped", why)
			}
		}
		return
	}
	if !seq.Halted {
		seq.Halted, seq.Reason = true, why+": "+firstLine(failed.Reason)
	}
	for _, st := range seq.Steps {
		if st.Status == "pending" {
			cancelStep(bridge, st, "cancelled", why)
		}
	}
}

// cancelStep ends a step that never started. Its runs are made terminal, as a retired run is, so they are nobody's waiting run; no run.finish handler fires for them.
func cancelStep(bridge string, st *seqStep, status, reason string) {
	st.Status, st.Reason = status, reason
	if st.Kind != "run" {
		return
	}
	for _, ru := range st.runs() {
		retireUnstarted(bridge, ru.Run, status+": "+reason)
	}
}

// retireUnstarted retires a run that is queued, or approved with its lead never dispatched. It reports whether it did.
func retireUnstarted(bridge, id, reason string) bool {
	r, err := openRun(bridge, id)
	if err != nil {
		return false
	}
	if st := r.State; st.Status != "pending" && !(st.Status == "running" && st.Approved && !st.Started && len(st.Dispatches) == 0) {
		r.close()
		return false
	}
	return markRetired(r, reason) == nil
}

// retireIf makes a run terminal, but only if it is still in one of the given states when its lock is held. It reports whether it did.
func retireIf(bridge, id, reason string, states ...string) bool {
	r, err := openRun(bridge, id)
	if err != nil {
		return false
	}
	if !slices.Contains(states, r.State.Status) {
		r.close()
		return false
	}
	return markRetired(r, reason) == nil
}

// advance starts the next step if none is under way. It returns what the session should be told, or "" when nothing changed.
func (sf *seqFile) advance(bridge, session string) (string, error) {
	sf.reconcile(bridge)
	var parts []string
	finish := func() string {
		if len(parts) == 0 {
			return ""
		}
		return strings.Join(parts, "\n") + "\n\n" + sf.listing(bridge)
	}
	for _, seq := range sf.Sequences {
		if seq.Closed {
			continue
		}
		if !seq.Approved && !seq.Halted {
			break
		}
		for {
			if !seq.Halted {
				// A run that waited for its folder may be free to start now.
				for _, st := range seq.Steps {
					if st.Kind != "run" || !st.holds() {
						continue
					}
					texts, _, _ := startRuns(bridge, session, st)
					for _, t := range texts {
						parts = append(parts, fmt.Sprintf("step %d: %s", st.N, t))
					}
				}
			}
			if seq.holding() {
				return finish(), nil
			}
			st := seq.nextPending()
			if st == nil {
				seq.Closed = true
				if seq.Halted {
					parts = append(parts, fmt.Sprintf("sequence %d halted: %s", seq.N, seq.Reason))
				} else {
					parts = append(parts, fmt.Sprintf("sequence %d finished.", seq.N))
				}
				break
			}
			switch st.Kind {
			case "run":
				texts, busy, err := startRuns(bridge, session, st)
				switch {
				case len(texts) > 0:
					st.Status, st.Reason = "active", ""
					parts = append(parts, fmt.Sprintf("step %d: %s", st.N, strings.Join(texts, "\n\n")))
					if busy != nil {
						parts = append(parts, fmt.Sprintf("step %d: another run of it can't start yet: %v", st.N, busy))
					}
				case busy != nil:
					st.Reason = "waiting: " + busy.Error()
					parts = append(parts, fmt.Sprintf("step %d can't start yet: %v", st.N, busy))
				case err != nil:
					st.Status, st.Reason = "failed", err.Error()
					sf.failed(bridge, seq, st)
					continue
				default:
					st.Status, st.Reason = "active", ""
				}
				return finish(), nil
			case "publish":
				out, err := runPublish(bridge, seq, st)
				if err != nil {
					st.Status, st.Reason = "failed", err.Error()
					sf.failed(bridge, seq, st)
					parts = append(parts, fmt.Sprintf("step %d (publish %s) failed: %v", st.N, st.Task, err))
					continue
				}
				st.Status, st.Reason = "done", out
			case "do":
				st.Status = "active"
				parts = append(parts, fmt.Sprintf("step %d: %s", st.N, doText(st)))
				return finish(), nil
			case "release":
				st.Status = "active"
				parts = append(parts, fmt.Sprintf("step %d: %s", st.N, releaseText(st)))
				return finish(), nil
			}
		}
	}
	return finish(), nil
}

func doText(st *seqStep) string {
	return fmt.Sprintf("%s\nWhen it is done run `ensign sequence done %d`, or `ensign sequence fail %d --reason …` if it can't be.",
		st.Instruction, st.N, st.N)
}

func releaseText(st *seqStep) string {
	return fmt.Sprintf("Release %s %s. Start `ensign sequence release-run %d` as a background shell command (Bash with run_in_background), wait for its completion notification without polling, then report the outcome. If the command dies without finishing, run `ensign sequence fail %d --reason …`.",
		st.Task, st.Version, st.N, st.N)
}

func decideText(st *seqStep) string {
	return fmt.Sprintf("Its run failed: %s\nRetry once only for a diagnosed, small, fixable cause: `ensign sequence retry %d --diagnosis \"<why it failed>\" --state \"<what is left>\"`. Otherwise `ensign sequence accept %d`.",
		firstLine(st.Reason), st.N, st.N)
}

// startRuns starts the queued runs of a step's group, each in its own code folder. It returns what the session does next for each run it started, the first folder-busy error of a run that must wait for a later advance, and the first other error; a run that fails to start is retired.
func startRuns(bridge, session string, st *seqStep) (texts []string, busy, failure error) {
	for _, ru := range st.runs() {
		if ru.Status != "" && ru.Status != "pending" {
			continue
		}
		text, err := startRun(bridge, session, st.Task, ru.Run)
		var b *folderBusyError
		switch {
		case errors.As(err, &b):
			ru.Reason = "waiting: " + err.Error()
			if busy == nil {
				busy = err
			}
		case err != nil:
			retireIf(bridge, ru.Run, "failed: "+err.Error(), "pending")
			ru.Status, ru.Reason = "error", err.Error()
			if failure == nil {
				failure = err
			}
		default:
			ru.Status, ru.Reason = "running", ""
			texts = append(texts, text)
		}
	}
	return texts, busy, failure
}

// startRun approves a queued run and binds it to the session. It returns what the session does next, the same text go gives a plain run.
func startRun(bridge, session, task, run string) (string, error) {
	if data, err := os.ReadFile(sessionFile(bridge, session) + ".pin"); err == nil {
		if started := strings.TrimSpace(string(data)); started != pinnedCommit(bridge) {
			return "", fmt.Errorf("the bridge was updated to a new starship release after this session started; restart claude before starting a run")
		}
	}
	r, err := openRun(bridge, run)
	if err != nil {
		return "", fmt.Errorf("its run can't be opened: %v", err)
	}
	defer r.close()
	if r.State.Status != "pending" {
		return "", fmt.Errorf("its run %s is %s, not waiting", run, r.State.Status)
	}
	l, err := lockFolders(bridge)
	if err != nil {
		return "", err
	}
	defer l.Unlock()
	if err := folderHolder(bridge, r.Snap, session, run); err != nil {
		return "", err
	}
	r.State.Session, r.State.Approved, r.State.Status = session, true, "running"
	r.State.log("approved", "", "", "sequence")
	if err := r.save(); err != nil {
		return "", err
	}
	l.Unlock()
	if err := bindSession(bridge, session, run); err != nil {
		return "", err
	}
	if task != "" {
		os.WriteFile(sessionFile(bridge, session)+".task", []byte(task+"\n"), 0o644)
	}
	return approvalText(r), nil
}

// publishCommand finds the one publish operation a task's extensions declare.
func publishCommand(bridge, task string) (string, error) {
	// The step is added before the runs it follows have finished, so a planned run's location will do to find the extensions.
	loc := latestRunLocation(bridge, task, true)
	if loc == "" {
		loc = latestRunLocation(bridge, task, false)
	}
	c, err := loadConfigAt(bridge, task, loc)
	if err != nil {
		return "", err
	}
	exts := availableExtensions(bridge, c.ControlPlane)
	var cmds []string
	for _, name := range c.Extensions {
		if x := exts[name]; x != nil && x.Publish != "" {
			cmds = append(cmds, x.Publish)
		}
	}
	switch len(cmds) {
	case 1:
		return cmds[0], nil
	case 0:
		return "", fmt.Errorf("%s: its project's extensions have no publish operation", task)
	}
	return "", fmt.Errorf("%s: more than one of its project's extensions declare a publish operation", task)
}

// runPublish carries out a publish step the way events run an extension's command: built-in "ensign …" operations in this process, anything else through the shell.
func runPublish(bridge string, seq *sequence, st *seqStep) (string, error) {
	loc := precedingRunLocation(bridge, seq, st.N, st.Task)
	if args, ok := strings.CutPrefix(st.Command, "ensign "); ok {
		if strings.TrimSpace(args) == "github-pr merge" {
			base, sha, err := githubMerge(bridge, st.Task, loc)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("pushed to %s: %s", base, short(sha)), nil
		}
		return "", fmt.Errorf("unknown built-in publish operation: %s", st.Command)
	}
	c, err := loadConfigAt(bridge, st.Task, loc)
	if err != nil {
		return "", err
	}
	out, err := shell(c.Workspace, st.Command+" "+shellQuote(st.Task), projectEnv(bridge, c.Project, os.Environ()))
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, lastLines(out, 5))
	}
	return lastLines(out, 1), nil
}

func advanceSequence(bridge, session string) (string, error) {
	var text string
	err := withSeq(bridge, session, func(sf *seqFile) (err error) {
		text, err = sf.advance(bridge, session)
		return
	})
	return text, err
}

// approveSequence is go for a session with an open sequence: it approves every step as it stands and starts the first. ok is false when there is no open sequence.
func approveSequence(bridge, session string) (text string, ok bool, err error) {
	if !seqExists(bridge, session) {
		return "", false, nil
	}
	err = withSeq(bridge, session, func(sf *seqFile) error {
		seq := sf.openSeq()
		if seq == nil {
			return nil
		}
		ok, seq.Approved = true, true
		text = fmt.Sprintf("sequence %d approved (%d steps). ", seq.N, len(seq.Steps))
		adv, err := sf.advance(bridge, session)
		if adv == "" {
			adv = "It starts when the step in progress finishes.\n\n" + sf.listing(bridge)
		}
		text += adv + "\n\nSequence rules: `ensign help sequence`."
		return err
	})
	return
}

// waitingText says what a run step waits for when its run, or the step, is waiting for a busy code folder, else "".
func waitingText(st *seqStep) string {
	var lines []string
	if strings.HasPrefix(st.Reason, "waiting:") {
		lines = append(lines, fmt.Sprintf("Step %d is %s", st.N, st.Reason))
	}
	for _, ru := range st.runs() {
		if strings.HasPrefix(ru.Reason, "waiting:") && ru.Reason != st.Reason {
			lines = append(lines, fmt.Sprintf("Step %d, run %s, is %s", st.N, ru.Run, ru.Reason))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\nIt starts by itself once that run has finished; this turn is held only a few times for it."
}

// sequenceStop holds a turn open while a step waits for the session to act on it. Each step gets seqStopBudget holds, as the sequence file's lock allows, then the turn may end.
func sequenceStop(bridge string, in *HookInput) any {
	if !seqExists(bridge, in.SessionID) {
		return nil
	}
	var out any
	withSeq(bridge, in.SessionID, func(sf *seqFile) error {
		adv, advErr := sf.advance(bridge, in.SessionID)
		var advNote string
		if advErr != nil {
			advNote = "\n\nThe sequence could not advance: " + advErr.Error()
		}
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				var todo string
				switch {
				case st.deciding():
					todo = decideText(st)
				case st.Kind == "run" && st.holds():
					if seq.Halted {
						continue
					}
					var leads []string
					for _, ru := range st.runs() {
						if r, err := readRun(bridge, ru.Run); err == nil && r.State.Approved && r.State.Status == "running" &&
							!r.State.Started && len(r.State.Dispatches) == 0 {
							leads = append(leads, approvalText(r))
						}
					}
					todo = strings.Join(leads, "\n\n")
				case st.Status != "active":
				case st.Kind == "do":
					// Background work in flight means the session is waiting on it, not stopping.
					if len(in.BackgroundTasks) == 0 {
						todo = doText(st)
					}
				case st.Kind == "release":
					if !st.Started {
						todo = releaseText(st)
					}
				}
				// Holds spent on a wait must not use up the holds that prompt the session to dispatch the lead once the step starts.
				blocks := &st.Blocks
				waitable := st.Kind == "run" && !seq.Halted && (st.Status == "pending" || st.holds())
				if waitable && todo == "" {
					blocks = &st.WaitBlocks
				}
				if waitable {
					todo = strings.TrimSpace(todo + "\n\n" + waitingText(st))
				}
				if todo == "" || *blocks >= seqStopBudget {
					continue
				}
				*blocks++
				// adv repeats this step's lead, wait or work text, which todo already carries.
				tail := adv
				for _, p := range []string{fmt.Sprintf("step %d: ", st.N), fmt.Sprintf("step %d can't start yet", st.N)} {
					if strings.HasPrefix(adv, p) || strings.Contains(adv, "\n"+p) {
						tail = ""
					}
				}
				if tail == "" {
					tail = sf.listing(bridge)
				}
				out = block(fmt.Sprintf("sequence step %d is waiting for you.\n\n%s\n\n%s%s\n\nSequence rules: `ensign help sequence`.", st.N, todo, tail, advNote))
				return nil
			}
		}
		return nil
	})
	return out
}

// carrySequence moves a session's sequence to the session that continues it, and points the runs that are not bound to the old session at the new one.
func carrySequence(bridge, old, session string) error {
	if !seqExists(bridge, old) || seqExists(bridge, session) {
		return nil
	}
	if os.Rename(seqPath(bridge, old), seqPath(bridge, session)) != nil {
		return nil
	}
	bound, err := sessionRuns(bridge, old)
	if err != nil {
		return err
	}
	return withSeq(bridge, session, func(sf *seqFile) error {
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				if st.Kind != "run" || !st.open() {
					continue
				}
				for _, ru := range st.runs() {
					if slices.Contains(bound, ru.Run) {
						continue
					}
					if r, err := openRun(bridge, ru.Run); err == nil {
						if r.State.Session == old {
							r.State.Session = session
							r.save()
						}
						r.close()
					}
				}
			}
		}
		return nil
	})
}

// releaseCommand finds a project's @release command and the folder it runs in: the project's workspace, or the folder of the location asked for (the project's only one if none is asked for), whose bindings add to the project's.
func releaseCommand(bridge, project, location string) (cmd, workspace string, err error) {
	var l Layer
	if err := readYAML(filepath.Join(bridge, "projects", project, "project.yaml"), &l); err != nil {
		return "", "", fmt.Errorf("project %q: %v", project, err)
	}
	c := &Config{Project: project, Bindings: map[string]Binding{}}
	if err := c.addBindings(&l.Bindings, "project "+project); err != nil {
		return "", "", err
	}
	location, workspace, locs, err := projectLocation(bridge, project, location)
	if err != nil {
		return "", "", err
	}
	if location != "" {
		c.Locations, c.Location = locs, location
		le := c.locationLayer(bridge)
		if err := c.addBindings(&le.layer.Bindings, le.label); err != nil {
			return "", "", err
		}
	}
	b := c.Bindings["release"]
	if b.Command == "" || b.NotApplicable != "" {
		return "", "", fmt.Errorf("project %s has no @release binding with a command", project)
	}
	return b.Command, workspace, nil
}

// releaseRun runs the project's @release command for the release step that is due. The command runs without the sequence lock and without a time limit; the step is marked started first so the Stop hook doesn't ask for a second start.
func releaseRun(bridge, session string, n int, out io.Writer) error {
	var project, version, location string
	token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	err := withSeq(bridge, session, func(sf *seqFile) error {
		_, st := sf.step(n)
		if st == nil || st.Kind != "release" || st.Status != "active" || st.Started {
			return fmt.Errorf("step %d is not a release step that is due", n)
		}
		st.Started, st.Token = true, token
		project, version, location = st.Task, st.Version, st.Location
		return nil
	})
	if err != nil {
		return err
	}
	cmd, ws, err := releaseCommand(bridge, project, location)
	if err == nil {
		var output string
		output, err = shellFor(ws, cmd, projectEnv(bridge, project, append(os.Environ(), "VERSION="+version)), 0)
		if err != nil {
			err = fmt.Errorf("%v: %s", err, lastLines(output, 5))
		}
	}
	finish := func(owner string) (bool, error) {
		found := false
		werr := withSeq(bridge, owner, func(sf *seqFile) error {
			var seq *sequence
			var st *seqStep
			for _, q := range sf.Sequences {
				for _, c := range q.Steps {
					if c.Kind == "release" && c.Token == token {
						seq, st = q, c
					}
				}
			}
			if st == nil {
				return nil
			}
			found = true
			if st.Status != "active" {
				fmt.Fprintf(out, "step %d is %s; the release command ended with: %v\n", st.N, st.Status, err)
				return nil
			}
			var note string
			if err != nil {
				st.Status, st.Reason = "failed", err.Error()
				sf.failed(bridge, seq, st)
				note = fmt.Sprintf("step %d (release %s %s) failed: %v", st.N, project, version, err)
			} else {
				st.Status, st.Reason = "done", ""
				note = fmt.Sprintf("step %d (release %s %s) done.", st.N, project, version)
			}
			text, aerr := sf.advance(bridge, owner)
			if text == "" {
				text = sf.listing(bridge)
			}
			fmt.Fprintf(out, "%s\n%s", note, text)
			return aerr
		})
		return found, werr
	}
	// The sequence may have been carried to a new session while the command ran.
	if found, werr := finish(session); found || werr != nil {
		return werr
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(seqPath(bridge, session)), "*.sequence"))
	for _, f := range files {
		owner := strings.TrimSuffix(filepath.Base(f), ".sequence")
		if owner == session {
			continue
		}
		if found, werr := finish(owner); found || werr != nil {
			return werr
		}
	}
	outcome := "done"
	if err != nil {
		outcome = "failed: " + err.Error()
	}
	fmt.Fprintf(out, "step %d (release %s %s) %s\n", n, project, version, outcome)
	return fmt.Errorf("step %d is no longer in any session's sequence; its release outcome was not recorded", n)
}

// cmdSequence implements `ensign sequence [run|publish|do|mode|halt|drop|done|fail …]`.
func cmdSequence(args []string, out io.Writer) error {
	session := os.Getenv("STARSHIP_SESSION")
	if session == "" {
		return fmt.Errorf("a sequence belongs to a session, and STARSHIP_SESSION is not set")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	deliverTopic(bridge, "sequence", out)
	if len(args) == 0 || (len(args) == 1 && args[0] == "--all") {
		all := len(args) == 1
		return withSeq(bridge, session, func(sf *seqFile) error {
			sf.reconcile(bridge)
			if all {
				if l := sf.listingAll(bridge); l != "" {
					fmt.Fprint(out, l)
				} else {
					fmt.Fprintln(out, "no sequence in this session")
				}
			} else if l := sf.listing(bridge); l != "" {
				fmt.Fprint(out, l)
			} else if sf.listingAll(bridge) != "" {
				fmt.Fprintln(out, "no open sequence; `ensign sequence --all` lists every sequence in this session")
			} else {
				fmt.Fprintln(out, "no sequence in this session")
			}
			return nil
		})
	}
	rest := args[1:]
	switch args[0] {
	case "run":
		o, err := parseRunArgs("ensign sequence run", rest)
		if err != nil {
			return err
		}
		if o.subtask != "" {
			return fmt.Errorf("a sequence step names a task, never a subtask; its run starts with the task's next pending subtask")
		}
		if err := checkAddable(bridge, session); err != nil {
			return err
		}
		var group []subtask
		if o.task != "" && !o.dry {
			pending, _, err := pendingSubtasks(bridge, o.task)
			if err != nil {
				return err
			}
			if len(pending) > 0 {
				group = subtaskGroups(bridge, o.task, pending)[0]
			}
		}
		if len(group) == 0 {
			snap, err := createRun(bridge, o, true, out)
			if err != nil || snap == nil {
				return err
			}
			return seqAdd(bridge, session, out, newRunStep(snap.Task, 0, []*Snapshot{snap}))
		}
		snaps, err := createGroupRuns(bridge, o.task, group, nil, o.staleOK, &o, out)
		if err != nil {
			return err
		}
		return seqAdd(bridge, session, out, newRunStep(o.task, 1, snaps))
	case "publish":
		if len(rest) != 1 {
			return fmt.Errorf("usage: ensign sequence publish <project>/<task>")
		}
		cmd, err := publishCommand(bridge, rest[0])
		if err != nil {
			return err
		}
		if err := checkAddable(bridge, session); err != nil {
			return err
		}
		return seqAdd(bridge, session, out, &seqStep{Kind: "publish", Task: rest[0], Command: cmd})
	case "do":
		text := strings.TrimSpace(strings.Join(rest, " "))
		if text == "" {
			return fmt.Errorf("usage: ensign sequence do <instruction>")
		}
		if err := checkAddable(bridge, session); err != nil {
			return err
		}
		return seqAdd(bridge, session, out, &seqStep{Kind: "do", Instruction: text})
	case "release":
		var location string
		if len(rest) == 4 && rest[2] == "--location" {
			location, rest = rest[3], rest[:2]
		}
		if len(rest) != 2 || strings.TrimSpace(rest[1]) == "" {
			return fmt.Errorf("usage: ensign sequence release <project> <version> [--location <name>]")
		}
		if _, _, err := releaseCommand(bridge, rest[0], location); err != nil {
			return err
		}
		if err := checkAddable(bridge, session); err != nil {
			return err
		}
		return seqAdd(bridge, session, out, &seqStep{Kind: "release", Task: rest[0], Version: strings.TrimSpace(rest[1]), Location: location})
	case "release-run":
		n, err := stepArg(rest, "usage: ensign sequence release-run <n>")
		if err != nil {
			return err
		}
		return releaseRun(bridge, session, n, out)
	case "mode":
		if len(rest) != 1 || (rest[0] != modeStop && rest[0] != modeKeepGoing) {
			return fmt.Errorf("usage: ensign sequence mode stop|keep-going")
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			seq := sf.openSeq()
			if seq == nil {
				return fmt.Errorf("there is no sequence waiting for go; add a step first")
			}
			seq.Mode = rest[0]
			fmt.Fprint(out, sf.listing(bridge))
			return nil
		})
	case "halt":
		reason, err := flagValue(rest, "--reason", "usage: ensign sequence halt [--reason <text>]")
		if err != nil {
			return err
		}
		// A paused step's run may be held by a finishing hook, whose order is run then sequence; so its run is retired here between two holds of the sequence lock.
		type pausedStep struct {
			n    int
			runs []string
		}
		var paused []pausedStep
		var haltedN int
		err = withSeq(bridge, session, func(sf *seqFile) error {
			var cur *sequence
			for _, seq := range sf.Sequences {
				if seq.Approved && seq.unfinished() {
					cur = seq
					break
				}
			}
			if cur == nil {
				cur = sf.openSeq()
			}
			if cur == nil {
				return fmt.Errorf("there is no sequence to halt")
			}
			if reason == "" {
				reason = "halted"
			}
			cur.Halted, cur.Reason = true, reason
			haltedN = cur.N
			sf.reconcile(bridge)
			for _, st := range cur.Steps {
				switch {
				case st.Status == "paused":
					p := pausedStep{n: st.N}
					for _, ru := range st.runs() {
						p.runs = append(p.runs, ru.Run)
					}
					paused = append(paused, p)
				case st.Kind == "run" && st.Status == "active":
					// A run still queued for a busy folder, or approved with no lead yet, never starts now.
					all := true
					for _, ru := range st.runs() {
						if !retireUnstarted(bridge, ru.Run, "cancelled: "+reason) {
							all = false
						}
					}
					if all {
						st.Status, st.Reason = "cancelled", reason
					}
				case st.Status == "pending" || st.deciding() || (st.Status == "active" && (st.Kind == "do" || (st.Kind == "release" && !st.Started))):
					cancelStep(bridge, st, "cancelled", reason)
				}
			}
			if !cur.Approved {
				cur.Closed = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		retired := map[int]bool{}
		for _, p := range paused {
			retired[p.n] = true
			for _, id := range p.runs {
				// A run that is still going keeps its step paused; one that has finished does not.
				if !retireIf(bridge, id, "cancelled: "+reason, "pending", "ask-user") {
					if r, err := readRun(bridge, id); err == nil && !finishedRun(r.State) {
						retired[p.n] = false
					}
				}
			}
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			for n, ok := range retired {
				// A run that was resumed meanwhile is left running; its step finishes like any active one.
				if _, st := sf.step(n); ok && st != nil && st.Status == "paused" {
					st.Status, st.Reason = "cancelled", reason
				}
			}
			text, err := sf.advance(bridge, session)
			if text != "" {
				fmt.Fprintln(out, text)
			} else {
				fmt.Fprintf(out, "sequence %d halted: %s\n", haltedN, reason)
				if l := sf.listing(bridge); l != "" {
					fmt.Fprint(out, "\n"+l)
				}
			}
			return err
		})
	case "drop":
		const dropUsage = "usage: ensign sequence drop <n>... [--reason <text>]"
		var ns []int
		reason := "dropped by the user"
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--reason" {
				if i+2 != len(rest) {
					return errors.New(dropUsage)
				}
				reason = rest[i+1]
				break
			}
			n, err := stepArg(rest[i:i+1], dropUsage)
			if err != nil {
				return err
			}
			ns = append(ns, n)
		}
		if len(ns) == 0 {
			return errors.New(dropUsage)
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			sf.reconcile(bridge)
			drop := map[*seqStep]bool{}
			for _, n := range ns {
				seq, st := sf.step(n)
				switch {
				case st == nil:
					return fmt.Errorf("there is no step %d", n)
				case !seq.Approved || seq.Closed:
					return fmt.Errorf("step %d is not in an approved, open sequence", n)
				case st.Status != "pending":
					return fmt.Errorf("step %d is %s; only pending steps can be dropped", n, st.Status)
				}
				drop[st] = true
				if st.Kind != "run" {
					continue
				}
				for _, other := range seq.Steps {
					if other.Kind == "publish" && other.Task == st.Task && other.Status == "pending" {
						drop[other] = true
					}
				}
			}
			for _, seq := range sf.Sequences {
				for _, st := range seq.Steps {
					if drop[st] {
						cancelStep(bridge, st, "cancelled", reason)
					}
				}
			}
			text, err := sf.advance(bridge, session)
			if text == "" {
				text = sf.listing(bridge)
			}
			fmt.Fprint(out, text)
			return err
		})
	case "accept":
		n, err := stepArg(rest, "usage: ensign sequence accept <n>")
		if err != nil {
			return err
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			seq, st := sf.step(n)
			if st == nil || !st.deciding() {
				return fmt.Errorf("step %d is not waiting for a decision", n)
			}
			st.Status = "failed"
			sf.failed(bridge, seq, st)
			text, err := sf.advance(bridge, session)
			if text == "" {
				text = sf.listing(bridge)
			}
			fmt.Fprint(out, text)
			return err
		})
	case "retry":
		usage := "usage: ensign sequence retry <n> --diagnosis <why it failed> --state <what is left>"
		if len(rest) < 1 {
			return errors.New(usage)
		}
		n, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("%q is not a step number", rest[0])
		}
		var diagnosis, left string
		for i := 1; i < len(rest); i += 2 {
			if i+1 >= len(rest) {
				return errors.New(usage)
			}
			switch rest[i] {
			case "--diagnosis":
				diagnosis = strings.TrimSpace(rest[i+1])
			case "--state":
				left = strings.TrimSpace(rest[i+1])
			default:
				return errors.New(usage)
			}
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			seq, st := sf.step(n)
			switch {
			case st == nil:
				return fmt.Errorf("there is no step %d", n)
			case seq.Halted:
				return fmt.Errorf("the sequence is halted; step %d can't be retried", n)
			case st.Kind != "run":
				return fmt.Errorf("step %d is a %s step; only run steps can be retried, not publish, do or release steps", n, st.Kind)
			case st.Prev != "":
				return fmt.Errorf("step %d was already retried; a step gets one retry", n)
			case !st.deciding():
				return fmt.Errorf("step %d is not waiting for a decision (it is %s)", n, st.Status)
			case diagnosis == "":
				return fmt.Errorf("ensign sequence retry needs --diagnosis <why it failed>")
			case left == "":
				return fmt.Errorf("ensign sequence retry needs --state <what is left>")
			}
			// Only the runs that failed are rerun; the others of the group stay as they are.
			var failed []*seqRun
			var fresh []*Snapshot
			for _, ru := range st.runs() {
				old, err := readRun(bridge, ru.Run)
				if err != nil || old.State.Status != "error" {
					continue
				}
				snap, err := retryRun(bridge, old, diagnosis, left, out)
				if err != nil {
					for _, f := range fresh {
						retireIf(bridge, f.Run, "withdrawn: "+err.Error(), "pending")
					}
					return err
				}
				failed, fresh = append(failed, ru), append(fresh, snap)
			}
			if len(failed) == 0 {
				return fmt.Errorf("step %d has no failed run to retry", n)
			}
			for i, ru := range failed {
				*ru = seqRun{Run: fresh[i].Run, Subtask: fresh[i].Subtask, Location: fresh[i].Location, Prev: ru.Run, Status: "pending"}
			}
			st.Prev, st.Diagnosis = failed[0].Prev, diagnosis
			st.setRuns(st.Runs)
			st.Status, st.Reason, st.Blocks, st.WaitBlocks = "pending", "", 0, 0
			text, err := sf.advance(bridge, session)
			if text == "" {
				text = sf.listing(bridge)
			}
			fmt.Fprint(out, text)
			return err
		})
	case "done", "fail":
		if len(rest) < 1 {
			return fmt.Errorf("usage: ensign sequence done <n> [--note <text>] | fail <n> --reason <text>")
		}
		n, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("%q is not a step number", rest[0])
		}
		flag := "--note"
		if args[0] == "fail" {
			flag = "--reason"
		}
		why, err := flagValue(rest[1:], flag, "usage: ensign sequence done <n> [--note <text>] | fail <n> --reason <text>")
		if err != nil {
			return err
		}
		if args[0] == "fail" && why == "" {
			return fmt.Errorf("ensign sequence fail needs --reason <text>")
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			seq, st := sf.step(n)
			if st == nil || st.Status != "active" || st.Kind != "do" && !(st.Kind == "release" && args[0] == "fail") {
				return fmt.Errorf("step %d is not an active do step (or, for fail, an active release step)", n)
			}
			st.Reason = why
			if args[0] == "done" {
				st.Status = "done"
			} else {
				st.Status = "failed"
				sf.failed(bridge, seq, st)
			}
			text, err := sf.advance(bridge, session)
			if text == "" {
				text = sf.listing(bridge)
			}
			fmt.Fprint(out, text)
			return err
		})
	}
	return fmt.Errorf("usage: ensign sequence [run <project>/<task> | publish <project>/<task> | release <project> <version> | release-run <n> | do <instruction> | mode stop|keep-going | halt | drop <n>... [--reason <text>] | retry <n> --diagnosis <why> --state <what is left> | accept <n> | done <n> | fail <n> --reason <text>]")
}

// retryRun queues a new run for a failed one, with the diagnosis in its work item.
func retryRun(bridge string, old *RunFiles, diagnosis, left string, out io.Writer) (*Snapshot, error) {
	o := runOpts{start: old.Snap.Start, staleOK: old.State.StaleOK, retryOf: old.State.Run, retryBase: old.State.Base, location: old.Snap.Location, subtask: old.Snap.Subtask, rebuild: old.State.Rebuild,
		workItem: old.Snap.WorkItem + "\n\n## Current state\n\nThis is a retry. The workspace holds the previous attempt's work, uncommitted changes included; build on it.\n\nWhy the previous attempt failed: " + diagnosis + "\n\nWhat is left: " + left + "\n"}
	if old.Snap.Narrowed {
		o.sides = sideNames(old.Snap.Sides)
	}
	for _, dep := range old.Snap.Dependencies {
		if dep.Branch != "" && !dep.Auto {
			o.dependencies = append(o.dependencies, dep.Location+"="+dep.Branch)
		}
	}
	if old.Snap.Task != "" {
		o.task = old.Snap.Task
	} else {
		o.pipeline, o.project = old.Snap.Pipeline.Name, old.Snap.Project
	}
	return createRun(bridge, o, true, out)
}

func stepArg(args []string, usage string) (int, error) {
	if len(args) != 1 {
		return 0, errors.New(usage)
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("%q is not a step number", args[0])
	}
	return n, nil
}

// flagValue reads one optional "--flag value" pair from args.
func flagValue(args []string, flag, usage string) (string, error) {
	switch {
	case len(args) == 0:
		return "", nil
	case len(args) == 2 && args[0] == flag:
		return args[1], nil
	}
	return "", errors.New(usage)
}

// checkAddable refuses to add steps while the session has a plain run under way.
func checkAddable(bridge, session string) error {
	live, err := liveSessionRuns(bridge, session)
	if err != nil {
		return err
	}
	for _, id := range live {
		r, err := readRun(bridge, id)
		if err == nil && !r.State.Sequence && r.State.Approved {
			return fmt.Errorf("this session already has an active run, %s; add steps once it has finished", id)
		}
	}
	return nil
}

// seqAdd appends a step to the session's open sequence and prints the sequence. A plain run waiting for go becomes the sequence's first step.
func seqAdd(bridge, session string, out io.Writer, st *seqStep) error {
	return withSeq(bridge, session, func(sf *seqFile) error {
		live, err := liveSessionRuns(bridge, session)
		if err != nil {
			return err
		}
		for _, id := range live {
			r, err := openRun(bridge, id)
			if err != nil {
				return err
			}
			if !r.State.Sequence && !r.State.Approved {
				r.State.Sequence = true
				if err := r.save(); err != nil {
					r.close()
					return err
				}
				if err := unbindSession(bridge, session, id); err != nil {
					r.close()
					return err
				}
				sf.add(&seqStep{Kind: "run", Task: r.Snap.Task, Run: r.State.Run, Headline: headline(r.Snap.WorkItem)})
				fmt.Fprintf(out, "run %s was waiting for go; it is now step %d of the sequence\n", r.State.Run, sf.NextStep)
			}
			r.close()
		}
		sf.add(st)
		fmt.Fprint(out, sf.listing(bridge))
		return nil
	})
}
