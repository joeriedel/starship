package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// The leak gates guard commits to the control plane: the author must be the personal
// identity, nothing from any bridge's denylist may appear, no agent attribution, and
// changes to prose an agent reads get a clean-room review by a model that knows no
// project. Install them in a control-plane clone with `ensign gate install`.

const preCommitHook = "#!/bin/sh\nexec ensign gate pre-commit\n"
const commitMsgHook = "#!/bin/sh\nexec ensign gate commit-msg \"$1\"\n"

func repoTop() (string, error) {
	wd, _ := os.Getwd()
	top, err := gitOut(wd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not inside a git repository")
	}
	return top, nil
}

func cmdGate(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign gate install --identity <email> | ensign gate pre-commit | ensign gate commit-msg <file>")
	if len(args) == 0 {
		return usage
	}
	top, err := repoTop()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		if len(args) != 3 || args[1] != "--identity" || !strings.Contains(args[2], "@") {
			return usage
		}
		if err := git(top, "config", "starship.identity", args[2]); err != nil {
			return err
		}
		hooks, err := gitOut(top, "rev-parse", "--git-path", "hooks")
		if err != nil {
			return err
		}
		if !filepath.IsAbs(hooks) {
			hooks = filepath.Join(top, hooks)
		}
		for name, body := range map[string]string{"pre-commit": preCommitHook, "commit-msg": commitMsgHook} {
			if err := writeFile(filepath.Join(hooks, name), body); err != nil {
				return err
			}
			os.Chmod(filepath.Join(hooks, name), 0o755)
		}
		fmt.Fprintf(out, "leak gates installed in %s; commits must be authored by %s\n", top, args[2])
		return nil
	case "pre-commit":
		return preCommit(top, out)
	case "commit-msg":
		if len(args) != 2 {
			return usage
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		if m := attributionRe.FindString(string(data)); m != "" {
			return fmt.Errorf("the commit message carries an agent attribution (%q); starship commits are signed by no agent", strings.TrimSpace(m))
		}
		return nil
	}
	return usage
}

var attributionRe = regexp.MustCompile(`(?im)^.*(co-authored-by:.*(claude|anthropic)|generated (with|by) .*claude|🤖).*$`)

var emailRe = regexp.MustCompile(`<([^>]+)>`)

func preCommit(top string, out io.Writer) error {
	want, _ := gitOut(top, "config", "starship.identity")
	if want == "" {
		return fmt.Errorf("no starship.identity configured; run ensign gate install --identity <email>")
	}
	ident, _ := gitOut(top, "var", "GIT_AUTHOR_IDENT")
	if m := emailRe.FindStringSubmatch(ident); m == nil || !strings.EqualFold(m[1], want) {
		return fmt.Errorf("commits here must be authored by %s, not %q", want, ident)
	}
	diff, err := gitOut(top, "diff", "--cached", "-U0", "--no-color")
	if err != nil {
		return err
	}
	terms, err := allDenylists()
	if err != nil {
		return err
	}
	if hits := denylistHits(diff, terms); len(hits) > 0 {
		return fmt.Errorf("the change contains terms from a bridge's denylist:\n  %s", strings.Join(hits, "\n  "))
	}
	if needsReview(diff) {
		if os.Getenv("STARSHIP_SKIP_REVIEW") == "1" {
			fmt.Fprintln(out, "starship: clean-room review skipped by STARSHIP_SKIP_REVIEW")
			return nil
		}
		return cleanRoomReview(top, diff, out)
	}
	return nil
}

// allDenylists reads the denylist of every bridge registered on this machine. A
// registered bridge without a usable denylist fails closed.
func allDenylists() ([]string, error) {
	var terms []string
	for _, b := range loadRegistry().Bridges {
		if _, err := os.Stat(filepath.Join(b, "bridge.yaml")); err != nil {
			continue // a bridge that has moved or been deleted
		}
		t, err := readDenylist(b)
		if err != nil {
			return nil, err
		}
		terms = append(terms, t...)
	}
	return terms, nil
}

func readDenylist(bridge string) ([]string, error) {
	f, err := os.Open(filepath.Join(bridge, "denylist.txt"))
	if err != nil {
		return nil, fmt.Errorf("bridge %s has no denylist.txt; the control plane refuses commits until it has one", bridge)
	}
	defer f.Close()
	var terms []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" && !strings.HasPrefix(t, "#") {
			terms = append(terms, strings.ToLower(t))
		}
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("bridge %s has an empty denylist.txt; the control plane refuses commits until it lists the terms to keep out", bridge)
	}
	return terms, nil
}

