// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// reconcileMark is the time after which a file counts as changed for the reconcile and its preview: the mark the last pass left, or the run's start for a run that has none yet.
func reconcileMark(r *RunFiles) (time.Time, error) {
	if r.State.ReconcileMark != 0 {
		return time.Unix(0, r.State.ReconcileMark), nil
	}
	t, err := time.Parse(time.RFC3339, r.Snap.Created)
	if err != nil {
		return time.Time{}, fmt.Errorf("the run has no reconcile mark and its start time is unreadable: %v", err)
	}
	return t, nil
}

// reconcileSlack is how far before a pass's start the mark it leaves is set: file times come from clocks coarser than the mark's (FAT keeps two seconds), so a write just after the pass can carry a time just before it, and rescanning a few unchanged files is harmless.
const reconcileSlack = 2 * time.Second

// p4Candidates returns, as p4 reads file arguments, the files shell commands may have changed since mark: the workspace's files whose modification or change time is at or after mark (a move or copy keeps a file's old modification time but not its change time; a symlink counts by its own times and is never followed), the folders not in the set recorded when the mark was set, passed whole, and the files p4 says are missing from the workspace. It also returns the workspace's folders, for the next set. Version-control and starship folders are not walked; p4's ignore rules apply to what is passed on.
func p4Candidates(r *RunFiles, env []string, v TaskVcs, mark time.Time) (candidates []string, dirs []reconcileDir, err error) {
	ws := r.Snap.Workspace
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			candidates = append(candidates, p)
		}
	}
	dirs, err = walkWorkspace(ws, readReconcileDirs(r), mark, add)
	if err != nil {
		return nil, nil, err
	}
	out, err := p4Run(ws, env, v.Client, "-ztag", "diff", "-sd", "...")
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range p4Records(out) {
		f := rec["depotFile"]
		if c := rec["clientFile"]; c != "" {
			f = p4Escape(c)
		}
		if f != "" {
			add(f)
		}
	}
	return candidates, dirs, nil
}

// p4ReconcilePreview previews the reconcile of the candidates (which opens nothing) and returns what it would open, less the files in skip, which holds depot paths and folded client paths.
func p4ReconcilePreview(r *RunFiles, env []string, v TaskVcs, candidates []string, skip map[string]bool) ([]map[string]string, error) {
	out, err := p4RunIn(r.Snap.Workspace, env, v.Client, strings.Join(candidates, "\n")+"\n", "-x", "-", "-ztag", "reconcile", "-n", "-c", v.Changelist)
	if err != nil {
		return nil, err
	}
	var found []map[string]string
	for _, rec := range p4Records(out) {
		client := rec["clientFile"]
		if client == "" || skip[rec["depotFile"]] || skip[foldPath(client, hostOS)] {
			continue
		}
		found = append(found, rec)
	}
	return found, nil
}

// walkWorkspace walks the workspace by its real path, as it may itself be a link, and reports files under ws. It passes each file at or after mark to add, and a folder that is not in known whole as "<folder>/..." without adding anything under it; a nil known means no folder is new. A folder is not in known when its path is not, or when both it and the recorded one have an identity and the two differ, as when another folder was renamed onto its path. It returns every folder, relative to ws with slashes, in walk order.
func walkWorkspace(ws string, known map[string]string, mark time.Time, add func(string)) ([]reconcileDir, error) {
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return nil, err
	}
	var dirs []reconcileDir
	var walk func(dir, shown, rel string, underNew bool) error
	walk = func(dir, shown, rel string, underNew bool) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir == real {
				return err
			}
			return nil
		}
		times, ids := entryTimes(dir, entries)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				switch name {
				case ".git", ".hg", ".svn", ".starship":
					continue
				}
				erel := rel + name
				dirs = append(dirs, reconcileDir{erel, ids[name]})
				isNew := underNew
				if !underNew && known != nil && dirIsNew(known, erel, ids[name]) {
					isNew = true
					add(p4Escape(filepath.Join(shown, name)) + "/...")
				}
				if err := walk(filepath.Join(dir, name), filepath.Join(shown, name), erel+"/", isNew); err != nil {
					return err
				}
				continue
			}
			if underNew {
				continue
			}
			t, ok := times[name]
			if !ok {
				info, err := e.Info()
				if err != nil {
					continue
				}
				t = info.ModTime()
			}
			if !t.Before(mark) {
				add(p4Escape(filepath.Join(shown, name)))
			}
		}
		return nil
	}
	if err := walk(real, ws, "", false); err != nil {
		return nil, err
	}
	return dirs, nil
}

// reconcileDir is a workspace folder: its path relative to the workspace with slashes, and its identity (device and inode, or NTFS file ID), empty where the system gives none.
type reconcileDir struct {
	Path, ID string
}

// dirIsNew reports whether the folder at rel with identity id is not the one recorded in known.
func dirIsNew(known map[string]string, rel, id string) bool {
	old, ok := known[foldPath(rel, hostOS)]
	return !ok || (old != "" && id != "" && old != id)
}

// reconcileDirsFile holds the workspace's folders as of the reconcile mark, one "<identity>\t<relative path>" line each (the identity may be empty), so a folder that appeared or was replaced since is known to be new.
func reconcileDirsFile(r *RunFiles) string {
	return filepath.Join(r.Dir, "reconcile-dirs.txt")
}

// readReconcileDirs returns the recorded folders' identities by path, folded as paths are compared, or nil when none are recorded. A line with no tab is a path alone.
func readReconcileDirs(r *RunFiles) map[string]string {
	data, err := os.ReadFile(reconcileDirsFile(r))
	if err != nil {
		return nil
	}
	known := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		id, path, found := strings.Cut(l, "\t")
		if !found {
			id, path = "", l
		}
		if path != "" {
			known[foldPath(path, hostOS)] = id
		}
	}
	return known
}

// writeReconcileDirs replaces the recorded folders atomically.
func writeReconcileDirs(r *RunFiles, dirs []reconcileDir) error {
	file := reconcileDirsFile(r)
	var b strings.Builder
	for _, d := range dirs {
		b.WriteString(d.ID + "\t" + d.Path + "\n")
	}
	data := b.String()
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		return err
	}
	return renameRetry(tmp, file)
}

// recordStartDirs records the workspace's folders when a Perforce run starts without a @reconcile binding, so a folder created or moved in during the run is passed whole.
func recordStartDirs(r *RunFiles) error {
	if r.Snap.Task == "" {
		return nil
	}
	if _, bound := r.Snap.Bindings["reconcile"]; bound {
		return nil
	}
	v, err := taskVcsOf(r.Snap.Bridge, r.Snap.Task)
	if err != nil || v.Kind != "p4" {
		return err
	}
	dirs, err := walkWorkspace(r.Snap.Workspace, nil, time.Time{}, func(string) {})
	if err != nil {
		return err
	}
	return writeReconcileDirs(r, dirs)
}
