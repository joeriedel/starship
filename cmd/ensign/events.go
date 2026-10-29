package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const handlerTimeout = 10 * time.Minute

// fire runs an event's require and observe handlers in declaration order. A failing
// require stops and returns its error; a failing observe is logged and ignored.
func fire(r *RunFiles, event string, d *Dispatch) error {
	for _, h := range r.Snap.Events[event] {
		if h.Kind == "augment" {
			continue
		}
		out, err := runHandler(r, h, d)
		if err == nil {
			r.State.log("handler", "", dispatchID(d), event+": "+h.Command)
			continue
		}
		msg := fmt.Sprintf("%s: %s failed: %v", event, h.Command, err)
		if out != "" {
			msg += ": " + lastLines(out, 5)
		}
		if h.Kind == "require" {
			return fmt.Errorf("%s (from %s)", msg, h.Source)
		}
		r.State.log("handler", "", dispatchID(d), "observe "+msg)
	}
	return nil
}

// augments returns the texts an event adds to the prompt being dispatched.
func augments(r *RunFiles, event string, d *Dispatch) []string {
	var out []string
	for _, h := range r.Snap.Events[event] {
		if h.Kind != "augment" {
			continue
		}
		if name, ok := strings.CutPrefix(h.Text, "@"); ok {
			if text := generatedAugment(r, name, h.Status, d); text != "" {
				out = append(out, text)
			}
			continue
		}
		out = append(out, h.Text)
	}
	return out
}

func dispatchID(d *Dispatch) string {
	if d == nil {
		return ""
	}
	return d.ID
}

// runHandler runs one handler. "ensign …" runs inside this process, which already holds
// the run's lock; "@name" runs a binding; anything else runs through the platform shell
// in the workspace.
func runHandler(r *RunFiles, h Handler, d *Dispatch) (string, error) {
	cmd := strings.TrimSpace(h.Command)
	if name, ok := strings.CutPrefix(cmd, "@"); ok {
		b, defined := r.Snap.Bindings[name]
		switch {
		case !defined:
			return "", fmt.Errorf("binding @%s is not defined", name)
		case b.NotApplicable != "":
			return "", nil
		case b.Command == "":
			return "", fmt.Errorf("binding @%s has no command", name)
		}
		cmd = b.Command
	}
	if args, ok := strings.CutPrefix(cmd, "ensign "); ok {
		return "", internalHandler(r, strings.Fields(args), d)
	}
	return shell(r.Snap.Workspace, cmd, handlerEnv(r, d))
}

func handlerEnv(r *RunFiles, d *Dispatch) []string {
	env := append(os.Environ(),
		"STARSHIP_RUN="+r.State.Run,
		"STARSHIP_RUN_DIR="+r.Dir,
		"STARSHIP_WORKSPACE="+r.Snap.Workspace,
		"STARSHIP_TASK="+r.Snap.Task,
	)
	if d != nil {
		env = append(env, "STARSHIP_STAGE="+d.Stage, "STARSHIP_DISPATCH="+d.ID, "STARSHIP_OUTCOME="+d.Outcome)
	}
	return env
}

// shell runs a command line through the platform's shell.
func shell(dir, command string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	var c *exec.Cmd
	switch {
	case runtime.GOOS != "windows":
		c = exec.CommandContext(ctx, "sh", "-c", command)
	default:
		if bash, err := exec.LookPath("bash"); err == nil {
			c = exec.CommandContext(ctx, bash, "-c", command)
		} else {
			c = exec.CommandContext(ctx, "cmd", "/C", command)
		}
	}
	c.Dir, c.Env = dir, env
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %s", handlerTimeout)
	}
	return out.String(), err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// internalHandler runs a built-in handler against the open run.
func internalHandler(r *RunFiles, args []string, d *Dispatch) error {
	switch strings.Join(args, " ") {
	case "git-catalog record-base":
		return catalogRecordBase(r)
	case "git-catalog commit-pass":
		return catalogCommitPass(r, d)
	case "git-catalog squash":
		return catalogSquashRun(r)
	}
	return fmt.Errorf("unknown built-in handler: ensign %s", strings.Join(args, " "))
}

// finishRun runs what follows a run's end: the @done check before a success stands,
// then the run.finish handlers.
func finishRun(r *RunFiles) {
	st := r.State
	m := &machine{snap: r.Snap, st: st}
	if st.Status == "success" {
		if b, ok := r.Snap.Bindings["done"]; ok && b.Command != "" {
			if out, err := shell(r.Snap.Workspace, b.Command, handlerEnv(r, nil)); err != nil {
				st.Status = "running" // let fail record it as an error
				m.fail("done-check", fmt.Sprintf("@done failed: %v: %s", err, lastLines(out, 5)))
			}
		}
	}
	status := st.Status
	if err := fire(r, "run.finish:"+status, nil); err != nil && status == "success" {
		st.Status = "running"
		m.fail("handler", err.Error())
		fire(r, "run.finish:error", nil)
	}
}
