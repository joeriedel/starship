// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"regexp"
	"slices"
	"strings"
)

// remoteTriggers are the lowercased names remoteChange looks for before it tokenises: a command that names none of them can't change a remote.
var remoteTriggers = []string{"git", "gh", "curl", "wget", "ensign", "npm", "pnpm", "yarn", "cargo", "docker", "twine", "gem"}

// shellDepthLimit bounds how deep remoteChange follows substitutions, sh -c strings and eval.
const shellDepthLimit = 6

// remoteChange returns the command in cmd that changes a remote (a push, a gh or API write, a publish, an ensign command that pushes), in a form fit for a denial, or "" when there is none. It is a guard against accidents by a read-only role, not a security boundary: git aliases, scripts and Makefiles that push, ssh, scp, rsync and variables holding a command are not looked into.
func remoteChange(cmd string) string {
	lower := strings.ToLower(cmd)
	if !slices.ContainsFunc(remoteTriggers, func(t string) bool { return strings.Contains(lower, t) }) {
		return ""
	}
	return scanShell(cmd, 0, false)
}

type heredoc struct {
	delim string
	strip bool
}

// scanShell splits s into simple commands, quote-aware, and classifies each; substitutions and subshells are scanned too. Unterminated input is classified as far as it was read.
func scanShell(s string, depth int, p4 bool) string {
	if depth > shellDepthLimit {
		return ""
	}
	var (
		words    []string
		cur      strings.Builder
		inWord   bool
		skipNext bool
		pending  []heredoc
		found    string
	)
	endWord := func() {
		if !inWord {
			return
		}
		w := cur.String()
		cur.Reset()
		inWord = false
		if skipNext {
			skipNext = false
			return
		}
		words = append(words, w)
	}
	endCmd := func() {
		endWord()
		if found == "" {
			found = classifyCommand(words, depth, p4)
		}
		words = nil
	}
	sub := func(inner string) {
		inWord = true
		if found == "" {
			found = scanShell(inner, depth+1, p4)
		}
	}
	for i := 0; i < len(s) && found == ""; i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 < len(s) {
				n := s[i+1]
				if n == '\n' {
					i++
					continue
				}
				if strings.IndexByte(" \t\"'$`\\;&|()<>#*?[]{}~!", n) >= 0 {
					cur.WriteByte(n)
					inWord = true
					i++
					continue
				}
			}
			cur.WriteByte(c)
			inWord = true
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
				continue
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			i = scanDouble(s, i+1, &cur, sub)
		case c == '`':
			j := closeBacktick(s, i+1)
			sub(s[i+1 : j])
			i = j
		case (c == '$' || c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '(':
			j := closeParen(s, i+2)
			sub(s[i+2 : j])
			i = j
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '\n':
			endCmd()
			if len(pending) > 0 {
				i = skipHeredocs(s, i+1, pending) - 1
				pending = nil
			}
		case c == '#' && !inWord:
			for i+1 < len(s) && s[i+1] != '\n' {
				i++
			}
		case c == ';' || c == '(' || c == ')' || c == '|' || (c == '&' && !(i+1 < len(s) && s[i+1] == '>')):
			endCmd()
		case c == '<' || c == '>' || c == '&':
			if inWord && strings.Trim(cur.String(), "0123456789") == "" {
				cur.Reset()
				inWord = false
			} else {
				endWord()
			}
			j := i
			for j < len(s) && strings.IndexByte("<>&", s[j]) >= 0 {
				j++
			}
			op := s[i:j]
			if strings.HasPrefix(op, "<<") && op != "<<<" {
				h := heredoc{}
				if j < len(s) && s[j] == '-' {
					h.strip = true
					j++
				}
				for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
					j++
				}
				for j < len(s) && strings.IndexByte(" \t\r\n;&|()<>", s[j]) < 0 {
					if s[j] != '\'' && s[j] != '"' && s[j] != '\\' {
						h.delim += s[j : j+1]
					}
					j++
				}
				pending = append(pending, h)
			} else {
				skipNext = true
			}
			i = j - 1
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if found == "" {
		endCmd()
	}
	return found
}

