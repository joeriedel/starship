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

func TestDeclaredSourcesDigest(t *testing.T) {
	w := sourcesWorld(t, "sources:\n  supernova-x:\n    kind: slack\n    digest: [\"#supernova-ci\", \"@DMS\"]\n    watch: [\"#supernova-launch\", \"#supernova-ci\", \"@dms\"]\n")
	got, err := loadDeclaredSources(w.bridge)
	if err != nil || len(got) != 1 || len(got[0].Digest) != 2 || got[0].Digest[0] != "#supernova-ci" || got[0].Digest[1] != "@dms" {
		t.Errorf("digest: %+v, %v", got, err)
	}
}

func TestDeclaredSourcesLookahead(t *testing.T) {
	for _, c := range []struct {
		line string
		want int
	}{{"", 7}, {"    lookahead: 0\n", 0}, {"    lookahead: 14\n", 14}, {"    lookahead: 31\n", 31}} {
		w := sourcesWorld(t, "sources:\n  supernova-x:\n    kind: calendar\n"+c.line+"    watch: [Work]\n")
		got, err := loadDeclaredSources(w.bridge)
		if err != nil || len(got) != 1 || got[0].Lookahead != c.want {
			t.Errorf("lookahead %q: want %d, got %+v, %v", c.line, c.want, got, err)
		}
	}
	w := sourcesWorld(t, "sources:\n  supernova-x:\n    lookahead: 2\n    kind: calendar\n    watch: [Work]\n  supernova-m:\n    kind: mail\n    watch: [inbox]\n")
	got, err := loadDeclaredSources(w.bridge)
	if err != nil || got[0].Lookahead != 0 || got[1].Lookahead != 2 {
		t.Errorf("key order is free and mail has no lookahead: %+v, %v", got, err)
	}
	_, err = loadDeclaredSources(sourcesWorld(t, "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    lookahead: 3\n").bridge)
	if err == nil || !strings.Contains(err.Error(), "supernova-x: lookahead is only for calendar sources") {
		t.Errorf("lookahead on mail: %v", err)
	}
}

