// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const documentsYAML = `documents:
  supernova-log:
    path: notes/supernova-log.md
    rubric: notes/supernova-rubric.md
  supernova-plan:
    path: projects/supernova/documents/plan.md
`

const docMarker = "ZZ-MARKER-7F3A"

func docWorld(t *testing.T, path string) *world {
	t.Helper()
	w := connWorld(t, "documents:\n  supernova-log:\n    path: "+path+"\n    rubric: notes/supernova-rubric.md\n")
	write(t, filepath.Join(w.bridge, "notes/supernova-rubric.md"), "# Rubric\n\nOnly launch decisions belong here.\n")
	return w
}

func (w *world) document(args ...string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	err := cmdDocument(args, &out)
	return out.String(), err
}

func (w *world) propose(name, text string) string {
	w.t.Helper()
	out, err := w.document("propose", name, text, "--evidence", "standup")
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func seedDocument(t *testing.T, w *world, rel string) {
	t.Helper()
	d := Doc{Entries: []Entry{{ID: "D1", Status: "active", Text: "Launch slipped to spring", Hits: 1, Evidence: []string{"standup"}}}}
	if err := writeDocumentFile(w.bridge, filepath.Join(w.bridge, filepath.FromSlash(rel)), "supernova-log", d); err != nil {
		t.Fatal(err)
	}
}

func goodRewrite() string {
	return curatorMessage("entries:\n  - id: D1\n    status: active\n    text: Launch slipped to spring\n    hits: 1\n    evidence: [standup]\n"+
		"  - id: D2\n    status: candidate\n    text: Nova owns the launch checklist\n    hits: 1\n    evidence: [review]\n", "D1: kept\nD2: new\n", "updated")
}

func pendingDocFiles(w *world, name string) []string {
	files, _ := filepath.Glob(filepath.Join(w.bridge, "documents", "pending", name, "*.yaml"))
	return files
}

func TestDeclaredDocumentsParse(t *testing.T) {
	w := connWorld(t, documentsYAML)
	got, err := loadDeclaredDocuments(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "supernova-log" || got[1].Name != "supernova-plan" {
		t.Fatalf("want documents sorted by name: %+v", got)
	}
	if got[0].Path != "notes/supernova-log.md" || got[0].Rubric != "notes/supernova-rubric.md" {
		t.Errorf("with a rubric: %+v", got[0])
	}
	if got[1].Path != "projects/supernova/documents/plan.md" || got[1].Rubric != "" {
		t.Errorf("without a rubric: %+v", got[1])
	}
	w = connWorld(t, "")
	if got, err := loadDeclaredDocuments(w.bridge); err != nil || len(got) != 0 {
		t.Errorf("no key declares none: %v %v", got, err)
	}
}

func TestDeclaredDocumentsInvalid(t *testing.T) {
	doc := func(body string) string { return "documents:\n  supernova-log:\n" + body }
	for name, yml := range map[string]string{
		"missing path":                doc("    rubric: notes/r.md\n"),
		"empty body":                  "documents:\n  supernova-log: {}\n",
		"unknown key":                 doc("    path: notes/a.md\n    cursor: 1\n"),
		"instructions key":            doc("    path: notes/a.md\n    instructions: notes/how.md\n"),
		"repeated key":                doc("    path: notes/a.md\n    path: notes/b.md\n"),
		"duplicate name":              "documents:\n  supernova-log:\n    path: notes/a.md\n  supernova-log:\n    path: notes/b.md\n",
		"same path":                   "documents:\n  supernova-log:\n    path: notes/a.md\n  supernova-plan:\n    path: notes/a.md\n",
		"same path, case":             "documents:\n  supernova-log:\n    path: notes/a.md\n  supernova-plan:\n    path: notes/A.MD\n",
		"bad name":                    "documents:\n  Bad_Name:\n    path: notes/a.md\n",
		"absolute":                    doc("    path: /notes/a.md\n"),
		"drive letter":                doc("    path: 'C:/notes/a.md'\n"),
		"dot dot":                     doc("    path: notes/../notes/a.md\n"),
		"backslash":                   doc("    path: 'notes\\a.md'\n"),
		"hidden segment":              doc("    path: notes/.hidden/a.md\n"),
		"hidden file":                 doc("    path: notes/.a.md\n"),
		"empty segment":               doc("    path: notes//a.md\n"),
		"not markdown":                doc("    path: notes/a.txt\n"),
		"project file":                doc("    path: projects/supernova/project.md\n"),
		"task file":                   doc("    path: projects/supernova/tasks/a.md\n"),
		"skill file":                  doc("    path: skills/a.md\n"),
		"top level":                   doc("    path: a.md\n"),
		"wrong case":                  doc("    path: Notes/a.md\n"),
		"notes inbox":                 doc("    path: notes/inbox.md\n"),
		"rubric not md":               doc("    path: notes/a.md\n    rubric: notes/r.txt\n"),
		"rubric is path":              doc("    path: notes/a.md\n    rubric: notes/A.md\n"),
		"rubric escapes":              doc("    path: notes/a.md\n    rubric: ../r.md\n"),
		"path not string":             doc("    path: [notes/a.md]\n"),
		"not a mapping":               "documents: [a, b]\n",
		"body not mapping":            "documents:\n  supernova-log: notes/a.md\n",
		"reserved name":               "documents:\n  con:\n    path: notes/a.md\n",
		"reserved name 2":             "documents:\n  com1:\n    path: notes/a.md\n",
		"reserved path":               doc("    path: notes/con.md\n"),
		"reserved dotted":             doc("    path: notes/aux.draft.md\n"),
		"reserved folder":             doc("    path: notes/NUL/a.md\n"),
		"reserved lpt":                doc("    path: projects/supernova/documents/lpt9.md\n"),
		"reserved superscript folder": doc("    path: notes/com²/a.md\n"),
		"reserved superscript lpt":    doc("    path: notes/lpt¹.md\n"),
		"reserved space":              doc("    path: 'notes/nul .md'\n"),
		"reserved conin":              doc("    path: 'notes/conin$.md'\n"),
		"reserved rubric space":       doc("    path: notes/a.md\n    rubric: 'notes/nul .md'\n"),
		"reserved rubric conout":      doc("    path: notes/a.md\n    rubric: 'notes/conout$.md'\n"),
		"reserved rubric":             doc("    path: notes/a.md\n    rubric: notes/prn.md\n"),
		"colon":                       doc("    path: 'notes/a:b.md'\n"),
		"angle":                       doc("    path: 'notes/a<b.md'\n"),
		"quote":                       doc("    path: 'notes/a\"b.md'\n"),
		"pipe":                        doc("    path: 'notes/a|b.md'\n"),
		"question":                    doc("    path: 'notes/a?b.md'\n"),
		"star":                        doc("    path: 'notes/a*b.md'\n"),
		"control":                     doc("    path: \"notes/a\\tb.md\"\n"),
		"trailing dot":                doc("    path: notes/dir./a.md\n"),
		"trailing space":              doc("    path: 'notes/dir /a.md'\n"),
		"rubric star":                 doc("    path: notes/a.md\n    rubric: 'notes/r*.md'\n"),
		"rubric space":                doc("    path: notes/a.md\n    rubric: 'notes /r.md'\n"),
	} {
		t.Run(name, func(t *testing.T) {
			w := connWorld(t, yml)
			_, err := loadDeclaredDocuments(w.bridge)
			if err == nil || !strings.HasPrefix(err.Error(), "bridge.yaml: documents: ") {
				t.Errorf("want a bridge.yaml: documents: error, got %v", err)
			}
			if _, err := loadBridgeFile(w.bridge); err != nil {
				t.Errorf("a malformed documents: must not break loadBridgeFile: %v", err)
			}
		})
	}
}

func TestDocumentFileFormat(t *testing.T) {
	bridge := t.TempDir()
	file := filepath.Join(bridge, "notes", "supernova-log.md")
	if d, err := loadDocumentFile(file); err != nil || len(d.Entries) != 0 {
		t.Fatalf("a missing file is an empty document: %+v %v", d, err)
	}
	want := Doc{Entries: []Entry{
		{ID: "D1", Status: "active", Text: "Launch slipped to spring", Hits: 2, Evidence: []string{"standup"}},
		{ID: "D2", Status: "candidate", Text: "before\n```\nafter\n```starship-document\nend", Hits: 1, Evidence: []string{"a", "b"}},
	}}
	if err := writeDocumentFile(bridge, file, "supernova-log", want); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if !strings.HasPrefix(string(data), "# supernova-log\n") || strings.Count(string(data), "```starship-document") < 1 {
		t.Errorf("unexpected file:\n%s", data)
	}
	got, err := loadDocumentFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].Hits != 2 || got.Entries[1].Text != want.Entries[1].Text || len(got.Entries[1].Evidence) != 2 {
		t.Errorf("round trip lost something: %+v", got)
	}
	want.Entries = want.Entries[:1]
	if err := writeDocumentFile(bridge, file, "supernova-log", want); err != nil {
		t.Fatal(err)
	}
	if got, err := loadDocumentFile(file); err != nil || len(got.Entries) != 1 {
		t.Errorf("rewrite: %+v %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".*")); len(left) != 0 {
		t.Errorf("a temp file was left behind: %v", left)
	}
}

