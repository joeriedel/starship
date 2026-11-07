// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newImpactTask creates a task with no pipeline and returns its task.md.
func newImpactTask(t *testing.T, w *world, name string) string {
	t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	w.enter()
	var out bytes.Buffer
	if err := cmdTask([]string{"new", "supernova/" + name, "--pipeline", "none"}, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks", name, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTaskNewHasImpactOnlyWhenTheBridgeUsesIt(t *testing.T) {
	got := newImpactTask(t, connWorld(t, "use: [impact]\n"), "launch")
	if !strings.Contains(got, "## Decisions\n\n## Impact\n\n## Notes\n") {
		t.Errorf("the section should sit between Decisions and Notes:\n%s", got)
	}
	plain := newImpactTask(t, connWorld(t, ""), "launch")
	if strings.Contains(plain, "Impact") || plain != strings.Replace(got, "## Impact\n\n", "", 1) {
		t.Errorf("a bridge without impact gets the plain template:\n%s", plain)
	}
}

func TestWorkItemKeepsImpactOut(t *testing.T) {
	impact := "## Impact\n\n### User\n\n- 2026-11-02: Nova caught the gate bug IMPACT-TEXT\n\n### Inferred\n\n[inferred] Saved a day (task.md)\n\n"
	t.Run("goal and scope", func(t *testing.T) {
		got := workItem("## Goal\n\nShip the launch checklist.\n\n## Scope\n\nThe checklist only.\n\n" + impact + "## Notes\n\nNone.\n")
		if want := "## Goal\n\nShip the launch checklist.\n\n## Scope\n\nThe checklist only."; got != want {
			t.Errorf("work item = %q", got)
		}
	})
	t.Run("whole body", func(t *testing.T) {
		got := workItem("# launch\n\nFree text.\n\n## status\n\nBegun.\n\n## IMPACT\n\n### User\n\nIMPACT-TEXT\n\n### Inferred\n\nmore IMPACT-TEXT\n\n## Notes\n\nA note.\n")
		if strings.Contains(got, "IMPACT-TEXT") || strings.Contains(got, "###") || strings.Contains(strings.ToLower(got), "impact") {
			t.Errorf("the fallback leaks the section:\n%s", got)
		}
		for _, want := range []string{"Free text.", "Begun.", "## Notes", "A note."} {
			if !strings.Contains(got, want) {
				t.Errorf("the fallback lost %q:\n%s", want, got)
			}
		}
	})
	t.Run("last section", func(t *testing.T) {
		got := workItem("Free text.\n\n" + impact)
		if got != "Free text." {
			t.Errorf("work item = %q", got)
		}
	})
}

func TestTaskCloseLeavesImpactAndPrintsNothingMore(t *testing.T) {
	w := connWorld(t, "use: [impact]\n")
	impact := "## Impact\n\n### User\n\n- 2026-11-02: Nova caught the gate bug.\n\n### Inferred\n\n[inferred] Saved a day (task.md)\n"
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"),
		"---\ndocumentation: on\n---\n\n## Goal\n\nDo t.\n\n## Status\n\nDone.\n\n## Decisions\n\nKeep it small.\n\n"+impact+"\n## Notes\n\nNone.\n")
	out, err := closeTask(w, "t")
	if err != nil || out != "Closed supernova/t.\n" {
		t.Fatalf("close printed %q, %v", out, err)
	}
	text := w.taskText("t")
	if !strings.Contains(text, "status: closed") {
		t.Errorf("the task did not close:\n%s", text)
	}
	if got := section(text, "Impact"); got+"\n" != strings.TrimPrefix(impact, "## Impact\n\n") {
		t.Errorf("the Impact section changed:\n%s", text)
	}
}

