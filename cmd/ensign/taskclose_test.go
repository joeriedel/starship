// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// TestCloseHelper stands in for a handler command on every OS: the handlers below run this test binary with arguments after "--".
func TestCloseHelper(t *testing.T) {
	i := 0
	for i < len(os.Args) && os.Args[i] != "--" {
		i++
	}
	if i == len(os.Args) {
		return
	}
	args := os.Args[i+1:]
	switch args[0] {
	case "echo":
		fmt.Println(strings.Join(args[1:], " "))
	case "fail":
		fmt.Println("boom one")
		fmt.Println("boom two")
		os.Exit(1)
	case "failunless":
		if _, err := os.Stat(args[1]); err != nil {
			fmt.Println("missing " + filepath.Base(args[1]))
			os.Exit(1)
		}
	case "mark":
		f, _ := os.OpenFile(args[1], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, "fired")
		f.Close()
	case "status":
		data, _ := os.ReadFile(filepath.Join(os.Getenv("STARSHIP_BRIDGE"), "projects", os.Getenv("STARSHIP_PROJECT"), "tasks", "t", "task.md"))
		fmt.Println("closed seen:", strings.Contains(string(data), "status: closed"))
	case "append":
		f, _ := os.OpenFile(args[1], os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, "\nnote from handler")
		f.Close()
	case "runenv":
		for _, n := range []string{"STARSHIP_RUN", "STARSHIP_RUN_DIR", "STARSHIP_WORKSPACE", "STARSHIP_LOCATION", "STARSHIP_STAGE", "STARSHIP_DISPATCH", "STARSHIP_OUTCOME"} {
			if _, ok := os.LookupEnv(n); ok {
				fmt.Println("leaked", n)
			}
		}
		fmt.Println("runenv checked")
	case "env":
		wd, _ := os.Getwd()
		_, hasRun := os.LookupEnv("STARSHIP_RUN")
		_, hasWs := os.LookupEnv("STARSHIP_WORKSPACE")
		fmt.Println("task", os.Getenv("STARSHIP_TASK"), "project", os.Getenv("STARSHIP_PROJECT"), "bridge", filepath.Base(os.Getenv("STARSHIP_BRIDGE")) == filepath.Base(wd), "run", hasRun, "ws", hasWs)
	}
	os.Exit(0)
}

func helper(args ...string) string {
	exe, _ := os.Executable()
	return `"` + filepath.ToSlash(exe) + `" -test.run=^TestCloseHelper$ -- ` + strings.Join(args, " ")
}

func handlerYAML(kind, cmd string) string {
	return "  - { " + kind + ": '" + cmd + "' }\n"
}

// closeEvents is an events: block for task.close from kind/command pairs.
func closeEvents(pairs ...string) string {
	s := "events:\n  task.close:\n"
	for i := 0; i < len(pairs); i += 2 {
		s += "  " + handlerYAML(pairs[i], pairs[i+1])
	}
	return s
}

func closeTask(w *world, task string) (string, error) {
	w.enter()
	var out bytes.Buffer
	err := cmdTask([]string{"close", "supernova/" + task}, &out)
	return out.String(), err
}

func (w *world) taskText(task string) string {
	data, err := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks", task, "task.md"))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(data)
}

func (w *world) historyText(task string) string {
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks", task, "history.md"))
	return string(data)
}

func TestValidEventTaskClose(t *testing.T) {
	if !validEvent("task.close", &Pipeline{}) {
		t.Error("task.close is not a valid event")
	}
}

func TestCloseNoCodeAndCodeTasks(t *testing.T) {
	for _, noCode := range []bool{true, false} {
		t.Run(fmt.Sprint("noCode=", noCode), func(t *testing.T) {
			var w *world
			if noCode {
				w = noCodeWorld(t, "pipeline: none")
			} else {
				w = newWorld(t)
			}
			w.frontTask("t", "")
			out, err := closeTask(w, "t")
			if err != nil {
				t.Fatalf("noCode=%v: %v", noCode, err)
			}
			if out != "Closed supernova/t.\n" {
				t.Errorf("noCode=%v: output %q", noCode, out)
			}
			want := "---\ndocumentation: on\nstatus: closed\n---\n\n## Goal\n\nDo t.\n\n## Status\n\nDone.\n\nBegun.\n\n## Decisions\n\nKeep it small.\n"
			if got := w.taskText("t"); got != want {
				t.Errorf("noCode=%v: task.md is\n%q\nwant\n%q", noCode, got, want)
			}
			if h := w.historyText("t"); !strings.Contains(h, "- "+time.Now().Format("2006-01-02")+": closed.") {
				t.Errorf("noCode=%v: history is %q", noCode, h)
			}
		})
	}
}

