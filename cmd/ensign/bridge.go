package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
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

If ` + "`.starship/control-plane/`" + ` does not exist and ` + "`bridge.yaml`" + ` names a remote, this bridge
is not set up on this machine. Tell the user, and with their OK run, from the bridge:

    git clone --depth 1 --branch <tag from bridge.yaml> <remote> .starship/control-plane
    .starship/control-plane/bin/<platform>/ensign setup --projects <path>

` + "`<platform>`" + ` is ` + "`linux-amd64`" + ` or ` + "`windows-amd64`" + `. Ask the user where this machine keeps its
workspaces (the projects root) for ` + "`--projects`" + `. Then ask them to restart claude.

@.starship/session.md
`

const bridgeGitignore = `# Generated per machine by ensign; never committed.
.starship/
.claude/agents/
.claude/skills/
.claude/settings.local.json
`

const bridgeDenylist = `# Terms that must never reach the public control plane from this bridge:
# project names, codenames, internal hostnames, usernames. One per line; matched
# case-insensitively. The control plane refuses commits while this list is empty.
`

func hookSettings() string {
	cmd := func(ev string) string {
		return fmt.Sprintf(`[{"hooks": [{"type": "command", "command": "ensign hook %s"}]}]`, ev)
	}
	return `{
  "hooks": {
    "SessionStart":     ` + cmd("SessionStart") + `,
    "UserPromptSubmit": ` + cmd("UserPromptSubmit") + `,
    "PreToolUse":       [{"matcher": "Agent|Task|Edit|Write|NotebookEdit", "hooks": [{"type": "command", "command": "ensign hook PreToolUse"}]}],
    "SubagentStop":     ` + cmd("SubagentStop") + `,
    "Stop":             ` + cmd("Stop") + `
  }
}
`
}

// cmdInit implements `ensign init <dir> (--control-plane <path> | --remote <url> --tag <tag>)`.
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
			return fmt.Errorf("usage: ensign init <dir> (--control-plane <path> | --remote <url> --tag <tag>)")
		}
	}
	if dir == "" || (cpPath == "") == (remote == "") || (remote != "" && tag == "") {
		return fmt.Errorf("usage: ensign init <dir> (--control-plane <path> | --remote <url> --tag <tag>)")
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
		"denylist.txt":          bridgeDenylist,
		"projects/.gitkeep":     "",
		"proposals/.gitkeep":    "",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := git(dir, "init", "-q"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "bridge created at %s\n", dir)
	if cpPath != "" {
		return link(dir, false, out)
	}
	fmt.Fprintln(out, "run `ensign setup` in it to fetch the control plane and generate its files")
	return nil
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

var frontmatterField = func(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + name + `:\s*(.*)$`)
}

// generatedFiles is everything link writes into a bridge, by path relative to it.
func generatedFiles(bridge string, bf *BridgeFile, cp string) (map[string][]byte, error) {
	files := map[string][]byte{}
	manual, err := os.ReadFile(filepath.Join(cp, "manual", "session.md"))
	if err != nil {
		return nil, err
	}
	files[".starship/session.md"] = manual

	roles := map[string][]byte{}
	cpRoles, _ := filepath.Glob(filepath.Join(cp, "roles", "*.md"))
	for _, f := range cpRoles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		roles[strings.TrimSuffix(filepath.Base(f), ".md")] = data
	}
	var bridgeRoles []string
	for _, d := range append([]string{filepath.Join(bridge, "roles")}, bundleRoleDirs(bridge, cp)...) {
		found, _ := filepath.Glob(filepath.Join(d, "*.md"))
		bridgeRoles = append(bridgeRoles, found...)
	}
	for _, f := range bridgeRoles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		if _, clash := roles[name]; clash {
			m := frontmatterField("replaces").FindSubmatch(data)
			if m == nil || strings.TrimSpace(string(m[1])) != name {
				return nil, fmt.Errorf("%s: a role named %q already exists; declare `replaces: %s` to replace it", f, name, name)
			}
		}
		roles[name] = data
	}
	for name, data := range roles {
		if o, ok := bf.Roles[name]; ok && o.Model != "" {
			re := frontmatterField("model")
			if re.Match(data) {
				data = re.ReplaceAll(data, []byte("model: "+o.Model))
			}
		}
		files[".claude/agents/"+name+".md"] = data
	}
	for rel, data := range onDemandSkills(bridge, cp) {
		files[rel] = data
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
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return err
			}
			changed++
		}
		m.Files[rel] = sha(data)
	}
	for rel := range old.Files {
		if _, keep := want[rel]; !keep {
			os.Remove(filepath.Join(bridge, filepath.FromSlash(rel)))
			changed++
		}
	}
	if err := writeYAML(manifestPath, m); err != nil {
		return err
	}
	if out != nil {
		fmt.Fprintf(out, "linked %d files from %s (%d changed)\n", len(want), cp, changed)
	}
	return nil
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

const projectTemplate = `# Project: %s
#
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

