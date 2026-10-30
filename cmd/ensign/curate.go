// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A curated document — a project's style guide, or a skill's learned entries — is a list
// of entries with stable ids. Only the curator rewrites one, and every rewrite must
// account for every existing entry.

type Entry struct {
	ID       string   `yaml:"id"`
	Status   string   `yaml:"status"` // candidate or active
	Text     string   `yaml:"text"`
	Hits     int      `yaml:"hits"`
	Evidence []string `yaml:"evidence"`
}

type Doc struct {
	Entries []Entry `yaml:"entries"`
}

func loadDoc(path string) (Doc, error) {
	var d Doc
	data, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	err = yaml.Unmarshal(data, &d)
	return d, err
}

// renderDoc is a document as agents read it.
func renderDoc(d Doc, onlyActive bool) string {
	var b strings.Builder
	for _, e := range d.Entries {
		if onlyActive && e.Status != "active" {
			continue
		}
		mark := ""
		if e.Status == "candidate" {
			mark = " (candidate)"
		}
		fmt.Fprintf(&b, "- **%s**%s %s\n", e.ID, mark, strings.TrimSpace(e.Text))
	}
	return b.String()
}

func styleDir(bridge, project string) string {
	return filepath.Join(bridge, "projects", project, "style")
}

func styleGuideFile(bridge, project string) string {
	return filepath.Join(styleDir(bridge, project), "learned.yaml")
}

// StyleRecord is one style pass, as kept in the project's committed style history. The
// run directories are per machine; the history goes with the bridge.
type StyleRecord struct {
	Run  string   `yaml:"run"`
	Date string   `yaml:"date"`
	Task string   `yaml:"task,omitempty"`
	New  int      `yaml:"new"`            // corrections with no rule in the guide yet
	Hits []string `yaml:"hits,omitempty"` // guide entries a correction fell under
}

type StyleHistory struct {
	Passes []StyleRecord `yaml:"passes"`
}

func styleHistoryFile(bridge, project string) string {
	return filepath.Join(styleDir(bridge, project), "history.yaml")
}

func loadStyleHistory(bridge, project string) StyleHistory {
	var h StyleHistory
	readYAML(styleHistoryFile(bridge, project), &h)
	return h
}

// styleRecord appends the style pass that just finished to the project's history.
func styleRecord(r *RunFiles, d *Dispatch) error {
	if d == nil || r.Snap.Project == "" {
		return fmt.Errorf("style record needs a style stage in a project run")
	}
	rec := StyleRecord{Run: r.State.Run, Date: time.Now().Format("2006-01-02"), Task: r.Snap.Task}
	for _, line := range d.Fields["rule"] {
		kind, rest, _ := strings.Cut(line, "|")
		switch strings.TrimSpace(kind) {
		case "new":
			rec.New++
		case "hit":
			if id := strings.TrimSpace(rest); id != "" {
				rec.Hits = append(rec.Hits, id)
			}
		}
	}
	h := loadStyleHistory(r.Snap.Bridge, r.Snap.Project)
	h.Passes = append(h.Passes, rec)
	return writeYAML(styleHistoryFile(r.Snap.Bridge, r.Snap.Project), h)
}

// recurringHits lists guide entries hit in at least min of the last window passes, most
// first. An active entry the coder keeps getting corrected on is badly worded, or isn't
// reaching the coder.
func recurringHits(h StyleHistory, window, min int) []string {
	passes := h.Passes
	if len(passes) > window {
		passes = passes[len(passes)-window:]
	}
	runs := map[string]int{}
	for _, p := range passes {
		seen := map[string]bool{}
		for _, id := range p.Hits {
			if !seen[id] {
				seen[id] = true
				runs[id]++
			}
		}
	}
	var out []string
	for id, n := range runs {
		if n >= min {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if runs[out[i]] != runs[out[j]] {
			return runs[out[i]] > runs[out[j]]
		}
		return out[i] < out[j]
	})
	for i, id := range out {
		out[i] = fmt.Sprintf("%s (%d of the last %d passes)", id, runs[id], len(passes))
	}
	return out
}

