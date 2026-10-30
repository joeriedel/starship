// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return shellFor(dir, command, env, handlerTimeout)
}

// shellFor runs a command line through the platform's shell with a time limit.
func shellFor(dir, command string, env []string, limit time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
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
		err = fmt.Errorf("timed out after %s", limit.Round(time.Second))
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
	case "style record":
		return styleRecord(r, d)
	}
	if len(args) > 0 && args[0] == "github-pr" {
		_, err := githubCommand(r, args[1:], time.Now().Add(handlerTimeout))
		return err
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
	if err := logRunInJournal(r); err != nil {
		st.log("journal", "", "", "the run could not be added to the task's progress log: "+err.Error())
	}
}

// runEntry is the line a finished run adds to its task's progress log: what happened, in
// facts only. The session writes the account under it.
func runEntry(r *RunFiles) string {
	st := r.State
	outcome := st.Status
	switch {
	case st.Status == "ask-user":
		outcome = "paused to ask the user"
	case st.Reason != "" && st.Status != "success":
		outcome += " (" + st.Reason + ")"
	}
	passes := 0
	files := map[string]bool{}
	for _, d := range st.Dispatches {
		if d.Role == "coder" && d.Done {
			passes++
		}
		for _, f := range d.Fields["files"] {
			files[f] = true
		}
	}
	ws := r.Snap.Workspace
	head := ""
	if st.StartHead != "" {
		head, _ = gitOut(ws, "rev-parse", "HEAD")
		if changed, err := gitOut(ws, "diff", "--name-only", st.StartHead); err == nil {
			for _, f := range strings.Fields(changed) {
				files[f] = true
			}
		}
		if untracked, err := gitOut(ws, "ls-files", "--others", "--exclude-standard"); err == nil {
			for _, f := range strings.Fields(untracked) {
				files[f] = true
			}
		}
	}
	line := fmt.Sprintf("- **%s, run %s (%s): %s.**", time.Now().Format("2006-01-02"), st.Run, r.Snap.Pipeline.Name, outcome)
	if passes > 0 {
		line += fmt.Sprintf(" %d coder pass%s.", passes, map[bool]string{true: "", false: "es"}[passes == 1])
	}
	if len(files) > 0 {
		line += " Changed: " + strings.Join(sortedStrings(files), ", ") + "."
	}
	if head != "" && head != st.StartHead {
		line += fmt.Sprintf(" Commits: %s..%s.", short(st.StartHead), short(head))
	} else if st.StartHead != "" {
		line += fmt.Sprintf(" Started at %s; nothing committed.", short(st.StartHead))
	}
	return line + costLine(r)
}

// logRunInJournal adds the run's line to the end of its task's progress log, so every
// run is in the journal even if nothing else gets written.
func logRunInJournal(r *RunFiles) error {
	p, t, ok := strings.Cut(r.Snap.Task, "/")
	if !ok {
		return nil
	}
	f := filepath.Join(r.Snap.Bridge, "projects", p, "tasks", t, "task.md")
	data, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	return os.WriteFile(f, []byte(insertProgress(string(data), runEntry(r))), 0o644)
}

// insertProgress puts an entry at the end of the "## Progress log" section, adding the
// section if there isn't one.
func insertProgress(doc, entry string) string {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "## Progress log" {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.TrimRight(doc, "\n") + "\n\n## Progress log\n\n" + entry + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	last := end - 1
	for last > start && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	out := append([]string{}, lines[:last+1]...)
	if last == start {
		out = append(out, "")
	}
	out = append(out, entry)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n")
}