// scanDouble reads a double-quoted string from i into cur, scanning the substitutions in it, and returns the index of the closing quote, or len(s) when it is unterminated.
func scanDouble(s string, i int, cur *strings.Builder, sub func(string)) int {
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return i
		case c == '\\' && i+1 < len(s):
			n := s[i+1]
			if n == '\n' {
				i++
			} else if strings.IndexByte("\"\\$`", n) >= 0 {
				cur.WriteByte(n)
				i++
			} else {
				cur.WriteByte(c)
			}
		case c == '`':
			j := closeBacktick(s, i+1)
			sub(s[i+1 : j])
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			j := closeParen(s, i+2)
			sub(s[i+2 : j])
			i = j
		default:
			cur.WriteByte(c)
		}
	}
	return len(s)
}

// closeBacktick returns the index of the backtick closing a substitution that starts at i, or len(s).
func closeBacktick(s string, i int) int {
	for ; i < len(s); i++ {
		if s[i] == '\\' {
			i++
		} else if s[i] == '`' {
			return i
		}
	}
	return len(s)
}

// closeParen returns the index of the parenthesis closing a substitution that starts at i, or len(s).
func closeParen(s string, i int) int {
	depth := 1
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				i += j + 1
			} else {
				return len(s)
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(s)
}

// skipHeredocs returns the index after the bodies of the pending here-documents, which start at i.
func skipHeredocs(s string, i int, pending []heredoc) int {
	for _, h := range pending {
		for i < len(s) {
			end := strings.IndexByte(s[i:], '\n')
			line := s[i:]
			if end >= 0 {
				line = s[i : i+end]
				i += end + 1
			} else {
				i = len(s)
			}
			line = strings.TrimSuffix(line, "\r")
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
		}
	}
	return i
}

var assignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var shellKeywords = map[string]bool{"if": true, "then": true, "elif": true, "else": true, "while": true, "until": true, "do": true, "!": true, "time": true, "{": true}

// wrapperOptions are the commands that run another command, with the options of each that take a value.
var wrapperOptions = map[string][]string{
	"env":     {"-u", "-C", "--unset", "--chdir"},
	"sudo":    {"-u", "-g", "-h", "-p", "-C", "-r", "-t", "-T", "-U", "-D"},
	"command": nil,
	"builtin": nil,
	"exec":    {"-a"},
	"nohup":   nil,
	"nice":    {"-n", "--adjustment"},
	"timeout": {"-s", "-k", "--signal", "--kill-after"},
	"xargs":   {"-I", "-n", "-P", "-d", "-L", "-s", "-E", "-a"},
}

var registryPublish = map[string]string{"npm": "publish", "pnpm": "publish", "yarn": "publish", "cargo": "publish", "docker": "push", "twine": "upload", "gem": "push"}

// hasHelp reports whether args ask for help, which only reads.
func hasHelp(args []string) bool {
	return slices.Contains(args, "--help") || slices.Contains(args, "-h")
}

// plainWords are the words of args that are not options.
func plainWords(args []string) []string {
	var out []string
	for _, w := range args {
		if !strings.HasPrefix(w, "-") {
			out = append(out, w)
		}
	}
	return out
}

// commandName is the lowercased basename of a command word, split on / and \, without a trailing .exe.
func commandName(w string) string {
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(w), ".exe")
}

// skipOptions drops the leading options (and VAR=value words) of a wrapper, with the values of those in withValue.
func skipOptions(args, withValue []string) []string {
	for len(args) > 0 {
		w := args[0]
		switch {
		case slices.Contains(withValue, w):
			args = args[min(2, len(args)):]
		case strings.HasPrefix(w, "-") && w != "-" || assignmentRe.MatchString(w):
			args = args[1:]
		default:
			return args
		}
	}
	return args
}

// classifyCommand finds the real command of one simple command, through assignments, keywords and wrappers, and returns it when it changes a remote.
func classifyCommand(ws []string, depth int, p4 bool) string {
	for len(ws) > 0 {
		if shellKeywords[ws[0]] || assignmentRe.MatchString(ws[0]) {
			ws = ws[1:]
			continue
		}
		name, rest := commandName(ws[0]), ws[1:]
		if opts, ok := wrapperOptions[name]; ok {
			if name == "command" && len(rest) > 0 && (rest[0] == "-v" || rest[0] == "-V") {
				return ""
			}
			ws = skipOptions(rest, opts)
			if name == "timeout" && len(ws) > 0 {
				ws = ws[1:]
			}
			continue
		}
		return classifyTool(name, rest, depth, p4)
	}
	return ""
}

