// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const bridgeSettings = `platforms: [linux]
use: [docker]
bindings:
  build: { command: "./build.sh" }
  test: { command: "./test.sh" }
pipelines:
  coding:
    ops:
      - set: { budgets.test: 2 }
`

// shippedWorld sets the bridge's project.yaml and the code's shipped file, and returns the workspace.
func shippedWorld(t *testing.T, bridgeYAML, shipped string) (*world, string) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), bridgeYAML)
	ws, err := workspaceFor(w.bridge, "supernova", "auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if shipped != "" {
		write(t, filepath.Join(ws, ".starship/project.yaml"), shipped)
	}
	return w, ws
}

func openShipped(t *testing.T, w *world) string {
	t.Helper()
	var out bytes.Buffer
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	if err := openTask(w.bridge, "supernova/auth-refactor", taskDir, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestShippedIdenticalAndFormattingOnlyAreQuiet(t *testing.T) {
	w, ws := shippedWorld(t, bridgeSettings, bridgeSettings)
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("identical settings should be quiet:\n%s", got)
	}
	reformatted := "# a comment\r\nuse:\r\n  - docker\r\npipelines:\r\n  coding:\r\n    ops:\r\n      - set:\r\n          budgets.test: 2\r\nbindings:\r\n  test:\r\n    command: ./test.sh\r\n  build:\r\n    command: ./build.sh\r\n"
	write(t, filepath.Join(ws, ".starship/project.yaml"), reformatted)
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("formatting-only differences should be quiet:\n%s", got)
	}
}

func TestShippedDriftIsReported(t *testing.T) {
	shipped := bridgeSettings
	shipped = strings.Replace(shipped, `./test.sh`, `./test2.sh`, 1)
	shipped = strings.Replace(shipped, "use: [docker]", "use: [docker, redis]", 1)
	shipped = strings.Replace(shipped, "budgets.test: 2", "budgets.test: 3", 1)
	w, _ := shippedWorld(t, bridgeSettings, shipped)
	got := openShipped(t, w)
	for _, want := range []string{"binding test: changed", "pipeline coding: changed", "extension redis: not in the bridge's copy", "ensign project reviewed supernova"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "binding build") || strings.Contains(got, "extension docker") {
		t.Errorf("unchanged items should not be reported:\n%s", got)
	}
	added := strings.Replace(bridgeSettings, "bindings:\n", "bindings:\n  deploy: { command: ./deploy.sh }\n", 1)
	write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), bridgeSettings)
	ws, _ := workspaceFor(w.bridge, "supernova", "auth-refactor")
	write(t, filepath.Join(ws, ".starship/project.yaml"), added)
	if got := openShipped(t, w); !strings.Contains(got, "binding deploy: not in the bridge's copy") {
		t.Errorf("an added binding should be reported:\n%s", got)
	}
}

func TestShippedReviewedSilencesUntilShippedValueChanges(t *testing.T) {
	shipped := strings.Replace(bridgeSettings, `./test.sh`, `./test2.sh`, 1)
	shipped = strings.Replace(shipped, "use: [docker]", "use: [docker, redis]", 1)
	w, ws := shippedWorld(t, bridgeSettings, shipped)
	var out bytes.Buffer
	if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &out); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(w.bridge, "projects/supernova/shipped-reviewed.yaml")
	if data, err := os.ReadFile(rec); err != nil || !strings.HasPrefix(string(data), "# ") {
		t.Fatalf("reviewed file with header expected: %v\n%s", err, data)
	}
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("reviewed differences should be quiet:\n%s", got)
	}
	write(t, filepath.Join(ws, ".starship/project.yaml"), strings.Replace(shipped, `./test2.sh`, `./test3.sh`, 1))
	got := openShipped(t, w)
	if !strings.Contains(got, "binding test: changed") || strings.Contains(got, "redis") {
		t.Errorf("only the newly changed value should be reported:\n%s", got)
	}
}

func TestShippedReviewedFloatSurvivesRoundTrip(t *testing.T) {
	shipped := strings.Replace(bridgeSettings, "budgets.test: 2", "budgets.test: 2.0", 1)
	shipped = strings.Replace(shipped, `./test.sh`, `./test2.sh`, 1)
	w, _ := shippedWorld(t, bridgeSettings, shipped)
	if got := openShipped(t, w); !strings.Contains(got, "binding test: changed") {
		t.Fatalf("drift expected before review:\n%s", got)
	}
	var out bytes.Buffer
	if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("a just-reviewed 2.0 should be quiet:\n%s", got)
	}
}

func TestShippedMissingPiecesAreQuiet(t *testing.T) {
	w, ws := shippedWorld(t, bridgeSettings, "")
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("no shipped file, no report:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova"}, &bytes.Buffer{}); err == nil {
		t.Error("project reviewed should fail when the code ships no file")
	}
	write(t, filepath.Join(ws, ".starship/project.yaml"), "bindings: [unclosed\n")
	if got := openShipped(t, w); !strings.Contains(got, "shipped    can't compare:") {
		t.Errorf("a broken shipped file should say so:\n%s", got)
	}
	os.RemoveAll(ws)
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("a missing workspace should not report:\n%s", got)
	}
}

const locShippedClient = `bindings:
  build: { command: "./build.sh" }
  test:  { command: "./client-test.sh" }
`

const locShippedServer = `bindings:
  build: { command: "./build.sh" }
  test:  { command: "./test.sh" }
