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
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
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
	// User config must not hide untracked files from this check.
	s, err := gitOut(ws, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, fmt.Errorf("git status: %v", err)
	}
	return s != "", nil
}

// startsAtDocs reports whether the run was started at the documentation stage. It runs nothing the test stage covers, and tested() checks what it committed, so it leaves the tested marker alone.
func startsAtDocs(r *RunFiles) bool {
	return r.Snap.Start == "docs"
}

// startsAtPlainEntry reports whether the run went through the whole pipeline: it has no start, or starts where a plain run enters, which is the first stage, that stage's first child when it leads others, or the stage after a first stage that is skipped.
func startsAtPlainEntry(r *RunFiles) bool {
	start := r.Snap.Start
	if start == "" {
		return true
	}
	for _, s := range r.Snap.Pipeline.Stages {
		if start == s.ID || (s.isLead() && start == s.Stages[0].ID) {
			return true
		}
		if skipReason(s, r.Snap.Bindings, r.Snap.Documentation) == "" {
			break
		}
		if t, _ := s.Outcomes.find(s.Skip.Outcome); !t.Next {
			break
		}
	}
	return false
}

// githubBranch runs at run.start: the workspace moves to the task's branch, created from
// the default branch the first time. Work on another branch is never carried along, and
// whatever passed before has to pass again before it can land.
func githubBranch(r *RunFiles) error {
	if err := checkGitVersion(); err != nil {
		return err
	}
	if !startsAtDocs(r) { // a docs start runs no tests and tested() checks what it commits
		removeRecord(testedFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location)) // a new run has to pass again
	}
	ws, want := r.Snap.Workspace, taskBranch(r.Snap.Task)
	cur := currentBranch(ws)
	_, verr := gitOut(ws, "rev-parse", "-q", "--verify", "refs/heads/"+want)
	exists := verr == nil
	ffBase := ""
	if exists && !r.State.StaleOK {
		var err error
		if ffBase, err = checkNotBehind(ws, want); err != nil {
			return err
		}
	}
	if exists && !r.State.StaleOK {
		if err := checkRemoteCopy(ws, want, true, ffBase); err != nil {
			return err
		}
	}
	if cur == want {
		return fastForward(ws, want, ffBase)
	}
	if d, err := dirty(ws); err != nil {
		return err
	} else if d {
		return fmt.Errorf("the workspace has uncommitted changes on %s; commit or stash them, then start the run again", orDetached(cur))
	}
	if exists {
		if err := git(ws, "switch", "-q", want); err != nil {
			return err
		}
		return fastForward(ws, want, ffBase)
	}
	base, err := defaultBranch(ws)
	if err != nil {
		return err
	}
	if err := git(ws, "fetch", "-q", "origin", base); err != nil {
		return err
	}
	if !r.State.StaleOK {
		if err := checkRemoteCopy(ws, want, false, base); err != nil {
			return err
		}
	}
	return git(ws, "switch", "-q", "-c", want, "origin/"+base)
}

// gitMinimum is the oldest git with --force-if-includes, which every task-branch push uses.
var gitMinimum = [2]int{2, 30}

