// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const connectorKeysMessage = ": is a bridge setting; declare it in bridge.yaml"

func TestConnectorKeysRefusedOutsideBridgeYaml(t *testing.T) {
	for _, key := range []string{"connectors", "connector_writes"} {
		for name, value := range map[string]string{"null": "", "empty list": " []", "empty map": " {}"} {
			want := key + connectorKeysMessage
			t.Run(key+" "+name, func(t *testing.T) {
				w := newWorld(t)
				line := key + ":" + value + "\n"
				pf := filepath.Join(w.bridge, "projects", "supernova", "project.yaml")
				orig, _ := os.ReadFile(pf)
				write(t, pf, string(orig)+"\n"+line)
				if _, err := loadConfig(w.bridge, "supernova/auth-refactor"); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), pf) {
					t.Errorf("project.yaml: got %v", err)
				}
				write(t, pf, string(orig))

				tf := filepath.Join(w.bridge, "projects", "supernova", "tasks", "auth-refactor", "task.md")
				task, _ := os.ReadFile(tf)
				write(t, tf, strings.Replace(string(task), "pipeline: coding\n", "pipeline: coding\n"+line, 1))
				if _, err := loadConfig(w.bridge, "supernova/auth-refactor"); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tf) {
					t.Errorf("task.md: got %v", err)
				}
				write(t, tf, string(task))

				ext := filepath.Join(w.bridge, "extensions", "house", "extension.yaml")
				write(t, ext, "name: house\n"+line)
				useExtension(t, w, "house")
				for path, err := range loadAllPaths(w) {
					if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), ext) {
						t.Errorf("extension.yaml, %s: got %v", path, err)
					}
				}
			})
		}
	}
}

func TestBridgeYamlRefusesUnknownKeys(t *testing.T) {
	w := newWorld(t)
	file := filepath.Join(w.bridge, "bridge.yaml")
	for _, key := range []string{"conectors", "links", "name"} {
		write(t, file, "control-plane:\n  path: "+repoRoot(t)+"\n"+key+": x\n")
		_, err := loadBridgeFile(w.bridge)
		if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: want an error naming the file and the key, got %v", key, err)
		}
	}
}

func TestBridgeYamlLoadsEveryLegitimateKey(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), `control-plane:
  path: `+repoRoot(t)+`
workspace: legacy
use: [house]
rules: ["one"]
bindings:
  test: { command: "./test.sh" }
skills: {}
pipelines: {}
events: {}
journal:
  commit: "on"
documentation: "on"
roles:
  coder: { model: haiku }
session-edit: [notes]
keep-runs: 5
connectors:
  reader:
    - `+supernovaRead+`
connector_writes: [`+supernovaSend+`]
sources:
  mail:
    kind: mail
documents:
  plan:
    path: notes/plan.md
`)
	if _, err := loadBridgeFile(w.bridge); err != nil {
		t.Fatalf("a bridge.yaml with every legitimate key must load: %v", err)
	}
	for name, body := range map[string]string{"empty": "", "comments": "# nothing yet\n"} {
		write(t, filepath.Join(w.bridge, "bridge.yaml"), body)
		if _, err := loadBridgeFile(w.bridge); err != nil {
			t.Errorf("%s bridge.yaml must load: %v", name, err)
		}
	}
}

func TestSessionStartNotReadyOnMisspelledKey(t *testing.T) {
	w := connWorld(t, "conectors:\n  reader:\n    - "+supernovaRead+"\n")
	t.Setenv("CLAUDE_PID", "4242")
	res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
	if code != 0 {
		t.Fatalf("SessionStart exited %d", code)
	}
	ctx := dig(res, "hookSpecificOutput", "additionalContext")
	if !strings.Contains(dig(res, "systemMessage"), "not ready") || strings.Contains(ctx, "STARSHIP-READY") || !strings.Contains(ctx, "conectors") {
		t.Fatalf("a misspelled key must be not ready and name the key: %q", ctx)
	}
	if !exists(filepath.Join(w.bridge, ".starship", "sessions", "4242.connectors")) {
		t.Error("the baseline is written even when the bridge is not ready")
	}
}

