// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Capabilities is what a starship role may do, from the `may:` block in its frontmatter. Everything the hooks can check is denied unless it is declared here.
type Capabilities struct {
	Tools      []string // tool whitelist; Agent and Task are one tool
	Background bool     // Bash with run_in_background
	Agents     []string // Agent subagent_type: "stages", "free" or literal role names
	Edit       []string // folder kinds, see editKinds
	Remote     bool     // may change a remote
	Connectors bool     // connector tools pass the whitelist; connectorDecision still governs them
}

// editKinds are the folder kinds `may.edit` can name. Each grants a folder only under the conditions the edit guard has always put on it: code in a live code or style dispatch, docs in a live documentation dispatch, run is the run's scratch and evidence folders in a live dispatch and the system temp folders, secretary is notes/secretary and notes/focal, reader is <bridge>/reader once bridge.yaml declares the role under connectors.
var editKinds = []string{"code", "docs", "run", "secretary", "reader"}

// harnessTools are the tools Claude Code gives every agent whatever its tools: line says, so the whitelist lets them through: SubagentHandback ends every agent's work, and ToolSearch only loads the schemas of tools the agent already has.
var harnessTools = []string{"SubagentHandback", "ToolSearch"}

// frontmatterError marks a role file whose frontmatter can't be read at all, as distinct from a bad `may:` block.
type frontmatterError struct{ err error }

func (e *frontmatterError) Error() string { return e.err.Error() }
func (e *frontmatterError) Unwrap() error { return e.err }

var mayKeys = []string{"tools", "background", "agents", "edit", "remote", "connectors"}

// parseCapabilities reads the `may:` block of a role file. A file with no `may:` gets empty capabilities, which allow nothing. label names the file in an error.
func parseCapabilities(label string, data []byte) (*Capabilities, error) {
	front, _, err := splitFrontmatterData(label, data)
	if err != nil {
		return nil, &frontmatterError{err}
	}
	var fm map[string]any
	if err := decodeFrontmatter(front, &fm); err != nil {
		return nil, &frontmatterError{fmt.Errorf("%s: frontmatter: %v", label, err)}
	}
	caps := &Capabilities{}
	raw, ok := fm["may"]
	if !ok {
		return caps, nil
	}
	may, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: may: must be a mapping of capabilities (%s)", label, strings.Join(mayKeys, ", "))
	}
	for _, key := range sortedKeys(may) {
		if !slices.Contains(mayKeys, key) {
			return nil, fmt.Errorf("%s: may.%s: unknown key (the keys are %s)", label, key, strings.Join(mayKeys, ", "))
		}
	}
	list := func(key string) ([]string, error) {
		v, ok := may[key]
		if !ok {
			return nil, nil
		}
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: may.%s: must be a list of names", label, key)
		}
		var out []string
		for _, item := range items {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" || strings.ContainsAny(s, ", \t") {
				return nil, fmt.Errorf("%s: may.%s: every entry must be one plain name, got %v", label, key, item)
			}
			out = append(out, s)
		}
		return out, nil
	}
	flag := func(key string) (bool, error) {
		v, ok := may[key]
		if !ok {
			return false, nil
		}
		b, ok := v.(bool)
		if !ok {
			return false, fmt.Errorf("%s: may.%s: must be true or false", label, key)
		}
		return b, nil
	}
	if caps.Tools, err = list("tools"); err != nil {
		return nil, err
	}
	if caps.Agents, err = list("agents"); err != nil {
		return nil, err
	}
	if caps.Edit, err = list("edit"); err != nil {
		return nil, err
	}
	for _, kind := range caps.Edit {
		if !slices.Contains(editKinds, kind) {
			return nil, fmt.Errorf("%s: may.edit: unknown folder kind %q (the kinds are %s)", label, kind, strings.Join(editKinds, ", "))
		}
	}
	if caps.Background, err = flag("background"); err != nil {
		return nil, err
	}
	if caps.Remote, err = flag("remote"); err != nil {
		return nil, err
	}
	if caps.Connectors, err = flag("connectors"); err != nil {
		return nil, err
	}
	return caps, nil
}

