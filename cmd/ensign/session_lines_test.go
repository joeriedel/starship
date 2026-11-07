// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func texts(lines []sessionLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.text)
	}
	return out
}

func TestSessionLinesAddSplitsRows(t *testing.T) {
	var c sessionLines
	c.add(3, "one\n\ntwo\n")
	c.add(3, "")
	c.add(1, "  \n")
	if got := strings.Join(texts(c.lines), "|"); got != "one|two" {
		t.Errorf("lines = %q", got)
	}
}

func TestFitSessionLines(t *testing.T) {
	line := func(p int, text string) sessionLine { return sessionLine{p, text} }
	t.Run("priority order and stable ties", func(t *testing.T) {
		in := []sessionLine{line(2, "c1"), line(1, "b"), line(2, "c2"), line(0, "a"), line(2, "c3")}
		print, collapsed := fitSessionLines(in, 1000, 50)
		if got := strings.Join(texts(print), " "); got != "a b c1 c2 c3" || len(collapsed) != 0 {
			t.Errorf("print %q, collapsed %q", got, texts(collapsed))
		}
		if in[0].text != "c1" {
			t.Errorf("the input was reordered")
		}
	})
	t.Run("stops at the first line that does not fit", func(t *testing.T) {
		in := []sessionLine{line(0, "aaaa"), line(1, strings.Repeat("b", 30)), line(2, "c")}
		// "aaaa" takes 5, the reserve 10: the 31-character line does not fit in 20, and the short low-priority line must not jump ahead of it.
		print, collapsed := fitSessionLines(in, 20, 10)
		if len(print) != 1 || print[0].text != "aaaa" || len(collapsed) != 2 || collapsed[0].priority != 1 || collapsed[1].text != "c" {
			t.Errorf("print %q, collapsed %q", texts(print), texts(collapsed))
		}
	})
	t.Run("the reserve applies only when something collapses", func(t *testing.T) {
		in := []sessionLine{line(0, "abcd"), line(1, "efgh")}
		if print, collapsed := fitSessionLines(in, 10, 100); len(print) != 2 || len(collapsed) != 0 {
			t.Errorf("lines that fit whole should all print: %q %q", texts(print), texts(collapsed))
		}
		if print, collapsed := fitSessionLines(in, 9, 3); len(print) != 1 || len(collapsed) != 1 {
			t.Errorf("one over: %q %q", texts(print), texts(collapsed))
		}
	})
	t.Run("a line counts its newline and its UTF-16 units", func(t *testing.T) {
		in := []sessionLine{line(0, "😀😀")}
		if print, _ := fitSessionLines(in, 5, 0); len(print) != 1 {
			t.Errorf("4 units and a newline fit in 5")
		}
		if print, _ := fitSessionLines(in, 4, 0); len(print) != 0 {
			t.Errorf("4 units and a newline do not fit in 4")
		}
	})
	t.Run("no room collapses everything", func(t *testing.T) {
		in := []sessionLine{line(0, "a"), line(1, "b")}
		for _, allowance := range []int{0, -5} {
			if print, collapsed := fitSessionLines(in, allowance, 0); len(print) != 0 || len(collapsed) != 2 {
				t.Errorf("allowance %d: %q %q", allowance, texts(print), texts(collapsed))
			}
		}
	})
	t.Run("a line is never cut", func(t *testing.T) {
		long := strings.Repeat("é", 500)
		print, collapsed := fitSessionLines([]sessionLine{line(0, long)}, 100, 10)
		if len(print) != 0 || len(collapsed) != 1 || collapsed[0].text != long {
			t.Errorf("print %d, collapsed %d", len(print), len(collapsed))
		}
	})
}

// sessionContext runs a SessionStart of the given source and returns what Claude reads.
func sessionContext(w *world, source string) string {
	w.t.Helper()
	res, code := w.hook("SessionStart", map[string]any{"source": source})
	if code != 0 {
		w.t.Fatalf("SessionStart exited %d: %v", code, res)
	}
	return dig(res, "hookSpecificOutput", "additionalContext")
}

