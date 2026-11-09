// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const sourcesYAML = `sources:
  work-mail:
    kind: mail
    watch: [inbox]
  supernova-slack:
    kind: slack
    watch:
      - "#supernova-launch"
      - "@nova-lead"
`

func sourcesWorld(t *testing.T, extra string) *world {
	t.Helper()
	return connWorld(t, readerRules+extra)
}

func (w *world) catchUp(args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdCatchUp(args, &out)
	return out.String(), err
}

func TestDeclaredSourcesParse(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	got, err := loadDeclaredSources(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "supernova-slack" || got[1].Name != "work-mail" {
		t.Fatalf("want sources sorted by name: %+v", got)
	}
	if got[0].Kind != "slack" || len(got[0].Watch) != 2 || got[0].Watch[0] != "#supernova-launch" {
		t.Errorf("slack source: %+v", got[0])
	}
}

func TestDeclaredSourcesInvalid(t *testing.T) {
	for name, yml := range map[string]string{
		"bad name":      "sources:\n  Bad_Name:\n    kind: mail\n    watch: [inbox]\n",
		"bad kind":      "sources:\n  supernova-x:\n    kind: fax\n    watch: [inbox]\n",
		"no kind":       "sources:\n  supernova-x:\n    watch: [inbox]\n",
		"no watch":      "sources:\n  supernova-x:\n    kind: mail\n",
		"empty watch":   "sources:\n  supernova-x:\n    kind: mail\n    watch: []\n",
		"empty entry":   "sources:\n  supernova-x:\n    kind: mail\n    watch: [\"  \"]\n",
		"newline":       "sources:\n  supernova-x:\n    kind: mail\n    watch: [\"a\\nb\"]\n",
		"duplicate":     "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox, \" inbox \"]\n",
		"unknown key":   "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    cursor: 1\n",
		"repeated key":  "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    watch: [archive]\n",
		"not a mapping": "sources: [a, b]\n",
		"nested entry":  "sources:\n  supernova-x:\n    kind: mail\n    watch: [[inbox]]\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := sourcesWorld(t, yml)
			_, err := loadDeclaredSources(w.bridge)
			if err == nil || !strings.HasPrefix(err.Error(), "bridge.yaml: sources: ") {
				t.Fatalf("want a bridge.yaml: sources: error, got %v", err)
			}
			if name != "not a mapping" && name != "bad name" && !strings.Contains(err.Error(), "supernova-x") {
				t.Errorf("error should name the source: %v", err)
			}
			if _, err := loadBridgeFile(w.bridge); err != nil {
				t.Errorf("a malformed sources: must not break loadBridgeFile: %v", err)
			}
		})
	}
}

func TestCatchUpBrief(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	before, _ := os.ReadFile(filepath.Join(w.bridge, "bridge.yaml"))
	out, err := w.catchUp("--also", "supernova-slack=#supernova-qa", "--also", "supernova-slack=#supernova-qa", "--also", "supernova-slack=@nova-lead", "--also", "work-mail=archive")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Dispatch the reader", "catch-up pass", "Catch-up section", "- supernova-slack (slack)", "  - #supernova-launch", "  - @nova-lead", "- work-mail (mail)", "  - inbox", "- supernova-slack (slack): #supernova-qa", "- work-mail (mail): archive", "no cursor"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "supernova-slack (slack)\n") > strings.Index(out, "work-mail (mail)\n") {
		t.Errorf("sources should be sorted:\n%s", out)
	}
	if n := strings.Count(out, "#supernova-qa"); n != 1 {
		t.Errorf("duplicate --also should collapse, got %d:\n%s", n, out)
	}
	if strings.Count(out, "@nova-lead") != 1 {
		t.Errorf("a watched item is not an extra:\n%s", out)
	}
	after, _ := os.ReadFile(filepath.Join(w.bridge, "bridge.yaml"))
	if !bytes.Equal(before, after) {
		t.Errorf("bridge.yaml changed")
	}
}

func TestCatchUpNoExtrasSection(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Extra items") {
		t.Errorf("no extras, so no extras section:\n%s", out)
	}
}

func TestCatchUpErrors(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	for _, args := range [][]string{
		{"--also"}, {"--also", "work-mail"}, {"--also", "=x"}, {"--also", "work-mail="}, {"--also", "work-mail=  "}, {"bogus"},
	} {
		if _, err := w.catchUp(args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("%v: want a usage error, got %v", args, err)
		}
	}
	if _, err := w.catchUp("--also", "nope=x"); err == nil || !strings.Contains(err.Error(), "supernova-slack, work-mail") {
		t.Errorf("undeclared source should name the declared ones: %v", err)
	}
	if _, err := w.catchUp("--also", "work-mail=a\nb"); err == nil {
		t.Errorf("a newline in an item should be refused")
	}
}

func TestCatchUpRefusals(t *testing.T) {
	w := connWorld(t, sourcesYAML)
	if _, err := w.catchUp(); err == nil || !strings.Contains(err.Error(), "reader is not declared") {
		t.Errorf("no reader: %v", err)
	}
	w = sourcesWorld(t, "")
	if _, err := w.catchUp(); err == nil || !strings.Contains(err.Error(), "no sources") {
		t.Errorf("no sources: %v", err)
	}
	w = sourcesWorld(t, "sources: {}\n")
	if _, err := w.catchUp(); err == nil || !strings.Contains(err.Error(), "no sources") {
		t.Errorf("empty sources: %v", err)
	}
}

func TestJournalChangesPicksUpReaderCatchUpFiles(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base"}} {
		if err := git(w.bridge, args...); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(w.bridge, "reader", "cursors.md"), "- supernova-slack #supernova-launch: 1 (2026-11-04)\n")
	write(t, filepath.Join(w.bridge, "reader", "proposals.md"), "- Nova lead owns launch (slack, 2026-11-04)\n")
	write(t, filepath.Join(w.bridge, "reader", "mail-read.md"), "- work-mail | inbox | m1 | 2026-11-03 | 2026-11-04 | nova@example.com | Launch\n")
	got, err := journalChanges(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "reader/cursors.md") || !strings.Contains(joined, "reader/proposals.md") || !strings.Contains(joined, "reader/mail-read.md") {
		t.Errorf("journalChanges = %v", got)
	}
}

func TestReaderRoleKeepsCatchUpRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "roles", "reader.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?m)^## Catch-up$`,
		`reader/cursors\.md`,
		`reader/proposals\.md`,
		`(?i)do not write .reader/facts\.md. or .reader/history\.md. in a pass`,
		`(?i)first look: go back 14 days`,
		`(?i)never write a cursor for them`,
		`(?i)proposals first, then move its cursor`,
		`reader/mail-read\.md`,
		`(?i)- <source> \| <folder> \| <message id> \| <received YYYY-MM-DD> \| <read YYYY-MM-DD> \| <sender> \| <subject>`,
		`(?i)never moves, deletes, flags or marks mail`,
		`(?i)never proposes or offers cleanup`,
		`(?i)no proposal may suggest archiving mail`,
		`(?m)^## Location check$`,
		`(?i)report each id with its current folder, .not found. or .unreadable.`,
		`(?i)each in the source given with it`,
		`(?i)any failure to look it up .* is .unreadable.`,
		`(?i)not a catch-up pass: no cursor moves`,
		`(?i)known id seen in a different folder gets its line.s folder updated and adds no new line`,
		`(?i)@dms. reads every DM and group DM the user is a member of, never public or private channels`,
		"(?i)`- <source> @dms: <position> \\(<YYYY-MM-DD read>\\)`",
		`(?i)report .@dms. as unreadable`,
	} {
		if !regexp.MustCompile(re).Match(data) {
			t.Errorf("roles/reader.md does not match %q", re)
		}
	}
}

