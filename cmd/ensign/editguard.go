// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// editGuard refuses an Edit, Write or NotebookEdit outside the folders the calling agent may edit. It returns the reason, or "" to allow, and the warning from binding the run, if any. The error means the run could not be looked up; a failed save of the rebind is a warning.
func editGuard(bridge string, in *HookInput, goos string) (reason, warning string, err error) {
	if in.ToolName != "Edit" && in.ToolName != "Write" && in.ToolName != "NotebookEdit" {
		return "", "", nil
	}
	target, _ := in.ToolInput["file_path"].(string)
	if target == "" {
		target, _ = in.ToolInput["notebook_path"].(string)
	}
	if target == "" {
		return "", "", nil
	}
	home, _ := os.UserHomeDir()
	expanded := claudeToolPath(target, home, goos)
	if windowsUnresolvable(expanded, goos) {
		return "this edit was refused because on Windows, give the full `C:\\...` path of the file; this spelling (a stream, a device or volume path, a drive-relative or a rooted path with no drive) can't be checked against the folders this agent may edit. Working around the guard is a failure, not a fix.", "", nil
	}
	if why := snapshotGuard(bridge, in, target, goos); why != "" {
		return why, "", nil
	}
	if in.AgentID != "" && in.AgentType == "reader" {
		bf, err := loadBridgeFile(bridge)
		var rules map[string][]string
		if err == nil {
			rules, err = bf.connectorRules()
		}
		if err != nil {
			return "this edit was refused because bridge.yaml can't be read or is invalid, so a reader's folder can't be checked: " + err.Error(), "", nil
		}
		if _, declared := rules["reader"]; declared {
			folder := filepath.Join(bridge, "reader")
			if pathWithin(resolveEditTarget(expanded, in.Cwd, goos), resolveEditFolder(folder, goos), goos) {
				return "", "", nil
			}
			return "this edit was refused because " + target + " is outside " + folder + ", the only folder a reader may edit. Working around the guard is a failure, not a fix.", "", nil
		}
	}
	var folders []string
	var who string
	if in.AgentID == "" {
		who = "the session"
		folders = sessionEditFolders(bridge)
	} else {
		r, err := openHookRun(bridge, in)
		if err != nil {
			return "", "", err
		}
		folders = editTempFolders()
		who = "this agent"
		if r != nil {
			defer r.close()
			if r.warn != nil {
				warning = r.warn.Error()
			}
			if r.rebound {
				if err := r.save(); err != nil && !errors.Is(err, fs.ErrNotExist) {
					msg := "the run's rebind to this session could not be saved: " + err.Error()
					if warning != "" {
						warning += "; " + msg
					} else {
						warning = msg
					}
				}
			}
			if d := callerDispatch(r, in); d != nil {
				scratch := filepath.Join(r.Dir, "scratch")
				who = "this " + d.Role
				if !d.Done && (d.Stage == "code" || d.Stage == "style") {
					folders = append(append(runCodeFolders(r), scratch), folders...)
				} else if !d.Done && d.Role == "documentation" {
					docs := docsFolders(r)
					who = "this documentation stage (only the @docs paths: " + strings.Join(docs, ", ") + ")"
					folders = append(append(docs, scratch), folders...)
				} else {
					folders = append([]string{scratch}, folders...)
				}
			}
		}
	}
	resolved := resolveEditTarget(expanded, in.Cwd, goos)
	for _, f := range folders {
		if pathWithin(resolved, resolveEditFolder(f, goos), goos) {
			return "", warning, nil
		}
	}
	var pointer string
	if in.AgentID == "" {
		allowed, held, err := directEdit(bridge, in.SessionID, resolved, goos)
		if err != nil {
			return "", warning, err
		}
		if allowed {
			return "", warning, nil
		}
		if held != "" {
			return "this edit was refused because a direct edit is refused in a folder a live run holds: " + held + ". Working around the guard is a failure, not a fix.", warning, nil
		}
		pointer = " A code folder needs the user's choice between a task and a direct edit: `ensign help direct`."
	}
	return "this edit was refused because " + target + " is outside the folders " + who + " may edit: " + strings.Join(folders, ", ") +
		". The same limit applies to shell commands (redirection, sed -i, cp, git apply, scripts) that write files. Working around the guard is a failure, not a fix." + pointer, warning, nil
}

