// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// designFile is one Markdown file under a project's design folder.
type designFile struct {
	Name    string // path relative to the design folder, with forward slashes
	Path    string // absolute path
	Summary string // the file's first non-blank line, without heading markers
}

func designDir(bridge, project string) string {
	return filepath.Join(bridge, "projects", project, "design")
}

// designFiles lists a project's design files sorted by name; a project with no design folder has none.
func designFiles(bridge, project string) []designFile {
	dir := designDir(bridge, project)
	var files []designFile
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		files = append(files, designFile{Name: filepath.ToSlash(rel), Path: path, Summary: designSummary(path)})
		return nil
	})
	slices.SortFunc(files, func(a, b designFile) int { return strings.Compare(a.Name, b.Name) })
	return files
}

// designSummary reads only as far as the first non-blank line.
func designSummary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.ReplaceAll(sc.Text(), "\r", ""))
		if line != "" {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return ""
}

// checkDesignNames refuses names that are not design files of the project; file is the file holding the design: key.
func checkDesignNames(bridge, project, file string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	files := designFiles(bridge, project)
	for _, n := range names {
		if !slices.ContainsFunc(files, func(f designFile) bool { return f.Name == n }) {
			return fmt.Errorf("%s: design: %q is not a design file (none in projects/%s/design/ by that name)", file, n, project)
		}
	}
	return nil
}

// designListing is the design block of task open, empty when the project has no design files.
func designListing(bridge, project string, review []string) string {
	files := designFiles(bridge, project)
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "design     projects/%s/design/ (* review: named by this task or its next subtask)\n", project)
	for _, f := range files {
		mark := "  "
		if slices.Contains(review, f.Name) {
			mark = "* "
		}
		b.WriteString("  " + mark + f.Name)
		if f.Summary != "" {
			b.WriteString(" - " + f.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// designSection is the run work item's design section, empty when the project has no design files.
func designSection(bridge, project string, review []string) string {
	files := designFiles(bridge, project)
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Design\n\nThe project's design files; read the ones pertinent to this work. Those marked (review) are named by this task or subtask: check your work against them.\n\n")
	for _, f := range files {
		b.WriteString("- " + f.Path)
		if f.Summary != "" {
			b.WriteString(" — " + f.Summary)
		}
		if slices.Contains(review, f.Name) {
			b.WriteString(" (review)")
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
