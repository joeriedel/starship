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
	"unicode/utf8"

	"github.com/gofrs/flock"
)

// A sequence is an ordered list of steps in one session, approved with a single go. The session's sequences live in <bridge>/.starship/sessions/<session>.sequence; every change is made under that file's lock.

const (
	modeStop      = "stop"
	modeKeepGoing = "keep-going"
	// seqStopBudget is how many times the Stop hook holds a turn open for one waiting step. A step that is deciding is held once, because the session has then been told.
	seqStopBudget = 3
	// maxSeqName is the longest name a sequence may have, in characters.
	maxSeqName = 60
)

type seqStep struct {
	N           int       `json:"n"`    // counts from 1 within its sequence
	Seq         int       `json:"-"`    // the number of its sequence, set when the file is read and by add
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
	Blocks      int       `json:"blocks,omitempty"`      // stops held open for this step; on a deciding step, nonzero once the session has been told
	WaitBlocks  int       `json:"wait_blocks,omitempty"` // stops held open while this step waited for a busy folder
	Prev        string    `json:"prev_run,omitempty"`    // the first run this step's latest retry replaces; set once the step has been retried
	Retries     int       `json:"retries,omitempty"`     // how many times the current group has been retried; a file from before this field has Prev set and 0 here
	SkippedAt   int64     `json:"skipped_at,omitempty"`  // unix nanoseconds when the step was skipped, by the user or because a keep-going run step failed; 0 for a skip from before this field
	Diagnosis   string    `json:"diagnosis,omitempty"`   // why the previous run failed, as the session wrote it
	Position    string    `json:"position,omitempty"`    // publish: its Step X/Y when it ran, taken before it and before any cancelling; "" for a cancelled one
	Queued      bool      `json:"queued,omitempty"`      // publish: its outcome line went into the sequence's Report, so it is not also a note
}

// attempt is the number of the attempt the step is on, counting the first as 1.
func (s *seqStep) attempt() int {
	if s.Retries == 0 && s.Prev != "" {
		return 2
	}
	return s.Retries + 1
}

// skipPrefix starts the reason of a step the user skipped. A keep-going run step that failed has SkippedAt but keeps the run's reason, since the user skipped nothing.
const skipPrefix = "skipped by the user: "