func (c *Capabilities) edits(kind string) bool { return slices.Contains(c.Edit, kind) }

// toolRefusal is why the role may not make this tool call, or "".
func (c *Capabilities) toolRefusal(role string, in *HookInput) string {
	tool := in.ToolName
	switch {
	case slices.Contains(harnessTools, tool):
		return ""
	case isConnectorTool(tool):
		if !c.Connectors {
			return fmt.Sprintf("this %s may not call %s: its definition's `may.connectors` is not declared. Working around the guard is a failure, not a fix; report the need to your caller.", role, tool)
		}
		return ""
	case tool == "Task" && slices.Contains(c.Tools, "Agent"), tool == "Agent" && slices.Contains(c.Tools, "Task"), slices.Contains(c.Tools, tool):
	default:
		return fmt.Sprintf("this %s may not call %s: its definition's `may.tools` does not list it. Working around the guard is a failure, not a fix; report the need to your caller.", role, tool)
	}
	if tool == "Bash" && in.ToolInput["run_in_background"] == true && !c.Background {
		return fmt.Sprintf("this %s may not run a Bash command in the background: its definition's `may.background` is not declared. Run it in the foreground; otherwise report the need to your caller.", role)
	}
	return ""
}

// agentRefusal is why the role may not dispatch subagent, or "". "stages" and "free" need the run's pipeline, which is nil when the agent's run can't be found, so only literal names cover then.
func (c *Capabilities) agentRefusal(role, subagent string, p *Pipeline) string {
	for _, a := range c.Agents {
		switch {
		case a == "stages" && p != nil && p.hasStageRole(subagent):
			return ""
		case a == "free" && p != nil && slices.Contains(p.FreeRoles, subagent):
			return ""
		case a == subagent && a != "stages" && a != "free":
			return ""
		}
	}
	return fmt.Sprintf("this %s may not dispatch %q: its definition's `may.agents` does not cover it. Working around the guard is a failure, not a fix; report the need to your caller.", role, subagent)
}

// roleDefinitionRefusal is the denial for a role whose definition can't be checked.
func roleDefinitionRefusal(role string, err error) string {
	return fmt.Sprintf("this %s's definition can't be checked, so every tool call of it is refused: %v. Report the problem to your caller.", role, err)
}

// roleCapabilities finds the definition of the starship role named role in the bridge's .claude/agents, the file Claude Code uses, and reads its capabilities. It returns nil capabilities and no error when no file there is that role: a Claude Code built-in or a user's own agent. A file that names the role by its file name but can't be read or parsed, or a second definition of the role, is an error.
func roleCapabilities(bridge, role string) (*Capabilities, error) {
	files, err := filesIn(filepath.Join(bridge, ".claude", "agents"), ".md")
	if err != nil {
		return nil, err
	}
	var found *Capabilities
	for _, f := range files {
		stem := strings.TrimSuffix(filepath.Base(f), ".md")
		data, err := os.ReadFile(f)
		if err != nil {
			if stem == role {
				return nil, err
			}
			continue
		}
		name, err := agentName(f, data)
		if err != nil {
			if stem == role {
				return nil, err
			}
			continue
		}
		if name != role {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("more than one file in %s defines the role %s", filepath.Dir(f), role)
		}
		if found, err = parseCapabilities(f, data); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// hookRoleCapabilities is the capabilities of the starship role a hook call comes from: nil for the session, for an agent with no role file in the bridge's .claude/agents, and when there is no bridge.
func hookRoleCapabilities(bridge string, in *HookInput) (*Capabilities, error) {
	if in.AgentID == "" || in.AgentType == "" || bridge == "" {
		return nil, nil
	}
	return roleCapabilities(bridge, in.AgentType)
}