func TestCloseAddsStatusSection(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\ndocumentation: on\n---\n\n## Goal\n\nDo t.")
	if _, err := closeTask(w, "t"); err != nil {
		t.Fatal(err)
	}
	want := "---\ndocumentation: on\nstatus: closed\n---\n\n## Goal\n\nDo t.\n\n## Status\n\nDone.\n"
	if got := w.taskText("t"); got != want {
		t.Errorf("task.md is %q, want %q", got, want)
	}
}

func TestCloseTwiceFiresNothing(t *testing.T) {
	w := newWorld(t)
	marker := filepath.Join(t.TempDir(), "marker")
	w.project(closeEvents("observe", helper("mark", filepath.ToSlash(marker))))
	w.frontTask("t", "")
	if _, err := closeTask(w, "t"); err != nil {
		t.Fatal(err)
	}
	task, hist := w.taskText("t"), w.historyText("t")
	out, err := closeTask(w, "t")
	if err != nil || !strings.Contains(out, "already closed") || strings.Contains(out, "Closed supernova") {
		t.Errorf("second close: %q, %v", out, err)
	}
	if w.taskText("t") != task || w.historyText("t") != hist {
		t.Error("second close changed the task or its history")
	}
	if data, _ := os.ReadFile(marker); strings.Count(string(data), "fired") != 1 {
		t.Errorf("handler fired %q times, want once", string(data))
	}
}

func TestCloseIgnoresHandWrittenDone(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\ndocumentation: on\n---\n\n## Status\n\nDone. All finished.\n")
	out, err := closeTask(w, "t")
	if err != nil || !strings.HasPrefix(out, "Closed supernova/t.") {
		t.Fatalf("close: %q, %v", out, err)
	}
	if got := w.taskText("t"); !strings.Contains(got, "status: closed") || !strings.Contains(got, "Done.\n\nDone. All finished.") {
		t.Errorf("task.md is %q", got)
	}
}

func TestCloseFailingRequireLeavesTaskOpen(t *testing.T) {
	w := newWorld(t)
	gate := filepath.Join(t.TempDir(), "gate")
	w.project(closeEvents("require", helper("echo", "first-ok"), "require", helper("failunless", filepath.ToSlash(gate))))
	w.frontTask("t", "")
	before := w.taskText("t")
	out, err := closeTask(w, "t")
	if err == nil || !strings.Contains(err.Error(), "missing gate") || !strings.Contains(err.Error(), "project:") {
		t.Fatalf("close error: %v", err)
	}
	if strings.Contains(out, "first-ok") {
		t.Errorf("output of a succeeded require was delivered: %q", out)
	}
	if w.taskText("t") != before || w.historyText("t") != "" {
		t.Error("a failed close wrote something")
	}
	write(t, gate, "x")
	out, err = closeTask(w, "t")
	if err != nil || !strings.HasPrefix(out, "Closed supernova/t.") {
		t.Fatalf("retry: %q, %v", out, err)
	}
}

func TestCloseFailingObserveWarns(t *testing.T) {
	w := newWorld(t)
	w.project(closeEvents("observe", helper("fail")))
	w.frontTask("t", "")
	out, err := closeTask(w, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: task.close observe") || !strings.Contains(out, "project:") || !strings.Contains(out, "Closed supernova/t.") {
		t.Errorf("output %q", out)
	}
	if !strings.Contains(w.taskText("t"), "status: closed") {
		t.Error("task not closed")
	}
}

