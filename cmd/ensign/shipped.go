// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// shippedSettings holds the parts of a project.yaml that are compared between the code's
// shipped copy and the bridge's (bindings, pipelines, extensions, rules), decoded generically
// so formatting and comments don't count.
type shippedSettings struct {
	Bindings  map[string]any
	Pipelines map[string]any
	Use       []string
	Rules     []string
	// docsProblems is what a run would refuse in this file's own docs binding; it is not compared or recorded.
	docsProblems []string
}

const reviewedHeader = `# The shipped settings (the code's .starship/project.yaml) last reviewed for this project.
# Its bindings, pipelines, extensions and rules stay quiet in the drift report that 'ensign task open' prints.
# 'ensign project reviewed' writes this file; commit it.
`

func reviewedFile(bridge, project string) string {
	return filepath.Join(bridge, "projects", project, "shipped-reviewed.yaml")
}

// settingsBlock is the bindings, pipelines, use and rules of one settings file, with bindings and rules kept as nodes so their shape can be checked.
type settingsBlock struct {
	Bindings  yaml.Node      `yaml:"bindings"`
	Pipelines map[string]any `yaml:"pipelines"`
	Use       []string       `yaml:"use"`
	Rules     yaml.Node      `yaml:"rules"`
}

// readSettings reads a settings file; a missing file reads as empty with ok false.
func readSettings(path string) (s shippedSettings, ok bool, err error) {
	return readSettingsAs(path, true)
}

// readSettingsAs is readSettings with the bindings checked or only decoded. Only the reviewed record is read unchecked: a run never reads it and it is only compared for equality with shipped values, which are checked first, so an invalid recorded value silences nothing, and records written by older versions may hold such values.
func readSettingsAs(path string, check bool) (s shippedSettings, ok bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	var raw settingsBlock
	if err := yamlUnmarshal(data, &raw); err != nil {
		return s, true, fmt.Errorf("%s: %v", path, err)
	}
	s, err = decodeSettings(path, raw, check)
	return s, true, err
}

// bindingsMapping resolves a bindings node for use: an alias is followed one level, an absent or null value (an alias to null included) gives nil, and any other non-mapping is an error. bindingEntries is the one place that reads what is inside it, so run time and the drift report agree.
func bindingsMapping(n *yaml.Node) (*yaml.Node, error) {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == 0 || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: bindings must be a mapping", n.Line)
	}
	return n, nil
}

// bindingEntry is one named binding: its key node (for the line) and its value node, which may be an alias.
type bindingEntry struct {
	name string
	key  *yaml.Node
	val  *yaml.Node
}

// bindingEntries lists the bindings a bindings node declares, with merge keys expanded and names normalised: one leading @ is dropped, a later explicit key replaces an earlier one, an explicit key beats a merged one wherever it sits, and an earlier source in a merge list beats a later one. The order of the result is not significant. A cyclic merge is an error.
func bindingEntries(n *yaml.Node) ([]bindingEntry, error) {
	n, err := bindingsMapping(n)
	if err != nil || n == nil {
		return nil, err
	}
	return expandBindings(n, map[*yaml.Node]bool{})
}

// expandBindings returns the entries of mapping n with @-stripped names; a repeated name keeps the later explicit entry, and merge sources add only names not already present, so an explicit key beats a merged one wherever it sits and an earlier source in a list beats a later one. active holds the mappings being expanded: a mapping that contains itself is an error, as is a merge value that is not a mapping or a list of mappings. Nodes are not copied, so a key's Line stays its true line in the file. The order of the entries is not significant.
func expandBindings(n *yaml.Node, active map[*yaml.Node]bool) ([]bindingEntry, error) {
	if active[n] {
		return nil, fmt.Errorf("line %d: bindings merge contains itself", n.Line)
	}
	active[n] = true
	defer delete(active, n)
	var out []bindingEntry
	at := map[string]int{}
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
			merges = append(merges, v)
			continue
		}
		e := bindingEntry{strings.TrimPrefix(k.Value, "@"), k, v}
		if j, ok := at[e.name]; ok {
			out[j] = e
			continue
		}
		at[e.name] = len(out)
		out = append(out, e)
	}
	for _, m := range merges {
		if m.Kind == yaml.AliasNode {
			m = m.Alias
		}
		sources := []*yaml.Node{m}
		if m.Kind == yaml.SequenceNode {
			sources = m.Content
		}
		for _, src := range sources {
			if src.Kind == yaml.AliasNode {
				src = src.Alias
			}
			if src.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: a merge key needs a mapping or a list of mappings", src.Line)
			}
			merged, err := expandBindings(src, active)
			if err != nil {
				return nil, err
			}
			for _, e := range merged {
				if _, ok := at[e.name]; !ok {
					at[e.name] = len(out)
					out = append(out, e)
				}
			}
		}
	}
	return out, nil
}

