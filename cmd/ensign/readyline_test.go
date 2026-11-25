// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadyLine(t *testing.T) {
	bridge := filepath.Join("home", "supernova")
	for _, c := range []struct{ version, want string }{
		{"v0.0.57", "Starship ready · bridge v0.0.57 (supernova)"},
		{"0.0.57", "Starship ready · bridge v0.0.57 (supernova)"},
		{"dev", "Starship ready · bridge (supernova)"},
		{"", "Starship ready · bridge (supernova)"},
	} {
		if got := readyLine(c.version, bridge); got != c.want {
			t.Errorf("readyLine(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestSessionStartLineVersionAfterBridge(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "v0.0.57"
	w := newWorld(t)
	show := sessionStartLine(t, w)
	head := "Starship ready · bridge v0.0.57 (bridge)"
	if !strings.HasPrefix(show, head) {
		t.Fatalf("session start line %q: want head %q", show, head)
	}
	if rest, want := show[len(head):], " · Bash guards missing: see .claude/settings.json"; rest != want {
		t.Errorf("session start line %q: after the head got %q, want %q", show, rest, want)
	}
}
