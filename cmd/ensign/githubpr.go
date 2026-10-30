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

	"gopkg.in/yaml.v3"
)

// The github-pr extension: each task works on its own branch, task/<name>, and a
// passing run ends as a pull request. Each step is its own:
//
//	run.start            @branch: create the task branch if needed, and switch to it
//	stage.before:test    commit what the run changed
//	run.finish:success   @land: push the branch and open the pull request; or, for a
//	                     direct task, move the default branch to the tested commit
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

func dirty(ws string) bool {
	s, _ := gitOut(ws, "status", "--porcelain")
	return s != ""
}

// githubBranch runs at run.start: the workspace moves to the task's branch, created from
// the default branch the first time. Work on another branch is never carried along.
func githubBranch(r *RunFiles) error {
	ws, want := r.Snap.Workspace, taskBranch(r.Snap.Task)
	cur := currentBranch(ws)
	if cur == want {
		return nil
	}
	if dirty(ws) {
		return fmt.Errorf("the workspace has uncommitted changes on %s; commit or stash them, then start the run again", orDetached(cur))
	}
	if _, err := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+want); err == nil {
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
	if !dirty(ws) {
		return nil
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
	if dirty(ws) {
		return "", fmt.Errorf("the workspace has uncommitted changes, so CI would not test them")
	}
	if err := git(ws, "push", "-q", "--force-with-lease", "-u", "origin", branch); err != nil {
		return "", err
	}
	sha, _ := gitOut(ws, "rev-parse", "HEAD")
	return waitForChecks(ws, sha, deadline)
}

// githubOpenPR runs when the run succeeds: it pushes the task branch, opens its pull
// request or finds the one already open, and notes it for the run's report.
func githubOpenPR(r *RunFiles) error {
	ws := r.Snap.Workspace
	branch, err := onTaskBranch(r)
	if err != nil {
		return err
	}
	if err := git(ws, "push", "-q", "--force-with-lease", "-u", "origin", branch); err != nil {
		return err
	}
	url, err := runGH(ws, "pr", "view", branch, "--json", "url", "--jq", ".url")
	if err != nil {
		base, err := defaultBranch(ws)
		if err != nil {
			return err
		}
		_, name, _ := strings.Cut(r.Snap.Task, "/")
		commits, _ := gitOut(ws, "log", "--format=- %s", "origin/"+base+"..HEAD")
		if _, err := runGH(ws, "pr", "create", "--head", branch, "--base", base, "--title", name,
			"--body", "Opened from a starship task.\n\n"+commits); err != nil {
			return err
		}
		if url, err = runGH(ws, "pr", "view", branch, "--json", "url", "--jq", ".url"); err != nil {
			return err
		}
	}
	r.State.note("Pull request: " + url)
	return nil
}

// githubMerge runs when a direct task's run succeeds: the default branch moves to the
// commit CI just passed, if that is a fast-forward, and the task branch is removed.
func githubMerge(r *RunFiles) error {
	ws := r.Snap.Workspace
	branch, err := onTaskBranch(r)
	if err != nil {
		return err
	}
	base, err := defaultBranch(ws)
	if err != nil {
		return err
	}
	if err := git(ws, "fetch", "-q", "origin", base); err != nil {
		return err
	}
	if _, err := gitOut(ws, "merge-base", "--is-ancestor", "origin/"+base, "HEAD"); err != nil {
		return fmt.Errorf("%s has moved since this task branched from it; the tested commit can't go there as it is. Rebase the task branch and run the tests again", base)
	}
	sha, _ := gitOut(ws, "rev-parse", "HEAD")
	if err := git(ws, "push", "-q", "origin", "HEAD:refs/heads/"+base); err != nil {
		return err
	}
	git(ws, "push", "-q", "origin", "--delete", branch) // it may never have been pushed under this name
	if err := git(ws, "switch", "-q", base); err == nil {
		git(ws, "merge", "-q", "--ff-only", sha)
		git(ws, "branch", "-q", "-D", branch)
	}
	r.State.note(fmt.Sprintf("Pushed to %s: %s", base, short(sha)))
	return nil
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
	case "open-pull-request":
		return "", githubOpenPR(r)
	case "merge":
		return "", githubMerge(r)
	}
	return "", fmt.Errorf("unknown github-pr command: %s", strings.Join(args, " "))
}

// cmdGithubPR implements `ensign github-pr direct <project>/<task>`: once its run passes
// CI, the task goes straight to the default branch with no pull request. It replaces
// @land in the task's own settings.
func cmdGithubPR(args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "direct" {
		return fmt.Errorf("usage: ensign github-pr direct <project>/<task>")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	p, t, ok := strings.Cut(args[1], "/")
	if !ok {
		return fmt.Errorf("name the task as <project>/<task>")
	}
	f := filepath.Join(bridge, "projects", p, "tasks", t, "task.md")
	front, body, err := splitFrontmatter(f)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return fmt.Errorf("%s: %v", f, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: the frontmatter is not a mapping", f)
	}
	root := doc.Content[0]
	var bindings *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "bindings" {
			bindings = root.Content[i+1]
		}
	}
	if bindings == nil {
		bindings = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "bindings"}, bindings)
	}
	set := func(key, command string, plain bool) {
		v := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "command"}, {Kind: yaml.ScalarNode, Value: command}}}
		if plain {
			v.Content = append(v.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "plain"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		}
		for i := 0; i+1 < len(bindings.Content); i += 2 {
			if strings.TrimPrefix(bindings.Content[i].Value, "@") == key {
				bindings.Content[i+1] = v
				return
			}
		}
		bindings.Content = append(bindings.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	}
	set("land", "ensign github-pr merge", false)
	newFront, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(f, []byte("---\n"+string(newFront)+"---\n\n"+string(body)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s now goes straight to the default branch, with no pull request\n", args[1])
	return nil
}
