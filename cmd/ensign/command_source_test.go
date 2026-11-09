// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commandSourceYAML declares a command source that re-runs this test binary as the fake program in the given mode.
func commandSourceYAML(t *testing.T, name, mode, extra string, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := "  " + name + ":\n    kind: command\n    command: ['" + exe + "'"
	for _, a := range args {
		s += ", '" + a + "'"
	}
	return s + "]\n    env:\n      STARSHIP_TEST_FAKE_CMD: " + mode + "\n" + extra
}

func TestDeclaredCommandSourceParse(t *testing.T) {
	w := sourcesWorld(t, "sources:\n  supernova-review:\n    kind: command\n    command: [gh, pr, list]\n    timeout: 600\n    env:\n      GH_HOST: git.supernova.example\n")
	got, err := loadDeclaredSources(w.bridge)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v, %v", got, err)
	}
	s := got[0]
	if strings.Join(s.Command, " ") != "gh pr list" || s.Timeout != 600 || s.Env["GH_HOST"] != "git.supernova.example" || len(s.Watch) != 0 {
		t.Errorf("%+v", s)
	}
	w = sourcesWorld(t, "sources:\n  supernova-review:\n    kind: command\n    command: [gh]\n")
	if got, err := loadDeclaredSources(w.bridge); err != nil || got[0].Timeout != 60 {
		t.Errorf("default timeout: %+v, %v", got, err)
	}
}

func TestDeclaredCommandSourceInvalid(t *testing.T) {
	const head = "sources:\n  supernova-x:\n"
	for name, c := range map[string]struct{ yml, want string }{
		"watch on command":     {head + "    kind: command\n    command: [gh]\n    watch: [a]\n", "has no watch list"},
		"no command":           {head + "    kind: command\n", "command must be a list"},
		"command on mail":      {head + "    kind: mail\n    watch: [inbox]\n    command: [gh]\n", "command is only for command sources"},
		"timeout on mail":      {head + "    kind: mail\n    watch: [inbox]\n    timeout: 5\n", "timeout is only for command sources"},
		"env on mail":          {head + "    kind: mail\n    watch: [inbox]\n    env: {A: b}\n", "env is only for command sources"},
		"empty command":        {head + "    kind: command\n    command: []\n", "command must be a list"},
		"empty program":        {head + "    kind: command\n    command: [\"\"]\n", "must not be empty"},
		"command a string":     {head + "    kind: command\n    command: gh pr list\n", "command must be a list"},
		"nested command":       {head + "    kind: command\n    command: [[gh]]\n", "each command entry must be a string"},
		"control char":         {head + "    kind: command\n    command: [\"a\\nb\"]\n", "control character"},
		"timeout 0":            {head + "    kind: command\n    command: [gh]\n    timeout: 0\n", "timeout must be"},
		"timeout 601":          {head + "    kind: command\n    command: [gh]\n    timeout: 601\n", "timeout must be"},
		"timeout word":         {head + "    kind: command\n    command: [gh]\n    timeout: soon\n", "timeout must be"},
		"timeout fraction":     {head + "    kind: command\n    command: [gh]\n    timeout: 1.5\n", "timeout must be"},
		"env bad name":         {head + "    kind: command\n    command: [gh]\n    env:\n      \"A-B\": x\n", "env name \"A-B\""},
		"env digit name":       {head + "    kind: command\n    command: [gh]\n    env:\n      \"1A\": x\n", "env name \"1A\""},
		"env not a mapping":    {head + "    kind: command\n    command: [gh]\n    env: [A]\n", "env must be a mapping"},
		"env value not string": {head + "    kind: command\n    command: [gh]\n    env:\n      A: [x]\n", "env A must be a string"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadDeclaredSources(sourcesWorld(t, c.yml).bridge)
			if err == nil || !strings.HasPrefix(err.Error(), "bridge.yaml: sources: supernova-x: ") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error naming the source and containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestCommandSourceWritesAndRotates(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "print", "", "pr", "list"))
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(w.bridge, "reader", "command", "supernova-review.out")
	data, err := os.ReadFile(outFile)
	if err != nil || string(data) != "pr list\n"+filepath.Base(w.bridge)+"\n" {
		t.Fatalf("out file %q, %v", data, err)
	}
	for _, want := range []string{"- supernova-review (command)", "reader/command/supernova-review.out", "reader/command/supernova-review.prev", "no cursor"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	write(t, outFile, "first\n")
	if _, err := w.catchUp(); err != nil {
		t.Fatal(err)
	}
	prev, _ := os.ReadFile(filepath.Join(w.bridge, "reader", "command", "supernova-review.prev"))
	if string(prev) != "first\n" {
		t.Errorf("prev = %q", prev)
	}
	ents, _ := os.ReadDir(filepath.Dir(outFile))
	if len(ents) != 2 {
		t.Errorf("want only .out and .prev, got %v", ents)
	}
}

func TestCommandSourceFailureKeepsFiles(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "fail", "")+"  work-mail:\n    kind: mail\n    watch: [inbox]\n")
	dir := filepath.Join(w.bridge, "reader", "command")
	write(t, filepath.Join(dir, "supernova-review.out"), "old\n")
	write(t, filepath.Join(dir, "supernova-review.prev"), "older\n")
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"unreadable this pass: exit status 3", "fake error 8", "fake error 4", "- work-mail (mail)", "  - inbox"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fake error 3") {
		t.Errorf("only the last lines of stderr belong in the brief:\n%s", out)
	}
	for f, want := range map[string]string{"supernova-review.out": "old\n", "supernova-review.prev": "older\n"} {
		if data, _ := os.ReadFile(filepath.Join(dir, f)); string(data) != want {
			t.Errorf("%s = %q, want it untouched", f, data)
		}
	}
}

