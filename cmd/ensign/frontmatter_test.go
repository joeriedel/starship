// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// front is the text after the opening --- line, so its first line is file line 2.
func TestDecodeFrontmatterLines(t *testing.T) {
	cases := []struct {
		name, front, want string
		same              bool
	}{
		{"flow sequence", "# a\n# b\nk: [a\n", "yaml: line 4: did not find expected ',' or ']'", false},
		{"indent", "a:\n    b: 1\n  c: 2", "yaml: line 4: did not find expected key", false},
		{"flow sequence on the first line", "k: [a\n", "yaml: line 2: did not find expected ',' or ']'", false},
		{"flow mapping", "# a\n# b\nk: {a: 1\n", "yaml: line 4: did not find expected ',' or '}'", false},
		{"block sequence", "- a\nk: b\n", "yaml: line 3: did not find expected '-' indicator", false},
		{"key", "# a\nk: v\n: x\n", "yaml: line 4: did not find expected key", false},
		{"scanner", "# a\n# b\n@x\n", "yaml: line 4: found character that cannot start any token", true},
		{"hex escape", "# a\n# b\nk: \"\\xZZ\"\n", "yaml: line 4: did not find expected hexdecimal number", true},
		{"type", "# a\n# b\nk: [1]\n", "line 4: cannot unmarshal", true},
		{"duplicate key", "k: 1\n\nk: 2\n", `yaml: unmarshal errors:` + "\n  line 4: mapping key \"k\" already defined at line 2", true},
		{"line-less", "k: *b\n", "yaml: unknown anchor 'b' referenced", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out struct {
				K string `yaml:"k"`
			}
			err := decodeFrontmatter([]byte(c.front), &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in: %v", c.want, err)
			}
			if !c.same {
				return
			}
			var again struct {
				K string `yaml:"k"`
			}
			if orig := yaml.Unmarshal([]byte("\n"+c.front), &again); err.Error() != orig.Error() {
				t.Errorf("should be unchanged: %v vs %v", err, orig)
			}
		})
	}
}

func TestDecodeFrontmatterKeepsTypeError(t *testing.T) {
	var out struct {
		K []string `yaml:"k"`
	}
	err := decodeFrontmatter([]byte("k: 1\n"), &out)
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("want a *yaml.TypeError: %v", err)
	}
}

func TestOpenTaskFrontmatterErrorNamesTheFile(t *testing.T) {
	w := newWorld(t)
	w.addTask("auth-refactor")
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	write(t, filepath.Join(taskDir, "task.md"), badTaskFile)
	err := openTask(w.bridge, "supernova/auth-refactor", taskDir, io.Discard)
	wantLine(t, err, filepath.Join(taskDir, "task.md")+": ")
}

func TestPickSubtaskFrontmatterErrorNamesTheFile(t *testing.T) {
	w := newWorld(t)
	w.subtaskTask("t", "1-a")
	file := filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md")
	write(t, file, badTaskFile)
	_, err := pickSubtask(w.bridge, runOpts{task: "supernova/t"})
	wantLine(t, err, taskDirOf(w.bridge, "supernova/t")+string(filepath.Separator)+"task.md: ")
}