// skipped reports whether a failed run step counts as skipped: the user skipped it, or its run failed in keep-going mode and the step was ended at once, which cancels later publishes of its task. A keep-going step whose run was lost, or that failed to start, is not marked; nor is a step that failed in stop mode and was not skipped.
func (s *seqStep) skipped() bool {
	return s.Status == "failed" && (s.SkippedAt != 0 || strings.HasPrefix(s.Reason, skipPrefix))
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
	N        int    `json:"n"`
	Name     string `json:"name,omitempty"` // what the sequence is for; display only
	Mode     string `json:"mode"`
	Approved bool   `json:"approved"`
	Halted   bool   `json:"halted,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Closed   bool   `json:"closed,omitempty"` // its end has been reported
	// QueueStart marks a sequence approved when no earlier approved sequence was still open, so it starts a new queue for the Step X/Y count
	QueueStart bool       `json:"queue_start,omitempty"`
	Steps      []*seqStep `json:"steps"`
	Report     string     `json:"report,omitempty"` // one-shot text for the session: publish outcomes as they happen and a keep-going sequence's failure report when it closes; cleared once a Stop hook or prompt has delivered it
}

type seqFile struct {
	NextStep  int         `json:"next_step,omitempty"` // only in files written when steps were numbered across the session
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
	existed := true
	if err := readJSON(path, sf); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		existed = false
	}
	sf.normalize()
	if err := fn(sf); err != nil {
		return err
	}
	// A file whose last sequence was discarded is rewritten empty; a session that never had one gets no file.
	if len(sf.Sequences) == 0 && !existed {
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
	sf.normalize()
	return sf
}

// normalize numbers each sequence's steps from 1 in their stored order, which also converts a file written when steps were numbered across the session, and tells each step its sequence.
func (sf *seqFile) normalize() {
	for _, seq := range sf.Sequences {
		for i, st := range seq.Steps {
			st.N, st.Seq = i+1, seq.N
		}
	}
	sf.NextStep = 0
}

// label names a sequence in text for the session: its number, then its name when it has one.
func (s *sequence) label() string {
	if s.Name == "" {
		return fmt.Sprintf("sequence %d", s.N)
	}
	return fmt.Sprintf("sequence %d \"%s\"", s.N, s.Name)
}

// ref is how text outside a sequence's own listing names a step: <sequence>.<step>.
func (s *seqStep) ref() string { return fmt.Sprintf("%d.%d", s.Seq, s.N) }

// counts reports whether a step is part of the session's progress count: one that was cancelled, skipped, dropped or discarded never ran.
func (s *seqStep) counts() bool {
	switch s.Status {
	case "cancelled", "skipped", "dropped", "discarded":
		return false
	}
	return true
}

// position says where a step stands among all the steps that run in its queue, such as "Step 5/26": the approved sequences from one QueueStart to the next. Closed sequences stay in the count within the queue so that it doesn't restart when the next queued sequence begins. A file without marks is one queue from its first approved sequence.
func (sf *seqFile) position(st *seqStep) string {
	x, y := 0, 0
	for _, seq := range sf.Sequences {
		if !seq.Approved {
			continue
		}
		if seq.QueueStart {
			if x > 0 {
				break
			}
			y = 0
		}
		for _, o := range seq.Steps {
			if !o.counts() {
				continue
			}
			y++
			if o == st {
				x = y
			}
		}
	}
	return fmt.Sprintf("Step %d/%d", x, y)
}

// nowAt is the line that tells the session which step it is at.
func (sf *seqFile) nowAt(st *seqStep) string {
	return fmt.Sprintf("Now at step %s (%s).", st.ref(), sf.position(st))
}

// outcomeLine says in one line how a step ended; it is the text of a failed or cancelled step in an advance and in a note, so the two agree.
func (s *seqStep) outcomeLine(status, reason string) string {
	what := s.Task
	switch s.Kind {
	case "do":
		what = firstLine(strings.TrimSpace(s.Instruction))
	case "release":
		what += " " + s.Version
	}
	lead := fmt.Sprintf("step %s (%s %s) %s", s.ref(), s.Kind, what, status)
	if reason == "" {
		return lead + "."
	}
	return lead + ": " + reason
}

// outcomeKey is the start of an outcome line that identifies it: up to and including the colon, or the whole line when there is no reason.
func (s *seqStep) outcomeKey(status, reason string) string {
	line := s.outcomeLine(status, reason)
	if reason == "" {
		return line
	}
	return line[:len(line)-len(reason)-1]
}

// notes returns a line for each failed or cancelled step, in step order, and the key that identifies each. A publish whose outcome is queued for the session, or will be when its keep-going sequence closes halted, is left out.
func (s *sequence) notes() (lines, keys []string) {
	for _, st := range s.Steps {
		if st.Status != "failed" && st.Status != "cancelled" {
			continue
		}
		if st.Kind == "publish" && (st.Queued || (s.Mode == modeKeepGoing && s.Halted)) {
			continue
		}
		reason := firstLine(st.Reason)
		lines = append(lines, st.outcomeLine(st.Status, reason))
		keys = append(keys, st.outcomeKey(st.Status, reason))
	}
	return
}

// unreportedNotes is the sequence's notes without those a part already reports. A keep-going sequence that isn't halted reports its failures once, in its report.
func (s *sequence) unreportedNotes(parts []string) []string {
	if s.Mode == modeKeepGoing && !s.Halted {
		return nil
	}
	lines, keys := s.notes()
	var out []string
	for i, line := range lines {
		if !slices.ContainsFunc(parts, func(p string) bool { return strings.HasPrefix(p, keys[i]) }) {
			out = append(out, line)
		}
	}
	return out
}

// held finds the step an advance dispatched or holds, walking the sequences as advanceParts does, except that a pending step that can't start yet is not held.
func (sf *seqFile) held() (*sequence, *seqStep) {
	for _, seq := range sf.Sequences {
		if seq.Closed {
			continue
		}
		if !seq.Approved && !seq.Halted {
			break
		}
		for _, st := range seq.Steps {
			if st.holds() || st.deciding() {
				return seq, st
			}
		}
	}
	return nil, nil
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

// onlyWaitingSequenceUnfinished reports whether the sequence waiting for go has unfinished steps and no approved sequence does, so discard is the way to clear it.
func onlyWaitingSequenceUnfinished(bridge, session string) bool {
	sf := readSeq(bridge, session)
	for _, seq := range sf.Sequences {
		if seq.Approved && seq.unfinished() {
			return false
		}
	}
	open := sf.openSeq()
	return open != nil && open.unfinished()
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

func (sf *seqFile) step(seqN, n int) (*sequence, *seqStep) {
	for _, seq := range sf.Sequences {
		if seq.N != seqN {
			continue
		}
		for _, st := range seq.Steps {
			if st.N == n {
				return seq, st
			}
		}
		return seq, nil
	}
	return nil, nil
}

// stepRef finds the step a command names: <seq>.<step>, or a plain <step> when exactly one sequence is open.
func (sf *seqFile) stepRef(arg string) (*sequence, *seqStep, error) {
	seqPart, stepPart, dotted := strings.Cut(arg, ".")
	if !dotted {
		stepPart = arg
	}
	n, err := strconv.Atoi(stepPart)
	seqN := 0
	if dotted && err == nil {
		seqN, err = strconv.Atoi(seqPart)
	}
	if err != nil || n < 1 || (dotted && seqN < 1) {
		return nil, nil, fmt.Errorf("%q is not a step; write <seq>.<step>, for example 2.3", arg)
	}
	if !dotted {
		var open []*sequence
		for _, seq := range sf.Sequences {
			if sf.isOpen(seq) {
				open = append(open, seq)
			}
		}
		switch len(open) {
		case 0:
			return nil, nil, fmt.Errorf("no sequence is open, so step %d is ambiguous; write <seq>.<step>, for example 2.%d (`ensign sequence --all` lists every sequence)", n, n)
		case 1:
			seqN = open[0].N
		default:
			var names []string
			for _, seq := range open {
				names = append(names, seq.label())
			}
			return nil, nil, fmt.Errorf("%d sequences are open (%s), so step %d is ambiguous; write <seq>.<step>, for example %d.%d", len(open), strings.Join(names, ", "), n, open[0].N, n)
		}
	}
	seq, st := sf.step(seqN, n)
	switch {
	case seq == nil && dotted:
		return nil, nil, fmt.Errorf("there is no sequence %d", seqN)
	case st == nil:
		return nil, nil, fmt.Errorf("%s has no step %d", seq.label(), n)
	}
	return seq, st, nil
}

// add appends a step to the open sequence, starting one with the given name if the newest is approved or halted.
func (sf *seqFile) add(st *seqStep, name string) {
	seq := sf.openSeq()
	if seq == nil {
		seq = &sequence{N: len(sf.Sequences) + 1, Mode: modeStop, Name: name}
		sf.Sequences = append(sf.Sequences, seq)
	}
	st.Seq, st.N, st.Status = seq.N, len(seq.Steps)+1, "pending"
	seq.Steps = append(seq.Steps, st)
}

// isOpen reports that a sequence still matters to the session: it has unfinished steps and is either approved or the one awaiting go.
func (sf *seqFile) isOpen(seq *sequence) bool {
	return len(seq.Steps) > 0 && !seq.Closed && seq.unfinished() && (seq.Approved || seq == sf.openSeq())
}

// listing renders the open sequences only, so it stays short however many sequences a session has run.
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
		fmt.Fprintf(&b, "%s (mode %s, %s)\n", seq.label(), seq.Mode, state)
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
				line += fmt.Sprintf(" (attempt %d", st.attempt())
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
	// pipeline.json was read, so the run exists; a missing state.json means a save is in flight.
	if err := readExistingJSON(filepath.Join(dir, "state.json"), r.State); err != nil {
		return nil, err
	}
	return r, nil
}

// haltPrefix starts the reason of a run that halt retired, so reconcile can tell it from a run that failed.
const haltPrefix = "cancelled: "

// reconcile brings the steps of runs under way in line with their runs' saved states. A step with several runs waits until all of them are over before it succeeds or fails.
func (sf *seqFile) reconcile(bridge string) {
	for _, seq := range sf.Sequences {
		for _, st := range seq.Steps {
			if sf.endStaleDecision(bridge, seq, st) {
				continue
			}
			if st.Kind != "run" || !st.holds() {
				continue
			}
			runs := st.runs()
			var snaps []*RunFiles
			over, failed, retired, lost := 0, 0, 0, false
			var reason, retiredReason, asked string
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
					if seq.Halted && strings.HasPrefix(ru.Reason, haltPrefix) {
						retired++
						if retiredReason == "" {
							retiredReason = strings.TrimPrefix(ru.Reason, haltPrefix)
						}
					} else if reason == "" {
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
			case failed > 0 && failed == retired:
				// Every failed run was retired by halt, so the step was cancelled, not failed.
				st.Status, st.Reason = "cancelled", retiredReason
			case failed > 0:
				st.Reason = reason
				switch {
				case !lost && !seq.Halted && seq.Mode == modeKeepGoing:
					sf.endKeepGoingFailure(bridge, seq, st)
				case !lost && !seq.Halted:
					st.Status = "decide"
					st.Blocks = 0
				default:
					st.Status = "failed"
					sf.failed(bridge, seq, st)
				}
			default:
				st.Status, st.Reason = "done", ""
				if snaps[0].Snap.Subtask != "" {
					sf.nextSubtaskGroup(bridge, seq, st, snaps)
				}
			}
		}
	}
}

// endStaleDecision ends a deciding run step of a keep-going sequence that isn't halted and reports whether it did. A file from before keep-going stopped asking can hold one.
func (sf *seqFile) endStaleDecision(bridge string, seq *sequence, st *seqStep) bool {
	if st.Kind != "run" || !st.deciding() || seq.Mode != modeKeepGoing || seq.Halted {
		return false
	}
	sf.endKeepGoingFailure(bridge, seq, st)
	return true
}

// endKeepGoingFailure ends a run step that failed in a keep-going sequence at once: no decision, the task's later steps skipped. SkippedAt keeps a later sequence from publishing the task, and the reason stays the run's own.
func (sf *seqFile) endKeepGoingFailure(bridge string, seq *sequence, st *seqStep) {
	st.Status, st.SkippedAt = "failed", time.Now().UnixNano()
	sf.failed(bridge, seq, st)
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
				if rerr := retireIf(bridge, m.Run, "withdrawn: "+err.Error(), "pending"); rerr != nil {
					err = errors.Join(err, fmt.Errorf("run %s could not be withdrawn: %w", m.Run, rerr))
				}
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
		st.Status, st.Reason = "failed", oneLine(err)
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
	st.Group, st.Prev, st.Retries, st.Diagnosis, st.Headline = fresh.Group, "", 0, "", fresh.Headline
	st.Status, st.Reason, st.Blocks, st.WaitBlocks = "pending", "", 0, 0
}

// oneLine flattens an error to one line, as the displays cut a reason to its first line.
func oneLine(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", "; ")
}

// seqStderr is where a sequence failure with no caller to return to is reported.
var seqStderr io.Writer = os.Stderr

// failed applies the failure rules for a step that has just failed. A run that could not be retired in the cascade has no caller to tell, so it goes to stderr.
func (sf *seqFile) failed(bridge string, seq *sequence, failed *seqStep) {
	why := fmt.Sprintf("step %s failed", failed.ref())
	if seq.Mode == modeKeepGoing {
		for _, st := range seq.Steps {
			if st.Status == "pending" && failed.Task != "" && (st.Task == failed.Task ||
				(failed.Kind == "release" && strings.HasPrefix(st.Task, failed.Task+"/"))) {
				if err := cancelStep(bridge, st, "skipped", why); err != nil {
					fmt.Fprintln(seqStderr, "ensign:", oneLine(err))
				}
			}
		}
		return
	}
	if !seq.Halted {
		seq.Halted, seq.Reason = true, why+": "+firstLine(failed.Reason)
	}
	for _, st := range seq.Steps {
		if st.Status == "pending" {
			if err := cancelStep(bridge, st, "cancelled", why); err != nil {
				fmt.Fprintln(seqStderr, "ensign:", oneLine(err))
			}
		}
	}
}

// cancelStep ends a step that never started. Its runs are made terminal, as a retired run is, so they are nobody's waiting run; no run.finish handler fires for them. Only runs still pending in the step are opened: a retried group step keeps its finished runs, and a finishing hook may hold one of those while it waits for the sequence lock, which the caller holds. A run that could not be retired is returned, and also added to the step's reason for `ensign sequence --all`.
func cancelStep(bridge string, st *seqStep, status, reason string) error {
	st.Status, st.Reason = status, reason
	if st.Kind != "run" {
		return nil
	}
	var errs []error
	for _, ru := range st.runs() {
		if ru.Status != "" && ru.Status != "pending" {
			continue
		}
		if err := retireForHalt(bridge, ru.Run, status+": "+reason, false); err != nil {
			errs = append(errs, fmt.Errorf("run %s could not be retired: %w", ru.Run, err))
		}
	}
	err := errors.Join(errs...)
	if err != nil {
		st.Reason += "; " + oneLine(err)
	}
	return err
}

// unstarted reports whether a run is queued, or approved with its lead never dispatched.
func unstarted(st *State) bool {
	return st.Status == "pending" || (st.Status == "running" && st.Approved && !st.Started && len(st.Dispatches) == 0)
}

// retireForHalt retires a run halt collected: an unstarted one, or one that asks the user when paused is set. A run in any other state, or whose folder is gone, is left alone with no error; the error is for a run that should have been retired and could not be.
func retireForHalt(bridge, id, reason string, paused bool) error {
	r, err := openRun(bridge, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !unstarted(r.State) && !(paused && r.State.Status == "ask-user") {
		r.close()
		return nil
	}
	return markRetired(r, reason)
}

// retireIf makes a run terminal, but only if it is still in one of the given states when its lock is held. A run that is not, or whose folder is gone, is left alone with no error.
func retireIf(bridge, id, reason string, states ...string) error {
	r, err := openRun(bridge, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(states, r.State.Status) {
		r.close()
		return nil
	}
	return markRetired(r, reason)
}

// advance starts the next step if none is under way. It returns what the session should be told, followed by the listing, or "" when nothing changed.
func (sf *seqFile) advance(bridge, session string) (string, error) {
	text, err := sf.advanceText(bridge, session)
	if text != "" {
		if _, held := sf.held(); held != nil {
			text += "\n\n" + sf.nowAt(held)
		}
		text += "\n\n" + sf.listing(bridge)
	}
	return text, err
}

// advanceText is advance without the listing.
func (sf *seqFile) advanceText(bridge, session string) (string, error) {
	parts, err := sf.advanceParts(bridge, session)
	return strings.Join(parts, "\n"), err
}

// advanceParts does the advancing and returns what the session should be told, one entry per piece; a piece can span lines.
func (sf *seqFile) advanceParts(bridge, session string) ([]string, error) {
	sf.reconcile(bridge)
	var parts []string
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
						parts = append(parts, fmt.Sprintf("step %s: %s", st.ref(), t))
					}
				}
			}
			if seq.holding() {
				return parts, nil
			}
			st := seq.nextPending()
			if st == nil {
				seq.Closed = true
				if seq.Halted {
					parts = append(parts, fmt.Sprintf("%s halted: %s", seq.label(), seq.Reason))
					if seq.Mode == modeKeepGoing {
						for _, st := range seq.Steps {
							if st.Kind == "publish" && (st.Status == "failed" || st.Status == "cancelled") {
								parts = publishReport(seq, st, parts, st.publishLine(st.Status, st.Reason), true)
							}
						}
					}
				} else {
					keepGoing := seq.Mode == modeKeepGoing
					var skips []string
					for _, s := range seq.Steps {
						if (keepGoing && s.Status == "failed") || (!keepGoing && s.skipped()) {
							skips = append(skips, s.ref())
						}
					}
					if keepGoing {
						seq.Report = joinReport(seq.Report, sf.failureReport(bridge, seq))
					}
					switch {
					case len(skips) == 0:
						parts = append(parts, fmt.Sprintf("%s finished.", seq.label()))
					case keepGoing:
						noun := "steps"
						if len(skips) == 1 {
							noun = "step"
						}
						parts = append(parts, fmt.Sprintf("%s finished with %d failed %s: %s.", seq.label(), len(skips), noun, strings.Join(skips, ", ")))
					case len(skips) == 1:
						parts = append(parts, fmt.Sprintf("%s finished with a skipped step: step %s.", seq.label(), skips[0]))
					default:
						parts = append(parts, fmt.Sprintf("%s finished with %d skipped steps: %s.", seq.label(), len(skips), strings.Join(skips, ", ")))
					}
				}
				break
			}
			switch st.Kind {
			case "run":
				texts, busy, err := startRuns(bridge, session, st)
				switch {
				case len(texts) > 0:
					st.Status, st.Reason = "active", ""
					parts = append(parts, fmt.Sprintf("step %s: %s", st.ref(), strings.Join(texts, "\n\n")))
					if busy != nil {
						parts = append(parts, fmt.Sprintf("step %s: another run of it can't start yet: %v", st.ref(), busy))
					}
				case busy != nil:
					st.Reason = "waiting: " + busy.Error()
					parts = append(parts, fmt.Sprintf("step %s can't start yet: %v", st.ref(), busy))
				case err != nil:
					st.Status, st.Reason = "failed", err.Error()
					sf.failed(bridge, seq, st)
					parts = append(parts, st.outcomeLine("failed", err.Error()))
					continue
				default:
					st.Status, st.Reason = "active", ""
				}
				return parts, nil
			case "publish":
				if sk := sf.skippedBefore(bridge, seq, st); sk != nil {
					cancelStep(bridge, st, "cancelled", fmt.Sprintf("step %s was skipped", sk.ref()))
					st.Position = ""
					parts = publishReport(seq, st, parts, st.publishLine("cancelled", fmt.Sprintf("step %s was skipped", sk.ref())), false)
					continue
				}
				st.Position = sf.position(st)
				out, err := runPublish(bridge, seq, st)
				if err != nil {
					st.Status, st.Reason = "failed", err.Error()
					sf.failed(bridge, seq, st)
					parts = publishReport(seq, st, parts, st.publishLine("failed", err.Error()), false)
					continue
				}
				st.Status, st.Reason = "done", out
				parts = publishReport(seq, st, parts, st.publishLine("done", out), true)
			case "do":
				st.Status = "active"
				parts = append(parts, fmt.Sprintf("step %s: %s", st.ref(), doText(st)))
				return parts, nil
			case "release":
				st.Status = "active"
				parts = append(parts, fmt.Sprintf("step %s: %s", st.ref(), releaseText(st)))
				return parts, nil
			}
		}
	}
	return parts, nil
}

// publishLine is a publish's outcome line led by the position it had when it ran, when it has one.
func (s *seqStep) publishLine(status, reason string) string {
	line := s.outcomeLine(status, reason)
	if s.Position == "" || status == "cancelled" {
		return line
	}
	return s.Position + ": " + line
}

// joinReport adds text to a queued report on a new line.
func joinReport(report, text string) string {
	if report == "" || text == "" {
		return report + text
	}
	return report + "\n" + text
}

// publishReport routes a publish's outcome line: a keep-going failure stays in parts, since its sequence's failure report lists it once at close; every other outcome is queued in the sequence's report, the one way out, so no caller that also prints parts delivers it twice.
func publishReport(seq *sequence, st *seqStep, parts []string, line string, queue bool) []string {
	if seq.Mode == modeKeepGoing && !queue {
		return append(parts, line)
	}
	st.Queued = true
	seq.Report = joinReport(seq.Report, line)
	return parts
}

// takeReports collects and clears the queued reports of every sequence.
func (sf *seqFile) takeReports() []string {
	var reports []string
	for _, seq := range sf.Sequences {
		if seq.Report != "" {
			reports = append(reports, seq.Report)
			seq.Report = ""
		}
	}
	return reports
}

// takeSequenceReports is what a prompt delivers: the session's queued reports, cleared. It looks without the lock first, since a publish holds the lock, so a prompt with nothing queued never waits for one.
func takeSequenceReports(bridge, session string) string {
	if !seqExists(bridge, session) || !slices.ContainsFunc(readSeq(bridge, session).Sequences, func(s *sequence) bool { return s.Report != "" }) {
		return ""
	}
	var reports []string
	if err := withSeq(bridge, session, func(sf *seqFile) error {
		reports = sf.takeReports()
		return nil
	}); err != nil {
		return ""
	}
	return strings.Join(reports, "\n\n")
}

// skippedBefore returns the latest skipped run step of the publish's task, before the publish step in any sequence, that ran in the location the publish will act in; it is nil when the publish may go ahead. When that location can't be resolved, any skipped step of the task counts and the clear check looks at the locations of its runs. A successful run of the task in the location that finished after the step was skipped clears the skip, whether it came from a sequence or from a plain `ensign run`. Run ids are stamped at creation and a retry is created after the steps queued behind it, so only finish times against SkippedAt say what came after; a step skipped before SkippedAt existed has none, and its skip stands.
func (sf *seqFile) skippedBefore(bridge string, seq *sequence, pub *seqStep) *seqStep {
	loc, known := "", false
	if c, err := loadConfigAt(bridge, pub.Task, precedingRunLocation(bridge, seq, pub.N, pub.Task)); err == nil {
		loc, known = c.Location, true
	}
	var skipped *seqStep
	var locs []string
	for _, s := range sf.Sequences {
		for _, st := range s.Steps {
			if s == seq && st.N >= pub.N {
				break
			}
			if st.Kind != "run" || st.Task != pub.Task || !st.skipped() {
				continue
			}
			var here []string
			for _, ru := range st.runs() {
				at, resolved := ru.Location, ru.Location != ""
				if !resolved {
					var snap Snapshot
					if readJSON(filepath.Join(runsDir(bridge), ru.Run, "pipeline.json"), &snap) == nil {
						at, resolved = snap.Location, true
					}
				}
				if !resolved || !known || at == loc {
					here = append(here, at)
				}
			}
			if len(here) > 0 {
				skipped, locs = st, here
			}
		}
		if s == seq {
			break
		}
	}
	if skipped == nil {
		return nil
	}
	if skipped.SkippedAt == 0 {
		return skipped
	}
	for _, ri := range listRuns(bridge) {
		if ri.snap.Task == pub.Task && slices.Contains(locs, ri.snap.Location) && ri.state.Status == "success" && ri.state.FinishedAt > skipped.SkippedAt {
			return nil
		}
	}
	return skipped
}

func doText(st *seqStep) string {
	return fmt.Sprintf("%s\nWhen it is done run `ensign sequence done %s`, or `ensign sequence fail %s --reason …` if it can't be.",
		st.Instruction, st.ref(), st.ref())
}

func releaseText(st *seqStep) string {
	return fmt.Sprintf("Release %s %s. Start `ensign sequence release-run %s` as a background shell command (Bash with run_in_background), wait for its completion notification without polling, then report the outcome. If the command dies without finishing, run `ensign sequence fail %s --reason …`.",
		st.Task, st.Version, st.ref(), st.ref())
}

// skipWorkspaceUnknown is told for a folder whose state can't be read.
const skipWorkspaceUnknown = "The failed run's uncommitted work stays in its workspace, and a later run in that folder won't start until it is committed or stashed."

// maxSkipPaths is how many changed paths the skip warning names for a folder.
const maxSkipPaths = 10

// skipWorkspaceText says what the folders of the step's failed runs hold now. It reads no run's lock and never fails: a folder it can't read gets the generic sentence.
func skipWorkspaceText(bridge string, st *seqStep) string {
	lines, unknown := failedFolderLines(bridge, st)
	if unknown || len(lines) == 0 {
		lines = append(lines, skipWorkspaceUnknown)
	}
	return strings.Join(lines, "\n")
}

// failedFolderLines is a line for the folder of each failed run of the step, and whether some run's folder could not be found.
func failedFolderLines(bridge string, st *seqStep) (lines []string, unknown bool) {
	var seen []string
	for _, ru := range st.runs() {
		r, err := readRun(bridge, ru.Run)
		if err != nil || r.Snap.Workspace == "" {
			unknown = true
			continue
		}
		ws := r.Snap.Workspace
		if r.State.Status != "error" || slices.ContainsFunc(seen, func(o string) bool { return sameFolder(o, ws, runtime.GOOS) }) {
			continue
		}
		seen = append(seen, ws)
		dirty, err := dirtyPaths(ws)
		branch := orDetached(currentBranch(ws))
		switch {
		case err != nil:
			lines = append(lines, ws+": its state could not be read. "+skipWorkspaceUnknown)
		case len(dirty) == 0:
			lines = append(lines, fmt.Sprintf("%s (on %s) has no uncommitted changes, so nothing blocks a later run there.", ws, branch))
		default:
			var names []string
			for _, d := range dirty[:min(len(dirty), maxSkipPaths)] {
				names = append(names, d.path)
			}
			list := strings.Join(names, ", ")
			if len(dirty) > maxSkipPaths {
				list += fmt.Sprintf(" and %d more", len(dirty)-maxSkipPaths)
			}
			lines = append(lines, fmt.Sprintf("%s (on %s) has uncommitted changes: %s. A later run in that folder won't start until they are committed or stashed.", ws, branch, list))
		}
	}
	return lines, unknown
}

// failureReport is the report of a closing keep-going sequence: every failed step with why, the steps skipped or cancelled, and the state of the failed runs' folders as they are now. It is "" when nothing failed, was skipped or was cancelled.
func (sf *seqFile) failureReport(bridge string, seq *sequence) string {
	var lines []string
	listed := map[*seqStep]bool{}
	for _, st := range seq.Steps {
		if st.Status != "failed" {
			continue
		}
		lines = append(lines, "- "+st.publishLine("failed", firstLine(st.Reason)))
		if st.Kind == "run" {
			if len(st.runs()) > 1 {
				for _, ru := range st.runs() {
					if ru.Status == "error" {
						lines = append(lines, fmt.Sprintf("  run %s failed: %s", ru.Run, firstLine(ru.Reason)))
					}
				}
			}
			folders, unknown := failedFolderLines(bridge, st)
			if unknown && len(folders) == 0 && slices.ContainsFunc(st.runs(), func(ru *seqRun) bool { return ru.Status == "error" }) {
				folders = append(folders, skipWorkspaceUnknown)
			}
			for _, f := range folders {
				lines = append(lines, "  folder: "+f)
			}
		}
		why := fmt.Sprintf("step %s failed", st.ref())
		for _, o := range seq.Steps {
			if o.Status == "skipped" && strings.HasPrefix(o.Reason, why) {
				lines = append(lines, "  "+o.outcomeLine("skipped", firstLine(o.Reason)))
				listed[o] = true
			}
		}
	}
	for _, st := range seq.Steps {
		if (st.Status == "skipped" && !listed[st]) || st.Status == "cancelled" {
			lines = append(lines, "- "+st.outcomeLine(st.Status, firstLine(st.Reason)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("%s ran in keep-going mode and did not do everything:\n%s\nTell the user this now, in plain words: what failed and why, what was skipped or cancelled because of it, and the state of the folders. Nothing is waiting for a decision, and you must not retry, skip or accept anything; the user decides what happens next.", seq.label(), strings.Join(lines, "\n"))
}

// decideBriefText is the requirement on the session's message to the user, for stop mode; the Stop hook repeats itself, so it asks for the whole brief every time.
const decideBriefText = "Your message to the user must be a full brief, every time, even if an earlier message already said the same thing. It says: what failed and why, in plain words, not just the reason line above; what state the work is in; what each of retry, skip and halt would do; and every decision a retry needs, with each option spelled out and a recommendation. Never refer to an option by letter or by pointing at an earlier message, because this message repeats and earlier text scrolls away.\n"

// decideText tells the session to take a failed step of a stop-mode sequence to the user, who chooses retry, skip or halt.
func decideText(bridge string, sf *seqFile, seq *sequence, st *seqStep) string {
	ref := st.ref()
	text := fmt.Sprintf("Its run failed: %s\nTake this to the user now. Offer exactly three choices, in this order: retry, skip, halt. Do not act until the user chooses, and end your turn to wait for the answer.\n", firstLine(st.Reason))
	text += decideBriefText
	text += fmt.Sprintf("- retry: `ensign sequence retry %s --diagnosis \"<why it failed>\" --state \"<what is left>\"`; fill in the diagnosis and state after the user chooses retry.\n", ref)
	text += fmt.Sprintf("- skip: `ensign sequence skip %s` ends the step as failed, cancels the task's publish step, and the rest of the sequence carries on. %s\n", ref, skipWorkspaceText(bridge, st))
	text += fmt.Sprintf("- halt: `ensign sequence accept %s` ends the step as failed, halts the sequence and cancels every later step.\n", ref)
	text += "Say retry, skip and halt to the user, not the command names."
	if affected := skipAffectedText(skipAffected(sf, seq, st)); affected != "" {
		text += "\n" + affected
	}
	return text
}

