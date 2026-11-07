// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf16"
)

// sessionStartRoom is the room kept free under manualLimit for text the session start budget can't foresee.
const sessionStartRoom = 400

// contextChars counts s the way Claude Code's limit on additionalContext is most strictly read. The limit is in characters and the docs don't say whether those are code points or UTF-16 code units; code units are the higher count, and bytes would overstate non-ASCII text.
func contextChars(s string) int { return len(utf16.Encode([]rune(s))) }

// The priorities of the session start lines, highest first. A line that doesn't fit is held back, and nothing of a lower priority prints ahead of it.
const (
	// Nothing else matters when starship isn't working in this session.
	linePriorityNotReady = iota
	// A restart is the one fix the user can't miss, and until it happens the guards don't apply.
	linePriorityConnectorRestart
	linePriorityCarryErr
	linePriorityLeftBehindHold
	linePriorityStrandedRetire
	linePrioritySync
	linePriorityMigrate
	linePrioritySplitHistory
	linePriorityBrokenExtension
	linePriorityEnv
	linePriorityRequest
	// The inbox is a task for this session; the index only saves a listing.
	linePriorityNotesInbox
	linePriorityNotesIndex
)

type sessionLine struct {
	priority int
	text     string
}

// sessionLines collects the situational lines of a session start.
type sessionLines struct{ lines []sessionLine }

// add collects text at a priority, one line per non-empty row.
func (c *sessionLines) add(priority int, text string) {
	for _, row := range strings.Split(text, "\n") {
		if strings.TrimSpace(row) != "" {
			c.lines = append(c.lines, sessionLine{priority, row})
		}
	}
}

// fitSessionLines sorts lines by priority, keeping the order of equal ones, and splits them into the lines that print and the lines that collapse. If every line fits in allowance, all print. Otherwise reserve is kept for the collapse line, lines print in order until the first that doesn't fit, and that line and every later one collapse. A line counts its newline.
func fitSessionLines(lines []sessionLine, allowance, reserve int) (print, collapsed []sessionLine) {
	sorted := append([]sessionLine(nil), lines...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].priority < sorted[j].priority })
	total := 0
	for _, l := range sorted {
		total += contextChars(l.text) + 1
	}
	if total <= allowance {
		return sorted, nil
	}
	room := allowance - reserve
	for i, l := range sorted {
		size := contextChars(l.text) + 1
		if size > room {
			return sorted[:i], sorted[i:]
		}
		room -= size
	}
	return sorted, nil
}

const (
	collapseHeldFmt    = "starship: %d more session-start lines held; read them with ensign status.\n"
	collapseNotHeldFmt = "starship: %d more session-start lines did not fit and could not be held.\n"
)

// render is the text of the lines for a reply whose fixed text, with extra characters more reserved, takes the rest of the budget. Lines that don't fit are held for the session and counted in a last line. Holding is best effort: a failure, or a start with no session id, changes the wording of that line.
func (c *sessionLines) render(bridge, session, fixed string, extra int) string {
	allowance := manualLimit - sessionStartRoom - contextChars(fixed) - extra
	reserve := max(contextChars(fmt.Sprintf(collapseHeldFmt, len(c.lines))), contextChars(fmt.Sprintf(collapseNotHeldFmt, len(c.lines))))
	print, collapsed := fitSessionLines(c.lines, allowance, reserve)
	var b strings.Builder
	for _, l := range print {
		b.WriteString(l.text + "\n")
	}
	if len(collapsed) > 0 {
		// The broken extensions are listed again whenever status is read, so holding them would only show them twice.
		var texts []string
		for _, l := range collapsed {
			if l.priority != linePriorityBrokenExtension {
				texts = append(texts, l.text)
			}
		}
		if len(texts) == 0 || (session != "" && holdSessionLines(bridge, session, texts) == nil) {
			fmt.Fprintf(&b, collapseHeldFmt, len(collapsed))
		} else {
			fmt.Fprintf(&b, collapseNotHeldFmt, len(collapsed))
		}
	}
	return b.String()
}

func heldLinesFile(bridge, session string) string { return sessionFile(bridge, session) + ".lines" }

// heldSessionLines reads the lines held for a session.
func heldSessionLines(bridge, session string) []string {
	if session == "" {
		return nil
	}
	data, err := os.ReadFile(heldLinesFile(bridge, session))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// holdSessionLines adds lines to those already held for the session, leaving out exact duplicates, so a later start (compact keeps the session id) doesn't lose an earlier one-shot line.
func holdSessionLines(bridge, session string, lines []string) error {
	held := heldSessionLines(bridge, session)
	seen := map[string]bool{}
	for _, l := range held {
		seen[l] = true
	}
	grew := false
	for _, l := range lines {
		if !seen[l] {
			seen[l] = true
			held = append(held, l)
			grew = true
		}
	}
	if !grew {
		return nil
	}
	return writeFileAtomic(bridge, heldLinesFile(bridge, session), []byte(strings.Join(held, "\n")+"\n"))
}

// copySessionLines carries the lines held for one session to the session that continues it. A failure leaves them behind.
func copySessionLines(bridge, from, to string) {
	if held := heldSessionLines(bridge, from); len(held) > 0 {
		holdSessionLines(bridge, to, held)
	}
}
