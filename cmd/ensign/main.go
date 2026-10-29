// Command ensign is starship's resolver, hook driver and CLI.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:], os.Stdout)
	case "next":
		err = cmdNext(os.Args[2:])
	case "hook":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		os.Exit(cmdHook(os.Args[2], os.Stdin, os.Stdout, os.Stderr))
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ensign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ensign run <project>/<task> [--start <stage>] [--dry-run]
  ensign next [--bridge <dir>] [--run <id>]
  ensign hook <SessionStart|PreToolUse|SubagentStop|UserPromptSubmit>`)
}

// cmdHook runs one hook event and returns the exit code for Claude Code.
// Internal errors exit 2, which Claude Code treats as blocking: ensign fails closed.
func cmdHook(event string, stdin io.Reader, stdout, stderr io.Writer) int {
	var in HookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		fmt.Fprintln(stderr, "starship: ensign could not read the hook input:", err)
		return 2
	}
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		dir = in.Cwd
	}
	bridge, err := findBridge(dir)
	if err != nil {
		return 0 // outside a bridge, hook events are no-ops
	}
	if event == "SessionStart" {
		return hookSessionStart(&in, bridge, stdout, stderr)
	}
	r, err := openSessionRun(bridge, in.SessionID)
	if err != nil {
		return failHook(&in, nil, err, stderr)
	}
	if r == nil && event == "UserPromptSubmit" && strings.TrimSpace(in.Prompt) == "go" {
		if r, err = claimRun(bridge, in.SessionID); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	} else if r == nil {
		return 0
	}
	if r != nil {
		defer r.close()
	}

	var out any
	switch event {
	case "PreToolUse":
		out, err = hookPreToolUse(&in, r)
	case "SubagentStop":
		out, err = hookSubagentStop(&in, r)
	case "UserPromptSubmit":
		if strings.TrimSpace(in.Prompt) == "go" {
			out, err = hookUserPromptSubmit(&in, r)
		}
	default:
		err = fmt.Errorf("unknown hook event %q", event)
	}
	if err != nil {
		return failHook(&in, r, err, stderr)
	}
	if r != nil {
		if err := r.save(); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	}
	if out != nil {
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	}
	return 0
}

// failHook ends the run as error: ensign, and blocks the action. A stop that is already
// being forced to continue is let through, so a persistent fault can't trap an agent.
func failHook(in *HookInput, r *RunFiles, err error, stderr io.Writer) int {
	if r != nil && !r.State.terminal() {
		m := &machine{snap: r.Snap, st: r.State}
		m.fail("ensign", err.Error())
		r.save()
	}
	fmt.Fprintln(stderr, "starship: ensign error, the run has stopped:", err)
	if in.StopHookActive {
		return 0
	}
	return 2
}

// hookSessionStart gives the session's commands its id, so `ensign run` can bind the run
// to this session, and prints the heartbeat the bridge's CLAUDE.md checks for.
func hookSessionStart(in *HookInput, bridge string, stdout, stderr io.Writer) int {
	if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" && in.SessionID != "" {
		env, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, err = fmt.Fprintf(env, "export STARSHIP_SESSION=%s\n", in.SessionID)
			env.Close()
		}
		if err != nil {
			fmt.Fprintln(stderr, "starship: could not record the session id:", err)
			return 2
		}
	}
	fmt.Fprintf(stdout, "STARSHIP-READY ensign bridge %s\n", bridge)
	return 0
}
