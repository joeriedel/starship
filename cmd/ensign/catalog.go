package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The git-catalog bundle: one local commit per coder pass, so the lead can read how the
// work moved between passes, and a squash placed wherever the project chooses.

func taskBaseFile(bridge, task string) string {
	return filepath.Join(bridge, ".starship", "tasks", strings.ReplaceAll(task, "/", "--")+".base")
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
	if status != "" {
		return fmt.Errorf("the workspace has uncommitted changes; the run won't start on top of someone's work: %s",
			lastLines(status, 10))
	}
	head, err := gitOut(ws, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("the workspace has no commits yet")
	}
	r.State.Base = head
	tb := taskBaseFile(r.Snap.Bridge, r.Snap.Task)
	if _, err := os.Stat(tb); err != nil {
		if err := writeFile(tb, head+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// catalogCommitPass runs after each coder pass: it commits exactly the files the coder
// reported, and refuses if anything else in the workspace changed.
func catalogCommitPass(r *RunFiles, d *Dispatch) error {
	if d == nil {
		return fmt.Errorf("commit-pass runs only after a stage")
	}
	ws := r.Snap.Workspace
	files := d.Fields["files"]
	for _, f := range files {
		if filepath.IsAbs(f) || strings.HasPrefix(filepath.Clean(f), "..") {
			return fmt.Errorf("reported file %q is not a path inside the workspace", f)
		}
	}
	if len(files) > 0 {
		if err := git(ws, append([]string{"add", "--"}, files...)...); err != nil {
			return err
		}
	}
	listed := map[string]bool{}
	for _, f := range files {
		listed[filepath.ToSlash(filepath.Clean(f))] = true
	}
	status, _ := gitOut(ws, "status", "--porcelain")
	var stray []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.Trim(line[3:], `"`)
		if _, after, ok := strings.Cut(path, " -> "); ok {
			path = after
		}
		if line[1] != ' ' || !listed[path] {
			stray = append(stray, path)
		}
	}
	if len(stray) > 0 {
		return fmt.Errorf("files changed that the coder did not report: %s", strings.Join(stray, ", "))
	}
	if staged, _ := gitOut(ws, "diff", "--cached", "--name-only"); staged == "" {
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
	usage := fmt.Errorf("usage: ensign git-catalog squash <project>/<task> -m <message>")
	if len(args) != 4 || args[0] != "squash" || args[2] != "-m" {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	task := args[1]
	for _, ri := range liveRuns(bridge) {
		if ri.snap.Task == task {
			return fmt.Errorf("run %s on this task is still active", ri.id)
		}
	}
	c, err := loadConfig(bridge, task)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(taskBaseFile(bridge, task))
	if err != nil {
		return fmt.Errorf("no catalog base recorded for %s on this machine", task)
	}
	sha, err := squash(c.Workspace, strings.TrimSpace(string(data)), args[3])
	if err != nil {
		return err
	}
	os.Remove(taskBaseFile(bridge, task))
	fmt.Fprintf(out, "squashed into %s; it has not been pushed\n", sha)
	return nil
}
