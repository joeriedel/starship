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
	"slices"
	"strconv"
	"strings"

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
	N           int    `json:"n"`
	Kind        string `json:"kind"` // run, publish, do
	Task        string `json:"task,omitempty"`
	Run         string `json:"run,omitempty"`
	Command     string `json:"command,omitempty"` // publish: the extension's operation
	Instruction string `json:"instruction,omitempty"`
	Headline    string `json:"headline,omitempty"`
	Status      string `json:"status"` // pending, active, paused, done, failed, skipped, cancelled
	Reason      string `json:"reason,omitempty"`
	Blocks      int    `json:"blocks,omitempty"` // stops held open for this step
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
	return readSeq(bridge, session).listing()
}

func (s *seqStep) open() bool {
	return s.Status == "pending" || s.Status == "active" || s.Status == "paused"
}

func (s *seqStep) holds() bool { return s.Status == "active" || s.Status == "paused" }

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
		if st.holds() {
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

func (sf *seqFile) listing() string {
	var b strings.Builder
	for _, seq := range sf.Sequences {
		if len(seq.Steps) == 0 {
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
			}
			line := fmt.Sprintf("  %d  %-7s %s  [%s]", st.N, st.Kind, strings.TrimSpace(what), st.Status)
			if st.Reason != "" {
				line += " " + firstLine(st.Reason)
			}
			b.WriteString(line + "\n")
		}
		if !seq.Approved && !seq.Halted {
			b.WriteString("type go to approve the sequence\n")
		}
	}
	return b.String()
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

// reconcile brings the steps of runs under way in line with their runs' saved states.
func (sf *seqFile) reconcile(bridge string) {
	for _, seq := range sf.Sequences {
		for _, st := range seq.Steps {
			if st.Kind != "run" || !st.holds() {
				continue
			}
			r, err := readRun(bridge, st.Run)
			if err != nil {
				st.Status, st.Reason = "failed", "its run can't be read: "+err.Error()
				sf.failed(bridge, seq, st)
				continue
			}
			switch r.State.Status {
			case "success":
				st.Status, st.Reason = "done", ""
			case "error":
				st.Status, st.Reason = "failed", r.State.Reason
				sf.failed(bridge, seq, st)
			case "ask-user":
				st.Status, st.Reason = "paused", r.State.Reason
			default:
				st.Status, st.Reason = "active", ""
			}
		}
	}
}

// failed applies the failure rules for a step that has just failed.
func (sf *seqFile) failed(bridge string, seq *sequence, failed *seqStep) {
	why := fmt.Sprintf("step %d failed", failed.N)
	if seq.Mode == modeKeepGoing {
		for _, st := range seq.Steps {
			if st.Status == "pending" && failed.Task != "" && st.Task == failed.Task {
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

// cancelStep ends a step that never started. Its run is made terminal, as a retired run is, so it is nobody's waiting run; no run.finish handler fires for it.
func cancelStep(bridge string, st *seqStep, status, reason string) {
	st.Status, st.Reason = status, reason
	if st.Kind != "run" {
		return
	}
	retireIf(bridge, st.Run, status+": "+reason, "pending")
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
		return strings.Join(parts, "\n") + "\n\n" + sf.listing()
	}
	for _, seq := range sf.Sequences {
		if seq.Closed {
			continue
		}
		if !seq.Approved && !seq.Halted {
			break
		}
		for {
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
				text, err := startRunStep(bridge, session, st)
				if err != nil {
					retireIf(bridge, st.Run, "failed: "+err.Error(), "pending")
					st.Status, st.Reason = "failed", err.Error()
					sf.failed(bridge, seq, st)
					continue
				}
				st.Status = "active"
				parts = append(parts, fmt.Sprintf("step %d: %s", st.N, text))
				return finish(), nil
			case "publish":
				out, err := runPublish(bridge, st)
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
			}
		}
	}
	return finish(), nil
}

func doText(st *seqStep) string {
	return fmt.Sprintf("%s\nWhen it is done run `ensign sequence done %d`, or `ensign sequence fail %d --reason …` if it can't be.",
		st.Instruction, st.N, st.N)
}

// startRunStep approves a queued run and binds it to the session. It returns what the session does next, the same text go gives a plain run.
func startRunStep(bridge, session string, st *seqStep) (string, error) {
	if data, err := os.ReadFile(sessionFile(bridge, session) + ".pin"); err == nil {
		if started := strings.TrimSpace(string(data)); started != pinnedCommit(bridge) {
			return "", fmt.Errorf("the bridge was updated to a new starship release after this session started; restart claude before starting a run")
		}
	}
	r, err := openRun(bridge, st.Run)
	if err != nil {
		return "", fmt.Errorf("its run can't be opened: %v", err)
	}
	defer r.close()
	if r.State.Status != "pending" {
		return "", fmt.Errorf("its run %s is %s, not waiting", st.Run, r.State.Status)
	}
	r.State.Session, r.State.Approved, r.State.Status = session, true, "running"
	r.State.log("approved", "", "", "sequence")
	if err := r.save(); err != nil {
		return "", err
	}
	if err := bindSession(bridge, session, st.Run); err != nil {
		return "", err
	}
	if st.Task != "" {
		os.WriteFile(sessionFile(bridge, session)+".task", []byte(st.Task+"\n"), 0o644)
	}
	return approvalText(r), nil
}

// publishCommand finds the one publish operation a task's extensions declare.
func publishCommand(bridge, task string) (string, error) {
	c, err := loadConfig(bridge, task)
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
func runPublish(bridge string, st *seqStep) (string, error) {
	if args, ok := strings.CutPrefix(st.Command, "ensign "); ok {
		if strings.TrimSpace(args) == "github-pr merge" {
			base, sha, err := githubMerge(bridge, st.Task)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("pushed to %s: %s", base, short(sha)), nil
		}
		return "", fmt.Errorf("unknown built-in publish operation: %s", st.Command)
	}
	c, err := loadConfig(bridge, st.Task)
	if err != nil {
		return "", err
	}
	out, err := shell(c.Workspace, st.Command+" "+shellQuote(st.Task), os.Environ())
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
			adv = "It starts when the step in progress finishes.\n\n" + sf.listing()
		}
		text += adv
		return err
	})
	return
}

// sequenceStop holds a turn open while a step waits for the session to act on it. Each step gets seqStopBudget holds, as the sequence file's lock allows, then the turn may end.
func sequenceStop(bridge string, in *HookInput) any {
	if !seqExists(bridge, in.SessionID) {
		return nil
	}
	var out any
	withSeq(bridge, in.SessionID, func(sf *seqFile) error {
		sf.advance(bridge, in.SessionID)
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				var todo string
				switch {
				case st.Status != "active":
				case st.Kind == "do":
					todo = doText(st)
				case st.Kind == "run":
					if r, err := readRun(bridge, st.Run); err == nil && r.State.Approved && r.State.Status == "running" &&
						!r.State.Started && len(r.State.Dispatches) == 0 {
						todo = approvalText(r)
					}
				}
				if todo == "" || st.Blocks >= seqStopBudget {
					continue
				}
				st.Blocks++
				out = block(fmt.Sprintf("sequence step %d is waiting for you.\n\n%s\n\n%s", st.N, todo, sf.listing()))
				return nil
			}
		}
		return nil
	})
	return out
}

