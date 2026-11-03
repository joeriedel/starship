// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The git-catalog extension: one local commit per coder pass, so the lead can read how the
// work moved between passes, and a squash placed wherever the project chooses.

func taskBaseFile(bridge, task, location string) string {
	return taskStateFile(bridge, task, location, ".base")
}

// catalogRecordBase runs at run.start: the workspace must be a clean git tree, and its
// HEAD becomes the run's base (and the task's, if the task has none yet).
func catalogRecordBase(r *RunFiles) error {
	ws := r.Snap.Workspace
	if inside, err := gitOut(ws, "rev-parse", "--is-inside-work-tree"); err != nil || inside != "true" {
		return fmt.Errorf("the workspace is not a git repository")
	}
	status, err := gitOut(ws, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %v", err)
	}
	retry := r.State.RetryOf != ""
	if status != "" && !retry {
		return fmt.Errorf("the workspace has uncommitted changes; the run won't start on top of someone's work: %s",
			lastLines(status, 10))
	}
	head, err := gitOut(ws, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("the workspace has no commits yet")
	}
	if !retry || r.State.Base == "" {
		r.State.Base = head
	}
	if retry {
		dirty, err := dirtyPaths(ws)
		if err != nil {
			return fmt.Errorf("git status: %v", err)
		}
		r.State.Carried = nil
		for _, d := range dirty {
			r.State.Carried = append(r.State.Carried, d.path)
		}
	}
	tb := taskBaseFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location)
	if _, err := os.Stat(tb); err != nil {
		if err := writeFile(tb, head+"\n"); err != nil {
			return err
		}
	}
	return nil
}

type dirtyPath struct {
	path     string
	unstaged bool // the working tree differs from the index
}

// dirtyPaths lists the staged, unstaged and untracked paths of the workspace.
func dirtyPaths(ws string) ([]dirtyPath, error) {
	toks, err := gitZ(ws, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var out []dirtyPath
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		out = append(out, dirtyPath{t[3:], t[1] != ' '})
		if strings.ContainsAny(t[:2], "RC") {
			i++ // the original path follows
		}
	}
	return out, nil
}

