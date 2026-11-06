// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// liveRuns lists the bridge's runs that have not finished.
func liveRuns(bridge string) []runInfo {
	var out []runInfo
	for _, ri := range listRuns(bridge) {
		if !ri.state.terminal() {
			out = append(out, ri)
		}
	}
	return out
}

const bridgeClaudeMD = `# Bridge: %s

This is a starship bridge: its projects and tasks live here, and each task's code lives
in that task's workspace.

## Setup on a new machine

If ` + "`.starship/bin/ensign`" + ` does not exist and ` + "`bridge.yaml`" + ` names a remote, this bridge is not
set up on this machine. Tell the user, and with their OK run, from the bridge:

    mkdir -p .starship/bin
    curl -fsSL -o .starship/bin/ensign "<remote, without .git>/releases/download/<tag>/ensign-<platform>"
    chmod +x .starship/bin/ensign
    .starship/bin/ensign setup

` + "`<remote>`" + ` and ` + "`<tag>`" + ` are in ` + "`bridge.yaml`" + `. ` + "`<platform>`" + ` is ` + "`linux-amd64`" + `, ` + "`darwin-arm64`" + ` (Apple silicon),
` + "`darwin-amd64`" + ` (Intel Mac) or ` + "`windows-amd64.exe`" + ` (save it as ` + "`ensign.exe`" + ` and skip ` + "`chmod`" + `).
Setup fetches the control plane and checks the binary against the release's checksums.
Then ask them to restart claude. Where each project's code is on this machine is asked
the first time a task needs it.

## The heartbeat

` + heartbeat + `
`

// oldHeartbeat is the heartbeat paragraph that bridges made before it was scoped to the
// session agent carry; SessionStart replaces it with heartbeat.
const oldHeartbeat = `At session start a starship hook prints a line beginning ` + "`STARSHIP-READY`" + `, followed by
the session manual. **If that line is not in your context, or starship hooks report
errors, starship is not working in this session. Say so plainly and do nothing except
help fix it**: no runs, no agent spawns, no workspace changes, no commits.`

const heartbeat = `At session start a starship hook prints a line beginning ` + "`STARSHIP-READY`" + `, followed by
the session manual. This rule is for the session agent only; subagents never see that
line and ignore this section. **If that line is not in your context, or starship hooks
report errors, starship is not working in this session. Say so plainly and do nothing
except help fix it**: no runs, no agent spawns, no workspace changes, no commits.`

// sessionImport is the line older bridges used to load the manual through CLAUDE.md,
// which also put it in every subagent's context. SessionStart now removes it.
const sessionImport = "@.starship/session.md"

// bridgeGitattributes keeps a bridge's text files at LF on every machine.
const bridgeGitattributes = "* text=auto eol=lf\n"

const bridgeGitignore = `# Generated per machine by ensign; never committed.
.starship/
machine.yaml
.claude/agents/
.claude/settings.local.json
`

func hookSettings() string { return hookSettingsWith(hookTimeout, hookTimeout) }

// hookSettingsFor is the settings.json for a bridge's connector declaration. Declared is separate from the tools because a role with no tools still turns the matcher on.
func hookSettingsFor(declared bool, tools []string) string {
	return hookSettingsConn(hookTimeout, hookTimeout, declared, tools)
}

func hookSettingsWith(preTimeout, stopTimeout int) string {
	return hookSettingsConn(preTimeout, stopTimeout, false, nil)
}

// hookSettingsConn is the generated settings.json. A timeout of 0 leaves that hook without one, as older ensigns wrote it; SessionStart upgrades those forms. Declared connectors add the mcp__, resource-tool, Read and Grep matchers and allow the tools, sorted and without duplicates.
func hookSettingsConn(preTimeout, stopTimeout int, declared bool, tools []string) string {
	matcher, allow := "Agent|Task|Edit|Write|NotebookEdit|SubagentHandback", `"Bash(ensign:*)"`
	if declared {
		matcher += "|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool|Read|Grep"
		sorted := slices.Compact(slices.Sorted(slices.Values(tools)))
		for _, t := range sorted {
			allow += fmt.Sprintf(", %q", t)
		}
	}
	hook := func(ev string, timeout int) string {
		if timeout > 0 {
			return fmt.Sprintf(`{"type": "command", "command": %q, "timeout": %d}`, bridgeEnsign+" hook "+ev, timeout)
		}
		return fmt.Sprintf(`{"type": "command", "command": %q}`, bridgeEnsign+" hook "+ev)
	}
	pre := hook("PreToolUse", preTimeout)
	cmd := func(ev string) string {
		return "[{\"hooks\": [" + hook(ev, 0) + "]}]"
	}
	return `{
  "permissions": {
    "allow": [` + allow + `]
  },
  "hooks": {
    "SessionStart":     ` + cmd("SessionStart") + `,
    "UserPromptSubmit": ` + cmd("UserPromptSubmit") + `,
    "PreToolUse":       [{"matcher": "` + matcher + `", "hooks": [` + pre + `]}],
    "PostToolUse":      [{"matcher": "Agent|Task|Bash", "hooks": [{"type": "command", "command": ` + fmt.Sprintf("%q", bridgeEnsign+" hook PostToolUse") + `}]}],
    "SubagentStop":     [{"hooks": [` + hook("SubagentStop", stopTimeout) + `]}],
    "Stop":             ` + cmd("Stop") + `
  }
}
`
}

const initUsage = "usage: ensign init <dir> [--control-plane <path> | --remote <url> [--tag <tag>]]"

