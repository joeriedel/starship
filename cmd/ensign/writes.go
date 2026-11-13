// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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

// writeEntry is one connector write the session registered: the tool, its complete input saved as compact JSON in File (sha256 FileSHA), and the part of the listing that describes it. Files is every file saved for the write, the input file and the plain-text files of its long texts. Approved means the user's go covered it and one matching call may still be made, with the input file still hashing to ApprovedSHA.
type writeEntry struct {
	Tool        string   `json:"tool"`
	File        string   `json:"file"`
	FileSHA     string   `json:"file_sha"`
	Files       []string `json:"files"`
	Summary     string   `json:"summary"`
	Approved    bool     `json:"approved,omitempty"`
	ApprovedSHA string   `json:"approved_sha,omitempty"`
}

// writesFile is a session's connector writes. ShownDigest is the digest of the pending listing the Stop hook saw in the session's reply.
type writesFile struct {
	Entries     []*writeEntry `json:"entries"`
	ShownDigest string        `json:"shown_digest,omitempty"`
}

func writesPath(bridge, session string) string { return sessionFile(bridge, session) + ".writes" }

// writesFileSession returns the session id of a file saved for a write, <id>.writes.<token>.json (the input) or <id>.writes.<token>.<n>.txt (a long text), the files the gc sweep and the session edit guard treat as writes state.
func writesFileSession(name string) (string, bool) {
	if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".txt") {
		return "", false
	}
	id, rest, ok := strings.Cut(name, ".writes.")
	return id, ok && id != "" && rest != ""
}

// removeFiles deletes the files saved for the write. One that can't be removed is left to the gc sweep.
func (e *writeEntry) removeFiles() {
	for _, f := range e.Files {
		retry(retryWindow, isTransientFileError, func() error { return os.Remove(f) })
	}
}

// allows reports whether a call's decoded input is what the user approved: the input file still has the approved hash and holds the same value.
func (e *writeEntry) allows(got any) bool {
	data, err := readFileRetry(e.File)
	if err != nil || e.ApprovedSHA == "" || sha(data) != e.ApprovedSHA {
		return false
	}
	want, err := decodeNumbers(data)
	return err == nil && sameJSON(want, got)
}

