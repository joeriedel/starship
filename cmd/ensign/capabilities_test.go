// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// installShippedRoles copies the control plane's role files into the bridge's .claude/agents, as link does for a bridge with no roles of its own.
func installShippedRoles(t *testing.T, bridge string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "roles", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped roles: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(bridge, ".claude", "agents", filepath.Base(f)), string(data))
	}
}

// roleFile is a role definition with the given lines between its name and the closing fence.
func roleFile(name, nl string, lines ...string) string {
	return strings.Join(append(append([]string{"---", "name: " + name}, lines...), "---", "body"), nl) + nl
}

func TestParseCapabilities(t *testing.T) {
	cases := map[string]struct {
		lines []string
		nl    string
		want  Capabilities
		err   string
	}{
		"no may": {want: Capabilities{}},
		"full": {lines: []string{"may:", "  tools: [Read, Bash]", "  background: true", "  agents: [stages, free, supernova-scout]", "  edit: [code, docs, run, secretary, reader]", "  remote: true", "  connectors: true"},
			want: Capabilities{Tools: []string{"Read", "Bash"}, Background: true, Agents: []string{"stages", "free", "supernova-scout"}, Edit: []string{"code", "docs", "run", "secretary", "reader"}, Remote: true, Connectors: true}},
		"block list":        {lines: []string{"may:", "  tools:", "    - Read"}, want: Capabilities{Tools: []string{"Read"}}},
		"crlf":              {nl: "\r\n", lines: []string{"may:", "  tools: [Read]", "  remote: true"}, want: Capabilities{Tools: []string{"Read"}, Remote: true}},
		"empty list":        {lines: []string{"may:", "  tools: []"}, want: Capabilities{}},
		"unknown key":       {lines: []string{"may:", "  tool: [Read]"}, err: "may.tool: unknown key"},
		"unknown edit kind": {lines: []string{"may:", "  edit: [code, bridge]"}, err: `may.edit: unknown folder kind "bridge"`},
		"tools not a list":  {lines: []string{"may:", "  tools: Read, Bash"}, err: "may.tools: must be a list"},
		"tool not a name":   {lines: []string{"may:", "  tools: [Read, 3]"}, err: "may.tools: every entry"},
		"flag not a bool":   {lines: []string{"may:", "  remote: yes please"}, err: "may.remote: must be true or false"},
		"edit not a list":   {lines: []string{"may:", "  edit: code"}, err: "may.edit: must be a list"},
		"may not a mapping": {lines: []string{"may: [Read]"}, err: "may: must be a mapping"},
		"may null":          {lines: []string{"may:"}, err: "may: must be a mapping"},
	}
	for name, c := range cases {
		nl := c.nl
		if nl == "" {
			nl = "\n"
		}
		got, err := parseCapabilities("supernova-scout.md", []byte(roleFile("supernova-scout", nl, c.lines...)))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) || !strings.Contains(err.Error(), "supernova-scout.md") {
				t.Errorf("%s: err = %v, want %q naming the file", name, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !slices.Equal(got.Tools, c.want.Tools) || !slices.Equal(got.Agents, c.want.Agents) || !slices.Equal(got.Edit, c.want.Edit) ||
			got.Background != c.want.Background || got.Remote != c.want.Remote || got.Connectors != c.want.Connectors {
			t.Errorf("%s: got %+v, want %+v", name, *got, c.want)
		}
	}
	if _, err := parseCapabilities("x.md", []byte("no frontmatter")); err == nil {
		t.Error("a file with no frontmatter parsed")
	}
}

func TestLinkRefusesAnInvalidMayBlock(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "roles", "supernova-scout.md"), roleFile("supernova-scout", "\n", "may:", "  edit: [elsewhere]"))
	bf, err := loadBridgeFile(w.bridge)
	if err != nil {
		t.Fatal(err)
	}
	_, err = generatedFiles(w.bridge, bf, repoRoot(t))
	if err == nil || !strings.Contains(err.Error(), "supernova-scout.md") || !strings.Contains(err.Error(), "may.edit") {
		t.Errorf("link error = %v", err)
	}
}