func TestDocumentFileRefusesWhatItCannotRead(t *testing.T) {
	for name, content := range map[string]string{
		"no block":   "# Notes\n\nHand-written, no block.\n",
		"two blocks": "# x\n\n```starship-document\nentries: []\n```\n\ntext\n\n```starship-document\nentries: []\n```\n",
		"bad yaml":   "# x\n\n```starship-document\nentries: [unclosed\n```\n",
		"unclosed":   "# x\n\n```starship-document\nentries: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			bridge := t.TempDir()
			file := filepath.Join(bridge, "a.md")
			write(t, file, content)
			if _, err := loadDocumentFile(file); err == nil {
				t.Error("loaded")
			}
			if err := writeDocumentFile(bridge, file, "supernova-log", Doc{}); err == nil {
				t.Error("wrote over it")
			}
			if data, _ := os.ReadFile(file); string(data) != content {
				t.Errorf("the file changed:\n%s", data)
			}
		})
	}
}

func TestDocumentFileCRLF(t *testing.T) {
	bridge := t.TempDir()
	file := filepath.Join(bridge, "a.md")
	crlf := "# supernova-log\r\n\r\nKept prose.\r\n\r\n```starship-document\r\nentries:\r\n  - id: D1\r\n    status: active\r\n    text: Launch slipped\r\n    hits: 1\r\n    evidence: [x]\r\n```\r\n\r\nTrailing prose.\r\n"
	write(t, file, crlf)
	d, err := loadDocumentFile(file)
	if err != nil || len(d.Entries) != 1 || d.Entries[0].Text != "Launch slipped" {
		t.Fatalf("a CRLF file did not load: %+v %v", d, err)
	}
	d.Entries[0].Hits = 5
	if err := writeDocumentFile(bridge, file, "supernova-log", d); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	s := string(data)
	if !strings.Contains(s, "Kept prose.\r\n") || !strings.Contains(s, "Trailing prose.\r\n") || regexp.MustCompile(`[^\r]\n`).MatchString(s) {
		t.Errorf("prose or line endings not kept: %q", s)
	}
	if got, err := loadDocumentFile(file); err != nil || got.Entries[0].Hits != 5 {
		t.Errorf("reload: %+v %v", got, err)
	}
}

