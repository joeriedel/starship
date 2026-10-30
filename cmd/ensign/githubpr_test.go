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

// githubWorld is a world whose project uses github-pr, with a git workspace whose origin
// is a local bare repository.
func githubWorld(t *testing.T, conclusion string) (*world, string, string, *fakeGH) {
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
	s := strings.Replace(string(data), `  build: { command: "./build.sh" }`, `  build: { not-applicable: "CI builds it" }`, 1)
	s = strings.Replace(s, `  test:  { command: "./test.sh" }`+"\n", "", 1)
	write(t, p, "use: [github-pr]\n"+s)

	gh := &fakeGH{prs: map[string]bool{}, conclusion: conclusion}
	oldGH, oldPoll := runGH, checkPoll
	runGH, checkPoll = gh.run, time.Millisecond
	t.Cleanup(func() { runGH, checkPoll = oldGH, oldPoll })
	return w, ws, remote, gh
}

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

func TestGithubPRBranchAndPullRequest(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success")
	lead := w.startLead()
	if b := currentBranch(ws); b != "task/auth-refactor" {
		t.Fatalf("run.start should move the workspace to the task branch, it is on %q", b)
	}
	w.codeChange(lead, ws)
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: pass") || !strings.Contains(why, "pull/7") {
		t.Fatalf("the test stage should open the pull request and pass on green checks: %s", why)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s (%s)", st.Status, st.Reason)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "task/auth-refactor:lib.txt"); got != "two" {
		t.Errorf("the change should be pushed to the task branch, got %q", got)
	}
	created := 0
	for _, c := range gh.calls {
		if strings.HasPrefix(c, "pr create --head task/auth-refactor --base main") {
			created++
		}
	}
	if created != 1 {
		t.Errorf("one pull request should be opened against main: %v", gh.calls)
	}
}

func TestGithubPRFailingChecksGoBackToCode(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "failure")
	lead := w.startLead()
	w.codeChange(lead, ws)
	_, why := w.spawn(lead, "orchestrator", "tester")
	if !strings.Contains(why, "Outcome: fail") || !strings.Contains(why, "TestLogin") || !strings.Contains(why, `stage "code"`) {
		t.Fatalf("failing checks should quote the log and send the run back to code: %s", why)
	}
}

func TestGithubPRWontStartOnSomeoneElsesWork(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success")
	write(t, filepath.Join(ws, "lib.txt"), "half done\n")
	w.run("supernova/auth-refactor")
	w.approve()
	if lead, why := w.spawn("", "", "orchestrator"); lead != "" || !strings.Contains(why, "uncommitted changes on main") {
		t.Fatalf("a run should not start over uncommitted work on another branch: %s", why)
	}
}

func TestGithubPRDirect(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success")
	runGit(t, ws, "switch", "-q", "-c", "elsewhere")
	var out bytes.Buffer
	wd, _ := os.Getwd()
	os.Chdir(w.bridge)
	err := cmdGithubPR([]string{"direct", "supernova/auth-refactor"}, &out)
	os.Chdir(wd)
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if c.Bindings["branch"].Command != "ensign github-pr direct-start" || !c.Bindings["test"].Plain {
		t.Fatalf("the task's settings should replace @branch and @test: %+v %+v", c.Bindings["branch"], c.Bindings["test"])
	}
	lead := w.startLead()
	if b := currentBranch(ws); b != "main" {
		t.Fatalf("a direct run should start on main, it is on %q", b)
	}
	w.codeChange(lead, ws)
	if _, why := w.spawn(lead, "orchestrator", "tester"); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("direct test stage: %s", why)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the change should be pushed to main, got %q", got)
	}
	for _, call := range gh.calls {
		if strings.HasPrefix(call, "pr ") {
			t.Errorf("a direct task opens no pull request: %s", call)
		}
	}
}
