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
	got, err := journalChanges(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "reader/cursors.md") || !strings.Contains(joined, "reader/proposals.md") {
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
	for _, want := range []string{"ensign catch-up", "--also <source>=<item>", "reader/proposals.md", "`go` is not the accept word", "ensign help notes", "ensign journal commit"} {
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
	for _, item := range []string{"#supernova-launch", "@nova-lead", "@Nova Lead"} {
		if err := checkSourceItem("slack", item); err != nil {
			t.Errorf("slack item %q: %v", item, err)
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

func TestCatchUpKindNotes(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML+"  work-calendar:\n    kind: calendar\n    watch: [Work]\n")
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	slack, cal := kindNote("slack"), kindNote("calendar")
	if strings.Count(out, slack) != 1 || strings.Count(out, cal) != 1 {
		t.Errorf("want one threads line and one calendar line:\n%s", out)
	}
	mail := out[strings.Index(out, "- work-mail (mail)"):]
	if strings.Contains(mail, slack) || strings.Contains(mail, cal) {
		t.Errorf("a mail source carries no kind note:\n%s", mail)
	}
	if !strings.Contains(out, "  - #supernova-launch\n  - @nova-lead\n  - "+slack+"\n") {
		t.Errorf("the threads line follows the slack watch items:\n%s", out)
	}
}
