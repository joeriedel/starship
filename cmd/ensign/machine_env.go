// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// A project's toolchain is set up differently on every machine, so what a command needs on its PATH and in its variables is kept in machine.yaml, per project, and ensign applies it wherever it runs a binding. Agents' shells get it from a setup file the session's env file sources.

// ProjectEnv is one project's environment on this machine: variables to set, and folders to put first on PATH, in order.
type ProjectEnv struct {
	Set  map[string]string `yaml:"set,omitempty"`
	Path []string          `yaml:"path,omitempty"`
}

func (e ProjectEnv) empty() bool { return len(e.Set) == 0 && len(e.Path) == 0 }

// hostOS is the platform ensign runs on; tests change it to exercise the Windows paths.
var hostOS = runtime.GOOS

// gitBashFound says whether Claude Code has a Git Bash to run commands in on this Windows machine.
var gitBashFound = detectGitBash

func detectGitBash() bool {
	if p := os.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return false
	}
	for _, rel := range []string{filepath.Join("..", "bin", "bash.exe"), filepath.Join("..", "..", "bin", "bash.exe")} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(git), rel)); err == nil {
			return true
		}
	}
	return false
}

// plainEnvFile is whether the session's env file may only hold NAME=value lines: Windows without a Git Bash, where Claude Code applies it in PowerShell.
func plainEnvFile() bool { return hostOS == "windows" && !gitBashFound() }

