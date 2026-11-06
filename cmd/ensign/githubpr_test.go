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

func TestGithubPRUpdateConflictOnAGitlinkHiddenByConfig(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	t.Setenv("GIT_COMMITTER_NAME", "t") // the rebase under test commits
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	link := func(dir, sha string) {
		runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+sha+",lib")
	}
	link(ws, strings.Repeat("1", 40))
	runGit(t, ws, "commit", "-qm", "link")
	runGit(t, ws, "push", "-q", "origin", "main")
	runGit(t, ws, "switch", "-q", "-c", "task/auth-refactor")
	link(ws, strings.Repeat("2", 40))
	write(t, filepath.Join(ws, "mine.txt"), "mine\n")
	runGit(t, ws, "add", "mine.txt")
	runGit(t, ws, "commit", "-qm", "mine")
	runGit(t, ws, "switch", "-q", "main")
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, ".", "clone", "-q", remote, other)
	link(other, strings.Repeat("3", 40))
	write(t, filepath.Join(other, "theirs.txt"), "theirs\n")
	runGit(t, other, "add", "theirs.txt")
	runGit(t, other, "commit", "-qm", "meanwhile")
	runGit(t, other, "push", "-q", "origin", "main")
	runGit(t, ws, "config", "diff.ignoreSubmodules", "all")
	_, err := w.land("update")
	if err == nil || !strings.Contains(err.Error(), "conflicts in: lib. ") {
		t.Fatalf("a conflict on a gitlink should be named even when config ignores submodules: %v", err)
	}
}

// subfolderWorkspace points the project's workspace at a tracked folder inside the repository at ws.
func subfolderWorkspace(t *testing.T, w *world, ws string) {
	t.Helper()
	write(t, filepath.Join(ws, "sub", "code.txt"), "one\n")
	runGit(t, ws, "add", "sub")
	runGit(t, ws, "commit", "-qm", "sub")
	runGit(t, ws, "push", "-q", "origin", "main")
	if _, err := setWorkspace(w.bridge, "supernova", filepath.Join(ws, "sub")); err != nil {
		t.Fatal(err)
	}
}

