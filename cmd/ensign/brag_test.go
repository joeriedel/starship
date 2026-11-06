// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
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
	for _, want := range []string{"Nova shipped the launch checklist", "INSTR-TEXT", "STYLE-TEXT", "## Document: supernova-focal", "- Alpha", "D2", "Use only the entries with `status: active`"} {
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
	if !strings.Contains(notes, "supernova-focal has active entries with no listed theme, left out unless its instructions bring them in: D2") {
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
		if !strings.Contains(notes, "feedback for supernova-focal2 is missing") || strings.Contains(notes, "feedback for supernova-focal is") {
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