// decodeAny decodes a node into any. yaml.v3 re-panics some malformed mappings (an unhashable merge key under a non-string key) rather than returning an error, so a panic from the decode becomes one.
func decodeAny(n *yaml.Node) (v any, err error) {
	err = guardedDecode(func() error { return n.Decode(&v) })
	return v, err
}

// bindingsOf decodes a bindings node with names normalised as Config.addBindings does, through bindingEntries, and refuses through checkBinding any value parseBinding refuses, except a binding with no variant for this machine (see checkBinding).
func bindingsOf(n *yaml.Node) (map[string]any, error) {
	return bindingsFrom(n, true)
}

// bindingsFrom is bindingsOf with the check optional; only the reviewed record is decoded unchecked.
func bindingsFrom(n *yaml.Node, check bool) (map[string]any, error) {
	entries, err := bindingEntries(n)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	for _, e := range entries {
		if check {
			if err := checkBinding(e.val, fmt.Sprintf("line %d", e.key.Line)); err != nil {
				return nil, err
			}
		}
		v, err := decodeAny(e.val)
		if err != nil {
			return nil, err
		}
		m[e.name] = v
	}
	return roundTrip(m)
}

// rulesOf decodes a rules node with the same normalisation as Config.addRules.
func rulesOf(path string, n *yaml.Node) ([]string, error) {
	c := &Config{}
	if err := c.addRules(n, path); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range c.Rules {
		out = append(out, r.Text)
	}
	return out, nil
}

