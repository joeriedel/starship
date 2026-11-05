package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var panicShapes = map[string]string{
	"sequence key": "? [a]\n: 1\n<<: {b: 1}\n",
	"mapping key":  "? {a: 1}\n: x\n<<: {b: 1}\n",
}

func indentLines(s, pad string) string {
	return pad + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n"+pad) + "\n"
}

func wantMalformed(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "malformed mapping") {
		t.Errorf("%s: want a malformed mapping error, got %v", name, err)
	}
}

func TestDecodesSurviveUnhashableKeyWithMerge(t *testing.T) {
	for shape, doc := range panicShapes {
		t.Run(shape, func(t *testing.T) {
			var m map[string]any
			wantMalformed(t, "decodeFrontmatter", decodeFrontmatter([]byte(doc), &m))
			role := []byte("---\n" + doc + "---\n\nBody.\n")
			_, _, err := frontmatterMcpServers("role.md", role)
			wantMalformed(t, "frontmatterMcpServers", err)
			_, err = frontmatterToolsValue("role.md", role)
			wantMalformed(t, "frontmatterToolsValue", err)
			dir := t.TempDir()
			path := filepath.Join(dir, "x.yaml")
			write(t, path, doc)
			var out map[string]any
			wantMalformed(t, "readYAML", readYAML(path, &out))
			write(t, filepath.Join(dir, "bridge.yaml"), doc)
			_, err = loadBridgeFile(dir)
			wantMalformed(t, "loadBridgeFile", err)
		})
	}
}

func TestShippedProjectSurvivesUnhashableKeyWithMerge(t *testing.T) {
	for shape, doc := range panicShapes {
		for place, body := range map[string]string{
			"top level":    doc,
			"locations":    "locations:\n" + indentLines(doc, "  "),
			"one location": "locations:\n  a:\n" + indentLines(doc, "    "),
		} {
			t.Run(shape+"/"+place, func(t *testing.T) {
				dir := t.TempDir()
				write(t, filepath.Join(dir, filepath.FromSlash(shippedProjectFile)), body)
				_, _, err := shippedProject(dir)
				if place != "top level" && (err == nil || !strings.Contains(err.Error(), "is not valid YAML")) {
					t.Errorf("want a not valid YAML error, got %v", err)
				}
			})
		}
	}
}

func TestMigrateWorkspacesSkipsUnhashableKeyWithMerge(t *testing.T) {
	for shape, doc := range panicShapes {
		t.Run(shape, func(t *testing.T) {
			b := t.TempDir()
			write(t, filepath.Join(b, ".gitignore"), ".starship/\n")
			write(t, filepath.Join(b, ".starship/machine.yaml"), "projects: "+filepath.ToSlash(t.TempDir())+"\n")
			write(t, filepath.Join(b, "projects/supernova/tasks/bad/task.md"), "---\n"+doc+"---\n\n# bad\n")
			migrateWorkspaces(b)
			data, err := os.ReadFile(filepath.Join(b, "projects/supernova/tasks/bad/task.md"))
			if err != nil || !strings.Contains(string(data), "<<") {
				t.Errorf("the task file should be left alone: %v", err)
			}
		})
	}
}

func TestPlainReadsNameTheTrueParserLine(t *testing.T) {
	for name, c := range map[string]struct {
		doc  string
		want string // substring of the error; "" means the error carries no line
	}{
		"flow sequence":     {"a: 1\nb: [1, 2\nc: 3", "line 2: "},
		"indent":            {"a:\n    b: 1\n  c: 2", "line 3: "},
		"sequence in map":   {"a: 1\n- x", "line 2: "},
		"stray bracket":     {"a: 1\nb: ]", "line 2: "},
		"tag handle":        {"a: 1\nb: !x!y z", "line 2: "},
		"scanner":           {"a: 1\nb: 2\nc: d: e", "line 3: mapping values are not allowed in this context"},
		"unmarshal":         {"use: {a: b}\n", "line 1: cannot unmarshal"},
		"first line parser": {"!x!y z", ""},
	} {
		reads := map[string]func(dir, doc string) error{
			"readYAML": func(dir, doc string) error {
				path := filepath.Join(dir, "x.yaml")
				write(t, path, doc)
				var l Layer
				return readYAML(path, &l)
			},
			"readSettingsAs": func(dir, doc string) error {
				path := filepath.Join(dir, "x.yaml")
				write(t, path, doc)
				_, _, err := readSettingsAs(path, true)
				return err
			},
			"loadBridgeFile": func(dir, doc string) error {
				write(t, filepath.Join(dir, "bridge.yaml"), doc)
				_, err := loadBridgeFile(dir)
				return err
			},
		}
		for read, f := range reads {
			t.Run(name+"/"+read, func(t *testing.T) {
				err := f(t.TempDir(), c.doc)
				if err == nil {
					t.Fatal("want an error")
				}
				if c.want == "" {
					if strings.Contains(err.Error(), "line ") {
						t.Errorf("want no line number, got %v", err)
					}
					return
				}
				wantLine(t, err, c.want)
			})
		}
	}
}