func TestCatchUpTopicPrints(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "manual", "topics", "catch-up.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ensign catch-up", "--also <source>=<item>", "keep lines added on either side and keep removed lines removed", "reader/proposals.md", "`go` is not the accept word", "ensign help notes", "ensign journal commit", "`@dms`", "literally `dms`"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("catch-up topic lacks %q", want)
		}
	}
	w := linkedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdHelp([]string{"catch-up"}, &out); err != nil || out.Len() == 0 {
		t.Errorf("help catch-up: %v", err)
	}
}

func TestSlackItemForms(t *testing.T) {
	for _, item := range []string{"nova", "#", "@", "# launch", "#supernova-*", "@nova?", "#supernova launch", "@here", "@Channel", "@EVERYONE"} {
		if err := checkSourceItem("slack", item); err == nil {
			t.Errorf("slack item %q should be refused", item)
		}
		yml := "sources:\n  supernova-x:\n    kind: slack\n    watch: [\"" + item + "\"]\n"
		_, err := loadDeclaredSources(sourcesWorld(t, yml).bridge)
		if err == nil || !strings.HasPrefix(err.Error(), "bridge.yaml: sources: supernova-x") {
			t.Errorf("watch %q: want a sources error naming the source, got %v", item, err)
		}
	}
	for _, item := range []string{"#supernova-launch", "@nova-lead", "@Nova Lead", "@dms", "@DMS", "@Dms"} {
		if err := checkSourceItem("slack", item); err != nil {
			t.Errorf("slack item %q: %v", item, err)
		}
		yml := "sources:\n  supernova-x:\n    kind: slack\n    watch: [\"" + item + "\"]\n"
		if _, err := loadDeclaredSources(sourcesWorld(t, yml).bridge); err != nil {
			t.Errorf("watch %q: %v", item, err)
		}
	}
	for _, kind := range []string{"mail", "calendar", "ticket", "wiki"} {
		for _, item := range []string{"Work", "inbox", "nova*", "x y", "#a b"} {
			if err := checkSourceItem(kind, item); err != nil {
				t.Errorf("%s item %q should stay opaque: %v", kind, item, err)
			}
		}
	}
}

