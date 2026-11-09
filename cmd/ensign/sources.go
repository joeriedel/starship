// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// DeclaredSource is one external place the user has declared for briefing passes, with the items to watch in it. Lookahead is the days of upcoming meetings a calendar source reads (0 is off) and is 0 for every other kind. A command source has no Watch; it has the Command to run, its Timeout in seconds and the Env set on top of the user's environment.
type DeclaredSource struct {
	Name      string
	Kind      string
	Watch     []string
	Lookahead int
	Command   []string
	Timeout   int
	Env       map[string]string
}

// defaultLookahead is the days ahead a calendar source reads when it gives no lookahead; maxLookahead is the largest value allowed.
const (
	defaultLookahead = 7
	maxLookahead     = 31
)

// defaultCommandTimeout and maxCommandTimeout are the seconds a command source may run when it gives no timeout, and the most it may ask for.
const (
	defaultCommandTimeout = 60
	maxCommandTimeout     = 600
)

var lookaheadRe = regexp.MustCompile(`^[0-9]{1,2}$`)

// declaredSourcesFile reads only the sources: key. It is separate from BridgeFile so that a malformed sources: cannot break any other reader of bridge.yaml.
type declaredSourcesFile struct {
	Sources yaml.Node `yaml:"sources"`
}

var declaredSourceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// declaredSourceKinds are the link kinds plus calendar and command.
func declaredSourceKinds() []string {
	return append(append([]string{}, linkKinds...), "calendar", "command")
}

// cleanSourceItem trims an item and checks it is one non-empty line with no control characters.
func cleanSourceItem(item string) (string, error) {
	item = strings.TrimSpace(item)
	if item == "" {
		return "", fmt.Errorf("an item is empty")
	}
	if strings.ContainsFunc(item, unicode.IsControl) {
		return "", fmt.Errorf("item %q has a control character (an item is one line)", item)
	}
	return item, nil
}

// allDMsItem is the reserved slack item for every DM and group DM the user is a member of; a person whose handle is literally dms can't be watched by name.
const allDMsItem = "@dms"

// isAllDMs reports whether a slack item is the reserved all-DMs item, in any case.
func isAllDMs(item string) bool { return strings.EqualFold(item, allDMsItem) }

// reservedItems are the items a kind reserves for what concerns the user, matched in any case and kept in this canonical form. A wiki page or ticket project with such a name can't be watched by that name.
var reservedItems = map[string][]string{
	"slack":  {allDMsItem},
	"wiki":   {"@mine", "@mentions"},
	"ticket": {"@assigned", "@reported", "@mentions"},
}

// itemPrefixes are the prefixes that make a wiki or ticket item a scope rather than a single page or project, matched in any case.
var itemPrefixes = map[string]string{"wiki": "tree:", "ticket": "query:"}

// canonicalItem returns an already checked item in its canonical form: a reserved item in lowercase, a prefixed item as the lowercase prefix, one space, and the rest as given.
func canonicalItem(kind, item string) string {
	for _, r := range reservedItems[kind] {
		if strings.EqualFold(item, r) {
			return r
		}
	}
	if p := itemPrefixes[kind]; p != "" && len(item) >= len(p) && strings.EqualFold(item[:len(p)], p) {
		return p + " " + strings.TrimSpace(item[len(p):])
	}
	return item
}

// checkSourceItem checks an already cleaned item against its source's kind. Slack, wiki and ticket have forms: a pattern or a bare space or tracker could reach everything, so each item names a channel, DM, page, page tree, project or query, or is a reserved item for what concerns the user. Mail and calendar items are opaque.
func checkSourceItem(kind, item string) error {
	switch kind {
	case "wiki":
		return checkScopeItem("wiki", item, "needs a page URL after tree:", "must be a page URL, tree: <page URL>, @mine or @mentions")
	case "ticket":
		return checkScopeItem("ticket", item, "needs a query after query:", "must be a project key, query: <query>, @assigned, @reported or @mentions")
	case "slack":
	default:
		return nil
	}
	if len(item) < 2 || (item[0] != '#' && item[0] != '@') {
		return fmt.Errorf("slack item %q must be #channel or @person", item)
	}
	if strings.ContainsAny(item, "*?") {
		return fmt.Errorf("slack item %q must not contain * or ?", item)
	}
	if item[0] == '#' && strings.ContainsFunc(item, unicode.IsSpace) {
		return fmt.Errorf("slack channel %q must not contain whitespace", item)
	}
	switch strings.ToLower(item) {
	case "@here", "@channel", "@everyone":
		return fmt.Errorf("slack item %q is not a DM", item)
	}
	return nil
}

// checkScopeItem checks a wiki or ticket item: a prefixed item needs something after the prefix, an @ item must be one of the kind's reserved items, and anything else is a page or project as given.
func checkScopeItem(kind, item, needs, forms string) error {
	p := itemPrefixes[kind]
	if len(item) >= len(p) && strings.EqualFold(item[:len(p)], p) && strings.TrimSpace(item[len(p):]) == "" {
		return fmt.Errorf("%s item %q %s", kind, item, needs)
	}
	if item[0] == '@' && !slices.ContainsFunc(reservedItems[kind], func(r string) bool { return strings.EqualFold(item, r) }) {
		return fmt.Errorf("%s item %q %s", kind, item, forms)
	}
	return nil
}

