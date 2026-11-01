//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pidGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndex(s, ")"); i >= 0 && len(s) > i+2 && s[i+2] == 'Z' {
			return true
		}
	}
	return false
}

func TestShellForTimeoutKillsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := shellFor(t.TempDir(), "sleep 30 & echo $! > "+pidFile+"; wait", os.Environ(), time.Second)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timed out", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("took %s, want well under the child's sleep", elapsed)
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		t.Fatal(perr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !pidGone(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestShellForLeftoverPipeHolderIsSuccess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	out, err := shellFor(t.TempDir(), "echo supernova; sleep 8 & echo $! > "+pidFile+"; true", os.Environ(), 30*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !strings.Contains(out, "supernova") {
		t.Fatalf("out = %q", out)
	}
	if elapsed > 7*time.Second {
		t.Fatalf("took %s, want about the wait delay", elapsed)
	}
}
