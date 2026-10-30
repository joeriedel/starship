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

// The github-pr extension: each task works on its own branch, task/<name>, and its test
// stage is the repository's CI on GitHub. The branch is pushed, its pull request opened,
// and the run waits for the checks. A task can go straight to the default branch instead
// ("direct"), with no pull request. Everything here is Go, git and gh, so it runs the same
// on every platform.

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

// githubDirectStart runs at run.start for a direct task: the workspace moves to the
// default branch, up to date with origin.
func githubDirectStart(r *RunFiles) error {
	ws := r.Snap.Workspace
	if dirty(ws) {
		return fmt.Errorf("the workspace has uncommitted changes on %s; commit or stash them, then start the run again", orDetached(currentBranch(ws)))
	}
	base, err := defaultBranch(ws)
	if err != nil {
		return err
	}
	if err := git(ws, "switch", "-q", base); err != nil {
		return err
	}
	return git(ws, "pull", "-q", "--ff-only", "origin", base)
}

func orDetached(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return b
}

// githubTest is the test stage: commit what the run changed, push, and wait for CI. With
// direct it pushes the default branch; otherwise the task branch, opening its pull request.
func githubTest(r *RunFiles, direct bool, deadline time.Time) (string, error) {
	ws, task := r.Snap.Workspace, r.Snap.Task
	var log strings.Builder
	want := taskBranch(task)
	if direct {
		base, err := defaultBranch(ws)
		if err != nil {
			return "", err
		}
		want = base
	}
	if cur := currentBranch(ws); cur != want {
		return "", fmt.Errorf("the workspace is on %s, not %s", orDetached(cur), want)
	}
	_, name, _ := strings.Cut(task, "/")
	if dirty(ws) {
		if err := git(ws, "add", "-A"); err != nil {
			return "", err
		}
		if err := git(ws, "commit", "-q", "-m", name); err != nil {
			return "", err
		}
	}
	push := []string{"push", "-q", "-u", "origin", want}
	if !direct {
		push = []string{"push", "-q", "--force-with-lease", "-u", "origin", want}
	}
	if err := git(ws, push...); err != nil {
		return "", err
	}
	sha, _ := gitOut(ws, "rev-parse", "HEAD")
	if !direct {
		url, err := runGH(ws, "pr", "view", want, "--json", "url", "--jq", ".url")
		if err != nil {
			base, err := defaultBranch(ws)
			if err != nil {
				return "", err
			}
			commits, _ := gitOut(ws, "log", "--format=- %s", "origin/"+base+"..HEAD")
			if _, err := runGH(ws, "pr", "create", "--head", want, "--base", base, "--title", name,
				"--body", "Opened from a starship task.\n\n"+commits); err != nil {
				return "", err
			}
			if url, err = runGH(ws, "pr", "view", want, "--json", "url", "--jq", ".url"); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&log, "pull request: %s\n", url)
	}
	out, err := waitForChecks(ws, sha, deadline)
	log.WriteString(out)
	return log.String(), err
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
			return "", fmt.Errorf("no CI checks appeared for %s; does the repository run CI on pushes and pull requests?", short(sha))
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

// githubCommand runs a built-in github-pr command as a binding: from run.start, or as the
// test stage with a deadline.
func githubCommand(r *RunFiles, args []string, deadline time.Time) (string, error) {
	switch strings.Join(args, " ") {
	case "branch":
		return "", githubBranch(r)
	case "direct-start":
		return "", githubDirectStart(r)
	case "pull-request":
		return githubTest(r, false, deadline)
	case "direct-test":
		return githubTest(r, true, deadline)
	}
	return "", fmt.Errorf("unknown github-pr command: %s", strings.Join(args, " "))
}

// cmdGithubPR implements `ensign github-pr direct <project>/<task>`: the task goes
// straight to the default branch, with no pull request, by replacing @branch and @test
// in its own settings.
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
	set("branch", "ensign github-pr direct-start", false)
	set("test", "ensign github-pr direct-test", true)
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
