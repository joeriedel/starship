// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// A project whose @test binding is plain needs no judgement to run its tests: the command
// either passes or it doesn't. Ensign then runs the test stage itself, as `ensign run-stage` (which `next` names, and the lead runs as a background shell command), so no agent reads the log. The tester is never spawned: the hook refuses a spawn of it.

// plainStage reports whether ensign runs this stage itself.
func plainStage(snap *Snapshot, s *Stage) bool {
	if s.isLead() || s.Role != "tester" || s.Parallel != nil {
		return false
	}
	for _, sd := range snap.sides() {
		if !sd.Bindings["test"].Plain {
			return false
		}
	}
	x := snap.crossTest()
	return x.NotApplicable != "" || (x.Command == "" && x.Guidance == "") || x.Plain
}

// sides is the locations the test stage covers. A run record from before sides, or a locationless run, has one: the run's own workspace and bindings.
func (snap *Snapshot) sides() []Side {
	if len(snap.Sides) > 0 {
		return snap.Sides
	}
	return []Side{{Location: snap.Location, Workspace: snap.Workspace, Bindings: snap.Bindings}}
}

// crossTest is the cross-location @test in any form, when several sides make it apply; the zero Binding otherwise.
func (snap *Snapshot) crossTest() Binding {
	if len(snap.Sides) < 2 {
		return Binding{}
	}
	return snap.Cross["test"]
}

// Valid says the binding has a command to run.
func (b Binding) Valid() bool { return b.Command != "" }

// hookTimeout is the PreToolUse hook's timeout in this bridge's settings.
const hookTimeout = 3600

// defaultBindingTimeout limits a plain binding's command that sets no timeout of its own.
const defaultBindingTimeout = 60 * time.Minute

// dispatchLock is the lock a running plain dispatch's owner holds, in the run folder. It is never deleted: it goes with the run.
func dispatchLock(r *RunFiles, id string) *flock.Flock {
	return flock.New(filepath.Join(r.Dir, "dispatch-"+id+".lock"))
}

// deadPlainOwner returns a running plain dispatch in scope whose `ensign run-stage` is gone, or nil. The caller holds the run lock, and run-stage holds the dispatch lock until after its completed state is saved, so a free lock means the owner died.
func deadPlainOwner(r *RunFiles, m *machine, scope string) *Dispatch {
	for _, d := range r.State.Dispatches {
		if d.Scope != scope || d.Done || !d.Locked {
			continue
		}
		if s := m.stage(d.Stage); s == nil || !plainStage(r.Snap, s) {
			continue
		}
		l := dispatchLock(r, d.ID)
		if ok, err := l.TryLock(); err == nil && ok {
			l.Close()
			return d
		}
	}
	return nil
}

// completeDeadOwner records a plain dispatch whose owner died as an error. Nothing is run again.
func completeDeadOwner(r *RunFiles, m *machine, d *Dispatch) error {
	s := m.stage(d.Stage)
	summary := fmt.Sprintf("stage %q's command died before it finished (killed, or the session or machine stopped); its effect on the run is unknown", s.ID)
	wasTerminal := r.State.terminal()
	completeDispatch(r, m, d, s, "error", summary, nil, "Run by ensign: "+summary+".\n")
	return finishIfTerminal(r, wasTerminal, os.Stderr)
}

// runBinding runs a plain binding's command: an ensign built-in, or a command line. A
// built-in reports failed tests as errTestsFailed; any other error means no verdict.
func runBinding(r *RunFiles, d *Dispatch, dir, command string, deadline time.Time) (string, error) {
	if args, ok := strings.CutPrefix(strings.TrimSpace(command), "ensign "); ok {
		f := strings.Fields(args)
		if len(f) > 0 && f[0] == "github-pr" {
			return githubCommand(r, f[1:], deadline)
		}
		return "", internalHandler(r, f, d)
	}
	return shellFor(dir, command, handlerEnv(r, d), time.Until(deadline))
}

// couldNotRun is an exit code that means the command could not do its job at all, as
// opposed to finding failures: 126 and 127 from the shell, and 125 for a script to say so
// itself (as git bisect uses it).
func couldNotRun(code int) bool { return code >= 125 && code <= 127 }