func TestDocumentPropose(t *testing.T) {
	w := connWorld(t, documentsYAML)
	out, err := w.document("propose", "supernova-log", "Nova owns the "+docMarker, "--evidence", "standup")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, docMarker) || !strings.Contains(out, "supernova-log") {
		t.Errorf("the output must name the document and not echo the text: %q", out)
	}
	files := pendingDocFiles(w, "supernova-log")
	if len(files) != 1 {
		t.Fatalf("want one pending file, got %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), docMarker) || !strings.Contains(string(data), "standup") {
		t.Errorf("proposal file: %s", data)
	}
	if _, err := w.document("propose", "supernova-other", "text"); err == nil {
		t.Error("an undeclared document was accepted")
	}
	if _, err := w.document("propose", "supernova-log", "  "); err == nil {
		t.Error("an empty proposal was accepted")
	}
	if _, err := w.document("propose", "supernova-log"); err == nil {
		t.Error("a missing text was accepted")
	}
	for i := 0; i < 20; i++ {
		w.propose("supernova-log", "again")
	}
	if n := len(pendingDocFiles(w, "supernova-log")); n != 21 {
		t.Errorf("quick calls collided: %d files", n)
	}
	for _, f := range pendingDocFiles(w, "supernova-log") {
		if strings.ContainsAny(filepath.Base(f), `:<>"|?*`) {
			t.Errorf("%s is not safe on Windows", f)
		}
	}
}

func TestDocumentCurate(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	w.propose("supernova-log", "Nova owns the launch checklist")

	w.run("--pipeline", "document-curate")
	cur, why := w.spawn("", "", "curator")
	if cur == "" {
		t.Fatalf("curator refused: %s", why)
	}
	p := w.decodedPromptOf(cur)
	for _, want := range []string{"## Document: supernova-log", "Only launch decisions belong here.", "Launch slipped to spring", "Nova owns the launch checklist", "(D ids)"} {
		if !strings.Contains(p, want) {
			t.Errorf("the curator's prompt lacks %q", want)
		}
	}
	// Proposed after the prompt was built, so it must survive.
	w.propose("supernova-log", "later proposal")

	bad := curatorMessage("entries:\n  - id: D2\n    status: candidate\n    text: Nova owns the launch checklist\n    hits: 1\n    evidence: [review]\n", "D2: new\n", "updated")
	if r := w.stop(cur, "curator", bad, false); !strings.Contains(r, "does not account for D1") {
		t.Fatalf("a rewrite losing D1 was not refused: %q", r)
	}
	if r := w.stop(cur, "curator", goodRewrite(), true); r != "" {
		t.Fatalf("a valid rewrite was refused: %s", r)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s %s", st.Status, st.Reason)
	}
	data, _ := os.ReadFile(filepath.Join(w.bridge, "notes/supernova-log.md"))
	if !strings.HasPrefix(string(data), "# supernova-log\n") {
		t.Errorf("not in the document format:\n%s", data)
	}
	doc, err := loadDocumentFile(filepath.Join(w.bridge, "notes/supernova-log.md"))
	if err != nil || len(doc.Entries) != 2 || doc.Entries[1].ID != "D2" {
		t.Fatalf("entries not written: %+v %v", doc, err)
	}
	left := pendingDocFiles(w, "supernova-log")
	if len(left) != 1 {
		t.Fatalf("only the later proposal should be left: %v", left)
	}
	if data, _ := os.ReadFile(left[0]); !strings.Contains(string(data), "later proposal") {
		t.Errorf("the wrong proposal survived: %s", data)
	}
}

