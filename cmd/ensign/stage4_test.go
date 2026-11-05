// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func (w *world) promptOf(agent string) string {
	data, _ := os.ReadFile(agentTranscript(w.session, agent))
	return string(data)
}

// decodedPromptOf is the prompt the agent was given, with the transcript's JSON escaping undone.
func (w *world) decodedPromptOf(agent string) string {
	var out strings.Builder
	for _, line := range strings.Split(w.promptOf(agent), "\n") {
		var rec struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil {
			out.WriteString(rec.Message.Content)
		}
	}
	return out.String()
}

func TestSkillsSelectionAndDelivery(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "skills/shell.md"), "---\ndescription: POSIX shell\ndefault-roles: [coder, reviewer]\n---\nQuote every expansion.\n")
	write(t, filepath.Join(w.bridge, "skills/big.md"), "---\ndescription: A large reference\ndelivery: on-demand\n---\nLots of text.\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/skills/shell.md"), "Project addition: use set -e.\n")
	w.project("skills: [shell, big]\n")
	task := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/task.md")
	data, _ := os.ReadFile(task)
	write(t, task, strings.Replace(string(data), "pipeline: coding\n", "pipeline: coding\nskills: { roles: { shell: [coder] } }\n", 1))

	c, err := loadConfig(w.bridge, "supernova/auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skills) != 2 || c.Skills[0].Name != "shell" || strings.Join(c.Skills[0].Roles, ",") != "coder" {
		t.Fatalf("skills: %+v", c.Skills)
	}
	if !strings.Contains(c.Skills[0].Text, "Quote every expansion.") || !strings.Contains(c.Skills[0].Text, "use set -e") {
		t.Errorf("project layer should extend the bridge's skill: %q", c.Skills[0].Text)
	}

	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	p := w.promptOf(coder)
	if !strings.Contains(p, "## Skill: shell") || !strings.Contains(p, "Quote every expansion.") {
		t.Errorf("the coder was not given the shell skill")
	}
	if strings.Contains(p, "Lots of text.") || !strings.Contains(p, "`big`") {
		t.Errorf("an on-demand skill should be named, not injected")
	}
	fact := "Done.\n\n```starship-report\noutcome: compiled\nsummary: ok\nfact: shell | set -e does not apply inside a function called in a condition | tested with bash 5\n```\n"
	w.stop(coder, "coder", fact, false)
	pending, _ := filepath.Glob(filepath.Join(w.bridge, "skills/pending/shell/*.yaml"))
	if len(pending) != 1 {
		t.Fatalf("expected one pending fact, got %v", pending)
	}

	reviewer, _ := w.spawn(lead, "orchestrator", "reviewer")
	if strings.Contains(w.promptOf(reviewer), "## Skill: shell") {
		t.Errorf("the task narrowed shell to the coder only")
	}

	files, err := generatedFiles(w.bridge, mustBridgeFile(t, w.bridge), repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for rel := range files {
		if strings.HasPrefix(rel, ".claude/skills/") {
			t.Errorf("link should not generate %s", rel)
		}
	}
}

func mustBridgeFile(t *testing.T, bridge string) *BridgeFile {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		t.Fatal(err)
	}
	return bf
}

func curatorMessage(doc, ledger, outcome string) string {
	return "Here it is.\n\n```starship-document\n" + doc + "```\n\n```starship-ledger\n" + ledger + "```\n\n```starship-report\noutcome: " +
		outcome + "\nsummary: curated\n```\n"
}

