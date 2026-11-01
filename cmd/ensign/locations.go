package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A project can keep its code in several repos. project.yaml declares them as locations, each with its own bindings and the locations it depends on; a task names the ones it uses, and each run edits exactly one but tests it and every location that depends on it (its sides).

// Locations is a project's declared code locations.
type Locations struct {
	deps     map[string][]string
	bindings map[string]yaml.Node
	// Cross holds the cross-location bindings; a run with several sides runs their @test once after the sides' own.
	Cross map[string]Binding
}

type projectLocationsFile struct {
	Locations map[string]struct {
		DependsOn []string  `yaml:"depends-on"`
		Bindings  yaml.Node `yaml:"bindings"`
	} `yaml:"locations"`
	CrossLocation struct {
		Bindings yaml.Node `yaml:"bindings"`
	} `yaml:"cross-location"`
}

// loadLocations reads a project's locations and cross-location bindings from project.yaml. A project that declares none gets an empty Locations.
func loadLocations(bridge, project string) (*Locations, error) {
	file := filepath.Join(bridge, "projects", project, "project.yaml")
	var pf projectLocationsFile
	if err := readYAML(file, &pf); err != nil {
		return nil, fmt.Errorf("project %q: %v", project, err)
	}
	l := &Locations{deps: map[string][]string{}, bindings: map[string]yaml.Node{}, Cross: map[string]Binding{}}
	label := relTo(bridge, file)
	for name, loc := range pf.Locations {
		if !validName(name) {
			return nil, fmt.Errorf("%s: location %q: name it with lower-case letters, digits and dashes", label, name)
		}
		l.deps[name] = append([]string(nil), loc.DependsOn...)
		sort.Strings(l.deps[name])
		l.bindings[name] = loc.Bindings
		if _, err := parseBindingNode(&loc.Bindings, "location:"+label+"#"+name); err != nil {
			return nil, err
		}
	}
	for _, name := range l.Names() {
		for _, d := range l.deps[name] {
			if d == name {
				return nil, fmt.Errorf("%s: location %s depends on itself", label, name)
			}
			if _, ok := l.deps[d]; !ok {
				return nil, fmt.Errorf("%s: location %s depends on %q, which is not a declared location", label, name, d)
			}
		}
	}
	if cycle := l.cycle(); cycle != "" {
		return nil, fmt.Errorf("%s: locations depend on each other in a cycle: %s", label, cycle)
	}
	var err error
	if l.Cross, err = parseBindingNode(&pf.CrossLocation.Bindings, "cross-location:"+label); err != nil {
		return nil, err
	}
	return l, nil
}

// parseBindingNode parses a bindings mapping the way a layer's bindings are parsed.
func parseBindingNode(n *yaml.Node, label string) (map[string]Binding, error) {
	out := map[string]Binding{}
	c := &Config{Bindings: out}
	if err := c.addBindings(n, label); err != nil {
		return nil, err
	}
	return out, nil
}

// cycle returns a path such as "a -> b -> a" when the dependencies loop, otherwise "".
func (l *Locations) cycle() string {
	state := map[string]int{} // 1 on the current path, 2 done
	var path []string
	var visit func(n string) string
	visit = func(n string) string {
		switch state[n] {
		case 1:
			i := 0
			for path[i] != n {
				i++
			}
			return strings.Join(append(append([]string(nil), path[i:]...), n), " -> ")
		case 2:
			return ""
		}
		state[n] = 1
		path = append(path, n)
		for _, d := range l.deps[n] {
			if c := visit(d); c != "" {
				return c
			}
		}
		path = path[:len(path)-1]
		state[n] = 2
		return ""
	}
	for _, n := range l.Names() {
		if c := visit(n); c != "" {
			return c
		}
	}
	return ""
}

