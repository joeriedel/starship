// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

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
	"time"
)

// killTreeOnCancel puts the command in its own process group and kills the whole group when its context ends or ensign is sent SIGINT, SIGTERM or SIGHUP. The returned started func is a no-op here. The returned finish func must be called after the command ends; if a signal was caught it returns an error, after re-raising the signal to ensign with the default action unless signals are held (see holdSignals), in which case it only notes the signal. While signals are shielded (see shieldSignals) the command is not cancelled by them at all.
func killTreeOnCancel(c *exec.Cmd, cancel context.CancelFunc) (started func(), finish func() error) {
	started = func() {}
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
	sigs := cancelSignals()
	if len(sigs) == 0 || shielded() {
		return started, func() error { return nil }
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
	return started, func() error {
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
			if !noteSignal(sig) {
				raiseSignal(sig)
			}
		}
		return fmt.Errorf("interrupted by %s", caught)
	}
}

// cancelSignals are the signals that cancel a command. Notify would un-ignore a signal ensign inherited as ignored (nohup, background job), so those are left alone.
func cancelSignals() []os.Signal {
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	return sigs
}

// watchSignals notes cancel signals with noteSignal until stop is called, so that one arriving between commands is not lost.
func watchSignals() (stop func()) {
	sigs := cancelSignals()
	if len(sigs) == 0 {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case sig := <-ch:
				noteSignal(sig)
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		wg.Wait()
		select {
		case sig := <-ch:
			noteSignal(sig)
		default:
		}
	}
}

// raiseSignal delivers sig to ensign with its default action.
func raiseSignal(sig os.Signal) {
	if s, ok := sig.(syscall.Signal); ok {
		signal.Reset(s)
		syscall.Kill(os.Getpid(), s)
		// The signal can reach another thread after kill returns; ensign must not carry on past it.
		time.Sleep(5 * time.Second)
	}
}
