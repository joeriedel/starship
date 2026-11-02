// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const depApply = `  apply-dependency: { command: "echo $STARSHIP_DEPENDENCY $STARSHIP_DEPENDENCY_BRANCH $STARSHIP_DEPENDENCY_PATH > dep.env; echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"; echo dep.env >> \"$STARSHIP_APPLIED_FILES\"" }`

// needPOSIXShell skips when the binding shell is not POSIX: shellFor runs sh -c, or bash -c on Windows when bash is on PATH, else cmd /C.
func needPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("bash"); err != nil {
			t.Skip("no bash on PATH, so bindings run under cmd /C and the commands here are POSIX shell")
		}
	}
}

// newRepo is a git repository on main with go.mod committed.
func newRepo(t *testing.T, dir string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@example.com")
	runGit(t, dir, "config", "user.name", "t")
	write(t, filepath.Join(dir, "go.mod"), "module x\n")
	runGit(t, dir, "add", "go.mod")
	runGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// depWorld is a locations world whose three folders are git repositories and whose bindings are plain: build notes whether go.mod carries the wiring, and what apply-dependency is given is ours to choose.
func depWorld(t *testing.T, apply, clientTest string) *world {
	t.Helper()
	needPOSIXShell(t)
	w := locationsWorld(t)
	for _, l := range []string{"client", "server", "models"} {
		newRepo(t, w.sideFolder(l))
	}
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), `platforms: [linux]

bindings:
  build: { command: "grep -q replace go.mod && touch built-wired && cp dep.env seen.env; touch built", plain: true }
  test:  { command: "touch tested", plain: true }
  lint:  { not-applicable: "no linter" }
`+apply+`

locations:
  models: {}
  client:
    depends-on: [models]
    bindings:
      test: { command: "`+clientTest+`", plain: true }
  server:
    depends-on: [models]
`)
	return w
}

func (w *world) runModelsPlain() (string, error) {
	w.t.Helper()
	w.run(locTask(w.t, w, "m", "models"))
	w.approve()
	lead, why := w.spawn("", "", "orchestrator")
	if lead == "" {
		w.t.Fatalf("lead refused: %s", why)
	}
	w.toTestOneReview(lead)
	return w.runStage()
}

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertRestored(t *testing.T, folder string) {
	t.Helper()
	if got := readFile(t, folder, "go.mod"); got != "module x\n" {
		t.Errorf("%s: go.mod = %q, want it restored", folder, got)
	}
	if markerExists(folder, "dep.env") {
		t.Errorf("%s: dep.env should be deleted", folder)
	}
	rec, err := appliedRecordPath(folder)
	if err != nil || markerExists(rec) {
		t.Errorf("%s: the applied record should be gone (%v)", folder, err)
	}
}

func TestParseRunArgsDependency(t *testing.T) {
	o, err := parseRunArgs("ensign run", []string{"supernova/t", "--dependency", "models=feature-x", "--dependency", "client=y"})
	if err != nil || !reflect.DeepEqual(o.dependencies, []string{"models=feature-x", "client=y"}) {
		t.Fatalf("got %v, %v", o.dependencies, err)
	}
	for _, bad := range []string{"models", "=x", "models="} {
		if _, err := parseRunArgs("ensign run", []string{"supernova/t", "--dependency", bad}); err == nil || !strings.Contains(err.Error(), "<location>=<branch>") {
			t.Errorf("--dependency %q: %v", bad, err)
		}
	}
	if _, err := parseRunArgs("ensign run", []string{"supernova/t", "--dependency"}); err == nil {
		t.Error("--dependency without a value should be refused")
	}
}

func TestDependencyRefusals(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "feature-x")
	server := locTask(t, w, "s", "server")
	models := locTask(t, w, "m", "models")
	cases := []struct{ task, dep, want string }{
		{server, "nowhere=x", `declares no location "nowhere"`},
		{server, "server=x", "the location this run edits"},
		{models, "server=x", "models does not depend on server"},
		{server, "client=x", "server does not depend on client"},
		{server, "models=missing", `no branch "missing"`},
		{server, "models=feature-x", ""},
	}
	for _, c := range cases {
		var out bytes.Buffer
		_, err := createRun(w.bridge, runOpts{task: c.task, dependencies: []string{c.dep}, dry: true}, false, &out)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.dep, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.dep, err, c.want)
		}
	}
	var out bytes.Buffer
	_, err := createRun(w.bridge, runOpts{task: server, dependencies: []string{"models=feature-x", "models=feature-x"}, dry: true}, false, &out)
	if err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Errorf("a repeated location: %v", err)
	}
}

