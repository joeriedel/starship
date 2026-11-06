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

func wantListErr(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "reading the runs") {
		t.Errorf("%s: want the listing error, got %v", what, err)
	}
}

// plantRun writes a run folder by hand with a pipeline.json and a state.json of the given status.
func plantRun(t *testing.T, bridge, id, status string) string {
	t.Helper()
	dir := filepath.Join(runsDir(bridge), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), &Snapshot{Run: id}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), &State{Run: id, Status: status}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (w *world) listRuns() []runInfo {
	w.t.Helper()
	runs, err := listRuns(w.bridge)
	if err != nil {
		w.t.Fatal(err)
	}
	return runs
}

func (w *world) liveRuns() []runInfo {
	w.t.Helper()
	runs, err := liveRuns(w.bridge)
	if err != nil {
		w.t.Fatal(err)
	}
	return runs
}

func TestListRunsMissingFolderAndPlainFile(t *testing.T) {
	w := newWorld(t)
	dir := runsDir(w.bridge)
	check := func(what string) {
		t.Helper()
		if runs, err := listRuns(w.bridge); len(runs) != 0 || err != nil {
			t.Errorf("%s: listRuns = %v, %v", what, runs, err)
		}
		if runs, err := liveRuns(w.bridge); len(runs) != 0 || err != nil {
			t.Errorf("%s: liveRuns = %v, %v", what, runs, err)
		}
	}
	os.RemoveAll(dir)
	check("missing folder")
	write(t, dir, "not a folder")
	check("plain file")
	os.RemoveAll(filepath.Dir(dir))
	write(t, filepath.Dir(dir), "not a folder")
	check("plain file above")
}

func TestListRunsTable(t *testing.T) {
	const good = "20240101-000001-supernova"
	const bad = "20240101-000002-supernova"
	cases := []struct {
		name    string
		plant   func(t *testing.T, w *world)
		wantErr bool
		want    []string
	}{
		{"healthy run", func(t *testing.T, w *world) { plantRun(t, w.bridge, bad, "running") }, false, []string{bad, good}},
		{"plain file", func(t *testing.T, w *world) { write(t, filepath.Join(runsDir(w.bridge), bad), "x") }, false, []string{good}},
		{"empty folder", func(t *testing.T, w *world) {
			if err := os.MkdirAll(filepath.Join(runsDir(w.bridge), bad), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false, []string{good}},
		{"pipeline only", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, bad, "running")
			os.Remove(filepath.Join(dir, "state.json"))
		}, false, []string{good}},
		{"corrupt state", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, bad, "success")
			write(t, filepath.Join(dir, "state.json"), "{not json")
		}, true, nil},
		{"live state, corrupt pipeline", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, bad, "running")
			write(t, filepath.Join(dir, "pipeline.json"), "{not json")
		}, true, nil},
		{"live state, missing pipeline", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, bad, "pending")
			os.Remove(filepath.Join(dir, "pipeline.json"))
		}, true, nil},
		{"finished state, corrupt pipeline", func(t *testing.T, w *world) {
			dir := plantRun(t, w.bridge, bad, "error")
			write(t, filepath.Join(dir, "pipeline.json"), "{not json")
		}, false, []string{good}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			plantRun(t, w.bridge, good, "success")
			c.plant(t, w)
			runs, err := listRuns(w.bridge)
			_, lerr := liveRuns(w.bridge)
			if c.wantErr {
				wantListErr(t, "listRuns", err)
				wantListErr(t, "liveRuns", lerr)
				if runs != nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "ruled out as live") || !strings.Contains(err.Error(), filepath.Join(runsDir(w.bridge), bad)) {
					t.Errorf("listRuns = %v, %v", runs, err)
				}
				return
			}
			if err != nil || lerr != nil {
				t.Fatalf("listRuns: %v, liveRuns: %v", err, lerr)
			}
			var ids []string
			for _, ri := range runs {
				ids = append(ids, ri.id)
			}
			if strings.Join(ids, ",") != strings.Join(c.want, ",") {
				t.Errorf("listed %v, want %v", ids, c.want)
			}
		})
	}
}

