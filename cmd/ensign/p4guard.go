// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"slices"
	"strings"
)

// p4Change returns the command in cmd that is a p4 command other than a read-only one, in a form fit for a denial, or "" when there is none. It reads commands the way remoteChange does. It is a guard against accidents by an agent, not a security boundary: scripts, aliases and variables holding a command are not looked into.
func p4Change(cmd string) string {
	if !strings.Contains(strings.ToLower(cmd), "p4") {
		return ""
	}
	return scanShell(cmd, 0, true)
}

// p4ValueOptions are the global options p4 takes a value for, as the next word or attached to the option.
const p4ValueOptions = "cCdHLpPQruvxzbE"

// p4FlagOptions are the global options p4 takes no value for.
var p4FlagOptions = []string{"-I", "-s", "-q", "-G", "-V", "-h", "-Ztag"}

// p4ReadOnly are the p4 commands that only report.
var p4ReadOnly = []string{"info", "fstat", "opened", "changes", "changelists", "describe", "diff", "diff2", "files", "filelog", "print", "where", "have", "annotate", "dirs", "sizes", "grep", "help", "depots", "users", "clients", "workspaces", "status", "labels", "branches", "streams", "jobs", "integrated", "interchanges"}

// p4Refuses reports whether a p4 command line (the words after p4) is not read-only. Global options before the subcommand are skipped: those in p4ValueOptions take a value, the rest take none. A bare option letter that p4 may know as a flag or a value-taking option (an unknown one) is handled as git's is: the word after it is tried as the subcommand and, if it is not a read-only command, refused, so that a write behind a global option a later p4 adds is still caught. A bare p4 is allowed; it only prints help.
func p4Refuses(args []string) bool {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		w := args[i]
		switch {
		case len(w) == 2 && strings.Contains(p4ValueOptions, w[1:]):
			i++
		case len(w) > 2 && strings.Contains(p4ValueOptions, w[1:2]):
		case slices.Contains(p4FlagOptions, w):
		default:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				if p4SubcommandRefuses(args[i+1], args[i+2:]) {
					return true
				}
				i++
			}
		}
		i++
	}
	if i >= len(args) {
		return false
	}
	return p4SubcommandRefuses(args[i], args[i+1:])
}

// p4SubcommandRefuses reports whether the p4 subcommand sub with the words rest after it is not read-only: a command on p4ReadOnly, or client, workspace, change, changelist with -o (and not -i or -d), or login -s.
func p4SubcommandRefuses(sub string, rest []string) bool {
	switch sub {
	case "client", "workspace", "change", "changelist":
		return !slices.Contains(rest, "-o") || slices.Contains(rest, "-i") || slices.Contains(rest, "-d")
	case "login":
		return !slices.Contains(rest, "-s")
	}
	return !slices.Contains(p4ReadOnly, sub)
}