func TestRunRefusedForASideWithoutTheBinding(t *testing.T) {
	w := depWorld(t, "", "touch tested")
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "feature-x")
	var out bytes.Buffer
	_, err := createRun(w.bridge, runOpts{task: locTask(t, w, "s", "server"), dependencies: []string{"models=feature-x"}, dry: true}, false, &out)
	if err == nil || !strings.Contains(err.Error(), "server (needs the branch of models)") || !strings.Contains(err.Error(), "add @apply-dependency") || !strings.Contains(err.Error(), "not-applicable") {
		t.Fatalf("explicit: got %v", err)
	}
	_, err = createRun(w.bridge, runOpts{task: locTask(t, w, "m", "models"), dry: true}, false, &out)
	if err == nil || !strings.Contains(err.Error(), "client (needs the branch of models), server (needs the branch of models)") || !strings.Contains(err.Error(), "narrow --sides") {
		t.Fatalf("automatic: got %v", err)
	}
	if _, err := createRun(w.bridge, runOpts{task: locTask(t, w, "m2", "models"), sides: []string{"models"}, dry: true}, false, &out); err != nil {
		t.Errorf("narrowed to the edited side: %v", err)
	}
}

func TestSummaryNamesTheDependencies(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "feature-x")
	var out bytes.Buffer
	if _, err := createRun(w.bridge, runOpts{task: locTask(t, w, "s", "server"), dependencies: []string{"models=feature-x"}, dry: true}, false, &out); err != nil {
		t.Fatal(err)
	}
	if want := "dependency models @ feature-x (" + w.sideFolder("models") + ") applied on server\n"; !strings.Contains(out.String(), want) {
		t.Errorf("summary lacks %q:\n%s", want, out.String())
	}
	out.Reset()
	if _, err := createRun(w.bridge, runOpts{task: locTask(t, w, "m", "models"), dry: true}, false, &out); err != nil {
		t.Fatal(err)
	}
	if want := "dependency models @ this run's branch (" + w.sideFolder("models") + ") applied on client, server\n"; !strings.Contains(out.String(), want) {
		t.Errorf("summary lacks %q:\n%s", want, out.String())
	}
}

func TestRunAcceptedWithNotApplicable(t *testing.T) {
	w := depWorld(t, `  apply-dependency: { not-applicable: "builds from the sibling checkout" }`, "touch tested")
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "feature-x")
	var out bytes.Buffer
	if _, err := createRun(w.bridge, runOpts{task: locTask(t, w, "s", "server"), dependencies: []string{"models=feature-x"}, dry: true}, false, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := createRun(w.bridge, runOpts{task: locTask(t, w, "m", "models"), dry: true}, false, &out); err != nil {
		t.Fatal(err)
	}
}

func TestPlainStageAppliesBeforeBuildAndRestoresAfter(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	out, err := w.runModelsPlain()
	if err != nil || !strings.Contains(out, "Outcome: pass") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, l := range []string{"client", "server"} {
		f := w.sideFolder(l)
		if !markerExists(f, "built-wired") || !markerExists(f, "tested") {
			t.Errorf("side %s should have been built with the wiring and tested", l)
		}
		if got, want := readFile(t, f, "seen.env"), "models main "+w.sideFolder("models")+"\n"; got != want {
			t.Errorf("side %s saw %q, want %q", l, got, want)
		}
		assertRestored(t, f)
	}
	if markerExists(w.sideFolder("models"), "built-wired") {
		t.Error("the edited location needs no wiring")
	}
}

func TestPlainStageRestoresAfterAFailure(t *testing.T) {
	w := depWorld(t, depApply, "exit 1")
	out, err := w.runModelsPlain()
	if err != nil || !strings.Contains(out, "Outcome: fail") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !markerExists(w.sideFolder("client"), "built-wired") {
		t.Error("client should have been built with the wiring")
	}
	assertRestored(t, w.sideFolder("client"))
}