// namesTask reports whether text contains the task as a whole name, either <project>/<task> or the bare <task>, ignoring case.
func namesTask(text, task string) bool {
	text, task = strings.ToLower(text), strings.ToLower(task)
	_, bare, _ := strings.Cut(task, "/")
	isName := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' }
	for _, name := range []string{task, bare} {
		if name == "" {
			continue
		}
		for from := 0; ; {
			i := strings.Index(text[from:], name)
			if i < 0 {
				break
			}
			i += from
			from = i + 1
			end := i + len(name)
			if i > 0 && isName(text[i-1]) {
				continue
			}
			if name == bare && i > 0 && text[i-1] == '/' && !(strings.HasSuffix(text[:i-1], "task") && (i == 5 || !isName(text[i-6]))) {
				continue
			}
			if end < len(text) && isName(text[end]) {
				continue
			}
			return true
		}
	}
	return false
}

// skipAffected lists the pending later steps that skipping a failed run step may leave without what they assume: release steps, do steps that name its task, and run steps of the same task, in its sequence after it and in every later open sequence (approved or not). Its own sequence's publish steps are cancelled, so they are not listed; a later sequence's are, and advance cancels each when reached unless the task ran again and passed first in that location.
func skipAffected(sf *seqFile, seq *sequence, failed *seqStep) []*seqStep {
	var out []*seqStep
	later := false
	for _, s := range sf.Sequences {
		if s == seq {
			later = true
		} else if !later || s.Closed || s.Halted {
			continue
		}
		for _, st := range s.Steps {
			if st.Status != "pending" || (s == seq && st.N <= failed.N) {
				continue
			}
			if st.Kind == "release" || (st.Kind == "do" && namesTask(st.Instruction, failed.Task)) || (st.Kind == "run" && st.Task == failed.Task) || (st.Kind == "publish" && s != seq && st.Task == failed.Task) {
				out = append(out, st)
			}
		}
	}
	return out
}

