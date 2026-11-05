// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// HelpEntry is one plain-language request a pipeline or extension answers to.
// Run is the command that carries it out. The session is told it; help never shows it.
type HelpEntry struct {
	Say  string `yaml:"say" json:"say"`
	Does string `yaml:"does" json:"does"`
	Run  string `yaml:"run" json:"run,omitempty"`
}

// requestCommands is the body of `ensign help requests`, about carrying out the requests that pipelines and extensions answer to; SessionStart prints only a pointer to that topic.
func requestCommands(bridge string) string {
	var lines []string
	extra, problems := extraHelp(bridge)
	for _, group := range sortedKeys(extra) {
		for _, e := range extra[group] {
			if e.Run != "" {
				lines = append(lines, fmt.Sprintf("- %s: `%s`", e.Say, e.Run))
			}
		}
	}
	out := ""
	if len(lines) > 0 {
		out = "## Requests this bridge answers, and the command for each\n\n" +
			"A command that starts a run needs the user's go, like any run.\n\n" + strings.Join(lines, "\n") + "\n"
	}
	if broken := helpProblemLines(bridge, problems); len(broken) > 0 {
		if out != "" {
			out += "\n"
		}
		out += "## Extensions and pipelines whose requests may be missing from the help, because their file is broken\n\n- " + strings.Join(broken, "\n- ") + "\n"
	}
	return out
}

// helpProblemLines merges the problems extraHelp met with the broken extensions into the lines of the "may be missing" section: sorted, with no line twice.
func helpProblemLines(bridge string, problems []string) []string {
	out := slices.Concat(problems, brokenExtensionLines(bridge))
	sort.Strings(out)
	return slices.Compact(out)
}

