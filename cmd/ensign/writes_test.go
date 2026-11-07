// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

const (
	writesYAML = "connector_writes:\n  - mcp__supernova_wiki__post\n  - mcp__supernova_mail__send\n"
	wikiPost   = "mcp__supernova_wiki__post"
	mailSend   = "mcp__supernova_mail__send"
	// A body with newlines, quotes, backslashes and non-ASCII, and an integer float64 can't hold.
	postA = `{"title":"Release notes","body":"line1\nline \"two\" \\ back\n ünï 日本 😀","id":12345678901234567890,"tags":["a","b"]}`
	// The same value with the keys reordered and whitespace added.
	postAReordered = `{ "tags": ["a", "b"], "id": 12345678901234567890, "body": "line1\nline \"two\" \\ back\n ünï 日本 😀", "title": "Release notes" }`
)

func parseBridge(t *testing.T, text string) *BridgeFile {
	t.Helper()
	var bf BridgeFile
	if err := yaml.Unmarshal([]byte(text), &bf); err != nil {
		t.Fatal(err)
	}
	return &bf
}

func writesWorld(t *testing.T) *world { return connWorld(t, writesYAML) }

func (w *world) runWrites(stdin string, args ...string) (string, error) {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdWrites(args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func (w *world) addWrite(tool, input string) string {
	w.t.Helper()
	out, err := w.runWrites(input, "add", "--tool", tool)
	if err != nil {
		w.t.Fatalf("writes add: %v", err)
	}
	return out
}

// callTool sends a connector call whose tool_input is the given JSON text, as the hook receives it.
func (w *world) callTool(agent, agentType, tool, input string) (denied bool, reason string) {
	w.t.Helper()
	data := fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":%q,"tool_name":%q,"tool_input":%s`, w.sid, tool, input)
	if agent != "" {
		data += fmt.Sprintf(`,"agent_id":%q,"agent_type":%q`, agent, agentType)
	}
	var out, errb bytes.Buffer
	if code := cmdHook("PreToolUse", strings.NewReader(data+"}"), &out, &errb); code != 0 {
		w.t.Fatalf("PreToolUse exited %d: %s", code, errb.String())
	}
	var res map[string]any
	if out.Len() > 0 {
		json.Unmarshal(out.Bytes(), &res)
	}
	return dig(res, "hookSpecificOutput", "permissionDecision") == "deny", dig(res, "hookSpecificOutput", "permissionDecisionReason")
}

func (w *world) replyStop(msg string, active bool) map[string]any {
	w.t.Helper()
	res, _ := w.hook("Stop", map[string]any{"last_assistant_message": msg, "stop_hook_active": active})
	return res
}

func (w *world) say(p string) string {
	w.t.Helper()
	res, _ := w.hook("UserPromptSubmit", map[string]any{"prompt": p})
	return dig(res, "hookSpecificOutput", "additionalContext")
}

// approveWrites shows the pending listing in a stopped turn and answers go.
func (w *world) approveWrites() {
	w.t.Helper()
	list, err := w.runWrites("")
	if err != nil {
		w.t.Fatal(err)
	}
	if res := w.replyStop("Pending:\n"+list+"\nGo?", false); res != nil {
		w.t.Fatalf("a reply showing the listing must not be blocked: %v", res)
	}
	if ctx := w.say("go"); !strings.Contains(ctx, "connector write(s) approved") {
		w.t.Fatalf("go did not approve: %q", ctx)
	}
}

func (w *world) writesState() *writesFile {
	w.t.Helper()
	wf := &writesFile{}
	if err := readJSON(writesPath(w.bridge, w.sid), wf); err != nil {
		w.t.Fatal(err)
	}
	return wf
}

func (w *world) runErr(args ...string) error {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	return cmdRun(args, &out)
}

func TestConnectorWritesValidation(t *testing.T) {
	for _, c := range []struct{ name, yaml, want string }{
		{"exact names", "connector_writes:\n  - mcp__a__b\n", ""},
		{"wildcard", "connector_writes:\n  - mcp__a__*\n", "not an exact"},
		{"whole server", "connector_writes:\n  - mcp__a\n", "not an exact"},
		{"resource tool", "connector_writes:\n  - ListMcpResourcesTool\n", "not an exact"},
		{"also read", "connectors:\n  scout:\n    - mcp__a__b\nconnector_writes:\n  - mcp__a__b\n", "also listed under connectors"},
		{"not a list", "connector_writes: mcp__a__b\n", "must be a list"},
	} {
		_, err := parseBridge(t, c.yaml).connectorRules()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	for yamlText, want := range map[string]bool{"": false, "connectors:\n  reader: []\n": false, "connector_writes: []\n": true, "connector_writes:\n": true} {
		if _, on, err := parseBridge(t, yamlText).connectorWrites(); on != want || err != nil {
			t.Errorf("%q: on = %v, %v, want %v", yamlText, on, err, want)
		}
	}
}

func TestConnectorWritesOffChangesNothing(t *testing.T) {
	w := connWorld(t, readerRules)
	if denied, reason := w.callTool("", "", wikiPost, postA); !denied || !strings.Contains(reason, "may not call") || strings.Contains(reason, "writes") {
		t.Fatalf("with the key absent the session is denied as before: %v %q", denied, reason)
	}
	if _, err := w.runWrites(postA, "add", "--tool", wikiPost); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("add must be refused while the feature is off: %v", err)
	}
	w.run("supernova/auth-refactor")
	if ctx := w.say("go"); !strings.Contains(ctx, "approved") {
		t.Fatalf("go must still approve the run: %q", ctx)
	}
	if _, err := os.Stat(writesPath(w.bridge, w.sid)); err == nil {
		t.Fatal("no writes file may appear while the feature is off")
	}
	if denied, _ := connWorld(t, "").callTool("", "", wikiPost, postA); denied {
		t.Fatal("a bridge with no connector declaration leaves connector calls alone")
	}
}

func TestConnectorWritesSettings(t *testing.T) {
	bf := parseBridge(t, writesYAML)
	writes, _, _ := bf.connectorWrites()
	want := hookSettingsFor(true, settingsTools(nil, writes))
	if !strings.Contains(want, `|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool|Read|Grep"`) || !strings.Contains(want, `"allow": ["Bash(ensign:*)", "mcp__supernova_mail__send", "mcp__supernova_wiki__post"]`) {
		t.Fatalf("matcher and allow:\n%s", want)
	}
	if hookSettings() == want {
		t.Fatal("turning writes on must change the settings")
	}
	b := t.TempDir()
	f := filepath.Join(b, ".claude", "settings.json")
	read := func() string { data, _ := os.ReadFile(f); return string(data) }
	step := func(name string, bf *BridgeFile, want string) {
		t.Helper()
		if err := upgradeHookSettings(b, bf); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if read() != want {
			t.Fatalf("%s: settings:\n%s", name, read())
		}
	}
	write(t, f, hookSettings())
	step("on", bf, want)
	step("current", bf, want)
	step("off", &BridgeFile{}, hookSettings())
	write(t, f, hookSettingsWith(0, 0))
	step("older form, on", bf, want)
	step("empty list", parseBridge(t, "connector_writes: []\n"), hookSettingsFor(true, nil))
	mixed := parseBridge(t, "connectors:\n  reader:\n    - mcp__supernova_mail__read_message\n"+writesYAML)
	step("with reads", mixed, hookSettingsFor(true, []string{"mcp__supernova_mail__read_message", mailSend, wikiPost}))

	edited := strings.Replace(hookSettings(), `"Bash(ensign:*)"`, `"Bash(ensign:*)", "Bash(make:*)"`, 1)
	write(t, f, edited)
	if err := upgradeHookSettings(b, bf); err == nil || !strings.Contains(err.Error(), wikiPost+" in permissions.allow") {
		t.Fatalf("a hand-edited file lacking the write tools is reported: %v", err)
	}
}

func TestConnectorWritesOnlyBridgeRefusesMcpServers(t *testing.T) {
	w := writesWorld(t)
	write(t, filepath.Join(w.bridge, ".claude", "agents", "scout.md"), "---\nname: scout\nmcpServers:\n  x: {}\n---\nbody\n")
	in := &HookInput{ToolInput: map[string]any{"subagent_type": "scout"}}
	if why := mcpServersRefusal(w.bridge, in); !strings.Contains(why, "mcpServers") {
		t.Fatalf("a bridge declaring only connector_writes refuses mcpServers roles: %q", why)
	}
	write(t, filepath.Join(w.bridge, "bridge.yaml"), "control-plane:\n  path: "+repoRoot(t)+"\n")
	if why := mcpServersRefusal(w.bridge, in); why != "" {
		t.Fatalf("with the key absent nothing changes: %q", why)
	}
}

func TestWriteDecisionTable(t *testing.T) {
	w := connWorld(t, "connectors:\n  reader:\n    - mcp__supernova_mail__read_message\n"+writesYAML)
	for _, c := range []struct {
		name, agent, agentType, tool, want string
	}{
		{"agent, listed read", "a1", "reader", "mcp__supernova_mail__read_message", ""},
		{"agent, write tool", "a1", "reader", wikiPost, "session only"},
		{"agent, undeclared", "a1", "reader", "mcp__supernova_other__thing", "session only"},
		{"session, declared read", "", "", "mcp__supernova_mail__read_message", "may not call"},
		{"session, resource tool", "", "", listResourcesTool, "may not call"},
		{"session, undeclared", "", "", "mcp__supernova_other__thing", "counts as a write"},
		{"session, unapproved write", "", "", wikiPost, "ensign writes add"},
	} {
		denied, reason := w.callTool(c.agent, c.agentType, c.tool, postA)
		if (c.want == "") == denied || !strings.Contains(reason, c.want) {
			t.Errorf("%s: denied=%v %q, want %q", c.name, denied, reason, c.want)
		}
	}
	if _, reason := w.callTool("", "", "mcp__supernova_other__thing", postA); !strings.Contains(reason, "next session start") {
		t.Errorf("an undeclared tool's reason says its allow entry arrives at the next session start: %q", reason)
	}
	if _, reason := w.callTool("", "", wikiPost, postA); !strings.Contains(reason, "ensign help writes") || !strings.Contains(reason, "repeat this call unchanged") {
		t.Errorf("the reason says what to do next: %q", reason)
	}
	// An agent is denied with an approval present, and the approval stays for the session.
	w.addWrite(wikiPost, postA)
	w.approveWrites()
	if denied, _ := w.callTool("a1", "reader", wikiPost, postA); !denied {
		t.Fatal("an agent may not make an approved write")
	}
	if denied, reason := w.callTool("", "", wikiPost, postA); denied {
		t.Fatalf("the session's approved call passes: %q", reason)
	}
	// Unreadable state fails closed.
	write(t, writesPath(w.bridge, w.sid), "{not json")
	if denied, _ := w.callTool("", "", wikiPost, postA); !denied {
		t.Fatal("unreadable writes state denies")
	}
}

func TestApprovedWriteIsOneExactCall(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, postA)
	if denied, _ := w.callTool("", "", wikiPost, postA); !denied {
		t.Fatal("a registered write is denied until go")
	}
	w.approveWrites()
	if denied, reason := w.callTool("", "", wikiPost, postAReordered); denied {
		t.Fatalf("the same decoded input passes: %q", reason)
	}
	if denied, _ := w.callTool("", "", wikiPost, postA); !denied {
		t.Fatal("one approval is one call")
	}

	for name, changed := range map[string]string{
		"text":    strings.Replace(postA, "Release notes", "Release note", 1),
		"big int": strings.Replace(postA, "12345678901234567890", "12345678901234567891", 1),
		"extra":   strings.Replace(postA, `"tags"`, `"cc":"x","tags"`, 1),
		"missing": strings.Replace(postA, `,"tags":["a","b"]`, "", 1),
	} {
		w.addWrite(wikiPost, postA)
		w.approveWrites()
		denied, reason := w.callTool("", "", wikiPost, changed)
		if !denied || !strings.Contains(reason, "voided") {
			t.Errorf("%s: changed input must be denied and void the rest: %v %q", name, denied, reason)
		}
		if denied, _ := w.callTool("", "", wikiPost, postA); !denied {
			t.Errorf("%s: the voided approval must not pass", name)
		}
	}
	w.addWrite(wikiPost, postA)
	w.approveWrites()
	if denied, _ := w.callTool("", "", mailSend, postA); !denied {
		t.Fatal("a different tool does not use the approval")
	}
}

func TestLargeWriteInput(t *testing.T) {
	w := writesWorld(t)
	body, _ := json.Marshal(strings.Repeat("line \"q\" \\ é\n", 2500))
	input := fmt.Sprintf(`{"body":%s,"n":9007199254740993}`, body)
	if len(input) < 34000 {
		t.Fatal("the input should be large")
	}
	w.addWrite(wikiPost, input)
	w.approveWrites()
	if denied, reason := w.callTool("", "", wikiPost, input); denied {
		t.Fatalf("a large input passes: %q", reason)
	}

	long := strings.Repeat("0123456789", 5000) + "TAIL-END"
	plain, _ := json.Marshal(long)
	other, _ := json.Marshal(long + "\x1b")
	wf := &writesFile{Entries: []*writeEntry{{Tool: wikiPost, Input: json.RawMessage(fmt.Sprintf(`{"a":%s,"b":%s}`, plain, other))}}}
	got := wf.listing()
	if !strings.Contains(got, "\n"+long+"\n```\n") {
		t.Error("the listing must carry a long plain string whole, tail included")
	}
	if !strings.Contains(got, long+`\u001b"`+"\n```\n") {
		t.Error("the listing must carry a long non-plain string whole as JSON, tail included")
	}
}

func TestSeveralWritesUnderOneGo(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"a":1}`)
	w.addWrite(mailSend, `{"a":1}`)
	w.addWrite(wikiPost, `{"a":1}`)
	list, _ := w.runWrites("")
	if !strings.Contains(list, "(3)") || !strings.Contains(list, "3. "+wikiPost) {
		t.Fatalf("several adds build one listing:\n%s", list)
	}
	w.approveWrites()
	for _, tool := range []string{mailSend, wikiPost, wikiPost} {
		if denied, reason := w.callTool("", "", tool, `{"a":1}`); denied {
			t.Fatalf("%s should pass once: %q", tool, reason)
		}
	}
	if denied, _ := w.callTool("", "", wikiPost, `{"a":1}`); !denied {
		t.Fatal("identical writes are approved once each")
	}
}

func TestLaterAddNeedsNewGo(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	w.approveWrites()
	w.callTool("", "", wikiPost, `{"n":1}`)
	w.addWrite(wikiPost, `{"n":2}`)
	if denied, _ := w.callTool("", "", wikiPost, `{"n":2}`); !denied {
		t.Fatal("a write added after go is not approved")
	}
	w.approveWrites()
	if denied, reason := w.callTool("", "", wikiPost, `{"n":2}`); denied {
		t.Fatalf("it passes after its own go: %q", reason)
	}
}

func TestApprovalEndsWithTheTurn(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	w.approveWrites()
	if ctx := w.say("thanks"); !strings.Contains(ctx, "voided") {
		t.Fatalf("the next prompt reports the voided approval: %q", ctx)
	}
	if denied, _ := w.callTool("", "", wikiPost, `{"n":1}`); !denied {
		t.Fatal("an approval does not outlive its turn")
	}
	if ctx := w.say("again"); ctx != "" {
		t.Fatalf("nothing to void, nothing said: %q", ctx)
	}
}

func TestGoNeedsTheShownListing(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	if ctx := w.say("go"); !strings.Contains(ctx, "not shown") {
		t.Fatalf("go without a shown listing approves nothing: %q", ctx)
	}
	list, _ := w.runWrites("")
	w.replyStop(list, false)
	w.addWrite(wikiPost, `{"n":2}`) // the listing the user saw is out of date
	if ctx := w.say("go"); !strings.Contains(ctx, "not shown") {
		t.Fatalf("an out-of-date listing approves nothing: %q", ctx)
	}
	if denied, _ := w.callTool("", "", wikiPost, `{"n":1}`); !denied {
		t.Fatal("nothing was approved")
	}
	// A drop changes the listing too.
	w.approveWrites()
	w.addWrite(wikiPost, `{"n":3}`)
	w.addWrite(wikiPost, `{"n":4}`)
	list, _ = w.runWrites("")
	w.replyStop(list, false)
	w.runWrites("", "drop", "1")
	if w.writesState().ShownDigest == (&writesFile{Entries: w.writesState().Entries}).digest() {
		t.Fatal("a drop must leave the recorded digest stale")
	}
}

func TestStopRecordsOrBlocks(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"body":"hello"}`)
	list, _ := w.runWrites("")
	res := w.replyStop("I registered it.", false)
	if dig(res, "decision") != "block" || !strings.Contains(dig(res, "reason"), "ensign writes") {
		t.Fatalf("a reply without the listing is blocked: %v", res)
	}
	if res := w.replyStop("I registered it.", true); res != nil {
		t.Fatalf("a stop already forced to continue is let through: %v", res)
	}
	if w.writesState().ShownDigest != "" {
		t.Fatal("nothing is recorded for a reply that did not show it")
	}
	if ctx := w.say("go"); !strings.Contains(ctx, "not shown") {
		t.Fatalf("go after an unshown listing: %q", ctx)
	}
	if res := w.replyStop("Here:\r\n"+strings.ReplaceAll(list, "\n", "\r\n"), false); res != nil {
		t.Fatalf("a reply with the listing (CRLF) passes: %v", res)
	}
	if w.writesState().ShownDigest == "" {
		t.Fatal("the shown digest is recorded")
	}
}

