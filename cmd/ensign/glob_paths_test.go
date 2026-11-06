// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// inBridge makes bridge the session's bridge and the working folder for the rest of the test.
func inBridge(t *testing.T, bridge string) {
	t.Helper()
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	t.Setenv("CLAUDE_PROJECT_DIR", bridge)
	if err := os.Chdir(bridge); err != nil {
		t.Fatal(err)
	}
}

// bracketedPair is a bridge and a control plane whose paths hold a bracket class and an unclosed bracket.
func bracketedPair(t *testing.T) (bridge, cp string) {
	t.Helper()
	bridge = filepath.Join(t.TempDir(), "supernova[b]", "bridge")
	cp = filepath.Join(t.TempDir(), "supernova[cp")
	write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(cp)+"\n")
	write(t, filepath.Join(cp, "manual", "session.md"), "manual\n")
	return bridge, cp
}

func TestListingSitesFindFilesUnderBracketedFolders(t *testing.T) {
	bridge, cp := bracketedPair(t)
	d := func() *Dispatch { return &Dispatch{Fields: map[string][]string{}} }

	t.Run("topicNames", func(t *testing.T) {
		write(t, filepath.Join(bridge, ".starship", "topics", "supernova.md"), "x\n")
		names, err := topicNames(bridge)
		if err != nil || !contains(names, "supernova") {
			t.Errorf("%v, %v", names, err)
		}
	})
	t.Run("generatedFiles", func(t *testing.T) {
		write(t, filepath.Join(cp, "manual", "topics", "supernova.md"), "x\n")
		bf, err := loadBridgeFile(bridge)
		if err != nil {
			t.Fatal(err)
		}
		files, err := generatedFiles(bridge, bf, cp)
		if err != nil || files[".starship/topics/supernova.md"] == nil {
			t.Errorf("topic missing from generated files (err %v)", err)
		}
	})
	t.Run("knownSkills", func(t *testing.T) {
		write(t, filepath.Join(cp, "skills", "supernova-cp.md"), "x\n")
		write(t, filepath.Join(bridge, "skills", "supernova-br.md"), "x\n")
		got, err := knownSkills(bridge, cp, "")
		if err != nil || !contains(got, "supernova-cp") || !contains(got, "supernova-br") {
			t.Errorf("%v, %v", got, err)
		}
	})
	t.Run("proposedSkills", func(t *testing.T) {
		write(t, filepath.Join(bridge, "skills", "proposed", "supernova", "f1.yaml"), "fact: x\n")
		got, err := proposedSkills(bridge)
		if err != nil || got["supernova"] != 1 {
			t.Errorf("%v, %v", got, err)
		}
	})
	t.Run("generatorItems and itemContext for pending skills", func(t *testing.T) {
		write(t, filepath.Join(bridge, "skills", "pending", "supernova", "f1.yaml"), "fact: nova-fact\n")
		items, err := generatorItems(&Config{Bridge: bridge}, "pending-skills", false)
		if err != nil || len(items) != 1 || items[0] != "supernova" {
			t.Fatalf("%v, %v", items, err)
		}
		r := &RunFiles{Snap: &Snapshot{Bridge: bridge, ControlPlane: cp}, State: &State{}}
		st := &Stage{Parallel: &Parallel{Over: "pending-skills"}}
		disp := d()
		text, err := itemContext(r, st, "supernova", disp)
		if err != nil || !strings.Contains(text, "nova-fact") || len(disp.Fields["inputs"]) != 1 {
			t.Errorf("%q, %v, %v", text, err, disp.Fields)
		}
	})
	t.Run("pendingDocuments and its inputs", func(t *testing.T) {
		write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(cp)+"\ndocuments:\n  supernova-plan:\n    path: notes/plan.md\n")
		write(t, filepath.Join(bridge, "documents", "pending", "supernova-plan", "p1.yaml"), "proposal: nova-proposal\n")
		got, err := pendingDocuments(bridge)
		if err != nil || len(got) != 1 || got[0] != "supernova-plan" {
			t.Fatalf("%v, %v", got, err)
		}
		texts, rels, err := pendingDocumentInputs(bridge, "supernova-plan")
		if err != nil || len(texts) != 1 || len(rels) != 1 || !strings.Contains(texts[0], "nova-proposal") {
			t.Errorf("%v %v %v", texts, rels, err)
		}
		r := &RunFiles{Snap: &Snapshot{Bridge: bridge, ControlPlane: cp}, State: &State{}}
		st := &Stage{Parallel: &Parallel{Over: "pending-documents"}}
		disp := d()
		if text, err := itemContext(r, st, "supernova-plan", disp); err != nil || !strings.Contains(text, "nova-proposal") {
			t.Errorf("%q, %v", text, err)
		}
	})
	t.Run("mcpServersRefusal and agentsSignature", func(t *testing.T) {
		write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(cp)+"\n"+writesYAML)
		write(t, filepath.Join(bridge, ".claude", "agents", "supernova-scout.md"), "---\nname: supernova-scout\nmcpServers:\n  x: {}\n---\nbody\n")
		in := &HookInput{ToolInput: map[string]any{"subagent_type": "supernova-scout"}}
		if why := mcpServersRefusal(bridge, in); !strings.Contains(why, "mcpServers") {
			t.Errorf("refusal = %q", why)
		}
		if sig := agentsSignature(bridge); !strings.Contains(sig, "supernova-scout.md") {
			t.Errorf("signature = %q", sig)
		}
	})
	t.Run("heldRuns", func(t *testing.T) {
		write(t, filepath.Join(bridge, ".starship", "sessions", "supernova.sequence"), "{")
		if _, ok := heldRuns(bridge); ok {
			t.Error("the unreadable sequence file was not found")
		}
		write(t, filepath.Join(bridge, ".starship", "sessions", "supernova.sequence"), "{}")
		if held, ok := heldRuns(bridge); !ok || len(held) != 0 {
			t.Errorf("%v, %v", held, ok)
		}
	})
	t.Run("skill new moves held facts", func(t *testing.T) {
		inBridge(t, bridge)
		os.RemoveAll(filepath.Join(bridge, "skills", "pending"))
		var out bytes.Buffer
		if err := cmdSkill([]string{"new", "supernova", "--description", "x"}, &out); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(bridge, "skills", "pending", "supernova", "f1.yaml")); err != nil {
			t.Errorf("the held fact did not become pending: %v", err)
		}
	})
}