`

// openLocShipped opens the named task; locShipped sets a location's shipped file.
func openLocShipped(t *testing.T, w *world, task string) string {
	t.Helper()
	var out bytes.Buffer
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks", strings.TrimPrefix(task, "supernova/"))
	if err := openTask(w.bridge, task, taskDir, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func locShipped(t *testing.T, w *world, loc, content string) {
	t.Helper()
	dir, err := locationPathFor(w.bridge, "supernova", loc)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".starship/project.yaml"), content)
}

func TestShippedLocationsQuietWhenEffectiveSettingsMatch(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", locShippedClient)
	locShipped(t, w, "server", locShippedServer)
	if got := openLocShipped(t, w, task); strings.Contains(got, "shipped") {
		t.Errorf("a shipped binding equal to the location's override is not drift:\n%s", got)
	}
}

func TestShippedLocationDriftNamesTheLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", strings.Replace(locShippedClient, "./client-test.sh", "./other.sh", 1))
	locShipped(t, w, "server", locShippedServer)
	got := openLocShipped(t, w, task)
	if !strings.Contains(got, "location client: the code's") || !strings.Contains(got, "binding test: changed") ||
		!strings.Contains(got, "project reviewed supernova --location client") {
		t.Errorf("client drift expected:\n%s", got)
	}
	if strings.Contains(got, "location server: the code's") || strings.Contains(got, "--location server") {
		t.Errorf("server has no drift:\n%s", got)
	}
}

func TestShippedLocationReviewedIsPerLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", strings.Replace(locShippedClient, "./client-test.sh", "./other.sh", 1))
	locShipped(t, w, "server", strings.Replace(locShippedServer, "./test.sh", "./other.sh", 1))
	if err := cmdProject([]string{"reviewed", "supernova", "--location", "client"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := openLocShipped(t, w, task)
	if strings.Contains(got, "--location client") || !strings.Contains(got, "--location server") {
		t.Errorf("only client should be silenced:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova", "--location", "server"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := openLocShipped(t, w, task); strings.Contains(got, "shipped") {
		t.Errorf("both should be quiet, client's entry kept:\n%s", got)
	}
}

func TestShippedLocationReviewedWithoutFlagRecordsAll(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", strings.Replace(locShippedClient, "./client-test.sh", "./other.sh", 1))
	locShipped(t, w, "server", strings.Replace(locShippedServer, "./test.sh", "./other.sh", 1))
	var out bytes.Buffer
	if err := cmdProject([]string{"reviewed", "supernova"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := openLocShipped(t, w, task); strings.Contains(got, "shipped") {
		t.Errorf("both locations should be recorded:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova", "--location", "nope"}, &out); err == nil {
		t.Error("an unknown location should fail")
	}
}

func TestShippedRulesMissingFromBridgeAreReported(t *testing.T) {
	bridge := bridgeSettings + "rules:\n  - Keep the hull sealed.\n  - Log every burn.\n"
	shipped := bridgeSettings + "rules:\n  - \"  Keep the hull sealed.  \"\n  - Log every burn.\n  - Log every burn.\n  - |-\n    Never vent\n    the cargo bay.\n"
	w, _ := shippedWorld(t, bridge, shipped)
	got := openShipped(t, w)
	if want := `rule "Never vent\nthe cargo bay.": not in the bridge's copy`; !strings.Contains(got, want) {
		t.Errorf("missing %q:\n%s", want, got)
	}
	if strings.Contains(got, "hull") || strings.Contains(got, "burn") {
		t.Errorf("rules the bridge has should be quiet:\n%s", got)
	}
	// A rule only the bridge has is quiet.
	ws, _ := workspaceFor(w.bridge, "supernova", "auth-refactor")
	write(t, filepath.Join(ws, ".starship/project.yaml"), bridgeSettings+"rules:\n  - Log every burn.\n")
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("a bridge-only rule should be quiet:\n%s", got)
	}
}

func TestShippedMalformedRulesCannotCompare(t *testing.T) {
	w, _ := shippedWorld(t, bridgeSettings, bridgeSettings+"rules: not-a-list\n")
	if got := openShipped(t, w); !strings.Contains(got, "shipped    can't compare:") || !strings.Contains(got, "rules must be a list of strings") {
		t.Errorf("malformed rules should say so:\n%s", got)
	}
}

func TestShippedRulesReviewed(t *testing.T) {
	shipped := bridgeSettings + "rules:\n  - Keep the hull sealed.\n"
	w, ws := shippedWorld(t, bridgeSettings, shipped)
	want := `rule "Keep the hull sealed.": not in the bridge's copy`
	if got := openShipped(t, w); !strings.Contains(got, want) {
		t.Fatalf("missing %q:\n%s", want, got)
	}
	if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("a reviewed rule should be quiet:\n%s", got)
	}
	write(t, filepath.Join(ws, ".starship/project.yaml"), shipped+"  - Log every burn.\n")
	got := openShipped(t, w)
	if !strings.Contains(got, `rule "Log every burn."`) || strings.Contains(got, "hull") {
		t.Errorf("only the new rule should be reported:\n%s", got)
	}
	rec := filepath.Join(w.bridge, "projects/supernova/shipped-reviewed.yaml")
	write(t, rec, "bindings:\n  build: { command: ./build.sh }\n")
	if got := openShipped(t, w); !strings.Contains(got, want) {
		t.Errorf("a record without rules reviews none:\n%s", got)
	}
}

const rulesLocationsProject = `platforms: [linux]
rules:
  - Keep the hull sealed.
locations:
  client:
    rules:
      - Log every burn.
  server:
`