// brokenExtensionLines describes, one line each and sorted, the extensions whose file is broken: one that does not decode, or one that decodes without a name.
func brokenExtensionLines(bridge string) []string {
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return nil
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return nil
	}
	var out []string
	dirs, derr := extensionDirs(bridge, cp)
	if derr != nil {
		out = append(out, flatLine("all extensions: "+derr.Error()))
	}
	_, broken := scanExtensions(bridge, cp)
	for name, err := range broken {
		out = append(out, flatLine(fmt.Sprintf("%s: %s", name, err)))
	}
	for _, dir := range dirs {
		file := filepath.Join(dir, "extension.yaml")
		var x Extension
		if readYAML(file, &x) == nil && x.Name == "" {
			out = append(out, flatLine(fmt.Sprintf("%s: %s declares no name", filepath.Base(dir), file)))
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

var fixedHelp = []struct {
	group   string
	entries []HelpEntry
}{
	{"Getting around", []HelpEntry{
		{`"open <project> / <task>"`, "pick up a task where it left off", ""},
		{`"new task <name> in <project>"`, "start a task in the project's code (or name a different location)", ""},
		{`"new project <name>, its code is in <folder>"`, "add a project to this bridge", ""},
		{"status", "where the current task and run stand", ""},
		{"help <project or task>", "more on one project or task", ""},
	}},
	{"Doing the work", []HelpEntry{
		{"describe what you want done", "I plan it as a run; type go to start it", ""},
		{"go", "approve the plan I just showed you", ""},
		{"describe several steps", "I line them up as a sequence; one go runs them all, one after another", "ensign sequence"},
	}},
	{"Housekeeping", []HelpEntry{
		{`"update the bridge"`, "move to the latest starship release", "ensign update"},
		{"clean up old runs", "delete finished runs beyond the bridge's limit and empty their scratch folders", "ensign gc"},
		{`"improve starship"`, "work on starship as a project; the change ends as a pull request", ""},
	}},
}

// sessionWord answers help and status without the model. It returns "" for anything else.
func sessionWord(bridge, session, prompt string) string {
	p := strings.TrimSpace(prompt)
	switch {
	case p == "help":
		return helpText(bridge, session)
	case strings.HasPrefix(p, "help "):
		return helpTopic(bridge, strings.TrimSpace(p[5:]))
	case p == "status":
		return statusText(bridge, session)
	}
	return ""
}

func sessionTask(bridge, session string) string {
	data, _ := os.ReadFile(sessionFile(bridge, session) + ".task")
	return strings.TrimSpace(string(data))
}

func header(bridge, session string) string {
	h := "Bridge: " + filepath.Base(bridge)
	if t := sessionTask(bridge, session); t != "" {
		p, task, _ := strings.Cut(t, "/")
		h += " · Project: " + p + " · Task: " + task
	}
	return h
}

// runLine describes the session's runs in plain words: one line for its one run or latest, one line each, led by the run id, when several are live.
func runLine(bridge, session string) string {
	live, err := liveSessionRuns(bridge, session)
	if err != nil {
		return err.Error()
	}
	if len(live) > 1 {
		var lines []string
		for _, id := range live {
			if l := describeRun(bridge, id); l != "" {
				lines = append(lines, id+": "+l)
			}
		}
		return strings.Join(lines, "\n")
	}
	id, _, _ := sessionRunID(bridge, session)
	return describeRun(bridge, id)
}

func describeRun(bridge, id string) string {
	r, err := openRunIfExists(bridge, id)
	if err != nil || r == nil {
		return ""
	}
	defer r.close()
	st := r.State
	m := &machine{snap: r.Snap, st: st}
	switch {
	case st.Status == "pending":
		return "A run is planned and waiting for you to type go."
	case st.terminal():
		return fmt.Sprintf("Last run: %s (%s).", st.Status, st.Reason)
	}
	lead := r.Snap.Pipeline.Stages[0]
	sc := st.Scopes[lead.ID]
	if sc == nil || sc.Stage == "" {
		return "Working: starting the run."
	}
	s := m.stage(sc.Stage)
	line := "Working on: " + s.ID
	if s.Parallel != nil {
		done := 0
		for _, id := range sc.Branches {
			if d := st.dispatch(id); d != nil && d.Done {
				done++
			}
		}
		line += fmt.Sprintf(", %d of %d reviewers done", done, len(s.Dimensions))
	}
	var parts []string
	for _, k := range sortedKeys(r.Snap.Pipeline.Budgets) {
		parts = append(parts, fmt.Sprintf("%d of %d %s", st.Spent[k], r.Snap.Pipeline.Budgets[k], budgetWord(k)))
	}
	return line + " · " + strings.Join(parts, " · ")
}

// seenLine says how long ago the live run's lead and its agents last made a hook call, for each live run.
func seenLine(bridge, session string) string {
	live, err := liveSessionRuns(bridge, session)
	if err != nil {
		return ""
	}
	if len(live) > 1 {
		var lines []string
		for _, id := range live {
			if l := seenOf(bridge, id); l != "" {
				lines = append(lines, id+": "+l)
			}
		}
		return strings.Join(lines, "\n")
	}
	id, _, _ := sessionRunID(bridge, session)
	return seenOf(bridge, id)
}

func seenOf(bridge, id string) string {
	r, err := openRunIfExists(bridge, id)
	if err != nil || r == nil {
		return ""
	}
	defer r.close()
	if r.State.terminal() || r.State.Status == "pending" {
		return ""
	}
	var lead, latest time.Time
	for _, d := range r.State.Dispatches {
		t, err := time.Parse(time.RFC3339, d.LastSeen)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
		if d.Stage == r.Snap.Pipeline.Stages[0].ID && t.After(lead) {
			lead = t
		}
	}
	ago := func(t time.Time) string {
		if t.IsZero() {
			return "not yet seen"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	}
	if !r.Snap.Pipeline.Stages[0].isLead() {
		return fmt.Sprintf("Last activity: %s.", ago(latest))
	}
	return fmt.Sprintf("Lead last seen: %s. Last activity: %s.", ago(lead), ago(latest))
}

func budgetWord(b string) string {
	switch b {
	case "coder-loop":
		return "coder loops used"
	case "test":
		return "test cycles used"
	}
	return b + " used"
}

func helpText(bridge, session string) string {
	var b strings.Builder
	b.WriteString(header(bridge, session) + "\n")
	if l := runLine(bridge, session); l != "" {
		b.WriteString(l + "\n")
	}
	groups := map[string][]HelpEntry{}
	var order []string
	for _, g := range fixedHelp {
		groups[g.group] = append(groups[g.group], g.entries...)
		order = append(order, g.group)
	}
	extra, problems := extraHelp(bridge)
	for group, entries := range extra {
		if _, ok := groups[group]; !ok {
			order = append(order[:len(order)-1], group, order[len(order)-1])
		}
		groups[group] = append(groups[group], entries...)
	}
	for _, g := range order {
		b.WriteString("\n" + g + "\n")
		for _, e := range groups[g] {
			fmt.Fprintf(&b, "  %-44s %s\n", e.Say, e.Does)
		}
	}
	if broken := helpProblemLines(bridge, problems); len(broken) > 0 {
		b.WriteString("\nExtensions and pipelines whose requests may be missing from this help, because their file is broken\n")
		for _, l := range broken {
			b.WriteString("  " + l + "\n")
		}
	}
	if projects := projectList(bridge); projects != "" {
		b.WriteString("\nProjects: " + projects + "\n")
	}
	return b.String()
}

// extraHelp gathers help entries from pipelines and extensions, by group. The lines returned are what it could not read, one each, for the "may be missing" section; a bridge with no bridge.yaml has none.
func extraHelp(bridge string) (map[string][]HelpEntry, []string) {
	out := map[string][]HelpEntry{}
	var problems []string
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, flatLine("all extensions and pipelines: "+err.Error()))
		}
		return out, problems
	}
	cp, err := controlPlaneDir(bridge, bf)
	if err != nil {
		return out, append(problems, flatLine("all extensions and pipelines: "+err.Error()))
	}
	glob := func(dir string) []string {
		found, err := filesIn(dir, ".yaml")
		if err != nil {
			problems = append(problems, flatLine(fmt.Sprintf("%s: %v", dir, err)))
		}
		return found
	}
	files := glob(filepath.Join(cp, "pipelines"))
	var dirs []string
	allDirs, derr := extensionDirs(bridge, cp)
	if derr != nil {
		problems = append(problems, flatLine("all extensions: "+derr.Error()))
	}
	for _, dir := range allDirs {
		var x Extension
		if readYAML(filepath.Join(dir, "extension.yaml"), &x) == nil && x.Quiet && !bridgeUses(bridge, x.Name) {
			continue
		}
		dirs = append(dirs, dir)
	}
	// findPipeline does not decode extension.yaml, so a broken extension's pipelines still run and still give help.
	for _, dir := range dirs {
		files = append(files, glob(filepath.Join(dir, "pipelines"))...)
	}
	for _, f := range files {
		var p struct {
			Help []HelpEntry `yaml:"help"`
		}
		if err := readYAML(f, &p); err != nil {
			problems = append(problems, flatLine(fmt.Sprintf("%s: %v", strings.TrimSuffix(filepath.Base(f), ".yaml"), err)))
			continue
		}
		out["Doing the work"] = append(out["Doing the work"], p.Help...)
	}
	for _, dir := range dirs {
		var x Extension
		if readYAML(filepath.Join(dir, "extension.yaml"), &x) == nil && x.Name != "" && len(x.Help) > 0 {
			group := strings.ToUpper(x.Name[:1]) + strings.ReplaceAll(x.Name[1:], "-", " ")
			out[group] = append(out[group], x.Help...)
		}
	}
	return out, problems
}