func TestDocumentCurateUnchanged(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	file := filepath.Join(w.bridge, "notes/supernova-log.md")
	before, _ := os.ReadFile(file)
	w.propose("supernova-log", "a one-off")
	w.run("--pipeline", "document-curate")
	cur, _ := w.spawn("", "", "curator")
	if r := w.stop(cur, "curator", "Nothing to change.\n\n```starship-report\noutcome: unchanged\nsummary: none\n```\n", false); r != "" {
		t.Fatalf("unchanged was refused: %s", r)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("unchanged touched the document")
	}
	if left := pendingDocFiles(w, "supernova-log"); len(left) != 0 {
		t.Errorf("considered proposals should be cleared: %v", left)
	}
}

func TestDocumentCurateFirstTimeAndProjectPath(t *testing.T) {
	for _, rel := range []string{"notes/supernova-log.md", "projects/supernova/documents/log.md"} {
		t.Run(rel, func(t *testing.T) {
			w := docWorld(t, rel)
			w.propose("supernova-log", "Nova owns the launch checklist")
			w.run("--pipeline", "document-curate")
			cur, _ := w.spawn("", "", "curator")
			first := curatorMessage("entries:\n  - id: D1\n    status: candidate\n    text: Nova owns the launch checklist\n    hits: 1\n    evidence: [review]\n", "D1: new\n", "updated")
			if r := w.stop(cur, "curator", first, false); r != "" {
				t.Fatalf("a first curation was refused: %s", r)
			}
			doc, err := loadDocumentFile(filepath.Join(w.bridge, filepath.FromSlash(rel)))
			if err != nil || len(doc.Entries) != 1 {
				t.Fatalf("not written: %+v %v", doc, err)
			}
		})
	}
}

func TestDocumentCurateFailsBeforeDispatch(t *testing.T) {
	handWritten := "# Notes\n\nHand-written " + docMarker + ", no block.\n"
	w := docWorld(t, "notes/supernova-log.md")
	file := filepath.Join(w.bridge, "notes/supernova-log.md")
	write(t, file, handWritten)
	w.propose("supernova-log", "text")
	err := w.runErr("--pipeline", "document-curate")
	if err == nil || !strings.Contains(err.Error(), "supernova-log") || !strings.Contains(err.Error(), "starship-document") || strings.Contains(err.Error(), docMarker) {
		t.Errorf("a hand-written file must fail the run, naming the document and not its text: %v", err)
	}
	if data, _ := os.ReadFile(file); string(data) != handWritten {
		t.Errorf("the hand-written file changed")
	}

	w = docWorld(t, "notes/supernova-log.md")
	os.Remove(filepath.Join(w.bridge, "notes/supernova-rubric.md"))
	w.propose("supernova-log", "text")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "supernova-log") || !strings.Contains(err.Error(), "rubric") {
		t.Errorf("a missing rubric must fail the run: %v", err)
	}

	w = docWorld(t, "notes/supernova-log.md")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "no declared document has pending proposals") {
		t.Errorf("no proposals: %v", err)
	}

	// A pending folder for an undeclared name is skipped, so it alone leaves nothing to curate.
	w = docWorld(t, "notes/supernova-log.md")
	write(t, filepath.Join(w.bridge, "documents/pending/supernova-other/x.yaml"), "proposal: text\n")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "no declared document has pending proposals") {
		t.Errorf("undeclared: %v", err)
	}
}

