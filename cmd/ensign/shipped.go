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

// shippedDrift lists what the code's shipped settings have that the bridge's copy lacks
// and the reviewed record doesn't cover. It is nil when the code ships no file.
func shippedDrift(bridge, project, ws string) ([]string, error) {
	shipped, ok, err := readSettings(filepath.Join(ws, filepath.FromSlash(shippedProjectFile)))
	if err != nil || !ok {
		return nil, err
	}
	mine, _, err := readSettings(filepath.Join(bridge, "projects", project, "project.yaml"))
	if err != nil {
		return nil, err
	}
	reviewed, _, err := readSettings(reviewedFile(bridge, project))
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
func printShippedDrift(bridge, project, ws string, out io.Writer) {
	lines, err := shippedDrift(bridge, project, ws)
	if err != nil {
		fmt.Fprintf(out, "shipped    can't compare: %v\n", err)
		return
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(out, "shipped    the code's %s differs from projects/%s/project.yaml:\n", shippedProjectFile, project)
	for _, l := range lines {
		fmt.Fprintf(out, "             %s\n", l)
	}
	fmt.Fprintf(out, "           Review these with the user, then run `ensign project reviewed %s` to mark them reviewed.\n", project)
}

// cmdProjectReviewed records the code's current shipped settings as reviewed.
func cmdProjectReviewed(arg string, out io.Writer) error {
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	project, task, _ := strings.Cut(arg, "/")
	if _, err := os.Stat(filepath.Join(bridge, "projects", project, "project.yaml")); err != nil {
		return fmt.Errorf("no project %s", project)
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
	rec := map[string]any{}
	if len(shipped.Bindings) > 0 {
		rec["bindings"] = shipped.Bindings
	}
	if len(shipped.Pipelines) > 0 {
		rec["pipelines"] = shipped.Pipelines
	}
	if len(shipped.Use) > 0 {
		rec["use"] = shipped.Use
	}
	path := reviewedFile(bridge, project)
	data, err := yaml.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeFile(path, reviewedHeader+string(data)); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s; commit it\n", path)
	return nil
}
