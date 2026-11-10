// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// declaredFocalFile reads only the focal: key. It is separate from BridgeFile so that a malformed focal: cannot break any other reader of bridge.yaml.
type declaredFocalFile struct {
	Focal yaml.Node `yaml:"focal"`
}

// focalConfig is the focal: declaration: the period whose brag file the secretary keeps, when it ends, and an optional rubric path relative to the bridge.
type focalConfig struct {
	Period string
	Starts string
	Ends   string
	Rubric string
}

// bragPath is the brag file's path relative to the bridge.
func (f *focalConfig) bragPath() string {
	return "notes/focal/" + f.Period + ".brag.md"
}

// loadFocal reads the focal: key of bridge.yaml. An absent or empty key declares none.
func loadFocal(bridge string) (*focalConfig, error) {
	data, err := os.ReadFile(filepath.Join(bridge, "bridge.yaml"))
	if err != nil {
		return nil, err
	}
	var ff declaredFocalFile
	if err := yamlUnmarshal(data, &ff); err != nil {
		return nil, fmt.Errorf("bridge.yaml: focal: %v", err)
	}
	n := &ff.Focal
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("bridge.yaml: focal: must be a mapping with period, ends and optionally starts and rubric")
	}
	f := &focalConfig{}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		if seen[key] {
			return nil, fmt.Errorf("bridge.yaml: focal: %s is given twice", key)
		}
		seen[key] = true
		if key != "period" && key != "ends" && key != "starts" && key != "rubric" {
			return nil, fmt.Errorf("bridge.yaml: focal: unknown key %q (focal has only period, starts, ends and rubric)", key)
		}
		if val.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("bridge.yaml: focal: %s must be a string", key)
		}
		switch key {
		case "period":
			f.Period = val.Value
		case "starts":
			f.Starts = val.Value
		case "ends":
			f.Ends = val.Value
		default:
			f.Rubric = val.Value
		}
	}
	if f.Period == "" {
		return nil, fmt.Errorf("bridge.yaml: focal: period is missing")
	}
	if strings.ContainsAny(f.Period, `/\:*?"<>|`) || strings.Contains(f.Period, "..") || strings.TrimSpace(f.Period) != f.Period || windowsReserved(f.Period) {
		return nil, fmt.Errorf("bridge.yaml: focal: period %q must be a name such as 2026-h2, usable as a file name", f.Period)
	}
	if f.Ends == "" {
		return nil, fmt.Errorf("bridge.yaml: focal: ends is missing")
	}
	endsAt, err := time.Parse("2006-01-02", f.Ends)
	if err != nil {
		return nil, fmt.Errorf("bridge.yaml: focal: ends %q must be a date, YYYY-MM-DD", f.Ends)
	}
	if f.Starts != "" {
		startsAt, err := time.Parse("2006-01-02", f.Starts)
		if err != nil {
			return nil, fmt.Errorf("bridge.yaml: focal: starts %q must be a date, YYYY-MM-DD", f.Starts)
		}
		if !startsAt.Before(endsAt) {
			return nil, fmt.Errorf("bridge.yaml: focal: starts %s must be before ends %s", f.Starts, f.Ends)
		}
	}
	if f.Rubric != "" {
		if strings.Contains(f.Rubric, `\`) || filepath.IsAbs(f.Rubric) || strings.HasPrefix(f.Rubric, "/") {
			return nil, fmt.Errorf("bridge.yaml: focal: rubric %q must be a path under notes/ written with /", f.Rubric)
		}
		clean := filepath.ToSlash(filepath.Clean(f.Rubric))
		if !strings.HasPrefix(clean, "notes/") || clean != f.Rubric {
			return nil, fmt.Errorf("bridge.yaml: focal: rubric %q must be a plain path under notes/", f.Rubric)
		}
		if !strings.HasSuffix(clean, ".md") {
			return nil, fmt.Errorf("bridge.yaml: focal: rubric %q must be a .md file", f.Rubric)
		}
		if st, err := os.Stat(filepath.Join(bridge, filepath.FromSlash(clean))); err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("bridge.yaml: focal: rubric %q is not a file in the bridge", f.Rubric)
		}
	}
	return f, nil
}