// skipAffectedText names the steps skipAffected found, one per line. It is empty when there are none.
func skipAffectedText(list []*seqStep) string {
	if len(list) == 0 {
		return ""
	}
	lines := make([]string, 0, len(list))
	for _, st := range list {
		what := st.Task
		switch st.Kind {
		case "release":
			what = st.Task + " " + st.Version
		case "do":
			what = firstLine(st.Instruction)
		}
		line := fmt.Sprintf("  step %s, %s: %s", st.ref(), st.Kind, what)
		if st.Kind == "publish" {
			line += " (cancelled when reached, unless the task runs again and passes first in that location)"
		}
		lines = append(lines, line)
	}
	return "These later steps may be affected, so check them:\n" + strings.Join(lines, "\n")
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
			if rerr := retireIf(bridge, ru.Run, "failed: "+err.Error(), "pending"); rerr != nil {
				err = fmt.Errorf("%w; run %s could not be retired: %s", err, ru.Run, oneLine(rerr))
				fmt.Fprintf(seqStderr, "ensign: run %s could not be retired: %s\n", ru.Run, oneLine(rerr))
			}
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
	bindErr := bindSession(bridge, session, run)
	if bindErr != nil && !errors.Is(bindErr, errSetupFile) {
		return "", bindErr
	}
	if task != "" {
		os.WriteFile(sessionFile(bridge, session)+".task", []byte(task+"\n"), 0o644)
	}
	text := approvalText(r)
	if bindErr != nil {
		text += "\nstarship: " + bindErr.Error()
	}
	return text, nil
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
		seq.QueueStart = !slices.ContainsFunc(sf.Sequences, func(o *sequence) bool { return o != seq && o.Approved && !o.Closed })
		ok, seq.Approved = true, true
		text = fmt.Sprintf("%s approved (%d steps).", seq.label(), len(seq.Steps))
		parts, err := sf.advanceParts(bridge, session)
		reports := sf.takeReports()
		heldSeq, heldStep := sf.held()
		if len(parts) > 0 {
			text += " " + strings.Join(parts, "\n")
		} else if heldStep != nil {
			text += " It starts when the step in progress finishes."
		}
		if heldStep != nil {
			text += "\n\n" + sf.nowAt(heldStep)
			for _, n := range heldSeq.unreportedNotes(parts) {
				text += "\n" + n
			}
		}
		if len(reports) > 0 {
			text += "\n\n" + strings.Join(reports, "\n\n")
		}
		text += "\n\nSequence rules: `ensign help sequence`."
		return err
	})
	return
}