func TestSessionStartBoundHoldsForAnyNumberAndLengthOfLines(t *testing.T) {
	limit := manualLimit - sessionStartRoom
	for _, c := range []struct {
		name     string
		n        int
		key      string
		declared string
	}{
		{"many short", 60, "k", ""},
		{"many multibyte", 40, "é😀" + strings.Repeat("😀", 150), ""},
		// A YAML key over 1024 characters does not parse, so the long line is a declared name, which is a value.
		{"one longer than the allowance", 1, "k", strings.Repeat("k", 2*manualLimit)},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := linkedWorld(t)
			write(t, filepath.Join(w.bridge, "notes", "topic.md"), "x\n")
			brokenRoleFixture(t, w.bridge, c.n, c.key)
			if c.declared != "" {
				write(t, filepath.Join(w.bridge, "extensions", "supernova-broken-0", "extension.yaml"), fmt.Sprintf("name: %s\nk: 1\n", c.declared))
				bf, err := loadBridgeFile(w.bridge)
				if err != nil {
					t.Fatal(err)
				}
				cp, err := controlPlaneDir(w.bridge, bf)
				if err != nil {
					t.Fatal(err)
				}
				broken := brokenRoleLines(w.bridge, cp)
				if len(broken) != 1 || contextChars(broken[0]) <= limit {
					t.Fatalf("the case needs one broken line longer than the budget, got %d lines", len(broken))
				}
			}
			ctx := sessionContext(w, "startup")
			if !strings.HasPrefix(ctx, "STARSHIP-READY") {
				t.Fatalf("the heartbeat should come first:\n%s", ctx)
			}
			if got := contextChars(ctx); got > limit {
				t.Errorf("the context is %d characters, the budget %d", got, limit)
			}
			if c.n == 1 && (strings.Contains(ctx, "notes/ (read what you need)") || !strings.Contains(ctx, "more session-start lines held")) {
				t.Errorf("a collapsed line must not let a lower one print ahead of it:\n%s", ctx)
			}
		})
	}
}

func TestSessionStartCollapsedLinesStayReadable(t *testing.T) {
	w := linkedWorld(t)
	write(t, filepath.Join(w.bridge, "notes", "topic.md"), "x\n")
	write(t, filepath.Join(w.bridge, "notes", "inbox.md"), "call Ada\n")
	brokenRoleFixture(t, w.bridge, 50, strings.Repeat("k", 300))
	ctx := sessionContext(w, "startup")
	if !strings.Contains(ctx, "more session-start lines held; read them with ensign status.") {
		t.Fatalf("want the collapse line:\n%s", ctx)
	}
	if strings.Contains(ctx, "unsorted notes") || strings.Contains(ctx, "notes/ (read what you need)") {
		t.Fatalf("the lines below the broken extensions should have collapsed:\n%s", ctx)
	}
	held := heldSessionLines(w.bridge, w.sid)
	if len(held) < 2 {
		t.Fatalf("held %q", held)
	}
	for _, l := range held {
		if strings.Contains(l, "is broken") {
			t.Errorf("a broken extension is recomputed, not held: %q", l)
		}
	}
	// The status is read during a live run, with and without a session.
	dir := plantRun(t, w.bridge, "20240101-000001-supernova", "running")
	pipe := &Snapshot{Run: "20240101-000001-supernova", Pipeline: &Pipeline{Stages: []*Stage{{ID: "code", Role: "coder"}}}}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), pipe); err != nil {
		t.Fatal(err)
	}
	if err := bindSession(w.bridge, w.sid, "20240101-000001-supernova"); err != nil && !errors.Is(err, errSetupFile) {
		t.Fatal(err)
	}
	if live, _ := liveSessionRuns(w.bridge, w.sid); len(live) != 1 {
		t.Fatalf("the run should be live in the session: %v", live)
	}
	var out bytes.Buffer
	if err := cmdStatus(nil, &out); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STARSHIP_SESSION", "")
	var bare bytes.Buffer
	if err := cmdStatus(nil, &bare); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"ensign status": out.String(), "status word": sessionWord(w.bridge, w.sid, "status")} {
		for _, l := range held {
			if !strings.Contains(text, l) {
				t.Errorf("%s lost %q:\n%s", name, l, text)
			}
		}
		for i := 0; i < 50; i++ {
			if !strings.Contains(text, fmt.Sprintf("extension supernova-broken-%d is broken", i)) {
				t.Fatalf("%s lacks broken extension %d", name, i)
			}
		}
		if strings.Count(text, "supernova-broken-0 is broken") != 1 {
			t.Errorf("%s shows a broken extension twice", name)
		}
	}
	if strings.Contains(bare.String(), "did not fit") || !strings.Contains(bare.String(), "supernova-broken-49") {
		t.Errorf("without a session only the broken list shows:\n%s", bare.String())
	}
}

func TestSessionStartHoldsKeepAcrossCompactAndClear(t *testing.T) {
	w := linkedWorld(t)
	write(t, filepath.Join(w.bridge, "notes", "inbox.md"), "call Ada\n")
	brokenRoleFixture(t, w.bridge, 50, strings.Repeat("k", 300))
	sessionContext(w, "startup")
	first := heldSessionLines(w.bridge, w.sid)
	if len(first) == 0 {
		t.Fatal("nothing was held")
	}
	write(t, filepath.Join(w.bridge, "notes", "topic.md"), "x\n")
	sessionContext(w, "compact")
	after := heldSessionLines(w.bridge, w.sid)
	if len(after) <= len(first) || strings.Join(after[:len(first)], "\n") != strings.Join(first, "\n") {
		t.Errorf("a compact should add to what is held: %q then %q", first, after)
	}
	again := after
	sessionContext(w, "compact")
	if got := heldSessionLines(w.bridge, w.sid); len(got) != len(again) {
		t.Errorf("a line held twice should stay once: %q", got)
	}
	prev := w.sid
	w.clearFrom(prev, "s2")
	if got := heldSessionLines(w.bridge, "s2"); len(got) < len(again) || strings.Join(got[:len(again)], "\n") != strings.Join(again, "\n") {
		t.Errorf("a clear should carry the held lines: %q", got)
	}
}

