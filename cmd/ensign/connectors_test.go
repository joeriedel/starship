package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const readerRules = "connectors:\n  reader:\n    - mcp__supernova_mail__read_message\n"

// connWorld is newWorld with the given text added to bridge.yaml.
func connWorld(t *testing.T, extra string) *world {
	t.Helper()
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\n"+extra)
	return w
}

// mcp sends a connector call shaped as Claude Code sends it; the server's name differs from the one in the tool.
func (w *world) mcp(agent, agentType, tool string) (res map[string]any, denied bool) {
	w.t.Helper()
	in := map[string]any{
		"tool_name":  tool,
		"tool_input": map[string]any{"id": "m1"},
		"mcp_server": map[string]any{"name": "claude.ai Supernova Mail", "source": "claude.ai"},
	}
	if agent != "" {
		in["agent_id"] = agent
	}
	if agentType != "" {
		in["agent_type"] = agentType
	}
	res, code := w.hook("PreToolUse", in)
	if code != 0 {
		w.t.Fatalf("PreToolUse exited %d: %v", code, res)
	}
	return res, dig(res, "hookSpecificOutput", "permissionDecision") == "deny"
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			data, _ := os.ReadFile(p)
			out[p] = string(data)
		}
		return nil
	})
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestConnectorsUndeclaredAllows(t *testing.T) {
	w := connWorld(t, "")
	before := snapshotTree(t, filepath.Dir(w.bridge))
	for _, agent := range []string{"", "a1"} {
		res, denied := w.mcp(agent, "reader", "mcp__supernova_mail__read_message")
		if res != nil || denied {
			t.Fatalf("undeclared bridge should allow with no output: %v", res)
		}
	}
	if !sameTree(before, snapshotTree(t, filepath.Dir(w.bridge))) {
		t.Fatal("an undeclared connector call must create and change nothing")
	}
}

func TestConnectorsDecisions(t *testing.T) {
	w := connWorld(t, readerRules+"  reviewer: []\n")
	const read = "mcp__supernova_mail__read_message"
	for _, c := range []struct {
		name, agent, typ, tool string
		deny                   bool
	}{
		{"session", "", "", read, true},
		{"session started with an agent", "", "reader", read, true},
		{"declared role and tool", "a1", "reader", read, false},
		{"undeclared tool", "a1", "reader", "mcp__supernova_mail__send_message", true},
		{"undeclared role", "a1", "scout", read, true},
		{"nested agent reporting its own role", "a2", "orchestrator", read, true},
		{"coder", "a3", "coder", read, true},
		{"empty type", "a4", "", read, true},
		{"prefixed type", "a5", "starship:reader", read, true},
		{"case differs", "a6", "Reader", read, true},
		{"tool case differs", "a7", "reader", "mcp__supernova_mail__Read_message", true},
		{"role with an empty list", "a8", "reviewer", read, true},
	} {
		res, denied := w.mcp(c.agent, c.typ, c.tool)
		if denied != c.deny {
			t.Errorf("%s: denied=%v, want %v (%v)", c.name, denied, c.deny, res)
		}
		if !c.deny && res != nil {
			t.Errorf("%s: an allow must give no output: %v", c.name, res)
		}
	}
	res, _ := w.mcp("a1", "reader", "mcp__supernova_mail__send_message")
	if r := dig(res, "hookSpecificOutput", "permissionDecisionReason"); !strings.Contains(r, "mcp__supernova_mail__send_message") || !strings.Contains(r, "reader") {
		t.Errorf("the reason should name the tool and role: %q", r)
	}
}

func TestConnectorsInvalidDeclarationsDeny(t *testing.T) {
	for name, extra := range map[string]string{
		"wildcard":    "connectors:\n  reader:\n    - mcp__supernova_mail__*\n",
		"bare server": "connectors:\n  reader:\n    - mcp__supernova_mail\n",
		"not mcp":     "connectors:\n  reader:\n    - read_message\n",
		"coder":       "connectors:\n  coder:\n    - mcp__supernova_mail__read_message\n",
		"coder empty": "connectors:\n  coder: []\n",
		"broken yaml": "connectors: [\n",
	} {
		w := connWorld(t, extra)
		res, denied := w.mcp("a1", "reader", "mcp__supernova_mail__read_message")
		if !denied {
			t.Errorf("%s: should deny: %v", name, res)
		}
	}
}

