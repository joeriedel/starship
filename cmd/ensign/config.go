// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
	// Curates names the document this stage's agents rewrite: style, skill or document.
	Curates string   `yaml:"curates" json:"curates,omitempty"`
	Stages  []*Stage `yaml:"stages" json:"stages,omitempty"`
	Source  string   `yaml:"-" json:"source"`
	line    int
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
		return parseBinding(&v, source)
	}
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
			return b, fmt.Errorf("%s: has platform variants but none for %s", source, runtime.GOOS)
		default:
			return b, fmt.Errorf("%s: unknown binding key %q", source, k)
		}
	}
	if b.Plain && b.Command == "" {
		return b, fmt.Errorf("%s: a plain binding needs a command", source)
	}
	if len(b.Paths) > 0 && (b.Command != "" || b.Guidance != "") {
		return b, fmt.Errorf("%s: a binding with paths cannot also have a command or guidance", source)
	}
	return b, nil
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
}

func loadBridgeFile(bridge string) (*BridgeFile, error) {
	var bf BridgeFile
	if err := readYAML(filepath.Join(bridge, "bridge.yaml"), &bf); err != nil {
		return nil, err
	}
	return &bf, nil
}

// bridgeUses reports whether the bridge's own bridge.yaml names the extension in use:. A bridge.yaml that cannot be loaded names nothing.
func bridgeUses(bridge, name string) bool {
	bf, err := loadBridgeFile(bridge)
	return err == nil && slices.Contains(bf.Use, name)
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
	if err := yaml.Unmarshal(data, out); err != nil {
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
	return loadTarget(bridge, Target{Task: task, Location: runLocation(bridge, task, location)})
}

// projectPipeline reads the project's pipeline: setting; none is the only value it may have. A file that can't be read has no setting; the callers report that themselves.
func projectPipeline(projectFile string) (string, error) {
	var pp struct {
		Pipeline string `yaml:"pipeline"`
	}
	if readYAML(projectFile, &pp) != nil {
		return "", nil
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
	if err != nil || yaml.Unmarshal(append([]byte("\n"), front...), &tf) != nil {
		return false, nil
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
		// splitFrontmatter drops the opening --- line; the leading newline puts node lines back on their file lines.
		if err := yaml.Unmarshal(append([]byte("\n"), front...), &tf); err != nil {
			return nil, fmt.Errorf("%s: %v", taskFile, err)
		}
		if hasPlatformsKey(front) {
			return nil, fmt.Errorf("%s: platforms: is a project setting; declare it in projects/%s/project.yaml", taskFile, c.Project)
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

	base, baseLabel := findPipeline(bridge, c.ControlPlane, name)
	if base == "" {
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
	if data, rerr := os.ReadFile(bridgeFile); rerr == nil && hasPlatformsKey(data) {
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
	if err := readYAML(projectFile, &pf); err != nil {
		return layerEntry{}, fmt.Errorf("project %q: %v", c.Project, err)
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
		if data, rerr := os.ReadFile(file); rerr != nil || !hasPlatformsKey(data) {
			return nil, fmt.Errorf("%s: extension %q: %v", l.label, use, berr)
		}
	} else if !ok {
		return nil, fmt.Errorf("%s: no extension %q", l.label, use)
	} else {
		file = filepath.Join(b.dir, "extension.yaml")
	}
	if data, rerr := os.ReadFile(file); rerr == nil && hasPlatformsKey(data) {
		project := c.Project
		if project == "" {
			project = "<project>"
		}
		return nil, fmt.Errorf("%s: platforms: is a project setting; declare it in projects/%s/project.yaml", file, project)
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
	if err := yaml.Unmarshal(append([]byte("\n"), front...), &tf); err != nil {
		return nil, fmt.Errorf("%s: %v", taskFile, err)
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

// sideBindings merges the bindings of the layers, extensions' defaults included, as loadTarget does, using location's layer where the edited location's sits. The task's layer is left out: its overrides are for the edited location, so another side goes bridge, project, then its location.
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
			if b, ok := extensions[use]; ok {
				if err := side.addBindings(&b.Bindings, "extension:"+use, filepath.Join(b.dir, "extension.yaml")); err != nil {
					return nil, err
				}
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
func findPipeline(bridge, cp, name string) (path, label string) {
	f := filepath.Join(cp, "pipelines", name+".yaml")
	if _, err := os.Stat(f); err == nil {
		return f, "control-plane:pipelines/" + name + ".yaml"
	}
	for _, dir := range extensionDirs(bridge, cp) {
		f := filepath.Join(dir, "pipelines", name+".yaml")
		if _, err := os.Stat(f); err == nil {
			return f, "extension:" + filepath.Base(dir) + "/pipelines/" + name + ".yaml"
		}
	}
	return "", ""
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
		if err := arg.Decode(&a); err != nil {
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
		if err := arg.Decode(&a); err != nil || a.With == nil {
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
		if err := val.Decode(&t); err != nil {
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
		if err := val.Decode(&o); err != nil {
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
		return val.Decode(s.Parallel)
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
		if err := val.Decode(&t); err != nil {
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

// hasPlatformsKey reports whether a YAML document has a top-level platforms: key, whatever its value (a null or empty list leaves no trace in Layer.Platforms).
func hasPlatformsKey(data []byte) bool {
	var keys map[string]any
	if yaml.Unmarshal(data, &keys) != nil {
		return false
	}
	_, has := keys["platforms"]
	return has
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
		if b.Command != "" || b.Guidance != "" {
			add("binding @docs must list paths or be not-applicable, not a command or guidance")
		}
		for _, d := range b.Paths {
			if why := docsPathProblem(d); why != "" {
				add("binding @docs: path %q %s", d, why)
			}
		}
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
	for _, s := range p.Stages {
		if s.isLead() && s.DispatchedBy != "session" {
			add("stage %q: a top-level lead must be dispatched-by: session", s.ID)
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
	_, err := c.findFile("roles/" + role + ".md")
	return err == nil
}

func yamlUnmarshal(data []byte, out any) error { return yaml.Unmarshal(data, out) }

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

// brokenExtension is an extension file that does not decode.
type brokenExtension struct {
	file string
	err  error
}

func (e *brokenExtension) Error() string { return e.err.Error() }

// scanExtensions is availableExtensions that also returns the extensions whose file does not decode, by the name they declare or, when none can be recovered, by their folder's name. A file that fails to decode has no name from the decode, so the name is recovered from its first column-0 name: line. A later extension of the same name, good or broken, decides the broken entry; the good map is as availableExtensions has always had it.
func scanExtensions(bridge, cp string) (map[string]*Extension, map[string]error) {
	out := map[string]*Extension{}
	broken := map[string]error{}
	for _, dir := range extensionDirs(bridge, cp) {
		var b Extension
		err := readYAML(filepath.Join(dir, "extension.yaml"), &b)
		switch {
		case err == nil && b.Name != "":
			b.dir = dir
			out[b.Name] = &b
			delete(broken, b.Name)
		case err != nil:
			name := b.Name
			if name == "" {
				name = declaredName(filepath.Join(dir, "extension.yaml"))
			}
			if name == "" {
				name = filepath.Base(dir)
			}
			broken[name] = &brokenExtension{file: filepath.Join(dir, "extension.yaml"), err: err}
		}
	}
	return out, broken
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

func extensionDirs(bridge, cp string) []string {
	var out []string
	for _, base := range []string{cp, bridge} {
		found, _ := filepath.Glob(filepath.Join(base, "extensions", "*", "extension.yaml"))
		for _, f := range found {
			out = append(out, filepath.Dir(f))
		}
	}
	return out
}

func extensionRoleDirs(bridge, cp string) []string {
	var out []string
	for _, b := range availableExtensions(bridge, cp) {
		out = append(out, filepath.Join(b.dir, "roles"))
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