// cmdRunStage implements `ensign run-stage [--bridge <dir>] [--run <id>]`: it runs the plain stage that is due now, and prints its outcome. A failing stage is an outcome, not an error.
func cmdRunStage(args []string, out io.Writer) error {
	r, err := openRunByFlags(args, "ensign run-stage [--bridge <dir>] [--run <id>]")
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("there is no active run")
	}
	defer r.close()
	m := &machine{snap: r.Snap, st: r.State}
	scope := leadScope(r)
	if r.State.terminal() {
		return fmt.Errorf("nothing to run: %s", m.expect(scope))
	}
	sc := r.State.Scopes[scope]
	if sc == nil || sc.Stage == "" {
		return fmt.Errorf("nothing to run: %s", m.expect(scope))
	}
	s := m.stage(sc.Stage)
	if !plainStage(r.Snap, s) {
		return fmt.Errorf("stage %q is not a plain stage, so there is nothing for run-stage to run. %s", s.ID, m.expect(scope))
	}
	if m.running(scope, s.ID) > 0 {
		if d := deadPlainOwner(r, m, scope); d != nil {
			if err := completeDeadOwner(r, m, d); err != nil {
				return err
			}
			if err := r.save(); err != nil {
				return err
			}
			fmt.Fprintf(out, "Stage %q was left running by an `ensign run-stage` that died; ensign recorded it as an error and ran nothing. Outcome: error.\n\nNext: %s\n", s.ID, m.expect(scope))
			return nil
		}
		return fmt.Errorf("stage %q is already running. %s", s.ID, m.expect(scope))
	}
	d := newDispatch(r, scope, s, s.Role)
	if len(sc.Branches) == 0 {
		if err := fire(r, "stage.before:"+s.ID, d); err != nil {
			m.fail("handler", err.Error())
			err1 := finishIfTerminal(r, false, os.Stderr)
			err2 := r.save()
			return errors.Join(fmt.Errorf("stage %s could not start: %s", s.ID, r.State.Reason), err1, err2)
		}
	}
	// Held until the completed state is saved, so a free lock while the dispatch is running means the owner died.
	dl := dispatchLock(r, d.ID)
	if ok, err := dl.TryLock(); err != nil || !ok {
		return fmt.Errorf("could not lock the dispatch of stage %q: %v", s.ID, err)
	}
	defer dl.Close()
	d.Locked = true
	r.State.Dispatches = append(r.State.Dispatches, d)
	r.State.log("dispatch", s.ID, d.ID, s.Role+" "+d.Dim)
	msg, err := runPlainStage(r, m, d, s, scope)
	if err != nil {
		return err
	}
	if err := r.save(); err != nil {
		return err
	}
	fmt.Fprintln(out, msg)
	return nil
}

