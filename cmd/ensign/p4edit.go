// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

var p4CreatedRe = regexp.MustCompile(`Change (\d+) created`)

// p4FlowChangelistRe is the changelist entry of a one-line `vcs: { ... }` mapping.
var p4FlowChangelistRe = regexp.MustCompile(`\bchangelist\s*:\s*('[^']*'|"[^"]*"|[^,}\s]*)`)

func isEditTool(name string) bool {
	return name == "Edit" || name == "Write" || name == "NotebookEdit"
}

// p4Escape writes a local path in the form p4 reads file arguments in: its wildcard and revision characters are escaped.
func p4Escape(p string) string {
	return strings.NewReplacer("%", "%25", "@", "%40", "#", "%23", "*", "%2A").Replace(p)
}

// p4Records splits -ztag output into its records, each a map from field to the first value given.
func p4Records(out string) []map[string]string {
	var recs []map[string]string
	cur := map[string]string{}
	flush := func() {
		if len(cur) > 0 {
			recs = append(recs, cur)
			cur = map[string]string{}
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		rest, ok := strings.CutPrefix(line, "... ")
		if !ok {
			if strings.TrimSpace(line) == "" {
				flush()
			}
			continue
		}
		k, v, _ := strings.Cut(rest, " ")
		if _, seen := cur[k]; !seen {
			cur[k] = strings.TrimSpace(v)
		}
	}
	flush()
	return recs
}

// taskVcsOf reads the vcs: block of a task's task.md.
func taskVcsOf(bridge, task string) (TaskVcs, error) {
	var tf TaskFile
	front, _, err := splitFrontmatter(taskMdFile(bridge, task))
	if err != nil {
		return tf.Vcs, err
	}
	err = decodeFrontmatter(front, &tf)
	return tf.Vcs, err
}

func taskMdFile(bridge, task string) string {
	project, name, _ := strings.Cut(task, "/")
	return filepath.Join(bridge, "projects", project, "tasks", name, "task.md")
}

// setTaskChangelist records the task's changelist in task.md from a fresh read, under the journal lock like writeClosed.
func setTaskChangelist(bridge, task, changelist string) error {
	lock := taskStateFile(bridge, task, "", ".journal.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return err
	}
	l := flock.New(lock)
	if err := l.Lock(); err != nil {
		return fmt.Errorf("locking the journal: %w", err)
	}
	defer l.Unlock()
	file := taskMdFile(bridge, task)
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	edited, err := withChangelist(data, changelist)
	if err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	return writeFileAtomic(bridge, file, edited)
}

// withChangelist sets `changelist:` in the vcs: block of the frontmatter, in a block or a one-line { } mapping, touching no other byte and keeping the file's line endings.
func withChangelist(data []byte, n string) ([]byte, error) {
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	lines := strings.SplitAfter(string(data), "\n")
	bare := func(i int) string { return strings.TrimRight(lines[i], "\r\n") }
	if bare(0) != "---" {
		return nil, fmt.Errorf("no YAML frontmatter")
	}
	closing := 1
	for closing < len(lines) && !strings.HasPrefix(bare(closing), "---") {
		closing++
	}
	if closing == len(lines) {
		return nil, fmt.Errorf("frontmatter is not closed with ---")
	}
	v := -1
	for i := 1; i < closing; i++ {
		if strings.HasPrefix(lines[i], "vcs:") {
			v = i
			break
		}
	}
	if v < 0 {
		return nil, fmt.Errorf("no vcs: in the frontmatter")
	}
	head := bare(v)
	rest := strings.TrimSpace(strings.TrimPrefix(head, "vcs:"))
	switch {
	case strings.HasPrefix(rest, "{"):
		open, end := strings.Index(head, "{"), strings.LastIndex(head, "}")
		if end < open {
			return nil, fmt.Errorf("vcs: is a { } mapping over several lines; put it on one line or as a block")
		}
		inside := head[open+1 : end]
		if p4FlowChangelistRe.MatchString(inside) {
			inside = p4FlowChangelistRe.ReplaceAllLiteralString(inside, "changelist: "+n)
		} else if strings.TrimSpace(inside) == "" {
			inside = " changelist: " + n + " "
		} else {
			trimmed := strings.TrimRight(inside, " ")
			inside = trimmed + ", changelist: " + n + inside[len(trimmed):]
		}
		lines[v] = head[:open+1] + inside + head[end:] + lines[v][len(head):]
	case rest == "" || strings.HasPrefix(rest, "#"):
		indent, last := "", v
		for j := v + 1; j < closing; j++ {
			text := strings.TrimSpace(bare(j))
			if text == "" {
				continue
			}
			if bare(j)[0] != ' ' && bare(j)[0] != '\t' {
				break
			}
			ind := bare(j)[:len(bare(j))-len(strings.TrimLeft(bare(j), " \t"))]
			if indent == "" {
				indent = ind
			}
			last = j
			if strings.HasPrefix(text, "changelist:") {
				lines[j] = ind + "changelist: " + n + lines[j][len(bare(j)):]
				return []byte(strings.Join(lines, "")), nil
			}
		}
		if indent == "" {
			indent = "  "
		}
		lines = append(lines[:last+1], append([]string{indent + "changelist: " + n + eol}, lines[last+1:]...)...)
	default:
		return nil, fmt.Errorf("vcs: is neither a block nor a one-line { } mapping")
	}
	return []byte(strings.Join(lines, "")), nil
}

// p4Env is the environment of a Perforce step: the handler environment and the task's client and changelist.
func p4Env(r *RunFiles, v TaskVcs, file string) []string {
	env := append(handlerEnv(r, nil), "STARSHIP_CHANGELIST="+v.Changelist, "STARSHIP_CLIENT="+v.Client)
	if file != "" {
		env = append(env, "STARSHIP_FILE="+file)
	}
	return env
}

// p4Bound runs the project's binding of that name, if it defines one, and reports whether it did: with no binding the caller runs p4. A not-applicable binding does nothing, as runHandler does.
func p4Bound(r *RunFiles, env []string, name string) (out string, bound bool, err error) {
	b, ok := r.Snap.Bindings[name]
	switch {
	case !ok:
		return "", false, nil
	case b.NotApplicable != "":
		return "", true, nil
	case b.Command == "":
		return "", true, fmt.Errorf("binding @%s has no command", name)
	}
	out, err = shell(r.Snap.Workspace, b.Command, env)
	if err != nil {
		err = fmt.Errorf("@%s failed: %v: %s", name, err, lastLines(out, 3))
	}
	return out, true, err
}

// p4TaskChangelist makes sure the task has a pending changelist on its client, creating one described by the task's name when the recorded one is missing or no longer pending, and recording it in task.md. replaced is the recorded changelist that was not pending, if any.
func p4TaskChangelist(r *RunFiles, v TaskVcs, env []string) (changelist, replaced string, err error) {
	if v.Client == "" {
		return "", "", errors.New("p4: the task's vcs: has no client")
	}
	ws := r.Snap.Workspace
	out, err := p4Run(ws, env, v.Client, "-ztag", "changes", "-s", "pending", "-c", v.Client)
	if err != nil {
		return "", "", err
	}
	if v.Changelist != "" {
		for _, m := range p4ChangeRe.FindAllStringSubmatch(out, -1) {
			if m[1] == v.Changelist {
				return v.Changelist, "", nil
			}
		}
		replaced = v.Changelist
	}
	form := "Change: new\nClient: " + v.Client + "\nDescription:\n\t" + r.Snap.Task + "\n"
	out, err = p4RunIn(ws, env, v.Client, form, "change", "-i")
	if err != nil {
		return "", "", err
	}
	m := p4CreatedRe.FindStringSubmatch(out)
	if m == nil {
		return "", "", fmt.Errorf("p4 change -i: unexpected output: %s", strings.TrimSpace(lastLines(out, 1)))
	}
	if err := setTaskChangelist(r.Snap.Bridge, r.Snap.Task, m[1]); err != nil {
		return "", "", fmt.Errorf("created changelist %s but could not record it in task.md: %v", m[1], err)
	}
	return m[1], replaced, nil
}

// p4AgentTask finds, without the run lock, the run folder, snapshot and vcs of the Perforce task an agent's run belongs to. The snapshot is nil for a session's call, an agent outside a run or a task of another kind.
func p4AgentTask(bridge string, in *HookInput) (dir string, snap *Snapshot, v TaskVcs) {
	if in.AgentID == "" {
		return "", nil, v
	}
	run, _ := agentTag(in)
	if run == "" {
		return "", nil, v
	}
	dir = filepath.Join(runsDir(bridge), run)
	snap = &Snapshot{}
	if readJSON(filepath.Join(dir, "pipeline.json"), snap) != nil || snap.Task == "" {
		return "", nil, v
	}
	v, err := taskVcsOf(bridge, snap.Task)
	if err != nil || v.Kind != "p4" {
		return "", nil, v
	}
	return dir, snap, v
}

// p4EditTarget finds, without the run lock, the Perforce task of an agent's Edit, Write or NotebookEdit and the file it changes. It returns nil for anything else: a session's call, an agent outside a run, a task of another kind, or a file outside the task's workspace. The run's snapshot never changes, so reading it needs no lock, and git tasks wait on nothing.
func p4EditTarget(bridge string, in *HookInput) (r *RunFiles, v TaskVcs, file string) {
	if !isEditTool(in.ToolName) || in.AgentID == "" {
		return nil, v, ""
	}
	dir, snap, v := p4AgentTask(bridge, in)
	if snap == nil || snap.Workspace == "" {
		return nil, v, ""
	}
	target, _ := in.ToolInput["file_path"].(string)
	if target == "" {
		target, _ = in.ToolInput["notebook_path"].(string)
	}
	if target == "" {
		return nil, v, ""
	}
	home, _ := os.UserHomeDir()
	goos := runtime.GOOS
	expanded := stripExtendedPrefix(claudeToolPath(target, home, goos), goos)
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(stripExtendedPrefix(in.Cwd, goos), expanded)
	}
	file = filepath.Clean(expanded)
	if !pathWithin(resolveEditTarget(expanded, in.Cwd, goos), resolveEditFolder(snap.Workspace, goos), goos) {
		return nil, v, ""
	}
	for _, kept := range []string{"scratch", filepath.Join("reports", "evidence")} {
		if pathWithin(resolveEditTarget(expanded, in.Cwd, goos), resolveEditFolder(filepath.Join(dir, kept), goos), goos) {
			return nil, v, ""
		}
	}
	return &RunFiles{Dir: dir, Snap: snap, State: &State{Run: filepath.Base(dir)}}, v, file
}

