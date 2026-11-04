// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The github-pr extension: each task works on its own branch, task/<name>, and a
// passing run leaves its tested commit there. Each step is its own:
//
//	run.start            @branch: create the task branch if needed, and switch to it
//	stage.before:test    commit what the run changed
//	run.finish:success   record the tested commit and tell the user it is waiting
//
// Landing it is a separate step the user asks for once the run is over: a pull request
// (`ensign github-pr pull-request`), or straight onto the default branch as a
// fast-forward (`ensign github-pr merge`). Either one refuses unless the task branch
// still holds exactly the tested commit, and refuses if a run of the task in that location failed after its last pass.
//
// The tests are the project's own @test. A project whose tests are its CI on GitHub
// binds @test to `ensign github-pr ci`: push the branch and wait for the checks.
//
// Everything here is Go, git and gh, so it runs the same on every platform.

// errTestsFailed marks a built-in test command whose tests ran and failed, as opposed to
// one that could not get a verdict at all.
var errTestsFailed = errors.New("the checks failed")

// runGH runs gh in dir. Tests replace it.
var runGH = func(dir string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("gh %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// How often, and for how long before any appear, the checks are polled.
var (
	checkPoll    = 15 * time.Second
	checkAppear  = 150 * time.Second
	checkTimeout = time.Hour
)

func taskBranch(task string) string {
	_, name, _ := strings.Cut(task, "/")
	return "task/" + name
}

// defaultBranch is the remote's default branch: origin/HEAD, or what the remote says.
func defaultBranch(ws string) (string, error) {
	if ref, err := gitOut(ws, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return strings.TrimPrefix(ref, "origin/"), nil
	}
	out, err := gitOut(ws, "ls-remote", "--symref", "origin", "HEAD")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				return strings.Fields(ref)[0], nil
			}
		}
	}
	return "", fmt.Errorf("could not tell the default branch of origin")
}

func currentBranch(ws string) string {
	b, _ := gitOut(ws, "symbolic-ref", "--short", "-q", "HEAD")
	return b
}

func dirty(ws string) (bool, error) {
	s, err := gitOut(ws, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %v", err)
	}
	return s != "", nil
}

// githubBranch runs at run.start: the workspace moves to the task's branch, created from
// the default branch the first time. Work on another branch is never carried along, and
// whatever passed before has to pass again before it can land.
func githubBranch(r *RunFiles) error {
	os.Remove(testedFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location)) // a new run has to pass again
	ws, want := r.Snap.Workspace, taskBranch(r.Snap.Task)
	cur := currentBranch(ws)
	_, verr := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+want)
	exists := verr == nil
	if exists && !r.State.StaleOK {
		if err := checkNotBehind(ws, want); err != nil {
			return err
		}
	}
	if cur == want {
		return nil
	}
	if d, err := dirty(ws); err != nil {
		return err
	} else if d {
		return fmt.Errorf("the workspace has uncommitted changes on %s; commit or stash them, then start the run again", orDetached(cur))
	}
	if exists {
		return git(ws, "switch", "-q", want)
	}
	base, err := defaultBranch(ws)
	if err != nil {
		return err
	}
	if err := git(ws, "fetch", "-q", "origin", base); err != nil {
		return err
	}
	return git(ws, "switch", "-q", "-c", want, "origin/"+base)
}

// branchBehind fetches the remote's default branch and tells whether it has commits that
// the branch lacks.
func branchBehind(ws, branch string) (base string, behind bool, err error) {
	if base, err = defaultBranch(ws); err != nil {
		return "", false, err
	}
	if err = git(ws, "fetch", "-q", "origin", base); err != nil {
		return "", false, err
	}
	err = exec.Command("git", "-C", ws, "merge-base", "--is-ancestor", "origin/"+base, "refs/heads/"+branch).Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return base, true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking %s against origin/%s: %v", branch, base, err)
	}
	return base, false, nil
}