// cmdInit implements `ensign init`. With neither --control-plane nor --remote, a
// released ensign pins the release it is: the repo it was built from, at its version. A pinned bridge is also set up, as setup would, so one
// command makes a working bridge; --projects records where this machine keeps code.
func cmdInit(args []string, out io.Writer) error {
	var dir, cpPath, remote, tag string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--control-plane" && i+1 < len(args):
			cpPath, i = args[i+1], i+1
		case a == "--remote" && i+1 < len(args):
			remote, i = args[i+1], i+1
		case a == "--tag" && i+1 < len(args):
			tag, i = args[i+1], i+1
		case !strings.HasPrefix(a, "-") && dir == "":
			dir = a
		default:
			return fmt.Errorf(initUsage)
		}
	}
	if dir == "" || (cpPath != "" && (remote != "" || tag != "")) {
		return fmt.Errorf(initUsage)
	}
	if cpPath == "" {
		var err error
		if remote, tag, err = releaseOrigin(remote, tag); err != nil {
			return err
		}
	}
	dir, _ = filepath.Abs(dir)
	if _, err := os.Stat(filepath.Join(dir, "bridge.yaml")); err == nil {
		return fmt.Errorf("%s is already a bridge", dir)
	}
	var cp string
	if cpPath != "" {
		abs, _ := filepath.Abs(cpPath)
		cp = fmt.Sprintf("control-plane:\n  path: %s\n", filepath.ToSlash(abs))
	} else {
		cp = fmt.Sprintf("control-plane:\n  remote: %s\n  tag: %s\n", remote, tag)
	}
	files := map[string]string{
		"bridge.yaml":           "# A starship bridge. See the control plane's docs/design.md.\n" + cp,
		"CLAUDE.md":             fmt.Sprintf(bridgeClaudeMD, filepath.Base(dir)),
		".gitignore":            bridgeGitignore,
		".claude/settings.json": hookSettings(),
		"projects/.gitkeep":     "",
	}
	// A .gitattributes the folder already has is the user's: leave it, and don't commit it.
	if _, err := os.Stat(filepath.Join(dir, ".gitattributes")); err != nil {
		files[".gitattributes"] = bridgeGitattributes
	}
	paths := sortedKeys(files)
	// The folder may already hold the user's own files. Never overwrite one.
	var taken []string
	for _, rel := range paths {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			taken = append(taken, rel)
		}
	}
	if len(taken) > 0 {
		return fmt.Errorf("%s already has %s; init won't overwrite them. Use an empty folder, or move them aside",
			dir, strings.Join(taken, ", "))
	}
	for _, rel := range paths {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(files[rel]), 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := git(dir, "init", "-q"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "bridge created at %s\n", dir)
	// A bridge pinned to a release is set up on this machine straight away. One that
	// follows a local control plane (development) is only linked, unless asked.
	if cpPath == "" {
		if err := setupBridge(dir, out); err != nil {
			return err
		}
	} else if err := link(dir, false, out); err != nil {
		return err
	}
	// Commit the starting files, and only those: the folder may hold other files. Without
	// this, the first session is asked to commit them as journal changes.
	if err := git(dir, append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	if err := git(dir, append([]string{"commit", "-q", "-m", "New bridge", "--"}, paths...)...); err != nil {
		fmt.Fprintf(out, "could not commit the new bridge (%v); commit its files before starting a session\n", err)
	}
	return nil
}

// releaseOrigin fills in the release a bridge pins: by default the repo this ensign was
// released from and its own version, both set when the release is built.
func releaseOrigin(remote, tag string) (string, string, error) {
	if remote == "" {
		remote = releaseRepo
	}
	if tag == "" && strings.HasPrefix(version, "v") {
		tag = version
	}
	switch {
	case remote == "":
		return "", "", fmt.Errorf("say which starship to use: pass --remote <url> (or --control-plane <path>)")
	case tag == "":
		return "", "", fmt.Errorf("say which release to pin: pass --tag <tag>")
	}
	return remote, tag, nil
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	// Trim only the end: porcelain status lines start with a meaningful space.
	return strings.TrimRight(string(out), "\r\n"), err
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

type linkManifest struct {
	Files map[string]string `yaml:"files"` // path relative to the bridge -> sha256 of what link wrote
}

var modelLineRe = regexp.MustCompile(`(?m)^(model:[ \t]*)(.*?)(\r?)$`)

// frontmatterReplaces is the role file's declared replaces: from its frontmatter; empty when absent.
func frontmatterReplaces(label string, data []byte) (string, error) {
	front, _, err := splitFrontmatterData(label, data)
	if err != nil {
		return "", err
	}
	var fm struct {
		Replaces string `yaml:"replaces"`
	}
	if err := decodeFrontmatter(front, &fm); err != nil {
		return "", fmt.Errorf("%s: frontmatter: %v", label, err)
	}
	return strings.TrimSpace(fm.Replaces), nil
}

// agentName is the name Claude Code knows a role file by: its frontmatter name, or the file stem when there is none. An unreadable frontmatter gives the stem and the error.
func agentName(path string, data []byte) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(path), ".md")
	_, name, err := frontmatterMcpServers(path, data)
	if err != nil || name == "" {
		return stem, err
	}
	return name, nil
}

// bridgeEnsign is how hooks run the bridge's own ensign: every bridge has its copy, so
// nothing is installed for the machine and PATH doesn't matter.
const bridgeEnsign = `"$CLAUDE_PROJECT_DIR/.starship/bin/ensign"`

// ensignFile is where a bridge keeps its own ensign, relative to the bridge.
func ensignFile() string { return ".starship/bin/" + exeName() }

var toolsLineRe = regexp.MustCompile(`(?m)^(tools:[ \t]*)(.*?)(\r?)$`)

