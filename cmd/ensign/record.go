// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// How a run records its tested work is the project's record: setting, see resolveRecord. Git tasks record by commit (the github-pr and git-catalog steps); Perforce tasks record at the end of a successful run, by p4Record.

// runRecord is the run's record mode for a task of kind vcsKind. A snapshot from before the setting reads empty, which is the VCS's default.
func runRecord(r *RunFiles, vcsKind string) string {
	switch {
	case r.Snap.Record != "":
		return r.Snap.Record
	case vcsKind == "p4":
		return "shelve"
	}
	return "commit"
}

// runVcs is the vcs: block of the run's task; a run without a task, or one whose task.md can't be read, has none, which is git.
func runVcs(r *RunFiles) TaskVcs {
	if r.Snap.Task == "" {
		return TaskVcs{}
	}
	v, _ := taskVcsOf(r.Snap.Bridge, r.Snap.Task)
	return v
}

// recordsNothing reports whether the run leaves its work uncommitted in the workspace (record: none).
func recordsNothing(r *RunFiles) bool {
	return r.Snap.Task != "" && runRecord(r, runVcs(r).Kind) == "none"
}

// errNoRecord is the publish refusal for a task whose record mode is none: nothing was recorded, so there is nothing for ensign to land.
func errNoRecord(task string) error {
	return fmt.Errorf("%s has record: none, so its runs record nothing and there is nothing for ensign to publish: the work is left in the workspace for you to land by hand", task)
}

// noRecordRefusal refuses a publish of a task whose record mode is none. A task whose settings can't be loaded passes: the publish step reports that itself.
func noRecordRefusal(bridge, task string) error {
	if c, err := loadTaskLayers(bridge, task, ""); err == nil && c.Record == "none" {
		return errNoRecord(task)
	}
	return nil
}

// p4Opened lists the files open in the changelist, as -ztag records.
func p4Opened(r *RunFiles, env []string, v TaskVcs) ([]map[string]string, error) {
	out, err := p4Run(r.Snap.Workspace, env, v.Client, "-ztag", "opened", "-C", v.Client)
	if err != nil {
		return nil, err
	}
	var files []map[string]string
	for _, rec := range p4Records(out) {
		if rec["change"] == v.Changelist {
			files = append(files, rec)
		}
	}
	return files, nil
}

// p4Record records a Perforce task's tested work when its run succeeds, under the condition githubReady writes the tested marker on: the test passed and the run went through the whole pipeline. commit submits the task's changelist, shelve shelves it, none leaves it open; a project @record binding replaces the submit or shelve, with STARSHIP_RECORD set to the mode. A changelist with no open files records nothing.
func p4Record(r *RunFiles) error {
	v := runVcs(r)
	if r.Snap.Task == "" || v.Kind != "p4" {
		return nil
	}
	mode := runRecord(r, v.Kind)
	if mode == "none" {
		r.State.note(fmt.Sprintf("record: none, so changelist %s is left open for you to land by hand", v.Changelist))
		return nil
	}
	noTestStage := false
	if s, _, _ := r.Snap.Pipeline.find("test"); s == nil {
		noTestStage = true
	}
	if !testPassed(r) || (noTestStage && !startsAtPlainEntry(r)) {
		r.State.note(fmt.Sprintf("Changelist %s was not %s: this run's work is not tested (no passing test, or it did not go through the whole pipeline)", v.Changelist, map[string]string{"commit": "submitted", "shelve": "shelved"}[mode]))
		return nil
	}
	if v.Changelist == "" || v.Client == "" {
		return fmt.Errorf("p4: the task's vcs: has no client or changelist to record")
	}
	env := append(p4Env(r, v, ""), "STARSHIP_RECORD="+mode)
	files, err := p4Opened(r, env, v)
	if err != nil {
		return fmt.Errorf("could not record changelist %s: %v", v.Changelist, err)
	}
	if len(files) == 0 {
		r.State.note(fmt.Sprintf("Changelist %s has no open files, so nothing was recorded (record: %s)", v.Changelist, mode))
		return nil
	}
	out, bound, err := p4Bound(r, env, "record")
	if err != nil {
		return fmt.Errorf("could not record changelist %s: %v", v.Changelist, err)
	}
	if !bound {
		args := []string{"submit", "-c", v.Changelist}
		if mode == "shelve" {
			args = []string{"shelve", "-f", "-c", v.Changelist}
		}
		if out, err = p4Run(r.Snap.Workspace, env, v.Client, args...); err != nil {
			return fmt.Errorf("could not record changelist %s: %v", v.Changelist, err)
		}
	}
	done := map[string]string{"commit": "Submitted", "shelve": "Shelved"}[mode]
	r.State.note(fmt.Sprintf("%s changelist %s (%d open files, record: %s)%s", done, v.Changelist, len(files), mode, lastLinePart(out)))
	return nil
}

// lastLinePart is " (<last line of out>)", or nothing when out says nothing.
func lastLinePart(out string) string {
	if strings.TrimSpace(out) == "" {
		return ""
	}
	return ": " + lastLines(out, 1)
}

