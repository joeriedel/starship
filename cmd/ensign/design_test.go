// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (w *world) designFile(name, content string) {
	w.t.Helper()
	write(w.t, filepath.Join(w.bridge, "projects/supernova/design", filepath.FromSlash(name)), content)
}

func (w *world) openOut(task string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	err := openTask(w.bridge, "supernova/"+task, taskDirOf(w.bridge, "supernova/"+task), &out)
	return out.String(), err
}

func TestDesignFilesAndSummaries(t *testing.T) {
	w := newWorld(t)
	w.designFile("b.md", "\n\n## Tokens are opaque\nsecond line\n")
	w.designFile("a.md", "# Auth\r\nmore\r\n")
	w.designFile("api/tokens.md", "Plain first line  \n")
	w.designFile("bom.md", "\xEF\xBB\xBF# Bom\r\n")
	w.designFile("empty.md", "")
	w.designFile("notes.txt", "ignored\n")
	files := designFiles(w.bridge, "supernova")
	var got []string
	for _, f := range files {
		got = append(got, f.Name+"|"+f.Summary)
	}
	want := "a.md|Auth,api/tokens.md|Plain first line,b.md|Tokens are opaque,bom.md|Bom,empty.md|"
	if strings.Join(got, ",") != want {
		t.Errorf("got %q want %q", strings.Join(got, ","), want)
	}
	if !filepath.IsAbs(files[0].Path) {
		t.Errorf("path %q is not absolute", files[0].Path)
	}
}

func TestProjectNewCreatesDesignFolder(t *testing.T) {
	w := noCodeWorld(t, "")
	w.enter()
	ws := t.TempDir()
	var out bytes.Buffer
	for name, args := range map[string][]string{
		"locations": {"new", "p1", "--locations", "a,b", "--platforms", "linux"},
		"no code":   {"new", "p2", "--pipeline", "none"},
		"workspace": {"new", "p3", "--workspace", ws, "--platforms", "linux"},
	} {
		if err := cmdProject(args, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fi, err := os.Stat(filepath.Join(w.bridge, "projects", args[1], "design"))
		if err != nil || !fi.IsDir() {
			t.Errorf("%s: no design folder: %v", name, err)
		}
	}
}

func TestTaskOpenListsDesign(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a", "02-b")
	out, err := w.openOut("t")
	if err != nil || strings.Contains(out, "design") {
		t.Fatalf("no folder, no design block: %v\n%s", err, out)
	}
	os.MkdirAll(filepath.Join(w.bridge, "projects/supernova/design"), 0o755)
	if out, _ = w.openOut("t"); strings.Contains(out, "design     ") {
		t.Errorf("empty folder, no design block:\n%s", out)
	}
	w.designFile("auth.md", "# Auth\nSECRET second line\n")
	w.designFile("api.md", "# API\n")
	w.designFile("zed.md", "# Zed\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\npipeline: coding\ndesign: [auth.md]\n---\n\n## Goal\n\nDo t.\n")
	w.subtaskFile("t", "01-a", "---\ndesign: [api.md]\n---\n## Goal\n\nG.\n")
	out, err = w.openOut("t")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"design     projects/supernova/design/", "  * api.md - API\n", "    zed.md - Zed\n", "  * auth.md - Auth\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("design contents printed:\n%s", out)
	}
	if strings.Index(out, "design     ") > strings.Index(out, "## Goal") {
		t.Errorf("design block should come before the task body:\n%s", out)
	}
}

func TestRunWorkItemDesignSection(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "01-a")
	snap, err := w.snapOf("t", runOpts{})
	if err != nil || strings.Contains(snap.WorkItem, "## Design") {
		t.Fatalf("no folder, no section: %v\n%s", err, snap.WorkItem)
	}
	w.designFile("auth.md", "# Auth\nSECRET\n")
	w.designFile("api/tokens.md", "# Tokens\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\npipeline: coding\ndesign: [auth.md]\n---\n\n## Goal\n\nDo t.\n")
	w.subtaskFile("t", "01-a", "---\ndesign: [api/tokens.md]\n---\n## Goal\n\nG.\n")
	snap, err = w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	auth := "- " + filepath.Join(w.bridge, "projects", "supernova", "design", "auth.md") + " — Auth (review)"
	tokens := "- " + filepath.Join(w.bridge, "projects", "supernova", "design", "api", "tokens.md") + " — Tokens (review)"
	for _, want := range []string{"## Design\n\n", auth, tokens} {
		if !strings.Contains(snap.WorkItem, want) {
			t.Errorf("lacks %q:\n%s", want, snap.WorkItem)
		}
	}
	if strings.Contains(snap.WorkItem, "SECRET") {
		t.Errorf("design contents in the work item:\n%s", snap.WorkItem)
	}
	if strings.Index(snap.WorkItem, "## Subtask") > strings.Index(snap.WorkItem, "## Design") {
		t.Errorf("design belongs after the subtask:\n%s", snap.WorkItem)
	}
}

func TestDesignNamesAreValidated(t *testing.T) {
	w := newWorld(t)
	w.designFile("auth.md", "# Auth\n")
	w.addTask("t")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\npipeline: coding\ndesign: [auth.md]\n---\n\n## Goal\n\nDo t.\n")
	if _, err := w.openOut("t"); err != nil {
		t.Fatalf("good name: %v", err)
	}
	if _, err := w.snapOf("t", runOpts{}); err != nil {
		t.Fatalf("good name: %v", err)
	}
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\npipeline: coding\ndesign: [nope.md]\n---\n\n## Goal\n\nDo t.\n")
	if _, err := w.openOut("t"); err == nil || !strings.Contains(err.Error(), `design: "nope.md" is not a design file`) {
		t.Errorf("task open: %v", err)
	}
	if _, err := w.snapOf("t", runOpts{}); err == nil || !strings.Contains(err.Error(), `"nope.md"`) {
		t.Errorf("run: %v", err)
	}

	w.subtaskTask("s", "01-a")
	w.subtaskFile("s", "01-a", "---\ndesign: [gone.md]\n---\n## Goal\n\nG.\n")
	if _, err := w.snapOf("s", runOpts{}); err == nil || !strings.Contains(err.Error(), `"gone.md"`) || !strings.Contains(err.Error(), "01-a.md") {
		t.Errorf("subtask run: %v", err)
	}
	if _, err := w.openOut("s"); err == nil || !strings.Contains(err.Error(), `"gone.md"`) {
		t.Errorf("subtask task open: %v", err)
	}
}
