// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newGuardWorld is newWorld with the guard's system temp folders replaced by one empty folder outside the world, so only the folder it returns is allowed as temp.
func newGuardWorld(t *testing.T) (*world, string) {
	t.Helper()
	w := newWorld(t)
	tmp := t.TempDir()
	old := editTempFolders
	editTempFolders = func() []string { return []string{tmp} }
	t.Cleanup(func() { editTempFolders = old })
	return w, tmp
}

// tryEdit sends an Edit for path and reports whether the guard refused it.
func (w *world) tryEdit(agent, agentType, path string) (bool, string) {
	w.t.Helper()
	in := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": path}}
	if agent != "" {
		in["agent_id"], in["agent_type"] = agent, agentType
	}
	res, code := w.hook("PreToolUse", in)
	if code != 0 {
		w.t.Fatalf("PreToolUse exited %d: %v", code, res)
	}
	return dig(res, "hookSpecificOutput", "permissionDecision") == "deny", dig(res, "hookSpecificOutput", "permissionDecisionReason")
}

func (w *world) workspace() string { return filepath.Join(filepath.Dir(w.bridge), "workspace") }

func (w *world) scratch() string {
	return filepath.Join(runsDir(w.bridge), w.state().Run, "scratch")
}

func (w *world) expectEdit(agent, agentType, path string, refused bool) {
	w.t.Helper()
	got, why := w.tryEdit(agent, agentType, path)
	if got != refused {
		w.t.Errorf("%s edit of %s: refused=%v (%s), want %v", agentType, path, got, why, refused)
	}
}

func TestSessionEditFolders(t *testing.T) {
	w, tmp := newGuardWorld(t)
	extra := filepath.Join(t.TempDir(), "extra")
	os.MkdirAll(extra, 0o755)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\nsession-edit:\n  - "+extra+"\n  - notes\n")
	w.expectEdit("", "", filepath.Join(w.bridge, "projects/supernova/project.yaml"), false)
	w.expectEdit("", "", filepath.Join(w.bridge, "notes/new/a.md"), false)
	w.expectEdit("", "", filepath.Join(tmp, "x.txt"), false)
	w.expectEdit("", "", filepath.Join(extra, "a.txt"), false)
	why := func() string { _, why := w.tryEdit("", "", filepath.Join(w.workspace(), "main.go")); return why }()
	w.expectEdit("", "", filepath.Join(w.workspace(), "main.go"), true)
	for _, want := range []string{"starship: ", w.bridge, "shell commands", "failure"} {
		if !strings.Contains(why, want) {
			t.Errorf("refusal lacks %q: %s", want, why)
		}
	}
}

func TestEditGuardPathTricks(t *testing.T) {
	w, _ := newGuardWorld(t)
	base := t.TempDir()
	a, ac := filepath.Join(base, "a", "b"), filepath.Join(base, "a", "bc")
	os.MkdirAll(a, 0o755)
	os.MkdirAll(ac, 0o755)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\nsession-edit:\n  - "+a+"\n")
	w.expectEdit("", "", filepath.Join(a, "f"), false)
	w.expectEdit("", "", filepath.Join(ac, "f"), true)
	w.expectEdit("", "", filepath.Join(a, "..", "bc", "f"), true)
	w.expectEdit("", "", filepath.Join(a, "new", "deeper", "f"), false)

	// A link inside the bridge that points out of it does not carry the permission along.
	link := filepath.Join(w.bridge, "out")
	if err := os.Symlink(w.workspace(), link); err != nil {
		t.Skip("symlinks unavailable")
	}
	w.expectEdit("", "", filepath.Join(link, "f"), true)
	// A symlinked allowed folder matches the real path.
	alias := filepath.Join(base, "alias")
	os.Symlink(a, alias)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\nsession-edit:\n  - "+alias+"\n")
	w.expectEdit("", "", filepath.Join(a, "f"), false)
	w.expectEdit("", "", filepath.Join(alias, "f"), false)
}

