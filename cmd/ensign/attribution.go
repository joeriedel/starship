// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
)

// attributionLine returns the first line of text that is an agent attribution line, trimmed, or "" when there is none: a Co-Authored-By or Claude-Session trailer, or a "Generated with Claude Code" line (with or without the link or a leading emoji). It is the one matcher every attribution check uses. Prose that merely mentions co-authored does not match.
func attributionLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if key, _, ok := strings.Cut(lower, ":"); ok && attributionKey(key) {
			return line
		}
		lower = strings.TrimLeftFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) && r != '[' })
		if strings.HasPrefix(lower, "generated with claude code") || strings.HasPrefix(lower, "generated with [claude code]") {
			return line
		}
	}
	return ""
}

func attributionKey(key string) bool {
	key = strings.TrimSpace(key)
	return key == "co-authored-by" || key == "claude-session"
}

// attributionTrailer returns v when it is a `git commit --trailer` value (key:value or key=value) whose key is an attribution trailer, or "".
func attributionTrailer(v string) string {
	if i := strings.IndexAny(v, ":="); i >= 0 && attributionKey(strings.ToLower(v[:i])) {
		return strings.TrimSpace(v)
	}
	return ""
}

// attributionFile returns the attribution line in the file at path (relative to dir), or "" when there is none or the file can't be read; `-` is standard input, whose content comes only from here-documents.
func attributionFile(dir, path string) string {
	if path == "-" {
		return ""
	}
	home, _ := os.UserHomeDir()
	path = claudeToolPath(path, home, runtime.GOOS)
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return attributionLine(string(data))
}

// attributionRefusal is the reason given when a commit message or pull request body carries an attribution line.
func attributionRefusal(line string) string {
	return "`" + line + "` is an attribution line, and attribution lines are never added to commits or pull requests here. Write the message again without it (and without any Co-Authored-By, Claude-Session or Generated-with trailer)."
}

// attributionError is attributionRefusal as an error for the commands that write a commit or pull request themselves.
func attributionError(what, line string) error {
	return fmt.Errorf("the %s carries the attribution line %q: attribution lines are never added to commits or pull requests; write it again without that line", what, line)
}

// attributionChange returns the attribution line in a `git commit` or `gh pr create|edit` found in cmd (a message, trailer, body, or a file or heredoc they read), or "". Relative files are read from dir. It is a guard against accidents, not a security boundary: aliases, scripts, variables, editors and messages built at run time are not looked into.
func attributionChange(cmd, dir string) string {
	lower := strings.ToLower(cmd)
	if !strings.Contains(lower, "git") && !strings.Contains(lower, "gh") {
		return ""
	}
	return scanShell(cmd, 0, scanMode{attr: true, dir: dir})
}

// attributionCommand returns the attribution line in a git or gh command line (args are the words after the tool name); extra is the text of the substitutions and here-documents of the command.
func attributionCommand(name string, args []string, m scanMode) string {
	var texts, trailers, files []string
	var valued map[string]*[]string
	switch name {
	case "git":
		i := 0
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			if slices.Contains(gitValueOptions, args[i]) {
				i++
			}
			i++
		}
		if i >= len(args) || args[i] != "commit" {
			return ""
		}
		args = args[i+1:]
		valued = map[string]*[]string{"-m": &texts, "--message": &texts, "--trailer": &trailers, "-F": &files, "--file": &files}
	case "gh":
		words := plainWords(args)
		if len(words) < 2 || words[0] != "pr" || (words[1] != "create" && words[1] != "edit") {
			return ""
		}
		valued = map[string]*[]string{"--body": &texts, "-b": &texts, "--title": &texts, "-t": &texts, "--body-file": &files, "-F": &files}
	}
	for i := 0; i < len(args); i++ {
		w := args[i]
		if w == "--" {
			break
		}
		if dst, ok := valued[w]; ok {
			if i+1 < len(args) {
				*dst = append(*dst, args[i+1])
				i++
			}
			continue
		}
		if opt, v, ok := strings.Cut(w, "="); ok && strings.HasPrefix(w, "--") {
			if dst, ok := valued[opt]; ok {
				*dst = append(*dst, v)
			}
			continue
		}
		if len(w) > 2 && w[0] == '-' && w[1] != '-' {
			// A short option cluster such as -am or -mText: the value is the rest of the word after the first value-taking letter, or the next word.
			for j := 1; j < len(w); j++ {
				dst, ok := valued["-"+w[j:j+1]]
				if !ok {
					continue
				}
				if j+1 < len(w) {
					*dst = append(*dst, w[j+1:])
				} else if i+1 < len(args) {
					*dst = append(*dst, args[i+1])
					i++
				}
				break
			}
		}
	}
	for _, t := range texts {
		if l := attributionLine(t); l != "" {
			return l
		}
	}
	for _, t := range trailers {
		if l := attributionTrailer(t); l != "" {
			return l
		}
	}
	for _, f := range files {
		if l := attributionFile(m.dir, f); l != "" {
			return l
		}
	}
	return attributionLine(m.extra)
}