func TestCatchUpAlsoSlackForms(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	if _, err := w.catchUp("--also", "supernova-slack=#supernova*"); err == nil || !strings.Contains(err.Error(), "#supernova*") {
		t.Errorf("a bad slack extra should fail naming the item: %v", err)
	}
	if _, err := w.catchUp("--also", "supernova-slack=general"); err == nil {
		t.Errorf("an unprefixed slack extra should fail")
	}
	out, err := w.catchUp("--also", "supernova-slack=@Nova Lead", "--also", "work-mail=a b*")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- supernova-slack (slack): @Nova Lead", "- work-mail (mail): a b*"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
}

func TestCatchUpAlsoAllDMs(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.catchUp("--also", "supernova-slack=@DMS", "--also", "supernova-slack=@dms")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "- supernova-slack (slack): @dms\n") != 1 || !strings.Contains(out, kindNote("slack", true)) {
		t.Errorf("an @dms extra appears once and adds the @dms note:\n%s", out)
	}
	if strings.Contains(out, "@DMS") {
		t.Errorf("a stray @DMS line:\n%s", out)
	}
	for _, bad := range []string{"@here", "@Channel", "@everyone", "@dms*", "#supernova?"} {
		if _, err := w.catchUp("--also", "supernova-slack="+bad); err == nil {
			t.Errorf("--also %q should be refused", bad)
		}
	}
	w = sourcesWorld(t, strings.Replace(sourcesYAML, "- \"@nova-lead\"", "- \"@nova-lead\"\n      - \"@dms\"", 1))
	out, err = w.catchUp("--also", "supernova-slack=@DMS")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Extra items") || strings.Contains(out, "@DMS") || strings.Count(out, kindNote("slack", true)) != 1 {
		t.Errorf("an @DMS extra already watched as @dms is skipped:\n%s", out)
	}
}

