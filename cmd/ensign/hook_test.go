// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"reflect"
	"strings"
	"testing"
)

func reportWith(lines ...string) string {
	return "done\n\n```starship-report\n" + strings.Join(lines, "\n") + "\n```\n"
}

func TestParseReportFiles(t *testing.T) {
	stage := &Stage{Outcomes: Outcomes{{Name: "compiled"}, {Name: "blocked"}}, ReportFields: []string{"files"}}
	cases := []struct {
		name  string
		files string
		want  []string
	}{
		{"plain list", `a.txt, b.txt`, []string{"a.txt", "b.txt"}},
		{"no spaces", `a.txt,b.txt`, []string{"a.txt", "b.txt"}},
		{"brackets", `[a.txt, b.txt]`, []string{"a.txt", "b.txt"}},
		{"single quotes", `'a.txt', 'b.txt'`, []string{"a.txt", "b.txt"}},
		{"empty entries dropped", `a.txt,, b.txt,`, []string{"a.txt", "b.txt"}},
		{"quoted comma name alone", `"a,b.txt"`, []string{"a,b.txt"}},
		{"quoted comma name among others", `x.txt, "a,b.txt", y.txt`, []string{"x.txt", "a,b.txt", "y.txt"}},
		{"quoted comma name first and last", `"a,b.txt", "c,d.txt"`, []string{"a,b.txt", "c,d.txt"}},
		{"doubled quotes", `"say ""hi"".txt"`, []string{`say "hi".txt`}},
		{"windows path in quotes", `"C:\ws\lib,v2.txt"`, []string{`C:\ws\lib,v2.txt`}},
		{"windows path unquoted", `C:\ws\lib.txt`, []string{`C:\ws\lib.txt`}},
		{"quoted name with brackets", `"[a].txt"`, []string{"[a].txt"}},
		{"quoted name keeps spaces and single quotes", `" it's.txt "`, []string{" it's.txt "}},
		{"tab before quoted comma name", "x.txt,\t\"a,b.txt\"", []string{"x.txt", "a,b.txt"}},
		{"tab before comma after closing quote", "\"a.txt\"\t, b.txt", []string{"a.txt", "b.txt"}},
		{"quoted list in brackets", `["a,b.txt", c.txt]`, []string{"a,b.txt", "c.txt"}},
	}
	for _, c := range cases {
		_, _, fields, problem := parseReport(reportWith("outcome: compiled", "summary: s", "files: "+c.files), stage)
		if problem != "" {
			t.Errorf("%s: problem %q", c.name, problem)
			continue
		}
		if !reflect.DeepEqual(fields["files"], c.want) {
			t.Errorf("%s: got %q, want %q", c.name, fields["files"], c.want)
		}
	}
}

func TestParseReportMalformedFiles(t *testing.T) {
	stage := &Stage{Outcomes: Outcomes{{Name: "compiled"}, {Name: "blocked"}}, ReportFields: []string{"files"}}
	for _, files := range []string{`"a,b.txt`, `"a.txt"x, b.txt`, `x.txt, "a,b.txt`, "x.txt,\t\"a,b.txt"} {
		for _, outcome := range []string{"compiled", "blocked"} {
			_, _, _, problem := parseReport(reportWith("outcome: "+outcome, "files: "+files), stage)
			if !strings.Contains(problem, "double quote") {
				t.Errorf("%s %s: problem %q should name the quoting form", outcome, files, problem)
			}
		}
	}
}

func TestParseReportOtherRules(t *testing.T) {
	stage := &Stage{Outcomes: Outcomes{{Name: "compiled"}, {Name: "blocked"}}, ReportFields: []string{"files"}}
	msg := reportWith("outcome: blocked", "files: old.txt") + reportWith("outcome: compiled", "summary: last", "files: new.txt")
	outcome, summary, fields, problem := parseReport(msg, stage)
	if problem != "" || outcome != "compiled" || summary != "last" || !reflect.DeepEqual(fields["files"], []string{"new.txt"}) {
		t.Errorf("last block should win: %q %q %v %q", outcome, summary, fields, problem)
	}
	if _, _, _, problem := parseReport(reportWith("outcome: nope", "files: a.txt"), stage); !strings.Contains(problem, "not one of this stage's outcomes") {
		t.Errorf("invalid outcome: %q", problem)
	}
	if _, _, _, problem := parseReport(reportWith("outcome: compiled"), stage); !strings.Contains(problem, "no files line") {
		t.Errorf("missing files: %q", problem)
	}
	if _, _, _, problem := parseReport(reportWith("outcome: blocked"), stage); problem != "" {
		t.Errorf("blocked needs no files: %q", problem)
	}
}

func TestReportInstructionsStateTheFilesForm(t *testing.T) {
	got := reportInstructions(&Stage{Outcomes: Outcomes{{Name: "compiled"}}, ReportFields: []string{"files"}})
	if !strings.Contains(got, "double quotes") || !strings.Contains(got, `""`) {
		t.Errorf("instructions should state the quoting form: %s", got)
	}
}
