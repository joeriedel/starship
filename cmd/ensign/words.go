// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HelpEntry is one plain-language request a pipeline or extension answers to.
// Run is the command that carries it out. The session is told it; help never shows it.
type HelpEntry struct {
	Say  string `yaml:"say" json:"say"`
	Does string `yaml:"does" json:"does"`
	Run  string `yaml:"run" json:"run,omitempty"`
}

// requestCommands is what the session is told, after its manual, about carrying out the
// requests that pipelines and extensions answer to.
func requestCommands(bridge string) string {
	var lines []string
	extra := extraHelp(bridge)
	for _, group := range sortedKeys(extra) {
		for _, e := range extra[group] {
			if e.Run != "" {
				lines = append(lines, fmt.Sprintf("- %s: `%s`", e.Say, e.Run))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Requests this bridge answers, and the command for each\n\n" +
		"A command that starts a run needs the user's go, like any run.\n\n" + strings.Join(lines, "\n") + "\n"
}

var fixedHelp = []struct {
	group   string
	entries []HelpEntry
}{
	{"Getting around", []HelpEntry{
		{`"open <project> / <task>"`, "pick up a task where it left off", ""},
		{`"new task <name> in <project>"`, "start a task in the project's code (or name a different location)", ""},
		{`"new project <name>, its code is in <folder>"`, "add a project to this bridge", ""},
		{"status", "where the current task and run stand", ""},
		{"help <project or task>", "more on one project or task", ""},
	}},
	{"Doing the work", []HelpEntry{
		{"describe what you want done", "I plan it as a run; type go to start it", ""},
		{"go", "approve the plan I just showed you", ""},
	}},
	{"Housekeeping", []HelpEntry{
		{`"update the bridge"`, "move to the latest starship release", "ensign update"},
		{`"improve starship"`, "work on starship as a project; the change ends as a pull request", ""},
	}},
}

// sessionWord answers help and status without the model. It returns "" for anything else.
func sessionWord(bridge, session, prompt string) string {
	p := strings.TrimSpace(prompt)
	switch {
	case p == "help":
		return helpText(bridge, session)
	case strings.HasPrefix(p, "help "):
		return helpTopic(bridge, strings.TrimSpace(p[5:]))
	case p == "status":
		return statusText(bridge, session)
	}
	return ""
}

func sessionTask(bridge, session string) string {
	data, _ := os.ReadFile(sessionFile(bridge, session) + ".task")
	return strings.TrimSpace(string(data))
}

func header(bridge, session string) string {
	h := "Bridge: " + filepath.Base(bridge)
	if t := sessionTask(bridge, session); t != "" {
		p, task, _ := strings.Cut(t, "/")
		h += " · Project: " + p + " · Task: " + task
	}
	return h
}

// runLine describes the session's run in plain words.
func runLine(bridge, session string) string {
	r, err := openSessionRun(bridge, session)
	if err != nil || r == nil {
		return ""
	}
	defer r.close()
	st := r.State
	m := &machine{snap: r.Snap, st: st}
	switch {
	case st.Status == "pending":
		return "A run is planned and waiting for you to type go."
	case st.terminal():
		return fmt.Sprintf("Last run: %s (%s).", st.Status, st.Reason)
	}
	lead := r.Snap.Pipeline.Stages[0]
	sc := st.Scopes[lead.ID]
	if sc == nil || sc.Stage == "" {
		return "Working: starting the run."
	}
	s := m.stage(sc.Stage)
	line := "Working on: " + s.ID
	if s.Parallel != nil {
		done := 0
		for _, id := range sc.Branches {
			if d := st.dispatch(id); d != nil && d.Done {
				done++
			}
		}
		line += fmt.Sprintf(", %d of %d reviewers done", done, len(s.Dimensions))
	}
	var parts []string
	for _, k := range sortedKeys(r.Snap.Pipeline.Budgets) {
		parts = append(parts, fmt.Sprintf("%d of %d %s", st.Spent[k], r.Snap.Pipeline.Budgets[k], budgetWord(k)))
	}
	return line + " · " + strings.Join(parts, " · ")
}

// seenLine says how long ago the live run's lead and its agents last made a hook call.
func seenLine(bridge, session string) string {
	r, err := openSessionRun(bridge, session)
	if err != nil || r == nil {
		return ""
	}
	defer r.close()
	if r.State.terminal() || r.State.Status == "pending" {
		return ""
	}
	var lead, any time.Time
	for _, d := range r.State.Dispatches {
		t, err := time.Parse(time.RFC3339, d.LastSeen)
		if err != nil {
			continue
		}
		if t.After(any) {
			any = t
		}
		if d.Stage == r.Snap.Pipeline.Stages[0].ID && t.After(lead) {
			lead = t
		}
	}
	ago := func(t time.Time) string {
		if t.IsZero() {
			return "not yet seen"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	}
	if !r.Snap.Pipeline.Stages[0].isLead() {
		return fmt.Sprintf("Last activity: %s.", ago(any))
	}
	return fmt.Sprintf("Lead last seen: %s. Last activity: %s.", ago(lead), ago(any))
}

func budgetWord(b string) string {
	switch b {
	case "loop":
		return "passes used this cycle"
	case "test":
		return "test cycles used"
	}
	return b + " used"
}

func helpText(bridge, session string) string {
	var b strings.Builder
	b.WriteString(header(bridge, session) + "\n")
	if l := runLine(bridge, session); l != "" {
		b.WriteString(l + "\n")
	}
	groups := map[string][]HelpEntry{}
	var order []string
	for _, g := range fixedHelp {
		groups[g.group] = append(groups[g.group], g.entries...)
		order = append(order, g.group)
	}
	for group, entries := range extraHelp(bridge) {
		if _, ok := groups[group]; !ok {
			order = append(order[:len(order)-1], group, order[len(order)-1])
		}
		groups[group] = append(groups[group], entries...)
	}
	for _, g := range order {
		b.WriteString("\n" + g + "\n")
		for _, e := range groups[g] {
			fmt.Fprintf(&b, "  %-44s %s\n", e.Say, e.Does)
		}
	}
	if projects := projectList(bridge); projects != "" {
		b.WriteString("\nProjects: " + projects + "\n")
	}
	return b.String()
}

// extraHelp gathers help entries from pipelines and extensions, by group.
func extraHelp(bridge string) map[string][]HelpEntry {
	out := map[string][]HelpEntry{}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return out
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(cp, "pipelines", "*.yaml"))
	for _, dir := range extensionDirs(bridge, cp) {
		more, _ := filepath.Glob(filepath.Join(dir, "pipelines", "*.yaml"))
		files = append(files, more...)
	}
	for _, f := range files {
		var p struct {
			Help []HelpEntry `yaml:"help"`
		}
		if readYAML(f, &p) == nil {
			out["Doing the work"] = append(out["Doing the work"], p.Help...)
		}
	}
	for _, dir := range extensionDirs(bridge, cp) {
		var x struct {
			Name string      `yaml:"name"`
			Help []HelpEntry `yaml:"help"`
		}
		if readYAML(filepath.Join(dir, "extension.yaml"), &x) == nil && len(x.Help) > 0 {
			group := strings.ToUpper(x.Name[:1]) + strings.ReplaceAll(x.Name[1:], "-", " ")
			out[group] = append(out[group], x.Help...)
		}
	}
	return out
}

func projectList(bridge string) string {
	dirs, _ := os.ReadDir(filepath.Join(bridge, "projects"))
	var parts []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		tasks, _ := os.ReadDir(filepath.Join(bridge, "projects", d.Name(), "tasks"))
		n := 0
		for _, t := range tasks {
			if t.IsDir() {
				n++
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%d task%s)", d.Name(), n, plural(n)))
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func helpTopic(bridge, topic string) string {
	topic = strings.ReplaceAll(strings.ReplaceAll(topic, " / ", "/"), " ", "")
	if p, t, ok := strings.Cut(topic, "/"); ok {
		return taskSummary(bridge, p, t)
	}
	if _, err := os.Stat(filepath.Join(bridge, "projects", topic, "project.yaml")); err == nil {
		return projectSummary(bridge, topic)
	}
	dirs, _ := os.ReadDir(filepath.Join(bridge, "projects"))
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(bridge, "projects", d.Name(), "tasks", topic, "task.md")); err == nil {
			return taskSummary(bridge, d.Name(), topic)
		}
	}
	return fmt.Sprintf("No project or task called %q. Type help for what's available.", topic)
}

func projectSummary(bridge, project string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project %s\n", project)
	tasks, _ := os.ReadDir(filepath.Join(bridge, "projects", project, "tasks"))
	if len(tasks) == 0 {
		b.WriteString("\nNo tasks yet.\n")
	}
	for _, t := range tasks {
		if !t.IsDir() {
			continue
		}
		_, body, err := splitFrontmatter(filepath.Join(bridge, "projects", project, "tasks", t.Name(), "task.md"))
		status := "(no task.md)"
		if err == nil {
			status = firstLine(section(string(body), "Status"))
		}
		fmt.Fprintf(&b, "  %-24s %s\n", t.Name(), status)
	}
	return b.String()
}

func taskSummary(bridge, project, task string) string {
	_, body, err := splitFrontmatter(filepath.Join(bridge, "projects", project, "tasks", task, "task.md"))
	if err != nil {
		return fmt.Sprintf("No task %s/%s.", project, task)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s / %s\n", project, task)
	for _, sec := range []string{"Goal", "Status", "Outstanding"} {
		if t := section(string(body), sec); t != "" {
			fmt.Fprintf(&b, "\n%s\n%s\n", sec, indent(t))
		}
	}
	return b.String()
}

func statusText(bridge, session string) string {
	var b strings.Builder
	b.WriteString(header(bridge, session) + "\n")
	if l := runLine(bridge, session); l != "" {
		b.WriteString(l + "\n")
		if l := seenLine(bridge, session); l != "" {
			b.WriteString(l + "\n")
		}
	} else {
		b.WriteString("No run in this session.\n")
	}
	if t := sessionTask(bridge, session); t != "" {
		p, task, _ := strings.Cut(t, "/")
		b.WriteString("\n" + taskSummary(bridge, p, task))
	} else {
		b.WriteString("\nNo task open. Say \"open <project> / <task>\".\n")
	}
	if held := proposedSkills(bridge); len(held) > 0 {
		b.WriteString("\nAgents reported facts about technologies with no skill yet:\n")
		for _, name := range sortedKeys(held) {
			fmt.Fprintf(&b, "  %s: %d fact%s. Say \"make a %s skill\" or \"discard the %s facts\".\n",
				name, held[name], plural(held[name]), name, name)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// controlPlaneEdit refuses edits to the control plane from a bridge session.
func controlPlaneEdit(bridge string, in *HookInput) string {
	path, _ := in.ToolInput["file_path"].(string)
	if path == "" {
		path, _ = in.ToolInput["notebook_path"].(string)
	}
	if path == "" {
		return ""
	}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return ""
	}
	roots := []string{filepath.Join(bridge, ".starship", "control-plane")}
	if cp, err := controlPlaneDir(bridge, bf); err == nil {
		roots = append(roots, cp)
	}
	abs, _ := filepath.Abs(path)
	for _, root := range roots {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if rel, err := filepath.Rel(root, resolveExisting(abs)); err == nil && !strings.HasPrefix(rel, "..") {
			return "this is the bridge's pinned copy of starship, which isn't edited. To change starship, work on a starship checkout as a project task; the change lands as a pull request."
		}
	}
	return ""
}

// resolveExisting follows symlinks in the longest existing prefix of a path.
func resolveExisting(p string) string {
	dir, rest := p, ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func sortedStrings(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