// roundTrip marshals and re-decodes m so values compare the way the reviewed record
// reads them back (a decoded 2.0 is written as 2 and re-read as an int).
func roundTrip(m map[string]any) (map[string]any, error) {
	if len(m) == 0 {
		return m, nil
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := yamlUnmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func containsName(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// driftOf lists the shipped values that differ from the bridge's and are not in the reviewed record. A name in refused is reported with its reason instead, and the reviewed record does not silence it.
func driftOf(kind string, shipped, bridge, reviewed map[string]any, refused map[string]string) []string {
	names := make([]string, 0, len(shipped))
	for n := range shipped {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		v := shipped[n]
		b, inBridge := bridge[n]
		if inBridge && reflect.DeepEqual(v, b) {
			continue
		}
		if why, ok := refused[n]; ok {
			lines = append(lines, fmt.Sprintf("%s %s: a run would refuse this: %s", kind, n, why))
			continue
		}
		if r, ok := reviewed[n]; ok && reflect.DeepEqual(v, r) {
			continue
		}
		if inBridge {
			lines = append(lines, fmt.Sprintf("%s %s: changed", kind, n))
		} else {
			lines = append(lines, fmt.Sprintf("%s %s: not in the bridge's copy", kind, n))
		}
	}
	return lines
}

// decodeSettings turns the generic bindings/pipelines/use/rules of one settings block into shippedSettings.
func decodeSettings(path string, blk settingsBlock, check bool) (shippedSettings, error) {
	b, err := bindingsFrom(&blk.Bindings, check)
	if err != nil {
		return shippedSettings{}, fmt.Errorf("%s: %v", path, err)
	}
	p, err := roundTrip(blk.Pipelines)
	if err != nil {
		return shippedSettings{}, fmt.Errorf("%s: %v", path, err)
	}
	r, err := rulesOf(path, &blk.Rules)
	if err != nil {
		return shippedSettings{}, err
	}
	var docs []string
	if check {
		docs = docsProblemsOf(&blk.Bindings)
	}
	return shippedSettings{Bindings: b, Pipelines: p, Use: blk.Use, Rules: r, docsProblems: docs}, nil
}

// docsProblemsOf applies the @docs rules a run enforces to one file's docs binding. Only the shipped file is judged this way: a run judges the merged config, so a bridge layer may be overridden later. A binding with no variant for this machine, or one parseBinding refuses (bindingsFrom has already reported that), has no problem here.
func docsProblemsOf(n *yaml.Node) []string {
	entries, err := bindingEntries(n)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.name != "docs" {
			continue
		}
		b, err := parseBinding(e.val, fmt.Sprintf("line %d", e.key.Line))
		if err != nil {
			return nil
		}
		return docsBindingProblems(b)
	}
	return nil
}

// effectiveSettings is what the bridge holds for a location: the project's settings with the location's bindings laid over the project's, as sideBindings does at run time (the location's layer comes after the project's, so it wins), and the location's rules added to the project's.
func effectiveSettings(bridge, project, location string) (shippedSettings, error) {
	file := filepath.Join(bridge, "projects", project, "project.yaml")
	mine, _, err := readSettings(file)
	if err != nil {
		return mine, err
	}
	var pf struct {
		Locations map[string]struct {
			Bindings yaml.Node `yaml:"bindings"`
			Rules    yaml.Node `yaml:"rules"`
		} `yaml:"locations"`
	}
	if err := readYAML(file, &pf); err != nil {
		return mine, fmt.Errorf("%s: %v", file, err)
	}
	loc, ok := pf.Locations[location]
	if !ok {
		return mine, fmt.Errorf("project %s has no location %s", project, location)
	}
	override, err := bindingsOf(&loc.Bindings)
	if err != nil {
		return mine, fmt.Errorf("%s: %v", file, err)
	}
	locRules, err := rulesOf(file, &loc.Rules)
	if err != nil {
		return mine, err
	}
	merged := map[string]any{}
	for k, v := range mine.Bindings {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	mine.Bindings = merged
	for _, r := range locRules {
		if !containsName(mine.Rules, r) {
			mine.Rules = append(mine.Rules, r)
		}
	}
	return mine, nil
}

// readReviewed reads the reviewed record for the project, or for one location's entry when location is set.
func readReviewed(bridge, project, location string) (shippedSettings, error) {
	path := reviewedFile(bridge, project)
	if location == "" {
		s, _, err := readSettingsAs(path, false)
		return s, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return shippedSettings{}, nil
	}
	if err != nil {
		return shippedSettings{}, err
	}
	var raw struct {
		Locations map[string]settingsBlock `yaml:"locations"`
	}
	if err := yamlUnmarshal(data, &raw); err != nil {
		return shippedSettings{}, fmt.Errorf("%s: %v", path, err)
	}
	e := raw.Locations[location]
	return decodeSettings(path, e, false)
}

// shippedDrift lists what the code's shipped settings have that the bridge's copy lacks
// and the reviewed record doesn't cover. It is nil when the code ships no file.
// A non-empty location compares against that location's effective settings and reviewed entry.
// The bool is true when the shipped docs binding has a problem a run would refuse.
func shippedDrift(bridge, project, ws, location string) ([]string, bool, error) {
	shipped, ok, err := readSettings(filepath.Join(ws, filepath.FromSlash(shippedProjectFile)))
	if err != nil || !ok {
		return nil, false, err
	}
	var mine shippedSettings
	if location == "" {
		mine, _, err = readSettings(filepath.Join(bridge, "projects", project, "project.yaml"))
	} else {
		mine, err = effectiveSettings(bridge, project, location)
	}
	if err != nil {
		return nil, false, err
	}
	reviewed, err := readReviewed(bridge, project, location)
	if err != nil {
		return nil, false, err
	}
	var refused map[string]string
	if len(shipped.docsProblems) > 0 {
		refused = map[string]string{"docs": strings.Join(shipped.docsProblems, "; ")}
	}
	lines := driftOf("binding", shipped.Bindings, mine.Bindings, reviewed.Bindings, refused)
	lines = append(lines, driftOf("pipeline", shipped.Pipelines, mine.Pipelines, reviewed.Pipelines, nil)...)
	for _, u := range shipped.Use {
		if !containsName(mine.Use, u) && !containsName(reviewed.Use, u) {
			lines = append(lines, fmt.Sprintf("extension %s: not in the bridge's copy", u))
		}
	}
	for _, r := range shipped.Rules {
		if !containsName(mine.Rules, r) && !containsName(reviewed.Rules, r) {
			lines = append(lines, fmt.Sprintf("rule %q: not in the bridge's copy", r))
		}
	}
	return lines, len(shipped.docsProblems) > 0, nil
}

// printShippedDrift is the part of `task open` that reports shipped-settings drift; it never fails.
func printShippedDrift(bridge, project, ws, location string, out io.Writer) {
	lines, docsBad, err := shippedDrift(bridge, project, ws, location)
	if err != nil {
		if location != "" {
			fmt.Fprintf(out, "shipped    location %s: can't compare: %v\n", location, err)
		} else {
			fmt.Fprintf(out, "shipped    can't compare: %v\n", err)
		}
		return
	}
	if len(lines) == 0 {
		return
	}
	if location != "" {
		fmt.Fprintf(out, "shipped    location %s: the code's %s differs from the bridge's settings for location %s in projects/%s/project.yaml:\n", location, shippedProjectFile, location, project)
	} else {
		fmt.Fprintf(out, "shipped    the code's %s differs from projects/%s/project.yaml:\n", shippedProjectFile, project)
	}
	for _, l := range lines {
		fmt.Fprintf(out, "             %s\n", l)
	}
	if docsBad {
		fmt.Fprintf(out, "           Fix the docs binding in the code's %s first: `ensign project reviewed` refuses until then.\n", shippedProjectFile)
	} else if location != "" {
		fmt.Fprintf(out, "           Review these with the user, then run `ensign project reviewed %s --location %s` to mark them reviewed.\n", project, location)
	} else {
		fmt.Fprintf(out, "           Review these with the user, then run `ensign project reviewed %s` to mark them reviewed.\n", project)
	}
}

func settingsRecord(s shippedSettings) map[string]any {
	rec := map[string]any{}
	if len(s.Bindings) > 0 {
		rec["bindings"] = s.Bindings
	}
	if len(s.Pipelines) > 0 {
		rec["pipelines"] = s.Pipelines
	}
	if len(s.Use) > 0 {
		rec["use"] = s.Use
	}
	if len(s.Rules) > 0 {
		rec["rules"] = s.Rules
	}
	return rec
}

// cmdProjectReviewed records the code's current shipped settings as reviewed.
// For a project with locations it records each location (the task's, or the named one) whose folder is set here and ships a file.
func cmdProjectReviewed(arg, onlyLocation string, out io.Writer) error {
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	project, task, _ := strings.Cut(arg, "/")
	if _, err := os.Stat(filepath.Join(bridge, "projects", project, "project.yaml")); err != nil {
		return fmt.Errorf("no project %s", project)
	}
	locs, err := loadLocations(bridge, project)
	if err != nil {
		return err
	}
	path := reviewedFile(bridge, project)
	if len(locs.Names()) == 0 {
		if onlyLocation != "" {
			return fmt.Errorf("project %s has no locations; --location does not apply", project)
		}
		ws, err := workspaceFor(bridge, project, task)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s does not exist", ws)
		}
		shipped, ok, err := readSettings(filepath.Join(ws, filepath.FromSlash(shippedProjectFile)))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("the code in %s ships no %s", ws, shippedProjectFile)
		}
		if len(shipped.docsProblems) > 0 {
			return fmt.Errorf("%s: a run would refuse this: %s", filepath.Join(ws, filepath.FromSlash(shippedProjectFile)), strings.Join(shipped.docsProblems, "; "))
		}
		data, err := yaml.Marshal(settingsRecord(shipped))
		if err != nil {
			return err
		}
		if err := writeFile(path, reviewedHeader+string(data)); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s; commit it\n", path)
		return nil
	}

	names := locs.Names()
	if onlyLocation != "" {
		if _, declared := locs.deps[onlyLocation]; !declared {
			return fmt.Errorf("project %s has no location %s", project, onlyLocation)
		}
		names = []string{onlyLocation}
	} else if task != "" {
		taskFile := filepath.Join(bridge, "projects", project, "tasks", task, "task.md")
		front, _, err := splitFrontmatter(taskFile)
		if err != nil {
			return err
		}
		var tf TaskFile
		if err := decodeFrontmatter(front, &tf); err != nil {
			return fmt.Errorf("%s: %v", taskFile, err)
		}
		if len(tf.Locations) > 0 {
			names = tf.Locations
		}
	}
	entries := map[string]any{}
	var recorded, skipped []string
	for _, name := range names {
		folder, err := locationPathFor(bridge, project, name)
		if err == nil {
			shipped, ok, rerr := readSettings(filepath.Join(folder, filepath.FromSlash(shippedProjectFile)))
			if rerr != nil {
				return rerr
			}
			if ok && len(shipped.docsProblems) > 0 {
				return fmt.Errorf("%s: a run would refuse this: %s", filepath.Join(folder, filepath.FromSlash(shippedProjectFile)), strings.Join(shipped.docsProblems, "; "))
			}
			if ok {
				entries[name] = settingsRecord(shipped)
				recorded = append(recorded, name)
				continue
			}
			err = fmt.Errorf("the code in %s ships no %s", folder, shippedProjectFile)
		}
		if onlyLocation != "" {
			return err
		}
		skipped = append(skipped, fmt.Sprintf("%s: %v", name, err))
	}
	if len(recorded) == 0 {
		return fmt.Errorf("nothing to record for project %s; no location has a folder here that ships %s:\n  %s", project, shippedProjectFile, strings.Join(skipped, "\n  "))
	}

	rec := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yamlUnmarshal(data, &rec); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if rec == nil {
		rec = map[string]any{}
	}
	all, _ := rec["locations"].(map[string]any)
	if all == nil {
		all = map[string]any{}
	}
	for name, e := range entries {
		all[name] = e
	}
	rec["locations"] = all
	data, err := yaml.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeFile(path, reviewedHeader+string(data)); err != nil {
		return err
	}
	for _, name := range recorded {
		fmt.Fprintf(out, "recorded location %s\n", name)
	}
	for _, s := range skipped {
		fmt.Fprintf(out, "skipped location %s\n", s)
	}
	fmt.Fprintf(out, "wrote %s; commit it\n", path)
	return nil
}