func TestMigrateAndSplitHistoryWalkBracketedBridge(t *testing.T) {
	bridge, _ := bracketedPair(t)
	ws := filepath.ToSlash(t.TempDir())
	write(t, filepath.Join(bridge, "projects", "supernova", "project.yaml"), "# Project: supernova\n\nworkspace: "+ws+"\n")
	write(t, filepath.Join(bridge, "projects", "supernova", "tasks", "launch", "task.md"),
		"---\npipeline: coding\n---\n\n# t\n\n## Progress log\n\n- one.\n\n## Outstanding\n\n- x\n")
	if note := migrateWorkspaces(bridge); !strings.Contains(note, "projects/supernova/project.yaml") {
		t.Errorf("migrate found nothing: %q", note)
	}
	if note := splitHistory(bridge); note != "" {
		t.Errorf("note: %q", note)
	}
	if _, err := os.Stat(filepath.Join(bridge, "projects", "supernova", "tasks", "launch", "history.md")); err != nil {
		t.Errorf("the progress log was not split: %v", err)
	}
}

func TestProjectAndTaskWalks(t *testing.T) {
	bridge := filepath.Join(t.TempDir(), "supernova[b")
	if got, err := taskFiles(bridge); got != nil || err != nil {
		t.Errorf("missing projects/: %v, %v", got, err)
	}
	if got, err := projectFiles(bridge, "project.yaml"); got != nil || err != nil {
		t.Errorf("missing projects/: %v, %v", got, err)
	}
	p := func(parts ...string) string { return filepath.Join(append([]string{bridge, "projects"}, parts...)...) }
	write(t, p("stray.txt"), "x\n")
	write(t, p("a", "project.yaml"), "x\n")
	write(t, p("a", "tasks"), "a file, not a folder\n")
	write(t, p("b", "project.yaml"), "x\n")
	write(t, p("b", "tasks", "t1", "task.md"), "x\n")
	write(t, p("b", "tasks", "stray.txt"), "x\n")
	write(t, p("b", "tasks", "t2", "other.md"), "x\n")
	write(t, p("b", "style", "sources.yaml"), "x\n")
	if err := os.MkdirAll(p("b", "tasks", "t3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(bridge, "nowhere"), p("b", "tasks", "t3", "task.md")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	linked := filepath.Join(t.TempDir(), "elsewhere")
	write(t, filepath.Join(linked, "project.yaml"), "x\n")
	write(t, filepath.Join(linked, "tasks", "t4", "task.md"), "x\n")
	if err := os.Symlink(linked, p("c")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	tasks, err := taskFiles(bridge)
	want := []string{p("b", "tasks", "t1", "task.md"), p("b", "tasks", "t3", "task.md"), p("c", "tasks", "t4", "task.md")}
	if err != nil || strings.Join(tasks, "\n") != strings.Join(want, "\n") {
		t.Errorf("taskFiles = %v, %v; want %v", tasks, err, want)
	}
	projects, err := projectFiles(bridge, "project.yaml")
	want = []string{p("a", "project.yaml"), p("b", "project.yaml"), p("c", "project.yaml")}
	if err != nil || strings.Join(projects, "\n") != strings.Join(want, "\n") {
		t.Errorf("projectFiles = %v, %v; want %v", projects, err, want)
	}
	styles, err := projectFiles(bridge, "style", "sources.yaml")
	if err != nil || len(styles) != 1 || styles[0] != p("b", "style", "sources.yaml") {
		t.Errorf("style sources = %v, %v", styles, err)
	}
}

func TestStrayFilesInListedFoldersAreNotErrors(t *testing.T) {
	bridge, cp := bracketedPair(t)
	write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(cp)+"\ndocuments:\n  supernova-plan:\n    path: notes/plan.md\n")
	write(t, filepath.Join(bridge, "skills", "proposed", ".DS_Store"), "x\n")
	write(t, filepath.Join(bridge, "skills", "pending", ".DS_Store"), "x\n")
	write(t, filepath.Join(bridge, "documents", "pending", ".DS_Store"), "x\n")
	if got, err := proposedSkills(bridge); err != nil || len(got) != 0 {
		t.Errorf("proposedSkills: %v, %v", got, err)
	}
	if _, err := generatorItems(&Config{Bridge: bridge}, "pending-skills", false); err == nil || !strings.Contains(err.Error(), "no skill has pending facts") {
		t.Errorf("pending skills: %v", err)
	}
	if _, err := pendingDocuments(bridge); err == nil || !strings.Contains(err.Error(), "no declared document has pending proposals") {
		t.Errorf("pending documents: %v", err)
	}
}

// fileAt replaces whatever is at path with a file, so listing path itself, or anything beneath it, as a folder fails on every platform.
func fileAt(t *testing.T, path string) {
	t.Helper()
	os.RemoveAll(path)
	write(t, path, "not a folder\n")
}

func TestListingFailuresReachTheirChannel(t *testing.T) {
	bridge, cp := bracketedPair(t)

	t.Run("heldRuns is not ok", func(t *testing.T) {
		fileAt(t, filepath.Join(bridge, ".starship", "sessions"))
		if _, ok := heldRuns(bridge); ok {
			t.Error("gc would treat every run as unheld")
		}
	})
	t.Run("mcpServersRefusal refuses", func(t *testing.T) {
		write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(cp)+"\n"+writesYAML)
		fileAt(t, filepath.Join(bridge, ".claude", "agents"))
		in := &HookInput{ToolInput: map[string]any{"subagent_type": "supernova-scout"}}
		if why := mcpServersRefusal(bridge, in); !strings.Contains(why, "agents") {
			t.Errorf("refusal = %q", why)
		}
	})
	t.Run("topicNames", func(t *testing.T) {
		fileAt(t, filepath.Join(bridge, ".starship", "topics"))
		if _, err := topicNames(bridge); err == nil {
			t.Error("no error")
		}
	})
	t.Run("generatedFiles", func(t *testing.T) {
		fileAt(t, filepath.Join(cp, "manual", "topics"))
		bf, err := loadBridgeFile(bridge)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := generatedFiles(bridge, bf, cp); err == nil {
			t.Error("no error")
		}
	})
	t.Run("skill new and discard", func(t *testing.T) {
		inBridge(t, bridge)
		fileAt(t, filepath.Join(bridge, "skills"))
		var out bytes.Buffer
		if err := cmdSkill([]string{"new", "supernova", "--description", "x"}, &out); err == nil || !strings.Contains(err.Error(), "listing skills") {
			t.Errorf("new: %v", err)
		}
		os.RemoveAll(filepath.Join(bridge, "skills"))
		fileAt(t, filepath.Join(bridge, "skills", "proposed"))
		if err := cmdSkill([]string{"discard", "supernova"}, &out); err == nil || !strings.Contains(err.Error(), "listing skills") {
			t.Errorf("discard: %v", err)
		}
		os.RemoveAll(filepath.Join(bridge, "skills"))
		fileAt(t, filepath.Join(bridge, "skills", "pending"))
		if _, err := generatorItems(&Config{Bridge: bridge}, "pending-skills", false); err == nil || !strings.Contains(err.Error(), "listing skills/pending") {
			t.Errorf("generatorItems: %v", err)
		}
	})
	t.Run("itemContext and pendingDocumentInputs", func(t *testing.T) {
		os.RemoveAll(filepath.Join(bridge, "skills"))
		fileAt(t, filepath.Join(bridge, "skills", "pending", "supernova"))
		r := &RunFiles{Snap: &Snapshot{Bridge: bridge, ControlPlane: cp}, State: &State{}}
		st := &Stage{Parallel: &Parallel{Over: "pending-skills"}}
		if _, err := itemContext(r, st, "supernova", &Dispatch{Fields: map[string][]string{}}); err == nil {
			t.Error("itemContext: no error")
		}
		os.RemoveAll(filepath.Join(bridge, "documents"))
		fileAt(t, filepath.Join(bridge, "documents", "pending", "supernova-plan"))
		if _, _, err := pendingDocumentInputs(bridge, "supernova-plan"); err == nil {
			t.Error("pendingDocumentInputs: no error")
		}
	})
	t.Run("pendingDocuments", func(t *testing.T) {
		write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(cp)+"\ndocuments:\n  supernova-plan:\n    path: notes/plan.md\n")
		os.RemoveAll(filepath.Join(bridge, "documents"))
		fileAt(t, filepath.Join(bridge, "documents", "pending"))
		if _, err := pendingDocuments(bridge); err == nil || !strings.Contains(err.Error(), "listing documents/pending") {
			t.Errorf("%v", err)
		}
	})
	t.Run("statusText", func(t *testing.T) {
		os.RemoveAll(filepath.Join(bridge, "skills"))
		fileAt(t, filepath.Join(bridge, "skills", "proposed"))
		if got := statusText(bridge, "s1"); !strings.Contains(got, "listing skills/proposed") {
			t.Errorf("the failure is not in the status:\n%s", got)
		}
	})
}

