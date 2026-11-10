// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readState(t *testing.T, w *world, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.bridge, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBriefNamesPerSourceStateFiles(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"supernova-slack", "work-mail"} {
		if !strings.Contains(out, "reader/cursors/"+s+".md") || !strings.Contains(out, "reader/mail-read/"+s+".md") {
			t.Errorf("brief does not name %s's state files:\n%s", s, out)
		}
	}
}

func TestBriefMigratesSharedReaderFiles(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	r := filepath.Join(w.bridge, "reader")
	write(t, filepath.Join(r, "cursors.md"), "- supernova-slack #supernova-launch: 5 (2026-11-05 read)\r\n\r\n"+
		"- supernova-slack #supernova-qa: 2 (2026-11-02 read)\n"+
		"- supernova-slack @nova-lead: 9 (2026-11-03 read)\n"+
		"- supernova-old #supernova-launch: 1 (2026-10-01 read)\n"+
		"- supernova-slack #supernova-tie: 1 (2026-11-04 read)\n"+
		"garbage line\n")
	write(t, filepath.Join(r, "mail-read.md"), "- work-mail | inbox | m1 | 2026-11-03 | 2026-11-04 | nova@example.com | Launch | now\n"+
		"- work-mail | inbox | m2 | 2026-11-03 | 2026-11-04 | nova@example.com | Plans\n"+
		"- work-mail | inbox | m3 | 2026-11-03\n")
	write(t, filepath.Join(r, "cursors", "supernova-slack.md"), "- supernova-slack #supernova-launch: 3 (2026-11-01 read)\n"+
		"- supernova-slack #supernova-qa: 7 (2026-11-06 read)\n"+
		"- supernova-slack @nova-lead: 9 (2026-11-03 read)\n"+
		"- supernova-slack #supernova-tie: 2 (2026-11-04 read)\n")
	write(t, filepath.Join(r, "mail-read", "work-mail.md"), "- work-mail | inbox | m2 | 2026-11-03 | 2026-11-04 | nova@example.com | Plans\n")

	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 lines") || !strings.Contains(out, "reader/cursors/unparsed.md") {
		t.Errorf("brief does not report the unparsed lines:\n%s", out)
	}
	wantCursors := "- supernova-slack #supernova-launch: 5 (2026-11-05 read)\n" +
		"- supernova-slack #supernova-qa: 7 (2026-11-06 read)\n" +
		"- supernova-slack @nova-lead: 9 (2026-11-03 read)\n" +
		"- supernova-slack #supernova-tie: 2 (2026-11-04 read)\n" +
		"- supernova-slack #supernova-tie: 1 (2026-11-04 read)\n"
	if got := readState(t, w, "reader/cursors/supernova-slack.md"); got != wantCursors {
		t.Errorf("supernova-slack cursors:\n%s", got)
	}
	if got := readState(t, w, "reader/cursors/supernova-old.md"); got != "- supernova-old #supernova-launch: 1 (2026-10-01 read)\n" {
		t.Errorf("undeclared source cursors:\n%s", got)
	}
	wantMail := "- work-mail | inbox | m2 | 2026-11-03 | 2026-11-04 | nova@example.com | Plans\n" +
		"- work-mail | inbox | m1 | 2026-11-03 | 2026-11-04 | nova@example.com | Launch | now\n"
	if got := readState(t, w, "reader/mail-read/work-mail.md"); got != wantMail {
		t.Errorf("work-mail records:\n%s", got)
	}
	if got := readState(t, w, "reader/cursors/unparsed.md"); got != "garbage line\n- work-mail | inbox | m3 | 2026-11-03\n" {
		t.Errorf("unparsed:\n%s", got)
	}
	for _, old := range []string{"cursors.md", "mail-read.md"} {
		if _, err := os.Stat(filepath.Join(r, old)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", old, err)
		}
	}

	before := readState(t, w, "reader/cursors/supernova-slack.md") + readState(t, w, "reader/mail-read/work-mail.md") + readState(t, w, "reader/cursors/unparsed.md")
	out, err = w.brief()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 lines that could not be read") {
		t.Errorf("second brief does not report the kept lines:\n%s", out)
	}
	after := readState(t, w, "reader/cursors/supernova-slack.md") + readState(t, w, "reader/mail-read/work-mail.md") + readState(t, w, "reader/cursors/unparsed.md")
	if before != after {
		t.Errorf("second brief changed the files:\n%s\n---\n%s", before, after)
	}
}

func TestBriefMigrationRerunDoesNotDuplicate(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	r := filepath.Join(w.bridge, "reader")
	line := "- supernova-slack #supernova-launch: 5 (2026-11-05 read)\n"
	write(t, filepath.Join(r, "cursors.md"), line+"garbage\n")
	write(t, filepath.Join(r, "cursors", "supernova-slack.md"), line)
	write(t, filepath.Join(r, "cursors", "unparsed.md"), "garbage\n")
	if _, err := w.brief(); err != nil {
		t.Fatal(err)
	}
	if got := readState(t, w, "reader/cursors/supernova-slack.md"); got != line {
		t.Errorf("cursors:\n%s", got)
	}
	if got := readState(t, w, "reader/cursors/unparsed.md"); got != "garbage\n" {
		t.Errorf("unparsed:\n%s", got)
	}
}

func TestBriefReportsUnparsedFileOnly(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "could not be read") {
		t.Errorf("brief reports unparsed lines with no file:\n%s", out)
	}
	write(t, filepath.Join(w.bridge, "reader", "cursors", "unparsed.md"), "garbage\n\n- work-mail | inbox | m3 | 2026-11-03\n")
	out, err = w.brief()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Problem: 2 lines that could not be read are kept in reader/cursors/unparsed.md") {
		t.Errorf("brief does not report the kept file:\n%s", out)
	}
}