// styleReport summarises a project's style history: whether passes need less correcting
// over time, and which entries keep being hit.
func styleReport(bridge, project string) string {
	h := loadStyleHistory(bridge, project)
	if len(h.Passes) == 0 {
		return fmt.Sprintf("No style passes recorded for %s yet.\n", project)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Style passes for %s: %d, from %s to %s.\n", project, len(h.Passes), h.Passes[0].Date, h.Passes[len(h.Passes)-1].Date)
	avg := func(ps []StyleRecord) float64 {
		n := 0
		for _, p := range ps {
			n += p.New + len(p.Hits)
		}
		return float64(n) / float64(len(ps))
	}
	if k := len(h.Passes) / 2; k >= 2 {
		early, late := avg(h.Passes[:k]), avg(h.Passes[len(h.Passes)-k:])
		trend := "about the same"
		switch {
		case late < early*0.8:
			trend = "fewer: the guide is working"
		case late > early*1.2:
			trend = "more: the guide isn't keeping up"
		}
		fmt.Fprintf(&b, "Corrections per pass: %.1f in the first %d, %.1f in the last %d (%s).\n", early, k, late, k, trend)
	} else {
		fmt.Fprintf(&b, "Corrections per pass: %.1f (too few passes for a trend).\n", avg(h.Passes))
	}
	last := h.Passes[len(h.Passes)-1]
	fmt.Fprintf(&b, "Last pass (%s): %d new rule%s, %d correction%s under existing entries.\n",
		last.Run, last.New, plural(last.New), len(last.Hits), plural(len(last.Hits)))
	if rec := recurringHits(h, 10, 3); len(rec) > 0 {
		b.WriteString("Entries the coder keeps being corrected on (reword them, or check they reach the coder):\n")
		for _, r := range rec {
			b.WriteString("  " + r + "\n")
		}
	}
	return b.String()
}

// Source is an ingested style source.
type Source struct {
	ID        string `yaml:"id"`
	Path      string `yaml:"path,omitempty"` // copied document, relative to style/
	Ref       string `yaml:"ref,omitempty"`  // code referenced in place
	Trust     string `yaml:"trust"`          // authoritative or exemplar
	Note      string `yaml:"note,omitempty"`
	Ingested  string `yaml:"ingested"`
	Processed string `yaml:"processed,omitempty"`
}

type Sources struct {
	Sources []Source `yaml:"sources"`
}

func sourcesFile(bridge, project string) string {
	return filepath.Join(styleDir(bridge, project), "sources.yaml")
}

func loadSources(bridge, project string) Sources {
	var s Sources
	readYAML(sourcesFile(bridge, project), &s)
	return s
}

func authoritativeSources(bridge, project string) map[string]bool {
	out := map[string]bool{}
	for _, s := range loadSources(bridge, project).Sources {
		if s.Trust == "authoritative" {
			out[s.ID] = true
		}
	}
	return out
}

var blockRe = func(name string) *regexp.Regexp {
	return regexp.MustCompile("(?s)```" + name + "\\s*\\n(.*?)```")
}

func lastBlock(msg, name string) (string, bool) {
	all := blockRe(name).FindAllStringSubmatch(msg, -1)
	if len(all) == 0 {
		return "", false
	}
	return all[len(all)-1][1], true
}

var sourceRef = regexp.MustCompile(`source:(\S+)`)

const lineBudget = 150

// validateCuration checks a curator's rewrite against the document it replaces.
func validateCuration(prefix string, old, next Doc, ledger map[string]string, authoritative map[string]bool) []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	idRe := regexp.MustCompile(`^` + prefix + `(\d+)$`)
	num := func(id string) int {
		m := idRe.FindStringSubmatch(id)
		if m == nil {
			return -1
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	authoritativeEvidence := func(e Entry) bool {
		for _, ev := range e.Evidence {
			for _, m := range sourceRef.FindAllStringSubmatch(ev, -1) {
				if authoritative[strings.Trim(m[1], ".,;)")] {
					return true
				}
			}
		}
		return false
	}
	before := map[string]Entry{}
	for _, e := range old.Entries {
		before[e.ID] = e
	}
	after := map[string]Entry{}
	for _, e := range next.Entries {
		if num(e.ID) < 0 {
			add("entry id %q must look like %s12", e.ID, prefix)
		}
		if _, dup := after[e.ID]; dup {
			add("entry %s appears twice", e.ID)
		}
		if e.Status != "active" && e.Status != "candidate" {
			add("entry %s: status must be active or candidate", e.ID)
		}
		if strings.TrimSpace(e.Text) == "" {
			add("entry %s has no text", e.ID)
		}
		after[e.ID] = e
	}
	for id, e := range before {
		action, ok := ledger[id]
		if !ok {
			add("the ledger does not account for %s", id)
			continue
		}
		verb, rest, _ := strings.Cut(strings.TrimSpace(action), " ")
		verb = strings.TrimSuffix(verb, ":")
		_, kept := after[id]
		switch verb {
		case "kept", "reworded":
			if !kept {
				add("%s is %s but missing from the document", id, verb)
			}
		case "promoted":
			n := after[id]
			if !kept || e.Status != "candidate" || n.Status != "active" {
				add("%s is promoted, so it must go from candidate to active", id)
			} else if n.Hits < 2 && !authoritativeEvidence(n) {
				add("%s is promoted on one sighting; a candidate needs a second sighting or an authoritative source", id)
			}
		case "merged-into":
			target := strings.TrimSpace(rest)
			if kept {
				add("%s is merged but still in the document", id)
			}
			if _, ok := after[target]; !ok {
				add("%s is merged into %s, which is not in the document", id, target)
			} else if num(target) > num(id) {
				add("%s is merged into %s; a merge keeps the lower id", id, target)
			}
		case "dropped":
			if kept {
				add("%s is dropped but still in the document", id)
			}
			if strings.Trim(rest, " -—:") == "" {
				add("%s is dropped without a reason", id)
			}
		default:
			add("ledger entry for %s must be kept, reworded, promoted, merged-into <id> or dropped <reason>", id)
		}
		if kept && e.Status == "active" && after[id].Status == "candidate" {
			add("%s went from active back to candidate", id)
		}
	}
	for id, e := range after {
		if _, existed := before[id]; existed {
			continue
		}
		if strings.TrimSpace(ledger[id]) != "new" {
			add("%s is new, so the ledger must say new", id)
		}
		if len(e.Evidence) == 0 {
			add("%s is new but has no evidence", id)
		}
		if e.Status == "active" && !authoritativeEvidence(e) {
			add("%s is new and active; a new entry starts as a candidate unless an authoritative source backs it", id)
		}
	}
	if n := strings.Count(renderDoc(next, false), "\n"); n > lineBudget {
		add("the document is %d lines; it must fit in %d — consolidate", n, lineBudget)
	}
	sort.Strings(errs)
	return errs
}

// applyCuration validates a curator's report and writes the document. It returns a
// problem to send back to the curator, or "".
func applyCuration(r *RunFiles, s *Stage, d *Dispatch, msg string) string {
	var path, prefix string
	authoritative := map[string]bool{}
	switch s.Curates {
	case "skill":
		path, prefix = learnedSkillFile(r.Snap.Bridge, d.Dim), "K"
	case "style":
		path, prefix = styleGuideFile(r.Snap.Bridge, r.Snap.Project), "S"
		authoritative = authoritativeSources(r.Snap.Bridge, r.Snap.Project)
	default:
		return ""
	}
	if d.Outcome == "updated" {
		docText, ok1 := lastBlock(msg, "starship-document")
		ledgerText, ok2 := lastBlock(msg, "starship-ledger")
		if !ok1 || !ok2 {
			return "An updated document needs a starship-document block and a starship-ledger block."
		}
		var next Doc
		if err := yaml.Unmarshal([]byte(docText), &next); err != nil {
			return "The starship-document block is not valid YAML: " + err.Error()
		}
		ledger := map[string]string{}
		if strings.TrimSpace(ledgerText) != "" {
			if err := yaml.Unmarshal([]byte(ledgerText), &ledger); err != nil {
				return "The starship-ledger block is not valid YAML (quote values containing colons): " + err.Error()
			}
		}
		old, _ := loadDoc(path)
		if errs := validateCuration(prefix, old, next, ledger, authoritative); len(errs) > 0 {
			return "The rewrite was refused:\n- " + strings.Join(errs, "\n- ")
		}
		if err := writeYAML(path, next); err != nil {
			return "Writing the document failed: " + err.Error()
		}
		r.State.log("curated", s.ID, d.ID, fmt.Sprintf("%s: %d entries; ledger %v", relTo(r.Snap.Bridge, path), len(next.Entries), ledger))
	}
	// The inputs were considered either way.
	switch s.Curates {
	case "skill":
		for _, rel := range d.Fields["inputs"] {
			os.Remove(filepath.Join(r.Snap.Bridge, filepath.FromSlash(rel)))
		}
	case "style":
		markProcessed(r)
	}
	return ""
}

// markProcessed records that the sources this run's generators read are absorbed.
func markProcessed(r *RunFiles) {
	used := map[string]bool{}
	for _, d := range r.State.Dispatches {
		if d.Role == "style-generator" && d.Done {
			used[d.Dim] = true
		}
	}
	if len(used) == 0 {
		return
	}
	src := loadSources(r.Snap.Bridge, r.Snap.Project)
	for i := range src.Sources {
		if used[src.Sources[i].ID] {
			src.Sources[i].Processed = time.Now().Format("2006-01-02") + " by run " + r.State.Run
		}
	}
	writeYAML(sourcesFile(r.Snap.Bridge, r.Snap.Project), src)
}

// generatedAugment renders an augment whose content comes from the bridge at dispatch time.
func generatedAugment(r *RunFiles, name, status string, d *Dispatch) string {
	switch name {
	case "style-guide":
		doc, err := loadDoc(styleGuideFile(r.Snap.Bridge, r.Snap.Project))
		if err != nil || len(doc.Entries) == 0 {
			return ""
		}
		text := renderDoc(doc, status == "active")
		if strings.TrimSpace(text) == "" {
			return ""
		}
		return "## This project's learned style\n\nFollow these. Each was learned from a real correction or a source the project chose.\n\n" + text
	case "style-curate-input":
		return styleCurateInput(r)
	}
	return ""
}

// styleCurateInput is what the style curator is given: the guide, the proposed rules from
// this run, the sources' trust, and the project's own @style.
func styleCurateInput(r *RunFiles) string {
	var b strings.Builder
	cur, _ := os.ReadFile(styleGuideFile(r.Snap.Bridge, r.Snap.Project))
	fmt.Fprintf(&b, "## The style guide now (S ids)\n\n```yaml\n%s```\n\n## Proposed in this run\n\n", string(cur))
	n := 0
	for _, d := range r.State.Dispatches {
		for _, rule := range d.Fields["rule"] {
			fmt.Fprintf(&b, "- from %s (%s): %s\n", d.Role, d.ID, rule)
			n++
		}
	}
	if n == 0 {
		b.WriteString("(nothing)\n")
	}
	if rec := recurringHits(loadStyleHistory(r.Snap.Bridge, r.Snap.Project), 10, 3); len(rec) > 0 {
		b.WriteString("\n## Entries hit run after run\n\nThe coder is given these and still gets corrected on them. Reword each so it can't be misread.\n\n")
		for _, x := range rec {
			b.WriteString("- " + x + "\n")
		}
	}
	if src := loadSources(r.Snap.Bridge, r.Snap.Project); len(src.Sources) > 0 {
		b.WriteString("\n## Sources\n\n")
		for _, s := range src.Sources {
			fmt.Fprintf(&b, "- source:%s — %s\n", s.ID, s.Trust)
		}
	}
	if st, ok := r.Snap.Bindings["style"]; ok && st.Guidance != "" {
		fmt.Fprintf(&b, "\n## The project's @style (outranks everything here)\n\n%s\n", st.Guidance)
	}
	if r.State.Rebuild {
		b.WriteString("\nThis is a rebuild: build the guide afresh from every source, but the ledger must still account for every existing id.\n")
	}
	return b.String()
}

// cmdStyle implements `ensign style ingest <project> <path-or-url> --trust <t> [--note <n>]`.
func cmdStyle(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign style ingest <project> <path-or-url> --trust authoritative|exemplar [--note <text>] | ensign style report <project>")
	if len(args) == 2 && args[0] == "report" {
		bridge, err := cwdBridge()
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(bridge, "projects", args[1], "project.yaml")); err != nil {
			return fmt.Errorf("no project %s", args[1])
		}
		fmt.Fprint(out, styleReport(bridge, args[1]))
		return nil
	}
	if len(args) < 5 || args[0] != "ingest" {
		return usage
	}
	project, from := args[1], args[2]
	var trust, note string
	for i := 3; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--trust":
			trust = args[i+1]
		case "--note":
			note = args[i+1]
		default:
			return usage
		}
	}
	if trust != "authoritative" && trust != "exemplar" {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(bridge, "projects", project, "project.yaml")); err != nil {
		return fmt.Errorf("no project %s", project)
	}
	src := loadSources(bridge, project)
	s := Source{ID: fmt.Sprintf("src%d", len(src.Sources)+1), Trust: trust, Note: note, Ingested: time.Now().Format("2006-01-02")}
	switch {
	case strings.HasPrefix(from, "http://") || strings.HasPrefix(from, "https://"):
		data, err := fetchURL(from)
		if err != nil {
			return err
		}
		s.Path = "sources/" + s.ID + ".txt"
		s.Note = strings.TrimSpace(s.Note + " (from " + from + ")")
		if err := writeFile(filepath.Join(styleDir(bridge, project), filepath.FromSlash(s.Path)), string(data)); err != nil {
			return err
		}
	default:
		fi, err := os.Stat(from)
		if err != nil {
			return err
		}
		if fi.IsDir() || trust == "exemplar" {
			abs, _ := filepath.Abs(from)
			ws, _ := workspaceFor(bridge, project, "")
			s.Ref = workspaceRef(ws, abs)
		} else {
			data, err := os.ReadFile(from)
			if err != nil {
				return err
			}
			s.Path = "sources/" + s.ID + filepath.Ext(from)
			if err := writeFile(filepath.Join(styleDir(bridge, project), filepath.FromSlash(s.Path)), string(data)); err != nil {
				return err
			}
		}
	}
	src.Sources = append(src.Sources, s)
	if err := writeYAML(sourcesFile(bridge, project), src); err != nil {
		return err
	}
	fmt.Fprintf(out, "ingested %s as %s (%s); run style-generate for project %s to build the guide from it\n", from, s.ID, trust, project)
	return nil
}

// fetchURL downloads a source. Its content is data for a generator, never instructions.
func fetchURL(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}