// frontmatterToolsLoc finds the tools: line inside a role file's frontmatter, as submatch indices into data; a body line is never found. It is nil when there is no frontmatter or no such line.
func frontmatterToolsLoc(data []byte) []int {
	return frontmatterLineLoc(data, toolsLineRe)
}

// frontmatterLineLoc finds the first line re matches inside a role file's frontmatter, as submatch indices into data.
func frontmatterLineLoc(data []byte, re *regexp.Regexp) []int {
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return nil
	}
	nl := bytes.IndexByte(data, '\n')
	end := bytes.Index(data[nl:], []byte("\n---"))
	if end < 1 {
		return nil
	}
	m := re.FindSubmatchIndex(data[nl+1 : nl+end])
	if m == nil {
		return nil
	}
	for i := range m {
		m[i] += nl + 1
	}
	return m
}

// frontmatterToolsValue decodes data's frontmatter and returns what YAML reads for tools:, which is nil when absent.
func frontmatterToolsValue(label string, data []byte) (any, error) {
	front, _, err := splitFrontmatterData(label, data)
	if err != nil {
		return nil, err
	}
	var fm struct {
		Tools any `yaml:"tools"`
	}
	if err := decodeFrontmatter(front, &fm); err != nil {
		return nil, fmt.Errorf("%s: frontmatter: %v", label, err)
	}
	return fm.Tools, nil
}

// withDeclaredTools appends a role's declared connector tools, sorted and without duplicates, to the tools: line in its frontmatter. A role with no such line is left alone. The line must be a plain comma list on one line, since it is edited as text; label names the role in the error.
func withDeclaredTools(label string, data []byte, declared []string) ([]byte, error) {
	m := frontmatterToolsLoc(data)
	if m == nil {
		return data, nil
	}
	value := strings.TrimSpace(string(data[m[4]:m[5]]))
	parsedTools, err := frontmatterToolsValue(label, data)
	if err != nil {
		return nil, err
	}
	// YAML must read the line as the very string the text edit sees: quotes, comments, flow or block forms, continuation lines and null all read differently.
	if parsed, ok := parsedTools.(string); !ok || parsed != value || value == "" {
		return nil, fmt.Errorf("%s: connectors are declared for this role, so its frontmatter tools: must be a plain comma-separated list on one line, like `tools: Read, Grep`, with no quotes, brackets, block list, comment or continuation line", label)
	}
	var have []string
	for _, t := range strings.Split(value, ",") {
		if t = strings.TrimSpace(t); t != "" {
			have = append(have, t)
		}
	}
	list := have
	for _, t := range connectorTools(map[string][]string{"": declared}) {
		if !slices.Contains(have, t) {
			list = append(list, t)
		}
	}
	out := append([]byte{}, data[:m[4]]...)
	out = append(out, strings.Join(list, ", ")...)
	return append(out, data[m[5]:]...), nil
}

// splitToolTokens splits a tools: value as Claude Code does: on commas and on spaces outside parentheses, so `Read Bash` is two tools and `Bash(git status)` is one.
func splitToolTokens(v string) []string {
	var tokens []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			tokens = append(tokens, t)
		}
		cur.Reset()
	}
	for _, r := range v {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case (r == ',' || r == ' ') && depth == 0:
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return tokens
}

// readerToolsProblem says why a role's own frontmatter tools: can't stand in for the reader's: it grants a shell, everything, or the Agent or Task tool (which can start a role that has a shell), or it doesn't list tools at all (a role with no list gets every tool). It is "" when the list is fine.
func readerToolsProblem(label string, data []byte) string {
	value, err := frontmatterToolsValue(label, data)
	var tools []string
	switch v := value.(type) {
	case string:
		tools = splitToolTokens(v)
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return "its frontmatter tools: can't be read as a list of tool names"
			}
			tools = append(tools, splitToolTokens(s)...)
		}
	}
	if err != nil {
		return "its frontmatter tools: can't be read as a list of tool names"
	}
	listed := false
	for _, t := range tools {
		name, _, _ := strings.Cut(strings.TrimSpace(t), "(")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		listed = true
		if name == "*" || strings.EqualFold(name, "Bash") || strings.EqualFold(name, "PowerShell") || strings.EqualFold(name, "Agent") || strings.EqualFold(name, "Task") {
			return fmt.Sprintf("its frontmatter tools: grants %s", strings.TrimSpace(t))
		}
	}
	if !listed {
		return "it needs a frontmatter tools: line listing its tools, without Bash"
	}
	return ""
}