func TestSkillNewListingFailureWritesNothing(t *testing.T) {
	bridge, _ := bracketedPair(t)
	inBridge(t, bridge)
	fileAt(t, filepath.Join(bridge, "skills", "proposed", "supernova"))
	var out bytes.Buffer
	if err := cmdSkill([]string{"new", "supernova", "--description", "x"}, &out); err == nil || !strings.Contains(err.Error(), "listing the facts held") {
		t.Errorf("new: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bridge, "skills", "supernova.md")); err == nil {
		t.Error("the skill was written before the listing failed")
	}
}

func TestWalkListingFailuresAreNotes(t *testing.T) {
	bridge, _ := bracketedPair(t)
	write(t, filepath.Join(bridge, "projects"), "not a folder\n")
	if note := migrateWorkspaces(bridge); !strings.Contains(note, "could not list") {
		t.Errorf("migrate: %q", note)
	}
	if note := splitHistory(bridge); !strings.Contains(note, "could not list") {
		t.Errorf("split: %q", note)
	}
}

func TestScanExtensionsLaterBrokenExtensionReplacesGoodOne(t *testing.T) {
	good := func(t *testing.T) (bridge, cp string) {
		bridge, cp = t.TempDir(), t.TempDir()
		write(t, filepath.Join(cp, "extensions", "supernova-feed", "extension.yaml"), "name: supernova-feed\n")
		return bridge, cp
	}
	t.Run("decoded name", func(t *testing.T) {
		bridge, cp := good(t)
		write(t, filepath.Join(bridge, "extensions", "mine", "extension.yaml"), "name: supernova-feed\nplatforms: linux\n")
		got, broken := scanExtensions(bridge, cp)
		if got["supernova-feed"] != nil || broken["supernova-feed"] == nil {
			t.Errorf("good = %v, broken = %v", got, broken)
		}
	})
	t.Run("name recovered from a syntax error", func(t *testing.T) {
		bridge, cp := good(t)
		write(t, filepath.Join(bridge, "extensions", "mine", "extension.yaml"), "name: supernova-feed\nsummary: [a\n")
		got, broken := scanExtensions(bridge, cp)
		if got["supernova-feed"] != nil || broken["supernova-feed"] == nil {
			t.Errorf("good = %v, broken = %v", got, broken)
		}
	})
	t.Run("folder name only", func(t *testing.T) {
		bridge, cp := good(t)
		write(t, filepath.Join(bridge, "extensions", "supernova-feed", "extension.yaml"), "summary: [a\n")
		got, broken := scanExtensions(bridge, cp)
		if got["supernova-feed"] == nil || broken["supernova-feed"] == nil {
			t.Errorf("good = %v, broken = %v", got, broken)
		}
	})
}

func TestReadDirIfAny(t *testing.T) {
	dir := t.TempDir()
	if entries, err := readDirIfAny(filepath.Join(dir, "missing")); err != nil || len(entries) != 0 {
		t.Errorf("missing folder: entries = %v, err = %v", entries, err)
	}
	file := filepath.Join(dir, "supernova.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDirIfAny(file); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plain file: err = %v, want an error that is not fs.ErrNotExist", err)
	}
	entries, err := readDirIfAny(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "supernova.txt" {
		t.Errorf("real folder: entries = %v, err = %v", entries, err)
	}
}

func TestReadDirIfAnyBeneathAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "supernova.txt")
	fileAt(t, file)
	for _, p := range []string{filepath.Join(file, "child"), filepath.Join(file, "child", "grandchild")} {
		if _, err := readDirIfAny(p); err == nil {
			t.Errorf("%s: want an error", p)
		}
	}
	if entries, err := readDirIfAny(filepath.Join(dir, "missing", "deeper")); err != nil || len(entries) != 0 {
		t.Errorf("missing: entries = %v, err = %v", entries, err)
	}
}

func TestPlainFileOnPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "supernova.txt")
	fileAt(t, file)
	for _, p := range []string{file, filepath.Join(file, "child"), filepath.Join(file, "child", "grandchild")} {
		if got := plainFileOnPath(p); got != file {
			t.Errorf("%s: got %q, want %q", p, got, file)
		}
	}
	if got := plainFileOnPath(filepath.Join(dir, "missing", "deeper")); got != "" {
		t.Errorf("missing: got %q", got)
	}
}

func TestJournalCommitTouchesOnlyTheLiteralName(t *testing.T) {
	needGit(t)
	const dir = "projects/supernova/tasks/"
	bracketed, sibling := dir+"a[X]/task.md", dir+"aX/task.md"
	for _, c := range []struct {
		name   string
		exists bool
	}{{"absent", false}, {"unchanged", true}} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			write(t, filepath.Join(w.bridge, filepath.FromSlash(sibling)), "# sibling\n")
			if c.exists {
				write(t, filepath.Join(w.bridge, filepath.FromSlash(bracketed)), "# bracketed\n")
			}
			journalFixture(t, w)
			head := runGit(t, w.bridge, "rev-parse", "HEAD")
			write(t, filepath.Join(w.bridge, filepath.FromSlash(sibling)), "# sibling\n\nSee .starship/runs/20261105-190410-supernova-x/d1.log\n")

			if err := checkJournalSources(w.bridge, []string{bracketed}); err != nil {
				t.Errorf("the sibling's lines were checked for the bracketed name: %v", err)
			}
			var out bytes.Buffer
			if err := cmdJournal([]string{"commit", "-m", "Bracketed", bracketed}, &out); err == nil {
				t.Errorf("nothing changed under the bracketed name, so the commit should fail:\n%s", out.String())
			}
			if got := runGit(t, w.bridge, "rev-parse", "HEAD"); got != head {
				t.Errorf("a commit was made: %s", runGit(t, w.bridge, "show", "--name-only", "--format=", "HEAD"))
			}
			if st := runGit(t, w.bridge, "status", "--porcelain", "--", dir); !strings.Contains(st, "aX/task.md") || !strings.HasPrefix(st, "M ") {
				t.Errorf("the sibling should stay modified and unstaged: %q", st)
			}
		})
	}
}