func TestRunsAndWritesNeverWaitTogether(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	for _, args := range [][]string{{"supernova/auth-refactor"}} {
		if err := w.runErr(args...); err == nil || !strings.Contains(err.Error(), "writes are waiting for go") {
			t.Fatalf("ensign run is refused while writes are pending: %v", err)
		}
	}
	for _, args := range [][]string{{"do", "something"}, {"run", "supernova/auth-refactor"}} {
		if _, err := w.seq(args...); err == nil || !strings.Contains(err.Error(), "writes are waiting for go") {
			t.Fatalf("sequence %v is refused while writes are pending: %v", args, err)
		}
	}
	w.approveWrites()
	if err := w.runErr("supernova/auth-refactor"); err != nil {
		t.Fatalf("approved writes don't block a run: %v", err)
	}
	if ctx := w.say("go"); !strings.Contains(ctx, "approved") {
		t.Fatalf("go approves the run: %q", ctx)
	}
	w.addWrite(wikiPost, `{"n":2}`) // an approved, running run doesn't block writes

	w = writesWorld(t)
	w.run("supernova/auth-refactor")
	if _, err := w.runWrites(`{"n":1}`, "add", "--tool", wikiPost); err == nil || !strings.Contains(err.Error(), "waiting for go") {
		t.Fatalf("add is refused while a run waits for go: %v", err)
	}
	w = writesWorld(t)
	w.mustSeq("do", "something")
	if _, err := w.runWrites(`{"n":1}`, "add", "--tool", wikiPost); err == nil || !strings.Contains(err.Error(), "waiting for go") {
		t.Fatalf("add is refused while a sequence waits for go: %v", err)
	}
}