// kindNote is the line a brief prints after a source's watch items, or "" for a kind with none. allDMs is true when a slack source's items include @dms.
func kindNote(kind string, allDMs bool) string {
	switch kind {
	case "slack":
		note := "also the threads the user is in or mentioned in; never read the rest of the workspace"
		if allDMs {
			note += "; @dms means every DM and group DM the user is a member of, never public or private channels, and if the DM conversations can't be listed the item is unreadable, never widened to a search"
		}
		return note
	case "calendar":
		return "read who met and across which orgs, per the Gather section of your role"
	case "mail":
		return "record every message read per the Gather section of your role; never change mail"
	case "wiki":
		return "read only what each item names; tree: is the page and every page under it, @mine the pages the user created or owns and new comments on them, @mentions pages and comments that mention the user; if the connector can't run the search an item needs, the item is unreadable, never widened to the space"
	case "ticket":
		return "read only what each item names; query: runs as given, @assigned is open tickets assigned to the user, @reported tickets the user reported, @mentions tickets and comments that mention the user; if the connector can't run the search an item needs, the item is unreadable, never widened to the tracker"
	case "command":
		return "the output file is a snapshot of current state with no cursor; read it with the Read tool (never a shell) and compare it with the previous file to see what changed"
	}
	return ""
}

// calendarWindow is the line a brief prints for a calendar source: the span of meetings the pass reads.
func calendarWindow(lookahead int) string {
	if lookahead == 0 {
		return "window: past meetings since the cursor; lookahead is off, read no upcoming meetings"
	}
	return fmt.Sprintf("window: past meetings since the cursor, and upcoming meetings from the time of the pass through %d days ahead", lookahead)
}

// loadDeclaredSources reads the sources: key of bridge.yaml, sorted by name. An absent or empty key declares none. A source has a kind and a watch list, a calendar source may also have lookahead, and a command source has a command instead of a watch list.
func loadDeclaredSources(bridge string) ([]DeclaredSource, error) {
	data, err := os.ReadFile(filepath.Join(bridge, "bridge.yaml"))
	if err != nil {
		return nil, err
	}
	var sf declaredSourcesFile
	if err := yamlUnmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("bridge.yaml: sources: %v", err)
	}
	n := &sf.Sources
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("bridge.yaml: sources: must be a mapping of source names")
	}
	kinds := declaredSourceKinds()
	var out []DeclaredSource
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name, body := n.Content[i].Value, n.Content[i+1]
		if !declaredSourceNameRe.MatchString(name) {
			return nil, fmt.Errorf("bridge.yaml: sources: name %q must be lower-case letters, digits and hyphens, starting with a letter or digit", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("bridge.yaml: sources: %s is declared twice", name)
		}
		seen[name] = true
		if body.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("bridge.yaml: sources: %s needs a kind and a watch list", name)
		}
		s := DeclaredSource{Name: name}
		var watch, lookahead, command, timeout, env *yaml.Node
		keys := map[string]bool{}
		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			if keys[key.Value] {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: %s is given twice", name, key.Value)
			}
			keys[key.Value] = true
			switch key.Value {
			case "kind":
				if val.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("bridge.yaml: sources: %s: kind must be one of %s", name, strings.Join(kinds, ", "))
				}
				s.Kind = val.Value
			case "watch":
				watch = val
			case "lookahead":
				lookahead = val
			case "command":
				command = val
			case "timeout":
				timeout = val
			case "env":
				env = val
			default:
				return nil, fmt.Errorf("bridge.yaml: sources: %s: unknown key %q (a source has only kind and watch, a calendar source also lookahead, and a command source command, timeout and env instead of watch)", name, key.Value)
			}
		}
		ok := false
		for _, k := range kinds {
			ok = ok || k == s.Kind
		}
		if !ok {
			return nil, fmt.Errorf("bridge.yaml: sources: %s: kind %q is not one of %s", name, s.Kind, strings.Join(kinds, ", "))
		}
		if s.Kind == "command" {
			if watch != nil {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: a command source has no watch list", name)
			}
			if lookahead != nil {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: lookahead is only for calendar sources", name)
			}
			if err := parseCommandSource(&s, command, timeout, env); err != nil {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: %v", name, err)
			}
			out = append(out, s)
			continue
		}
		for _, k := range []struct {
			key string
			val *yaml.Node
		}{{"command", command}, {"timeout", timeout}, {"env", env}} {
			if k.val != nil {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: %s is only for command sources", name, k.key)
			}
		}
		switch {
		case lookahead != nil && s.Kind != "calendar":
			return nil, fmt.Errorf("bridge.yaml: sources: %s: lookahead is only for calendar sources", name)
		case lookahead != nil:
			if lookahead.Kind != yaml.ScalarNode || !lookaheadRe.MatchString(lookahead.Value) {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: lookahead must be a whole number of days from 0 to %d", name, maxLookahead)
			}
			s.Lookahead, _ = strconv.Atoi(lookahead.Value)
			if s.Lookahead > maxLookahead {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: lookahead %d is more than %d days", name, s.Lookahead, maxLookahead)
			}
		case s.Kind == "calendar":
			s.Lookahead = defaultLookahead
		}
		if watch == nil || watch.Kind != yaml.SequenceNode || len(watch.Content) == 0 {
			return nil, fmt.Errorf("bridge.yaml: sources: %s: watch must be a list with at least one entry", name)
		}
		have := map[string]bool{}
		for _, e := range watch.Content {
			if e.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: each watch entry must be a string", name)
			}
			item, err := cleanSourceItem(e.Value)
			if err != nil {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: watch: %v", name, err)
			}
			if err := checkSourceItem(s.Kind, item); err != nil {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: watch: %v", name, err)
			}
			item = canonicalItem(s.Kind, item)
			if have[item] {
				return nil, fmt.Errorf("bridge.yaml: sources: %s: watch lists %q twice", name, item)
			}
			have[item] = true
			s.Watch = append(s.Watch, item)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseCommandSource fills in the command, timeout and env of a command source.