func TestSessionStartSaysWhenLinesCouldNotBeHeld(t *testing.T) {
	w := linkedWorld(t)
	write(t, filepath.Join(w.bridge, "notes", "inbox.md"), "call Ada\n")
	brokenRoleFixture(t, w.bridge, 50, strings.Repeat("k", 300))
	// A file where writeFileAtomic wants its temp folder makes the hold fail.
	os.RemoveAll(filepath.Join(w.bridge, ".starship", "tmp"))
	write(t, filepath.Join(w.bridge, ".starship", "tmp"), "x")
	ctx := sessionContext(w, "startup")
	if !strings.Contains(ctx, "more session-start lines did not fit and could not be held.") || strings.Contains(ctx, "held; read them") {
		t.Errorf("want the could-not-be-held line:\n%s", ctx)
	}
	if contextChars(ctx) > manualLimit-sessionStartRoom {
		t.Errorf("the variant must fit too: %d", contextChars(ctx))
	}
}

func TestNotReadyRepliesGoThroughTheBudget(t *testing.T) {
	w := linkedWorld(t)
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(w.bridge, "no-such-folder", "env"))
	// An unwritable env file is the could-not-record path, which prints no heartbeat and leads with its own line.
	write(t, filepath.Join(w.bridge, "no-such-folder"), "a file, not a folder")
	ctx := sessionContext(w, "startup")
	if !strings.HasPrefix(ctx, "starship: could not record the session id") || strings.Contains(ctx, "STARSHIP-READY") {
		t.Errorf("the could-not-record line should lead:\n%s", ctx)
	}
}

func TestCouldNotRecordWithOverflowIsNotHeld(t *testing.T) {
	w := linkedWorld(t)
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(w.bridge, "no-such-folder", "env"))
	write(t, filepath.Join(w.bridge, "no-such-folder"), "a file, not a folder")
	// Moved sessions with unreadable sequences each give a line, enough to overflow the budget.
	for i := 0; i < 60; i++ {
		session := fmt.Sprintf("old-%02d-%s", i, strings.Repeat("s", 150))
		write(t, movedPath(w.bridge, session), "s2\n")
		write(t, seqPath(w.bridge, session), "{not json")
	}
	ctx := sessionContext(w, "startup")
	if !strings.Contains(ctx, "could not record the session id") || !strings.Contains(ctx, "more session-start lines did not fit and could not be held.") || strings.Contains(ctx, "read them with ensign status") {
		t.Errorf("a session that gets no id cannot read held lines:\n%s", ctx)
	}
	if held := heldSessionLines(w.bridge, w.sid); len(held) != 0 {
		t.Errorf("nothing should be held for a session without an id: %q", held)
	}
}

// sessionLinesFloor is the least allowance the situational lines may be left with: room for the collapse line (about 75 characters) plus one of the largest realistic lines (a broken-extension row runs 200-400 characters).
const sessionLinesFloor = 500

// worstCaseAllowance is the allowance for the given manual with the longest realistic bridge path, version, project and task.
func worstCaseAllowance(manual string) int {
	bridge := "/supernova" + strings.Repeat("b", 190)
	version := "supernova" + strings.Repeat("v", 11)
	project := "supernova" + strings.Repeat("p", 21)
	task := project + "/supernova" + strings.Repeat("t", 20)
	head := readyHeading(version, bridge) + "\n" + manual
	return sessionLinesAllowance(head, contextChars(reprintPointer(project, task)))
}

func TestManualLeavesRoomForSessionLines(t *testing.T) {
	manual, err := os.ReadFile(filepath.Join(repoRoot(t), "manual", "session.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := worstCaseAllowance(string(manual)); got < sessionLinesFloor {
		t.Errorf("the session lines are left %d characters in the worst case, under the floor of %d: the manual has grown into their room, so trim manual/session.md", got, sessionLinesFloor)
	}
}

func TestSessionLinesFloorHasTeeth(t *testing.T) {
	if got := worstCaseAllowance(strings.Repeat("x", manualLimit)); got >= sessionLinesFloor {
		t.Errorf("a manual of %d bytes leaves %d, which is not under the floor of %d", manualLimit, got, sessionLinesFloor)
	}
}