func projectList(bridge string) string {
	dirs, _ := os.ReadDir(filepath.Join(bridge, "projects"))
	var parts []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		tasks, _ := os.ReadDir(filepath.Join(bridge, "projects", d.Name(), "tasks"))
		n := 0
		for _, t := range tasks {
			if t.IsDir() {
				n++
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%d task%s)", d.Name(), n, plural(n)))
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func helpTopic(bridge, topic string) string {
	topic = strings.ReplaceAll(strings.ReplaceAll(topic, " / ", "/"), " ", "")
	if p, t, ok := strings.Cut(topic, "/"); ok {
		return taskSummary(bridge, p, t)
	}
	if _, err := os.Stat(filepath.Join(bridge, "projects", topic, "project.yaml")); err == nil {
		return projectSummary(bridge, topic)
	}
	dirs, _ := os.ReadDir(filepath.Join(bridge, "projects"))
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(bridge, "projects", d.Name(), "tasks", topic, "task.md")); err == nil {
			return taskSummary(bridge, d.Name(), topic)
		}
	}
	return fmt.Sprintf("No project or task called %q. Type help for what's available.", topic)
}

func projectSummary(bridge, project string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project %s\n", project)
	tasks, _ := os.ReadDir(filepath.Join(bridge, "projects", project, "tasks"))
	if len(tasks) == 0 {
		b.WriteString("\nNo tasks yet.\n")
	}
	for _, t := range tasks {
		if !t.IsDir() {
			continue
		}
		_, body, err := splitFrontmatter(filepath.Join(bridge, "projects", project, "tasks", t.Name(), "task.md"))
		status := "(no task.md)"
		if err == nil {
			status = firstLine(section(string(body), "Status"))
			if p := subtaskProgress(bridge, project+"/"+t.Name()); p != "" {
				status += " (" + p + ")"
			}
		}
		fmt.Fprintf(&b, "  %-24s %s\n", t.Name(), status)
	}
	return b.String()
}

