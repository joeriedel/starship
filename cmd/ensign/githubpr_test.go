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
	why, _ := w.runStage()
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
	if d, _ := dirty(ws); !d {
		t.Fatal("nothing should be committed until the test stage is about to start")
	}
	why, _ := w.runStage()
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
	why, _ := w.runStage()
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

// staleBranch makes task/auth-refactor with one commit on lib.txt, then moves origin's main
// on by one commit that writes file with content. The workspace ends on main.
func staleBranch(t *testing.T, ws, remote, file, content string) string {
	t.Helper()
	t.Setenv("GIT_COMMITTER_NAME", "t") // the rebase under test commits
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	runGit(t, ws, "switch", "-q", "-c", "task/auth-refactor")
	write(t, filepath.Join(ws, "lib.txt"), "mine\n")
	runGit(t, ws, "commit", "-qam", "mine")
	runGit(t, ws, "switch", "-q", "main")
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	write(t, filepath.Join(other, file), content)
	runGit(t, other, "add", file)
	runGit(t, other, "commit", "-q", "-m", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
	return runGit(t, ws, "rev-parse", "task/auth-refactor")
}

func TestGithubPRRefusesStaleBranch(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	staleBranch(t, ws, remote, "news.txt", "meanwhile\n")
	w.run("supernova/auth-refactor")
	w.approve()
	if lead, why := w.spawn("", "", "orchestrator"); lead != "" || !strings.Contains(why, "task branch task/auth-refactor is behind main") || !strings.Contains(why, "--stale-branch") {
		t.Fatalf("a run should not start on a branch behind main: %q %s", lead, why)
	}
}

func TestGithubPRStaleBranchOverride(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	staleBranch(t, ws, remote, "news.txt", "meanwhile\n")
	w.run("supernova/auth-refactor", "--stale-branch")
	w.approve()
	if lead, why := w.spawn("", "", "orchestrator"); lead == "" {
		t.Fatalf("--stale-branch should let the run start: %s", why)
	}
	if b := currentBranch(ws); b != "task/auth-refactor" {
		t.Errorf("the run should be on the task branch, it is on %q", b)
	}
}

func TestGithubPRUpdateRebases(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	old := staleBranch(t, ws, remote, "news.txt", "meanwhile\n")
	out, err := w.land("update")
	if err != nil || !strings.Contains(out, "Rebased task/auth-refactor onto origin/main:") {
		t.Fatalf("update: %q %v", out, err)
	}
	if now := runGit(t, ws, "rev-parse", "task/auth-refactor"); now == old {
		t.Error("the branch should have moved")
	}
	if b := currentBranch(ws); b != "main" {
		t.Errorf("the workspace should be back on main, it is on %q", b)
	}
	if out, err := w.land("update"); err != nil || !strings.Contains(out, "up to date") {
		t.Errorf("a second update has nothing to do: %q %v", out, err)
	}
	w.run("supernova/auth-refactor")
	w.approve()
	if lead, why := w.spawn("", "", "orchestrator"); lead == "" {
		t.Fatalf("the updated branch should start: %s", why)
	}
}

func TestGithubPRUpdateConflict(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	old := staleBranch(t, ws, remote, "lib.txt", "theirs\n")
	_, err := w.land("update")
	if err == nil || !strings.Contains(err.Error(), "lib.txt") || !strings.Contains(err.Error(), "left as it was") {
		t.Fatalf("a conflict should name the file: %v", err)
	}
	if now := runGit(t, ws, "rev-parse", "task/auth-refactor"); now != old {
		t.Errorf("the branch moved from %s to %s", old, now)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "rebase-merge")); err == nil {
		t.Error("a rebase is still in progress")
	}
	if b := currentBranch(ws); b != "main" {
		t.Errorf("the workspace should be on main, it is on %q", b)
	}
}

func TestGithubPRUpdateConflictAwkwardNames(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"old nova.txt", "old nova.txt"},
		{"a,b.txt", `"a,b.txt"`},
	} {
		w, ws, remote, _ := githubWorld(t, "success", localTest)
		write(t, filepath.Join(ws, c.file), "base\n")
		runGit(t, ws, "add", c.file)
		runGit(t, ws, "commit", "-qm", "base")
		runGit(t, ws, "push", "-q", "origin", "main")
		write(t, filepath.Join(ws, c.file), "mine\n") // staleBranch commits tracked changes on the task branch
		staleBranch(t, ws, remote, c.file, "theirs\n")
		_, err := w.land("update")
		if want := "conflicts in: " + c.want + ". The branch was left as it was"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want it to contain %q", c.file, err, want)
		}
	}
}