func TestSkillCurate(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "skills/shell.md"), "Quote every expansion.\n")
	write(t, filepath.Join(w.bridge, "skills/pending/shell/r1-d1-1.yaml"), "fact: set -e is ignored in conditions\nevidence: bash 5\n")
	write(t, learnedSkillFile(w.bridge, "shell"), "entries:\n  - id: K1\n    status: active\n    text: old fact\n    hits: 1\n    evidence: [x]\n")

	w.run("--pipeline", "skill-curate")
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, "You dispatch its stages yourself") {
		t.Fatalf("a lead-less run should tell the session to dispatch: %q", ctx)
	}
	cur, why := w.spawn("", "", "curator")
	if cur == "" {
		t.Fatalf("curator refused: %s", why)
	}
	if p := w.promptOf(cur); !strings.Contains(p, "set -e is ignored in conditions") || !strings.Contains(p, "old fact") {
		t.Fatalf("the curator was not given the pending facts and current entries")
	}
	// Dropping K1 silently is refused.
	bad := curatorMessage("entries:\n  - id: K2\n    status: candidate\n    text: set -e is ignored in conditions\n    hits: 1\n    evidence: [bash 5]\n", "K2: new\n", "updated")
	if r := w.stop(cur, "curator", bad, false); !strings.Contains(r, "does not account for K1") {
		t.Fatalf("a rewrite losing K1 was not refused: %q", r)
	}
	good := curatorMessage("entries:\n  - id: K1\n    status: active\n    text: old fact\n    hits: 1\n    evidence: [x]\n  - id: K2\n    status: candidate\n    text: set -e is ignored in conditions\n    hits: 1\n    evidence: [bash 5]\n",
		"K1: kept\nK2: new\n", "updated")
	if r := w.stop(cur, "curator", good, true); r != "" {
		t.Fatalf("a valid rewrite was refused: %s", r)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s %s", st.Status, st.Reason)
	}
	doc, _ := loadDoc(learnedSkillFile(w.bridge, "shell"))
	if len(doc.Entries) != 2 {
		t.Fatalf("learned entries not written: %+v", doc)
	}
	if left, _ := filepath.Glob(filepath.Join(w.bridge, "skills/pending/shell/*.yaml")); len(left) != 0 {
		t.Errorf("considered facts should be cleared: %v", left)
	}
}

func TestKeptMeansUnchanged(t *testing.T) {
	base := Entry{ID: "D1", Status: "active", Text: "t", Theme: "th", Why: "w", Part: "p", Hits: 3, Evidence: []string{"a", "a", "b"}}
	old := Doc{Entries: []Entry{base}}
	with := func(f func(e *Entry)) Entry {
		e := base
		e.Evidence = append([]string{}, base.Evidence...)
		f(&e)
		return e
	}
	cases := []struct {
		name   string
		e      Entry
		ledger string
		want   string
	}{
		{"text", with(func(e *Entry) { e.Text = "SECRET" }), "kept", "text"},
		{"theme", with(func(e *Entry) { e.Theme = "SECRET" }), "kept", "theme"},
		{"why", with(func(e *Entry) { e.Why = "SECRET" }), "kept", "why"},
		{"part", with(func(e *Entry) { e.Part = "SECRET" }), "kept", "part"},
		{"status", with(func(e *Entry) { e.Status = "candidate" }), "kept", "status"},
		{"hits fall", with(func(e *Entry) { e.Hits = 2 }), "kept", "hits"},
		{"evidence loses an element", with(func(e *Entry) { e.Evidence = []string{"a", "a"} }), "kept", "evidence"},
		{"evidence loses a duplicate", with(func(e *Entry) { e.Evidence = []string{"a", "b", "c"} }), "kept", "evidence"},
		{"hits rise", with(func(e *Entry) { e.Hits = 5 }), "kept", ""},
		{"evidence grows", with(func(e *Entry) { e.Evidence = []string{"b", "a", "c", "a"} }), "kept", ""},
		{"reworded is free", with(func(e *Entry) { e.Text = "SECRET"; e.Hits = 1 }), "reworded", ""},
	}
	for _, c := range cases {
		for _, private := range []bool{false, true} {
			errs := strings.Join(validateCurationAs(private, "D", old, Doc{Entries: []Entry{c.e}}, map[string]string{"D1": c.ledger}, nil), "; ")
			if c.want == "" && errs != "" {
				t.Errorf("%s: unexpected errors: %s", c.name, errs)
			}
			if c.want != "" && (!strings.Contains(errs, "D1 is kept but its "+c.want) || strings.Contains(errs, "SECRET")) {
				t.Errorf("%s: want a refusal naming %q and no value, got %q", c.name, c.want, errs)
			}
		}
	}
	empty := Doc{Entries: []Entry{{ID: "D1", Status: "active", Text: "t", Hits: 1, Evidence: []string{}}}}
	nilEv := Doc{Entries: []Entry{{ID: "D1", Status: "active", Text: "t", Hits: 1}}}
	if errs := validateCuration("D", empty, nilEv, map[string]string{"D1": "kept"}, nil); len(errs) != 0 {
		t.Errorf("nil and empty evidence differ: %v", errs)
	}
	cand := Doc{Entries: []Entry{{ID: "D1", Status: "candidate", Text: "t", Hits: 2, Evidence: []string{"a"}}}}
	act := Doc{Entries: []Entry{{ID: "D1", Status: "active", Text: "t", Hits: 2, Evidence: []string{"a"}}}}
	if errs := validateCuration("D", cand, act, map[string]string{"D1": "kept"}, nil); len(errs) == 0 {
		t.Errorf("candidate to active under kept was allowed")
	}
}