// checkNotBehind refuses a task branch that lacks commits of the remote's default branch.
func checkNotBehind(ws, branch string) error {
	base, behind, err := branchBehind(ws, branch)
	if err != nil {
		return err
	}
	if behind {
		return fmt.Errorf("task branch %s is behind %s: origin/%s has commits it doesn't contain. Ask to update the branch (rebases it onto %s), or start the run again with --stale-branch to run on it as it is", branch, base, base, base)
	}
	return nil
}

// githubUpdate rebases a task's branch onto the remote's default branch. A conflict leaves
// the branch exactly as it was.
func githubUpdate(bridge, task, location string) (base, sha string, upToDate bool, err error) {
	for _, ri := range liveRuns(bridge) {
		// A queued sequence run has not started, so it does not hold the task.
		if ri.snap.Task == task && !ri.state.queued() {
			return "", "", false, fmt.Errorf("a run of %s is still going; wait for it to finish", task)
		}
	}
	c, err := loadConfigAt(bridge, task, location)
	if err != nil {
		return "", "", false, err
	}
	ws, want := c.Workspace, taskBranch(task)
	orig, err := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+want)
	if err != nil {
		return "", "", false, fmt.Errorf("the task branch %s doesn't exist; there is nothing to update", want)
	}
	cur := currentBranch(ws)
	if d, err := dirty(ws); err != nil {
		return "", "", false, err
	} else if d {
		return "", "", false, fmt.Errorf("the workspace has uncommitted changes on %s; commit or stash them first", orDetached(cur))
	}
	base, behind, err := branchBehind(ws, want)
	if err != nil {
		return "", "", false, err
	}
	if !behind {
		return base, short(orig), true, nil
	}
	if out, err := exec.Command("git", "-C", ws, "rebase", "origin/"+base, want).CombinedOutput(); err != nil {
		files, _ := gitZ(ws, "diff", "--name-only", "--diff-filter=U", "-z")
		git(ws, "rebase", "--abort")
		if cur != "" && cur != want {
			git(ws, "switch", "-q", cur)
		}
		if head, _ := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+want); head != orig {
			return "", "", false, fmt.Errorf("rebasing %s onto origin/%s failed and the branch could not be restored (was %s, now %s): %s", want, base, short(orig), short(head), strings.TrimSpace(string(out)))
		}
		if len(files) == 0 {
			return "", "", false, fmt.Errorf("rebasing %s onto origin/%s failed; the branch was left as it was: %s", want, base, strings.TrimSpace(string(out)))
		}
		return "", "", false, fmt.Errorf("rebasing %s onto origin/%s conflicts in: %s. The branch was left as it was", want, base, joinFileNames(files))
	}
	if cur != "" && cur != want {
		if err := git(ws, "switch", "-q", cur); err != nil {
			return "", "", false, err
		}
	}
	head, _ := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+want)
	return base, short(head), false, nil
}

func orDetached(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return b
}

// onTaskBranch checks the workspace is on the run's task branch.
func onTaskBranch(r *RunFiles) (string, error) {
	want := taskBranch(r.Snap.Task)
	if cur := currentBranch(r.Snap.Workspace); cur != want {
		return "", fmt.Errorf("the workspace is on %s, not the task branch %s", orDetached(cur), want)
	}
	return want, nil
}

// githubCommit runs before the test stage: everything the run changed is committed on
// the task branch, so the commit CI tests is exactly the work.
func githubCommit(r *RunFiles) error {
	ws := r.Snap.Workspace
	if _, err := onTaskBranch(r); err != nil {
		return err
	}
	if err := checkNoDependencyWiring(r); err != nil {
		return err
	}
	if d, err := dirty(ws); err != nil {
		return err
	} else if !d {
		return nil
	}
	if err := checkNoStrayFiles(r); err != nil {
		return err
	}
	_, name, _ := strings.Cut(r.Snap.Task, "/")
	if err := git(ws, "add", "-A"); err != nil {
		return err
	}
	return git(ws, "commit", "-q", "-m", name)
}

