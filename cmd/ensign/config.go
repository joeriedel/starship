// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"gopkg.in/yaml.v3"
)

// Transition is what happens when a stage reports an outcome.
type Transition struct {
	Next    bool   `json:"next,omitempty"`
	Goto    string `json:"goto,omitempty"`
	Finish  string `json:"finish,omitempty"`
	Error   string `json:"error,omitempty"`
	AskUser string `json:"ask_user,omitempty"`
	Spend   string `json:"spend,omitempty"`
	Reset   string `json:"reset,omitempty"`
}

func (t *Transition) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "next" {
			return fmt.Errorf("line %d: transition %q: only \"next\" may be written bare", n.Line, n.Value)
		}
		t.Next = true
		return nil
	}
	var m map[string]string
	if err := n.Decode(&m); err != nil {
		return fmt.Errorf("line %d: transition: %v", n.Line, err)
	}
	for k, v := range m {
		switch k {
		case "goto":
			t.Goto = v
		case "finish":
			t.Finish = v
		case "error":
			t.Error = v
		case "ask-user":
			t.AskUser = v
		case "spend":
			t.Spend = v
		case "reset":
			t.Reset = v
		default:
			return fmt.Errorf("line %d: unknown transition key %q", n.Line, k)
		}
	}
	return nil
}

func (t Transition) String() string {
	var s string
	switch {
	case t.Next:
		s = "next"
	case t.Goto != "":
		s = "goto " + t.Goto
	case t.Finish != "":
		s = "finish " + t.Finish
	case t.Error != "":
		s = "error " + t.Error
	case t.AskUser != "":
		s = "ask-user"
	}
	if t.Spend != "" {
		s += ", spend " + t.Spend
	}
	if t.Reset != "" {
		s += ", reset " + t.Reset
	}
	return s
}

func (t Transition) terminal() bool { return t.Finish != "" || t.Error != "" || t.AskUser != "" }

type Outcome struct {
	Name string     `json:"name"`
	Then Transition `json:"then"`
}

// Outcomes keeps YAML order: for a parallel stage the order is the join's precedence.
type Outcomes []Outcome

func (o *Outcomes) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: outcomes must be a mapping", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		var t Transition
		if err := n.Content[i+1].Decode(&t); err != nil {
			return err
		}
		*o = append(*o, Outcome{Name: n.Content[i].Value, Then: t})
	}
	return nil
}

func (o Outcomes) find(name string) (Transition, bool) {
	for _, x := range o {
		if x.Name == name {
			return x.Then, true
		}
	}
	return Transition{}, false
}

func (o Outcomes) names() []string {
	var out []string
	for _, x := range o {
		out = append(out, x.Name)
	}
	return out
}

type Parallel struct {
	Over string `yaml:"over" json:"over"`
	Join string `yaml:"join" json:"join"`
	// Empty is the outcome a stage takes, without a dispatch, when it has no items.
	Empty string `yaml:"empty" json:"empty,omitempty"`
}

// StageSkip says a stage is not run unless a binding declares paths; the stage then takes Outcome's transition.
type StageSkip struct {
	UnlessBinding string `yaml:"unless-binding" json:"unless_binding"`
	Outcome       string `yaml:"outcome" json:"outcome"`
}

type Stage struct {
	ID           string    `yaml:"id" json:"id"`
	Role         string    `yaml:"role" json:"role"`
	DispatchedBy string    `yaml:"dispatched-by" json:"dispatched_by,omitempty"`
	Brief        string    `yaml:"brief" json:"brief,omitempty"`
	Spend        string    `yaml:"spend" json:"spend,omitempty"`
	Parallel     *Parallel `yaml:"parallel" json:"parallel,omitempty"`
	Dimensions   []string  `yaml:"dimensions" json:"dimensions,omitempty"`
	Outcomes     Outcomes  `yaml:"outcomes" json:"outcomes,omitempty"`
	ReportFields []string  `yaml:"report-fields" json:"report_fields,omitempty"`
	// Skip takes the named outcome without a dispatch when the binding declares nothing.
	Skip *StageSkip `yaml:"skip" json:"skip,omitempty"`
	// Curates names what this stage's agents write: style, skill or document.
	Curates string `yaml:"curates" json:"curates,omitempty"`
	// Advisory is the outcome a dispatch takes when it fails to report: nothing on this stage fails the run or retries.
	Advisory string   `yaml:"advisory" json:"advisory,omitempty"`
	Stages   []*Stage `yaml:"stages" json:"stages,omitempty"`
	Source   string   `yaml:"-" json:"source"`
	line     int
}

func (s *Stage) UnmarshalYAML(n *yaml.Node) error {
	type plain Stage
	if err := n.Decode((*plain)(s)); err != nil {
		return err
	}
	s.line = n.Line
	return nil
}

func (s *Stage) isLead() bool { return len(s.Stages) > 0 }

type Pipeline struct {
	Name             string                `yaml:"pipeline" json:"pipeline"`
	Version          int                   `yaml:"version" json:"version"`
	Summary          string                `yaml:"summary" json:"summary"`
	RequiresBindings []string              `yaml:"requires-bindings" json:"requires_bindings"`
	Budgets          map[string]int        `yaml:"budgets" json:"budgets"`
	OnExhausted      map[string]Transition `yaml:"on-exhausted" json:"on_exhausted"`
	FreeRoles        []string              `yaml:"free-roles" json:"free_roles"`
	Help             []HelpEntry           `yaml:"help" json:"help,omitempty"`
	Scope            string                `yaml:"scope" json:"scope,omitempty"` // task (default), project or bridge
	Stages           []*Stage              `yaml:"stages" json:"stages"`
	// Sources records where non-stage fields were last set, e.g. "budgets.test".
	Sources map[string]string `yaml:"-" json:"sources"`
}

// hasStageRole reports whether any stage, nested ones included, has this role.
func (p *Pipeline) hasStageRole(role string) bool {
	var walk func(list []*Stage) bool
	walk = func(list []*Stage) bool {
		for _, s := range list {
			if s.Role == role || walk(s.Stages) {
				return true
			}
		}
		return false
	}
	return walk(p.Stages)
}

// find returns the stage with this id, and the list (scope) that holds it.
func (p *Pipeline) find(id string) (*Stage, *[]*Stage, string) {
	var walk func(list *[]*Stage, scope string) (*Stage, *[]*Stage, string)
	walk = func(list *[]*Stage, scope string) (*Stage, *[]*Stage, string) {
		for _, s := range *list {
			if s.ID == id {
				return s, list, scope
			}
			if found, l, sc := walk(&s.Stages, s.ID); found != nil {
				return found, l, sc
			}
		}
		return nil, nil, ""
	}
	return walk(&p.Stages, "")
}

// scopeList returns the stages dispatched from a scope: "" is the session, otherwise a lead's id.
func (p *Pipeline) scopeList(scope string) []*Stage {
	if scope == "" {
		return p.Stages
	}
	if s, _, _ := p.find(scope); s != nil {
		return s.Stages
	}
	return nil
}

// Binding is what a @name means in a project: a command, guidance, or a declared absence.
type Binding struct {
	Command       string `json:"command,omitempty"`
	Guidance      string `json:"guidance,omitempty"`
	NotApplicable string `json:"not_applicable,omitempty"`
	// Paths are workspace-relative files and folders, for a binding that names places rather than a command (@docs).
	Paths []string `json:"paths,omitempty"`
	// Plain says the command needs no judgement to run: ensign runs it and reads the exit
	// code. On @test it means ensign runs the test stage itself, with no tester agent.
	Plain bool `json:"plain,omitempty"`
	// Timeout limits a plain binding's command; zero means the default.
	Timeout time.Duration `json:"timeout,omitempty"`
	Source  string        `json:"source"`
}

// noVariantError says a binding has platform variants but none for the platform asked about.
type noVariantError struct{ msg string }

func (e *noVariantError) Error() string { return e.msg }

// parseBinding resolves platform variants for this machine. An alias is followed wherever a value is read.
func parseBinding(n *yaml.Node, source string) (Binding, error) {
	b := Binding{Source: source}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode {
		return b, fmt.Errorf("%s: a binding must be a mapping (command, guidance, not-applicable, or per-platform)", source)
	}
	// Decoding into any descends the whole binding, so yaml refuses a cycle anywhere in it, including under another platform's key.
	if _, err := decodeAny(n); err != nil {
		return b, fmt.Errorf("%s: %v", source, err)
	}
	var m map[string]yaml.Node
	if err := n.Decode(&m); err != nil {
		return b, fmt.Errorf("%s: %v", source, err)
	}
	if v, ok := m[runtime.GOOS]; ok {
		b, err := parseBinding(&v, source)
		var nv *noVariantError
		if errors.As(err, &nv) {
			err = errors.New(nv.msg)
		}
		return b, err
	}
	platformKey := false
	for k, v := range m {
		if v.Kind == yaml.AliasNode {
			v = *v.Alias
		}
		switch k {
		case "command":
			b.Command = v.Value
		case "guidance":
			b.Guidance = v.Value
		case "not-applicable":
			b.NotApplicable = v.Value
		case "paths":
			if err := v.Decode(&b.Paths); err != nil {
				return b, fmt.Errorf("%s: paths must be a list of strings", source)
			}
		case "plain":
			if err := v.Decode(&b.Plain); err != nil {
				return b, fmt.Errorf("%s: plain must be true or false", source)
			}
		case "timeout":
			d, err := time.ParseDuration(v.Value)
			if err != nil || d <= 0 {
				return b, fmt.Errorf("%s: timeout must be a positive duration such as 90m", source)
			}
			b.Timeout = d
		case "linux", "windows", "darwin":
			platformKey = true
		default:
			return b, fmt.Errorf("%s: unknown binding key %q", source, k)
		}
	}
	// Returned after the loop, not from it: the loop ranges over a map, so returning mid-loop would make this error and another key's error a matter of map order.
	if platformKey {
		return b, &noVariantError{fmt.Sprintf("%s: has platform variants but none for %s", source, runtime.GOOS)}
	}
	if b.Plain && b.Command == "" {
		return b, fmt.Errorf("%s: a plain binding needs a command", source)
	}
	if len(b.Paths) > 0 && (b.Command != "" || b.Guidance != "") {
		return b, fmt.Errorf("%s: a binding with paths cannot also have a command or guidance", source)
	}
	return b, nil
}

