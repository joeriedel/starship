// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A declared document is a Markdown file the curator maintains: bridge.yaml names it and, optionally, a rubric that says what belongs in it. Its text can be private, so nothing here puts entry or proposal text into a message, a log line or a command's output.

// DeclaredDocument is one entry of bridge.yaml's documents: key. Path and Rubric are slash-form and relative to the bridge.
type DeclaredDocument struct {
	Name   string
	Path   string
	Rubric string
}

// declaredDocumentsFile reads only the documents: key. It is separate from BridgeFile so that a malformed documents: cannot break any other reader of bridge.yaml.
type declaredDocumentsFile struct {
	Documents yaml.Node `yaml:"documents"`
}

func (d DeclaredDocument) file(bridge string) string {
	return filepath.Join(bridge, filepath.FromSlash(d.Path))
}

func (d DeclaredDocument) rubricFile(bridge string) string {
	return filepath.Join(bridge, filepath.FromSlash(d.Rubric))
}

func pendingDocumentDir(bridge, name string) string {
	return filepath.Join(bridge, "documents", "pending", name)
}

// windowsReserved reports whether a file or folder name is a DOS device name on Windows, including the superscript com and lpt names. The part before the first dot decides.
func windowsReserved(seg string) bool {
	base, _, _ := strings.Cut(strings.ToLower(seg), ".")
	base = strings.TrimRight(base, " ")
	switch base {
	case "con", "prn", "aux", "nul", "conin$", "conout$":
		return true
	}
	if rest, ok := strings.CutPrefix(base, "com"); ok {
		return isDeviceDigit(rest)
	}
	if rest, ok := strings.CutPrefix(base, "lpt"); ok {
		return isDeviceDigit(rest)
	}
	return false
}

func isDeviceDigit(s string) bool {
	return len(s) == 1 && s[0] >= '1' && s[0] <= '9' || s == "¹" || s == "²" || s == "³"
}

// documentPathProblem says why a document or rubric path is not allowed, or "" if it is.
func documentPathProblem(p string) string {
	if why := docsPathProblem(p); why != "" {
		return why
	}
	if strings.Contains(p, `\`) {
		return "must use / separators"
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) }) {
		return `must not contain control characters or any of < > : " | ? *`
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "":
			return "has an empty path segment"
		case strings.HasPrefix(seg, "."):
			return "must not have a path segment starting with a dot"
		case strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " "):
			return "must not have a path segment ending in a dot or a space"
		case windowsReserved(seg):
			return "must not have a path segment Windows reserves (con, prn, aux, nul, conin$, conout$, com1-9, lpt1-9, com¹ com² com³ lpt¹ lpt² lpt³)"
		}
	}
	if !strings.EqualFold(path.Ext(p), ".md") {
		return "must end in .md"
	}
	return ""
}

// documentLocationProblem says why a document may not live at p: only notes/ and projects/<project>/documents/ are committed by the journal and written by nothing else.
func documentLocationProblem(p string) string {
	segs := strings.Split(p, "/")
	switch {
	case segs[0] == "notes" && len(segs) >= 2:
		if len(segs) == 2 && isInboxName(segs[1]) {
			return "must not be the notes inbox"
		}
		return ""
	case segs[0] == "projects" && len(segs) >= 4 && segs[2] == "documents":
		return ""
	}
	return "must be under notes/ or projects/<project>/documents/"
}