func TestValidateCuration(t *testing.T) {
	old := Doc{Entries: []Entry{
		{ID: "S1", Status: "active", Text: "a", Hits: 3, Evidence: []string{"x"}},
		{ID: "S2", Status: "active", Text: "b", Hits: 1, Evidence: []string{"x"}},
		{ID: "S3", Status: "candidate", Text: "c", Hits: 1, Evidence: []string{"x"}},
	}}
	cases := []struct {
		name   string
		next   []Entry
		ledger map[string]string
		want   string
	}{
		{"merge into a higher id", []Entry{old.Entries[1], old.Entries[2]},
			map[string]string{"S1": "merged-into S2", "S2": "kept", "S3": "kept"}, "keeps the lower id"},
		{"drop without reason", []Entry{old.Entries[0], old.Entries[1]},
			map[string]string{"S1": "kept", "S2": "kept", "S3": "dropped"}, "without a reason"},
		{"promote on one sighting", []Entry{old.Entries[0], old.Entries[1], {ID: "S3", Status: "active", Text: "c", Hits: 1, Evidence: []string{"x"}}},
			map[string]string{"S1": "kept", "S2": "kept", "S3": "promoted"}, "one sighting"},
		{"new and active without a source", append(append([]Entry{}, old.Entries...), Entry{ID: "S4", Status: "active", Text: "d", Hits: 1, Evidence: []string{"y"}}),
			map[string]string{"S1": "kept", "S2": "kept", "S3": "kept", "S4": "new"}, "starts as a candidate"},
		{"new and active from an authoritative source", append(append([]Entry{}, old.Entries...), Entry{ID: "S4", Status: "active", Text: "d", Hits: 0, Evidence: []string{"source:src1 §2"}}),
			map[string]string{"S1": "kept", "S2": "kept", "S3": "kept", "S4": "new"}, ""},
		{"merge into a lower id", []Entry{old.Entries[0], old.Entries[2]},
			map[string]string{"S1": "kept", "S2": "merged-into S1", "S3": "kept"}, ""},
	}
	for _, c := range cases {
		errs := strings.Join(validateCuration("S", old, Doc{Entries: c.next}, c.ledger, map[string]bool{"src1": true}), "; ")
		if c.want == "" && errs != "" {
			t.Errorf("%s: unexpected errors: %s", c.name, errs)
		}
		if c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("%s: want %q, got %q", c.name, c.want, errs)
		}
	}
}