func TestShippedRulesPerLocation(t *testing.T) {
	w := locationsWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), rulesLocationsProject)
	task := locTask(t, w, "both", "client,server")
	shipped := "rules:\n  - Keep the hull sealed.\n  - Log every burn.\n"
	locShipped(t, w, "client", shipped)
	locShipped(t, w, "server", shipped)
	got := openLocShipped(t, w, task)
	if !strings.Contains(got, `rule "Log every burn.": not in the bridge's copy`) || strings.Count(got, "Log every burn.") != 1 {
		t.Errorf("the client's rule should be reported once, for server:\n%s", got)
	}
	if strings.Contains(got, "hull") || strings.Contains(got, "location client: the code's") || !strings.Contains(got, "--location server") {
		t.Errorf("only server drifts, and the project's rule is quiet:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova", "--location", "server"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := openLocShipped(t, w, task); strings.Contains(got, "shipped") {
		t.Errorf("server's review should silence it:\n%s", got)
	}
	write(t, reviewedFile(w.bridge, "supernova"), "")
	if got := openLocShipped(t, w, task); !strings.Contains(got, "--location server") {
		t.Errorf("an empty record reviews nothing:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := openLocShipped(t, w, task); strings.Contains(got, "shipped") {
		t.Errorf("reviewed without --location records every location's rules:\n%s", got)
	}
}

func TestShippedBindingKeysNormalise(t *testing.T) {
	w := locationsWorld(t)
	project := strings.Replace(locationsProject, `test: { command: "./client-test.sh" }`, `"@build": { command: "./client-build.sh" }`, 1)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), project)
	task := locTask(t, w, "both", "client,server")
	client := "bindings:\n  build: { command: ./client-build.sh }\n  test: { command: ./test.sh }\n"
	locShipped(t, w, "client", client)
	locShipped(t, w, "server", locShippedServer)
	if got := openLocShipped(t, w, task); strings.Contains(got, "shipped") {
		t.Errorf("a @build overlay should lay over build:\n%s", got)
	}
	locShipped(t, w, "client", strings.Replace(client, "./client-build.sh", "./other.sh", 1))
	if got := openLocShipped(t, w, task); !strings.Contains(got, "binding build: changed") {
		t.Errorf("a real difference should report:\n%s", got)
	}
}

func TestShippedBindingKeysAtSignInFiles(t *testing.T) {
	at := func(body string) string { return "bindings:\n" + body }
	w, ws := shippedWorld(t, bridgeSettings, at("  \"@build\": { command: ./build.sh }\n  test: { command: ./test.sh }\n"))
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("shipped @build should match bridge build:\n%s", got)
	}
	// Reviewed @build silences a shipped build.
	write(t, filepath.Join(ws, ".starship/project.yaml"), at("  build: { command: ./new.sh }\n"))
	write(t, filepath.Join(w.bridge, "projects/supernova/shipped-reviewed.yaml"), at("  \"@build\": { command: ./new.sh }\n"))
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("reviewed @build should silence build:\n%s", got)
	}
	// With both keys the later wins.
	os.Remove(filepath.Join(w.bridge, "projects/supernova/shipped-reviewed.yaml"))
	write(t, filepath.Join(ws, ".starship/project.yaml"), at("  build: { command: ./old.sh }\n  \"@build\": { command: ./build.sh }\n"))
	if got := openShipped(t, w); strings.Contains(got, "binding build") {
		t.Errorf("the later key should win:\n%s", got)
	}
	write(t, filepath.Join(ws, ".starship/project.yaml"), at("  \"@build\": { command: ./old.sh }\n  build: { command: ./build.sh }\n"))
	if got := openShipped(t, w); strings.Contains(got, "binding build") {
		t.Errorf("the later key should win:\n%s", got)
	}
	write(t, filepath.Join(ws, ".starship/project.yaml"), at("  build: { command: ./build.sh }\n  \"@build\": { command: ./old.sh }\n"))
	if got := openShipped(t, w); !strings.Contains(got, "binding build: changed") {
		t.Errorf("the later key should win:\n%s", got)
	}
}

func TestShippedNonMappingBindingsCannotCompare(t *testing.T) {
	for _, body := range []string{"bindings: not-a-map\n", "bindings: [a, b]\n"} {
		w, _ := shippedWorld(t, bridgeSettings, body)
		if got := openShipped(t, w); !strings.Contains(got, "shipped    can't compare:") || !strings.Contains(got, "bindings must be a mapping") {
			t.Errorf("%q should say it can't compare:\n%s", body, got)
		}
		if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{}); err == nil {
			t.Errorf("project reviewed should fail on %q", body)
		}
	}
}

func TestShippedBindingsAliasComparesByValue(t *testing.T) {
	shipped := "x-common: &common\n  build: { command: ./build.sh }\n  test: { command: ./test.sh }\nbindings: *common\n"
	w, ws := shippedWorld(t, bridgeSettings, shipped)
	if got := openShipped(t, w); strings.Contains(got, "shipped") {
		t.Errorf("an aliased binding set that matches should be quiet:\n%s", got)
	}
	write(t, filepath.Join(ws, ".starship/project.yaml"), strings.Replace(shipped, "./test.sh", "./other.sh", 1))
	if got := openShipped(t, w); !strings.Contains(got, "binding test: changed") {
		t.Errorf("an aliased binding that differs should report:\n%s", got)
	}
}

// loadBindings writes the bridge's project.yaml and loads the auth-refactor task's config.
func loadBindings(t *testing.T, w *world, body string) (*Config, error) {
	t.Helper()
	write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), "platforms: [linux]\n"+body)
	return loadConfig(w.bridge, "supernova/auth-refactor")
}

func TestRunBindingsAliasIsApplied(t *testing.T) {
	w := newWorld(t)
	c, err := loadBindings(t, w, "x-common: &common\n  deploy: { command: ./deploy.sh }\nbindings: *common\n")
	if err != nil {
		t.Fatal(err)
	}
	b, ok := c.Bindings["deploy"]
	if !ok || b.Command != "./deploy.sh" || !strings.Contains(b.Source, "project.yaml:3") {
		t.Errorf("aliased binding: %v, %+v", ok, b)
	}
}

