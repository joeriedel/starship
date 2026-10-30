// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

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
	// Curates names the document this stage's agents rewrite: style or skill.
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
	// Plain says the command needs no judgement to run: ensign runs it and reads the exit
	// code. On @test it means ensign runs the test stage itself, with no tester agent.
	Plain  bool   `json:"plain,omitempty"`
	Source string `json:"source"`
}

// parseBinding resolves platform variants for this machine.
func parseBinding(n *yaml.Node, source string) (Binding, error) {
	b := Binding{Source: source}
	if n.Kind != yaml.MappingNode {
		return b, fmt.Errorf("%s: a binding must be a mapping (command, guidance, not-applicable, or per-platform)", source)
	}
	var m map[string]yaml.Node
	if err := n.Decode(&m); err != nil {
		return b, fmt.Errorf("%s: %v", source, err)
	}
	if v, ok := m[runtime.GOOS]; ok {
		return parseBinding(&v, source)
	}
	for k, v := range m {
		switch k {
		case "command":
			b.Command = v.Value
		case "guidance":
			b.Guidance = v.Value
		case "not-applicable":
			b.NotApplicable = v.Value
		case "plain":
			if err := v.Decode(&b.Plain); err != nil {
				return b, fmt.Errorf("%s: plain must be true or false", source)
			}
		case "linux", "windows", "darwin":
			return b, fmt.Errorf("%s: has platform variants but none for %s", source, runtime.GOOS)
		default:
			return b, fmt.Errorf("%s: unknown binding key %q", source, k)
		}
	}
	if b.Plain && b.Command == "" {
		return b, fmt.Errorf("%s: a plain binding needs a command", source)
	}
	return b, nil
}

// Layer is the shape shared by bridge.yaml, project.yaml and task.md frontmatter.
type Layer struct {
	Pipelines map[string]struct {
		Ops []yaml.Node `yaml:"ops"`
	} `yaml:"pipelines"`
	Bindings  yaml.Node                `yaml:"bindings"`
	Use       []string                 `yaml:"use"`
	Skills    yaml.Node                `yaml:"skills"`
	Workspace string                   `yaml:"workspace"` // a project's code location: new tasks start with it, and project-wide runs use it
	Events    map[string][]HandlerSpec `yaml:"events"`
	Journal   struct {
		Commit string `yaml:"commit"`
		Reason string `yaml:"reason"`
	} `yaml:"journal"`
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
}

func loadBridgeFile(bridge string) (*BridgeFile, error) {
	var bf BridgeFile
	if err := readYAML(filepath.Join(bridge, "bridge.yaml"), &bf); err != nil {
		return nil, err
	}
	return &bf, nil
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

// Machine is this machine's settings for a bridge, kept out of git.
type Machine struct {
	Projects string `yaml:"projects"` // the root that $PROJECTS in workspace paths expands to
}

func machineFile(bridge string) string { return filepath.Join(bridge, ".starship", "machine.yaml") }

func loadMachine(bridge string) Machine {
	var m Machine
	readYAML(machineFile(bridge), &m)
	return m
}

// expandWorkspace resolves $PROJECTS against this machine's root.
func expandWorkspace(bridge, ws string) (string, error) {
	rest, ok := strings.CutPrefix(ws, "$PROJECTS")
	if !ok {
		return ws, nil
	}
	root := loadMachine(bridge).Projects
	if root == "" {
		return "", fmt.Errorf("workspace %s uses $PROJECTS, which is not set on this machine (.starship/machine.yaml: projects)", ws)
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rest, "/"))), nil
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
}

