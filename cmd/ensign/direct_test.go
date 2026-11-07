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

func (w *world) direct(args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdDirect(args, &out)
	return out.String(), err
}

// codeFolder is a code folder outside the bridge that no run holds.
func (w *world) codeFolder(name string) string {
	w.t.Helper()
	dir := filepath.Join(filepath.Dir(w.bridge), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	return dir
}

// openDirect asks for a direct edit, shows it at a Stop and answers go.
func (w *world) openDirect(folder string) {
	w.t.Helper()
	if _, err := w.direct("ask", folder, "fix the typo"); err != nil {
		w.t.Fatal(err)
	}
	w.replyStop("I want to edit "+folder, false)
	if ctx := w.say("go"); !strings.Contains(ctx, "guard is open") {
		w.t.Fatalf("go did not open the guard: %q", ctx)
	}
}

func TestDirectEditLifecycle(t *testing.T) {
	w, _ := newGuardWorld(t)
	code := w.codeFolder("code")
	file := filepath.Join(code, "main.go")
	w.expectEdit("", "", file, true)
	if _, err := w.direct("ask", code, "fix the typo"); err != nil {
		t.Fatal(err)
	}
	w.expectEdit("", "", file, true)
	w.replyStop("shown", false)
	w.expectEdit("", "", file, true)
	if ctx := w.say("go"); !strings.Contains(ctx, "guard is open") || !strings.Contains(ctx, "ensign direct close") {
		t.Fatalf("go: %q", ctx)
	}
	w.expectEdit("", "", file, false)
	w.expectEdit("", "", filepath.Join(code, "sub", "new.go"), false)
	w.expectEdit("", "", filepath.Join(filepath.Dir(code), "elsewhere.go"), true)
	w.expectEdit("a1", "coder", file, true)
	if _, why := w.tryEdit("", "", filepath.Join(filepath.Dir(code), "elsewhere.go")); !strings.Contains(why, "ensign help direct") {
		t.Errorf("the refusal points at the topic: %s", why)
	}
	w2 := *w
	w2.sid = "s2"
	w2.expectEdit("", "", file, true)

	w.replyStop("done", false)
	w.expectEdit("", "", file, true)

	w.openDirect(code)
	w.say("something else")
	w.expectEdit("", "", file, true)

	w.openDirect(code)
	out, err := w.direct("close")
	if err != nil || !strings.Contains(out, "closed the direct edit") {
		t.Fatalf("close: %q %v", out, err)
	}
	w.expectEdit("", "", file, true)
	if out, _ := w.direct("close"); !strings.Contains(out, "no direct edit") {
		t.Errorf("close with none: %q", out)
	}
	if _, err := w.direct("ask", code, "again"); err != nil {
		t.Fatal(err)
	}
	if out, _ := w.direct("close"); !strings.Contains(out, "dropped the pending request") {
		t.Errorf("close of a pending request: %q", out)
	}
}

func TestDirectGoNeedsAShownRequest(t *testing.T) {
	w, _ := newGuardWorld(t)
	code := w.codeFolder("code")
	file := filepath.Join(code, "main.go")
	w.direct("ask", code, "fix the typo")
	if ctx := w.say("go"); !strings.Contains(ctx, "not shown") {
		t.Errorf("go before the Stop: %q", ctx)
	}
	w.expectEdit("", "", file, true)
	w.replyStop("shown", false)
	w.expectEdit("", "", file, true)
	if _, err := os.Stat(directPath(w.bridge, w.sid)); err == nil {
		t.Error("the unshown request must be voided")
	}

	w.direct("ask", code, "fix the typo")
	w.replyStop("shown", false)
	if ctx := w.say("yes"); !strings.Contains(ctx, "not approved") {
		t.Errorf("a reply other than go: %q", ctx)
	}
	w.replyStop("again", false)
	if ctx := w.say("go"); strings.Contains(ctx, "guard is open") {
		t.Errorf("go after a voided request opened the guard: %q", ctx)
	}
	w.expectEdit("", "", file, true)

	if _, err := w.direct("ask", filepath.Join(code, "missing"), "x"); err == nil {
		t.Error("the folder must exist")
	}
	if _, err := w.direct("ask", filepath.Join(code, "main.go"), "x"); err == nil {
		t.Error("the folder must be a directory")
	}
	t.Setenv("STARSHIP_SESSION", "")
	if _, err := w.direct("ask", code, "x"); err == nil || !strings.Contains(err.Error(), "STARSHIP_SESSION") {
		t.Errorf("no session: %v", err)
	}
}

func TestDirectRefusedWhereARunHolds(t *testing.T) {
	w, _ := newGuardWorld(t)
	ws := w.workspace()
	sub := filepath.Join(ws, "sub")
	os.MkdirAll(sub, 0o755)
	w.run("supernova/auth-refactor")
	if _, err := w.direct("ask", ws, "x"); err == nil || !strings.Contains(err.Error(), "waiting for go") {
		t.Errorf("ask while a run waits for go: %v", err)
	}
	w.approve()
	for _, folder := range []string{ws, sub} {
		if _, err := w.direct("ask", folder, "x"); err == nil || !strings.Contains(err.Error(), "a live run holds") {
			t.Errorf("ask in %s while a run holds it: %v", folder, err)
		}
	}

	// A broader folder is allowed, except under the held one.
	parent := filepath.Dir(ws)
	other := filepath.Join(parent, "other")
	os.MkdirAll(other, 0o755)
	if _, err := w.direct("ask", parent, "x"); err != nil {
		t.Fatal(err)
	}
	w.replyStop("shown", false)
	if ctx := w.say("go"); !strings.Contains(ctx, "guard is open") {
		t.Fatalf("go: %q", ctx)
	}
	w.expectEdit("", "", filepath.Join(other, "a.go"), false)
	w.expectEdit("", "", filepath.Join(ws, "main.go"), true)
	w.expectEdit("", "", filepath.Join(sub, "main.go"), true)
	if _, why := w.tryEdit("", "", filepath.Join(ws, "main.go")); !strings.Contains(why, "a live run holds") {
		t.Errorf("held refusal: %s", why)
	}
}

func TestDirectRefusedWhereARunReadsOrStartsLater(t *testing.T) {
	w, _ := newGuardWorld(t)
	f := supernovaFolders{w.codeFolder("client"), w.codeFolder("server"), w.codeFolder("models")}
	holdingRun(t, w, "20260101-000001-client", "s2", f.clientSnap())
	if _, err := w.direct("ask", f.models, "x"); err == nil || !strings.Contains(err.Error(), "reads") {
		t.Errorf("ask in a folder a run only reads: %v", err)
	}
	w.openDirect(f.server)
	w.expectEdit("", "", filepath.Join(f.server, "a.go"), false)
	holdingRun(t, w, "20260101-000002-server", "s2", f.serverSnap())
	w.expectEdit("", "", filepath.Join(f.server, "a.go"), true)

	// A go sees a run that started after the ask.
	w.replyStop("done", false)
	if _, err := w.direct("ask", f.client, "x"); err == nil {
		t.Error("client is held")
	}
	other := w.codeFolder("other")
	w.direct("ask", other, "x")
	w.replyStop("shown", false)
	holdingRun(t, w, "20260101-000003-other", "s2", &Snapshot{Workspace: other})
	if ctx := w.say("go"); !strings.Contains(ctx, "not opened") {
		t.Errorf("go while a run holds the folder: %q", ctx)
	}
	w.expectEdit("", "", filepath.Join(other, "a.go"), true)
}

func TestDirectAndRunNeverWaitTogether(t *testing.T) {
	w, _ := newGuardWorld(t)
	code := w.codeFolder("code")
	w.run("supernova/auth-refactor")
	if _, err := w.direct("ask", code, "x"); err == nil || !strings.Contains(err.Error(), "waiting for go") {
		t.Errorf("ask while a run waits: %v", err)
	}

	w, _ = newGuardWorld(t)
	code = w.codeFolder("code")
	w.direct("ask", code, "x")
	w.replyStop("shown", false)
	w.run("supernova/auth-refactor")
	ctx := w.say("go")
	if !strings.Contains(ctx, "both wait for go") {
		t.Errorf("go with both waiting: %q", ctx)
	}
	for _, ri := range w.listRuns() {
		if ri.state.Status != "pending" {
			t.Errorf("the run must not be approved: %s", ri.state.Status)
		}
	}
	w.expectEdit("", "", filepath.Join(code, "a.go"), true)
	w.approve() // the voided request leaves the run waiting for its own go
}

func TestGoForARunUnchangedWithoutDirect(t *testing.T) {
	w, _ := newGuardWorld(t)
	w.run("supernova/auth-refactor")
	w.approve()
}

func TestDirectBlocksWritesAdd(t *testing.T) {
	w := writesWorld(t)
	code := w.codeFolder("code")
	w.direct("ask", code, "x")
	if _, err := w.runWrites(`{"n":1}`, "add", "--tool", wikiPost); err == nil || !strings.Contains(err.Error(), "direct edit") {
		t.Errorf("add while a direct request waits: %v", err)
	}
}

func TestDirectStateIsNotEditable(t *testing.T) {
	w, _ := newGuardWorld(t)
	sessions := filepath.Join(w.bridge, ".starship", "sessions")
	for _, n := range []string{w.sid + ".direct", w.sid + ".direct.lock", w.sid + ".direct.tmp", "other.direct"} {
		w.expectEdit("", "", filepath.Join(sessions, n), true)
	}
}

func TestDirectEditWindowsSpelling(t *testing.T) {
	w, _ := newGuardWorld(t)
	code := w.codeFolder("code")
	w.openDirect(code)
	// A rooted path with no drive can't be placed on Windows, open folder or not.
	in := &HookInput{SessionID: w.sid, ToolName: "Edit", ToolInput: map[string]any{"file_path": `\code\main.go`}}
	if why, _, _ := editGuard(w.bridge, in, "windows"); !strings.Contains(why, "full `C:") {
		t.Errorf("a rooted path with no drive: %q", why)
	}
}

func TestDirectEditClosedByWritesGo(t *testing.T) {
	w := writesWorld(t)
	tmp := t.TempDir()
	old := editTempFolders
	editTempFolders = func() []string { return []string{tmp} }
	t.Cleanup(func() { editTempFolders = old })
	code := w.codeFolder("code")
	file := filepath.Join(code, "main.go")
	w.openDirect(code)
	w.expectEdit("", "", file, false)
	w.addWrite(wikiPost, postA)
	if ctx := w.say("go"); !strings.Contains(ctx, "direct edit") || !strings.Contains(ctx, "is closed") {
		t.Errorf("the writes go did not report the closed edit: %q", ctx)
	}
	w.expectEdit("", "", file, true)
}

func TestDirtyNotice(t *testing.T) {
	w := newWorld(t)
	ws := w.workspace()
	if got := dirtyNotice(&Snapshot{Workspace: ws}); got != "" {
		t.Errorf("a workspace that is not a git repository gets no notice: %q", got)
	}
	runGit(t, ws, "init", "-q", "-b", "main")
	write(t, filepath.Join(ws, "a.go"), "package a\n")
	runGit(t, ws, "add", "-A")
	runGit(t, ws, "commit", "-q", "-m", "base")
	if got := dirtyNotice(&Snapshot{Workspace: ws}); got != "" {
		t.Errorf("a clean workspace gets no notice: %q", got)
	}
	write(t, filepath.Join(ws, "b.go"), "package b\n")
	write(t, filepath.Join(ws, "a.go"), "package a // edited\n")
	got := dirtyNotice(&Snapshot{Workspace: ws}, &Snapshot{Workspace: ws})
	if strings.Count(got, "uncommitted") != 1 || !strings.Contains(got, ws) || !strings.Contains(got, "2 uncommitted path(s)") || !strings.Contains(got, "b.go") || !strings.Contains(got, "ask how to proceed") {
		t.Errorf("notice: %q", got)
	}
	if out := w.run("supernova/auth-refactor"); !strings.Contains(out, "2 uncommitted path(s)") {
		t.Errorf("ensign run prints the notice:\n%s", out)
	}
	if out, err := w.seq("run", "supernova/auth-refactor"); err != nil || !strings.Contains(out, "2 uncommitted path(s)") {
		t.Errorf("a sequence step prints the notice: %v\n%s", err, out)
	}
}

func TestSweepRemovesOldDirectFiles(t *testing.T) {
	w := writesWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	old := filepath.Join(dir, "old.id.direct")
	oldFile(t, old, oldAge)
	young := filepath.Join(dir, "young.direct")
	write(t, young, "{}")
	dropDeadLinks(w.bridge)
	if exists(old) {
		t.Error("an old session's .direct must be swept")
	}
	if !exists(young) {
		t.Error("a young session's .direct must stay")
	}
}

func TestDirectEditWindowsCaseAndSlashes(t *testing.T) {
	w, _ := newGuardWorld(t)
	os.MkdirAll(filepath.Dir(directPath(w.bridge, w.sid)), 0o755)
	df := &directFile{Folder: `C:\Work\Code`, Work: "x", Phase: directOpen}
	if err := writeJSON(directPath(w.bridge, w.sid), df); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]bool{`c:/work/code/Sub/main.go`: true, `c:/WORK/CODE`: true, `c:/work/codex/main.go`: false} {
		allowed, held, err := directEdit(w.bridge, w.sid, resolveEditTarget(target, "", "windows"), "windows")
		if err != nil || held != "" || allowed != want {
			t.Errorf("%s: allowed=%v held=%q err=%v, want %v", target, allowed, held, err, want)
		}
	}
}

