// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGH stands in for gh: it keeps which pull requests exist and answers every check
// with one conclusion.
type fakeGH struct {
	calls      []string
	prs        map[string]bool
	conclusion string
}

func (f *fakeGH) run(dir string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[0] == "pr" && args[1] == "list":
		if f.prs[args[3]] {
			return "https://github.com/example/repo/pull/7", nil
		}
		return "", nil // a closed one would not be listed
	case args[0] == "pr" && args[1] == "create":
		if f.prs[args[3]] {
			return "", fmt.Errorf("a pull request for branch %q already exists", args[3])
		}
		for i, a := range args {
			if a == "--head" {
				f.prs[args[i+1]] = true
			}
		}
		return "", nil
	case args[0] == "api":
		return `[{"name":"test (ubuntu-latest)","status":"completed","conclusion":"` + f.conclusion + `"}]`, nil
	case args[0] == "run" && args[1] == "list":
		return "42", nil
	case args[0] == "run" && args[1] == "view":
		return "--- FAIL: TestLogin\n    login_test.go:9: trailing space rejected", nil
	}
	return "", fmt.Errorf("unexpected gh %v", args)
}

// githubWorld is a world whose project uses github-pr with the given @test binding, and
// a git workspace whose origin is a local bare repository.
func githubWorld(t *testing.T, conclusion, test string) (*world, string, string, *fakeGH) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	w := newWorld(t)
	ws := w.gitWorkspace()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "init", "-q", "--bare", "-b", "main", remote)
	runGit(t, ws, "remote", "add", "origin", remote)
	runGit(t, ws, "push", "-q", "-u", "origin", "main")
	runGit(t, ws, "remote", "set-head", "origin", "main")

	p := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(p)
	s := strings.Replace(string(data), `  build: { command: "./build.sh" }`, `  build: { not-applicable: "none here" }`, 1)
	s = strings.Replace(s, `  test:  { command: "./test.sh" }`, `  test:  `+test, 1)
	write(t, p, "use: [github-pr]\n"+s)

	gh := &fakeGH{prs: map[string]bool{}, conclusion: conclusion}
	oldGH, oldPoll := runGH, checkPoll
	runGH, checkPoll = gh.run, time.Millisecond
	t.Cleanup(func() { runGH, checkPoll = oldGH, oldPoll })
	return w, ws, remote, gh
}

const (
	ciTest    = `{ command: "ensign github-pr ci", plain: true }`
	localTest = `{ command: "git --version", plain: true }` // passes anywhere git is
)

// codeChange runs a coder pass that changes the workspace, then review and style.
func (w *world) codeChange(lead, ws string) {
	w.t.Helper()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		w.t.Fatalf("coder refused: %s", why)
	}
	write(w.t, filepath.Join(ws, "lib.txt"), "two\n")
	w.stop(coder, "coder", report("compiled"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "style-reviewer", "done")
}

func (gh *fakeGH) index(prefix string) int {
	for i, c := range gh.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// land runs one of the steps the user asks for after the run.
func (w *world) land(step string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	wd, _ := os.Getwd()
	os.Chdir(w.bridge)
	defer os.Chdir(wd)
	err := cmdGithubPR([]string{step, "supernova/auth-refactor"}, &out)
	return out.String(), err
}

// passingRun runs the task to success and checks nothing was landed by it.
func (w *world) passingRun(ws string, gh *fakeGH) {
	w.t.Helper()
	lead := w.startLead()
	w.codeChange(lead, ws)
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: pass") || !strings.Contains(why, "committed on task/auth-refactor") {
		w.t.Fatalf("a passing run should end with its work waiting on the task branch: %s", why)
	}
	if st := w.state(); st.Status != "success" {
		w.t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if gh.index("pr ") >= 0 {
		w.t.Fatalf("the run should open no pull request; that's the user's step: %v", gh.calls)
	}
}

func TestGithubPRWithCIAsTheTests(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", ciTest)
	lead := w.startLead()
	if b := currentBranch(ws); b != "task/auth-refactor" {
		t.Fatalf("run.start should move the workspace to the task branch, it is on %q", b)
	}
	w.codeChange(lead, ws)
	if !dirty(ws) {
		t.Fatal("nothing should be committed until the test stage is about to start")
	}
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: pass") || gh.index("api ") < 0 {
		t.Fatalf("CI should be the tests: %s %v", why, gh.calls)
	}
	if gh.index("pr ") >= 0 {
		t.Errorf("the run should open no pull request: %v", gh.calls)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "task/auth-refactor:lib.txt"); got != "two" {
		t.Errorf("CI tests the pushed task branch, got %q", got)
	}
	out, err := w.land("pull-request")
	if err != nil || !strings.Contains(out, "Pull request: https://github.com/example/repo/pull/7") {
		t.Fatalf("asked for, the pull request opens: %q %v", out, err)
	}
	if gh.index("pr create --head task/auth-refactor --base main") < 0 {
		t.Errorf("no pull request created: %v", gh.calls)
	}
	if out, err := w.land("pull-request"); err != nil || !strings.Contains(out, "pull/7") {
		t.Errorf("asking again finds the open one: %q %v", out, err)
	}
}