func TestConnectorsDenyLeavesRunAlone(t *testing.T) {
	w := connWorld(t, readerRules)
	lead := w.startLead()
	before := snapshotTree(t, filepath.Dir(w.bridge))
	for _, c := range [][2]string{{"", ""}, {lead, "orchestrator"}} {
		if _, denied := w.mcp(c[0], c[1], "mcp__supernova_mail__read_message"); !denied {
			t.Fatalf("%q should be denied", c[1])
		}
	}
	if !sameTree(before, snapshotTree(t, filepath.Dir(w.bridge))) {
		t.Fatal("a connector denial must not touch run state")
	}
}

func TestConnectorSettingsGeneration(t *testing.T) {
	if hookSettingsFor(false, nil) != hookSettings() || hookSettingsFor(false, []string{"mcp__a__b"}) != hookSettings() {
		t.Fatal("undeclared settings must be exactly hookSettings()")
	}
	s := hookSettingsFor(true, []string{"mcp__b__z", "mcp__a__y", "mcp__b__z"})
	if !strings.Contains(s, `"matcher": "Agent|Task|Edit|Write|NotebookEdit|SubagentHandback|mcp__.*"`) {
		t.Fatalf("matcher missing:\n%s", s)
	}
	if !strings.Contains(s, `"allow": ["Bash(ensign:*)", "mcp__a__y", "mcp__b__z"]`) {
		t.Fatalf("allow should be sorted and deduplicated after the Bash rule:\n%s", s)
	}
	empty := hookSettingsFor(true, nil)
	if !strings.Contains(empty, "|mcp__.*") || !strings.Contains(empty, `"allow": ["Bash(ensign:*)"]`) {
		t.Fatalf("an empty declaration still turns the matcher on:\n%s", empty)
	}
}