func TestDeclaredSourcesInvalid(t *testing.T) {
	for name, yml := range map[string]string{
		"bad name":                 "sources:\n  Bad_Name:\n    kind: mail\n    watch: [inbox]\n",
		"bad kind":                 "sources:\n  supernova-x:\n    kind: fax\n    watch: [inbox]\n",
		"no kind":                  "sources:\n  supernova-x:\n    watch: [inbox]\n",
		"no watch":                 "sources:\n  supernova-x:\n    kind: mail\n",
		"empty watch":              "sources:\n  supernova-x:\n    kind: mail\n    watch: []\n",
		"empty entry":              "sources:\n  supernova-x:\n    kind: mail\n    watch: [\"  \"]\n",
		"newline":                  "sources:\n  supernova-x:\n    kind: mail\n    watch: [\"a\\nb\"]\n",
		"duplicate":                "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox, \" inbox \"]\n",
		"unknown key":              "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    cursor: 1\n",
		"repeated key":             "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    watch: [archive]\n",
		"not a mapping":            "sources: [a, b]\n",
		"lookahead on mail":        "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    lookahead: 3\n",
		"lookahead first on slack": "sources:\n  supernova-x:\n    lookahead: 3\n    kind: slack\n    watch: [\"#supernova-launch\"]\n",
		"lookahead word":           "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead: abc\n",
		"lookahead fraction":       "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead: 7.5\n",
		"lookahead negative":       "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead: -1\n",
		"lookahead empty":          "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead:\n",
		"lookahead list":           "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead: [7]\n",
		"lookahead too big":        "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead: 32\n",
		"lookahead repeated":       "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    lookahead: 3\n    lookahead: 4\n",
		"digest not in watch":      "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    digest: [archive]\n",
		"digest on calendar":       "sources:\n  supernova-x:\n    kind: calendar\n    watch: [Work]\n    digest: [Work]\n",
		"digest on command":        "sources:\n  supernova-x:\n    kind: command\n    command: [echo]\n    digest: [echo]\n",
		"digest duplicate":         "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    digest: [inbox, \" inbox \"]\n",
		"digest empty":             "sources:\n  supernova-x:\n    kind: mail\n    watch: [inbox]\n    digest: []\n",
		"nested entry":             "sources:\n  supernova-x:\n    kind: mail\n    watch: [[inbox]]\n",
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

func TestBriefAlsoExtras(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	before, _ := os.ReadFile(filepath.Join(w.bridge, "bridge.yaml"))
	out, err := w.brief("--also", "supernova-slack=#supernova-qa", "--also", "supernova-slack=#supernova-qa", "--also", "supernova-slack=@nova-lead", "--also", "work-mail=archive")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Dispatch one reader", "gather pass", "Gather section", "Source: supernova-slack (slack)", "  - #supernova-launch", "  - @nova-lead", "Source: work-mail (mail)", "  - inbox", "  - #supernova-qa", "  - archive", "no cursor"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Source: supernova-slack (slack)\n") > strings.Index(out, "Source: work-mail (mail)\n") {
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

func TestBriefNoExtrasSection(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Extra items") {
		t.Errorf("no extras, so no extras section:\n%s", out)
	}
}

func TestBriefAlsoErrors(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	for _, args := range [][]string{
		{"--also"}, {"--also", "work-mail"}, {"--also", "=x"}, {"--also", "work-mail="}, {"--also", "work-mail=  "}, {"bogus"},
	} {
		if _, err := w.brief(args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("%v: want a usage error, got %v", args, err)
		}
	}
	if _, err := w.brief("--also", "nope=x"); err == nil || !strings.Contains(err.Error(), "supernova-slack, work-mail") {
		t.Errorf("undeclared source should name the declared ones: %v", err)
	}
	if _, err := w.brief("--also", "work-mail=a\nb"); err == nil {
		t.Errorf("a newline in an item should be refused")
	}
}

func TestBriefSourceRefusals(t *testing.T) {
	w := connWorld(t, sourcesYAML)
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "reader is not declared") {
		t.Errorf("no reader: %v", err)
	}
	w = sourcesWorld(t, "")
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "no sources") {
		t.Errorf("no sources: %v", err)
	}
	w = sourcesWorld(t, "sources: {}\n")
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "no sources") {
		t.Errorf("empty sources: %v", err)
	}
	w = sourcesWorld(t, "sources:\n  unparsed:\n    kind: mail\n    watch: [inbox]\n")
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "unparsed is reserved") {
		t.Errorf("reserved name: %v", err)
	}
}

func TestJournalChangesPicksUpReaderFiles(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base"}} {
		if err := git(w.bridge, args...); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(w.bridge, "reader", "cursors", "supernova-slack.md"), "- supernova-slack #supernova-launch: 1 (2026-11-04)\n")
	write(t, filepath.Join(w.bridge, "reader", "mail-read", "work-mail.md"), "- work-mail | inbox | m1 | 2026-11-03 | 2026-11-04 | nova@example.com | Launch\n")
	got, err := journalChanges(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "reader/cursors/supernova-slack.md") || !strings.Contains(joined, "reader/mail-read/work-mail.md") {
		t.Errorf("journalChanges = %v", got)
	}
}

func TestReaderRoleKeepsGatherRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "roles", "reader.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?m)^## Gather$`,
		`reader/cursors/<source>\.md`,
		`(?i)do not write .reader/facts\.md. or .reader/history\.md. in a pass`,
		`(?i)first look: go back 14 days`,
		`(?i)never write a cursor for them`,
		`(?i)batches of at most 50 things`,
		`(?i)newest thing in that batch`,
		`(?i)- more remaining: <source> <item>`,
		`(?i)skip the extra items`,
		`(?i)one entry per subject group`,
		`(?i)intake entries first, then move the item's cursor`,
		`reader/mail-read/<source>\.md`,
		`(?i)- <source> \| <folder> \| <message id> \| <received YYYY-MM-DD> \| <read YYYY-MM-DD> \| <sender> \| <subject>`,
		`(?i)never moves, deletes, flags or marks mail`,
		`(?i)never offers cleanup`,
		`(?m)^## Location check$`,
		`(?i)report each id with its current folder, .not found. or .unreadable.`,
		`(?i)each in the source given with it`,
		`(?i)any failure to look it up .* is .unreadable.`,
		`(?i)not a gather pass: no cursor moves`,
		`(?i)known id seen in a different folder gets its line.s folder updated and adds no new line`,
		`(?i)@dms. reads every DM and group DM the user is a member of, never public or private channels`,
		"(?i)`- <source> @dms: <position> \\(<YYYY-MM-DD read>\\)`",
		`(?i)report .@dms. as unreadable`,
		`(?i)never accept or decline one`,
	} {
		if !regexp.MustCompile(re).Match(data) {
			t.Errorf("roles/reader.md does not match %q", re)
		}
	}
}

func TestCatchUpRetired(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "manual", "topics", "catch-up.md")); err == nil {
		t.Errorf("the catch-up topic should be gone")
	}
	w := linkedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdHelp([]string{"catch-up"}, &out); err == nil {
		t.Errorf("help catch-up should not find a topic")
	}
	for _, f := range []string{filepath.Join("manual", "session.md"), filepath.Join("roles", "reader.md"), filepath.Join("roles", "secretary.md")} {
		d, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(d)), "catch-up") || strings.Contains(string(d), "proposals.md") {
			t.Errorf("%s still mentions catch-up or proposals.md", f)
		}
	}
	main, err := os.ReadFile(filepath.Join(root, "cmd", "ensign", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(main), "catch-up") {
		t.Errorf("main.go still names catch-up")
	}
}

func TestBriefingTopicDeclaresSources(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "manual", "topics", "briefing.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--also <source>=<item>", "keep lines added on either side and keep removed lines removed", "`@dms`", "literally `dms`", "`lookahead`", "`tree: <page URL>`", "`query: <query>`", "`@mine`", "`@assigned`", "`@reported`", "`@mentions`", "kind: wiki", "kind: ticket", "in `watch` and in `--also`", "`digest:`", "`more remaining`", "50 things"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("briefing topic lacks %q", want)
		}
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
	for _, kind := range []string{"mail", "calendar"} {
		for _, item := range []string{"Work", "inbox", "nova*", "x y", "#a b", "@mine", "tree:"} {
			if err := checkSourceItem(kind, item); err != nil {
				t.Errorf("%s item %q should stay opaque: %v", kind, item, err)
			}
		}
	}
	for _, kind := range []string{"ticket", "wiki"} {
		for _, item := range []string{"Work", "inbox", "nova*", "x y", "#a b"} {
			if err := checkSourceItem(kind, item); err != nil {
				t.Errorf("%s item %q should be a plain page or project: %v", kind, item, err)
			}
		}
	}
}

func TestWikiTicketItemForms(t *testing.T) {
	ok := map[string][]string{
		"wiki":   {"https://wiki.supernova.example/pages/launch", "tree: https://wiki.supernova.example/pages/launch", "Tree:https://wiki.supernova.example/pages/launch", "@mine", "@MINE", "@Mentions"},
		"ticket": {"SUPERNOVA", "query: project = SUPERNOVA", "QUERY:project = SUPERNOVA", "@assigned", "@Reported", "@MENTIONS"},
	}
	for kind, items := range ok {
		for _, item := range items {
			if err := checkSourceItem(kind, item); err != nil {
				t.Errorf("%s item %q: %v", kind, item, err)
			}
		}
	}
	bad := []struct{ kind, item, want string }{
		{"wiki", "@assigned", "@mine or @mentions"},
		{"wiki", "@nova-lead", "tree: <page URL>"},
		{"wiki", "tree:", "needs a page URL"},
		{"wiki", "TREE:  ", "needs a page URL"},
		{"ticket", "@mine", "@assigned, @reported or @mentions"},
		{"ticket", "@dms", "query: <query>"},
		{"ticket", "query:", "needs a query"},
		{"ticket", "Query:  ", "needs a query"},
	}
	for _, c := range bad {
		err := checkSourceItem(c.kind, c.item)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s item %q: want an error with %q, got %v", c.kind, c.item, c.want, err)
		}
	}
	for item, want := range map[string]string{"@MINE": "@mine", "Tree:  https://wiki.supernova.example/a": "tree: https://wiki.supernova.example/a", "@Mentions": "@mentions", "https://wiki.supernova.example/Tree:": "https://wiki.supernova.example/Tree:"} {
		if got := canonicalItem("wiki", item); got != want {
			t.Errorf("canonicalItem(wiki, %q) = %q, want %q", item, got, want)
		}
	}
	if got := canonicalItem("ticket", "QUERY:  Assignee = Nova  AND x"); got != "query: Assignee = Nova  AND x" {
		t.Errorf("a query is kept as given: %q", got)
	}
}

