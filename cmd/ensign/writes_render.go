// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// longTextRunes is the length beyond which a text counts as long: about a paragraph. A long text is saved to a file of its own, and a session that registers a write with --short sees only its start.
const longTextRunes = 500

// shortStartRunes is how much of a shortened text the listing shows.
const shortStartRunes = 200

// writeOpts is what the session chose for one write at `ensign writes add`.
type writeOpts struct {
	short    bool
	summary  string // the session's own few sentences on the write
	title    string // Google Docs: a title the session gives the document
	readPath string // Google Docs: the session's own read of the document, as plain text
	readRev  string // Google Docs: the revision the session says its read is at
	hasRead  bool
	read     string // the text of readPath, line ends as "\n"
	readSHA  string // sha256 of the file's bytes
}

type savedText struct{ path, text string }

// writeCtx renders one write's part of the listing and collects the plain-text files that go with it.
type writeCtx struct {
	short bool
	base  string // path prefix of the saved files: <session file>.writes.<token>
	texts []savedText
	urls  []string // links found in text shown shortened, until links() prints them
	brief bool     // something was summarised by kind, size and excerpt, so the exact JSON is only in the input file
}

func (c *writeCtx) save(text string) string {
	p := fmt.Sprintf("%s.%d.txt", c.base, len(c.texts)+1)
	c.texts = append(c.texts, savedText{p, text})
	return p
}

func isLong(s string) bool { return utf8.RuneCountInString(s) > longTextRunes }

// linkPattern finds the URLs and email addresses in a text: anything with a scheme, www. or mailto: up to white space or a quote, and name@host.
var linkPattern = regexp.MustCompile("(?i)(?:\\b[a-z][a-z0-9+.-]*://|\\bwww\\.|\\bmailto:)[^\\s<>\"'`]+|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)+")

func (c *writeCtx) noteLinks(s string) {
	for _, m := range linkPattern.FindAllString(s, -1) {
		if m = strings.TrimRight(m, ".,;:!?"); !slices.Contains(c.urls, m) {
			c.urls = append(c.urls, m)
		}
	}
}

// shown is s on one line with every invisible character escaped.
func shown(s string) string {
	if plainString(s) && !strings.ContainsAny(s, "\n\t") {
		return s
	}
	return jsonText(s)
}