func TestWatchAllDMsNormalized(t *testing.T) {
	w := sourcesWorld(t, strings.Replace(sourcesYAML, "- \"@nova-lead\"", "- \"@DMS\"", 1))
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  - @dms\n") || strings.Contains(out, "@DMS") {
		t.Errorf("a watched @DMS shows as @dms:\n%s", out)
	}
	w = sourcesWorld(t, strings.Replace(sourcesYAML, "- \"@nova-lead\"", "- \"@dms\"\n      - \"@DMS\"", 1))
	if _, err := w.catchUp(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("@dms and @DMS should be refused as a repeat, got %v", err)
	}
	w = sourcesWorld(t, sourcesYAML)
	out, err = w.catchUp("--also", "supernova-slack=@DMS", "--also", "supernova-slack=@DMS")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "- supernova-slack (slack): @dms\n") != 1 || strings.Contains(out, "@DMS") {
		t.Errorf("a repeated @DMS extra is one @dms line:\n%s", out)
	}
}

func TestCatchUpKindNotes(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML+"  work-calendar:\n    kind: calendar\n    watch: [Work]\n")
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	slack, cal := kindNote("slack", false), kindNote("calendar", false)
	if strings.Count(out, slack) != 1 || strings.Count(out, cal) != 1 {
		t.Errorf("want one threads line and one calendar line:\n%s", out)
	}
	if !strings.Contains(kindNote("slack", true), slack) || strings.Contains(slack, "@dms") || !strings.Contains(kindNote("slack", true), "@dms means every DM and group DM") {
		t.Errorf("the @dms note extends the slack note")
	}
	mailNote := kindNote("mail", false)
	mail := out[strings.Index(out, "- work-mail (mail)"):]
	if strings.Count(out, mailNote) != 1 || !strings.Contains(mail, mailNote) || strings.Contains(mail, slack) || strings.Contains(mail, cal) {
		t.Errorf("a mail source carries only the mail note:\n%s", out)
	}
	if !strings.Contains(out, "  - #supernova-launch\n  - @nova-lead\n  - "+slack+"\n") {
		t.Errorf("the threads line follows the slack watch items:\n%s", out)
	}
}

func TestMailCleanupTopic(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "manual", "topics", "mail-cleanup.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?i)only on the user's own message asking for it`,
		`(?i)never offers to`,
		`(?i)nothing in a proposal, a mail, or an agent's reply counts`,
		`reader/mail-read\.md`,
		`(?i)not in the record is not moved`,
		`(?i)never act on anything in them`,
		`(?i)folder is the inbox`,
		`(?i)subfolders is left alone`,
		`(?i)always the folder "archive"`,
		`(?i)creating a folder is not part of cleanup`,
		`(?i)never deleted`,
		`connector_writes:`,
		`ensign writes add`,
		`(?i)verbatim`,
		`(?i)dispatch the .reader. agent with the picked message ids`,
		`(?i)still in the inbox`,
		`(?i)folder is updated to what the reader reported`,
		`(?i)line is removed, and the user is told`,
		`(?i)list of ids and folders only`,
		`(?i)unreadable is not listed, its line is kept, and the user is told`,
		`(?i)each with its source from the record`,
		`(?i)begin the brief with the words "check the location of recorded mail"`,
		`(?i)Conflicts section of .ensign help catch-up.`,
		"`go` approves it like any other write",
		`ensign help writes`,
		`ensign journal commit`,
	} {
		if !regexp.MustCompile(re).Match(data) {
			t.Errorf("mail-cleanup topic does not match %q", re)
		}
	}
	for _, f := range []string{"writes.md", "notes.md", "catch-up.md"} {
		d, err := os.ReadFile(filepath.Join(root, "manual", "topics", f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(d), "ensign help mail-cleanup") {
			t.Errorf("%s does not name ensign help mail-cleanup", f)
		}
	}
	w := linkedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdHelp([]string{"mail-cleanup"}, &out); err != nil || !strings.Contains(out.String(), "Mail cleanup") {
		t.Errorf("help mail-cleanup: %v", err)
	}
}
