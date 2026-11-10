// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A reader writes only its own source's cursor and mail-record files, so parallel readers never overwrite each other. The lines of the old shared files, reader/cursors.md and reader/mail-read.md, are split into those files by migrateReaderState.
var (
	cursorLineRe = regexp.MustCompile(`^- ([a-z0-9][a-z0-9-]*) (.+): (.+) \((\d{4}-\d{2}-\d{2})(?: read)?\)$`)
	mailSourceRe = regexp.MustCompile(`^- ([a-z0-9][a-z0-9-]*)$`)
)

// stateLine is one parsed line of an old shared file: the per-source file it belongs in, its key within that file, and its read date.
type stateLine struct {
	text, source, key, date string
}

func parseCursorLine(line string) (stateLine, bool) {
	m := cursorLineRe.FindStringSubmatch(line)
	if m == nil || !validDate(m[4]) {
		return stateLine{}, false
	}
	return stateLine{text: line, source: m[1], key: m[2], date: m[4]}, true
}

func parseMailLine(line string) (stateLine, bool) {
	f := strings.SplitN(line, " | ", 7)
	if len(f) != 7 {
		return stateLine{}, false
	}
	m := mailSourceRe.FindStringSubmatch(f[0])
	if m == nil || !validDate(f[3]) || !validDate(f[4]) {
		return stateLine{}, false
	}
	return stateLine{text: line, source: m[1], key: f[2], date: f[4]}, true
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// splitStateLines returns the non-blank lines of data with line endings trimmed, so CRLF files migrate cleanly.
func splitStateLines(data []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// mergeStateLine adds one line to a per-source file's lines: an exact duplicate collapses, a line with the same key as existing ones replaces them when it is read later and is dropped when they are read later, and otherwise it is appended.
func mergeStateLine(lines []string, parse func(string) (stateLine, bool), add stateLine) []string {
	if slices.Contains(lines, add.text) {
		return lines
	}
	var same []int
	for i, l := range lines {
		if p, ok := parse(l); ok && p.key == add.key {
			if p.date > add.date {
				return lines
			}
			same = append(same, i)
		}
	}
	older := len(same) > 0
	for _, i := range same {
		p, _ := parse(lines[i])
		older = older && p.date < add.date
	}
	if !older {
		return append(lines, add.text)
	}
	lines[same[0]] = add.text
	for _, i := range slices.Backward(same[1:]) {
		lines = slices.Delete(lines, i, i+1)
	}
	return lines
}

// migrateReaderState moves the lines of reader/cursors.md and reader/mail-read.md into the per-source files and removes the old files once every write has succeeded. Lines that cannot be read are kept in reader/cursors/unparsed.md.
func migrateReaderState(bridge string) error {
	reader := filepath.Join(bridge, "reader")
	unparsedPath := filepath.Join(reader, "cursors", "unparsed.md")
	type oldFile struct {
		path, dir string
		parse     func(string) (stateLine, bool)
	}
	olds := []oldFile{
		{filepath.Join(reader, "cursors.md"), "cursors", parseCursorLine},
		{filepath.Join(reader, "mail-read.md"), "mail-read", parseMailLine},
	}
	files := map[string][]string{}
	load := func(path string) ([]string, error) {
		if lines, ok := files[path]; ok {
			return lines, nil
		}
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return splitStateLines(data), nil
	}
	var found []oldFile
	for _, o := range olds {
		data, err := os.ReadFile(o.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("migrating %s: %v; the old file is left in place", o.path, err)
		}
		found = append(found, o)
		for _, line := range splitStateLines(data) {
			path, add := unparsedPath, stateLine{text: line}
			p, ok := o.parse(line)
			if ok {
				path, add = filepath.Join(reader, o.dir, p.source+".md"), p
			}
			lines, err := load(path)
			if err != nil {
				return fmt.Errorf("migrating %s: %v; the old file is left in place", o.path, err)
			}
			if ok {
				lines = mergeStateLine(lines, o.parse, add)
			} else if !slices.Contains(lines, line) {
				lines = append(lines, line)
			}
			files[path] = lines
		}
	}
	for path, lines := range files {
		if err := writeFileAtomic(bridge, path, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
			return fmt.Errorf("migrating the old reader files: writing %s: %v; the old files are left in place", path, err)
		}
	}
	for _, o := range found {
		if err := os.Remove(o.path); err != nil {
			return fmt.Errorf("migrating %s: removing it: %v; its lines are already in the per-source files, so run the briefing again", o.path, err)
		}
	}
	return nil
}