func TestJournalCommitRejectsEmptyFileName(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "log.md"), "# log\n")
	journalFixture(t, w)
	head := runGit(t, w.bridge, "rev-parse", "HEAD")
	write(t, filepath.Join(w.bridge, "log.md"), "# log, edited\n")

	for _, files := range [][]string{{""}, {"log.md", ""}} {
		var out bytes.Buffer
		if err := cmdJournal(append([]string{"commit", "-m", "Empty"}, files...), &out); err == nil || !strings.Contains(err.Error(), "empty file name") {
			t.Errorf("files %q: err = %v", files, err)
		}
		if got := runGit(t, w.bridge, "rev-parse", "HEAD"); got != head {
			t.Errorf("files %q: a commit was made: %s", files, runGit(t, w.bridge, "show", "--name-only", "--format=", "HEAD"))
		}
		if staged := runGit(t, w.bridge, "diff", "--cached", "--name-only"); staged != "" {
			t.Errorf("files %q: staged %q", files, staged)
		}
		if st := runGit(t, w.bridge, "status", "--porcelain"); st != "M log.md" {
			t.Errorf("files %q: status %q", files, st)
		}
	}
}

func TestCommitNotesLeavesJunkSiblingOfBracketedNote(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	journalFixture(t, w)
	write(t, notesPath(w, "notes/.a/n.md"), "junk\n")
	runGit(t, w.bridge, "add", "-f", "notes/.a/n.md")
	runGit(t, w.bridge, "commit", "-q", "-m", "junk")
	write(t, notesPath(w, "notes/.a/n.md"), "junk, edited\n")
	write(t, notesPath(w, "notes/[.]a/n.md"), "note\n")
	if note, _ := commitNotes(w.bridge); note == "" || note == notesCommitFailed {
		t.Fatalf("note %q", note)
	}
	if got := committedNotes(t, w.bridge); got != "notes/[.]a/n.md" {
		t.Errorf("committed %q, want only the bracketed note", got)
	}
	if st := runGit(t, w.bridge, "status", "--porcelain", "--", "notes/.a"); !strings.HasPrefix(st, "M ") {
		t.Errorf("the hidden file should stay modified: %q", st)
	}
}

