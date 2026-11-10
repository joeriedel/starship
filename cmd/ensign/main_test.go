// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
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

// fakeP4 answers the p4 commands the workspace inspection runs, in -ztag form. STARSHIP_TEST_P4_LOGIN=fail and STARSHIP_TEST_P4_CLIENT=unknown make those commands fail; STARSHIP_TEST_P4_PENDING lists the pending changelists ("101 102"), STARSHIP_TEST_P4_OPENED the opened files' changelists ("101 101 default"). STARSHIP_TEST_P4_NEED names a variable that must be set for any command to succeed.
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
	if rest[0] == "-ztag" {
		rest = rest[1:]
	}
	switch rest[0] {
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
	default:
		os.Stderr.WriteString("fake p4: unknown command " + rest[0] + "\n")
		os.Exit(2)
	}
	os.Exit(0)
}
