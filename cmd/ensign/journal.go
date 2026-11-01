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

// journalChanges lists changed files under projects/ and skills/, leaving out
// projects that turned journal commits off.
func journalChanges(bridge string) []string {
	status, err := gitOut(bridge, "status", "--porcelain", "--untracked-files=all", "--", "projects", "skills")
	if err != nil || status == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.Trim(line[3:], `"`)
		if _, after, ok := strings.Cut(path, " -> "); ok {
			path = after
		}
		if p, ok := strings.CutPrefix(path, "projects/"); ok {
			project, _, _ := strings.Cut(p, "/")
			if journalOff(bridge, project) {
				continue
			}
		}
		out = append(out, path)
	}
	return out
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
	changed := journalChanges(bridge)
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
		files = journalChanges(bridge)
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "no journal changes to commit")
		return pushBridge(bridge, out)
	}
	if err := checkJournalSources(bridge, files); err != nil {
		return err
	}
	if err := git(bridge, append([]string{"add", "--"}, files...)...); err != nil {
		return err
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
	if remoteHas(bridge, remote, branch) {
		if err := git(bridge, "pull", "-q", "--rebase", remote, branch); err != nil {
			conflicts, _ := gitOut(bridge, "diff", "--name-only", "--diff-filter=U")
			git(bridge, "rebase", "--abort")
			return fmt.Errorf("another machine changed the same files (%s). Pull with rebase, merge the two versions — "+
				"keep both where each only added lines — and run ensign journal push", strings.ReplaceAll(conflicts, "\n", ", "))
		}
	}
	if err := git(bridge, "push", "-q", "-u", remote, "HEAD"); err != nil {
		return err
	}
	fmt.Fprintf(out, "pushed to %s/%s\n", remote, branch)
	return nil
}

func remoteHas(dir, remote, branch string) bool {
	out, err := gitOut(dir, "ls-remote", remote, "refs/heads/"+branch)
	return err == nil && out != ""
}

// syncBridge fast-forwards a clean bridge from its remote at session start.
func syncBridge(bridge string) string {
	if !isGit(bridge) || remoteName(bridge) == "" {
		return ""
	}
	if status, _ := gitOut(bridge, "status", "--porcelain", "--untracked-files=no"); status != "" {
		return "starship: the bridge has local changes, so it was not updated from its remote."
	}
	branch, err := gitOut(bridge, "symbolic-ref", "--short", "HEAD")
	if err != nil || !remoteHas(bridge, remoteName(bridge), branch) {
		return ""
	}
	if err := git(bridge, "pull", "-q", "--ff-only", remoteName(bridge), branch); err != nil {
		return "starship: the bridge could not fast-forward from its remote (it has diverged); resolve with a rebase."
	}
	return ""
}
