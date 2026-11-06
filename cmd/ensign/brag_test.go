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

const (
	bragFile    = "notes/focal/2026-h2.brag.md"
	instrFile   = "notes/focal/2026-h2-instructions.md"
	secondSheet = "notes/focal/2027.md"
)

// bragWorld is a bridge with the focal-sheet extension and one focal document with a rubric and an instructions declaration; second adds another focal document.
func bragWorld(t *testing.T, second bool) *world {
	t.Helper()
	y := "use: [focal-sheet]\ndocuments:\n  supernova-focal:\n    path: " + focalPath + "\n    rubric: notes/focal/rubric.md\n    instructions: " + instrFile + "\n"
	if second {
		y += "  supernova-focal2:\n    path: " + secondSheet + "\n    rubric: notes/focal/rubric.md\n"
	}
	w := connWorld(t, y)
	write(t, filepath.Join(w.bridge, "notes/focal/rubric.md"), "## Alpha\n\n## Beta\n")
	return w
}

func bragEntries() Doc {
	return Doc{Entries: []Entry{
		{ID: "D1", Status: "active", Theme: "Alpha", Text: "Nova shipped the launch checklist", Part: "Nova", Hits: 2, Evidence: []string{"review"}},
		{ID: "D2", Status: "active", Theme: "Nowhere", Text: "Nova fixed the gate", Part: "Nova", Hits: 2, Evidence: []string{"review"}},
	}}
}

func writeBragInputs(t *testing.T, w *world) {
	t.Helper()
	write(t, filepath.Join(w.bridge, instrFile), "INSTR-TEXT: lead with the launch.\n")
	write(t, filepath.Join(w.bridge, focalStylePath), "STYLE-TEXT: plain and warm.\n")
}

func bragMessage(body string) string {
	return "Here it is.\n\n" + bragBegin + "\n" + body + bragEnd + "\n\n```starship-report\noutcome: written\nsummary: wrote it\n```\n"
}

func feedbackMessage(text string) string {
	return text + "\n\n```starship-report\noutcome: noted\nsummary: 1 claim\n```\n"
}

func readBrag(w *world) string {
	data, _ := os.ReadFile(filepath.Join(w.bridge, filepath.FromSlash(bragFile)))
	return string(data)
}

// bragStart starts a run with no proposals and spawns the brag writer.
func bragStart(t *testing.T, w *world) string {
	t.Helper()
	w.run("--pipeline", "document-curate")
	id, why := w.spawn("", "", "brag-writer")
	if id == "" {
		t.Fatalf("the brag writer was refused: %s", why)
	}
	return id
}

func TestBragRunGoesStraightToTheBragStage(t *testing.T) {
	w := bragWorld(t, false)
	seedSheet(t, w, bragEntries())
	writeBragInputs(t, w)
	w.run("--pipeline", "document-curate")
	if got := w.next(); !strings.Contains(got, "brag-writer") {
		t.Fatalf("the run should start at the brag stage: %s", got)
	}
	if id, _ := w.spawn("", "", "curator"); id != "" {
		t.Fatal("the curator has nothing to do")
	}
	id, why := w.spawn("", "", "brag-writer")
	if id == "" {
		t.Fatal(why)
	}
	p := w.decodedPromptOf(id)
	for _, want := range []string{"Nova shipped the launch checklist", "INSTR-TEXT", "STYLE-TEXT", "## Document: supernova-focal", "- Alpha", "D2", "Use every entry, candidates included"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, p)
		}
	}
	if r := w.stop(id, "brag-writer", bragMessage("# Nova\n\nShipped it.\n\n\n"), true); r != "" {
		t.Fatalf("refused: %s", r)
	}
	if got := readBrag(w); got != "# Nova\n\nShipped it.\n" {
		t.Errorf("brag file = %q", got)
	}
	notes := strings.Join(w.state().Notes, "\n")
	if !strings.Contains(notes, "supernova-focal has entries with no listed theme, left out unless its instructions bring them in: D2") {
		t.Errorf("notes lack the left-out ids:\n%s", notes)
	}
	if strings.Contains(notes, "without") {
		t.Errorf("both inputs were given:\n%s", notes)
	}
	if got := w.next(); !strings.Contains(got, "brag-reviewer") {
		t.Fatalf("the feedback stage should follow: %s", got)
	}
	rev, why := w.spawn("", "", "brag-reviewer")
	if rev == "" {
		t.Fatal(why)
	}
	rp := w.decodedPromptOf(rev)
	for _, want := range []string{"Nova shipped the launch checklist", "INSTR-TEXT", "Shipped it."} {
		if !strings.Contains(rp, want) {
			t.Errorf("the reviewer's prompt lacks %q", want)
		}
	}
	if r := w.stop(rev, "brag-reviewer", feedbackMessage("Nothing unsupported."), true); r != "" {
		t.Fatalf("refused: %s", r)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s %s", st.Status, st.Reason)
	}
	if got := w.next(); !strings.Contains(got, "The run is finished") || !strings.Contains(got, "relay each feedback agent's findings") {
		t.Errorf("finish line = %s", got)
	}
}

