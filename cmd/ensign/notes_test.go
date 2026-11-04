// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

// startRaw runs SessionStart and returns the hook's stdout and its context.
func startRaw(t *testing.T, w *world, source string) (string, string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "transcript_path": w.session, "session_id": w.sid, "source": source})
	var out, errb bytes.Buffer
	if code := cmdHook("SessionStart", bytes.NewReader(data), &out, &errb); code != 0 {
		t.Fatalf("SessionStart exited %d: %s", code, errb.String())
	}
	_, ctx := sessionStartReply(t, out.Bytes())
	return out.String(), ctx
}

func notesPath(w *world, rel string) string {
	return filepath.Join(w.bridge, filepath.FromSlash(rel))
}

func committedNotes(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "log", "-1", "--grep="+notesCommitMessage, "--name-only", "--format=")
}

func TestJournalCoversNotes(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	journalFixture(t, w)
	write(t, notesPath(w, "notes/profile.md"), "Maintainer of supernova.\n")
	changed, err := journalChanges(w.bridge)
	if err != nil || strings.Join(changed, ",") != "notes/profile.md" {
		t.Fatalf("new file: %v %v", changed, err)
	}
	out := journalStop(w.bridge, &HookInput{})
	if out == nil || !strings.Contains(fmt.Sprint(out), "notes/profile.md") {
		t.Fatalf("the stop should block on an uncommitted note: %v", out)
	}
	commit := func() {
		t.Helper()
		var buf bytes.Buffer
		wd, _ := os.Getwd()
		defer os.Chdir(wd)
		os.Chdir(w.bridge)
		if err := cmdJournal([]string{"commit", "-m", "Notes"}, &buf); err != nil {
			t.Fatalf("journal commit: %v\n%s", err, buf.String())
		}
		if st := runGit(t, w.bridge, "status", "--porcelain", "--", "notes"); st != "" {
			t.Fatalf("notes not committed:\n%s", st)
		}
	}
	commit()
	write(t, notesPath(w, "notes/profile.md"), "Maintainer of supernova, since 2026.\n")
	if changed, _ := journalChanges(w.bridge); strings.Join(changed, ",") != "notes/profile.md" {
		t.Fatalf("edit: %v", changed)
	}
	commit()
	os.Remove(notesPath(w, "notes/profile.md"))
	if changed, _ := journalChanges(w.bridge); strings.Join(changed, ",") != "notes/profile.md" {
		t.Fatalf("deletion: %v", changed)
	}
	commit()
}

func TestNoNotesChangesNothing(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	journalFixture(t, w)
	_, ctx := startRaw(t, w, "startup")
	if notesContext(w.bridge) != "" || strings.Contains(ctx, "unsorted") || strings.Contains(ctx, "hand edits") {
		t.Errorf("unexpected notes output:\n%s", ctx)
	}
	if got := committedNotes(t, w.bridge); got != "" {
		t.Errorf("no commit expected, got %s", got)
	}
	// A notes file is not a folder to index.
	write(t, notesPath(w, "notes"), "x")
	if notesContext(w.bridge) != "" {
		t.Errorf("a notes file should give no index")
	}
}