func TestGithubPRUpdateConflictOutsideTheWorkspaceUnderDiffRelative(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	runGit(t, ws, "config", "diff.relative", "true")
	subfolderWorkspace(t, w, ws)
	staleBranch(t, ws, remote, "lib.txt", "theirs\n")
	_, err := w.land("update")
	if err == nil || !strings.Contains(err.Error(), "conflicts in: lib.txt") {
		t.Fatalf("a conflict outside the workspace folder should be named: %v", err)
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

func TestLeaveLandedBranchKeepsEditsWithAutoStash(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ws := t.TempDir()
	runGit(t, ws, "init", "-q", "-b", "main")
	runGit(t, ws, "config", "merge.autoStash", "true")
	a := filepath.Join(ws, "a")
	write(t, a, "old\n")
	runGit(t, ws, "add", "a")
	runGit(t, ws, "commit", "-q", "-m", "base")
	oldBase := runGit(t, ws, "rev-parse", "HEAD")
	runGit(t, ws, "switch", "-q", "-c", "task/auth-refactor")
	write(t, a, "new\n")
	runGit(t, ws, "commit", "-qam", "task")
	sha := runGit(t, ws, "rev-parse", "HEAD")
	write(t, a, "old\n")
	runGit(t, ws, "add", "a")
	write(t, a, "old\nhalf done\n")

	leaveLandedBranch(ws, "task/auth-refactor", "main", sha)

	if got, _ := os.ReadFile(a); string(got) != "old\nhalf done\n" {
		t.Errorf("the unstaged edit should be kept without conflict markers: %q", got)
	}
	if stash := runGit(t, ws, "stash", "list"); stash != "" {
		t.Errorf("nothing should be stashed: %q", stash)
	}
	if head := runGit(t, ws, "rev-parse", "HEAD"); head != oldBase {
		t.Errorf("HEAD should stay at the old base: %s != %s", head, oldBase)
	}
}

// remoteTaskCommit adds a commit to origin's task branch from another clone, creating the branch from main if origin has none. It returns the new tip.
func remoteTaskCommit(t *testing.T, remote, file string) string {
	t.Helper()
	other := cloneRemote(t, remote)
	if runGit(t, ".", "--git-dir", remote, "branch", "--list", "task/*") == "" {
		runGit(t, other, "switch", "-q", "-c", "task/auth-refactor")
	} else {
		runGit(t, other, "switch", "-q", "task/auth-refactor")
	}
	write(t, filepath.Join(other, file), "theirs\n")
	runGit(t, other, "add", file)
	runGit(t, other, "commit", "-q", "-m", "theirs")
	runGit(t, other, "push", "-q", "origin", "task/auth-refactor")
	return runGit(t, other, "rev-parse", "HEAD")
}

// localTaskCommit adds a commit to the workspace's task branch, creating it from main, and leaves the workspace on main.
func localTaskCommit(t *testing.T, ws, file string) {
	t.Helper()
	if runGit(t, ws, "branch", "--list", "task/auth-refactor") == "" {
		runGit(t, ws, "switch", "-q", "-c", "task/auth-refactor")
	} else {
		runGit(t, ws, "switch", "-q", "task/auth-refactor")
	}
	write(t, filepath.Join(ws, file), "mine\n")
	runGit(t, ws, "add", file)
	runGit(t, ws, "commit", "-q", "-m", "mine")
	runGit(t, ws, "switch", "-q", "main")
}

func TestGithubPRRefusesRemoteCopyWeLack(t *testing.T) {
	cases := map[string]func(t *testing.T, ws, remote string){
		"local behind": func(t *testing.T, ws, remote string) {
			localTaskCommit(t, ws, "mine.txt")
			runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
			remoteTaskCommit(t, remote, "theirs.txt")
		},
		"diverged": func(t *testing.T, ws, remote string) {
			localTaskCommit(t, ws, "mine.txt")
			runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
			remoteTaskCommit(t, remote, "theirs.txt")
			localTaskCommit(t, ws, "more.txt")
		},
		"no local branch, current tracking ref": func(t *testing.T, ws, remote string) {
			remoteTaskCommit(t, remote, "theirs.txt")
			runGit(t, ws, "fetch", "-q", "origin")
		},
		"no local branch, no tracking ref": func(t *testing.T, ws, remote string) {
			remoteTaskCommit(t, remote, "theirs.txt")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w, ws, remote, _ := githubWorld(t, "success", localTest)
			setup(t, ws, remote)
			hadLocal := runGit(t, ws, "branch", "--list", "task/auth-refactor") != ""
			tracking := func() string {
				got, _ := gitOut(ws, "rev-parse", "-q", "--verify", "refs/remotes/origin/task/auth-refactor")
				return got
			}
			before := tracking()
			lead, why := startEmpty(w)
			if lead != "" || !strings.Contains(why, "task branch task/auth-refactor") || !strings.Contains(why, "origin") {
				t.Fatalf("the run should be refused naming the branch: %q %s", lead, why)
			}
			if b := currentBranch(ws); b != "main" {
				t.Errorf("the workspace should stay on main, it is on %q", b)
			}
			if has := runGit(t, ws, "branch", "--list", "task/auth-refactor") != ""; has != hadLocal {
				t.Errorf("the local task branch must not be created or removed (had %v, has %v)", hadLocal, has)
			}
			if after := tracking(); after != before {
				t.Errorf("a refused run must leave the tracking ref alone: %q != %q", after, before)
			}
		})
	}
}

func TestGithubPRAllowsRemoteCopyWeHave(t *testing.T) {
	cases := map[string]func(t *testing.T, ws, remote string){
		"equal": func(t *testing.T, ws, remote string) {
			localTaskCommit(t, ws, "mine.txt")
			runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
		},
		"remote is an ancestor": func(t *testing.T, ws, remote string) {
			localTaskCommit(t, ws, "mine.txt")
			runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
			localTaskCommit(t, ws, "more.txt")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w, ws, remote, _ := githubWorld(t, "success", localTest)
			setup(t, ws, remote)
			if lead, why := startEmpty(w); lead == "" {
				t.Fatalf("a remote copy we contain should not refuse the run: %s", why)
			}
		})
	}
}

func TestGithubPRAllowsPreRebaseRemoteCopy(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	staleBranch(t, ws, remote, "news.txt", "meanwhile\n")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	old := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor")
	if out, err := w.land("update"); err != nil {
		t.Fatalf("update: %q %v", out, err)
	}
	lead := w.startLead()
	w.codeChange(lead, ws)
	if why, _ := w.runStage(); !strings.Contains(why, "Outcome: pass") {
		t.Fatalf("the rebased branch should run: %s", why)
	}
	if _, err := w.land("pull-request"); err != nil {
		t.Fatalf("the push after a rebase should succeed: %v", err)
	}
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip == old || tip != runGit(t, ws, "rev-parse", "task/auth-refactor") {
		t.Errorf("origin should hold the rebased branch, got %s (was %s)", tip, old)
	}
}