// denylistHits finds denylisted terms in a diff's added lines and file names.
func denylistHits(diff string, terms []string) []string {
	var hits []string
	file := ""
	for _, line := range strings.Split(diff, "\n") {
		if name, ok := strings.CutPrefix(line, "+++ b/"); ok {
			file = name
			for _, t := range terms {
				if strings.Contains(strings.ToLower(name), t) {
					hits = append(hits, fmt.Sprintf("%q in the file name %s", t, name))
				}
			}
			continue
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		low := strings.ToLower(line)
		for _, t := range terms {
			if strings.Contains(low, t) {
				hits = append(hits, fmt.Sprintf("%q in %s", t, file))
			}
		}
	}
	return hits
}

// needsReview reports whether the change touches prose agents read: roles, the manual,
// skills, briefs, docs.
func needsReview(diff string) bool {
	for _, line := range strings.Split(diff, "\n") {
		if name, ok := strings.CutPrefix(line, "+++ b/"); ok {
			for _, prefix := range []string{"roles/", "manual/", "skills/", "briefs/", "docs/", "extensions/"} {
				if strings.HasPrefix(name, prefix) && !strings.HasSuffix(name, ".yaml") {
					return true
				}
			}
		}
	}
	return false
}

const reviewPrompt = `You are reviewing a change to a public, open-source configuration repository for an
agent-orchestration tool. It must contain nothing that came from any specific private
project or employer: no product or project names, codenames, internal hostnames or URLs,
people's names or usernames, proprietary type or function names, or details specific
enough that someone could recognise the system they describe. Generic examples using the
fictional project "supernova" are fine. Generic technology facts (about a language,
engine or library) are fine.

Judge only the added lines in this diff. Reply with only a JSON object:
{"flagged": true|false, "items": ["<file>: <what looks project-specific, and why>"]}

The diff:

`

// cleanClaudeEnv is this process's environment without the variables Claude Code sets
// for its own commands. A claude started with them runs as a child of the calling
// session: it is told that session's project directory, and its prompt arrives with a
// "Claude" line in front, so `help` no longer matches.
// Only the variables a running session sets about itself are removed; configuration a
// user sets (a provider, a token, limits) is kept.
func cleanClaudeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !sessionVars[name] {
			env = append(env, kv)
		}
	}
	return env
}

var sessionVars = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_PROJECT_DIR": true, "CLAUDE_EFFORT": true,
	"CLAUDE_ENV_FILE": true, "CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ID": true,
	"CLAUDE_CODE_SESSION_ATTENDED": true, "CLAUDE_CODE_MESSAGING_SOCKET": true,
	"CLAUDE_CODE_MESSAGING_TOKEN": true, "CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_CODE_EXECPATH": true,
	"STARSHIP_SESSION": true,
}

// cleanRoomReview asks a model with no project context whether the change leaks.
func cleanRoomReview(top, diff string, out io.Writer) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("this change needs a clean-room review and claude is not on PATH (set STARSHIP_SKIP_REVIEW=1 to commit without it, deliberately)")
	}
	fmt.Fprintln(out, "starship: clean-room review of the change…")
	cmd := exec.Command("claude", "-p", "--model", "sonnet", "--output-format", "json", "--max-turns", "1")
	cmd.Dir = os.TempDir() // no project context: not run inside any repo
	cmd.Env = cleanClaudeEnv()
	cmd.Stdin = strings.NewReader(reviewPrompt + diff)
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("the clean-room review could not run: %v", err)
	}
	var res struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("the clean-room review returned something unreadable")
	}
	text := res.Result
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var verdict struct {
		Flagged bool     `json:"flagged"`
		Items   []string `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &verdict); err != nil {
		return fmt.Errorf("the clean-room review's verdict was unreadable: %s", truncate(res.Result, 200))
	}
	if verdict.Flagged {
		return fmt.Errorf("the clean-room review flagged the change:\n  %s", strings.Join(verdict.Items, "\n  "))
	}
	fmt.Fprintln(out, "starship: clean-room review passed")
	return nil
}

// cmdProposal implements `ensign proposal check <file>`: a proposal is checked against
// every denylist on this machine before it is carried into the control plane.
func cmdProposal(args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "check" {
		return fmt.Errorf("usage: ensign proposal check <file>")
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	terms, err := allDenylists()
	if err != nil {
		return err
	}
	if bridge, err := cwdBridge(); err == nil {
		if t, err := readDenylist(bridge); err == nil {
			terms = append(terms, t...)
		}
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		lines = append(lines, "+"+l)
	}
	hits := denylistHits("+++ b/"+filepath.Base(args[1])+"\n"+strings.Join(lines, "\n"), terms)
	if len(hits) > 0 {
		return fmt.Errorf("the proposal contains denylisted terms:\n  %s", strings.Join(hits, "\n  "))
	}
	fmt.Fprintln(out, "no denylisted terms found")
	return nil
}
