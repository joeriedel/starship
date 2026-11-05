// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseLinksText(t *testing.T, doc string) ([]Link, error) {
	t.Helper()
	var lf linksFile
	if err := yaml.Unmarshal([]byte(doc), &lf); err != nil {
		t.Fatal(err)
	}
	return parseLinks(&lf.Links, "task.md")
}

func TestParseLinks(t *testing.T) {
	got, err := parseLinksText(t, "links:\n  - kind: ticket\n    ref: SUPER-42\n  - kind: wiki\n    ref: https://wiki.example/supernova/launch-plan\n  - {kind: mail, ref: msg-1}\n  - {kind: slack, ref: \"#supernova/p123\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []Link{{"ticket", "SUPER-42"}, {"wiki", "https://wiki.example/supernova/launch-plan"}, {"mail", "msg-1"}, {"slack", "#supernova/p123"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d: got %v, want %v", i, got[i], want[i])
		}
	}
	if got, err := parseLinksText(t, "pipeline: coding\n"); err != nil || got != nil {
		t.Errorf("no links: %v, %v", got, err)
	}
}

func TestParseLinksRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown kind":  "links:\n  - {kind: fax, ref: x}\n",
		"missing kind":  "links:\n  - {ref: x}\n",
		"empty ref":     "links:\n  - {kind: wiki, ref: \"\"}\n",
		"blank ref":     "links:\n  - {kind: wiki, ref: \"  \"}\n",
		"missing ref":   "links:\n  - {kind: wiki}\n",
		"newline":       "links:\n  - {kind: wiki, ref: \"a\\nb\"}\n",
		"control":       "links:\n  - {kind: wiki, ref: \"a\\u0007b\"}\n",
		"extra key":     "links:\n  - {kind: wiki, ref: x, title: y}\n",
		"not a list":    "links: wiki\n",
		"entry scalar":  "links:\n  - wiki\n",
		"ref a list":    "links:\n  - {kind: wiki, ref: [a]}\n",
		"duplicate key": "links:\n  - {kind: wiki, kind: mail, ref: x}\n",
	} {
		if _, err := parseLinksText(t, doc); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func linksOpen(t *testing.T, w *world, taskFront string) string {
	t.Helper()
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	write(t, filepath.Join(taskDir, "task.md"), "---\n"+taskFront+"---\n\n# auth-refactor\n\n## Goal\n\nBody text.\n")
	var out bytes.Buffer
	if err := openTask(w.bridge, "supernova/auth-refactor", taskDir, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func declareReader(t *testing.T, w *world) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+root+"\nconnectors:\n  reader:\n    - mcp__supernova_mail__read_message\n")
}

const twoLinks = "links:\n  - {kind: wiki, ref: \"https://wiki.example/supernova/launch-plan\"}\n  - {kind: ticket, ref: SUPER-42}\n"

func TestOpenTaskListsLinks(t *testing.T) {
	w := newWorld(t)
	out := linksOpen(t, w, "pipeline: coding\n"+twoLinks)
	if !strings.Contains(out, "links      wiki https://wiki.example/supernova/launch-plan\n           ticket SUPER-42\n") {
		t.Errorf("links not listed:\n%s", out)
	}
	if strings.Contains(out, "reader") {
		t.Errorf("no reader declared, so no mention of it:\n%s", out)
	}
	declareReader(t, w)
	out = linksOpen(t, w, "pipeline: coding\n"+twoLinks)
	if !strings.Contains(out, "ticket SUPER-42\nreader     before starting work, dispatch the reader agent with these links") || !strings.Contains(out, "Body text.") {
		t.Errorf("reader line missing:\n%s", out)
	}
}

func TestOpenTaskLinksNoCodeTask(t *testing.T) {
	w := newWorld(t)
	out := linksOpen(t, w, "pipeline: none\n"+twoLinks)
	if !strings.Contains(out, "pipeline   none") || !strings.Contains(out, "links      wiki ") || strings.Contains(out, "dispatch") {
		t.Errorf("no-code task without reader:\n%s", out)
	}
	declareReader(t, w)
	out = linksOpen(t, w, "pipeline: none\n"+twoLinks)
	if !strings.Contains(out, "pipeline   none") || !strings.Contains(out, "dispatch the reader agent") {
		t.Errorf("no-code task with reader:\n%s", out)
	}
}

func TestOpenTaskLinksWithWorkspaceStop(t *testing.T) {
	w := newWorld(t)
	ws, err := workspaceFor(w.bridge, "supernova", "auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(ws)
	out := linksOpen(t, w, "pipeline: coding\n"+twoLinks)
	if !strings.Contains(out, "workspace  STOP:") || !strings.Contains(out, "ticket SUPER-42") {
		t.Errorf("links must list beside a workspace STOP:\n%s", out)
	}
}

func TestOpenTaskNoLinksPrintsNothing(t *testing.T) {
	w := newWorld(t)
	declareReader(t, w)
	out := linksOpen(t, w, "pipeline: coding\n")
	if strings.Contains(out, "links") || strings.Contains(out, "dispatch") {
		t.Errorf("no links, nothing printed:\n%s", out)
	}
}

func TestOpenTaskLinksMergeAndDedupe(t *testing.T) {
	w := newWorld(t)
	pj := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(pj)
	write(t, pj, string(data)+"\nlinks:\n  - {kind: ticket, ref: SUPER-42}\n  - {kind: mail, ref: msg-1}\n")
	out := linksOpen(t, w, "pipeline: coding\r\nlinks:\r\n  - {kind: ticket, ref: SUPER-42}\r\n  - {kind: slack, ref: s-1}\r\n")
	if strings.Count(out, "SUPER-42") != 1 {
		t.Errorf("duplicate shown more than once:\n%s", out)
	}
	i, j, k := strings.Index(out, "ticket SUPER-42"), strings.Index(out, "mail msg-1"), strings.Index(out, "slack s-1")
	if i < 0 || j < i || k < j {
		t.Errorf("want project links, then the task's:\n%s", out)
	}
}

func TestOpenTaskInvalidLinks(t *testing.T) {
	w := newWorld(t)
	declareReader(t, w)
	out := linksOpen(t, w, "pipeline: coding\nlinks:\n  - {kind: ticket, ref: A-1}\n  - {kind: fax, ref: x}\n")
	if !strings.Contains(out, "links      STOP: projects/supernova/tasks/auth-refactor/task.md:5") || !strings.Contains(out, "Body text.") || strings.Contains(out, "dispatch") {
		t.Errorf("invalid task links:\n%s", out)
	}
	pj := filepath.Join(w.bridge, "projects/supernova/project.yaml")
	data, _ := os.ReadFile(pj)
	write(t, pj, string(data)+"\nlinks:\n  - {kind: wiki, ref: \"\"}\n")
	out = linksOpen(t, w, "pipeline: coding\nlinks:\n  - {kind: mail, ref: msg-1}\n")
	if !strings.Contains(out, "STOP: projects/supernova/project.yaml:") || !strings.Contains(out, "mail msg-1") {
		t.Errorf("invalid project links:\n%s", out)
	}
}

func TestOpenTaskLinksReaderCheckFails(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "connectors:\n  coder: [x]\n")
	out := linksOpen(t, w, "pipeline: coding\n"+twoLinks)
	if !strings.Contains(out, "ticket SUPER-42") || !strings.Contains(out, "links      STOP:") || strings.Contains(out, "dispatch") || !strings.Contains(out, "Body text.") {
		t.Errorf("reader check failure:\n%s", out)
	}
}

func TestMalformedLinksDoNotBreakTaskFile(t *testing.T) {
	front := []byte("pipeline: coding\nlinks: 7\n")
	var tf TaskFile
	if err := yamlUnmarshal(front, &tf); err != nil {
		t.Fatalf("TaskFile must ignore links: %v", err)
	}
	if tf.Pipeline != "coding" {
		t.Errorf("TaskFile read %+v", tf)
	}
	var bf BridgeFile
	if err := yamlUnmarshal([]byte("links: 7\n"), &bf); err != nil {
		t.Errorf("BridgeFile must ignore links: %v", err)
	}
}
