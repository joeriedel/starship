// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// cmdP4 runs `ensign p4 open <path>...`.
func cmdP4(args []string, out io.Writer) error {
	const usage = "ensign p4 open <path>... [--bridge <dir>] [--run <id>]"
	if len(args) == 0 || args[0] != "open" {
		return fmt.Errorf("usage: %s", usage)
	}
	var flags, paths []string
	for i := 1; i < len(args); i++ {
		if (args[i] == "--bridge" || args[i] == "--run") && i+1 < len(args) {
			flags = append(flags, args[i], args[i+1])
			i++
			continue
		}
		paths = append(paths, args[i])
	}
	if len(paths) == 0 {
		return fmt.Errorf("usage: %s", usage)
	}
	hasBridge := false
	for i := 0; i < len(flags); i += 2 {
		hasBridge = hasBridge || flags[i] == "--bridge"
	}
	if !hasBridge {
		cwd, _ := os.Getwd()
		if _, err := findBridge(cwd); err != nil {
			bridge, err := executableBridge()
			if err != nil {
				return errors.New("not inside a bridge and ensign is not run from one; pass --bridge <bridge>")
			}
			flags = append(flags, "--bridge", bridge)
		}
	}
	r, err := openRunByFlags(flags, usage)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("there is no active run; ensign p4 open is for the agents of a Perforce task's run")
	}
	r.close() // the run is only read, and p4 may take a while
	v, err := taskVcsOf(r.Snap.Bridge, r.Snap.Task)
	if err != nil {
		return err
	}
	if v.Kind != "p4" {
		return fmt.Errorf("task %s is not a Perforce task; ensign p4 open is only for Perforce tasks", r.Snap.Task)
	}
	if v.Changelist == "" {
		return errors.New("the task has no changelist recorded in task.md; the run's start creates it. Report it to your caller.")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	failed := 0
	for _, p := range paths {
		file := filepath.Clean(p)
		if !filepath.IsAbs(file) {
			file = filepath.Join(cwd, file)
		}
		what, err := p4OpenFile(r, v, file, cwd)
		if err != nil {
			failed++
			what = "refused: " + err.Error()
		}
		fmt.Fprintf(out, "%s: %s\n", p, what)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d paths were not opened", failed, len(paths))
	}
	return nil
}

// p4Executable is the path of the running binary.
var p4Executable = os.Executable

// executableBridge is the bridge whose .starship/bin holds the running binary, for agents whose shell is in a workspace outside the bridge.
func executableBridge() (string, error) {
	exe, err := p4Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	bin := filepath.Dir(exe)
	dot := filepath.Dir(bin)
	if !strings.EqualFold(filepath.Base(bin), "bin") || !strings.EqualFold(filepath.Base(dot), ".starship") {
		return "", errors.New("ensign is not in a bridge's .starship/bin")
	}
	root := filepath.Dir(dot)
	if _, err := os.Stat(filepath.Join(root, "bridge.yaml")); err != nil {
		return "", err
	}
	return root, nil
}

// p4OpenFile opens one file in the task's changelist as its state on disk and in the depot call for, and says what it did. A file open in the changelist is left alone and one open in another is never moved.
func p4OpenFile(r *RunFiles, v TaskVcs, file, cwd string) (string, error) {
	if !p4Tracked(r.Dir, r.Snap.Workspace, file, cwd, runtime.GOOS) {
		return "", fmt.Errorf("%s is outside the task's workspace, or in the run's scratch or evidence folder", file)
	}
	env := p4Env(r, v, file)
	f, err := p4Fstat(r, env, v, file)
	if err != nil {
		return "", fmt.Errorf("its Perforce state could not be read: %v", err)
	}
	switch {
	case f.opened && f.change == v.Changelist:
		return "already open in changelist " + v.Changelist, nil
	case f.opened:
		return "", fmt.Errorf("%s is open in changelist %s, not the task's changelist %s; starship never moves files between changelists, so report it to your caller", file, f.change, v.Changelist)
	}
	_, statErr := os.Lstat(file)
	exists := statErr == nil
	var name, done string
	switch {
	case f.inDepot && exists:
		name, done = "checkout", "opened for edit"
	case f.inDepot:
		name, done = "delete", "opened for delete"
	case exists:
		name, done = "add", "added"
	default:
		return "", errors.New("no such file")
	}
	verb := map[string]string{"checkout": "edit", "delete": "delete", "add": "add"}[name]
	if _, bound, err := p4Bound(r, env, name); bound || err != nil {
		if err != nil {
			return "", err
		}
		return done, nil
	}
	if _, err := p4Run(r.Snap.Workspace, env, v.Client, verb, "-c", v.Changelist, p4Escape(file)); err != nil {
		return "", fmt.Errorf("p4 %s failed: %s", verb, strings.TrimSpace(err.Error()))
	}
	return done, nil
}