// runPlainStage runs @build (when bound) and @test in the workspace, completes the
// dispatch with the result, and returns what the dispatching agent is told. The commands
// run without the run lock, so other hooks are not held up for minutes; r comes back
// locked and holding the state as it is then on disk.
func runPlainStage(r *RunFiles, m *machine, d *Dispatch, s *Stage, scope string) (string, error) {
	if err := r.save(); err != nil {
		return "", err
	}
	r.lock.Unlock()
	nHistory, nNotes := len(r.State.History), len(r.State.Notes)
	outcome := "pass"
	var steps, log []string
	failed, last := "", ""
	type planned struct {
		label, name, dir string
		b                Binding
		side             *Side // the side whose wiring the command needs; the edited side for the cross-location test
		deps             []Dependency
	}
	var plan []planned
	multi := len(r.Snap.sides()) > 1
	for i, sd := range r.Snap.sides() {
		sp := planSide(sd, r.Snap.Dependencies)
		if sp.na != "" {
			steps = append(steps, fmt.Sprintf("side %s: @%s is not applicable (%s); tested as it is", sd.Location, applyDependencyBinding, sp.na))
		}
		for _, name := range []string{"build", "test"} {
			if b, ok := sd.Bindings[name]; ok && b.NotApplicable == "" && b.Command != "" {
				label := ""
				if multi {
					label = "side " + sd.Location + ": "
				}
				plan = append(plan, planned{label, name, sd.Workspace, b, &r.Snap.sides()[i], sp.apply})
			}
		}
	}
	if b := r.Snap.crossTest(); b.Valid() {
		edited := &r.Snap.sides()[0]
		plan = append(plan, planned{"cross-location: ", "test", r.Snap.Workspace, b, edited, planSide(*edited, r.Snap.Dependencies).apply})
	}
	var applied *Side
	restore := func() {
		if applied == nil {
			return
		}
		notice, err := restoreDependencies(applied.Workspace, settleCommand(r, applied.Location))
		if notice != "" {
			steps = append(steps, fmt.Sprintf("side %s: %s", applied.Location, notice))
		}
		if err != nil {
			outcome = "error"
			steps = append(steps, fmt.Sprintf("side %s: could not restore the dependency wiring: %v", applied.Location, err))
		}
		applied = nil
	}
	for _, p := range plan {
		name, b := p.name, p.b
		if p.side != applied {
			restore()
			if p.side != nil && len(p.deps) > 0 {
				aout, err := applyDependencies(r, d, *p.side, p.deps)
				log = append(log, aout)
				applied = p.side
				if err != nil {
					outcome = "error"
					steps = append(steps, fmt.Sprintf("side %s: could not apply the dependency: %v", p.side.Location, err))
					failed = aout
					break
				}
			}
		}
		limit := b.Timeout
		if limit == 0 {
			limit = defaultBindingTimeout
		}
		start := time.Now()
		out, err := runBinding(r, d, p.dir, b.Command, start.Add(limit))
		took := time.Since(start).Round(time.Second)
		log = append(log, fmt.Sprintf("$ %s\n%s", b.Command, out))
		var exit *exec.ExitError
		switch {
		case err == nil:
			result := "exit 0"
			if strings.HasPrefix(strings.TrimSpace(b.Command), "ensign ") {
				result = "passed"
			}
			steps = append(steps, fmt.Sprintf("%s@%s `%s`: %s (%s)", p.label, name, b.Command, result, took))
			last = out
			continue
		case errors.Is(err, errTestsFailed):
			outcome = "fail"
			steps = append(steps, fmt.Sprintf("%s@%s `%s`: failed (%s)", p.label, name, b.Command, took))
		case errors.As(err, &exit) && !couldNotRun(exit.ExitCode()):
			outcome = "fail"
			steps = append(steps, fmt.Sprintf("%s@%s `%s`: exit %d (%s)", p.label, name, b.Command, exit.ExitCode(), took))
		default:
			outcome = "error"
			steps = append(steps, fmt.Sprintf("%s@%s `%s`: could not run: %v (%s)", p.label, name, b.Command, err, took))
		}
		failed = out
		break
	}
	restore()
	logFile := filepath.Join(r.Dir, "reports", d.ID+".log")
	os.WriteFile(logFile, []byte(strings.Join(log, "\n")), 0o644)

	summary := strings.Join(steps, "; ")
	report := "Run by ensign: this project's @test binding is plain.\n\n- " + strings.Join(steps, "\n- ") + "\n"
	switch {
	case failed != "":
		report += "\nThe end of the failing command's output:\n\n```\n" + tailLines(failed, 40) + "\n```\n"
	case strings.TrimSpace(last) != "":
		report += "\nThe end of its output:\n\n```\n" + tailLines(last, 10) + "\n```\n"
	}
	report += "\nFull output: " + logFile + "\n"

	newHistory, newNotes := r.State.History[nHistory:], r.State.Notes[nNotes:]
	if err := r.lock.Lock(); err != nil {
		return "", fmt.Errorf("locking run %s: %v", filepath.Base(r.Dir), err)
	}
	snap, st := &Snapshot{}, &State{}
	err1 := readJSON(filepath.Join(r.Dir, "pipeline.json"), snap)
	if err := errors.Join(err1, readJSON(filepath.Join(r.Dir, "state.json"), st)); err != nil {
		r.lock.Unlock() // r still holds the stale state, which must not be saved
		return "", err
	}
	r.Snap, r.State = snap, st
	st.History = append(st.History, newHistory...)
	st.Notes = append(st.Notes, newNotes...)
	m = &machine{snap: snap, st: st}
	s = m.stage(s.ID)

	switch cur := st.dispatch(d.ID); {
	case cur == nil || cur.Done:
		os.WriteFile(filepath.Join(r.Dir, "reports", d.ID+".md"), []byte(report), 0o644)
		st.log("report", s.ID, d.ID, outcome+": "+summary+" (dispatch no longer running; not applied)")
	default:
		wasTerminal := st.terminal()
		completeDispatch(r, m, cur, s, outcome, summary, nil, report)
		// Now, so what finishing adds (a pull request, say) is in the reply.
		if err := finishIfTerminal(r, wasTerminal, os.Stderr); err != nil {
			return "", err
		}
	}

	return fmt.Sprintf("ensign ran stage %q itself, because this project's @test binding is plain; no %s agent was started. "+
		"Outcome: %s.\n\n%s\nNext: %s", s.ID, s.Role, outcome, report, m.expect(scope)), nil
}
