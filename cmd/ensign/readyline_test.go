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
	head := readyLine(version, w.bridge)
	if !strings.HasPrefix(show, head) || strings.Contains(show[len(head):], "0.0.57") {
		t.Errorf("session start line %q: want head %q and no later version", show, head)
	}
	if rest := show[len(head):]; rest != "" && !strings.HasPrefix(rest, " · ") {
		t.Errorf("session start line %q: notes after the name must start with a separator", show)
	}
}