func envYAML(env map[string]ProjectEnv) string {
	var projects []string
	for p, e := range env {
		if !e.empty() {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return ""
	}
	sort.Strings(projects)
	var b strings.Builder
	b.WriteString("\nenv:\n")
	for _, p := range projects {
		e := env[p]
		fmt.Fprintf(&b, "  %s:\n", p)
		if len(e.Set) > 0 {
			b.WriteString("    set:\n")
			for _, k := range sortedKeys(e.Set) {
				fmt.Fprintf(&b, "      %s: %s\n", k, yamlScalar(e.Set[k]))
			}
		}
		if len(e.Path) > 0 {
			b.WriteString("    path:\n")
			for _, f := range e.Path {
				fmt.Fprintf(&b, "      - %s\n", yamlScalar(f))
			}
		}
	}
	return b.String()
}

// projectEnv is base with the project's recorded environment applied. machine.yaml is read each time, so a change takes effect on the next command.
func projectEnv(bridge, project string, base []string) []string {
	return applyEnv(base, loadMachine(bridge).Env[project], hostOS == "windows", string(os.PathListSeparator))
}

// applyEnv sets the variables, replacing any entry of the same name, and puts the folders first on PATH. Windows names are case-insensitive, and its PATH is often spelled Path.
func applyEnv(base []string, e ProjectEnv, windows bool, sep string) []string {
	same := func(a, b string) bool {
		if windows {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	name := func(entry string) string { n, _, _ := strings.Cut(entry, "="); return n }
	out := make([]string, 0, len(base)+len(e.Set)+1)
	for _, entry := range base {
		replaced := false
		for k := range e.Set {
			replaced = replaced || same(name(entry), k)
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	for _, k := range sortedKeys(e.Set) {
		out = append(out, k+"="+e.Set[k])
	}
	if len(e.Path) == 0 {
		return out
	}
	folders := make([]string, len(e.Path))
	for i, f := range e.Path {
		folders[i] = filepath.FromSlash(f)
	}
	prefix := strings.Join(folders, sep)
	for i, entry := range out {
		if same(name(entry), "PATH") {
			n, v, _ := strings.Cut(entry, "=")
			if v != "" {
				prefix += sep + v
			}
			out[i] = n + "=" + prefix
			return out
		}
	}
	return append(out, "PATH="+prefix)
}

// posixPath writes a path for bash. Git Bash on Windows splits PATH at colons, so C:/x is written /c/x.
func posixPath(p string, windows bool) string {
	p = filepath.ToSlash(p)
	if windows && len(p) >= 2 && p[1] == ':' {
		return "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

func singleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func setupFile(bridge, session string) string { return sessionFile(bridge, session) + ".env" }

// sourceSetupLine is the one line SessionStart adds to the env file for the setup file, which a run writes later.
func sourceSetupLine(bridge, session string) string {
	p := singleQuote(posixPath(setupFile(bridge, session), hostOS == "windows"))
	return fmt.Sprintf("if [ -f %s ]; then . %s; fi\n", p, p)
}

// setupScript is the setup file's content for a project: exports for its variables and one line putting its folders on PATH.
func setupScript(e ProjectEnv, windows bool) string {
	var b strings.Builder
	for _, k := range sortedKeys(e.Set) {
		fmt.Fprintf(&b, "export %s=%s\n", k, singleQuote(e.Set[k]))
	}
	if len(e.Path) > 0 {
		folders := make([]string, len(e.Path))
		for i, f := range e.Path {
			folders[i] = posixPath(f, windows)
		}
		fmt.Fprintf(&b, "export PATH=%s:\"$PATH\"\n", singleQuote(strings.Join(folders, ":")))
	}
	return b.String()
}

// writeSetupFile records the project's environment for the session's agents, or removes the file when the project has none, so an earlier run's project doesn't linger. Windows without Git Bash gets its environment in the env file instead.
func writeSetupFile(bridge, session, project string) {
	if plainEnvFile() {
		return
	}
	content := setupScript(loadMachine(bridge).Env[project], hostOS == "windows")
	if content == "" {
		os.Remove(setupFile(bridge, session))
		return
	}
	writeFile(setupFile(bridge, session), content)
}

// plainSessionEnv is the env file's content where it may hold only NAME=value lines: every project's folders and variables, since the file is written once. A variable two projects set differently is left out, and the note says so.
func plainSessionEnv(bridge, session string) (content, note string) {
	m := loadMachine(bridge)
	projects := make([]string, 0, len(m.Env))
	for p := range m.Env {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	bin := filepath.FromSlash(filepath.ToSlash(filepath.Join(bridge, ".starship", "bin")))
	path := []string{bin}
	values := map[string]string{}
	owners := map[string][]string{}
	conflict := map[string]bool{}
	for _, p := range projects {
		for _, f := range m.Env[p].Path {
			path = append(path, filepath.FromSlash(f))
		}
		for k, v := range m.Env[p].Set {
			if old, ok := values[k]; ok && old != v {
				conflict[k] = true
			}
			values[k] = v
			owners[k] = append(owners[k], p)
		}
	}
	path = append(path, os.Getenv("PATH"))
	var b strings.Builder
	fmt.Fprintf(&b, "STARSHIP_SESSION=%s\nPATH=%s\n", session, strings.Join(path, ";"))
	var notes []string
	for _, k := range sortedKeys(values) {
		if conflict[k] {
			notes = append(notes, fmt.Sprintf("%s (set differently by %s)", k, strings.Join(owners[k], " and ")))
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, values[k])
	}
	if len(notes) > 0 {
		note = "starship: not set for agents' shells, projects disagree: " + strings.Join(notes, ", ")
	}
	return b.String(), note
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// cmdEnv implements `ensign env <project> [NAME=<value> | --path <folder> | --drop-path <folder>]`: record this machine's toolchain setup for a project, or show it.
func cmdEnv(args []string, out io.Writer) error {
	const usage = "usage: ensign env <project> [NAME=<value> | --path <folder> | --drop-path <folder>]"
	if len(args) < 1 || len(args) > 3 {
		return fmt.Errorf(usage)
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	project := args[0]
	if !validName(project) {
		return fmt.Errorf("name the project: %s", usage)
	}
	if _, err := os.Stat(filepath.Join(bridge, "projects", project, "project.yaml")); err != nil {
		return fmt.Errorf("no project %s", project)
	}
	m := loadMachine(bridge)
	if m.Env == nil {
		m.Env = map[string]ProjectEnv{}
	}
	e := m.Env[project]
	switch {
	case len(args) == 1:
		if e.empty() {
			fmt.Fprintf(out, "%s has no environment recorded on this machine\n", project)
		}
		for _, k := range sortedKeys(e.Set) {
			fmt.Fprintf(out, "%s=%s\n", k, e.Set[k])
		}
		for _, f := range e.Path {
			fmt.Fprintf(out, "path %s\n", f)
		}
		return nil
	case len(args) == 2:
		name, value, ok := strings.Cut(args[1], "=")
		if !ok || !envNameRe.MatchString(name) {
			return fmt.Errorf("%s\nname the variable as NAME=value (letters, digits and _)", usage)
		}
		if strings.EqualFold(name, "PATH") {
			return fmt.Errorf("PATH is not set here; put a folder first on it with `ensign env %s --path <folder>`", project)
		}
		if value == "" {
			delete(e.Set, name)
			fmt.Fprintf(out, "%s no longer sets %s\n", project, name)
		} else {
			if e.Set == nil {
				e.Set = map[string]string{}
			}
			e.Set[name] = value
			fmt.Fprintf(out, "%s sets %s=%s\n", project, name, value)
		}
	case args[1] == "--path":
		abs, err := filepath.Abs(expandHome(args[2]))
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a folder on this machine; ask the user where the toolchain is, never guess", abs)
		}
		slash := filepath.ToSlash(abs)
		for _, f := range e.Path {
			if f == slash {
				return fmt.Errorf("%s is already on %s's path", slash, project)
			}
		}
		e.Path = append(e.Path, slash)
		fmt.Fprintf(out, "%s puts %s on PATH\n", project, slash)
	case args[1] == "--drop-path":
		abs, err := filepath.Abs(expandHome(args[2]))
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(abs)
		kept := e.Path[:0:0]
		for _, f := range e.Path {
			if f != slash {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(e.Path) {
			return fmt.Errorf("%s is not on %s's path", slash, project)
		}
		e.Path = kept
		fmt.Fprintf(out, "%s no longer puts %s on PATH\n", project, slash)
	default:
		return fmt.Errorf(usage)
	}
	if e.empty() {
		delete(m.Env, project)
	} else {
		m.Env[project] = e
	}
	return saveMachine(bridge, m)
}