// checkBinding refuses what parseBinding refuses on this machine, so the drift report is never stricter than a run: the per-name `@docs` rules in Config validation (a command or guidance, path checks) are not applied, because they judge the merged config after all layers, not one file's value. A bad value under another platform's key is not looked at, as a run doesn't look at it. The one thing it lets through is a binding with no variant for this machine at its top level, because the settings file serves every platform and one windows-only binding must not blank the whole report on Linux. A variant for this machine that is itself unusable (`{linux: {windows: {command: x}}}` on Linux) is refused. Cycles anywhere are refused by parseBinding's decodeAny.
func checkBinding(n *yaml.Node, source string) error {
	_, err := parseBinding(n, source)
	var nv *noVariantError
	if errors.As(err, &nv) {
		return nil
	}
	return err
}

// Rule is one standing run rule and where it was declared.
type Rule struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

// Layer is the shape shared by bridge.yaml, project.yaml and task.md frontmatter.
type Layer struct {
	Pipelines map[string]struct {
		Ops []yaml.Node `yaml:"ops"`
	} `yaml:"pipelines"`
	Bindings  yaml.Node                `yaml:"bindings"`
	Rules     yaml.Node                `yaml:"rules"` // standing run rules, one string each; the task layer's are ignored
	Use       []string                 `yaml:"use"`
	Skills    yaml.Node                `yaml:"skills"`
	Workspace string                   `yaml:"workspace"` // the old place for a code location, now in machine.yaml; read only to move it
	Events    map[string][]HandlerSpec `yaml:"events"`
	Journal   struct {
		Commit string `yaml:"commit"`
		Reason string `yaml:"reason"`
	} `yaml:"journal"`
	Platforms []string `yaml:"platforms"` // a project setting: read from project.yaml only
	// Documentation is on or off: whether the pipeline's documentation stage runs. A later layer wins.
	Documentation string `yaml:"documentation"`
}

// HandlerSpec is one handler as written: exactly one of require, observe or augment.
type HandlerSpec struct {
	Require string `yaml:"require"`
	Observe string `yaml:"observe"`
	Augment string `yaml:"augment"`
	Status  string `yaml:"status"` // for a generated augment, e.g. only active rules
}

// Handler is a resolved handler, frozen into a run's snapshot.
type Handler struct {
	Kind    string `json:"kind"` // require, observe, augment
	Command string `json:"command,omitempty"`
	Text    string `json:"text,omitempty"`   // an augment's content
	Source  string `json:"source"`           // where it was declared
	Dir     string `json:"dir"`              // the directory relative paths resolve against
	Status  string `json:"status,omitempty"` // passed to generated augments
}

// Extension is an extension's extension.yaml.
type Extension struct {
	Name             string      `yaml:"name"`
	Summary          string      `yaml:"summary"`
	RequiresBindings []string    `yaml:"requires-bindings"`
	Help             []HelpEntry `yaml:"help"`
	Quiet            bool        `yaml:"quiet"`   // leaves no trace in a bridge whose own use: does not name it
	Publish          string      `yaml:"publish"` // the command that publishes a task's tested work; the task is its argument
	Layer            `yaml:",inline"`
	dir              string
}

type BridgeFile struct {
	Layer        `yaml:",inline"`
	ControlPlane struct {
		Path   string `yaml:"path"`   // a local control plane, used directly (development)
		Remote string `yaml:"remote"` // otherwise: cloned into .starship/control-plane …
		Tag    string `yaml:"tag"`    // … at a release tag,
		Ref    string `yaml:"ref"`    // … or any ref, for testing unreleased changes
	} `yaml:"control-plane"`
	// Roles adjusts generated role files for this bridge, e.g. a cheaper model.
	Roles map[string]struct {
		Model string `yaml:"model"`
	} `yaml:"roles"`
	// SessionEdit lists further folders the session agent may edit; relative entries are taken from the bridge folder. Code folders don't belong here.
	SessionEdit []string `yaml:"session-edit"`
	// KeepRuns is how many finished runs the bridge keeps in all; unset means defaultKeepRuns.
	KeepRuns *int `yaml:"keep-runs"`
	// Connectors maps a role to the exact connector (MCP) tools it may call. Declaring any role turns enforcement on for the bridge.
	Connectors map[string][]string `yaml:"connectors"`
	// ConnectorWrites lists the exact connector tools the session may call once the user has approved the call with go. Its presence, even as an empty list, turns approved connector writes on; connectorWrites reads it.
	ConnectorWrites yaml.Node `yaml:"connector_writes"`
	// Sources and Documents are read from bridge.yaml by loadDeclaredSources and loadDeclaredDocuments; they are here so strict decoding accepts them.
	Sources   yaml.Node `yaml:"sources"`
	Documents yaml.Node `yaml:"documents"`
}

func loadBridgeFile(bridge string) (*BridgeFile, error) {
	var bf BridgeFile
	path := filepath.Join(bridge, "bridge.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// An unknown key is refused so a misspelled declaration can't quietly do nothing. An empty file decodes to io.EOF.
	err = guardedDecode(func() error {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		return dec.Decode(&bf)
	})
	if err = correctParserLine(err, data); err != nil && err != io.EOF {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return &bf, nil
}

// bridgeUses reports whether the bridge's own bridge.yaml names the extension in use. A bridge with no bridge.yaml names nothing; one that cannot be loaded is the error.
func bridgeUses(bridge, name string) (bool, error) {
	bf, err := loadBridgeFile(bridge)
	if reallyMissing(filepath.Join(bridge, "bridge.yaml"), err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(bf.Use, name), nil
}

// controlPlaneDir is where this bridge reads the control plane from.
func controlPlaneDir(bridge string, bf *BridgeFile) (string, error) {
	if p := bf.ControlPlane.Path; p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(bridge, p)
		}
		return p, nil
	}
	if bf.ControlPlane.Remote == "" {
		return "", fmt.Errorf("bridge.yaml: control-plane needs a path, or a remote with a tag or ref")
	}
	dir := filepath.Join(bridge, ".starship", "control-plane")
	if _, err := os.Stat(filepath.Join(dir, "pipelines")); err != nil {
		return "", fmt.Errorf("this bridge is not set up on this machine: %s is missing (run setup)", dir)
	}
	return dir, nil
}

type TaskVcs struct {
	Kind   string `yaml:"kind"`
	Branch string `yaml:"branch"`
	Client string `yaml:"client"`
}

type TaskFile struct {
	Layer    `yaml:",inline"`
	Pipeline string  `yaml:"pipeline"`
	Vcs      TaskVcs `yaml:"vcs"`
	// Locations are the project's code locations the task uses; each run works in one.
	Locations []string `yaml:"locations"`
}

// Config is everything a run is built from, after merging the layers.
type Config struct {
	Bridge        string
	ControlPlane  string
	Project       string
	Task          string // "<project>/<task>"
	TaskDir       string
	Workspace     string
	Location      string     // the project location this run works in, when the project declares locations
	Locations     *Locations // the project's declared locations
	WorkItem      string     // task.md body
	Subtask       string     // the subtask this run works on, when the task has subtasks
	Pipeline      *Pipeline
	Bindings      map[string]Binding
	Rules         []Rule // the project's standing run rules, first declaration kept
	locationRules map[string]yaml.Node
	Events        map[string][]Handler
	Extensions    []string
	Platforms     []string // the platforms the project declares, from project.yaml
	JournalOff    string   // reason, when the project turned journal commits off
	Documentation string   // "off" when a layer turned the documentation stage off; empty means on
	Skills        []SkillDelivery
	needs         map[string]string // binding -> what requires it, from extensions
	problems      []string
	// Sides are the locations a run tests, the edited one first; Narrowed says they are fewer than the default. Cross holds the cross-location bindings when there are several sides.
	Sides        []Side
	Narrowed     bool
	Cross        map[string]Binding
	Dependencies []Dependency // other locations' unlanded branches the run applies, see resolveDependencies
}

// Side is one location a run tests, with what its test stage needs frozen: its folder on this machine and its merged bindings.
type Side struct {
	Location  string             `json:"location"`
	Workspace string             `json:"workspace"`
	Bindings  map[string]Binding `json:"bindings"`
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yamlUnmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// addBindings sets the bindings a layer or extension declares, replacing earlier ones. An alias is followed to its mapping and applied like a plain one, and merge keys are expanded; an absent or null value declares none; any other non-mapping, a bad merge value or a cycle is refused, naming file and line, as the drift report does. Both paths read the names through bindingEntries.
func (c *Config) addBindings(n *yaml.Node, label, file string) error {
	entries, err := bindingEntries(n)
	if err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	for _, e := range entries {
		b, err := parseBinding(e.val, fmt.Sprintf("%s:%d", label, e.key.Line))
		if err != nil {
			return err
		}
		c.Bindings[e.name] = b
	}
	return nil
}

// addRules adds the rules a layer or extension declares, dropping any already present. An alias is followed for the whole value and for each item; a rule's source is the line of its item as written.
func (c *Config) addRules(n *yaml.Node, label string) error {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == 0 || n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s:%d: rules must be a list of strings", label, n.Line)
	}
	for _, written := range n.Content {
		item := written
		if item.Kind == yaml.AliasNode {
			item = item.Alias
		}
		if item.Tag == "!!null" {
			continue
		}
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("%s:%d: a rule must be a string", label, written.Line)
		}
		text := strings.TrimSpace(item.Value)
		if text == "" || slices.ContainsFunc(c.Rules, func(r Rule) bool { return r.Text == text }) {
			continue
		}
		c.Rules = append(c.Rules, Rule{Text: text, Source: fmt.Sprintf("%s:%d", label, written.Line)})
	}
	return nil
}