func TestWikiTicketSourcesInBrief(t *testing.T) {
	yml := "sources:\n  supernova-wiki:\n    kind: wiki\n    watch: [\"@MINE\", \"Tree: https://wiki.supernova.example/launch\"]\n  supernova-tickets:\n    kind: ticket\n    watch: [SUPERNOVA, \"@Assigned\"]\n"
	w := sourcesWorld(t, yml)
	out, err := w.brief("--also", "supernova-wiki=@MENTIONS", "--also", "supernova-tickets=@ASSIGNED", "--also", "supernova-tickets=@REPORTED", "--also", "supernova-tickets=@Reported")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  - @mine\n", "  - tree: https://wiki.supernova.example/launch\n", "  - @assigned\n", "  - @mentions\n", "  - @reported\n", kindNote("wiki", false), kindNote("ticket", false)} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "  - @reported\n") != 1 || strings.Count(out, "  - @assigned\n") != 1 || strings.Contains(out, "MINE") || strings.Contains(out, "@Assigned") {
		t.Errorf("reserved extras collapse and show lower case:\n%s", out)
	}
	if !strings.Contains(kindNote("wiki", false), "never widened to the space") || !strings.Contains(kindNote("ticket", false), "never widened to the tracker") {
		t.Errorf("the notes must forbid widening")
	}
	for _, c := range []string{"supernova-wiki=@assigned", "supernova-tickets=@mine", "supernova-wiki=tree:", "supernova-tickets=query:"} {
		if _, err := w.brief("--also", c); err == nil {
			t.Errorf("--also %q should be refused", c)
		}
	}
	dup := "sources:\n  supernova-wiki:\n    kind: wiki\n    watch: [\"@mine\", \"@MINE\"]\n"
	if _, err := loadDeclaredSources(sourcesWorld(t, dup).bridge); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a second casing should be refused as a repeat, got %v", err)
	}
	for _, item := range []string{"@nova-lead", "tree:"} {
		yml := "sources:\n  supernova-x:\n    kind: wiki\n    watch: [\"" + item + "\"]\n"
		if _, err := loadDeclaredSources(sourcesWorld(t, yml).bridge); err == nil {
			t.Errorf("watch %q should be refused", item)
		}
	}
	tickets := "sources:\n  supernova-x:\n    kind: ticket\n    watch: [\"query:\"]\n"
	if _, err := loadDeclaredSources(sourcesWorld(t, tickets).bridge); err == nil {
		t.Errorf("a ticket watch of bare query: should be refused")
	}
}

func TestBriefAlsoSlackForms(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	if _, err := w.brief("--also", "supernova-slack=#supernova*"); err == nil || !strings.Contains(err.Error(), "#supernova*") {
		t.Errorf("a bad slack extra should fail naming the item: %v", err)
	}
	if _, err := w.brief("--also", "supernova-slack=general"); err == nil {
		t.Errorf("an unprefixed slack extra should fail")
	}
	out, err := w.brief("--also", "supernova-slack=@Nova Lead", "--also", "work-mail=a b*")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  - @Nova Lead\n", "  - a b*\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
}