// restartNote starts a session under pid and reports whether it carries the restart note.
func (w *world) restartNote(pid, source string) bool {
	w.t.Helper()
	w.t.Setenv("CLAUDE_PID", pid)
	res, code := w.hook("SessionStart", map[string]any{"source": source})
	if code != 0 {
		w.t.Fatalf("SessionStart exited %d", code)
	}
	show, ctx := dig(res, "systemMessage"), dig(res, "hookSpecificOutput", "additionalContext")
	if strings.Contains(show, "not ready") {
		w.t.Fatalf("not ready: %q", ctx)
	}
	if strings.Contains(show, "restart claude") != strings.Contains(ctx, "restarted") {
		w.t.Fatalf("the two channels disagree: %q %q", show, ctx)
	}
	return strings.Contains(show, "restart claude")
}

func TestRestartNoteLastsUntilProcessRestarts(t *testing.T) {
	w := connWorld(t, "")
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.bridge, ".claude", "settings.json"), hookSettings())
	for _, pid := range []string{"111", "222"} {
		if w.restartNote(pid, "startup") {
			t.Fatalf("pid %s: nothing declared, so no note", pid)
		}
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\n"+readerRules)
	if !w.restartNote("111", "clear") {
		t.Fatal("the process that saw the rewrite needs the note")
	}
	for _, c := range []struct{ pid, source string }{{"111", "clear"}, {"111", "resume"}, {"111", "compact"}, {"222", "resume"}} {
		if !w.restartNote(c.pid, c.source) {
			t.Errorf("pid %s %s: the note repeats until the process restarts", c.pid, c.source)
		}
	}
	if w.restartNote("333", "startup") {
		t.Error("a process started after the change needs no note")
	}
	if w.restartNote("333", "clear") {
		t.Error("a process started after the change needs no note on /clear either")
	}
	if w.restartNote("111", "startup") {
		t.Error("startup on a reused pid replaces the stale baseline")
	}
	if w.restartNote("111", "clear") {
		t.Error("the reset baseline is the new one")
	}
	if !w.restartNote("222", "clear") {
		t.Error("pid 222 still runs with what it started with")
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\n")
	if w.restartNote("222", "clear") {
		t.Error("a change reverted to what the process started with stops the note")
	}
}

func TestRestartNoteWithoutPidIsOneShot(t *testing.T) {
	w := connWorld(t, readerRules)
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.bridge, ".claude", "settings.json"), hookSettings())
	if !w.restartNote("", "startup") {
		t.Fatal("the rewrite needs the note")
	}
	if w.restartNote("", "clear") {
		t.Error("with no CLAUDE_PID the note shows once")
	}
	if m, _ := filepath.Glob(filepath.Join(w.bridge, ".starship", "sessions", "*.connectors")); len(m) != 0 {
		t.Errorf("no baseline without a pid: %v", m)
	}
}

func TestSweepRemovesStaleBaselines(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	write(t, filepath.Join(dir, "7.connectors"), "{}\n")
	write(t, filepath.Join(dir, "8.connectors"), "{}\n")
	past := time.Now().Add(-pidRecordAge - time.Hour)
	os.Chtimes(filepath.Join(dir, "7.connectors"), past, past)
	dropDeadLinks(w.bridge)
	if exists(filepath.Join(dir, "7.connectors")) || !exists(filepath.Join(dir, "8.connectors")) {
		t.Error("a baseline goes by the same age rule as a .pid record")
	}
}