func TestDirectVoidsApprovedWritesOnTheGoThatOpensIt(t *testing.T) {
	w := writesWorld(t)
	code := w.codeFolder("code")
	w.addWrite(wikiPost, `{"n":1}`)
	w.addWrite(wikiPost, `{"n":2}`)
	w.approveWrites()
	if denied, reason := w.callTool("", "", wikiPost, `{"n":1}`); denied {
		t.Fatalf("the first approved write must pass: %s", reason)
	}
	if _, err := w.direct("ask", code, "x"); err != nil {
		t.Fatal(err)
	}
	w.replyStop("I want to edit "+code, false)
	ctx := w.say("go")
	if !strings.Contains(ctx, "guard is open") || !strings.Contains(ctx, "voided") {
		t.Fatalf("the go that opens the guard must report the voided write: %q", ctx)
	}
	if denied, _ := w.callTool("", "", wikiPost, `{"n":2}`); !denied {
		t.Error("the approval left over from an earlier turn must be void")
	}
}

func TestDirectRefusedWhereAQueuedRunWillStart(t *testing.T) {
	w, _ := newGuardWorld(t)
	f := supernovaFolders{w.codeFolder("client"), w.codeFolder("server"), w.codeFolder("models")}
	holdingRun(t, w, "20260101-000001-client", "s2", f.clientSnap())
	st := &State{Run: "20260101-000001-client", Session: "s2", Status: "pending", Sequence: true, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}
	if err := writeJSON(filepath.Join(runsDir(w.bridge), st.Run, "state.json"), st); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{f.client, f.models} {
		if _, err := w.direct("ask", folder, "x"); err == nil || !strings.Contains(err.Error(), "queued") {
			t.Errorf("ask in %s with a queued run: %v", folder, err)
		}
	}
	other := w.codeFolder("other")
	w.direct("ask", other, "x")
	w.replyStop("shown", false)
	queued := &Snapshot{Workspace: other}
	holdingRun(t, w, "20260101-000002-other", "s2", queued)
	st = &State{Run: "20260101-000002-other", Session: "s2", Status: "pending", Sequence: true, Dispatches: []*Dispatch{}, History: []HistoryEntry{}}
	if err := writeJSON(filepath.Join(runsDir(w.bridge), st.Run, "state.json"), st); err != nil {
		t.Fatal(err)
	}
	if ctx := w.say("go"); !strings.Contains(ctx, "not opened") || !strings.Contains(ctx, "queued") {
		t.Errorf("go with a queued run in the folder: %q", ctx)
	}
	w.expectEdit("", "", filepath.Join(other, "a.go"), true)
}