// loadDeclaredDocuments reads the documents: key of bridge.yaml, sorted by name. An absent or empty key declares none.
func loadDeclaredDocuments(bridge string) ([]DeclaredDocument, error) {
	data, err := os.ReadFile(filepath.Join(bridge, "bridge.yaml"))
	if err != nil {
		return nil, err
	}
	var df declaredDocumentsFile
	if err := yaml.Unmarshal(data, &df); err != nil {
		return nil, fmt.Errorf("bridge.yaml: documents: %v", err)
	}
	n := &df.Documents
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("bridge.yaml: documents: must be a mapping of document names")
	}
	var out []DeclaredDocument
	names := map[string]bool{}
	paths := map[string]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name, body := n.Content[i].Value, n.Content[i+1]
		if n.Content[i].Kind != yaml.ScalarNode || !declaredSourceNameRe.MatchString(name) {
			return nil, fmt.Errorf("bridge.yaml: documents: name %q must be lower-case letters, digits and hyphens, starting with a letter or digit", name)
		}
		if windowsReserved(name) {
			return nil, fmt.Errorf("bridge.yaml: documents: name %q is a name Windows reserves, and the name becomes a committed folder", name)
		}
		if names[name] {
			return nil, fmt.Errorf("bridge.yaml: documents: %s is declared twice", name)
		}
		names[name] = true
		if body.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("bridge.yaml: documents: %s needs a path", name)
		}
		d := DeclaredDocument{Name: name}
		keys := map[string]bool{}
		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			if keys[key.Value] {
				return nil, fmt.Errorf("bridge.yaml: documents: %s: %s is given twice", name, key.Value)
			}
			keys[key.Value] = true
			if key.Value != "path" && key.Value != "rubric" {
				return nil, fmt.Errorf("bridge.yaml: documents: %s: unknown key %q (a document has only path and rubric)", name, key.Value)
			}
			if val.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("bridge.yaml: documents: %s: %s must be a string", name, key.Value)
			}
			if key.Value == "path" {
				d.Path = val.Value
			} else {
				d.Rubric = val.Value
			}
		}
		if !keys["path"] || d.Path == "" {
			return nil, fmt.Errorf("bridge.yaml: documents: %s: path is missing", name)
		}
		if why := documentPathProblem(d.Path); why != "" {
			return nil, fmt.Errorf("bridge.yaml: documents: %s: path %q %s", name, d.Path, why)
		}
		if why := documentLocationProblem(d.Path); why != "" {
			return nil, fmt.Errorf("bridge.yaml: documents: %s: path %q %s", name, d.Path, why)
		}
		if keys["rubric"] {
			if why := documentPathProblem(d.Rubric); why != "" {
				return nil, fmt.Errorf("bridge.yaml: documents: %s: rubric %q %s", name, d.Rubric, why)
			}
			if strings.EqualFold(d.Rubric, d.Path) {
				return nil, fmt.Errorf("bridge.yaml: documents: %s: rubric is the document itself", name)
			}
		}
		// Lower-cased so that two names cannot reach one file on a case-insensitive filesystem.
		key := strings.ToLower(d.Path)
		if other, dup := paths[key]; dup {
			return nil, fmt.Errorf("bridge.yaml: documents: %s: path is already the path of %s", name, other)
		}
		paths[key] = name
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// findDeclaredDocument returns the declared document of that name.
func findDeclaredDocument(bridge, name string) (DeclaredDocument, error) {
	docs, err := loadDeclaredDocuments(bridge)
	if err != nil {
		return DeclaredDocument{}, err
	}
	for _, d := range docs {
		if d.Name == name {
			return d, nil
		}
	}
	return DeclaredDocument{}, fmt.Errorf("no document %q is declared in bridge.yaml (documents:)", name)
}

var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// quietYAMLProblem describes a YAML error without quoting the document, since a decoder's message can carry a value.
func quietYAMLProblem(err error) string {
	if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
		return "is not valid YAML (near line " + m[1] + " of the block)"
	}
	return "is not valid YAML"
}

const documentFence = "starship-document"

// fencedBlock is a ```name block found in lines: first and last are the line indexes of its fences, body is the text between them.
type fencedBlock struct {
	first, last int
	body        string
}

// fencedBlocks finds every ```name block. A block ends at the first later line that is exactly three backticks, so a line of backticks inside entry text, which YAML indents, never ends it.
func fencedBlocks(lines []string, name string) ([]fencedBlock, error) {
	var out []fencedBlock
	open := -1
	for i, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		switch {
		case open < 0 && l == "```"+name:
			open = i
		case open >= 0 && l == "```":
			var body strings.Builder
			for _, b := range lines[open+1 : i] {
				body.WriteString(strings.TrimRight(b, "\r") + "\n")
			}
			out = append(out, fencedBlock{open, i, body.String()})
			open = -1
		}
	}
	if open >= 0 {
		return nil, fmt.Errorf("a %s block is not closed", name)
	}
	return out, nil
}

