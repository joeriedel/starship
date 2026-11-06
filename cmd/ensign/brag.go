// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A focal document's brag file is written beside its sheet by an agent, from the sheet, the document's instructions file and the bridge-wide style file. Everything here is private: no content reaches an error, a log line, a summary or a note.

const (
	bragBegin = "=== starship-brag begin ==="
	bragEnd   = "=== starship-brag end ==="

	feedbackRelayNote = "relay each feedback agent's findings to the user; they quote the document, so keep them out of commits, journals and summaries"
)

// privateStage reports whether a stage handles private document content, which stays out of its dispatches' summaries, fields and facts.
func privateStage(s *Stage) bool {
	switch s.Curates {
	case "document", "brag", "brag-feedback":
		return true
	}
	return false
}

// bragPath is the bridge-relative path of a focal sheet's brag file: the sheet's path with .md replaced by .brag.md.
func bragPath(decl DeclaredDocument) string {
	return decl.Path[:len(decl.Path)-len(".md")] + ".brag.md"
}

// noteOnce adds a run note unless the run already has it.
func (st *State) noteOnce(s string) {
	if !slices.Contains(st.Notes, s) {
		st.note(s)
	}
}

// fencedText wraps text in a code fence longer than any run of backticks inside it.
func fencedText(text string) string {
	longest, run := 0, 0
	for _, c := range text {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + strings.TrimRight(text, "\n") + "\n" + fence
}

// readOptional reads a file that may be missing: found is false then, and the text is empty.
func readOptional(file string) (text string, found bool, err error) {
	data, err := readFileRetry(file)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return string(data), err == nil, err
}

// bragInputs is the text of a section for each input file of a document's brag, with none-given lines for the absent ones.
type bragInputs struct {
	sheet, instructions, style          string
	hasSheet, hasInstructions, hasStyle bool
}

func readBragInputs(bridge string, decl DeclaredDocument) (in bragInputs, err error) {
	if in.sheet, in.hasSheet, err = readOptional(decl.file(bridge)); err != nil {
		return in, fmt.Errorf("the sheet %s cannot be read", decl.Path)
	}
	if decl.Instructions != "" {
		if in.instructions, in.hasInstructions, err = readOptional(decl.instructionsFile(bridge)); err != nil {
			return in, fmt.Errorf("the instructions file %s cannot be read", decl.Instructions)
		}
	}
	if in.style, in.hasStyle, err = readOptional(focalStyleFile(bridge)); err != nil {
		return in, fmt.Errorf("the style file %s cannot be read", focalStylePath)
	}
	return in, nil
}

func (in bragInputs) sheetSection(decl DeclaredDocument) string {
	if !in.hasSheet {
		return fmt.Sprintf("## Focal sheet (%s)\n\nNone yet: the sheet file does not exist.\n\n", decl.Path)
	}
	return fmt.Sprintf("## Focal sheet (%s)\n\n%s\n\n", decl.Path, fencedText(in.sheet))
}

func (in bragInputs) instructionsSection(decl DeclaredDocument) string {
	switch {
	case decl.Instructions == "":
		return "## Instructions file\n\nNone given: this document declares no instructions file.\n\n"
	case !in.hasInstructions:
		return fmt.Sprintf("## Instructions file (%s)\n\nNone given: the declared file does not exist.\n\n", decl.Instructions)
	}
	return fmt.Sprintf("## Instructions file (%s)\n\n%s\n\n", decl.Instructions, fencedText(in.instructions))
}

// bragContext is what a brag agent is given about its document. It records on the dispatch the notes for what the agent did not get, for applyBrag to add once the file is written.
func bragContext(r *RunFiles, name string, d *Dispatch) (string, error) {
	bridge := r.Snap.Bridge
	decl, err := findDeclaredDocument(bridge, name)
	if err != nil {
		return "", err
	}
	in, err := readBragInputs(bridge, decl)
	if err != nil {
		return "", err
	}
	themes, err := focalThemes(bridge, decl)
	if err != nil {
		return "", fmt.Errorf("the themes of document %s cannot be read: %v", name, err)
	}
	sheet, err := loadDocumentFile(decl.file(bridge))
	if err != nil {
		return "", fmt.Errorf("document %s %v", name, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Document: %s\n\n", name)
	b.WriteString(in.sheetSection(decl))
	b.WriteString(in.instructionsSection(decl))
	if in.hasStyle {
		fmt.Fprintf(&b, "## Style file (%s)\n\n%s\n\n", focalStylePath, fencedText(in.style))
	} else {
		fmt.Fprintf(&b, "## Style file (%s)\n\nNone given: the file does not exist.\n\n", focalStylePath)
	}
	b.WriteString("## Listed themes\n\n")
	for _, th := range themes {
		fmt.Fprintf(&b, "- %s\n", th)
	}
	var notes []string
	if !in.hasInstructions {
		notes = append(notes, name+" ran without an instructions file")
	}
	if !in.hasStyle {
		notes = append(notes, "curation ran without "+focalStylePath)
	}
	b.WriteString("\n## Entries left out\n\n")
	if left := leftOutEntries(themes, sheet); len(left) > 0 {
		ids := make([]string, len(left))
		for i, e := range left {
			ids[i] = focalEntryID("D", e.ID)
		}
		list := strings.Join(ids, ", ")
		fmt.Fprintf(&b, "These entries have no listed theme and are left out unless the instructions bring them in: %s.\n", list)
		notes = append(notes, fmt.Sprintf("%s has entries with no listed theme, left out unless its instructions bring them in: %s", name, list))
	} else {
		b.WriteString("None.\n")
	}
	d.Fields["brag-notes"] = notes
	return b.String(), nil
}

// feedbackContext is what a feedback agent is given about its document: the sheet, the instructions file and the brag file.
func feedbackContext(r *RunFiles, name string, d *Dispatch) (string, error) {
	bridge := r.Snap.Bridge
	decl, err := findDeclaredDocument(bridge, name)
	if err != nil {
		return "", err
	}
	in, err := readBragInputs(bridge, decl)
	if err != nil {
		return "", err
	}
	brag, found, err := readOptional(filepath.Join(bridge, filepath.FromSlash(bragPath(decl))))
	if err != nil {
		return "", fmt.Errorf("the brag file %s cannot be read", bragPath(decl))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Document: %s\n\n", name)
	b.WriteString(in.sheetSection(decl))
	b.WriteString(in.instructionsSection(decl))
	if found {
		fmt.Fprintf(&b, "## Brag file (%s)\n\n%s\n", bragPath(decl), fencedText(brag))
	} else {
		fmt.Fprintf(&b, "## Brag file (%s)\n\nNone: the file does not exist.\n", bragPath(decl))
	}
	return b.String(), nil
}

// bragBody is the file between the first begin line and the last end line of msg, trailing whitespace trimmed and one final newline. It is false when the markers are missing or the body is empty.
func bragBody(msg string) (string, bool) {
	lines := strings.Split(msg, "\n")
	first, last := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case bragBegin:
			if first < 0 {
				first = i
			}
		case bragEnd:
			last = i
		}
	}
	if first < 0 || last <= first {
		return "", false
	}
	body := strings.TrimRight(strings.Join(lines[first+1:last], "\n"), " \t\r\n")
	if strings.TrimSpace(body) == "" {
		return "", false
	}
	return strings.ReplaceAll(body, "\r\n", "\n") + "\n", true
}

// applyBrag writes a brag agent's file beside its sheet. It returns a problem to send back to the agent, which quotes nothing, or "".
func applyBrag(r *RunFiles, s *Stage, d *Dispatch, msg string) string {
	bridge := r.Snap.Bridge
	decl, err := findDeclaredDocument(bridge, d.Dim)
	if err != nil {
		return err.Error()
	}
	body, ok := bragBody(msg)
	if !ok {
		return "The report needs the brag file, not empty, between a line reading " + bragBegin + " and a line reading " + bragEnd + ", followed by the starship-report block."
	}
	docs, err := loadDeclaredDocuments(bridge)
	if err != nil {
		return "Writing the brag file failed: " + err.Error()
	}
	rel, other := focalCollision(decl, docs)
	if other != "" {
		return fmt.Sprintf("The brag file %s can't be written: it is declared as the path, rubric or instructions of document %s, and only the user can move that declaration in bridge.yaml. Repeating the report exhausts the stop budget and fails the run.", rel, other)
	}
	if err := writeFileAtomic(bridge, filepath.Join(bridge, filepath.FromSlash(rel)), []byte(body)); err != nil {
		return "Writing the brag file failed: " + err.Error()
	}
	r.State.log("brag", s.ID, d.ID, rel)
	for _, n := range d.Fields["brag-notes"] {
		r.State.noteOnce(n)
	}
	return ""
}
