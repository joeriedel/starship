package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A skill is knowledge of a technology. Its text comes from the control plane, the
// bridge and the project, each extending the one above unless it says replace: true,
// plus the entries the curator has learned for it in the bridge.

type skillMeta struct {
	Description  string   `yaml:"description"`
	DefaultRoles []string `yaml:"default-roles"`
	Delivery     string   `yaml:"delivery"`
	Replace      bool     `yaml:"replace"`
}

// SkillDelivery is a selected skill as frozen into a run.
type SkillDelivery struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Roles       []string `json:"roles"`
	Delivery    string   `json:"delivery"` // inject or on-demand
	Text        string   `json:"text"`
}

func skillDirs(bridge, cp, project string) []string {
	dirs := []string{filepath.Join(cp, "skills"), filepath.Join(bridge, "skills")}
	if project != "" {
		dirs = append(dirs, filepath.Join(bridge, "projects", project, "skills"))
	}
	return dirs
}

func learnedSkillFile(bridge, name string) string {
	return filepath.Join(bridge, "skills", name+".learned.yaml")
}

// assembleSkill builds a skill's text and settings from every layer that has it.
func assembleSkill(bridge, cp, project, name string) (*SkillDelivery, error) {
	sd := &SkillDelivery{Name: name, Delivery: "inject"}
	var meta skillMeta
	found := false
	var body []string
	for _, dir := range skillDirs(bridge, cp, project) {
		f := filepath.Join(dir, name+".md")
		front, text, err := readSkill(f)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = true
		var m skillMeta
		if err := yaml.Unmarshal(front, &m); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		if m.Replace {
			body = nil
		}
		body = append(body, strings.TrimSpace(text))
		if m.Description != "" {
			meta.Description = m.Description
		}
		if len(m.DefaultRoles) > 0 {
			meta.DefaultRoles = m.DefaultRoles
		}
		if m.Delivery != "" {
			meta.Delivery = m.Delivery
		}
	}
	if !found {
		return nil, fmt.Errorf("no skill %q (looked in skills/ of the control plane, bridge and project)", name)
	}
	if doc, err := loadDoc(learnedSkillFile(bridge, name)); err == nil && len(doc.Entries) > 0 {
		body = append(body, "### Learned\n\n"+renderDoc(doc, true))
	}
	sd.Description, sd.Roles, sd.Text = meta.Description, meta.DefaultRoles, strings.Join(body, "\n\n")
	if meta.Delivery != "" {
		sd.Delivery = meta.Delivery
	}
	if len(sd.Roles) == 0 {
		sd.Roles = []string{"coder", "reviewer", "style-reviewer"}
	}
	return sd, nil
}

// readSkill splits a skill file into its frontmatter (possibly empty) and body.
func readSkill(f string) ([]byte, string, error) {
	data, err := os.ReadFile(f)
	if err != nil {
		return nil, "", err
	}
	if strings.HasPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), "---\n") {
		front, body, err := splitFrontmatter(f)
		return front, string(body), err
	}
	return nil, string(data), nil
}

// skillSelection is one layer's `skills:` — a list to add, or add/remove/roles.
type skillSelection struct {
	Add    []string            `yaml:"add"`
	Remove []string            `yaml:"remove"`
	Roles  map[string][]string `yaml:"roles"`
}

// selectSkills works out which skills this run uses, and for which roles.
func (c *Config) selectSkills(layers []layerEntry) error {
	var order []string
	chosen := map[string]bool{}
	roles := map[string][]string{}
	for _, l := range layers {
		n := &l.layer.Skills
		var sel skillSelection
		switch n.Kind {
		case 0:
			continue
		case yaml.SequenceNode:
			if err := n.Decode(&sel.Add); err != nil {
				return fmt.Errorf("%s: skills: %v", l.label, err)
			}
		case yaml.MappingNode:
			if err := n.Decode(&sel); err != nil {
				return fmt.Errorf("%s: skills: %v", l.label, err)
			}
		default:
			return fmt.Errorf("%s: skills must be a list, or add/remove/roles", l.label)
		}
		for _, s := range sel.Add {
			if !chosen[s] {
				chosen[s] = true
				order = append(order, s)
			}
		}
		for _, s := range sel.Remove {
			delete(chosen, s)
		}
		for s, r := range sel.Roles {
			roles[s] = r
		}
	}
	for _, name := range order {
		if !chosen[name] {
			continue
		}
		sd, err := assembleSkill(c.Bridge, c.ControlPlane, c.Project, name)
		if err != nil {
			return err
		}
		if r, ok := roles[name]; ok {
			sd.Roles = r
		}
		c.Skills = append(c.Skills, *sd)
	}
	return nil
}

// skillSections is what a dispatch of role is told about this run's skills.
func skillSections(r *RunFiles, role string) string {
	var inject, onDemand []string
	for _, sk := range r.Snap.Skills {
		if !contains(sk.Roles, role) {
			continue
		}
		if sk.Delivery == "on-demand" {
			onDemand = append(onDemand, fmt.Sprintf("- `%s` — %s", sk.Name, sk.Description))
			continue
		}
		inject = append(inject, "## Skill: "+sk.Name+"\n\n"+sk.Text)
	}
	out := strings.Join(inject, "\n\n")
	if len(onDemand) > 0 {
		out += "\n\n## Skills to load when relevant\n\n" + strings.Join(onDemand, "\n")
	}
	return strings.TrimSpace(out)
}