func cmdProject(args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "new" {
		return fmt.Errorf("usage: ensign project new <name>")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	name := args[1]
	if !validName(name) {
		return fmt.Errorf("project names are lower-case letters, digits and dashes")
	}
	p := filepath.Join(bridge, "projects", name, "project.yaml")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("project %s already exists", name)
	}
	if err := writeFile(p, fmt.Sprintf(projectTemplate, name)); err != nil {
		return err
	}
	fmt.Fprintf(out, "created %s; define its bindings before starting a run\n", relTo(bridge, p))
	return nil
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
pipeline: coding
workspace: %s
%s---

# %s

## Goal

## Scope

In:

Out:

## Status

New.

## Progress log

- %s: created.

## Outstanding

## Decisions

## Notes
`

func cmdTask(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign task new <project>/<task> --workspace <path> [--branch <branch>] | ensign task open <project>/<task>")
	if len(args) < 2 {
		return usage
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
	case "new":
		var ws, branch string
		for i := 2; i+1 < len(args); i += 2 {
			switch args[i] {
			case "--workspace":
				ws = args[i+1]
			case "--branch":
				branch = args[i+1]
			default:
				return usage
			}
		}
		if ws == "" {
			return fmt.Errorf("a task needs --workspace: ask the user for the path, never guess it")
		}
		if _, err := os.Stat(filepath.Join(bridge, "projects", parts[0], "project.yaml")); err != nil {
			return fmt.Errorf("no project %s (ensign project new %s)", parts[0], parts[0])
		}
		if _, err := os.Stat(filepath.Join(taskDir, "task.md")); err == nil {
			return fmt.Errorf("task %s already exists", args[1])
		}
		vcs := ""
		if branch != "" {
			vcs = fmt.Sprintf("vcs: { kind: git, branch: %s }\n", branch)
		}
		content := fmt.Sprintf(taskTemplate, filepath.ToSlash(ws), vcs, parts[1], time.Now().Format("2006-01-02"))
		if err := writeFile(filepath.Join(taskDir, "task.md"), content); err != nil {
			return err
		}
		fmt.Fprintf(out, "created %s; fill in its goal and scope\n", relTo(bridge, filepath.Join(taskDir, "task.md")))
		return nil
	case "open":
		return openTask(bridge, args[1], taskDir, out)
	}
	return usage
}

// openTask checks a task's workspace and VCS state, records it as the session's task,
// and prints where it stands.
func openTask(bridge, task, taskDir string, out io.Writer) error {
	front, body, err := splitFrontmatter(filepath.Join(taskDir, "task.md"))
	if err != nil {
		return err
	}
	var tf TaskFile
	if err := yamlUnmarshal(front, &tf); err != nil {
		return err
	}
	fmt.Fprintf(out, "task       %s\n", task)
	ws, err := expandWorkspace(bridge, tf.Workspace)
	switch {
	case err != nil:
		fmt.Fprintf(out, "workspace  STOP: %v\n", err)
	case tf.Workspace == "":
		fmt.Fprintln(out, "workspace  STOP: not set in task.md")
	default:
		if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
			fmt.Fprintf(out, "workspace  STOP: %s does not exist\n", ws)
		} else {
			fmt.Fprintf(out, "workspace  %s\n", ws)
			fmt.Fprintln(out, "vcs        "+vcsState(ws, tf.Vcs))
		}
	}
	for _, sec := range []string{"Goal", "Status", "Outstanding"} {
		if text := section(string(body), sec); text != "" {
			fmt.Fprintf(out, "\n%s\n%s\n", strings.ToLower(sec), indent(text))
		}
	}
	if sid := os.Getenv("STARSHIP_SESSION"); sid != "" {
		writeFile(sessionFile(bridge, sid)+".task", task+"\n")
	}
	return nil
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
		status, _ := gitOut(ws, "status", "--porcelain")
		dirty := 0
		if status != "" {
			dirty = len(strings.Split(status, "\n"))
		}
		s := fmt.Sprintf("git, on %s, %d changed files", branch, dirty)
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