func TestReplacingReaderNeedsNoShell(t *testing.T) {
	for name, c := range map[string]struct {
		front   string
		refused bool
		rules   string // bridge.yaml connectors; readerRules when empty
		file    string // role file name; reader when empty
		fmName  string // frontmatter name; reader when empty
	}{
		"no tools":               {front: "", refused: true},
		"empty":                  {front: "tools:\n", refused: true},
		"empty string":           {front: "tools: \"\"\n", refused: true},
		"empty list":             {front: "tools: []\n", refused: true},
		"unreadable":             {front: "tools: {a: b}\n", refused: true},
		"Bash":                   {front: "tools: Read, Bash\n", refused: true},
		"Bash scoped":            {front: "tools: Read, Bash(git:*)\n", refused: true},
		"PowerShell":             {front: "tools: PowerShell, Read\n", refused: true},
		"PowerShell()":           {front: "tools: Read, PowerShell(Get-Date)\n", refused: true},
		"star":                   {front: "tools: \"*\"\n", refused: true},
		"list Bash":              {front: "tools:\n  - Read\n  - Bash\n", refused: true},
		"list scoped":            {front: "tools: [Read, \"Bash(git:*)\"]\n", refused: true},
		"list star":              {front: "tools:\n  - \"*\"\n", refused: true},
		"clean":                  {front: "tools: Read, Grep, Write\n", refused: false},
		"clean list":             {front: "tools:\n  - Read\n  - Grep\n", refused: false, rules: "connectors:\n  reader: []\n"},
		"Read Bash":              {front: "tools: Read Bash\n", refused: true},
		"Read Bash(git:*)":       {front: "tools: Read Bash(git:*)\n", refused: true},
		"Read *":                 {front: "tools: Read *\n", refused: true},
		"Agent":                  {front: "tools: Agent\n", refused: true},
		"task":                   {front: "tools: task\n", refused: true},
		"Agent scoped":           {front: "tools: Agent(explorer)\n", refused: true},
		"Read Agent":             {front: "tools: Read, Agent\n", refused: true},
		"shell named reader":     {front: "tools: Bash, Read\n", refused: true, file: "shell", fmName: "reader"},
		"reader named librarian": {front: "tools: Bash, Read\n", refused: false, file: "reader", fmName: "librarian"},
	} {
		rules, file, fmName := c.rules, c.file, c.fmName
		if rules == "" {
			rules = readerRules
		}
		if file == "" {
			file = "reader"
		}
		if fmName == "" {
			fmName = "reader"
		}
		w := connWorld(t, rules)
		role := filepath.Join(w.bridge, "roles", file+".md")
		write(t, role, scoutRole(fmName, c.front+"replaces: reader\n", "\n"))
		bf, err := loadBridgeFile(w.bridge)
		if err != nil {
			t.Fatal(err)
		}
		_, err = generatedFiles(w.bridge, bf, repoRoot(t))
		if (err != nil) != c.refused {
			t.Errorf("%s: err=%v, want refused=%v", name, err, c.refused)
		}
		if err != nil && (!strings.Contains(err.Error(), role) || !strings.Contains(err.Error(), "no shell")) {
			t.Errorf("%s: the error should name the role file and why: %v", name, err)
		}
		// Without a declared reader there is no write limit to protect.
		w = connWorld(t, "")
		write(t, filepath.Join(w.bridge, "roles", file+".md"), scoutRole(fmName, c.front+"replaces: reader\n", "\n"))
		w.generated()
	}
}

func TestHasKey(t *testing.T) {
	for name, c := range map[string]struct {
		doc  string
		want bool
	}{
		"plain":            {"connectors:\n  a: b\n", true},
		"null value":       {"connectors:\n", true},
		"empty value":      {"connectors: []\n", true},
		"other key":        {"name: x\n", false},
		"nested only":      {"a:\n  connectors: x\n", false},
		"unparsable":       {"connectors: [\n", false},
		"empty document":   {"", false},
		"comments only":    {"# nothing\n", false},
		"merged alias":     {"base: &b {connectors: x}\n<<: *b\n", true},
		"merged mapping":   {"<<: {connectors: x}\n", true},
		"merged sequence":  {"base: &b {connectors: x}\n<<: [{a: b}, *b]\n", true},
		"merge misses":     {"<<: {other: x}\n", false},
		"unhashable key":   {"? {a: b}\n: c\nconnectors: x\n", true},
		"unhashable merge": {"bindings:\n  build:\n    1: y\n    <<:\n      ? {a: b}\n      : c\n", false},
		"unhashable top":   {"<<:\n  ? {a: b}\n  : c\n", false},
		"alias cycle":      {"a: &a [*a]\n<<: *a\n", false},
		"sequence root":    {"- connectors: x\n", false},
		"nested merge seq": {"<<: [[{connectors: x}]]\n", false},
		"merge seq alias":  {"s: &s [{connectors: x}]\n<<: *s\n", false},
		"nested bad merge": {"<<: [{<<: 5}, {connectors: x}]\n", false},
		"key before bad":   {"connectors: x\n<<: 5\n", true},
		"alias merge key":  {"x: &m <<\n*m : {connectors: y}\n", false},
		"merge alias seq":  {"s: &s [[{connectors: x}]]\n<<: *s\n", false},
		"alias key":        {"x-k: &k connectors\n*k :\n  a: b\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hasKey([]byte(c.doc), "connectors"); got != c.want {
				t.Errorf("hasKey(%q) = %v, want %v", c.doc, got, c.want)
			}
		})
	}
}

