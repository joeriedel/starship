package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var connectorToolRe = regexp.MustCompile(`^mcp__[A-Za-z0-9_.-]+__[A-Za-z0-9_.-]+$`)

// Claude Code's built-in tools for reading MCP resources; they count as connector tools.
const (
	listResourcesTool = "ListMcpResourcesTool"
	readResourceTool  = "ReadMcpResourceTool"
)

// isConnectorTool reports whether a tool is one connector rules decide: an mcp__ tool or a built-in resource tool.
func isConnectorTool(name string) bool {
	return strings.HasPrefix(name, "mcp__") || name == listResourcesTool || name == readResourceTool
}

// connectorRules is the bridge's connector declaration: role to the exact tools it may call. Nil means none is declared; a role with an empty list still declares.
func (bf *BridgeFile) connectorRules() (map[string][]string, error) {
	if _, _, err := bf.connectorWrites(); err != nil {
		return nil, err
	}
	if len(bf.Connectors) == 0 {
		return nil, nil
	}
	for _, role := range sortedKeys(bf.Connectors) {
		if role == "coder" {
			return nil, fmt.Errorf("bridge.yaml: connectors: the coder role never gets connectors")
		}
		for _, tool := range bf.Connectors[role] {
			if !connectorToolRe.MatchString(tool) && tool != listResourcesTool && tool != readResourceTool {
				return nil, fmt.Errorf("bridge.yaml: connectors: %s: %q is not an exact mcp__<server>__<tool> name, or ListMcpResourcesTool or ReadMcpResourceTool (no wildcards, and a whole server cannot be granted)", role, tool)
			}
		}
	}
	return bf.Connectors, nil
}

// connectorWrites is the bridge's connector_writes declaration: the exact tools the session may call after the user approves the call. on is true whenever the key is present, an empty list included.
func (bf *BridgeFile) connectorWrites() (tools []string, on bool, err error) {
	if bf.ConnectorWrites.Kind == 0 {
		return nil, false, nil
	}
	if err := bf.ConnectorWrites.Decode(&tools); err != nil {
		return nil, true, fmt.Errorf("bridge.yaml: connector_writes: must be a list of exact mcp__<server>__<tool> names: %v", err)
	}
	for _, tool := range tools {
		if !connectorToolRe.MatchString(tool) {
			return nil, true, fmt.Errorf("bridge.yaml: connector_writes: %q is not an exact mcp__<server>__<tool> name (no wildcards, and a whole server cannot be granted)", tool)
		}
		for _, role := range sortedKeys(bf.Connectors) {
			if slices.Contains(bf.Connectors[role], tool) {
				return nil, true, fmt.Errorf("bridge.yaml: connector_writes: %q is also listed under connectors: %s; a tool is either read by a role or written with the user's approval", tool, role)
			}
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(tools))), true, nil
}

// settingsTools is every tool the settings must allow: the roles' declared tools and the approved writes, sorted and without duplicates.
func settingsTools(rules map[string][]string, writes []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(append(connectorTools(rules), writes...))))
}

// connectorTools is every declared tool, sorted and without duplicates.
func connectorTools(rules map[string][]string) []string {
	var all []string
	for _, tools := range rules {
		all = append(all, tools...)
	}
	return slices.Compact(slices.Sorted(slices.Values(all)))
}

// connectorDecision decides a connector call (a tool named mcp__<server>__<tool>, or a built-in resource tool) from bridge.yaml, and with connector_writes declared from the session's approved writes (writeDecision). It returns nil to leave the call to the permission system, else the denial. It never touches a run.
func connectorDecision(bridge string, in *HookInput) any {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return denial(fmt.Sprintf("connector rules can't be read, so %s is refused: %v", in.ToolName, err))
	}
	rules, err := bf.connectorRules()
	if err != nil {
		return denial(fmt.Sprintf("%s is refused: %v", in.ToolName, err))
	}
	writes, writesOn, _ := bf.connectorWrites()
	if writesOn {
		return writeDecision(bridge, in, rules, writes)
	}
	if rules == nil {
		return nil
	}
	if in.AgentID == "" {
		return sessionReadDenial(rules, in.ToolName)
	}
	if slices.Contains(rules[in.AgentType], in.ToolName) {
		return nil
	}
	return denial(fmt.Sprintf("role %q may not call %s: bridge.yaml connectors does not list it for that role", in.AgentType, in.ToolName))
}

