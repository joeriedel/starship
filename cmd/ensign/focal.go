// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// The focal-sheet extension keeps a themed brag sheet as a declared document. Its themes come from the document's rubric or from the extension's shipped default, and its entries carry a theme, why and part. Nothing here puts an entry's text into an error.

const (
	focalExtension = "focal-sheet"
	focalPrefix    = "notes/focal/"
	// focalStylePath is the bridge-wide style file that sets the voice of every brag file.
	focalStylePath = "notes/focal_style.md"
)

func focalStyleFile(bridge string) string {
	return filepath.Join(bridge, filepath.FromSlash(focalStylePath))
}

// isFocalDocument reports whether d is a focal document: under notes/focal/ in a bridge whose own bridge.yaml uses focal-sheet.
func isFocalDocument(bridge string, d DeclaredDocument) bool {
	return strings.HasPrefix(strings.ToLower(d.Path), focalPrefix) && bridgeUses(bridge, focalExtension)
}

// parseThemes is the "## " headings of a Markdown text, in order, ignoring CRLF and anything inside a code fence.
func parseThemes(text string) []string {
	var out []string
	fence := ""
	text = strings.TrimPrefix(text, "\ufeff")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if marker := fenceMarker(trimmed); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "":
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if h, ok := strings.CutPrefix(line, "## "); ok && strings.TrimSpace(h) != "" {
			out = append(out, strings.TrimSpace(h))
		}
	}
	return out
}

// fenceMarker is the run of backticks or tildes that opens a fence on the trimmed line, or "".
func fenceMarker(trimmed string) string {
	for _, c := range []string{"`", "~"} {
		n := len(trimmed) - len(strings.TrimLeft(trimmed, c))
		if n >= 3 {
			return strings.Repeat(c, n)
		}
	}
	return ""
}

// focalFile reads a file of the focal-sheet extension, found by its declared name.
func focalFile(bridge, rel string) (string, error) {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return "", err
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return "", err
	}
	good, broken := scanExtensions(bridge, cp)
	x := good[focalExtension]
	if x == nil {
		if berr := broken[focalExtension]; berr != nil {
			return "", fmt.Errorf("the %s extension is broken: %w", focalExtension, berr)
		}
		return "", fmt.Errorf("the %s extension has no %s", focalExtension, rel)
	}
	data, err := os.ReadFile(filepath.Join(x.dir, filepath.FromSlash(rel)))
	return string(data), err
}

// focalNote is the trimmed text of a focal-sheet file for a hook to print: empty when the bridge doesn't use the extension, and a one-line notice when the file can't be read.
func focalNote(bridge, rel string) string {
	if !bridgeUses(bridge, focalExtension) {
		return ""
	}
	text, err := focalFile(bridge, rel)
	if err != nil {
		return fmt.Sprintf("the focal-sheet extension's %s can't be read: %v", rel, err)
	}
	return strings.TrimSpace(text)
}

// focalThemes is the document's themes: its rubric's headings, or the shipped defaults when it declares none. An empty list is an error.
func focalThemes(bridge string, d DeclaredDocument) ([]string, error) {
	var text string
	if d.Rubric != "" {
		data, err := os.ReadFile(d.rubricFile(bridge))
		if err != nil {
			return nil, fmt.Errorf("its rubric %s cannot be read", d.Rubric)
		}
		text = string(data)
	} else {
		var err error
		if text, err = focalFile(bridge, "briefs/default-themes.md"); err != nil {
			return nil, err
		}
	}
	themes := parseThemes(text)
	if len(themes) == 0 {
		return nil, fmt.Errorf("it has no themes: a theme is a line starting with \"## \"")
	}
	return themes, nil
}

// focalEntryID is an entry's id for a message, or a placeholder when it is malformed, since a bad id field could hold anything.
func focalEntryID(prefix, id string) string {
	if !regexp.MustCompile(`^` + prefix + `\d+$`).MatchString(id) {
		return "(a malformed id)"
	}
	return id
}

