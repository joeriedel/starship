// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	goalsFence = "starship-goals"
	itemsFence = "starship-items"
	// intakeKeepDays is how many days of reader/intake/<date>/ folders a briefing keeps.
	intakeKeepDays = 30
)

// secretaryGoal is one goal in notes/secretary/goals.md. Target is "" when the goal has none.
type secretaryGoal struct {
	ID, Title, Target, Status string
	Line                      int
}

// secretaryItem is one item in notes/secretary/items.md. Goal and Due are "" when the item has none.
type secretaryItem struct {
	ID, Title, Goal, Status, Due string
	Sources, Tasks               []string
	Line                         int
}

var (
	goalIDRe     = regexp.MustCompile(`^G[1-9][0-9]*$`)
	itemIDRe     = regexp.MustCompile(`^I[1-9][0-9]*$`)
	dateFolderRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

var itemStatuses = []string{"open", "waiting", "done", "dropped"}

func secretaryDir(bridge string) string { return filepath.Join(bridge, "notes", "secretary") }

// secretaryBlock finds the one fenced block called name in the Markdown file at path. found is false when the file doesn't exist.
func secretaryBlock(path, name string) (fb fencedBlock, found bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fb, false, nil
	}
	if err != nil {
		return fb, false, err
	}
	blocks, err := fencedBlocks(strings.Split(string(data), "\n"), name)
	if err != nil {
		return fb, false, fmt.Errorf("%s: %v", path, err)
	}
	if len(blocks) != 1 {
		return fb, false, fmt.Errorf("%s: it must hold exactly one %s block, and it holds %d", path, name, len(blocks))
	}
	return blocks[0], true, nil
}

// blockProblem is a refusal that names the file and the line within it.
type blockProblem struct {
	path string
	line int
}

func (b blockProblem) errorf(format string, a ...any) error {
	return fmt.Errorf("%s: line %d: %s", b.path, b.line, fmt.Sprintf(format, a...))
}

// blockEntries decodes a block's body as a list of mappings, each as its keys' value nodes. Lines are the lines of the Markdown file, not of the block.
func blockEntries(path string, fb fencedBlock, name string, allowed []string) ([]map[string]*yaml.Node, []int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fb.body), &doc); err != nil {
		msg := yamlLineRe.ReplaceAllStringFunc(err.Error(), func(s string) string {
			n, _ := strconv.Atoi(strings.TrimPrefix(s, "line "))
			return "line " + strconv.Itoa(fb.first+1+n)
		})
		return nil, nil, fmt.Errorf("%s: %s block is not valid YAML: %s", path, name, strings.TrimPrefix(msg, "yaml: "))
	}
	if doc.Kind == 0 {
		return nil, nil, nil
	}
	shiftLines(&doc, fb.first+1)
	at := func(n *yaml.Node) blockProblem { return blockProblem{path, n.Line} }
	root := doc.Content[0]
	if root.Kind != yaml.SequenceNode {
		return nil, nil, at(root).errorf("the %s block must be a list", name)
	}
	var entries []map[string]*yaml.Node
	var lines []int
	for _, n := range root.Content {
		if n.Kind != yaml.MappingNode {
			return nil, nil, at(n).errorf("each entry in the %s block must be a mapping of keys", name)
		}
		m := map[string]*yaml.Node{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if !containsString(allowed, k.Value) {
				return nil, nil, at(k).errorf("unknown key %q (the keys are %s)", k.Value, strings.Join(allowed, ", "))
			}
			if _, dup := m[k.Value]; dup {
				return nil, nil, at(k).errorf("key %q appears twice", k.Value)
			}
			m[k.Value] = n.Content[i+1]
		}
		entries = append(entries, m)
		lines = append(lines, at(n).line)
	}
	return entries, lines, nil
}