func TestCloseObserveSeesClosedAndEnv(t *testing.T) {
	w := newWorld(t)
	w.project(closeEvents("observe", helper("status"), "observe", helper("env")))
	w.frontTask("t", "")
	out, err := closeTask(w, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "closed seen: true") {
		t.Errorf("observe did not see a closed task: %q", out)
	}
	if !strings.Contains(out, "task supernova/t project supernova bridge true run false ws false") {
		t.Errorf("handler env or cwd wrong: %q", out)
	}
}

func TestCloseDeliversOutputAndAugments(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "ask.md"), "Is the release note written?\n")
	w.project(closeEvents("require", helper("echo", "from-require"), "observe", helper("echo", "from-observe"), "augment", "ask.md"))
	w.frontTask("t", "")
	out, err := closeTask(w, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Closed supernova/t.\n", "before ending this turn", "from-require", "from-observe", "Is the release note written?", "[project:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
}

func TestCloseNothingToDeliver(t *testing.T) {
	w2 := newWorld(t)
	w2.project(closeEvents("observe", helper("mark", filepath.ToSlash(filepath.Join(t.TempDir(), "m")))))
	w2.frontTask("t", "")
	out, err := closeTask(w2, "t")
	if err != nil || out != "Closed supernova/t.\n" {
		t.Errorf("nothing to deliver: %q, %v", out, err)
	}
}

func TestCloseRefusesRunOnlyForms(t *testing.T) {
	for _, h := range [][2]string{{"require", "@build"}, {"observe", "ensign git-catalog record-base"}, {"augment", "@summary"}} {
		t.Run(h[1], func(t *testing.T) {
			w := newWorld(t)
			w.project(closeEvents(h[0], h[1]))
			w.frontTask("t", "")
			c, err := loadTaskEvents(w.bridge, "supernova/t")
			if err != nil {
				t.Fatal(err)
			}
			c.Pipeline = &Pipeline{}
			found := false
			for _, e := range validate(c) {
				found = found || strings.Contains(e, "task.close can't")
			}
			if !found {
				t.Errorf("validation accepted %s %s", h[0], h[1])
			}
			before := w.taskText("t")
			if _, err := closeTask(w, "t"); err == nil || !strings.Contains(err.Error(), "task.close can't") {
				t.Errorf("close of %s %s: %v", h[0], h[1], err)
			}
			if w.taskText("t") != before {
				t.Error("a refused close wrote something")
			}
		})
	}
	if closeHandlerProblem("run.start", Handler{Kind: "require", Command: "@build"}) != "" {
		t.Error("another event's handler was refused")
	}
}

func TestCloseHeldLockRefuses(t *testing.T) {
	w := newWorld(t)
	w.frontTask("t", "")
	lock := taskStateFile(w.bridge, "supernova/t", "", ".close.lock")
	os.MkdirAll(filepath.Dir(lock), 0o755)
	l := flock.New(lock)
	if ok, err := l.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	defer l.Unlock()
	before := w.taskText("t")
	if _, err := closeTask(w, "t"); err == nil || !strings.Contains(err.Error(), "already under way") {
		t.Errorf("close with the lock held: %v", err)
	}
	if w.taskText("t") != before {
		t.Error("task changed")
	}
}

func TestCloseKeepsCRLF(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\r\ndocumentation: on\r\n---\r\n\r\n## Status\r\n\r\nBegun.\r\n")
	if _, err := closeTask(w, "t"); err != nil {
		t.Fatal(err)
	}
	want := "---\r\ndocumentation: on\r\nstatus: closed\r\n---\r\n\r\n## Status\r\n\r\nDone.\r\n\r\nBegun.\r\n"
	if got := w.taskText("t"); got != want {
		t.Errorf("task.md is %q, want %q", got, want)
	}
}

func TestCloseMissingTask(t *testing.T) {
	w := newWorld(t)
	if _, err := closeTask(w, "nope"); err == nil || !strings.Contains(err.Error(), "no task") {
		t.Errorf("got %v", err)
	}
}

func TestCloseKeepsRequireHandlerEdit(t *testing.T) {
	w := newWorld(t)
	taskFile := filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md")
	w.project(closeEvents("require", helper("append", filepath.ToSlash(taskFile))))
	w.frontTask("t", "")
	if _, err := closeTask(w, "t"); err != nil {
		t.Fatal(err)
	}
	got := w.taskText("t")
	if !strings.Contains(got, "note from handler") || !strings.Contains(got, "status: closed") {
		t.Errorf("task.md is %q", got)
	}
}

func TestCloseMalformedHandlerRefuses(t *testing.T) {
	w := newWorld(t)
	marker := filepath.Join(t.TempDir(), "marker")
	w.project("events:\n  task.close:\n    - { requires: ./check.sh }\n  " + handlerYAML("observe", helper("mark", filepath.ToSlash(marker))))
	w.frontTask("t", "")
	before := w.taskText("t")
	if _, err := closeTask(w, "t"); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("close error: %v", err)
	}
	if w.taskText("t") != before || w.historyText("t") != "" {
		t.Error("a refused close wrote something")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a handler fired")
	}
}