// generatedFiles is everything link writes into a bridge, by path relative to it.
func generatedFiles(bridge string, bf *BridgeFile, cp string) (map[string][]byte, error) {
	files := map[string][]byte{}
	// A pinned bridge downloads its release's ensign (fetchEnsign). One that follows a
	// local control plane (development) gets a copy of the ensign running now.
	if bf.ControlPlane.Path != "" {
		src, _ := os.Executable()
		bin, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("reading ensign: %v", err)
		}
		files[ensignFile()] = bin
	}
	manual, err := os.ReadFile(filepath.Join(cp, "manual", "session.md"))
	if err != nil {
		return nil, err
	}
	files[".starship/session.md"] = manual
	topics, err := filesIn(filepath.Join(cp, "manual", "topics"), ".md")
	if err != nil {
		return nil, err
	}
	for _, f := range topics {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		files[".starship/topics/"+filepath.Base(f)] = data
	}

	rules, err := bf.connectorRules()
	if err != nil {
		return nil, err
	}
	roles := map[string][]byte{}
	sources := map[string]string{}
	// effective is the name Claude Code knows each role by: its frontmatter name, or its file stem when there is none or it can't be read.
	effective := map[string]string{}
	cpRoles, err := filesIn(filepath.Join(cp, "roles"), ".md")
	if err != nil {
		return nil, err
	}
	for _, f := range cpRoles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		if _, declared := bf.Connectors[name]; name == "reader" && !declared {
			continue
		}
		roles[name], sources[name] = data, f
		effective[name], _ = agentName(f, data)
	}
	var bridgeRoles []string
	for _, d := range append([]string{filepath.Join(bridge, "roles")}, extensionRoleDirs(bridge, cp)...) {
		found, err := filesIn(d, ".md")
		if err != nil {
			return nil, err
		}
		bridgeRoles = append(bridgeRoles, found...)
	}
	for _, f := range bridgeRoles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		// The hook matches agent_type, which is the frontmatter name; an unreadable frontmatter falls back to the file name and is reported by the check below.
		agentType, _ := agentName(f, data)
		// hits maps each existing role this file collides with to the names (file stem, frontmatter name) that hit it.
		hits := map[string][]string{}
		if _, ok := roles[name]; ok {
			hits[name] = append(hits[name], name)
		}
		for _, k := range sortedKeys(roles) {
			if effective[k] == agentType && !slices.Contains(hits[k], agentType) {
				hits[k] = append(hits[k], agentType)
			}
		}
		replaced := sortedKeys(hits)
		switch len(replaced) {
		case 0:
		case 1:
			replaces, err := frontmatterReplaces(f, data)
			if err != nil {
				return nil, err
			}
			names := hits[replaced[0]]
			if !slices.Contains(names, replaces) {
				return nil, fmt.Errorf("%s: a role named %q already exists; declare `replaces: %s` to replace it", f, names[0], names[0])
			}
		default:
			return nil, fmt.Errorf("%s: roles named %q and %q already exist, and one replaces: can't replace both", f, hits[replaced[0]][0], hits[replaced[1]][0])
		}
		for _, k := range replaced {
			delete(roles, k)
			delete(sources, k)
			delete(effective, k)
		}
		if _, declared := bf.Connectors["reader"]; declared && agentType == "reader" {
			if why := readerToolsProblem(f, data); why != "" {
				return nil, fmt.Errorf("%s: role %q replaces the reader, whose write limit holds only because it has no shell, and a role that can start other roles can reach one with a shell: %s", f, name, why)
			}
		}
		roles[name], sources[name], effective[name] = data, f, agentType
	}
	if _, writesDeclared, _ := bf.connectorWrites(); len(rules) > 0 || writesDeclared {
		for _, name := range sortedKeys(roles) {
			if has, _, err := frontmatterMcpServers(sources[name], roles[name]); err != nil {
				return nil, fmt.Errorf("%s: connector rules are declared, so a role's frontmatter must parse: %v", sources[name], err)
			} else if has {
				return nil, fmt.Errorf("%s: role %q sets mcpServers, which bypasses the connector rules in bridge.yaml; remove it", sources[name], name)
			}
		}
	}
	for name, data := range roles {
		if o, ok := bf.Roles[name]; ok && o.Model != "" {
			if m := frontmatterLineLoc(data, modelLineRe); m != nil {
				data = slices.Concat(data[:m[0]], []byte("model: "+o.Model), data[m[6]:])
			}
		}
		if declared := rules[effective[name]]; len(declared) > 0 {
			var err error
			if data, err = withDeclaredTools(sources[name], data, declared); err != nil {
				return nil, err
			}
		}
		files[".claude/agents/"+name+".md"] = data
	}
	return files, nil
}

// link generates the bridge's role files and session manual from its control plane.
func link(bridge string, force bool, out io.Writer) error {
	if live := liveRuns(bridge); len(live) > 0 {
		return fmt.Errorf("run %s is active; link changes the roles under it, so it waits until no run is active", live[0].id)
	}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return err
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return err
	}
	want, err := generatedFiles(bridge, bf, cp)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(bridge, ".starship", "link.yaml")
	var old linkManifest
	readYAML(manifestPath, &old)
	for rel, hash := range old.Files {
		data, err := os.ReadFile(filepath.Join(bridge, filepath.FromSlash(rel)))
		if err == nil && sha(data) != hash && !force {
			return fmt.Errorf("%s was edited by hand; change it in the control plane or in a bridge role instead (or link --force to overwrite)", rel)
		}
	}
	changed := 0
	m := linkManifest{Files: map[string]string{}}
	for rel, data := range want {
		p := filepath.Join(bridge, filepath.FromSlash(rel))
		if cur, err := os.ReadFile(p); err != nil || !bytes.Equal(cur, data) {
			if err := replaceFile(p, data, rel == ensignFile()); err != nil {
				return err
			}
			changed++
		}
		m.Files[rel] = sha(data)
	}
	for rel := range old.Files {
		if _, keep := want[rel]; !keep {
			p := filepath.Join(bridge, filepath.FromSlash(rel))
			os.Remove(p)
			for d := filepath.Dir(p); d != bridge && strings.HasPrefix(d, bridge); d = filepath.Dir(d) {
				if os.Remove(d) != nil {
					break
				}
			}
			changed++
		}
	}
	if err := writeYAML(manifestPath, m); err != nil {
		return err
	}
	if out != nil {
		for _, l := range brokenRoleLines(bridge, cp) {
			fmt.Fprintln(out, l)
		}
		fmt.Fprintf(out, "linked %d files from %s (%d changed)\n", len(want), cp, changed)
	}
	return nil
}

// renameAside is set where a running executable cannot be renamed over but can be renamed aside (Windows); tests flip it.
var renameAside = runtime.GOOS == "windows"

// rename is os.Rename; tests swap it to fail a step of replaceFile.
var rename = os.Rename

