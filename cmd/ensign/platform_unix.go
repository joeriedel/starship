//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// execReplace replaces this process with another program.
func execReplace(path string, args []string) {
	err := syscall.Exec(path, args, os.Environ())
	fmt.Fprintln(os.Stderr, "starship: could not hand off to", path+":", err)
	os.Exit(2)
}

// ensureOnPath reports how to put dir on PATH if it isn't; on Unix it changes nothing.
func ensureOnPath(dir string) (string, error) {
	if onPath(dir) {
		return "", nil
	}
	return fmt.Sprintf("note: %s is not on your PATH; add it in your shell profile so Claude Code's hooks can find ensign", dir), nil
}
