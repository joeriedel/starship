package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const focalPath = "notes/focal/2026-h2.md"

// focalWorld is a bridge with one declared document; use turns the focal-sheet extension on, and rubric (if not empty) is the document's rubric text.
func focalWorld(t *testing.T, use bool, path, rubric string) *world {
	t.Helper()
	y := ""
	if use {
		y = "use: [focal-sheet]\n"
	}
	y += "documents:\n  supernova-focal:\n    path: " + path + "\n"
	if rubric != "" {
		y += "    rubric: notes/focal/rubric.md\n"
	}
	w := connWorld(t, y)
	if rubric != "" {
		write(t, filepath.Join(w.bridge, "notes/focal/rubric.md"), rubric)
	}
	return w
}

func focalEntry(id, theme, text, why, part string) string {
	s := "  - id: " + id + "\n    status: candidate\n    theme: \"" + theme + "\"\n    text: \"" + text + "\"\n"
	if why != "" {
		s += "    why: \"" + why + "\"\n"
	}
	if part != "" {
		s += "    part: \"" + part + "\"\n"
	}
	return s + "    hits: 1\n    evidence: [review]\n"
}

// focalCurator proposes to the document, starts the curation and returns the curator's agent and its prompt.
func focalCurator(w *world) (string, string) {
	w.t.Helper()
	w.propose("supernova-focal", "Nova shipped the launch checklist")
	w.run("--pipeline", "document-curate")
	cur, _ := w.spawn("", "", "curator")
	return cur, w.decodedPromptOf(cur)
}

func focalLedger(ids ...string) string {
	return strings.Join(ids, ": new\n") + ": new\n"
}

// Named without "focal": t.TempDir embeds the test name in the bridge path, and session start prints that path.
func TestSheetLeavesNoTraceWithoutUse(t *testing.T) {
	w := focalWorld(t, false, focalPath, "")
	for name, got := range map[string]string{"help": helpText(w.bridge, w.session), "requests": requestCommands(w.bridge), "session start": sessionStartText(t, w)} {
		if strings.Contains(strings.ToLower(got), "focal") {
			t.Errorf("%s mentions focal without use:\n%s", name, got)
		}
	}
	if !strings.Contains(sessionStartText(t, w), "read `ensign help requests`") {
		t.Errorf("the pointer line is missing")
	}

	w = focalWorld(t, true, focalPath, "")
	if !strings.Contains(strings.ToLower(helpText(w.bridge, w.session)), "focal") || !strings.Contains(requestCommands(w.bridge), "ensign document propose") {
		t.Errorf("use: focal-sheet should show its help")
	}
	if got := requestCommands(w.bridge); !strings.Contains(got, "ensign git-catalog squash") {
		t.Errorf("other extensions' requests changed:\n%s", got)
	}
}

func sessionStartText(t *testing.T, w *world) string {
	t.Helper()
	write(t, filepath.Join(w.bridge, ".starship", "session.md"), "Supernova manual.\n")
	res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
	if code != 0 {
		t.Fatalf("SessionStart exited %d", code)
	}
	return dig(res, "hookSpecificOutput", "additionalContext")
}

func TestFocalDocumentIsOnlyUnderPrefixWithUse(t *testing.T) {
	cases := []struct {
		use   bool
		path  string
		focal bool
	}{
		{true, "notes/focal/2026-h2.md", true},
		{true, "notes/Focal/2026-h2.md", true},
		{true, "notes/supernova-log.md", false},
		{false, "notes/focal/2026-h2.md", false},
	}
	for _, c := range cases {
		w := focalWorld(t, c.use, c.path, "")
		if got := isFocalDocument(w.bridge, DeclaredDocument{Path: c.path}); got != c.focal {
			t.Errorf("use=%v %s: focal = %v", c.use, c.path, got)
		}
		cur, prompt := focalCurator(w)
		if has := strings.Contains(prompt, "Entry format for this document"); has != c.focal {
			t.Errorf("use=%v %s: entry format in prompt = %v", c.use, c.path, has)
		}
		if has := strings.Contains(prompt, "Cross-org reach"); has != c.focal {
			t.Errorf("use=%v %s: default themes in prompt = %v", c.use, c.path, has)
		}
		if !c.focal {
			doc := "entries:\n  - id: D1\n    status: candidate\n    text: Nova owns the checklist\n    hits: 1\n    evidence: [review]\n"
			if r := w.stop(cur, "curator", curatorMessage(doc, "D1: new\n", "updated"), true); r != "" {
				t.Errorf("use=%v %s: a document with no part was refused: %s", c.use, c.path, r)
			}
		}
	}
}