func TestConnectorSettingsUpgrade(t *testing.T) {
	b := t.TempDir()
	f := filepath.Join(b, ".claude", "settings.json")
	read := func() string { data, _ := os.ReadFile(f); return string(data) }
	rules := func(tools ...string) *BridgeFile {
		return &BridgeFile{Connectors: map[string][]string{"reader": tools}}
	}
	step := func(name string, bf *BridgeFile, want string) {
		t.Helper()
		err := upgradeHookSettings(b, bf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if read() != want {
			t.Fatalf("%s: settings:\n%s", name, read())
		}
	}
	write(t, f, hookSettings())
	step("undeclared and current", &BridgeFile{}, hookSettings())
	step("add", rules("mcp__a__x"), hookSettingsFor(true, []string{"mcp__a__x"}))
	step("current", rules("mcp__a__x"), hookSettingsFor(true, []string{"mcp__a__x"}))
	step("change", rules("mcp__a__y"), hookSettingsFor(true, []string{"mcp__a__y"}))
	step("empty list", rules(), hookSettingsFor(true, nil))
	step("remove", &BridgeFile{}, hookSettings())
	write(t, f, hookSettingsWith(0, 0))
	step("older form, undeclared", &BridgeFile{}, hookSettings())
	write(t, f, hookSettingsWith(0, 0))
	step("older form, declared", rules("mcp__a__x"), hookSettingsFor(true, []string{"mcp__a__x"}))

	edited := strings.Replace(hookSettings(), "ensign:*", "ensign:*\", \"Bash(make:*)", 1)
	write(t, f, edited)
	step("hand-edited, undeclared", &BridgeFile{}, edited)
	if err := upgradeHookSettings(b, rules("mcp__a__x")); err == nil || !strings.Contains(err.Error(), "mcp__.*") || !strings.Contains(err.Error(), "mcp__a__x") {
		t.Fatalf("a hand-edited file without the matcher and tool should be reported: %v", err)
	}
	complete := strings.Replace(edited, "SubagentHandback", "SubagentHandback|mcp__.*", 1)
	complete = strings.Replace(complete, "Bash(make:*)", "Bash(make:*)\", \"mcp__a__x", 1)
	write(t, f, complete)
	step("hand-edited, declared and complete", rules("mcp__a__x"), complete)
	if err := upgradeHookSettings(b, rules("mcp__a__x", "mcp__a__z")); err == nil || !strings.Contains(err.Error(), "mcp__a__z") {
		t.Fatalf("a missing tool should be reported: %v", err)
	}
	if err := upgradeHookSettings(b, rules("mcp__a__*")); err == nil {
		t.Fatal("an invalid declaration is an error")
	}
}

func TestConnectorSettingsShapeAndLineEndings(t *testing.T) {
	b := t.TempDir()
	f := filepath.Join(b, ".claude", "settings.json")
	read := func() string { data, _ := os.ReadFile(f); return string(data) }
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	rules := &BridgeFile{Connectors: map[string][]string{"reader": {"mcp__a__x"}}}
	declared := hookSettingsFor(true, []string{"mcp__a__x"})

	handAdded := strings.Replace(hookSettings(), `"Bash(ensign:*)"`, `"Bash(ensign:*)", "mcp__supernova_drive__search"`, 1)
	write(t, f, handAdded)
	if err := upgradeHookSettings(b, &BridgeFile{}); err != nil || read() != handAdded {
		t.Fatalf("a hand-added mcp__ allow entry on an undeclared bridge must be left alone: %v\n%s", err, read())
	}
	if err := upgradeHookSettings(b, rules); err == nil || !strings.Contains(err.Error(), "mcp__.*") {
		t.Fatalf("the same file on a declared bridge lacks the matcher: %v", err)
	}
	if read() != handAdded {
		t.Fatalf("the file must not change:\n%s", read())
	}

	write(t, f, crlf(hookSettingsWith(hookTimeout, 0)))
	if err := upgradeHookSettings(b, rules); err != nil || read() != declared {
		t.Fatalf("a CRLF known form on a declared bridge is rewritten: %v\n%s", err, read())
	}
	write(t, f, crlf(declared))
	if err := upgradeHookSettings(b, rules); err != nil || read() != crlf(declared) {
		t.Fatalf("a CRLF file equal to the wanted form isn't written: %v", err)
	}
	old := crlf(hookSettingsWith(0, 0))
	write(t, f, old)
	if err := upgradeHookSettings(b, &BridgeFile{}); err != nil || read() != old {
		t.Fatalf("an undeclared CRLF file without connector parts isn't written: %v", err)
	}
}

func TestConnectorSessionStartPullsDeclaration(t *testing.T) {
	needGit(t)
	w := connWorld(t, "")
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	remote := journalFixture(t, w)
	other := cloneRemote(t, remote)
	runGit(t, other, "config", "user.email", "t@example.com")
	runGit(t, other, "config", "user.name", "t")
	yaml := "control-plane:\n  path: " + repoRoot(t) + "\n" + readerRules
	write(t, filepath.Join(other, "bridge.yaml"), yaml)
	write(t, filepath.Join(other, ".claude", "settings.json"), hookSettingsFor(true, []string{"mcp__supernova_mail__read_message"}))
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-q", "-m", "declare connectors")
	runGit(t, other, "push", "-q", "origin", "main")

	res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
	if code != 0 || strings.Contains(dig(res, "systemMessage"), "not ready") {
		t.Fatalf("SessionStart: %d %v", code, res)
	}
	got, _ := os.ReadFile(filepath.Join(w.bridge, ".claude", "settings.json"))
	if string(got) != hookSettingsFor(true, []string{"mcp__supernova_mail__read_message"}) {
		t.Fatalf("a declaration pulled by the sync must keep its settings:\n%s", got)
	}
	if show := dig(res, "systemMessage"); !strings.Contains(show, "restart claude") {
		t.Fatalf("a pulled declaration needs the restart note: %q", show)
	}
	if ctx := dig(res, "hookSpecificOutput", "additionalContext"); !strings.Contains(ctx, "restarted") {
		t.Fatalf("a pulled declaration needs the restart note in the context: %q", ctx)
	}
	res, _ = w.hook("SessionStart", map[string]any{"source": "startup"})
	if show := dig(res, "systemMessage"); strings.Contains(show, "restart") {
		t.Fatalf("no restart note when nothing changed: %q", show)
	}
}

func TestConnectorSessionStartNotReady(t *testing.T) {
	w := connWorld(t, readerRules)
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(w.bridge, ".claude", "settings.json")
	start := func() (string, string) {
		res, code := w.hook("SessionStart", map[string]any{"source": "startup"})
		if code != 0 {
			t.Fatalf("SessionStart exited %d", code)
		}
		return dig(res, "systemMessage"), dig(res, "hookSpecificOutput", "additionalContext")
	}
	edited := strings.Replace(hookSettings(), "ensign:*", "ensign:*\", \"Bash(make:*)", 1)
	write(t, f, edited)
	if show, ctx := start(); !strings.Contains(show, "not ready") || strings.Contains(ctx, "STARSHIP-READY") || !strings.Contains(ctx, "mcp__supernova_mail__read_message") {
		t.Fatalf("a hand-edited file that can't enforce the rules must be not ready: %q %q", show, ctx)
	}
	write(t, f, hookSettings())
	show, ctx := start()
	if strings.Contains(show, "not ready") || !strings.Contains(ctx, "STARSHIP-READY") || !strings.Contains(show, "restart claude") || !strings.Contains(ctx, "restarted") {
		t.Fatalf("a rewrite of the connector settings should be ready and ask for a restart: %q %q", show, ctx)
	}
	if got, _ := os.ReadFile(f); string(got) != hookSettingsFor(true, []string{"mcp__supernova_mail__read_message"}) {
		t.Fatalf("settings not rewritten:\n%s", got)
	}
	if show, _ := start(); strings.Contains(show, "restart") {
		t.Fatalf("no restart note when nothing changed: %q", show)
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\nconnectors:\n  reader:\n    - mcp__supernova_mail__*\n")
	if show, _ := start(); !strings.Contains(show, "not ready") {
		t.Fatalf("an invalid declaration must be not ready: %q", show)
	}
}

func scoutRole(name, front, nl string) string {
	return strings.ReplaceAll("---\nname: "+name+"\ndescription: Reads.\n"+front+"---\n\nBody.\n", "\n", nl)
}

func TestConnectorsRefuseMcpServersAtLink(t *testing.T) {
	for name, c := range map[string]struct {
		role    string
		refused bool
	}{
		"plain":       {scoutRole("scout", "mcpServers: [supernova_mail]\n", "\n"), true},
		"quoted key":  {scoutRole("scout", "\"mcpServers\": [supernova_mail]\n", "\n"), true},
		"crlf":        {scoutRole("scout", "mcpServers:\n  - supernova_mail\n", "\r\n"), true},
		"body only":   {"---\nname: scout\ndescription: Reads.\n---\n\nSet mcpServers: [x] in frontmatter.\n", false},
		"no mention":  {scoutRole("scout", "", "\n"), false},
		"unparseable": {"---\nname: [scout\n---\n", true},
	} {
		for _, declared := range []bool{true, false} {
			extra := ""
			if declared {
				extra = readerRules
			}
			w := connWorld(t, extra)
			write(t, filepath.Join(w.bridge, "roles", "scout.md"), c.role)
			err := link(w.bridge, true, nil)
			if want := declared && c.refused; (err != nil) != want {
				t.Errorf("%s declared=%v: err=%v, want refused=%v", name, declared, err, want)
			}
			if err != nil && (!strings.Contains(err.Error(), "scout") || !strings.Contains(err.Error(), "scout.md")) {
				t.Errorf("%s: the error should name the role and its source: %v", name, err)
			}
		}
	}
}

func TestConnectorsRefuseMcpServersAtDispatch(t *testing.T) {
	cases := map[string]struct {
		file, content, role string
		refused             bool
	}{
		"file name":        {"scout.md", scoutRole("scout", "mcpServers: [supernova_mail]\n", "\n"), "scout", true},
		"frontmatter name": {"other.md", scoutRole("scout", "mcpServers: [supernova_mail]\n", "\n"), "scout", true},
		"quoted key":       {"scout.md", scoutRole("scout", "\"mcpServers\": [supernova_mail]\n", "\n"), "scout", true},
		"crlf":             {"scout.md", scoutRole("scout", "mcpServers:\n  - supernova_mail\n", "\r\n"), "scout", true},
		"unparseable":      {"scout.md", "---\nname: [scout\n---\n", "scout", true},
		"body mention":     {"scout.md", "---\nname: scout\n---\n\nmcpServers: [x]\n", "scout", false},
		"another role":     {"scout.md", scoutRole("scout", "mcpServers: [supernova_mail]\n", "\n"), "reader", false},
	}
	for name, c := range cases {
		for _, declared := range []bool{true, false} {
			for _, withRun := range []bool{false, true} {
				extra := ""
				if declared {
					extra = readerRules
				}
				w := connWorld(t, extra)
				write(t, filepath.Join(w.bridge, ".claude", "agents", c.file), c.content)
				caller, callerType := "", ""
				if withRun {
					caller, callerType = w.startLead(), "orchestrator"
				}
				in := map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": c.role, "prompt": "x", "description": "x"}}
				if caller != "" {
					in["agent_id"], in["agent_type"] = caller, callerType
				}
				res, code := w.hook("PreToolUse", in)
				if code != 0 {
					t.Fatalf("%s: exited %d", name, code)
				}
				denied := dig(res, "hookSpecificOutput", "permissionDecision") == "deny"
				if want := declared && c.refused; denied != want {
					t.Errorf("%s declared=%v run=%v: denied=%v, want %v (%v)", name, declared, withRun, denied, want, res)
				}
				if denied && !strings.Contains(dig(res, "hookSpecificOutput", "permissionDecisionReason"), "mcpServers") {
					t.Errorf("%s: the reason should name mcpServers", name)
				}
			}
		}
	}
}