func TestDirectRefusedWhereALandingIs(t *testing.T) {
	w, _ := newGuardWorld(t)
	ws := w.workspace()
	src := filepath.Join(ws, "src")
	os.MkdirAll(src, 0o755)
	release := holdLanding(t, w, ws)
	if _, err := w.direct("ask", src, "x"); err == nil || !strings.Contains(err.Error(), "landing") {
		t.Errorf("ask below a landing: %v", err)
	}
	release()

	// A landing that starts after go refuses the edit, and a sibling of it stays open.
	parent := filepath.Dir(ws)
	child := filepath.Join(parent, "child")
	sibling := filepath.Join(parent, "sibling")
	os.MkdirAll(child, 0o755)
	os.MkdirAll(sibling, 0o755)
	w.openDirect(parent)
	w.expectEdit("", "", filepath.Join(child, "a.go"), false)
	holdLanding(t, w, child)
	w.expectEdit("", "", filepath.Join(child, "a.go"), true)
	w.expectEdit("", "", filepath.Join(child, "new", "deeper", "a.go"), true)
	w.expectEdit("", "", filepath.Join(sibling, "a.go"), false)

	w.openDirect(src)
	holdLanding(t, w, ws)
	w.expectEdit("", "", filepath.Join(src, "a.go"), true)
	if _, why := w.tryEdit("", "", filepath.Join(src, "new", "a.go")); !strings.Contains(why, "landing") {
		t.Errorf("landing refusal: %s", why)
	}
}