func TestCloseHandlerWithTwoFieldsRefuses(t *testing.T) {
	w := newWorld(t)
	w.project("events:\n  task.close:\n    - { require: ./check.sh, observe: ./note.sh }\n")
	w.frontTask("t", "")
	if _, err := closeTask(w, "t"); err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("close error: %v", err)
	}
}

func TestCloseUnwritableJournalStillRunsObserve(t *testing.T) {
	w := newWorld(t)
	w.project(closeEvents("observe", helper("echo", "from-observe")))
	w.frontTask("t", "")
	if err := os.MkdirAll(filepath.Join(w.bridge, "projects/supernova/tasks/t/history.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := closeTask(w, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"warning: task.close could not journal", "Closed supernova/t.", "before ending this turn", "from-observe"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if !strings.Contains(w.taskText("t"), "status: closed") {
		t.Error("task not closed")
	}
}

func TestCloseOddClosingFence(t *testing.T) {
	for _, fence := range []string{"--- ", "----"} {
		t.Run(fence, func(t *testing.T) {
			w := newWorld(t)
			marker := filepath.Join(t.TempDir(), "marker")
			w.project(closeEvents("observe", helper("mark", filepath.ToSlash(marker))))
			write(t, filepath.Join(w.bridge, "projects/supernova/tasks/t/task.md"), "---\ndocumentation: on\n"+fence+"\n\n## Status\n\nBegun.\n")
			if _, err := closeTask(w, "t"); err != nil {
				t.Fatal(err)
			}
			if got := w.taskText("t"); !strings.Contains(got, "status: closed\n"+fence+"\n") {
				t.Errorf("task.md is %q", got)
			}
			out, err := closeTask(w, "t")
			if err != nil || !strings.Contains(out, "already closed") {
				t.Errorf("second close: %q, %v", out, err)
			}
			if data, _ := os.ReadFile(marker); strings.Count(string(data), "fired") != 1 {
				t.Errorf("handler fired %q times, want once", string(data))
			}
		})
	}
}

func TestCloseDropsRunEnv(t *testing.T) {
	w := newWorld(t)
	w.project(closeEvents("require", helper("runenv"), "observe", helper("runenv")))
	w.frontTask("t", "")
	for _, n := range []string{"STARSHIP_RUN", "STARSHIP_RUN_DIR", "STARSHIP_WORKSPACE", "STARSHIP_LOCATION", "STARSHIP_STAGE", "STARSHIP_DISPATCH", "STARSHIP_OUTCOME"} {
		t.Setenv(n, "x")
	}
	out, err := closeTask(w, "t")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "leaked") || strings.Count(out, "runenv checked") != 2 {
		t.Errorf("output %q", out)
	}
}

func TestWithoutRunEnvWindowsCase(t *testing.T) {
	got := withoutRunEnv([]string{"Starship_Run_Dir=x", "PATH=y"}, true)
	if len(got) != 1 || got[0] != "PATH=y" {
		t.Errorf("windows: %v", got)
	}
	if got := withoutRunEnv([]string{"Starship_Run_Dir=x"}, false); len(got) != 1 {
		t.Errorf("unix: %v", got)
	}
}