func containsString(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// textField reads a key's non-empty text value; required says whether its absence is a refusal.
func textField(path string, m map[string]*yaml.Node, key string, line int, required bool) (string, error) {
	n, ok := m[key]
	if !ok {
		if required {
			return "", blockProblem{path, line}.errorf("missing required key %q", key)
		}
		return "", nil
	}
	if n.Kind != yaml.ScalarNode || strings.TrimSpace(n.Value) == "" {
		return "", blockProblem{path, n.Line}.errorf("%q must be non-empty text", key)
	}
	return strings.TrimSpace(n.Value), nil
}

func dateField(path string, m map[string]*yaml.Node, key string, line int) (string, error) {
	v, err := textField(path, m, key, line, false)
	if err != nil || v == "" {
		return v, err
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", blockProblem{path, m[key].Line}.errorf("%q must be a date as YYYY-MM-DD, got %q", key, v)
	}
	return v, nil
}

func listField(path string, m map[string]*yaml.Node, key string, line int, required bool) ([]string, error) {
	n, ok := m[key]
	if !ok {
		if required {
			return nil, blockProblem{path, line}.errorf("missing required key %q", key)
		}
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode || (required && len(n.Content) == 0) {
		return nil, blockProblem{path, n.Line}.errorf("%q must be a list of text, with at least one entry", key)
	}
	var out []string
	for _, e := range n.Content {
		if e.Kind != yaml.ScalarNode || strings.TrimSpace(e.Value) == "" {
			return nil, blockProblem{path, e.Line}.errorf("each entry of %q must be non-empty text", key)
		}
		out = append(out, strings.TrimSpace(e.Value))
	}
	return out, nil
}

// shiftLines makes every node's Line the line in the Markdown file, given the block's first fence line.
func shiftLines(n *yaml.Node, by int) {
	n.Line += by
	for _, c := range n.Content {
		shiftLines(c, by)
	}
}

// parseGoals reads notes/secretary/goals.md. A file that doesn't exist has no goals.
func parseGoals(bridge string) ([]secretaryGoal, error) {
	path := filepath.Join(secretaryDir(bridge), "goals.md")
	fb, found, err := secretaryBlock(path, goalsFence)
	if err != nil || !found {
		return nil, err
	}
	entries, lines, err := blockEntries(path, fb, goalsFence, []string{"id", "title", "target", "status"})
	if err != nil {
		return nil, err
	}
	var goals []secretaryGoal
	seen := map[string]bool{}
	for i, m := range entries {
		g := secretaryGoal{Line: lines[i]}
		if g.ID, err = textField(path, m, "id", g.Line, true); err != nil {
			return nil, err
		}
		if !goalIDRe.MatchString(g.ID) {
			return nil, blockProblem{path, m["id"].Line}.errorf("goal id %q must be G followed by a number, such as G1", g.ID)
		}
		if seen[g.ID] {
			return nil, blockProblem{path, m["id"].Line}.errorf("goal id %s appears twice", g.ID)
		}
		seen[g.ID] = true
		if g.Title, err = textField(path, m, "title", g.Line, true); err != nil {
			return nil, err
		}
		if g.Status, err = textField(path, m, "status", g.Line, true); err != nil {
			return nil, err
		}
		if g.Target, err = dateField(path, m, "target", g.Line); err != nil {
			return nil, err
		}
		goals = append(goals, g)
	}
	return goals, nil
}

// parseItems reads notes/secretary/items.md and checks each item's goal against goals. A file that doesn't exist has no items.
func parseItems(bridge string, goals []secretaryGoal) ([]secretaryItem, error) {
	path := filepath.Join(secretaryDir(bridge), "items.md")
	fb, found, err := secretaryBlock(path, itemsFence)
	if err != nil || !found {
		return nil, err
	}
	entries, lines, err := blockEntries(path, fb, itemsFence, []string{"id", "title", "goal", "status", "due", "sources", "tasks", "log"})
	if err != nil {
		return nil, err
	}
	goalIDs := map[string]bool{}
	for _, g := range goals {
		goalIDs[g.ID] = true
	}
	var items []secretaryItem
	seen := map[string]bool{}
	for i, m := range entries {
		it := secretaryItem{Line: lines[i]}
		if it.ID, err = textField(path, m, "id", it.Line, true); err != nil {
			return nil, err
		}
		if !itemIDRe.MatchString(it.ID) {
			return nil, blockProblem{path, m["id"].Line}.errorf("item id %q must be I followed by a number, such as I1", it.ID)
		}
		if seen[it.ID] {
			return nil, blockProblem{path, m["id"].Line}.errorf("item id %s appears twice", it.ID)
		}
		seen[it.ID] = true
		if it.Title, err = textField(path, m, "title", it.Line, true); err != nil {
			return nil, err
		}
		if it.Status, err = textField(path, m, "status", it.Line, true); err != nil {
			return nil, err
		}
		if !containsString(itemStatuses, it.Status) {
			return nil, blockProblem{path, m["status"].Line}.errorf("status %q of %s must be one of %s", it.Status, it.ID, strings.Join(itemStatuses, ", "))
		}
		if it.Goal, err = textField(path, m, "goal", it.Line, false); err != nil {
			return nil, err
		}
		if it.Goal != "" && !goalIDs[it.Goal] {
			return nil, blockProblem{path, m["goal"].Line}.errorf("%s names goal %q, which goals.md does not define", it.ID, it.Goal)
		}
		if it.Due, err = dateField(path, m, "due", it.Line); err != nil {
			return nil, err
		}
		if it.Sources, err = listField(path, m, "sources", it.Line, true); err != nil {
			return nil, err
		}
		if it.Tasks, err = listField(path, m, "tasks", it.Line, false); err != nil {
			return nil, err
		}
		for _, t := range it.Tasks {
			if p := strings.Split(t, "/"); len(p) != 2 || p[0] == "" || p[1] == "" {
				return nil, blockProblem{path, m["tasks"].Line}.errorf("task %q of %s must be project/task", t, it.ID)
			}
		}
		if n, ok := m["log"]; ok {
			if err := checkItemLog(path, it.ID, n); err != nil {
				return nil, err
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// checkItemLog checks an item's log: a list of entries with a date, the change and its source.
func checkItemLog(path, id string, n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return blockProblem{path, n.Line}.errorf("the log of %s must be a list of entries with date, change and source", id)
	}
	for _, e := range n.Content {
		if e.Kind != yaml.MappingNode {
			return blockProblem{path, e.Line}.errorf("each log entry of %s must have date, change and source", id)
		}
		m := map[string]*yaml.Node{}
		for i := 0; i+1 < len(e.Content); i += 2 {
			k := e.Content[i]
			if !containsString([]string{"date", "change", "source"}, k.Value) {
				return blockProblem{path, k.Line}.errorf("unknown key %q in a log entry of %s (the keys are date, change, source)", k.Value, id)
			}
			m[k.Value] = e.Content[i+1]
		}
		for _, k := range []string{"change", "source"} {
			if _, err := textField(path, m, k, e.Line, true); err != nil {
				return err
			}
		}
		if _, ok := m["date"]; !ok {
			return blockProblem{path, e.Line}.errorf("missing required key %q", "date")
		}
		if _, err := dateField(path, m, "date", e.Line); err != nil {
			return err
		}
	}
	return nil
}

// checkSecretaryFiles refuses a goals.md or items.md that doesn't parse. Missing files are fine.
func checkSecretaryFiles(bridge string) error {
	goals, err := parseGoals(bridge)
	if err != nil {
		return err
	}
	_, err = parseItems(bridge, goals)
	return err
}

// linkedItem is one listed line of a linked secretary item and the item's status.
type linkedItem struct{ Line, Status string }

// linkedItemLines returns one line per secretary item whose tasks: names task (project/task), with the item's id, title, status, due date and goal. The error is a goals.md or items.md that doesn't parse; missing files give no lines.
func linkedItemLines(bridge, task string) ([]linkedItem, error) {
	goals, err := parseGoals(bridge)
	if err != nil {
		return nil, err
	}
	items, err := parseItems(bridge, goals)
	if err != nil {
		return nil, err
	}
	var lines []linkedItem
	for _, it := range items {
		if !containsString(it.Tasks, task) {
			continue
		}
		line := fmt.Sprintf("%s %q [%s]", it.ID, it.Title, it.Status)
		if it.Due != "" {
			line += " due " + it.Due
		}
		for _, g := range goals {
			if g.ID == it.Goal {
				line += fmt.Sprintf(" goal %s %q", g.ID, g.Title)
			}
		}
		lines = append(lines, linkedItem{line, it.Status})
	}
	return lines, nil
}

// missingItemTasks returns one report per tasks: entry of an item that names a task without a task.md.
func missingItemTasks(bridge string) ([]string, error) {
	goals, err := parseGoals(bridge)
	if err != nil {
		return nil, err
	}
	items, err := parseItems(bridge, goals)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, it := range items {
		for _, t := range it.Tasks {
			if _, err := os.Stat(filepath.Join(taskDirOf(bridge, t), "task.md")); err != nil {
				out = append(out, fmt.Sprintf("item %s (%q) lists task %s in tasks:, but there is no such task", it.ID, it.Title, t))
			}
		}
	}
	return out, nil
}

// pruneIntake removes the reader/intake/<YYYY-MM-DD>/ folders dated more than intakeKeepDays before now; anything else in reader/intake is left alone.
func pruneIntake(bridge string, now time.Time) error {
	dir := filepath.Join(bridge, "reader", "intake")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -intakeKeepDays).Format("2006-01-02")
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !dateFolderRe.MatchString(name) {
			continue
		}
		if _, err := time.Parse("2006-01-02", name); err != nil || name >= cutoff {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