func TestGithubPRStaleBranchOverStartsOverRemoteCopy(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	remoteTaskCommit(t, remote, "theirs.txt")
	if lead, why := startEmpty(w, "--stale-branch"); lead == "" {
		t.Fatalf("--stale-branch should take the branch as it is: %s", why)
	}
}

func TestGithubPRCIPushKeepsCommitsThatArrivedDuringTheRun(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", ciTest)
	lead := w.startLead()
	w.codeChange(lead, ws)
	theirs := remoteTaskCommit(t, remote, "theirs.txt")
	runGit(t, ws, "fetch", "-q", "origin")
	if why, _ := w.runStage(); strings.Contains(why, "Outcome: pass") || gh.index("api ") >= 0 {
		t.Errorf("the push should have been refused: %s %v", why, gh.calls)
	}
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip != theirs {
		t.Errorf("origin's commit was overwritten: %s != %s", tip, theirs)
	}
}

func TestGithubPRPullRequestPushKeepsCommitsThatArrivedDuringTheRun(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	theirs := remoteTaskCommit(t, remote, "theirs.txt")
	runGit(t, ws, "fetch", "-q", "origin")
	if _, err := w.land("pull-request"); err == nil {
		t.Error("the push should have been refused")
	}
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip != theirs {
		t.Errorf("origin's commit was overwritten: %s != %s", tip, theirs)
	}
}

func TestGithubPRMergeLeavesCommitsWeLack(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	w.passingRun(ws, gh)
	theirs := remoteTaskCommit(t, remote, "theirs.txt")
	out, err := w.land("merge")
	if err != nil || !strings.Contains(out, "Pushed to main:") || !strings.Contains(out, "task/auth-refactor was left on origin") || !strings.Contains(out, "reflog") {
		t.Fatalf("the landing should succeed and name the branch left behind: %q %v", out, err)
	}
	if got := runGit(t, ".", "--git-dir", remote, "show", "main:lib.txt"); got != "two" {
		t.Errorf("the tested commit should be on main, got %q", got)
	}
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip != theirs {
		t.Errorf("origin's commit was deleted: %s != %s", tip, theirs)
	}
}

func TestGithubPRMergeNoteMentionsReflogWhenPushedCommitsCantBeShown(t *testing.T) {
	_, ws, remote, _ := githubWorld(t, "success", localTest)
	first := rewrittenWithoutReflog(t, ws)
	note := deleteLandedRemote(ws, "task/auth-refactor")
	if !strings.Contains(note, "left on origin") || !strings.Contains(note, "reflog") {
		t.Errorf("the note should say the branch was left and that the reflog may be the cause: %q", note)
	}
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip != first {
		t.Errorf("origin's branch should be untouched: %s != %s", tip, first)
	}
}

func TestGithubPRMergeDeletesPushedBranch(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", ciTest)
	w.passingRun(ws, gh)
	if out := runGit(t, ".", "--git-dir", remote, "branch", "--list", "task/*"); out == "" {
		t.Fatal("CI should have pushed the branch")
	}
	out, err := w.land("merge")
	if err != nil || strings.Contains(out, "left on origin") {
		t.Fatalf("merge: %q %v", out, err)
	}
	if out := runGit(t, ".", "--git-dir", remote, "branch", "--list", "task/*"); out != "" {
		t.Errorf("the landed branch should be gone from origin: %q", out)
	}
}

func TestGithubPRAllowsRemoteBranchAlreadyInMain(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", localTest)
	runGit(t, cloneRemote(t, remote), "push", "-q", "origin", "main:refs/heads/task/auth-refactor")
	w.passingRun(ws, gh)
	if _, err := w.land("pull-request"); err != nil {
		t.Fatalf("the push over a remote branch already in main should succeed: %v", err)
	}
}

func TestGithubPRAllowsHeldRemoteTipBeyondTrackingRef(t *testing.T) {
	w, ws, _, gh := githubWorld(t, "success", localTest)
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	localTaskCommit(t, ws, "more.txt")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	runGit(t, ws, "update-ref", "refs/remotes/origin/task/auth-refactor", "main")
	w.passingRun(ws, gh)
	if _, err := w.land("pull-request"); err != nil {
		t.Fatalf("the push should succeed with a held tip and an older tracking ref: %v", err)
	}
}

