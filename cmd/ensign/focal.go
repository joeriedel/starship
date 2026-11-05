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
)

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
	x := availableExtensions(bridge, cp)[focalExtension]
	if x == nil {
		return "", fmt.Errorf("the %s extension has no %s", focalExtension, rel)
	}
	data, err := os.ReadFile(filepath.Join(x.dir, filepath.FromSlash(rel)))
	return string(data), err
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

// focalProblems are the refusals for a focal rewrite, one per entry and field, naming the entry's id and never a value.
func focalProblems(prefix string, themes []string, next Doc) []string {
	idRe := regexp.MustCompile(`^` + prefix + `\d+$`)
	var out []string
	for _, e := range next.Entries {
		id := e.ID
		if !idRe.MatchString(id) {
			id = "(a malformed id)"
		}
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
