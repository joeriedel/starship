// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// docsBatch reports whether a write's input has the shape of a Google Docs documents.batchUpdate: a string documentId and a requests list. The shape, not the tool name, selects the summary, so it covers the tool of any server (mcp__<server>__update_doc and the like).
func docsBatch(obj map[string]any) bool {
	_, id := obj["documentId"].(string)
	_, reqs := obj["requests"].([]any)
	return id && reqs
}

func num(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return i, err == nil
}

func onlyKeys(m map[string]any, keys ...string) bool {
	for k := range m {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return true
}

// docRange reads a Range with a startIndex and an endIndex and nothing else: a segmentId or tabId is a place the summary cannot name, so such a range is not covered.
func docRange(v any) (start, end int64, ok bool) {
	m, _ := v.(map[string]any)
	if m == nil || len(m) != 2 {
		return 0, 0, false
	}
	start, ok1 := num(m["startIndex"])
	end, ok2 := num(m["endIndex"])
	return start, end, ok1 && ok2 && start >= 0 && start < end
}

// tally counts strings and keeps the order they first appeared in.
type tally struct {
	keys []string
	n    map[string]int
	all  int
}

func (t *tally) add(k string) {
	if t.n == nil {
		t.n = map[string]int{}
	}
	if t.n[k] == 0 {
		t.keys = append(t.keys, k)
	}
	t.n[k]++
	t.all++
}

// kinds is " (A ×1, B ×2)", or nothing for an empty tally.
func (t *tally) kinds() string {
	if t.all == 0 {
		return ""
	}
	parts := make([]string, len(t.keys))
	for i, k := range t.keys {
		parts[i] = fmt.Sprintf("%s ×%d", shown(k), t.n[k])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func countOf(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// docRead is the session's own read of the document, kept as UTF-16 code units because Docs counts positions in them, and edited by the batch's requests in order. Once a request can't be applied, err says why and nothing after it is derived.
//
// Index mapping (Google Docs API reference, developers.google.com/docs/api, as of 2026): the body's first character is at index 1, so index i is the 0-based offset i-1 in the read; an endIndex is exclusive. The read is the body's text with "\n" line ends; it need not carry the final newline, so an insert may be at index len+1. An endOfSegmentLocation insert lands before the final newline when the read ends in one.
type docRead struct {
	u   []uint16
	err string
}

func (r *docRead) fail(format string, a ...any) string {
	if r.err == "" {
		r.err = fmt.Sprintf(format, a...)
	}
	return r.err
}

// splits reports whether offset p lies inside a surrogate pair.
func (r *docRead) splits(p int) bool {
	return p > 0 && p < len(r.u) && r.u[p-1] >= 0xD800 && r.u[p-1] < 0xDC00 && r.u[p] >= 0xDC00 && r.u[p] < 0xE000
}

// span checks the range [start, end) of document indexes against the read and returns it as offsets.
func (r *docRead) span(start, end int64) (int, int, string) {
	if r.err != "" {
		return 0, 0, r.err
	}
	if start < 1 || end > int64(len(r.u))+1 {
		return 0, 0, r.fail("indexes %d to %d fall outside the read (it covers 1 to %d)", start, end, len(r.u)+1)
	}
	s, e := int(start-1), int(end-1)
	if r.splits(s) || r.splits(e) {
		return 0, 0, r.fail("indexes %d to %d split a surrogate pair in the read", start, end)
	}
	return s, e, ""
}

func (r *docRead) insert(idx int64, atEnd bool, text string) string {
	if r.err != "" {
		return r.err
	}
	if atEnd {
		p := len(r.u)
		if p > 0 && r.u[p-1] == '\n' {
			p--
		}
		idx = int64(p + 1)
	}
	p, _, why := r.span(idx, idx)
	if why != "" {
		return why
	}
	r.u = slices.Insert(r.u, p, utf16.Encode([]rune(text))...)
	return ""
}

func (r *docRead) deleteRange(start, end int64) (string, string) {
	s, e, why := r.span(start, end)
	if why != "" {
		return "", why
	}
	gone := string(utf16.Decode(r.u[s:e]))
	r.u = slices.Delete(r.u, s, e)
	return gone, ""
}

// replaceAll applies a replaceAllText and returns what each match was, in first-seen order.
func (r *docRead) replaceAll(search string, matchCase, regex bool, with string) (*tally, string) {
	if r.err != "" {
		return nil, r.err
	}
	if regex {
		return nil, r.fail("a regular-expression replacement is not simulated")
	}
	pat := regexp.QuoteMeta(search)
	if !matchCase {
		pat = "(?i)" + pat
	}
	text := string(utf16.Decode(r.u))
	re := regexp.MustCompile(pat)
	var t tally
	for _, m := range re.FindAllString(text, -1) {
		t.add(m)
	}
	r.u = utf16.Encode([]rune(re.ReplaceAllLiteralString(text, with)))
	return &t, ""
}

// docsStructure is what the batch's formatting requests add up to.
type docsStructure struct {
	paragraphs, styles, bullets tally
	styleRuns                   int
	links                       []string
}

// docs summarizes a Google Docs batch update: the destination, the guard, each text change in request order (inserted text as plain text, every deletion and replacement with what goes) and one line per kind of formatting. A request of a kind the summary does not know, or carrying anything it does not account for, is shown in full as JSON, and so is any other input field.
func (c *writeCtx) docs(b *strings.Builder, obj map[string]any, o *writeOpts) {
	fmt.Fprintf(b, "Destination: Google Doc %s\n", shown(obj["documentId"].(string)))
	if o.title != "" {
		fmt.Fprintf(b, "Title, given by the session and not checked: %s\n", jsonText(o.title))
	}
	skip := map[string]bool{"documentId": true, "requests": true}
	var guards []string
	required := ""
	if wc, _ := obj["writeControl"].(map[string]any); len(wc) > 0 && onlyKeys(wc, "requiredRevisionId", "targetRevisionId") {
		req, ok1 := wc["requiredRevisionId"].(string)
		tgt, ok2 := wc["targetRevisionId"].(string)
		if (ok1 || wc["requiredRevisionId"] == nil) && (ok2 || wc["targetRevisionId"] == nil) {
			skip["writeControl"] = true
			if ok1 {
				guards = append(guards, "guarded by revision "+shown(req)+" (nothing is written if the document has moved on)")
				required = req
			}
			if ok2 {
				guards = append(guards, "aimed at revision "+shown(tgt))
			}
		}
	}
	var rd *docRead
	if o.hasRead {
		rd = &docRead{u: utf16.Encode([]rune(o.read))}
		label := "the session's own read of the document, unverified"
		if o.readRev != "" && o.readRev == required {
			label = "exact: the session read the document at revision " + shown(required) + ", the one this batch requires"
		} else if o.readRev != "" {
			label += fmt.Sprintf(" (declared revision %s, batch guard %s)", shown(o.readRev), map[bool]string{true: shown(required), false: "none"}[required != ""])
		}
		fmt.Fprintf(b, "Before and after, derived by applying the requests in order to %s (%d characters, sha256 %s): %s\n", o.readPath, len(rd.u), o.readSHA, label)
	}
	var st docsStructure
	reqs := obj["requests"].([]any)
	if len(reqs) == 0 {
		b.WriteString("No requests.\n")
	}
	for i, r := range reqs {
		m, _ := r.(map[string]any)
		ok := false
		for kind, v := range m {
			ok = len(m) == 1 && c.docsRequest(b, kind, v, &st, rd)
		}
		if !ok {
			if rd != nil {
				rd.fail("request %d is not a kind the summary applies", i+1)
			}
			fmt.Fprintf(b, "Request %d, a kind or shape this summary does not cover, in full:\n%s", i+1, fence(jsonText(r), "json"))
		}
	}
	var lines []string
	if st.paragraphs.all > 0 {
		lines = append(lines, countOf(st.paragraphs.all, "paragraph style")+st.paragraphs.kinds())
	}
	if st.styleRuns > 0 {
		lines = append(lines, countOf(st.styleRuns, "text style run")+st.styles.kinds())
	}
	if len(st.links) > 0 {
		shownLinks := make([]string, len(st.links))
		for i, u := range st.links {
			shownLinks[i] = shown(u)
		}
		lines = append(lines, countOf(len(st.links), "link")+" ("+strings.Join(shownLinks, ", ")+")")
	}
	if st.bullets.all > 0 {
		lines = append(lines, countOf(st.bullets.all, "bullet run")+st.bullets.kinds())
	}
	lines = append(lines, guards...)
	if len(lines) > 0 {
		b.WriteString("Formatting and guards:\n")
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
	}
	if rd != nil && rd.err != "" {
		fmt.Fprintf(b, "The read could not be applied past this point: %s. Later changes above give ranges only.\n", rd.err)
	}
	c.fields(b, obj, skip)
}

// docsRequest summarizes one request and reports whether it is covered; a request that is not has written nothing. rd may be nil.
func (c *writeCtx) docsRequest(b *strings.Builder, kind string, v any, st *docsStructure, rd *docRead) bool {
	m, _ := v.(map[string]any)
	if m == nil {
		return false
	}
	switch kind {
	case "insertText":
		text, ok := m["text"].(string)
		if !ok || !onlyKeys(m, "text", "location", "endOfSegmentLocation") || len(m) != 2 {
			return false
		}
		var label string
		var idx int64
		atEnd := false
		if loc, ok := m["location"].(map[string]any); ok {
			i, iok := num(loc["index"])
			if len(loc) != 1 || !iok {
				return false
			}
			idx, label = i, fmt.Sprintf("Inserts at index %d", i)
		} else if end, ok := m["endOfSegmentLocation"].(map[string]any); ok && len(end) == 0 {
			atEnd, label = true, "Inserts at the end of the body"
		} else {
			return false
		}
		if rd != nil {
			if why := rd.insert(idx, atEnd, text); why != "" {
				label += " (position not checked against the read: " + why + ")"
			}
		}
		c.text(b, label, text, false)
	case "deleteContentRange":
		s, e, ok := docRange(m["range"])
		if !ok || len(m) != 1 {
			return false
		}
		head := fmt.Sprintf("Deletes %d characters at index %d to %d (end excluded)", e-s, s, e)
		switch {
		case rd == nil:
			b.WriteString(head + "; what goes is not known: no read of the document was given.\n")
		default:
			gone, why := rd.deleteRange(s, e)
			if why != "" {
				b.WriteString(head + "; what goes can't be shown: " + why + ".\n")
			} else {
				c.text(b, head+", which hold", gone, false)
			}
		}
	case "replaceAllText":
		ct, _ := m["containsText"].(map[string]any)
		search, ok := ct["text"].(string)
		with, withOK := m["replaceText"].(string)
		mc, mcOK := ct["matchCase"].(bool)
		rx, rxOK := ct["searchByRegex"].(bool)
		if ct == nil || !onlyKeys(m, "containsText", "replaceText") || !onlyKeys(ct, "text", "matchCase", "searchByRegex") ||
			!ok || search == "" || (!withOK && m["replaceText"] != nil) || (!mcOK && ct["matchCase"] != nil) || (!rxOK && ct["searchByRegex"] != nil) {
			return false
		}
		to := c.inline(with)
		if with == "" {
			to = `"" (the matches are deleted)`
		}
		fmt.Fprintf(b, "Replaces every match of %s (case-sensitive: %s, regular expression: %s) with %s\n", c.inline(search), yesNo(mc), yesNo(rx), to)
		b.WriteString(c.links())
		if rd != nil {
			t, why := rd.replaceAll(search, mc, rx, with)
			if why != "" {
				fmt.Fprintf(b, "  Not checked against the read: %s.\n", why)
			} else {
				fmt.Fprintf(b, "  In the read: %s%s\n", countOf(t.all, "match"), t.kindsQuoted())
			}
		}
	case "updateParagraphStyle":
		_, _, ok := docRange(m["range"])
		ps, _ := m["paragraphStyle"].(map[string]any)
		named, nok := ps["namedStyleType"].(string)
		if !ok || !onlyKeys(m, "range", "paragraphStyle", "fields") || len(ps) != 1 || !nok || m["fields"] != "namedStyleType" {
			return false
		}
		st.paragraphs.add(named)
	case "updateTextStyle":
		if _, _, ok := docRange(m["range"]); !ok || !onlyKeys(m, "range", "textStyle", "fields") {
			return false
		}
		ts, _ := m["textStyle"].(map[string]any)
		mask, _ := m["fields"].(string)
		if len(ts) == 0 || !onlyKeys(ts, "link", "bold", "italic", "underline", "strikethrough") {
			return false
		}
		want := make([]string, 0, len(ts))
		var runs []string
		var link string
		for _, k := range sortedKeys(ts) {
			want = append(want, k)
			if k == "link" {
				lm, _ := ts[k].(map[string]any)
				u, ok := lm["url"].(string)
				if len(lm) != 1 || !ok {
					return false
				}
				link = u
				continue
			}
			on, ok := ts[k].(bool)
			if !ok {
				return false
			}
			runs = append(runs, map[bool]string{true: k, false: "not " + k}[on])
		}
		got := strings.Split(mask, ",")
		sort.Strings(got)
		if !slices.Equal(got, want) {
			return false
		}
		if _, has := ts["link"]; has {
			st.links = append(st.links, link)
		}
		if len(runs) > 0 {
			st.styleRuns++
			for _, r := range runs {
				st.styles.add(r)
			}
		}
	case "createParagraphBullets":
		s, e, ok := docRange(m["range"])
		preset, pok := m["bulletPreset"].(string)
		if !ok || !pok || !onlyKeys(m, "range", "bulletPreset") || len(m) != 2 {
			return false
		}
		st.bullets.add(preset)
		if rd != nil && rd.err == "" {
			if from, to, why := rd.span(s, e); why == "" && slices.Contains(rd.u[from:to], '\t') {
				rd.fail("createParagraphBullets removes leading tabs, which the summary does not apply")
			}
		}
	default:
		return false
	}
	return true
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// kindsQuoted is the tally as ": "a" ×2, "A" ×1" with each key quoted, or nothing when it is empty.
func (t *tally) kindsQuoted() string {
	if t.all == 0 {
		return ""
	}
	parts := make([]string, len(t.keys))
	for i, k := range t.keys {
		parts[i] = fmt.Sprintf("%s ×%d", jsonText(k), t.n[k])
	}
	return ": " + strings.Join(parts, ", ")
}
