package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

const rulesBridge = `control-plane:
  path: %s
use: [house]
rules:
  - Keep commits small.
  - Run the linter.
`

const rulesExtension = `name: house
summary: house rules
rules:
  - Name tests after behaviour.
  - Keep commits small.
`

func (w *world) declareRules(projectExtra string) {
	w.t.Helper()
	root, _ := filepath.Abs("../..")
	write(w.t, filepath.Join(w.bridge, "bridge.yaml"), strings.Replace(rulesBridge, "%s", root, 1))
	write(w.t, filepath.Join(w.bridge, "extensions", "house", "extension.yaml"), rulesExtension)
	write(w.t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), `platforms: [linux]

bindings:
  build: { command: "./build.sh" }
  test:  { command: "./test.sh" }
  lint:  { not-applicable: "no linter in this toy project" }
`+projectExtra)
}

const projectRules = `rules:
  - "  Run the linter.  "
  - Never touch the vault.
`

func TestRulesCombineInLayerOrderWithDuplicatesDropped(t *testing.T) {
	w := newWorld(t)
	w.declareRules(projectRules)
	w.addTask("t")
	snap, err := w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := "Do t.\n\n## Project rules\n\n- Name tests after behaviour.\n- Keep commits small.\n- Run the linter.\n- Never touch the vault.\n"
	if !strings.HasSuffix(snap.WorkItem, want) {
		t.Errorf("work item:\n%s", snap.WorkItem)
	}
	if len(snap.Rules) != 4 || !strings.HasPrefix(snap.Rules[0].Source, "extension:house:") || !strings.HasPrefix(snap.Rules[1].Source, "extension:house:") || !strings.HasPrefix(snap.Rules[2].Source, "bridge:bridge.yaml:") || !strings.HasPrefix(snap.Rules[3].Source, "project:") {
		t.Errorf("rules %+v", snap.Rules)
	}
}

func TestRulesComeFromLocationsAndNotFromTheTask(t *testing.T) {
	w := locationsWorld(t)
	project := strings.Replace(locationsProject, "  client:\n    depends-on: [models]\n", "  client:\n    depends-on: [models]\n    rules:\n      - Mock the network.\n", 1)
	write(t, filepath.Join(w.bridge, "projects", "supernova", "project.yaml"), "rules:\n  - Never touch the vault.\n"+project)
	name := locTask(t, w, "t", "client")
	write(t, filepath.Join(w.bridge, "projects", "supernova", "tasks", "t", "task.md"), "---\npipeline: coding\nlocations: [client]\nrules:\n  - Task rule.\n---\n\n## Goal\n\nDo it.\n")
	snap, err := createRun(w.bridge, runOpts{task: name}, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(snap.WorkItem, "## Project rules\n\n- Never touch the vault.\n- Mock the network.\n") {
		t.Errorf("work item:\n%s", snap.WorkItem)
	}
	if !strings.Contains(snap.Rules[1].Source, "#client:") {
		t.Errorf("rules %+v", snap.Rules)
	}
}

func TestRulesInSubtaskRunAndRetry(t *testing.T) {
	w := newWorld(t)
	w.declareRules(projectRules)
	w.subtaskTask("t", "01-a")
	snap, err := w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.WorkItem, "## Subtask 01-a") || strings.Count(snap.WorkItem, "## Project rules") != 1 || strings.Index(snap.WorkItem, "## Subtask") > strings.Index(snap.WorkItem, "## Project rules") {
		t.Errorf("subtask work item:\n%s", snap.WorkItem)
	}

	w.mustSeq("run", "supernova/auth-refactor")
	w.approve()
	lead, _ := w.spawn("", "", "orchestrator")
	w.failRun(lead)
	w.declareRules("rules:\n  - Changed after the first run.\n")
	w.retryStep("1")
	r, err := readRun(w.bridge, w.step(1).Run)
	if err != nil {
		t.Fatal(err)
	}
	wi := r.Snap.WorkItem
	if strings.Count(wi, "## Project rules") != 1 || strings.Index(wi, "## Project rules") > strings.Index(wi, "## Current state") {
		t.Errorf("retry work item:\n%s", wi)
	}
	if strings.Contains(wi, "Changed after") || len(r.Snap.Rules) != 4 {
		t.Errorf("the retry should keep the frozen rules: %+v\n%s", r.Snap.Rules, wi)
	}
}

func TestEmptyProjectRulesKeyIsIgnored(t *testing.T) {
	w := newWorld(t)
	w.declareRules("rules:\n  # - Never touch the vault.\n")
	w.addTask("t")
	snap, err := w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := "Do t.\n\n## Project rules\n\n- Name tests after behaviour.\n- Keep commits small.\n- Run the linter.\n"
	if !strings.HasSuffix(snap.WorkItem, want) || len(snap.Rules) != 3 {
		t.Errorf("work item:\n%s", snap.WorkItem)
	}
}

func TestNoRulesLeavesTheWorkItemAlone(t *testing.T) {
	w := newWorld(t)
	w.addTask("t")
	snap, err := w.snapOf("t", runOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snap.WorkItem, "Project rules") || snap.Rules != nil {
		t.Errorf("work item:\n%s", snap.WorkItem)
	}
	if out := w.run("supernova/t", "--dry-run"); strings.Contains(out, "\nrules\n") {
		t.Errorf("summary:\n%s", out)
	}
}

func TestRunSummaryShowsRuleSources(t *testing.T) {
	w := newWorld(t)
	w.declareRules(projectRules)
	w.addTask("t")
	out := w.run("supernova/t", "--dry-run")
	for _, want := range []string{"\nrules\n", "Name tests after behaviour.   from extension:house:", "Run the linter.   from bridge:bridge.yaml:", "Never touch the vault.   from project:"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}