// carrySequence moves a session's sequence to the session that continues it, and points the runs that are not bound to the old session at the new one.
func carrySequence(bridge, old, session string) {
	if !seqExists(bridge, old) || seqExists(bridge, session) {
		return
	}
	if os.Rename(seqPath(bridge, old), seqPath(bridge, session)) != nil {
		return
	}
	bound, _ := os.ReadFile(sessionFile(bridge, old))
	withSeq(bridge, session, func(sf *seqFile) error {
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				if st.Kind != "run" || !st.open() || st.Run == strings.TrimSpace(string(bound)) {
					continue
				}
				if r, err := openRun(bridge, st.Run); err == nil {
					if r.State.Session == old {
						r.State.Session = session
						r.save()
					}
					r.close()
				}
			}
		}
		return nil
	})
}

// cmdSequence implements `ensign sequence [run|publish|do|mode|halt|done|fail …]`.
func cmdSequence(args []string, out io.Writer) error {
	session := os.Getenv("STARSHIP_SESSION")
	if session == "" {
		return fmt.Errorf("a sequence belongs to a session, and STARSHIP_SESSION is not set")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return withSeq(bridge, session, func(sf *seqFile) error {
			sf.reconcile(bridge)
			if l := sf.listing(); l != "" {
				fmt.Fprint(out, l)
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
		if err := checkAddable(bridge, session); err != nil {
			return err
		}
		snap, err := createRun(bridge, o, true, out)
		if err != nil || snap == nil {
			return err
		}
		return seqAdd(bridge, session, out, &seqStep{Kind: "run", Task: snap.Task, Run: snap.Run, Headline: headline(snap.WorkItem)})
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
			fmt.Fprint(out, sf.listing())
			return nil
		})
	case "halt":
		reason, err := flagValue(rest, "--reason", "usage: ensign sequence halt [--reason <text>]")
		if err != nil {
			return err
		}
		// A paused step's run may be held by a finishing hook, whose order is run then sequence; so its run is retired here between two holds of the sequence lock.
		var paused []*seqStep
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
			sf.reconcile(bridge)
			for _, st := range cur.Steps {
				switch {
				case st.Status == "paused":
					paused = append(paused, &seqStep{N: st.N, Run: st.Run})
				case st.Status == "pending" || (st.Status == "active" && st.Kind == "do"):
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
			retired[p.N] = retireIf(bridge, p.Run, "cancelled: "+reason, "pending", "ask-user")
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
				fmt.Fprint(out, sf.listing())
			}
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
			if st == nil || st.Kind != "do" || st.Status != "active" {
				return fmt.Errorf("step %d is not an active do step", n)
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
				text = sf.listing()
			}
			fmt.Fprint(out, text)
			return err
		})
	}
	return fmt.Errorf("usage: ensign sequence [run <project>/<task> | publish <project>/<task> | do <instruction> | mode stop|keep-going | halt | done <n> | fail <n> --reason <text>]")
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
	r, err := openSessionRun(bridge, session)
	if err != nil || r == nil {
		return err
	}
	defer r.close()
	if !r.State.terminal() && !r.State.Sequence && r.State.Approved {
		return fmt.Errorf("this session already has an active run, %s; add steps once it has finished", r.State.Run)
	}
	return nil
}

// seqAdd appends a step to the session's open sequence and prints the sequence. A plain run waiting for go becomes the sequence's first step.
func seqAdd(bridge, session string, out io.Writer, st *seqStep) error {
	return withSeq(bridge, session, func(sf *seqFile) error {
		if r, err := openSessionRun(bridge, session); err != nil {
			return err
		} else if r != nil {
			if !r.State.terminal() && !r.State.Sequence && !r.State.Approved {
				r.State.Sequence = true
				if err := r.save(); err != nil {
					r.close()
					return err
				}
				os.Remove(sessionFile(bridge, session))
				sf.add(&seqStep{Kind: "run", Task: r.Snap.Task, Run: r.State.Run, Headline: headline(r.Snap.WorkItem)})
				fmt.Fprintf(out, "run %s was waiting for go; it is now step %d of the sequence\n", r.State.Run, sf.NextStep)
			}
			r.close()
		}
		sf.add(st)
		fmt.Fprint(out, sf.listing())
		return nil
	})
}
