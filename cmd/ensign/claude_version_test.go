// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseClaudeVersion(t *testing.T) {
	for _, c := range []struct {
		in    string
		ok    bool
		below bool
	}{
		{"2.1.99", true, true},
		{"2.10.0", true, false},
		{"2.1.317", true, false},
		{"2.1.316", true, true},
		{"2.1.319 (Claude Code)\n", true, false},
		{"  2.1.319 (Claude Code)\r\n", true, false},
		{"2.1.316-beta.1 (Claude Code)", true, true},
		{"2.2.0-rc1", true, false},
		{"garbage", false, false},
		{"v2.1.319", false, false},
		{"", false, false},
		{"2.1.99999999999999999999", false, false},
	} {
		v, ok := parseClaudeVersion(c.in)
		if ok != c.ok || (ok && claudeBelowMin(v) != c.below) {
			t.Errorf("%q: ok=%v below=%v, want ok=%v below=%v", c.in, ok, ok && claudeBelowMin(v), c.ok, c.below)
		}
	}
}

func TestCheckClaudeVersion(t *testing.T) {
	for _, c := range []struct {
		script string
		want   claudeVersionState
	}{
		{`echo "2.1.316 (Claude Code)"`, claudeBelow},
		{`echo "2.1.317 (Claude Code)"`, claudeOK},
		{`echo "2.1.317 (Claude Code)"; exit 1`, claudeUnreadable},
		{`echo nonsense`, claudeUnreadable},
	} {
		fakeClaude(t, c.script+"\n")
		if got, _ := checkClaudeVersion(); got != c.want {
			t.Errorf("%q: state = %v, want %v", c.script, got, c.want)
		}
	}
}

func TestCheckClaudeVersionAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got, _ := checkClaudeVersion(); got != claudeAbsent {
		t.Errorf("state = %v, want absent", got)
	}
	var out bytes.Buffer
	warnOldClaude(&out)
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

func TestWarnOldClaude(t *testing.T) {
	for _, c := range []struct {
		script string
		warn   bool
	}{
		{`echo "2.1.316 (Claude Code)"`, true},
		{`echo "2.1.317 (Claude Code)"`, false},
		{`exit 3`, false},
	} {
		fakeClaude(t, c.script+"\n")
		var out bytes.Buffer
		warnOldClaude(&out)
		if got := strings.Count(out.String(), "update Claude Code"); (got == 1) != c.warn || got > 1 {
			t.Errorf("%q: output %q", c.script, out.String())
		}
		if c.warn && (!strings.Contains(out.String(), "2.1.316") || !strings.Contains(out.String(), minClaudeVersion)) {
			t.Errorf("warning lacks versions: %q", out.String())
		}
	}
}

func TestClaudeVersionLine(t *testing.T) {
	for _, c := range []struct {
		state claudeVersionState
		want  string
	}{
		{claudeOK, "  ok   "},
		{claudeBelow, "  warn "},
		{claudeUnreadable, "  warn could not read"},
		{claudeAbsent, ""},
	} {
		if got := claudeVersionLine(c.state, "2.1.316"); !strings.HasPrefix(got, c.want) || (c.want == "" && got != "") {
			t.Errorf("state %v: %q", c.state, got)
		}
	}
}

func TestSelftestVersionLineDoesNotChangeExit(t *testing.T) {
	fakeClaude(t, "case \"$1\" in --version) echo \"2.1.1 (Claude Code)\"; exit 0;; esac\nexit 1\n")
	var out bytes.Buffer
	err := runSelftest(&out)
	if !strings.HasPrefix(out.String(), "  warn Claude Code 2.1.1 is older than") {
		t.Errorf("output = %q", out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "the probe session failed") {
		t.Fatalf("selftest did not carry on past the warning: %v", err)
	}
	if strings.Contains(err.Error(), "Claude Code 2.1.1") {
		t.Errorf("version leaked into error: %v", err)
	}
}