func TestShippedRolesDeclareTheirTools(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(repoRoot(t), "roles", "*.md"))
	if len(files) == 0 {
		t.Fatal("no shipped roles")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		role := strings.TrimSuffix(filepath.Base(f), ".md")
		caps, err := parseCapabilities(f, data)
		if err != nil {
			t.Errorf("%s: %v", role, err)
			continue
		}
		var tools []string
		for _, tool := range strings.Split(strings.TrimSpace(string(data[frontmatterToolsLoc(data)[4]:frontmatterToolsLoc(data)[5]])), ",") {
			tools = append(tools, strings.TrimSpace(tool))
		}
		if !slices.Equal(caps.Tools, tools) {
			t.Errorf("%s: may.tools %v, tools: line %v", role, caps.Tools, tools)
		}
		if caps.Background != slices.Contains(tools, "Bash") {
			t.Errorf("%s: background = %v with tools %v", role, caps.Background, tools)
		}
		if caps.Remote == (role == "explorer" || role == "reviewer") {
			t.Errorf("%s: remote = %v", role, caps.Remote)
		}
		if got, want := slices.Contains(caps.Agents, "stages"), role == "orchestrator"; got != want {
			t.Errorf("%s: agents = %v", role, caps.Agents)
		}
	}
}

// capWorld is a world whose bridge has the shipped roles, with temp folders confined to one.
func capWorld(t *testing.T) *world {
	t.Helper()
	w, _ := newGuardWorld(t)
	installShippedRoles(t, w.bridge)
	return w
}

// capCall sends a PreToolUse from an agent and reports whether it was refused, and why.
func (w *world) capCall(agent, agentType, tool string, input map[string]any) (bool, string) {
	w.t.Helper()
	in := map[string]any{"tool_name": tool, "tool_input": input}
	if agent != "" {
		in["agent_id"], in["agent_type"] = agent, agentType
	}
	res, code := w.hook("PreToolUse", in)
	if code != 0 {
		w.t.Fatalf("%s from %s exited %d: %v", tool, agentType, code, res)
	}
	return dig(res, "hookSpecificOutput", "permissionDecision") == "deny", dig(res, "hookSpecificOutput", "permissionDecisionReason")
}

func TestRoleToolWhitelist(t *testing.T) {
	w := capWorld(t)
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-scout.md"), "---\nname: supernova-scout\ntools: Read, Bash\n---\nbody\n")
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-shell.md"), roleFile("supernova-shell", "\n", "may:", "  tools: [Read, Bash]", "  remote: true"))
	run := map[string]any{"command": "go build ./..."}
	cases := []struct {
		role, tool string
		input      map[string]any
		denied     bool
		why        string
	}{
		{"explorer", "Read", map[string]any{"file_path": "/nonexistent/supernova/a.go"}, false, ""},
		{"explorer", "Bash", run, false, ""},
		{"explorer", "Edit", map[string]any{"file_path": "/tmp/a"}, true, "may.tools"},
		{"explorer", "Write", map[string]any{"file_path": "/tmp/a"}, true, "may.tools"},
		{"explorer", "Agent", map[string]any{"subagent_type": "coder"}, true, "may.tools"},
		{"curator", "Bash", run, true, "may.tools"},
		{"coder", "Agent", map[string]any{"subagent_type": "explorer"}, true, "may.tools"},
		{"coder", "Bash", run, false, ""},
		{"coder", "TaskStop", map[string]any{"task_id": "t1"}, false, ""},
		{"reviewer", "mcp__supernova_mail__read_message", map[string]any{}, false, ""},
		{"coder", "mcp__supernova_mail__read_message", map[string]any{}, true, "may.connectors"},
		// A harness-injected tool passes whatever the definition lists.
		{"curator", "SubagentHandback", map[string]any{"message": "done"}, false, ""},
		{"supernova-shell", "Bash", run, false, ""},
		{"supernova-shell", "Edit", map[string]any{"file_path": "/tmp/a"}, true, "may.tools"},
		// A role file with a tools: line but no may: block may do nothing.
		{"supernova-scout", "Read", map[string]any{"file_path": "/nonexistent/supernova/a.go"}, true, "may.tools"},
		{"supernova-scout", "Bash", run, true, "may.tools"},
		{"supernova-scout", "SubagentHandback", map[string]any{"message": "done"}, false, ""},
		// An agent with no role file, and the session, are not judged by a definition.
		{"general-purpose", "Bash", map[string]any{"command": "ls", "run_in_background": true}, false, ""},
		{"general-purpose", "Edit", map[string]any{"file_path": "/tmp/a"}, false, ""},
	}
	for _, c := range cases {
		denied, why := w.capCall("a1", c.role, c.tool, c.input)
		if denied != c.denied || (c.why != "" && !strings.Contains(why, c.why)) {
			t.Errorf("%s %s: denied=%v (%s), want %v %q", c.role, c.tool, denied, why, c.denied, c.why)
		}
		if denied && (!strings.HasPrefix(why, "starship: ") || !strings.Contains(why, c.role) || !strings.Contains(why, "caller")) {
			t.Errorf("%s %s: refusal wording: %s", c.role, c.tool, why)
		}
	}
	if denied, why := w.capCall("", "", "Bash", map[string]any{"command": "ls", "run_in_background": true}); denied {
		t.Errorf("the session was refused: %s", why)
	}
	if denied, why := w.capCall("", "", "Edit", map[string]any{"file_path": filepath.Join(w.bridge, "notes", "a.md")}); denied {
		t.Errorf("the session was refused: %s", why)
	}
}