func TestRunNonMappingBindingsAreRefused(t *testing.T) {
	w := newWorld(t)
	for _, body := range []string{"bindings: not-a-map\n", "bindings: [a, b]\n", "x-s: &s plain\nbindings: *s\n"} {
		_, err := loadBindings(t, w, body)
		if err == nil || !strings.Contains(err.Error(), "project.yaml: line ") || !strings.Contains(err.Error(), "bindings must be a mapping") {
			t.Errorf("%q should be refused naming file and line: %v", body, err)
		}
	}
}

func TestRunNullBindingsAreEmpty(t *testing.T) {
	w := newWorld(t)
	for _, body := range []string{"bindings:\n", "bindings: ~\n", "x-n: &n ~\nbindings: *n\n"} {
		c, err := loadBindings(t, w, body)
		if err != nil {
			t.Errorf("%q should load: %v", body, err)
			continue
		}
		for name, b := range c.Bindings {
			if strings.Contains(b.Source, "project.yaml") {
				t.Errorf("%q gave binding %s from the project", body, name)
			}
		}
	}
}

func TestRunNonMappingBindingsInExtensionAreRefused(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "extensions", "house", "extension.yaml"), "name: house\nsummary: house rules\nbindings: [a, b]\n")
	_, err := loadBindings(t, w, "use: [house]\n")
	if err == nil || !strings.Contains(err.Error(), "extension.yaml: line 3: bindings must be a mapping") {
		t.Errorf("extension bindings should be refused: %v", err)
	}
}

func TestLocationNonMappingBindingsAreRefused(t *testing.T) {
	w := newWorld(t)
	for _, body := range []string{
		"locations:\n  api:\n    bindings: not-a-map\n",
		"cross-location:\n  bindings: [a, b]\n",
	} {
		write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), "platforms: [linux]\n"+body)
		_, err := loadLocations(w.bridge, "supernova")
		if err == nil || !strings.Contains(err.Error(), "project.yaml: line ") || !strings.Contains(err.Error(), "bindings must be a mapping") {
			t.Errorf("%q should be refused naming file and line: %v", body, err)
		}
	}
}

func TestTaskNonMappingBindingsNameTrueLine(t *testing.T) {
	w := newWorld(t)
	taskFile := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
	write(t, taskFile, "---\npipeline: coding\nbindings: [a, b]\n---\nbody\n")
	_, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err == nil || !strings.Contains(err.Error(), "task.md: line 3: bindings must be a mapping") {
		t.Errorf("task bindings should be refused at the true file line: %v", err)
	}
	write(t, taskFile, "---\npipeline: coding\nbindings:\n  deploy:\n    command: ./d.sh\n---\nbody\n")
	c, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Bindings["deploy"]; !strings.Contains(b.Source, "task.md:4") {
		t.Errorf("task binding source should carry its true line: %+v", b)
	}
}

// reportBindings is what the drift report reads from a shipped file's bindings.
func reportBindings(t *testing.T, body string) (map[string]any, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "project.yaml")
	write(t, path, body)
	s, _, err := readSettings(path)
	return s.Bindings, err
}

func reportCommand(m map[string]any, name string) string {
	b, _ := m[name].(map[string]any)
	s, _ := b["command"].(string)
	return s
}

func TestBindingsValueAliasOnBothPaths(t *testing.T) {
	body := "x-b: &b { command: ./b.sh }\nbindings:\n  build: *b\n  deploy:\n    " + runtime.GOOS + ": *b\n"
	c, err := loadBindings(t, newWorld(t), body)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bindings["build"].Command != "./b.sh" || c.Bindings["deploy"].Command != "./b.sh" {
		t.Errorf("run: %+v %+v", c.Bindings["build"], c.Bindings["deploy"])
	}
	m, err := reportBindings(t, body)
	if err != nil || reportCommand(m, "build") != "./b.sh" {
		t.Errorf("report: %v %v", m, err)
	}
	_, err = loadBindings(t, newWorld(t), "x-s: &s plain\nbindings:\n  build: *s\n")
	if err == nil || !strings.Contains(err.Error(), "a binding must be a mapping") {
		t.Errorf("alias to a scalar should be refused: %v", err)
	}
}

func TestBindingsFieldAliasUsesValue(t *testing.T) {
	body := "x-c: &c ./c.sh\nx-g: &g look at it\nx-n: &n only on paper\nx-t: &t 5m\nbindings:\n  build: { command: *c, timeout: *t, plain: true }\n  review: { guidance: *g }\n  deploy: { not-applicable: *n }\n"
	c, err := loadBindings(t, newWorld(t), body)
	if err != nil {
		t.Fatal(err)
	}
	b := c.Bindings["build"]
	if b.Command != "./c.sh" || b.Timeout != 5*time.Minute || c.Bindings["review"].Guidance != "look at it" || c.Bindings["deploy"].NotApplicable != "only on paper" {
		t.Errorf("field aliases: %+v %+v %+v", b, c.Bindings["review"], c.Bindings["deploy"])
	}
	m, err := reportBindings(t, body)
	if err != nil || reportCommand(m, "build") != "./c.sh" {
		t.Errorf("report: %v %v", m, err)
	}
}