// decodeFrontmatter decodes the front returned by splitFrontmatter; the leading newline stands in for the dropped opening --- line, so node lines are file lines. The line correction is applied once, by yamlUnmarshal, to the prefixed text.
func decodeFrontmatter(front []byte, out any) error {
	return yamlUnmarshal(append([]byte("\n"), front...), out)
}

// guardedDecode runs a decode and turns a panic into an error. yaml.v3 re-panics some malformed mappings (an unhashable key beside a merge key) rather than returning an error.
func guardedDecode(decode func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed mapping: %v", r)
		}
	}()
	return decode()
}

// decodeNode decodes a stored raw node into out, turning a yaml.v3 panic into an error as guardedDecode does.
func decodeNode(n *yaml.Node, out any) error {
	return guardedDecode(func() error { return n.Decode(out) })
}

// correctParserLine rewrites a yaml error to name the real line; other errors pass unchanged. data is the source that was decoded. yaml.v3 names the context mark's line when there is one, otherwise the problem mark's, and the text doesn't say which, so after the parser-class correction the line is searched for: the first prefix of the source, from that line on, that fails with the same problem text ends at the true line.
func correctParserLine(err error, data []byte) error {
	if err == nil {
		return nil
	}
	m := yamlLineError.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	was, _ := strconv.Atoi(m[1])
	n := was
	if yamlParserProblems[m[2]] {
		n++
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	end := 0
	for i, l := range lines {
		end += len(l)
		if i+1 < n {
			continue
		}
		if yamlProblemOf(data[:end]) == m[2] {
			n = i + 1
			break
		}
	}
	if n == was {
		return err
	}
	return errors.New("yaml: line " + strconv.Itoa(n) + ": " + m[2])
}

// yamlProblemOf is the problem text of the error from parsing data into nodes, without its position prefix; empty when it parses.
func yamlProblemOf(data []byte) string {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if err == io.EOF {
			return ""
		}
		if err != nil {
			if m := yamlLineError.FindStringSubmatch(err.Error()); m != nil {
				return m[2]
			}
			return strings.TrimPrefix(err.Error(), "yaml: ")
		}
	}
}

// yamlLineError and yamlParserProblems exist because yaml.v3's parser.fail() formats parser errors from 0-based marks and adds 1 only for scanner errors, so parser errors name the line before the real one. The error carries no kind, only text, so the line is corrected by matching the whole problem string; scanner messages can also begin "did not find expected" and must not match.
var yamlLineError = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

var yamlParserProblems = map[string]bool{
	"did not find expected <stream-start>":   true,
	"did not find expected <document start>": true,
	"did not find expected node content":     true,
	"did not find expected '-' indicator":    true,
	"did not find expected key":              true,
	"did not find expected ',' or ']'":       true,
	"did not find expected ',' or '}'":       true,
	"found undefined tag handle":             true,
	"found duplicate %YAML directive":        true,
	"found duplicate %TAG directive":         true,
	"found incompatible YAML document":       true,
}

func splitFrontmatter(path string) (front, body []byte, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return splitFrontmatterData(path, data)
}

func splitFrontmatterData(path string, data []byte) (front, body []byte, err error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, fmt.Errorf("%s: no YAML frontmatter", path)
	}
	rest := data[4:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, nil, fmt.Errorf("%s: frontmatter is not closed with ---", path)
	}
	body = rest[end+4:]
	body = bytes.TrimLeft(body, "-\n")
	return rest[:end], body, nil
}

// findBridge walks up from dir to the directory holding bridge.yaml.
func findBridge(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "bridge.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a bridge (no bridge.yaml found)")
		}
		dir = parent
	}
}

// Target is what a run is for: a task, or — for pipelines that work at that level — a
// whole project or the whole bridge.
type Target struct {
	Task     string // <project>/<task>
	Project  string
	Pipeline string
	Location string // the project location a run works in
	// Sides asks for the run's affected sides (see loadSides); WithSides must be set for them to be worked out.
	Sides     []string
	WithSides bool
}

type layerEntry struct {
	label string
	file  string
	layer Layer
}

// loadConfig merges control plane → bridge → project → task for one task.
func loadConfig(bridge, task string) (*Config, error) {
	return loadConfigAt(bridge, task, "")
}

// loadConfigAt is loadConfig for the commands that land a task's work, with the location they were asked to act in.
func loadConfigAt(bridge, task, location string) (*Config, error) {
	loc, err := runLocation(bridge, task, location)
	if err != nil {
		return nil, err
	}
	return loadTarget(bridge, Target{Task: task, Location: loc})
}

// projectPipeline reads the project's pipeline: setting; none is the only value it may have. A file that can't be read has no setting; the callers report that themselves.
func projectPipeline(projectFile string) (string, error) {
	var pp struct {
		Pipeline string `yaml:"pipeline"`
	}
	if readYAML(projectFile, &pp) != nil {
		return "", nil
	}
	if err := checkProjectKeys(projectFile); err != nil {
		return "", err
	}
	if pp.Pipeline != "" && pp.Pipeline != "none" {
		return "", fmt.Errorf("%s: pipeline: %q is not allowed; the only value a project can set is none (tasks without code)", projectFile, pp.Pipeline)
	}
	return pp.Pipeline, nil
}

// taskHasNoCode reports whether a task is one without code (pipeline: none). The task's own pipeline: wins, then the project's. A task file that can't be read or parsed gives false, so the usual errors still come from the callers.
func taskHasNoCode(bridge, project, task string) (bool, error) {
	var tf struct {
		Pipeline string `yaml:"pipeline"`
	}
	front, _, err := splitFrontmatter(filepath.Join(bridge, "projects", project, "tasks", task, "task.md"))
	if err != nil || decodeFrontmatter(front, &tf) != nil {
		return false, nil
	}
	if err := checkTaskKeys(filepath.Join(bridge, "projects", project, "tasks", task, "task.md"), front); err != nil {
		return false, err
	}
	if tf.Pipeline != "" {
		return tf.Pipeline == "none", nil
	}
	pipeline, err := projectPipeline(filepath.Join(bridge, "projects", project, "project.yaml"))
	return pipeline == "none", err
}

// refuseNoCode fails for a task <project>/<task> that has no code, so nothing runs, lands or publishes for it.
func refuseNoCode(bridge, ref string) error {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	none, err := taskHasNoCode(bridge, parts[0], parts[1])
	if err != nil {
		return err
	}
	if none {
		return fmt.Errorf("task %s has no pipeline, so it can't run: it is a task without code (pipeline: none)", ref)
	}
	return nil
}