func TestPlainStageApplyFailureIsAnError(t *testing.T) {
	w := depWorld(t, `  apply-dependency: { command: "echo cannot wire; exit 1" }`, "touch tested")
	out, err := w.runModelsPlain()
	if err != nil || !strings.Contains(out, "Outcome: error") || !strings.Contains(out, "could not apply the dependency") || !strings.Contains(out, "cannot wire") {
		t.Fatalf("%v\n%s", err, out)
	}
	if markerExists(w.sideFolder("client"), "built") {
		t.Error("a side that could not be wired is not built")
	}
}

func TestPlainStageCrossTestSeesTheEditedWiring(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "feature-x")
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), `platforms: [linux]

bindings:
  build: { command: "touch built", plain: true }
  test:  { command: "touch tested", plain: true }
  lint:  { not-applicable: "no linter" }
`+depApply+`

locations:
  models: {}
  client:
    depends-on: [models]
  server:
    depends-on: [client]

cross-location:
  bindings:
    test: { command: "grep -q replace go.mod && touch cross-wired", plain: true }
`)
	w.run(locTask(t, w, "c", "client"), "--dependency", "models=feature-x")
	w.approve()
	lead, why := w.spawn("", "", "orchestrator")
	if lead == "" {
		t.Fatalf("lead refused: %s", why)
	}
	w.toTestOneReview(lead)
	out, err := w.runStage()
	if err != nil || !strings.Contains(out, "Outcome: pass") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !markerExists(w.sideFolder("client"), "cross-wired") {
		t.Error("the cross-location test must run with the edited location's wiring applied")
	}
	assertRestored(t, w.sideFolder("client"))
	assertRestored(t, w.sideFolder("server"))
}

func TestPlainStageHonoursNotApplicable(t *testing.T) {
	w := depWorld(t, `  apply-dependency: { not-applicable: "builds from the sibling checkout" }`, "touch tested")
	out, err := w.runModelsPlain()
	if err != nil || !strings.Contains(out, "Outcome: pass") || !strings.Contains(out, "builds from the sibling checkout") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, l := range []string{"client", "server"} {
		if !markerExists(w.sideFolder(l), "built") || markerExists(w.sideFolder(l), "built-wired") || markerExists(w.sideFolder(l), "dep.env") {
			t.Errorf("side %s should be tested as it is", l)
		}
	}
}

func TestAgentCommandsAndPrompt(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	id := w.state().Run
	r, err := openRun(w.bridge, id)
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	tester := dependenciesSection(r, false)
	for _, want := range []string{"ensign dependency apply", "--side client", "--side server", "ensign dependency restore", "models @ this run's branch"} {
		if !strings.Contains(tester, want) {
			t.Errorf("the tester's section lacks %q:\n%s", want, tester)
		}
	}
	if got := dependenciesSection(r, true); got != "" {
		t.Errorf("the coder works in the edited location only, got:\n%s", got)
	}
	var out bytes.Buffer
	args := []string{"--bridge", w.bridge, "--run", id, "--side", "client"}
	if err := cmdDependency(append([]string{"apply"}, args...), &out); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if got := readFile(t, w.sideFolder("client"), "go.mod"); !strings.Contains(got, "replace") {
		t.Errorf("apply left go.mod as %q", got)
	}
	if err := cmdDependency(append([]string{"restore"}, args...), &out); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertRestored(t, w.sideFolder("client"))
	if err := cmdDependency([]string{"apply", "--bridge", w.bridge, "--run", id}, &out); err == nil {
		t.Error("apply without --side should be refused")
	}
}

func unitRun(t *testing.T, ws string) *RunFiles {
	t.Helper()
	w := newWorld(t)
	return &RunFiles{Dir: t.TempDir(), Snap: &Snapshot{Bridge: w.bridge, Project: "supernova", Task: "supernova/dep", Workspace: ws}, State: &State{Run: "r1"}}
}

func unitSide(ws, command string) Side {
	return Side{Location: "server", Workspace: ws, Bindings: map[string]Binding{applyDependencyBinding: {Command: command}}}
}

