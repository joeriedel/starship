// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func focalBridge(t *testing.T, yamlText string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "bridge.yaml"), yamlText)
	write(t, filepath.Join(dir, "notes", "rubric.md"), "# Supernova rubric\n")
	write(t, filepath.Join(dir, "notes", "rubric.txt"), "x\n")
	if err := os.MkdirAll(filepath.Join(dir, "notes", "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadFocal(t *testing.T) {
	dir := focalBridge(t, "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n")
	f, err := loadFocal(dir)
	if err != nil || f == nil || f.Period != "2026-h2" || f.Ends != "2026-12-31" || f.Rubric != "" || f.bragPath() != "notes/focal/2026-h2.brag.md" {
		t.Fatalf("focal = %+v, %v", f, err)
	}
	dir = focalBridge(t, "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: notes/rubric.md\n")
	if f, err = loadFocal(dir); err != nil || f == nil || f.Rubric != "notes/rubric.md" {
		t.Fatalf("focal with rubric = %+v, %v", f, err)
	}
	for _, y := range []string{"use: [impact]\n", "focal:\n", "focal: null\n"} {
		if f, err = loadFocal(focalBridge(t, y)); f != nil || err != nil {
			t.Errorf("%q: want no focal and no error, got %+v, %v", y, f, err)
		}
	}
}

func TestLoadFocalErrors(t *testing.T) {
	cases := []struct{ label, yaml string }{
		{"not a mapping", "focal: 2026-h2\n"},
		{"a list", "focal: [a]\n"},
		{"missing period", "focal:\n  ends: 2026-12-31\n"},
		{"empty period", "focal:\n  period: ''\n  ends: 2026-12-31\n"},
		{"period with separator", "focal:\n  period: a/b\n  ends: 2026-12-31\n"},
		{"period with dots", "focal:\n  period: ..\n  ends: 2026-12-31\n"},
		{"period with backslash", "focal:\n  period: 'a\\b'\n  ends: 2026-12-31\n"},
		{"missing ends", "focal:\n  period: 2026-h2\n"},
		{"bad ends", "focal:\n  period: 2026-h2\n  ends: end of year\n"},
		{"impossible ends", "focal:\n  period: 2026-h2\n  ends: 2026-02-30\n"},
		{"rubric outside notes", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: projects/rubric.md\n"},
		{"rubric escapes notes", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: notes/../bridge.yaml\n"},
		{"rubric absolute", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: /notes/rubric.md\n"},
		{"rubric not md", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: notes/rubric.txt\n"},
		{"rubric missing", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: notes/none.md\n"},
		{"rubric a folder", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: notes/dir.md\n"},
		{"unknown key", "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubrik: notes/rubric.md\n"},
		{"nested value", "focal:\n  period: [a]\n  ends: 2026-12-31\n"},
	}
	for _, c := range cases {
		dir := focalBridge(t, c.yaml)
		f, err := loadFocal(dir)
		if err == nil || f != nil || !strings.HasPrefix(err.Error(), "bridge.yaml: focal: ") {
			t.Errorf("%s: want a bridge.yaml: focal: error, got %+v, %v", c.label, f, err)
		}
		if _, err := loadBridgeFile(dir); err != nil {
			t.Errorf("%s: a malformed focal: must not break loadBridgeFile: %v", c.label, err)
		}
	}
}

func TestFocalWithoutImpactWarns(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "focal:\n  period: 2026-h2\n  ends: 2026-12-31\n")
	got := impactWarnings(w.bridge)
	if len(got) != 1 || !strings.Contains(got[0], "focal is ignored because use: does not name the impact extension") {
		t.Errorf("want the focal warning, got %q", got)
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "use: [impact]\nfocal:\n  period: 2026-h2\n  ends: 2026-12-31\n")
	if got := impactWarnings(w.bridge); len(got) != 0 {
		t.Errorf("warned although the bridge uses impact: %q", got)
	}
}

func TestBriefNamesFocal(t *testing.T) {
	focal := "use: [impact]\nfocal:\n  period: 2026-h2\n  ends: 2026-12-31\n  rubric: notes/rubric.md\n"
	w := sourcesWorld(t, sourcesYAML+focal)
	write(t, filepath.Join(w.bridge, "notes", "rubric.md"), "# Supernova rubric\n")
	out, err := w.brief()
	if err != nil {
		t.Fatal(err)
	}
	closing := out[strings.LastIndex(out, "When all the readers"):]
	for _, want := range []string{"focal: period 2026-h2", "ends 2026-12-31", "notes/focal/2026-h2.brag.md", "rubric notes/rubric.md"} {
		if !strings.Contains(closing, want) {
			t.Errorf("closing instruction lacks %q:\n%s", want, closing)
		}
	}

	w = sourcesWorld(t, sourcesYAML+"use: [impact]\nfocal:\n  period: 2026-h2\n  ends: 2026-12-31\n")
	if out, err = w.brief(); err != nil || !strings.Contains(out, "default themes apply") {
		t.Errorf("no rubric should say the defaults apply: %v\n%s", err, out)
	}

	w = sourcesWorld(t, sourcesYAML+"use: [impact]\nfocal:\n  period: 2026-h2\n")
	out, err = w.brief()
	if err != nil || !strings.Contains(out, "Problem: bridge.yaml: focal: ends is missing") || !strings.Contains(out, "Brief for work-mail:") || strings.Contains(out, "brag file") {
		t.Errorf("a malformed focal: should print a Problem line and leave the brief working: %v\n%s", err, out)
	}

	w = sourcesWorld(t, sourcesYAML+"focal:\n  period: 2026-h2\n  ends: 2026-12-31\n")
	if out, err = w.brief(); err != nil || strings.Contains(out, "brag file") || strings.Contains(out, "Problem:") {
		t.Errorf("focal without impact should be ignored: %v\n%s", err, out)
	}
}

func TestFocalTexts(t *testing.T) {
	read := func(parts ...string) string {
		data, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	role := read("roles", "secretary.md")
	for _, want := range []string{"`<bridge>/notes/secretary/` and `<bridge>/notes/focal/`", "only when your brief names a focal", "notes/focal/<period>.brag.md", "Business impact, Cross-org reach, Leadership and influence", "`[inferred]`", "never in the task file", "a confirmed line drops its mark", "a rubric theme with nothing under it", "within 2 weeks", "counts for <period>.` line overrides that date", "gives no start: do not guess one"} {
		if !strings.Contains(role, want) {
			t.Errorf("secretary role lacks %q", want)
		}
	}
	topic := read("manual", "topics", "impact.md")
	for _, want := range []string{"focal:", "period: 2026-h2", "ends:", "rubric:", "living document", "relaxed", "ships no rubric content", "polishes the living file", "gives the secretary no start"} {
		if !strings.Contains(topic, want) {
			t.Errorf("impact topic lacks %q", want)
		}
	}
	briefing := read("manual", "topics", "briefing.md")
	for _, want := range []string{"focal", "brag file path", "notes/focal/"} {
		if !strings.Contains(briefing, want) {
			t.Errorf("briefing topic lacks %q", want)
		}
	}
	if help := read("extensions", "impact", "extension.yaml"); !strings.Contains(help, "polish the living file") {
		t.Error("impact help does not mention the living file")
	}
}