func TestCommandSourceTimeout(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "sleep", "    timeout: 1\n"))
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timed out after 1s") {
		t.Errorf("brief:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, "reader", "command", "supernova-review.out")); err == nil {
		t.Errorf("a timed-out run must write no output")
	}
}

func TestCommandSourceMissingProgram(t *testing.T) {
	w := sourcesWorld(t, "sources:\n  supernova-review:\n    kind: command\n    command: [supernova-no-such-program]\n")
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unreadable this pass: could not run:") {
		t.Errorf("brief:\n%s", out)
	}
}

func TestCommandSourceOutputCap(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "big", ""))
	if _, err := w.catchUp(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "reader", "command", "supernova-review.out"))
	const marker = "[ensign: output cut at 1 MB]\n"
	if !strings.HasSuffix(string(data), marker) || len(data) != commandOutputCap+len(marker) {
		t.Errorf("want %d bytes ending in the marker, got %d", commandOutputCap+len(marker), len(data))
	}
}

func TestCommandSourceEnv(t *testing.T) {
	t.Setenv("SUPERNOVA_USER", "from-user")
	t.Setenv("SUPERNOVA_HOST", "from-user")
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "env", "      STARSHIP_TEST_FAKE_VAR: SUPERNOVA_HOST\n      SUPERNOVA_HOST: git.supernova.example\n"))
	if _, err := w.catchUp(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "reader", "command", "supernova-review.out"))
	if string(data) != "git.supernova.example\n" {
		t.Errorf("env: %q", data)
	}
	got := commandEnv([]string{"A=1", "B=2"}, map[string]string{"B": "3", "C": "4"})
	if strings.Join(got, " ") != "A=1 B=3 C=4" {
		t.Errorf("commandEnv = %v", got)
	}
}

func TestCatchUpAlsoOnCommandSource(t *testing.T) {
	w := sourcesWorld(t, "sources:\n"+commandSourceYAML(t, "supernova-review", "print", ""))
	if _, err := w.catchUp("--also", "supernova-review=x"); err == nil || !strings.Contains(err.Error(), "command source") {
		t.Errorf("--also on a command source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, "reader", "command")); err == nil {
		t.Errorf("a refused catch-up must not run the command")
	}
}

func TestJournalChangesKeepsCommandOutput(t *testing.T) {
	w := sourcesWorld(t, sourcesYAML)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base"}} {
		if err := git(w.bridge, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"command/x.out", "command/x.prev", "command/.x-1.tmp", "command/x.txt", "other.out", "data.json", ".hidden.out"} {
		write(t, filepath.Join(w.bridge, "reader", f), "data\n")
	}
	got, err := journalChanges(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	want := "reader/command/x.out reader/command/x.prev"
	if joined := strings.Join(got, " "); joined != want {
		t.Errorf("journalChanges = %q, want %q", joined, want)
	}
}
