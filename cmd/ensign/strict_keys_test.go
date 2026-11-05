// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeExt(t *testing.T, w *world, folder, body string) string {
	t.Helper()
	p := filepath.Join(w.bridge, "extensions", folder, "extension.yaml")
	write(t, p, body)
	return p
}

func loadSupernova(w *world, task string) error {
	_, err := loadConfig(w.bridge, task)
	return err
}

func TestStrictKeysProject(t *testing.T) {
	for name, c := range map[string]struct{ yaml, want string }{
		"top level":      {"conectors: x\n", "conectors"},
		"journal":        {"journal:\n  comit: off\n", "journal.comit"},
		"handler":        {"events:\n  task.close:\n    - { requires: ./check.sh }\n", "events.task.close[0].requires"},
		"location":       {"locations:\n  app:\n    depend-on: []\n", "locations.app.depend-on"},
		"cross-location": {"cross-location:\n  rules: x\n", "cross-location.rules"},
		"pipeline op":    {"pipelines:\n  coding:\n    opz: []\n", "pipelines.coding.opz"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			file := filepath.Join(w.bridge, "projects", "supernova", "project.yaml")
			w.project(c.yaml)
			w.frontTask("t", "")
			err := loadSupernova(w, "supernova/t")
			if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), c.want+": unknown key") {
				t.Fatalf("got %v; want unknown key %s in %s", err, c.want, file)
			}
		})
	}
}

func TestStrictKeysMappingWhereListExpected(t *testing.T) {
	w := newWorld(t)
	w.project("events:\n  task.close: {require: ./check.sh}\n")
	w.frontTask("t", "")
	err := loadSupernova(w, "supernova/t")
	if err == nil || strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("got %v; want the decoder's type error", err)
	}
}

func TestStrictKeysProjectAllowed(t *testing.T) {
	w := newWorld(t)
	w.project(`x-common: &m
  journal: {commit: "on", reason: "r"}
  documentation: "on"
x-other: {anything: [1, 2], at: all}
<<: *m
platforms: [linux]
use: []
skills: []
rules: []
workspace: ""
links: []
pipelines:
  coding: {ops: []}
events:
  task.close:
    - { observe: ./a.sh, status: active }
locations:
  app: {depends-on: [], bindings: {}, rules: []}
`)
	w.frontTask("t", "")
	if err := loadSupernova(w, "supernova/t"); err != nil && strings.Contains(err.Error(), "unknown key") {
		t.Fatal(err)
	}
}

func TestStrictKeysProjectMerge(t *testing.T) {
	w := newWorld(t)
	w.project("x-m: &m\n  conectors: x\n<<: *m\n")
	w.frontTask("t", "")
	if err := loadSupernova(w, "supernova/t"); err == nil || !strings.Contains(err.Error(), "conectors: unknown key") {
		t.Fatalf("got %v", err)
	}
}

func TestStrictKeysDedicatedRefusals(t *testing.T) {
	w := newWorld(t)
	w.project("connectors: x\nconectors: y\n")
	w.frontTask("t", "")
	if err := loadSupernova(w, "supernova/t"); err == nil || !strings.Contains(err.Error(), "connectors: is a bridge setting") {
		t.Fatalf("project: %v", err)
	}

	w = newWorld(t)
	w.frontTask("t", "platforms: [linux]\ntypo: 1\n")
	if err := loadSupernova(w, "supernova/t"); err == nil || !strings.Contains(err.Error(), "platforms: is a project setting") {
		t.Fatalf("task: %v", err)
	}

	w = newWorld(t)
	file := writeExt(t, w, "supernova-feed", "name: supernova-feed\nplatforms: [linux]\ntypo: 1\n")
	w.project("use: [supernova-feed]\n")
	w.frontTask("t", "")
	if err := loadSupernova(w, "supernova/t"); err == nil || !strings.Contains(err.Error(), file+": platforms: is a project setting") {
		t.Fatalf("extension: %v", err)
	}
}

