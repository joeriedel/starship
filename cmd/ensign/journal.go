// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Journals — task journals and the other records a bridge keeps under projects/ and
// skills/ — are committed and pushed in the bridge's own git, whatever VCS the
// projects use.

func isGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// journalChanges lists changed files under projects/, skills/ and notes/, and under reader/ when bridge.yaml declares the reader, leaving out projects that turned journal commits off. Under notes/ and reader/ only Markdown files without a hidden segment count.
func journalChanges(bridge string) ([]string, error) {
	paths := []string{"projects", "skills", "notes"}
	// An unreadable or invalid bridge.yaml leaves reader/ out, as if undeclared; the invalid declaration is reported where it is used.
	if declared, err := readerDeclared(bridge); err == nil && declared {
		paths = append(paths, "reader")
	}
	toks, err := gitZ(bridge, append([]string{"status", "--porcelain", "-z", "--untracked-files=all", "--"}, paths...)...)
	if err != nil {
		return nil, fmt.Errorf("git status: %v", err)
	}
	var out []string
	add := func(path string, renamed bool) {
		if !renamed && (strings.HasPrefix(path, "notes/") || strings.HasPrefix(path, "reader/")) && !isNoteFile(path) {
			return
		}
		if p, ok := strings.CutPrefix(path, "projects/"); ok {
			project, _, _ := strings.Cut(p, "/")
			if journalOff(bridge, project) {
				return
			}
		}
		out = append(out, path)
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		// A rename lists both sides; both are committed so the old path is removed, and a Markdown file on either side keeps both.
		if strings.ContainsAny(t[:2], "RC") && i+1 < len(toks) {
			keep := isNoteFile(t[3:]) || isNoteFile(toks[i+1])
			add(t[3:], keep)
			i++
			add(toks[i], keep)
			continue
		}
		add(t[3:], false)
	}
	return out, nil
}

func journalOff(bridge, project string) bool {
	var l Layer
	readYAML(filepath.Join(bridge, "projects", project, "project.yaml"), &l)
	return l.Journal.Commit == "off"
}

func remoteName(dir string) string {
	out, _ := gitOut(dir, "remote")
	if out == "" {
		return ""
	}
	return strings.Fields(out)[0]
}

// unpushed reports whether the bridge's branch has commits its remote doesn't. It asks
// the remote, not the local tracking ref, which can be stale.
func unpushed(bridge string) bool {
	remote := remoteName(bridge)
	if remote == "" {
		return false
	}
	branch, err := gitOut(bridge, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return false
	}
	head, err := gitOut(bridge, "rev-parse", "HEAD")
	if err != nil {
		return false
	}
	out, err := gitOut(bridge, "ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return false // can't reach the remote; don't trap the session
	}
	fields := strings.Fields(out)
	return len(fields) == 0 || fields[0] != head
}

// hookStop keeps a turn from ending with journal changes uncommitted or unpushed, or with a sequence step waiting.
func hookStop(bridge string, in *HookInput) any {
	if out := journalStop(bridge, in); out != nil {
		return out // a journal block spends none of the sequence's budget
	}
	return sequenceStop(bridge, in)
}

func journalStop(bridge string, in *HookInput) any {
	if !isGit(bridge) {
		return nil
	}
	changed, err := journalChanges(bridge)
	if err != nil {
		if in.StopHookActive {
			return nil // asked once already; don't trap the session
		}
		return block(fmt.Sprintf("the bridge's journal changes could not be read, so they may be uncommitted: %v", err))
	}
	ahead := len(changed) == 0 && unpushed(bridge)
	if len(changed) == 0 && !ahead {
		return nil
	}
	if in.StopHookActive {
		return nil // asked once already; don't trap the session
	}
	if ahead {
		return block("the bridge has journal commits that are not pushed. Run: ensign journal push")
	}
	return block(fmt.Sprintf("commit the bridge's journal changes before ending the turn: "+
		"ensign journal commit -m \"<what changed>\". Changed: %s", strings.Join(changed, ", ")))
}

