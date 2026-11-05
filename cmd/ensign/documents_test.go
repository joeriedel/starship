// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