func ignoredNow(t *testing.T, ws string) []string {
	t.Helper()
	got, err := listIgnored(ws)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// unitDep is a dependency on the feature-x branch of a models checkout that has it checked out.
func unitDep(t *testing.T) Dependency {
	t.Helper()
	models := newRepo(t, filepath.Join(t.TempDir(), "models"))
	runGit(t, models, "checkout", "-q", "-b", "feature-x")
	return Dependency{Location: "models", Branch: "feature-x", Path: models, Sides: []string{"server"}}
}

func TestParseAppliedList(t *testing.T) {
	got, err := parseAppliedList("go.mod\r\n\r\nsub\\dir\\go.sum\r\n./a/../b.txt\n")
	if err != nil || !reflect.DeepEqual(got, []string{"go.mod", "sub/dir/go.sum", "b.txt"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"/etc/passwd", `\etc\passwd`, "C:/x", `C:\x`, "../x", "a/../../x", ".."} {
		if _, err := parseAppliedList(bad + "\n"); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRestoreReturnsADirtyFileToItsPreApplyContent(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	runGit(t, ws, "checkout", "-q", "-b", "task/dep")
	write(t, filepath.Join(ws, "go.mod"), "module x\nmine\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; touch new.txt; echo go.mod > \"$STARSHIP_APPLIED_FILES\"; echo new.txt >> \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\nmine\n" {
		t.Errorf("go.mod = %q, want the content from before the apply", got)
	}
	if markerExists(ws, "new.txt") {
		t.Error("new.txt was clean before and not in HEAD, so restore deletes it")
	}
	if err := checkNoDependencyWiring(r); err != nil {
		t.Fatalf("nothing of the wiring is left: %v", err)
	}
	write(t, filepath.Join(ws, "lib.txt"), "work\n")
	if err := githubCommit(r); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreLeavesAFileEditedAfterTheApply(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; touch new.txt; echo go.mod > \"$STARSHIP_APPLIED_FILES\"; echo new.txt >> \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "go.mod"), "module x\nreplace\nfixed\n")
	_, err := restoreDependencies(ws, "ensign dependency restore --side server")
	if err == nil || !strings.Contains(err.Error(), "go.mod") || !strings.Contains(err.Error(), "ensign dependency restore --side server --done go.mod") {
		t.Fatalf("restore should name go.mod and the settle command, got %v", err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\nreplace\nfixed\n" {
		t.Errorf("the edit was overwritten: %q", got)
	}
	if markerExists(ws, "new.txt") {
		t.Error("new.txt is untouched since the apply, so restore deletes it")
	}
	if err := checkNoDependencyWiring(r); err == nil || !strings.Contains(err.Error(), "--done go.mod") {
		t.Errorf("the commit check should name the settle command: %v", err)
	}
	if err := settleApplied(ws, []string{"nothing.txt"}); err == nil {
		t.Error("settling a file that is not recorded should be refused")
	}
	write(t, filepath.Join(ws, "go.mod"), "module x\nfixed\n")
	if err := settleApplied(ws, []string{"go.mod"}); err != nil {
		t.Fatal(err)
	}
	if err := checkNoDependencyWiring(r); err != nil {
		t.Errorf("a settled file no longer blocks: %v", err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\nfixed\n" {
		t.Errorf("settle must not touch the file: %q", got)
	}
}

func TestSettleThroughTheCommand(t *testing.T) {
	w := depWorld(t, depApply, "touch tested")
	w.run(locTask(t, w, "m", "models"))
	w.approve()
	id := w.state().Run
	ws := w.sideFolder("client")
	args := []string{"--bridge", w.bridge, "--run", id, "--side", "client"}
	var out bytes.Buffer
	if err := cmdDependency(append([]string{"apply"}, args...), &out); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "go.mod"), "module x\nedited\n")
	err := cmdDependency(append([]string{"restore"}, args...), &out)
	if err == nil || !strings.Contains(err.Error(), "--side client --done go.mod") {
		t.Fatalf("got %v", err)
	}
	if err := cmdDependency(append([]string{"restore", "--done", "go.mod"}, args...), &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdDependency(append([]string{"restore"}, args...), &out); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFailureKeepsWhatWasTouchedRestorable(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"; exit 3")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err == nil {
		t.Fatal("a failing binding is an error")
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\n" {
		t.Errorf("go.mod = %q", got)
	}
}

func TestApplyRefusesPathsOutsideTheWorkspace(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo ../outside > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err == nil || !strings.Contains(err.Error(), "not a file inside the workspace") {
		t.Fatalf("got %v", err)
	}
}

func TestBadLineInTheAppliedListKeepsTheValidEntries(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"; echo /etc/passwd >> \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err == nil {
		t.Fatal("a bad line is an error")
	}
	if err := checkNoDependencyWiring(r); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("go.mod should still be recorded: %v", err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\n" {
		t.Errorf("go.mod = %q", got)
	}
}

func TestAutomaticDependencyOnADetachedHeadIsAnError(t *testing.T) {
	needPOSIXShell(t)
	models := newRepo(t, filepath.Join(t.TempDir(), "models"))
	runGit(t, models, "checkout", "-q", "--detach")
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	dep := Dependency{Location: "models", Path: models, Sides: []string{"server"}}
	if _, err := applyDependency(r, nil, unitSide(ws, "true"), dep, ignoredNow(t, ws)); err == nil || !strings.Contains(err.Error(), models) {
		t.Fatalf("a detached HEAD should name the folder, got %v", err)
	}
}

func TestGithubCommitRefusesWhileWiringIsInTheTree(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	runGit(t, ws, "checkout", "-q", "-b", "task/dep")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "lib.txt"), "work\n")
	err := githubCommit(r)
	if err == nil || !strings.Contains(err.Error(), "dependency wiring from @apply-dependency is still in the tree: go.mod") || !strings.Contains(err.Error(), "ensign dependency restore --bridge") {
		t.Fatalf("got %v", err)
	}
	if runGit(t, ws, "rev-list", "--count", "HEAD") != "1" {
		t.Error("nothing may be committed while the wiring is in the tree")
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if err := githubCommit(r); err != nil {
		t.Fatal(err)
	}
	if runGit(t, ws, "show", "--name-only", "--format=", "HEAD") != "lib.txt" {
		t.Error("the commit should hold the work and not the wiring")
	}
}

func TestCatalogCommitPassRefusesWhileWiringIsInTheTree(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "lib.txt"), "work\n")
	d := &Dispatch{ID: "d1", Summary: "added lib", Fields: map[string][]string{"files": {"lib.txt"}}}
	if err := catalogCommitPass(r, d); err == nil || !strings.Contains(err.Error(), "still in the tree: go.mod") {
		t.Fatalf("got %v", err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if err := catalogCommitPass(r, d); err != nil {
		t.Fatal(err)
	}
	if runGit(t, ws, "show", "--name-only", "--format=", "HEAD") != "lib.txt" {
		t.Error("the commit should hold the work and not the wiring")
	}
}

// ignoredRepo is newRepo with go.work ignored.
func ignoredRepo(t *testing.T) string {
	t.Helper()
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	write(t, filepath.Join(ws, ".gitignore"), "go.work\n")
	runGit(t, ws, "add", ".gitignore")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	return ws
}

func TestRestoreLeavesAPreExistingIgnoredFileTheApplyModified(t *testing.T) {
	needPOSIXShell(t)
	ws := ignoredRepo(t)
	write(t, filepath.Join(ws, "go.work"), "use ./a\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo use ../models >> go.work; echo go.work > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	if err := checkNoDependencyWiring(r); err != nil {
		t.Errorf("an ignored file cannot block a commit: %v", err)
	}
	notice, err := restoreDependencies(ws, "settle")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice, "go.work") || !strings.Contains(notice, "in place") {
		t.Errorf("the notice should say go.work was left in place: %q", notice)
	}
	if got := readFile(t, ws, "go.work"); got != "use ./a\nuse ../models\n" {
		t.Errorf("go.work = %q, want it left as it is", got)
	}
	if rec, _ := appliedRecordPath(ws); markerExists(rec) {
		t.Error("the file should be out of the record")
	}
}

func TestRestoreDeletesAnIgnoredFileTheApplyCreated(t *testing.T) {
	needPOSIXShell(t)
	ws := ignoredRepo(t)
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo use ../models >> go.work; echo go.work > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	notice, err := restoreDependencies(ws, "settle")
	if err != nil || notice != "" {
		t.Fatalf("%q, %v", notice, err)
	}
	if markerExists(ws, "go.work") {
		t.Error("go.work was created by the apply, so restore deletes it")
	}
}

func TestSecondApplyAfterRestoreRestoresAFileInAnIgnoredFolder(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	write(t, filepath.Join(ws, ".gitignore"), "gen/\n")
	runGit(t, ws, "add", ".gitignore")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	r := unitRun(t, ws)
	sd := unitSide(ws, "mkdir -p gen; echo use ../models >> gen/go.work; echo gen/go.work > \"$STARSHIP_APPLIED_FILES\"")
	for i := 0; i < 2; i++ {
		if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
			t.Fatal(err)
		}
		notice, err := restoreDependencies(ws, "settle")
		if err != nil || notice != "" {
			t.Fatalf("round %d: %q, %v", i, notice, err)
		}
		if markerExists(ws, "gen", "go.work") {
			t.Errorf("round %d: gen/go.work was created by the apply, so restore deletes it", i)
		}
		if markerExists(ws, "gen") {
			t.Errorf("round %d: restore should remove the folder it left empty", i)
		}
	}
}

func TestApplyLeavesIgnoredFilesThatExistedBefore(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	write(t, filepath.Join(ws, ".gitignore"), "gen/\n")
	runGit(t, ws, "add", ".gitignore")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	write(t, filepath.Join(ws, "gen", "go.work"), "use ./a\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo use ../models >> gen/go.work; echo gen/go.work > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	notice, err := restoreDependencies(ws, "settle")
	if err != nil || !strings.Contains(notice, "gen/go.work") {
		t.Fatalf("%q, %v", notice, err)
	}
	if got := readFile(t, ws, "gen", "go.work"); got != "use ./a\nuse ../models\n" {
		t.Errorf("gen/go.work = %q, want it left as it is", got)
	}
}

func TestApplyRestoresAFileCreatedInANonEmptyIgnoredFolder(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	write(t, filepath.Join(ws, ".gitignore"), "gen/\n")
	runGit(t, ws, "add", ".gitignore")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	write(t, filepath.Join(ws, "gen", "cache"), "c\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo use ../models > gen/go.work; echo gen/go.work > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "gen", "go.work")); !os.IsNotExist(err) {
		t.Errorf("gen/go.work should be removed, stat err %v", err)
	}
	if got := readFile(t, ws, "gen", "cache"); got != "c\n" {
		t.Errorf("gen/cache = %q", got)
	}
}

func TestRestoreKeepsAnIgnoredFileInAnUntrackedFolder(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	write(t, filepath.Join(ws, ".gitignore"), "*.local\n")
	runGit(t, ws, "add", ".gitignore")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	write(t, filepath.Join(ws, "tools", "run.sh"), "#!/bin/sh\n")
	write(t, filepath.Join(ws, "tools", "env.local"), "A=1\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo B=2 >> tools/env.local; echo tools/env.local > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "tools", "env.local"); got != "A=1\nB=2\n" {
		t.Errorf("tools/env.local = %q, want the user's file kept", got)
	}
}

func TestReapplyIsRefusedWhileAKeptFileIsUnsettled(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "go.mod"), "module x\nreplace\nfixed\n")
	if _, err := restoreDependencies(ws, "settle"); err == nil {
		t.Fatal("restore keeps the edited file")
	}
	_, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws))
	if err == nil || !strings.Contains(err.Error(), "--done go.mod") {
		t.Fatalf("apply should be refused naming --done go.mod, got %v", err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\nreplace\nfixed\n" {
		t.Errorf("the edit was touched: %q", got)
	}
	write(t, filepath.Join(ws, "go.mod"), "module x\nfixed\n")
	if err := settleApplied(ws, []string{"go.mod"}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatalf("apply after --done: %v", err)
	}
}

func TestReapplyOfAFileAtItsPostKeepsTheOriginalTarget(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	write(t, filepath.Join(ws, "go.mod"), "module x\nmine\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo go.mod > \"$STARSHIP_APPLIED_FILES\"")
	for i := 0; i < 2; i++ {
		if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\nmine\n" {
		t.Errorf("go.mod = %q, want the content from before the first apply", got)
	}
}

func TestRestoreReturnsADirtyNonASCIIFileToItsPreApplyContent(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	os.MkdirAll(filepath.Join(ws, "docs"), 0o755)
	write(t, filepath.Join(ws, "docs", "café.md"), "mine\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo wired >> docs/café.md; echo docs/café.md > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "docs", "café.md"); got != "mine\n" {
		t.Errorf("café.md = %q, want the content from before the apply", got)
	}
}

// autoWorld is a dependency world whose task t has its models subtask done and its server subtask pending, with models' checkout on main and an origin that has it.
func autoWorld(t *testing.T) *world {
	t.Helper()
	w := depWorld(t, depApply, "touch tested")
	models := w.sideFolder("models")
	bare := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, models, "clone", "-q", "--bare", models, bare)
	runGit(t, models, "remote", "add", "origin", bare)
	runGit(t, models, "fetch", "-q", "origin")
	runGit(t, models, "remote", "set-head", "origin", "main")
	locTask(t, w, "t", "models, server")
	w.subtaskFile("t", "01-models", "---\nlocation: models\n---\n\n## Goal\n\nmodels.\n")
	w.subtaskFile("t", "02-server", "---\nlocation: server\n---\n\n## Goal\n\nserver.\n")
	if out, err := w.taskCmd("subtask", "done", "supernova/t", "01-models"); err != nil {
		t.Fatal(out, err)
	}
	return w
}

