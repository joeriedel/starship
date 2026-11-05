package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func wantLine(t *testing.T, err error, line string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a decode error, got none")
	}
	if !strings.Contains(err.Error(), line) {
		t.Errorf("want %q in the error, got: %v", line, err)
	}
}

const badTaskFile = "---\n# one\n# two\n# three\npipeline: [coding]\n---\nbody\n"

func TestOpenTaskFrontmatterErrorLine(t *testing.T) {
	w := newWorld(t)
	w.addTask("auth-refactor")
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	write(t, filepath.Join(taskDir, "task.md"), badTaskFile)
	err := openTask(w.bridge, "supernova/auth-refactor", taskDir, io.Discard)
	wantLine(t, err, "line 5: ")
}

func TestLoadSubtasksFrontmatterErrorLine(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "1-a")
	w.subtaskFile("t", "1-a", "---\n# one\n# two\nlocation: [a, b]\n---\n## Goal\n\ng\n")
	_, err := loadSubtasks(taskDirOf(w.bridge, "supernova/t"))
	wantLine(t, err, "line 4: ")
}

func TestPickSubtaskFrontmatterErrorLine(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "1-a")
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), badTaskFile)
	_, err := pickSubtask(w.bridge, runOpts{task: "supernova/t"})
	wantLine(t, err, "line 5: ")
}

func TestAssembleSkillFrontmatterErrorLine(t *testing.T) {
	w := newWorld(t)
	cp, _ := filepath.Abs("../..")
	write(t, filepath.Join(w.bridge, "skills/fmline.md"), "---\n# one\n# two\ndescription: [a, b]\n---\nbody\n")
	_, err := assembleSkill(w.bridge, cp, "supernova", "fmline")
	wantLine(t, err, "line 4: ")
}

func TestAssembleSkillNoFrontmatter(t *testing.T) {
	w := newWorld(t)
	cp, _ := filepath.Abs("../..")
	write(t, filepath.Join(w.bridge, "skills/fmplain.md"), "just text\n")
	sd, err := assembleSkill(w.bridge, cp, "supernova", "fmplain")
	if err != nil || !strings.Contains(sd.Text, "just text") {
		t.Fatalf("%v %+v", err, sd)
	}
}

const dupKeyRoleFile = "---\nname: x\ntools: Read\n\nname: y\n---\nbody\n"

func TestFrontmatterMcpServersErrorLine(t *testing.T) {
	_, _, err := frontmatterMcpServers("supernova-role", []byte(dupKeyRoleFile))
	wantLine(t, err, `line 5: mapping key "name" already defined at line 2`)
}

func TestWithDeclaredToolsErrorLine(t *testing.T) {
	_, err := withDeclaredTools("supernova-role", []byte(dupKeyRoleFile), []string{"mcp__supernova__read"})
	wantLine(t, err, `line 5: mapping key "name" already defined at line 2`)
}