func TestGithubPRRefusesReflogOnlyRemoteTipBehindTrackingRef(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	first := runGit(t, ws, "rev-parse", "task/auth-refactor")
	runGit(t, ws, "switch", "-q", "task/auth-refactor")
	runGit(t, ws, "commit", "-q", "--amend", "-m", "rewritten")
	runGit(t, ws, "switch", "-q", "main")
	runGit(t, ws, "push", "-q", "--force", "origin", "task/auth-refactor")
	second := runGit(t, ws, "rev-parse", "refs/remotes/origin/task/auth-refactor")
	runGit(t, cloneRemote(t, remote), "push", "-q", "--force", "origin", first+":refs/heads/task/auth-refactor")
	lead, why := startEmpty(w)
	if lead != "" || !strings.Contains(why, "task/auth-refactor") {
		t.Fatalf("a tip held only in the reflog, with the tracking ref elsewhere, should refuse the run naming the branch: %q %q", lead, why)
	}
	if got := runGit(t, ws, "rev-parse", "refs/remotes/origin/task/auth-refactor"); got != second {
		t.Errorf("the tracking ref moved to %s, it was %s", got, second)
	}
}

func TestGithubPRRefusesReflogOnlyRemoteTipRefetchedOntoTrackingRef(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	first := runGit(t, ws, "rev-parse", "task/auth-refactor")
	runGit(t, ws, "switch", "-q", "task/auth-refactor")
	runGit(t, ws, "commit", "-q", "--amend", "-m", "rewritten")
	runGit(t, ws, "switch", "-q", "main")
	runGit(t, ws, "push", "-q", "--force", "origin", "task/auth-refactor")
	runGit(t, cloneRemote(t, remote), "push", "-q", "--force", "origin", first+":refs/heads/task/auth-refactor")
	fetch := exec.Command("git", "-C", ws, "fetch", "-q", "origin")
	fetch.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+time.Now().Add(time.Hour).Format(time.RFC3339))
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	if got := runGit(t, ws, "rev-parse", "refs/remotes/origin/task/auth-refactor"); got != first {
		t.Fatalf("the fetch should put the tracking ref on %s, it is at %s", first, got)
	}
	lead, why := startEmpty(w)
	if lead != "" || !strings.Contains(why, "task/auth-refactor") {
		t.Fatalf("a reflog-only tip that git would reject as updated since checkout should refuse the run naming the branch: %q %q", lead, why)
	}
}

func TestGithubPRFastForwardsEmptyBranchWhoseRemoteCopyIsInMain(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	emptyBranch(t, ws, remote, "news.txt", "meanwhile\n", false)
	other := cloneRemote(t, remote)
	runGit(t, other, "push", "-q", "origin", "main:refs/heads/task/auth-refactor")
	tip := runGit(t, other, "rev-parse", "HEAD")
	if lead, why := startEmpty(w); lead == "" {
		t.Fatalf("a remote copy already in main loses nothing: %s", why)
	}
	if got := runGit(t, ws, "rev-parse", "task/auth-refactor"); got != tip {
		t.Errorf("the branch should be fast-forwarded to %s, it is at %s", tip, got)
	}
	if got := runGit(t, ws, "rev-parse", "refs/remotes/origin/task/auth-refactor"); got != tip {
		t.Errorf("the tracking ref should be at origin's tip %s, it is at %s", tip, got)
	}
}

func TestGithubPRDropsStaleTrackingRef(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	runGit(t, cloneRemote(t, remote), "push", "-q", "origin", "--delete", "task/auth-refactor")
	if lead, why := startEmpty(w); lead == "" {
		t.Fatalf("a branch deleted on origin should not refuse the run: %s", why)
	}
	if _, err := gitOut(ws, "rev-parse", "-q", "--verify", "refs/remotes/origin/task/auth-refactor"); err == nil {
		t.Error("the stale tracking ref should be removed")
	}
	runGit(t, ws, "push", "-q", "--force-with-lease", "--force-if-includes", "origin", "task/auth-refactor")
}

func TestParseGitVersion(t *testing.T) {
	cases := []struct {
		out          string
		major, minor int
		ok           bool
	}{
		{"git version 2.29.2\n", 2, 29, true},
		{"git version 2.30.0\n", 2, 30, true},
		{"git version 2.100.0\n", 2, 100, true},
		{"git version 3.0.0\n", 3, 0, true},
		{"git version 1.31.0\n", 1, 31, true},
		{"git version 2.39.5 (Apple Git-154)\n", 2, 39, true},
		{"git version 2.43.0.windows.1\n", 2, 43, true},
		{"git version 2\n", 0, 0, false},
		{"git version next\n", 0, 0, false},
		{"something else entirely", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		major, minor, ok := parseGitVersion(c.out)
		if major != c.major || minor != c.minor || ok != c.ok {
			t.Errorf("parseGitVersion(%q) = %d, %d, %v; want %d, %d, %v", c.out, major, minor, ok, c.major, c.minor, c.ok)
		}
	}
}

func TestGithubPRPushesFastForwardWithReflogsOff(t *testing.T) {
	w, ws, remote, gh := githubWorld(t, "success", ciTest)
	runGit(t, ws, "config", "core.logAllRefUpdates", "false")
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "-u", "origin", "task/auth-refactor")
	w.passingRun(ws, gh)
	local := runGit(t, ws, "rev-parse", "task/auth-refactor")
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip != local {
		t.Errorf("origin should hold the fast-forwarded branch %s, got %s", local, tip)
	}
}

