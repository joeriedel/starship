// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// cmdTaskClose implements `ensign task close <project>/<task>`. It needs no run, workspace or pipeline, so it works the same for a task with and without code. The task's frontmatter `status: closed` is the once-only marker.
func cmdTaskClose(bridge, task string, out io.Writer) error {
	taskFile := filepath.Join(taskDirOf(bridge, task), "task.md")
	data, err := os.ReadFile(taskFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no task %s", task)
		}
		return err
	}
	lock := taskStateFile(bridge, task, "", ".close.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return err
	}
	l := flock.New(lock)
	if ok, err := l.TryLock(); err != nil {
		return fmt.Errorf("locking the close of %s: %w", task, err)
	} else if !ok {
		return fmt.Errorf("a close of %s is already under way", task)
	}
	defer l.Unlock()

	// The lock is held now, so read again: a close that just finished must be seen.
	if data, err = os.ReadFile(taskFile); err != nil {
		return err
	}
	front, _, err := splitFrontmatterData(taskFile, data)
	if err != nil {
		return err
	}
	var tf struct {
		Status string `yaml:"status"`
	}
	if err := decodeFrontmatter(front, &tf); err != nil {
		return fmt.Errorf("%s: %v", taskFile, err)
	}
	if err := checkTaskKeys(taskFile, front); err != nil {
		return err
	}
	if tf.Status == "closed" {
		fmt.Fprintf(out, "Task %s is already closed.\n", task)
		return nil
	}

	if _, err := markClosed(data); err != nil {
		return fmt.Errorf("%s: %v", taskFile, err)
	}
	c, err := loadTaskEvents(bridge, task)
	if err != nil {
		return err
	}
	if len(c.problems) > 0 {
		return fmt.Errorf("%s; %s stays open, fix this and close it again", strings.Join(c.problems, "; "), task)
	}
	handlers := c.Events["task.close"]
	type item struct{ source, text string }
	var items, augmentItems []item
	for _, h := range handlers {
		if msg := closeHandlerProblem("task.close", h); msg != "" {
			return fmt.Errorf("%s: %s", h.Source, msg)
		}
		if h.Kind != "augment" {
			continue
		}
		p := filepath.Join(h.Dir, filepath.FromSlash(h.Text))
		if _, err := os.Stat(p); err != nil {
			if p, err = c.findFile(h.Text); err != nil {
				return fmt.Errorf("%s: augment %s not found", h.Source, h.Text)
			}
		}
		text, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("%s: augment %s: %v", h.Source, h.Text, err)
		}
		if t := strings.TrimSpace(string(text)); t != "" {
			augmentItems = append(augmentItems, item{h.Source, t})
		}
	}

	project, _, _ := strings.Cut(task, "/")
	env := projectEnv(bridge, project, append(withoutRunEnv(os.Environ(), hostOS == "windows"),
		"STARSHIP_BRIDGE="+bridge,
		"STARSHIP_PROJECT="+project,
		"STARSHIP_TASK="+task,
	))
	for _, h := range handlers {
		if h.Kind != "require" {
			continue
		}
		res, err := shell(bridge, h.Command, env)
		if err != nil {
			msg := fmt.Sprintf("task.close: %s failed: %v", h.Command, err)
			if res != "" {
				msg += ": " + lastLines(res, 5)
			}
			return fmt.Errorf("%s (from %s); %s stays open, fix this and close it again", msg, h.Source, task)
		}
		if t := strings.TrimSpace(res); t != "" {
			items = append(items, item{h.Source, t})
		}
	}

	unlockPublish, err := lockPublish(bridge, task)
	if err != nil {
		return fmt.Errorf("%v; %s stays open, close it again once the publish is done", err, task)
	}
	defer func() {
		if unlockPublish != nil {
			unlockPublish()
		}
	}()
	if err := writeClosed(bridge, task, taskFile); err != nil {
		return err
	}
	if why := peerKeepsRecords(bridge, task); why != "" {
		fmt.Fprintf(out, "warning: task.close kept the publish and tested records of %s, which share their file names with another task: %s; gc removes them once all are closed\n", task, why)
	} else {
		tested, err := testedFilesOf(bridge, task)
		if err != nil {
			fmt.Fprintf(out, "warning: task.close could not list the tested records of %s: %v\n", task, err)
		}
		for _, f := range append(tested, publishRecordFile(bridge, task)) {
			if err := removeRecord(f); err != nil {
				fmt.Fprintf(out, "warning: task.close could not remove %s: %v\n", f, err)
			}
		}
	}
	unlockPublish()
	unlockPublish = nil
	if err := addToJournal(bridge, task, "- "+time.Now().Format("2006-01-02")+": closed."); err != nil {
		fmt.Fprintf(out, "warning: task.close could not journal the close of %s: %v\n", task, err)
	}

	for _, h := range handlers {
		if h.Kind != "observe" {
			continue
		}
		res, err := shell(bridge, h.Command, env)
		if err != nil {
			fmt.Fprintf(out, "warning: task.close observe %s (from %s) failed: %v\n", h.Command, h.Source, err)
			continue
		}
		if t := strings.TrimSpace(res); t != "" {
			items = append(items, item{h.Source, t})
		}
	}

	fmt.Fprintf(out, "Closed %s.\n", task)
	items = append(items, augmentItems...)
	if len(items) > 0 {
		fmt.Fprintln(out, "Deal with the following before ending this turn; if it needs the user, ask them now:")
		for _, it := range items {
			fmt.Fprintf(out, "[%s] %s\n", it.source, it.text)
		}
	}
	return nil
}

