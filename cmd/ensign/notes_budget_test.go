// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func notesOfSize(n int) string { return strings.Repeat("a", n-1) + "\n" }

func TestTaskOpenWarnsWhenNotesPassTheBudget(t *testing.T) {
	w := linkedWorld(t)
	notes := filepath.Join(w.bridge, "projects/supernova/project.md")
	write(t, notes, notesOfSize(projectNotesBudget))
	out, err := w.openOut("auth-refactor")
	if err != nil || strings.Contains(out, "budget") {
		t.Errorf("notes at the budget should not warn (err %v):\n%s", err, out)
	}
	write(t, notes, notesOfSize(projectNotesBudget+1))
	out, err = w.openOut("auth-refactor")
	if err != nil {
		t.Fatalf("a warning must not refuse the open: %v", err)
	}
	for _, want := range []string{"projects/supernova/project.md is 2001 bytes", "budget of 2000", "cannot be reprinted after a compaction", "projects/supernova/design/", "\n" + notesOfSize(projectNotesBudget+1)} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "budget") > strings.Index(out, "aaaa") {
		t.Errorf("the warning should come before the notes")
	}
}

func TestSessionStartWarnsWhenNotesPassTheBudget(t *testing.T) {
	w := linkedWorld(t)
	notes := filepath.Join(w.bridge, "projects/supernova/project.md")
	write(t, notes, notesOfSize(projectNotesBudget))
	if ctx := sessionContext(w, "startup"); strings.Contains(ctx, "budget") {
		t.Errorf("notes at the budget should not warn:\n%s", ctx)
	}
	write(t, notes, notesOfSize(projectNotesBudget+1))
	ctx := sessionContext(w, "startup")
	want := "starship: projects/supernova/project.md is 2001 bytes, over its budget of 2000"
	if !strings.Contains(ctx, want) || !strings.Contains(ctx, "projects/supernova/design/") {
		t.Errorf("want the warning:\n%s", ctx)
	}
}

func TestNotesAtTheBudgetAreReprintedWithTheSessionLinesFull(t *testing.T) {
	w := linkedWorld(t)
	brokenRoleFixture(t, w.bridge, 50, strings.Repeat("k", 300))
	ctx := w.startWithNotes("compact", notesOfSize(projectNotesBudget))
	block, _ := projectNotes(w.bridge, "supernova")
	if !strings.HasSuffix(ctx, block) || len(ctx) >= sessionContextLimit {
		t.Errorf("notes at the budget should be reprinted whole under the limit: %d bytes", len(ctx))
	}
	if !strings.Contains(ctx, "more session-start lines held") {
		t.Errorf("the session lines should be full:\n%s", ctx)
	}
}