func TestReplacingByAgentName(t *testing.T) {
	for name, c := range map[string]struct {
		file, fmName, replaces string
		rules                  string // bridge.yaml connectors; readerRules when empty
		refused                bool
		gone                   string // a stem that must not be generated
		want                   string // the one stem generated under the agent name
	}{
		"name clash needs replaces":     {file: "shell", fmName: "reader", refused: true},
		"name clash replaces":           {file: "shell", fmName: "reader", replaces: "reader", want: "shell", gone: "reader"},
		"name clash replaces coder":     {file: "shell", fmName: "coder", rules: "use: []\n", replaces: "coder", want: "shell", gone: "coder"},
		"name clash coder needs":        {file: "shell", fmName: "coder", rules: "use: []\n", refused: true},
		"stem clash only":               {file: "reader", fmName: "librarian", replaces: "reader", want: "reader"},
		"stem and other name clash":     {file: "coder", fmName: "reviewer", replaces: "coder", rules: "use: []\n", refused: true},
		"undeclared reader, no clash":   {file: "shell", fmName: "reader", rules: "use: []\n", want: "shell"},
		"wrong replaces for name clash": {file: "shell", fmName: "reader", replaces: "shell", refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			rules := c.rules
			if rules == "" {
				rules = readerRules
			}
			w := connWorld(t, rules)
			front := "tools: Read, Grep\n"
			if c.replaces != "" {
				front += "replaces: " + c.replaces + "\n"
			}
			write(t, filepath.Join(w.bridge, "roles", c.file+".md"), scoutRole(c.fmName, front, "\n"))
			bf, err := loadBridgeFile(w.bridge)
			if err != nil {
				t.Fatal(err)
			}
			files, err := generatedFiles(w.bridge, bf, repoRoot(t))
			if (err != nil) != c.refused {
				t.Fatalf("err=%v, want refused=%v", err, c.refused)
			}
			if c.refused {
				return
			}
			named := 0
			for path, data := range files {
				if !strings.HasPrefix(path, ".claude/agents/") {
					continue
				}
				if strings.Contains(string(data), "name: "+c.fmName+"\n") {
					named++
					if path != ".claude/agents/"+c.want+".md" {
						t.Errorf("agent %q generated as %s, want stem %s", c.fmName, path, c.want)
					}
				}
			}
			if named != 1 {
				t.Errorf("%d files named %q, want exactly one", named, c.fmName)
			}
			if _, ok := files[".claude/agents/"+c.gone+".md"]; c.gone != "" && ok && c.gone != c.want {
				t.Errorf("the replaced role %s should not be generated", c.gone)
			}
		})
	}
}

func TestReplacingOneRoleByStemAndName(t *testing.T) {
	for name, c := range map[string]struct {
		replaces string
		refused  bool
	}{
		"no replaces":      {"", true},
		"replaces stem":    {"scout", false},
		"replaces name":    {"supernova-scout", false},
		"replaces neither": {"other", true},
	} {
		t.Run(name, func(t *testing.T) {
			w := connWorld(t, readerRules)
			write(t, filepath.Join(w.bridge, "roles", "scout.md"), scoutRole("supernova-scout", "tools: Read, Grep\n", "\n"))
			write(t, filepath.Join(w.bridge, "extensions", "house", "extension.yaml"), "name: house\n")
			front := "tools: Read, Grep\n"
			if c.replaces != "" {
				front += "replaces: " + c.replaces + "\n"
			}
			write(t, filepath.Join(w.bridge, "extensions", "house", "roles", "scout.md"), scoutRole("supernova-scout", front, "\n"))
			useExtension(t, w, "house")
			bf, err := loadBridgeFile(w.bridge)
			if err != nil {
				t.Fatal(err)
			}
			_, err = generatedFiles(w.bridge, bf, repoRoot(t))
			if (err != nil) != c.refused {
				t.Fatalf("err=%v, want refused=%v", err, c.refused)
			}
		})
	}
}