func TestSplitHistoryAndMigrateIgnoreGlobSiblings(t *testing.T) {
	needGit(t)
	repo := func(t *testing.T) string {
		t.Helper()
		bridge := filepath.Join(t.TempDir(), "supernova[b]", "bridge")
		newRepo(t, bridge)
		return bridge
	}
	files := func(bridge string) string {
		return runGit(t, bridge, "-c", "core.quotepath=off", "show", "--name-only", "--format=", "HEAD")
	}

	t.Run("splitHistory", func(t *testing.T) {
		bridge := repo(t)
		const dir = "projects/supernova/tasks/"
		doc := "---\npipeline: coding\n---\n\n# t\n\n## Progress log\n\n- one.\n\n## Outstanding\n\n- x\n"
		write(t, filepath.Join(bridge, dir, "a[X]", "task.md"), doc)
		write(t, filepath.Join(bridge, dir, "aX", "task.md"), doc)
		runGit(t, bridge, "add", "-A")
		runGit(t, bridge, "commit", "-q", "-m", "tasks")
		write(t, filepath.Join(bridge, dir, "aX", "task.md"), doc+"- dirty\n")

		if note := splitHistory(bridge); note != "" {
			t.Errorf("note: %q", note)
		}
		if _, err := os.Stat(filepath.Join(bridge, dir, "a[X]", "history.md")); err != nil {
			t.Fatalf("the sibling's changes stopped the split: %v", err)
		}
		if got := files(bridge); got != dir+"a[X]/history.md\n"+dir+"a[X]/task.md" {
			t.Errorf("committed %q", got)
		}
		if st := runGit(t, bridge, "status", "--porcelain", "--", "projects"); st != "M "+dir+"aX/task.md" {
			t.Errorf("the sibling should stay modified: %q", st)
		}
	})

	t.Run("migrateWorkspaces", func(t *testing.T) {
		bridge := repo(t)
		ws := filepath.ToSlash(t.TempDir())
		write(t, filepath.Join(bridge, ".gitignore"), ".starship/\n")
		write(t, filepath.Join(bridge, "projects", "a[X]", "project.yaml"), "workspace: "+ws+"\n")
		write(t, filepath.Join(bridge, "projects", "aX", "project.yaml"), "# Project: aX\n")
		runGit(t, bridge, "add", "-A")
		runGit(t, bridge, "commit", "-q", "-m", "projects")
		write(t, filepath.Join(bridge, "projects", "aX", "project.yaml"), "# Project: aX, edited\n")

		if note := migrateWorkspaces(bridge); note == "" {
			t.Fatalf("note: %q", note)
		}
		if got := files(bridge); got != ".gitignore\nprojects/a[X]/project.yaml" {
			t.Errorf("committed %q", got)
		}
		if st := runGit(t, bridge, "status", "--porcelain"); st != "M projects/aX/project.yaml" {
			t.Errorf("the sibling should stay modified: %q", st)
		}
	})
}

