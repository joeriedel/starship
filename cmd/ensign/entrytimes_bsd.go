// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build darwin || freebsd || netbsd

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// entryTimes returns, by name, the later of each entry's modification time and its inode change time, which a rename, copy or in-place overwrite sets even when the file keeps an old modification time. A symlink counts by its own times. ids holds each entry's device and inode, by name, which tell a folder from another renamed onto its path.
func entryTimes(dir string, entries []os.DirEntry) (times map[string]time.Time, ids map[string]string) {
	times = make(map[string]time.Time, len(entries))
	ids = make(map[string]string, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		t := info.ModTime()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			ids[e.Name()] = fmt.Sprintf("%x:%x", uint64(st.Dev), uint64(st.Ino))
			if c := time.Unix(int64(st.Ctimespec.Sec), int64(st.Ctimespec.Nsec)); c.After(t) {
				t = c
			}
		}
		times[e.Name()] = t
	}
	return times, ids
}