func TestEditGuardRunAgents(t *testing.T) {
	w, tmp := newGuardWorld(t)
	lead := w.startLead()
	scratch, ws := w.scratch(), w.workspace()
	tmpFile := filepath.Join(tmp, "x")

	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	w.expectEdit(coder, "coder", filepath.Join(ws, "new", "main.go"), false)
	w.expectEdit(coder, "coder", filepath.Join(scratch, "log"), false)
	w.expectEdit(coder, "coder", tmpFile, false)
	w.expectEdit(coder, "coder", filepath.Join(w.bridge, "projects/supernova/project.yaml"), true)
	w.expectEdit(lead, "orchestrator", filepath.Join(ws, "main.go"), true)
	w.expectEdit(lead, "orchestrator", filepath.Join(scratch, "log"), false)

	if r := w.stop(coder, "coder", report("compiled"), false); r != "" {
		t.Fatal(r)
	}
	w.expectEdit(coder, "coder", filepath.Join(ws, "main.go"), true)
	reviewer, why := w.spawn(lead, "orchestrator", "reviewer")
	if reviewer == "" {
		t.Fatal(why)
	}
	w.expectEdit(reviewer, "reviewer", filepath.Join(ws, "main.go"), true)
	w.expectEdit(reviewer, "reviewer", filepath.Join(w.bridge, "bridge.yaml"), true)
	w.expectEdit(reviewer, "reviewer", filepath.Join(scratch, "notes"), false)
	w.expectEdit(reviewer, "reviewer", tmpFile, false)
}

func TestEditGuardAgentWithoutRun(t *testing.T) {
	w, tmp := newGuardWorld(t)
	w.expectEdit("ghost", "explorer", filepath.Join(w.workspace(), "main.go"), true)
	w.expectEdit("ghost", "explorer", filepath.Join(w.bridge, "bridge.yaml"), true)
	w.expectEdit("ghost", "explorer", filepath.Join(tmp, "x"), false)
}

func TestEditGuardPlatformPaths(t *testing.T) {
	cases := []struct {
		goos, p, dir string
		want         bool
	}{
		{"windows", `C:\Work\Bridge\a.md`, `c:\work\bridge`, true},
		{"windows", `\\?\C:\Work\Bridge\a.md`, `C:\work\bridge`, true},
		{"windows", `C:/Work/Bridge/a.md`, `C:\Work\Bridge`, true},
		{"windows", `D:\Work\Bridge\a.md`, `C:\Work\Bridge`, false},
		{"windows", `C:\Work\BridgeX\a.md`, `C:\Work\Bridge`, false},
		{"windows", `C:\Work\Bridge\..\Other\a`, `C:\Work\Bridge`, false},
		{"windows", `\\server\share\x\a`, `\\server\share\x`, true},
		{"darwin", `/Users/Me/Bridge/a`, `/users/me/bridge`, true},
		{"darwin", `/Users/Me/BridgeX/a`, `/users/me/bridge`, false},
		{"linux", `/Users/Me/Bridge/a`, `/users/me/bridge`, false},
		{"linux", `/a/bc`, `/a/b`, false},
		{"linux", `/a/b`, `/a/b`, true},
		{"linux", `/a/b/..foo`, `/a/b`, true},
	}
	for _, c := range cases {
		p, dir := stripExtendedPrefix(c.p, c.goos), stripExtendedPrefix(c.dir, c.goos)
		if got := pathWithin(p, dir, c.goos); got != c.want {
			t.Errorf("%s: %s within %s = %v, want %v", c.goos, c.p, c.dir, got, c.want)
		}
	}
	if got := stripExtendedPrefix(`\\?\UNC\srv\share`, "windows"); got != `\\srv\share` {
		t.Errorf("UNC prefix: %s", got)
	}
	if got := stripExtendedPrefix(`\\?\C:\a`, "linux"); got != `\\?\C:\a` {
		t.Errorf("prefix stripped off Windows: %s", got)
	}
	if tmp := systemTempFolders("windows"); len(tmp) != 1 {
		t.Errorf("windows temp folders: %v", tmp)
	}
	if tmp := systemTempFolders("darwin"); len(tmp) != 2 || tmp[1] != "/tmp" {
		t.Errorf("darwin temp folders: %v", tmp)
	}
}

