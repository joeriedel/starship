// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	procRoleEnv    = "STARSHIP_TEST_PROC_ROLE"
	procPidFileEnv = "STARSHIP_TEST_PROC_PIDFILE"
)

// TestProcHelper is not a test: shellFor re-runs this test binary with procRoleEnv set, so the commands under test work the same under bash and cmd. Role "sleep" idles for 60s. Role "parent-exit" starts a sleeper without sharing its output pipe, records the sleeper's pid, prints supernova and exits. Role "outer" records its own pid, runs "middle" to completion, then idles for 60s, so it outlives the sleeper's parent. Role "middle" starts a sleeper, records its pid and exits at once.
func TestProcHelper(t *testing.T) {
	role := os.Getenv(procRoleEnv)
	switch role {
	case "":
		t.Skip("only runs as a child process of the shellFor tests")
	case "sleep":
		time.Sleep(60 * time.Second)
	case "parent-exit", "middle":
		startSleeper(t)
		if role == "parent-exit" {
			os.Stdout.WriteString("supernova\n")
		}
	case "outer":
		pidFile := os.Getenv(procPidFileEnv)
		if err := os.WriteFile(pidFile+".outer", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		middle := exec.Command(exe, "-test.run=TestProcHelper")
		middle.Env = append(os.Environ(), procRoleEnv+"=middle")
		if err := middle.Run(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Second)
	default:
		t.Fatalf("unknown role %q", role)
	}
}

// startSleeper starts a "sleep" helper without stdio and writes its pid to the pid file.
func startSleeper(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=TestProcHelper")
	child.Env = append(os.Environ(), procRoleEnv+"=sleep")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(procPidFileEnv), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// helperCommand is a shell command line that runs TestProcHelper, with the env that selects its role.
func helperCommand(t *testing.T, role string) (command string, env []string, pidFile string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile = filepath.Join(t.TempDir(), "pid")
	command = `"` + filepath.ToSlash(exe) + `" -test.run=TestProcHelper`
	env = append(os.Environ(), procRoleEnv+"="+role, procPidFileEnv+"="+pidFile)
	return command, env, pidFile
}

// shellLimit leaves the helper tree ample time to start on a slow runner; the helpers sleep for 60s, well beyond it.
const shellLimit = 10 * time.Second

type shellRun struct {
	done  chan struct{}
	out   string
	err   error
	start time.Time
	limit time.Duration
}

// startShell runs shellFor in the background so the test can watch the process tree while it runs. Cleanup waits for it to finish.
func startShell(t *testing.T, command string, env []string, limit time.Duration) *shellRun {
	dir := t.TempDir()
	r := &shellRun{done: make(chan struct{}), start: time.Now(), limit: limit}
	go func() {
		r.out, r.err = shellFor(dir, command, env, limit)
		close(r.done)
	}()
	t.Cleanup(func() { <-r.done })
	return r
}

// readPid reads a pid file written by a helper.
func readPid(pidFile string) (uint32, bool) {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return uint32(pid), err == nil
}

// awaitPid polls for a pid file written by a helper. A pid file that appears after the shell's limit is useless, so callers pass at most the limit.
func awaitPid(t *testing.T, pidFile string, wait time.Duration) uint32 {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if pid, ok := readPid(pidFile); ok {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s never appeared", pidFile)
	return 0
}

// inFlight reports whether the run's deadline has not yet passed. start precedes shellFor's own deadline, so this errs towards false.
func (r *shellRun) inFlight() bool {
	return !r.finished() && time.Since(r.start) < r.limit
}

func (r *shellRun) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// openProc opens a handle to a process that is known to be alive, and terminates and closes it at cleanup. All probing and killing goes through the handle so a reused pid can never be hit.
func openProc(t *testing.T, pid uint32) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	t.Cleanup(func() {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	})
	return h
}

// openInFlight is openProc for a helper of a run that must still be in flight. Once the run's deadline has passed its job is being or has been terminated and the pid may have been reused, so the handle is closed untouched and ok is false.
func openInFlight(t *testing.T, run *shellRun, pid uint32) (h windows.Handle, ok bool) {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		if !run.inFlight() {
			return 0, false
		}
		t.Fatalf("open process %d: %v", pid, err)
	}
	if !run.inFlight() {
		windows.CloseHandle(h)
		return 0, false
	}
	t.Cleanup(func() {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	})
	return h, true
}

// exitedWithin reports whether the process exits within d.
func exitedWithin(h windows.Handle, d time.Duration) bool {
	ev, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// watchTree waits for the helper tree to come up within the limit and returns handles to the sleeper and the outer helper. Registering the handles after startShell means they are terminated before the shell run is waited on.
func watchTree(t *testing.T, run *shellRun, pidFile string, limit time.Duration) (sleeper, outer windows.Handle) {
	t.Helper()
	var ok bool
	sleeper, ok = openInFlight(t, run, awaitPid(t, pidFile, limit))
	if !ok {
		t.Fatal("helper tree came up after the limit")
	}
	outer, ok = openInFlight(t, run, awaitPid(t, pidFile+".outer", limit))
	if !ok {
		t.Fatal("helper tree came up after the limit")
	}
	return sleeper, outer
}

func TestShellForTimeoutKillsChildren(t *testing.T) {
	command, env, pidFile := helperCommand(t, "outer")
	start := time.Now()
	run := startShell(t, command, env, shellLimit)
	sleeper, outer := watchTree(t, run, pidFile, shellLimit)
	if exitedWithin(sleeper, 0) || exitedWithin(outer, 0) {
		t.Fatal("helpers exited before the timeout")
	}
	<-run.done
	elapsed := time.Since(start)
	if run.err == nil || !strings.Contains(run.err.Error(), "timed out") {
		t.Fatalf("err = %v, want timed out", run.err)
	}
	if strings.Contains(run.out, "job object unavailable") {
		t.Fatalf("out = %q, the job object was not used", run.out)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("took %s, want well under the child's sleep", elapsed)
	}
	if !exitedWithin(sleeper, 2*time.Second) {
		t.Fatal("sleeper outlived its parent and the timeout")
	}
	if !exitedWithin(outer, 2*time.Second) {
		t.Fatal("outer helper still running")
	}
}

func TestShellForLeftoverChildIsSuccess(t *testing.T) {
	command, env, pidFile := helperCommand(t, "parent-exit")
	start := time.Now()
	run := startShell(t, command, env, 30*time.Second)
	<-run.done
	elapsed := time.Since(start)
	if run.err != nil {
		t.Fatalf("err = %v, want nil", run.err)
	}
	if !strings.Contains(run.out, "supernova") {
		t.Fatalf("out = %q", run.out)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("took %s, want well under the child's sleep", elapsed)
	}
	sleeper := openProc(t, awaitPid(t, pidFile, shellLimit))
	if exitedWithin(sleeper, 0) {
		t.Fatal("background child did not survive")
	}
}

func TestShellForTaskkillFallbackIsNoted(t *testing.T) {
	orig := assignToJobFn
	assignToJobFn = func(windows.Handle, uint32) error { return errors.New("no job for you") }
	t.Cleanup(func() { assignToJobFn = orig })
	command, env, pidFile := helperCommand(t, "outer")
	run := startShell(t, command, env, shellLimit)
	// Taskkill may take down the shell and outer before the sleeper starts, so the tree is only opened for cleanup and nothing is required of it. A pid file that appears after the run's deadline is ignored.
	opened := map[string]bool{}
	for !run.finished() {
		for _, f := range []string{pidFile, pidFile + ".outer"} {
			if pid, ok := readPid(f); ok && !opened[f] {
				opened[f] = true
				openInFlight(t, run, pid)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-run.done
	if run.err == nil || !strings.Contains(run.err.Error(), "timed out") {
		t.Fatalf("err = %v, want timed out", run.err)
	}
	if !strings.Contains(run.out, "job object unavailable") {
		t.Fatalf("out = %q, want the fallback note", run.out)
	}
}
