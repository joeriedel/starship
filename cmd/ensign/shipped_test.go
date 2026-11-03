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

// openLocShipped sets each location's shipped file and opens a task with both locations.
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