// githubCI is a test stage that is the repository's CI: push the task branch and wait
// for the checks on its commit. It commits nothing; uncommitted changes mean the commit
// step didn't run.
func githubCI(r *RunFiles, deadline time.Time) (string, error) {
	ws := r.Snap.Workspace
	branch, err := onTaskBranch(r)
	if err != nil {
		return "", err
	}
	if d, err := dirty(ws); err != nil {
		return "", err
	} else if d {
		return "", fmt.Errorf("the workspace has uncommitted changes, so CI would not test them")
	}
	if err := git(ws, "push", "-q", "--force-with-lease", "-u", "origin", branch); err != nil {
		return "", err
	}
	sha, _ := gitOut(ws, "rev-parse", "HEAD")
	return waitForChecks(ws, sha, deadline)
}

// testedFile records the commit of a task's last passing run, on this machine, so what
// the user later lands is exactly what was tested.
func testedFile(bridge, task, location string) string {
	return taskStateFile(bridge, task, location, ".tested")
}

// githubReady runs when the run succeeds: it records the tested commit and tells the user
// the work is waiting on the task branch. Nothing is pushed or opened until they ask.
func githubReady(r *RunFiles) error {
	ws := r.Snap.Workspace
	branch, err := onTaskBranch(r)
	if err != nil {
		return err
	}
	sha, err := gitOut(ws, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	f := testedFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(f, []byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	base, _ := defaultBranch(ws)
	r.State.note(fmt.Sprintf("The tested work is committed on %s (%s). Nothing has been opened yet: the user can ask for a pull request, or for it to go straight to %s",
		branch, short(sha), orDefault(base)))
	return nil
}

func orDefault(b string) string {
	if b == "" {
		return "the main branch"
	}
	return b
}

// noFaultPrefixes start the reason of a run that was retired before it started without failing at anything: the sequence cancelled, skipped, dropped, discarded, withdrew or replaced its step.
var noFaultPrefixes = []string{haltPrefix, "skipped: ", "discarded: ", "dropped: ", "withdrawn: ", "replaced by run "}

func retiredWithoutFault(st *State) bool {
	if st.Status != "error" || st.Started {
		return false
	}
	for _, p := range noFaultPrefixes {
		if strings.HasPrefix(st.Reason, p) {
			return true
		}
	}
	return false
}

// endedAfterPass reports whether a failed run ended after the given pass, by the At of its last history entry; history is to the second, so a tie, a pass without a finish time or a run without a readable entry falls back on run-id order.
func endedAfterPass(failed runInfo, pass runInfo) bool {
	if n := len(failed.state.History); n > 0 && pass.state.FinishedAt != 0 {
		if t, err := time.Parse(time.RFC3339, failed.state.History[n-1].At); err == nil {
			if sec, passSec := t.Unix(), time.Unix(0, pass.state.FinishedAt).Unix(); sec != passSec {
				return sec > passSec
			}
		}
	}
	return failed.id > pass.id
}

// latestRunPassed refuses when a run of the task in this location failed after the task's last pass (or when none passed), so a marker an earlier pass left behind cannot land failed work. A run retired without a history entry after the pass was created did no work and can slip past this.
func latestRunPassed(bridge, task, location string) error {
	var runs []runInfo
	var pass *runInfo
	for _, ri := range listRuns(bridge) {
		if ri.snap.Task != task || ri.snap.Location != location || !ri.state.terminal() {
			continue
		}
		if ri.state.Status != "success" {
			runs = append(runs, ri)
		} else if pass == nil || ri.state.FinishedAt > pass.state.FinishedAt || (ri.state.FinishedAt == pass.state.FinishedAt && ri.id > pass.id) {
			cp := ri
			pass = &cp
		}
	}
	for _, ri := range runs {
		if retiredWithoutFault(&ri.state) || (pass != nil && !endedAfterPass(ri, *pass)) {
			continue
		}
		return fmt.Errorf("the latest run of %s did not pass (%s); run the task again", task, ri.state.Reason)
	}
	return nil
}

// tested finds a task's tested commit, and checks the task branch still holds exactly it.
func tested(bridge, task, location string) (c *Config, branch, sha string, err error) {
	for _, ri := range liveRuns(bridge) {
		// A queued sequence run has not started, so it does not hold the task.
		if ri.snap.Task == task && !ri.state.queued() {
			return nil, "", "", fmt.Errorf("a run of %s is still going; wait for it to finish", task)
		}
	}
	c, err = loadConfigAt(bridge, task, location)
	if err != nil {
		return nil, "", "", err
	}
	data, err := os.ReadFile(testedFile(bridge, task, c.Location))
	if err != nil {
		return nil, "", "", fmt.Errorf("%s has no passing run on this machine yet; run it first", task)
	}
	if err := latestRunPassed(bridge, task, c.Location); err != nil {
		return nil, "", "", err
	}
	ws := c.Workspace
	branch, sha = taskBranch(task), strings.TrimSpace(string(data))
	head, err := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+branch)
	if err != nil {
		return nil, "", "", fmt.Errorf("the task branch %s is gone", branch)
	}
	if head != sha {
		return nil, "", "", fmt.Errorf("%s has changed since its tests passed (tested %s, now %s); run the task again", branch, short(sha), short(head))
	}
	if currentBranch(ws) == branch {
		if d, err := dirty(ws); err != nil {
			return nil, "", "", err
		} else if d {
			return nil, "", "", fmt.Errorf("%s has uncommitted changes that were never tested; commit or discard them first", branch)
		}
	}
	return c, branch, sha, nil
}

// githubOpenPR pushes a tested task branch and opens its pull request, or finds the one
// already open. It returns the pull request's URL.
func githubOpenPR(bridge, task, location string) (string, error) {
	c, branch, _, err := tested(bridge, task, location)
	if err != nil {
		return "", err
	}
	ws := c.Workspace
	if err := git(ws, "push", "-q", "--force-with-lease", "-u", "origin", branch); err != nil {
		return "", err
	}
	// Only an open pull request counts: a closed one for the same branch name is history.
	openPR := func() (string, error) {
		return runGH(ws, "pr", "list", "--head", branch, "--state", "open", "--json", "url", "--jq", ".[0].url // empty")
	}
	url, err := openPR()
	if err != nil || url != "" {
		return url, err
	}
	base, err := defaultBranch(ws)
	if err != nil {
		return "", err
	}
	_, name, _ := strings.Cut(task, "/")
	commits, _ := gitOut(ws, "log", "--format=- %s", "origin/"+base+".."+branch)
	if _, err := runGH(ws, "pr", "create", "--head", branch, "--base", base, "--title", name,
		"--body", "Opened from a starship task.\n\n"+commits); err != nil {
		return "", err
	}
	if url, err = openPR(); err != nil || url == "" {
		return "", fmt.Errorf("the pull request for %s was created but can't be found: %v", branch, err)
	}
	return url, nil
}

// githubMerge moves the default branch to a task's tested commit, if that is a
// fast-forward, and removes the task branch. It returns the default branch's name.
func githubMerge(bridge, task, location string) (string, string, error) {
	c, branch, sha, err := tested(bridge, task, location)
	if err != nil {
		return "", "", err
	}
	ws := c.Workspace
	base, err := defaultBranch(ws)
	if err != nil {
		return "", "", err
	}
	if err := git(ws, "fetch", "-q", "origin", base); err != nil {
		return "", "", err
	}
	if _, err := gitOut(ws, "merge-base", "--is-ancestor", "origin/"+base, sha); err != nil {
		return "", "", fmt.Errorf("%s has moved since this task branched from it, so the tested commit can't go there as it is. Run the task again to rebase and retest it", base)
	}
	if err := git(ws, "push", "-q", "origin", sha+":refs/heads/"+base); err != nil {
		return "", "", err
	}
	git(ws, "push", "-q", "origin", "--delete", branch) // it may never have been pushed
	if currentBranch(ws) == branch {
		if err := git(ws, "switch", "-q", base); err == nil {
			git(ws, "merge", "-q", "--ff-only", sha)
		}
	}
	if currentBranch(ws) != branch {
		git(ws, "branch", "-q", "-D", branch)
	}
	os.Remove(testedFile(bridge, task, c.Location))
	return base, sha, nil
}

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// waitForChecks polls GitHub's checks for a commit until they have all finished. Any check
// that didn't succeed makes it errTestsFailed, with the failing Actions logs' last lines.
func waitForChecks(ws, sha string, deadline time.Time) (string, error) {
	start := time.Now()
	if d := start.Add(checkTimeout); d.Before(deadline) {
		deadline = d
	}
	for {
		raw, err := runGH(ws, "api", "repos/{owner}/{repo}/commits/"+sha+"/check-runs?per_page=100", "--jq", ".check_runs")
		var runs []checkRun
		if err == nil {
			err = json.Unmarshal([]byte(raw), &runs)
		}
		if err != nil {
			return "", fmt.Errorf("reading the checks for %s: %v", short(sha), err)
		}
		pending := 0
		for _, c := range runs {
			if c.Status != "completed" {
				pending++
			}
		}
		switch {
		case len(runs) == 0 && time.Since(start) > checkAppear:
			return "", fmt.Errorf("no CI checks appeared for %s; does the repository's CI run on pushes to task branches?", short(sha))
		case len(runs) > 0 && pending == 0:
			return checkReport(ws, sha, runs)
		case time.Now().Add(checkPoll).After(deadline):
			return "", fmt.Errorf("the checks for %s were still running when the time ran out", short(sha))
		}
		time.Sleep(checkPoll)
	}
}

func checkReport(ws, sha string, runs []checkRun) (string, error) {
	var b strings.Builder
	failed := 0
	for _, c := range runs {
		fmt.Fprintf(&b, "%s: %s\n", c.Name, c.Conclusion)
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		default:
			failed++
		}
	}
	if failed == 0 {
		return b.String(), nil
	}
	ids, _ := runGH(ws, "run", "list", "--commit", sha, "--json", "databaseId,conclusion",
		"--jq", `.[] | select(.conclusion == "failure") | .databaseId`)
	for _, id := range strings.Fields(ids) {
		if logs, err := runGH(ws, "run", "view", id, "--log-failed"); err == nil {
			b.WriteString(tailLines(logs, 80) + "\n")
		}
	}
	return b.String(), errTestsFailed
}

