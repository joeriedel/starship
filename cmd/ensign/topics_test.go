package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func linkedWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	if err := link(w.bridge, false, nil); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) skillCmd(args ...string) string {
	w.t.Helper()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	cmdSkill(args, &out)
	return out.String()
}

func TestTopicDeliveredOncePerSession(t *testing.T) {
	w := linkedWorld(t)
	const marker = "# Sequences"
	if out, _ := w.seq(); !strings.Contains(out, marker) {
		t.Fatalf("first use should print the topic:\n%s", out)
	}
	if out, _ := w.seq(); strings.Contains(out, marker) {
		t.Errorf("second use should not print the topic")
	}
	t.Setenv("STARSHIP_SESSION", "s2")
	if out, _ := w.seq(); !strings.Contains(out, marker) {
		t.Errorf("a new session should get the topic again")
	}
}

func TestTopicEverySkillCallWithoutSession(t *testing.T) {
	w := linkedWorld(t)
	t.Setenv("STARSHIP_SESSION", "")
	for i := 0; i < 2; i++ {
		if out := w.skillCmd("discard", "nothing"); !strings.Contains(out, "# Facts with no skill") {
			t.Errorf("call %d should print the topic:\n%s", i, out)
		}
	}
	t.Setenv("STARSHIP_SESSION", "s1")
	w.skillCmd("discard", "nothing")
	if out := w.skillCmd("discard", "nothing"); strings.Contains(out, "# Facts with no skill") {
		t.Errorf("a session should get the topic once")
	}
}

func TestTopicMissingIsReported(t *testing.T) {
	w := newWorld(t)
	out, _ := w.seq()
	if !strings.Contains(out, "ensign link") {
		t.Errorf("a missing topic should say to link:\n%s", out)
	}
	if _, err := os.Stat(sessionFile(w.bridge, "s1") + ".topic-sequence"); err == nil {
		t.Errorf("a missing topic should not be recorded as delivered")
	}
}

func TestHelpCommand(t *testing.T) {
	w := linkedWorld(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	err := cmdHelp([]string{"nope"}, &out)
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "project, sequence, skill") {
		t.Errorf("unknown topic should list topics: %v", err)
	}
	if err := cmdHelp([]string{"../session"}, &out); err == nil {
		t.Errorf("a path is not a topic")
	}
	if err := cmdHelp(nil, &out); err != nil || !strings.Contains(out.String(), "sequence") {
		t.Errorf("no topic should list them: %v %q", err, out.String())
	}
}

func TestManualTopicsExistAndPrint(t *testing.T) {
	w := linkedWorld(t)
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "manual", "session.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile("ensign help ([a-z-]+)").FindAllStringSubmatch(string(data), -1)
	if len(named) == 0 {
		t.Fatal("the manual names no topics")
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	for _, m := range named {
		if _, err := os.Stat(filepath.Join(root, "manual", "topics", m[1]+".md")); err != nil {
			t.Errorf("manual names topic %s: %v", m[1], err)
		}
		var out bytes.Buffer
		if err := cmdHelp([]string{m[1]}, &out); err != nil || out.Len() == 0 {
			t.Errorf("help %s: %v", m[1], err)
		}
	}
}

func TestSessionStartClearsTopicsOnlyAfterCompactOrClear(t *testing.T) {
	const marker = "# Sequences"
	for _, c := range []struct {
		source string
		again  bool
	}{{"compact", true}, {"clear", true}, {"resume", false}, {"startup", false}} {
		t.Run(c.source, func(t *testing.T) {
			w := linkedWorld(t)
			if out, _ := w.seq(); !strings.Contains(out, marker) {
				t.Fatalf("first use should print the topic:\n%s", out)
			}
			w.hook("SessionStart", map[string]any{"source": c.source})
			if out, _ := w.seq(); strings.Contains(out, marker) != c.again {
				t.Errorf("after %s, topic printed again = %v, want %v:\n%s", c.source, !c.again, c.again, out)
			}
		})
	}
}

func TestCompactLeavesOtherSessionFiles(t *testing.T) {
	w := linkedWorld(t)
	w.seq()
	t.Setenv("STARSHIP_SESSION", "s2")
	w.seq()
	for _, suffix := range []string{"", ".task", ".sequence"} {
		writeFile(sessionFile(w.bridge, w.sid)+suffix, "x")
	}
	w.hook("SessionStart", map[string]any{"source": "compact"})
	for _, f := range []string{
		sessionFile(w.bridge, "s2") + ".topic-sequence",
		sessionFile(w.bridge, w.sid),
		sessionFile(w.bridge, w.sid) + ".pin",
		sessionFile(w.bridge, w.sid) + ".task",
		sessionFile(w.bridge, w.sid) + ".sequence",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s should survive a compaction: %v", filepath.Base(f), err)
		}
	}
	if _, err := os.Stat(sessionFile(w.bridge, w.sid) + ".topic-sequence"); err == nil {
		t.Errorf("the compacted session's topic marker should be gone")
	}
}
