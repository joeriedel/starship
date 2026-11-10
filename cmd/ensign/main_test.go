// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets this test binary stand in for a released ensign: with STARSHIP_TEST_ENSIGN set it records its arguments in STARSHIP_TEST_ENSIGN_LOG, then fails if STARSHIP_TEST_ENSIGN_FAIL is set, and otherwise runs as ensign.
func TestMain(m *testing.M) {
	if mode := os.Getenv("STARSHIP_TEST_FAKE_CMD"); mode != "" {
		fakeCommand(mode)
	}
	if os.Getenv("STARSHIP_TEST_ENSIGN") != "" {
		if log := os.Getenv("STARSHIP_TEST_ENSIGN_LOG"); log != "" {
			if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.WriteString(strings.Join(os.Args[1:], " ") + "\n")
				f.Close()
			}
		}
		if os.Getenv("STARSHIP_TEST_ENSIGN_FAIL") != "" {
			os.Stderr.WriteString("fake ensign: link failed\n")
			os.Exit(1)
		}
		main()
		os.Exit(0)
	}
	// A SessionStart without CLAUDE_ENV_FILE reports that starship cannot work in the session, so the tests that start sessions get a file of their own, which also keeps them from writing to the env file of the session running them.
	dir, err := os.MkdirTemp("", "ensign-test-env-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_ENV_FILE", filepath.Join(dir, "env"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeCommand is the program a command-source test runs: this test binary with STARSHIP_TEST_FAKE_CMD naming what it does. print writes its arguments and the working directory's base name, fail writes eight stderr lines and exits 3, sleep outlasts any timeout, big writes more than 1 MB, env writes the variable named by STARSHIP_TEST_FAKE_VAR.
func fakeCommand(mode string) {
	switch mode {
	case "print":
		wd, _ := os.Getwd()
		os.Stdout.WriteString(strings.Join(os.Args[1:], " ") + "\n" + filepath.Base(wd) + "\n")
	case "fail":
		for i := 1; i <= 8; i++ {
			os.Stderr.WriteString("fake error " + strconv.Itoa(i) + "\n")
		}
		os.Exit(3)
	case "sleep":
		time.Sleep(time.Minute)
	case "big":
		line := strings.Repeat("x", 1023) + "\n"
		for i := 0; i < 1024+100; i++ {
			os.Stdout.WriteString(line)
		}
	case "env":
		os.Stdout.WriteString(os.Getenv(os.Getenv("STARSHIP_TEST_FAKE_VAR")) + "\n")
	case "p4":
		fakeP4(os.Args[1:])
	}
	os.Exit(0)
}

// fakeP4 answers the p4 commands the workspace inspection and the changelist handling run, in -ztag form. `change -i` creates changelist STARSHIP_TEST_P4_NEWCHANGE (4242); fstat knows the files in STARSHIP_TEST_P4_FILES ("a.go=head b.go=open:101"); reconcile finds STARSHIP_TEST_P4_RECONCILE ("a.go:edit b.go:add"); opened also lists STARSHIP_TEST_P4_OPENFILES ("a.go:102", or "b.go:102:add" with an action); submit and shelve succeed unless STARSHIP_TEST_P4_RECORD_FAIL holds a message; diff, describe and where answer with fixed text; STARSHIP_TEST_P4_OPEN_FAIL makes edit and add fail with its text; every command is appended to the file STARSHIP_TEST_P4_LOG. STARSHIP_TEST_P4_LOGIN=fail and STARSHIP_TEST_P4_CLIENT=unknown make those commands fail; STARSHIP_TEST_P4_PENDING lists the pending changelists ("101 102"), STARSHIP_TEST_P4_OPENED the opened files' changelists ("101 101 default"). STARSHIP_TEST_P4_NEED names a variable that must be set for any command to succeed.
func fakeP4(args []string) {
	if len(args) < 3 || args[0] != "-c" {
		os.Stderr.WriteString("fake p4: expected -c <client>\n")
		os.Exit(2)
	}
	client, rest := args[1], args[2:]
	if need := os.Getenv("STARSHIP_TEST_P4_NEED"); need != "" && os.Getenv(need) == "" {
		os.Stderr.WriteString("fake p4: " + need + " is not set\n")
		os.Exit(1)
	}
	stdin := ""
	if len(rest) > 2 && rest[0] == "-x" && rest[1] == "-" {
		data, _ := io.ReadAll(os.Stdin)
		stdin, rest = string(data), rest[2:]
	}
	if rest[0] == "-ztag" {
		rest = rest[1:]
	}
	if log := os.Getenv("STARSHIP_TEST_P4_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.WriteString(strings.Join(rest, " ") + "\n")
			f.Close()
		}
	}
	switch rest[0] {
	case "change":
		form, _ := io.ReadAll(os.Stdin)
		if !strings.Contains(string(form), "Description:\n\tsupernova/") {
			os.Stderr.WriteString("fake p4: change form without the task's name\n")
			os.Exit(1)
		}
		n := os.Getenv("STARSHIP_TEST_P4_NEWCHANGE")
		if n == "" {
			n = "4242"
		}
		os.Stdout.WriteString("Change " + n + " created.\n")
	case "fstat":
		base := filepath.Base(strings.NewReplacer("%25", "%", "%40", "@", "%23", "#", "%2A", "*").Replace(rest[len(rest)-1]))
		for _, f := range strings.Fields(os.Getenv("STARSHIP_TEST_P4_FILES")) {
			name, state, _ := strings.Cut(f, "=")
			if name != base {
				continue
			}
			os.Stdout.WriteString("... depotFile //supernova/main/" + name + "\n... headAction edit\n")
			if cl, ok := strings.CutPrefix(state, "open:"); ok {
				os.Stdout.WriteString("... action edit\n... change " + cl + "\n")
			}
			os.Stdout.WriteString("\n")
		}
	case "edit", "add":
		if need := os.Getenv("STARSHIP_TEST_P4_OPEN_FAIL"); need != "" {
			os.Stderr.WriteString("fake p4: " + need + "\n")
			os.Exit(1)
		}
	case "reconcile":
		wd, _ := os.Getwd()
		listed := map[string]bool{}
		for _, l := range strings.Fields(stdin) {
			listed[filepath.Base(l)] = true
		}
		for _, f := range strings.Fields(os.Getenv("STARSHIP_TEST_P4_RECONCILE")) {
			name, action, _ := strings.Cut(f, ":")
			if stdin != "" && !listed[name] {
				continue
			}
			os.Stdout.WriteString("... depotFile //supernova/main/" + name + "\n... clientFile " + filepath.Join(wd, name) + "\n... action " + action + "\n\n")
		}
	case "login":
		if os.Getenv("STARSHIP_TEST_P4_LOGIN") == "fail" {
			os.Stderr.WriteString("Perforce password (P4PASSWD) invalid or unset.\nsecond line\n")
			os.Exit(1)
		}
	case "clients":
		if os.Getenv("STARSHIP_TEST_P4_CLIENT") != "unknown" {
			os.Stdout.WriteString("... client " + client + "\n... Owner supernova\n\n")
		}
	case "changes":
		for _, c := range strings.Fields(os.Getenv("STARSHIP_TEST_P4_PENDING")) {
			os.Stdout.WriteString("... change " + c + "\n... client " + client + "\n... status pending\n\n")
		}
	case "opened":
		for i, c := range strings.Fields(os.Getenv("STARSHIP_TEST_P4_OPENED")) {
			os.Stdout.WriteString("... depotFile //supernova/main/f" + strconv.Itoa(i) + ".go\n... change " + c + "\n\n")
		}
		for _, f := range strings.Fields(os.Getenv("STARSHIP_TEST_P4_OPENFILES")) {
			name, c, _ := strings.Cut(f, ":")
			c, action, _ := strings.Cut(c, ":")
			os.Stdout.WriteString("... depotFile //supernova/main/" + name + "\n... change " + c + "\n")
			if action != "" {
				os.Stdout.WriteString("... action " + action + "\n")
			}
			os.Stdout.WriteString("\n")
		}
	case "submit", "shelve":
		if msg := os.Getenv("STARSHIP_TEST_P4_RECORD_FAIL"); msg != "" {
			os.Stderr.WriteString("fake p4: " + msg + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("Change " + rest[len(rest)-1] + " " + rest[0] + " done.\n")
	case "diff":
		name := filepath.Base(rest[len(rest)-1])
		os.Stdout.WriteString("--- //supernova/main/" + name + "#1\n+++ " + name + "\n@@ -1 +1 @@\n-old\n+new\n")
	case "describe":
		os.Stdout.WriteString("Change " + rest[len(rest)-1] + " by supernova@" + client + " *pending*\n\nShelved files ...\n+shelved line\n")
	case "where":
		wd, _ := os.Getwd()
		local := strings.NewReplacer("%25", "%", "%40", "@", "%23", "#", "%2A", "*").Replace(filepath.Base(rest[len(rest)-1]))
		os.Stdout.WriteString("... depotFile " + rest[len(rest)-1] + "\n... path " + filepath.Join(wd, local) + "\n\n")
	default:
		os.Stderr.WriteString("fake p4: unknown command " + rest[0] + "\n")
		os.Exit(2)
	}
	os.Exit(0)
}