// parseGitVersion reads the major and minor numbers from `git --version` output such as "git version 2.39.5 (Apple Git-154)".
func parseGitVersion(out string) (major, minor int, ok bool) {
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != "git" || f[1] != "version" {
		return 0, 0, false
	}
	parts := strings.Split(f[2], ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	var err1, err2 error
	major, err1 = strconv.Atoi(parts[0])
	minor, err2 = strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

// checkGitVersion refuses a git older than gitMinimum. Output it can't read passes: the guard must not become a false refusal.
func checkGitVersion() error {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return nil
	}
	major, minor, ok := parseGitVersion(string(out))
	if !ok || major > gitMinimum[0] || (major == gitMinimum[0] && minor >= gitMinimum[1]) {
		return nil
	}
	return fmt.Errorf("git %d.%d is too old: the github-pr extension needs git %d.%d or newer, for push --force-if-includes", major, minor, gitMinimum[0], gitMinimum[1])
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

// checkNotBehind refuses a task branch that lacks commits of the remote's default branch and has commits of its own. A behind branch with none of its own is no work to protect: it returns the default branch's name so the caller can fast-forward it.
func checkNotBehind(ws, branch string) (ffBase string, err error) {
	base, behind, err := branchBehind(ws, branch)
	if err != nil || !behind {
		return "", err
	}
	own, err := gitOut(ws, "rev-list", "--count", "origin/"+base+"..refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("counting the commits of %s beyond origin/%s: %v", branch, base, err)
	}
	if own != "0" {
		return "", fmt.Errorf("task branch %s is behind %s: origin/%s has commits it doesn't contain. Ask to update the branch (rebases it onto %s), or start the run again with --stale-branch to run on it as it is", branch, base, base, base)
	}
	return base, nil
}

// remoteTip returns the tip of a branch on origin, or "" if origin has no such branch.
func remoteTip(ws, branch string) (string, error) {
	out, err := gitOut(ws, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// inReflog reports whether a commit is in the local branch's reflog, so was pushed from here even before a rebase.
func inReflog(ws, branch, tip string) bool {
	log, err := gitOut(ws, "log", "-g", "--format=%H", "refs/heads/"+branch)
	if err != nil {
		return false
	}
	for _, h := range strings.Fields(log) {
		if h == tip {
			return true
		}
	}
	return false
}

// isAncestor reports whether a commit is an ancestor of (or is) a ref.
func isAncestor(ws, tip, ref string) bool {
	return exec.Command("git", "-C", ws, "merge-base", "--is-ancestor", tip, ref).Run() == nil
}

// holdsTip reports whether a commit is one this workspace has: in the local branch's reflog (pushed from here, even before a rebase) or an ancestor of the branch.
func holdsTip(ws, branch, tip string) bool {
	return inReflog(ws, branch, tip) || isAncestor(ws, tip, "refs/heads/"+branch)
}

// checkRemoteCopy refuses a run when origin has the task branch with commits this workspace lacks. A tip the local branch holds is not a loss (one held only through the reflog is decided by asking git whether the push would pass, because --force-if-includes depends on reflog timing and update-ref would stamp a newer entry), nor is one already in origin/<base> when base is given, so base must have been fetched. An accepted tip becomes the tracking ref, or the later lease push fails as stale. A branch absent from origin has its tracking ref removed, as fetch --prune would, or the later push fails as stale.
func checkRemoteCopy(ws, branch string, exists bool, base string) error {
	tip, err := remoteTip(ws, branch)
	if err != nil {
		return fmt.Errorf("looking for %s on origin: %v", branch, err)
	}
	if tip == "" {
		ref := "refs/remotes/origin/" + branch
		if _, err := gitOut(ws, "rev-parse", "-q", "--verify", ref); err == nil {
			if err := git(ws, "update-ref", "-d", ref); err != nil {
				return fmt.Errorf("removing the stale %s: %v", ref, err)
			}
		}
		return nil
	}
	held := exists && isAncestor(ws, tip, "refs/heads/"+branch)
	if !held && exists && inReflog(ws, branch, tip) {
		// --no-verify: this only asks git the force-if-includes question, and the hook still runs on the real push.
		held = git(ws, "push", "-q", "--dry-run", "--no-verify", "--force-with-lease", "--force-if-includes", "origin", branch) == nil
	}
	if !held && base != "" {
		held = isAncestor(ws, tip, "origin/"+base)
	}
	if !held {
		if !exists {
			return fmt.Errorf("task branch %s exists on origin but not in this workspace, and starting the run would overwrite it. Fetch it and switch to it, or delete the remote branch if its commits aren't wanted, then start the run again", branch)
		}
		return fmt.Errorf("task branch %s on origin has commits this workspace doesn't, and pushing would destroy them. This can also mean this workspace's reflog for the branch is disabled or expired, so git can't show the commits were pushed from here. Bring them into the local branch, or delete the remote branch if they aren't wanted, or, once you've checked nothing there is needed, run `git push --force-with-lease=%s:%s origin %s` to overwrite it, then start the run again", branch, branch, tip, branch)
	}
	// The tip was just proved held, so the lease now only guards what arrives later.
	if err := git(ws, "update-ref", "refs/remotes/origin/"+branch, tip); err != nil {
		return fmt.Errorf("recording %s as origin's copy of %s: %v", tip, branch, err)
	}
	return nil
}

// fastForward moves the checked-out task branch to origin/base. It does nothing when base is empty. Git refuses, and changes nothing, if uncommitted work overlaps the incoming commits.
func fastForward(ws, branch, base string) error {
	if base == "" {
		return nil
	}
	// merge.autoStash=true makes git exit 0 on a conflicting stash pop instead of refusing.
	if err := git(ws, "-c", "merge.autoStash=false", "merge", "-q", "--ff-only", "origin/"+base); err != nil {
		return fmt.Errorf("task branch %s couldn't be fast-forwarded to origin/%s: %v", branch, base, err)
	}
	return nil
}

// githubUpdate rebases a task's branch onto the remote's default branch. A conflict leaves
// the branch exactly as it was.
func githubUpdate(bridge, task, location string) (base, sha string, upToDate bool, err error) {
	live, err := liveRuns(bridge)
	if err != nil {
		return "", "", false, err
	}
	for _, ri := range live {
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
		// User config must not narrow this listing: an omitted conflict would go unreported.
		files, _ := gitZ(ws, "diff", "--name-only", "--diff-filter=U", "--no-relative", "-z")
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

// githubCommitDocs runs after the documentation stage: it commits only the @docs paths the stage changed, so earlier uncommitted work stays dirty and untested. Unlike githubCommit it does not run checkNoStrayFiles, which looks at the whole workspace rather than at what is staged.
func githubCommitDocs(r *RunFiles) error {
	if _, err := onTaskBranch(r); err != nil {
		return err
	}
	var base map[string]string
	if d := lastDocsDispatch(r); d != nil {
		base = d.DocsBase
	}
	root, inside, _, err := docsChanged(r, base)
	if err != nil {
		return err
	}
	_, name, _ := strings.Cut(r.Snap.Task, "/")
	return commitDocsPaths(root, inside, false, "-m", name)
}

// pushTask pushes the task branch to origin. When origin has no such branch or its tip is an ancestor of ours, nothing is forced, so it pushes plainly and git itself rejects a non-fast-forward if origin moves meanwhile: --force-if-includes would refuse that push whenever the local reflog is off or expired, and the lease would fail as stale on a tracking ref left by a branch deleted on origin. Anything else is a forced update, guarded by the lease.
func pushTask(ws, branch string) error {
	tip, err := remoteTip(ws, branch)
	if err == nil && (tip == "" || isAncestor(ws, tip, "refs/heads/"+branch)) {
		return git(ws, "push", "-q", "-u", "origin", branch)
	}
	err = git(ws, "push", "-q", "--force-with-lease", "--force-if-includes", "-u", "origin", branch)
	if err == nil || tip == "" || inReflog(ws, branch, tip) {
		return err
	}
	return fmt.Errorf("%v\norigin's %s is at %s, which isn't in this branch's history or reflog, so git can't show this workspace has seen it (reflogs that are off or expired can cause this). Once you've checked nothing there is needed, `git push --force-with-lease=%s:%s origin %s` overwrites it", err, branch, tip, branch, tip, branch)
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
	if err := pushTask(ws, branch); err != nil {
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

// publishRecordFile lists the locations a publish step still has to land, one per line, so a new publish step after a failed one lands only those.
func publishRecordFile(bridge, task string) string {
	return taskStateFile(bridge, task, "", ".publish")
}

// removeRecord removes a per-task record, retried while another process briefly holds it on Windows. A record that is not there is removed.
func removeRecord(f string) error {
	err := retry(retryWindow, isTransientFileError, func() error { return os.Remove(f) })
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// publishLockEnv names the task whose publish lock a publish command's process tree is running under.
const publishLockEnv = "STARSHIP_PUBLISH_LOCK_HELD"

// lockPublish takes a task's publish lock without waiting, and returns the func that releases it.
func lockPublish(bridge, task string) (func(), error) {
	lock := taskStateFile(bridge, task, "", ".publish.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	l := flock.New(lock)
	if ok, err := l.TryLock(); err != nil {
		return nil, fmt.Errorf("locking the publish of %s: %w", task, err)
	} else if !ok {
		return nil, fmt.Errorf("a publish of %s is already under way", task)
	}
	return func() { l.Unlock() }, nil
}

// testedFilesOf lists every .tested record of a task: the one with no location, and one per location.
func testedFilesOf(bridge, task string) []string {
	entries, _ := os.ReadDir(filepath.Join(bridge, ".starship", "tasks"))
	e := strings.ReplaceAll(task, "/", "--")
	var out []string
	for _, ent := range entries {
		n := ent.Name()
		loc, ok := strings.CutPrefix(n, e+"@")
		loc, ok2 := strings.CutSuffix(loc, ".tested")
		if n == e+".tested" || (ok && ok2 && validName(loc)) {
			out = append(out, filepath.Join(bridge, ".starship", "tasks", n))
		}
	}
	return out
}

func publishRecorded(bridge, task string) bool {
	_, err := os.Stat(publishRecordFile(bridge, task))
	return err == nil
}

func readPublishRecord(bridge, task string) []string {
	data, _ := readFileRetry(publishRecordFile(bridge, task))
	return strings.Fields(string(data))
}

// writePublishRecord records the locations left to land, and removes the record when there are none.
func writePublishRecord(bridge, task string, left []string) error {
	f := publishRecordFile(bridge, task)
	if len(left) == 0 {
		if err := removeRecord(f); err != nil {
			return fmt.Errorf("could not remove the record of locations still to land: %v", err)
		}
		return nil
	}
	if err := writeFileAtomic(bridge, f, []byte(strings.Join(left, "\n")+"\n")); err != nil {
		return fmt.Errorf("could not record the locations still to land: %v", err)
	}
	return nil
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
	data, markerErr := readFileRetry(testedFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location))
	noTestStage := false
	if s, _, _ := r.Snap.Pipeline.find("test"); s == nil {
		noTestStage = true
	}
	// With no test stage testPassed is always true, so only a run through the whole pipeline may write a marker: a partial run ran nothing that vouches for the commits already on the branch.
	partial := noTestStage && !startsAtPlainEntry(r)
	if !testPassed(r) || partial {
		if markerErr == nil {
			marker := strings.TrimSpace(string(data))
			if sha == marker {
				r.State.note(fmt.Sprintf("The task branch %s is at %s, which is still the tested commit", branch, short(sha)))
				return nil
			}
			if docsOnlySince(ws, marker, sha, r.Snap.Bindings["docs"].Paths) {
				r.State.note(fmt.Sprintf("The task branch %s is at %s and is still tested: only @docs paths changed since %s, which passed its tests",
					branch, short(sha), short(marker)))
				return nil
			}
		}
		if partial {
			r.State.note(fmt.Sprintf("The task branch %s is at %s. This run did not go through the whole pipeline, so the branch is not recorded as tested: `ensign run %s` records it before it is published",
				branch, short(sha), r.Snap.Task))
			return nil
		}
		r.State.note(fmt.Sprintf("The task branch %s is at %s. No test ran in this run, so the branch is not recorded as tested: `ensign run %s --start test` records it before it is published",
			branch, short(sha), r.Snap.Task))
		return nil
	}
	f := testedFile(r.Snap.Bridge, r.Snap.Task, r.Snap.Location)
	if err := writeFileAtomic(r.Snap.Bridge, f, []byte(sha+"\n")); err != nil {
		return err
	}
	base, _ := defaultBranch(ws)
	ahead := false
	if r.State.Sequence {
		var aerr error
		if ahead, aerr = publishStepAhead(r); aerr != nil {
			r.State.note(fmt.Sprintf("The tested work is committed on %s (%s). The session's sequence file can't be read, so whether a publish step is ahead is unknown (%s): sort that out before asking for a pull request",
				branch, short(sha), oneLine(aerr)))
			return nil
		}
	}
	if ahead {
		r.State.note(fmt.Sprintf("The tested work is committed on %s (%s). A publish step in the session's sequence takes it from here: when that step runs, the work goes straight to %s and the branch is removed, so finding it there with no task branch is the sequence's doing, not an unexplained push. The sequence's report to the session gives the result",
			branch, short(sha), orDefault(base)))
		return nil
	}
	r.State.note(fmt.Sprintf("The tested work is committed on %s (%s). Nothing has been opened yet: the user can ask for a pull request, or for it to go straight to %s",
		branch, short(sha), orDefault(base)))
	return nil
}

// publishStepAhead reports whether the run's sequence has a pending publish step for the task after the run's step, with no later run of the task in between. Such a publish lands every location the step's runs worked in.
func publishStepAhead(r *RunFiles) (bool, error) {
	sf, err := readSeqErr(r.Snap.Bridge, r.State.Session)
	if err != nil {
		return false, err
	}
	for _, seq := range sf.Sequences {
		found := false
		for _, st := range seq.Steps {
			if !found {
				if st.Kind != "run" {
					continue
				}
				if !slices.ContainsFunc(st.runs(), func(sr *seqRun) bool { return sr.Run == r.State.Run }) {
					continue
				}
				found = true
				continue
			}
			if st.Task != r.Snap.Task {
				continue
			}
			if st.Kind == "run" {
				break
			}
			if st.Kind == "publish" && st.Status == "pending" {
				return true, nil
			}
		}
	}
	return false, nil
}

// docsOnlySince reports whether marker is an ancestor of head and every path changed between them is inside the declared @docs paths. It compares in git's own path space, lexically, so nothing is resolved on disk. The diff has no rename detection, so moving a code file into a docs folder still lists its source.
func docsOnlySince(ws, marker, head string, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	if git(ws, "merge-base", "--is-ancestor", marker, head) != nil {
		return false
	}
	prefix, err := gitOut(ws, "rev-parse", "--show-prefix")
	if err != nil {
		return false
	}
	// User config must not narrow this listing: an omitted path would pass the gate.
	changed, err := gitZ(ws, "diff", "--name-only", "--no-renames", "--no-relative", "--ignore-submodules=none", "-z", marker, head)
	if err != nil {
		return false
	}
	for _, p := range changed {
		in := false
		for _, d := range paths {
			if pathWithin(p, path.Join(prefix, strings.ReplaceAll(d, `\`, "/")), runtime.GOOS) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return true
}

// testPassed reports whether the run's latest test outcome was a pass. A pipeline with no test stage counts as passed. It reads the history, which keeps every outcome for agent and plain stages alike; a dispatch's Outcome is blanked on some paths.
func testPassed(r *RunFiles) bool {
	if s, _, _ := r.Snap.Pipeline.find("test"); s == nil {
		return true
	}
	passed := false
	for _, h := range r.State.History {
		if h.Kind == "outcome" && h.Stage == "test" {
			passed = strings.HasPrefix(h.Detail, "pass ")
		}
	}
	return passed
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

// endedAfterPass reports whether a failed run ended after the given pass, by their finish times. Runs without a finish time (older ones), or with the very same one, fall back on the At of the failed run's last history entry; history is to the second, so a tie, a pass without a finish time or a run without a readable entry falls back on run-id order.
func endedAfterPass(failed runInfo, pass runInfo) bool {
	if f, p := failed.state.FinishedAt, pass.state.FinishedAt; f != 0 && p != 0 && f != p {
		return f > p
	}
	if n := len(failed.state.History); n > 0 && pass.state.FinishedAt != 0 {
		if t, err := time.Parse(time.RFC3339, failed.state.History[n-1].At); err == nil {
			if sec, passSec := t.Unix(), time.Unix(0, pass.state.FinishedAt).Unix(); sec != passSec {
				return sec > passSec
			}
		}
	}
	return failed.id > pass.id
}

// runIDMayBelongTo reports whether a run id, <YYYYmmdd-HHMMSS>-<label> with an optional -N for a collision, could be a run of the task. The label is the task with its slashes turned into dashes, so tasks can share one: it may match too much, never too little.
func runIDMayBelongTo(id, task string) bool {
	const stamp = len("20060102-150405-")
	if len(id) < stamp {
		return false
	}
	rest := id[stamp:]
	label := strings.ReplaceAll(task, "/", "-")
	if rest == label {
		return true
	}
	n, ok := strings.CutPrefix(rest, label+"-")
	if !ok {
		return false
	}
	v, err := strconv.Atoi(n)
	return err == nil && v >= 2 && v <= 99
}

// latestRunPassed refuses when a run of the task in this location failed after the task's last pass (or when none passed), so a marker an earlier pass left behind cannot land failed work. An old run without a finish time, retired without a history entry after the pass was created, did no work and can slip past this.
func latestRunPassed(bridge, task, location string) error {
	var runs []runInfo
	var pass *runInfo
	all, noPipeline, err := scanRuns(bridge)
	if err != nil {
		return err
	}
	// A finished run with no readable pipeline.json has no task or location to read, so its id attributes it: it counts for every location of the task.
	unreadable := map[string]bool{}
	for _, ri := range noPipeline {
		if ri.state.Status != "success" && runIDMayBelongTo(ri.id, task) {
			runs = append(runs, ri)
			unreadable[ri.id] = true
		}
	}
	for _, ri := range all {
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
		if unreadable[ri.id] {
			return fmt.Errorf("run %s may be a run of %s that did not pass (%s), and its pipeline.json can't be read; run the task again", ri.id, task, ri.state.Reason)
		}
		return fmt.Errorf("the latest run of %s did not pass (%s); run the task again", task, ri.state.Reason)
	}
	return nil
}

// tested finds a task's tested commit, and checks the task branch still holds it, or only @docs paths on top of it. It returns the branch head, so what lands includes those docs commits.
func tested(bridge, task, location string) (c *Config, branch, sha string, err error) {
	live, err := liveRuns(bridge)
	if err != nil {
		return nil, "", "", err
	}
	for _, ri := range live {
		// A queued sequence run has not started, so it does not hold the task.
		if ri.snap.Task == task && !ri.state.queued() {
			return nil, "", "", fmt.Errorf("a run of %s is still going; wait for it to finish", task)
		}
	}
	c, err = loadConfigAt(bridge, task, location)
	if err != nil {
		return nil, "", "", err
	}
	data, err := readFileRetry(testedFile(bridge, task, c.Location))
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
		if !docsOnlySince(ws, sha, head, c.Bindings["docs"].Paths) {
			return nil, "", "", fmt.Errorf("%s has changed since its tests passed (tested %s, now %s); run the task again", branch, short(sha), short(head))
		}
		sha = head
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
	if err := pushTask(ws, branch); err != nil {
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
// fast-forward, and removes the task branch. It returns the default branch's name, the commit, and a note that is empty unless the branch was left on origin because it has commits this workspace doesn't, or can't show it pushed.
func githubMerge(bridge, task, location string) (string, string, string, error) {
	c, branch, sha, err := tested(bridge, task, location)
	if err != nil {
		return "", "", "", err
	}
	ws := c.Workspace
	base, err := defaultBranch(ws)
	if err != nil {
		return "", "", "", err
	}
	if err := git(ws, "fetch", "-q", "origin", base); err != nil {
		return "", "", "", err
	}
	if _, err := gitOut(ws, "merge-base", "--is-ancestor", "origin/"+base, sha); err != nil {
		return "", "", "", fmt.Errorf("%s has moved since this task branched from it, so the tested commit can't go there as it is. Run the task again to rebase and retest it", base)
	}
	if err := git(ws, "push", "-q", "origin", sha+":refs/heads/"+base); err != nil {
		return "", "", "", err
	}
	note := deleteLandedRemote(ws, branch)
	leaveLandedBranch(ws, branch, base, sha)
	removeRecord(testedFile(bridge, task, c.Location))
	removeSameFolderMarkers(bridge, task, c.Location)
	if left := readPublishRecord(bridge, task); slices.Contains(left, c.Location) {
		// The landing has happened; a record that can't be shrunk only makes a later publish try this location again.
		writePublishRecord(bridge, task, slices.DeleteFunc(left, func(l string) bool { return l == c.Location }))
	}
	return base, sha, note, nil
}

// removeSameFolderMarkers removes the .tested of each other location of the task that works in the landed location's folder: the landing deleted the task branch they tested, and collapseFolders may have dropped them from the publish. A location with no recorded folder is not compared.
func removeSameFolderMarkers(bridge, task, landed string) {
	project, _, _ := strings.Cut(task, "/")
	landedPath, err := locationPathFor(bridge, project, landed)
	if err != nil {
		return
	}
	for _, f := range testedFilesOf(bridge, task) {
		loc, ok := strings.CutPrefix(filepath.Base(f), strings.ReplaceAll(task, "/", "--")+"@")
		loc = strings.TrimSuffix(loc, ".tested")
		if !ok || loc == landed {
			continue
		}
		if p, err := locationPathFor(bridge, project, loc); err == nil && sameFolder(landedPath, p, runtime.GOOS) {
			removeRecord(f)
		}
	}
}

// deleteLandedRemote removes the landed task branch from origin if its tip is one this workspace holds, and returns a note when it leaves the branch there. The delete is pinned to the tip it checked, so a push arriving in between survives. It must run before the local branch and its reflog are deleted.
func deleteLandedRemote(ws, branch string) string {
	tip, err := remoteTip(ws, branch)
	if err != nil {
		return fmt.Sprintf("couldn't look for %s on origin (%v), so it was not deleted there", branch, err)
	}
	if tip == "" { // it may never have been pushed
		return ""
	}
	if !holdsTip(ws, branch, tip) {
		return fmt.Sprintf("%s was left on origin: it has commits this workspace doesn't (this can also mean this workspace's reflog for the branch is disabled or expired, so git can't show the commits were pushed from here)", branch)
	}
	git(ws, "push", "-q", "origin", "--delete", "--force-with-lease="+branch+":"+tip, branch)
	return ""
}

// leaveLandedBranch moves a workspace off the landed task branch onto base and deletes the branch. A refused merge is fine: the commit is on origin and uncommitted work stays as it is.
func leaveLandedBranch(ws, branch, base, sha string) {
	if currentBranch(ws) == branch {
		if err := git(ws, "switch", "-q", base); err == nil {
			// merge.autoStash=true makes git exit 0 on a conflicting stash pop, leaving conflict markers.
			git(ws, "-c", "merge.autoStash=false", "merge", "-q", "--ff-only", sha)
		}
	}
	if currentBranch(ws) != branch {
		git(ws, "branch", "-q", "-D", branch)
	}
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
	case "commit-docs":
		return "", githubCommitDocs(r)
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
	if os.Getenv(publishLockEnv) != args[1] {
		unlock, err := lockPublish(bridge, args[1])
		if err != nil {
			return err
		}
		defer unlock()
	}
	base, sha, note, err := githubMerge(bridge, args[1], location)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Pushed to %s: %s\n", base, short(sha))
	if note != "" {
		fmt.Fprintf(out, "Note: %s\n", note)
	}
	return nil
}