type p4File struct {
	inDepot bool   // the depot has the file, not deleted at its head
	opened  bool   // the client has it open
	change  string // the changelist it is open in
}

func p4Fstat(r *RunFiles, env []string, v TaskVcs, file string) (p4File, error) {
	out, err := p4Run(r.Snap.Workspace, env, v.Client, "-ztag", "fstat", p4Escape(file))
	if err != nil {
		return p4File{}, err
	}
	var f p4File
	if recs := p4Records(out); len(recs) > 0 {
		rec := recs[0]
		f.opened, f.change = rec["action"] != "", rec["change"]
		f.inDepot = rec["depotFile"] != "" && !strings.Contains(rec["headAction"], "delete") && rec["headAction"] != "purge" && rec["headAction"] != "archive"
	}
	return f, nil
}

// p4BeforeEdit makes the file of an allowed Edit, Write or NotebookEdit by a run agent open in the task's changelist before it is written. It returns the reason to refuse the edit, or "".
func p4BeforeEdit(bridge string, in *HookInput) string {
	r, v, file := p4EditTarget(bridge, in)
	if r == nil {
		return ""
	}
	if v.Changelist == "" {
		return "this edit was refused because the task has no changelist recorded in task.md; the run's start creates it. Report it to your caller."
	}
	env := p4Env(r, v, file)
	f, err := p4Fstat(r, env, v, file)
	if err != nil {
		return "this edit was refused because the file's Perforce state could not be read: " + err.Error()
	}
	switch {
	case f.opened && f.change == v.Changelist:
		return ""
	case f.opened:
		return fmt.Sprintf("this edit was refused because %s is open in changelist %s, not the task's changelist %s. Starship never moves files between changelists; report it to your caller and do not run p4 yourself.", file, f.change, v.Changelist)
	case !f.inDepot:
		return ""
	}
	if _, bound, err := p4Bound(r, env, "checkout"); bound || err != nil {
		if err != nil {
			return "this edit was refused because opening the file failed: " + err.Error()
		}
		return ""
	}
	if _, err := p4Run(r.Snap.Workspace, env, v.Client, "edit", "-c", v.Changelist, p4Escape(file)); err != nil {
		return "this edit was refused because opening the file for edit failed: " + err.Error()
	}
	return ""
}

