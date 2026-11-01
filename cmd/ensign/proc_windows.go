//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
)

// killTreeOnCancel kills the command and everything it started when its context ends; the returned finish func has nothing to do here.
func killTreeOnCancel(c *exec.Cmd, _ context.CancelFunc) func() error {
	c.Cancel = func() error {
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(c.Process.Pid)).Run(); err != nil {
			if err := c.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
		return nil
	}
	return func() error { return nil }
}