// rewrittenWithoutReflog leaves origin on a commit this workspace replaced by an amend, with the reflog expired. It returns origin's tip.
func rewrittenWithoutReflog(t *testing.T, ws string) string {
	t.Helper()
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "-u", "origin", "task/auth-refactor")
	first := runGit(t, ws, "rev-parse", "task/auth-refactor")
	runGit(t, ws, "switch", "-q", "task/auth-refactor")
	runGit(t, ws, "commit", "-q", "--amend", "-m", "rewritten")
	runGit(t, ws, "switch", "-q", "main")
	runGit(t, ws, "reflog", "expire", "--expire=now", "--all")
	return first
}

func TestGithubPRRefusesPushWithoutReflogAndNamesTheRemedy(t *testing.T) {
	_, ws, remote, _ := githubWorld(t, "success", localTest)
	first := rewrittenWithoutReflog(t, ws)
	err := pushTask(ws, "task/auth-refactor")
	if err == nil {
		t.Fatal("the push should be refused")
	}
	if want := "git push --force-with-lease=task/auth-refactor:" + first + " origin task/auth-refactor"; !strings.Contains(err.Error(), want) {
		t.Errorf("the error should carry %q: %v", want, err)
	}
	if tip := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"); tip != first {
		t.Errorf("origin changed to %s, it was %s", tip, first)
	}
}

func TestGithubPRStartWithoutReflogNamesTheRemedy(t *testing.T) {
	w, ws, _, _ := githubWorld(t, "success", localTest)
	first := rewrittenWithoutReflog(t, ws)
	lead, why := startEmpty(w)
	if lead != "" {
		t.Fatal("the start should be refused")
	}
	for _, want := range []string{"has commits this workspace doesn't", "reflog", "git push --force-with-lease=task/auth-refactor:" + first + " origin task/auth-refactor"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal should contain %q: %s", want, why)
		}
	}
}

func TestGithubPRStaleBranchPushesPastTrackingRefOfDeletedBranch(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	localTaskCommit(t, ws, "mine.txt")
	runGit(t, ws, "push", "-q", "-u", "origin", "task/auth-refactor")
	runGit(t, cloneRemote(t, remote), "push", "-q", "origin", "--delete", "task/auth-refactor")
	if lead, why := startEmpty(w, "--stale-branch"); lead == "" {
		t.Fatalf("--stale-branch should start: %s", why)
	}
	if _, err := gitOut(ws, "rev-parse", "-q", "--verify", "refs/remotes/origin/task/auth-refactor"); err != nil {
		t.Fatal("--stale-branch should leave the tracking ref in place")
	}
	if err := pushTask(ws, "task/auth-refactor"); err != nil {
		t.Fatalf("the push should succeed: %v", err)
	}
	if tip, local := runGit(t, ".", "--git-dir", remote, "rev-parse", "task/auth-refactor"), runGit(t, ws, "rev-parse", "task/auth-refactor"); tip != local {
		t.Errorf("origin should hold %s, got %s", local, tip)
	}
}

func TestGithubPRPrePushHookDoesNotRunAtStartOnReflogOnlyTip(t *testing.T) {
	w, ws, remote, _ := githubWorld(t, "success", localTest)
	staleBranch(t, ws, remote, "news.txt", "meanwhile\n")
	runGit(t, ws, "push", "-q", "origin", "task/auth-refactor")
	if out, err := w.land("update"); err != nil {
		t.Fatalf("update: %q %v", out, err)
	}
	hooks := t.TempDir()
	runGit(t, ws, "config", "core.hooksPath", hooks)
	log := filepath.Join(t.TempDir(), "hook.log")
	hook := "#!/bin/sh\necho ran >> '" + filepath.ToSlash(log) + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if lead, why := startEmpty(w); lead == "" {
		t.Fatalf("a failing pre-push hook should not refuse the start: %s", why)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("the pre-push hook ran during the start")
	}
}