// p4AfterEdit adds a new file a run agent has written to the task's changelist. It returns a note for the agent when that failed, or "".
func p4AfterEdit(bridge string, in *HookInput) string {
	r, v, file := p4EditTarget(bridge, in)
	if r == nil || v.Changelist == "" {
		return ""
	}
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	env := p4Env(r, v, file)
	f, err := p4Fstat(r, env, v, file)
	if err != nil {
		return "starship: " + file + " was written but its Perforce state could not be read, so it was not added to the task's changelist: " + err.Error()
	}
	if f.opened || f.inDepot {
		return ""
	}
	if _, bound, err := p4Bound(r, env, "add"); bound || err != nil {
		if err != nil {
			return "starship: " + file + " was written but not added to the task's changelist: " + err.Error()
		}
		return ""
	}
	if _, err := p4Run(r.Snap.Workspace, env, v.Client, "add", "-c", v.Changelist, p4Escape(file)); err != nil {
		return "starship: " + file + " was written but not added to the task's changelist: " + err.Error()
	}
	return ""
}

// p4ReconcileBefore opens, in the task's changelist, the files shell commands changed since the last pass (found among the files modified or changed since the mark and the folders new since then), before a test stage starts. Files open in another changelist are left alone. The pass is appended to reports/reconcile.md and summarised in d.Reconciled for the lead. A task of another kind does nothing.
func p4ReconcileBefore(r *RunFiles, d *Dispatch) error {
	if r.Snap.Task == "" {
		return nil
	}
	v, err := taskVcsOf(r.Snap.Bridge, r.Snap.Task)
	if err != nil {
		return err
	}
	if v.Kind != "p4" {
		return nil
	}
	if v.Changelist == "" {
		return errors.New("reconcile: the task has no changelist recorded in task.md")
	}
	env := p4Env(r, v, "")
	var opened []string
	if out, bound, err := p4Bound(r, env, "reconcile"); bound || err != nil {
		if err != nil {
			return err
		}
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				opened = append(opened, l)
			}
		}
	} else if opened, err = p4ReconcileRun(r, env, v); err != nil {
		return err
	}
	section := fmt.Sprintf("## %s (%s), %s\n\nChangelist %s.\n\n", d.Stage, d.ID, time.Now().Format(time.RFC3339), v.Changelist)
	if len(opened) == 0 {
		section += "No files opened.\n\n"
	}
	for _, l := range opened {
		section += "- " + l + "\n"
	}
	if len(opened) > 0 {
		section += "\n"
	}
	file := filepath.Join(r.Dir, "reports", "reconcile.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(section)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return errors.Join(werr, cerr)
	}
	if len(opened) > 0 {
		shown := opened
		if len(shown) > 10 {
			shown = shown[:10]
		}
		d.Reconciled = fmt.Sprintf("Reconcile before this stage opened %d file(s) in changelist %s: %s", len(opened), v.Changelist, joinFileNames(shown))
		if len(opened) > len(shown) {
			d.Reconciled += fmt.Sprintf(", and %d more", len(opened)-len(shown))
		}
		d.Reconciled += ". Every pass is listed in " + file
	}
	return nil
}

