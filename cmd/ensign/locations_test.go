// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const locationsProject = `platforms: [linux]

bindings:
  build: { command: "./build.sh" }
  test:  { command: "./test.sh" }
  lint:  { not-applicable: "no linter" }
  apply-dependency: { not-applicable: "these sides build from the sibling checkout" }

locations:
  models:
    bindings:
      test: { command: "./models-test.sh" }
  client:
    depends-on: [models]
    bindings:
      test: { command: "./client-test.sh" }
  server:
    depends-on: [models]

cross-location:
  bindings:
    test: { command: "./cross-test.sh" }
`

// locationsWorld is a fixture bridge whose supernova project declares locations, with a folder recorded for each.
func locationsWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), locationsProject)
	for _, l := range []string{"client", "server", "models"} {
		dir := filepath.Join(t.TempDir(), l)
		os.MkdirAll(dir, 0o755)
		if _, err := setLocationPath(w.bridge, "supernova", l, dir); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func locTask(t *testing.T, w *world, name, locations string) string {
	t.Helper()
	front := "pipeline: coding\n"
	if locations != "" {
		front += "locations: [" + locations + "]\n"
	}
	write(t, filepath.Join(w.bridge, "projects", "supernova", "tasks", name, "task.md"), "---\n"+front+"---\n\n# "+name+"\n\n## Goal\n\nDo it.\n")
	return "supernova/" + name
}

func TestLocationsParseAndDependencies(t *testing.T) {
	w := locationsWorld(t)
	l, err := loadLocations(w.bridge, "supernova")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Names(); !reflect.DeepEqual(got, []string{"client", "models", "server"}) {
		t.Errorf("Names = %v", got)
	}
	if got := l.DependsOn("client"); !reflect.DeepEqual(got, []string{"models"}) {
		t.Errorf("DependsOn(client) = %v", got)
	}
	if got := l.Dependents("models"); !reflect.DeepEqual(got, []string{"client", "server"}) {
		t.Errorf("Dependents(models) = %v", got)
	}
	if got := l.Dependents("client"); len(got) != 0 {
		t.Errorf("Dependents(client) = %v", got)
	}
	if l.Cross["test"].Command != "./cross-test.sh" {
		t.Errorf("cross-location bindings = %v", l.Cross)
	}
}

func TestLocationsTransitiveDependents(t *testing.T) {
	w := locationsWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"),
		"locations:\n  models: {}\n  client:\n    depends-on: [server]\n  server:\n    depends-on: [models]\n")
	l, err := loadLocations(w.bridge, "supernova")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Dependents("models"); !reflect.DeepEqual(got, []string{"client", "server"}) {
		t.Errorf("Dependents(models) = %v", got)
	}
}

func TestLocationsValidation(t *testing.T) {
	w := newWorld(t)
	for name, c := range map[string]struct{ yaml, want string }{
		"unknown": {"locations:\n  client:\n    depends-on: [nope]\n", `"nope", which is not a declared location`},
		"self":    {"locations:\n  client:\n    depends-on: [client]\n", "client depends on itself"},
		"cycle":   {"locations:\n  a:\n    depends-on: [b]\n  b:\n    depends-on: [a]\n", "cycle: a -> b -> a"},
		"name":    {"locations:\n  Client: {}\n", `location "Client"`},
		"binding": {"locations:\n  client:\n    bindings:\n      test: { bogus: 1 }\n", "unknown binding key"},
	} {
		write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), c.yaml)
		_, err := loadLocations(w.bridge, "supernova")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", name, err, c.want)
		}
	}
}

func TestMachineLocationsRoundTrip(t *testing.T) {
	w := locationsWorld(t)
	m := loadMachine(w.bridge)
	if len(m.Workspaces) != 1 || len(m.Locations["supernova"]) != 3 {
		t.Fatalf("machine = %+v", m)
	}
	data, _ := os.ReadFile(machineFile(w.bridge))
	text := string(data)
	if !strings.Contains(text, "\nlocations:\n  supernova:\n    client: ") || strings.Index(text, "workspaces:") > strings.Index(text, "locations:") {
		t.Errorf("machine.yaml:\n%s", text)
	}
	if strings.Index(text, "client:") > strings.Index(text, "models:") || strings.Index(text, "models:") > strings.Index(text, "server:") {
		t.Errorf("locations not sorted:\n%s", text)
	}
	p, err := locationPathFor(w.bridge, "supernova", "client")
	if err != nil || filepath.Base(p) != "client" {
		t.Errorf("locationPathFor = %q, %v", p, err)
	}
	_, err = locationPathFor(w.bridge, "supernova", "other")
	if err == nil || !strings.Contains(err.Error(), "ensign workspace supernova --location other <path>") {
		t.Errorf("missing location error: %v", err)
	}
	if err := saveMachine(w.bridge, Machine{Workspaces: map[string]string{"a": "/x"}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(machineFile(w.bridge))
	if strings.Contains(string(data), "locations:") {
		t.Errorf("empty locations should be omitted:\n%s", data)
	}
}

func TestWorkspaceLocationCommand(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), locationsProject)
	dir := t.TempDir()
	var out bytes.Buffer
	if err := cmdWorkspace([]string{"supernova", "--location", "client", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "client is at "+filepath.ToSlash(dir)) {
		t.Errorf("output %q", out.String())
	}
	out.Reset()
	if err := cmdWorkspace([]string{"supernova", "--location", "client"}, &out); err != nil || strings.TrimSpace(out.String()) != filepath.ToSlash(dir) {
		t.Errorf("show: %q, %v", out.String(), err)
	}
	if err := cmdWorkspace([]string{"supernova", "--location", "server"}, &out); err == nil || !strings.Contains(err.Error(), "no folder recorded") {
		t.Errorf("unrecorded: %v", err)
	}
	if err := cmdWorkspace([]string{"supernova", "--location", "nope", dir}, &out); err == nil || !strings.Contains(err.Error(), "declares no location") {
		t.Errorf("undeclared: %v", err)
	}
	if err := cmdWorkspace([]string{"supernova/auth-refactor", "--location", "client", dir}, &out); err == nil || !strings.Contains(err.Error(), "--location belongs to a project") {
		t.Errorf("task: %v", err)
	}
	if err := cmdWorkspace([]string{"supernova", "--location", "client", filepath.Join(dir, "missing")}, &out); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("not a folder: %v", err)
	}
}

func TestTaskNewLocations(t *testing.T) {
	w := locationsWorld(t)
	var out bytes.Buffer
	for _, args := range [][]string{
		{"new", "supernova/a"},
		{"new", "supernova/a", "--locations", "nope"},
		{"new", "supernova/a", "--workspace", t.TempDir()},
	} {
		if err := cmdTask(args, &out); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
	if err := cmdTask([]string{"new", "supernova/a", "--locations", "client,models"}, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects", "supernova", "tasks", "a", "task.md"))
	if !strings.Contains(string(data), "locations: [client, models]\n") {
		t.Errorf("task.md:\n%s", data)
	}
	m := loadMachine(w.bridge)
	delete(m.Locations["supernova"], "server")
	saveMachine(w.bridge, m)
	if err := cmdTask([]string{"new", "supernova/b", "--locations", "server"}, &out); err == nil || !strings.Contains(err.Error(), "ensign workspace supernova --location server") {
		t.Errorf("unrecorded location: %v", err)
	}
	// A project without locations refuses --locations.
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), "platforms: [linux]\n")
	if err := cmdTask([]string{"new", "supernova/c", "--locations", "client"}, &out); err == nil || !strings.Contains(err.Error(), "declares no locations") {
		t.Errorf("locationless: %v", err)
	}
}

func TestRunPicksOneLocation(t *testing.T) {
	w := locationsWorld(t)
	one := locTask(t, w, "one", "client")
	two := locTask(t, w, "two", "client, models")
	none := locTask(t, w, "none", "")
	bad := locTask(t, w, "bad", "nope")
	var out bytes.Buffer

	snap, err := createRun(w.bridge, runOpts{task: one}, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	clientPath, _ := locationPathFor(w.bridge, "supernova", "client")
	if snap.Location != "client" || snap.Workspace != clientPath {
		t.Errorf("snapshot location %q workspace %q, want client at %q", snap.Location, snap.Workspace, clientPath)
	}
	if got := snap.Bindings["test"].Command; got != "./client-test.sh" {
		t.Errorf("test binding = %q, want the location's", got)
	}
	if got := snap.Bindings["build"].Command; got != "./build.sh" {
		t.Errorf("build binding = %q, want the project's", got)
	}
	if !strings.Contains(out.String(), "location   client") {
		t.Errorf("summary lacks the location:\n%s", out.String())
	}
	if folders := runCodeFolders(&RunFiles{Snap: snap}); !reflect.DeepEqual(folders, []string{clientPath}) {
		t.Errorf("runCodeFolders = %v", folders)
	}

	// The task's own bindings come after the location's.
	write(t, filepath.Join(w.bridge, "projects", "supernova", "tasks", "one", "task.md"),
		"---\npipeline: coding\nlocations: [client]\nbindings:\n  test: { command: \"./task-test.sh\" }\n---\n\n## Goal\n\nDo it.\n")
	if snap, err = createRun(w.bridge, runOpts{task: one}, true, &out); err != nil || snap.Bindings["test"].Command != "./task-test.sh" {
		t.Errorf("task binding: %v, %+v", err, snap)
	}

	if _, err := createRun(w.bridge, runOpts{task: two}, true, &out); err == nil || !strings.Contains(err.Error(), "--location") {
		t.Errorf("several locations without --location: %v", err)
	}
	snap, err = createRun(w.bridge, runOpts{task: two, location: "models"}, true, &out)
	if err != nil || snap.Location != "models" || snap.Bindings["test"].Command != "./models-test.sh" {
		t.Errorf("models run: %v, %+v", err, snap)
	}
	if _, err := createRun(w.bridge, runOpts{task: two, location: "server"}, true, &out); err == nil || !strings.Contains(err.Error(), "does not use location") {
		t.Errorf("location outside the task: %v", err)
	}
	if _, err := createRun(w.bridge, runOpts{task: none}, true, &out); err == nil || !strings.Contains(err.Error(), "must name the locations") {
		t.Errorf("task without locations: %v", err)
	}
	if _, err := createRun(w.bridge, runOpts{task: bad}, true, &out); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("undeclared location: %v", err)
	}
	m := loadMachine(w.bridge)
	delete(m.Locations["supernova"], "client")
	saveMachine(w.bridge, m)
	if _, err := createRun(w.bridge, runOpts{task: one}, true, &out); err == nil || !strings.Contains(err.Error(), "ensign workspace supernova --location client") {
		t.Errorf("unrecorded path: %v", err)
	}
}

func TestRunLocationOnLocationlessProject(t *testing.T) {
	w := newWorld(t)
	var out bytes.Buffer
	if _, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor", location: "client"}, true, &out); err == nil || !strings.Contains(err.Error(), "declares no locations") {
		t.Errorf("--location on a locationless project: %v", err)
	}
	write(t, filepath.Join(w.bridge, "projects", "supernova", "tasks", "x", "task.md"), "---\nlocations: [client]\n---\n\n## Goal\n\nx\n")
	if _, err := createRun(w.bridge, runOpts{task: "supernova/x"}, true, &out); err == nil || !strings.Contains(err.Error(), "declares none") {
		t.Errorf("task naming locations: %v", err)
	}
	snap, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor"}, true, &out)
	if err != nil || snap.Location != "" {
		t.Errorf("plain run: %v, %+v", err, snap)
	}
}

func TestParseRunArgsLocation(t *testing.T) {
	o, err := parseRunArgs("ensign run", []string{"supernova/x", "--location", "client"})
	if err != nil || o.location != "client" || o.task != "supernova/x" {
		t.Errorf("%+v, %v", o, err)
	}
	if _, err := parseRunArgs("ensign run", []string{"supernova/x", "--location"}); err == nil {
		t.Error("--location without a value should fail")
	}
}

func TestLoadConfigUsesTheRunsLocation(t *testing.T) {
	w := locationsWorld(t)
	two := locTask(t, w, "two", "client, models")
	var out bytes.Buffer
	if _, err := loadConfig(w.bridge, two); err == nil {
		t.Error("a several-location task with no run should need a location")
	}
	snap, err := createRun(w.bridge, runOpts{task: two, location: "models"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	setRunStatus(t, w, snap.Run, "success")
	c, err := loadConfig(w.bridge, two)
	if err != nil || c.Location != "models" || filepath.Base(c.Workspace) != "models" {
		t.Errorf("loadConfig after a models run: %v, %+v", err, c)
	}
}

func setRunStatus(t *testing.T, w *world, run, status string) {
	t.Helper()
	r, err := readRun(w.bridge, run)
	if err != nil {
		t.Fatal(err)
	}
	r.State.Status = status
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
}

func TestLandingLocationResolution(t *testing.T) {
	w := locationsWorld(t)
	two := locTask(t, w, "land", "client, server")
	var out bytes.Buffer
	client, err := createRun(w.bridge, runOpts{task: two, location: "client"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	setRunStatus(t, w, client.Run, "success")
	server, err := createRun(w.bridge, runOpts{task: two, location: "server"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "running", "error"} {
		setRunStatus(t, w, server.Run, status)
		c, err := loadConfig(w.bridge, two)
		if err != nil || c.Location != "client" {
			t.Errorf("with the newer server run %s, landing resolved %v, %v; want client", status, c, err)
		}
	}
	c, err := loadConfigAt(w.bridge, two, "server")
	if err != nil || c.Location != "server" {
		t.Errorf("--location server: %v, %v", err, c)
	}
	if _, err := loadConfigAt(w.bridge, two, "models"); err == nil {
		t.Error("--location outside the task's locations should fail")
	}
	setRunStatus(t, w, server.Run, "success")
	if c, err := loadConfig(w.bridge, two); err != nil || c.Location != "server" {
		t.Errorf("newest successful run is server: %v, %v", err, c)
	}
	// With no successful run, a sole location is taken and several are refused.
	one := locTask(t, w, "solo", "server")
	if c, err := loadConfig(w.bridge, one); err != nil || c.Location != "server" {
		t.Errorf("sole location: %v, %v", err, c)
	}
	other := locTask(t, w, "none", "client, server")
	if _, err := createRun(w.bridge, runOpts{task: other, location: "client"}, true, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(w.bridge, other); err == nil || !strings.Contains(err.Error(), "--location") {
		t.Errorf("no successful run and several locations should name --location, got %v", err)
	}
}

func TestTaskStateFilesPerLocation(t *testing.T) {
	b := "b"
	if got, want := taskBaseFile(b, "p/t", ""), filepath.Join(b, ".starship", "tasks", "p--t.base"); got != want {
		t.Errorf("locationless base file %s, want %s", got, want)
	}
	if got, want := testedFile(b, "p/t", ""), filepath.Join(b, ".starship", "tasks", "p--t.tested"); got != want {
		t.Errorf("locationless tested file %s, want %s", got, want)
	}
	if taskBaseFile(b, "p/t", "client") == taskBaseFile(b, "p/t", "server") || testedFile(b, "p/t", "client") == testedFile(b, "p/t", "server") {
		t.Error("base and tested files should differ per location")
	}
	if taskBaseFile(b, "p/t", "client") == taskBaseFile(b, "p/t", "") {
		t.Error("a located file should not reuse the locationless name")
	}
}

func TestLandingRanksSuccessByFinishTime(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "rank", "client, server")
	var out bytes.Buffer
	client, err := createRun(w.bridge, runOpts{task: task, location: "client"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	server, err := createRun(w.bridge, runOpts{task: task, location: "server"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	finish := func(run string, at int64) {
		r, err := readRun(w.bridge, run)
		if err != nil {
			t.Fatal(err)
		}
		r.State.Status, r.State.FinishedAt = "success", at
		if err := r.save(); err != nil {
			t.Fatal(err)
		}
	}
	// The server run was created later but finished first.
	finish(client.Run, 200)
	finish(server.Run, 100)
	if got := latestRunLocation(w.bridge, task, true); got != "client" {
		t.Errorf("newest finish is client, got %q", got)
	}
	finish(server.Run, 300)
	if got := latestRunLocation(w.bridge, task, true); got != "server" {
		t.Errorf("newest finish is server, got %q", got)
	}
}

func TestPublishStepUsesThePrecedingRunsLocation(t *testing.T) {
	w := locationsWorld(t)
	task := locTask(t, w, "retry", "client, server")
	var out bytes.Buffer
	c1, err := createRun(w.bridge, runOpts{task: task, location: "client"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := createRun(w.bridge, runOpts{task: task, location: "server"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	// Client's retry C2 is created after the queued server run.
	c2, err := createRun(w.bridge, runOpts{task: task, location: "client"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	seq := &sequence{Steps: []*seqStep{
		{N: 1, Kind: "run", Task: task, Run: c2.Run, Prev: c1.Run},
		{N: 2, Kind: "publish", Task: task},
		{N: 3, Kind: "run", Task: task, Run: s1.Run},
		{N: 4, Kind: "publish", Task: task},
	}}
	if got := precedingRunLocation(w.bridge, seq, 2, task); got != "client" {
		t.Errorf("step 2 location %q, want client", got)
	}
	if got := precedingRunLocation(w.bridge, seq, 4, task); got != "server" {
		t.Errorf("step 4 location %q, want server", got)
	}
	if got := precedingRunLocation(w.bridge, seq, 1, task); got != "" {
		t.Errorf("no preceding run step should give no location, got %q", got)
	}
}

func sideNamesOf(s *Snapshot) []string { return sideNames(s.Sides) }

func TestRunSidesDefaultAndNarrowed(t *testing.T) {
	w := locationsWorld(t)
	var out bytes.Buffer
	models := locTask(t, w, "m", "models")
	client := locTask(t, w, "c", "client")
	snap, err := createRun(w.bridge, runOpts{task: models}, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got := sideNamesOf(snap); !reflect.DeepEqual(got, []string{"models", "client", "server"}) || snap.Narrowed {
		t.Errorf("default sides = %v, narrowed %v", got, snap.Narrowed)
	}
	if snap.Cross["test"].Command != "./cross-test.sh" {
		t.Errorf("cross = %v", snap.Cross)
	}
	if got := snap.Sides[1].Bindings["test"].Command; got != "./client-test.sh" {
		t.Errorf("client side test = %q", got)
	}
	if got := snap.Sides[2].Bindings["test"].Command; got != "./test.sh" {
		t.Errorf("server side test = %q", got)
	}
	if want := loadMachine(w.bridge).Locations["supernova"]["client"]; filepath.ToSlash(snap.Sides[1].Workspace) != want {
		t.Errorf("client side folder = %q, want %q", snap.Sides[1].Workspace, want)
	}
	if !strings.Contains(out.String(), "sides      models, client, server (default: models and its dependents)") {
		t.Errorf("summary:\n%s", out.String())
	}

	out.Reset()
	snap, err = createRun(w.bridge, runOpts{task: models, sides: []string{"server", "models"}}, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got := sideNamesOf(snap); !reflect.DeepEqual(got, []string{"models", "server"}) || !snap.Narrowed {
		t.Errorf("narrowed sides = %v, narrowed %v", got, snap.Narrowed)
	}
	if !strings.Contains(out.String(), "sides      models, server (narrowed)") {
		t.Errorf("summary:\n%s", out.String())
	}

	snap, err = createRun(w.bridge, runOpts{task: client}, true, &out)
	if err != nil || !reflect.DeepEqual(sideNamesOf(snap), []string{"client"}) || snap.Narrowed || snap.Cross != nil {
		t.Errorf("client run: %v, %+v", err, snap)
	}
}

func TestRunSidesRefused(t *testing.T) {
	w := locationsWorld(t)
	var out bytes.Buffer
	models := locTask(t, w, "m", "models")
	for name, c := range map[string]struct {
		sides []string
		want  string
	}{
		"missing edited": {[]string{"client", "server"}, "must include models"},
		"undeclared":     {[]string{"models", "nope"}, "declares no location"},
	} {
		if _, err := createRun(w.bridge, runOpts{task: models, sides: c.sides}, true, &out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	client := locTask(t, w, "c", "client")
	if _, err := createRun(w.bridge, runOpts{task: client, sides: []string{"client", "server"}}, true, &out); err == nil || !strings.Contains(err.Error(), "not affected") {
		t.Errorf("unaffected side: %v", err)
	}
	m := loadMachine(w.bridge)
	delete(m.Locations["supernova"], "server")
	saveMachine(w.bridge, m)
	if _, err := createRun(w.bridge, runOpts{task: models}, true, &out); err == nil || !strings.Contains(err.Error(), "ensign workspace supernova --location server") {
		t.Errorf("unrecorded side folder: %v", err)
	}
	if _, err := createRun(w.bridge, runOpts{task: models, sides: []string{"models", "client"}}, true, &out); err != nil {
		t.Errorf("narrowing away the unrecorded side: %v", err)
	}

	w = newWorld(t)
	if _, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor", sides: []string{"client"}}, true, &out); err == nil || !strings.Contains(err.Error(), "--sides") {
		t.Errorf("--sides on a locationless project: %v", err)
	}
	if snap, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor"}, true, &out); err != nil || len(snap.Sides) != 0 || strings.Contains(out.String(), "sides ") {
		t.Errorf("locationless run: %v, %+v", err, snap)
	}
}

func TestParseRunArgsSides(t *testing.T) {
	o, err := parseRunArgs("ensign run", []string{"supernova/x", "--sides", "models, server"})
	if err != nil || !reflect.DeepEqual(o.sides, []string{"models", "server"}) {
		t.Errorf("%+v, %v", o, err)
	}
	if _, err := parseRunArgs("ensign run", []string{"supernova/x", "--sides"}); err == nil {
		t.Error("--sides without a value should fail")
	}
}

func TestTesterPromptListsSides(t *testing.T) {
	w := locationsWorld(t)
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: locTask(t, w, "m", "models")}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	r := &RunFiles{Dir: filepath.Join(w.bridge, "run"), Snap: snap, State: &State{Run: snap.Run}}
	tester, _, _ := snap.Pipeline.find("test")
	if tester == nil {
		t.Fatal("no test stage")
	}
	prompt := stagePrompt(r, &Dispatch{ID: "d1", Stage: "test"}, tester, "brief", nil)
	client := loadMachine(w.bridge).Locations["supernova"]["client"]
	for _, want := range []string{"side models: ", "side client: " + filepath.FromSlash(client), "side server: ",
		"- @test: `cd " + shellQuote(filepath.FromSlash(client)) + " && ./client-test.sh`", "cross-location test", "./cross-test.sh"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("tester prompt lacks %q:\n%s", want, prompt)
		}
	}
	single, err := createRun(w.bridge, runOpts{task: locTask(t, w, "c", "client")}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	r.Snap = single
	if p := stagePrompt(r, &Dispatch{ID: "d1", Stage: "test"}, tester, "brief", nil); strings.Contains(p, "## Sides") {
		t.Errorf("a single side needs no sides section")
	}
}

func TestSideBindingsLeaveOutTheTaskLayer(t *testing.T) {
	w := locationsWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "tasks", "m", "task.md"),
		"---\npipeline: coding\nlocations: [models]\nbindings:\n  test: { command: \"go test ./schema/...\" }\n---\n\n# m\n\n## Goal\n\nDo it.\n")
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: "supernova/m"}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Sides[0].Bindings["test"].Command; got != "go test ./schema/..." {
		t.Errorf("models side test = %q", got)
	}
	if got := snap.Sides[1].Bindings["test"].Command; got != "./client-test.sh" {
		t.Errorf("client side test = %q", got)
	}
	if got := snap.Sides[2].Bindings["test"].Command; got != "./test.sh" {
		t.Errorf("server side test = %q", got)
	}
}

func TestTesterPromptShowsGuidanceSideBindings(t *testing.T) {
	w := locationsWorld(t)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"),
		strings.Replace(locationsProject, `test: { command: "./client-test.sh" }`, `test: { guidance: "run the client suite by hand" }`, 1))
	var out bytes.Buffer
	snap, err := createRun(w.bridge, runOpts{task: locTask(t, w, "m", "models")}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	r := &RunFiles{Dir: filepath.Join(w.bridge, "run"), Snap: snap, State: &State{Run: snap.Run}}
	tester, _, _ := snap.Pipeline.find("test")
	prompt := stagePrompt(r, &Dispatch{ID: "d1", Stage: "test"}, tester, "brief", nil)
	if !strings.Contains(prompt, "- @test: run the client suite by hand") {
		t.Errorf("tester prompt lacks the guidance binding:\n%s", prompt)
	}
}