// parseDocumentFile finds the one starship-document block of a document file and decodes it.
func parseDocumentFile(data []byte) (Doc, fencedBlock, error) {
	var d Doc
	blocks, err := fencedBlocks(strings.Split(string(data), "\n"), documentFence)
	if err != nil {
		return d, fencedBlock{}, err
	}
	switch len(blocks) {
	case 0:
		return d, fencedBlock{}, fmt.Errorf("has no %s block", documentFence)
	case 1:
	default:
		return d, fencedBlock{}, fmt.Errorf("has %d %s blocks, not one", len(blocks), documentFence)
	}
	if err := yaml.Unmarshal([]byte(blocks[0].body), &d); err != nil {
		return Doc{}, fencedBlock{}, fmt.Errorf("%s block %s", documentFence, quietYAMLProblem(err))
	}
	return d, blocks[0], nil
}

// loadDocumentFile reads a declared document. A missing file is an empty document, so a first curation works; a file that is not in the document format is an error, never an empty document.
func loadDocumentFile(file string) (Doc, error) {
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return Doc{}, nil
	}
	if err != nil {
		return Doc{}, err
	}
	d, _, err := parseDocumentFile(data)
	return d, err
}

// writeDocumentFile writes the document's entries. A new file gets a heading and a note; an existing one keeps all its text outside the block. A file that does not load is never overwritten.
func writeDocumentFile(bridge, file, name string, d Doc) error {
	data, err := os.ReadFile(file)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var block fencedBlock
	if exists {
		if _, block, err = parseDocumentFile(data); err != nil {
			return err
		}
	}
	body, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	cr := ""
	if strings.Contains(string(data), "\r\n") {
		cr = "\r"
	}
	fence := []string{"```" + documentFence}
	fence = append(fence, strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")...)
	fence = append(fence, "```")
	for i := range fence {
		fence[i] += cr
	}
	var out string
	if exists {
		lines := strings.Split(string(data), "\n")
		out = strings.Join(append(append(append([]string{}, lines[:block.first]...), fence...), lines[block.last+1:]...), "\n")
	} else {
		out = "# " + name + "\n\nThe curator maintains this file. Propose changes with `ensign document propose`; the block below is rewritten on each curation.\n\n" + strings.Join(fence, "\n") + "\n"
	}
	return writeFileAtomic(bridge, file, []byte(out))
}

// writeFileAtomic replaces the file through a temp file, creating the file's folder. The temp file lives in the bridge's untracked .starship folder (same volume as the file), so a killed write leaves nothing for the journal to commit.
func writeFileAtomic(bridge, file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmpDir := filepath.Join(bridge, ".starship", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "write-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := renameRetry(tmp.Name(), file); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// documentProblems is why the selected documents cannot be curated, without any of their content: a file that does not load or a rubric that is missing.
func documentProblems(bridge string, docs []DeclaredDocument) []string {
	var out []string
	for _, d := range docs {
		if _, err := loadDocumentFile(d.file(bridge)); err != nil {
			out = append(out, fmt.Sprintf("document %s (%s) %v", d.Name, d.Path, err))
		}
		if d.Rubric != "" {
			if fi, err := os.Stat(d.rubricFile(bridge)); err != nil || fi.IsDir() {
				out = append(out, fmt.Sprintf("document %s: its rubric %s is missing", d.Name, d.Rubric))
				continue
			}
		}
		if isFocalDocument(bridge, d) {
			themes, err := focalThemes(bridge, d)
			if _, ferr := focalFile(bridge, "briefs/entry-format.md"); err != nil || len(themes) == 0 || ferr != nil {
				out = append(out, fmt.Sprintf("document %s (%s): its themes or entry format cannot be read", d.Name, d.Path))
			}
		}
	}
	return out
}

// pendingDocuments resolves a pending-documents stage: the declared documents with at least one proposal waiting. A pending folder for an undeclared name is skipped with a warning that names the folder only.
func pendingDocuments(bridge string) ([]string, error) {
	declared, err := loadDeclaredDocuments(bridge)
	if err != nil {
		return nil, err
	}
	byName := map[string]DeclaredDocument{}
	for _, d := range declared {
		byName[d.Name] = d
	}
	dirs, err := readDirIfAny(filepath.Join(bridge, "documents", "pending"))
	if err != nil {
		return nil, fmt.Errorf("listing documents/pending: %v", err)
	}
	var out []string
	var picked []DeclaredDocument
	for _, e := range dirs {
		if !e.IsDir() {
			continue
		}
		files, err := filesIn(pendingDocumentDir(bridge, e.Name()), ".yaml")
		if err != nil {
			return nil, fmt.Errorf("listing documents/pending/%s: %v", e.Name(), err)
		}
		if len(files) == 0 {
			continue
		}
		d, ok := byName[e.Name()]
		if !ok {
			fmt.Fprintf(os.Stderr, "ensign: documents/pending/%s has proposals but no document of that name is declared; skipped\n", e.Name())
			continue
		}
		out = append(out, d.Name)
		picked = append(picked, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no declared document has pending proposals to curate")
	}
	if problems := documentProblems(bridge, picked); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return out, nil
}

// pendingDocumentInputs reads a document's waiting proposals: their text in file order, and their bridge-relative paths.
func pendingDocumentInputs(bridge, name string) (texts, rels []string, err error) {
	files, err := filesIn(pendingDocumentDir(bridge, name), ".yaml")
	if err != nil {
		return nil, nil, fmt.Errorf("listing documents/pending/%s: %v", name, err)
	}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		texts = append(texts, strings.TrimSpace(string(data)))
		rel, _ := filepath.Rel(bridge, f)
		rels = append(rels, filepath.ToSlash(rel))
	}
	return texts, rels, nil
}

// documentContext is what the curator is given about one declared document.
func documentContext(bridge, name string, d *Dispatch) (string, error) {
	decl, err := findDeclaredDocument(bridge, name)
	if err != nil {
		return "", nil
	}
	cur, _ := loadDocumentFile(decl.file(bridge))
	entries, _ := yaml.Marshal(cur)
	texts, rels, err := pendingDocumentInputs(bridge, name)
	if err != nil {
		return "", err
	}
	d.Fields["inputs"] = append(d.Fields["inputs"], rels...)
	var b strings.Builder
	fmt.Fprintf(&b, "## Document: %s\n\n", name)
	focal := isFocalDocument(bridge, decl)
	if decl.Rubric != "" {
		rubric, _ := os.ReadFile(decl.rubricFile(bridge))
		fmt.Fprintf(&b, "## Rubric: what belongs in this document\n\n%s\n\n", strings.TrimSpace(string(rubric)))
	} else if focal {
		themes, _ := focalFile(bridge, "briefs/default-themes.md")
		fmt.Fprintf(&b, "## Rubric: what belongs in this document\n\n%s\n\n", strings.TrimSpace(themes))
	}
	if focal {
		format, _ := focalFile(bridge, "briefs/entry-format.md")
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(format))
	}
	fmt.Fprintf(&b, "## Its entries now (D ids)\n\n```yaml\n%s```\n\n## Pending proposals\n\n```yaml\n%s\n```\n", entries, strings.Join(texts, "\n---\n"))
	return b.String(), nil
}

// cmdDocument implements `ensign document propose <name> <text> [--evidence <text>]`. It prints the document's name, never the text.
func cmdDocument(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign document propose <name> <text> [--evidence <text>]")
	if len(args) < 3 || args[0] != "propose" {
		return usage
	}
	name, text := args[1], strings.TrimSpace(args[2])
	evidence := ""
	switch {
	case len(args) == 3:
	case len(args) == 5 && args[3] == "--evidence":
		evidence = strings.TrimSpace(args[4])
	default:
		return usage
	}
	if text == "" {
		return fmt.Errorf("the proposal is empty")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	decl, err := findDeclaredDocument(bridge, name)
	if err != nil {
		return err
	}
	if evidence == "" && isFocalDocument(bridge, decl) {
		return fmt.Errorf("document %s is a focal sheet: every claim on it traces to a source, so a proposal needs --evidence", name)
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	// Time and random bytes keep names unique across machines and sessions with no coordination, and contain nothing Windows refuses.
	file := time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd[:]) + ".yaml"
	p := map[string]string{"proposal": text, "date": time.Now().UTC().Format("2006-01-02")}
	if evidence != "" {
		p["evidence"] = evidence
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(bridge, filepath.Join(pendingDocumentDir(bridge, name), file), data); err != nil {
		return err
	}
	fmt.Fprintf(out, "proposed for document %s (documents/pending/%s/%s). Run the document-curate pipeline to fold it in\n", name, name, file)
	return nil
}
