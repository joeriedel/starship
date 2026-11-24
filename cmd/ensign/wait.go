// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"time"
)

// waitPoll is how often `ensign wait` reads the run's state, and waitUnreadable how long a run's state may stay unreadable before it gives up.
var (
	waitPoll       = 500 * time.Millisecond
	waitUnreadable = time.Minute
)

// agentsRunning reports whether an agent the lead of scope started is still running: a dispatch not done that is an agent, not a stage ensign runs itself, or a helper that has not finished.
func agentsRunning(snap *Snapshot, st *State, scope string) bool {
	return dispatchesRunning(snap, st, scope) || len(liveHelpers(snap, st)) > 0
}

// dispatchesRunning reports whether a dispatch of scope that is not done is an agent, not a stage ensign runs itself. A dispatch whose agent has not bound yet counts.
func dispatchesRunning(snap *Snapshot, st *State, scope string) bool {
	m := &machine{snap: snap, st: st}
	for _, d := range st.Dispatches {
		if d.Scope != scope || d.Done || d.Prebuild {
			continue
		}
		if s := m.stage(d.Stage); s != nil && plainStage(snap, s) {
			continue
		}
		return true
	}
	return false
}

// liveHelpers lists, sorted, the helpers that have not finished and were started by the run's current lead: one an earlier lead started no longer counts.
func liveHelpers(snap *Snapshot, st *State) []string {
	lead := snap.Pipeline.Stages[0]
	if !lead.isLead() {
		return nil
	}
	owner := ""
	for _, d := range st.Dispatches {
		if d.Stage == lead.ID {
			owner = d.AgentID
		}
	}
	var ids []string
	for id := range st.Helpers {
		if owner != "" && st.Tasks[id] == owner {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// cmdWait implements `ensign wait [--bridge <dir>] [--run <id>]`: it returns once no agent the run's lead started is running, or the run is over. The lead runs it as a background shell command, so that ending its turn leaves a command of its own running, which wakes it when it exits. It decides from the recorded state alone, read without the run's lock, which is held only to find the run.
func cmdWait(args []string, out io.Writer) error {
	r, err := openRunByFlags(args, "ensign wait [--bridge <dir>] [--run <id>]")
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("there is no such run")
	}
	dir, snap, st, scope := r.Dir, r.Snap, r.State, leadScope(r)
	r.close()
	waited := false
	var unreadableSince time.Time
	for {
		why := ""
		switch {
		case st.terminal():
			why = "The run is finished."
		case !agentsRunning(snap, st, scope):
			why = "No agent you started is still running."
			if waited {
				why = "No agent you started is running any more."
			}
		}
		if why != "" {
			fmt.Fprintln(out, why+" Run `ensign next`.")
			return nil
		}
		waited = true
		time.Sleep(waitPoll)
		var next State
		err := readRunState(dir, &next)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("run %s is gone", st.Run)
		}
		if err != nil {
			if unreadableSince.IsZero() {
				unreadableSince = time.Now()
			} else if time.Since(unreadableSince) > waitUnreadable {
				return fmt.Errorf("run %s could not be read for %s: %v", st.Run, waitUnreadable, err)
			}
			continue
		}
		unreadableSince = time.Time{}
		st = &next
	}
}