// loadTarget merges the layers that apply to a target.
func loadTarget(bridge string, t Target) (*Config, error) {
	c := &Config{Bridge: bridge, Project: t.Project, Task: t.Task, Bindings: map[string]Binding{},
		Events: map[string][]Handler{}, needs: map[string]string{}}

	bl, err := c.bridgeLayer(bridge)
	if err != nil {
		return nil, err
	}
	layers := []layerEntry{bl}
	name := t.Pipeline

	loadProject := func() error {
		pl, err := c.projectLayer(bridge)
		if err != nil {
			return err
		}
		layers = append(layers, pl)
		return nil
	}

	if t.Task != "" {
		parts := strings.Split(t.Task, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("task must be <project>/<task>, got %q", t.Task)
		}
		c.Project = parts[0]
		if err := refuseNoCode(bridge, t.Task); err != nil {
			return nil, err
		}
		if err := loadProject(); err != nil {
			return nil, err
		}
		c.TaskDir = filepath.Join(bridge, "projects", c.Project, "tasks", parts[1])
		taskFile := filepath.Join(c.TaskDir, "task.md")
		front, body, err := splitFrontmatter(taskFile)
		if err != nil {
			return nil, fmt.Errorf("task %q: %v", t.Task, err)
		}
		var tf TaskFile
		if err := decodeFrontmatter(front, &tf); err != nil {
			return nil, fmt.Errorf("%s: %v", taskFile, err)
		}
		if hasKey(front, "platforms") {
			return nil, fmt.Errorf("%s: platforms: is a project setting; declare it in projects/%s/project.yaml", taskFile, c.Project)
		}
		if key := connectorKey(front); key != "" {
			return nil, fmt.Errorf("%s: %s: is a bridge setting; declare it in bridge.yaml", taskFile, key)
		}
		if err := checkTaskKeys(taskFile, front); err != nil {
			return nil, err
		}
		if c.Location, err = taskLocation(c.Locations, c.Project, t.Task, tf.Locations, t.Location); err != nil {
			return nil, err
		}
		if c.Location != "" {
			if c.Workspace, err = locationPathFor(bridge, c.Project, c.Location); err != nil {
				return nil, err
			}
			layers = append(layers, c.locationLayer(bridge))
		} else if c.Workspace, err = workspaceFor(bridge, c.Project, parts[1]); err != nil {
			return nil, err
		}
		c.WorkItem = workItem(string(body))
		if name == "" {
			name = tf.Pipeline
		}
		if name == "" {
			name = "coding"
		}
		layers = append(layers, layerEntry{"task:" + relTo(bridge, taskFile), taskFile, tf.Layer})
	} else {
		if name == "" {
			return nil, fmt.Errorf("a run needs a task, or a pipeline (with --project for a project-wide one)")
		}
		if c.Project != "" {
			if err := loadProject(); err != nil {
				return nil, err
			}
			if t.Location != "" {
				switch {
				case len(c.Locations.Names()) == 0:
					return nil, fmt.Errorf("project %s declares no locations, so --location %s has no meaning", c.Project, t.Location)
				case !c.Locations.Has(t.Location):
					return nil, fmt.Errorf("project %s declares no location %q (it declares %s)", c.Project, t.Location, strings.Join(c.Locations.Names(), ", "))
				}
				c.Location = t.Location
				if c.Workspace, err = locationPathFor(bridge, c.Project, c.Location); err != nil {
					return nil, err
				}
				layers = append(layers, c.locationLayer(bridge))
			} else if ws, err := workspaceFor(bridge, c.Project, ""); err == nil {
				c.Workspace = ws
			}
		}
	}

	base, baseLabel, ferr := findPipeline(bridge, c.ControlPlane, name)
	if base == "" {
		if ferr != nil {
			return nil, fmt.Errorf("no pipeline %q (extensions could not be listed: %v)", name, ferr)
		}
		return nil, fmt.Errorf("no pipeline %q", name)
	}
	p := &Pipeline{}
	if err := readYAML(base, p); err != nil {
		return nil, fmt.Errorf("pipeline %q: %v", name, err)
	}
	p.Sources = map[string]string{}
	if err := p.canonicalizeBudgets(); err != nil {
		return nil, fmt.Errorf("pipeline %q: %v", name, err)
	}
	rel := baseLabel
	setSources(p.Stages, rel)
	switch scope := p.scope(); {
	case t.Task != "" && scope != "task":
		return nil, fmt.Errorf("pipeline %s runs for a whole %s, not a task", name, scope)
	case t.Task == "" && scope == "task":
		return nil, fmt.Errorf("pipeline %s runs for a task: ensign run <project>/<task>", name)
	case t.Task == "" && scope == "project" && c.Project == "":
		return nil, fmt.Errorf("pipeline %s runs for a project: add --project <name>", name)
	case t.Task == "" && scope == "bridge" && c.Project != "":
		return nil, fmt.Errorf("pipeline %s runs for the whole bridge; drop --project", name)
	}
	if t.Task == "" {
		c.WorkItem = p.Summary
	}
	for k := range p.Budgets {
		p.Sources["budgets."+k] = rel
	}
	for k := range p.OnExhausted {
		p.Sources["on-exhausted."+k] = rel
	}
	c.Pipeline = p

	extensions, broken := scanExtensions(bridge, c.ControlPlane)
	for _, l := range layers {
		switch l.layer.Documentation {
		case "":
		case "on":
			c.Documentation = ""
		case "off":
			c.Documentation = "off"
		default:
			return nil, fmt.Errorf("%s: documentation must be on or off, not %q", l.label, l.layer.Documentation)
		}
		for _, use := range l.layer.Use {
			b, err := c.enableExtension(l, use, extensions, broken)
			if err != nil {
				return nil, err
			}
			if b == nil {
				continue
			}
			label := "extension:" + use
			if po, ok := b.Pipelines[name]; ok {
				for i := range po.Ops {
					if err := applyOp(p, &po.Ops[i], label); err != nil {
						return nil, err
					}
				}
			}
			if err := c.mergeExtension(b, label); err != nil {
				return nil, err
			}
			if err := c.addRules(&b.Rules, label); err != nil {
				return nil, err
			}
		}
		c.addEvents(l.layer.Events, l.label, filepath.Dir(l.file))
		if po, ok := l.layer.Pipelines[name]; ok {
			for i := range po.Ops {
				if err := applyOp(p, &po.Ops[i], l.label); err != nil {
					return nil, err
				}
			}
		}
		if err := c.addBindings(&l.layer.Bindings, l.label, l.file); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(l.label, "task:") {
			if err := c.addRules(&l.layer.Rules, l.label); err != nil {
				return nil, err
			}
		}
	}
	if err := c.selectSkills(layers); err != nil {
		return nil, err
	}
	if t.WithSides {
		if err := c.loadSides(bridge, layers, extensions, t.Sides); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// bridgeLayer loads bridge.yaml and the control plane it names, and returns the bridge's layer.
func (c *Config) bridgeLayer(bridge string) (layerEntry, error) {
	bridgeFile := filepath.Join(bridge, "bridge.yaml")
	bfp, err := loadBridgeFile(bridge)
	if err != nil {
		return layerEntry{}, err
	}
	if data, rerr := os.ReadFile(bridgeFile); rerr == nil && hasKey(data, "platforms") {
		return layerEntry{}, fmt.Errorf("%s: platforms: is a project setting; declare it in projects/<project>/project.yaml", bridgeFile)
	}
	if c.ControlPlane, err = controlPlaneDir(bridge, bfp); err != nil {
		return layerEntry{}, err
	}
	return layerEntry{"bridge:" + relTo(bridge, bridgeFile), bridgeFile, bfp.Layer}, nil
}

// projectLayer loads c.Project's project.yaml, sets the project's locations, platforms and journal setting, and returns the project's layer.
func (c *Config) projectLayer(bridge string) (layerEntry, error) {
	var pf Layer
	projectFile := filepath.Join(bridge, "projects", c.Project, "project.yaml")
	if err := readProjectYAML(projectFile, &pf); err != nil {
		return layerEntry{}, fmt.Errorf("project %q: %v", c.Project, err)
	}
	if data, rerr := os.ReadFile(projectFile); rerr == nil {
		if key := connectorKey(data); key != "" {
			return layerEntry{}, fmt.Errorf("%s: %s: is a bridge setting; declare it in bridge.yaml", projectFile, key)
		}
	}
	var err error
	if c.Locations, err = loadLocations(bridge, c.Project); err != nil {
		return layerEntry{}, err
	}
	var lr struct {
		Locations map[string]struct {
			Rules yaml.Node `yaml:"rules"`
		} `yaml:"locations"`
	}
	if err := readYAML(projectFile, &lr); err != nil {
		return layerEntry{}, fmt.Errorf("project %q: %v", c.Project, err)
	}
	c.locationRules = map[string]yaml.Node{}
	for name, loc := range lr.Locations {
		c.locationRules[name] = loc.Rules
	}
	if _, err := projectPipeline(projectFile); err != nil {
		return layerEntry{}, fmt.Errorf("project %q: %v", c.Project, err)
	}
	c.Platforms = pf.Platforms
	if pf.Journal.Commit == "off" {
		c.JournalOff = pf.Journal.Reason
	}
	return layerEntry{"project:" + relTo(bridge, projectFile), projectFile, pf}, nil
}

// enableExtension records that layer l uses an extension. It returns nil if an earlier layer already enabled it.
func (c *Config) enableExtension(l layerEntry, use string, extensions map[string]*Extension, broken map[string]error) (*Extension, error) {
	if contains(c.Extensions, use) {
		return nil, nil
	}
	var file string
	b, ok := extensions[use]
	if berr, isBroken := broken[use]; isBroken {
		file = berr.(*brokenExtension).file
		if data, rerr := os.ReadFile(file); rerr != nil || (!hasKey(data, "platforms") && connectorKey(data) == "") {
			return nil, fmt.Errorf("%s: extension %q: %v", l.label, use, berr)
		}
	} else if !ok {
		return nil, fmt.Errorf("%s: no extension %q", l.label, use)
	} else {
		file = filepath.Join(b.dir, "extension.yaml")
	}
	if data, rerr := os.ReadFile(file); rerr == nil && hasKey(data, "platforms") {
		project := c.Project
		if project == "" {
			project = "<project>"
		}
		return nil, fmt.Errorf("%s: platforms: is a project setting; declare it in projects/%s/project.yaml", file, project)
	}
	if data, rerr := os.ReadFile(file); rerr == nil {
		if key := connectorKey(data); key != "" {
			return nil, fmt.Errorf("%s: %s: is a bridge setting; declare it in bridge.yaml", file, key)
		}
	}
	c.Extensions = append(c.Extensions, use)
	return b, nil
}

// mergeExtension adds an extension's events, needs and bindings. Its bindings are defaults: the layer that uses it, and any later layer, can replace them.
func (c *Config) mergeExtension(b *Extension, label string) error {
	c.addEvents(b.Events, label, b.dir)
	for _, req := range b.RequiresBindings {
		c.needs[req] = label
	}
	return c.addBindings(&b.Bindings, label, filepath.Join(b.dir, "extension.yaml"))
}

// loadProjectBindings merges the bindings of the layers that apply to a project, with no task or pipeline: bridge, project, then the location's layer when location is not empty. Extensions merge as in loadTarget.
func loadProjectBindings(bridge, project, location string) (*Config, error) {
	c := &Config{Bridge: bridge, Project: project, Bindings: map[string]Binding{},
		Events: map[string][]Handler{}, needs: map[string]string{}}
	bl, err := c.bridgeLayer(bridge)
	if err != nil {
		return nil, err
	}
	pl, err := c.projectLayer(bridge)
	if err != nil {
		return nil, err
	}
	layers := []layerEntry{bl, pl}
	if location != "" {
		c.Location = location
		layers = append(layers, c.locationLayer(bridge))
	}
	if err := c.mergeLayers(layers); err != nil {
		return nil, err
	}
	return c, nil
}

// loadTaskEvents merges the layers that name a task's events and bindings with no pipeline, workspace or location: bridge, project, then the task. Extensions merge as in loadTarget.
func loadTaskEvents(bridge, task string) (*Config, error) {
	project, name, _ := strings.Cut(task, "/")
	c := &Config{Bridge: bridge, Project: project, Task: task, Bindings: map[string]Binding{},
		Events: map[string][]Handler{}, needs: map[string]string{}}
	bl, err := c.bridgeLayer(bridge)
	if err != nil {
		return nil, err
	}
	pl, err := c.projectLayer(bridge)
	if err != nil {
		return nil, err
	}
	taskFile := filepath.Join(bridge, "projects", project, "tasks", name, "task.md")
	front, _, err := splitFrontmatter(taskFile)
	if err != nil {
		return nil, fmt.Errorf("task %q: %v", task, err)
	}
	var tf TaskFile
	if err := decodeFrontmatter(front, &tf); err != nil {
		return nil, fmt.Errorf("%s: %v", taskFile, err)
	}
	if err := checkTaskKeys(taskFile, front); err != nil {
		return nil, err
	}
	if err := c.mergeLayers([]layerEntry{bl, pl, {"task:" + relTo(bridge, taskFile), taskFile, tf.Layer}}); err != nil {
		return nil, err
	}
	return c, nil
}

// mergeLayers adds each layer's extensions, events and bindings in order, an extension's before the layer that uses it.
func (c *Config) mergeLayers(layers []layerEntry) error {
	extensions, broken := scanExtensions(c.Bridge, c.ControlPlane)
	for _, l := range layers {
		for _, use := range l.layer.Use {
			b, err := c.enableExtension(l, use, extensions, broken)
			if err != nil {
				return err
			}
			if b == nil {
				continue
			}
			if err := c.mergeExtension(b, "extension:"+use); err != nil {
				return err
			}
		}
		c.addEvents(l.layer.Events, l.label, filepath.Dir(l.file))
		if err := c.addBindings(&l.layer.Bindings, l.label, l.file); err != nil {
			return err
		}
	}
	return nil
}

// loadSides works out the run's affected sides: the edited location and every location that depends on it, or the narrower set asked for in want. Each side's bindings merge the layers the way the edited location's do, with that location's layer in place of the edited one's.
func (c *Config) loadSides(bridge string, layers []layerEntry, extensions map[string]*Extension, want []string) error {
	if c.Location == "" {
		if len(want) > 0 {
			return fmt.Errorf("--sides has no meaning for a run without a location: the project declares none, or the run names none")
		}
		return nil
	}
	def := append([]string{c.Location}, c.Locations.Dependents(c.Location)...)
	sides := def
	if len(want) > 0 {
		for _, n := range want {
			if !c.Locations.Has(n) {
				return fmt.Errorf("--sides: project %s declares no location %q (it declares %s)", c.Project, n, strings.Join(c.Locations.Names(), ", "))
			}
		}
		if !contains(want, c.Location) {
			return fmt.Errorf("--sides must include %s, the location this run edits", c.Location)
		}
		for _, n := range want {
			if !contains(def, n) {
				return fmt.Errorf("--sides: %s is not affected by a change to %s (affected: %s), so a run does not test it", n, c.Location, strings.Join(def, ", "))
			}
		}
		sides = []string{c.Location}
		for _, n := range def[1:] {
			if contains(want, n) {
				sides = append(sides, n)
			}
		}
	}
	c.Narrowed = len(sides) < len(def)
	for _, n := range sides {
		side := Side{Location: n, Workspace: c.Workspace, Bindings: c.Bindings}
		if n != c.Location {
			ws, err := locationPathFor(bridge, c.Project, n)
			if err != nil {
				return err
			}
			side.Workspace = ws
			if side.Bindings, err = c.sideBindings(bridge, layers, extensions, n); err != nil {
				return err
			}
		}
		c.Sides = append(c.Sides, side)
	}
	if len(c.Sides) > 1 {
		c.Cross = c.Locations.Cross
	}
	return nil
}

// sideBindings merges the bindings of the layers, extensions' defaults included, as loadTarget does, using location's layer where the edited location's sits. The task's layer is left out: its overrides are for the edited location, so another side goes bridge, project, then its location. A use: naming no extension is an error: a guard, as loadTarget's mergeLayers normally finds it first.
func (c *Config) sideBindings(bridge string, layers []layerEntry, extensions map[string]*Extension, location string) (map[string]Binding, error) {
	side := &Config{Bindings: map[string]Binding{}}
	used := map[string]bool{}
	for _, l := range layers {
		if strings.HasPrefix(l.label, "task:") {
			continue
		}
		if strings.HasPrefix(l.label, "location:") {
			l = c.locationLayerFor(bridge, location)
		}
		for _, use := range l.layer.Use {
			if used[use] {
				continue
			}
			used[use] = true
			b, ok := extensions[use]
			if !ok {
				return nil, fmt.Errorf("%s: no extension %q", l.label, use)
			}
			if err := side.addBindings(&b.Bindings, "extension:"+use, filepath.Join(b.dir, "extension.yaml")); err != nil {
				return nil, err
			}
		}
		if err := side.addBindings(&l.layer.Bindings, l.label, l.file); err != nil {
			return nil, err
		}
	}
	return side.Bindings, nil
}

// locationLayer is the layer a location's bindings make, between the project's and the task's.
func (c *Config) locationLayer(bridge string) layerEntry {
	return c.locationLayerFor(bridge, c.Location)
}

func (c *Config) locationLayerFor(bridge, location string) layerEntry {
	file := filepath.Join(bridge, "projects", c.Project, "project.yaml")
	return layerEntry{"location:" + relTo(bridge, file) + "#" + location, file, Layer{Bindings: c.Locations.bindings[location], Rules: c.locationRules[location]}}
}

// findPipeline locates a pipeline file: the control plane's, then any extension's.
// The error is what kept the extensions from being listed, which a pipeline not found may be down to.
func findPipeline(bridge, cp, name string) (path, label string, err error) {
	f := filepath.Join(cp, "pipelines", name+".yaml")
	if _, err := os.Stat(f); err == nil {
		return f, "control-plane:pipelines/" + name + ".yaml", nil
	}
	dirs, err := extensionDirs(bridge, cp)
	for _, dir := range dirs {
		f := filepath.Join(dir, "pipelines", name+".yaml")
		if _, err := os.Stat(f); err == nil {
			return f, "extension:" + filepath.Base(dir) + "/pipelines/" + name + ".yaml", nil
		}
	}
	return "", "", err
}

func (p *Pipeline) scope() string {
	if p.Scope == "" {
		return "task"
	}
	return p.Scope
}

func relTo(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil {
		return filepath.ToSlash(r)
	}
	return path
}

func setSources(stages []*Stage, file string) {
	for _, s := range stages {
		s.Source = fmt.Sprintf("%s:%d", file, s.line)
		setSources(s.Stages, file)
	}
}

// applyOp applies one pipeline operation from a lower layer.
func applyOp(p *Pipeline, n *yaml.Node, label string) error {
	where := fmt.Sprintf("%s:%d", label, n.Line)
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 {
		return fmt.Errorf("%s: an op is a mapping with exactly one key (insert, remove, replace, set, append)", where)
	}
	kind, arg := n.Content[0].Value, n.Content[1]
	switch kind {
	case "insert":
		var a struct {
			After  string `yaml:"after"`
			Before string `yaml:"before"`
			Stage  *Stage `yaml:"stage"`
		}
		if err := decodeNode(arg, &a); err != nil {
			return fmt.Errorf("%s: insert: %v", where, err)
		}
		if a.Stage == nil || (a.After == "") == (a.Before == "") {
			return fmt.Errorf("%s: insert needs a stage and exactly one of after/before", where)
		}
		anchor := a.After + a.Before
		_, list, _ := p.find(anchor)
		if list == nil {
			return fmt.Errorf("%s: insert: no stage %q", where, anchor)
		}
		a.Stage.Source = "inserted by " + where
		setSources(a.Stage.Stages, where)
		for i, s := range *list {
			if s.ID == anchor {
				at := i
				if a.After != "" {
					at = i + 1
				}
				*list = append((*list)[:at], append([]*Stage{a.Stage}, (*list)[at:]...)...)
				return nil
			}
		}
	case "remove":
		id := arg.Value
		_, list, _ := p.find(id)
		if list == nil {
			return fmt.Errorf("%s: remove: no stage %q", where, id)
		}
		for i, s := range *list {
			if s.ID == id {
				*list = append((*list)[:i], (*list)[i+1:]...)
				p.Sources["removed."+id] = where
				return nil
			}
		}
	case "replace":
		var a struct {
			Stage string `yaml:"stage"`
			With  *Stage `yaml:"with"`
		}
		if err := decodeNode(arg, &a); err != nil || a.With == nil {
			return fmt.Errorf("%s: replace needs stage and with", where)
		}
		_, list, _ := p.find(a.Stage)
		if list == nil {
			return fmt.Errorf("%s: replace: no stage %q", where, a.Stage)
		}
		for i, s := range *list {
			if s.ID == a.Stage {
				a.With.Source = "replaced by " + where
				(*list)[i] = a.With
				return nil
			}
		}
	case "set", "append":
		return applySet(p, arg, kind, where)
	default:
		return fmt.Errorf("%s: unknown op %q", where, kind)
	}
	return nil
}

func applySet(p *Pipeline, arg *yaml.Node, kind, where string) error {
	if arg.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: %s takes a mapping", where, kind)
	}
	var stage *Stage
	for i := 0; i < len(arg.Content); i += 2 {
		if arg.Content[i].Value == "stage" {
			id := arg.Content[i+1].Value
			if stage, _, _ = p.find(id); stage == nil {
				return fmt.Errorf("%s: %s: no stage %q", where, kind, id)
			}
		}
	}
	for i := 0; i < len(arg.Content); i += 2 {
		key, val := arg.Content[i].Value, arg.Content[i+1]
		if key == "stage" {
			continue
		}
		var err error
		if stage != nil {
			err = setStageField(stage, key, val, kind)
			if err == nil {
				stage.Source += "; " + key + " " + kind + " by " + where
			}
		} else {
			err = setPipelineField(p, key, val, kind)
			if err == nil {
				if name, ok := strings.CutPrefix(key, "budgets."); ok {
					key = "budgets." + canonicalBudget(name)
				} else if name, ok := strings.CutPrefix(key, "on-exhausted."); ok {
					key = "on-exhausted." + canonicalBudget(name)
				}
				p.Sources[key] = where
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %s %s: %v", where, kind, key, err)
		}
	}
	return nil
}

func setStageField(s *Stage, key string, val *yaml.Node, kind string) error {
	if kind == "append" {
		if key != "dimensions" {
			return fmt.Errorf("only dimensions can be appended to on a stage")
		}
		var more []string
		if err := val.Decode(&more); err != nil {
			return err
		}
		s.Dimensions = append(s.Dimensions, more...)
		return nil
	}
	if name, ok := strings.CutPrefix(key, "outcomes."); ok {
		var t Transition
		if err := decodeNode(val, &t); err != nil {
			return err
		}
		for i := range s.Outcomes {
			if s.Outcomes[i].Name == name {
				s.Outcomes[i].Then = t
				return nil
			}
		}
		s.Outcomes = append(s.Outcomes, Outcome{Name: name, Then: t})
		return nil
	}
	switch key {
	case "outcomes":
		var o Outcomes
		if err := decodeNode(val, &o); err != nil {
			return err
		}
		s.Outcomes = o
	case "role":
		s.Role = val.Value
	case "brief":
		s.Brief = val.Value
	case "spend":
		s.Spend = val.Value
	case "dimensions":
		s.Dimensions = nil
		return val.Decode(&s.Dimensions)
	case "report-fields":
		s.ReportFields = nil
		return val.Decode(&s.ReportFields)
	case "parallel":
		s.Parallel = &Parallel{}
		return decodeNode(val, s.Parallel)
	default:
		return fmt.Errorf("unknown stage field")
	}
	return nil
}

func setPipelineField(p *Pipeline, key string, val *yaml.Node, kind string) error {
	if kind == "append" {
		if key != "free-roles" {
			return fmt.Errorf("only free-roles can be appended to on a pipeline")
		}
		var more []string
		if err := val.Decode(&more); err != nil {
			return err
		}
		p.FreeRoles = append(p.FreeRoles, more...)
		return nil
	}
	if name, ok := strings.CutPrefix(key, "budgets."); ok {
		name = canonicalBudget(name)
		var v int
		if err := val.Decode(&v); err != nil {
			return err
		}
		if p.Budgets == nil {
			p.Budgets = map[string]int{}
		}
		p.Budgets[name] = v
		return nil
	}
	if name, ok := strings.CutPrefix(key, "on-exhausted."); ok {
		name = canonicalBudget(name)
		var t Transition
		if err := decodeNode(val, &t); err != nil {
			return err
		}
		t = t.canonicalBudgets()
		if p.OnExhausted == nil {
			p.OnExhausted = map[string]Transition{}
		}
		p.OnExhausted[name] = t
		return nil
	}
	if key == "free-roles" {
		p.FreeRoles = nil
		return val.Decode(&p.FreeRoles)
	}
	return fmt.Errorf("unknown pipeline field")
}

// hasKey reports whether a YAML document has a top-level key, whatever its value (a null or empty value leaves no trace in the decoded struct). A document whose root is not a mapping has no keys, and neither does one where the decoder would reject a merge (a merge value that is not a mapping or a list of mappings) before the key is reached, so the caller reports the decode error instead. A key declared before a bad merge still counts.
func hasKey(data []byte, key string) bool {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return false
	}
	found, _ := mappingHasKey(doc.Content[0], key, map[*yaml.Node]bool{})
	return found
}

// mappingHasKey walks a mapping's scalar keys (following alias keys), and the mappings a merge key (<<) pulls in, without decoding any values, mirroring yaml.v3's decoder: a merge is a key written as a scalar <<, and its value is judged as written. valid is false when the walk reached a merge the decoder would reject before finding the key. seen guards against alias cycles.
func mappingHasKey(n *yaml.Node, key string, seen map[*yaml.Node]bool) (found, valid bool) {
	if seen[n] {
		return false, true
	}
	seen[n] = true
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if !(k.Kind == yaml.ScalarNode && k.Value == "<<" && (k.Tag == "" || k.Tag == "!" || k.Tag == "!!merge")) {
			if k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k.Kind == yaml.ScalarNode && k.Value == key {
				return true, true
			}
			continue
		}
		m := n.Content[i+1]
		var sources []*yaml.Node
		switch m.Kind {
		case yaml.MappingNode, yaml.AliasNode:
			sources = []*yaml.Node{m}
		case yaml.SequenceNode:
			sources = m.Content
		default:
			return false, false
		}
		for _, src := range sources {
			if src.Kind == yaml.AliasNode {
				src = src.Alias
			}
			if src == nil || src.Kind != yaml.MappingNode {
				return false, false
			}
			found, valid := mappingHasKey(src, key, seen)
			if found || !valid {
				return found, valid
			}
		}
	}
	return false, true
}

