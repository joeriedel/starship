// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"
)

func linkedWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	return w
}

// contextChars counts s the way Claude Code's limit on additionalContext is most strictly read. The limit is in characters and the docs don't say whether those are code points or UTF-16 code units; code units are the higher count, and bytes would overstate non-ASCII text.
func contextChars(s string) int { return len(utf16.Encode([]rune(s))) }

// brokenRoleFixture writes n broken extensions with a role each. A long unknown key puts a long error text in each line.
func brokenRoleFixture(t *testing.T, bridge string, n int, key string) {
	t.Helper()
	for i := 0; i < n; i++ {
		folder := fmt.Sprintf("supernova-broken-%d", i)
		write(t, filepath.Join(bridge, "extensions", folder, "extension.yaml"), fmt.Sprintf("name: %s\n%s: 1\n", folder, key))
		write(t, filepath.Join(bridge, "extensions", folder, "roles", "supernova-role.md"), "---\nname: supernova-role\n---\nx\n")
	}
}

// paddedChars is contextChars with the bridge path counted as if it were 200 characters long. A broken-extension line is cut at brokenRoleLineLimit, so a longer path cannot lengthen it.
func paddedChars(ctx, bridge string) int {
	size := contextChars(ctx)
	if n := contextChars(bridge); n < 200 {
		for _, l := range strings.Split(ctx, "\n") {
			if !strings.HasPrefix(l, "Note: extension supernova-broken-") {
				size += (200 - n) * strings.Count(l, bridge)
			}
		}
	}
	return size
}

func (w *world) skillCmd(args ...string) string {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	cmdSkill(args, &out)
	return out.String()
}

func TestTopicDeliveredOncePerSession(t *testing.T) {
	w := linkedWorld(t)
	const marker = "# Sequences"
	if out, _ := w.seq(); !strings.Contains(out, marker) {
		t.Fatalf("first use should print the topic:\n%s", out)
	}
	if out, _ := w.seq(); strings.Contains(out, marker) {
		t.Errorf("second use should not print the topic")
	}
	t.Setenv("STARSHIP_SESSION", "s2")
	if out, _ := w.seq(); !strings.Contains(out, marker) {
		t.Errorf("a new session should get the topic again")
	}
}

func TestTopicEverySkillCallWithoutSession(t *testing.T) {
	w := linkedWorld(t)
	t.Setenv("STARSHIP_SESSION", "")
	for i := 0; i < 2; i++ {
		if out := w.skillCmd("discard", "nothing"); !strings.Contains(out, "# Facts with no skill") {
			t.Errorf("call %d should print the topic:\n%s", i, out)
		}
	}
	t.Setenv("STARSHIP_SESSION", "s1")
	w.skillCmd("discard", "nothing")
	if out := w.skillCmd("discard", "nothing"); strings.Contains(out, "# Facts with no skill") {
		t.Errorf("a session should get the topic once")
	}
}

func TestTopicMissingIsReported(t *testing.T) {
	w := newWorld(t)
	out, _ := w.seq()
	if !strings.Contains(out, "ensign link") {
		t.Errorf("a missing topic should say to link:\n%s", out)
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s1") + ".topic-sequence"); err == nil {
		t.Errorf("a missing topic should not be recorded as delivered")
	}
}

func TestHelpCommand(t *testing.T) {
	w := linkedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdHelp([]string{"nope"}, &out)
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "project, requests, runs, sequence, skill") {
		t.Errorf("unknown topic should list topics: %v", err)
	}
	if err := cmdHelp([]string{"../session"}, &out); err == nil {
		t.Errorf("a path is not a topic")
	}
	if err := cmdHelp(nil, &out); err != nil || !strings.Contains(out.String(), "sequence") {
		t.Errorf("no topic should list them: %v %q", err, out.String())
	}
}