// Names lists the declared locations, sorted.
func (l *Locations) Names() []string {
	names := make([]string, 0, len(l.deps))
	for n := range l.deps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Has says whether the project declares the location.
func (l *Locations) Has(name string) bool {
	_, ok := l.deps[name]
	return ok
}

// DependsOn lists the locations name depends on directly, sorted.
func (l *Locations) DependsOn(name string) []string {
	return append([]string(nil), l.deps[name]...)
}

// Dependents lists every location that depends on name, directly or through others, sorted.
func (l *Locations) Dependents(name string) []string {
	var out []string
	for _, n := range l.Names() {
		if n != name && l.reaches(n, name, map[string]bool{}) {
			out = append(out, n)
		}
	}
	return out
}

func (l *Locations) reaches(from, to string, seen map[string]bool) bool {
	for _, d := range l.deps[from] {
		if d == to {
			return true
		}
		if !seen[d] {
			seen[d] = true
			if l.reaches(d, to, seen) {
				return true
			}
		}
	}
	return false
}

// taskLocation picks the location a run of a task works in. want is the one asked for, "" for none. A project without locations has none; otherwise the task must name its locations, and a run takes the one asked for, or else the task's sole one.
func taskLocation(locs *Locations, project, task string, taskLocs []string, want string) (string, error) {
	declared := len(locs.deps) > 0
	switch {
	case !declared && len(taskLocs) > 0:
		return "", fmt.Errorf("task %s names locations (%s) but project %s declares none", task, strings.Join(taskLocs, ", "), project)
	case !declared && want != "":
		return "", fmt.Errorf("project %s declares no locations, so --location %s has no meaning", project, want)
	case !declared:
		return "", nil
	case len(taskLocs) == 0:
		return "", fmt.Errorf("task %s must name the locations it uses (locations: [...] in its task.md); project %s declares %s", task, project, strings.Join(locs.Names(), ", "))
	}
	for _, n := range taskLocs {
		if !locs.Has(n) {
			return "", fmt.Errorf("task %s names location %q, which project %s does not declare (it declares %s)", task, n, project, strings.Join(locs.Names(), ", "))
		}
	}
	if want == "" {
		if len(taskLocs) == 1 {
			return taskLocs[0], nil
		}
		return "", fmt.Errorf("task %s uses several locations (%s); name the one this run works in with --location", task, strings.Join(taskLocs, ", "))
	}
	if !contains(taskLocs, want) {
		return "", fmt.Errorf("task %s does not use location %q (it uses %s)", task, want, strings.Join(taskLocs, ", "))
	}
	return want, nil
}

// locationPathFor is where a project's location is on this machine.
func locationPathFor(bridge, project, location string) (string, error) {
	if p := loadMachine(bridge).Locations[project][location]; p != "" {
		return filepath.FromSlash(p), nil
	}
	return "", fmt.Errorf("this machine has no folder recorded for location %s of project %s; ask the user where its code is on this machine, never guess, then record it with `ensign workspace %s --location %s <path>`",
		location, project, project, location)
}

// setLocationPath records where a project's location is on this machine.
func setLocationPath(bridge, project, location, path string) (string, error) {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder on this machine; ask the user where the code is, never guess", abs)
	}
	m := loadMachine(bridge)
	if m.Locations[project] == nil {
		m.Locations[project] = map[string]string{}
	}
	m.Locations[project][location] = filepath.ToSlash(abs)
	return abs, saveMachine(bridge, m)
}

// locationsYAML writes machine.yaml's locations block, empty when there are none.
func locationsYAML(locs map[string]map[string]string) string {
	var projects []string
	for p, m := range locs {
		if len(m) > 0 {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return ""
	}
	sort.Strings(projects)
	var b strings.Builder
	b.WriteString("\nlocations:\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "  %s:\n", p)
		names := make([]string, 0, len(locs[p]))
		for n := range locs[p] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "    %s: %s\n", n, yamlScalar(locs[p][n]))
		}
	}
	return b.String()
}

// runLocation is the location the commands that land a task's work act in: the one asked for, else the one its newest successful run worked in. "" leaves it to taskLocation, which takes the task's sole location or asks for --location.
func runLocation(bridge, task, want string) string {
	if want != "" {
		return want
	}
	return latestRunLocation(bridge, task, true)
}

// latestRunLocation is the location of the task's newest run, or of the one that succeeded last. Runs are listed by creation, but a retry is created after the steps queued behind it, so success is ranked by when the run finished; a run with no finish time is oldest.
func latestRunLocation(bridge, task string, successOnly bool) string {
	loc, best := "", int64(-1)
	for _, ri := range listRuns(bridge) {
		if ri.snap.Task != task {
			continue
		}
		if !successOnly {
			return ri.snap.Location
		}
		if ri.state.Status == "success" && ri.state.FinishedAt > best {
			loc, best = ri.snap.Location, ri.state.FinishedAt
		}
	}
	return loc
}

// precedingRunLocation is the location of the run step nearest before step n for the task, taking the latest attempt, or "" when there is none.
func precedingRunLocation(bridge string, seq *sequence, n int, task string) string {
	var run string
	for _, s := range seq.Steps {
		if s.N < n && s.Kind == "run" && s.Task == task && s.Run != "" {
			run = s.Run
		}
	}
	if run == "" {
		return ""
	}
	var snap Snapshot
	if readJSON(filepath.Join(runsDir(bridge), run, "pipeline.json"), &snap) != nil {
		return ""
	}
	return snap.Location
}

// taskStateFile is a per-task file under .starship/tasks, kept per location when the task's runs have one so each repo records its own commits.
func taskStateFile(bridge, task, location, ext string) string {
	name := strings.ReplaceAll(task, "/", "--")
	if location != "" {
		name += "@" + location
	}
	return filepath.Join(bridge, ".starship", "tasks", name+ext)
}