// taskBranchAhead puts the task's branch in models one commit ahead of main and leaves it checked out.
func taskBranchAhead(t *testing.T, w *world) {
	t.Helper()
	models := w.sideFolder("models")
	runGit(t, models, "checkout", "-q", "-b", "task/t")
	write(t, filepath.Join(models, "schema.txt"), "schema\n")
	runGit(t, models, "add", "schema.txt")
	runGit(t, models, "commit", "-q", "-m", "schema")
}

func TestAutomaticDependencyOnTheTaskBranchWhenItIsAhead(t *testing.T) {
	w := autoWorld(t)
	taskBranchAhead(t, w)
	snap, err := w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := Dependency{Location: "models", Branch: "task/t", Path: w.sideFolder("models"), Sides: []string{"server"}, Auto: true}
	if !reflect.DeepEqual(snap.Dependencies, []Dependency{want}) {
		t.Fatalf("dependencies %+v, want %+v", snap.Dependencies, want)
	}
	if lines := strings.Join(dependencyLines(snap.sides(), snap.Dependencies), "\n"); !strings.Contains(lines, "dependency models @ task/t") || !strings.Contains(lines, "automatic") {
		t.Errorf("the summary should show it as automatic:\n%s", lines)
	}
}

func TestAutomaticDependencyIsLeftOutWhenTheBranchIsMissingOrNotAhead(t *testing.T) {
	w := autoWorld(t)
	snap, err := w.snapOf("t", runOpts{})
	if err != nil || len(snap.Dependencies) != 0 {
		t.Fatalf("no task branch (published): %v %+v", err, snap)
	}
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "task/t")
	snap, err = w.snapOf("t", runOpts{})
	if err != nil || len(snap.Dependencies) != 0 {
		t.Fatalf("a branch with nothing its base lacks: %v %+v", err, snap)
	}
}

