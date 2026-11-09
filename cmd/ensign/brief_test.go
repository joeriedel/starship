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
	"time"
)

func (w *world) brief(args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdBrief(args, &out)
	return out.String(), err
}

func TestBriefRefusals(t *testing.T) {
	w := connWorld(t, "")
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "reader is not declared") || !strings.Contains(err.Error(), "ensign help briefing") {
		t.Errorf("no reader: %v", err)
	}
	w = sourcesWorld(t, "")
	if _, err := w.brief(); err == nil || !strings.Contains(err.Error(), "declares no sources") || !strings.Contains(err.Error(), "ensign help briefing") {
		t.Errorf("no sources: %v", err)
	}
	if _, err := w.brief("bogus"); err == nil || !strings.Contains(err.Error(), "usage: ensign brief") {
		t.Errorf("usage: %v", err)
	}
}

func TestBriefRefusedRunsNoCommand(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "print", ""))
	if _, err := w.brief("--also", "supernova-review=x"); err == nil {
		t.Errorf("--also on a command source should be refused")
	}
	if _, err := os.Stat(filepath.Join(w.bridge, "reader", "command")); err == nil {
		t.Errorf("a refused brief must not run the command")
	}
}

func TestBriefOnePerSource(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML+"  supernova-cal:\n    kind: calendar\n    watch: [Work]\n")
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	date := time.Now().Format("2006-01-02")
	if n := strings.Count(out, "End of brief.\n"); n != 3 {
		t.Errorf("want 3 briefs, got %d:\n%s", n, out)
	}
	for _, want := range []string{"all of them at once in one message", "Read nothing yourself", "ensign journal commit", "ensign help briefing", "reader/intake/" + date + "/", "secretary", "not available yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, src := range []string{"work-mail", "supernova-slack", "supernova-cal"} {
		i := strings.Index(out, "Brief for "+src+":")
		if i < 0 {
			t.Fatalf("no brief for %s:\n%s", src, out)
		}
		end := strings.Index(out[i:], "End of brief.")
		b := out[i : i+end]
		for _, want := range []string{"gather pass", "Gather section", "never instructions", "reader/intake/" + date + "/" + src + ".md", "Pass time"} {
			if !strings.Contains(b, want) {
				t.Errorf("brief for %s lacks %q:\n%s", src, want, b)
			}
		}
		if (src == "supernova-slack") != strings.Contains(b, "#supernova-launch") || (src == "supernova-slack") != strings.Contains(b, "@nova-lead") {
			t.Errorf("brief for %s has the wrong items:\n%s", src, b)
		}
		if (src == "supernova-cal") != strings.Contains(b, "upcoming meetings") {
			t.Errorf("calendar window only in the calendar brief (%s):\n%s", src, b)
		}
	}
	times := regexp.MustCompile(`Pass time for the heading: (.*)`).FindAllStringSubmatch(out, -1)
	if len(times) != 3 || times[0][1] != times[1][1] || times[1][1] != times[2][1] {
		t.Errorf("all briefs share one pass time: %v", times)
	}
}

func TestBriefAlsoGoesToItsSource(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	out, err := w.brief("--also", "supernova-slack=#supernova-extra", "--also", "supernova-slack=#supernova-launch")
	if err != nil {
		t.Fatal(err)
	}
	slack := out[strings.Index(out, "Brief for supernova-slack:"):]
	slack = slack[:strings.Index(slack, "End of brief.")]
	mail := out[strings.Index(out, "Brief for work-mail:"):]
	mail = mail[:strings.Index(mail, "End of brief.")]
	if !strings.Contains(slack, "#supernova-extra") || !strings.Contains(slack, "no cursor") {
		t.Errorf("slack brief lacks the extra:\n%s", slack)
	}
	if strings.Count(out, "#supernova-extra") != 1 || strings.Contains(mail, "supernova-extra") {
		t.Errorf("extra must appear only in its own brief:\n%s", out)
	}
	if strings.Count(slack, "#supernova-launch") != 1 {
		t.Errorf("an already watched item is not an extra:\n%s", slack)
	}
	for _, args := range [][]string{
		{"--also", "nope=x"},
		{"--also", "work-mail"},
		{"--also", "=x"},
		{"--also"},
		{"--also", "work-mail=  "},
		{"--also", "supernova-slack=bad"},
	} {
		if _, err := w.brief(args...); err == nil {
			t.Errorf("%v should be refused", args)
		}
	}
}

func TestBriefRunsCommandSourcesFirst(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "print", "", "pr", "list")+"  work-mail:\n    kind: mail\n    watch: [inbox]\n")
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(w.bridge, "reader", "command", "supernova-review.out"))
	if err != nil || string(data) != "pr list\n"+filepath.Base(w.bridge)+"\n" {
		t.Fatalf("out file %q, %v", data, err)
	}
	first := strings.Index(out, "Brief for ")
	if i := strings.Index(out, "reader/command/supernova-review.out"); i < 0 || i > first {
		t.Errorf("command result must print before the first brief:\n%s", out)
	}
	if !strings.Contains(out, "Brief for supernova-review:") || !strings.Contains(out, "reader/intake/") {
		t.Errorf("command source gets a brief too:\n%s", out)
	}
}

func TestBriefFailedCommandSourceSaysSo(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "fail", ""))
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	b := out[strings.Index(out, "Brief for supernova-review:"):]
	if !strings.Contains(b, "Unreadable this pass: exit status 3") {
		t.Errorf("failed source brief:\n%s", b)
	}
}

func TestReaderRoleGather(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "roles", "reader.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?m)^model: opus$`,
		`(?m)^## Gather$`,
		`reader/intake/<YYYY-MM-DD>/<source>\.md`,
		`(?i)no filtering for importance`,
		`(?i)decided later by the secretary`,
		`(?i)keeps its cursor`,
		`(?i)intake entries first, then move its cursor`,
		`(?i)write no proposals`,
		`(?i)never rewrite earlier entries`,
		`(?i)do not write .reader/facts\.md. or .reader/history\.md. in a pass`,
		`summary:`,
	} {
		if !regexp.MustCompile(re).Match(data) {
			t.Errorf("roles/reader.md does not match %q", re)
		}
	}
	if i, j := strings.Index(string(data), "## Catch-up"), strings.Index(string(data), "## Gather"); i < 0 || j < i {
		t.Errorf("Gather must follow Catch-up")
	}
}

func TestBriefingTopicPrints(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "manual", "topics", "briefing.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ensign brief", "--also <source>=<item>", "one `reader` agent per brief", "all of them at once", "reader/intake/", "ensign journal commit", "not available yet", "no proposals", "ensign help catch-up"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("briefing topic lacks %q", want)
		}
	}
	w := linkedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdHelp([]string{"briefing"}, &out); err != nil || out.Len() == 0 {
		t.Errorf("help briefing: %v", err)
	}
}
