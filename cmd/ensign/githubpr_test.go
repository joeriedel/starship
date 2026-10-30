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
	case args[0] == "pr" && args[1] == "view":
		if f.prs[args[2]] {
			return "https://github.com/example/repo/pull/7", nil
		}
		return "", fmt.Errorf("no pull requests found for branch %q", args[2])
	case args[0] == "pr" && args[1] == "create":
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
	if !strings.Contains(why, "Outcome: pass") || !strings.Contains(why, "Pull request: https://github.com/example/repo/pull/7") {
		t.Fatalf("a passing run should end with its pull request: %s", why)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "task/auth-refactor:lib.txt"); got != "two" {
		t.Errorf("the tested commit should be on the task branch, got %q", got)
	}
	checks, created := gh.index("api "), gh.index("pr create --head task/auth-refactor --base main")
	if checks < 0 || created < 0 || created < checks {
		t.Errorf("the pull request should be opened after CI passes, not before: %v", gh.calls)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md"))
	if !strings.Contains(string(data), "Pull request: https://github.com/example/repo/pull/7.") {
		t.Errorf("the progress log should name the pull request:\n%s", data)
	}
}

func TestGithubPRWithLocalTests(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	lead := w.startLead()
	w.codeChange(lead, ws)
	if _, why := w.spawn(lead, "orchestrator", "tester"); !strings.Contains(why, "Outcome: pass") || !strings.Contains(why, "Pull request:") {
		t.Fatalf("local tests, then the pull request: %s", why)
	}
	if gh.index("api ") >= 0 {
		t.Errorf("local tests should not wait on CI: %v", gh.calls)
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
	if gh.index("pr ") >= 0 {
		t.Errorf("no pull request while the tests fail: %v", gh.calls)
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

func (w *world) makeDirect() {
	w.t.Helper()
	var out bytes.Buffer
	wd, _ := os.Getwd()
	os.Chdir(w.bridge)
	defer os.Chdir(wd)
	if err := cmdGithubPR([]string{"direct", "supernova/auth-refactor"}, &out); err != nil {
		w.t.Fatal(err)
	}
}

func TestGithubPRDirect(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.makeDirect()
	c, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if c.Bindings["land"].Command != "ensign github-pr merge" || c.Bindings["test"].Command != "git --version" {
		t.Fatalf("direct should replace only @land: %+v %+v", c.Bindings["land"], c.Bindings["test"])
	}
	lead := w.startLead()
	w.codeChange(lead, ws)
	if _, why := w.spawn(lead, "orchestrator", "tester"); !strings.Contains(why, "Outcome: pass") || !strings.Contains(why, "Pushed to main:") {
		t.Fatalf("a direct run should end on main: %s", why)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the tested commit should be on main, got %q", got)
	}
	if gh.index("pr ") >= 0 {
		t.Errorf("a direct task opens no pull request: %v", gh.calls)
	}
	if b := currentBranch(ws); b != "main" {
		t.Errorf("the workspace should be back on main, it is on %q", b)
	}
}

func TestGithubPRDirectRefusesWhenMainMoved(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	w.makeDirect()
	lead := w.startLead()
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	write(t, filepath.Join(other, "news.txt"), "meanwhile\n")
	runGit(t, other, "add", "news.txt")
	runGit(t, other, "commit", "-q", "-m", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
	w.codeChange(lead, ws)
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "has moved since this task branched") {
		t.Fatalf("main moved, so the tested commit can't go there: %s", why)
	}
	if st := w.state(); st.Status != "error" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
}
