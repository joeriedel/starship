package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// strayMaxSize is the largest untracked file the commit takes without a coder reporting it.
const strayMaxSize = 1 << 20

// checkNoStrayFiles refuses untracked files that look like build artifacts (binary, executable or large) unless a finished coder pass reported them as intended.
func checkNoStrayFiles(r *RunFiles) error {
	ws := r.Snap.Workspace
	untracked, err := gitZ(ws, "ls-files", "-o", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, d := range r.State.Dispatches {
		if d.Role == "coder" && d.Done {
			for _, f := range d.Fields["files"] {
				allowed[journalPath(ws, f)] = true
			}
		}
	}
	// Filesystems without exec bits (WSL /mnt/c, FAT) show every file as 0777, and git then sets core.fileMode=false.
	execBits := true
	if out, err := gitOut(ws, "config", "--bool", "core.fileMode"); err == nil && out == "false" {
		execBits = false
	}
	var lines []string
	for _, f := range untracked {
		if allowed[journalPath(ws, f)] {
			continue
		}
		path := filepath.Join(ws, filepath.FromSlash(f))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		var reasons []string
		if isBinaryFile(path) {
			reasons = append(reasons, "binary")
		}
		if runtime.GOOS == "windows" {
			if strings.EqualFold(filepath.Ext(f), ".exe") {
				reasons = append(reasons, "executable")
			}
		} else if execBits && info.Mode().Perm()&0o111 != 0 {
			reasons = append(reasons, "executable")
		}
		if info.Size() > strayMaxSize {
			reasons = append(reasons, fmt.Sprintf("%d bytes, over the %d byte limit", info.Size(), strayMaxSize))
		}
		if len(reasons) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%s; modified %s; %s)", f, strings.Join(reasons, ", "), info.ModTime().UTC().Format(time.RFC3339), strayStage(r.State, info.ModTime())))
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("untracked files that look like build artifacts would be committed: %s. Delete them, or have a coder pass report them as intended files", strings.Join(lines, "; "))
}

func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8000)
	n, _ := io.ReadFull(f, buf)
	return strings.IndexByte(string(buf[:n]), 0) >= 0
}

// strayStage names the stage that was running when a file was last modified: the last dispatch at or before its mtime.
func strayStage(st *State, mtime time.Time) string {
	var last *HistoryEntry
	for i := range st.History {
		h := &st.History[i]
		if h.Kind != "dispatch" {
			continue
		}
		at, err := time.Parse(time.RFC3339, h.At)
		if err != nil || at.After(mtime) {
			continue
		}
		last = h
	}
	if last == nil {
		return "it predates every dispatch of this run"
	}
	return fmt.Sprintf("likely from stage %s, dispatch %s at %s", last.Stage, last.Dispatch, last.At)
}