// waitingText says what a run step waits for when its run, or the step, is waiting for a busy code folder, else "".
func waitingText(st *seqStep) string {
	var lines []string
	if strings.HasPrefix(st.Reason, "waiting:") {
		lines = append(lines, fmt.Sprintf("Step %s is %s", st.ref(), st.Reason))
	}
	for _, ru := range st.runs() {
		if strings.HasPrefix(ru.Reason, "waiting:") && ru.Reason != st.Reason {
			lines = append(lines, fmt.Sprintf("Step %s, run %s, is %s", st.ref(), ru.Run, ru.Reason))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\nIt starts by itself once that run has finished; this turn is held only a few times for it."
}

// sequenceStop holds a turn open while a step waits for the session to act on it. Each step gets seqStopBudget holds, as the sequence file's lock allows, then the turn may end. A deciding step is held once per failed attempt, because the session has then been told.
func sequenceStop(bridge string, in *HookInput) any {
	if !seqExists(bridge, in.SessionID) {
		return nil
	}
	var out any
	withSeq(bridge, in.SessionID, func(sf *seqFile) error {
		adv, advErr := sf.advanceParts(bridge, in.SessionID)
		var advNote string
		if advErr != nil {
			advNote = "The sequence could not advance: " + advErr.Error()
		}
		reports := sf.takeReports()
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				var todo string
				switch {
				case st.deciding():
					if st.Blocks == 0 {
						todo = decideText(bridge, sf, seq, st)
					}
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
				own := []string{fmt.Sprintf("step %s: ", st.ref()), fmt.Sprintf("step %s can't start yet", st.ref())}
				kept := slices.DeleteFunc(slices.Clone(adv), func(p string) bool {
					return slices.ContainsFunc(own, func(o string) bool { return strings.HasPrefix(p, o) })
				})
				msg := []string{fmt.Sprintf("sequence step %s is waiting for you (%s).", st.ref(), sf.position(st)), todo, strings.Join(kept, "\n"), strings.Join(seq.unreportedNotes(kept), "\n"), strings.Join(reports, "\n\n"), advNote, "Sequence rules: `ensign help sequence`."}
				msg = slices.DeleteFunc(msg, func(b string) bool { return strings.TrimSpace(b) == "" })
				out = block(strings.Join(msg, "\n\n"))
				return nil
			}
		}
		if len(reports) > 0 {
			out = block(strings.Join(append(reports, "Sequence rules: `ensign help sequence`."), "\n\n"))
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
	// A finishing hook holds a run's lock and then takes this sequence lock, so the runs are opened after the lock is released.
	var ids []string
	err = withSeq(bridge, session, func(sf *seqFile) error {
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				if st.Kind != "run" || !st.open() {
					continue
				}
				if st.deciding() {
					st.Blocks = 0
				}
				for _, ru := range st.runs() {
					if !slices.Contains(bound, ru.Run) {
						ids = append(ids, ru.Run)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if r, err := openRun(bridge, id); err == nil {
			if r.State.Session == old {
				r.State.Session = session
				r.save()
			}
			r.close()
		}
	}
	return nil
}

// releaseCommand finds a project's @release command and the folder it runs in: the project's workspace, or the folder of the location asked for (the project's only one if none is asked for), whose bindings add to the project's.
func releaseCommand(bridge, project, location string) (cmd, workspace string, err error) {
	location, workspace, _, err = projectLocation(bridge, project, location)
	if err != nil {
		return "", "", err
	}
	c, err := loadProjectBindings(bridge, project, location)
	if err != nil {
		return "", "", err
	}
	b := c.Bindings["release"]
	if b.Command == "" || b.NotApplicable != "" {
		return "", "", fmt.Errorf("project %s has no @release binding with a command", project)
	}
	return b.Command, workspace, nil
}

// releaseRun runs the project's @release command for the release step that is due. The command runs without the sequence lock and without a time limit; the step is marked started first so the Stop hook doesn't ask for a second start.
func releaseRun(bridge, session, arg string, out io.Writer) error {
	var project, version, location, ref string
	token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	err := withSeq(bridge, session, func(sf *seqFile) error {
		_, st, err := sf.stepRef(arg)
		if err != nil {
			return err
		}
		ref = st.ref()
		if st.Kind != "release" || st.Status != "active" || st.Started {
			return fmt.Errorf("step %s is not a release step that is due", ref)
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
				fmt.Fprintf(out, "step %s is %s; the release command ended with: %v\n", st.ref(), st.Status, err)
				return nil
			}
			var note string
			if err != nil {
				st.Status, st.Reason = "failed", err.Error()
				sf.failed(bridge, seq, st)
				note = fmt.Sprintf("step %s (release %s %s) failed: %v", st.ref(), project, version, err)
			} else {
				st.Status, st.Reason = "done", ""
				note = fmt.Sprintf("step %s (release %s %s) done.", st.ref(), project, version)
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
	fmt.Fprintf(out, "step %s (release %s %s) %s\n", ref, project, version, outcome)
	return fmt.Errorf("step %s is no longer in any session's sequence; its release outcome was not recorded", ref)
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
	var name string
	if slices.Contains([]string{"run", "publish", "do", "release"}, args[0]) {
		if name, rest, err = takeName(rest); err != nil {
			return err
		}
	}
	switch args[0] {
	case "run":
		o, err := parseRunArgs("ensign sequence run", rest)
		if err != nil {
			return err
		}
		if o.subtask != "" {
			return fmt.Errorf("a sequence step names a task, never a subtask; its run starts with the task's next pending subtask")
		}
		if err := refuseNoCode(bridge, o.task); err != nil {
			return err
		}
		if err := checkAddable(bridge, session, name); err != nil {
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
			return seqAdd(bridge, session, out, name, newRunStep(snap.Task, 0, []*Snapshot{snap}))
		}
		snaps, err := createGroupRuns(bridge, o.task, group, nil, o.staleOK, &o, out)
		if err != nil {
			return err
		}
		return seqAdd(bridge, session, out, name, newRunStep(o.task, 1, snaps))
	case "publish":
		if len(rest) != 1 {
			return fmt.Errorf("usage: ensign sequence publish <project>/<task>")
		}
		cmd, err := publishCommand(bridge, rest[0])
		if err != nil {
			return err
		}
		if err := checkAddable(bridge, session, name); err != nil {
			return err
		}
		return seqAdd(bridge, session, out, name, &seqStep{Kind: "publish", Task: rest[0], Command: cmd})
	case "do":
		text := strings.TrimSpace(strings.Join(rest, " "))
		if text == "" {
			return fmt.Errorf("usage: ensign sequence do <instruction>")
		}
		if err := checkAddable(bridge, session, name); err != nil {
			return err
		}
		return seqAdd(bridge, session, out, name, &seqStep{Kind: "do", Instruction: text})
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
		if err := checkAddable(bridge, session, name); err != nil {
			return err
		}
		return seqAdd(bridge, session, out, name, &seqStep{Kind: "release", Task: rest[0], Version: strings.TrimSpace(rest[1]), Location: location})
	case "release-run":
		if len(rest) != 1 {
			return errors.New("usage: ensign sequence release-run <seq>.<step>")
		}
		return releaseRun(bridge, session, rest[0], out)
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
		// A finishing hook holds a run's lock and then takes the sequence lock, so the runs of an active or paused step are retired between two holds of it, and "run ensign sequence halt again" is true for them; a pending step's unstarted runs are retired under the lock because no finishing hook holds a pending run's lock, and a decide step's runs are all over and may be held by a hook, so none is opened.
		var pausedRuns, unstartedRuns []string
		var pendingErrs []error
		var halted string
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
			halted = cur.label()
			sf.reconcile(bridge)
			for _, st := range cur.Steps {
				switch {
				case st.Status == "paused":
					for _, ru := range st.runs() {
						pausedRuns = append(pausedRuns, ru.Run)
					}
				case st.Kind == "run" && st.Status == "active":
					// A run still queued for a busy folder, or approved with no lead yet, never starts now; the second hold's reconcile cancels the step once they are retired.
					for _, ru := range st.runs() {
						unstartedRuns = append(unstartedRuns, ru.Run)
					}
				case st.Kind == "run" && (st.Status == "pending" || st.deciding()):
					wasPending := st.Status == "pending"
					st.Status, st.Reason = "cancelled", reason
					if wasPending {
						for _, ru := range st.runs() {
							if ru.Status == "" || ru.Status == "pending" {
								if err := retireForHalt(bridge, ru.Run, haltPrefix+reason, false); err != nil {
									pendingErrs = append(pendingErrs, fmt.Errorf("run %s could not be retired and is left pending under the cancelled step: %w", ru.Run, err))
								}
							}
						}
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
		var retireErrs []error
		for _, id := range pausedRuns {
			if err := retireForHalt(bridge, id, haltPrefix+reason, true); err != nil {
				retireErrs = append(retireErrs, fmt.Errorf("run %s could not be retired: %w", id, err))
			}
		}
		for _, id := range unstartedRuns {
			if err := retireForHalt(bridge, id, haltPrefix+reason, false); err != nil {
				retireErrs = append(retireErrs, fmt.Errorf("run %s could not be retired: %w", id, err))
			}
		}
		err = withSeq(bridge, session, func(sf *seqFile) error {
			text, err := sf.advance(bridge, session)
			if text != "" {
				fmt.Fprintln(out, text)
			} else {
				fmt.Fprintf(out, "%s halted: %s\n", halted, reason)
				if l := sf.listing(bridge); l != "" {
					fmt.Fprint(out, "\n"+l)
				}
			}
			return err
		})
		if len(retireErrs) > 0 {
			err = errors.Join(append(retireErrs, err)...)
			err = fmt.Errorf("%w\nrun ensign sequence halt again to retire what is left", err)
		}
		// The step is already cancelled, so a second halt does not reach these runs.
		return errors.Join(append([]error{err}, pendingErrs...)...)
	case "drop":
		const dropUsage = "usage: ensign sequence drop <step>... [--reason <text>]"
		var steps []string
		reason := "dropped by the user"
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--reason" {
				if i+2 != len(rest) {
					return errors.New(dropUsage)
				}
				reason = rest[i+1]
				break
			}
			steps = append(steps, rest[i])
		}
		if len(steps) == 0 {
			return errors.New(dropUsage)
		}
		var retireErr error
		err := withSeq(bridge, session, func(sf *seqFile) error {
			sf.reconcile(bridge)
			open := sf.openSeq()
			drop := map[*seqStep]bool{}
			for _, arg := range steps {
				seq, st, err := sf.stepRef(arg)
				switch {
				case err != nil:
					return err
				case seq.Closed || (!seq.Approved && seq != open):
					return fmt.Errorf("step %s is in a closed or halted sequence; only steps of the sequence waiting for go or of an approved, open sequence can be dropped", st.ref())
				case seq.Approved && st.Status != "pending":
					return fmt.Errorf("step %s is %s; only pending steps can be dropped", st.ref(), st.Status)
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
			removed, cancelled := 0, false
			for _, seq := range sf.Sequences {
				if seq == open {
					kept := seq.Steps[:0:0]
					for _, st := range seq.Steps {
						if drop[st] {
							if err := cancelStep(bridge, st, "dropped", reason); err != nil {
								retireErr = errors.Join(retireErr, err)
							}
							removed++
						} else {
							kept = append(kept, st)
						}
					}
					seq.Steps = kept
					continue
				}
				for _, st := range seq.Steps {
					if drop[st] {
						if err := cancelStep(bridge, st, "cancelled", reason); err != nil {
							retireErr = errors.Join(retireErr, err)
						}
						cancelled = true
					}
				}
			}
			if open != nil && len(open.Steps) == 0 {
				sf.Sequences = sf.Sequences[:len(sf.Sequences)-1]
			}
			sf.normalize()
			var text string
			var err error
			if cancelled {
				text, err = sf.advance(bridge, session)
			}
			if text == "" {
				text = sf.listing(bridge)
			}
			if text == "" && removed > 0 {
				text = "no steps are left waiting for go\n"
			}
			fmt.Fprint(out, text)
			return err
		})
		if err != nil {
			return err
		}
		return retireErr
	case "discard":
		reason, err := flagValue(rest, "--reason", "usage: ensign sequence discard [--reason <text>]")
		if err != nil {
			return err
		}
		if reason == "" {
			reason = "discarded"
		}
		var retireErr error
		err = withSeq(bridge, session, func(sf *seqFile) error {
			seq := sf.openSeq()
			if n := len(sf.Sequences); seq == nil && n > 0 && sf.Sequences[n-1].Halted && sf.Sequences[n-1].unfinished() {
				return fmt.Errorf("%s is already halted and closes when the step still active finishes", sf.Sequences[n-1].label())
			}
			if seq == nil {
				return fmt.Errorf("there is no sequence waiting for go to discard; an approved sequence is stopped with ensign sequence halt")
			}
			sf.reconcile(bridge)
			for _, st := range seq.Steps {
				if err := cancelStep(bridge, st, "discarded", reason); err != nil {
					retireErr = errors.Join(retireErr, err)
				}
			}
			sf.Sequences = sf.Sequences[:len(sf.Sequences)-1]
			fmt.Fprintf(out, "%s discarded (%d steps removed)\n", seq.label(), len(seq.Steps))
			return nil
		})
		if err != nil {
			return err
		}
		return retireErr
	case "accept":
		if len(rest) != 1 {
			return errors.New("usage: ensign sequence accept <seq>.<step>")
		}
		var refused error
		err := withSeq(bridge, session, func(sf *seqFile) error {
			seq, st, err := sf.stepRef(rest[0])
			if err != nil {
				return err
			}
			if sf.endStaleDecision(bridge, seq, st) {
				// The ending is saved, so the refusal is returned after withSeq.
				refused = fmt.Errorf("step %s is not waiting for a decision (it is %s)", st.ref(), st.Status)
				return nil
			}
			if !st.deciding() {
				return fmt.Errorf("step %s is not waiting for a decision", st.ref())
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
		if err != nil {
			return err
		}
		return refused
	case "skip":
		if len(rest) != 1 {
			return errors.New("usage: ensign sequence skip <seq>.<step>")
		}
		var refused error
		err := withSeq(bridge, session, func(sf *seqFile) error {
			seq, st, err := sf.stepRef(rest[0])
			if err != nil {
				return err
			}
			if sf.endStaleDecision(bridge, seq, st) {
				refused = fmt.Errorf("step %s is not waiting for a decision (it is %s)", st.ref(), st.Status)
				return nil
			}
			switch {
			case seq.Halted:
				return fmt.Errorf("the sequence is halted; step %s can't be skipped", st.ref())
			case st.Kind != "run":
				return fmt.Errorf("step %s is a %s step; only run steps can be skipped, not publish, do or release steps", st.ref(), st.Kind)
			case !st.deciding():
				return fmt.Errorf("step %s is not waiting for a decision (it is %s)", st.ref(), st.Status)
			}
			affected := skipAffectedText(skipAffected(sf, seq, st))
			workspace := skipWorkspaceText(bridge, st)
			st.Status, st.Reason, st.SkippedAt = "failed", skipPrefix+st.Reason, time.Now().UnixNano()
			for _, other := range seq.Steps {
				if other.Kind == "publish" && other.Task == st.Task && other.Status == "pending" {
					cancelStep(bridge, other, "cancelled", fmt.Sprintf("step %s was skipped", st.ref()))
				}
			}
			text, err := sf.advance(bridge, session)
			if text == "" {
				text = sf.listing(bridge)
			}
			if affected != "" {
				text = affected + "\n" + text
			}
			text = workspace + "\n" + text
			fmt.Fprint(out, text)
			return err
		})
		if err != nil {
			return err
		}
		return refused
	case "retry":
		usage := "usage: ensign sequence retry <seq>.<step> --diagnosis <why it failed> --state <what is left>"
		if len(rest) < 1 {
			return errors.New(usage)
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
		var refused error
		err := withSeq(bridge, session, func(sf *seqFile) error {
			seq, st, err := sf.stepRef(rest[0])
			if err != nil {
				return err
			}
			if sf.endStaleDecision(bridge, seq, st) {
				refused = fmt.Errorf("step %s is not waiting for a decision (it is %s)", st.ref(), st.Status)
				return nil
			}
			switch {
			case seq.Halted:
				return fmt.Errorf("the sequence is halted; step %s can't be retried", st.ref())
			case st.Kind != "run":
				return fmt.Errorf("step %s is a %s step; only run steps can be retried, not publish, do or release steps", st.ref(), st.Kind)
			case !st.deciding():
				return fmt.Errorf("step %s is not waiting for a decision (it is %s)", st.ref(), st.Status)
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
						if rerr := retireIf(bridge, f.Run, "withdrawn: "+err.Error(), "pending"); rerr != nil {
							err = errors.Join(err, fmt.Errorf("run %s could not be withdrawn: %w", f.Run, rerr))
						}
					}
					return err
				}
				failed, fresh = append(failed, ru), append(fresh, snap)
			}
			if len(failed) == 0 {
				return fmt.Errorf("step %s has no failed run to retry", st.ref())
			}
			for i, ru := range failed {
				*ru = seqRun{Run: fresh[i].Run, Subtask: fresh[i].Subtask, Location: fresh[i].Location, Prev: ru.Run, Status: "pending"}
			}
			st.Prev, st.Retries, st.Diagnosis = failed[0].Prev, st.attempt(), diagnosis
			st.setRuns(st.Runs)
			st.Status, st.Reason, st.Blocks, st.WaitBlocks = "pending", "", 0, 0
			text, err := sf.advance(bridge, session)
			if text == "" {
				text = sf.listing(bridge)
			}
			fmt.Fprint(out, text)
			return err
		})
		if err != nil {
			return err
		}
		return refused
	case "done", "fail":
		if len(rest) < 1 {
			return fmt.Errorf("usage: ensign sequence done <seq>.<step> [--note <text>] | fail <seq>.<step> --reason <text>")
		}
		flag := "--note"
		if args[0] == "fail" {
			flag = "--reason"
		}
		why, err := flagValue(rest[1:], flag, "usage: ensign sequence done <seq>.<step> [--note <text>] | fail <seq>.<step> --reason <text>")
		if err != nil {
			return err
		}
		if args[0] == "fail" && why == "" {
			return fmt.Errorf("ensign sequence fail needs --reason <text>")
		}
		return withSeq(bridge, session, func(sf *seqFile) error {
			seq, st, err := sf.stepRef(rest[0])
			if err != nil {
				return err
			}
			if st.Status != "active" || st.Kind != "do" && !(st.Kind == "release" && args[0] == "fail") {
				return fmt.Errorf("step %s is not an active do step (or, for fail, an active release step)", st.ref())
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
	return fmt.Errorf("usage: ensign sequence [run <project>/<task> | publish <project>/<task> | release <project> <version> | release-run <seq>.<step> | do <instruction> | mode stop|keep-going | halt | discard | drop <step>... [--reason <text>] | retry <step> --diagnosis <why> --state <what is left> | accept <step> | skip <step> | done <step> | fail <step> --reason <text>]; add steps with --name <text> to name a new sequence]")
}

const currentStateHeading = "## Current state"

// retryRun queues a new run for a failed one, with the diagnosis in its work item.
func retryRun(bridge string, old *RunFiles, diagnosis, left string, out io.Writer) (*Snapshot, error) {
	o := runOpts{start: old.Snap.Start, staleOK: old.State.StaleOK, retryOf: old.State.Run, retryBase: old.State.Base, location: old.Snap.Location, subtask: old.Snap.Subtask, rebuild: old.State.Rebuild, rules: old.Snap.Rules,
		currentState: "\n\n" + currentStateHeading + "\n\nThis is a retry. The workspace holds the previous attempt's work, uncommitted changes included; build on it.\n\nWhy the previous attempt failed: " + diagnosis + "\n\nWhat is left: " + left + "\n"}
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

// takeName removes a "--name <text>" pair from the arguments of a command that adds a step, and checks the name.
func takeName(args []string) (string, []string, error) {
	i := slices.Index(args, "--name")
	if i < 0 {
		return "", args, nil
	}
	if i+1 >= len(args) {
		return "", nil, errors.New("--name needs the sequence's name")
	}
	name := strings.TrimSpace(args[i+1])
	switch {
	case name == "":
		return "", nil, errors.New("--name needs a name that is not empty")
	case strings.ContainsAny(name, "\"\r\n"):
		return "", nil, errors.New("a sequence name can't contain a double quote or a line break")
	case utf8.RuneCountInString(name) > maxSeqName:
		return "", nil, fmt.Errorf("a sequence name is at most %d characters; this one has %d", maxSeqName, utf8.RuneCountInString(name))
	}
	return name, slices.Delete(slices.Clone(args), i, i+2), nil
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

// errNameJoins refuses a name for a step that would join the sequence waiting for go.
var errNameJoins = errors.New("--name can't be used here: the name is set by the sequence's first step, and this step joins the sequence already waiting for go")

// checkAddable refuses to add steps while the session has a plain run under way.
func checkAddable(bridge, session, name string) error {
	if err := writesPendingRefusal(bridge, session); err != nil {
		return err
	}
	if name != "" && readSeq(bridge, session).openSeq() != nil {
		return errNameJoins
	}
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
func seqAdd(bridge, session string, out io.Writer, name string, st *seqStep) error {
	return withSeq(bridge, session, func(sf *seqFile) error {
		if name != "" && sf.openSeq() != nil {
			return errNameJoins
		}
		live, err := liveSessionRuns(bridge, session)
		if err != nil {
			return err
		}
		for _, id := range live {
			// A sequence run's hook holds its run lock while it waits for this sequence lock, so only a run that is not in a sequence is opened.
			peek, err := readRun(bridge, id)
			if err != nil {
				return err
			}
			if peek.State.Sequence || peek.State.Approved {
				continue
			}
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
				first := &seqStep{Kind: "run", Task: r.Snap.Task, Run: r.State.Run, Headline: headline(r.Snap.WorkItem)}
				sf.add(first, name)
				fmt.Fprintf(out, "run %s was waiting for go; it is now step %s of the sequence\n", r.State.Run, first.ref())
			}
			r.close()
		}
		sf.add(st, name)
		fmt.Fprint(out, sf.listing(bridge))
		return nil
	})
}