// replaceFile writes a file under a temporary name and renames it into place, so a
// binary can be replaced while it is running (the hook that relinks is that binary).
// Where renameAside is set, an existing executable is first renamed to p+".old" (removing a stale one), and renamed back if the new one cannot take its place; removeOldExe deletes the leftover on a later run.
func replaceFile(p string, data []byte, executable bool) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	tmp := p + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	os.Chmod(tmp, mode)
	aside := false
	if renameAside && executable {
		if _, err := os.Lstat(p); err == nil {
			os.Remove(p + ".old")
			if err := rename(p, p+".old"); err != nil {
				os.Remove(tmp)
				return err
			}
			aside = true
		}
	}
	if err := rename(tmp, p); err != nil {
		if aside {
			rename(p+".old", p)
		}
		os.Remove(tmp)
		return err
	}
	return nil
}

// removeOldExe deletes the ensign that an update renamed aside; it may still be running in another process, so a failure is ignored.
func removeOldExe(bridge string) {
	os.Remove(filepath.Join(bridge, filepath.FromSlash(ensignFile())) + ".old")
}

// linkStale reports whether the generated files differ from what link would write now.
func linkStale(bridge string) bool {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return false
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return false
	}
	want, err := generatedFiles(bridge, bf, cp)
	if err != nil {
		return true
	}
	var m linkManifest
	readYAML(filepath.Join(bridge, ".starship", "link.yaml"), &m)
	if len(m.Files) != len(want) {
		return true
	}
	for rel, data := range want {
		if m.Files[rel] != sha(data) {
			return true
		}
	}
	return false
}

const projectHeader = `# Project: %s

# Where its code is differs per machine, so it is in this machine's machine.yaml.
`

const projectPlatforms = `
# The platforms the project supports; reviewers treat a failure on one of them as real.
platforms: [%s]
`

const projectTail = `
# Bindings say what each @name means here. Declare every binding the pipelines need,
# or declare it not applicable with a reason; a missing one stops a run.
bindings:
  # build: { command: "make" }
  # test:  { command: "make test" }
  # style: { guidance: "..." }

# Pipeline changes for this project, e.g.:
# pipelines:
#   coding:
#     ops:
#       - append: { stage: review, dimensions: [concurrency] }
`

const projectTemplate = projectHeader + projectPlatforms + projectTail

// noCodeProjectContent is the project file of a project whose tasks have no code by default; platforms are only written when given.
func noCodeProjectContent(name string, platforms []string) string {
	content := fmt.Sprintf(projectHeader, name) + "\n# Tasks in this project have no code unless they say pipeline: coding.\npipeline: none\n"
	if len(platforms) > 0 {
		content += fmt.Sprintf(projectPlatforms, strings.Join(platforms, ", "))
	}
	return content + projectTail
}

func cmdProject(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign project new <name> --workspace <path> | --locations <a,b> [--platforms <list>] | ensign project new <name> --pipeline none [--workspace <path> | --locations <a,b>] [--platforms <list>] | ensign project reviewed <project>[/<task>] [--location <name>]")
	if len(args) >= 2 && args[0] == "reviewed" {
		switch {
		case len(args) == 2:
			return cmdProjectReviewed(args[1], "", out)
		case len(args) == 4 && args[2] == "--location":
			return cmdProjectReviewed(args[1], args[3], out)
		}
		return usage
	}
	if len(args) < 4 || args[0] != "new" {
		return usage
	}
	var workspace, platformList, locationList, pipeline string
	for i := 2; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return usage
		}
		switch args[i] {
		case "--workspace":
			workspace = args[i+1]
		case "--locations":
			locationList = args[i+1]
		case "--platforms":
			platformList = args[i+1]
		case "--pipeline":
			pipeline = args[i+1]
		default:
			return usage
		}
	}
	if pipeline != "" && pipeline != "none" {
		return fmt.Errorf("--pipeline must be none (a project of tasks without code); a project without it is a code project")
	}
	noCode := pipeline == "none"
	if workspace != "" && locationList != "" || (workspace == "" && locationList == "" && !noCode) {
		return usage
	}
	var platforms []string
	if platformList != "" {
		for _, pl := range strings.Split(platformList, ",") {
			platforms = append(platforms, strings.TrimSpace(pl))
		}
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	deliverTopic(bridge, "project", out)
	name := args[1]
	if !validName(name) {
		return fmt.Errorf("project names are lower-case letters, digits and dashes")
	}
	if windowsReserved(name) {
		return fmt.Errorf("project name %q is a name Windows reserves, and the name becomes a committed folder", name)
	}
	p := filepath.Join(bridge, "projects", name, "project.yaml")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("project %s already exists", name)
	}
	if locationList != "" {
		return newLocationsProject(bridge, name, p, locationList, platforms, platformList, noCode, out)
	}
	if workspace == "" {
		if errs := platformProblems(platforms); len(errs) > 0 {
			return fmt.Errorf("--platforms %q: %s", platformList, strings.Join(errs, "; "))
		}
		if err := writeFile(p, noCodeProjectContent(name, platforms)); err != nil {
			return err
		}
		fmt.Fprintf(out, "created %s, a project of tasks without code\n", relTo(bridge, p))
		return nil
	}
	dir, err := filepath.Abs(expandHome(workspace))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a folder on this machine; ask the user where the code is, never guess", dir)
	}
	shipped, shippedPlatforms, err := shippedProject(dir)
	if err != nil {
		return err
	}
	switch {
	case len(platforms) > 0 && len(shippedPlatforms) > 0 && !sameSet(platforms, shippedPlatforms):
		return fmt.Errorf("--platforms %s differs from the platforms in the code's own %s (%s); ask the user which is right", platformList, shippedProjectFile, strings.Join(shippedPlatforms, ","))
	case len(platforms) == 0 && len(shippedPlatforms) == 0 && !noCode:
		return fmt.Errorf("a project declares the platforms it supports: pass --platforms <list>, e.g. --platforms linux,macos,windows; ask the user, never guess")
	}
	if errs := platformProblems(platforms); len(errs) > 0 {
		return fmt.Errorf("--platforms %q: %s", platformList, strings.Join(errs, "; "))
	}
	content := fmt.Sprintf(projectTemplate, name, strings.Join(platforms, ", "))
	if noCode {
		content = noCodeProjectContent(name, platforms)
	}
	if shipped != "" {
		content = fmt.Sprintf(projectHeader, name) + "\n# From the code's own " + shippedProjectFile + ":\n\n" + shipped
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if len(shippedPlatforms) == 0 && len(platforms) > 0 {
			content += fmt.Sprintf("platforms: [%s]\n", strings.Join(platforms, ", "))
		}
		if noCode {
			content += "pipeline: none\n"
		}
	}
	if err := writeFile(p, content); err != nil {
		return err
	}
	if _, err := setWorkspace(bridge, name, dir); err != nil {
		return err
	}
	if shipped != "" {
		fmt.Fprintf(out, "created %s from the code's own %s; confirm its bindings with the user\n", relTo(bridge, p), shippedProjectFile)
		return nil
	}
	fmt.Fprintf(out, "created %s; define its bindings before starting a run\n", relTo(bridge, p))
	return nil
}