// cmdJournal implements `ensign journal commit -m <message> [files…]` and `ensign journal push`.
func cmdJournal(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign journal commit -m <message> [files…] | ensign journal push")
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if !isGit(bridge) {
		return fmt.Errorf("the bridge is not a git repository")
	}
	if len(args) == 1 && args[0] == "push" {
		return pushBridge(bridge, out)
	}
	if len(args) < 3 || args[0] != "commit" || args[1] != "-m" || strings.TrimSpace(args[2]) == "" {
		return usage
	}
	files := args[3:]
	if len(files) == 0 {
		if files, err = journalChanges(bridge); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "no journal changes to commit")
		return pushBridge(bridge, out)
	}
	if err := checkJournalSources(bridge, files); err != nil {
		return err
	}
	// add errors on a path that is neither in the worktree nor the index (a deletion already staged), but commit -- still takes it.
	var addable []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(bridge, filepath.FromSlash(f))); err == nil {
			addable = append(addable, f)
		} else if tracked, _ := gitOut(bridge, "ls-files", "--", f); tracked != "" {
			addable = append(addable, f)
		}
	}
	if len(addable) > 0 {
		if err := git(bridge, append([]string{"add", "-A", "--"}, addable...)...); err != nil {
			return err
		}
	}
	if err := git(bridge, append([]string{"commit", "-q", "-m", args[2], "--"}, files...)...); err != nil {
		return err
	}
	fmt.Fprintf(out, "committed %d file(s)\n", len(files))
	return pushBridge(bridge, out)
}

// runCitation matches a path into one run's folder (a concrete run id) or a dispatch log.
var runCitation = regexp.MustCompile(`\.starship[/\\]runs[/\\]\d{8}-\d{6}-[A-Za-z0-9]|\bd\d+\.log\b`)