func TestDocumentCurateKeepsContentPrivate(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	write(t, filepath.Join(w.bridge, "skills/shell.md"), "Quote every expansion.\n")
	d := Doc{Entries: []Entry{{ID: "D1", Status: "active", Text: "Old " + docMarker, Hits: 1, Evidence: []string{"standup"}}}}
	if err := writeDocumentFile(w.bridge, filepath.Join(w.bridge, "notes/supernova-log.md"), "supernova-log", d); err != nil {
		t.Fatal(err)
	}
	proposed := w.propose("supernova-log", "Proposal "+docMarker)
	runOut := w.run("--pipeline", "document-curate")
	if strings.Contains(proposed, docMarker) || strings.Contains(runOut, docMarker) {
		t.Errorf("propose or run printed content:\n%s\n%s", proposed, runOut)
	}
	cur, _ := w.spawn("", "", "curator")
	if p := w.decodedPromptOf(cur); strings.Contains(p, "add a line per fact") {
		t.Errorf("a document curator must not be asked for facts")
	}
	bad := curatorMessage("entries:\n  - id: D2\n    status: candidate\n    text: New "+docMarker+"\n    hits: 1\n    evidence: [review]\n", "D2: new\n", "updated")
	if r := w.stop(cur, "curator", bad, false); strings.Contains(r, docMarker) || !strings.Contains(r, "D1") {
		t.Fatalf("a refusal must name ids and not content: %q", r)
	}
	msg := curatorMessage("entries:\n  - id: D2\n    status: candidate\n    text: New "+docMarker+"\n    hits: 1\n    evidence: [review]\n", "D1: \"dropped - "+docMarker+"\"\nD2: new\n", "updated")
	msg = strings.Replace(msg, "summary: curated", "summary: "+docMarker+"\nfact: shell | "+docMarker+" | x", 1)
	if r := w.stop(cur, "curator", msg, true); r != "" {
		t.Fatalf("a valid rewrite was refused: %s", r)
	}
	st := w.state()
	if st.Status != "success" {
		t.Fatalf("status %s %s", st.Status, st.Reason)
	}
	curated := false
	for _, h := range st.History {
		if strings.Contains(h.Detail, docMarker) {
			t.Errorf("history carries content: %+v", h)
		}
		curated = curated || (h.Kind == "curated" && strings.Contains(h.Detail, "notes/supernova-log.md: 1 entries"))
	}
	if !curated {
		t.Errorf("no curated line with the path and entry count: %+v", st.History)
	}
	for _, dp := range st.Dispatches {
		if strings.Contains(dp.Summary, docMarker) {
			t.Errorf("dispatch %s stores the summary", dp.ID)
		}
		for k, v := range dp.Fields {
			if k != "inputs" && strings.Contains(strings.Join(v, " "), docMarker) {
				t.Errorf("dispatch %s stores field %s", dp.ID, k)
			}
		}
	}
	for _, glob := range []string{"skills/pending/*/*.yaml", "skills/proposed/*/*.yaml"} {
		if found, _ := filepath.Glob(filepath.Join(w.bridge, glob)); len(found) != 0 {
			t.Errorf("a fact was recorded: %v", found)
		}
	}
}

func TestJournalChangesPicksUpDocumentsAndProposals(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base"}} {
		if err := git(w.bridge, args...); err != nil {
			t.Fatal(err)
		}
	}
	seedDocument(t, w, "notes/supernova-log.md")
	w.propose("supernova-log", "Nova owns the launch checklist")
	got, err := journalChanges(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "notes/supernova-log.md") || !strings.Contains(joined, "documents/pending/supernova-log/") {
		t.Errorf("journalChanges = %v", got)
	}
}

func TestDocumentRefusalsQuoteNoLedgerValue(t *testing.T) {
	entries := "entries:\n  - id: D1\n    status: active\n    text: Launch slipped to spring\n    hits: 1\n    evidence: [standup]\n"
	for name, c := range map[string]struct{ msg, want string }{
		"merged-into target": {curatorMessage(entries, "D1: merged-into D9 because "+docMarker+"\n", "updated"), "D1 is merged"},
		"malformed id": {curatorMessage(strings.Replace(entries, "id: D1", "id: "+docMarker, 1)+"  - id: D2\n    status: candidate\n    text: x\n    hits: 1\n    evidence: [y]\n",
			"D1: kept\nD2: new\n", "updated"), "an entry id must look like D12"},
		"bad outcome":         {curatorMessage(entries, "D1: kept\n", "updated-"+docMarker), "outcome"},
		"ledger yaml":         {curatorMessage(entries, "D1: [unclosed "+docMarker+"\n", "updated"), "quote values containing colons"},
		"ledger unknown verb": {curatorMessage(entries, "D1: "+docMarker+"\n", "updated"), "must be kept, reworded"},
	} {
		t.Run(name, func(t *testing.T) {
			w := docWorld(t, "notes/supernova-log.md")
			seedDocument(t, w, "notes/supernova-log.md")
			w.propose("supernova-log", "Nova owns the launch checklist")
			w.run("--pipeline", "document-curate")
			cur, _ := w.spawn("", "", "curator")
			r := w.stop(cur, "curator", c.msg, false)
			if r == "" || strings.Contains(r, docMarker) || !strings.Contains(r, c.want) {
				t.Errorf("the refusal must name the problem and quote no value (want %q): %q", c.want, r)
			}
			st, _ := json.Marshal(w.state())
			if strings.Contains(string(st), docMarker) {
				t.Errorf("state.json quotes a ledger value: %s", st)
			}
		})
	}
}

func TestDocumentKeptChangeRefusalQuotesNoText(t *testing.T) {
	const newText = "ZZ-NEW-TEXT-91C2"
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	w.propose("supernova-log", "Nova owns the launch checklist")
	w.run("--pipeline", "document-curate")
	cur, _ := w.spawn("", "", "curator")
	msg := curatorMessage("entries:\n  - id: D1\n    status: active\n    text: "+newText+"\n    hits: 1\n    evidence: [standup]\n", "D1: kept\n", "updated")
	r := w.stop(cur, "curator", msg, false)
	if !strings.Contains(r, "D1 is kept but its text") {
		t.Errorf("want a kept refusal naming the field: %q", r)
	}
	st, _ := json.Marshal(w.state())
	for _, v := range []string{newText, "Launch slipped to spring"} {
		if strings.Contains(r, v) || strings.Contains(string(st), v) {
			t.Errorf("%q leaked into the refusal or the run log", v)
		}
	}
}