// mergeCases are bindings written with merge keys and the command each name should end up with, on a run and in the report.
var mergeCases = []struct {
	name, body string
	want       map[string]string
}{
	{"alias", "x-m: &m\n  build: { command: ./m-build.sh }\n  test: { command: ./m-test.sh }\nbindings:\n  <<: *m\n", map[string]string{"build": "./m-build.sh", "test": "./m-test.sh"}},
	{"list-earlier-wins", "x-a: &a\n  build: { command: ./a.sh }\nx-b: &b\n  build: { command: ./b.sh }\n  test: { command: ./b-test.sh }\nbindings:\n  <<: [*a, *b]\n", map[string]string{"build": "./a.sh", "test": "./b-test.sh"}},
	{"inline", "bindings:\n  <<: { build: { command: ./i.sh } }\n", map[string]string{"build": "./i.sh"}},
	{"explicit-beats-merged-before", "x-m: &m\n  build: { command: ./m.sh }\nbindings:\n  build: { command: ./own.sh }\n  <<: *m\n", map[string]string{"build": "./own.sh"}},
	{"explicit-beats-merged-after", "x-m: &m\n  build: { command: ./m.sh }\nbindings:\n  <<: *m\n  build: { command: ./own.sh }\n", map[string]string{"build": "./own.sh"}},
	{"explicit-at-beats-merged", "x-m: &m\n  build: { command: ./m.sh }\nbindings:\n  <<: *m\n  \"@build\": { command: ./own.sh }\n", map[string]string{"build": "./own.sh"}},
	{"anchor-with-own-merge", "x-base: &base\n  test: { command: ./base-test.sh }\n  build: { command: ./base.sh }\nx-m: &m\n  <<: *base\n  build: { command: ./m.sh }\nbindings:\n  <<: *m\n", map[string]string{"build": "./m.sh", "test": "./base-test.sh"}},
	{"at-collapse", "bindings:\n  build: { command: ./one.sh }\n  \"@build\": { command: ./two.sh }\n  test: { command: ./t.sh }\n", map[string]string{"build": "./two.sh", "test": "./t.sh"}},
}

func TestBindingsMergeKeysOnBothPaths(t *testing.T) {
	for _, tc := range mergeCases {
		c, err := loadBindings(t, newWorld(t), tc.body)
		if err != nil {
			t.Errorf("%s: run: %v", tc.name, err)
			continue
		}
		m, err := reportBindings(t, tc.body)
		if err != nil {
			t.Errorf("%s: report: %v", tc.name, err)
			continue
		}
		if _, ok := c.Bindings["<<"]; ok {
			t.Errorf("%s: run has a << binding", tc.name)
		}
		if _, ok := m["<<"]; ok {
			t.Errorf("%s: report has a << binding", tc.name)
		}
		for name, want := range tc.want {
			if got := c.Bindings[name].Command; got != want {
				t.Errorf("%s: run %s = %q, want %q", tc.name, name, got, want)
			}
			if got := reportCommand(m, name); got != want {
				t.Errorf("%s: report %s = %q, want %q", tc.name, name, got, want)
			}
		}
		if len(m) != len(tc.want) {
			t.Errorf("%s: report has %v", tc.name, m)
		}
	}
}

func TestBindingsMergedSourceIsTrueLine(t *testing.T) {
	c, err := loadBindings(t, newWorld(t), "x-m: &m\n  build: { command: ./m.sh }\nbindings:\n  <<: *m\n")
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Bindings["build"]; !strings.Contains(b.Source, "project.yaml:3") {
		t.Errorf("merged source should be the line of build in the file: %+v", b)
	}
}

func TestBindingsBadMergeValueIsRefused(t *testing.T) {
	for _, body := range []string{
		"bindings:\n  <<: plain\n",
		"x-s: &s plain\nbindings:\n  <<: *s\n",
		"x-m: &m { build: { command: ./m.sh } }\nbindings:\n  <<: [*m, plain]\n",
	} {
		_, err := loadBindings(t, newWorld(t), body)
		if err == nil || !strings.Contains(err.Error(), "project.yaml") || !strings.Contains(err.Error(), "line ") {
			t.Errorf("%q: run should refuse naming file and line: %v", body, err)
		}
		if _, err := reportBindings(t, body); err == nil || !strings.Contains(err.Error(), "line ") {
			t.Errorf("%q: report should refuse naming a line: %v", body, err)
		}
	}
}

func TestBindingsCyclesAreRefused(t *testing.T) {
	otherPlatform := "linux"
	if runtime.GOOS == "linux" {
		otherPlatform = "windows"
	}
	for _, body := range []string{
		"bindings: &x\n  <<: *x\n",
		"bindings:\n  build: &x\n    " + runtime.GOOS + ": *x\n",
		"bindings:\n  build: &x\n    " + runtime.GOOS + ":\n      <<: *x\n",
		"bindings:\n  build: &x\n    command: *x\n",
		"bindings:\n  build: &x\n    guidance: *x\n",
		"bindings:\n  build: &x\n    not-applicable: *x\n",
		"bindings:\n  build: &x\n    " + runtime.GOOS + ": { command: ./b.sh }\n    " + otherPlatform + ": *x\n",
	} {
		_, err := loadBindings(t, newWorld(t), body)
		if err == nil || !strings.Contains(err.Error(), "project.yaml:") {
			t.Errorf("%q: run should refuse naming the file: %v", body, err)
		}
		if _, err := reportBindings(t, body); err == nil {
			t.Errorf("%q: report should refuse", body)
		}
	}
}

func TestBindingsUnhashableMergeKeyIsRefused(t *testing.T) {
	body := "bindings:\n  build:\n    1: y\n    <<:\n      ? {a: b}\n      : c\n"
	_, err := loadBindings(t, newWorld(t), body)
	if err == nil || !strings.Contains(err.Error(), "project.yaml:") {
		t.Errorf("run should refuse naming the file: %v", err)
	}
	if _, err := reportBindings(t, body); err == nil {
		t.Error("report should refuse")
	}
}