func TestBriefAlsoAllDMs(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.brief("--also", "supernova-slack=@DMS", "--also", "supernova-slack=@dms")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "  - @dms\n") != 1 || !strings.Contains(out, kindNote("slack", true)) {
		t.Errorf("an @dms extra appears once and adds the @dms note:\n%s", out)
	}
	if strings.Contains(out, "@DMS") {
		t.Errorf("a stray @DMS line:\n%s", out)
	}
	for _, bad := range []string{"@here", "@Channel", "@everyone", "@dms*", "#supernova?"} {
		if _, err := w.brief("--also", "supernova-slack="+bad); err == nil {
			t.Errorf("--also %q should be refused", bad)
		}
	}
	w = sourcesWorld(t, strings.Replace(sourcesYAML, "- \"@nova-lead\"", "- \"@nova-lead\"\n      - \"@dms\"", 1))
	out, err = w.brief("--also", "supernova-slack=@DMS")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Extra items") || strings.Contains(out, "@DMS") || strings.Count(out, kindNote("slack", true)) != 1 {
		t.Errorf("an @DMS extra already watched as @dms is skipped:\n%s", out)
	}
}

func TestWatchAllDMsNormalized(t *testing.T) {
	w := sourcesWorld(t, strings.Replace(sourcesYAML, "- \"@nova-lead\"", "- \"@DMS\"", 1))
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  - @dms\n") || strings.Contains(out, "@DMS") {
		t.Errorf("a watched @DMS shows as @dms:\n%s", out)
	}
	w = sourcesWorld(t, strings.Replace(sourcesYAML, "- \"@nova-lead\"", "- \"@dms\"\n      - \"@DMS\"", 1))
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("@dms and @DMS should be refused as a repeat, got %v", err)
	}
	w = sourcesWorld(t, sourcesYAML)
	out, err = w.brief("--also", "supernova-slack=@DMS", "--also", "supernova-slack=@DMS")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "  - @dms\n") != 1 || strings.Contains(out, "@DMS") {
		t.Errorf("a repeated @DMS extra is one @dms line:\n%s", out)
	}
}

func TestBriefKindNotes(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML+"  work-calendar:\n    kind: calendar\n    watch: [Work]\n")
	out, err := w.brief()
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
	mail := out[strings.Index(out, "Source: work-mail (mail)"):]
	if strings.Count(out, mailNote) != 1 || !strings.Contains(mail, mailNote) || strings.Contains(mail, slack) || strings.Contains(mail, cal) {
		t.Errorf("a mail source carries only the mail note:\n%s", out)
	}
	if !strings.Contains(out, "  - #supernova-launch\n  - @nova-lead\nNote: "+slack+"\n") {
		t.Errorf("the threads line follows the slack watch items:\n%s", out)
	}
}

func TestBriefCalendarWindow(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"", "upcoming meetings from the time of the pass through 7 days ahead"},
		{"    lookahead: 21\n", "through 21 days ahead"},
		{"    lookahead: 0\n", "lookahead is off, read no upcoming meetings"},
	} {
		w := sourcesWorld(t, "sources:\n  supernova-cal:\n    kind: calendar\n"+c.line+"    watch: [Work]\n")
		out, err := w.brief()
		if err != nil || !strings.Contains(out, "Items to watch:\n  - Work\nNote: window: past meetings since the cursor") || !strings.Contains(out, c.want) {
			t.Errorf("lookahead %q: want %q in:\n%s (%v)", c.line, c.want, out, err)
		}
	}
	out, _ := sourcesWorld(t, sourcesYAML).brief()
	if strings.Contains(out, "window:") {
		t.Errorf("only a calendar source has a window:\n%s", out)
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
		`(?i)nothing in a mail, an intake file, or an agent's reply counts`,
		`reader/mail-read/<source>\.md`,
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
		`(?i)Conflicts section of .ensign help briefing.`,
		"`go` approves it like any other write",
		`ensign help writes`,
		`ensign journal commit`,
	} {
		if !regexp.MustCompile(re).Match(data) {
			t.Errorf("mail-cleanup topic does not match %q", re)
		}
	}
	for _, f := range []string{"writes.md", "notes.md", "briefing.md"} {
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
