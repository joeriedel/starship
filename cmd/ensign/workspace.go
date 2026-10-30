// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Where a project's code is differs per machine, so it is never written in the bridge's
// committed files. Each machine keeps its own machine.yaml at the bridge's root,
// gitignored, mapping a project (or a task with a checkout of its own) to a folder. A
// machine that has no entry yet asks the user once, the first time it needs one.

// Machine is this machine's settings for a bridge.
type Machine struct {
	Workspaces map[string]string `yaml:"workspaces"` // "<project>" or "<project>/<task>" -> folder
}

const machineHeader = `# Where this machine keeps each project's code. Every machine has its own copy of this
# file; it is never committed. A task with a checkout of its own is listed as
# <project>/<task>.
`

func machineFile(bridge string) string { return filepath.Join(bridge, "machine.yaml") }

func loadMachine(bridge string) Machine {
	var m Machine
	readYAML(machineFile(bridge), &m)
	if m.Workspaces == nil {
		m.Workspaces = map[string]string{}
	}
	return m
}

func saveMachine(bridge string, m Machine) error {
	var b strings.Builder
	b.WriteString(machineHeader + "\nworkspaces:\n")
	keys := make([]string, 0, len(m.Workspaces))
	for k := range m.Workspaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", k, yamlScalar(m.Workspaces[k]))
	}
	return writeFile(machineFile(bridge), b.String())
}

// errNoWorkspace is a project (or task) this machine has no folder recorded for.
type errNoWorkspace struct{ key, project string }

func (e errNoWorkspace) Error() string {
	return fmt.Sprintf("this machine has no folder recorded for %s; ask the user where its code is on this machine, never guess, then record it with `ensign workspace %s <path>`",
		e.project, e.key)
}

// workspaceFor is where a task's code is on this machine: the task's own entry, or its
// project's. With task "" it is the project's.
func workspaceFor(bridge, project, task string) (string, error) {
	m := loadMachine(bridge)
	if task != "" {
		if ws := m.Workspaces[project+"/"+task]; ws != "" {
			return filepath.FromSlash(ws), nil
		}
	}
	if ws := m.Workspaces[project]; ws != "" {
		return filepath.FromSlash(ws), nil
	}
	return "", errNoWorkspace{key: project, project: "project " + project}
}

// setWorkspace records where a project's (or task's) code is on this machine.
func setWorkspace(bridge, key, path string) (string, error) {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder on this machine; ask the user where the code is, never guess", abs)
	}
	m := loadMachine(bridge)
	m.Workspaces[key] = filepath.ToSlash(abs)
	return abs, saveMachine(bridge, m)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// cmdWorkspace implements `ensign workspace <project>[/<task>] [<path>]`: record where the
// code is on this machine, or show it.
func cmdWorkspace(args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: ensign workspace <project>[/<task>] [<path>]")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	project, task, _ := strings.Cut(args[0], "/")
	if !validName(project) || (task != "" && !validName(task)) {
		return fmt.Errorf("name it as <project> or <project>/<task>")
	}
	if _, err := os.Stat(filepath.Join(bridge, "projects", project, "project.yaml")); err != nil {
		return fmt.Errorf("no project %s", project)
	}
	if len(args) == 1 {
		ws, err := workspaceFor(bridge, project, task)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, filepath.ToSlash(ws))
		return nil
	}
	abs, err := setWorkspace(bridge, args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s is at %s on this machine\n", args[0], filepath.ToSlash(abs))
	return nil
}

// Before workspaces were per machine, project.yaml and task.md held a `workspace:` line,
// which could start with $PROJECTS, a root kept in .starship/machine.yaml.
// The comment `ensign project new` wrote above it goes with it.
var workspaceLine = regexp.MustCompile(`(?m)^(# Where this project's code is\. New tasks use it unless they name a different location\.\n)?workspace:[^\n]*\n?`)

func oldMachineFile(bridge string) string {
	return filepath.Join(bridge, ".starship", "machine.yaml")
}

