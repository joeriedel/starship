package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// A subtask is one file in a task's subtasks folder, run on its own on the task's branch.
type subtask struct {
	Name     string // the file name without .md, e.g. 01-models-schema
	Path     string
	Location string
	Goal     string
	Scope    string
}

// subtaskMark records that a subtask is done or dropped; a subtask with no mark is pending.
type subtaskMark struct {
	Status string // done, dropped
	Run    string // the run that finished it, empty for a mark by hand
}

func subtasksDir(taskDir string) string { return filepath.Join(taskDir, "subtasks") }

func taskDirOf(bridge, task string) string {
	project, name, _ := strings.Cut(task, "/")
	return filepath.Join(bridge, "projects", project, "tasks", name)
}

// loadSubtasks reads a task's subtasks in order: by the number its name starts with, then by name. A task with no subtasks folder, or an empty one, has none.
func loadSubtasks(taskDir string) ([]subtask, error) {
	entries, err := os.ReadDir(subtasksDir(taskDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var subs []subtask
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(subtasksDir(taskDir), e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		body := string(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
		var meta struct {
			Location string `yaml:"location"`
		}
		if strings.HasPrefix(body, "---\n") {
			front, rest, err := splitFrontmatter(path)
			if err != nil {
				return nil, err
			}
			if err := decodeFrontmatter(front, &meta); err != nil {
				return nil, fmt.Errorf("%s: %v", path, err)
			}
			body = string(rest)
		}
		subs = append(subs, subtask{
			Name:     strings.TrimSuffix(e.Name(), ".md"),
			Path:     path,
			Location: meta.Location,
			Goal:     section(body, "Goal"),
			Scope:    section(body, "Scope"),
		})
	}
	sort.SliceStable(subs, func(i, j int) bool {
		ni, nj := leadingNumber(subs[i].Name), leadingNumber(subs[j].Name)
		if ni != nj {
			return ni < nj
		}
		return subs[i].Name < subs[j].Name
	})
	return subs, nil
}

// leadingNumber is the number a name starts with; a name with none sorts after the numbered ones.
func leadingNumber(name string) int {
	n, digits := 0, 0
	for _, r := range name {
		if r < '0' || r > '9' || digits >= 9 {
			break
		}
		n, digits = n*10+int(r-'0'), digits+1
	}
	if digits == 0 {
		return 1 << 30
	}
	return n
}

// checkSubtasks refuses subtasks a run could not work from.
func checkSubtasks(subs []subtask, taskLocs []string) error {
	for _, s := range subs {
		switch {
		case s.Goal == "" && s.Scope == "":
			return fmt.Errorf("subtask %s has no work item: %s has neither a \"## Goal\" nor a \"## Scope\" section with text in it; add one and run again", s.Name, s.Path)
		case s.Location == "" && len(taskLocs) > 1:
			return fmt.Errorf("subtask %s must name its location (location: <name> in its frontmatter): the task uses %s", s.Name, strings.Join(taskLocs, ", "))
		case s.Location != "" && !contains(taskLocs, s.Location):
			return fmt.Errorf("subtask %s names location %q, which the task does not use (it uses %s)", s.Name, s.Location, strings.Join(taskLocs, ", "))
		}
	}
	return nil
}

// subtaskMarks reads which of subs are done or dropped from the task's journal, which the bridge commits: a run line "subtask <name>: success" and a hand line "subtask <name>: done by hand" or "dropped by hand" mark it, the last such line winning. A task whose history.md is missing and still logs in task.md is read there. No journal means no progress.
func subtaskMarks(bridge, task string, subs []subtask) (map[string]subtaskMark, error) {
	marks := map[string]subtaskMark{}
	dir := taskDirOf(bridge, task)
	// Writers rewrite task.md and create history.md in place, so read under their lock.
	lock := taskStateFile(bridge, task, "", ".journal.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	l := flock.New(lock)
	if err := l.RLock(); err != nil {
		return nil, fmt.Errorf("locking the journal: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "history.md"))
	if errors.Is(err, fs.ErrNotExist) {
		data, err = os.ReadFile(filepath.Join(dir, "task.md"))
	}
	if errors.Is(err, fs.ErrNotExist) {
		l.Unlock()
		return marks, nil
	}
	if err != nil {
		l.Unlock()
		return nil, err
	}
	l.Unlock()
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		head, _, ok := strings.Cut(strings.TrimPrefix(line, "- **"), ".**")
		if !ok || !strings.HasPrefix(line, "- **") {
			continue
		}
		before, after, ok := strings.Cut(head, ", subtask ")
		if !ok {
			continue
		}
		name, outcome, ok := strings.Cut(after, ": ")
		if !ok || !slices.ContainsFunc(subs, func(s subtask) bool { return s.Name == name }) {
			continue
		}
		m := subtaskMark{}
		switch outcome {
		case "success":
			m.Status = "done"
			if _, run, ok := strings.Cut(before, ", run "); ok {
				m.Run, _, _ = strings.Cut(run, " ")
			}
		case "done by hand":
			m.Status = "done"
		case "dropped by hand":
			m.Status = "dropped"
		default:
			continue
		}
		marks[name] = m
	}
	return marks, nil
}

// pendingSubtasks is a task's subtasks with no mark, in order, and the task's subtasks as a whole.
func pendingSubtasks(bridge, task string) (pending, all []subtask, err error) {
	all, err = loadSubtasks(taskDirOf(bridge, task))
	if err != nil || len(all) == 0 {
		return nil, all, err
	}
	marks, err := subtaskMarks(bridge, task, all)
	if err != nil {
		return nil, all, err
	}
	for _, s := range all {
		if _, ok := marks[s.Name]; !ok {
			pending = append(pending, s)
		}
	}
	return pending, all, nil
}

// subtaskProgress is the line task open and the listings show for a task with subtasks, "" for one without.
func subtaskProgress(bridge, task string) string {
	pending, all, err := pendingSubtasks(bridge, task)
	if err != nil || len(all) == 0 {
		return ""
	}
	line := fmt.Sprintf("subtasks %d/%d", len(all)-len(pending), len(all))
	if len(pending) > 0 {
		line += ", next " + pending[0].Name
	}
	return line
}

// chosenSubtask is the subtask a run works on, with the subtasks before it that are on the branch or dropped.
type chosenSubtask struct {
	sub      subtask
	earlier  []string
	doneLocs []string // the locations of the earlier subtasks that are done
}

// pickSubtask finds the subtask a run of o.task works on: the one named, else the first pending. It returns nil for a task without subtasks.
func pickSubtask(bridge string, o runOpts) (*chosenSubtask, error) {
	if o.task == "" {
		if o.subtask != "" {
			return nil, fmt.Errorf("--subtask needs a task")
		}
		return nil, nil
	}
	taskDir := taskDirOf(bridge, o.task)
	subs, err := loadSubtasks(taskDir)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		if o.subtask != "" {
			return nil, fmt.Errorf("task %s has no subtasks, so --subtask %s has no meaning", o.task, o.subtask)
		}
		return nil, nil
	}
	front, _, err := splitFrontmatter(filepath.Join(taskDir, "task.md"))
	if err != nil {
		return nil, fmt.Errorf("task %q: %v", o.task, err)
	}
	var tf TaskFile
	if err := decodeFrontmatter(front, &tf); err != nil {
		return nil, err
	}
	if err := checkSubtasks(subs, tf.Locations); err != nil {
		return nil, err
	}
	marks, err := subtaskMarks(bridge, o.task, subs)
	if err != nil {
		return nil, err
	}
	at := -1
	for i, s := range subs {
		_, marked := marks[s.Name]
		switch {
		case o.subtask == "" && !marked:
			at = i
		case o.subtask == s.Name:
			if marked {
				return nil, fmt.Errorf("subtask %s of %s is already %s", s.Name, o.task, marks[s.Name].Status)
			}
			at = i
		}
		if at >= 0 {
			break
		}
	}
	if at < 0 {
		if o.subtask != "" {
			return nil, fmt.Errorf("task %s has no subtask %q (it has %s)", o.task, o.subtask, subtaskNames(subs))
		}
		return nil, fmt.Errorf("every subtask of %s is done or dropped; there is nothing left to run", o.task)
	}
	sub := subs[at]
	if sub.Location != "" && o.location != "" && sub.Location != o.location {
		return nil, fmt.Errorf("subtask %s works in location %s, so --location %s does not fit it", sub.Name, sub.Location, o.location)
	}
	ch := &chosenSubtask{sub: sub}
	for _, s := range subs[:at] {
		switch marks[s.Name].Status {
		case "done":
			ch.earlier = append(ch.earlier, "- "+s.Name+": "+firstLine(firstNonEmpty(s.Goal, s.Scope)))
			if s.Location != "" {
				ch.doneLocs = append(ch.doneLocs, s.Location)
			}
		case "dropped":
			ch.earlier = append(ch.earlier, "- "+s.Name+": dropped, not on the branch")
		}
	}
	return ch, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func subtaskNames(subs []subtask) string {
	var names []string
	for _, s := range subs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// workItem is the task's work item followed by the subtask's, and what earlier subtasks put on the branch. Later subtasks are not mentioned.
func (ch *chosenSubtask) workItem(taskItem string) string {
	parts := []string{taskItem, "## Subtask " + ch.sub.Name}
	if ch.sub.Goal != "" {
		parts = append(parts, "### Goal\n\n"+ch.sub.Goal)
	}
	if ch.sub.Scope != "" {
		parts = append(parts, "### Scope\n\n"+ch.sub.Scope)
	}
	if len(ch.earlier) > 0 {
		parts = append(parts, "## Already on the branch\n\n"+strings.Join(ch.earlier, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// cmdTaskSubtask implements `ensign task subtask <done|drop> <project>/<task> <name>`.
func cmdTaskSubtask(args []string, out io.Writer) error {
	usage := fmt.Errorf("usage: ensign task subtask <done|drop> <project>/<task> <name>")
	if len(args) != 3 || (args[0] != "done" && args[0] != "drop") {
		return usage
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	task, name := args[1], args[2]
	parts := strings.Split(task, "/")
	if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
		return fmt.Errorf("task must be <project>/<task>, lower-case letters, digits and dashes")
	}
	subs, err := loadSubtasks(taskDirOf(bridge, task))
	if err != nil {
		return err
	}
	found := false
	for _, s := range subs {
		found = found || s.Name == name
	}
	if !found {
		if len(subs) == 0 {
			return fmt.Errorf("task %s has no subtasks", task)
		}
		return fmt.Errorf("task %s has no subtask %q (it has %s)", task, name, subtaskNames(subs))
	}
	status := map[string]string{"done": "done", "drop": "dropped"}[args[0]]
	entry := fmt.Sprintf("- **%s, subtask %s: %s by hand.**", time.Now().Format("2006-01-02"), name, status)
	if err := addToJournal(bridge, task, entry); err != nil {
		return err
	}
	fmt.Fprintf(out, "subtask %s of %s marked %s; %s\n", name, task, status, subtaskProgress(bridge, task))
	return nil
}