// newLocationsProject writes a project that declares code locations in place of a workspace; each location's folder is recorded afterwards, per machine.
func newLocationsProject(bridge, name, file, locationList string, platforms []string, platformList string, noCode bool, out io.Writer) error {
	var names []string
	for _, n := range strings.Split(locationList, ",") {
		n = strings.TrimSpace(n)
		if !validName(n) {
			return fmt.Errorf("--locations %q: name each location with lower-case letters, digits and dashes", locationList)
		}
		if windowsReserved(n) {
			return fmt.Errorf("--locations %q: %s is a name Windows reserves", locationList, n)
		}
		if contains(names, n) {
			return fmt.Errorf("--locations %q: %s is named twice", locationList, n)
		}
		names = append(names, n)
	}
	if len(platforms) == 0 && !noCode {
		return fmt.Errorf("a project declares the platforms it supports: pass --platforms <list>, e.g. --platforms linux,macos,windows; ask the user, never guess")
	}
	if errs := platformProblems(platforms); len(errs) > 0 {
		return fmt.Errorf("--platforms %q: %s", platformList, strings.Join(errs, "; "))
	}
	content := fmt.Sprintf(projectTemplate, name, strings.Join(platforms, ", "))
	if noCode {
		content = noCodeProjectContent(name, platforms)
	}
	content += "\n# The code locations; each location's folder is recorded per machine.\nlocations:\n"
	for _, n := range names {
		content += "  " + n + ": {}\n"
	}
	if err := writeFile(file, content); err != nil {
		return err
	}
	fmt.Fprintf(out, "created %s with locations %s; define its bindings before starting a run\n", relTo(bridge, file), strings.Join(names, ", "))
	for _, n := range names {
		fmt.Fprintf(out, "record where %s is on this machine: ensign workspace %s --location %s <path>\n", n, name, n)
	}
	return nil
}

func sameSet(a, b []string) bool {
	in := map[string]bool{}
	for _, s := range a {
		in[platformName(s)] = true
	}
	for _, s := range b {
		s = platformName(s)
		if !in[s] {
			return false
		}
		delete(in, s)
	}
	return len(in) == 0
}

// A repository can ship the project settings a bridge needs to work on it, so adding it
// as a project needs nothing filled in. It is read once, when the project is created.
const shippedProjectFile = ".starship/project.yaml"

func shippedProject(dir string) (string, []string, error) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(shippedProjectFile)))
	if err != nil {
		return "", nil, nil
	}
	var l struct {
		Workspace string   `yaml:"workspace"`
		Platforms []string `yaml:"platforms"`
	}
	if err := yamlUnmarshal(data, &l); err != nil {
		return "", nil, fmt.Errorf("%s in %s is not valid YAML: %v", shippedProjectFile, dir, err)
	}
	if l.Workspace != "" {
		return "", nil, fmt.Errorf("%s in %s sets a workspace; that is the bridge's to decide", shippedProjectFile, dir)
	}
	var keys map[string]any
	if err := yamlUnmarshal(data, &keys); err != nil {
		return "", nil, fmt.Errorf("%s in %s is not valid YAML: %v", shippedProjectFile, dir, err)
	}
	if _, has := keys["platforms"]; has && len(l.Platforms) == 0 {
		return "", nil, fmt.Errorf("%s in %s declares an empty platforms:; fill it in there, e.g. platforms: [linux, macos, windows]", shippedProjectFile, dir)
	}
	var locs struct {
		Locations map[string]yaml.Node `yaml:"locations"`
	}
	yamlUnmarshal(data, &locs)
	declared := map[string]yaml.Node{}
	documented := map[string]yaml.Node{}
	for name, node := range locs.Locations {
		var loc struct {
			Platforms     yaml.Node `yaml:"platforms"`
			Documentation yaml.Node `yaml:"documentation"`
		}
		guardedDecode(func() error { return node.Decode(&loc) })
		declared[name] = loc.Platforms
		documented[name] = loc.Documentation
	}
	if err := refuseLocationPlatforms(fmt.Sprintf("%s in %s", shippedProjectFile, dir), declared); err != nil {
		return "", nil, err
	}
	if err := refuseLocationDocumentation(fmt.Sprintf("%s in %s", shippedProjectFile, dir), documented); err != nil {
		return "", nil, err
	}
	for _, name := range sortedKeys(declared) {
		if !validName(name) {
			return "", nil, fmt.Errorf("%s in %s: location %q: name it with lower-case letters, digits and dashes", shippedProjectFile, dir, name)
		}
		if windowsReserved(name) {
			return "", nil, fmt.Errorf("%s in %s: location %q: a name Windows reserves", shippedProjectFile, dir, name)
		}
	}
	if errs := platformProblems(l.Platforms); len(errs) > 0 {
		return "", nil, fmt.Errorf("%s in %s: %s", shippedProjectFile, dir, strings.Join(errs, "; "))
	}
	return string(data), l.Platforms, nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validName(s string) bool { return nameRe.MatchString(s) }

