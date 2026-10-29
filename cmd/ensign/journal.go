package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Journals — task journals and the other records a bridge keeps under projects/ and
// proposals/ — are committed and pushed in the bridge's own git, whatever VCS the
// projects use.

func isGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// journalChanges lists changed files under projects/ and proposals/, leaving out
// projects that turned journal commits off.
func journalChanges(bridge string) []string {
	status, err := gitOut(bridge, "status", "--porcelain", "--untracked-files=all", "--", "projects", "proposals", "skills")
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

// hookStop keeps a turn from ending with journal changes uncommitted or unpushed.
func hookStop(bridge string, in *HookInput) any {
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
	if err := git(bridge, append([]string{"add", "--"}, files...)...); err != nil {
		return err
	}
	if err := git(bridge, append([]string{"commit", "-q", "-m", args[2], "--"}, files...)...); err != nil {
		return err
	}
	fmt.Fprintf(out, "committed %d file(s)\n", len(files))
	return pushBridge(bridge, out)
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