func TestDocumentProposalWithoutEvidenceCitesItsDate(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	if _, err := w.document("propose", "supernova-log", "Nova owns the launch checklist"); err != nil {
		t.Fatal(err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	w.run("--pipeline", "document-curate")
	cur, _ := w.spawn("", "", "curator")
	if p := w.decodedPromptOf(cur); !strings.Contains(p, date) {
		t.Errorf("the prompt does not show the proposal's date %s", date)
	}
	msg := curatorMessage("entries:\n  - id: D1\n    status: active\n    text: Launch slipped to spring\n    hits: 1\n    evidence: [standup]\n"+
		"  - id: D2\n    status: candidate\n    text: Nova owns the launch checklist\n    hits: 1\n    evidence: [\"proposal:"+date+"\"]\n", "D1: kept\nD2: new\n", "updated")
	if r := w.stop(cur, "curator", msg, true); r != "" {
		t.Fatalf("a rewrite citing the proposal's date was refused: %s", r)
	}
}

func TestWriteFileAtomicLeavesNothingBesideTheTarget(t *testing.T) {
	bridge := t.TempDir()
	file := filepath.Join(bridge, "documents", "pending", "supernova-log", "a.yaml")
	if err := writeFileAtomic(bridge, file, []byte("x: 1\n")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(file))
	if len(entries) != 1 || entries[0].Name() != "a.yaml" {
		t.Errorf("want only the target beside it, got %v", entries)
	}
	if left, _ := filepath.Glob(filepath.Join(bridge, ".starship", "tmp", "*")); len(left) != 0 {
		t.Errorf("the temp file was not renamed away: %v", left)
	}
}

func TestWindowsReserved(t *testing.T) {
	for _, s := range []string{"con", "CON", "prn", "aux", "nul", "conin$", "conout$", "com1", "com9", "lpt1", "lpt9", "COM1", "com¹", "com²", "com³", "lpt¹", "lpt²", "lpt³", "con.txt", "aux.draft.md", "con.", "nul ", "nul .md"} {
		if !windowsReserved(s) {
			t.Errorf("%q should be reserved", s)
		}
	}
	for _, s := range []string{"", "com", "lpt", "com0", "lpt0", "com10", "lpt10", "com1a", "console", "auxiliary", "nula", "supernova"} {
		if windowsReserved(s) {
			t.Errorf("%q should not be reserved", s)
		}
	}
}

func TestValidateAdvisoryAndEmpty(t *testing.T) {
	ok := Outcomes{{Name: "a", Then: Transition{Finish: "success"}}, {Name: "e", Then: Transition{Error: "x"}}}
	for name, c := range map[string]struct {
		s    Stage
		want string
	}{
		"fine":             {Stage{Advisory: "a", Parallel: &Parallel{Over: "pending-documents", Join: "all", Empty: "a"}}, ""},
		"advisory unknown": {Stage{Advisory: "z"}, "advisory outcome"},
		"advisory error":   {Stage{Advisory: "e"}, "must not be an error"},
		"empty unknown":    {Stage{Parallel: &Parallel{Over: "pending-documents", Join: "all", Empty: "z"}}, "empty outcome"},
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

func TestPrivateStageIsTheDocumentStageOnly(t *testing.T) {
	for curates, want := range map[string]bool{"document": true, "": false, "style": false, "skill": false} {
		if got := privateStage(&Stage{Curates: curates}); got != want {
			t.Errorf("privateStage(%q) = %v", curates, got)
		}
	}
}

func TestNoteOnceAddsANoteOnce(t *testing.T) {
	st := &State{}
	st.noteOnce("one")
	st.noteOnce("one")
	st.noteOnce("two")
	if strings.Join(st.Notes, ",") != "one,two" {
		t.Errorf("notes = %v", st.Notes)
	}
}

func TestStopAdvisoryNoteReadsTheSame(t *testing.T) {
	if got := missingNote("supernova-log"); got != "feedback for supernova-log is missing" {
		t.Errorf("note = %q", got)
	}
}

func TestDocumentCurateIsOneStageThatFinishes(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	seedDocument(t, w, "notes/supernova-log.md")
	w.propose("supernova-log", "Nova owns the launch checklist")
	w.run("--pipeline", "document-curate", "--dry-run")
	w.run("--pipeline", "document-curate")
	if got := w.next(); !strings.Contains(got, "curator") {
		t.Fatalf("the run should start at the curator: %s", got)
	}
	cur, _ := w.spawn("", "", "curator")
	if r := w.stop(cur, "curator", goodRewrite(), true); r != "" {
		t.Fatal(r)
	}
	if st := w.state(); st.Status != "success" || len(st.Notes) != 0 {
		t.Errorf("status %s notes %v", st.Status, st.Notes)
	}
}

func TestFocalConfigurationIsRefused(t *testing.T) {
	w := docWorld(t, "notes/supernova-log.md")
	by := filepath.Join(w.bridge, "bridge.yaml")
	data, _ := os.ReadFile(by)
	write(t, by, string(data)+"    instructions: notes/how.md\n")
	if _, err := loadDeclaredDocuments(w.bridge); err == nil || !strings.Contains(err.Error(), `unknown key "instructions"`) {
		t.Errorf("instructions key: %v", err)
	}
	for _, over := range []string{"focal-documents"} {
		c := &Config{ControlPlane: repoRoot(t), Pipeline: &Pipeline{Stages: []*Stage{{ID: "s", Role: "coder",
			Parallel: &Parallel{Over: over, Join: "all"}, Outcomes: Outcomes{{Name: "a", Then: Transition{Finish: "success"}}}}}}}
		if got := strings.Join(validate(c), "\n"); !strings.Contains(got, "parallel over "+strconv.Quote(over)+" is not supported") {
			t.Errorf("over %s:\n%s", over, got)
		}
	}
}

// twoDocWorld is a bridge with two plain documents, each with one proposal.
func twoDocWorld(t *testing.T) *world {
	t.Helper()
	w := connWorld(t, "documents:\n  supernova-log:\n    path: notes/supernova-log.md\n  supernova-plan:\n    path: notes/supernova-plan.md\n")
	w.propose("supernova-log", "Nova shipped the launch checklist")
	w.propose("supernova-plan", "Nova fixed the gate")
	return w
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

func TestRunDocumentNarrowsTheStage(t *testing.T) {
	w := twoDocWorld(t)
	w.run("--pipeline", "document-curate", "--document", "supernova-log")
	if got := strings.Join(stageItems(t, w)["curate"], ","); got != "supernova-log" {
		t.Errorf("curate items = %q", got)
	}
	if st := w.state(); st.Document != "supernova-log" {
		t.Errorf("State.Document = %q", st.Document)
	}
	if got := pendingDocFiles(w, "supernova-plan"); len(got) != 1 {
		t.Errorf("the other document's proposals = %v", got)
	}
	if id, _ := w.spawn("", "", "curator"); id == "" {
		t.Fatal("the curator was refused")
	}
	if id, _ := w.spawn("", "", "curator"); id != "" {
		t.Error("a second curator was allowed")
	}
}

func TestRunDocumentWithoutTheFlagKeepsEveryDocument(t *testing.T) {
	w := twoDocWorld(t)
	w.run("--pipeline", "document-curate")
	if got := strings.Join(stageItems(t, w)["curate"], ","); got != "supernova-log,supernova-plan" {
		t.Errorf("curate items = %q", got)
	}
}

func TestRunDocumentRefusals(t *testing.T) {
	w := twoDocWorld(t)
	if err := w.runErr("--pipeline", "document-curate", "--document", "supernova-nowhere"); err == nil || !strings.Contains(err.Error(), "supernova-nowhere") {
		t.Errorf("undeclared: %v", err)
	}
	if err := w.runErr("--pipeline", "skill-curate", "--document", "supernova-log"); err == nil || !strings.Contains(err.Error(), "no stage over pending-documents") {
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
	w := twoDocWorld(t)
	by := filepath.Join(w.bridge, "bridge.yaml")
	data, _ := os.ReadFile(by)
	write(t, by, string(data)+"  supernova-notes:\n    path: notes/supernova-notes.md\n    rubric: notes/missing-rubric.md\n")
	w.propose("supernova-notes", "Nova logged the burn")
	if err := w.runErr("--pipeline", "document-curate"); err == nil || !strings.Contains(err.Error(), "supernova-notes") {
		t.Fatalf("the whole run should be refused for the broken document: %v", err)
	}
	w.run("--pipeline", "document-curate", "--document", "supernova-log")
	if got := strings.Join(stageItems(t, w)["curate"], ","); got != "supernova-log" {
		t.Errorf("curate items = %q", got)
	}
}

func TestRunDocumentDryRunAndRetry(t *testing.T) {
	w := twoDocWorld(t)
	out := w.run("--pipeline", "document-curate", "--document", "supernova-log", "--dry-run")
	if !strings.Contains(out, "[supernova-log]") || strings.Contains(out, "supernova-plan") {
		t.Errorf("dry run:\n%s", out)
	}
	w.run("--pipeline", "document-curate", "--document", "supernova-log")
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
	if st := w.runState(snap.Run); st.Document != "supernova-log" {
		t.Errorf("retry State.Document = %q", st.Document)
	}
	for _, s := range snap.Pipeline.Stages {
		if s.Parallel != nil && strings.Join(s.Dimensions, ",") != "supernova-log" {
			t.Errorf("retry stage %s items = %v", s.ID, s.Dimensions)
		}
	}
}

func TestCurationAgentFileUnderAnotherFileName(t *testing.T) {
	w := twoDocWorld(t)
	agents := filepath.Join(w.bridge, ".claude", "agents")
	data, err := os.ReadFile(filepath.Join(agents, "curator.md"))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(agents, "curator.md"))
	write(t, filepath.Join(agents, "my-curator.md"), string(data))
	out := w.run("--pipeline", "document-curate")
	if strings.Contains(out, "linked") {
		t.Errorf("link ran:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(agents, "curator.md")); err == nil {
		t.Error("curator.md was created")
	}
}

func TestCurationAgentFilePreflight(t *testing.T) {
	agent := func(w *world) string { return filepath.Join(w.bridge, ".claude", "agents", "curator.md") }
	t.Run("linked", func(t *testing.T) {
		w := twoDocWorld(t)
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
		w := twoDocWorld(t)
		os.Remove(agent(w))
		out := w.run("--pipeline", "document-curate")
		if _, err := os.Stat(agent(w)); err != nil || strings.Contains(out, "restart claude") {
			t.Errorf("%v\n%s", err, out)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		w := twoDocWorld(t)
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
		w := twoDocWorld(t)
		w.run("--pipeline", "document-curate")
		os.Remove(agent(w))
		err := w.runErr("--pipeline", "document-curate")
		if err == nil || !strings.Contains(err.Error(), "curator") || !strings.Contains(err.Error(), ".claude/agents/curator.md") || !strings.Contains(err.Error(), "ensign link") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(agent(w)); err == nil {
			t.Error("link ran under a live run")
		}
	})
}

func TestCurationAgentFileIgnoresUnrelatedBrokenFile(t *testing.T) {
	w := twoDocWorld(t)
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
	w := twoDocWorld(t)
	agents := filepath.Join(w.bridge, ".claude", "agents")
	write(t, filepath.Join(agents, "curator.md"), "---\nname: [\n---\n")
	for _, args := range [][]string{{"--pipeline", "document-curate"}, {"--pipeline", "document-curate", "--dry-run"}} {
		err := w.runErr(args...)
		if err == nil || !strings.Contains(err.Error(), "curator.md") || !strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "missing") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(agents, "curator.md")); string(data) != "---\nname: [\n---\n" {
		t.Errorf("link rewrote the broken file:\n%s", data)
	}
}

// advisoryWorld has a pipeline of its own with one advisory stage over the pending documents, since no shipped pipeline has one.
func advisoryWorld(t *testing.T) *world {
	t.Helper()
	w := twoDocWorld(t)
	write(t, filepath.Join(w.bridge, "extensions", "supernova-advisory", "extension.yaml"), "name: supernova-advisory\nsummary: an advisory stage\n")
	write(t, filepath.Join(w.bridge, "extensions", "supernova-advisory", "pipelines", "supernova-advise.yaml"), `pipeline: supernova-advise
version: 1
scope: bridge
summary: An advisory stage.
stages:
  - id: check
    role: curator
    dispatched-by: session
    brief: briefs/curate-document.md
    advisory: missing
    parallel: { over: pending-documents, join: all, empty: noted }
    outcomes:
      noted:   { finish: success }
      missing: { finish: success }
`)
	w.run("--pipeline", "supernova-advise", "--document", "supernova-log")
	return w
}

func TestAdvisoryStageNeverFailsTheRun(t *testing.T) {
	missing := "feedback for supernova-log is missing"
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
			if *d.StopsLeft != stopBudget {
				t.Errorf("stop budget spent: %d", *d.StopsLeft)
			}
		}
	}
	for name, msg := range map[string]string{"bad report": "no report here", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			w := advisoryWorld(t)
			id, why := w.spawn("", "", "curator")
			if id == "" {
				t.Fatal(why)
			}
			if r := w.stop(id, "curator", msg, false); r != "" {
				t.Fatalf("the stop was blocked: %s", r)
			}
			check(t, w)
		})
	}
	t.Run("handback", func(t *testing.T) {
		w := advisoryWorld(t)
		id, why := w.spawn("", "", "curator")
		if id == "" {
			t.Fatal(why)
		}
		if r := w.handback(id, "curator", "no report here"); r != "" {
			t.Fatalf("the handback was refused: %s", r)
		}
		check(t, w)
	})
	t.Run("run stop", func(t *testing.T) {
		w := advisoryWorld(t)
		if id, why := w.spawn("", "", "curator"); id == "" {
			t.Fatal(why)
		}
		run := w.state().Run
		if _, err := w.stopRun(run); err != nil {
			t.Fatal(err)
		}
		st := w.runState(run)
		if st.Status != "success" || !strings.Contains(strings.Join(st.Notes, "\n"), missing) {
			t.Errorf("status %s notes %v", st.Status, st.Notes)
		}
	})
	t.Run("reported", func(t *testing.T) {
		w := advisoryWorld(t)
		id, why := w.spawn("", "", "curator")
		if id == "" {
			t.Fatal(why)
		}
		if r := w.stop(id, "curator", "Fine.\n\n```starship-report\noutcome: noted\nsummary: ok\n```\n", true); r != "" {
			t.Fatal(r)
		}
		if st := w.state(); st.Status != "success" || strings.Contains(strings.Join(st.Notes, "\n"), missing) {
			t.Errorf("status %s notes %v", st.Status, st.Notes)
		}
	})
}