// emptyBranch makes task/auth-refactor with no commits of its own, then moves origin's main on by one commit that writes file with content. The workspace ends on main, or on the task branch if onTask. It returns the branch's tip.
func emptyBranch(t *testing.T, ws, remote, file, content string, onTask bool) string {
	t.Helper()
	runGit(t, ws, "branch", "task/auth-refactor")
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	write(t, filepath.Join(other, file), content)
	runGit(t, other, "add", file)
	runGit(t, other, "commit", "-q", "-m", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
	if onTask {
		runGit(t, ws, "switch", "-q", "task/auth-refactor")
	}
	return runGit(t, ws, "rev-parse", "task/auth-refactor")
}

// startEmpty starts a run and returns the lead (empty if refused) and the reason.
func startEmpty(w *world, args ...string) (string, string) {
	w.run(append([]string{"supernova/auth-refactor"}, args...)...)
	w.approve()
	return w.spawn("", "", "orchestrator")
}

func TestGithubPRFastForwardsEmptyBranch(t *testing.T) {
	for _, onTask := range []bool{false, true} {
		w, ws, remote, _ := githubWorld(t, "success", localTest)
		emptyBranch(t, ws, remote, "news.txt", "meanwhile\n", onTask)
		if lead, why := startEmpty(w); lead == "" {
			t.Fatalf("an empty branch should be fast-forwarded (on task: %v): %s", onTask, why)
		}
		if b := currentBranch(ws); b != "task/auth-refactor" {
			t.Errorf("the run should be on the task branch, it is on %q", b)
		}
		if tip, main := runGit(t, ws, "rev-parse", "task/auth-refactor"), runGit(t, ws, "rev-parse", "origin/main"); tip != main {
			t.Errorf("the branch should be at origin/main (on task: %v): %s != %s", onTask, tip, main)
		}
	}
}

func TestGithubPRStaleBranchOverrideKeepsEmptyBranch(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	old := emptyBranch(t, ws, remote, "news.txt", "meanwhile\n", false)
	if lead, why := startEmpty(w, "--stale-branch"); lead == "" {
		t.Fatalf("--stale-branch should let the run start: %s", why)
	}
	if tip := runGit(t, ws, "rev-parse", "task/auth-refactor"); tip != old {
		t.Errorf("--stale-branch takes the branch as it is, but it moved: %s != %s", tip, old)
	}
}

func TestGithubPREmptyBranchNotMovedOverDirtyOtherBranch(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	old := emptyBranch(t, ws, remote, "news.txt", "meanwhile\n", false)
	write(t, filepath.Join(ws, "lib.txt"), "half done\n")
	if lead, why := startEmpty(w); lead != "" || !strings.Contains(why, "uncommitted changes on main") {
		t.Fatalf("a run should not start over uncommitted work on another branch: %q %s", lead, why)
	}
	if tip := runGit(t, ws, "rev-parse", "task/auth-refactor"); tip != old {
		t.Errorf("the branch should not move: %s != %s", tip, old)
	}
}

func TestGithubPRFastForwardKeepsUncommittedWork(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	emptyBranch(t, ws, remote, "news.txt", "meanwhile\n", true)
	write(t, filepath.Join(ws, "lib.txt"), "half done\n")
	if lead, why := startEmpty(w); lead == "" {
		t.Fatalf("uncommitted work that doesn't overlap should not stop the fast-forward: %s", why)
	}
	if tip, main := runGit(t, ws, "rev-parse", "task/auth-refactor"), runGit(t, ws, "rev-parse", "origin/main"); tip != main {
		t.Errorf("the branch should be at origin/main: %s != %s", tip, main)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "lib.txt")); string(got) != "half done\n" {
		t.Errorf("the uncommitted change should survive: %q", got)
	}
}

func TestGithubPRFastForwardRefusesOverlappingWork(t *testing.T) {
	for _, autoStash := range []string{"", "true"} {
		t.Run("autoStash="+autoStash, func(t *testing.T) {
			w, ws, remote, _ := githubWorld(t, "success", localTest)
			if autoStash != "" {
				runGit(t, ws, "config", "merge.autoStash", autoStash)
			}
			old := emptyBranch(t, ws, remote, "lib.txt", "upstream\n", true)
			write(t, filepath.Join(ws, "lib.txt"), "half done\n")
			if lead, why := startEmpty(w); lead != "" || !strings.Contains(why, "task/auth-refactor couldn't be fast-forwarded to origin/main") {
				t.Fatalf("overlapping uncommitted work should refuse the run: %q %s", lead, why)
			}
			if tip := runGit(t, ws, "rev-parse", "task/auth-refactor"); tip != old {
				t.Errorf("the branch should not move: %s != %s", tip, old)
			}
			if got, _ := os.ReadFile(filepath.Join(ws, "lib.txt")); string(got) != "half done\n" {
				t.Errorf("the uncommitted change should survive: %q", got)
			}
			if stash := runGit(t, ws, "stash", "list"); stash != "" {
				t.Errorf("nothing should be stashed: %q", stash)
			}
		})
	}
}
