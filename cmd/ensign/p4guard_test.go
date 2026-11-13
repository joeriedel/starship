// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import "testing"

func TestP4Change(t *testing.T) {
	refused := []string{
		"p4 edit a", "p4 -c x change -i", "p4 reopen -c 5 a", "p4 -ztag edit a", "p4 -z tag edit a", "p4 -x list.txt add",
		"p4 change", "p4 changelist -i", "p4 client", "p4 client -i", "p4 workspace -i", "p4 workspace -d x", "p4 workspace", "p4 login", "p4 submit", "p4 sync", "p4 revert a",
		"cd a && p4 edit b", "echo $(p4 add a)", "sh -c 'p4 edit a'", "env X=1 p4 edit a", "/usr/bin/p4 edit a", "p4.exe edit a",
		"p4 -Zfoo edit a", "p4 --future edit a", "p4 -cx edit a", "find . -exec p4 edit {} +",
	}
	for _, c := range refused {
		if p4Change(c) == "" {
			t.Errorf("p4Change(%q) allowed", c)
		}
	}
	allowed := []string{
		"p4 opened", "p4 -ztag fstat a", "p4 diff", "p4 -c x change -o", "p4 client -o", "p4 status", "p4 workspaces", "p4 workspace -o", "p4 changelist -o 5", "p4 login -s", "p4",
		"p4 -c x -p host:1666 opened", "echo \"p4 edit\"", "echo p4edit", "ls", "git log", "p4 describe -s 5",
	}
	for _, c := range allowed {
		if what := p4Change(c); what != "" {
			t.Errorf("p4Change(%q) refused: %s", c, what)
		}
	}
	if remoteChange("p4 edit a") != "" {
		t.Error("remoteChange looks at p4")
	}
	if remoteChange("git push") == "" || p4Change("git push") != "" {
		t.Error("the two guards leak into each other")
	}
}

func TestP4GuardHook(t *testing.T) {
	bash := func(w *world, agent, agentType, cmd string) bool {
		t.Helper()
		in := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}
		if agent != "" {
			in["agent_id"], in["agent_type"] = agent, agentType
		}
		res, code := w.hook("PreToolUse", in)
		if code != 0 {
			t.Fatalf("PreToolUse exited %d: %v", code, res)
		}
		return dig(res, "hookSpecificOutput", "permissionDecision") == "deny"
	}
	refused := []string{"p4 edit a", "p4 -c x change -i", "p4 reopen -c 5 a"}
	allowed := []string{"p4 opened", "p4 -ztag fstat a", "p4 diff"}

	w := p4World(t, "{ kind: p4, client: supernova-ws, changelist: '101' }")
	lead, coder, _ := p4Coder(t, w)
	for _, c := range refused {
		if !bash(w, coder, "coder", c) {
			t.Errorf("coder's %q allowed", c)
		}
		if !bash(w, lead, "orchestrator", c) {
			t.Errorf("lead's %q allowed", c)
		}
		if bash(w, "", "", c) {
			t.Errorf("session's %q denied", c)
		}
	}
	for _, c := range allowed {
		if bash(w, coder, "coder", c) {
			t.Errorf("coder's %q denied", c)
		}
	}

	g := newWorld(t)
	glead, why := g.runStart()
	if glead == "" {
		t.Fatalf("run start refused: %s", why)
	}
	gcoder, why := g.spawn(glead, "orchestrator", "coder")
	if gcoder == "" {
		t.Fatalf("coder refused: %s", why)
	}
	for _, c := range refused {
		if bash(g, gcoder, "coder", c) {
			t.Errorf("git task: coder's %q denied", c)
		}
	}
}