// checkJournalSources refuses task journals (task.md, history.md) that cite a specific
// run's files: run folders are local to one machine and get cleaned up. It expands args
// to the files git would commit and scans only the lines this commit adds.
func checkJournalSources(bridge string, args []string) error {
	hasHead := git(bridge, "rev-parse", "-q", "--verify", "HEAD") == nil
	var files []string
	tracked := map[string]bool{}
	if hasHead {
		out, _ := gitOut(bridge, append([]string{"diff", "--name-only", "--relative", "HEAD", "--"}, args...)...)
		for _, f := range strings.Split(out, "\n") {
			if f != "" {
				files = append(files, f)
				tracked[f] = true
			}
		}
	} else {
		out, _ := gitOut(bridge, append([]string{"ls-files", "--"}, args...)...)
		files = append(files, strings.Split(out, "\n")...)
	}
	out, _ := gitOut(bridge, append([]string{"ls-files", "-o", "--exclude-standard", "--"}, args...)...)
	files = append(files, strings.Split(out, "\n")...)

	var found []string
	for _, rel := range files {
		if ok, _ := path.Match("projects/*/tasks/*/*.md", rel); !ok || (path.Base(rel) != "task.md" && path.Base(rel) != "history.md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(bridge, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		var added []int
		if tracked[rel] {
			diff, _ := gitOut(bridge, "diff", "-U0", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD", "--", rel)
			added = addedLines(diff)
		} else {
			for i := range lines {
				added = append(added, i+1)
			}
		}
		for _, n := range added {
			if n <= len(lines) && runCitation.MatchString(strings.TrimSuffix(lines[n-1], "\r")) {
				found = append(found, fmt.Sprintf("%s:%d", rel, n))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("task journals must not cite a specific run's files (%s). Put what the source showed into the account instead of citing it: run folders are local to one machine and get cleaned up", strings.Join(found, ", "))
}

// addedLines returns the new-file line numbers of the lines a zero-context diff adds.
func addedLines(diff string) []int {
	var added []int
	n := 0
	inHunk := false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			i := strings.Index(l, " +")
			if i < 0 {
				continue
			}
			rest := l[i+2:]
			if j := strings.IndexAny(rest, ", "); j >= 0 {
				rest = rest[:j]
			}
			n, _ = strconv.Atoi(rest)
		case !inHunk:
		case strings.HasPrefix(l, "+"):
			added = append(added, n)
			n++
		}
	}
	return added
}

// pushBridge rebases onto the remote and pushes. A conflict is left for the session to
// resolve: it aborts the rebase and names the files.
func pushBridge(bridge string, out io.Writer) error {
	remote := remoteName(bridge)
	if remote == "" {
		fmt.Fprintln(out, "the bridge has no remote; nothing pushed")
		return nil
	}
	branch, err := gitOut(bridge, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("the bridge's HEAD is detached")
	}
	files, err := unmergedFiles(bridge)
	if err != nil {
		return fmt.Errorf("couldn't check the bridge for unmerged files, so nothing was pushed: %w. Fix that, then run ensign journal push", err)
	}
	if len(files) > 0 {
		return fmt.Errorf("the bridge has unmerged files from an earlier conflict (%s). Resolve them, then run git add or git reset -- %s, and run ensign journal push",
			strings.Join(files, ", "), strings.Join(files, " "))
	}
	var conflicted []string
	if remoteHas(bridge, remote, branch) {
		if err := git(bridge, "pull", "-q", "--rebase", "--autostash", remote, branch); err != nil {
			conflicts, checkErr := unmergedFiles(bridge)
			git(bridge, "rebase", "--abort")
			if checkErr != nil {
				return fmt.Errorf("pulling %s/%s failed: %w. Whether files conflicted is unknown, because checking for unmerged files failed: %v. Nothing was pushed; fix that, then run ensign journal push", remote, branch, err, checkErr)
			}
			if len(conflicts) == 0 {
				return fmt.Errorf("pulling %s/%s failed: %w. Fix that, then run ensign journal push", remote, branch, err)
			}
			return fmt.Errorf("another machine changed the same files (%s). Pull with rebase, merge the two versions — "+
				"keep both where each only added lines — and run ensign journal push", strings.Join(conflicts, ", "))
		}
		conflicted, err = unmergedFiles(bridge)
		if err != nil {
			return fmt.Errorf("pulling %s/%s succeeded, but whether re-applying the bridge's uncommitted changes conflicted is unknown, because checking for unmerged files failed: %w. Nothing was pushed; the changes may still be in git stash", remote, branch, err)
		}
	}
	// Re-applying the autostash can conflict while git still exits 0, keeping the stash.
	stashNote := ""
	if len(conflicted) > 0 {
		// Unmerged index entries would block every later pull, so leave the markers as plain modifications.
		if err := git(bridge, append([]string{"reset", "-q", "--"}, conflicted...)...); err != nil {
			return fmt.Errorf("clearing the unmerged state of %s failed: %w", strings.Join(conflicted, ", "), err)
		}
		// A file the pushed commits deleted is untracked now; committing it would bring it back.
		var marked, removed []string
		for _, f := range conflicted {
			if git(bridge, "cat-file", "-e", "HEAD:"+f) == nil {
				marked = append(marked, f)
				continue
			}
			if err := os.Remove(filepath.Join(bridge, filepath.FromSlash(f))); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s, which the pushed version deleted, failed: %w", f, err)
			}
			removed = append(removed, f)
		}
		var notes []string
		if len(marked) > 0 {
			notes = append(notes, fmt.Sprintf("re-applying the bridge's uncommitted changes conflicted in (%s); they are kept in git stash and the files hold conflict markers. "+
				"Either resolve the markers and run git stash drop, or run git checkout -- %s to take the pushed version and keep your changes in the stash",
				strings.Join(marked, ", "), strings.Join(marked, " ")))
		}
		if len(removed) > 0 {
			notes = append(notes, fmt.Sprintf("the pushed version deleted (%s), so they were removed from the bridge; your changes to them are kept in git stash (see git stash show -p)",
				strings.Join(removed, ", ")))
		}
		stashNote = strings.Join(notes, "; and ")
	}
	if err := git(bridge, "push", "-q", "-u", remote, "HEAD"); err != nil {
		if stashNote != "" {
			return fmt.Errorf("%w; also, %s", err, stashNote)
		}
		return err
	}
	if stashNote != "" {
		return fmt.Errorf("the journal was pushed to %s/%s, but %s", remote, branch, stashNote)
	}
	fmt.Fprintf(out, "pushed to %s/%s\n", remote, branch)
	return nil
}

// unmergedFiles lists the bridge's unmerged paths, NUL-separated so odd names survive.
func unmergedFiles(bridge string) ([]string, error) {
	out, err := diffUnmerged(bridge)
	if err != nil {
		return nil, fmt.Errorf("git diff --diff-filter=U: %w", err)
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// diffUnmerged is a variable so tests can make the check fail midway through a push.
var diffUnmerged = func(bridge string) (string, error) {
	return gitOut(bridge, "diff", "--name-only", "-z", "--diff-filter=U")
}

func remoteHas(dir, remote, branch string) bool {
	out, err := gitOut(dir, "ls-remote", remote, "refs/heads/"+branch)
	return err == nil && out != ""
}

const syncLocalChangesNote = "starship: the bridge has local changes, so it was not updated from its remote."
const syncDivergedNote = "starship: the bridge could not fast-forward from its remote (it has diverged); resolve with a rebase."
const syncStatusFmt = "starship: the bridge's status could not be read (%v), so it was not updated from its remote."

// syncBridge fast-forwards a clean bridge from its remote at session start.
func syncBridge(bridge string) string {
	if !isGit(bridge) || remoteName(bridge) == "" {
		return ""
	}
	status, err := gitOut(bridge, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Sprintf(syncStatusFmt, err)
	}
	if status != "" {
		return syncLocalChangesNote
	}
	branch, err := gitOut(bridge, "symbolic-ref", "--short", "HEAD")
	if err != nil || !remoteHas(bridge, remoteName(bridge), branch) {
		return ""
	}
	if err := git(bridge, "pull", "-q", "--ff-only", remoteName(bridge), branch); err != nil {
		return syncDivergedNote
	}
	return ""
}
