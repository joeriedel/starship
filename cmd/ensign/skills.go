// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		if err := decodeFrontmatter(front, &m); err != nil {
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
			if err := decodeNode(n, &sel); err != nil {
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

// onDemandPath is where a run keeps the full text of an on-demand skill.
func onDemandPath(runDir, name string) string {
	return filepath.Join(runDir, "skills", name+".md")
}

func onDemandDescription(sk SkillDelivery) string {
	if sk.Description == "" {
		return "Knowledge of " + sk.Name
	}
	return sk.Description
}

// writeOnDemandSkills puts each on-demand skill's full text in the run's folder, where the listing points.
func writeOnDemandSkills(runDir string, skills []SkillDelivery) error {
	for _, sk := range skills {
		if sk.Delivery != "on-demand" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(runDir, "skills"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(onDemandPath(runDir, sk.Name), []byte(sk.Text+"\n"), 0o644); err != nil {
			return err
		}
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
			onDemand = append(onDemand, fmt.Sprintf("- `%s` — %s: %s", sk.Name, onDemandDescription(sk), onDemandPath(r.Dir, sk.Name)))
			continue
		}
		inject = append(inject, "## Skill: "+sk.Name+"\n\n"+sk.Text)
	}
	out := strings.Join(inject, "\n\n")
	if len(onDemand) > 0 {
		out += "\n\n## Skills to load when relevant\n\nRead a skill's file when it bears on your task.\n\n" + strings.Join(onDemand, "\n")
	}
	return strings.TrimSpace(out)
}

func yamlScalar(s string) string {
	b, _ := yaml.Marshal(s)
	return strings.TrimSpace(string(b))
}

func pendingSkillDir(bridge, skill string) string {
	return filepath.Join(bridge, "skills", "pending", skill)
}

// proposedSkillDir holds facts reported for a skill that doesn't exist yet.
func proposedSkillDir(bridge, skill string) string {
	return filepath.Join(bridge, "skills", "proposed", skill)
}

// knownSkills lists every skill defined in the control plane, the bridge or the project.
func knownSkills(bridge, cp, project string) ([]string, error) {
	names := map[string]bool{}
	for _, dir := range skillDirs(bridge, cp, project) {
		found, err := filesIn(dir, ".md")
		if err != nil {
			return nil, fmt.Errorf("listing skills: %v", err)
		}
		for _, f := range found {
			names[strings.TrimSuffix(filepath.Base(f), ".md")] = true
		}
	}
	return sortedStrings(names), nil
}

// proposedSkills counts the held facts for each skill that doesn't exist yet.
func proposedSkills(bridge string) (map[string]int, error) {
	out := map[string]int{}
	dirs, err := readDirIfAny(filepath.Join(bridge, "skills", "proposed"))
	if err != nil {
		return nil, fmt.Errorf("listing skills/proposed: %v", err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := filesIn(filepath.Join(bridge, "skills", "proposed", d.Name()), ".yaml")
		if err != nil {
			return nil, fmt.Errorf("listing skills/proposed/%s: %v", d.Name(), err)
		}
		if len(files) > 0 {
			out[d.Name()] = len(files)
		}
	}
	return out, nil
}

// cmdSkill implements `ensign skill new <name> --description <text>` and
// `ensign skill discard <name>`, which settle facts held for a skill that doesn't exist.
func cmdSkill(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign skill new <name> --description <text> | ensign skill discard <name>")
	if len(args) < 2 || !validName(args[1]) {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if args[0] == "new" || args[0] == "discard" {
		deliverTopic(bridge, "skill", out)
	}
	name, held := args[1], proposedSkillDir(bridge, args[1])
	switch {
	case args[0] == "discard" && len(args) == 2:
		proposed, err := proposedSkills(bridge)
		if err != nil {
			return err
		}
		n := proposed[name]
		if n == 0 {
			return fmt.Errorf("no facts are held for %q", name)
		}
		if err := os.RemoveAll(held); err != nil {
			return err
		}
		fmt.Fprintf(out, "discarded %d fact(s) held for %s\n", n, name)
		return nil
	case args[0] == "new" && len(args) == 4 && args[2] == "--description" && strings.TrimSpace(args[3]) != "":
		bf, err := loadBridgeFile(bridge)
		if err != nil {
			return err
		}
		cp, err := controlPlaneDir(bridge, bf)
		if err != nil {
			return err
		}
		known, err := knownSkills(bridge, cp, "")
		if err != nil {
			return err
		}
		if contains(known, name) {
			return fmt.Errorf("skill %q already exists", name)
		}
		if windowsReserved(name) {
			return fmt.Errorf("skill name %q is a name Windows reserves, and the name becomes a committed folder", name)
		}
		body := fmt.Sprintf("---\ndescription: %s\n---\n\n# %s\n\nWhat agents have established about %s, curated from their reports.\n",
			yamlScalar(args[3]), name, name)
		files, err := filesIn(held, ".yaml")
		if err != nil {
			return fmt.Errorf("listing the facts held for %s: %v", name, err)
		}
		if err := writeFile(filepath.Join(bridge, "skills", name+".md"), body); err != nil {
			return err
		}
		for _, f := range files {
			dest := filepath.Join(pendingSkillDir(bridge, name), filepath.Base(f))
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			if err := os.Rename(f, dest); err != nil {
				return err
			}
		}
		os.Remove(held)
		fmt.Fprintf(out, "created skills/%s.md", name)
		if len(files) > 0 {
			fmt.Fprintf(out, "; %d held fact(s) are now pending. Run the skill-curate pipeline to fold them in", len(files))
		}
		fmt.Fprintln(out)
		return nil
	}
	return usage
}

// recordFacts saves the facts a stage reported, one file each, for the curator.
func recordFacts(r *RunFiles, d *Dispatch) {
	for i, line := range d.Fields["fact"] {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}
		skill := strings.TrimSpace(parts[0])
		if !usableSkillName(skill) {
			continue
		}
		f := map[string]string{"fact": strings.TrimSpace(parts[1]), "run": r.State.Run, "stage": d.Stage}
		if len(parts) == 3 {
			f["evidence"] = strings.TrimSpace(parts[2])
		}
		dir, note := pendingSkillDir(r.Snap.Bridge, skill), ""
		if !contains(r.Snap.KnownSkills, skill) {
			// No skill of that name exists, so curating it would reach no one. It waits
			// for the user to create the skill or discard the facts.
			dir, note = proposedSkillDir(r.Snap.Bridge, skill), " (no such skill; held for the user)"
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d.yaml", r.State.Run, d.ID, i+1))
		if err := writeYAML(path, f); err == nil {
			r.State.log("fact", d.Stage, d.ID, skill+": "+f["fact"]+note)
		}
	}
}

// usableSkillName reports whether name can name a skill's files: a valid name that Windows does not reserve.
func usableSkillName(name string) bool {
	return validName(name) && !windowsReserved(name)
}

// generatorItems resolves a parallel stage's list for this run.
func generatorItems(c *Config, over string, rebuild bool) ([]string, error) {
	switch over {
	case "pending-skills":
		dirs, err := readDirIfAny(filepath.Join(c.Bridge, "skills", "pending"))
		if err != nil {
			return nil, fmt.Errorf("listing skills/pending: %v", err)
		}
		var out []string
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			files, err := filesIn(filepath.Join(c.Bridge, "skills", "pending", d.Name()), ".yaml")
			if err != nil {
				return nil, fmt.Errorf("listing skills/pending/%s: %v", d.Name(), err)
			}
			if len(files) == 0 {
				continue
			}
			if !usableSkillName(d.Name()) {
				fmt.Fprintf(os.Stderr, "ensign: skills/pending/%s has facts but is not a usable skill name; skipped\n", d.Name())
				continue
			}
			out = append(out, d.Name())
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no skill has pending facts to curate")
		}
		return out, nil
	case "pending-documents":
		return pendingDocuments(c.Bridge)
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
func itemContext(r *RunFiles, s *Stage, item string, d *Dispatch) (string, error) {
	switch s.Parallel.Over {
	case "pending-skills":
		files, err := filesIn(pendingSkillDir(r.Snap.Bridge, item), ".yaml")
		if err != nil {
			return "", fmt.Errorf("listing the facts pending for %s: %v", item, err)
		}
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
			item, baseText, string(cur), strings.Join(facts, "\n---\n")), nil
	case "pending-documents":
		return documentContext(r.Snap.Bridge, item, d)
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
				ws := r.Snap.Workspace
				if s.Location != "" && strings.HasPrefix(s.Ref, "$WORKSPACE") {
					p, err := locationPathFor(r.Snap.Bridge, r.Snap.Project, s.Location)
					if err != nil {
						text += fmt.Sprintf("This source is code in location %s, which has no folder recorded on this machine, so it cannot be read. Ask the user where that code is, never guess, then record it with `ensign workspace %s --location %s <path>`.\n", s.Location, r.Snap.Project, s.Location)
						return text, nil
					}
					ws = p
				}
				text += "Read the code at " + resolveRef(ws, s.Ref) + " — the files a person would hold up as the project's style.\n"
			}
			return text, nil
		}
	}
	return "", nil
}