func TestPendingSkillsSkipsUnusableNames(t *testing.T) {
	bridge := t.TempDir()
	pending := filepath.Join(bridge, "skills", "pending")
	names := []string{"Bad Name", ".hidden"}
	if runtime.GOOS != "windows" {
		names = append(names, "con")
	}
	for _, name := range names {
		write(t, filepath.Join(pending, name, "f1.yaml"), "fact: x\n")
	}
	write(t, filepath.Join(pending, "Empty Name", "notes.txt"), "x\n")
	if _, err := generatorItems(&Config{Bridge: bridge}, "pending-skills", false); err == nil || !strings.Contains(err.Error(), "no skill has pending facts to curate") {
		t.Errorf("only unusable names should leave nothing to curate: %v", err)
	}
	write(t, filepath.Join(pending, "supernova-notes", "f1.yaml"), "fact: x\n")
	items, err := generatorItems(&Config{Bridge: bridge}, "pending-skills", false)
	if err != nil || len(items) != 1 || items[0] != "supernova-notes" {
		t.Errorf("%v, %v", items, err)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(pending, name, "f1.yaml")); err != nil {
			t.Errorf("%s should stay on disk: %v", name, err)
		}
	}
}

func TestApplyCurationRefusesUnusableSkillName(t *testing.T) {
	bridge := t.TempDir()
	r := &RunFiles{Snap: &Snapshot{Bridge: bridge}}
	s := &Stage{ID: "curate", Curates: "skill"}
	d := &Dispatch{Outcome: "updated", Dim: "Bad Name"}
	if got := applyCuration(r, s, d, ""); got == "" {
		t.Error("an unusable skill name should be a problem")
	}
	if matches, _ := filepath.Glob(filepath.Join(bridge, "skills", "*.learned.yaml")); len(matches) != 0 {
		t.Errorf("nothing should be written: %v", matches)
	}
}
