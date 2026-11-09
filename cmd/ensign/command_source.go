// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// commandOutputCap is the most stdout bytes a command source keeps; commandStderrKeep is how much of the tail of stderr is kept for a failure line; commandStderrLines is how many of its last non-empty lines the brief shows.
	commandOutputCap   = 1 << 20
	commandStderrKeep  = 64 << 10
	commandStderrLines = 5
)

// commandFailure is why a command source could not be read this pass, with the last lines of its stderr.
type commandFailure struct {
	Reason string
	Stderr []string
}

// capWriter keeps the first max bytes written and discards the rest, so a chatty child never blocks on a full pipe.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); len(p) <= room {
		w.buf.Write(p)
	} else {
		if room > 0 {
			w.buf.Write(p[:room])
		}
		w.over = true
	}
	return len(p), nil
}

// tailWriter keeps the last max bytes written.
type tailWriter struct {
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}

// commandSourcePath is a command source's output file (ext ".out") or its previous pass's (".prev"), relative to the bridge with forward slashes.
func commandSourcePath(name, ext string) string { return path.Join("reader", "command", name+ext) }

// isCommandOutput reports whether a bridge-relative path is a command source's .out or .prev file under reader/command/, with no hidden segment.
func isCommandOutput(p string) bool {
	if !strings.HasPrefix(p, "reader/command/") {
		return false
	}
	if ext := path.Ext(p); ext != ".out" && ext != ".prev" {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// commandEnv is the environment with the source's entries set on top, replacing a name already present (in any case on Windows).
func commandEnv(base []string, set map[string]string) []string {
	var env []string
	for _, e := range base {
		name, _, _ := strings.Cut(e, "=")
		replaced := false
		for k := range set {
			if name == k || (runtime.GOOS == "windows" && strings.EqualFold(name, k)) {
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, e)
		}
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		env = append(env, k+"="+set[k])
	}
	return env
}

// runCommandSource runs a command source's program directly, with no shell, from the bridge folder, and on success keeps its output as reader/command/<name>.out with the one before as .prev. A failed run is returned as a commandFailure and touches no file. The error is for a signal to ensign or a file that can't be written, which end the pass.
func runCommandSource(bridge string, s DeclaredSource) (*commandFailure, error) {
	if sig := caughtSignal(); sig != nil {
		return nil, fmt.Errorf("interrupted by %s", sig)
	}
	timeout := time.Duration(s.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	cmd.Dir = bridge
	cmd.Env = commandEnv(os.Environ(), s.Env)
	started, finish := killTreeOnCancel(cmd, cancel)
	cmd.WaitDelay = killWaitDelay
	stdout := &capWriter{max: commandOutputCap}
	stderr := &tailWriter{max: commandStderrKeep}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Start()
	if err == nil {
		started()
		err = cmd.Wait()
	}
	if cmd.Process != nil {
		cmd.Cancel()
	}
	sigErr := finish()
	fail := func(reason string) *commandFailure {
		var lines []string
		for _, l := range strings.Split(string(stderr.buf), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) > commandStderrLines {
			lines = lines[len(lines)-commandStderrLines:]
		}
		return &commandFailure{Reason: reason, Stderr: lines}
	}
	switch {
	case sigErr != nil:
		return nil, sigErr
	case ctx.Err() != nil:
		return fail(fmt.Sprintf("timed out after %ds", s.Timeout)), nil
	case err != nil && !errors.Is(err, exec.ErrWaitDelay):
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fail(err.Error()), nil
		}
		return fail("could not run: " + err.Error()), nil
	}
	data := stdout.buf.Bytes()
	if stdout.over {
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		data = append(data, "[ensign: output cut at 1 MB]\n"...)
	}
	return nil, writeCommandOutput(bridge, s.Name, data)
}

// writeCommandOutput makes data the source's .out file and the old .out its .prev, through a hidden temp file in the same folder so a crash leaves no half file.
func writeCommandOutput(bridge, name string, data []byte) error {
	dir := filepath.Join(bridge, "reader", "command")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	out := filepath.Join(dir, name+".out")
	if err := os.Rename(out, filepath.Join(dir, name+".prev")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmp.Name(), out)
}

// printCommandSource prints the lines under a command source in the brief: where its output is, or why it is unreadable this pass.
func printCommandSource(out io.Writer, s DeclaredSource, f *commandFailure) {
	outPath, prevPath := commandSourcePath(s.Name, ".out"), commandSourcePath(s.Name, ".prev")
	if f == nil {
		fmt.Fprintf(out, "  - output: %s (the previous pass's output: %s, which may not exist yet)\n", outPath, prevPath)
		fmt.Fprintf(out, "  - %s\n", kindNote("command", false))
		return
	}
	fmt.Fprintf(out, "  - unreadable this pass: %s\n", f.Reason)
	for _, l := range f.Stderr {
		fmt.Fprintf(out, "    stderr: %s\n", l)
	}
	fmt.Fprintf(out, "  - report it as unreadable; %s and %s, if they exist, are from an earlier pass and are not this pass's state\n", outPath, prevPath)
}