func TestGithubPRWithLocalTests(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	if gh.index("api ") >= 0 {
		t.Errorf("local tests should not wait on CI: %v", gh.calls)
	}
	if out := runGit(t, ".", "--git-dir", remote, "branch", "--list", "task/*"); out != "" {
		t.Errorf("with local tests nothing is pushed until the user asks: %q", out)
	}
	if _, err := w.land("pull-request"); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "task/auth-refactor:lib.txt"); got != "two" {
		t.Errorf("opening the pull request should push the branch, got %q", got)
	}
}

func TestGithubPRFailingChecksGoBackToCode(t *testing.T) {
	w, ws, _, gh := githubWorld(t, "failure", ciTest)
	lead := w.startLead()
	w.codeChange(lead, ws)
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: fail") || !strings.Contains(why, "--- FAIL: TestLogin\n") || !strings.Contains(why, `stage "code"`) {
		t.Fatalf("failing checks should quote the log and send the run back to code: %s", why)
	}
	if _, err := w.land("pull-request"); err == nil || !strings.Contains(err.Error(), "still going") {
		t.Errorf("the run is back in code, so nothing can land: %v", err)
	}
	if gh.index("pr ") >= 0 {
		t.Errorf("no pull request while the tests fail: %v", gh.calls)
	}
}

func TestGithubPRWontLandUntestedChanges(t *testing.T) {
	w, ws, _, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	write(t, filepath.Join(ws, "lib.txt"), "three\n")
	if _, err := w.land("pull-request"); err == nil || !strings.Contains(err.Error(), "never tested") {
		t.Errorf("uncommitted changes weren't tested: %v", err)
	}
	runGit(t, ws, "commit", "-qam", "later")
	if _, err := w.land("merge"); err == nil || !strings.Contains(err.Error(), "changed since its tests passed") {
		t.Errorf("a later commit wasn't tested: %v", err)
	}
}

func TestGithubPRWontStartOnSomeoneElsesWork(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	write(t, filepath.Join(ws, "lib.txt"), "half done\n")
	w.run("supernova/auth-refactor")
	w.approve()
	if lead, why := w.spawn("", "", "orchestrator"); lead != "" || !strings.Contains(why, "uncommitted changes on main") {
		t.Fatalf("a run should not start over uncommitted work on another branch: %s", why)
	}
}

func TestGithubPRMerge(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got == "two" {
		t.Fatal("main should not move until the user asks")
	}
	out, err := w.land("merge")
	if err != nil || !strings.Contains(out, "Pushed to main:") {
		t.Fatalf("merge: %q %v", out, err)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the tested commit should be on main, got %q", got)
	}
	if gh.index("pr ") >= 0 {
		t.Errorf("a merge opens no pull request: %v", gh.calls)
	}
	if b := currentBranch(ws); b != "main" {
		t.Errorf("the workspace should be back on main, it is on %q", b)
	}
	if _, err := w.land("merge"); err == nil {
		t.Error("a task landed once has nothing left to land")
	}
}

func TestGithubPRMergeRefusesWhenMainMoved(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	write(t, filepath.Join(other, "news.txt"), "meanwhile\n")
	runGit(t, other, "add", "news.txt")
	runGit(t, other, "commit", "-q", "-m", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
	if _, err := w.land("merge"); err == nil || !strings.Contains(err.Error(), "has moved since this task branched") {
		t.Fatalf("main moved, so the tested commit can't go there: %v", err)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:news.txt"); got != "meanwhile" {
		t.Errorf("main should be untouched, got %q", got)
	}
}
