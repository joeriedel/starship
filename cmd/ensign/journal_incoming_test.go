// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pushFromElsewhere commits files in a second clone of the remote and pushes them.
func pushFromElsewhere(t *testing.T, remote string, files map[string]string) {
	t.Helper()
	other := cloneRemote(t, remote)
	runGit(t, other, "config", "user.email", "o@example.com")
	runGit(t, other, "config", "user.name", "o")
	for f, c := range files {
		write(t, filepath.Join(other, filepath.FromSlash(f)), c)
	}
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "elsewhere")
	runGit(t, other, "push", "-q", "origin", "main")
}

func commitLocal(t *testing.T, w *world, rel string) {
	t.Helper()
	write(t, filepath.Join(w.bridge, filepath.FromSlash(rel)), "local\n")
	runGit(t, w.bridge, "add", "-A")
	runGit(t, w.bridge, "commit", "-q", "-m", "local")
}

func incomingWorld(t *testing.T) (*world, string) {
	t.Helper()
	needGit(t)
	w := newWorld(t)
	remote := journalFixture(t, w)
	write(t, sessionFile(w.bridge, w.sid)+".task", "supernova/auth-refactor\n")
	return w, remote
}

func stopReason(out any) string {
	if out == nil {
		return ""
	}
	return fmt.Sprint(out)
}

func TestClassifyBridge(t *testing.T) {
	w, remote := incomingWorld(t)
	if st, _, _, _ := classifyBridge(w.bridge); st != syncInSync {
		t.Fatalf("in sync: got %v", st)
	}
	commitLocal(t, w, "notes/ahead.md")
	if st, _, _, _ := classifyBridge(w.bridge); st != syncAhead {
		t.Fatalf("ahead: got %v", st)
	}
	runGit(t, w.bridge, "push", "-q", "origin", "main")
	// The remote's new head is not in the bridge's objects, so classifying fetches it.
	pushFromElsewhere(t, remote, map[string]string{"notes/behind.md": "x\n"})
	st, _, _, head := classifyBridge(w.bridge)
	if st != syncBehind || head == "" {
		t.Fatalf("behind: got %v %q", st, head)
	}
	if err := git(w.bridge, "cat-file", "-e", head); err != nil {
		t.Fatalf("the remote head should have been fetched: %v", err)
	}
	commitLocal(t, w, "notes/diverged.md")
	if st, _, _, _ := classifyBridge(w.bridge); st != syncDiverged {
		t.Fatalf("diverged: got %v", st)
	}
}

func TestJournalStopFastForwardsABehindBridge(t *testing.T) {
	w, remote := incomingWorld(t)
	pushFromElsewhere(t, remote, map[string]string{"notes/news.md": "x\n"})
	if out := journalStop(w.bridge, &HookInput{SessionID: w.sid}); out != nil {
		t.Fatalf("a bridge that is only behind should not block: %v", out)
	}
	if got := runGit(t, w.bridge, "rev-parse", "HEAD"); got != runGit(t, remote, "rev-parse", "main") {
		t.Errorf("the bridge should have fast-forwarded")
	}
}

func TestJournalStopBehindWithADirtyFileBlocksWithoutFastForwarding(t *testing.T) {
	w, remote := incomingWorld(t)
	pushFromElsewhere(t, remote, map[string]string{"notes/news.md": "x\n"})
	before := runGit(t, w.bridge, "rev-parse", "HEAD")
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "dirty: 1\n")
	reason := stopReason(journalStop(w.bridge, &HookInput{SessionID: w.sid}))
	if !strings.Contains(reason, "behind") || !strings.Contains(reason, "bridge.yaml") || !strings.Contains(reason, "ensign journal push") || !strings.Contains(reason, "Don't commit") || strings.Contains(reason, "ensign journal commit") || strings.Contains(reason, "moved") {
		t.Errorf("got %q", reason)
	}
	if runGit(t, w.bridge, "rev-parse", "HEAD") != before {
		t.Errorf("a dirty bridge must not fast-forward")
	}
	if out := journalStop(w.bridge, &HookInput{SessionID: w.sid, StopHookActive: true}); out != nil {
		t.Errorf("a second stop must not trap the session: %v", out)
	}
}