// frontmatterMcpServers reports whether a role file's frontmatter has an mcpServers key, and the role's name field.
func frontmatterMcpServers(label string, data []byte) (has bool, name string, err error) {
	front, _, err := splitFrontmatterData(label, data)
	if err != nil {
		return false, "", err
	}
	var fm map[string]any
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return false, "", fmt.Errorf("%s: frontmatter: %v", label, err)
	}
	_, has = fm["mcpServers"]
	name, _ = fm["name"].(string)
	return has, name, nil
}

// mcpServersRefusal is why dispatching a role must be refused while connector rules are declared: its definition in the bridge's .claude/agents sets mcpServers, which bypasses them. Claude Code knows an agent by its frontmatter name, so that is matched as well as the file name. A definition named by its file that can't be parsed is refused; one named otherwise that can't be parsed cannot be told apart, and is skipped.
func mcpServersRefusal(bridge string, in *HookInput) string {
	role, _ := in.ToolInput["subagent_type"].(string)
	if role == "" {
		return ""
	}
	// A bridge.yaml that can't be read or declares badly refuses every connector call already.
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return ""
	}
	rules, err := bf.connectorRules()
	if _, writesOn, _ := bf.connectorWrites(); err != nil || (rules == nil && !writesOn) {
		return ""
	}
	files, _ := filepath.Glob(filepath.Join(bridge, ".claude", "agents", "*.md"))
	for _, f := range files {
		stem := strings.TrimSuffix(filepath.Base(f), ".md")
		data, err := os.ReadFile(f)
		if err != nil {
			if stem == role {
				return fmt.Sprintf("role %s can't be dispatched: %s can't be checked for mcpServers: %v", role, f, err)
			}
			continue
		}
		has, name, err := frontmatterMcpServers(f, data)
		if err != nil {
			if stem == role {
				return fmt.Sprintf("role %s can't be dispatched: %s can't be checked for mcpServers: %v", role, f, err)
			}
			continue
		}
		if has && (stem == role || name == role) {
			return fmt.Sprintf("role %s can't be dispatched: %s sets mcpServers, which bypasses the connector rules in bridge.yaml", role, f)
		}
	}
	return ""
}

const (
	// connectorMatcher is what a declaration adds to the PreToolUse matcher; connectorMatcherPrev and connectorMatcherOld are what earlier ensigns added.
	connectorMatcher     = `|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool|Read|Grep"`
	connectorMatcherPrev = `|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool"`
	connectorMatcherOld  = `|mcp__.*"`
)

var (
	connectorAllowRe   = regexp.MustCompile(`, "(mcp__[^"]*|ListMcpResourcesTool|ReadMcpResourceTool)"`)
	connectorToolStrRe = regexp.MustCompile(`"(mcp__[^"]*|ListMcpResourcesTool|ReadMcpResourceTool)"`)
)

// withoutConnectors is settings with the connector parts removed, to compare against the forms ensign has written.
func withoutConnectors(settings string) string {
	for _, m := range []string{connectorMatcher, connectorMatcherPrev, connectorMatcherOld} {
		if strings.Contains(settings, m) {
			settings = strings.Replace(settings, m, `"`, 1)
			break
		}
	}
	return connectorAllowRe.ReplaceAllString(settings, "")
}

// ensignShaped reports whether the connector parts of settings have the shape ensign writes: none at all, or the matcher with any mcp__ tools sorted and without duplicates directly after Bash(ensign:*).
func ensignShaped(settings string) bool {
	tools := connectorToolStrRe.FindAllString(settings, -1)
	if !strings.Contains(settings, connectorMatcher) && !strings.Contains(settings, connectorMatcherPrev) && !strings.Contains(settings, connectorMatcherOld) {
		return len(tools) == 0
	}
	if !slices.IsSorted(tools) || len(slices.Compact(slices.Clone(tools))) != len(tools) {
		return false
	}
	return strings.Contains(settings, `"Bash(ensign:*)"`+strings.Join(append([]string{""}, tools...), ", "))
}

// connectorSignature is what a settings file carries for connectors.
func connectorSignature(settings string) string {
	return fmt.Sprint(strings.Contains(settings, connectorMatcher), strings.Contains(settings, connectorMatcherPrev), strings.Contains(settings, connectorMatcherOld), connectorToolStrRe.FindAllString(settings, -1))
}

