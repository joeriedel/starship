// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// impactExtension is the extension whose only effects come from bridge.yaml's use:.
const impactExtension = "impact"

// impactNameLimit caps each name or path an impact warning prints.
const impactNameLimit = 40

// impactWarnings are session-start notes about projects and open tasks that name the impact extension in use:, where it does nothing. The session start text has a fixed budget, so there is at most one line, and names are cut. A bridge.yaml that cannot be loaded gives no warning, since what it names is unknown. Subtask files are not layers and carry no use:, so they are not scanned.
func impactWarnings(bridge string) []string {
	bf, err := loadBridgeFile(bridge)
	if err != nil || slices.Contains(bf.Use, impactExtension) {
		return nil
	}
	var hits []string
	projects, _ := os.ReadDir(filepath.Join(bridge, "projects"))
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		var pl Layer
		if readYAML(filepath.Join(bridge, "projects", p.Name(), "project.yaml"), &pl) == nil && slices.Contains(pl.Use, impactExtension) {
			hits = append(hits, "project "+truncate(p.Name(), impactNameLimit))
		}
		tasks, _ := os.ReadDir(filepath.Join(bridge, "projects", p.Name(), "tasks"))
		for _, t := range tasks {
			front, _, err := splitFrontmatter(filepath.Join(bridge, "projects", p.Name(), "tasks", t.Name(), "task.md"))
			if err != nil {
				continue
			}
			var tf struct {
				Use    []string `yaml:"use"`
				Status string   `yaml:"status"`
			}
			if decodeFrontmatter(front, &tf) == nil && tf.Status != "closed" && slices.Contains(tf.Use, impactExtension) {
				hits = append(hits, "task "+truncate(p.Name()+"/"+t.Name(), impactNameLimit))
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	line := fmt.Sprintf("%s names the %s extension in its use: list, but that has no effect there: %s reads use: in bridge.yaml only.", hits[0], impactExtension, impactExtension)
	if len(hits) > 1 {
		line += fmt.Sprintf(" And %d more like it.", len(hits)-1)
	}
	return []string{line}
}