// githubCommand runs a built-in github-pr command: as a handler, or as the test stage
// with a deadline.
func githubCommand(r *RunFiles, args []string, deadline time.Time) (string, error) {
	switch strings.Join(args, " ") {
	case "branch":
		return "", githubBranch(r)
	case "commit":
		return "", githubCommit(r)
	case "ci":
		return githubCI(r, deadline)
	case "ready":
		return "", githubReady(r)
	}
	return "", fmt.Errorf("unknown github-pr command: %s", strings.Join(args, " "))
}

// cmdGithubPR implements the steps the user asks for once a task's run has passed:
// `ensign github-pr pull-request|merge|update <project>/<task>`.
func cmdGithubPR(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign github-pr pull-request|merge|update <project>/<task> [--location <name>]")
	location := ""
	if len(args) == 4 && args[2] == "--location" {
		location = args[3]
		args = args[:2]
	}
	if len(args) != 2 || (args[0] != "pull-request" && args[0] != "merge" && args[0] != "update") {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if args[0] == "update" {
		base, sha, upToDate, err := githubUpdate(bridge, args[1], location)
		if err != nil {
			return err
		}
		if upToDate {
			fmt.Fprintf(out, "%s is up to date with origin/%s\n", taskBranch(args[1]), base)
			return nil
		}
		fmt.Fprintf(out, "Rebased %s onto origin/%s: %s\n", taskBranch(args[1]), base, sha)
		return nil
	}
	if args[0] == "pull-request" {
		url, err := githubOpenPR(bridge, args[1], location)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Pull request: %s\n", url)
		return nil
	}
	base, sha, err := githubMerge(bridge, args[1], location)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Pushed to %s: %s\n", base, short(sha))
	return nil
}