func TestRulesAliasesAreFollowed(t *testing.T) {
	c, err := loadBindings(t, newWorld(t), "x-r: &r\n  - keep it small\nrules: *r\n")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Rules, func(r Rule) bool { return r.Text == "keep it small" }) {
		t.Errorf("rules: *r: %+v", c.Rules)
	}
	c, err = loadBindings(t, newWorld(t), "x-s: &s keep it kind\nx-n: &n ~\nrules:\n  - *s\n  - *n\n")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Text == "keep it kind" })
	if i < 0 || !strings.Contains(c.Rules[i].Source, "project.yaml:5") {
		t.Errorf("rules item alias: %+v", c.Rules)
	}
	if _, err = loadBindings(t, newWorld(t), "x-n: &n ~\nrules: *n\n"); err != nil {
		t.Errorf("rules aliased to null should load: %v", err)
	}
	path := filepath.Join(t.TempDir(), "project.yaml")
	write(t, path, "x-s: &s keep it kind\nrules:\n  - *s\n")
	s, _, err := readSettings(path)
	if err != nil || len(s.Rules) != 1 || s.Rules[0] != "keep it kind" {
		t.Errorf("report rules: %v %v", s.Rules, err)
	}
	_, err = loadBindings(t, newWorld(t), "x-m: &m { a: b }\nrules:\n  - *m\n")
	if err == nil || !strings.Contains(err.Error(), "a rule must be a string") {
		t.Errorf("alias to a mapping should be refused: %v", err)
	}
}

func TestShippedTaskFrontmatterErrorNamesTrueLine(t *testing.T) {
	w, _ := shippedWorld(t, bridgeSettings+"locations:\n  api: {}\n", "")
	taskFile := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
	write(t, taskFile, "---\npipeline: coding\nlocations: [a\n---\nbody\n")
	_, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err == nil {
		t.Fatal("run should fail")
	}
	want := strings.Fields(err.Error()[strings.Index(err.Error(), "line "):])[1]
	err = cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "line "+want) {
		t.Errorf("reviewed should name the same line as a run (%q): %v", want, err)
	}
}

// otherOS is a platform key that is not this machine's, so the tests hold on every platform the suite runs on.
func otherOS() string {
	for _, g := range []string{"linux", "windows", "darwin"} {
		if g != runtime.GOOS {
			return g
		}
	}
	return ""
}

// invalidBindingCases are binding values a run refuses, each with text its error holds. The report must refuse every one too.
func invalidBindingCases() []struct{ name, value, want string } {
	o, g := otherOS(), runtime.GOOS
	return []struct{ name, value, want string }{
		{"scalar", "plain", "a binding must be a mapping"},
		{"null", "null", "a binding must be a mapping"},
		{"list", "[a, b]", "a binding must be a mapping"},
		{"unknown-key", "{ cmd: ./x.sh }", `unknown binding key "cmd"`},
		{"plain-maybe", "{ command: ./x.sh, plain: maybe }", "plain must be true or false"},
		{"timeout-soon", "{ command: ./x.sh, timeout: soon }", "timeout must be a positive duration"},
		{"timeout-zero", "{ command: ./x.sh, timeout: 0s }", "timeout must be a positive duration"},
		{"timeout-negative", "{ command: ./x.sh, timeout: -5m }", "timeout must be a positive duration"},
		{"plain-without-command", "{ plain: true }", "a plain binding needs a command"},
		{"paths-and-command", "{ paths: [docs], command: ./x.sh }", "cannot also have a command or guidance"},
		{"paths-not-a-list", "{ paths: docs }", "paths must be a list of strings"},
		{"only-another-platform-nested", "{ " + g + ": { " + o + ": { command: ./x.sh } } }", "has platform variants but none for " + g},
		{"bad-value-under-this-platform", "{ " + g + ": { timeout: soon } }", "timeout must be a positive duration"},
	}
}

func TestBindingsInvalidValuesRefusedOnBothPaths(t *testing.T) {
	for _, tc := range invalidBindingCases() {
		body := "bindings:\n  build: " + tc.value + "\n"
		if _, err := loadBindings(t, newWorld(t), body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: run: %v, want %q", tc.name, err, tc.want)
		}
		if _, err := reportBindings(t, body); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "line 2: ") {
			t.Errorf("%s: report: %v, want line 2 and %q", tc.name, err, tc.want)
		}
	}
}

func TestBindingsValidValuesAcceptedOnBothPaths(t *testing.T) {
	for name, value := range map[string]string{
		"command":        "{ command: ./x.sh }",
		"guidance":       "{ guidance: look at it }",
		"not-applicable": "{ not-applicable: no build }",
		"paths":          "{ paths: [docs] }",
		"plain":          "{ command: ./x.sh, plain: true }",
		"timeout":        "{ command: ./x.sh, timeout: 5m }",
		"this-platform":  "{ " + runtime.GOOS + ": { command: ./x.sh } }",
	} {
		body := "bindings:\n  build: " + value + "\n"
		if _, err := loadBindings(t, newWorld(t), body); err != nil {
			t.Errorf("%s: run: %v", name, err)
		}
		if _, err := reportBindings(t, body); err != nil {
			t.Errorf("%s: report: %v", name, err)
		}
	}
}

func TestBindingsOnlyAnotherPlatformIsComparedByTheReport(t *testing.T) {
	body := "bindings:\n  build: { " + otherOS() + ": { command: ./x.sh } }\n"
	m, err := reportBindings(t, body)
	if err != nil || m["build"] == nil {
		t.Errorf("report should compare a variant missing only for this machine: %v %v", m, err)
	}
	if _, err := loadBindings(t, newWorld(t), body); err == nil || !strings.Contains(err.Error(), "has platform variants but none for "+runtime.GOOS) {
		t.Errorf("a run on this machine should refuse it: %v", err)
	}
}

