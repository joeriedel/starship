// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// p4Program is the Perforce command line client; tests point it at the test binary's fake.
var p4Program = "p4"

var p4ChangeRe = regexp.MustCompile(`(?m)^\.\.\. change (\S+)\s*$`)

// lookP4 finds p4 on the PATH of env, not the process's: the project's environment may supply it. A p4Program with a folder in it is used as it is.
func lookP4(env []string) (string, error) {
	if strings.ContainsAny(p4Program, `/\`) {
		return exec.LookPath(p4Program)
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if k == "PATH" || (hostOS == "windows" && strings.EqualFold(k, "PATH")) {
			for _, dir := range filepath.SplitList(v) {
				if p, err := exec.LookPath(filepath.Join(dir, p4Program)); err == nil {
					return p, nil
				}
			}
		}
	}
	return "", exec.ErrNotFound
}

// p4Run runs one p4 command for the client in the workspace and returns its output; a failure is an error of one line.
func p4Run(ws string, env []string, client string, args ...string) (string, error) {
	prog, err := lookP4(env)
	if err != nil {
		return "", errors.New("p4: not found on PATH")
	}
	c := exec.Command(prog, append([]string{"-c", client}, args...)...)
	c.Dir = ws
	c.Env = append(env, "PWD="+ws)
	out, err := c.Output()
	if err == nil {
		return string(out), nil
	}
	msg := err.Error()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg = strings.TrimSpace(string(ee.Stderr))
		if msg == "" {
			msg = ee.Error()
		}
	}
	first, _, _ := strings.Cut(msg, "\n")
	return "", fmt.Errorf("p4 %s: %s", strings.Join(args, " "), strings.TrimSpace(first))
}

// p4State reports a Perforce workspace for a task with vcs.kind p4: the state line, and a problem (one line) when the workspace cannot be inspected. Changelists beyond the task's are information, never a problem. A @state binding replaces the inspection and its output is shown as is.
func p4State(ws string, v TaskVcs, bindings map[string]Binding, env []string) (string, error) {
	if b, ok := bindings["state"]; ok && b.NotApplicable == "" && b.Command != "" {
		out, err := shell(ws, b.Command, env)
		if err != nil {
			return "", fmt.Errorf("@state failed: %v: %s", err, lastLines(out, 3))
		}
		return strings.TrimRight(out, "\r\n"), nil
	}
	client := v.Client
	if client == "" {
		return "", errors.New("p4: the task's vcs: has no client")
	}
	if _, err := lookP4(env); err != nil {
		return "", errors.New("p4: not found on PATH")
	}
	if _, err := p4Run(ws, env, client, "login", "-s"); err != nil {
		return "", fmt.Errorf("p4: not logged in, run p4 login (%v)", err)
	}
	out, err := p4Run(ws, env, client, "-ztag", "clients", "-e", client)
	if err != nil {
		return "", err
	}
	if !strings.Contains(out, "... client ") {
		return "", fmt.Errorf("p4: client %s is unknown to the server", client)
	}
	out, err = p4Run(ws, env, client, "-ztag", "changes", "-s", "pending", "-c", client)
	if err != nil {
		return "", err
	}
	pending := map[string]bool{}
	for _, m := range p4ChangeRe.FindAllStringSubmatch(out, -1) {
		pending[m[1]] = true
	}
	out, err = p4Run(ws, env, client, "-ztag", "opened", "-C", client)
	if err != nil {
		return "", err
	}
	opened := map[string]int{}
	for _, m := range p4ChangeRe.FindAllStringSubmatch(out, -1) {
		opened[m[1]]++
	}
	s := "p4, client " + client
	if v.Changelist != "" {
		if pending[v.Changelist] {
			s += fmt.Sprintf(", task changelist %s (%d opened files)", v.Changelist, opened[v.Changelist])
		} else {
			s += fmt.Sprintf(", task changelist %s is not among the client's pending changelists", v.Changelist)
		}
	}
	var others []string
	for c := range pending {
		if c != v.Changelist {
			others = append(others, c)
		}
	}
	if opened["default"] > 0 && v.Changelist != "default" {
		others = append(others, "default")
	}
	sort.Strings(others)
	if len(others) == 0 {
		return s + ", no other pending changelists", nil
	}
	for i, c := range others {
		others[i] = fmt.Sprintf("%s (%d opened files)", c, opened[c])
	}
	return s + ", other changelists: " + strings.Join(others, ", "), nil
}

// taskVcsLine is the VCS line of task open: Perforce tasks are inspected here, everything else by vcsState. A problem is shown as the line.
func taskVcsLine(bridge, task, location, ws string, v TaskVcs) string {
	if v.Kind != "p4" {
		return vcsState(ws, v)
	}
	project, _, _ := strings.Cut(task, "/")
	c, err := loadTaskLayers(bridge, task, location)
	if err != nil {
		return fmt.Sprintf("p4: could not load the project's bindings: %v", err)
	}
	s, err := p4State(ws, v, c.Bindings, projectEnv(bridge, project, os.Environ()))
	if err != nil {
		return err.Error()
	}
	return s
}

// runP4State inspects the Perforce workspace of a run whose task has vcs.kind p4; for any other task it returns an empty state.
func runP4State(r *RunFiles) (string, error) {
	if r.Snap.Task == "" {
		return "", nil
	}
	project, name, _ := strings.Cut(r.Snap.Task, "/")
	front, _, err := splitFrontmatter(filepath.Join(r.Snap.Bridge, "projects", project, "tasks", name, "task.md"))
	if err != nil {
		return "", err
	}
	var tf TaskFile
	if err := decodeFrontmatter(front, &tf); err != nil {
		return "", err
	}
	if tf.Vcs.Kind != "p4" {
		return "", nil
	}
	return p4State(r.Snap.Workspace, tf.Vcs, r.Snap.Bindings, handlerEnv(r, nil))
}
