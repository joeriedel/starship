package main

import (
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// editGuard refuses an Edit, Write or NotebookEdit outside the folders the calling agent may edit. It returns the reason, or "" to allow. An error means the run could not be looked up.
func editGuard(bridge string, in *HookInput) (string, error) {
	if in.ToolName != "Edit" && in.ToolName != "Write" && in.ToolName != "NotebookEdit" {
		return "", nil
	}
	target, _ := in.ToolInput["file_path"].(string)
	if target == "" {
		target, _ = in.ToolInput["notebook_path"].(string)
	}
	if target == "" {
		return "", nil
	}
	var folders []string
	var who string
	if in.AgentID == "" {
		who = "the session"
		folders = sessionEditFolders(bridge)
	} else {
		r, err := openSessionRun(bridge, in.SessionID)
		if err == nil && r == nil {
			r, err = openAgentRun(bridge, in)
		}
		if err != nil {
			return "", err
		}
		folders = editTempFolders()
		who = "this agent"
		if r != nil {
			defer r.close()
			if d := callerDispatch(r, in); d != nil {
				scratch := filepath.Join(r.Dir, "scratch")
				who = "this " + d.Role
				if !d.Done && (d.Stage == "code" || d.Stage == "style") {
					folders = append(append(runCodeFolders(r), scratch), folders...)
				} else {
					folders = append([]string{scratch}, folders...)
				}
			}
		}
	}
	resolved := resolveEditTarget(target, in.Cwd, runtime.GOOS)
	for _, f := range folders {
		if pathWithin(resolved, resolveEditFolder(f, runtime.GOOS), runtime.GOOS) {
			return "", nil
		}
	}
	return "this edit was refused because " + target + " is outside the folders " + who + " may edit: " + strings.Join(folders, ", ") +
		". The same limit applies to shell commands (redirection, sed -i, cp, git apply, scripts) that write files. Working around the guard is a failure, not a fix.", nil
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

// stripExtendedPrefix removes a Windows \\?\ prefix, turning \\?\UNC\x into \\x.
func stripExtendedPrefix(p, goos string) string {
	if goos != "windows" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
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