func TestRoleDefinitionThatCannotBeCheckedDenies(t *testing.T) {
	w := capWorld(t)
	agents := filepath.Join(w.bridge, ".claude", "agents")
	write(t, filepath.Join(agents, "supernova-scout.md"), roleFile("supernova-scout", "\n", "may:", "  tools: [Read", "  remote: true"))
	write(t, filepath.Join(agents, "supernova-bad.md"), roleFile("supernova-bad", "\n", "may:", "  edit: [elsewhere]"))
	write(t, filepath.Join(agents, "supernova-twin.md"), roleFile("supernova-bad", "\n", "may:", "  tools: [Read]"))
	for _, role := range []string{"supernova-scout", "supernova-bad"} {
		denied, why := w.capCall("a1", role, "Read", map[string]any{"file_path": "/nonexistent/supernova/a.go"})
		if !denied || !strings.Contains(why, "can't be checked") {
			t.Errorf("%s: denied=%v %s", role, denied, why)
		}
	}
}

func TestRoleBackground(t *testing.T) {
	w := capWorld(t)
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-shell.md"), roleFile("supernova-shell", "\n", "may:", "  tools: [Bash]", "  remote: true"))
	bg := map[string]any{"command": "ls", "run_in_background": true}
	for _, c := range []struct {
		role   string
		input  map[string]any
		denied bool
	}{
		{"coder", bg, false},
		{"tester", bg, false},
		{"supernova-shell", map[string]any{"command": "ls"}, false},
		{"supernova-shell", map[string]any{"command": "ls", "run_in_background": false}, false},
		{"supernova-shell", bg, true},
		// The orchestrator's stage runner is a background shell.
		{"orchestrator", map[string]any{"command": "ensign run-stage", "run_in_background": true}, false},
	} {
		denied, why := w.capCall("a1", c.role, "Bash", c.input)
		if denied != c.denied {
			t.Errorf("%s %v: denied=%v (%s)", c.role, c.input, denied, why)
		}
		if denied && !strings.Contains(why, "may.background") {
			t.Errorf("%s: %s", c.role, why)
		}
	}
}

func TestRoleAgents(t *testing.T) {
	w := capWorld(t)
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-lead.md"), roleFile("supernova-lead", "\n", "may:", "  tools: [Agent]", "  agents: [supernova-scout]"))
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-bare.md"), roleFile("supernova-bare", "\n", "may:", "  tools: [Task]"))
	sub := func(role string) map[string]any {
		return map[string]any{"subagent_type": role, "prompt": "x", "description": "x"}
	}
	for _, c := range []struct {
		caller, role string
		denied       bool
	}{
		{"supernova-lead", "supernova-scout", false},
		{"supernova-lead", "coder", true},
		{"supernova-lead", "stages", true},
		{"supernova-bare", "supernova-scout", true},
		{"supernova-bare", "", true},
		// Without a run, "stages" and "free" cover nothing.
		{"orchestrator", "coder", true},
		{"general-purpose", "supernova-scout", false},
	} {
		denied, why := w.capCall("a1", c.caller, "Agent", sub(c.role))
		if denied != c.denied {
			t.Errorf("%s dispatching %q: denied=%v (%s)", c.caller, c.role, denied, why)
		}
		if denied && !strings.Contains(why, "may.agents") && !strings.Contains(why, "agent's run can't be found") {
			t.Errorf("%s dispatching %q: %s", c.caller, c.role, why)
		}
	}
	// In a run the orchestrator's stages and free roles are covered, and anything else is not.
	lead := w.startLead()
	if id, why := w.spawn(lead, "orchestrator", "coder"); id == "" {
		t.Errorf("orchestrator dispatching a stage role: %s", why)
	}
	if id, why := w.spawn(lead, "orchestrator", "supernova-scout"); id != "" || !strings.Contains(why, "may.agents") {
		t.Errorf("orchestrator dispatching an unlisted role: id=%q %s", id, why)
	}
}

