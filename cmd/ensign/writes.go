// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/gofrs/flock"
)

// writeEntry is one connector write the session registered: the tool and its complete input as compact JSON. Approved means the user's go covered it and one matching call may still be made.
type writeEntry struct {
	Tool     string          `json:"tool"`
	Input    json.RawMessage `json:"input"`
	Approved bool            `json:"approved,omitempty"`
}

// writesFile is a session's connector writes. ShownDigest is the digest of the pending listing the Stop hook saw in the session's reply.
type writesFile struct {
	Entries     []*writeEntry `json:"entries"`
	ShownDigest string        `json:"shown_digest,omitempty"`
}

func writesPath(bridge, session string) string { return sessionFile(bridge, session) + ".writes" }

// withWrites reads the session's writes under their lock, runs fn, and saves them when fn reports a change.
func withWrites(bridge, session string, fn func(wf *writesFile) (changed bool, err error)) error {
	path := writesPath(bridge, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	l := flock.New(path + ".lock")
	if err := l.Lock(); err != nil {
		return fmt.Errorf("locking the connector writes: %w", err)
	}
	defer l.Unlock()
	wf := &writesFile{}
	if err := readJSON(path, wf); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	changed, err := fn(wf)
	if err != nil || !changed {
		return err
	}
	return writeJSON(path, wf)
}

func (wf *writesFile) pending() []*writeEntry {
	var p []*writeEntry
	for _, e := range wf.Entries {
		if !e.Approved {
			p = append(p, e)
		}
	}
	return p
}

// voidApproved drops the approved entries and returns how many there were.
func (wf *writesFile) voidApproved() int {
	n := len(wf.Entries)
	wf.Entries = slices.DeleteFunc(wf.Entries, func(e *writeEntry) bool { return e.Approved })
	return n - len(wf.Entries)
}

// decodeNumbers decodes one JSON value, keeping numbers as their literal text so large integers compare exactly, and refuses trailing data.
func decodeNumbers(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON value")
	}
	return v, nil
}

// sameJSON compares two values from decodeNumbers: numbers are equal when their literals match or both parse to the same exact value.
func sameJSON(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		rx, okx := new(big.Rat).SetString(string(x))
		ry, oky := new(big.Rat).SetString(string(y))
		return okx && oky && rx.Cmp(ry) == 0
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameJSON(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameJSON(v, w) {
				return false
			}
		}
		return true
	}
	return a == b
}

// invisible reports a rune the listing must show escaped: a control or format character, or a space other than U+0020.
func invisible(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.Is(unicode.Z, r) && r != ' ')
}

// jsonText is v as indented JSON with every invisible character other than the layout's newlines escaped, so nothing in it is hidden.
func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v) // cannot fail: v is a string or a decodeNumbers value, and b is an in-memory buffer
	var out strings.Builder
	for _, r := range strings.TrimRight(b.String(), "\n") {
		switch {
		case r != '\n' && invisible(r):
			for _, u := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(&out, `\u%04x`, u)
			}
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// plainString reports whether a string is shown verbatim: nothing in it is invisible except newlines and tabs, and it does not end in whitespace, which a fence would not show.
func plainString(s string) bool {
	if r := []rune(s); len(r) > 0 && unicode.IsSpace(r[len(r)-1]) {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && invisible(r) {
			return false
		}
	}
	return true
}

// fence wraps text in a code fence longer than any backtick run in it.
func fence(text, info string) string {
	n, run := 3, 0
	for _, r := range text {
		if r == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	return f + info + "\n" + text + "\n" + f + "\n"
}

// listing renders the pending writes: a number, the tool, then every input field in key order with its full value. Nothing is truncated.
func (wf *writesFile) listing() string {
	pend := wf.pending()
	if len(pend) == 0 {
		return "No connector writes are pending.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Connector writes waiting for go (%d):\n", len(pend))
	for i, e := range pend {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, e.Tool)
		v, err := decodeNumbers(e.Input)
		obj, _ := v.(map[string]any)
		if err != nil || obj == nil {
			b.WriteString(fence(jsonText(string(e.Input)), "json"))
			continue
		}
		if len(obj) == 0 {
			b.WriteString("(no input fields)\n")
		}
		for _, k := range sortedKeys(obj) {
			fmt.Fprintf(&b, "%s:\n", jsonText(k))
			if s, ok := obj[k].(string); ok && plainString(s) {
				b.WriteString(fence(s, ""))
			} else {
				b.WriteString(fence(jsonText(obj[k]), "json"))
			}
		}
	}
	return b.String()
}

func (wf *writesFile) digest() string { return sha([]byte(wf.listing())) }

// goWaiting reports whether a run or sequence waits for the user's go in this session, an unclaimed run included.
func goWaiting(bridge, session string) (bool, error) {
	if readSeq(bridge, session).openSeq() != nil {
		return true, nil
	}
	runs, err := listRuns(bridge)
	if err != nil {
		return false, err
	}
	for _, ri := range runs {
		if ri.state.Status == "pending" && !ri.state.queued() && (ri.state.Session == session || ri.state.Session == "") {
			return true, nil
		}
	}
	return false, nil
}

// writesOn reports whether bridge.yaml turns connector writes on.
func writesOn(bridge string) bool {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return false
	}
	_, on, _ := bf.connectorWrites()
	return on
}