func TestJournalStopNeverAutoStashes(t *testing.T) {
	w, remote := incomingWorld(t)
	runGit(t, w.bridge, "config", "merge.autoStash", "true")
	pushFromElsewhere(t, remote, map[string]string{"bridge.yaml": "remote: 1\n"})
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "local: 1\n")
	if out := journalStop(w.bridge, &HookInput{SessionID: w.sid}); out == nil {
		t.Errorf("want the dirty-tree block")
	}
	if got, _ := os.ReadFile(filepath.Join(w.bridge, "bridge.yaml")); string(got) != "local: 1\n" {
		t.Errorf("the file was touched: %q", got)
	}
	if stash := runGit(t, w.bridge, "stash", "list"); stash != "" {
		t.Errorf("a stash was left: %q", stash)
	}
}

func TestCompactionKeepsAnIntakeTheSessionWasNotTold(t *testing.T) {
	w, remote := incomingWorld(t)
	in := &HookInput{SessionID: w.sid}
	setIncomingMark(w.bridge, w.sid, incomingEnd(w.bridge))
	pushFromElsewhere(t, remote, map[string]string{"notes/inbox.md": "x\n"})
	// The active stop fast-forwards and logs the intake but does not tell the session.
	if out := journalStop(w.bridge, &HookInput{SessionID: w.sid, StopHookActive: true}); out != nil {
		t.Fatal(out)
	}
	startRaw(t, w, "compact")
	runGit(t, w.bridge, "push", "-q", "origin", "main") // the start's workspace migration commits locally
	if reason := stopReason(journalStop(w.bridge, in)); !strings.Contains(reason, "notes/inbox.md") {
		t.Errorf("got %q", reason)
	}
}

func TestJournalStopDivergedBlocksWithTheRemoteMoved(t *testing.T) {
	w, remote := incomingWorld(t)
	pushFromElsewhere(t, remote, map[string]string{"notes/news.md": "x\n"})
	commitLocal(t, w, "notes/mine.md")
	reason := stopReason(journalStop(w.bridge, &HookInput{SessionID: w.sid}))
	if !strings.Contains(reason, "moved") || !strings.Contains(reason, "ensign journal push") || strings.Contains(reason, "not pushed") {
		t.Errorf("got %q", reason)
	}
	if out := journalStop(w.bridge, &HookInput{SessionID: w.sid, StopHookActive: true}); out != nil {
		t.Errorf("a second stop must not trap the session: %v", out)
	}
}

func TestJournalStopAheadStillBlocks(t *testing.T) {
	w, _ := incomingWorld(t)
	commitLocal(t, w, "notes/mine.md")
	if reason := stopReason(journalStop(w.bridge, &HookInput{SessionID: w.sid})); !strings.Contains(reason, "not pushed") {
		t.Errorf("got %q", reason)
	}
}