// Config is everything a run is built from, after merging the layers.
type Config struct {
	Bridge       string
	ControlPlane string
	Project      string
	Task         string // "<project>/<task>"
	TaskDir      string
	Workspace    string
	WorkItem     string // task.md body
	Pipeline     *Pipeline
	Bindings     map[string]Binding
	Events       map[string][]Handler
	Extensions   []string
	JournalOff   string // reason, when the project turned journal commits off
	Skills       []SkillDelivery
	needs        map[string]string // binding -> what requires it, from extensions
	problems     []string
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

func splitFrontmatter(path string) (front, body []byte, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
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
}

type layerEntry struct {
	label string
	file  string
	layer Layer
}

// loadConfig merges control plane → bridge → project → task for one task.
func loadConfig(bridge, task string) (*Config, error) {
	return loadTarget(bridge, Target{Task: task})
}

// loadTarget merges the layers that apply to a target.
func loadTarget(bridge string, t Target) (*Config, error) {
	c := &Config{Bridge: bridge, Project: t.Project, Task: t.Task, Bindings: map[string]Binding{},
		Events: map[string][]Handler{}, needs: map[string]string{}}

	bridgeFile := filepath.Join(bridge, "bridge.yaml")
	bfp, err := loadBridgeFile(bridge)
	if err != nil {
		return nil, err
	}
	if c.ControlPlane, err = controlPlaneDir(bridge, bfp); err != nil {
		return nil, err
	}
	layers := []layerEntry{{"bridge:" + relTo(bridge, bridgeFile), bridgeFile, bfp.Layer}}
	name := t.Pipeline

	var pf Layer
	loadProject := func() error {
		projectFile := filepath.Join(bridge, "projects", c.Project, "project.yaml")
		if err := readYAML(projectFile, &pf); err != nil {
			return fmt.Errorf("project %q: %v", c.Project, err)
		}
		layers = append(layers, layerEntry{"project:" + relTo(bridge, projectFile), projectFile, pf})
		if pf.Journal.Commit == "off" {
			c.JournalOff = pf.Journal.Reason
		}
		return nil
	}

	if t.Task != "" {
		parts := strings.Split(t.Task, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("task must be <project>/<task>, got %q", t.Task)
		}
		c.Project = parts[0]
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
		if err := yaml.Unmarshal(front, &tf); err != nil {
			return nil, fmt.Errorf("%s: %v", taskFile, err)
		}
		if c.Workspace, err = expandWorkspace(bridge, tf.Workspace); err != nil {
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
			if pf.Workspace != "" {
				if c.Workspace, err = expandWorkspace(bridge, pf.Workspace); err != nil {
					return nil, err
				}
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

	extensions := availableExtensions(bridge, c.ControlPlane)
	for _, l := range layers {
		for _, use := range l.layer.Use {
			if contains(c.Extensions, use) {
				continue
			}
			b, ok := extensions[use]
			if !ok {
				return nil, fmt.Errorf("%s: no extension %q", l.label, use)
			}
			c.Extensions = append(c.Extensions, use)
			label := "extension:" + use
			if po, ok := b.Pipelines[name]; ok {
				for i := range po.Ops {
					if err := applyOp(p, &po.Ops[i], label); err != nil {
						return nil, err
					}
				}
			}
			c.addEvents(b.Events, label, b.dir)
			for _, req := range b.RequiresBindings {
				c.needs[req] = label
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
		if l.layer.Bindings.Kind == yaml.MappingNode {
			n := &l.layer.Bindings
			for i := 0; i < len(n.Content); i += 2 {
				key := strings.TrimPrefix(n.Content[i].Value, "@")
				b, err := parseBinding(n.Content[i+1], fmt.Sprintf("%s:%d", l.label, n.Content[i].Line))
				if err != nil {
					return nil, err
				}
				c.Bindings[key] = b
			}
		}
	}
	if err := c.selectSkills(layers); err != nil {
		return nil, err
	}
	return c, nil
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
		var t Transition
		if err := val.Decode(&t); err != nil {
			return err
		}
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
				case "pending-skills", "unprocessed-sources":
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

// validEvent reports whether starship fires this event for this pipeline.
func validEvent(ev string, p *Pipeline) bool {
	switch ev {
	case "run.start", "run.finish:success", "run.finish:error", "run.finish:ask-user":
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
	out := map[string]*Extension{}
	for _, dir := range extensionDirs(bridge, cp) {
		var b Extension
		if readYAML(filepath.Join(dir, "extension.yaml"), &b) == nil && b.Name != "" {
			b.dir = dir
			out[b.Name] = &b
		}
	}
	return out
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
