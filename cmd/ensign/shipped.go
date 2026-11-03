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
// shipped copy and the bridge's, decoded generically so formatting and comments don't count.
type shippedSettings struct {
	Bindings  map[string]any
	Pipelines map[string]any
	Use       []string
}

const reviewedHeader = `# The shipped settings (the code's .starship/project.yaml) last reviewed for this project.
# Items here stay quiet in the drift report that 'ensign task open' prints.
# 'ensign project reviewed' writes this file; commit it.
`

func reviewedFile(bridge, project string) string {
	return filepath.Join(bridge, "projects", project, "shipped-reviewed.yaml")
}

// readSettings reads a settings file; a missing file reads as empty with ok false.
func readSettings(path string) (s shippedSettings, ok bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	var raw struct {
		Bindings  map[string]any `yaml:"bindings"`
		Pipelines map[string]any `yaml:"pipelines"`
		Use       []string       `yaml:"use"`
	}
	if err := yamlUnmarshal(data, &raw); err != nil {
		return s, true, fmt.Errorf("%s: %v", path, err)
	}
	bindings, err := roundTrip(raw.Bindings)
	if err != nil {
		return s, true, fmt.Errorf("%s: %v", path, err)
	}
	pipelines, err := roundTrip(raw.Pipelines)
	if err != nil {
		return s, true, fmt.Errorf("%s: %v", path, err)
	}
	return shippedSettings{bindings, pipelines, raw.Use}, true, nil
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

func driftOf(kind string, shipped, bridge, reviewed map[string]any) []string {
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

// decodeSettings turns the generic bindings/pipelines/use of one settings block into shippedSettings.
func decodeSettings(path string, bindings, pipelines map[string]any, use []string) (shippedSettings, error) {
	b, err := roundTrip(bindings)
	if err != nil {
		return shippedSettings{}, fmt.Errorf("%s: %v", path, err)
	}
	p, err := roundTrip(pipelines)
	if err != nil {
		return shippedSettings{}, fmt.Errorf("%s: %v", path, err)
	}
	return shippedSettings{b, p, use}, nil
}

type settingsBlock struct {
	Bindings  map[string]any `yaml:"bindings"`
	Pipelines map[string]any `yaml:"pipelines"`
	Use       []string       `yaml:"use"`
}

// effectiveSettings is what the bridge holds for a location: the project's settings with the location's bindings laid over the project's, as sideBindings does at run time (the location's layer comes after the project's, so it wins).
func effectiveSettings(bridge, project, location string) (shippedSettings, error) {
	file := filepath.Join(bridge, "projects", project, "project.yaml")
	mine, _, err := readSettings(file)
	if err != nil {
		return mine, err
	}
	var pf struct {
		Locations map[string]struct {
			Bindings map[string]any `yaml:"bindings"`
		} `yaml:"locations"`
	}
	if err := readYAML(file, &pf); err != nil {
		return mine, fmt.Errorf("%s: %v", file, err)
	}
	loc, ok := pf.Locations[location]
	if !ok {
		return mine, fmt.Errorf("project %s has no location %s", project, location)
	}
	override, err := roundTrip(loc.Bindings)
	if err != nil {
		return mine, fmt.Errorf("%s: %v", file, err)
	}
	merged := map[string]any{}
	for k, v := range mine.Bindings {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	mine.Bindings = merged
	return mine, nil
}

// readReviewed reads the reviewed record for the project, or for one location's entry when location is set.
func readReviewed(bridge, project, location string) (shippedSettings, error) {
	path := reviewedFile(bridge, project)
	if location == "" {
		s, _, err := readSettings(path)
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
	return decodeSettings(path, e.Bindings, e.Pipelines, e.Use)
}

// shippedDrift lists what the code's shipped settings have that the bridge's copy lacks
// and the reviewed record doesn't cover. It is nil when the code ships no file.
// A non-empty location compares against that location's effective settings and reviewed entry.
func shippedDrift(bridge, project, ws, location string) ([]string, error) {
	shipped, ok, err := readSettings(filepath.Join(ws, filepath.FromSlash(shippedProjectFile)))
	if err != nil || !ok {
		return nil, err
	}
	var mine shippedSettings
	if location == "" {
		mine, _, err = readSettings(filepath.Join(bridge, "projects", project, "project.yaml"))
	} else {
		mine, err = effectiveSettings(bridge, project, location)
	}
	if err != nil {
		return nil, err
	}
	reviewed, err := readReviewed(bridge, project, location)
	if err != nil {
		return nil, err
	}
	lines := driftOf("binding", shipped.Bindings, mine.Bindings, reviewed.Bindings)
	lines = append(lines, driftOf("pipeline", shipped.Pipelines, mine.Pipelines, reviewed.Pipelines)...)
	for _, u := range shipped.Use {
		if !containsName(mine.Use, u) && !containsName(reviewed.Use, u) {
			lines = append(lines, fmt.Sprintf("extension %s: not in the bridge's copy", u))
		}
	}
	return lines, nil
}

// printShippedDrift is the part of `task open` that reports shipped-settings drift; it never fails.
func printShippedDrift(bridge, project, ws, location string, out io.Writer) {
	lines, err := shippedDrift(bridge, project, ws, location)
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
	if location != "" {
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
		front, _, err := splitFrontmatter(filepath.Join(bridge, "projects", project, "tasks", task, "task.md"))
		if err != nil {
			return err
		}
		var tf TaskFile
		if err := yamlUnmarshal(front, &tf); err != nil {
			return err
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