func writeFile(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func cwdBridge() (string, error) {
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		if b, err := findBridge(d); err == nil {
			return b, nil
		}
	}
	wd, _ := os.Getwd()
	return findBridge(wd)
}

const taskTemplate = `---
pipeline: %s
%s---

# %s

## Goal

## Scope

## Status

New.

## Outstanding

## Decisions

## Notes
`

func cmdTask(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign task new <project>/<task> [--workspace <path> | --locations <a,b>] [--branch <branch>] [--pipeline <none|coding>] | ensign task open <project>/<task> | ensign task close <project>/<task> | ensign task subtask <done|drop> <project>/<task> <name>")
	if len(args) < 2 {
		return usage
	}
	if args[0] == "subtask" {
		return cmdTaskSubtask(args[1:], out)
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	parts := strings.Split(args[1], "/")
	if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
		return fmt.Errorf("task must be <project>/<task>, lower-case letters, digits and dashes")
	}
	taskDir := filepath.Join(bridge, "projects", parts[0], "tasks", parts[1])
	switch args[0] {
	case "close":
		if len(args) != 2 {
			return usage
		}
		return cmdTaskClose(bridge, args[1], out)
	case "new":
		if windowsReserved(parts[1]) {
			return fmt.Errorf("task name %q is a name Windows reserves, and the name becomes a committed folder", parts[1])
		}
		var ws, branch, locations, pipeline string
		for i := 2; i+1 < len(args); i += 2 {
			switch args[i] {
			case "--pipeline":
				pipeline = args[i+1]
			case "--workspace":
				ws = args[i+1]
			case "--locations":
				locations = args[i+1]
			case "--branch":
				branch = args[i+1]
			default:
				return usage
			}
		}
		pf := filepath.Join(bridge, "projects", parts[0], "project.yaml")
		if _, err := os.Stat(pf); err != nil {
			return fmt.Errorf("no project %s (ensign project new %s --workspace <path> --platforms <list>)", parts[0], parts[0])
		}
		if _, err := os.Stat(filepath.Join(taskDir, "task.md")); err == nil {
			return fmt.Errorf("task %s already exists", args[1])
		}
		if pipeline != "" && pipeline != "none" && pipeline != "coding" {
			return fmt.Errorf("--pipeline must be none or coding, got %q", pipeline)
		}
		if pipeline == "" {
			dflt, err := projectPipeline(pf)
			if err != nil {
				return err
			}
			pipeline = "coding"
			if dflt == "none" {
				pipeline = "none"
			}
		}
		if pipeline == "none" {
			for _, f := range []struct{ name, val string }{{"--workspace", ws}, {"--locations", locations}, {"--branch", branch}} {
				if f.val != "" {
					return fmt.Errorf("task %s has no pipeline (a task without code), so %s has no meaning", args[1], f.name)
				}
			}
			content := fmt.Sprintf(taskTemplate, pipeline, "", parts[1])
			return createTaskFiles(bridge, args[1], taskDir, content, out)
		}
		locs, err := loadLocations(bridge, parts[0])
		if err != nil {
			return err
		}
		var named []string
		for _, n := range strings.Split(locations, ",") {
			if n = strings.TrimSpace(n); n != "" {
				named = append(named, n)
			}
		}
		switch {
		case len(locs.Names()) == 0 && locations != "":
			return fmt.Errorf("project %s declares no locations, so --locations has no meaning", parts[0])
		case len(locs.Names()) == 0:
		case ws != "":
			return fmt.Errorf("project %s declares locations (%s); name the ones the task uses with --locations, not --workspace", parts[0], strings.Join(locs.Names(), ", "))
		default:
			if len(named) == 0 {
				return fmt.Errorf("project %s declares locations (%s); name the ones the task uses with --locations <a,b>", parts[0], strings.Join(locs.Names(), ", "))
			}
			for _, n := range named {
				if !locs.Has(n) {
					return fmt.Errorf("project %s declares no location %q (it declares %s)", parts[0], n, strings.Join(locs.Names(), ", "))
				}
				if _, err := locationPathFor(bridge, parts[0], n); err != nil {
					return err
				}
			}
		}
		if len(locs.Names()) == 0 {
			if ws != "" {
				if _, err := setWorkspace(bridge, args[1], ws); err != nil {
					return err
				}
			} else if _, err := workspaceFor(bridge, parts[0], ""); err != nil {
				return err
			}
		}
		vcs := ""
		if len(named) > 0 {
			vcs = "locations: [" + strings.Join(named, ", ") + "]\n"
		}
		if branch != "" {
			vcs += fmt.Sprintf("vcs: { kind: git, branch: %s }\n", branch)
		}
		content := fmt.Sprintf(taskTemplate, pipeline, vcs, parts[1])
		return createTaskFiles(bridge, args[1], taskDir, content, out)
	case "open":
		return openTask(bridge, args[1], taskDir, out)
	}
	return usage
}

