// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build windows

package main

import (
	"os"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileIDBothDirInfoName is the offset of FileName in FILE_ID_BOTH_DIR_INFO; LastWriteTime is at 24, ChangeTime at 32, FileNameLength at 60, FileId at 96.
const fileIDBothDirInfoName = 104

// entryTimes returns, by name, the later of each entry's last-write time and its NTFS change time, which a copy or overwrite sets even when the file keeps the source's last-write time. The directory is opened once and enumerated; a symlink counts by its own times. The end of the listing and any failure both end it, and the caller falls back to each entry's modification time.
func entryTimes(dir string, entries []os.DirEntry) (times map[string]time.Time, ids map[string]string) {
	times = make(map[string]time.Time, len(entries))
	ids = make(map[string]string, len(entries))
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return times, ids
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return times, ids
	}
	defer windows.CloseHandle(h)
	raw := make([]uint64, 8192)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&raw[0])), len(raw)*8)
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	for {
		if err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf))); err != nil {
			return times, ids
		}
		class = windows.FileIdBothDirectoryInfo
		for off := uint32(0); ; {
			lastWrite := *(*int64)(unsafe.Pointer(&buf[off+24]))
			change := *(*int64)(unsafe.Pointer(&buf[off+32]))
			fileID := *(*uint64)(unsafe.Pointer(&buf[off+96]))
			n := *(*uint32)(unsafe.Pointer(&buf[off+60]))
			name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off+fileIDBothDirInfoName])), n/2))
			if change < lastWrite {
				change = lastWrite
			}
			ids[name] = strconv.FormatUint(fileID, 16)
			times[name] = time.Unix(0, (change-116444736000000000)*100)
			next := *(*uint32)(unsafe.Pointer(&buf[off]))
			if next == 0 {
				break
			}
			off += next
		}
	}
}