func TestRoleEditFolders(t *testing.T) {
	w, tmp := newGuardWorld(t)
	installShippedRoles(t, w.bridge)
	w.docsBinding(`{ paths: [docs, README.md] }`)
	bridgeYAML := filepath.Join(w.bridge, "bridge.yaml")
	write(t, bridgeYAML, "control-plane:\n  path: "+repoRoot(t)+"\nconnectors:\n  reader:\n    - mcp__supernova__read\n")
	ws := w.workspace()
	tmpFile := filepath.Join(tmp, "x")

	// Outside a run the agents that edit anywhere get the temp folder alone.
	for _, role := range []string{"coder", "documentation", "style-reviewer"} {
		w.expectEdit("ghost", role, tmpFile, false)
		w.expectEdit("ghost", role, filepath.Join(ws, "main.go"), true)
	}
	// The reader's and secretary's folders.
	w.expectEdit("rd", "reader", filepath.Join(w.bridge, "reader", "facts.md"), false)
	w.expectEdit("rd", "reader", tmpFile, true)
	w.expectEdit("rd", "reader", bridgeYAML, true)
	w.expectEdit("sec", "secretary", filepath.Join(w.bridge, "notes", "secretary", "a.md"), false)
	w.expectEdit("sec", "secretary", filepath.Join(w.bridge, "notes", "focal", "a.md"), false)
	w.expectEdit("sec", "secretary", filepath.Join(w.bridge, "reader", "facts.md"), true)
	w.expectEdit("sec", "secretary", tmpFile, true)
	// Declared kinds that name nothing the role has no folder for are refused, and so is a role that declares none.
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-scout.md"), "---\nname: supernova-scout\ntools: Edit\n---\n")
	w.expectEdit("a1", "supernova-scout", tmpFile, true)
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-quiet.md"), roleFile("supernova-quiet", "\n", "may:", "  tools: [Edit]"))
	if refused, why := w.tryEdit("a1", "supernova-quiet", tmpFile); !refused || !strings.Contains(why, "may.edit") {
		t.Errorf("a role with no edit folder: refused=%v %s", refused, why)
	}
	write(t, filepath.Join(w.bridge, ".claude", "agents", "supernova-temp.md"), roleFile("supernova-temp", "\n", "may:", "  tools: [Edit]", "  edit: [run]"))
	w.expectEdit("a1", "supernova-temp", tmpFile, false)
	w.expectEdit("a1", "supernova-temp", filepath.Join(ws, "main.go"), true)
	// A reader that bridge.yaml does not declare has no folder.
	write(t, bridgeYAML, "control-plane:\n  path: "+repoRoot(t)+"\n")
	w.expectEdit("rd", "reader", filepath.Join(w.bridge, "reader", "facts.md"), true)

	// In a run.
	lead := w.startLead()
	scratch := w.scratch()
	coder, why := w.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	w.expectEdit(coder, "coder", filepath.Join(ws, "new", "main.go"), false)
	w.expectEdit(coder, "coder", filepath.Join(scratch, "log"), false)
	w.expectEdit(coder, "coder", tmpFile, false)
	w.expectEdit(coder, "coder", bridgeYAML, true)
	w.stop(coder, "coder", report("compiled"), false)
	w.expectEdit(coder, "coder", filepath.Join(ws, "main.go"), true)
}

func TestRoleEditFoldersDocumentationAndStyle(t *testing.T) {
	w, tmp := newGuardWorld(t)
	installShippedRoles(t, w.bridge)
	w.docsBinding(`{ paths: [docs, README.md] }`)
	lead := w.startLead()
	w.toDocs(lead)
	doc, why := w.spawn(lead, "orchestrator", "documentation")
	if doc == "" {
		t.Fatal(why)
	}
	ws := w.workspace()
	w.expectEdit(doc, "documentation", filepath.Join(ws, "docs", "a.md"), false)
	w.expectEdit(doc, "documentation", filepath.Join(ws, "README.md"), false)
	w.expectEdit(doc, "documentation", filepath.Join(ws, "main.go"), true)
	w.expectEdit(doc, "documentation", filepath.Join(w.scratch(), "log"), false)
	w.expectEdit(doc, "documentation", filepath.Join(tmp, "x"), false)

	w2, _ := newGuardWorld(t)
	installShippedRoles(t, w2.bridge)
	w2.project("use: [style-learning]\n")
	lead = w2.startLead()
	coder, why := w2.spawn(lead, "orchestrator", "coder")
	if coder == "" {
		t.Fatal(why)
	}
	w2.stop(coder, "coder", report("compiled"), false)
	w2.leaf(lead, "reviewer", "clean")
	w2.leaf(lead, "reviewer", "clean")
	style, why := w2.spawn(lead, "orchestrator", "style-reviewer")
	if style == "" {
		t.Fatal(why)
	}
	w2.expectEdit(style, "style-reviewer", filepath.Join(w2.workspace(), "main.go"), false)
	w2.expectEdit(style, "style-reviewer", filepath.Join(w2.bridge, "bridge.yaml"), true)
}