func parseCommandSource(s *DeclaredSource, command, timeout, env *yaml.Node) error {
	if command == nil || command.Kind != yaml.SequenceNode || len(command.Content) == 0 {
		return fmt.Errorf("command must be a list of strings with at least the program to run")
	}
	for i, e := range command.Content {
		if e.Kind != yaml.ScalarNode {
			return fmt.Errorf("each command entry must be a string")
		}
		if strings.ContainsFunc(e.Value, unicode.IsControl) {
			return fmt.Errorf("command entry %q has a control character", e.Value)
		}
		if i == 0 && e.Value == "" {
			return fmt.Errorf("the first command entry is the program and must not be empty")
		}
		s.Command = append(s.Command, e.Value)
	}
	s.Timeout = defaultCommandTimeout
	if timeout != nil {
		n, err := strconv.Atoi(timeout.Value)
		if timeout.Kind != yaml.ScalarNode || err != nil || n < 1 || n > maxCommandTimeout {
			return fmt.Errorf("timeout must be a whole number of seconds from 1 to %d", maxCommandTimeout)
		}
		s.Timeout = n
	}
	if env != nil {
		if env.Kind != yaml.MappingNode {
			return fmt.Errorf("env must be a mapping of names to string values")
		}
		s.Env = map[string]string{}
		for i := 0; i+1 < len(env.Content); i += 2 {
			k, v := env.Content[i], env.Content[i+1]
			if k.Kind != yaml.ScalarNode || !envNameRe.MatchString(k.Value) {
				return fmt.Errorf("env name %q must be letters, digits and underscores, not starting with a digit", k.Value)
			}
			if v.Kind != yaml.ScalarNode {
				return fmt.Errorf("env %s must be a string value", k.Value)
			}
			if _, dup := s.Env[k.Value]; dup {
				return fmt.Errorf("env names %s twice", k.Value)
			}
			s.Env[k.Value] = v.Value
		}
	}
	return nil
}

// alsoExtra is one --also <source>=<item> value.
type alsoExtra struct{ source, item string }

// parseAlso reads a list of --also <source>=<item> arguments; any other argument is the usage error.
func parseAlso(args []string, usage error) ([]alsoExtra, error) {
	var also []alsoExtra
	for i := 0; i < len(args); i++ {
		if args[i] != "--also" || i+1 >= len(args) {
			return nil, usage
		}
		i++
		src, item, ok := strings.Cut(args[i], "=")
		src = strings.TrimSpace(src)
		if !ok || src == "" || strings.TrimSpace(item) == "" {
			return nil, usage
		}
		item, err := cleanSourceItem(item)
		if err != nil {
			return nil, fmt.Errorf("--also %s: %v", src, err)
		}
		also = append(also, alsoExtra{src, item})
	}
	return also, nil
}

// checkAlso validates the extras against the declared sources and drops duplicates and items already watched. It also returns the sources by name.
func checkAlso(also []alsoExtra, sources []DeclaredSource) ([]alsoExtra, map[string]DeclaredSource, error) {
	byName := map[string]DeclaredSource{}
	var names []string
	for _, s := range sources {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	var extras []alsoExtra
	seen := map[alsoExtra]bool{}
	for _, e := range also {
		s, ok := byName[e.source]
		if !ok {
			return nil, nil, fmt.Errorf("--also: %q is not a declared source; declared: %s", e.source, strings.Join(names, ", "))
		}
		if s.Kind == "command" {
			return nil, nil, fmt.Errorf("--also does not apply to a command source (%s): it has no items to add", e.source)
		}
		if err := checkSourceItem(s.Kind, e.item); err != nil {
			return nil, nil, fmt.Errorf("--also %s=%s: %v", e.source, e.item, err)
		}
		e.item = canonicalItem(s.Kind, e.item)
		if seen[e] || contains(s.Watch, e.item) {
			continue
		}
		seen[e] = true
		extras = append(extras, e)
	}
	return extras, byName, nil
}
