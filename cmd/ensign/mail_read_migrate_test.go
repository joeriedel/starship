// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	mailJan  = "- work-mail | inbox | j1 | 2026-10-30 | 2026-11-04 | nova@example.com | Launch\n"
	mailNov  = "- work-mail | inbox | n1 | 2026-11-03 | 2026-11-04 | nova@example.com | Plans\n"
	mailNov2 = "- work-mail | inbox | n2 | 2026-11-05 | 2026-11-06 | nova@example.com | Crew | with | bars\n"
)

func mailPath(w *world, rel ...string) string {
	return filepath.Join(append([]string{w.bridge, "reader", "mail-read"}, rel...)...)
}

func TestMigrateMailReadSplitsByReceivedMonth(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov+mailJan+"\r\n"+strings.TrimSuffix(mailNov2, "\n")+"\r\n"+
		"- work-mail | inbox | u1 | not-a-date | 2026-11-06 | nova@example.com | Odd\n"+
		"garbage line\n"+mailNov)
	note, err := migrateMailRead(w.bridge)
	if err != nil || !strings.Contains(note, "1 source") {
		t.Fatalf("note %q, err %v", note, err)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-11.md"); got != mailNov+mailNov2 {
		t.Errorf("2026-11:\n%q", got)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-10.md"); got != mailJan {
		t.Errorf("2026-10:\n%q", got)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/undated.md"); got != "- work-mail | inbox | u1 | not-a-date | 2026-11-06 | nova@example.com | Odd\ngarbage line\n" {
		t.Errorf("undated:\n%q", got)
	}
	if _, err := os.Stat(mailPath(w, "work-mail.md")); !os.IsNotExist(err) {
		t.Errorf("old file remains: %v", err)
	}
	if note, err := migrateMailRead(w.bridge); note != "" || err != nil {
		t.Errorf("second run: %q, %v", note, err)
	}
}

func TestMigrateMailReadResumesAfterInterruption(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov+mailJan+mailNov2+"garbage line\n")
	write(t, mailPath(w, "work-mail", "2026-11.md"), mailNov)
	write(t, mailPath(w, "work-mail", "undated.md"), "garbage line\n")
	if _, err := migrateMailRead(w.bridge); err != nil {
		t.Fatal(err)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-11.md"); got != mailNov+mailNov2 {
		t.Errorf("2026-11:\n%q", got)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-10.md"); got != mailJan {
		t.Errorf("2026-10:\n%q", got)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/undated.md"); got != "garbage line\n" {
		t.Errorf("undated:\n%q", got)
	}
	if _, err := os.Stat(mailPath(w, "work-mail.md")); !os.IsNotExist(err) {
		t.Errorf("old file remains: %v", err)
	}
}

func TestMigrateMailReadKeepsOldFileWhenAWriteFails(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov+mailJan)
	// A file where the source folder must be makes every month write fail.
	write(t, mailPath(w, "work-mail"), "not a folder\n")
	if _, err := migrateMailRead(w.bridge); err == nil {
		t.Fatal("no error")
	}
	if got := readState(t, w, "reader/mail-read/work-mail.md"); got != mailNov+mailJan {
		t.Errorf("old file changed:\n%q", got)
	}
}

func TestMigrateMailReadNothingToDo(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	if note, err := migrateMailRead(w.bridge); note != "" || err != nil {
		t.Fatalf("no folder: %q, %v", note, err)
	}
	write(t, mailPath(w, "work-mail", "2026-11.md"), mailNov)
	if note, err := migrateMailRead(w.bridge); note != "" || err != nil {
		t.Fatalf("new layout: %q, %v", note, err)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-11.md"); got != mailNov {
		t.Errorf("changed:\n%q", got)
	}
}

func TestSessionStartConvertsMailRecords(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov+mailJan)
	if ctx := sessionContext(w, "startup"); !strings.Contains(ctx, "converted the reader's mail records") {
		t.Errorf("session start does not report the conversion:\n%s", ctx)
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-11.md"); got != mailNov {
		t.Errorf("2026-11:\n%q", got)
	}
	if ctx := sessionContext(w, "startup"); strings.Contains(ctx, "converted the reader's mail records") {
		t.Errorf("second session start reports again:\n%s", ctx)
	}
}

func TestSessionStartReportsMailConversionError(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov)
	write(t, mailPath(w, "work-mail"), "not a folder\n")
	if ctx := sessionContext(w, "startup"); !strings.Contains(ctx, "converting") {
		t.Errorf("session start does not report the error:\n%s", ctx)
	}
}

func TestReaderDispatchConvertsMailRecords(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov)
	if _, denied := w.toolCall("", "", "Agent", map[string]any{"subagent_type": "reader", "prompt": "gather"}); denied {
		t.Fatal("dispatch denied")
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-11.md"); got != mailNov {
		t.Errorf("2026-11:\n%q", got)
	}
	write(t, mailPath(w, "other-mail.md"), mailNov)
	write(t, mailPath(w, "other-mail"), "not a folder\n")
	res, denied := w.toolCall("", "", "Agent", map[string]any{"subagent_type": "reader", "prompt": "gather"})
	if !denied || !strings.Contains(dig(res, "hookSpecificOutput", "permissionDecisionReason"), "other-mail") {
		t.Errorf("failed conversion did not deny, naming the error: %v", res)
	}
}

func TestLinkMigrateConvertsMailRecords(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	write(t, mailPath(w, "work-mail.md"), mailNov)
	var out bytes.Buffer
	if err := w.inBridge(func() error { return cmdLink([]string{"--migrate"}, &out) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "converted the reader's mail records") {
		t.Errorf("output:\n%s", out.String())
	}
	if got := readState(t, w, "reader/mail-read/work-mail/2026-11.md"); got != mailNov {
		t.Errorf("2026-11:\n%q", got)
	}
}

func TestMigrateMailSourceSkipsAnOldFileAlreadyConverted(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	done, err := migrateMailSource(w.bridge, mailPath(w), "work-mail.md")
	if done || err != nil {
		t.Fatalf("old file gone: %v, %v", done, err)
	}
}
