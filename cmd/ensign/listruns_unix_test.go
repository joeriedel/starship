// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unlistable makes the world's runs folder exist but refuse to be listed.
func unlistable(t *testing.T, w *world) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := runsDir(w.bridge)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

func TestUnlistableRunsFolderFailsGuards(t *testing.T) {
	w := newWorld(t)
	unlistable(t, w)
	bridge := w.bridge

	runs, err := listRuns(bridge)
	if runs != nil {
		t.Errorf("listRuns returned runs with an error: %v", runs)
	}
	wantListErr(t, "listRuns", err)
	_, err = liveRuns(bridge)
	wantListErr(t, "liveRuns", err)

	wantListErr(t, "link", link(bridge, true, nil))
	_, _, _, err = githubUpdate(bridge, "supernova/auth-refactor", "")
	wantListErr(t, "githubUpdate", err)
	_, _, _, err = tested(bridge, "supernova/auth-refactor", "")
	wantListErr(t, "tested", err)
	wantListErr(t, "latestRunPassed", latestRunPassed(bridge, "supernova/auth-refactor", ""))
	wantListErr(t, "folderHolder", folderHolder(bridge, &Snapshot{Workspace: w.bridge}, "s1"))
	_, err = claimRun(bridge, "s1")
	wantListErr(t, "claimRun", err)
	_, err = findAgentRun(bridge, &HookInput{AgentID: "a1"})
	wantListErr(t, "findAgentRun", err)
	_, err = goWaiting(bridge, "s1")
	wantListErr(t, "goWaiting", err)

	found, unread, _ := stageRoleAnywhere(bridge, "no-such-role")
	if found || len(unread) == 0 || !strings.Contains(unread[0], "reading the runs") {
		t.Errorf("stageRoleAnywhere = %v, %v", found, unread)
	}
	if found, _, _ := stageRoleAnywhere(bridge, "reviewer"); !found {
		t.Error("stageRoleAnywhere missed a role a pipeline file names")
	}
}

func TestUnlistableRunsFolderRefusesCommands(t *testing.T) {
	w := newWorld(t)
	unlistable(t, w)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	wantListErr(t, "catalog squash", cmdCatalog([]string{"squash", "supernova/auth-refactor", "-m", "x"}, &out))
	wantListErr(t, "run", cmdRun([]string{"supernova/auth-refactor"}, &out))
}

func TestUnlistableRunsFolderSweepsNothing(t *testing.T) {
	w := newWorld(t)
	paths := sessFiles(t, w, "old", 2*sessionFileAge, "")
	unlistable(t, w)
	entries, err := os.ReadDir(filepath.Join(w.bridge, ".starship", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sweepSessionFiles(w.bridge, entries, &sessionSweep{deadline: time.Now().Add(retryWindow)})
	if !allExist(paths) {
		t.Error("session files were swept although the runs could not be listed")
	}
}

func TestGCRunsReportsAnUnlistableRunsFolder(t *testing.T) {
	w := newWorld(t)
	unlistable(t, w)
	stderr := captureStderr(t, func() {
		if res := gcRuns(w.bridge, 500, 0); !res.ListFailed || res.Removed != 0 {
			t.Errorf("gcRuns = %+v, want ListFailed", res)
		}
	})
	if !strings.Contains(stderr, "warning: could not list the runs") {
		t.Errorf("stderr = %q", stderr)
	}
	var out bytes.Buffer
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	captureStderr(t, func() {
		if err := cmdGC(nil, &out); err == nil {
			t.Error("cmdGC must fail when the runs could not be listed")
		}
	})
	if !strings.Contains(out.String(), ", could not list the runs") {
		t.Errorf("gc output = %q", out.String())
	}
}
