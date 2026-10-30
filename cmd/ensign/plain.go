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

// runPlainStage runs @build (when bound) and @test in the workspace, completes the
// dispatch with the result, and returns what the dispatching agent is told.
func runPlainStage(r *RunFiles, m *machine, d *Dispatch, s *Stage, scope string) string {
	limit := preToolUseTimeout(r.Snap.Bridge) - time.Minute
	if limit < 30*time.Second {
		limit = 30 * time.Second
	}
	deadline := time.Now().Add(limit)
	outcome := "pass"
	var steps, log []string
	failed := ""
	for _, name := range []string{"build", "test"} {
		b, ok := r.Snap.Bindings[name]
		if !ok || b.NotApplicable != "" || b.Command == "" {
			continue
		}
		start := time.Now()
		out, err := shellFor(r.Snap.Workspace, b.Command, handlerEnv(r, d), time.Until(deadline))
		took := time.Since(start).Round(time.Second)
		log = append(log, fmt.Sprintf("$ %s\n%s", b.Command, out))
		var exit *exec.ExitError
		switch {
		case err == nil:
			steps = append(steps, fmt.Sprintf("@%s `%s`: exit 0 (%s)", name, b.Command, took))
			continue
		case errors.As(err, &exit) && exit.ExitCode() != 126 && exit.ExitCode() != 127:
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
	if failed != "" {
		report += "\nThe end of the failing command's output:\n\n```\n" + lastLines(failed, 40) + "\n```\n"
	}
	report += "\nFull output: " + logFile + "\n"
	completeDispatch(r, m, d, s, outcome, summary, nil, report)

	return fmt.Sprintf("ensign ran stage %q itself, because this project's @test binding is plain; no %s agent was started. "+
		"Outcome: %s.\n\n%s\nNext: %s", s.ID, s.Role, outcome, report, m.expect(scope))
}