// docsFolders are the run's @docs paths, joined to its workspace.
func docsFolders(r *RunFiles) []string {
	var out []string
	for _, p := range r.Snap.Bindings["docs"].Paths {
		out = append(out, filepath.Join(r.Snap.Workspace, filepath.FromSlash(strings.ReplaceAll(p, `\`, "/"))))
	}
	return out
}

// gitDirtyPaths lists the paths git status reports as changed or untracked, relative to the repository root, including the original path of a rename or copy.
func gitDirtyPaths(ws string) ([]string, error) {
	toks, err := gitZ(ws, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git status: %v", err)
	}
	var out []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		out = append(out, t[3:])
		if strings.ContainsAny(t[:2], "RC") && i+1 < len(toks) {
			i++
			out = append(out, toks[i])
		}
	}
	return out, nil
}

// docsBaseline records each dirty path of the workspace with its content hash, taken when the documentation agent is spawned; nil if the workspace is not a git repository.
func docsBaseline(r *RunFiles) (map[string]string, error) {
	ws := r.Snap.Workspace
	root, err := gitOut(ws, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil
	}
	dirty, err := gitDirtyPaths(ws)
	if err != nil {
		return nil, err
	}
	base := map[string]string{}
	for _, p := range dirty {
		base[p] = fileHash(root, p)
	}
	return base, nil
}

// docsChanged lists, relative to the repository root, the paths changed since the baseline (a dirty path not in it, or one whose content differs, a deleted file included), split into those inside the @docs paths and those outside. root is empty if the workspace is not a git repository.
func docsChanged(r *RunFiles, base map[string]string) (root string, inside, outside []string, err error) {
	ws := r.Snap.Workspace
	root, rerr := gitOut(ws, "rev-parse", "--show-toplevel")
	if rerr != nil {
		return "", nil, nil, nil
	}
	dirty, err := gitDirtyPaths(ws)
	if err != nil {
		return "", nil, nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range append(dirty, mapKeys(base)...) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var allowed []string
	for _, f := range docsFolders(r) {
		allowed = append(allowed, resolveEditFolder(f, runtime.GOOS))
	}
	for _, p := range paths {
		if was, ok := base[p]; ok && was == fileHash(root, p) {
			continue
		}
		resolved := resolveEditTarget(filepath.FromSlash(p), root, runtime.GOOS)
		in := false
		for _, a := range allowed {
			if pathWithin(resolved, a, runtime.GOOS) {
				in = true
				break
			}
		}
		if in {
			inside = append(inside, p)
		} else {
			outside = append(outside, p)
		}
	}
	return root, inside, outside, nil
}

func mapKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lastDocsDispatch is the run's latest documentation dispatch, or nil.
func lastDocsDispatch(r *RunFiles) *Dispatch {
	for i := len(r.State.Dispatches) - 1; i >= 0; i-- {
		if r.State.Dispatches[i].Role == "documentation" {
			return r.State.Dispatches[i]
		}
	}
	return nil
}

// literalPathspecs prefixes each path with :(literal), so a name with glob or magic characters matches only itself. The global --literal-pathspecs option would set GIT_LITERAL_PATHSPECS for the commit's hooks, which would then silently match nothing.
func literalPathspecs(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = ":(literal)" + p
	}
	return out
}

// commitDocsPaths adds the changed @docs paths to the index and commits just them; it does nothing when none differ from HEAD.
func commitDocsPaths(root string, paths []string, noVerify bool, args ...string) error {
	var present []string
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, p)
		}
		if _, err := os.Lstat(abs); err != nil {
			known, kerr := gitOut(root, "ls-files", "--", ":(literal)"+p)
			if kerr != nil {
				return fmt.Errorf("git ls-files: %v", kerr)
			}
			if known == "" {
				continue
			}
		}
		present = append(present, p)
	}
	if len(present) == 0 {
		return nil
	}
	paths = literalPathspecs(present)
	if err := git(root, append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	// User config must not hide a staged change: it would be skipped as "nothing to commit".
	staged, err := gitOut(root, append([]string{"diff", "--cached", "--name-only", "--no-relative", "--ignore-submodules=none", "--"}, paths...)...)
	if err != nil {
		return fmt.Errorf("git diff --cached: %v", err)
	}
	if staged == "" {
		return nil
	}
	commit := []string{"commit", "-q"}
	if noVerify {
		commit = append(commit, "--no-verify")
	}
	return git(root, append(append(append(commit, "--only"), args...), append([]string{"--"}, paths...)...)...)
}

// sessionEditFolders is what the session agent may edit: the bridge, temp, Claude Code's user config, and the folders bridge.yaml lists under session-edit.
func sessionEditFolders(bridge string) []string {
	folders := append([]string{bridge}, editTempFolders()...)
	if home, err := os.UserHomeDir(); err == nil {
		folders = append(folders, filepath.Join(home, ".claude"))
	}
	if bf, err := loadBridgeFile(bridge); err == nil {
		for _, f := range bf.SessionEdit {
			if !filepath.IsAbs(f) {
				f = filepath.Join(bridge, f)
			}
			folders = append(folders, f)
		}
	}
	return folders
}

// runCodeFolders are the run's code locations.
func runCodeFolders(r *RunFiles) []string {
	if r.Snap.Workspace == "" {
		return nil
	}
	return []string{r.Snap.Workspace}
}

// editTempFolders is the guard's system temp folders; tests replace it.
var editTempFolders = func() []string { return systemTempFolders(runtime.GOOS) }

// systemTempFolders also names /tmp off Windows: on macOS os.TempDir is under /var/folders while Claude Code's scratch folders may live under /tmp.
func systemTempFolders(goos string) []string {
	out := []string{os.TempDir()}
	if goos != "windows" {
		out = append(out, "/tmp")
	}
	return out
}

// stripExtendedPrefix removes a Windows \\?\ prefix, turning \\?\UNC\x into \\x; UNC is matched in any case, since a lower-case one would otherwise become a relative path. A forward-slash //?/ prefix is left alone: tool-input paths of that form are refused up front, and a cwd or folder comes from the OS with backslashes.
func stripExtendedPrefix(p, goos string) string {
	if goos != "windows" {
		return p
	}
	if len(p) >= 8 && strings.HasPrefix(p, `\\?\`) && strings.EqualFold(p[4:7], "UNC") && p[7] == '\\' {
		return `\\` + p[8:]
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// foldPath puts a path in the form used to compare paths: slashes as separators, cleaned, and lower case where the file system ignores case.
func foldPath(p, goos string) string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	if goos == "windows" || goos == "darwin" {
		p = strings.ToLower(p)
	}
	return path.Clean(p)
}

// pathWithin reports whether p is dir or inside it; both are already folded.
func pathWithin(p, dir, goos string) bool {
	p, dir = foldPath(p, goos), foldPath(dir, goos)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func resolveEditTarget(p, cwd, goos string) string {
	p = stripExtendedPrefix(p, goos)
	if !filepath.IsAbs(p) {
		p = filepath.Join(stripExtendedPrefix(cwd, goos), p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return resolveExisting(abs)
}

func resolveEditFolder(p, goos string) string {
	abs, err := filepath.Abs(stripExtendedPrefix(p, goos))
	if err != nil {
		return p
	}
	return resolveExisting(abs)
}