// createTaskFiles writes a new task's task.md and history.md.
func createTaskFiles(bridge, task, taskDir, content string, out io.Writer) error {
	name := filepath.Base(taskDir)
	if err := writeFile(filepath.Join(taskDir, "task.md"), content); err != nil {
		return err
	}
	created := historyHeading(name) + "- " + time.Now().Format("2006-01-02") + ": created.\n"
	if err := writeFile(filepath.Join(taskDir, "history.md"), created); err != nil {
		return err
	}
	fmt.Fprintf(out, "created %s; fill in its goal and scope\n", relTo(bridge, filepath.Join(taskDir, "task.md")))
	return nil
}

// openTask checks a task's workspace and VCS state, records it as the session's task,
// and prints where it stands.
func openTask(bridge, task, taskDir string, out io.Writer) error {
	taskFile := filepath.Join(taskDir, "task.md")
	front, body, err := splitFrontmatter(taskFile)
	if err != nil {
		return err
	}
	var tf TaskFile
	if err := decodeFrontmatter(front, &tf); err != nil {
		return fmt.Errorf("%s: %v", taskFile, err)
	}
	if err := checkTaskKeys(taskFile, front); err != nil {
		return err
	}
	fmt.Fprintf(out, "task       %s\n", task)
	if p := subtaskProgress(bridge, task); p != "" {
		fmt.Fprintf(out, "subtasks   %s\n", strings.TrimPrefix(p, "subtasks "))
	}
	project, name, _ := strings.Cut(task, "/")
	noCode, err := taskHasNoCode(bridge, project, name)
	if err != nil {
		return err
	}
	if noCode {
		fmt.Fprintln(out, "pipeline   none: a task without code, so it has no workspace, no version control checks and no runs; its journal takes dated entries (ensign help journal)")
	} else {
		if len(tf.Locations) > 0 {
			for _, loc := range tf.Locations {
				p, err := locationPathFor(bridge, project, loc)
				switch {
				case err != nil:
					fmt.Fprintf(out, "location   %s: STOP: %v\n", loc, err)
				default:
					fmt.Fprintf(out, "location   %s: %s\n", loc, p)
					fmt.Fprintln(out, "vcs        "+vcsState(p, tf.Vcs))
				}
			}
		}
		ws, err := workspaceFor(bridge, project, name)
		switch {
		case len(tf.Locations) > 0:
			for _, loc := range tf.Locations {
				if p, err := locationPathFor(bridge, project, loc); err == nil {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						printShippedDrift(bridge, project, p, loc, out)
					}
				}
			}
		case err != nil:
			fmt.Fprintf(out, "workspace  STOP: %v\n", err)
		default:
			if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
				fmt.Fprintf(out, "workspace  STOP: %s does not exist\n", ws)
			} else {
				fmt.Fprintf(out, "workspace  %s\n", ws)
				fmt.Fprintln(out, "vcs        "+vcsState(ws, tf.Vcs))
				printShippedDrift(bridge, project, ws, "", out)
			}
		}
	}
	printLinks(bridge, project, filepath.Join(taskDir, "task.md"), front, out)
	notes, err := projectNotes(bridge, project)
	if err != nil {
		return err
	}
	fmt.Fprint(out, notes)
	fmt.Fprintf(out, "\n%s\n", strings.TrimSpace(string(body)))
	if sid := os.Getenv("STARSHIP_SESSION"); sid != "" {
		writeFile(sessionFile(bridge, sid)+".task", task+"\n")
	}
	return nil
}

// projectNotes is the block task open prints for a project's project.md, empty when the
// file is missing or blank.
func projectNotes(bridge, project string) (string, error) {
	notes, err := os.ReadFile(filepath.Join(bridge, "projects", project, "project.md"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	n := strings.TrimSpace(string(notes))
	if n == "" {
		return "", nil
	}
	return fmt.Sprintf("\nnotes      projects/%s/project.md\n\n%s\n", project, n), nil
}

func vcsState(ws string, v TaskVcs) string {
	if _, err := os.Stat(filepath.Join(ws, ".git")); err == nil || v.Kind == "git" {
		branch, err := gitOut(ws, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			if branch, err = gitOut(ws, "rev-parse", "--short", "HEAD"); err != nil {
				return "git: could not read the branch"
			}
			branch = "detached at " + branch
		}
		var changed string
		if status, err := gitOut(ws, "status", "--porcelain"); err != nil {
			changed = fmt.Sprintf("status unknown (git status: %v)", err)
		} else {
			dirty := 0
			if status != "" {
				dirty = len(strings.Split(status, "\n"))
			}
			changed = fmt.Sprintf("%d changed files", dirty)
		}
		s := fmt.Sprintf("git, on %s, %s", branch, changed)
		if v.Branch != "" && v.Branch != branch {
			s += fmt.Sprintf(" — STOP: the task expects branch %s", v.Branch)
		}
		return s
	}
	if v.Kind != "" {
		return v.Kind + " (state not inspected)"
	}
	return "no VCS detected"
}

// section returns the body of a "## name" section of a markdown document.
func section(doc, name string) string {
	lines := strings.Split(doc, "\n")
	var out []string
	in := false
	for _, l := range lines {
		if strings.HasPrefix(l, "## ") {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(l[3:]), name)
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

// workItem is what a run is asked to do: the task's Goal and Scope, or its whole body.
func workItem(body string) string {
	var parts []string
	for _, sec := range []string{"Goal", "Scope"} {
		if t := section(body, sec); t != "" {
			parts = append(parts, "## "+sec+"\n\n"+t)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(body)
	}
	return strings.Join(parts, "\n\n")
}

func platformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

func sortedFiles(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