// catalogCommitPass runs after each coder pass: it commits the files the coder reported plus the leftover files a retry carried over (r.State.Carried) that are still dirty, and refuses if anything else in the workspace changed.
func catalogCommitPass(r *RunFiles, d *Dispatch) error {
	if d == nil {
		return fmt.Errorf("commit-pass runs only after a stage")
	}
	ws := r.Snap.Workspace
	if err := checkNoDependencyWiring(r); err != nil {
		return err
	}
	var files []string
	for _, f := range d.Fields["files"] {
		if filepath.IsAbs(f) {
			if rel, err := filepath.Rel(ws, f); err == nil {
				f = rel
			}
		}
		if filepath.IsAbs(f) || strings.HasPrefix(filepath.Clean(f), "..") {
			return fmt.Errorf("reported file %q is not a path inside the workspace", f)
		}
		files = append(files, filepath.ToSlash(filepath.Clean(f)))
	}
	listed := map[string]bool{}
	for _, f := range files {
		listed[f] = true
	}
	// A retry starts on the previous attempt's leftovers; those still dirty count as reported.
	dirty, err := dirtyPaths(ws)
	if err != nil {
		return fmt.Errorf("git status: %v", err)
	}
	for _, d := range dirty {
		for _, c := range r.State.Carried {
			if d.path == c && !listed[c] {
				listed[c] = true
				files = append(files, c)
			}
		}
	}
	if len(files) > 0 {
		if err := git(ws, append([]string{"add", "--"}, files...)...); err != nil {
			return err
		}
	}
	dirty, err = dirtyPaths(ws)
	if err != nil {
		return fmt.Errorf("git status: %v", err)
	}
	var stray []string
	for _, d := range dirty {
		if d.unstaged || !listed[d.path] {
			stray = append(stray, d.path)
		}
	}
	if len(stray) > 0 {
		return fmt.Errorf("files changed that the coder did not report: %s", strings.Join(stray, ", "))
	}
	staged, err := gitOut(ws, "diff", "--cached", "--name-only")
	if err != nil {
		return fmt.Errorf("git diff --cached: %v", err)
	}
	if staged == "" {
		r.State.log("handler", "", d.ID, "commit-pass: no changes to commit")
		return nil
	}
	cycle, pass := r.State.Spent["test"]+1, r.State.Spent["loop"]
	subject := fmt.Sprintf("wip(c%d p%d)", cycle, pass)
	if d.Outcome == "broken" {
		subject += " BROKEN"
	}
	subject += ": " + truncate(d.Summary, 60)
	return git(ws, "commit", "-q", "--no-verify", "-m", subject, "-m", d.Summary)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// squash collapses base..HEAD into one commit with message, refusing if any of those
// commits has already reached the remote.
func squash(ws, base, message string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("no base commit recorded")
	}
	if n, _ := gitOut(ws, "rev-list", "--count", base+"..HEAD"); n == "0" || n == "" {
		return "", fmt.Errorf("nothing to squash after %s", short(base))
	}
	if status, _ := gitOut(ws, "status", "--porcelain", "--untracked-files=no"); status != "" {
		return "", fmt.Errorf("the workspace has uncommitted changes to tracked files: %s", lastLines(status, 5))
	}
	branch, err := gitOut(ws, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("HEAD is detached")
	}
	if remote, _ := gitOut(ws, "remote"); remote != "" {
		name := strings.Fields(remote)[0]
		out, err := gitOut(ws, "ls-remote", name, "refs/heads/"+branch)
		if err != nil {
			return "", fmt.Errorf("could not ask the remote about %s: %v", branch, err)
		}
		if fields := strings.Fields(out); len(fields) > 0 {
			pushed := fields[0]
			if gitOK(ws, "merge-base", "--is-ancestor", base, pushed) && pushed != base {
				return "", fmt.Errorf("commits after %s are already on %s/%s; pushed history is never rewritten", short(base), name, branch)
			}
		}
	}
	tree, _ := gitOut(ws, "rev-parse", "HEAD^{tree}")
	if err := git(ws, "reset", "--soft", base); err != nil {
		return "", err
	}
	if err := git(ws, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	if after, _ := gitOut(ws, "rev-parse", "HEAD^{tree}"); after != tree {
		return "", fmt.Errorf("the squashed tree differs from the original (%s vs %s); check the range", short(after), short(tree))
	}
	return gitOut(ws, "rev-parse", "--short", "HEAD")
}

func gitOK(dir string, args ...string) bool {
	_, err := gitOut(dir, args...)
	return err == nil
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

// catalogSquashRun is the handler form: it squashes this run's own commits.
func catalogSquashRun(r *RunFiles) error {
	msg := firstLine(strings.TrimPrefix(strings.TrimSpace(section(r.Snap.WorkItem, "Goal")), "- "))
	if msg == "" {
		msg = r.Snap.Task
	}
	sha, err := squash(r.Snap.Workspace, r.State.Base, msg)
	if err == nil {
		r.State.log("handler", "", "", "squashed the run's commits into "+sha)
	}
	return err
}

// cmdCatalog implements `ensign git-catalog squash <project>/<task> -m <message>`: it
// collapses everything since the task's base into one commit, ready for the user to push.
func cmdCatalog(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign git-catalog squash <project>/<task> [--location <name>] -m <message>")
	location := ""
	if len(args) == 6 && args[2] == "--location" {
		location = args[3]
		args = append(args[:2:2], args[4:]...)
	}
	if len(args) != 4 || args[0] != "squash" || args[2] != "-m" {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	task := args[1]
	for _, ri := range liveRuns(bridge) {
		// A queued sequence run has not started, so it does not hold the task.
		if ri.snap.Task == task && !ri.state.queued() {
			return fmt.Errorf("run %s on this task is still active", ri.id)
		}
	}
	c, err := loadConfigAt(bridge, task, location)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(taskBaseFile(bridge, task, c.Location))
	if err != nil {
		return fmt.Errorf("no catalog base recorded for %s on this machine", task)
	}
	sha, err := squash(c.Workspace, strings.TrimSpace(string(data)), args[3])
	if err != nil {
		return err
	}
	os.Remove(taskBaseFile(bridge, task, c.Location))
	fmt.Fprintf(out, "squashed into %s; it has not been pushed\n", sha)
	return nil
}