// writesPendingRefusal is the error for creating a run or sequence step while the session has connector writes waiting for go; they never wait together. It is nil when the feature is off.
func writesPendingRefusal(bridge, session string) error {
	if session == "" {
		return nil
	}
	if _, err := os.Stat(writesPath(bridge, session)); err != nil || !writesOn(bridge) {
		return nil
	}
	wf := &writesFile{}
	if err := readJSON(writesPath(bridge, session), wf); err != nil {
		return fmt.Errorf("the connector writes can't be read: %v", err)
	}
	if len(wf.pending()) > 0 {
		return errors.New("connector writes are waiting for go, and a run or sequence can't wait with them; have the user approve the writes with go, or drop them (ensign writes drop --all), first")
	}
	return nil
}

// rawInput is the call's tool_input as the hook received it, for comparison with the registered input.
func (in *HookInput) rawInput() []byte {
	if in.rawToolInput != nil {
		return in.rawToolInput
	}
	raw, _ := json.Marshal(in.ToolInput)
	return raw
}

// sessionReadDenial refuses the session a connector read.
func sessionReadDenial(rules map[string][]string, tool string) any {
	if _, ok := rules["reader"]; !ok {
		return denial(fmt.Sprintf("the session may not call %s; connector reads go through a role this bridge declares in bridge.yaml connectors", tool))
	}
	return denial(fmt.Sprintf("the session may not call %s; dispatch a reader with what to look for and work from its facts (ensign help edits, Connector tools)", tool))
}

// writeDecision decides a connector call when connector_writes is declared. An agent may call only what its role lists. The session may call a write tool only as a call the user approved with go: the whole input must equal an approved entry, which one call consumes. A call that matches nothing voids the approvals still unused.
func writeDecision(bridge string, in *HookInput, rules map[string][]string, writes []string) any {
	tool := in.ToolName
	if in.AgentID != "" {
		if slices.Contains(rules[in.AgentType], tool) {
			return nil
		}
		return denial(fmt.Sprintf("role %q may not call %s: bridge.yaml connectors does not list it for that role; connector writes come from the session only (ensign help writes)", in.AgentType, tool))
	}
	if !slices.Contains(writes, tool) {
		if slices.Contains(connectorTools(rules), tool) || !strings.HasPrefix(tool, "mcp__") {
			return sessionReadDenial(rules, tool)
		}
		return denial(fmt.Sprintf("%s is in neither connectors nor connector_writes in bridge.yaml, so it counts as a write and is refused; the user must add it to connector_writes and restart claude (ensign help writes)", tool))
	}
	if in.SessionID == "" {
		return denial(fmt.Sprintf("%s is refused: the hook input has no session, so no approved write can be found", tool))
	}
	got, err := decodeNumbers(in.rawInput())
	if err != nil {
		return denial(fmt.Sprintf("%s is refused: its input can't be read: %v", tool, err))
	}
	matched, voided := false, 0
	err = withWrites(bridge, in.SessionID, func(wf *writesFile) (bool, error) {
		for i, e := range wf.Entries {
			if !e.Approved || e.Tool != tool {
				continue
			}
			if want, err := decodeNumbers(e.Input); err == nil && sameJSON(want, got) {
				wf.Entries = slices.Delete(wf.Entries, i, i+1)
				matched = true
				return true, nil
			}
		}
		voided = wf.voidApproved()
		return voided > 0, nil
	})
	if err != nil {
		return denial(fmt.Sprintf("%s is refused: the approved connector writes can't be read: %v", tool, err))
	}
	if matched {
		return nil
	}
	reason := fmt.Sprintf("%s is a connector write and nothing is sent until the user approves this exact call: register it with `ensign writes add --tool %s` (the complete tool input as one JSON object on stdin), show the user the listing it prints, wait for the user to type go, then repeat this call unchanged. Details: ensign help writes.", tool, tool)
	if voided > 0 {
		reason += fmt.Sprintf(" This call matched no approved write, so the %d approved write(s) not yet used are voided; register them again.", voided)
	}
	return denial(reason)
}

