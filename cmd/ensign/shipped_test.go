package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func loadBindings(t *testing.T, w *world, project string) (*Config, error) {
	t.Helper()
	write(t, filepath.Join(w.bridge, "projects/supernova/project.yaml"), "platforms: [linux]\n"+project)
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