func TestStrictKeysTask(t *testing.T) {
	for name, c := range map[string]struct{ front, want string }{
		"top level": {"documentaton: on\n", "documentaton"},
		"vcs":       {"vcs:\n  brach: x\n", "vcs.brach"},
		"x- key":    {"x-note: 1\n", "x-note"},
		"journal":   {"journal:\n  reasn: x\n", "journal.reasn"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.frontTask("t", c.front)
			file := filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md")
			err := loadSupernova(w, "supernova/t")
			if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), c.want+": unknown key") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStrictKeysTaskAllowed(t *testing.T) {
	w := newWorld(t)
	w.frontTask("t", "pipelines: {}\nbindings: {}\nrules: []\nuse: []\nskills: []\nworkspace: \"\"\nevents: {}\njournal: {commit: \"on\"}\ndocumentation: \"on\"\npipeline: coding\nvcs: {kind: git, branch: b, client: c}\nlocations: []\nlinks: []\nstatus: open\n")
	if err := loadSupernova(w, "supernova/t"); err != nil && strings.Contains(err.Error(), "unknown key") {
		t.Fatal(err)
	}
}

func TestStrictKeysTaskThroughOtherPaths(t *testing.T) {
	w := newWorld(t)
	w.frontTask("t", "documentaton: on\n")
	if _, err := loadTaskEvents(w.bridge, "supernova/t"); err == nil || !strings.Contains(err.Error(), "documentaton: unknown key") {
		t.Fatalf("loadTaskEvents: %v", err)
	}
	if _, err := taskHasNoCode(w.bridge, "supernova", "t"); err == nil || !strings.Contains(err.Error(), "documentaton: unknown key") {
		t.Fatalf("taskHasNoCode: %v", err)
	}
}

func TestStrictKeysProjectBypassingProjectLayer(t *testing.T) {
	w := newWorld(t)
	w.project("conectors: x\n")
	if _, err := loadLocations(w.bridge, "supernova"); err == nil || !strings.Contains(err.Error(), "conectors: unknown key") {
		t.Fatalf("loadLocations: %v", err)
	}
	if _, err := projectPipeline(filepath.Join(w.bridge, "projects/supernova/project.yaml")); err == nil || !strings.Contains(err.Error(), "conectors: unknown key") {
		t.Fatalf("projectPipeline: %v", err)
	}
}

func TestStrictKeysExtension(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"top level": {"name: supernova-feed\nsumary: x\n", "sumary"},
		"provides":  {"name: supernova-feed\nprovides: x\n", "provides"},
		"x- key":    {"name: supernova-feed\nx-a: 1\n", "x-a"},
		"help":      {"name: supernova-feed\nhelp:\n  - { say: a, doe: b }\n", "help[0].doe"},
		"handler":   {"name: supernova-feed\nevents:\n  task.close:\n    - { requires: ./a.sh }\n", "events.task.close[0].requires"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			file := writeExt(t, w, "supernova-feed", c.body)
			_, broken := scanExtensions(w.bridge, w.bridge)
			err := broken["supernova-feed"]
			if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), c.want+": unknown key") {
				t.Fatalf("got %v; broken = %v", err, broken)
			}
			help, cmds := helpBoth(w)
			if !strings.Contains(help, "supernova-feed: "+file) || !strings.Contains(cmds, "supernova-feed: "+file) {
				t.Errorf("help does not name the broken extension:\n%s\n%s", help, cmds)
			}
			w.project("use: [supernova-feed]\n")
			w.frontTask("t", "")
			if err := loadSupernova(w, "supernova/t"); err == nil || !strings.Contains(err.Error(), `extension "supernova-feed"`) {
				t.Errorf("enabling it: %v", err)
			}
		})
	}
}

func TestStrictKeysBrokenExtensionDoesNotStopOthers(t *testing.T) {
	w := newWorld(t)
	writeExt(t, w, "supernova-feed", "name: supernova-feed\nprovides: x\n")
	w.frontTask("t", "")
	if err := loadSupernova(w, "supernova/t"); err != nil {
		t.Fatalf("unrelated load: %v", err)
	}
}

func TestStrictKeysExtensionAllowed(t *testing.T) {
	w := newWorld(t)
	writeExt(t, w, "supernova-feed", `name: supernova-feed
summary: s
requires-bindings: []
help:
  - { say: a, does: b, run: c }
quiet: false
publish: ./p.sh
pipelines: {}
bindings: {}
rules: []
use: []
skills: []
workspace: ""
events: {}
journal: {commit: "on"}
documentation: "on"
`)
	_, broken := scanExtensions(w.bridge, w.bridge)
	if broken["supernova-feed"] != nil {
		t.Fatal(broken["supernova-feed"])
	}
}

