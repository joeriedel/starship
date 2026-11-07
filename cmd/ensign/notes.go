// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gofrs/flock"
)

// The bridge's notes live as plain Markdown under notes/ and exist only once someone writes one.

// notesIndexLimit caps the byte length of the index block printed at session start. It is one line among those that share the session start budget; see fitSessionLines.
const notesIndexLimit = 120

const notesCommitMessage = "Commit hand edits to the bridge's notes"

// The notes commitNotes returns are fixed text, so the session start text stays bounded; the details are in `ensign journal push` and the Stop hook.
const (
	notesCommitFailed = "starship: couldn't commit hand edits to notes/; the Stop hook will ask"
	notesCommittedFmt = "starship: committed hand edits to notes/ (%d files)."
	notesPushFailed   = " The push failed; run `ensign journal push`."
)

// isNoteFile reports whether a slash-form path is a note: a Markdown file with no hidden path segment, which leaves out .DS_Store, Thumbs.db and editor swap files.
func isNoteFile(rel string) bool {
	if !strings.EqualFold(path.Ext(rel), ".md") {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

func isInboxName(name string) bool {
	return strings.EqualFold(name, "inbox.md")
}

var notesLogMonth = regexp.MustCompile(`^\d{4}-\d{2}\.md$`)

// commitNotes commits files changed by hand under notes/, by name, so the sync that follows sees a clean bridge. When it committed on a bridge with a remote and no tracked changes remain, it also rebases and pushes (a plain fast-forward fails when the remote moved too), and synced is true. Every failure comes back as a note: the Stop hook still catches the uncommitted files.
func commitNotes(bridge string) (note string, synced bool) {
	if !isGit(bridge) || os.MkdirAll(filepath.Join(bridge, ".starship"), 0o755) != nil {
		return "", false
	}
	lock := flock.New(filepath.Join(bridge, ".starship", "notes.lock"))
	if ok, err := lock.TryLock(); err != nil || !ok {
		return "", false
	}
	defer lock.Unlock()
	// A rebase in progress leaves HEAD detached, and a commit there would land in the rebase.
	if _, err := gitOut(bridge, "symbolic-ref", "--short", "HEAD"); err != nil {
		return "", false
	}
	if unmerged, err := unmergedFiles(bridge); err != nil || len(unmerged) > 0 {
		return "", false
	}
	toks, err := gitZ(bridge, "status", "--porcelain", "-z", "--untracked-files=all", "--", "notes")
	if err != nil {
		return "", false
	}
	var files []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		paths := []string{t[3:]}
		// A rename lists both sides; both are committed when either is a note, so the old path is removed.
		if strings.ContainsAny(t[:2], "RC") && i+1 < len(toks) {
			i++
			paths = append(paths, toks[i])
		}
		for _, p := range paths {
			if isNoteFile(p) {
				files = append(files, paths...)
				break
			}
		}
	}
	if len(files) == 0 {
		return "", false
	}
	// add errors on a path that is neither in the worktree nor the index, but commit -- still takes it.
	var addable []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(bridge, filepath.FromSlash(f))); err == nil {
			addable = append(addable, f)
		} else if tracked, _ := gitOut(bridge, append([]string{"ls-files", "--"}, literalPathspecs([]string{f})...)...); tracked != "" {
			addable = append(addable, f)
		}
	}
	if len(addable) > 0 {
		if err := git(bridge, append([]string{"add", "-A", "--"}, literalPathspecs(addable)...)...); err != nil {
			return notesCommitFailed, false
		}
	}
	if err := git(bridge, append([]string{"commit", "-q", "-m", notesCommitMessage, "--"}, literalPathspecs(files)...)...); err != nil {
		return notesCommitFailed, false
	}
	note = fmt.Sprintf(notesCommittedFmt, len(files))
	if remoteName(bridge) == "" {
		return note, false
	}
	// Autostash must not touch another session's in-progress edits, so only a clean tree is rebased.
	if dirty, err := gitOut(bridge, "status", "--porcelain", "--untracked-files=no"); err != nil || dirty != "" {
		return note, false
	}
	if err := pushBridge(bridge, io.Discard); err != nil {
		return note + notesPushFailed, true
	}
	return note, true
}

func notesIndex(bridge string) string {
	entries, err := os.ReadDir(filepath.Join(bridge, "notes"))
	if err != nil {
		return ""
	}
	var items []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isNoteFile("notes/"+name) || isInboxName(name) {
			continue
		}
		items = append(items, name)
	}
	if latest := latestLogMonth(bridge); latest != "" {
		items = append([]string{"log/ (latest " + latest + ")"}, items...)
	}
	if len(items) == 0 {
		return ""
	}
	const head = "\nnotes/ (read what you need): "
	for k := len(items); k >= 0; k-- {
		block := head + strings.Join(items[:k], ", ")
		if k < len(items) {
			if k > 0 {
				block += ", "
			}
			block += fmt.Sprintf("and %d more", len(items)-k)
		}
		if len(block)+1 <= notesIndexLimit || k == 0 {
			return block + "\n"
		}
	}
	return ""
}

// latestLogMonth names the newest YYYY-MM file in notes/log, or "".
func latestLogMonth(bridge string) string {
	entries, err := os.ReadDir(filepath.Join(bridge, "notes", "log"))
	if err != nil {
		return ""
	}
	latest := ""
	for _, e := range entries {
		if !e.IsDir() && notesLogMonth.MatchString(e.Name()) && e.Name() > latest {
			latest = e.Name()
		}
	}
	return strings.TrimSuffix(latest, ".md")
}

func notesInboxLine(bridge string) string {
	entries, err := os.ReadDir(filepath.Join(bridge, "notes"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !isInboxName(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(bridge, "notes", e.Name()))
		if err != nil {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "\ufeff")) != "" {
			return "notes/" + e.Name() + " has unsorted notes: sort them first; see `ensign help notes`.\n"
		}
	}
	return ""
}