func TestImpactExtensionLoadsAndHelpsOnlyWhereUsed(t *testing.T) {
	w := connWorld(t, "use: [impact]\n")
	good, broken := scanExtensions(w.bridge, repoRoot(t))
	if good["impact"] == nil || broken["impact"] != nil {
		t.Fatalf("impact did not load: %v", broken)
	}
	if good["focal-sheet"] != nil {
		t.Error("the focal-sheet extension still exists")
	}
	if len(good["impact"].Events) != 0 {
		t.Errorf("impact has events: %v", good["impact"].Events)
	}
	if got := requestCommands(w.bridge); !strings.Contains(got, "ensign help impact") {
		t.Errorf("use: impact should show its help:\n%s", got)
	}
	plain := connWorld(t, "")
	if got := requestCommands(plain.bridge); strings.Contains(got, "ensign help impact") {
		t.Errorf("without use, impact shows its help:\n%s", got)
	}
	if got := helpText(plain.bridge, plain.sid); strings.Contains(strings.ToLower(got), "impact") {
		t.Errorf("without use, help mentions impact:\n%s", got)
	}
}

func TestHelpImpactPrintsTheTopic(t *testing.T) {
	w := linkedWorld(t)
	w.enter()
	var out bytes.Buffer
	if err := cmdHelp([]string{"impact"}, &out); err != nil || !strings.Contains(out.String(), "# Impact") || !strings.Contains(out.String(), "### User") {
		t.Errorf("help impact: %v\n%s", err, out.String())
	}
}

func sessionStartText(t *testing.T, w *world) string {
	t.Helper()
	res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
	if code != 0 {
		t.Fatalf("SessionStart exited %d", code)
	}
	return dig(res, "hookSpecificOutput", "additionalContext")
}

const impactNoEffect = "has no effect there: impact reads use: in bridge.yaml only."

func TestImpactWarnings(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		w := newWorld(t)
		w.project("use: [impact]\n")
		got := sessionStartText(t, w)
		if !strings.Contains(got, "project supernova names the impact extension in its use: list, but that "+impactNoEffect) {
			t.Errorf("project not warned:\n%s", got)
		}
	})
	t.Run("task", func(t *testing.T) {
		w := newWorld(t)
		task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
		data, _ := os.ReadFile(task)
		body := strings.Replace(string(data), "---\n", "---\nuse: [impact]\n", 1)
		write(t, task, body)
		if got := sessionStartText(t, w); !strings.Contains(got, "task supernova/auth-refactor names the impact extension") || !strings.Contains(got, impactNoEffect) {
			t.Errorf("task not warned:\n%s", got)
		}
		write(t, task, strings.Replace(body, "---\n", "---\nstatus: closed\n", 1))
		if got := sessionStartText(t, w); strings.Contains(got, impactNoEffect) {
			t.Errorf("a closed task warned:\n%s", got)
		}
	})
	t.Run("bridge uses impact", func(t *testing.T) {
		w := connWorld(t, "use: [impact]\n")
		w.project("use: [impact]\n")
		if got := impactWarnings(w.bridge); len(got) != 0 {
			t.Errorf("warned although bridge.yaml uses impact: %q", got)
		}
	})
	t.Run("unreadable bridge.yaml", func(t *testing.T) {
		w := newWorld(t)
		w.project("use: [impact]\n")
		write(t, filepath.Join(w.bridge, "bridge.yaml"), "use: [impact\n  : :\n")
		if got := impactWarnings(w.bridge); len(got) != 0 {
			t.Errorf("warned although bridge.yaml cannot be loaded: %q", got)
		}
	})
	t.Run("long names", func(t *testing.T) {
		w := newWorld(t)
		long := strings.Repeat("a", 200)
		write(t, filepath.Join(w.bridge, "projects", "supernova-"+long, "project.yaml"), "use: [impact]\n")
		got := impactWarnings(w.bridge)
		if len(got) != 1 || strings.Contains(got[0], long) || len(got[0]) > 300 || !strings.Contains(got[0], "project supernova-") {
			t.Errorf("names not cut:\n%q", got)
		}
	})
	t.Run("several hits", func(t *testing.T) {
		w := newWorld(t)
		for _, n := range []string{"nova-a", "nova-b", "nova-c"} {
			write(t, filepath.Join(w.bridge, "projects", n, "project.yaml"), "use: [impact]\n")
		}
		got := impactWarnings(w.bridge)
		if len(got) != 1 || !strings.HasSuffix(got[0], " And 2 more like it.") || strings.Count(got[0], "names the impact") != 1 {
			t.Errorf("want one line with a tail:\n%q", got)
		}
	})
}
