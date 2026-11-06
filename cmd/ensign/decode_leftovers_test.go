// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGuardedDecodeSites(t *testing.T) {
	for shape, doc := range panicShapes {
		fenced := "```starship-document\n" + doc + "```\n"
		t.Run(shape, func(t *testing.T) {
			w := newWorld(t)
			pf := filepath.Join(w.bridge, "projects", "supernova", "project.yaml")
			orig, err := os.ReadFile(pf)
			if err != nil {
				t.Fatal(err)
			}
			// The op rows extend the fixture's existing ops list, which ends the file. replace folds its decode error into its own message.
			for name, c := range map[string]struct {
				add  string
				want string
			}{
				"skills":   {"\nskills:\n" + indentLines(doc, "  "), "malformed mapping"},
				"insert":   {"      - insert:\n" + indentLines(doc, "          "), "malformed mapping"},
				"replace":  {"      - replace:\n" + indentLines(doc, "          "), "replace needs stage and with"},
				"parallel": {"      - set:\n          stage: review\n          parallel:\n" + indentLines(doc, "            "), "malformed mapping"},
			} {
				write(t, pf, string(orig)+c.add)
				_, err := loadConfig(w.bridge, "supernova/auth-refactor")
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("%s: want %q, got %v", name, c.want, err)
				}
			}

			dir := t.TempDir()
			path := filepath.Join(dir, "doc.yaml")
			write(t, path, doc)
			_, err = loadDoc(path)
			wantMalformed(t, "loadDoc", err)

			_, _, err = parseDocumentFile([]byte(fenced))
			if err == nil || !strings.Contains(err.Error(), "is not valid YAML") {
				t.Errorf("parseDocumentFile: %v", err)
			}

			write(t, filepath.Join(dir, "bridge.yaml"), doc)
			_, err = loadDeclaredSources(dir)
			wantMalformed(t, "loadDeclaredSources", err)
			_, err = loadDeclaredDocuments(dir)
			wantMalformed(t, "loadDeclaredDocuments", err)

			write(t, filepath.Join(dir, "projects", "supernova", "project.yaml"), doc)
			_, problems := collectLinks(dir, "supernova", filepath.Join(dir, "task.md"), nil)
			if len(problems) != 1 || !strings.Contains(problems[0], "malformed mapping") {
				t.Errorf("collectLinks project: %v", problems)
			}
			_, problems = collectLinks(dir, "supernova", filepath.Join(dir, "task.md"), []byte(doc))
			if len(problems) != 2 || !strings.Contains(problems[1], "malformed mapping") {
				t.Errorf("collectLinks task: %v", problems)
			}

			r := &RunFiles{Snap: &Snapshot{Bridge: w.bridge, Project: "supernova"}}
			s := &Stage{ID: "curate", Curates: "style"}
			d := &Dispatch{Outcome: "updated"}
			block := func(name, body string) string { return "```" + name + "\n" + body + "```\n" }
			if got := applyCuration(r, s, d, block("starship-document", doc)+block("starship-ledger", "{}\n")); !strings.Contains(got, "malformed mapping") {
				t.Errorf("applyCuration document: %q", got)
			}
			if got := applyCuration(r, s, d, block("starship-document", "{}\n")+block("starship-ledger", doc)); !strings.Contains(got, "malformed mapping") {
				t.Errorf("applyCuration ledger: %q", got)
			}
		})
	}
}

func TestParserLineIsTheTrueLine(t *testing.T) {
	// Each doc is what the decoder sees, so its first line is line 1.
	for name, c := range map[string]struct {
		doc  string
		line int
	}{
		"frontmatter indent":        {"\na:\n    b: 1\n  c: 2", 4},
		"nested indent":             {"top:\n  a:\n      b: 1\n    c: 2", 4},
		"comment first":             {"# comment\na:\n    b: 1\n  c: 2", 4},
		"unchanged":                 {"x: 1\na:\n    b: 1\n  c: 2", 4},
		"directive":                 {"%TAG !e! tag:example.com,2000:\n---\na:\n    b: !e!x 1\n  c: 2", 5},
		"anchor":                    {"m: &x 1\n\nn:\n    b: 1\n  c: *x\n  d: 2", 5},
		"later line of a scalar":    {"\na: \"x\n\\q\"", 3},
		"unclosed quote":            {"\na: 1\nb: 'x\nc: 3\nd: 4", 3},
		"unclosed flow":             {"\na: 1\nb: [1, 2\nc: 3", 3},
		"fault after a long scalar": {"a: 'x\n  y'\nb: 'z", 3},
	} {
		wantAt := "line " + strconv.Itoa(c.line) + ": "
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			wantLine(t, yamlUnmarshal([]byte(c.doc), &m), wantAt)
			path := filepath.Join(t.TempDir(), "x.yaml")
			write(t, path, c.doc)
			var out map[string]any
			wantLine(t, readYAML(path, &out), wantAt)
			// decodeFrontmatter stands in the dropped --- line, so the doc's line 1 is file line 2.
			var fm map[string]any
			wantLine(t, decodeFrontmatter([]byte(c.doc), &fm), "line "+strconv.Itoa(c.line+1)+": ")
		})
	}
}