// classifyTool returns the command when name with args changes a remote, or with p4 set, when it is a p4 command that is not read-only.
func classifyTool(name string, args []string, depth int, p4 bool) string {
	label := func() string {
		s := name + " " + strings.Join(args, " ")
		if len(s) > 100 {
			s = s[:100] + "..."
		}
		return strings.TrimSpace(s)
	}
	if p4 && name == "p4" {
		if p4Refuses(args) {
			return label()
		}
		return ""
	}
	refused := false
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh":
		wantCommand := false
		for i := 0; i < len(args); i++ {
			w := args[i]
			if (!strings.HasPrefix(w, "-") && !(strings.HasPrefix(w, "+") && len(w) > 1)) || w == "--" {
				if wantCommand {
					if w == "--" {
						i++
					}
					if i < len(args) {
						return scanShell(args[i], depth+1, p4)
					}
				}
				break
			}
			if strings.HasPrefix(w, "--") {
				continue
			}
			if strings.HasPrefix(w, "-") && strings.Contains(w, "c") {
				wantCommand = true
			}
			i += strings.Count(w, "o") + strings.Count(w, "O")
		}
	case "eval":
		return scanShell(strings.Join(args, " "), depth+1, p4)
	case "cmd":
		for i, w := range args {
			if w = strings.ToLower(w); w == "/c" || w == "//c" || w == "/k" || w == "//k" {
				return scanShell(strings.Join(args[i+1:], " "), depth+1, p4)
			}
		}
	case "powershell", "pwsh":
		for i, w := range args {
			if w = strings.ToLower(w); w == "-c" || w == "-command" {
				return scanShell(strings.Join(args[i+1:], " "), depth+1, p4)
			}
		}
	case "find":
		for i, w := range args {
			if w != "-exec" && w != "-execdir" && w != "-ok" && w != "-okdir" {
				continue
			}
			end := i + 1
			for end < len(args) && args[end] != ";" && args[end] != "+" {
				end++
			}
			if r := classifyCommand(args[i+1:end], depth, p4); r != "" {
				return r
			}
		}
	case "git":
		refused = !hasHelp(args) && gitChangesRemote(args)
	case "gh":
		refused = !hasHelp(args) && ghChangesRemote(args)
	case "curl":
		refused = curlChangesRemote(args)
	case "wget":
		refused = wgetChangesRemote(args)
	case "ensign":
		refused = ensignChangesRemote(args)
	case "python", "python3", "py":
		for i, w := range args {
			if w == "-m" && i+1 < len(args) && args[i+1] == "twine" {
				words := plainWords(args[i+2:])
				refused = !hasHelp(args) && len(words) > 0 && words[0] == "upload"
				break
			}
		}
	default:
		if verb, ok := registryPublish[name]; ok && !hasHelp(args) {
			words := plainWords(args)
			refused = len(words) > 0 && words[0] == verb ||
				name == "yarn" && len(words) > 1 && words[0] == "npm" && words[1] == "publish" ||
				name == "docker" && len(words) > 1 && words[0] == "image" && words[1] == "push"
		}
	}
	if refused && !p4 {
		return label()
	}
	return ""
}

// gitChangesRemote reports whether a git command line (the words after git) writes to a remote: a push, unless it names an explicit filesystem path, or a git lfs push, lock or unlock, a git subtree push, a git svn dcommit or a git p4 submit. Global options before the subcommand are skipped: the options git is known to give a value (-C, -c, --attr-source and so on) take the next word with them, and the known flags and the words with an = take none. An option not on either list may or may not take a value, so the word after it is tried as the subcommand and, if it is not a write, skipped as the option's value. That errs on the side of refusing: `git --future-flag log push` is refused, though git would read log as the subcommand, so that a push behind a global option a later git adds is still caught.
func gitChangesRemote(args []string) bool {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		w := args[i]
		switch {
		case slices.Contains(gitValueOptions, w):
			i++
		case slices.Contains(gitFlagOptions, w) || strings.Contains(w, "="):
		default:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				if gitSubcommandChangesRemote(args[i+1], args[i+2:]) {
					return true
				}
				i++
			}
		}
		i++
	}
	if i >= len(args) {
		return false
	}
	return gitSubcommandChangesRemote(args[i], args[i+1:])
}

// gitValueOptions are the global options git takes a value for as the next word; with an = the value is part of the word.
var gitValueOptions = []string{"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix", "--attr-source"}