// withWrites reads the session's writes under their lock, runs fn, and saves them when fn reports a change.
func withWrites(bridge, session string, fn func(wf *writesFile) (changed bool, err error)) error {
	path := writesPath(bridge, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	l := flock.New(path + ".lock")
	if err := lockWith(l, "the connector writes"); err != nil {
		var be *lockBusyError
		if errors.As(err, &be) {
			return err
		}
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

// voidApproved drops the approved entries and their files and returns how many there were.
func (wf *writesFile) voidApproved() int {
	n := len(wf.Entries)
	wf.Entries = slices.DeleteFunc(wf.Entries, func(e *writeEntry) bool {
		if e.Approved {
			e.removeFiles()
		}
		return e.Approved
	})
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

// listing renders the pending writes: a number and the tool, then each write's summary, which names the file holding its full input and that file's sha256. The digest of the listing therefore binds the input the user approves.
func (wf *writesFile) listing() string {
	pend := wf.pending()
	if len(pend) == 0 {
		return "No connector writes are pending.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Connector writes waiting for go (%d):\n", len(pend))
	for i, e := range pend {
		fmt.Fprintf(&b, "\n%d. %s\n%s", i+1, e.Tool, e.Summary)
	}
	return b.String()
}

func (wf *writesFile) digest() string { return sha([]byte(wf.listing())) }

// goWaiting reports whether a run or sequence waits for the user's go in this session, an unclaimed run included.
func goWaiting(bridge, session string) (bool, error) {
	sf, err := readSeqErr(bridge, session)
	if err != nil {
		return false, err
	}
	if sf.nextToApprove() != nil {
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

// writeDecision decides a connector call when connector_writes is declared. An agent may call only what its role lists. The session may call a write tool only as a call the user approved with go: the whole input must equal the input file of an approved entry that still has its approved hash, and one call consumes the entry. A call that matches nothing voids the approvals still unused.
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
		return denial(fmt.Sprintf("%s is in neither connectors nor connector_writes in bridge.yaml, so it counts as a write and is refused; when the user asks, the session adds it to connector_writes in bridge.yaml, and its allow entry arrives at the next session start: /compact, /clear or restarting claude (ensign help writes)", tool))
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
			if e.allows(got) {
				e.removeFiles()
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

// promptWrites runs first at every UserPromptSubmit while connector writes are on, before promptDirect, so a go that opens a direct edit still voids them. Approvals never outlive the turn they were given for, so any unused one is voided and reported in voided. On go with writes pending it approves them all, if the user was shown exactly the pending listing and no run or sequence also waits for go; handled is then true and text says what happened, and the run and sequence paths must not see this go.
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
			for i, e := range pend {
				data, rerr := readFileRetry(e.File)
				if rerr != nil || sha(data) != e.FileSHA {
					text = fmt.Sprintf("this go approved no connector write: the input file of write %d (%s) is missing or was changed since it was listed. Drop the writes (ensign writes drop --all) and register them again.", i+1, e.Tool)
					return changed, nil
				}
			}
			for _, e := range pend {
				e.Approved, e.ApprovedSHA = true, e.FileSHA
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

// writesUsage is the usage line of `ensign writes`.
const writesUsage = "usage: ensign writes [add --tool <name> [--short] [--title <text>] [--read <file> [--read-revision <id>]] (the tool input as one JSON object on stdin) | drop <n>... | drop --all]"

// cmdWrites implements `ensign writes [add --tool <name> [--short] [--title <text>] [--read <file> [--read-revision <id>]] | drop <n>... | drop --all]`.
func cmdWrites(args []string, stdin io.Reader, out io.Writer) error {
	usage := errors.New(writesUsage)
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
		tool, o, err := parseAddArgs(args[1:])
		if err != nil {
			return fmt.Errorf("%v\n%s", err, usage)
		}
		if err := movedRefusal(bridge, session); err != nil {
			return err
		}
		return writesAdd(bridge, session, tool, o, stdin, show)
	case args[0] == "drop" && len(args) > 1:
		return writesDrop(bridge, session, args[1:], show)
	}
	return usage
}

// parseAddArgs reads the arguments of `ensign writes add`: the tool and the session's choices for this write. The read file is not opened here.
func parseAddArgs(args []string) (tool string, o *writeOpts, err error) {
	o = &writeOpts{}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if seen[a] {
			return "", nil, fmt.Errorf("%s given twice", a)
		}
		seen[a] = true
		if a == "--short" {
			o.short = true
			continue
		}
		if a != "--tool" && a != "--title" && a != "--read" && a != "--read-revision" {
			return "", nil, fmt.Errorf("unknown argument %q", a)
		}
		if i+1 >= len(args) {
			return "", nil, fmt.Errorf("%s needs a value", a)
		}
		i++
		switch a {
		case "--tool":
			tool = args[i]
		case "--title":
			o.title = args[i]
		case "--read":
			o.readPath = args[i]
		case "--read-revision":
			o.readRev = args[i]
		}
	}
	switch {
	case tool == "":
		return "", nil, errors.New("--tool is required")
	case o.readRev != "" && o.readPath == "":
		return "", nil, errors.New("--read-revision goes with --read")
	}
	return tool, o, nil
}

func writesAdd(bridge, session, tool string, o *writeOpts, stdin io.Reader, show func(*writesFile)) error {
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
		return fmt.Errorf("%s is not in connector_writes in bridge.yaml; when the user asks, the session adds it, and its allow entry arrives at the next session start (/compact, /clear or restarting claude)", tool)
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	input, err := decodeNumbers(raw)
	if err != nil {
		return fmt.Errorf("the tool input on stdin must be one JSON object: %v", err)
	}
	obj, ok := input.(map[string]any)
	if !ok {
		return errors.New("the tool input on stdin must be one JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return err
	}
	if o.readPath != "" {
		data, err := os.ReadFile(o.readPath)
		if err != nil {
			return fmt.Errorf("--read: %v", err)
		}
		o.hasRead, o.readSHA = true, sha(data)
		o.read = strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	waiting, err := goWaiting(bridge, session)
	if err != nil {
		return err
	}
	if waiting {
		return errors.New("a run or sequence is waiting for go, and connector writes can't wait with it; have the user approve or discard it first")
	}
	if df, err := readDirect(bridge, session); err != nil || (df != nil && df.Phase != directOpen) {
		return errors.New("a direct edit request is waiting for go, and connector writes can't wait with it; have the user answer it, or run `ensign direct close`, first")
	}
	return withWrites(bridge, session, func(wf *writesFile) (bool, error) {
		var token [8]byte
		if _, err := rand.Read(token[:]); err != nil {
			return false, err
		}
		base, err := filepath.Abs(sessionFile(bridge, session) + ".writes." + hex.EncodeToString(token[:]))
		if err != nil {
			return false, err
		}
		c := &writeCtx{short: o.short, base: base}
		body, err := c.render(obj, o)
		if err != nil {
			return false, err
		}
		e := &writeEntry{Tool: tool, File: base + ".json", FileSHA: sha(compact.Bytes())}
		e.Summary = body + fmt.Sprintf("Full input: %s (sha256 %s)\n", e.File, e.FileSHA)
		files := map[string][]byte{e.File: compact.Bytes()}
		e.Files = []string{e.File}
		for _, t := range c.texts {
			files[t.path] = []byte(t.text)
			e.Files = append(e.Files, t.path)
		}
		for _, f := range e.Files {
			if err := writeFileAtomic(bridge, f, files[f]); err != nil {
				e.removeFiles()
				return false, err
			}
		}
		wf.Entries = append(wf.Entries, e)
		show(wf)
		return true, nil
	})
}

func writesDrop(bridge, session string, args []string, show func(*writesFile)) error {
	var drop []*writeEntry
	err := withWrites(bridge, session, func(wf *writesFile) (bool, error) {
		pend := wf.pending()
		drop = nil
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
	if err != nil {
		return err
	}
	for _, e := range drop {
		e.removeFiles()
	}
	return nil
}