// p4ReconcileRun previews the reconcile of the files changed since the mark, drops the files open in another changelist, and reconciles just the rest into the task's changelist, so a file in another changelist is never touched whatever p4 reconcile does with one. A pass that succeeds moves the mark to when it began and records the folders then. It returns the files it opened, as "action path".
func p4ReconcileRun(r *RunFiles, env []string, v TaskVcs) ([]string, error) {
	ws := r.Snap.Workspace
	mark, err := reconcileMark(r)
	if err != nil {
		return nil, err
	}
	began := time.Now().Add(-reconcileSlack)
	candidates, dirs, err := p4Candidates(r, env, v, mark)
	if err != nil {
		return nil, err
	}
	moveMark := func() error {
		if err := writeReconcileDirs(r, dirs); err != nil {
			return err
		}
		r.State.ReconcileMark = began.UnixNano()
		return nil
	}
	if len(candidates) == 0 {
		return nil, moveMark()
	}
	out, err := p4Run(ws, env, v.Client, "-ztag", "opened", "-C", v.Client)
	if err != nil {
		return nil, err
	}
	elsewhere := map[string]bool{}
	for _, rec := range p4Records(out) {
		if rec["change"] != v.Changelist {
			elsewhere[rec["depotFile"]] = true
			elsewhere[foldPath(rec["clientFile"], hostOS)] = true
		}
	}
	previewed, err := p4ReconcilePreview(r, env, v, candidates, elsewhere)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rec := range previewed {
		files = append(files, p4Escape(rec["clientFile"]))
	}
	if len(files) == 0 {
		return nil, moveMark()
	}
	out, err = p4RunIn(ws, env, v.Client, strings.Join(files, "\n")+"\n", "-x", "-", "-ztag", "reconcile", "-c", v.Changelist)
	if err != nil {
		return nil, err
	}
	if err := moveMark(); err != nil {
		return nil, err
	}
	var opened []string
	for _, rec := range p4Records(out) {
		path := rec["clientFile"]
		if path == "" {
			path = rec["depotFile"]
		}
		if path != "" {
			opened = append(opened, rec["action"]+" "+path)
		}
	}
	return opened, nil
}