func TestParseThemes(t *testing.T) {
	text := "# Rubric\r\n\r\n## Alpha  \r\nWhat belongs.\r\n\r\n```md\r\n## Hidden\r\n```\r\n\r\n### Not a theme\r\n## Beta\r\n~~~\r\n## Also hidden\r\n~~~\r\n"
	got := parseThemes(text)
	if strings.Join(got, "|") != "Alpha|Beta" {
		t.Errorf("themes = %q", got)
	}
	if got := parseThemes("\ufeff## Alpha\n## Beta\n"); strings.Join(got, "|") != "Alpha|Beta" {
		t.Errorf("themes after a BOM = %q", got)
	}
}

func TestFocalHelpTemplateNamesUsersPart(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "extensions", "focal-sheet", "extension.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var x Extension
	if err := yaml.Unmarshal(data, &x); err != nil {
		t.Fatal(err)
	}
	var propose, start bool
	for _, h := range x.Help {
		if strings.ContainsAny(h.Run, "`\n") {
			t.Errorf("run must have no backtick or newline: %q", h.Run)
		}
		if strings.Contains(h.Run, "ensign document propose") {
			propose = true
			if !strings.Contains(h.Run, "my part") || !strings.Contains(h.Run, "ask the user for their part") {
				t.Errorf("the propose run should carry the user's part: %q", h.Run)
			}
		} else if strings.Contains(h.Run, "notes/focal/") {
			start = true
		}
	}
	if !propose {
		t.Error("focal-sheet has no propose help entry")
	}
	if !start {
		t.Error("focal-sheet has no start entry whose run names notes/focal/")
	}
}

func TestFocalThemesComeFromExtensionByDeclaredName(t *testing.T) {
	w := focalWorld(t, true, focalPath, "")
	dir := filepath.Join(w.bridge, "extensions", "my-focal")
	write(t, filepath.Join(dir, "extension.yaml"), "name: focal-sheet\nsummary: mine\nquiet: true\n")
	write(t, filepath.Join(dir, "briefs", "default-themes.md"), "## Supernova theme\n")
	got, err := focalThemes(w.bridge, DeclaredDocument{Path: focalPath})
	if err != nil || strings.Join(got, "|") != "Supernova theme" {
		t.Errorf("themes = %q, %v", got, err)
	}
}