func TestStyleLearningInACodingRun(t *testing.T) {
	w := newWorld(t)
	write(t, styleGuideFile(w.bridge, "supernova"), "entries:\n  - id: S1\n    status: active\n    text: Name errors err.\n    hits: 2\n    evidence: [x]\n  - id: S2\n    status: candidate\n    text: Prefer early returns.\n    hits: 1\n    evidence: [y]\n")
	write(t, styleHistoryFile(w.bridge, "supernova"), "passes:\n  - {run: r1, date: 2026-10-01, new: 2, hits: [S2]}\n  - {run: r2, date: 2026-10-02, new: 0, hits: [S2, S1]}\n")
	w.project("use: [style-learning]\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	if p := w.promptOf(coder); !strings.Contains(p, "Name errors err.") || strings.Contains(p, "Prefer early returns.") {
		t.Errorf("the coder should get active rules only")
	}
	w.stop(coder, "coder", report("compiled"), false)
	w.leaf(lead, "reviewer", "clean")
	w.leaf(lead, "reviewer", "clean")
	style, _ := w.spawn(lead, "orchestrator", "style-reviewer")
	if p := w.promptOf(style); !strings.Contains(p, "Prefer early returns.") || !strings.Contains(p, "rule: hit | S12") {
		t.Errorf("the style reviewer should get every rule and the rule-reporting brief")
	}
	learned := "Fixed.\n\n```starship-report\noutcome: learned\nsummary: two fixes\nrule: hit | S2\nrule: new | Wrap errors with context | lib.go:12 — added context\n```\n"
	w.stop(style, "style-reviewer", learned, false)
	if st := w.state(); st.Scopes["run"].Stage != "curate" {
		t.Fatalf("learned should go to curate, at %s", st.Scopes["run"].Stage)
	}
	h := loadStyleHistory(w.bridge, "supernova")
	if len(h.Passes) != 3 || h.Passes[2].New != 1 || strings.Join(h.Passes[2].Hits, ",") != "S2" || h.Passes[2].Task != "supernova/auth-refactor" {
		t.Fatalf("the style pass was not recorded: %+v", h.Passes)
	}
	cur, _ := w.spawn(lead, "orchestrator", "curator")
	p := w.promptOf(cur)
	if !strings.Contains(p, "Wrap errors with context") || !strings.Contains(p, "hit | S2") {
		t.Fatalf("the curator was not given this run's proposals")
	}
	if !strings.Contains(p, "Entries hit run after run") || !strings.Contains(p, "S2 (3 of the last 3 passes)") || strings.Contains(p, "S1 (") {
		t.Errorf("the curator should be told S2 keeps being hit, and only S2")
	}
	doc := "entries:\n  - id: S1\n    status: active\n    text: Name errors err.\n    hits: 2\n    evidence: [x]\n" +
		"  - id: S2\n    status: active\n    text: Prefer early returns.\n    hits: 2\n    evidence: [y, lib.go:3]\n" +
		"  - id: S3\n    status: candidate\n    text: Wrap errors with context.\n    hits: 1\n    evidence: [lib.go:12]\n"
	if r := w.stop(cur, "curator", curatorMessage(doc, "S1: kept\nS2: promoted\nS3: new\n", "updated"), false); r != "" {
		t.Fatalf("curation refused: %s", r)
	}
	if st := w.state(); st.Scopes["run"].Stage != "test" {
		t.Fatalf("after curate, at %s", st.Scopes["run"].Stage)
	}
	if d, _ := loadDoc(styleGuideFile(w.bridge, "supernova")); len(d.Entries) != 3 || d.Entries[1].Status != "active" {
		t.Fatalf("guide not updated: %+v", d)
	}
}

