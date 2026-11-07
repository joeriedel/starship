// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// minClaudeVersion is the lowest Claude Code version CLAUDE_PID has been seen on.
const minClaudeVersion = "2.1.317"

var claudeVersionRe = regexp.MustCompile(`^\s*(\d+)\.(\d+)\.(\d+)`)

// parseClaudeVersion reads the leading major.minor.patch of `claude --version` output. A number too large to parse is unreadable.
func parseClaudeVersion(s string) ([3]int, bool) {
	var v [3]int
	m := claudeVersionRe.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// claudeBelowMin reports whether version v is lower than the minimum.
func claudeBelowMin(v [3]int) bool {
	min, _ := parseClaudeVersion(minClaudeVersion)
	for i := range v {
		if v[i] != min[i] {
			return v[i] < min[i]
		}
	}
	return false
}

type claudeVersionState int

const (
	claudeAbsent claudeVersionState = iota
	claudeUnreadable
	claudeBelow
	claudeOK
)

// checkClaudeVersion runs `claude --version`. The version is the parsed major.minor.patch, and is set for claudeBelow and claudeOK.
func checkClaudeVersion() (claudeVersionState, string) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return claudeAbsent, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = cleanClaudeEnv()
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return claudeUnreadable, ""
	}
	v, ok := parseClaudeVersion(string(out))
	if !ok {
		return claudeUnreadable, ""
	}
	text := fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
	if claudeBelowMin(v) {
		return claudeBelow, text
	}
	return claudeOK, text
}

// claudeVersionWarning is the text shared by setup and the selftest for an installed version below the minimum.
func claudeVersionWarning(installed string) string {
	return fmt.Sprintf("Claude Code %s is older than %s, the oldest version starship expects; update Claude Code", installed, minClaudeVersion)
}

// warnOldClaude prints the warning when the installed Claude Code is below the minimum, and nothing otherwise.
func warnOldClaude(out io.Writer) {
	if state, v := checkClaudeVersion(); state == claudeBelow {
		fmt.Fprintf(out, "warning: %s\n", claudeVersionWarning(v))
	}
}

// claudeVersionLine is the selftest's advisory line for a check result, or "" when claude is absent.
func claudeVersionLine(state claudeVersionState, v string) string {
	switch state {
	case claudeOK:
		return fmt.Sprintf("  ok   Claude Code %s is at least %s\n", v, minClaudeVersion)
	case claudeBelow:
		return fmt.Sprintf("  warn %s\n", claudeVersionWarning(v))
	case claudeUnreadable:
		return "  warn could not read the Claude Code version from `claude --version`\n"
	}
	return ""
}
