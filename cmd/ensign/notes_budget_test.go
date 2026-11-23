// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func notesOfSize(n int) string { return strings.Repeat("a", n-1) + "\n" }

func (w *world) notesFile() string {
	abs, _ := filepath.Abs(filepath.Join(w.bridge, "projects/supernova/project.md"))
	return abs
}

func (w *world) gated() bool {
	_, err := os.Stat(notesGatePath(w.bridge, w.sid))
	return err == nil
}

func (w *world) readAs(agent, path string) {
	w.t.Helper()
	in := map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": path}}
	if agent != "" {
		in["agent_id"], in["agent_type"] = agent, "coder"
	}
	if _, code := w.hook("PreToolUse", in); code != 0 {
		w.t.Fatalf("Read exited %d", code)
	}
}

func TestTaskOpenWarnsWhenNotesPassTheLimit(t *testing.T) {
	w := linkedWorld(t)
	notes := filepath.Join(w.bridge, "projects/supernova/project.md")
	write(t, notes, notesOfSize(projectNotesBudget))
	out, err := w.openOut("auth-refactor")
	if err != nil || strings.Contains(out, "limit") {
		t.Errorf("notes at the limit should not warn (err %v):\n%s", err, out)
	}
	write(t, notes, notesOfSize(projectNotesBudget+1))
	out, err = w.openOut("auth-refactor")
	if err != nil {
		t.Fatalf("a warning must not refuse the open: %v", err)
	}
	for _, want := range []string{"projects/supernova/project.md is 8001 bytes", "limit of 8000", "standing instructions", "projects/supernova/design/", "\n" + notesOfSize(projectNotesBudget+1)} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reprint") {
		t.Errorf("the warning should not mention reprinting:\n%s", out)
	}
}

func TestSessionStartWarnsWhenNotesPassTheLimit(t *testing.T) {
	w := linkedWorld(t)
	notes := filepath.Join(w.bridge, "projects/supernova/project.md")
	write(t, notes, notesOfSize(projectNotesBudget))
	if ctx := sessionContext(w, "startup"); strings.Contains(ctx, "limit of") {
		t.Errorf("notes at the limit should not warn:\n%s", ctx)
	}
	write(t, notes, notesOfSize(projectNotesBudget+1))
	ctx := sessionContext(w, "startup")
	want := "starship: projects/supernova/project.md is 8001 bytes, over its limit of 8000"
	if !strings.Contains(ctx, want) || !strings.Contains(ctx, "projects/supernova/design/") {
		t.Errorf("want the warning:\n%s", ctx)
	}
}

func TestSessionStartNamesNotesAfterCompactOrClear(t *testing.T) {
	const notes = "Always answer in haiku.\n"
	for _, c := range []struct {
		source string
		want   bool
	}{{"compact", true}, {"clear", true}, {"resume", false}, {"startup", false}} {
		t.Run(c.source, func(t *testing.T) {
			w := linkedWorld(t)
			ctx := w.startWithNotes(c.source, notes)
			if strings.Contains(ctx, "haiku") {
				t.Fatalf("the notes must not be printed:\n%s", ctx)
			}
			if got := strings.Contains(ctx, "notes      read "+w.notesFile()); got != c.want {
				t.Errorf("line = %v, want %v:\n%s", got, c.want, ctx)
			}
			if w.gated() != c.want {
				t.Errorf("gate = %v, want %v", w.gated(), c.want)
			}
		})
	}
}

func TestSessionStartNamesTheDesignFolderWhenThereIsOne(t *testing.T) {
	w := linkedWorld(t)
	design, _ := filepath.Abs(filepath.Join(w.bridge, "projects/supernova/design"))
	if ctx := w.startWithNotes("compact", "x\n"); strings.Contains(ctx, design) {
		t.Errorf("no design folder, none named:\n%s", ctx)
	}
	write(t, filepath.Join(design, "overview.md"), "x\n")
	if ctx := w.startWithNotes("compact", "x\n"); !strings.Contains(ctx, design) {
		t.Errorf("want the design folder named:\n%s", ctx)
	}
}

func TestSessionStartNamesNothingWithoutTaskOrNotes(t *testing.T) {
	w := linkedWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/project.md"), "Always answer in haiku.\n")
	if ctx := sessionContext(w, "compact"); strings.Contains(ctx, "notes      read") || w.gated() {
		t.Errorf("no recorded task, no line and no gate (gate %v):\n%s", w.gated(), ctx)
	}
	w = linkedWorld(t)
	if ctx := w.startWithNotes("compact", ""); strings.Contains(ctx, "notes      read") || w.gated() {
		t.Errorf("no project.md, no line and no gate (gate %v):\n%s", w.gated(), ctx)
	}
}

func TestStartupLeavesAnExistingGate(t *testing.T) {
	w := linkedWorld(t)
	w.startWithNotes("compact", "x\n")
	sessionContext(w, "startup")
	if !w.gated() {
		t.Error("a start that is not a compaction or clear should leave the marker as it is")
	}
}