// contextNote adds text to the additionalContext of a UserPromptSubmit reply, making a reply of its own when there is none.
func contextNote(out any, note string) any {
	if m, ok := out.(map[string]any); ok {
		if h, ok := m["hookSpecificOutput"].(map[string]any); ok {
			if old, _ := h["additionalContext"].(string); old != "" {
				note += "\n\n" + old
			}
			h["additionalContext"] = note
			return m
		}
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": note}}
}

// promptReply is a UserPromptSubmit reply carrying text, after the note on voided approvals when there is one.
func promptReply(voided, text string) any {
	out := contextNote(nil, "starship: "+text)
	if voided != "" {
		out = contextNote(out, voided)
	}
	return out
}

// promptWrites runs first at every UserPromptSubmit while connector writes are on. Approvals never outlive the turn they were given for, so any unused one is voided and reported in voided. On go with writes pending it approves them all, if the user was shown exactly the pending listing and no run or sequence also waits for go; handled is then true and text says what happened, and the run and sequence paths must not see this go.
func promptWrites(bridge string, in *HookInput) (voided, text string, handled bool, err error) {
	if in.SessionID == "" {
		return
	}
	if _, serr := os.Stat(writesPath(bridge, in.SessionID)); serr != nil || !writesOn(bridge) {
		return
	}
	isGo := strings.TrimSpace(in.Prompt) == "go"
	var listErr error
	err = withWrites(bridge, in.SessionID, func(wf *writesFile) (bool, error) {
		n := wf.voidApproved()
		changed := n > 0
		if n > 0 {
			voided = fmt.Sprintf("starship: %d approved connector write(s) were not used before this prompt and are voided; an approval lasts only for the turn it was given in. Register them again with ensign writes add if they are still wanted.", n)
		}
		pend := wf.pending()
		if !isGo || len(pend) == 0 {
			return changed, nil
		}
		handled = true
		waiting, lerr := goWaiting(bridge, in.SessionID)
		if lerr != nil {
			listErr = lerr
			return changed, nil
		}
		switch {
		case waiting:
			text = "connector writes are pending and a run or sequence also waits for go, so this go approved nothing. Tell the user; drop the writes (ensign writes drop --all) or discard the run or sequence, then ask for go again."
		case wf.ShownDigest != wf.digest():
			text = "this go approved no connector write: the pending writes were not shown to the user. Run `ensign writes`, reproduce its output verbatim in your reply, and ask the user for go again."
		default:
			for _, e := range pend {
				e.Approved = true
			}
			wf.ShownDigest = ""
			changed = true
			text = fmt.Sprintf("%d connector write(s) approved. Make each call now with exactly the registered input; each approval covers one call, and any left unused when this turn ends are voided.", len(pend))
		}
		return changed, nil
	})
	if err == nil {
		err = listErr
	}
	return
}

// writesStop blocks a turn from ending while pending writes have not been shown to the user: the session's last message must contain the listing verbatim, and then its digest is recorded for go to check. A stop already forced to continue is let through without recording.
func writesStop(bridge string, in *HookInput) any {
	if in.SessionID == "" {
		return nil
	}
	if _, err := os.Stat(writesPath(bridge, in.SessionID)); err != nil || !writesOn(bridge) {
		return nil
	}
	var out any
	err := withWrites(bridge, in.SessionID, func(wf *writesFile) (bool, error) {
		if len(wf.pending()) == 0 || wf.ShownDigest == wf.digest() {
			return false, nil
		}
		msg := strings.ReplaceAll(in.LastMessage, "\r\n", "\n")
		if strings.Contains(msg, strings.TrimRight(wf.listing(), "\n")) {
			wf.ShownDigest = wf.digest()
			return true, nil
		}
		if !in.StopHookActive {
			out = block("connector writes are pending and the user has not been shown them. Run `ensign writes` and reproduce its output verbatim in your reply, then ask the user to type go to approve them or to say what to change.")
		}
		return false, nil
	})
	if err != nil && !in.StopHookActive {
		return block(fmt.Sprintf("the pending connector writes could not be read: %v", err))
	}
	return out
}

// cmdWrites implements `ensign writes [add --tool <name> | drop <n>... | drop --all]`.
func cmdWrites(args []string, stdin io.Reader, out io.Writer) error {
	usage := errors.New("usage: ensign writes [add --tool <name> (the tool input as one JSON object on stdin) | drop <n>... | drop --all]")
	session := os.Getenv("STARSHIP_SESSION")
	if session == "" {
		return errors.New("connector writes belong to a session, and STARSHIP_SESSION is not set")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	show := func(wf *writesFile) { io.WriteString(out, wf.listing()) }
	switch {
	case len(args) == 0:
		wf := &writesFile{}
		if err := readJSON(writesPath(bridge, session), wf); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		show(wf)
		return nil
	case args[0] == "add":
		if len(args) != 3 || args[1] != "--tool" {
			return usage
		}
		if err := movedRefusal(bridge, session); err != nil {
			return err
		}
		return writesAdd(bridge, session, args[2], stdin, show)
	case args[0] == "drop" && len(args) > 1:
		return writesDrop(bridge, session, args[1:], show)
	}
	return usage
}

func writesAdd(bridge, session, tool string, stdin io.Reader, show func(*writesFile)) error {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return err
	}
	writes, on, err := bf.connectorWrites()
	if err != nil {
		return err
	}
	switch {
	case !on:
		return errors.New("connector writes are off: bridge.yaml has no connector_writes:")
	case !slices.Contains(writes, tool):
		return fmt.Errorf("%s is not in connector_writes in bridge.yaml; the user must add it and restart claude", tool)
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	input, err := decodeNumbers(raw)
	if err != nil {
		return fmt.Errorf("the tool input on stdin must be one JSON object: %v", err)
	}
	if _, ok := input.(map[string]any); !ok {
		return errors.New("the tool input on stdin must be one JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return err
	}
	waiting, err := goWaiting(bridge, session)
	if err != nil {
		return err
	}
	if waiting {
		return errors.New("a run or sequence is waiting for go, and connector writes can't wait with it; have the user approve or discard it first")
	}
	return withWrites(bridge, session, func(wf *writesFile) (bool, error) {
		wf.Entries = append(wf.Entries, &writeEntry{Tool: tool, Input: compact.Bytes()})
		show(wf)
		return true, nil
	})
}

func writesDrop(bridge, session string, args []string, show func(*writesFile)) error {
	return withWrites(bridge, session, func(wf *writesFile) (bool, error) {
		pend := wf.pending()
		var drop []*writeEntry
		if len(args) == 1 && args[0] == "--all" {
			drop = pend
		} else {
			for _, a := range args {
				n, err := strconv.Atoi(a)
				if err != nil || n < 1 || n > len(pend) {
					return false, fmt.Errorf("%q is not a pending write number (1 to %d)", a, len(pend))
				}
				drop = append(drop, pend[n-1])
			}
		}
		wf.Entries = slices.DeleteFunc(wf.Entries, func(e *writeEntry) bool { return slices.Contains(drop, e) })
		show(wf)
		return len(drop) > 0, nil
	})
}