func taskSummary(bridge, project, task string) string {
	_, body, err := splitFrontmatter(filepath.Join(bridge, "projects", project, "tasks", task, "task.md"))
	if err != nil {
		return fmt.Sprintf("No task %s/%s.", project, task)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s / %s\n", project, task)
	if p := subtaskProgress(bridge, project+"/"+task); p != "" {
		fmt.Fprintf(&b, "%s\n", p)
	}
	for _, sec := range []string{"Goal", "Status", "Outstanding"} {
		if t := section(string(body), sec); t != "" {
			fmt.Fprintf(&b, "\n%s\n%s\n", sec, indent(t))
		}
	}
	return b.String()
}

func statusText(bridge, session string) string {
	var b strings.Builder
	b.WriteString(header(bridge, session) + "\n")
	if l := runLine(bridge, session); l != "" {
		b.WriteString(l + "\n")
		if l := seenLine(bridge, session); l != "" {
			b.WriteString(l + "\n")
		}
	} else {
		b.WriteString("No run in this session.\n")
	}
	if l := sequenceListing(bridge, session); l != "" {
		b.WriteString("\n" + l)
	}
	if t := sessionTask(bridge, session); t != "" {
		p, task, _ := strings.Cut(t, "/")
		b.WriteString("\n" + taskSummary(bridge, p, task))
	} else {
		b.WriteString("\nNo task open. Say \"open <project> / <task>\".\n")
	}
	held, herr := proposedSkills(bridge)
	if herr != nil {
		fmt.Fprintf(&b, "\nstarship: %v\n", herr)
	}
	if len(held) > 0 {
		b.WriteString("\nAgents reported facts about technologies with no skill yet:\n")
		for _, name := range sortedKeys(held) {
			fmt.Fprintf(&b, "  %s: %d fact%s. Say \"make a %s skill\" or \"discard the %s facts\".\n",
				name, held[name], plural(held[name]), name, name)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// controlPlaneEdit refuses edits to the control plane from a bridge session.
func controlPlaneEdit(bridge string, in *HookInput, goos string) string {
	path, _ := in.ToolInput["file_path"].(string)
	if path == "" {
		path, _ = in.ToolInput["notebook_path"].(string)
	}
	if path == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	expanded := claudeToolPath(path, home, goos)
	if windowsUnresolvable(path, goos) || windowsUnresolvable(expanded, goos) {
		return "on Windows, give the full `C:\\...` path of the file; this spelling (a stream, a device or volume path, a drive-relative or a rooted path with no drive) can't be checked against the bridge's pinned copy of starship."
	}
	bf, err := loadBridgeFile(bridge)
	if err != nil {
		return ""
	}
	roots := []string{filepath.Join(bridge, ".starship", "control-plane")}
	if cp, err := controlPlaneDir(bridge, bf); err == nil {
		roots = append(roots, cp)
	}
	target := resolveEditTarget(path, in.Cwd, goos)
	for _, root := range roots {
		folder := resolveEditFolder(root, goos)
		if pathWithin(target, folder, goos) || uncNamesFolder(expanded, root, goos) || uncNamesFolder(expanded, folder, goos) {
			return "this is the bridge's pinned copy of starship, which isn't edited. To change starship, work on a starship checkout as a project task; the change lands as a pull request."
		}
	}
	return ""
}

// writesStateEdit refuses file-tool edits to a session's connector writes file and its lock and temp companions, so the session can't approve a write itself.
func writesStateEdit(bridge string, in *HookInput, goos string) string {
	p, _ := in.ToolInput["file_path"].(string)
	if p == "" {
		p, _ = in.ToolInput["notebook_path"].(string)
	}
	if p == "" {
		return ""
	}
	unc := false
	if goos == "windows" {
		stripped := stripExtendedPrefix(p, goos)
		if windowsDevicePath(p) || windowsAmbiguousPath(stripped) {
			return "on Windows, give the full `C:\\...` path of the file; this spelling (a stream, a device or volume path, a drive-relative or a rooted path with no drive) can't be checked against the session's connector writes state."
		}
		unc = len(stripped) > 1 && (stripped[0] == '\\' || stripped[0] == '/') && (stripped[1] == '\\' || stripped[1] == '/')
	}
	target := foldPath(resolveEditTarget(p, in.Cwd, goos), goos)
	dir := foldPath(resolveEditFolder(filepath.Join(bridge, ".starship", "sessions"), goos), goos)
	// A share can name the sessions folder in ways that can't be listed (host, address, share), so a UNC path is decided by its name alone.
	if !unc && path.Dir(target) != dir {
		return ""
	}
	name := path.Base(target)
	for _, suffix := range []string{".writes", ".writes.lock", ".writes.tmp"} {
		if strings.HasSuffix(name, suffix) {
			return "this is the session's connector writes state, which is changed only through `ensign writes` and the user's go."
		}
	}
	return ""
}

// windowsUnresolvable reports, on Windows only, a path spelling that can't be placed by name: a device or volume path, a stream, a drive-relative or a rooted path with no drive. It takes the raw path.
func windowsUnresolvable(p, goos string) bool {
	return goos == "windows" && (windowsDevicePath(p) || windowsAmbiguousPath(stripExtendedPrefix(p, goos)))
}

// isUNCPath reports two leading slashes of either kind, after any \\?\ prefix.
func isUNCPath(p string) bool {
	stripped := stripExtendedPrefix(p, "windows")
	return len(stripped) >= 2 && (stripped[0] == '\\' || stripped[0] == '/') && (stripped[1] == '\\' || stripped[1] == '/')
}

// uncNamesFolder reports, on Windows only, a UNC path (two leading slashes of either kind, after any \\?\ prefix) whose segments hold the last two segments of root, a drive excluded, one after the other. A share can name any folder, so the string can't prove where a UNC path lands; like writesStateEdit, this decides by name alone. Trailing dots and spaces are dropped from each segment because Win32 drops them when opening a file and no filepath.Abs runs on this string.
func uncNamesFolder(p, root, goos string) bool {
	if goos != "windows" {
		return false
	}
	if !isUNCPath(p) {
		return false
	}
	segs := func(s string) []string {
		parts := strings.Split(strings.ReplaceAll(s, `\`, "/"), "/")
		for i, part := range parts {
			if part != "." && part != ".." {
				parts[i] = strings.TrimRight(part, ". ")
			}
		}
		var out []string
		for _, seg := range strings.Split(foldPath(strings.Join(parts, "/"), goos), "/") {
			if seg != "" {
				out = append(out, seg)
			}
		}
		return out
	}
	want := segs(stripExtendedPrefix(root, goos))
	if len(want) > 0 && strings.HasSuffix(want[0], ":") {
		want = want[1:]
	}
	if len(want) > 2 {
		want = want[len(want)-2:]
	}
	if len(want) == 0 {
		return false
	}
	have := segs(stripExtendedPrefix(p, goos))
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for k, w := range want {
			if have[i+k] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// windowsDevicePath reports a raw Windows device or volume spelling: two slashes of either kind, then `?` or `.`, then a slash, other than `\\?\X:\` and `\\?\UNC\`. It takes the path before any prefix is stripped. It is pure, so it runs on any host.
func windowsDevicePath(raw string) bool {
	isSlash := func(c byte) bool { return c == '\\' || c == '/' }
	if len(raw) < 4 || !isSlash(raw[0]) || !isSlash(raw[1]) || (raw[2] != '?' && raw[2] != '.') || !isSlash(raw[3]) {
		return false
	}
	if raw[0] == '\\' && raw[1] == '\\' && raw[2] == '?' && raw[3] == '\\' {
		rest := raw[4:]
		if len(rest) > 2 && rest[1] == ':' && isSlash(rest[2]) && (rest[0]|0x20 >= 'a' && rest[0]|0x20 <= 'z') {
			return false
		}
		if len(rest) >= 4 && rest[:3] == "UNC" && rest[3] == '\\' {
			return false
		}
	}
	return true
}

// windowsAmbiguousPath reports a Windows spelling the Go resolver can't be trusted on: a colon other than the drive's in `C:\` or `C:/` (a stream, `C:rel`, `\\.\C:\`) or a single leading slash (rooted, no drive). UNC paths and plain relative paths are not ambiguous. It is pure, so it runs on any host.
func windowsAmbiguousPath(raw string) bool {
	isSlash := func(c byte) bool { return c == '\\' || c == '/' }
	if len(raw) > 0 && isSlash(raw[0]) && !(len(raw) > 1 && isSlash(raw[1])) {
		return true
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != ':' {
			continue
		}
		drive := i == 1 && len(raw) > 2 && isSlash(raw[2]) && (raw[0]|0x20 >= 'a' && raw[0]|0x20 <= 'z')
		if !drive {
			return true
		}
	}
	return false
}

// resolveExisting follows symlinks in the longest existing prefix of a path.
func resolveExisting(p string) string {
	dir, rest := p, ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func sortedStrings(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