func TestDirectWaitsThroughStatusAndHelp(t *testing.T) {
	w, _ := newGuardWorld(t)
	code := w.codeFolder("code")
	w.direct("ask", code, "x")
	w.replyStop("shown", false)
	w.hook("UserPromptSubmit", map[string]any{"prompt": "status"})
	w.hook("UserPromptSubmit", map[string]any{"prompt": "help direct"})
	if ctx := w.say("go"); !strings.Contains(ctx, "guard is open") {
		t.Errorf("go after status: %q", ctx)
	}
}

func TestDirectRefusedWhereAnUpdateIs(t *testing.T) {
	w, _ := newGuardWorld(t)
	ws := w.workspace()
	src := filepath.Join(ws, "src")
	os.MkdirAll(src, 0o755)
	release := holdUpdate(t, w, ws)
	if _, err := w.direct("ask", src, "x"); err == nil || !strings.Contains(err.Error(), "an update is under way") {
		t.Errorf("ask below an update: %v", err)
	}
	release()

	w.openDirect(src)
	holdUpdate(t, w, ws)
	w.expectEdit("", "", filepath.Join(src, "a.go"), true)
	if _, why := w.tryEdit("", "", filepath.Join(src, "new", "a.go")); !strings.Contains(why, "an update is under way") {
		t.Errorf("update refusal: %s", why)
	}
}
