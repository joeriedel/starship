// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReaderFactsMigrated(t *testing.T) {
	b := newBridge(t)
	facts, history := filepath.Join(b, "reader", "facts.md"), filepath.Join(b, "reader", "history.md")
	if ok, err := readerFactsMigrated(b); err != nil || !ok {
		t.Errorf("no file: %v %v", ok, err)
	}
	write(t, facts, "The launch window is 12 March.\n")
	if ok, err := readerFactsMigrated(b); err != nil || ok {
		t.Errorf("non-empty facts.md: %v %v", ok, err)
	}
	write(t, facts, " \n\t\n")
	write(t, history, "\n")
	if ok, err := readerFactsMigrated(b); err != nil || !ok {
		t.Errorf("whitespace only: %v %v", ok, err)
	}
	for _, p := range []string{facts, history} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s should be removed", p)
		}
	}
	write(t, history, "Old fact.\n")
	if ok, err := readerFactsMigrated(b); err != nil || !ok {
		t.Errorf("history.md alone: %v %v", ok, err)
	}
	if _, err := os.Stat(history); err != nil {
		t.Errorf("non-empty history.md should stay: %v", err)
	}
}

func TestReaderFactsSessionLineAndDispatch(t *testing.T) {
	w, _ := declaredGuardWorld(t)
	write(t, filepath.Join(w.bridge, ".claude", "settings.json"), hookSettings())
	reader := func(prompt string) (string, bool) {
		res, denied := w.toolCall("", "", "Agent", map[string]any{"subagent_type": "reader", "prompt": prompt})
		return dig(res, "hookSpecificOutput", "permissionDecisionReason"), denied
	}
	if strings.Contains(sessionContext(w, "startup"), "old layout") {
		t.Error("no old file, so no line")
	}
	if _, denied := reader("gather"); denied {
		t.Error("migrated: dispatch allowed")
	}
	write(t, filepath.Join(w.bridge, "reader", "facts.md"), "A fact.\n")
	if ctx := sessionContext(w, "startup"); !strings.Contains(ctx, "starship: the reader's facts are in the old layout") || !strings.Contains(ctx, "facts migration") {
		t.Errorf("missing line:\n%s", ctx)
	}
	if why, denied := reader("Look at SUPER-42"); !denied || !strings.HasPrefix(why, "starship: the reader's facts") || !strings.Contains(why, "facts migration") {
		t.Errorf("refusal: %v %q", denied, why)
	}
	if why, denied := reader("This is a Facts Migration."); denied {
		t.Errorf("migration dispatch refused: %q", why)
	}
	if why, denied := w.toolCall("", "", "Agent", map[string]any{"subagent_type": "reader", "prompt": "Look at SUPER-42"}); !denied {
		t.Errorf("session dispatch allowed: %v", why)
	}
	if res, denied := w.toolCall("agent-1", "orchestrator", "Agent", map[string]any{"subagent_type": "reader", "prompt": "Look at SUPER-42"}); !denied || !strings.Contains(dig(res, "hookSpecificOutput", "permissionDecisionReason"), "facts migration") {
		t.Errorf("subagent dispatch allowed: %v", res)
	}
	if _, denied := w.toolCall("", "", "Agent", map[string]any{"subagent_type": "coder", "prompt": "x"}); denied {
		t.Error("another agent type is not the reader's business")
	}
}

func TestReaderFactsUndeclared(t *testing.T) {
	w := connWorld(t, "")
	write(t, filepath.Join(w.bridge, "reader", "facts.md"), "A fact.\n")
	if strings.Contains(sessionContext(w, "startup"), "old layout") {
		t.Error("reader not declared: no line")
	}
	if pending, err := readerFactsPending(w.bridge); err != nil || pending {
		t.Errorf("pending: %v %v", pending, err)
	}
}

func TestReaderFactsRefuseBriefAndLinks(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, filepath.Join(w.bridge, "reader", "facts.md"), "A fact.\n")
	out, err := w.brief()
	if err == nil || !strings.Contains(err.Error(), "facts migration") || strings.Contains(out, "Brief for") {
		t.Errorf("brief: %v\n%s", err, out)
	}

	w = newWorld(t)
	declareReader(t, w)
	write(t, filepath.Join(w.bridge, "reader", "facts.md"), "A fact.\n")
	got := linksOpen(t, w, "pipeline: coding\n"+twoLinks)
	if !strings.Contains(got, "ticket SUPER-42\nreader     the look-up of these links waits for the facts migration") || strings.Contains(got, "with these links") {
		t.Errorf("links notice:\n%s", got)
	}
}