func TestBragFollowsTheCurator(t *testing.T) {
	w := bragWorld(t, false)
	w.propose("supernova-focal", "Nova shipped the launch checklist")
	w.run("--pipeline", "document-curate")
	if id, _ := w.spawn("", "", "brag-writer"); id != "" {
		t.Fatal("the curator goes first")
	}
	cur, _ := w.spawn("", "", "curator")
	ok := focalEntry("D1", "Alpha", "Nova shipped", "", "Nova decided")
	if r := w.stop(cur, "curator", curatorMessage("entries:\n"+ok, "D1: new\n", "updated"), true); r != "" {
		t.Fatal(r)
	}
	id, why := w.spawn("", "", "brag-writer")
	if id == "" {
		t.Fatal(why)
	}
	if p := w.decodedPromptOf(id); !strings.Contains(p, "Nova shipped") || !strings.Contains(p, "None given: the declared file does not exist") {
		t.Errorf("the prompt should carry the new sheet and say the instructions are missing:\n%s", p)
	}
}

func TestBragNewInstructionsMakeANewFile(t *testing.T) {
	w := bragWorld(t, false)
	seedSheet(t, w, bragEntries())
	writeBragInputs(t, w)
	id := bragStart(t, w)
	if r := w.stop(id, "brag-writer", bragMessage("first\n"), true); r != "" {
		t.Fatal(r)
	}
	rev, _ := w.spawn("", "", "brag-reviewer")
	w.stop(rev, "brag-reviewer", feedbackMessage("fine"), true)
	write(t, filepath.Join(w.bridge, instrFile), "NEW-INSTR-TEXT\n")
	id = bragStart(t, w)
	if p := w.decodedPromptOf(id); !strings.Contains(p, "NEW-INSTR-TEXT") || strings.Contains(p, "INSTR-TEXT: lead") {
		t.Errorf("the prompt should carry the new instructions:\n%s", p)
	}
	if r := w.stop(id, "brag-writer", bragMessage("second\n"), true); r != "" {
		t.Fatal(r)
	}
	if got := readBrag(w); got != "second\n" {
		t.Errorf("brag file = %q", got)
	}
}

func TestBragOptionalInputsMissing(t *testing.T) {
	w := bragWorld(t, false)
	seedSheet(t, w, Doc{Entries: bragEntries().Entries[:1]})
	id := bragStart(t, w)
	if p := w.decodedPromptOf(id); strings.Count(p, "None given") != 2 {
		t.Errorf("the prompt should say twice that nothing was given:\n%s", p)
	}
	if r := w.stop(id, "brag-writer", bragMessage("body\n"), true); r != "" {
		t.Fatal(r)
	}
	rev, _ := w.spawn("", "", "brag-reviewer")
	if r := w.stop(rev, "brag-reviewer", feedbackMessage("fine"), true); r != "" {
		t.Fatal(r)
	}
	got := w.next()
	for _, want := range []string{"supernova-focal ran without an instructions file", "curation ran without notes/focal_style.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("the finish line lacks %q: %s", want, got)
		}
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s", st.Status)
	}
}

func TestBragNothingToDoKeepsTheOldError(t *testing.T) {
	w := connWorld(t, "use: [focal-sheet]\n")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "no declared document has pending proposals to curate") {
		t.Errorf("err = %v", err)
	}
	w = docWorld(t, "notes/supernova-log.md")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "no declared document has pending proposals to curate") {
		t.Errorf("a plain document: err = %v", err)
	}
}

func TestBragStartRefusesAStageWithNoItems(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	w.propose("supernova-log", "Nova owns the launch checklist")
	if err := w.runErr("--pipeline", "document-curate", "--start", "brag"); err == nil || !strings.Contains(err.Error(), `--start: stage "brag" has no items`) {
		t.Errorf("err = %v", err)
	}
}

