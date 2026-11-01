//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// killTreeOnCancel puts the command in its own process group and kills the whole group when its context ends or ensign is sent SIGINT, SIGTERM or SIGHUP. The returned finish func must be called after the command ends; if a signal was caught it re-raises it to ensign with the default action and returns an error.
func killTreeOnCancel(c *exec.Cmd, cancel context.CancelFunc) func() error {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err := c.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
	// Notify would un-ignore a signal ensign inherited as ignored (nohup, background job), so those are left alone.
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return func() error { return nil }
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	var wg sync.WaitGroup
	var caught os.Signal
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case caught = <-ch:
			cancel()
		case <-done:
		}
	}()
	return func() error {
		signal.Stop(ch)
		close(done)
		wg.Wait()
		if caught == nil {
			select {
			case caught = <-ch:
			default:
				return nil
			}
		}
		if sig, ok := caught.(syscall.Signal); ok {
			if c.Process != nil {
				syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			}
			signal.Reset(sig)
			syscall.Kill(os.Getpid(), sig)
		}
		return fmt.Errorf("interrupted by %s", caught)
	}
}
