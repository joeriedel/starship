// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain lets this test binary stand in for a released ensign: with STARSHIP_TEST_ENSIGN set it records its arguments in STARSHIP_TEST_ENSIGN_LOG, then fails if STARSHIP_TEST_ENSIGN_FAIL is set, and otherwise runs as ensign.
func TestMain(m *testing.M) {
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
	os.Exit(m.Run())
}
