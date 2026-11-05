// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Link is one external item a task or project points at. Ref is an identifier or URL that Go treats as opaque.
type Link struct {
	Kind string
	Ref  string
}

var linkKinds = []string{"ticket", "wiki", "mail", "slack"}

// linksFile reads only the links: key. It is separate from Layer and TaskFile so that a malformed links: cannot break any other reader of the same file.
type linksFile struct {
	Links yaml.Node `yaml:"links"`
}

// parseLinks checks a links: node and returns its entries. label names the file for errors.
func parseLinks(n *yaml.Node, label string) ([]Link, error) {
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s:%d: links: must be a list of kind/ref entries", label, n.Line)
	}
	var links []Link
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s:%d: links: each entry needs a kind and a ref", label, item.Line)
		}
		var l Link
		seen := map[string]bool{}
		for i := 0; i+1 < len(item.Content); i += 2 {
			key, val := item.Content[i], item.Content[i+1]
			if val.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%s:%d: links: %s must be a single-line string", label, key.Line, key.Value)
			}
			switch key.Value {
			case "kind":
				l.Kind = val.Value
			case "ref":
				l.Ref = val.Value
			default:
				return nil, fmt.Errorf("%s:%d: links: unknown key %q (an entry has only kind and ref)", label, key.Line, key.Value)
			}
			if seen[key.Value] {
				return nil, fmt.Errorf("%s:%d: links: %s is given twice", label, key.Line, key.Value)
			}
			seen[key.Value] = true
		}
		ok := false
		for _, k := range linkKinds {
			ok = ok || k == l.Kind
		}
		if !ok {
			return nil, fmt.Errorf("%s:%d: links: kind %q is not one of %s", label, item.Line, l.Kind, strings.Join(linkKinds, ", "))
		}
		if strings.TrimSpace(l.Ref) == "" {
			return nil, fmt.Errorf("%s:%d: links: a %s link needs a non-empty ref", label, item.Line, l.Kind)
		}
		if strings.ContainsFunc(l.Ref, unicode.IsControl) {
			return nil, fmt.Errorf("%s:%d: links: a ref is one line with no control characters", label, item.Line)
		}
		links = append(links, l)
	}
	return links, nil
}

// collectLinks returns a task's links, the project's first and then the task's own, each (kind, ref) once. front is the task's frontmatter. A file whose links are invalid contributes none and adds a problem; a missing project.yaml has no links.
func collectLinks(bridge, project, taskFile string, front []byte) (links []Link, problems []string) {
	add := func(ls []Link) {
		for _, l := range ls {
			if !containsLink(links, l) {
				links = append(links, l)
			}
		}
	}
	projectFile := filepath.Join(bridge, "projects", project, "project.yaml")
	label := relTo(bridge, projectFile)
	data, err := os.ReadFile(projectFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		problems = append(problems, fmt.Sprintf("%s: %v", label, err))
	default:
		var lf linksFile
		if err := yaml.Unmarshal(data, &lf); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", label, err))
		} else if err := checkKeys(label, data, projectKeys); err != nil {
			problems = append(problems, err.Error())
		} else if ls, err := parseLinks(&lf.Links, label); err != nil {
			problems = append(problems, err.Error())
		} else {
			add(ls)
		}
	}
	var lf linksFile
	if err := decodeFrontmatter(front, &lf); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", relTo(bridge, taskFile), err))
	} else if err := checkTaskKeys(relTo(bridge, taskFile), front); err != nil {
		problems = append(problems, err.Error())
	} else if ls, err := parseLinks(&lf.Links, relTo(bridge, taskFile)); err != nil {
		problems = append(problems, err.Error())
	} else {
		add(ls)
	}
	return links, problems
}

func containsLink(ls []Link, l Link) bool {
	for _, x := range ls {
		if x == l {
			return true
		}
	}
	return false
}

// printLinks prints a task's links for task open and, when the bridge declares the reader, the instruction to dispatch it. Nothing is printed when there are no links and no problems.
func printLinks(bridge, project, taskFile string, front []byte, out io.Writer) {
	links, problems := collectLinks(bridge, project, taskFile, front)
	for _, p := range problems {
		fmt.Fprintf(out, "links      STOP: %s\n", p)
	}
	if len(links) == 0 {
		return
	}
	for i, l := range links {
		label := "links     "
		if i > 0 {
			label = "          "
		}
		fmt.Fprintf(out, "%s %s %s\n", label, l.Kind, l.Ref)
	}
	declared, err := readerDeclared(bridge)
	switch {
	case err != nil:
		fmt.Fprintf(out, "links      STOP: the reader can't be checked: %v\n", err)
	case declared:
		fmt.Fprintln(out, "reader     before starting work, dispatch the reader agent with these links: for each, read its current state, compare it with its kept entry, update the entry; then tell the user what it reports changed")
		if slices.ContainsFunc(links, func(l Link) bool { return l.Kind == "ticket" }) {
			if note := focalNote(bridge, "briefs/ticket-resolved.md"); note != "" {
				fmt.Fprintf(out, "focal      %s\n", note)
			}
		}
	}
}