func TestFeedbackMissingOutcomeIsNoted(t *testing.T) {
	w := bragWorld(t, false)
	id := bragStart(t, w)
	w.stop(id, "brag-writer", bragMessage("body\n"), true)
	rev, why := w.spawn("", "", "brag-reviewer")
	if rev == "" {
		t.Fatal(why)
	}
	msg := "Nothing came back.\n\n```starship-report\noutcome: missing\nsummary: none\n```\n"
	if r := w.stop(rev, "brag-reviewer", msg, true); r != "" {
		t.Fatal(r)
	}
	st := w.state()
	if st.Status != "success" || !strings.Contains(strings.Join(st.Notes, "\n"), "feedback for supernova-focal is missing") {
		t.Errorf("status %s notes %v", st.Status, st.Notes)
	}
}

func TestFeedbackHandlerFailureIsAdvisory(t *testing.T) {
	w := bragWorld(t, false)
	by := filepath.Join(w.bridge, "bridge.yaml")
	data, _ := os.ReadFile(by)
	write(t, by, string(data)+"events:\n  stage.before:feedback: [{ require: \"exit 1\" }]\n")
	id := bragStart(t, w)
	w.stop(id, "brag-writer", bragMessage("body\n"), true)
	w.spawn("", "", "brag-reviewer")
	st := w.state()
	if st.Status != "success" || !strings.Contains(strings.Join(st.Notes, "\n"), "feedback for supernova-focal is missing") {
		t.Errorf("status %s %s notes %v", st.Status, st.Reason, st.Notes)
	}
}

func TestBragPlainDocumentSkipsBragAndFeedback(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	w.propose("supernova-log", "Nova owns the launch checklist")
	w.run("--pipeline", "document-curate")
	cur, _ := w.spawn("", "", "curator")
	if r := w.stop(cur, "curator", goodRewrite(), true); r != "" {
		t.Fatal(r)
	}
	if st := w.state(); st.Status != "success" || len(st.Notes) != 0 {
		t.Errorf("status %s notes %v", st.Status, st.Notes)
	}
}

func TestBragBodyAndRefusals(t *testing.T) {
	body := "# Nova\n\n```go\nfmt.Println(\"hi\")\n```\n\nDone.\n"
	t.Run("code fences survive", func(t *testing.T) {
		w := bragWorld(t, false)
		id := bragStart(t, w)
		msg := "intro\r\n" + bragBegin + "\r\n" + body + bragEnd + "\r\n\r\n```starship-report\noutcome: written\nsummary: ok\n```\n"
		if r := w.stop(id, "brag-writer", msg, true); r != "" {
			t.Fatal(r)
		}
		if got := readBrag(w); got != body {
			t.Errorf("brag file = %q", got)
		}
	})
	t.Run("refused without quoting", func(t *testing.T) {
		for name, msg := range map[string]string{
			"empty":   bragMessage("  \n\n") + docMarker,
			"markers": docMarker + "\n```starship-report\noutcome: written\nsummary: ok\n```\n",
		} {
			w := bragWorld(t, false)
			id := bragStart(t, w)
			r := w.stop(id, "brag-writer", msg, false)
			if r == "" || strings.Contains(r, docMarker) || !strings.Contains(r, bragBegin) {
				t.Errorf("%s: refusal = %q", name, r)
			}
			if readBrag(w) != "" {
				t.Errorf("%s: a file was written", name)
			}
		}
	})
	t.Run("budget", func(t *testing.T) {
		w := bragWorld(t, false)
		write(t, filepath.Join(w.bridge, filepath.FromSlash(bragFile)), "old\n")
		id := bragStart(t, w)
		for i := 0; i < stopBudget; i++ {
			w.stop(id, "brag-writer", "no file here\n```starship-report\noutcome: written\nsummary: ok\n```\n", false)
		}
		w.wantFailed("bad-report")
		if got := readBrag(w); got != "old\n" {
			t.Errorf("the old file changed: %q", got)
		}
	})
}

func TestBragLeavesAReadableFileAlone(t *testing.T) {
	w := bragWorld(t, false)
	write(t, filepath.Join(w.bridge, "notes/focal/2026-h2.readable.md"), "old rendering\n")
	id := bragStart(t, w)
	if r := w.stop(id, "brag-writer", bragMessage("new\n"), true); r != "" {
		t.Fatal(r)
	}
	if data, _ := os.ReadFile(filepath.Join(w.bridge, "notes/focal/2026-h2.readable.md")); string(data) != "old rendering\n" {
		t.Errorf("the old file changed: %q", data)
	}
}

