// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// keySchema is the keys a YAML mapping may have. A key whose child is nil has a free-form value. A schema with values set is a mapping whose keys are free (names the user chose) and whose every value follows values; one with items set is a list whose every item follows items.
type keySchema struct {
	keys   map[string]*keySchema
	prefix string
	values *keySchema
	items  *keySchema
}

func keyNames(names ...string) map[string]*keySchema {
	m := map[string]*keySchema{}
	for _, n := range names {
		m[n] = nil
	}
	return m
}

// layerKeySchema is the keys of Layer that bridge.yaml, project.yaml, task.md and extension.yaml share; platforms is a project setting and left to each file's schema.
func layerKeySchema() map[string]*keySchema {
	m := keyNames("bindings", "rules", "use", "skills", "workspace", "documentation")
	m["pipelines"] = &keySchema{values: &keySchema{keys: keyNames("ops")}}
	m["events"] = &keySchema{values: &keySchema{items: &keySchema{keys: keyNames("require", "observe", "augment", "status")}}}
	m["journal"] = &keySchema{keys: keyNames("commit", "reason")}
	return m
}

var projectKeys = func() *keySchema {
	m := layerKeySchema()
	for _, k := range []string{"platforms", "pipeline", "links"} {
		m[k] = nil
	}
	m["locations"] = &keySchema{values: &keySchema{keys: keyNames("depends-on", "bindings", "platforms", "documentation", "rules")}}
	m["cross-location"] = &keySchema{keys: keyNames("bindings")}
	return &keySchema{keys: m, prefix: "x-"}
}()

var taskKeys = func() *keySchema {
	m := layerKeySchema()
	for _, k := range []string{"pipeline", "locations", "links", "status"} {
		m[k] = nil
	}
	m["vcs"] = &keySchema{keys: keyNames("kind", "branch", "client", "changelist")}
	return &keySchema{keys: m}
}()

var extensionKeys = func() *keySchema {
	m := layerKeySchema()
	for _, k := range []string{"name", "summary", "requires-bindings", "quiet", "publish"} {
		m[k] = nil
	}
	m["help"] = &keySchema{items: &keySchema{keys: keyNames("say", "does", "run")}}
	return &keySchema{keys: m}
}()

type keyVisit struct {
	n *yaml.Node
	s *keySchema
}

// firstUnknownKey returns the dotted place and line of the first key in a document that its schema doesn't allow. Anything that isn't the shape the schema expects, a malformed document included, is left to the decoders.
func firstUnknownKey(data []byte, s *keySchema) (place string, line int) {
	defer func() {
		if r := recover(); r != nil {
			place, line = "", 0
		}
	}()
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", 0
	}
	return walkKeys(doc.Content[0], s, "", map[keyVisit]bool{})
}

func walkKeys(n *yaml.Node, s *keySchema, place string, seen map[keyVisit]bool) (string, int) {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || s == nil || seen[keyVisit{n, s}] {
		return "", 0
	}
	seen[keyVisit{n, s}] = true
	switch n.Kind {
	case yaml.SequenceNode:
		if s.items == nil {
			return "", 0
		}
		for i, item := range n.Content {
			if p, l := walkKeys(item, s.items, fmt.Sprintf("%s[%d]", place, i), seen); p != "" {
				return p, l
			}
		}
	case yaml.MappingNode:
		if s.keys == nil && s.values == nil {
			return "", 0
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == "<<" && (k.Tag == "" || k.Tag == "!" || k.Tag == "!!merge") {
				sources := []*yaml.Node{v}
				if v.Kind == yaml.SequenceNode {
					sources = v.Content
				}
				for _, src := range sources {
					if src.Kind == yaml.AliasNode {
						src = src.Alias
					}
					if src != nil && src.Kind == yaml.MappingNode {
						if p, l := walkKeys(src, s, place, seen); p != "" {
							return p, l
						}
					}
				}
				continue
			}
			line := k.Line
			if k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k == nil || k.Kind != yaml.ScalarNode {
				continue
			}
			name := k.Value
			if place != "" {
				name = place + "." + name
			}
			child := s.values
			if child == nil {
				c, ok := s.keys[k.Value]
				if !ok && !(s.prefix != "" && strings.HasPrefix(k.Value, s.prefix)) {
					return name, line
				}
				child = c
			}
			if p, l := walkKeys(v, child, name, seen); p != "" {
				return p, l
			}
		}
	}
	return "", 0
}

// checkKeys refuses the first key in a file that its schema doesn't allow. A file with a key that has its own refusal (platforms where the schema leaves it out, connectors and connector_writes) passes, so that refusal speaks.
func checkKeys(file string, data []byte, s *keySchema) error {
	if _, ok := s.keys["platforms"]; connectorKey(data) != "" || (!ok && hasKey(data, "platforms")) {
		return nil
	}
	if place, line := firstUnknownKey(data, s); place != "" {
		return fmt.Errorf("%s: %s: unknown key (line %d)", file, place, line)
	}
	return nil
}

// checkProjectKeys checks a bridge project's project.yaml, which must exist.
func checkProjectKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return checkKeys(path, data, projectKeys)
}

// readProjectYAML is readYAML for a bridge project's project.yaml, which also refuses a key no reader of it knows.
func readProjectYAML(path string, out any) error {
	if err := readYAML(path, out); err != nil {
		return err
	}
	return checkProjectKeys(path)
}

// checkTaskKeys checks a task.md's front matter, as splitFrontmatter returned it, naming lines as the file has them.
func checkTaskKeys(file string, front []byte) error {
	return checkKeys(file, append([]byte("\n"), front...), taskKeys)
}

// readExtensionYAML is readYAML for an extension.yaml, which also refuses a key no reader of it knows. out is filled even when a key is refused, so the extension's name can still be reported.
func readExtensionYAML(path string, out *Extension) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yamlUnmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return checkKeys(path, data, extensionKeys)
}