func TestHandEditCommittedAndPushedAtStart(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	remote := journalFixture(t, w)
	write(t, notesPath(w, "notes/profile.md"), "Maintainer of supernova.\n")
	write(t, notesPath(w, "notes/log/2026-11.md"), "- 2026-11-04 shipped\n")
	write(t, notesPath(w, "stray.txt"), "untouched\n")
	runGit(t, w.bridge, "add", "-N", "stray.txt")
	runGit(t, w.bridge, "reset", "-q", "stray.txt")
	out, ctx := startRaw(t, w, "startup")
	if !json.Valid([]byte(out)) {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	if !strings.Contains(ctx, "committed hand edits to notes/ (2 files)") {
		t.Errorf("missing commit note:\n%s", ctx)
	}
	got := strings.Fields(committedNotes(t, w.bridge))
	if strings.Join(got, ",") != "notes/log/2026-11.md,notes/profile.md" {
		t.Errorf("commit paths: %v", got)
	}
	if st := runGit(t, w.bridge, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(st, "?? stray.txt") {
		t.Errorf("the unrelated file should stay untracked:\n%s", st)
	}
	if files := runGit(t, remote, "ls-tree", "-r", "--name-only", "HEAD"); !strings.Contains(files, "notes/profile.md") {
		t.Errorf("not pushed:\n%s", files)
	}
}

func TestHandEditWithRemoteInboxEdit(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, "notes/inbox.md"), "buy milk for supernova\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "phone")
	runGit(t, other, "push", "-q")
	write(t, notesPath(w, "notes/people.md"), "Ada: reviewer.\n")
	_, ctx := startRaw(t, w, "startup")
	data, err := os.ReadFile(notesPath(w, "notes/inbox.md"))
	if err != nil || !strings.Contains(string(data), "buy milk") {
		t.Fatalf("the inbox should have arrived: %v %q", err, data)
	}
	if !strings.Contains(ctx, "notes/inbox.md has unsorted notes") || !strings.Contains(ctx, "people.md") {
		t.Errorf("inbox line or index missing:\n%s", ctx)
	}
	if strings.Contains(strings.Split(ctx, "STARSHIP-READY")[0], "diverged") {
		t.Errorf("sync note unexpected:\n%s", ctx)
	}
}

func TestHandEditWithOtherTrackedFileDirty(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	write(t, filepath.Join(other, "elsewhere.md"), "remote\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "remote")
	runGit(t, other, "push", "-q")
	tracked := "projects/supernova/project.yaml"
	if _, err := os.Stat(notesPath(w, tracked)); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(notesPath(w, tracked), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("# in progress\n")
	f.Close()
	before, _ := os.ReadFile(notesPath(w, tracked))
	write(t, notesPath(w, "notes/profile.md"), "x\n")
	_, ctx := startRaw(t, w, "startup")
	if committedNotes(t, w.bridge) != "notes/profile.md" {
		t.Errorf("the note should be committed")
	}
	after, _ := os.ReadFile(notesPath(w, tracked))
	if string(before) != string(after) {
		t.Errorf("the dirty file changed")
	}
	if runGit(t, w.bridge, "stash", "list") != "" {
		t.Errorf("nothing should be autostashed")
	}
	if !strings.Contains(ctx, "local changes") {
		t.Errorf("the sync note should say why it did not update:\n%s", ctx)
	}
	if _, err := os.Stat(notesPath(w, "elsewhere.md")); err == nil {
		t.Errorf("the bridge should not have been rebased")
	}
}

func TestHandEditSkips(t *testing.T) {
	needGit(t)
	t.Run("compact", func(t *testing.T) {
		w := linkedWorld(t)
		journalFixture(t, w)
		write(t, notesPath(w, "notes/profile.md"), "x\n")
		startRaw(t, w, "compact")
		if committedNotes(t, w.bridge) != "" {
			t.Errorf("a compaction must not commit")
		}
	})
	t.Run("detached", func(t *testing.T) {
		w := linkedWorld(t)
		journalFixture(t, w)
		runGit(t, w.bridge, "checkout", "-q", "--detach")
		write(t, notesPath(w, "notes/profile.md"), "x\n")
		startRaw(t, w, "startup")
		if committedNotes(t, w.bridge) != "" {
			t.Errorf("a detached HEAD must not get a commit")
		}
	})
	t.Run("locked", func(t *testing.T) {
		w := linkedWorld(t)
		journalFixture(t, w)
		write(t, notesPath(w, "notes/profile.md"), "x\n")
		l := flock.New(filepath.Join(w.bridge, ".starship", "notes.lock"))
		if ok, err := l.TryLock(); !ok || err != nil {
			t.Fatalf("lock: %v %v", ok, err)
		}
		defer l.Unlock()
		startRaw(t, w, "startup")
		if committedNotes(t, w.bridge) != "" {
			t.Errorf("a held lock must skip")
		}
	})
	t.Run("not git", func(t *testing.T) {
		w := linkedWorld(t)
		write(t, notesPath(w, "notes/profile.md"), "x\n")
		out, ctx := startRaw(t, w, "startup")
		if !json.Valid([]byte(out)) || strings.Contains(ctx, "hand edits") {
			t.Errorf("unexpected: %q", ctx)
		}
		if !strings.Contains(ctx, "profile.md") {
			t.Errorf("the index should still print:\n%s", ctx)
		}
	})
}

func TestNotesIndex(t *testing.T) {
	w := newWorld(t)
	write(t, notesPath(w, "notes/people.md"), "x")
	write(t, notesPath(w, "notes/profile.md"), "x")
	write(t, notesPath(w, "notes/inbox.md"), "")
	write(t, notesPath(w, "notes/readme.txt"), "x")
	write(t, notesPath(w, "notes/.#profile.md"), "x")
	write(t, notesPath(w, "notes/._profile.md"), "x")
	write(t, notesPath(w, "notes/log/2026-09.md"), "x")
	write(t, notesPath(w, "notes/log/2026-11.md"), "x")
	write(t, notesPath(w, "notes/log/draft.md"), "x")
	got := notesIndex(w.bridge)
	if want := "\nnotes/ (read what you need): log/ (latest 2026-11), people.md, profile.md\n"; got != want {
		t.Errorf("index %q, want %q", got, want)
	}
	if strings.Contains(got, w.bridge) {
		t.Errorf("the index must not print the bridge path")
	}

	w = newWorld(t)
	for i := 0; i < 40; i++ {
		write(t, notesPath(w, fmt.Sprintf("notes/topic-%02d-with-a-long-name.md", i)), "x")
	}
	got = notesIndex(w.bridge)
	var more int
	idx := strings.LastIndex(got, "and ")
	if len(got) > notesIndexLimit || idx < 0 {
		t.Fatalf("cap broken (%d bytes): %q", len(got), got)
	}
	fmt.Sscanf(got[idx:], "and %d more", &more)
	if shown := strings.Count(got, ".md"); shown+more != 40 || !strings.Contains(got, "topic-00-with-a-long-name.md") {
		t.Errorf("shown %d + %d more != 40: %q", shown, more, got)
	}
	for _, name := range strings.Split(strings.TrimSuffix(strings.SplitN(got, ": ", 2)[1], "\n"), ", ") {
		if !strings.HasPrefix(name, "topic-") && !strings.HasPrefix(name, "and ") || strings.HasPrefix(name, "topic-") && !strings.HasSuffix(name, ".md") {
			t.Errorf("cut mid-name: %q", name)
		}
	}

	w = newWorld(t)
	write(t, notesPath(w, "notes/inbox.md"), "\n  \n")
	if notesContext(w.bridge) != "" {
		t.Errorf("a blank inbox alone should give nothing: %q", notesContext(w.bridge))
	}
}

func TestNotesIndexAfterCompactAndClear(t *testing.T) {
	for _, source := range []string{"compact", "clear", "startup", "resume"} {
		w := linkedWorld(t)
		write(t, notesPath(w, "notes/profile.md"), "x")
		_, ctx := startRaw(t, w, source)
		if !strings.Contains(ctx, "profile.md") {
			t.Errorf("%s: index missing:\n%s", source, ctx)
		}
		if strings.Index(ctx, "profile.md") < strings.Index(ctx, "Reading more") {
			t.Errorf("%s: the index should follow the manual", source)
		}
	}
}

func TestNotesInboxLine(t *testing.T) {
	for _, c := range []struct {
		name, content string
		want          bool
	}{
		{"blank", "", false},
		{"whitespace", " \n\t\r\n", false},
		{"bom only", "\ufeff", false},
		{"bom and whitespace", "\ufeff \n", false},
		{"text", "call Ada\n", true},
		{"bom text", "\ufeffcall Ada\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			write(t, notesPath(w, "notes/inbox.md"), c.content)
			got := notesInboxLine(w.bridge)
			if (got != "") != c.want || got != "" && !strings.Contains(got, "ensign help notes") {
				t.Errorf("got %q", got)
			}
		})
	}
	w := newWorld(t)
	if notesInboxLine(w.bridge) != "" {
		t.Errorf("a missing inbox gives no line")
	}
}

func TestNotesEditGuard(t *testing.T) {
	w, _ := newGuardWorld(t)
	w.expectEdit("", "", filepath.Join(w.bridge, "notes/x.md"), false)
	w.expectEdit("a1", "coder", filepath.Join(w.bridge, "notes/x.md"), true)
}

func TestFailedCommitNoteIsShortAndNamesNoFiles(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	journalFixture(t, w)
	runGit(t, w.bridge, "config", "user.useConfigOnly", "true")
	runGit(t, w.bridge, "config", "--unset", "user.email")
	runGit(t, w.bridge, "config", "--unset", "user.name")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for i := 0; i < 100; i++ {
		write(t, notesPath(w, fmt.Sprintf("notes/topic-%03d-with-a-long-name.md", i)), "x\n")
	}
	note, synced := commitNotes(w.bridge)
	if note != notesCommitFailed || synced {
		t.Fatalf("note %q synced %v, want the fixed failure note", note, synced)
	}
	if strings.Contains(note, "topic-") || strings.Contains(note, "git ") {
		t.Errorf("the note leaks file names or git output: %q", note)
	}
}

func TestJunkInNotesIsNotCommitted(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	journalFixture(t, w)
	for _, junk := range []string{"notes/.DS_Store", "notes/Thumbs.db", "notes/desktop.ini", "notes/.profile.md.swp", "notes/.hidden/x.md", "notes/readme.txt"} {
		write(t, notesPath(w, junk), "junk\n")
	}
	if changed, err := journalChanges(w.bridge); err != nil || len(changed) != 0 {
		t.Fatalf("junk listed: %v %v", changed, err)
	}
	if out := journalStop(w.bridge, &HookInput{}); out != nil {
		t.Errorf("the stop should not block on junk: %v", out)
	}
	write(t, notesPath(w, "notes/Profile.MD"), "Maintainer of supernova.\n")
	if changed, _ := journalChanges(w.bridge); strings.Join(changed, ",") != "notes/Profile.MD" {
		t.Errorf("changed %v", changed)
	}
	startRaw(t, w, "startup")
	if got := strings.Fields(committedNotes(t, w.bridge)); strings.Join(got, ",") != "notes/Profile.MD" {
		t.Errorf("commit paths: %v", got)
	}
	if st := runGit(t, w.bridge, "status", "--porcelain", "--untracked-files=all", "--", "notes"); !strings.Contains(st, ".DS_Store") {
		t.Errorf("junk should stay untracked:\n%s", st)
	}
}

func TestNotesInboxCaseVariant(t *testing.T) {
	w := newWorld(t)
	write(t, notesPath(w, "notes/Inbox.md"), "call Ada\n")
	ctx := notesContext(w.bridge)
	if !strings.Contains(ctx, "notes/Inbox.md has unsorted notes") || strings.Contains(ctx, "notes/ (read") {
		t.Errorf("want an inbox line naming Inbox.md and no index entry: %q", ctx)
	}
}

func TestNotesRenameAcrossFilterCommitsBothSides(t *testing.T) {
	needGit(t)
	for _, c := range []struct{ from, to string }{{"notes/todo.txt", "notes/todo.md"}, {"notes/a.md", "notes/a.txt"}} {
		w := linkedWorld(t)
		journalFixture(t, w)
		write(t, notesPath(w, c.from), "supernova\n")
		runGit(t, w.bridge, "add", "-f", c.from)
		runGit(t, w.bridge, "commit", "-q", "-m", "seed")
		runGit(t, w.bridge, "mv", c.from, c.to)
		changed, err := journalChanges(w.bridge)
		if err != nil || len(changed) != 2 {
			t.Fatalf("%s to %s: journalChanges %v %v, want both sides", c.from, c.to, changed, err)
		}
		if note, _ := commitNotes(w.bridge); note == "" || note == notesCommitFailed {
			t.Fatalf("%s to %s: note %q", c.from, c.to, note)
		}
		if st := runGit(t, w.bridge, "status", "--porcelain", "--untracked-files=all", "--", "notes"); strings.TrimSpace(st) != "" {
			t.Errorf("%s to %s: notes/ not clean:\n%s", c.from, c.to, st)
		}
	}
}