func TestGoWithWritesAndRunApprovesNothing(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	list, _ := w.runWrites("")
	w.replyStop(list, false)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	if _, err := createRun(w.bridge, runOpts{task: "supernova/auth-refactor"}, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if ctx := w.say("go"); !strings.Contains(ctx, "also waits for go") {
		t.Fatalf("go with both pending says why it approved nothing: %q", ctx)
	}
	for _, ri := range w.listRuns() {
		if ri.state.Status != "pending" {
			t.Fatalf("the run must not be approved: %s", ri.state.Status)
		}
	}
	if denied, _ := w.callTool("", "", wikiPost, `{"n":1}`); !denied {
		t.Fatal("the write must not be approved")
	}
}

func TestWritesAddAndDrop(t *testing.T) {
	w := writesWorld(t)
	for name, c := range map[string]struct{ tool, stdin, want string }{
		"not declared":  {"mcp__supernova_other__thing", `{}`, "not in connector_writes"},
		"array":         {wikiPost, `[1]`, "JSON object"},
		"trailing data": {wikiPost, `{} {}`, "JSON object"},
		"empty":         {wikiPost, ``, "JSON object"},
	} {
		if _, err := w.runWrites(c.stdin, "add", "--tool", c.tool); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	t.Setenv("STARSHIP_SESSION", "")
	if _, err := w.runWrites(`{}`, "add", "--tool", wikiPost); err == nil || !strings.Contains(err.Error(), "STARSHIP_SESSION") {
		t.Errorf("no session: %v", err)
	}
	t.Setenv("STARSHIP_SESSION", w.sid)
	if out, _ := w.runWrites(""); !strings.Contains(out, "No connector writes are pending") {
		t.Errorf("empty listing: %q", out)
	}
	w.addWrite(wikiPost, `{"n":1}`)
	w.addWrite(mailSend, `{"n":2}`)
	if out, err := w.runWrites("", "drop", "1"); err != nil || strings.Contains(out, wikiPost) || !strings.Contains(out, "1. "+mailSend) {
		t.Fatalf("drop 1: %v\n%s", err, out)
	}
	if _, err := w.runWrites("", "drop", "2"); err == nil {
		t.Fatal("a number past the end is refused")
	}
	if out, err := w.runWrites("", "drop", "--all"); err != nil || !strings.Contains(out, "No connector writes are pending") {
		t.Fatalf("drop --all: %v\n%s", err, out)
	}
	if out, err := w.runWrites("", "drop", "--all"); err != nil || !strings.Contains(out, "No connector writes are pending") {
		t.Fatalf("drop --all with nothing pending: %v\n%s", err, out)
	}
	w.addWrite(wikiPost, `{"n":3}`)
	w.approveWrites()
	if out, err := w.runWrites("", "drop", "--all"); err != nil || !strings.Contains(out, "No connector writes are pending") {
		t.Fatalf("drop --all with only approved entries: %v\n%s", err, out)
	}
	if n := len(w.writesState().Entries); n != 1 {
		t.Fatalf("drop --all must keep approved entries, have %d", n)
	}
}

func TestSameJSON(t *testing.T) {
	dec := func(s string) any {
		v, err := decodeNumbers([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`1.0`, `1`, true},
		{`1e3`, `1000`, true},
		{`12345678901234567890`, `12345678901234567891`, false},
		{`12345678901234567890.0`, `12345678901234567890`, true},
		{`1e999999999999`, `1e999999999998`, false},
		{`1e999999999999`, `1e999999999999`, true},
		{`{"a":[1.0,{"b":1e3}]}`, `{"a":[1,{"b":1000}]}`, true},
		{`{"a":[1.0,{"b":1e3}]}`, `{"a":[1,{"b":1001}]}`, false},
		{`{"a":[12345678901234567890]}`, `{"a":[12345678901234567891]}`, false},
		{`[1,2]`, `[1]`, false},
		{`{"a":1}`, `{"b":1}`, false},
		{`1`, `"1"`, false},
		{`"\u00fc\u65e5"`, `"ü日"`, true},
	} {
		if got := sameJSON(dec(c.a), dec(c.b)); got != c.want {
			t.Errorf("%s vs %s: %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEscapedUnicodeInputMatchesRealCharacters(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"body":"\u00fc\u65e5 \ud83d\ude00","n":1.0}`)
	w.approveWrites()
	if denied, reason := w.callTool("", "", wikiPost, `{"body":"ü日 😀","n":1}`); denied {
		t.Fatalf("escaped input must match the real characters: %q", reason)
	}
}

func TestSessionWritesStateIsNotEditable(t *testing.T) {
	w := writesWorld(t)
	sessions := filepath.Join(w.bridge, ".starship", "sessions")
	edit := func(tool, key, p string) bool {
		w.t.Helper()
		data := fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":%q,"cwd":%q,"tool_name":%q,"tool_input":{%q:%q}}`, w.sid, w.bridge, tool, key, p)
		var out, errb bytes.Buffer
		if code := cmdHook("PreToolUse", strings.NewReader(data), &out, &errb); code != 0 {
			w.t.Fatalf("PreToolUse exited %d: %s", code, errb.String())
		}
		var res map[string]any
		if out.Len() > 0 {
			json.Unmarshal(out.Bytes(), &res)
		}
		return dig(res, "hookSpecificOutput", "permissionDecision") == "deny"
	}
	for _, p := range []string{
		filepath.Join(sessions, w.sid+".writes"),
		filepath.Join(sessions, w.sid+".writes.lock"),
		filepath.Join(sessions, w.sid+".writes.tmp"),
		filepath.Join(sessions, "other.writes"),
		filepath.Join(sessions, "..", "sessions", w.sid+".writes"),
		filepath.Join(".starship", "sessions", w.sid+".writes"),
	} {
		for _, tool := range []string{"Write", "Edit"} {
			if !edit(tool, "file_path", p) {
				t.Errorf("%s of %s must be refused", tool, p)
			}
		}
	}
	if !edit("NotebookEdit", "notebook_path", filepath.Join(sessions, w.sid+".writes")) {
		t.Error("NotebookEdit must be refused")
	}
	if edit("Write", "file_path", filepath.Join(w.bridge, "notes.md")) {
		t.Error("an ordinary bridge file stays allowed")
	}
	goses := []string{"windows"}
	if runtime.GOOS != "windows" {
		goses = append(goses, "darwin")
	}
	for _, goos := range goses {
		in := &HookInput{Cwd: w.bridge, ToolInput: map[string]any{"file_path": strings.ToUpper(filepath.Join(sessions, w.sid+".writes"))}}
		if writesStateEdit(w.bridge, in, goos) == "" {
			t.Errorf("%s: a case-variant path must be refused", goos)
		}
	}
	for _, stream := range []string{"::$DATA", ":stream", "::$DATA:x"} {
		in := &HookInput{Cwd: w.bridge, ToolInput: map[string]any{"file_path": filepath.Join(sessions, w.sid+".writes"+stream)}}
		if writesStateEdit(w.bridge, in, "windows") == "" {
			t.Errorf("windows: a stream suffix %q must be refused", stream)
		}
	}
	for _, folder := range []string{"sessions::$INDEX_ALLOCATION", "sessions:$I30:$INDEX_ALLOCATION"} {
		in := &HookInput{Cwd: w.bridge, ToolInput: map[string]any{"file_path": filepath.Join(filepath.Dir(sessions), folder, w.sid+".writes")}}
		if writesStateEdit(w.bridge, in, "windows") == "" {
			t.Errorf("windows: a stream spelling of the folder %q must be refused", folder)
		}
	}
	in := &HookInput{Cwd: w.bridge, ToolInput: map[string]any{"file_path": "notes.md"}}
	if writesStateEdit(w.bridge, in, "windows") != "" {
		t.Error("windows: an ordinary relative file stays allowed")
	}
}

func TestWindowsAmbiguousPath(t *testing.T) {
	sid := "ABCDEF12-3456"
	for raw, want := range map[string]bool{
		`C:\Users\me\bridge\.starship\sessions\` + sid + `.writes`: false,
		`c:/Users/me/bridge/notes.md`:                              false,
		`.starship\sessions\` + sid + `.writes`:                    false,
		`notes.md`:                                                 false,
		`..\notes.md`:                                              false,
		`\\host\share\x.txt`:                                       false,
		`//host/share/x.txt`:                                       false,
		`\Users\me\bridge\.starship\sessions\` + sid + `.writes`:   true,
		`/Users/me/bridge/.starship/sessions/` + sid + `.writes`:   true,
		`\x`: true,
		`C:.starship\sessions\` + sid + `.writes`: true,
		`C:rel.txt`:                    true,
		`C:`:                           true,
		`\\.\C:\Users\me\bridge\x.txt`: true,
		`C:\bridge\.starship\sessions\ABCDEF~1.WRI::$DATA`:                   true,
		`C:\bridge\.starship\sessions\` + sid + `.writes:stream`:             true,
		`C:\bridge\.starship\SESSIO~1::$INDEX_ALLOCATION\` + sid + `.writes`: true,
		`.starship\sessions\` + sid + `.writes::$DATA`:                       true,
		`1:\x`:   true,
		`C:\a:b`: true,
	} {
		if got := windowsAmbiguousPath(raw); got != want {
			t.Errorf("windowsAmbiguousPath(%q) = %v, want %v", raw, got, want)
		}
	}
	for _, raw := range []string{`\Users\me\x.writes`, `C:rel`, `\\.\C:\x`} {
		in := &HookInput{Cwd: `C:\bridge`, ToolInput: map[string]any{"notebook_path": raw}}
		if why := writesStateEdit(`C:\bridge`, in, "windows"); !strings.Contains(why, `full`) {
			t.Errorf("windows: %q must be refused with the full-path reason, got %q", raw, why)
		}
	}
}

func TestWritesListing(t *testing.T) {
	wf := &writesFile{Entries: []*writeEntry{{Tool: wikiPost, Input: json.RawMessage(
		"{\"z\":\"```fence``` inside\",\"a\":\"plain\\ttext\\nline\",\"hidden\":\"x\\u202ey\",\"trail\":\"x\\n\",\"num\":12345678901234567890,\"list\":[1,\"b\"]}")}}}
	got := wf.listing()
	for _, want := range []string{
		"1. " + wikiPost + "\n\"a\":\n```\nplain\ttext\nline\n```\n",
		"\"z\":\n````\n```fence``` inside\n````\n",
		`"hidden":` + "\n```json\n\"x\\u202ey\"\n```\n",
		`"trail":` + "\n```json\n\"x\\n\"\n```\n",
		"12345678901234567890\n```\n",
		"\"list\":\n```json\n[\n  1,\n  \"b\"\n]\n```\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, `"a":`) > strings.Index(got, `"z":`) || strings.Index(got, `"list":`) > strings.Index(got, `"num":`) {
		t.Errorf("fields are in sorted key order:\n%s", got)
	}
	if got != wf.listing() || wf.digest() != wf.digest() {
		t.Error("the listing is deterministic")
	}
}

func TestWritesListingShowsControlCharacters(t *testing.T) {
	wf := &writesFile{Entries: []*writeEntry{{Tool: wikiPost, Input: json.RawMessage(
		`{"cr":"a\rb","del":"x\u007fy","nel":"x\u0085y"}`)}}}
	got := wf.listing()
	for _, want := range []string{
		"\"cr\":\n```json\n\"a\\rb\"\n```\n",
		"\"del\":\n```json\n\"x\\u007fy\"\n```\n",
		"\"nel\":\n```json\n\"x\\u0085y\"\n```\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
	for _, r := range "\r\u007f\u0085" {
		if strings.ContainsRune(got, r) {
			t.Errorf("the listing must not carry %U raw:\n%q", r, got)
		}
	}
}

func TestWritesListingFallbacks(t *testing.T) {
	for _, input := range []string{`[1,2]`, `{"a":`, `"text"`} {
		wf := &writesFile{Entries: []*writeEntry{{Tool: wikiPost, Input: json.RawMessage(input)}}}
		got := wf.listing()
		if !strings.Contains(got, "1. "+wikiPost+"\n```json\n") || strings.Contains(got, "(no input fields)") {
			t.Errorf("%s: a non-object input is listed as JSON text:\n%s", input, got)
		}
	}
	wf := &writesFile{Entries: []*writeEntry{{Tool: wikiPost, Input: json.RawMessage(`{}`)}}}
	if got := wf.listing(); !strings.Contains(got, "1. "+wikiPost+"\n(no input fields)\n") {
		t.Errorf("an empty object says it has no fields:\n%s", got)
	}
}

func TestStopWithUnreadableWritesState(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	write(t, filepath.Join(w.bridge, ".starship", "sessions", w.sid+".writes"), "not json{")
	res := w.replyStop("nothing", false)
	if dig(res, "decision") != "block" || !strings.Contains(dig(res, "reason"), "could not be read") {
		t.Errorf("an unreadable writes file blocks the stop: %v", res)
	}
	if res := w.replyStop("nothing", true); res != nil {
		t.Errorf("a stop already forced to continue is let through: %v", res)
	}
}

func TestSweepRemovesOldWritesFiles(t *testing.T) {
	w := writesWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	var files []string
	for _, n := range []string{"old.id.writes", "old.id.writes.lock", "orphan.writes.lock"} {
		oldFile(t, filepath.Join(dir, n), oldAge)
		files = append(files, filepath.Join(dir, n))
	}
	young := filepath.Join(dir, "young.writes")
	write(t, young, "{}")
	dropDeadLinks(w.bridge)
	if !noneExist(files) {
		t.Error("an old session's .writes and .writes.lock must be swept")
	}
	if !exists(young) {
		t.Error("a young session's .writes must stay")
	}
}

func TestSweepWritesTmp(t *testing.T) {
	w := writesWorld(t)
	dir := filepath.Join(w.bridge, ".starship", "sessions")
	oldFile(t, filepath.Join(dir, "old.writes.tmp"), oldAge)
	write(t, filepath.Join(dir, "young.writes.tmp"), "")
	oldFile(t, filepath.Join(dir, "held.writes.tmp"), oldAge)
	oldFile(t, filepath.Join(dir, "held.writes.lock"), oldAge)
	l := flock.New(filepath.Join(dir, "held.writes.lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer l.Unlock()
	oldFile(t, filepath.Join(dir, "mixed.writes.tmp"), oldAge)
	oldFile(t, filepath.Join(dir, "mixed.lock"), oldAge)
	write(t, filepath.Join(dir, "mixed"), "")
	dropDeadLinks(w.bridge)
	if exists(filepath.Join(dir, "old.writes.tmp")) || exists(filepath.Join(dir, "old.writes.lock")) || exists(filepath.Join(dir, "old.lock")) {
		t.Error("an old lone .writes.tmp must be swept without leaving or taking other locks")
	}
	if !exists(filepath.Join(dir, "young.writes.tmp")) {
		t.Error("a young .writes.tmp must stay")
	}
	if !exists(filepath.Join(dir, "held.writes.tmp")) {
		t.Error("a .writes.tmp must stay while its .writes.lock is held")
	}
	if !exists(filepath.Join(dir, "mixed.writes.tmp")) {
		t.Error("a fresh file must keep the group young")
	}
}

func TestWindowsDevicePath(t *testing.T) {
	for raw, want := range map[string]bool{
		`\\?\Volume{1234}\bridge\x`: true,
		`\\.\Volume{1234}\bridge\x`: true,
		`//?/Volume{1234}/bridge/x`: true,
		`//./Volume{1234}/bridge/x`: true,
		`\\?\GLOBALROOT\Device\x`:   true,
		`\\?\unc\srv\share\x`:       true,
		`\\.\C:\x`:                  true,
		`\/?\Volume{1234}\x`:        true,
		`//?\Volume{1234}\x`:        true,
		`\\?\C:\x`:                  false,
		`\\?\UNC\srv\share\x`:       false,
		`\\srv\share\x`:             false,
		`//srv/share/x`:             false,
		`C:\x`:                      false,
		`notes.md`:                  false,
		`..\notes.md`:               false,
	} {
		if got := windowsDevicePath(raw); got != want {
			t.Errorf("windowsDevicePath(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestWritesStateEditDeviceAndUNC(t *testing.T) {
	w := writesWorld(t)
	edit := func(p string) string {
		in := &HookInput{Cwd: w.bridge, ToolInput: map[string]any{"file_path": p}}
		return writesStateEdit(w.bridge, in, "windows")
	}
	for _, p := range []string{
		`\\?\Volume{1234}\bridge\.starship\sessions\ID.writes`,
		`\\.\Volume{1234}\bridge\.starship\sessions\ID.writes`,
		`//?/Volume{1234}/bridge/.starship/sessions/ID.writes`,
		`//./Volume{1234}/bridge/.starship/sessions/ID.writes`,
		`\\?\GLOBALROOT\Device\x\ID.writes`,
		`\\?\unc\srv\share\ID.writes`,
	} {
		if why := edit(p); !strings.Contains(why, "full") {
			t.Errorf("%q must be refused with the full-path reason, got %q", p, why)
		}
	}
	for _, name := range []string{"ID.writes", "ID.writes.lock", "ID.writes.tmp"} {
		for _, p := range []string{
			`\\localhost\C$\bridge\.starship\sessions\` + name,
			`\\127.0.0.1\c$\bridge\.starship\sessions\` + name,
			`//localhost/C$/bridge/.starship/sessions/` + name,
			`\\?\UNC\localhost\C$\bridge\.starship\sessions\` + name,
		} {
			if why := edit(p); !strings.Contains(why, "connector writes state") {
				t.Errorf("%q must be refused as writes state, got %q", p, why)
			}
		}
	}
	if why := edit(`\\nas\share\notes.md`); why != "" {
		t.Errorf("an ordinary UNC file stays allowed, got %q", why)
	}
}

func TestWritesStateEditTrailingDotsAndSpaces(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("trailing dots and spaces are dropped by the Windows path resolver")
	}
	w := writesWorld(t)
	sessions := filepath.Join(w.bridge, ".starship", "sessions")
	for _, suffix := range []string{".writes", ".writes.lock", ".writes.tmp"} {
		for _, tail := range []string{".", " ", ". ."} {
			in := &HookInput{Cwd: w.bridge, ToolInput: map[string]any{"file_path": filepath.Join(sessions, w.sid+suffix) + tail}}
			if writesStateEdit(w.bridge, in, "windows") == "" {
				t.Errorf("%q must be refused", in.ToolInput["file_path"])
			}
		}
	}
}

func TestGarbledListingIsNotShown(t *testing.T) {
	for _, garbled := range []string{"caf?", "cafÃ©"} {
		w := writesWorld(t)
		w.addWrite(wikiPost, `{"body":"café"}`)
		list, _ := w.runWrites("")
		bad := strings.Replace(list, "café", garbled, 1)
		if bad == list {
			t.Fatal("the listing must contain the text")
		}
		if res := w.replyStop("Pending:\n"+bad, false); dig(res, "decision") != "block" {
			t.Errorf("%q: a garbled listing must be blocked: %v", garbled, res)
		}
		if w.writesState().ShownDigest != "" {
			t.Errorf("%q: a garbled listing must not be recorded as shown", garbled)
		}
		if ctx := w.say("go"); !strings.Contains(ctx, "not shown") {
			t.Errorf("%q: go must approve nothing: %q", garbled, ctx)
		}
		w.approveWrites()
	}
}

func TestJSONText(t *testing.T) {
	for _, in := range []string{`"s"`, `12345678901234567890`, `1e400`, `1.50`, `true`, `null`, `[1,[2,{"a":[3]}]]`, `{"a":{"b":[1e400,"x"]},"c":null}`} {
		v, err := decodeNumbers([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		got := jsonText(v)
		if got == "" {
			t.Errorf("jsonText(%s) is empty", in)
		}
		for _, lit := range []string{"12345678901234567890", "1e400", "1.50"} {
			if strings.Contains(in, lit) && !strings.Contains(got, lit) {
				t.Errorf("jsonText(%s) = %q lost %s", in, got, lit)
			}
		}
	}
}

func TestWritesAddFailsWhenGoWaitingCannotBeJudged(t *testing.T) {
	w := writesWorld(t)
	write(t, seqPath(w.bridge, w.sid), "{not json")
	if _, err := w.runWrites(`{"n":1}`, "add", "--tool", wikiPost); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Errorf("a corrupt sequence file: %v", err)
	}
	if _, err := os.Stat(writesPath(w.bridge, w.sid)); err == nil {
		t.Error("nothing should have been added")
	}

	w = writesWorld(t)
	dir := plantRun(t, w.bridge, "20240101-000001-supernova", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	_, err := w.runWrites(`{"n":1}`, "add", "--tool", wikiPost)
	wantListErr(t, "writes add", err)
	if _, err := os.Stat(writesPath(w.bridge, w.sid)); err == nil {
		t.Error("nothing should have been added")
	}
}

func TestGoApprovesNoWriteWhenGoWaitingCannotBeJudged(t *testing.T) {
	w := writesWorld(t)
	w.addWrite(wikiPost, `{"n":1}`)
	dir := plantRun(t, w.bridge, "20240101-000001-supernova", "running")
	write(t, filepath.Join(dir, "state.json"), "{not json")
	res, code := w.hook("UserPromptSubmit", map[string]any{"prompt": "go"})
	if code != 2 {
		t.Fatalf("exit %d: %v", code, res)
	}
	for _, e := range w.writesState().Entries {
		if e.Approved {
			t.Error("a write was approved")
		}
	}
}