func TestExplicitDependencyWinsOverTheAutomaticOne(t *testing.T) {
	w := autoWorld(t)
	taskBranchAhead(t, w)
	runGit(t, w.sideFolder("models"), "checkout", "-q", "-b", "feature-x")
	snap, err := w.snapOf("t", runOpts{dependencies: []string{"models=feature-x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Dependencies) != 1 || snap.Dependencies[0].Branch != "feature-x" || snap.Dependencies[0].Auto {
		t.Fatalf("dependencies %+v", snap.Dependencies)
	}
}

func TestDependencyRefusedWhenItsCheckoutIsOnAnotherBranch(t *testing.T) {
	w := autoWorld(t)
	taskBranchAhead(t, w)
	models := w.sideFolder("models")
	runGit(t, models, "checkout", "-q", "main")
	_, err := w.snapOf("t", runOpts{})
	for _, want := range []string{models, "branch main", "task/t"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the automatic dependency: got %v, want it to name %q", err, want)
		}
	}
	runGit(t, models, "checkout", "-q", "--detach")
	if _, err := w.snapOf("t", runOpts{dependencies: []string{"models=task/t"}}); err == nil || !strings.Contains(err.Error(), "detached HEAD") || !strings.Contains(err.Error(), "task/t") {
		t.Fatalf("an explicit dependency on a detached checkout: %v", err)
	}
}

func TestApplyRefusesWhenTheDependencyCheckoutMovedAway(t *testing.T) {
	ws := newRepo(t, filepath.Join(t.TempDir(), "server"))
	r := unitRun(t, ws)
	dep := unitDep(t)
	runGit(t, dep.Path, "checkout", "-q", "-b", "other")
	_, err := applyDependency(r, nil, unitSide(ws, "true"), dep, ignoredNow(t, ws))
	if err == nil || !strings.Contains(err.Error(), dep.Path) || !strings.Contains(err.Error(), "branch other") || !strings.Contains(err.Error(), "feature-x") {
		t.Fatalf("got %v", err)
	}
}

func TestGitSpelling(t *testing.T) {
	ws := newRepo(t, filepath.Join(t.TempDir(), "models"))
	write(t, filepath.Join(ws, "Docs", "Readme.md"), "docs\n")
	write(t, filepath.Join(ws, "notes", "Été.md"), "accent\n")
	runGit(t, ws, "add", "Docs/Readme.md", "notes/Été.md")
	runGit(t, ws, "commit", "-q", "-m", "docs")
	rec := []appliedEntry{{Path: "rec/File.txt"}}
	before := map[string]string{"dirty/B.txt": "", "dirty/b.txt": ""}
	ignored := []string{"gen/go.work"}
	for _, c := range []struct{ in, want string }{
		{"REC/file.txt", "rec/File.txt"},
		{"dirty/b.txt", "dirty/b.txt"},
		{"DIRTY/B.TXT", "dirty/B.txt"},
		{"Gen/go.work", "gen/go.work"},
		{"GO.MOD", "go.mod"},
		{"docs/readme.md", "Docs/Readme.md"},
		{"notes/été.md", "notes/Été.md"},
		{"Brand/New.txt", "Brand/New.txt"},
	} {
		if got := gitSpelling(ws, c.in, rec, before, ignored); got != c.want {
			t.Errorf("gitSpelling(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestApplyMapsAReportedPathToGitsSpellingOnACaseInsensitiveRepo(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "client"))
	runGit(t, ws, "config", "core.ignorecase", "true")
	write(t, filepath.Join(ws, ".gitignore"), "gen/\n")
	runGit(t, ws, "add", ".gitignore")
	runGit(t, ws, "commit", "-q", "-m", "ignore")
	write(t, filepath.Join(ws, "gen", "go.work"), "use ./a\n")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo use ../models >> gen/go.work; echo Gen/go.work > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	rec, _, err := loadApplied(ws)
	if err != nil || len(rec.Files) != 1 || rec.Files[0].Path != "gen/go.work" || !rec.Files[0].Left {
		t.Fatalf("the record should hold gen/go.work as left in place: %+v, %v", rec.Files, err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "gen", "go.work"); got != "use ./a\nuse ../models\n" {
		t.Errorf("gen/go.work = %q, want it left in place", got)
	}
}

func TestRestoreReturnsATrackedFileReportedInAnotherCaseToHead(t *testing.T) {
	needPOSIXShell(t)
	ws := newRepo(t, filepath.Join(t.TempDir(), "client"))
	runGit(t, ws, "config", "core.ignorecase", "true")
	r := unitRun(t, ws)
	sd := unitSide(ws, "echo replace >> go.mod; echo GO.MOD > \"$STARSHIP_APPLIED_FILES\"")
	if _, err := applyDependency(r, nil, sd, unitDep(t), ignoredNow(t, ws)); err != nil {
		t.Fatal(err)
	}
	rec, _, err := loadApplied(ws)
	if err != nil || len(rec.Files) != 1 || rec.Files[0].Path != "go.mod" {
		t.Fatalf("the record should hold go.mod: %+v, %v", rec.Files, err)
	}
	if _, err := restoreDependencies(ws, "settle"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ws, "go.mod"); got != "module x\n" {
		t.Errorf("go.mod = %q, want HEAD's content", got)
	}
}