func TestEditGuardEmptyWorkspaceAllowsNothing(t *testing.T) {
	if got := runCodeFolders(&RunFiles{Snap: &Snapshot{}}); len(got) != 0 {
		t.Errorf("empty workspace gave code folders %v", got)
	}
}

func TestEditGuardFolderNotYetCreatedBehindSymlink(t *testing.T) {
	w, _ := newGuardWorld(t)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	os.MkdirAll(filepath.Join(real, "projects"), 0o755)
	if err := os.Symlink(real, filepath.Join(base, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	folder := filepath.Join(base, "link", "projects", "notes")
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\nsession-edit:\n  - "+folder+"\n")
	w.expectEdit("", "", filepath.Join(folder, "a.md"), false)
	w.expectEdit("", "", filepath.Join(real, "projects", "notes", "a.md"), false)
	w.expectEdit("", "", filepath.Join(real, "projects", "other", "a.md"), true)
}

func TestEditGuardSeveralLiveRuns(t *testing.T) {
	w, _ := newGuardWorld(t)
	client, server := filepath.Join(t.TempDir(), "client"), filepath.Join(t.TempDir(), "server")
	os.MkdirAll(client, 0o755)
	os.MkdirAll(server, 0o755)
	w.liveStepRun(client, "agentA")
	w.liveStepRun(server, "agentB")
	w.expectEdit("agentA", "coder", filepath.Join(client, "main.go"), false)
	w.expectEdit("agentA", "coder", filepath.Join(server, "main.go"), true)
	w.expectEdit("agentB", "coder", filepath.Join(server, "main.go"), false)
	w.expectEdit("agentB", "coder", filepath.Join(client, "main.go"), true)
	w.expectEdit("ghost", "coder", filepath.Join(client, "main.go"), true)
	w.expectEdit("ghost", "coder", filepath.Join(server, "main.go"), true)
}

func TestStripExtendedPrefixUNCCase(t *testing.T) {
	for _, p := range []string{`\\?\unc\srv\share`, `\\?\Unc\srv\share`, `\\?\UNC\srv\share`} {
		if got := stripExtendedPrefix(p, "windows"); got != `\\srv\share` {
			t.Errorf("%s stripped to %s", p, got)
		}
		if got := stripExtendedPrefix(p, "linux"); got != p {
			t.Errorf("%s changed off Windows: %s", p, got)
		}
	}
	if got := stripExtendedPrefix(`//?/C:/x`, "windows"); got != `//?/C:/x` {
		t.Errorf("forward-slash prefix stripped: %s", got)
	}
}

var deviceSpellings = []string{`\\?\Volume{1234}\x`, `\\.\C:\x`, `//?/C:/x`, `\\?\unc\localhost\c$\x`, `\\?\GLOBALROOT\x`, `C:rel\x`, `\rooted\x`}

func guardInput(tool, key, p string) *HookInput {
	return &HookInput{ToolName: tool, ToolInput: map[string]any{key: p}}
}

func TestWindowsSpellingsControlPlaneAndEditGuard(t *testing.T) {
	w, _ := newGuardWorld(t)
	for _, p := range deviceSpellings {
		in := guardInput("Edit", "file_path", p)
		if why := controlPlaneEdit(w.bridge, in, "windows"); !strings.Contains(why, "full") {
			t.Errorf("controlPlaneEdit windows %s: %q", p, why)
		}
		if why, _, err := editGuard(w.bridge, in, "windows"); err != nil || !strings.Contains(why, "full") || !strings.Contains(why, "Working around the guard is a failure, not a fix.") {
			t.Errorf("editGuard windows %s: %q %v", p, why, err)
		}
		if why := controlPlaneEdit(w.bridge, in, "linux"); strings.Contains(why, "full") {
			t.Errorf("controlPlaneEdit linux %s: %q", p, why)
		}
		if why, _, _ := editGuard(w.bridge, in, "linux"); strings.Contains(why, "full `C:") {
			t.Errorf("editGuard linux %s: %q", p, why)
		}
	}
}

func TestControlPlaneEditUNCByName(t *testing.T) {
	w, _ := newGuardWorld(t)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: /opt/supernova/ship\n")
	for _, p := range []string{
		`\\localhost\C$\work\bridge\.starship\control-plane\x`,
		`\\?\UNC\localhost\C$\work\bridge\.starship\control-plane\x`,
		`//localhost/C$/work/bridge/.starship/control-plane/x`,
		`\\LOCALHOST\C$\Work\Bridge\.STARSHIP\Control-Plane\X`,
		`\\localhost\C$\work\bridge\other\..\.starship\control-plane\x`,
		`\\localhost\C$\work\bridge\.starship.\control-plane \x`,
		`\\srv\share\supernova\ship\x`,
	} {
		if why := controlPlaneEdit(w.bridge, guardInput("Write", "file_path", p), "windows"); !strings.Contains(why, "pinned copy") {
			t.Errorf("windows %s allowed: %q", p, why)
		}
	}
	for _, p := range []string{`\\nas\share\notes.md`, `\\?\C:\supernova\notes.md`, `C:\supernova\notes.md`} {
		if why := controlPlaneEdit(w.bridge, guardInput("Write", "file_path", p), "windows"); why != "" {
			t.Errorf("windows %s refused: %q", p, why)
		}
	}
}

func TestWindowsLeadingSpaceSpellings(t *testing.T) {
	w, _ := newGuardWorld(t)
	for _, p := range []string{` \\?\Volume{1234}\x`, ` \\.\C:\x`} {
		in := guardInput("Write", "file_path", p)
		if why := controlPlaneEdit(w.bridge, in, "windows"); !strings.Contains(why, "full") {
			t.Errorf("controlPlaneEdit windows %q: %q", p, why)
		}
		if why, _, err := editGuard(w.bridge, in, "windows"); err != nil || !strings.Contains(why, "full") {
			t.Errorf("editGuard windows %q: %q %v", p, why, err)
		}
		if why := controlPlaneEdit(w.bridge, in, "linux"); strings.Contains(why, "full") {
			t.Errorf("controlPlaneEdit linux %q: %q", p, why)
		}
		if why, _, _ := editGuard(w.bridge, in, "linux"); strings.Contains(why, "full `C:") {
			t.Errorf("editGuard linux %q: %q", p, why)
		}
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: /opt/supernova/ship\n")
	unc := ` \\localhost\C$\work\bridge\.starship\control-plane\x`
	if why := controlPlaneEdit(w.bridge, guardInput("Write", "file_path", unc), "windows"); !strings.Contains(why, "pinned copy") {
		t.Errorf("controlPlaneEdit windows %q: %q", unc, why)
	}
	if why := controlPlaneEdit(w.bridge, guardInput("Write", "file_path", unc), "linux"); why != "" {
		t.Errorf("controlPlaneEdit linux %q: %q", unc, why)
	}
}

func TestEditFolderTarget(t *testing.T) {
	for _, c := range []struct{ in, goos, want string }{
		{` \\fs\share\x.txt`, "windows", `\\fs\share\x.txt`},
		{` //fs/share/x.txt`, "windows", `//fs/share/x.txt`},
		{` C:\x`, "windows", ` C:\x`},
		{`notes.md`, "windows", `notes.md`},
		{` notes.md`, "windows", ` notes.md`},
		{` \\fs\share\x.txt`, "linux", ` \\fs\share\x.txt`},
	} {
		if got := editFolderTarget(c.in, "/home/u", c.goos); got != c.want {
			t.Errorf("editFolderTarget(%q, %s) = %q, want %q", c.in, c.goos, got, c.want)
		}
	}
}