func TestReadClearsTheGateOnlyForTheMainAgentReadingTheNotes(t *testing.T) {
	w := linkedWorld(t)
	w.startWithNotes("compact", "x\n")
	w.readAs("", filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md"))
	w.readAs("sub1", w.notesFile())
	if !w.gated() {
		t.Fatal("another file or a subagent's read must not clear the gate")
	}
	w.readAs("", w.notesFile())
	if w.gated() {
		t.Error("the main agent's read of project.md should clear the gate")
	}
}

func TestReadClearsTheGateForARelativePath(t *testing.T) {
	w := linkedWorld(t)
	w.startWithNotes("compact", "x\n")
	w.hook("PreToolUse", map[string]any{"tool_name": "Read", "cwd": w.bridge, "tool_input": map[string]any{"file_path": "projects/supernova/../supernova/project.md"}})
	if w.gated() {
		t.Error("a relative path to project.md should clear the gate")
	}
}

func TestSameNotesFileFallsBackToThePathWhenAFileIsMissing(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "projects", "supernova", "project.md")
	if !sameNotesFile(gate, filepath.Join(filepath.Dir(gate), "..", "supernova", "project.md"), "", "linux") {
		t.Error("a cleaned path should match")
	}
	if sameNotesFile(gate, strings.ToUpper(gate), "", "linux") {
		t.Error("case matters off Windows")
	}
	if !sameNotesFile(gate, strings.ToUpper(gate), "", "windows") {
		t.Error("case does not matter on Windows")
	}
}

func TestGateRefusalsNameTheFile(t *testing.T) {
	w := linkedWorld(t)
	w.mustSeq("run", "supernova/auth-refactor")
	w.startWithNotes("compact", "x\n")
	file := w.notesFile()
	named := func(what, got string) {
		t.Helper()
		if !strings.Contains(got, "read "+file+" first") {
			t.Errorf("%s should name the file:\n%s", what, got)
		}
	}

	err := w.runErr("supernova/auth-refactor")
	if err == nil {
		t.Fatal("a run should be refused")
	}
	named("run", err.Error())
	if err := w.runErr("--dry-run", "supernova/auth-refactor"); err != nil {
		t.Errorf("a dry run is allowed: %v", err)
	}

	_, err = w.seq("do", "tidy")
	if err == nil {
		t.Fatal("a step add should be refused")
	}
	named("step add", err.Error())

	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	named("go", dig(res, "hookSpecificOutput", "additionalContext"))
	if readSeq(w.bridge, w.sid).Sequences[0].Approved {
		t.Error("go must not approve while the gate is set")
	}

	agent := map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "coder", "prompt": "x", "description": "x"}}
	res, _ = w.hook("PreToolUse", agent)
	named("Agent dispatch", dig(res, "hookSpecificOutput", "permissionDecisionReason"))
	agent["agent_id"], agent["agent_type"] = "lead1", "orchestrator"
	res, _ = w.hook("PreToolUse", agent)
	if strings.Contains(dig(res, "hookSpecificOutput", "permissionDecisionReason"), "read "+file) {
		t.Errorf("a subagent's dispatch is not refused: %v", res)
	}

	w.readAs("", file)
	res, _ = w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, "approved") {
		t.Errorf("after the read go should approve:\n%s", ctx)
	}
}

func TestGatedStepWaitsForTheRead(t *testing.T) {
	w := linkedWorld(t)
	w.startWithNotes("compact", "x\n")
	_, err := startRun(w.bridge, w.sid, "", "20260101-000001-supernova")
	var gated *notesGateError
	if !errors.As(err, &gated) || gated.file != w.notesFile() {
		t.Fatalf("startRun should refuse with the gate error naming the file: %v", err)
	}
	st := &seqStep{N: 1, Kind: "run", Task: "supernova/auth-refactor", Runs: []*seqRun{{Run: "20260101-000001-supernova"}}}
	texts, busy, failure := startRuns(w.bridge, w.sid, st)
	if len(texts) != 0 || busy == nil || failure != nil {
		t.Fatalf("a gated step should wait: texts %v busy %v failure %v", texts, busy, failure)
	}
	if st.Runs[0].Status != "" || !strings.HasPrefix(st.Runs[0].Reason, "waiting: read "+w.notesFile()) {
		t.Errorf("the run should stay unstarted and say what it waits for: %+v", st.Runs[0])
	}
}

func TestWaitingTextForTheGateNamesTheRead(t *testing.T) {
	w := linkedWorld(t)
	w.startWithNotes("compact", "x\n")
	st := &seqStep{N: 1, Kind: "run", Task: "supernova/auth-refactor", Runs: []*seqRun{{Run: "20260101-000001-supernova"}}}
	startRuns(w.bridge, w.sid, st)
	got := waitingText(st, notesGateRefusal(w.bridge, w.sid))
	if !strings.Contains(got, "with the Read tool") || !strings.Contains(got, w.notesFile()) || strings.Contains(got, "once the folder is free") {
		t.Errorf("a gated step should be told to read the notes, not that it starts by itself:\n%s", got)
	}
}

func TestWaitingTextForAPublishStepKeepsTheFolderWording(t *testing.T) {
	w := linkedWorld(t)
	w.startWithNotes("compact", "x\n")
	st := &seqStep{N: 1, Kind: "publish", Task: "supernova/auth-refactor", Reason: "waiting: code folder is busy"}
	got := waitingText(st, notesGateRefusal(w.bridge, w.sid))
	if !strings.Contains(got, "code folder is busy") || !strings.Contains(got, "once the folder is free") || strings.Contains(got, "with the Read tool") {
		t.Errorf("a publish step waits on the folder, not the notes:\n%s", got)
	}
}