// connectorKey returns the first of connectors: and connector_writes: a YAML document declares at its top level, or "" when it declares neither.
func connectorKey(data []byte) string {
	for _, key := range []string{"connectors", "connector_writes"} {
		if hasKey(data, key) {
			return key
		}
	}
	return ""
}

// platformName normalises a platform name for comparison: surrounding space and case do not matter.
func platformName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// platformProblems names the empty and duplicate entries in a project's platform list, ignoring surrounding space and case.
func platformProblems(platforms []string) []string {
	var errs []string
	seen := map[string]bool{}
	for _, pl := range platforms {
		name := strings.TrimSpace(pl)
		key := platformName(pl)
		switch {
		case name == "":
			errs = append(errs, "a platform name is empty")
		case seen[key]:
			errs = append(errs, fmt.Sprintf("platform %q is declared twice", name))
		}
		seen[key] = true
	}
	return errs
}

// docsPathProblem says why a @docs path is not allowed, or "" if it is.
func docsPathProblem(p string) string {
	switch {
	case strings.TrimSpace(p) == "":
		return "is empty"
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || (len(p) >= 2 && p[1] == ':'):
		return "must be relative to the workspace"
	}
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "must not contain .."
		}
	}
	if filepath.Clean(filepath.FromSlash(strings.ReplaceAll(p, `\`, "/"))) == "." {
		return "would cover the whole workspace"
	}
	return ""
}

// docsBindingProblems is what a run refuses in the merged @docs binding: a command or guidance, and bad paths.
func docsBindingProblems(b Binding) []string {
	var probs []string
	if b.Command != "" || b.Guidance != "" {
		probs = append(probs, "binding @docs must list paths or be not-applicable, not a command or guidance")
	}
	for _, d := range b.Paths {
		if why := docsPathProblem(d); why != "" {
			probs = append(probs, fmt.Sprintf("binding @docs: path %q %s", d, why))
		}
	}
	return probs
}

// validate checks everything a run depends on before it starts.
func validate(c *Config) []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	p := c.Pipeline

	if c.Task != "" && c.Workspace == "" {
		add("task %s: workspace is not set", c.Task)
	} else if c.Workspace != "" {
		if fi, err := os.Stat(c.Workspace); err != nil || !fi.IsDir() {
			add("workspace %s does not exist", c.Workspace)
		}
	}
	if c.Project != "" {
		if len(c.Platforms) == 0 {
			add("project %q declares no platforms: add them to projects/%s/project.yaml, e.g.\n  platforms: [linux, macos, windows]", c.Project, c.Project)
		}
		for _, e := range platformProblems(c.Platforms) {
			add("project %q: %s", c.Project, e)
		}
	}
	for _, name := range p.RequiresBindings {
		if _, ok := c.Bindings[name]; !ok {
			add("binding @%s is required by pipeline %s but not defined (declare it, or declare it not-applicable)", name, p.Name)
		}
	}
	for b := range p.OnExhausted {
		if _, ok := p.Budgets[b]; !ok {
			add("on-exhausted.%s: no budget %q", b, b)
		}
	}
	for b, t := range p.OnExhausted {
		if !t.terminal() {
			add("on-exhausted.%s must finish, error or ask-user", b)
		}
	}
	for _, sk := range c.Skills {
		if sk.Delivery == "on-demand" && sk.Name != filepath.Base(sk.Name) {
			add("skill %q: a skill name must be a single path element", sk.Name)
		}
	}
	if b, ok := c.Bindings["docs"]; ok {
		errs = append(errs, docsBindingProblems(b)...)
	}
	for _, r := range p.FreeRoles {
		if !c.roleExists(r) {
			add("free role %q has no definition", r)
		}
	}

	seen := map[string]bool{}
	var check func(list []*Stage, depth int)
	check = func(list []*Stage, depth int) {
		for i, s := range list {
			at := fmt.Sprintf("stage %q (%s)", s.ID, s.Source)
			if s.ID == "" {
				add("a stage has no id (%s)", s.Source)
				continue
			}
			if seen[s.ID] {
				add("%s: duplicate stage id", at)
			}
			seen[s.ID] = true
			if s.Role == "" {
				add("%s: no role", at)
			} else if !c.roleExists(s.Role) {
				add("%s: role %q has no definition", at, s.Role)
			}
			if s.Skip != nil {
				if s.Skip.UnlessBinding == "" {
					add("%s: skip needs unless-binding", at)
				}
				if _, ok := s.Outcomes.find(s.Skip.Outcome); !ok {
					add("%s: skip outcome %q is not one of the stage's outcomes", at, s.Skip.Outcome)
				}
			}
			if s.Brief != "" {
				if _, err := c.findFile(s.Brief); err != nil {
					add("%s: brief %s not found", at, s.Brief)
				}
			}
			if s.Spend != "" {
				if _, ok := p.Budgets[s.Spend]; !ok {
					add("%s: spends unknown budget %q", at, s.Spend)
				}
			}
			switch s.Curates {
			case "", "style", "skill", "document":
			default:
				add("%s: curates %q is not supported (style, skill or document)", at, s.Curates)
			}
			if s.Advisory != "" {
				if o, ok := s.Outcomes.find(s.Advisory); !ok {
					add("%s: advisory outcome %q is not one of the stage's outcomes", at, s.Advisory)
				} else if o.Error != "" {
					add("%s: advisory outcome %q must not be an error", at, s.Advisory)
				}
			}
			if s.isLead() {
				if depth >= 1 {
					add("%s: nested leads are not implemented yet", at)
				}
				if len(s.Outcomes) > 0 {
					add("%s: a lead has no outcomes of its own; its stages decide the run", at)
				}
				check(s.Stages, depth+1)
				continue
			}
			if len(s.Outcomes) == 0 {
				add("%s: no outcomes", at)
			}
			if s.Parallel != nil {
				if s.Parallel.Join != "all" {
					add("%s: join %q is not supported (only all)", at, s.Parallel.Join)
				}
				switch s.Parallel.Over {
				case "dimensions":
					if len(s.Dimensions) == 0 {
						add("%s: parallel stage with no dimensions", at)
					}
					for _, d := range s.Dimensions {
						if _, err := c.findFile("briefs/review/" + d + ".md"); err != nil {
							add("%s: dimension %q has no brief (briefs/review/%s.md)", at, d, d)
						}
					}
				case "pending-skills", "pending-documents", "unprocessed-sources":
				default:
					add("%s: parallel over %q is not supported", at, s.Parallel.Over)
				}
				if s.Parallel.Empty != "" {
					if _, ok := s.Outcomes.find(s.Parallel.Empty); !ok {
						add("%s: empty outcome %q is not one of the stage's outcomes", at, s.Parallel.Empty)
					}
					if s.Parallel.Over == "dimensions" {
						add("%s: empty cannot be used with parallel over dimensions", at)
					}
				}
			}
			for _, o := range s.Outcomes {
				t := o.Then
				switch {
				case t.Next:
					if i == len(list)-1 {
						add("%s: outcome %s is next, but this is the last stage", at, o.Name)
					}
				case t.Goto != "":
					found := false
					for _, x := range list {
						found = found || x.ID == t.Goto
					}
					if !found {
						add("%s: outcome %s goes to %q, which is not a stage at this level", at, o.Name, t.Goto)
					}
				case t.Finish != "":
					if t.Finish != "success" {
						add("%s: outcome %s: finish must be success", at, o.Name)
					}
				case t.Error != "", t.AskUser != "":
				default:
					add("%s: outcome %s has no transition", at, o.Name)
				}
				for _, b := range []string{t.Spend, t.Reset} {
					if b != "" {
						if _, ok := p.Budgets[b]; !ok {
							add("%s: outcome %s uses unknown budget %q", at, o.Name, b)
						}
					}
				}
			}
		}
	}
	check(p.Stages, 0)
	for i, s := range p.Stages {
		if s.isLead() && s.DispatchedBy != "session" {
			add("stage %q: a top-level lead must be dispatched-by: session", s.ID)
		}
		if s.isLead() && i != 0 {
			add("stage %q: a top-level lead must be the pipeline's first stage", s.ID)
		}
	}
	errs = append(errs, c.problems...)
	for name, by := range c.needs {
		if _, ok := c.Bindings[name]; !ok {
			add("binding @%s is required by %s but not defined", name, by)
		}
	}
	for ev, hs := range c.Events {
		if !validEvent(ev, p) {
			add("event %q is not one starship fires (%s)", ev, hs[0].Source)
		}
		for _, h := range hs {
			if msg := closeHandlerProblem(ev, h); msg != "" {
				add("%s: %s", h.Source, msg)
			}
			if h.Kind == "augment" && !strings.HasPrefix(h.Text, "@") {
				if _, err := os.Stat(filepath.Join(h.Dir, filepath.FromSlash(h.Text))); err != nil {
					if _, err := c.findFile(h.Text); err != nil {
						add("%s: augment %s not found", h.Source, h.Text)
					}
				}
			}
		}
	}
	sort.Strings(errs)
	return errs
}

// closeHandlerProblem says why a handler can't serve task.close, which has no run or workspace: it can't be a binding, an internal ensign command or a generated augment. It is empty for any other event.
func closeHandlerProblem(ev string, h Handler) string {
	if ev != "task.close" {
		return ""
	}
	cmd := strings.TrimSpace(h.Command)
	switch {
	case strings.HasPrefix(cmd, "@"):
		return "task.close can't run the binding " + cmd + ": it has no run or workspace"
	case strings.HasPrefix(cmd, "ensign "):
		return "task.close can't run the internal handler " + cmd + ": it has no run"
	case h.Kind == "augment" && strings.HasPrefix(h.Text, "@"):
		return "task.close can't use the generated augment " + h.Text + ": it has no run"
	}
	return ""
}

// validEvent reports whether starship fires this event for this pipeline.
func validEvent(ev string, p *Pipeline) bool {
	switch ev {
	case "task.close", "run.start", "run.finish:success", "run.finish:error", "run.finish:ask-user":
		return true
	}
	// Events on stages this pipeline doesn't have are inactive for it: a layer or extension
	// serves several pipelines.
	return strings.HasPrefix(ev, "stage.before:") || strings.HasPrefix(ev, "stage.after:")
}

// findFile looks for a control-plane-relative file in the bridge first, then the control plane.
func (c *Config) findFile(rel string) (string, error) {
	for _, base := range []string{c.Bridge, c.ControlPlane} {
		p := filepath.Join(base, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", rel)
}

func (c *Config) roleExists(role string) bool {
	if _, err := c.findFile("roles/" + role + ".md"); err == nil {
		return true
	}
	for _, dir := range extensionRoleDirs(c.Bridge, c.ControlPlane) {
		if _, err := os.Stat(filepath.Join(dir, role+".md")); err == nil {
			return true
		}
	}
	return false
}

// yamlUnmarshal decodes a whole file, guarded against yaml.v3 panics and with parser-class line numbers corrected. Frontmatter goes through decodeFrontmatter, which supplies the line the dropped --- would have taken.
func yamlUnmarshal(data []byte, out any) error {
	return correctParserLine(guardedDecode(func() error { return yaml.Unmarshal(data, out) }), data)
}

func writeYAML(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// availableExtensions finds every extension: the control plane's extensions, then the bridge's,
// which replace a control-plane extension of the same name.
func availableExtensions(bridge, cp string) map[string]*Extension {
	good, _ := scanExtensions(bridge, cp)
	return good
}

// brokenExtension is an extension file that does not decode, or that declares no name.
type brokenExtension struct {
	file string
	err  error
}

func (e *brokenExtension) Error() string { return e.err.Error() }

// scanExtensions is availableExtensions that also returns the extensions whose file is broken: one that does not decode, by the name it declares or, when none can be recovered, by its folder's name; and one that decodes without a name, by its folder's name unless a good extension or another broken one already has that key. A file that fails to decode has no name from the decode, so the name is recovered from its first column-0 name: line. A later extension of the same name, good or broken, decides the broken entry. A later broken extension whose name was decoded or recovered also removes an earlier good one of that name from the good map, since it replaces it; one keyed only by its folder's name leaves good entries alone, because that name can equal an unrelated extension's.
func scanExtensions(bridge, cp string) (map[string]*Extension, map[string]error) {
	out := map[string]*Extension{}
	broken := map[string]error{}
	// A folder extensionDirs could not list is reported by its other callers (help, link).
	dirs, _ := extensionDirs(bridge, cp)
	for _, dir := range dirs {
		var b Extension
		err := readExtensionYAML(filepath.Join(dir, "extension.yaml"), &b)
		switch {
		case err == nil && b.Name != "":
			b.dir = dir
			out[b.Name] = &b
			delete(broken, b.Name)
		case err != nil:
			key, named := brokenKey(dir, b.Name)
			if named {
				delete(out, key)
			}
			broken[key] = &brokenExtension{file: filepath.Join(dir, "extension.yaml"), err: err}
		case err == nil:
			key, file := filepath.Base(dir), filepath.Join(dir, "extension.yaml")
			_, isGood := out[key]
			_, isBroken := broken[key]
			if !isGood && !isBroken {
				broken[key] = &brokenExtension{file: file, err: fmt.Errorf("%s declares no name", file)}
			}
		}
	}
	return out, broken
}

// brokenKey is the name an extension that failed to decode goes by: the decoded name if any, else the name: line, else its folder's name. named is false for the folder's name, which no extension declared.
func brokenKey(dir, decoded string) (key string, named bool) {
	if decoded != "" {
		return decoded, true
	}
	if name := declaredName(filepath.Join(dir, "extension.yaml")); name != "" {
		return name, true
	}
	return filepath.Base(dir), false
}

// declaredName reads the name: line of a file that failed to decode as a whole, or "" when there is none or the file cannot be read. It accepts a UTF-8 BOM and UTF-16, with or without a BOM, which Windows editors write.
func declaredName(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(normalizeYAMLBytes(data)), "\n") {
		if strings.HasPrefix(line, "name:") {
			var n struct{ Name string }
			if yaml.Unmarshal([]byte(line), &n) != nil {
				return ""
			}
			return n.Name
		}
	}
	return ""
}

// normalizeYAMLBytes strips a UTF-8 BOM and decodes UTF-16 to UTF-8, found by its BOM or else by the YAML spec's first-two-bytes rule (`x 00` little-endian, `00 x` big-endian); other input is returned unchanged. It runs only on a file that already failed to decode, so a false positive can only change the key of an already-broken file.
func normalizeYAMLBytes(data []byte) []byte {
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return data[3:]
	}
	if len(data) < 2 {
		return data
	}
	var little bool
	switch {
	case data[0] == 0xFF && data[1] == 0xFE:
		little, data = true, data[2:]
	case data[0] == 0xFE && data[1] == 0xFF:
		data = data[2:]
	case data[0] != 0 && data[1] == 0:
		little = true
	case data[0] == 0 && data[1] != 0:
	default:
		return data
	}
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		if little {
			units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
		} else {
			units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
		}
	}
	return []byte(string(utf16.Decode(units)))
}

// extensionDirs lists the extension folders: the control plane's, then the bridge's. A folder is one with an extension.yaml, found through symlinks. What could not be listed or checked is the error; the folders found are returned with it.
func extensionDirs(bridge, cp string) ([]string, error) {
	var out []string
	var errs []error
	for _, base := range []string{cp, bridge} {
		dir := filepath.Join(base, "extensions")
		entries, err := readDirIfAny(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			d := filepath.Join(dir, e.Name())
			yamlPath := filepath.Join(d, "extension.yaml")
			_, err := os.Stat(yamlPath)
			if errors.Is(err, fs.ErrNotExist) {
				// A dangling symlink still counts, so it is reported as broken.
				if _, lerr := os.Lstat(yamlPath); lerr == nil {
					err = nil
				}
			}
			if err == nil {
				out = append(out, d)
				continue
			}
			// A stray file in extensions/ is skipped on both platforms: the stat fails with ENOTDIR on Linux and reads as not-exist on Windows, and either way d is not a directory.
			if fi, serr := os.Stat(d); !errors.Is(err, fs.ErrNotExist) && (serr != nil || fi.IsDir()) {
				errs = append(errs, err)
			}
		}
	}
	return out, errors.Join(errs...)
}

// readDirIfAny lists dir, and a missing dir has no entries. Windows reports a path beneath a plain file as not-exist, so a plain file at dir or anywhere higher up the path is an error, not a missing folder.
func readDirIfAny(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if file := plainFileOnPath(dir); file != "" {
			return nil, fmt.Errorf("%s is not a folder", file)
		}
		return nil, nil
	}
	return entries, err
}

// plainFileOnPath returns the nearest existing ancestor-or-self of p when it is not a folder, or "" when it is a folder or nothing on the path exists.
func plainFileOnPath(p string) string {
	for {
		if fi, err := os.Stat(p); err == nil {
			if fi.IsDir() {
				return ""
			}
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}

// reallyMissing reports whether err says path is not there and no plain file sits on it.
func reallyMissing(path string, err error) bool {
	// Windows reports a plain file on the path as not-exist.
	return errors.Is(err, fs.ErrNotExist) && plainFileOnPath(path) == ""
}

// filesIn lists the files in dir whose names end in suffix, as paths, in name order. It reads the folder rather than matching a pattern, so brackets in the path mean nothing. A missing dir has none.
func filesIn(dir, suffix string) ([]string, error) {
	entries, err := readDirIfAny(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// subDirs lists the folders in dir as paths, in name order, following symlinks to folders and skipping files. A missing dir has none.
func subDirs(dir string) ([]string, error) {
	entries, err := readDirIfAny(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out, nil
}

// projectFiles lists projects/*/<rel> that exist, in project order. A dangling symlink exists.
func projectFiles(bridge string, rel ...string) ([]string, error) {
	projects, err := subDirs(filepath.Join(bridge, "projects"))
	var out []string
	for _, p := range projects {
		f := filepath.Join(append([]string{p}, rel...)...)
		if _, lerr := os.Lstat(f); lerr == nil {
			out = append(out, f)
		}
	}
	return out, err
}

// taskFiles lists projects/*/tasks/*/task.md that exist, in project then task order. A project whose tasks is not a folder has none.
func taskFiles(bridge string) ([]string, error) {
	projects, err := subDirs(filepath.Join(bridge, "projects"))
	var out []string
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, p := range projects {
		tasks, terr := subDirs(filepath.Join(p, "tasks"))
		if terr != nil {
			if fi, serr := os.Stat(filepath.Join(p, "tasks")); serr == nil && !fi.IsDir() {
				continue
			}
			errs = append(errs, terr)
			continue
		}
		for _, t := range tasks {
			f := filepath.Join(t, "task.md")
			if _, lerr := os.Lstat(f); lerr == nil {
				out = append(out, f)
			}
		}
	}
	return out, errors.Join(errs...)
}

// flatLine collapses all whitespace, newlines included, to single spaces.
func flatLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func extensionRoleDirs(bridge, cp string) []string {
	var out []string
	for _, b := range availableExtensions(bridge, cp) {
		out = append(out, filepath.Join(b.dir, "roles"))
	}
	sort.Strings(out)
	return out
}

// brokenRoleLines says, one line each and sorted, which broken or nameless extensions have roles that link leaves out. It walks every extension folder, so a broken file that another extension shadows is still counted: unless, for a file that fails to decode, its key is gone from the broken map, which means a later good extension took its place. A folder that cannot be listed is a line too.
func brokenRoleLines(bridge, cp string) []string {
	_, broken := scanExtensions(bridge, cp)
	var out []string
	dirs, derr := extensionDirs(bridge, cp)
	if derr != nil {
		out = append(out, flatLine("Note: could not list all extensions: "+derr.Error()))
	}
	for _, dir := range dirs {
		file := filepath.Join(dir, "extension.yaml")
		var b Extension
		err := readExtensionYAML(file, &b)
		var name string
		switch {
		case err == nil && b.Name != "":
			continue
		case err == nil:
			name = filepath.Base(dir)
			err = fmt.Errorf("%s declares no name", file)
		default:
			name, _ = brokenKey(dir, b.Name)
			if _, ok := broken[name]; !ok {
				continue
			}
		}
		rolesDir := filepath.Join(dir, "roles")
		found, gerr := filesIn(rolesDir, ".md")
		switch {
		case gerr != nil:
			out = append(out, flatLine(fmt.Sprintf("Note: extension %s: could not look for its roles in %s: %v", name, rolesDir, gerr)))
		case len(found) > 0:
			out = append(out, flatLine(fmt.Sprintf("Note: extension %s is broken, so its roles are not linked: %v", name, err)))
		}
	}
	sort.Strings(out)
	return out
}

func (c *Config) addEvents(events map[string][]HandlerSpec, label, dir string) {
	for _, ev := range sortedKeys(events) {
		for _, h := range events[ev] {
			r := Handler{Source: label, Dir: dir, Status: h.Status}
			n := 0
			if h.Require != "" {
				r.Kind, r.Command, n = "require", h.Require, n+1
			}
			if h.Observe != "" {
				r.Kind, r.Command, n = "observe", h.Observe, n+1
			}
			if h.Augment != "" {
				r.Kind, r.Text, n = "augment", h.Augment, n+1
			}
			if n != 1 {
				c.problems = append(c.problems, fmt.Sprintf("%s: a handler for %s needs exactly one of require, observe, augment", label, ev))
				continue
			}
			c.Events[ev] = append(c.Events[ev], r)
		}
	}
}