// focalProblems are the refusals for a focal rewrite, one per entry and field, naming the entry's id and never a value.
func focalProblems(prefix string, themes []string, next Doc) []string {
	var out []string
	for _, e := range next.Entries {
		id := focalEntryID(prefix, e.ID)
		if strings.TrimSpace(e.Part) == "" {
			out = append(out, fmt.Sprintf("entry %s: part is missing", id))
		}
		if !slices.Contains(themes, strings.TrimSpace(e.Theme)) {
			out = append(out, fmt.Sprintf("entry %s: theme is missing or not one of the listed themes", id))
		}
		if !slices.ContainsFunc(e.Evidence, func(ev string) bool { return strings.TrimSpace(ev) != "" }) {
			out = append(out, fmt.Sprintf("entry %s: evidence is missing", id))
		}
	}
	return out
}

// sortByTheme orders entries by theme order, keeping the order within a theme and every id.
func sortByTheme(d *Doc, themes []string) {
	rank := func(e Entry) int { return slices.Index(themes, strings.TrimSpace(e.Theme)) }
	sort.SliceStable(d.Entries, func(i, j int) bool { return rank(d.Entries[i]) < rank(d.Entries[j]) })
}

// leftOutEntries are the entries, candidates included, whose theme is not in themes, which the brag file leaves out unless the instructions bring them in.
func leftOutEntries(themes []string, d Doc) []Entry {
	var out []Entry
	for _, e := range d.Entries {
		if !slices.Contains(themes, strings.TrimSpace(e.Theme)) {
			out = append(out, e)
		}
	}
	return out
}

// focalNameLimit caps each name or path a focal warning prints.
const focalNameLimit = 40

// capFocalWarnings keeps the first warning and counts the rest.
func capFocalWarnings(out []string) []string {
	if len(out) < 2 {
		return out
	}
	return []string{fmt.Sprintf("%s And %d more like it.", out[0], len(out)-1)}
}

// focalWarnings are session-start notes about a focal-sheet setup that has no effect. They reach the session start text, which has a fixed budget, so names are cut and at most one line is returned.
func focalWarnings(bridge string) []string {
	var out []string
	if bridgeUses(bridge, focalExtension) {
		docs, err := loadDeclaredDocuments(bridge)
		if err != nil {
			return nil
		}
		for _, d := range docs {
			path := strings.ToLower(d.Path)
			if !strings.HasPrefix(path, focalPrefix) && (strings.Contains(path, "focal") || strings.Contains(strings.ToLower(d.Name), "focal")) {
				out = append(out, fmt.Sprintf("the document %s (%s) sits outside %s, so it is curated as a plain document with no focal rules.", truncate(d.Name, focalNameLimit), truncate(d.Path, focalNameLimit), focalPrefix))
			}
		}
		return capFocalWarnings(out)
	}
	loaded := func(who string) {
		out = append(out, fmt.Sprintf("%s names the %s extension in its use: list, but that has no focal effect there: the sheet reads use: in bridge.yaml only.", who, focalExtension))
	}
	projects, _ := os.ReadDir(filepath.Join(bridge, "projects"))
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		var pl Layer
		if readYAML(filepath.Join(bridge, "projects", p.Name(), "project.yaml"), &pl) == nil && slices.Contains(pl.Use, focalExtension) {
			loaded("project " + truncate(p.Name(), focalNameLimit))
		}
		tasks, _ := os.ReadDir(filepath.Join(bridge, "projects", p.Name(), "tasks"))
		for _, t := range tasks {
			front, _, err := splitFrontmatter(filepath.Join(bridge, "projects", p.Name(), "tasks", t.Name(), "task.md"))
			if err != nil {
				continue
			}
			var tf struct {
				Use    []string `yaml:"use"`
				Status string   `yaml:"status"`
			}
			if decodeFrontmatter(front, &tf) == nil && tf.Status != "closed" && slices.Contains(tf.Use, focalExtension) {
				loaded("task " + truncate(p.Name()+"/"+t.Name(), focalNameLimit))
			}
		}
	}
	return capFocalWarnings(out)
}