// gitFlagOptions are the global options git takes no value for.
var gitFlagOptions = []string{"-p", "--paginate", "-P", "--no-pager", "--no-replace-objects", "--no-lazy-fetch", "--no-optional-locks", "--no-advice", "--bare", "--exec-path", "--html-path", "--man-path", "--info-path", "--literal-pathspecs", "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "-v", "--version", "-h", "--help"}

// gitSubcommandChangesRemote reports whether the git subcommand sub with the words rest after it writes to a remote.
func gitSubcommandChangesRemote(sub string, rest []string) bool {
	switch sub {
	case "push":
		return !pushesToPath(rest)
	case "lfs":
		return len(rest) > 0 && (rest[0] == "push" || rest[0] == "lock" || rest[0] == "unlock")
	case "subtree":
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			if rest[0] == "-P" || rest[0] == "--prefix" {
				rest = rest[1:]
			}
			rest = rest[min(1, len(rest)):]
		}
		return len(rest) > 0 && rest[0] == "push"
	case "svn":
		return len(rest) > 0 && rest[0] == "dcommit"
	case "p4":
		return len(rest) > 0 && rest[0] == "submit"
	}
	return false
}

// pushesToPath reports whether the repository of a git push (the words after push) is an explicit filesystem path.
func pushesToPath(args []string) bool {
	var positional, flagRepo string
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "--repo" && i+1 < len(args):
			flagRepo = args[i+1]
			i++
		case strings.HasPrefix(w, "--repo="):
			flagRepo = w[len("--repo="):]
		case w == "-o" || w == "--push-option" || w == "--receive-pack" || w == "--exec":
			i++
		case strings.HasPrefix(w, "-"):
		case positional == "":
			positional = w
		}
	}
	repo := positional
	if repo == "" {
		repo = flagRepo
	}
	if repo == "." || repo == ".." {
		return true
	}
	for _, p := range []string{"/", "./", "../", "~/", "file://"} {
		if strings.HasPrefix(repo, p) {
			return true
		}
	}
	return len(repo) >= 3 && repo[1] == ':' && (repo[2] == '\\' || repo[2] == '/') && (repo[0]|0x20 >= 'a' && repo[0]|0x20 <= 'z')
}

var ghReadVerbs = []string{"list", "view", "status", "diff", "checks", "check", "get", "watch", "download", "verify"}

// ghChangesRemote allows the reading verbs of gh and refuses everything else, so a new writing verb is refused by default.
func ghChangesRemote(args []string) bool {
	var pos []string
	first := -1
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "--repo" || w == "-R" || w == "--hostname":
			i++
		case strings.HasPrefix(w, "-"):
		default:
			if first < 0 {
				first = i
			}
			pos = append(pos, w)
		}
	}
	if len(pos) == 0 {
		return false
	}
	switch pos[0] {
	case "api":
		return ghAPIWrites(args[first+1:])
	case "search", "status", "browse", "help", "version", "completion", "config", "alias", "auth":
		return false
	}
	if len(pos) == 1 {
		return true
	}
	if pos[0] == "repo" && pos[1] == "clone" || pos[0] == "pr" && pos[1] == "checkout" {
		return false
	}
	if len(pos) > 2 && pos[0] == "repo" {
		switch pos[1] {
		case "deploy-key":
			return pos[2] != "list"
		case "gitignore", "license":
			return pos[2] != "list" && pos[2] != "view"
		}
	}
	if pos[0] == "extension" && pos[1] == "search" {
		return false
	}
	return !slices.Contains(ghReadVerbs, pos[1]) && !strings.HasSuffix(pos[1], "-list")
}

var mutationRe = regexp.MustCompile(`(?i)\bmutation\b`)