// A run reads only this machine's variant, and the report checks no more than a run does.
func TestBindingsBadOtherPlatformVariantAcceptedOnBothPaths(t *testing.T) {
	body := "bindings:\n  build: { " + runtime.GOOS + ": { command: ./x.sh }, " + otherOS() + ": { timeout: soon } }\n"
	if _, err := loadBindings(t, newWorld(t), body); err != nil {
		t.Errorf("a run on this machine reads only its own variant: %v", err)
	}
	if _, err := reportBindings(t, body); err != nil {
		t.Errorf("the report should not look at another platform's key: %v", err)
	}
}

// A variant for this machine that is itself only a variant for another platform is unusable, though another platform's key sits beside it.
func TestBindingsNestedOtherPlatformVariantRefusedOnBothPaths(t *testing.T) {
	g, o := runtime.GOOS, otherOS()
	body := "bindings:\n  build: { " + g + ": { " + o + ": { command: x } }, " + o + ": { command: y } }\n"
	want := "has platform variants but none for " + g
	if _, err := loadBindings(t, newWorld(t), body); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("run: %v", err)
	}
	if _, err := reportBindings(t, body); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("report: %v", err)
	}
}

// With a platform key beside an unknown key the refusal is always the unknown key, whatever the order the keys are visited in.
func TestBindingsPlatformKeyBesideUnknownKeyIsDeterministic(t *testing.T) {
	body := "bindings:\n  build: { " + otherOS() + ": { command: ./x.sh }, bogus: 1 }\n"
	for i := 0; i < 50; i++ {
		if _, err := loadBindings(t, newWorld(t), body); err == nil || !strings.Contains(err.Error(), `unknown binding key "bogus"`) {
			t.Fatalf("run: %v", err)
		}
		if _, err := reportBindings(t, body); err == nil || !strings.Contains(err.Error(), `unknown binding key "bogus"`) {
			t.Fatalf("report: %v", err)
		}
	}
}

func TestBindingsMergedInvalidValueNamesTheTrueLine(t *testing.T) {
	_, err := reportBindings(t, "x-m: &m\n  build: plain\nbindings:\n  <<: *m\n")
	if err == nil || !strings.Contains(err.Error(), "line 2: ") {
		t.Errorf("a merged entry should be named by the line of its key: %v", err)
	}
}

func TestShippedInvalidBindingSaysItCannotCompare(t *testing.T) {
	bad := "bindings:\n  build: { command: ./build.sh }\n  test: plain\n"
	w, _ := shippedWorld(t, bridgeSettings, bad)
	got := openShipped(t, w)
	if !strings.Contains(got, "shipped    can't compare:") || !strings.Contains(got, ".starship") || !strings.Contains(got, "project.yaml: line 3: ") {
		t.Errorf("an invalid shipped binding should say it can't compare:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{}); err == nil {
		t.Error("project reviewed should fail on an invalid shipped file")
	}
	if _, err := os.Stat(reviewedFile(w.bridge, "supernova")); err == nil {
		t.Error("project reviewed should write nothing for an invalid shipped file")
	}

	w, _ = shippedWorld(t, "platforms: [linux]\nbindings:\n  build: { command: ./build.sh }\n  test: plain\n", bridgeSettings)
	got = openShipped(t, w)
	if !strings.Contains(got, "shipped    can't compare:") || !strings.Contains(got, "projects"+string(filepath.Separator)+"supernova"+string(filepath.Separator)+"project.yaml: line 4: ") {
		t.Errorf("an invalid binding in the bridge's project.yaml should say it can't compare:\n%s", got)
	}
}

func TestShippedInvalidLocationOverlaySaysItCannotCompare(t *testing.T) {
	w := locationsWorld(t)
	body := strings.Replace(locationsProject, `test: { command: "./client-test.sh" }`, "test: plain", 1)
	line := 1 + strings.Count(body[:strings.Index(body, "test: plain")], "\n")
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), body)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", locShippedClient)
	locShipped(t, w, "server", locShippedServer)
	got := openLocShipped(t, w, task)
	if !strings.Contains(got, "shipped    location client: can't compare:") || !strings.Contains(got, fmt.Sprintf("project.yaml: line %d: ", line)) {
		t.Errorf("an invalid location overlay should say it can't compare, naming line %d:\n%s", line, got)
	}
}

// The reviewed record is only compared for equality with shipped values, which are checked first, so an invalid recorded value is not refused.
func TestShippedReviewedRecordIsNotChecked(t *testing.T) {
	changed := strings.Replace(bridgeSettings, "./test.sh", "./other.sh", 1)
	w, _ := shippedWorld(t, changed, bridgeSettings)
	write(t, reviewedFile(w.bridge, "supernova"), "bindings:\n  build: plain\n  deploy: 7\n")
	got := openShipped(t, w)
	if strings.Contains(got, "can't compare") || !strings.Contains(got, "binding test: changed") {
		t.Errorf("an invalid recorded value should not blank the report:\n%s", got)
	}

	w = locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", strings.Replace(locShippedClient, "./client-test.sh", "./other.sh", 1))
	locShipped(t, w, "server", locShippedServer)
	write(t, reviewedFile(w.bridge, "supernova"), "locations:\n  client:\n    bindings:\n      build: plain\n")
	got = openLocShipped(t, w, task)
	if strings.Contains(got, "can't compare") || !strings.Contains(got, "binding test: changed") {
		t.Errorf("an invalid recorded location value should not blank the report:\n%s", got)
	}
}

func shippedDocs(extra, docs string) string {
	return "bindings:\n  build: { command: \"./build.sh\" }\n  test: { command: \"./" + extra + ".sh\" }\n  docs: " + docs + "\n"
}