func TestStrictKeysExoticShapesDoNotPanic(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":            "",
		"scalar":           "x\n",
		"sequence root":    "- a\n",
		"alias merge key":  "x: &m <<\n*m : {connectors: y}\n",
		"alias key":        "x-k: &k connectors\n*k :\n  a: b\n",
		"unhashable key":   "[a]: 1\n<<: {a: 1}\n",
		"alias cycle":      "a: &a [*a]\nevents: &e {x: *e}\n",
		"bad merge":        "<<: 5\n",
		"merge cycle":      "&a {<<: *a}\n",
		"nested merge seq": "<<: [[{journal: {commit: x}}]]\n",
		"malformed":        "a: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, s := range []*keySchema{projectKeys, taskKeys, extensionKeys} {
				firstUnknownKey([]byte(doc), s)
			}
		})
	}
	// Repeated aliases must not blow up.
	doc := "events:\n  e: &l0 [{observe: a}]\n"
	for i := 1; i < 40; i++ {
		doc += fmt.Sprintf("  e%d: &l%d [*l%d, *l%d]\n", i, i, i-1, i-1)
	}
	firstUnknownKey([]byte(doc), projectKeys)
}

func yamlTags(t *testing.T, v any) []string {
	t.Helper()
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		if tag == "" || strings.HasPrefix(tag, ",") {
			continue
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	return out
}

func TestStrictKeysSchemasCoverStructs(t *testing.T) {
	has := func(s *keySchema, key string) bool { _, ok := s.keys[key]; return ok }
	var layer Layer
	var task TaskFile
	var ext Extension
	var handler HandlerSpec
	for _, k := range yamlTags(t, layer) {
		if !has(projectKeys, k) || !has(taskKeys, k) && k != "platforms" || !has(extensionKeys, k) && k != "platforms" {
			t.Errorf("Layer key %q is missing from a schema", k)
		}
	}
	for _, k := range yamlTags(t, task) {
		if !has(taskKeys, k) {
			t.Errorf("TaskFile key %q is missing from the task schema", k)
		}
	}
	for _, k := range yamlTags(t, ext) {
		if !has(extensionKeys, k) {
			t.Errorf("Extension key %q is missing from the extension schema", k)
		}
	}
	nested := func(who string, s *keySchema, fields []string) {
		for _, k := range fields {
			if !has(s, k) {
				t.Errorf("%s key %q is missing from its schema", who, k)
			}
		}
	}
	nested("HandlerSpec", projectKeys.keys["events"].values.items, yamlTags(t, handler))
	nested("TaskVcs", taskKeys.keys["vcs"], yamlTags(t, TaskVcs{}))
	nested("HelpEntry", extensionKeys.keys["help"].items, yamlTags(t, HelpEntry{}))
	nested("Journal", projectKeys.keys["journal"], yamlTags(t, layer.Journal))
	nested("Pipelines", projectKeys.keys["pipelines"].values, []string{"ops"})
	loc := reflect.TypeOf(projectLocationsFile{}).Field(0).Type.Elem()
	var locTags []string
	for i := 0; i < loc.NumField(); i++ {
		locTags = append(locTags, strings.Split(loc.Field(i).Tag.Get("yaml"), ",")[0])
	}
	nested("location", projectKeys.keys["locations"].values, locTags)
	nested("project file", projectKeys, yamlTags(t, projectLocationsFile{}))
	nested("cross-location", projectKeys.keys["cross-location"], []string{"bindings"})
}

func TestStrictKeysShippedFiles(t *testing.T) {
	root, _ := filepath.Abs("../..")
	exts, _ := filepath.Glob(filepath.Join(root, "extensions", "*", "extension.yaml"))
	if len(exts) == 0 {
		t.Fatal("no shipped extensions found")
	}
	for _, f := range exts {
		var x Extension
		if err := readExtensionYAML(f, &x); err != nil {
			t.Errorf("%v", err)
		}
	}
	for _, f := range []string{
		filepath.Join(root, ".starship", "project.yaml"),
		filepath.Join(root, "testdata", "bridge", "projects", "supernova", "project.yaml"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%v", err)
			continue
		}
		if err := checkProjectKeys(f); err != nil {
			t.Errorf("%v", err)
		}
	}
}
