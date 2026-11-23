// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// mailLineFields reads what a conversion needs from a mail-record line without requiring the rest of it to parse: the message id when the line starts like a mail record, and the received month when its received date is valid.
func mailLineFields(line string) (id, month string) {
	f := strings.SplitN(line, " | ", 7)
	if len(f) < 3 || mailSourceRe.FindStringSubmatch(f[0]) == nil || strings.TrimSpace(f[2]) == "" {
		return "", ""
	}
	if len(f) >= 4 && validDate(f[3]) {
		return f[2], f[3][:7]
	}
	return f[2], ""
}

// sameMailRecord reports whether an existing line of a month file stands for the same message as a line being converted: the same message id, or the same whole line when the converted line has no id.
func sameMailRecord(existing, line, id string) bool {
	if id == "" {
		return existing == line
	}
	eid, _ := mailLineFields(existing)
	return eid == id
}

// migrateMailRead converts the old layout of the reader's mail records, one reader/mail-read/<source>.md file per source, into one reader/mail-read/<source>/<YYYY-MM>.md file per received month. Lines with no readable received date go to undated.md. A line whose message id is already in its month file is skipped, and an old file is removed only once all its month files are written, so a rerun after an interruption finishes without duplicates. It says what it did, or "" when there was nothing to convert.
func migrateMailRead(bridge string) (string, error) {
	dir := filepath.Join(bridge, "reader", "mail-read")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("converting the reader's mail records: %v", err)
	}
	converted := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") || !e.Type().IsRegular() {
			continue
		}
		done, err := migrateMailSource(bridge, dir, e.Name())
		if err != nil {
			return "", err
		}
		if done {
			converted++
		}
	}
	if converted == 0 {
		return "", nil
	}
	return fmt.Sprintf("starship: converted the reader's mail records of %d source(s) into one file per received month under reader/mail-read/<source>/.", converted), nil
}

// migrateMailSource converts one old source file in dir. An old file that is already gone was converted by a concurrent run, so it is skipped and reports false.
func migrateMailSource(bridge, dir, name string) (bool, error) {
	source := strings.TrimSuffix(name, ".md")
	oldPath := filepath.Join(dir, name)
	data, err := os.ReadFile(oldPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("converting %s: %v; the old file is left in place", oldPath, err)
	}
	files := map[string][]string{}
	var order []string
	for _, line := range splitStateLines(data) {
		id, month := mailLineFields(line)
		name := "undated"
		if month != "" {
			name = month
		}
		path := filepath.Join(dir, source, name+".md")
		lines, ok := files[path]
		if !ok {
			existing, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				return false, fmt.Errorf("converting %s: %v; the old file is left in place", oldPath, err)
			}
			lines = splitStateLines(existing)
			order = append(order, path)
		}
		if !slices.ContainsFunc(lines, func(l string) bool { return sameMailRecord(l, line, id) }) {
			lines = append(lines, line)
		}
		files[path] = lines
	}
	for _, path := range order {
		if err := writeFileAtomic(bridge, path, []byte(strings.Join(files[path], "\n")+"\n")); err != nil {
			return false, fmt.Errorf("converting %s: writing %s: %v; the old file is left in place", oldPath, path, err)
		}
	}
	if err := os.Remove(oldPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("converting %s: removing it: %v; its lines are already in %s, so run the conversion again", oldPath, err, filepath.Join(dir, source))
	}
	return true, nil
}

// readerMailRefusal converts the mail records before a reader is dispatched, and refuses the dispatch only when the conversion fails.
func readerMailRefusal(bridge string, in *HookInput) string {
	if sub, _ := in.ToolInput["subagent_type"].(string); sub != "reader" {
		return ""
	}
	if _, err := migrateMailRead(bridge); err != nil {
		return "the reader's mail records are in the old layout and could not be converted: " + err.Error()
	}
	return ""
}