// reviewChangeLine tells a reviewer where to read the run's change. For Perforce the diff of the task's changelist is written to the run's reports folder first.
func reviewChangeLine(r *RunFiles, d *Dispatch) string {
	v := runVcs(r)
	if r.Snap.Task == "" {
		return ""
	}
	if v.Kind != "p4" {
		if !isGit(r.Snap.Workspace) {
			return ""
		}
		base := r.State.Base
		if base == "" {
			base = r.State.StartHead
		}
		if base == "" {
			base = "HEAD"
		}
		line := fmt.Sprintf("\nThe run's change: `git diff %s` in the workspace", base)
		if runRecord(r, v.Kind) == "none" {
			line += " (the work is uncommitted)"
		}
		// A coder's new files are untracked until the commit, which can come after review.
		if toks, err := gitZ(r.Snap.Workspace, "status", "--porcelain", "-z", "--untracked-files=normal"); err == nil {
			var untracked []string
			for _, t := range toks {
				if p, ok := strings.CutPrefix(t, "?? "); ok {
					untracked = append(untracked, p)
				}
			}
			if len(untracked) > 0 {
				line += "; untracked files, which that diff does not show, are to be read directly: " + strings.Join(untracked, ", ")
			}
		}
		return line + "\n"
	}
	file := filepath.Join(r.Dir, "reports", "change-"+d.ID+".diff")
	what, err := writeP4Change(r, v, file)
	if err != nil {
		return fmt.Sprintf("\nThe run's change could not be read from Perforce: %v\n", err)
	}
	return fmt.Sprintf("\nThe run's change (%s of changelist %s) is in %s\n", what, v.Changelist, file)
}

// writeP4Change writes the diff of the task's changelist to file, and says what it covers: the files open in it plus the changes shell commands made that no reconcile has opened yet (found by a preview, which opens nothing), or, when there are none, its shelf.
func writeP4Change(r *RunFiles, v TaskVcs, file string) (string, error) {
	ws := r.Snap.Workspace
	env := p4Env(r, v, "")
	if v.Changelist == "" || v.Client == "" {
		return "", fmt.Errorf("the task's vcs: has no client or changelist")
	}
	files, err := p4Opened(r, env, v)
	if err != nil {
		return "", err
	}
	unopened, err := p4Unopened(r, env, v, files)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	what := "the open files"
	if len(unopened) > 0 {
		what = "the open files and the changes not yet opened"
	}
	if len(files) == 0 && len(unopened) == 0 {
		out, err := p4Run(ws, env, v.Client, "describe", "-S", "-du", v.Changelist)
		if err != nil {
			return "", fmt.Errorf("changelist %s has no open files and no shelf (%v)", v.Changelist, err)
		}
		b.WriteString(out)
		what = "the shelf"
	}
	for _, f := range files {
		depot := f["depotFile"]
		switch action := f["action"]; {
		case strings.Contains(action, "delete") || action == "purge":
			fmt.Fprintf(&b, "deleted: %s\n", depot)
		case action == "add" || action == "branch" || action == "move/add":
			// depotFile is already escaped as p4 takes it; p4Escape is for local paths.
			out, err := p4Run(ws, env, v.Client, "-ztag", "where", depot)
			if err != nil {
				return "", err
			}
			recs := p4Records(out)
			if len(recs) == 0 || recs[0]["path"] == "" {
				return "", fmt.Errorf("p4 where %s: no local path", depot)
			}
			if err := writeNewFileDiff(&b, depot, recs[0]["path"]); err != nil {
				return "", err
			}
		default:
			out, err := p4Run(ws, env, v.Client, "diff", "-du", depot)
			if err != nil {
				return "", err
			}
			writeDiffText(&b, out)
		}
	}
	for _, f := range unopened {
		depot, client := f["depotFile"], f["clientFile"]
		switch action := f["action"]; action {
		case "delete":
			fmt.Fprintf(&b, "deleted: %s\n", depot)
		case "add":
			if err := writeNewFileDiff(&b, depot, client); err != nil {
				return "", err
			}
		default:
			out, err := p4Run(ws, env, v.Client, "diff", "-f", "-du", p4Escape(client))
			if err != nil {
				return "", err
			}
			writeDiffText(&b, out)
		}
	}
	if err := writeFile(file, b.String()); err != nil {
		return "", err
	}
	return what, nil
}

// p4Unopened previews the reconcile (which opens nothing) of the files changed since the reconcile mark, and returns what it would open: the changes shell commands made. It leaves the mark where it is. It drops files already open in the changelist (open) and files open in another changelist. A project's @reconcile binding can't be previewed, so p4's own reconcile is what is previewed.
func p4Unopened(r *RunFiles, env []string, v TaskVcs, open []map[string]string) ([]map[string]string, error) {
	ws := r.Snap.Workspace
	out, err := p4Run(ws, env, v.Client, "-ztag", "opened", "-C", v.Client)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, rec := range p4Records(out) {
		skip[rec["depotFile"]] = true
		skip[foldPath(rec["clientFile"], hostOS)] = true
	}
	for _, rec := range open {
		skip[rec["depotFile"]] = true
	}
	mark, err := reconcileMark(r)
	if err != nil {
		return nil, err
	}
	candidates, _, err := p4Candidates(r, env, v, mark)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	return p4ReconcilePreview(r, env, v, candidates, skip)
}

// writeNewFileDiff writes the local file at path as a whole-file addition of depot.
func writeNewFileDiff(b *strings.Builder, depot, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	fmt.Fprintf(b, "==== %s (new file)\n--- /dev/null\n+++ %s\n@@ -0,0 +1,%d @@\n", depot, depot, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return nil
}

func writeDiffText(b *strings.Builder, out string) {
	b.WriteString(out)
	if !strings.HasSuffix(out, "\n") {
		b.WriteString("\n")
	}
}