func TestManualTopicsExistAndPrint(t *testing.T) {
	w := linkedWorld(t)
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "manual", "session.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile("ensign help ([a-z-]+)").FindAllStringSubmatch(string(data), -1)
	if len(named) == 0 {
		t.Fatal("the manual names no topics")
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	for _, m := range named {
		if _, err := os.Stat(filepath.Join(root, "manual", "topics", m[1]+".md")); err != nil {
			t.Errorf("manual names topic %s: %v", m[1], err)
		}
		var out bytes.Buffer
		if err := cmdHelp([]string{m[1]}, &out); err != nil || out.Len() == 0 {
			t.Errorf("help %s: %v", m[1], err)
		}
	}
}

func TestSessionStartClearsTopicsOnlyAfterCompactOrClear(t *testing.T) {
	const marker = "# Sequences"
	for _, c := range []struct {
		source string
		again  bool
	}{{"compact", true}, {"clear", true}, {"resume", false}, {"startup", false}} {
		t.Run(c.source, func(t *testing.T) {
			w := linkedWorld(t)
			if out, _ := w.seq(); !strings.Contains(out, marker) {
				t.Fatalf("first use should print the topic:\n%s", out)
			}
			w.hook("SessionStart", map[string]any{"source": c.source})
			if out, _ := w.seq(); strings.Contains(out, marker) != c.again {
				t.Errorf("after %s, topic printed again = %v, want %v:\n%s", c.source, !c.again, c.again, out)
			}
		})
	}
}

func TestCompactLeavesOtherSessionFiles(t *testing.T) {
	w := linkedWorld(t)
	w.seq()
	t.Setenv("STARSHIP_SESSION", "s2")
	w.seq()
	for _, suffix := range []string{"", ".task", ".sequence"} {
		writeFile(sessionFile(w.bridge, w.sid)+suffix, "x")
	}
	if err := os.MkdirAll(filepath.Join(runsDir(w.bridge), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.hook("SessionStart", map[string]any{"source": "compact"})
	for _, f := range []string{
		sessionFile(w.bridge, "s2") + ".topic-sequence",
		sessionFile(w.bridge, w.sid),
		sessionFile(w.bridge, w.sid) + ".pin",
		sessionFile(w.bridge, w.sid) + ".task",
		sessionFile(w.bridge, w.sid) + ".sequence",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s should survive a compaction: %v", filepath.Base(f), err)
		}
	}
	if _, err := os.Stat(sessionFile(w.bridge, w.sid) + ".topic-sequence"); err == nil {
		t.Errorf("the compacted session's topic marker should be gone")
	}
}

func (w *world) startWithNotes(source, notes string) string {
	w.t.Helper()
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	if notes != "" {
		write(w.t, filepath.Join(w.bridge, "projects/supernova/project.md"), notes)
	}
	var open bytes.Buffer
	if err := openTask(w.bridge, "supernova/auth-refactor", taskDir, &open); err != nil {
		w.t.Fatal(err)
	}
	res, _ := w.hook("SessionStart", map[string]any{"source": source})
	spec, _ := res["hookSpecificOutput"].(map[string]any)
	ctx, _ := spec["additionalContext"].(string)
	return ctx
}

func TestSessionStartReprintsNotesAfterCompactOrClear(t *testing.T) {
	const notes = "Always answer in haiku.\n"
	for _, c := range []struct {
		source string
		want   bool
	}{{"compact", true}, {"clear", true}, {"resume", false}, {"startup", false}} {
		t.Run(c.source, func(t *testing.T) {
			w := linkedWorld(t)
			ctx := w.startWithNotes(c.source, notes)
			if got := strings.Contains(ctx, "Always answer in haiku."); got != c.want {
				t.Fatalf("notes reprinted = %v, want %v:\n%s", got, c.want, ctx)
			}
			if c.want {
				block, _ := projectNotes(w.bridge, "supernova")
				if !strings.HasSuffix(ctx, block) || contextChars(ctx) >= sessionContextLimit {
					t.Errorf("notes should come last, under the limit (%d):\n%s", contextChars(ctx), ctx)
				}
				if strings.Index(ctx, "STARSHIP-READY") > strings.Index(ctx, "notes      projects/supernova/project.md") {
					t.Errorf("notes should follow the manual")
				}
			}
		})
	}
}

func TestSessionStartPointsAtNotesTooLongToReprint(t *testing.T) {
	w := linkedWorld(t)
	body := strings.Repeat("Prefer small commits and explain each one.\n", 400)
	ctx := w.startWithNotes("compact", body)
	if strings.Contains(ctx, "Prefer small commits") {
		t.Errorf("long notes should not be reprinted")
	}
	if !strings.Contains(ctx, "`ensign task open supernova/auth-refactor`") {
		t.Errorf("want a pointer to task open:\n%s", ctx)
	}
	if contextChars(ctx) >= sessionContextLimit {
		t.Errorf("context is %d characters", contextChars(ctx))
	}
}

func TestSessionStartReprintsNothingWithoutTaskOrNotes(t *testing.T) {
	w := linkedWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/project.md"), "Always answer in haiku.\n")
	res, _ := w.hook("SessionStart", map[string]any{"source": "compact"})
	spec, _ := res["hookSpecificOutput"].(map[string]any)
	if ctx, _ := spec["additionalContext"].(string); strings.Contains(ctx, "haiku") {
		t.Errorf("no recorded task, no notes:\n%s", ctx)
	}
	w = linkedWorld(t)
	if ctx := w.startWithNotes("compact", ""); strings.Contains(ctx, "notes      ") {
		t.Errorf("no project.md, no notes:\n%s", ctx)
	}
}

func TestSessionStartPointsAtRequestsTopic(t *testing.T) {
	w := linkedWorld(t)
	ctx := w.startWithNotes("startup", "")
	if !strings.Contains(ctx, "`ensign help requests`") || strings.Contains(ctx, "ensign git-catalog squash") {
		t.Errorf("want a pointer to the requests topic and not the list:\n%s", ctx)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdHelp([]string{"requests"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != requestCommands(w.bridge) || !strings.Contains(out.String(), "ensign git-catalog squash") {
		t.Errorf("requests topic should print the entries:\n%s", out.String())
	}
}

// sessionStartRoom is the room kept free under manualLimit for lines the budget tests can't foresee.
const sessionStartRoom = 400

// The notes commitNotes returns and the sync note can both reach the context; the worst of them is added to a full session start.
func TestSessionStartFitsBudgetWithWorstNotes(t *testing.T) {
	needGit(t)
	w := linkedWorld(t)
	// The focal worst case is committed with the bridge, so the commit note still counts only the 42 notes files.
	yaml := "use: [focal-sheet]\ndocuments:\n"
	for i := 0; i < 6; i++ {
		yaml += fmt.Sprintf("  supernova-focal-%d-%s:\n    path: notes/reviews/%s-%d.md\n", i, strings.Repeat("a", 300), strings.Repeat("b", 300), i)
	}
	bridgeYAML := filepath.Join(w.bridge, "bridge.yaml")
	data, err := os.ReadFile(bridgeYAML)
	if err != nil {
		t.Fatal(err)
	}
	write(t, bridgeYAML, string(data)+yaml)
	journalFixture(t, w)
	write(t, filepath.Join(w.bridge, "notes", "log", "2026-11.md"), "x\n")
	for i := 0; i < 40; i++ {
		write(t, filepath.Join(w.bridge, "notes", fmt.Sprintf("topic-%02d.md", i)), "x\n")
	}
	write(t, filepath.Join(w.bridge, "notes", "inbox.md"), "call Ada\n")
	brokenRoleFixture(t, w.bridge, brokenRoleLineCount+2, strings.Repeat("k", 2*brokenRoleLineLimit))
	ctx := w.startWithNotes("clear", strings.Repeat("Prefer small commits and explain each one.\n", 400))
	for _, want := range []string{"too long to reprint", "and ", " more", "unsorted notes", "sits outside", " more like it.", "2 more broken."} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("the worst case should hold %q:\n%s", want, ctx)
		}
	}
	committed := fmt.Sprintf(notesCommittedFmt, math.MaxInt) + "\n"
	statusRead := fmt.Sprintf(syncStatusFmt, "exit status 255") + "\n"
	syncWorst := max(len(syncLocalChangesNote)+1, len(syncDivergedNote)+1, len(statusRead))
	// A push that fails returns synced, so main skips syncBridge and no sync note follows notesPushFailed.
	worst := max(len(notesCommitFailed)+1+syncWorst, len(committed)+syncWorst, len(committed)+len(notesPushFailed))
	size := paddedChars(ctx, w.bridge) - len(fmt.Sprintf(notesCommittedFmt, 42)+"\n") + worst
	t.Logf("padded size %d, limit %d, room %d", size, manualLimit, sessionStartRoom)
	if size > manualLimit-sessionStartRoom {
		t.Errorf("worst case session start text is %d characters with a 200-character bridge path; the budget is %d less %d of room", size, manualLimit, sessionStartRoom)
	}
}

// Claude Code cuts additionalContext at 10,000 characters; the bridge path is repeated in the text, so the budget is checked as if the path were 200 characters long.
func TestSessionStartFitsBudgetWithLongBridgePath(t *testing.T) {
	for _, c := range []struct {
		name, source, notes string
		seed, broken        bool
		want                []string
	}{
		{"startup", "startup", "", false, false, nil},
		{"full notes block", "startup", "", true, false, []string{"and ", " more", "unsorted notes", "committed hand edits"}},
		{"notes too long to reprint", "compact", strings.Repeat("Prefer small commits and explain each one.\n", 400), false, true, []string{"too long to reprint", "2 more broken."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := linkedWorld(t)
			if c.seed {
				needGit(t)
				journalFixture(t, w)
				for i := 0; i < 40; i++ {
					write(t, filepath.Join(w.bridge, "notes", fmt.Sprintf("topic-%02d-with-a-long-name.md", i)), "x\n")
				}
				write(t, filepath.Join(w.bridge, "notes", "inbox.md"), "call Ada\n")
			}
			if c.broken {
				brokenRoleFixture(t, w.bridge, brokenRoleLineCount+2, strings.Repeat("k", 2*brokenRoleLineLimit))
			}
			ctx := w.startWithNotes(c.source, c.notes)
			for _, want := range c.want {
				if !strings.Contains(ctx, want) {
					t.Fatalf("the worst case should hold %q:\n%s", want, ctx)
				}
			}
			size := paddedChars(ctx, w.bridge)
			t.Logf("padded size %d, limit %d, room %d", size, manualLimit, sessionStartRoom)
			if size > manualLimit-sessionStartRoom {
				t.Errorf("session start text is %d characters with a 200-character bridge path; the budget is %d less %d of room", size, manualLimit, sessionStartRoom)
			}
		})
	}
}

// Notes that fit under sessionContextLimit are reprinted whole even when that takes the text past manualLimit; one byte more and the pointer replaces them.
func TestSessionStartReprintsNotesUpToTheLimit(t *testing.T) {
	for _, source := range []string{"compact", "clear"} {
		t.Run(source, func(t *testing.T) {
			w := linkedWorld(t)
			notesOf := func(n int) string { return strings.Repeat("a", n-1) + "\n" }
			const small = 100
			rest := len(w.startWithNotes(source, notesOf(small))) - small
			fit := w.startWithNotes(source, notesOf(sessionContextLimit-1-rest))
			block, _ := projectNotes(w.bridge, "supernova")
			if len(fit) != sessionContextLimit-1 || len(fit) <= manualLimit || !strings.HasSuffix(fit, block) || strings.Contains(fit, "too long to reprint") {
				t.Fatalf("notes one byte under the limit should be reprinted whole: %d bytes, limit %d, manual %d", len(fit), sessionContextLimit, manualLimit)
			}
			over := w.startWithNotes(source, notesOf(sessionContextLimit-rest))
			if !strings.Contains(over, "too long to reprint") || strings.Contains(over, "aaaa") || contextChars(over) > manualLimit-sessionStartRoom {
				t.Errorf("notes at the limit should give the pointer and fit the budget: %d characters", contextChars(over))
			}
		})
	}
}

// The reprint check compares bytes, so non-ASCII notes that are under the limit in characters still get the pointer.
func TestSessionStartCountsBytesForNotes(t *testing.T) {
	w := linkedWorld(t)
	const small = 100
	rest := len(w.startWithNotes("compact", strings.Repeat("a", small-1)+"\n")) - small
	ctx := w.startWithNotes("compact", strings.Repeat("é", sessionContextLimit-rest-2)+"\n")
	pointer := "\nnotes      projects/supernova/project.md is too long to reprint"
	i := strings.Index(ctx, pointer)
	if i < 0 {
		t.Fatalf("want the pointer, not the notes")
	}
	block, _ := projectNotes(w.bridge, "supernova")
	if bytes, chars := i+len(block), contextChars(ctx[:i])+contextChars(block); bytes < sessionContextLimit || chars >= sessionContextLimit {
		t.Errorf("fixture should pass the limit in bytes (%d) and not in characters (%d)", bytes, chars)
	}
}

// Text outside the ASCII range and outside the Basic Multilingual Plane reaches session start through document paths and an extension's error text, and the budget still holds when counted in UTF-16 code units.
func TestSessionStartFitsBudgetWithNonASCIIText(t *testing.T) {
	w := linkedWorld(t)
	yaml := "use: [focal-sheet]\ndocuments:\n"
	for i := 0; i < 6; i++ {
		yaml += fmt.Sprintf("  supernova-focal-%d-%s:\n    path: notes/reviews/%s-%d.md\n", i, strings.Repeat("a", 300), strings.Repeat("é😀", 150), i)
	}
	bridgeYAML := filepath.Join(w.bridge, "bridge.yaml")
	data, err := os.ReadFile(bridgeYAML)
	if err != nil {
		t.Fatal(err)
	}
	write(t, bridgeYAML, string(data)+yaml)
	brokenRoleFixture(t, w.bridge, brokenRoleLineCount+2, "é😀"+strings.Repeat("k", 2*brokenRoleLineLimit))
	ctx := w.startWithNotes("startup", "")
	for _, want := range []string{"😀", "sits outside", "2 more broken."} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("the fixture should hold %q:\n%s", want, ctx)
		}
	}
	var broken string
	for _, l := range strings.Split(ctx, "\n") {
		if strings.HasPrefix(l, "Note: extension supernova-broken-") {
			broken = l
		}
	}
	if !strings.Contains(broken, "é😀") {
		t.Fatalf("the kept broken-extension line should hold the non-ASCII key: %q", broken)
	}
	size := paddedChars(ctx, w.bridge)
	t.Logf("padded size %d, limit %d, room %d", size, manualLimit, sessionStartRoom)
	if size > manualLimit-sessionStartRoom {
		t.Errorf("session start text is %d characters with a 200-character bridge path; the budget is %d less %d of room", size, manualLimit, sessionStartRoom)
	}
}