// onDemandSkills is what link writes into .claude/skills/ for on-demand skills.
func onDemandSkills(bridge, cp string) map[string][]byte {
	out := map[string][]byte{}
	names := map[string]bool{}
	for _, dir := range skillDirs(bridge, cp, "") {
		found, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		for _, f := range found {
			names[strings.TrimSuffix(filepath.Base(f), ".md")] = true
		}
	}
	for _, name := range sortedStrings(names) {
		sd, err := assembleSkill(bridge, cp, "", name)
		if err != nil || sd.Delivery != "on-demand" {
			continue
		}
		desc := sd.Description
		if desc == "" {
			desc = "Knowledge of " + name
		}
		out[".claude/skills/"+name+"/SKILL.md"] = []byte(fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n",
			name, yamlScalar(desc), sd.Text))
	}
	return out
}

func yamlScalar(s string) string {
	b, _ := yaml.Marshal(s)
	return strings.TrimSpace(string(b))
}

func pendingSkillDir(bridge, skill string) string {
	return filepath.Join(bridge, "skills", "pending", skill)
}

// recordFacts saves the facts a stage reported, one file each, for the curator.
func recordFacts(r *RunFiles, d *Dispatch) {
	for i, line := range d.Fields["fact"] {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}
		skill := strings.TrimSpace(parts[0])
		if !validName(skill) {
			continue
		}
		f := map[string]string{"fact": strings.TrimSpace(parts[1]), "run": r.State.Run, "stage": d.Stage}
		if len(parts) == 3 {
			f["evidence"] = strings.TrimSpace(parts[2])
		}
		path := filepath.Join(pendingSkillDir(r.Snap.Bridge, skill), fmt.Sprintf("%s-%s-%d.yaml", r.State.Run, d.ID, i+1))
		if err := writeYAML(path, f); err == nil {
			r.State.log("fact", d.Stage, d.ID, skill+": "+f["fact"])
		}
	}
}

// generatorItems resolves a parallel stage's list for this run.
func generatorItems(c *Config, over string, rebuild bool) ([]string, error) {
	switch over {
	case "pending-skills":
		dirs, _ := os.ReadDir(filepath.Join(c.Bridge, "skills", "pending"))
		var out []string
		for _, d := range dirs {
			files, _ := filepath.Glob(filepath.Join(c.Bridge, "skills", "pending", d.Name(), "*.yaml"))
			if d.IsDir() && len(files) > 0 {
				out = append(out, d.Name())
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no skill has pending facts to curate")
		}
		return out, nil
	case "unprocessed-sources":
		src := loadSources(c.Bridge, c.Project)
		var out []string
		for _, s := range src.Sources {
			if rebuild || s.Processed == "" {
				out = append(out, s.ID)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("project %s has no unprocessed style sources (ingest one, or rebuild)", c.Project)
		}
		return out, nil
	}
	return nil, nil
}

// itemContext is what one branch of a generated parallel stage is given about its item.
func itemContext(r *RunFiles, s *Stage, item string, d *Dispatch) string {
	switch s.Parallel.Over {
	case "pending-skills":
		files, _ := filepath.Glob(filepath.Join(pendingSkillDir(r.Snap.Bridge, item), "*.yaml"))
		sort.Strings(files)
		var facts []string
		for _, f := range files {
			data, _ := os.ReadFile(f)
			facts = append(facts, strings.TrimSpace(string(data)))
			rel, _ := filepath.Rel(r.Snap.Bridge, f)
			d.Fields["inputs"] = append(d.Fields["inputs"], filepath.ToSlash(rel))
		}
		base, _ := assembleSkill(r.Snap.Bridge, r.Snap.ControlPlane, "", item)
		baseText := ""
		if base != nil {
			baseText = base.Text
		}
		cur, _ := os.ReadFile(learnedSkillFile(r.Snap.Bridge, item))
		return fmt.Sprintf("## Skill: %s\n\n%s\n\n## Its learned entries now (K ids)\n\n```yaml\n%s```\n\n## Pending facts reported by agents\n\n```yaml\n%s\n```\n",
			item, baseText, string(cur), strings.Join(facts, "\n---\n"))
	case "unprocessed-sources":
		src := loadSources(r.Snap.Bridge, r.Snap.Project)
		for _, s := range src.Sources {
			if s.ID != item {
				continue
			}
			d.Fields["inputs"] = append(d.Fields["inputs"], s.ID)
			text := fmt.Sprintf("## Source %s (trust: %s)\n\n", s.ID, s.Trust)
			if s.Note != "" {
				text += "Note: " + s.Note + "\n\n"
			}
			if s.Path != "" {
				data, _ := os.ReadFile(filepath.Join(r.Snap.Bridge, "projects", r.Snap.Project, "style", filepath.FromSlash(s.Path)))
				if len(data) > 60000 {
					data = append(data[:60000], []byte("\n…(truncated)")...)
				}
				text += "```\n" + string(data) + "\n```\n"
			} else {
				text += "Read the code at " + s.Ref + " — the files a person would hold up as the project's style.\n"
			}
			return text
		}
	}
	return ""
}
