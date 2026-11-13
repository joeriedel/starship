// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build !windows && !linux && !darwin && !freebsd && !netbsd

package main

import (
	"os"
	"time"
)

// entryTimes returns nothing on a system with no known inode change time or folder identity; the caller falls back to each entry's modification time.
func entryTimes(dir string, entries []os.DirEntry) (times map[string]time.Time, ids map[string]string) {
	return nil, nil
}