func TestStyleIngestAndGenerate(t *testing.T) {
	w := newWorld(t)
	w.project("use: [style-learning]\n")
	doc := filepath.Join(t.TempDir(), "guide.md")
	write(t, doc, "# House style\n\nAlways return early.\n")
	var out bytes.Buffer
	if err := cmdStyle([]string{"ingest", "supernova", doc, "--trust", "authoritative"}, &out); err != nil {
		t.Fatal(err)
	}
	if src := loadSources(w.bridge, "supernova"); len(src.Sources) != 1 || src.Sources[0].ID != "src1" {
		t.Fatalf("sources: %+v", src)
	}
	w.run("--pipeline", "style-generate", "--project", "supernova")
	w.approve()
	gen, why := w.spawn("", "", "style-generator")
	if gen == "" {
		t.Fatalf("generator refused: %s", why)
	}
	if p := w.promptOf(gen); !strings.Contains(p, "Always return early.") || !strings.Contains(p, "trust: authoritative") {
		t.Fatalf("the generator was not given its source")
	}
	w.stop(gen, "style-generator", "```starship-report\noutcome: proposed\nsummary: one rule\nrule: new | Return early | source:src1 §1\n```\n", false)
	cur, _ := w.spawn("", "", "curator")
	next := "entries:\n  - id: S1\n    status: active\n    text: Return early.\n    hits: 0\n    evidence: [\"source:src1 §1\"]\n"
	if r := w.stop(cur, "curator", curatorMessage(next, "S1: new\n", "updated"), false); r != "" {
		t.Fatalf("curation refused: %s", r)
	}
	if st := w.state(); st.Status != "success" {
		t.Fatalf("status %s %s", st.Status, st.Reason)
	}
	if src := loadSources(w.bridge, "supernova"); src.Sources[0].Processed == "" {
		t.Errorf("the source should be marked processed")
	}
	// With everything processed, another run has nothing to do.
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	t.Setenv("STARSHIP_SESSION", "s9")
	if err := cmdRun([]string{"--pipeline", "style-generate", "--project", "supernova"}, &out); err == nil ||
		!strings.Contains(err.Error(), "no unprocessed style sources") {
		t.Fatalf("expected nothing to do: %v", err)
	}
}

func TestFactsForAMissingSkillAreHeld(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "skills/shell.md"), "---\ndescription: POSIX shell\n---\nQuote every expansion.\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	if p := w.promptOf(coder); !strings.Contains(p, "The skills are: shell.") {
		t.Errorf("the coder was not told which skills exist")
	}
	msg := "Done.\n\n```starship-report\noutcome: compiled\nsummary: ok\n" +
		"fact: shell | quoting matters | tested\nfact: make | recipes need tabs | GNU make 4\n```\n"
	w.stop(coder, "coder", msg, false)
	if got, _ := filepath.Glob(filepath.Join(w.bridge, "skills/pending/shell/*.yaml")); len(got) != 1 {
		t.Errorf("a fact for an existing skill should be pending: %v", got)
	}
	if got, _ := filepath.Glob(filepath.Join(w.bridge, "skills/pending/make")); len(got) != 0 {
		t.Errorf("a fact for a missing skill must not be pending")
	}
	if held, _ := proposedSkills(w.bridge); held["make"] != 1 {
		t.Fatalf("held facts: %v", held)
	}
	if s := statusText(w.bridge, w.session); !strings.Contains(s, "make: 1 fact.") {
		t.Errorf("status should list the held facts:\n%s", s)
	}

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdSkill([]string{"new", "shell", "--description", "x"}, &out); err == nil {
		t.Errorf("creating a skill that exists should fail")
	}
	if err := cmdSkill([]string{"new", "make", "--description", "GNU make: Makefiles"}, &out); err != nil {
		t.Fatal(err)
	}
	if known, _ := knownSkills(w.bridge, repoRoot(t), ""); !contains(known, "make") {
		t.Errorf("make should now be a skill")
	}
	if got, _ := filepath.Glob(filepath.Join(w.bridge, "skills/pending/make/*.yaml")); len(got) != 1 {
		t.Errorf("the held fact should now be pending: %v", got)
	}
	if held, _ := proposedSkills(w.bridge); len(held) != 0 {
		t.Errorf("nothing should be held any more")
	}

	write(t, filepath.Join(proposedSkillDir(w.bridge, "awk"), "f.yaml"), "fact: x\n")
	if err := cmdSkill([]string{"discard", "awk"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(proposedSkillDir(w.bridge, "awk")); !os.IsNotExist(err) {
		t.Errorf("discarded facts should be gone")
	}
	if err := cmdSkill([]string{"discard", "awk"}, &out); err == nil {
		t.Errorf("discarding nothing should fail")
	}
}

// The selftest proves SessionStart output up to manualLimit bytes reaches the session;
// the manual must fit inside what was proved.
func TestManualFitsWhatSelftestChecks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "manual", "session.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > manualLimit {
		t.Fatalf("manual/session.md is %d bytes; selftest only proves %d reach the session", len(data), manualLimit)
	}
}