func TestCorruptLiveRunRefusesCommands(t *testing.T) {
	w := newWorld(t)
	dir := plantRun(t, w.bridge, "20240101-000001-supernova", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	wantListErr(t, "link", link(w.bridge, true, nil))
	wantListErr(t, "folderHolder", folderHolder(w.bridge, &Snapshot{Workspace: w.bridge}, "s1"))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	wantListErr(t, "run", cmdRun([]string{"supernova/auth-refactor"}, &out))
}

func TestCmdUpdateWithRuns(t *testing.T) {
	repo := fakeRelease(t)
	bridge := pinnedBridge(t, repo)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(bridge)
	tag := func() string {
		bf, err := loadBridgeFile(bridge)
		if err != nil {
			t.Fatal(err)
		}
		return bf.ControlPlane.Tag
	}
	const id = "20240101-000001-supernova"
	dir := plantRun(t, bridge, id, "running")
	var out bytes.Buffer
	if err := cmdUpdate(nil, &out); err == nil || err.Error() != "run "+id+" is active; update waits until no run is active" || tag() != "v0.1.0" {
		t.Errorf("a live run: err %v, tag %s", err, tag())
	}
	write(t, filepath.Join(dir, "state.json"), "{not json")
	wantListErr(t, "cmdUpdate", cmdUpdate(nil, &out))
	if tag() != "v0.1.0" {
		t.Errorf("tag moved to %s", tag())
	}
}

func TestCmdUpdateRefusesAnUnpinnedBridge(t *testing.T) {
	w := newWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdUpdate(nil, &out); err == nil || !strings.Contains(err.Error(), "not pinned to a release") {
		t.Errorf("err = %v", err)
	}
}

func TestGCRunsMissingFolderReportsNothing(t *testing.T) {
	w := newWorld(t)
	os.RemoveAll(runsDir(w.bridge))
	if res := gcRuns(w.bridge, 500, 0); res.ListFailed {
		t.Errorf("gcRuns = %+v", res)
	}
	write(t, runsDir(w.bridge), "not a folder")
	if res := gcRuns(w.bridge, 500, 0); res.ListFailed {
		t.Errorf("plain file: gcRuns = %+v", res)
	}
}

func TestGoWaitingReadsTheSequenceFile(t *testing.T) {
	w := newWorld(t)
	if waiting, err := goWaiting(w.bridge, "s1"); waiting || err != nil {
		t.Errorf("missing file: %v, %v", waiting, err)
	}
	write(t, seqPath(w.bridge, "s1"), "{not json")
	if _, err := goWaiting(w.bridge, "s1"); err == nil {
		t.Error("a corrupt sequence file should be an error")
	}
}

func TestStageRoleAnywhereFailsClosedOnAListingError(t *testing.T) {
	w := newWorld(t)
	dir := plantRun(t, w.bridge, "20240101-000001-supernova", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	found, unread, listFailed := stageRoleAnywhere(w.bridge, "supernova-role")
	if found || !listFailed || len(unread) == 0 || !strings.HasPrefix(unread[0], "the runs: ") {
		t.Errorf("stageRoleAnywhere = %v, %v, %v", found, unread, listFailed)
	}
}

func TestUntiedAgentGuardFailsClosedOnAListingError(t *testing.T) {
	w := newWorld(t)
	w.sid = ""
	dir := plantRun(t, w.bridge, "20240101-000001-supernova", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	spawn := func(role string) (map[string]any, int) {
		return w.hook("PreToolUse", map[string]any{"agent_id": "ghost", "agent_type": "orchestrator",
			"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": role, "prompt": "x"}})
	}
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-reader.md"), "---\nname: supernova-reader\ntools: Read, Grep, Glob, Bash\n---\nbody\n")
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-editor.md"), "---\nname: supernova-editor\ntools: Read, Edit\n---\nbody\n")
	for _, role := range []string{"supernova-reader", "supernova-editor"} {
		res, code := spawn(role)
		why := dig(res, "hookSpecificOutput", "permissionDecisionReason")
		if code != 0 || dig(res, "hookSpecificOutput", "permissionDecision") != "deny" || !strings.Contains(why, "the runs") || !strings.Contains(why, "because these could not be read: ") || strings.Contains(why, "a pipeline file") {
			t.Errorf("%s: %d %v", role, code, res)
		}
	}
}

func TestSessionStartWithAStaleLink(t *testing.T) {
	gitBashFound = func() bool { return true }
	t.Cleanup(func() { gitBashFound = detectGitBash })
	start := func(w *world) (string, int) {
		res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
		return dig(res, "hookSpecificOutput", "additionalContext"), code
	}
	t.Run("live run", func(t *testing.T) {
		w := newWorld(t)
		if !linkStale(w.bridge) {
			t.Fatal("the test bridge should start with a stale link")
		}
		plantRun(t, w.bridge, "20240101-000001-supernova", "running")
		ctx, code := start(w)
		if code != 0 || !strings.HasPrefix(ctx, "STARSHIP-READY") || !linkStale(w.bridge) {
			t.Errorf("code %d, stale %v, context %q", code, linkStale(w.bridge), ctx)
		}
	})
	t.Run("no live run", func(t *testing.T) {
		w := newWorld(t)
		plantRun(t, w.bridge, "20240101-000001-supernova", "success")
		ctx, code := start(w)
		if code != 0 || !strings.HasPrefix(ctx, "STARSHIP-READY") || linkStale(w.bridge) {
			t.Errorf("code %d, stale %v, context %q", code, linkStale(w.bridge), ctx)
		}
	})
	t.Run("corrupt live run", func(t *testing.T) {
		w := newWorld(t)
		const id = "20240101-000001-supernova"
		dir := plantRun(t, w.bridge, id, "running")
		write(t, filepath.Join(dir, "state.json"), "{not json")
		ctx, code := start(w)
		if code != 0 || strings.HasPrefix(ctx, "STARSHIP-READY") || !strings.Contains(ctx, "not ready") || !strings.Contains(ctx, id) {
			t.Errorf("code %d, context %q", code, ctx)
		}
	})
}