func TestBragCollisions(t *testing.T) {
	const head = "use: [focal-sheet]\ndocuments:\n  supernova-focal:\n    path: " + focalPath + "\n"
	for name, other := range map[string]string{
		"path":         "  supernova-other:\n    path: notes/focal/2026-H2.brag.md\n",
		"rubric":       "  supernova-other:\n    path: notes/supernova-log.md\n    rubric: notes/focal/2026-h2.brag.md\n",
		"instructions": "    instructions: notes/focal/2026-h2.BRAG.md\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := connWorld(t, head+other)
			err := w.runErr("--pipeline", "document-curate")
			if err == nil || !strings.Contains(err.Error(), bragFile) {
				t.Fatalf("the run should be refused naming the brag file: %v", err)
			}
		})
	}
	t.Run("backstop", func(t *testing.T) {
		w := bragWorld(t, false)
		id := bragStart(t, w)
		write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\n"+head+"  supernova-other:\n    path: notes/focal/2026-h2.brag.md\n")
		r := w.stop(id, "brag-writer", bragMessage("x\n"), false)
		if !strings.Contains(r, bragFile) {
			t.Errorf("refusal = %q", r)
		}
		if readBrag(w) != "" {
			t.Error("the other document was written over")
		}
	})
}

func TestInstructionsKey(t *testing.T) {
	const head = "use: [focal-sheet]\ndocuments:\n  supernova-focal:\n    path: " + focalPath + "\n"
	for name, c := range map[string]struct{ extra, want string }{
		"ok":          {"    instructions: notes/focal/how.md\n", ""},
		"is the path": {"    instructions: notes/focal/2026-H2.md\n", "instructions is the document itself"},
		"is rubric":   {"    rubric: notes/focal/how.md\n    instructions: notes/focal/how.md\n", "instructions is the rubric"},
		"not md":      {"    instructions: notes/focal/how.txt\n", "must end in .md"},
		"unknown key": {"    guide: notes/focal/how.md\n", "path, rubric and instructions"},
	} {
		t.Run(name, func(t *testing.T) {
			w := connWorld(t, head+c.extra)
			docs, err := loadDeclaredDocuments(w.bridge)
			if c.want == "" {
				if err != nil || docs[0].Instructions != "notes/focal/how.md" {
					t.Fatalf("%+v %v", docs, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	t.Run("non-focal", func(t *testing.T) {
		w := connWorld(t, "documents:\n  supernova-log:\n    path: notes/supernova-log.md\n    instructions: notes/how.md\n")
		docs, err := loadDeclaredDocuments(w.bridge)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(documentProblems(w.bridge, docs), "\n"); !strings.Contains(got, "supernova-log") || !strings.Contains(got, "instructions") {
			t.Errorf("problems = %q", got)
		}
		if err := w.runErr("--pipeline", "document-curate"); err == nil {
			t.Error("the run should be refused")
		}
	})
	t.Run("unreadable files", func(t *testing.T) {
		w := bragWorld(t, false)
		docs, _ := loadDeclaredDocuments(w.bridge)
		if got := documentProblems(w.bridge, docs); len(got) != 0 {
			t.Fatalf("missing files are not problems: %v", got)
		}
		if err := os.MkdirAll(filepath.Join(w.bridge, instrFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(w.bridge, focalStylePath), 0o755); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(documentProblems(w.bridge, docs), "\n")
		if !strings.Contains(got, "supernova-focal") || !strings.Contains(got, instrFile) || !strings.Contains(got, focalStylePath) {
			t.Errorf("problems = %q", got)
		}
	})
}

func TestBragLeftOutIdsStayOutOfSessionStart(t *testing.T) {
	w := bragWorld(t, false)
	seedSheet(t, w, bragEntries())
	if got := sessionStartText(t, w); strings.Contains(got, "no listed theme") || strings.Contains(got, "D2") {
		t.Errorf("session start carries left-out ids:\n%s", got)
	}
}

func TestFeedbackIsAdvisory(t *testing.T) {
	missing := "feedback for supernova-focal is missing"
	toFeedback := func(t *testing.T, w *world) string {
		t.Helper()
		id := bragStart(t, w)
		if r := w.stop(id, "brag-writer", bragMessage("body\n"), true); r != "" {
			t.Fatal(r)
		}
		rev, why := w.spawn("", "", "brag-reviewer")
		if rev == "" {
			t.Fatal(why)
		}
		return rev
	}
	check := func(t *testing.T, w *world) {
		t.Helper()
		st := w.state()
		if st.Status != "success" {
			t.Fatalf("status %s %s", st.Status, st.Reason)
		}
		if !strings.Contains(strings.Join(st.Notes, "\n"), missing) {
			t.Errorf("notes = %v", st.Notes)
		}
		for _, d := range st.Dispatches {
			if d.Stage == "feedback" && *d.StopsLeft != stopBudget {
				t.Errorf("stop budget spent: %d", *d.StopsLeft)
			}
		}
	}
	for name, msg := range map[string]string{"bad report": "no report here", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			w := bragWorld(t, false)
			rev := toFeedback(t, w)
			if r := w.stop(rev, "brag-reviewer", msg, false); r != "" {
				t.Fatalf("the stop was blocked: %s", r)
			}
			check(t, w)
		})
	}
	t.Run("handback", func(t *testing.T) {
		w := bragWorld(t, false)
		rev := toFeedback(t, w)
		if r := w.handback(rev, "brag-reviewer", "no report here"); r != "" {
			t.Fatalf("the handback was refused: %s", r)
		}
		check(t, w)
	})
	t.Run("prompt failure", func(t *testing.T) {
		w := bragWorld(t, false)
		id := bragStart(t, w)
		w.stop(id, "brag-writer", bragMessage("body\n"), true)
		file := filepath.Join(w.bridge, filepath.FromSlash(bragFile))
		os.Remove(file)
		os.Mkdir(file, 0o755)
		rev, why := w.spawn("", "", "brag-reviewer")
		if rev != "" || !strings.Contains(why, "missing") || !strings.Contains(why, "ensign next") {
			t.Fatalf("the spawn should be denied: %q %q", rev, why)
		}
		check(t, w)
	})
	t.Run("one of two", func(t *testing.T) {
		w := bragWorld(t, true)
		w.run("--pipeline", "document-curate")
		for i := 0; i < 2; i++ {
			id, why := w.spawn("", "", "brag-writer")
			if id == "" {
				t.Fatal(why)
			}
			if r := w.stop(id, "brag-writer", bragMessage("body\n"), true); r != "" {
				t.Fatal(r)
			}
		}
		r1, _ := w.spawn("", "", "brag-reviewer")
		r2, _ := w.spawn("", "", "brag-reviewer")
		w.stop(r1, "brag-reviewer", "nothing", false)
		if st := w.state(); st.Status == "success" || st.Status == "error" {
			t.Fatalf("one branch going missing ended the run: %s", st.Status)
		}
		if r := w.stop(r2, "brag-reviewer", feedbackMessage("fine"), true); r != "" {
			t.Fatal(r)
		}
		check(t, w)
	})
	t.Run("run stop", func(t *testing.T) {
		w := bragWorld(t, true)
		w.run("--pipeline", "document-curate")
		for i := 0; i < 2; i++ {
			id, _ := w.spawn("", "", "brag-writer")
			w.stop(id, "brag-writer", bragMessage("body\n"), true)
		}
		r1, _ := w.spawn("", "", "brag-reviewer")
		w.stop(r1, "brag-reviewer", feedbackMessage("fine"), true)
		w.spawn("", "", "brag-reviewer")
		run := w.state().Run
		if _, err := w.stopRun(run); err != nil {
			t.Fatal(err)
		}
		st := w.runState(run)
		if st.Status != "success" {
			t.Fatalf("status %s %s", st.Status, st.Reason)
		}
		notes := strings.Join(st.Notes, "\n")
		if !strings.Contains(notes, "feedback for supernova-focal2 is missing") || strings.Contains(notes, "feedback for supernova-focal is missing") {
			t.Errorf("notes = %s", notes)
		}
	})
}

func TestBragKeepsContentPrivate(t *testing.T) {
	w := bragWorld(t, false)
	entries := bragEntries()
	entries.Entries[0].Text = "Said " + docMarker
	seedSheet(t, w, entries)
	write(t, filepath.Join(w.bridge, instrFile), "instructions "+docMarker+"\n")
	write(t, filepath.Join(w.bridge, focalStylePath), "style "+docMarker+"\n")
	id := bragStart(t, w)
	msg := strings.Replace(bragMessage("brag "+docMarker+"\n"), "summary: wrote it", "summary: wrote it\nfact: shell | "+docMarker+" | x", 1)
	if r := w.stop(id, "brag-writer", msg, true); r != "" {
		t.Fatal(r)
	}
	rev, _ := w.spawn("", "", "brag-reviewer")
	if r := w.stop(rev, "brag-reviewer", feedbackMessage("claim "+docMarker), true); r != "" {
		t.Fatal(r)
	}
	st := w.state()
	for _, h := range st.History {
		if strings.Contains(h.Detail, docMarker) {
			t.Errorf("history carries content: %+v", h)
		}
	}
	for _, n := range st.Notes {
		if strings.Contains(n, docMarker) {
			t.Errorf("a note carries content: %s", n)
		}
	}
	for _, dp := range st.Dispatches {
		if strings.Contains(dp.Summary, docMarker) {
			t.Errorf("dispatch %s stores the summary", dp.ID)
		}
		for k, v := range dp.Fields {
			if strings.Contains(strings.Join(v, " "), docMarker) {
				t.Errorf("dispatch %s stores field %s", dp.ID, k)
			}
		}
	}
	if strings.Contains(w.next(), docMarker) {
		t.Error("ensign next printed content")
	}
}

func TestBragRolesRunOnOpus(t *testing.T) {
	for _, name := range []string{"brag-writer", "brag-reviewer"} {
		front, _, err := splitFrontmatter(filepath.Join(repoRoot(t), "roles", name+".md"))
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Name  string `yaml:"name"`
			Model string `yaml:"model"`
			Tools string `yaml:"tools"`
		}
		if err := decodeFrontmatter(front, &f); err != nil {
			t.Fatal(err)
		}
		if f.Name != name || f.Model != "opus" || f.Tools != "Read" {
			t.Errorf("%s: %+v", name, f)
		}
	}
}

func TestValidateAdvisoryAndEmpty(t *testing.T) {
	ok := Outcomes{{Name: "a", Then: Transition{Finish: "success"}}, {Name: "e", Then: Transition{Error: "x"}}}
	for name, c := range map[string]struct {
		s    Stage
		want string
	}{
		"fine":             {Stage{Advisory: "a", Parallel: &Parallel{Over: "focal-documents", Join: "all", Empty: "a"}}, ""},
		"advisory unknown": {Stage{Advisory: "z"}, "advisory outcome"},
		"advisory error":   {Stage{Advisory: "e"}, "must not be an error"},
		"empty unknown":    {Stage{Parallel: &Parallel{Over: "focal-documents", Join: "all", Empty: "z"}}, "empty outcome"},
		"empty dimensions": {Stage{Dimensions: []string{"x"}, Parallel: &Parallel{Over: "dimensions", Join: "all", Empty: "a"}}, "empty cannot be used"},
	} {
		t.Run(name, func(t *testing.T) {
			s := c.s
			s.ID, s.Role, s.Outcomes = "s", "coder", ok
			got := strings.Join(validate(&Config{ControlPlane: repoRoot(t), Pipeline: &Pipeline{Stages: []*Stage{&s}}}), "\n")
			if c.want == "" && (strings.Contains(got, "advisory") || strings.Contains(got, "empty")) || c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("want %q in:\n%s", c.want, got)
			}
		})
	}
}

// stageItems is the frozen item list of each stage of the session's run, by stage id.
func stageItems(t *testing.T, w *world) map[string][]string {
	t.Helper()
	r, err := openSessionRun(w.bridge, w.sid)
	if err != nil || r == nil {
		t.Fatalf("no active run: %v", err)
	}
	defer r.close()
	out := map[string][]string{}
	for _, s := range r.Snap.Pipeline.Stages {
		out[s.ID] = s.Dimensions
	}
	return out
}

func twoFocalWorld(t *testing.T) *world {
	t.Helper()
	w := bragWorld(t, true)
	w.propose("supernova-focal", "Nova shipped the launch checklist")
	w.propose("supernova-focal2", "Nova fixed the gate")
	return w
}

func TestRunDocumentNarrowsBothStages(t *testing.T) {
	w := twoFocalWorld(t)
	w.run("--pipeline", "document-curate", "--document", "supernova-focal")
	items := stageItems(t, w)
	for _, id := range []string{"curate", "brag", "feedback"} {
		if got := strings.Join(items[id], ","); got != "supernova-focal" {
			t.Errorf("stage %s items = %q", id, got)
		}
	}
	if st := w.state(); st.Document != "supernova-focal" {
		t.Errorf("State.Document = %q", st.Document)
	}
	if got := pendingDocFiles(w, "supernova-focal2"); len(got) != 1 {
		t.Errorf("the other document's proposals = %v", got)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, "notes/focal/2027.brag.md")); err == nil {
		t.Error("the other document's brag file was written")
	}
	if id, _ := w.spawn("", "", "curator"); id == "" {
		t.Fatal("the curator was refused")
	}
	if id, _ := w.spawn("", "", "curator"); id != "" {
		t.Error("a second curator was allowed")
	}
}

func TestRunDocumentWithoutTheFlagKeepsEveryDocument(t *testing.T) {
	w := twoFocalWorld(t)
	w.run("--pipeline", "document-curate")
	items := stageItems(t, w)
	if got := strings.Join(items["curate"], ","); got != "supernova-focal,supernova-focal2" {
		t.Errorf("curate items = %q", got)
	}
}

func TestRunDocumentRefusals(t *testing.T) {
	w := twoFocalWorld(t)
	if err := w.runErr("--pipeline", "document-curate", "--document", "supernova-nowhere"); err == nil || !strings.Contains(err.Error(), "supernova-nowhere") {
		t.Errorf("undeclared: %v", err)
	}
	if err := w.runErr("--pipeline", "skill-curate", "--document", "supernova-focal"); err == nil || !strings.Contains(err.Error(), "no stage over pending-documents or focal-documents") {
		t.Errorf("pipeline without document stages: %v", err)
	}
}

func TestRunDocumentWithNothingToDoNamesIt(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	if err := w.runErr("--pipeline", "document-curate", "--document", "supernova-log"); err == nil || !strings.Contains(err.Error(), "document supernova-log has nothing to curate") {
		t.Errorf("err = %v", err)
	}
}

func TestRunDocumentIgnoresAnotherDocumentsProblem(t *testing.T) {
	w := twoFocalWorld(t)
	by := filepath.Join(w.bridge, "bridge.yaml")
	data, _ := os.ReadFile(by)
	write(t, by, string(data)+"  supernova-log:\n    path: notes/supernova-log.md\n    rubric: notes/missing-rubric.md\n")
	w.propose("supernova-log", "Nova logged the burn")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "supernova-log") {
		t.Fatalf("the whole run should be refused for the broken document: %v", err)
	}
	w.run("--pipeline", "document-curate", "--document", "supernova-focal")
	if got := strings.Join(stageItems(t, w)["brag"], ","); got != "supernova-focal" {
		t.Errorf("brag items = %q", got)
	}
}

func TestCurationAgentFileUnderAnotherFileName(t *testing.T) {
	w := bragWorld(t, false)
	agents := filepath.Join(w.bridge, ".claude", "agents")
	data, err := os.ReadFile(filepath.Join(agents, "brag-writer.md"))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(agents, "brag-writer.md"))
	write(t, filepath.Join(agents, "my-writer.md"), string(data))
	out := w.run("--pipeline", "document-curate")
	if strings.Contains(out, "linked") {
		t.Errorf("link ran:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(agents, "brag-writer.md")); err == nil {
		t.Error("brag-writer.md was created")
	}
}

func TestRunDocumentDryRunAndRetry(t *testing.T) {
	w := twoFocalWorld(t)
	out := w.run("--pipeline", "document-curate", "--document", "supernova-focal", "--dry-run")
	if !strings.Contains(out, "[supernova-focal]") || strings.Contains(out, "supernova-focal2") {
		t.Errorf("dry run:\n%s", out)
	}
	w.run("--pipeline", "document-curate", "--document", "supernova-focal")
	old, err := openRun(w.bridge, w.state().Run)
	if err != nil {
		t.Fatal(err)
	}
	defer old.close()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var buf bytes.Buffer
	snap, err := retryRun(w.bridge, old, "it stopped", "everything", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if st := w.runState(snap.Run); st.Document != "supernova-focal" {
		t.Errorf("retry State.Document = %q", st.Document)
	}
	for _, s := range snap.Pipeline.Stages {
		if s.Parallel != nil && strings.Join(s.Dimensions, ",") != "supernova-focal" {
			t.Errorf("retry stage %s items = %v", s.ID, s.Dimensions)
		}
	}
}

func TestFeedbackNoteNamesTheReportFile(t *testing.T) {
	w := bragWorld(t, false)
	id := bragStart(t, w)
	w.stop(id, "brag-writer", bragMessage("body\n"), true)
	rev, _ := w.spawn("", "", "brag-reviewer")
	if r := w.stop(rev, "brag-reviewer", feedbackMessage("Claim one is unsupported."), true); r != "" {
		t.Fatal(r)
	}
	st := w.state()
	prefix := "feedback for supernova-focal is in "
	var rel string
	for _, n := range st.Notes {
		if strings.HasPrefix(n, prefix) {
			rel = strings.TrimPrefix(n, prefix)
		}
	}
	if rel == "" || strings.Contains(rel, `\`) || strings.Contains(rel, "unsupported") {
		t.Fatalf("notes = %v", st.Notes)
	}
	if !strings.HasPrefix(rel, ".starship/runs/"+st.Run+"/reports/") {
		t.Errorf("path = %s", rel)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, filepath.FromSlash(rel))); err != nil {
		t.Errorf("the named report does not exist: %v", err)
	}
}

func TestFeedbackMissingHasNoReportNote(t *testing.T) {
	w := bragWorld(t, false)
	id := bragStart(t, w)
	w.stop(id, "brag-writer", bragMessage("body\n"), true)
	rev, _ := w.spawn("", "", "brag-reviewer")
	w.stop(rev, "brag-reviewer", "Nothing came back.\n\n```starship-report\noutcome: missing\nsummary: none\n```\n", true)
	if notes := strings.Join(w.state().Notes, "\n"); strings.Contains(notes, " is in ") {
		t.Errorf("notes = %s", notes)
	}
}

func TestCurationAgentFilePreflight(t *testing.T) {
	agent := func(w *world) string { return filepath.Join(w.bridge, ".claude", "agents", "brag-writer.md") }
	t.Run("linked", func(t *testing.T) {
		w := bragWorld(t, false)
		os.RemoveAll(filepath.Join(w.bridge, ".claude"))
		out := w.run("--pipeline", "document-curate")
		if _, err := os.Stat(agent(w)); err != nil {
			t.Errorf("the agent file was not linked: %v", err)
		}
		if !strings.Contains(out, "restart claude") {
			t.Errorf("no restart line:\n%s", out)
		}
	})
	t.Run("directory existed", func(t *testing.T) {
		w := bragWorld(t, false)
		os.Remove(agent(w))
		out := w.run("--pipeline", "document-curate")
		if _, err := os.Stat(agent(w)); err != nil || strings.Contains(out, "restart claude") {
			t.Errorf("%v\n%s", err, out)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		w := bragWorld(t, false)
		os.RemoveAll(filepath.Join(w.bridge, ".claude"))
		out := w.run("--pipeline", "document-curate", "--dry-run")
		if _, err := os.Stat(filepath.Join(w.bridge, ".claude")); err == nil {
			t.Error("a dry run created agent files")
		}
		if !strings.Contains(out, ".claude/agents/") || !strings.Contains(out, "would run ensign link") {
			t.Errorf("dry run:\n%s", out)
		}
	})
	t.Run("run live", func(t *testing.T) {
		w := bragWorld(t, false)
		w.run("--pipeline", "document-curate")
		os.Remove(agent(w))
		err := w.runErr("--pipeline", "document-curate")
		if err == nil || !strings.Contains(err.Error(), "brag-writer") || !strings.Contains(err.Error(), ".claude/agents/brag-writer.md") || !strings.Contains(err.Error(), "ensign link") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(agent(w)); err == nil {
			t.Error("link ran under a live run")
		}
	})
}

func TestLeftOutEntriesIncludeCandidates(t *testing.T) {
	d := Doc{Entries: []Entry{
		{ID: "D1", Status: "candidate", Theme: "Nowhere"},
		{ID: "D2", Status: "candidate", Theme: "Alpha"},
		{ID: "D3", Status: "active", Theme: "Nowhere"},
	}}
	got := leftOutEntries([]string{"Alpha", "Beta"}, d)
	if len(got) != 2 || got[0].ID != "D1" || got[1].ID != "D3" {
		t.Errorf("left out = %+v", got)
	}
}

func TestStopAdvisoryNoteReadsTheSame(t *testing.T) {
	if got := missingNote("supernova-focal"); got != "feedback for supernova-focal is missing" {
		t.Errorf("note = %q", got)
	}
}

func TestCurationAgentFileIgnoresUnrelatedBrokenFile(t *testing.T) {
	w := bragWorld(t, false)
	agents := filepath.Join(w.bridge, ".claude", "agents")
	write(t, filepath.Join(agents, "README.md"), "no frontmatter\n")
	write(t, filepath.Join(agents, "supernova-notes.md"), "---\nname: [\n---\n")
	out := w.run("--pipeline", "document-curate")
	if strings.Contains(out, "linked") || strings.Contains(out, "is missing") {
		t.Errorf("the unrelated files were read as a missing agent:\n%s", out)
	}
	write(t, filepath.Join(agents, "supernova-reader.md"), "---\nname: supernova-reader\ntools: Read\n---\n")
	if readOnlyRole(w.bridge, "supernova-reader") {
		t.Error("readOnlyRole must fail closed while an unrelated file is broken")
	}
}

func TestCurationAgentFileBrokenOwnFileIsNotMissing(t *testing.T) {
	w := bragWorld(t, false)
	agents := filepath.Join(w.bridge, ".claude", "agents")
	write(t, filepath.Join(agents, "brag-writer.md"), "---\nname: [\n---\n")
	for _, args := range [][]string{{"--pipeline", "document-curate"}, {"--pipeline", "document-curate", "--dry-run"}} {
		err := w.runErr(args...)
		if err == nil || !strings.Contains(err.Error(), "brag-writer.md") || !strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "missing") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(agents, "brag-writer.md")); string(data) != "---\nname: [\n---\n" {
		t.Errorf("link rewrote the broken file:\n%s", data)
	}
}