// writeClosed marks task.md closed from a fresh read, under the journal lock so that no journal write or history split between the read and the write is lost.
func writeClosed(bridge, task, taskFile string) error {
	lock := taskStateFile(bridge, task, "", ".journal.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return err
	}
	l := flock.New(lock)
	if err := l.Lock(); err != nil {
		return fmt.Errorf("locking the journal: %w", err)
	}
	defer l.Unlock()
	data, err := os.ReadFile(taskFile)
	if err != nil {
		return err
	}
	marked, err := markClosed(data)
	if err != nil {
		return fmt.Errorf("%s: %v", taskFile, err)
	}
	return writeFileAtomic(bridge, taskFile, marked)
}

// withoutRunEnv drops the variables a run sets, so a handler script that calls `ensign` does not act as part of the run that closed the task.
func withoutRunEnv(base []string, windows bool) []string {
	names := []string{"STARSHIP_RUN", "STARSHIP_RUN_DIR", "STARSHIP_WORKSPACE", "STARSHIP_LOCATION", "STARSHIP_STAGE", "STARSHIP_DISPATCH", "STARSHIP_OUTCOME"}
	out := make([]string, 0, len(base))
	for _, entry := range base {
		n, _, _ := strings.Cut(entry, "=")
		drop := false
		for _, v := range names {
			if n == v || windows && strings.EqualFold(n, v) {
				drop = true
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}

// markClosed adds `status: closed` to the frontmatter and a first line `Done.` to the `## Status` section, touching no other byte and keeping the file's line endings.
func markClosed(data []byte) ([]byte, error) {
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	lines := strings.SplitAfter(string(data), "\n")
	bare := func(i int) string { return strings.TrimRight(lines[i], "\r\n") }
	if bare(0) != "---" {
		return nil, fmt.Errorf("no YAML frontmatter")
	}
	// Any line starting with --- closes the frontmatter, as splitFrontmatterData reads it.
	closing := 1
	for closing < len(lines) && !strings.HasPrefix(bare(closing), "---") {
		closing++
	}
	if closing == len(lines) {
		return nil, fmt.Errorf("frontmatter is not closed with ---")
	}
	marked := false
	for i := 1; i < closing && !marked; i++ {
		if strings.HasPrefix(lines[i], "status:") {
			lines[i] = "status: closed" + lines[i][len(bare(i)):]
			marked = true
		}
	}
	if !marked {
		lines = append(lines[:closing], append([]string{"status: closed" + eol}, lines[closing:]...)...)
		closing++
	}
	for i := closing + 1; i < len(lines); i++ {
		if h, ok := strings.CutPrefix(bare(i), "## "); ok && strings.EqualFold(strings.TrimSpace(h), "Status") {
			if !strings.HasSuffix(lines[i], "\n") {
				lines[i] += eol
			}
			lines[i] += eol + "Done." + eol
			return []byte(strings.Join(lines, "")), nil
		}
	}
	out := strings.Join(lines, "")
	if !strings.HasSuffix(out, "\n") {
		out += eol
	}
	return []byte(out + eol + "## Status" + eol + eol + "Done." + eol), nil
}

// peerKeepsRecords says why a closed task's records must stay: the tasks could not be listed, or another task that encodes to the same record names is open or has a status that can't be read. It returns "" when every other such task is closed.
func peerKeepsRecords(bridge, task string) string {
	files, err := taskFiles(bridge)
	if err != nil {
		return fmt.Sprintf("the tasks could not be listed: %v", err)
	}
	enc := strings.ReplaceAll(task, "/", "--")
	for _, f := range files {
		peer := taskOfFile(f)
		if peer == task || strings.ReplaceAll(peer, "/", "--") != enc {
			continue
		}
		closed, err := taskIsClosed(f)
		if err != nil {
			return fmt.Sprintf("the status of %s can't be read: %v", peer, err)
		}
		if !closed {
			return peer + " is open"
		}
	}
	return ""
}
