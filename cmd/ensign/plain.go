// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A project whose @test binding is plain needs no judgement to run its tests: the command
// either passes or it doesn't. Ensign then runs the test stage itself when the tester is
// dispatched, and the dispatch is refused with the result, so no agent reads the log.

// plainStage reports whether ensign runs this stage itself.
func plainStage(r *RunFiles, s *Stage) bool {
	return s.Role == "tester" && r.Snap.Bindings["test"].Plain
}

// hookTimeout is the PreToolUse hook's timeout in this bridge's settings. A plain stage
// runs inside that hook, and a hook that times out lets the tool call through unchecked.
const hookTimeout = 3600

func preToolUseTimeout(bridge string) time.Duration {
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	data, err := os.ReadFile(filepath.Join(bridge, ".claude", "settings.json"))
	if err == nil && json.Unmarshal(data, &settings) == nil {
		for _, m := range settings.Hooks["PreToolUse"] {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, "hook PreToolUse") && h.Timeout > 0 {
					return time.Duration(h.Timeout) * time.Second
				}
			}
		}
	}
	return 600 * time.Second // Claude Code's default for a command hook
}

// runBinding runs a plain binding's command: an ensign built-in, or a command line. A
// built-in reports failed tests as errTestsFailed; any other error means no verdict.
func runBinding(r *RunFiles, d *Dispatch, command string, deadline time.Time) (string, error) {
	if args, ok := strings.CutPrefix(strings.TrimSpace(command), "ensign "); ok {
		f := strings.Fields(args)
		if len(f) > 0 && f[0] == "github-pr" {
			return githubCommand(r, f[1:], deadline)
		}
		return "", internalHandler(r, f, d)
	}
	return shellFor(r.Snap.Workspace, command, handlerEnv(r, d), time.Until(deadline))
}

// couldNotRun is an exit code that means the command could not do its job at all, as
// opposed to finding failures: 126 and 127 from the shell, and 125 for a script to say so
// itself (as git bisect uses it).
func couldNotRun(code int) bool { return code >= 125 && code <= 127 }

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
	limit := preToolUseTimeout(r.Snap.Bridge) - time.Minute
	if limit < 30*time.Second {
		limit = 30 * time.Second
	}
	deadline := time.Now().Add(limit)
	outcome := "pass"
	var steps, log []string
	failed, last := "", ""
	for _, name := range []string{"build", "test"} {
		b, ok := r.Snap.Bindings[name]
		if !ok || b.NotApplicable != "" || b.Command == "" {
			continue
		}
		start := time.Now()
		out, err := runBinding(r, d, b.Command, deadline)
		took := time.Since(start).Round(time.Second)
		log = append(log, fmt.Sprintf("$ %s\n%s", b.Command, out))
		var exit *exec.ExitError
		switch {
		case err == nil:
			result := "exit 0"
			if strings.HasPrefix(strings.TrimSpace(b.Command), "ensign ") {
				result = "passed"
			}
			steps = append(steps, fmt.Sprintf("@%s `%s`: %s (%s)", name, b.Command, result, took))
			last = out
			continue
		case errors.Is(err, errTestsFailed):
			outcome = "fail"
			steps = append(steps, fmt.Sprintf("@%s `%s`: failed (%s)", name, b.Command, took))
		case errors.As(err, &exit) && !couldNotRun(exit.ExitCode()):
			outcome = "fail"
			steps = append(steps, fmt.Sprintf("@%s `%s`: exit %d (%s)", name, b.Command, exit.ExitCode(), took))
		default:
			outcome = "error"
			steps = append(steps, fmt.Sprintf("@%s `%s`: could not run: %v (%s)", name, b.Command, err, took))
		}
		failed = out
		break
	}
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
		if !wasTerminal && st.terminal() {
			finishRun(r) // now, so what it adds (a pull request, say) is in the reply
		}
	}

	return fmt.Sprintf("ensign ran stage %q itself, because this project's @test binding is plain; no %s agent was started. "+
		"Outcome: %s.\n\n%s\nNext: %s", s.ID, s.Role, outcome, report, m.expect(scope)), nil
}