// migrateWorkspaces moves workspace paths out of the bridge's committed files into this
// machine's machine.yaml, and makes sure machine.yaml is gitignored. It says what it
// changed, or "" when there was nothing to do.
func migrateWorkspaces(bridge string) string {
	var old struct {
		Projects string `yaml:"projects"`
	}
	readYAML(oldMachineFile(bridge), &old)
	expand := func(ws string) string {
		rest, ok := strings.CutPrefix(ws, "$PROJECTS")
		if !ok {
			return ws
		}
		if old.Projects == "" {
			return ""
		}
		return filepath.ToSlash(filepath.Join(old.Projects, strings.TrimPrefix(rest, "/")))
	}
	// What was already uncommitted is the user's; only the files this alone changed are
	// committed.
	before := map[string]bool{}
	if isGit(bridge) {
		st, _ := gitOut(bridge, "status", "--porcelain")
		for _, line := range strings.Split(st, "\n") {
			if len(line) > 3 {
				before[strings.Trim(line[3:], `"`)] = true
			}
		}
	}
	m := loadMachine(bridge)
	added := false
	var changed []string
	var stripped []string
	strip := func(path string) { stripped = append(stripped, path) }
	projects, _ := filepath.Glob(filepath.Join(bridge, "projects", "*", "project.yaml"))
	for _, pf := range projects {
		project := filepath.Base(filepath.Dir(pf))
		var l Layer
		if readYAML(pf, &l) != nil || l.Workspace == "" {
			continue
		}
		if m.Workspaces[project] == "" {
			ws := expand(l.Workspace)
			if ws == "" {
				continue // asked for the first time it is needed; the line goes then
			}
			m.Workspaces[project], added = ws, true
		}
		strip(pf)
	}
	tasks, _ := filepath.Glob(filepath.Join(bridge, "projects", "*", "tasks", "*", "task.md"))
	for _, tf := range tasks {
		task := filepath.Base(filepath.Dir(tf))
		project := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(tf))))
		front, _, err := splitFrontmatter(tf)
		var l Layer
		if err != nil || yamlUnmarshal(front, &l) != nil || l.Workspace == "" {
			continue
		}
		key := project + "/" + task
		if m.Workspaces[key] == "" {
			ws := expand(l.Workspace)
			if ws == "" {
				if m.Workspaces[project] == "" {
					continue
				}
				ws = m.Workspaces[project] // the root is gone; the project's folder is the best answer
			}
			if ws != m.Workspaces[project] {
				m.Workspaces[key], added = ws, true
			}
		}
		strip(tf)
	}
	for _, sf := range mustGlob(filepath.Join(bridge, "projects", "*", "style", "sources.yaml")) {
		project := filepath.Base(filepath.Dir(filepath.Dir(sf)))
		src := loadSources(bridge, project)
		edited := false
		for i, s := range src.Sources {
			if !strings.HasPrefix(s.Ref, "$PROJECTS") {
				continue
			}
			if abs := expand(s.Ref); abs != "" {
				src.Sources[i].Ref = workspaceRef(m.Workspaces[project], abs)
				edited = true
			}
		}
		if edited && writeYAML(sf, src) == nil {
			changed = append(changed, relTo(bridge, sf))
		}
	}
	// machine.yaml is written first, so a failure leaves the old lines in place.
	if added && saveMachine(bridge, m) != nil {
		return ""
	}
	for _, path := range stripped {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out := workspaceLine.ReplaceAll(data, nil)
		if len(out) != len(data) {
			out = bytes.ReplaceAll(out, []byte("\n\n\n"), []byte("\n\n"))
		}
		if len(out) != len(data) && os.WriteFile(path, out, 0o644) == nil {
			changed = append(changed, relTo(bridge, path))
		}
	}
	if old.Projects != "" {
		os.Remove(oldMachineFile(bridge))
	}
	if ignoreMachineFile(bridge) {
		changed = append(changed, ".gitignore")
	}
	if len(changed) == 0 {
		return ""
	}
	var commit, left []string
	for _, f := range changed {
		if isGit(bridge) && !before[f] {
			commit = append(commit, f)
		} else {
			left = append(left, f)
		}
	}
	if len(commit) > 0 {
		if err := git(bridge, append([]string{"commit", "-q", "-m", "Workspace paths move to each machine's machine.yaml", "--"}, commit...)...); err != nil {
			left = append(left, commit...)
		}
	}
	msg := ""
	if moved := len(changed) > 1 || changed[0] != ".gitignore"; moved {
		msg = "Where each project's code is moved from the bridge's files into machine.yaml, which is this machine's own."
	}
	if len(left) > 0 {
		msg += " Commit these with the journal: " + strings.Join(left, ", ") + "."
	}
	return strings.TrimSpace(msg)
}

func mustGlob(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	return m
}

// ignoreMachineFile adds machine.yaml to a bridge's .gitignore if it isn't there, and
// says whether it did.
func ignoreMachineFile(bridge string) bool {
	p := filepath.Join(bridge, ".gitignore")
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(line); t == "machine.yaml" || t == "/machine.yaml" {
			return false
		}
	}
	s := string(data)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return os.WriteFile(p, []byte(s+"machine.yaml\n"), 0o644) == nil
}

// workspaceRef writes a path inside a project's code as $WORKSPACE/<rel>, so it resolves
// on every machine; anything else stays as it is.
func workspaceRef(workspace, path string) string {
	if workspace != "" {
		if rel, err := filepath.Rel(filepath.FromSlash(workspace), filepath.FromSlash(path)); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return "$WORKSPACE"
			}
			return "$WORKSPACE/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

// resolveRef turns a $WORKSPACE ref back into a path on this machine.
func resolveRef(workspace, ref string) string {
	rest, ok := strings.CutPrefix(ref, "$WORKSPACE")
	if !ok || workspace == "" {
		return ref
	}
	return filepath.ToSlash(filepath.Join(workspace, strings.TrimPrefix(rest, "/")))
}