func genRoles(t *testing.T, w *world, rolesYAML string, files map[string]string) map[string][]byte {
	t.Helper()
	root, _ := filepath.Abs("../..")
	for name, content := range files {
		write(t, filepath.Join(w.bridge, "roles", name+".md"), content)
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+filepath.ToSlash(root)+"\n"+rolesYAML)
	bf, err := loadBridgeFile(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generatedFiles(w.bridge, bf, root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoleModelOverrideTouchesOnlyFrontmatter(t *testing.T) {
	const override = "roles:\n  auditor:\n    model: "
	for name, c := range map[string]struct {
		file, model, want string
	}{
		"body model line untouched":       {"---\nname: auditor\n---\nmodel: x\n", "m1", "---\nname: auditor\n---\nmodel: x\n"},
		"empty model keeps the next line": {"---\nname: auditor\nmodel:\ndescription: d\n---\nbody\n", "m1", "---\nname: auditor\nmodel: m1\ndescription: d\n---\nbody\n"},
		"no model line":                   {"---\nname: auditor\n---\nbody\n", "m1", "---\nname: auditor\n---\nbody\n"},
		"dollar is literal":               {"---\nname: auditor\nmodel: old\n---\nbody\n", "x$1", "---\nname: auditor\nmodel: x$1\n---\nbody\n"},
		"crlf":                            {"---\r\nname: auditor\r\nmodel: old\r\n---\r\nbody\r\n", "m1", "---\r\nname: auditor\r\nmodel: m1\r\n---\r\nbody\r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			got := genRoles(t, w, override+c.model+"\n", map[string]string{"auditor": c.file})
			if string(got[".claude/agents/auditor.md"]) != c.want {
				t.Errorf("got %q, want %q", got[".claude/agents/auditor.md"], c.want)
			}
		})
	}
}

func TestRoleReplacesReadFromFrontmatter(t *testing.T) {
	cases := map[string]struct {
		file    string
		wantErr string
	}{
		"declared":           {"---\nname: curator\nreplaces: curator\n---\nbody\n", ""},
		"declared with crlf": {"---\r\nname: curator\r\nreplaces: curator\r\n---\r\nbody\r\n", ""},
		"body line":          {"---\nname: curator\n---\nreplaces: curator\n", "declare `replaces: curator`"},
		"empty":              {"---\nname: curator\nreplaces:\n---\nbody\n", "declare `replaces: curator`"},
		"undecodable":        {"---\nname: curator\nreplaces: [curator]\n---\nbody\n", "frontmatter"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			root, _ := filepath.Abs("../..")
			write(t, filepath.Join(w.bridge, "roles", "curator.md"), c.file)
			bf, err := loadBridgeFile(w.bridge)
			if err != nil {
				t.Fatal(err)
			}
			_, err = generatedFiles(w.bridge, bf, root)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("want %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestReadOnlyRole(t *testing.T) {
	agent := func(name, tools string) string {
		return "---\nname: " + name + "\ntools: " + tools + "\n---\nbody\n"
	}
	for name, c := range map[string]struct {
		files map[string]string
		role  string
		want  bool
	}{
		"name differs from stem":     {map[string]string{"lib-reader": agent("librarian", "Read, Grep")}, "librarian", true},
		"stem matches, name differs": {map[string]string{"reader": agent("librarian", "Read, Grep")}, "reader", false},
		"other tools":                {map[string]string{"librarian": agent("librarian", "Read, Write")}, "librarian", false},
		"no name, stem is the name":  {map[string]string{"librarian": "---\ntools: Read\n---\nbody\n"}, "librarian", true},
		"two files":                  {map[string]string{"a": agent("librarian", "Read"), "b": agent("librarian", "Read")}, "librarian", false},
		"no file":                    {map[string]string{"other": agent("other", "Read")}, "librarian", false},
		"unparseable file":           {map[string]string{"a": agent("librarian", "Read"), "b": "---\nname: [\n---\n"}, "librarian", false},
	} {
		t.Run(name, func(t *testing.T) {
			bridge := t.TempDir()
			for stem, content := range c.files {
				write(t, filepath.Join(bridge, ".claude", "agents", stem+".md"), content)
			}
			if got := readOnlyRole(bridge, c.role); got != c.want {
				t.Errorf("readOnlyRole = %v, want %v", got, c.want)
			}
		})
	}
	if readOnlyRole(t.TempDir(), "librarian") {
		t.Error("a bridge with no agents folder has no read-only role")
	}
}