func TestPushBridgeReportsWhatItPulled(t *testing.T) {
	w, remote := incomingWorld(t)
	pushFromElsewhere(t, remote, map[string]string{
		"projects/supernova/project.md":      "new\n",
		"projects/pulsar/tasks/x/task.md":    "x\n",
		"projects/pulsar/tasks/y/task.md":    "y\n",
		"notes/inbox.md":                     "hi\n",
		"projects/supernova/tasks/t/task.md": "t\n",
	})
	commitLocal(t, w, "notes/mine.md")
	t.Setenv("STARSHIP_SESSION", w.sid)
	var out bytes.Buffer
	if err := pushBridge(w.bridge, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"took in 1 commit", "project supernova: projects/supernova/project.md, projects/supernova/tasks/t/task.md", "bridge-wide: notes/inbox.md", "2 file(s) in 1 other project(s)", "pushed to origin/main"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestStopTellsTheSessionOnceWhatArrived(t *testing.T) {
	w, remote := incomingWorld(t)
	in := &HookInput{SessionID: w.sid}
	if out := journalStop(w.bridge, in); out != nil {
		t.Fatalf("a session with no mark is not blocked: %v", out)
	}
	if _, ok := readIncomingMark(w.bridge, w.sid); !ok {
		t.Fatal("the stop should set a mark")
	}
	pushFromElsewhere(t, remote, map[string]string{"projects/supernova/tasks/auth-refactor/task.md": "changed\n"})
	reason := stopReason(journalStop(w.bridge, in))
	if !strings.Contains(reason, "Re-read") || !strings.Contains(reason, "projects/supernova/tasks/auth-refactor/task.md") {
		t.Fatalf("got %q", reason)
	}
	if out := journalStop(w.bridge, in); out != nil {
		t.Errorf("the same commits must not block twice: %v", out)
	}
}

func TestStopBlocksForProjectMdAndBridgeWideFiles(t *testing.T) {
	for _, file := range []string{"projects/supernova/project.md", "bridge.yaml", "notes/inbox.md"} {
		t.Run(file, func(t *testing.T) {
			w, remote := incomingWorld(t)
			in := &HookInput{SessionID: w.sid}
			setIncomingMark(w.bridge, w.sid, incomingEnd(w.bridge))
			pushFromElsewhere(t, remote, map[string]string{file: "changed\n"})
			if reason := stopReason(journalStop(w.bridge, in)); !strings.Contains(reason, file) {
				t.Errorf("got %q", reason)
			}
		})
	}
}

func TestStopDoesNotBlockForOtherProjectsOrUntaskedProjectFiles(t *testing.T) {
	w, remote := incomingWorld(t)
	in := &HookInput{SessionID: w.sid}
	setIncomingMark(w.bridge, w.sid, incomingEnd(w.bridge))
	pushFromElsewhere(t, remote, map[string]string{"projects/pulsar/tasks/x/task.md": "x\n", "projects/supernova/notes-extra.txt": "x\n"})
	out := journalStop(w.bridge, in)
	if m, ok := out.(map[string]any); !ok || m["decision"] != nil || !strings.Contains(fmt.Sprint(m["systemMessage"]), "1 file(s) in 1 other project(s)") {
		t.Errorf("want a mention without a block, got %v", out)
	}
	if out := journalStop(w.bridge, in); out != nil {
		t.Errorf("the mention is made once: %v", out)
	}
	// With no task, project files count as other projects, but bridge-wide files still block.
	write(t, sessionFile(w.bridge, w.sid)+".task", "")
	pushFromElsewhere(t, remote, map[string]string{"projects/supernova/project.md": "x\n"})
	if m, _ := journalStop(w.bridge, in).(map[string]any); m == nil || m["decision"] != nil {
		t.Errorf("want a mention without a block, got %v", m)
	}
	pushFromElsewhere(t, remote, map[string]string{"bridge.yaml": "x: 1\n"})
	if reason := stopReason(journalStop(w.bridge, in)); !strings.Contains(reason, "bridge.yaml") {
		t.Errorf("got %q", reason)
	}
}

func TestSyncBridgeReportsWhatArrivedAndSessionStartMarksIt(t *testing.T) {
	w, remote := incomingWorld(t)
	pushFromElsewhere(t, remote, map[string]string{"notes/news.md": "x\n", "projects/pulsar/project.md": "x\n"})
	got := syncBridge(w.bridge, w.sid)
	if !strings.Contains(got, "took in 1 commit") || !strings.Contains(got, "bridge-wide: notes/news.md") || !strings.Contains(got, "1 file(s) in 1 other project(s)") {
		t.Errorf("got %q", got)
	}
	_, ctx := startRaw(t, w, "startup")
	if mark, ok := readIncomingMark(w.bridge, w.sid); !ok || mark != incomingEnd(w.bridge) {
		t.Errorf("the session start should mark the log's end: %d %v", mark, ok)
	}
	_ = ctx
}

func TestIncomingLogIsCapped(t *testing.T) {
	w, _ := incomingWorld(t)
	for i := 0; i < incomingLogMax+10; i++ {
		appendIncoming(w.bridge, fmt.Sprintf("%d a b\n", i+1))
	}
	if n := len(readIncomingLog(w.bridge)); n > incomingLogMax || n < incomingLogKeep {
		t.Errorf("log has %d lines", n)
	}
	if incomingEnd(w.bridge) != int64(incomingLogMax+10) {
		t.Errorf("the newest line must survive: %d", incomingEnd(w.bridge))
	}
}