// ghAPIWrites reports whether a gh api call (the words after api) writes: a method other than GET or HEAD, fields or input without a method, or a GraphQL mutation.
func ghAPIWrites(args []string) bool {
	var method, endpoint string
	var hasMethod, field, input, mutation bool
	for i := 0; i < len(args); i++ {
		w := args[i]
		if mutationRe.MatchString(w) {
			mutation = true
		}
		switch {
		case w == "-X" || w == "--method":
			hasMethod = true
			if i+1 < len(args) {
				method = args[i+1]
				i++
			}
		case strings.HasPrefix(w, "--method="):
			hasMethod, method = true, w[len("--method="):]
		case strings.HasPrefix(w, "-X") && !strings.HasPrefix(w, "--"):
			hasMethod, method = true, w[2:]
		case w == "--input":
			input = true
			i++
		case strings.HasPrefix(w, "--input="):
			input = true
		case w == "--field" || w == "--raw-field" || w == "-f" || w == "-F":
			field = true
			if i+1 < len(args) && mutationRe.MatchString(args[i+1]) {
				mutation = true
			}
			i++
		case strings.HasPrefix(w, "--field=") || strings.HasPrefix(w, "--raw-field=") || (!strings.HasPrefix(w, "--") && (strings.HasPrefix(w, "-f") || strings.HasPrefix(w, "-F"))):
			field = true
		case w == "-H" || w == "--header" || w == "-q" || w == "--jq" || w == "-t" || w == "--template" || w == "--hostname" || w == "--cache" || w == "-p" || w == "--preview":
			i++
		case strings.HasPrefix(w, "-"):
		case endpoint == "":
			endpoint = w
		}
	}
	if hasMethod {
		m := strings.ToUpper(method)
		return m != "GET" && m != "HEAD"
	}
	if endpoint == "graphql" {
		return mutation || input
	}
	return field || input
}

// isReadMethod reports whether an HTTP method is GET or HEAD, in any case; any other method, including an empty one, is not a read.
func isReadMethod(m string) bool {
	m = strings.ToUpper(m)
	return m == "GET" || m == "HEAD"
}

// curlShortWithValue are the short options of curl that take a value, other than -X, -d, -F and -T, which are handled by name.
const curlShortWithValue = "oHuAebcmwxKEyYzrCDPQtU"

// curlChangesRemote reports whether a curl command line (the words after curl) can change a remote. An explicit -X or --request method decides it, so a missing value or any method other than GET or HEAD counts as a write; otherwise it is a write when it sends data (-d, -F, -T, --data*, --json, --form, --upload-file) and does not turn that into a query with -G or --get.
func curlChangesRemote(args []string) bool {
	var method string
	var hasMethod, data, get bool
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "--request":
			hasMethod = true
			if i+1 < len(args) {
				method = args[i+1]
				i++
			}
		case strings.HasPrefix(w, "--request="):
			hasMethod, method = true, w[len("--request="):]
		case w == "--get":
			get = true
		case strings.HasPrefix(w, "--data") || strings.HasPrefix(w, "--json") || strings.HasPrefix(w, "--form") || strings.HasPrefix(w, "--upload-file"):
			data = true
		case strings.HasPrefix(w, "--"):
		case strings.HasPrefix(w, "-") && len(w) > 1:
		cluster:
			for j := 1; j < len(w); j++ {
				c := w[j]
				switch {
				case c == 'G':
					get = true
				case c == 'X':
					hasMethod, method = true, w[j+1:]
					if j+1 == len(w) && i+1 < len(args) {
						method = args[i+1]
						i++
					}
					break cluster
				case c == 'd' || c == 'F' || c == 'T':
					data = true
					if j+1 == len(w) {
						i++
					}
					break cluster
				case strings.IndexByte(curlShortWithValue, c) >= 0:
					if j+1 == len(w) {
						i++
					}
					break cluster
				}
			}
		}
	}
	if hasMethod {
		return !isReadMethod(method)
	}
	return data && !get
}

// wgetChangesRemote reports whether a wget command line (the words after wget) can change a remote: any --post-data, --post-file, --body-data or --body-file, or a --method that is not GET or HEAD, where a missing or empty method counts as a write.
func wgetChangesRemote(args []string) bool {
	for i, w := range args {
		name, val, hasVal := strings.Cut(w, "=")
		switch name {
		case "--post-data", "--post-file", "--body-data", "--body-file":
			return true
		case "--method":
			if !hasVal && i+1 < len(args) {
				val = args[i+1]
			}
			if !isReadMethod(val) {
				return true
			}
		}
	}
	return false
}

// ensignChangesRemote reports whether an ensign command line (the words after ensign) publishes to a remote: github-pr, update, run, run-stage and resume always do, journal only for push and commit, and sequence only for release-run.
func ensignChangesRemote(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "github-pr", "update", "run", "run-stage", "resume":
		return true
	case "journal":
		return len(args) > 1 && (args[1] == "push" || args[1] == "commit")
	case "sequence":
		return len(args) > 1 && args[1] == "release-run"
	}
	return false
}
