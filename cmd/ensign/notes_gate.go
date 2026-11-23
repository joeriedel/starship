// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func notesGatePath(bridge, session string) string {
	return sessionFile(bridge, session) + ".notes-gate"
}

// projectNotesFile is the absolute path of a project's project.md.
func projectNotesFile(bridge, project string) string {
	p := filepath.Join(bridge, "projects", project, "project.md")
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// notesGateError is a run that can't start because the session has not yet read its project's project.md after a compaction or clear.
type notesGateError struct{ file string }

func (e *notesGateError) Error() string {
	return fmt.Sprintf("read %s first: it holds the project's standing instructions, and runs, sequence approval and Agent dispatches are refused until this session has read it", e.file)
}

// notesGateRefusal is a *notesGateError while the session's gate marker exists, nil when it doesn't or can't be read.
func notesGateRefusal(bridge, session string) error {
	if session == "" {
		return nil
	}
	data, err := readFileRetry(notesGatePath(bridge, session))
	if err != nil {
		return nil
	}
	return &notesGateError{file: strings.TrimSpace(string(data))}
}

// sameNotesFile reports whether target, as the Read tool was given it, is the file gate. A relative target is taken from cwd. The files are compared with os.SameFile, and by cleaned absolute path (ignoring case on Windows) when one can't be statted.
func sameNotesFile(gate, target, cwd, goos string) bool {
	if target == "" {
		return false
	}
	if !filepath.IsAbs(target) && cwd != "" {
		target = filepath.Join(cwd, target)
	}
	a, errA := os.Stat(gate)
	b, errB := os.Stat(target)
	if errA == nil && errB == nil {
		return os.SameFile(a, b)
	}
	absA, errA := filepath.Abs(gate)
	absB, errB := filepath.Abs(target)
	if errA != nil || errB != nil {
		return false
	}
	if goos == "windows" {
		return strings.EqualFold(absA, absB)
	}
	return absA == absB
}

// clearNotesGateOnRead lifts the session's gate when its main agent reads the gated file.
func clearNotesGateOnRead(bridge string, in *HookInput) {
	if in.AgentID != "" || in.SessionID == "" {
		return
	}
	data, err := readFileRetry(notesGatePath(bridge, in.SessionID))
	if err != nil {
		return
	}
	target, _ := in.ToolInput["file_path"].(string)
	if sameNotesFile(strings.TrimSpace(string(data)), target, in.Cwd, runtime.GOOS) {
		if err := os.Remove(notesGatePath(bridge, in.SessionID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "starship:", err)
		}
	}
}

// notesReadLine is the session-start line that names the notes a session must read after a compaction or clear, and sets the gate that holds work until it has. It is empty, with no gate, when the session has no task or its project has no project.md.
func notesReadLine(bridge, session, task string) string {
	if session == "" || task == "" {
		return ""
	}
	project, _, _ := strings.Cut(task, "/")
	file := projectNotesFile(bridge, project)
	if fi, err := os.Stat(file); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if err := writeFile(notesGatePath(bridge, session), file+"\n"); err != nil {
		fmt.Fprintln(os.Stderr, "starship:", err)
	}
	design := ""
	if dir, err := filepath.Abs(designDir(bridge, project)); err == nil {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			design = dir
		}
	}
	return notesReadText(file, design)
}

// notesReadText is the session-start line for the notes at file, and for the design folder when design is not empty.
func notesReadText(file, design string) string {
	if design != "" {
		design = " (design: " + design + ")"
	}
	return fmt.Sprintf("\nnotes      read %s%s first; runs, go and Agent are refused until you do\n", file, design)
}