func TestFocalThemesReachCuratorAndCheckEntries(t *testing.T) {
	w := focalWorld(t, true, focalPath, "")
	cur, prompt := focalCurator(w)
	for _, want := range []string{"Business impact", "Cross-org reach", "Leadership and influence", "Entry format for this document"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the curator is not given %q", want)
		}
	}
	// A rewrite refused for each missing field names the entry and the field, never a value.
	bad := focalEntry("D1", "Business impact", "Nova shipped", "", "") +
		focalEntry("D2", "Made up "+docMarker, "Nova fixed", "", "Nova decided") +
		"  - id: D3\n    status: candidate\n    theme: Business impact\n    text: Nova fixed " + docMarker + "\n    part: Nova caught it\n    hits: 1\n"
	r := w.stop(cur, "curator", curatorMessage("entries:\n"+bad, focalLedger("D1", "D2", "D3"), "updated"), false)
	for _, want := range []string{"entry D1: part is missing", "entry D2: theme is missing or not one of", "entry D3: evidence is missing"} {
		if !strings.Contains(r, want) {
			t.Errorf("refusal lacks %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, docMarker) || strings.Contains(r, "Made up") {
		t.Errorf("a refusal quotes a value:\n%s", r)
	}

	// No why and no number is complete.
	ok := focalEntry("D1", "Business impact", "Nova shipped the checklist", "", "Nova decided the scope")
	if r := w.stop(cur, "curator", curatorMessage("entries:\n"+ok, "D1: new\n", "updated"), true); r != "" {
		t.Errorf("an entry with no why and no number was refused: %s", r)
	}
}

func TestFocalRubricReplacesDefaults(t *testing.T) {
	rubric := "# Rubric\r\n\r\n## Alpha\r\nWhat belongs.\r\n\r\n```md\r\n## Hidden\r\n```\r\n## Beta\r\n"
	w := focalWorld(t, true, focalPath, rubric)
	cur, prompt := focalCurator(w)
	if strings.Contains(prompt, "Cross-org reach") || !strings.Contains(prompt, "## Alpha") {
		t.Errorf("the rubric should replace the default themes:\n%s", prompt)
	}
	good := focalEntry("D1", "Alpha", "Nova shipped", "", "Nova decided")
	for _, theme := range []string{"Hidden", "Business impact"} {
		bad := focalEntry("D1", theme, "Nova shipped", "", "Nova decided")
		if r := w.stop(cur, "curator", curatorMessage("entries:\n"+bad, "D1: new\n", "updated"), false); !strings.Contains(r, "entry D1: theme") {
			t.Errorf("theme %q should be refused: %q", theme, r)
		}
	}
	if r := w.stop(cur, "curator", curatorMessage("entries:\n"+good, "D1: new\n", "updated"), true); r != "" {
		t.Errorf("a rubric theme was refused: %s", r)
	}
}

func TestFocalNoThemesFailsBeforeDispatch(t *testing.T) {
	w := focalWorld(t, true, focalPath, "# Rubric\n\nNo headings here "+docMarker+".\n")
	w.propose("supernova-focal", "Nova shipped")
	err := w.runErr("--pipeline", "document-curate")
	if err == nil || !strings.Contains(err.Error(), "supernova-focal") || !strings.Contains(err.Error(), focalPath) || strings.Contains(err.Error(), docMarker) {
		t.Errorf("want a failure naming the document and path only: %v", err)
	}
}

func TestFocalWritesInThemeOrderAndKeepsFields(t *testing.T) {
	w := focalWorld(t, true, focalPath, "")
	cur, _ := focalCurator(w)
	doc := focalEntry("D1", "Leadership and influence", "Nova led the review", "Others reused it", "Nova directed it") +
		focalEntry("D2", "Business impact", "Nova shipped", "", "Nova decided") +
		focalEntry("D3", "Cross-org reach", "Nova unblocked ops", "", "Nova caught it")
	if r := w.stop(cur, "curator", curatorMessage("entries:\n"+doc, focalLedger("D1", "D2", "D3"), "updated"), true); r != "" {
		t.Fatalf("refused: %s", r)
	}
	got, err := loadDocumentFile(filepath.Join(w.bridge, filepath.FromSlash(focalPath)))
	if err != nil {
		t.Fatal(err)
	}
	var ids, themes []string
	for _, e := range got.Entries {
		ids = append(ids, e.ID)
		themes = append(themes, e.Theme)
	}
	if strings.Join(ids, " ") != "D2 D3 D1" || strings.Join(themes, "|") != "Business impact|Cross-org reach|Leadership and influence" {
		t.Errorf("order: %v %v", ids, themes)
	}
	if e := got.Entries[2]; e.Why != "Others reused it" || e.Part != "Nova directed it" {
		t.Errorf("fields lost: %+v", e)
	}
}

func TestEntryFieldsAreOmittedWhenEmpty(t *testing.T) {
	out, err := yaml.Marshal(Doc{Entries: []Entry{{ID: "S1", Status: "active", Text: "Quote every expansion", Hits: 1, Evidence: []string{"review"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"theme:", "why:", "part:"} {
		if strings.Contains(string(out), key) {
			t.Errorf("an empty entry carries %s:\n%s", key, out)
		}
	}
}

func TestFocalCurateKeepsContentPrivate(t *testing.T) {
	w := focalWorld(t, true, focalPath, "# Rubric\n\n## Theme "+docMarker+"\nWhat belongs.\n")
	proposed := w.propose("supernova-focal", "Nova shipped")
	runOut := proposed + w.run("--pipeline", "document-curate")
	cur, _ := w.spawn("", "", "curator")
	bad := "entries:\n  - id: D1\n    status: candidate\n    theme: Other " + docMarker + "\n    text: Said " + docMarker + "\n    why: Because " + docMarker + "\n    part: Did " + docMarker + "\n    hits: 1\n"
	if r := w.stop(cur, "curator", curatorMessage(bad, "D1: new\n", "updated"), false); strings.Contains(r, docMarker) || !strings.Contains(r, "D1") {
		t.Fatalf("a refusal must name ids and not content: %q", r)
	}
	good := focalEntry("D1", "Theme "+docMarker, "Said "+docMarker, "Because "+docMarker, "Did "+docMarker)
	msg := curatorMessage("entries:\n"+good, "D1: new\n", "updated")
	msg = strings.Replace(msg, "summary: curated", "summary: "+docMarker+"\nfact: shell | "+docMarker+" | x", 1)
	if r := w.stop(cur, "curator", msg, true); r != "" {
		t.Fatalf("a valid rewrite was refused: %s", r)
	}
	if strings.Contains(runOut, docMarker) {
		t.Errorf("run printed content:\n%s", runOut)
	}
	st := w.state()
	for _, h := range st.History {
		if strings.Contains(h.Detail, docMarker) {
			t.Errorf("history carries content: %+v", h)
		}
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
	if _, err := os.Stat(filepath.Join(w.bridge, filepath.FromSlash(focalPath))); err != nil {
		t.Errorf("the sheet was not written: %v", err)
	}
}

// focalRouteWorld is a bridge with the sources the catch-up needs, and optionally the focal sheet and the reader.
func focalRouteWorld(t *testing.T, use, reader bool) *world {
	t.Helper()
	y := ""
	if use {
		y += "use: [focal-sheet]\n"
	}
	if reader {
		y += readerRules
	}
	y += sourcesYAML + "documents:\n  supernova-focal:\n    path: " + focalPath + "\n"
	return connWorld(t, y)
}

func focalOpen(t *testing.T, w *world, front string) string {
	t.Helper()
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	write(t, filepath.Join(taskDir, "task.md"), "---\n"+front+"---\n\n# auth-refactor\n\n## Goal\n\nBody text.\n")
	var out bytes.Buffer
	if err := openTask(w.bridge, "supernova/auth-refactor", taskDir, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestFocalCloseAugmentReachesSession(t *testing.T) {
	w := focalRouteWorld(t, true, false)
	w.frontTask("t", "")
	out, err := closeTask(w, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Closed supernova/t.", "who benefited", "metric", "my part", "ensign document propose", "--evidence"} {
		if !strings.Contains(out, want) {
			t.Errorf("close output lacks %q:\n%s", want, out)
		}
	}
}

func TestCloseWithoutFocalUseIsUnchanged(t *testing.T) {
	w := focalRouteWorld(t, false, false)
	w.frontTask("t", "")
	if out, err := closeTask(w, "t"); err != nil || out != "Closed supernova/t.\n" {
		t.Errorf("close without use: %q, %v", out, err)
	}
}

func TestFocalTicketNotePrintsOnlyWithUseReaderAndTicket(t *testing.T) {
	cases := []struct {
		name        string
		use, reader bool
		front       string
		want        bool
	}{
		{"all three", true, true, twoLinks, true},
		{"no use", false, true, twoLinks, false},
		{"no reader", true, false, twoLinks, false},
		{"no ticket", true, true, "links:\n  - {kind: wiki, ref: \"https://wiki.example/supernova/launch-plan\"}\n", false},
	}
	for _, c := range cases {
		out := focalOpen(t, focalRouteWorld(t, c.use, c.reader), c.front)
		if got := strings.Contains(out, "focal      "); got != c.want {
			t.Errorf("%s: note printed = %v:\n%s", c.name, got, out)
		}
		if c.want && !strings.Contains(out, "reader     before starting work") {
			t.Errorf("%s: reader line missing:\n%s", c.name, out)
		}
	}
}

func TestFocalCatchUpNoteFollowsBrief(t *testing.T) {
	w := focalRouteWorld(t, true, true)
	out, err := w.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	end := strings.Index(out, "End of brief.\n")
	note := strings.Index(out, "focal sheet")
	if end < 0 || note < end || !strings.Contains(out[end:], "\n\n") {
		t.Errorf("note should follow End of brief. after a blank line:\n%s", out)
	}
	w = focalRouteWorld(t, false, true)
	if out, err := w.catchUp(); err != nil || strings.Contains(strings.ToLower(out), "focal") {
		t.Errorf("catch-up without use: %q, %v", out, err)
	}
}

func TestFocalProposeNeedsEvidence(t *testing.T) {
	w := focalWorld(t, true, focalPath, "")
	for _, ev := range [][]string{nil, {"--evidence", ""}, {"--evidence", "  "}} {
		args := append([]string{"propose", "supernova-focal", "Nova shipped " + docMarker}, ev...)
		_, err := w.document(args...)
		if err == nil || !strings.Contains(err.Error(), "supernova-focal") || !strings.Contains(err.Error(), "traces to a source") || strings.Contains(err.Error(), docMarker) {
			t.Errorf("evidence %q: error = %v", ev, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(w.bridge, "documents", "pending", "*", "*")); len(matches) != 0 {
		t.Errorf("refused proposals wrote %v", matches)
	}
	if _, err := w.document("propose", "supernova-focal", "Nova shipped", "--evidence", "user, 2026-11-04"); err != nil {
		t.Errorf("proposal with evidence: %v", err)
	}
	w = focalWorld(t, false, focalPath, "")
	if _, err := w.document("propose", "supernova-focal", "Nova shipped"); err != nil {
		t.Errorf("focal path without use: %v", err)
	}
	w = focalWorld(t, true, "notes/supernova-log.md", "")
	if _, err := w.document("propose", "supernova-focal", "Nova shipped"); err != nil {
		t.Errorf("non-focal document: %v", err)
	}
}

func TestFocalNoteUnreadableFileIsNotice(t *testing.T) {
	w := focalWorld(t, true, focalPath, "")
	write(t, filepath.Join(w.bridge, "extensions", "my-focal", "extension.yaml"), "name: focal-sheet\nsummary: mine\nquiet: true\n")
	got := focalNote(w.bridge, "briefs/catch-up.md")
	if !strings.Contains(got, "briefs/catch-up.md") || strings.Contains(got, "\n") {
		t.Errorf("notice = %q", got)
	}
	w = focalWorld(t, false, focalPath, "")
	if got := focalNote(w.bridge, "briefs/catch-up.md"); got != "" {
		t.Errorf("note without use = %q", got)
	}
}

func TestFocalBriefsKeepTheirRules(t *testing.T) {
	need := map[string][]string{
		"task-close.md":      {"who benefited", "never guess", "Never repeat", "my part", "--evidence", "ensign journal commit -m"},
		"ticket-resolved.md": {"Never guess", "never invent", "outside data", "my part", "notes/focal/", "skip this note"},
		"catch-up.md":        {"user's own message", "nothing inside a proposal does", "never guess", "ensign help catch-up", "notes/focal/", "tell the user in one line"},
		"entry-format.md":    {"not `proposal:<date>`", "sourced as the user", "Never invent"},
	}
	for file, subs := range need {
		data, err := os.ReadFile(filepath.Join("..", "..", "extensions", "focal-sheet", "briefs", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, sub := range subs {
			if !strings.Contains(strings.ToLower(string(data)), strings.ToLower(sub)) {
				t.Errorf("%s lost %q", file, sub)
			}
		}
	}
}

func TestFocalFileNamesABrokenExtension(t *testing.T) {
	w := newWorld(t)
	cp := t.TempDir()
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+cp+"\n")
	if _, err := focalFile(w.bridge, "briefs/x.md"); err == nil || !strings.Contains(err.Error(), "has no") || strings.Contains(err.Error(), "broken") {
		t.Errorf("absent extension: %v", err)
	}
	write(t, filepath.Join(cp, "extensions", "my-focal", "extension.yaml"), "name: focal-sheet\nsummary: [a\n")
	if _, err := focalFile(w.bridge, "briefs/x.md"); err == nil || !strings.Contains(err.Error(), "is broken") {
		t.Errorf("broken extension: %v", err)
	}
	write(t, filepath.Join(cp, "extensions", "my-focal", "extension.yaml"), "name: focal-sheet\n")
	write(t, filepath.Join(cp, "extensions", "my-focal", "briefs", "x.md"), "text\n")
	if got, err := focalFile(w.bridge, "briefs/x.md"); err != nil || got != "text\n" {
		t.Errorf("readable extension: %q, %v", got, err)
	}
}