// links prints, in full, the links found in the text just shortened, and forgets them.
func (c *writeCtx) links() string {
	if len(c.urls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("URLs and addresses in the shortened text, in full:\n")
	for _, u := range c.urls {
		b.WriteString("- " + shown(u) + "\n")
	}
	c.urls = nil
	return b.String()
}

// cutNote saves a long text whole and returns its start and the note that says what was cut.
func (c *writeCtx) cutNote(s string) (start, note string) {
	path := c.save(s)
	c.noteLinks(s)
	r := []rune(s)
	return string(r[:shortStartRunes]), fmt.Sprintf("shortened: first %d of %d characters, sha256 %s, all of it in %s", shortStartRunes, len(r), sha([]byte(s)), path)
}

// text writes a labelled text as a fenced block. A long text is saved to a file whatever the session chose, and with --short only its start is shown. keepFull marks a destination, which is never shortened.
func (c *writeCtx) text(b *strings.Builder, label, s string, keepFull bool) {
	body := s
	switch {
	case keepFull || !isLong(s):
	case c.short:
		start, note := c.cutNote(s)
		label += " (" + note + ")"
		body = start
	default:
		label += fmt.Sprintf(" (%d characters, sha256 %s, also saved in %s)", utf8.RuneCountInString(s), sha([]byte(s)), c.save(s))
	}
	b.WriteString(label + ":\n")
	if plainString(body) {
		b.WriteString(fence(body, ""))
	} else {
		b.WriteString(fence(jsonText(body), "json"))
	}
	b.WriteString(c.links())
}

// inline is a text as a quoted JSON string for use inside a line, shortened under --short when long. The caller prints links() after the line.
func (c *writeCtx) inline(s string) string {
	if c.short && isLong(s) {
		start, note := c.cutNote(s)
		return jsonText(start) + " [" + note + "]"
	}
	return jsonText(s)
}

// fullWords are the words of a field name that mark a destination, a recipient or an ID of what is written to. Such a field is never shortened.
var fullWords = map[string]bool{
	"to": true, "cc": true, "bcc": true, "from": true, "sender": true, "reply": true,
	"recipient": true, "recipients": true, "attendee": true, "attendees": true, "invitee": true, "invitees": true, "participants": true,
	"email": true, "emails": true, "address": true, "addresses": true, "destination": true, "destinations": true,
	"url": true, "urls": true, "uri": true, "link": true, "links": true, "id": true, "ids": true,
	"folder": true, "parent": true, "channel": true, "thread": true, "target": true, "path": true, "calendar": true, "room": true,
	"owner": true, "assignee": true, "assignees": true, "user": true, "users": true,
}

// textWords are the words of a field name that mark running text. A field whose last word is one is not a feed destination.
var textWords = map[string]bool{
	"text": true, "body": true, "message": true, "content": true, "html": true, "note": true, "description": true, "comment": true,
}

// keepFullKey reports whether a field name, split at separators and at lower-to-upper case changes, holds a word of fullWords.
func keepFullKey(k string) bool { return keyHasWord(k, fullWords) }

// textKey reports whether the last word of a field name, split as keepFullKey does, is a word of textWords.
func textKey(k string) bool {
	words := keyWords(k)
	return len(words) > 0 && textWords[words[len(words)-1]]
}

func keyHasWord(k string, set map[string]bool) bool {
	for _, w := range keyWords(k) {
		if set[w] {
			return true
		}
	}
	return false
}

func keyWords(k string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	var prev rune
	for _, r := range k {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return words
}

// excerptRunes is how much of a text or value a one-line digest shows.
const excerptRunes = 60

// excerpt is the start of s on one line with every invisible character escaped, marked when it was cut.
func excerpt(s string) string {
	r := []rune(s)
	if len(r) <= excerptRunes {
		return shown(s)
	}
	return shown(string(r[:excerptRunes])) + "…"
}

// leaves writes every scalar under v as one line, its path and its value in full. A long text outside a destination-like field is saved as a plain-text file and named instead, and an empty object or list says so.
func (c *writeCtx) leaves(b *strings.Builder, v any, key, path string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			fmt.Fprintf(b, "%s: empty object\n", jsonText(path))
		}
		for _, k := range sortedKeys(x) {
			c.leaves(b, x[k], k, path+"."+k)
		}
	case []any:
		if len(x) == 0 {
			fmt.Fprintf(b, "%s: empty list\n", jsonText(path))
		}
		for i, e := range x {
			c.leaves(b, e, key, fmt.Sprintf("%s[%d]", path, i))
		}
	case string:
		if isLong(x) && !keepFullKey(key) {
			c.noteLinks(x)
			fmt.Fprintf(b, "%s: long text (%d characters, sha256 %s, also saved in %s)\n", jsonText(path), utf8.RuneCountInString(x), sha([]byte(x)), c.save(x))
		} else {
			fmt.Fprintf(b, "%s: %s\n", jsonText(path), jsonText(x))
		}
	case json.Number:
		fmt.Fprintf(b, "%s: %s\n", jsonText(path), x)
	case bool:
		fmt.Fprintf(b, "%s: %v\n", jsonText(path), x)
	case nil:
		fmt.Fprintf(b, "%s: null\n", jsonText(path))
	}
}

// fields writes the input fields in key order, skipping those in skip: a string as a text block, anything else as one line per scalar in it.
func (c *writeCtx) fields(b *strings.Builder, obj map[string]any, skip map[string]bool) {
	for _, k := range sortedKeys(obj) {
		if skip[k] {
			continue
		}
		if s, ok := obj[k].(string); ok {
			c.text(b, jsonText(k), s, keepFullKey(k))
			continue
		}
		c.leaves(b, obj[k], k, k)
		b.WriteString(c.links())
	}
}

// render writes the part of a write's listing that comes from its input. A Google Docs batch update gets its own summary; every other tool lists its fields.
func (c *writeCtx) render(obj map[string]any, o *writeOpts) (string, error) {
	var b strings.Builder
	if docsBatch(obj) {
		c.docs(&b, obj, o)
		c.briefNote(&b)
		return b.String(), nil
	}
	if o.title != "" || o.hasRead {
		return "", fmt.Errorf("--title, --read and --read-revision apply only to a Google Docs batch update: input with a string documentId and a requests list")
	}
	if len(obj) == 0 {
		b.WriteString("(no input fields)\n")
	}
	c.fields(&b, obj, nil)
	c.briefNote(&b)
	return b.String(), nil
}

// briefNote says where the exact JSON of what was only summarised is.
func (c *writeCtx) briefNote(b *strings.Builder) {
	if c.brief {
		fmt.Fprintf(b, "Some input is summarised above by kind, size and start; its exact JSON is in %s.\n", c.base+".json")
	}
}