func TestProbeManualSize(t *testing.T) {
	m := probeManual("abc123")
	if len(m) > manualLimit || len(m) < manualLimit*9/10 {
		t.Fatalf("probe block is %d bytes, want close to %d", len(m), manualLimit)
	}
	if !sawWholeManual(m, "abc123") || sawWholeManual(m[:len(m)/2], "abc123") {
		t.Fatalf("markers should all be in the block, and not in half of it")
	}
}

func TestStyleReport(t *testing.T) {
	w := newWorld(t)
	if r := styleReport(w.bridge, "supernova"); !strings.Contains(r, "No style passes") {
		t.Errorf("empty history: %q", r)
	}
	write(t, styleHistoryFile(w.bridge, "supernova"), "passes:\n"+
		"  - {run: r1, date: 2026-10-01, new: 4, hits: [S1]}\n"+
		"  - {run: r2, date: 2026-10-02, new: 3, hits: [S1, S1]}\n"+
		"  - {run: r3, date: 2026-10-03, new: 0, hits: [S1]}\n"+
		"  - {run: r4, date: 2026-10-04, new: 0, hits: [S2]}\n")
	r := styleReport(w.bridge, "supernova")
	for _, want := range []string{"Style passes for supernova: 4", "5.0 in the first 2, 1.0 in the last 2 (fewer: the guide is working)",
		"Last pass (r4): 0 new rules, 1 correction", "S1 (3 of the last 4 passes)"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "S2 (") {
		t.Errorf("S2 was hit once and should not be flagged:\n%s", r)
	}
}

func TestSessionIsToldRequestCommands(t *testing.T) {
	w := newWorld(t)
	cmds := requestCommands(w.bridge)
	for _, want := range []string{"ensign run --pipeline skill-curate", "ensign style report <project>", "ensign git-catalog squash"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q:\n%s", want, cmds)
		}
	}
	if strings.Contains(helpText(w.bridge, w.session), "ensign") {
		t.Errorf("help must not mention ensign")
	}
}

func (w *world) onDemandFile(name string) string {
	w.t.Helper()
	found, _ := filepath.Glob(filepath.Join(w.bridge, ".starship", "runs", "*", "skills", name+".md"))
	if len(found) != 1 {
		w.t.Fatalf("expected one run file for %s, got %v", name, found)
	}
	return found[0]
}

func TestOnDemandSkillFile(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "skills/big.md"), "---\ndescription: A large reference\ndelivery: on-demand\ndefault-roles: [coder]\n---\nBridge text.\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/skills/big.md"), "Project text.\n")
	write(t, filepath.Join(w.bridge, "projects/supernova/skills/plain.md"), "---\ndelivery: on-demand\ndefault-roles: [coder]\n---\nPlain body.\n")
	w.project("skills: [big, plain]\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	p := w.decodedPromptOf(coder)
	path := w.onDemandFile("big")
	if !strings.Contains(p, "`big`") || !strings.Contains(p, "A large reference") || !strings.Contains(p, path) {
		t.Errorf("the coder's listing lacks the skill or its path %s", path)
	}
	if !strings.Contains(p, "Knowledge of plain") || !strings.Contains(p, w.onDemandFile("plain")) {
		t.Errorf("a skill with no description should get the default")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "Bridge text.") || !strings.Contains(string(data), "Project text.") || !strings.HasSuffix(string(data), "\n") {
		t.Errorf("skill file: %q", data)
	}
	if strings.Contains(p, "Project text.") {
		t.Errorf("the body should not be in the prompt")
	}
	w.stop(coder, "coder", "Done.\n\n```starship-report\noutcome: compiled\nsummary: ok\n```\n", false)
	reviewer, reason := w.spawn(lead, "orchestrator", "reviewer")
	if reviewer == "" {
		t.Fatalf("the reviewer spawn was refused: %s", reason)
	}
	if strings.Contains(w.decodedPromptOf(reviewer), "`big`") {
		t.Errorf("the reviewer is not listed this skill")
	}
}

