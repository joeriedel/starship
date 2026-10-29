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
	Stages       []*Stage  `yaml:"stages" json:"stages,omitempty"`
	Source       string    `yaml:"-" json:"source"`
	line         int
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
	Source        string `json:"source"`
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
		case "linux", "windows", "darwin":
			return b, fmt.Errorf("%s: has platform variants but none for %s", source, runtime.GOOS)
		default:
			return b, fmt.Errorf("%s: unknown binding key %q", source, k)
		}
	}
	return b, nil
}

// Layer is the shape shared by bridge.yaml, project.yaml and task.md frontmatter.
type Layer struct {
	Pipelines map[string]struct {
		Ops []yaml.Node `yaml:"ops"`
	} `yaml:"pipelines"`
	Bindings yaml.Node `yaml:"bindings"`
}

type BridgeFile struct {
	Layer        `yaml:",inline"`
	ControlPlane struct {
		Path string `yaml:"path"`
	} `yaml:"control-plane"`
}

type TaskFile struct {
	Layer     `yaml:",inline"`
	Pipeline  string `yaml:"pipeline"`
	Workspace string `yaml:"workspace"`
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

// loadConfig merges control plane → bridge → project → task for one task.
func loadConfig(bridge, task string) (*Config, error) {
	parts := strings.Split(task, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("task must be <project>/<task>, got %q", task)
	}
	c := &Config{Bridge: bridge, Project: parts[0], Task: task, Bindings: map[string]Binding{}}

	var bf BridgeFile
	bridgeFile := filepath.Join(bridge, "bridge.yaml")
	if err := readYAML(bridgeFile, &bf); err != nil {
		return nil, err
	}
	if bf.ControlPlane.Path == "" {
		return nil, fmt.Errorf("%s: control-plane.path is not set", bridgeFile)
	}
	c.ControlPlane = bf.ControlPlane.Path
	if !filepath.IsAbs(c.ControlPlane) {
		c.ControlPlane = filepath.Join(bridge, c.ControlPlane)
	}

	projectDir := filepath.Join(bridge, "projects", c.Project)
	projectFile := filepath.Join(projectDir, "project.yaml")
	var pf Layer
	if err := readYAML(projectFile, &pf); err != nil {
		return nil, fmt.Errorf("project %q: %v", c.Project, err)
	}

	c.TaskDir = filepath.Join(projectDir, "tasks", parts[1])
	taskFile := filepath.Join(c.TaskDir, "task.md")
	front, body, err := splitFrontmatter(taskFile)
	if err != nil {
		return nil, fmt.Errorf("task %q: %v", task, err)
	}
	var tf TaskFile
	if err := yaml.Unmarshal(front, &tf); err != nil {
		return nil, fmt.Errorf("%s: %v", taskFile, err)
	}
	c.Workspace = tf.Workspace
	c.WorkItem = strings.TrimSpace(string(body))
	name := tf.Pipeline
	if name == "" {
		name = "coding"
	}

	base := filepath.Join(c.ControlPlane, "pipelines", name+".yaml")
	p := &Pipeline{}
	if err := readYAML(base, p); err != nil {
		return nil, fmt.Errorf("pipeline %q: %v", name, err)
	}
	p.Sources = map[string]string{}
	rel := relTo(c.ControlPlane, base)
	setSources(p.Stages, "control-plane:"+rel)
	for k := range p.Budgets {
		p.Sources["budgets."+k] = "control-plane:" + rel
	}
	for k := range p.OnExhausted {
		p.Sources["on-exhausted."+k] = "control-plane:" + rel
	}
	c.Pipeline = p

	layers := []struct {
		label string
		file  string
		layer Layer
	}{
		{"bridge:" + relTo(bridge, bridgeFile), bridgeFile, bf.Layer},
		{"project:" + relTo(bridge, projectFile), projectFile, pf},
		{"task:" + relTo(bridge, taskFile), taskFile, tf.Layer},
	}
	for _, l := range layers {
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
	return c, nil
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

	if c.Workspace == "" {
		add("task %s: workspace is not set", c.Task)
	} else if fi, err := os.Stat(c.Workspace); err != nil || !fi.IsDir() {
		add("task %s: workspace %s does not exist", c.Task, c.Workspace)
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
				if s.Parallel.Over != "dimensions" {
					add("%s: parallel over %q is not supported (only dimensions)", at, s.Parallel.Over)
				}
				if s.Parallel.Join != "all" {
					add("%s: join %q is not supported (only all)", at, s.Parallel.Join)
				}
				if len(s.Dimensions) == 0 {
					add("%s: parallel stage with no dimensions", at)
				}
				for _, d := range s.Dimensions {
					if _, err := c.findFile("briefs/review/" + d + ".md"); err != nil {
						add("%s: dimension %q has no brief (briefs/review/%s.md)", at, d, d)
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
	for _, s := range p.Stages {
		if s.isLead() && s.DispatchedBy != "session" {
			add("stage %q: a top-level lead must be dispatched-by: session", s.ID)
		}
	}
	sort.Strings(errs)
	return errs
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