// agentsSignature is the connector-relevant state of a bridge's role definitions: which files there are and each one's frontmatter tools: value. Other edits to a role's prompt are not part of it.
func agentsSignature(bridge string) string {
	files, _ := filepath.Glob(filepath.Join(bridge, ".claude", "agents", "*.md"))
	slices.Sort(files)
	var sig strings.Builder
	for _, f := range files {
		data, _ := os.ReadFile(f)
		tools := "-"
		if m := frontmatterToolsLoc(data); m != nil {
			tools = strings.TrimSpace(string(data[m[4]:m[5]]))
		}
		fmt.Fprintf(&sig, "%s\x00%s\x00", filepath.Base(f), tools)
	}
	return sig.String()
}

// missingConnectorSettings lists what a hand-edited settings.json lacks for the declaration to be enforced: the mcp__.*, ListMcpResourcesTool, ReadMcpResourceTool, and, when the reader is declared, Read and Grep matcher entries on the ensign PreToolUse hook and each declared tool, connector_writes included, in allow.
func missingConnectorSettings(data []byte, tools []string, reader bool) ([]string, error) {
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	var missing []string
	entries := []string{"mcp__.*", listResourcesTool, readResourceTool}
	if reader {
		entries = append(entries, "Read", "Grep")
	}
	for _, entry := range entries {
		matched := false
		for _, e := range s.Hooks.PreToolUse {
			if !slices.Contains(strings.Split(e.Matcher, "|"), entry) {
				continue
			}
			for _, h := range e.Hooks {
				matched = matched || strings.Contains(h.Command, " hook PreToolUse")
			}
		}
		if !matched {
			missing = append(missing, fmt.Sprintf("the %q matcher on the ensign PreToolUse hook", entry))
		}
	}
	for _, t := range tools {
		if !slices.Contains(s.Permissions.Allow, t) {
			missing = append(missing, t+" in permissions.allow")
		}
	}
	return missing, nil
}

// upgradeHookSettings brings a bridge's settings.json up to date (a timeout on the PreToolUse and SubagentStop hooks, the PostToolUse hook, SubagentHandback, and the connector parts bridge.yaml calls for) when it is a form ensign wrote, with or without connector parts, and commits that. Any other file is left alone, but when connectors are declared and it can't enforce them an error says what is missing. An invalid declaration is an error too.
func upgradeHookSettings(bridge string, bf *BridgeFile) error {
	rules, err := bf.connectorRules()
	if err != nil {
		return err
	}
	writes, writesOn, _ := bf.connectorWrites()
	declared, tools := rules != nil || writesOn, settingsTools(rules, writes)
	_, readerRule := rules["reader"]
	f := filepath.Join(bridge, ".claude", "settings.json")
	data, rerr := os.ReadFile(f)
	if rerr != nil {
		if declared {
			return fmt.Errorf("connector rules are declared but %s can't be read: %v", f, rerr)
		}
		return nil
	}
	want := hookSettingsFor(declared, tools)
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if text == want {
		return nil
	}
	base := withoutConnectors(text)
	known := false
	for _, form := range []string{hookSettings(), hookSettingsWith(0, 0), hookSettingsWith(hookTimeout, 0)} {
		known = known || base == form || base == beforeHandback(form)
	}
	known = known && ensignShaped(text)
	if !declared && text != string(data) && withoutConnectors(text) == text {
		return nil
	}
	if !known {
		if !declared {
			return nil
		}
		missing, err := missingConnectorSettings(data, tools, readerRule)
		if err != nil {
			return fmt.Errorf("connector rules are declared but %s can't be read: %v", f, err)
		}
		if len(missing) > 0 {
			what := "connector calls would not be checked"
			if !slices.ContainsFunc(missing, func(m string) bool {
				return !strings.HasPrefix(m, `the "Read" `) && !strings.HasPrefix(m, `the "Grep" `)
			}) {
				what = "reads of reader/sources/ would not be checked"
			}
			return fmt.Errorf("connector rules are declared but %s was edited by hand and lacks %s; %s", f, strings.Join(missing, ", "), what)
		}
		return nil
	}
	if werr := os.WriteFile(f, []byte(want), 0o644); werr != nil {
		if declared {
			return fmt.Errorf("connector rules are declared but %s can't be written: %v", f, werr)
		}
		return nil
	}
	if isGit(bridge) {
		git(bridge, "commit", "-q", "-m", "Update the starship hook settings", "--", ".claude/settings.json")
	}
	return nil
}