func TestOnDemandSkillNameMustBeOneElement(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "x.md"), "---\ndelivery: on-demand\n---\nText.\n")
	w.project("skills: [\"../x\"]\n")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	const want = "a skill name must be a single path element"
	var out bytes.Buffer
	err := cmdRun([]string{"supernova/auth-refactor", "--dry-run"}, &out)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a dry run with skill ../x should report %q: %v\n%s", want, err, out.String())
	}
	out.Reset()
	err = cmdRun([]string{"supernova/auth-refactor"}, &out)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a run with skill ../x should fail with %q: %v\n%s", want, err, out.String())
	}
	entries, _ := os.ReadDir(filepath.Join(w.bridge, ".starship", "runs"))
	if len(entries) != 0 {
		t.Errorf("a refused run should leave no folder: %v", entries)
	}
}

func TestLinkRemovesEmptyStaleSkillFolders(t *testing.T) {
	w := linkedWorld(t)
	stale := []byte("old\n")
	write(t, filepath.Join(w.bridge, ".claude/skills/big/SKILL.md"), string(stale))
	mine := filepath.Join(w.bridge, ".claude/skills/mine/SKILL.md")
	write(t, mine, "mine\n")
	manifestPath := filepath.Join(w.bridge, ".starship", "link.yaml")
	var m linkManifest
	readYAML(manifestPath, &m)
	m.Files[".claude/skills/big/SKILL.md"] = sha(stale)
	if err := writeYAML(manifestPath, m); err != nil {
		t.Fatal(err)
	}
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.bridge, ".claude/skills/big")); !os.IsNotExist(err) {
		t.Errorf("the stale folder should be gone: %v", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("a file outside the manifest must stay: %v", err)
	}
}

func TestSkillNewRefusesWindowsReservedNames(t *testing.T) {
	w := newWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	for _, name := range []string{"con", "com1"} {
		err := cmdSkill([]string{"new", name, "--description", "x"}, &out)
		if err == nil || !strings.Contains(err.Error(), "Windows reserves") {
			t.Errorf("skill new %s: %v", name, err)
		}
		if dirHasEntry(filepath.Join(w.bridge, "skills"), name+".md") || dirHasEntry(filepath.Join(w.bridge, "skills/proposed"), name) {
			t.Errorf("skill new %s wrote something", name)
		}
	}
}

// dirHasEntry lists dir rather than stat-ing the name, which Windows can resolve to a DOS device; a missing dir has no entries.
func dirHasEntry(dir, name string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func TestSkillDiscardClearsReservedName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a folder named con can't exist on Windows")
	}
	w := newWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	write(t, filepath.Join(proposedSkillDir(w.bridge, "con"), "f.yaml"), "fact: x\n")
	var out bytes.Buffer
	if err := cmdSkill([]string{"discard", "con"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(proposedSkillDir(w.bridge, "con")); !os.IsNotExist(err) {
		t.Errorf("discarded facts should be gone")
	}
}

func TestFactForReservedSkillNameIsSkipped(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "skills/shell.md"), "---\ndescription: POSIX shell\n---\nQuote every expansion.\n")
	lead := w.startLead()
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	msg := "Done.\n\n```starship-report\noutcome: compiled\nsummary: ok\n" +
		"fact: con | reserved | x\nfact: shell | quoting matters | tested\n```\n"
	w.stop(coder, "coder", msg, false)
	for _, d := range []string{"pending", "proposed"} {
		if dirHasEntry(filepath.Join(w.bridge, "skills", d), "con") {
			t.Errorf("skills/%s/con was created", d)
		}
	}
	if got, _ := filepath.Glob(filepath.Join(w.bridge, "skills/pending/shell/*.yaml")); len(got) != 1 {
		t.Errorf("the normal fact should be pending: %v", got)
	}
}