func TestShippedDocsAValueARunRefusesIsReported(t *testing.T) {
	cases := map[string]string{
		"command":  "{ command: ./gen.sh }",
		"guidance": "{ guidance: write them up }",
		"bad path": "{ paths: [../x] }",
	}
	for name, docs := range cases {
		t.Run(name, func(t *testing.T) {
			w, ws := shippedWorld(t, bridgeSettings, shippedDocs("other", docs))
			got := openShipped(t, w)
			if strings.Contains(got, "can't compare") || !strings.Contains(got, "binding docs: a run would refuse this: binding @docs") || strings.Contains(got, "binding docs: changed") || strings.Contains(got, "binding docs: not in") {
				t.Errorf("a docs value a run would refuse should say so:\n%s", got)
			}
			if !strings.Contains(got, "binding test: changed") {
				t.Errorf("other drift should still be listed:\n%s", got)
			}
			// a reviewed record holding the value does not silence it
			write(t, reviewedFile(w.bridge, "supernova"), "bindings:\n  docs: "+docs+"\n  test: { command: ./other.sh }\n")
			got = openShipped(t, w)
			if !strings.Contains(got, "binding docs: a run would refuse this:") || strings.Contains(got, "binding test:") {
				t.Errorf("the reviewed record should silence the other drift only:\n%s", got)
			}
			// the bridge holding the same value is not drift
			write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), "platforms: [linux]\n"+shippedDocs("other", docs))
			if got := openShipped(t, w); strings.Contains(got, "binding docs") {
				t.Errorf("a value equal to the bridge's is not drift:\n%s", got)
			}
			write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), bridgeSettings)
			os.Remove(reviewedFile(w.bridge, "supernova"))
			err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), filepath.Join(ws, ".starship", "project.yaml")) || !strings.Contains(err.Error(), "binding @docs") {
				t.Errorf("project reviewed should refuse, naming the file and the problem: %v", err)
			}
			if _, err := os.Stat(reviewedFile(w.bridge, "supernova")); err == nil {
				t.Error("project reviewed should write nothing")
			}
		})
	}
}

func TestShippedDocsRefusedTrailerSaysToFixFirst(t *testing.T) {
	const fix = "Fix the docs binding in the code's .starship/project.yaml first"
	const review = "ensign project reviewed supernova"
	w, _ := shippedWorld(t, bridgeSettings, shippedDocs("other", "{ command: ./gen.sh }"))
	got := openShipped(t, w)
	if !strings.Contains(got, fix) || strings.Contains(got, "Review these") || strings.Contains(got, review) {
		t.Errorf("the refused report should say to fix docs first:\n%s", got)
	}
	// the bridge holding the same bad value leaves no refused line, but project reviewed still refuses
	write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), "platforms: [linux]\n"+shippedDocs("test", "{ command: ./gen.sh }"))
	got = openShipped(t, w)
	if !strings.Contains(got, "binding test: changed") || strings.Contains(got, "a run would refuse") || !strings.Contains(got, fix) || strings.Contains(got, review) {
		t.Errorf("a bad docs equal to the bridge's plus other drift should say to fix docs first:\n%s", got)
	}

	w = locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", locShippedClient)
	locShipped(t, w, "server", locShippedServer+"  docs: { command: ./gen.sh }\n")
	got = openLocShipped(t, w, task)
	if !strings.Contains(got, fix) || strings.Contains(got, "Review these") || strings.Contains(got, "--location") {
		t.Errorf("the location's refused report should say to fix docs first:\n%s", got)
	}
}

func TestShippedOrdinaryDriftKeepsReviewTrailer(t *testing.T) {
	w, _ := shippedWorld(t, bridgeSettings, shippedDocs("other", "{ paths: [docs] }"))
	got := openShipped(t, w)
	if !strings.Contains(got, "Review these with the user, then run `ensign project reviewed supernova` to mark them reviewed.") || strings.Contains(got, "Fix the docs binding") {
		t.Errorf("ordinary drift should keep the review trailer:\n%s", got)
	}
}

func TestShippedDocsAValueARunRefusesBlocksReviewedAtLocations(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "both", "client,server")
	locShipped(t, w, "client", locShippedClient)
	locShipped(t, w, "server", locShippedServer+"  docs: { command: ./gen.sh }\n")
	got := openLocShipped(t, w, task)
	if strings.Contains(got, "can't compare") || !strings.Contains(got, "binding docs: a run would refuse this:") {
		t.Errorf("the location's docs value should be reported as refused:\n%s", got)
	}
	err := cmdProject([]string{"reviewed", task}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "binding @docs") {
		t.Errorf("project reviewed should refuse: %v", err)
	}
	if _, err := os.Stat(reviewedFile(w.bridge, "supernova")); err == nil {
		t.Error("nothing should be written when any location has the problem")
	}
}

func TestShippedDocsPathsAndNotApplicableAreOrdinaryDrift(t *testing.T) {
	for _, docs := range []string{"{ paths: [docs] }", `{ not-applicable: "no docs" }`} {
		w, _ := shippedWorld(t, bridgeSettings, shippedDocs("test", docs))
		got := openShipped(t, w)
		if !strings.Contains(got, "binding docs: not in the bridge's copy") || strings.Contains(got, "refuse") {
			t.Errorf("%s should be ordinary drift:\n%s", docs, got)
		}
		if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if got := openShipped(t, w); strings.Contains(got, "shipped") {
			t.Errorf("recording should silence %s:\n%s", docs, got)
		}
	}
}

func TestShippedDocsOnlyAnotherPlatformsVariantIsNotFlagged(t *testing.T) {
	docs := "{ " + otherOS() + ": { command: ./gen.sh } }"
	w, _ := shippedWorld(t, bridgeSettings, shippedDocs("test", docs))
	got := openShipped(t, w)
	if strings.Contains(got, "refuse") || strings.Contains(got, "can't compare") {
		t.Errorf("another platform's variant should not be flagged:\n%s", got)
	}
	if err := cmdProject([]string{"reviewed", "supernova/auth-refactor"}, &bytes.Buffer{}); err != nil {
		t.Error(err)
	}
}
