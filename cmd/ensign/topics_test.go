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
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "project, requests, runs, sequence, skill") {
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
	if err := os.MkdirAll(filepath.Join(runsDir(w.bridge), "x"), 0o755); err != nil {
		t.Fatal(err)
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

func (w *world) startWithNotes(source, notes string) string {
	w.t.Helper()
	taskDir := filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor")
	if notes != "" {
		write(w.t, filepath.Join(w.bridge, "projects/supernova/project.md"), notes)
	}
	var open bytes.Buffer
	if err := openTask(w.bridge, "supernova/auth-refactor", taskDir, &open); err != nil {
		w.t.Fatal(err)
	}
	res, _ := w.hook("SessionStart", map[string]any{"source": source})
	spec, _ := res["hookSpecificOutput"].(map[string]any)
	ctx, _ := spec["additionalContext"].(string)
	return ctx
}

func TestSessionStartReprintsNotesAfterCompactOrClear(t *testing.T) {
	const notes = "Always answer in haiku.\n"
	for _, c := range []struct {
		source string
		want   bool
	}{{"compact", true}, {"clear", true}, {"resume", false}, {"startup", false}} {
		t.Run(c.source, func(t *testing.T) {
			w := linkedWorld(t)
			ctx := w.startWithNotes(c.source, notes)
			if got := strings.Contains(ctx, "Always answer in haiku."); got != c.want {
				t.Fatalf("notes reprinted = %v, want %v:\n%s", got, c.want, ctx)
			}
			if c.want {
				block, _ := projectNotes(w.bridge, "supernova")
				if !strings.HasSuffix(ctx, block) || len(ctx) >= sessionContextLimit {
					t.Errorf("notes should come last, under the limit (%d):\n%s", len(ctx), ctx)
				}
				if strings.Index(ctx, "STARSHIP-READY") > strings.Index(ctx, "notes      projects/supernova/project.md") {
					t.Errorf("notes should follow the manual")
				}
			}
		})
	}
}

func TestSessionStartPointsAtNotesTooLongToReprint(t *testing.T) {
	w := linkedWorld(t)
	body := strings.Repeat("Prefer small commits and explain each one.\n", 400)
	ctx := w.startWithNotes("compact", body)
	if strings.Contains(ctx, "Prefer small commits") {
		t.Errorf("long notes should not be reprinted")
	}
	if !strings.Contains(ctx, "`ensign task open supernova/auth-refactor`") {
		t.Errorf("want a pointer to task open:\n%s", ctx)
	}
	if len(ctx) >= sessionContextLimit {
		t.Errorf("context is %d characters", len(ctx))
	}
}

func TestSessionStartReprintsNothingWithoutTaskOrNotes(t *testing.T) {
	w := linkedWorld(t)
	write(t, filepath.Join(w.bridge, "projects/supernova/project.md"), "Always answer in haiku.\n")
	res, _ := w.hook("SessionStart", map[string]any{"source": "compact"})
	spec, _ := res["hookSpecificOutput"].(map[string]any)
	if ctx, _ := spec["additionalContext"].(string); strings.Contains(ctx, "haiku") {
		t.Errorf("no recorded task, no notes:\n%s", ctx)
	}
	w = linkedWorld(t)
	if ctx := w.startWithNotes("compact", ""); strings.Contains(ctx, "notes      ") {
		t.Errorf("no project.md, no notes:\n%s", ctx)
	}
}

func TestSessionStartPointsAtRequestsTopic(t *testing.T) {
	w := linkedWorld(t)
	ctx := w.startWithNotes("startup", "")
	if !strings.Contains(ctx, "`ensign help requests`") || strings.Contains(ctx, "ensign git-catalog squash") {
		t.Errorf("want a pointer to the requests topic and not the list:\n%s", ctx)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	var out bytes.Buffer
	if err := cmdHelp([]string{"requests"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != requestCommands(w.bridge) || !strings.Contains(out.String(), "ensign git-catalog squash") {
		t.Errorf("requests topic should print the entries:\n%s", out.String())
	}
}

// Claude Code cuts additionalContext at 10,000 characters; the bridge path is repeated in the text, so the budget is checked as if the path were 200 characters long.
func TestSessionStartFitsBudgetWithLongBridgePath(t *testing.T) {
	for _, c := range []struct{ name, source, notes string }{
		{"startup", "startup", ""},
		{"notes too long to reprint", "compact", strings.Repeat("Prefer small commits and explain each one.\n", 400)},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := linkedWorld(t)
			ctx := w.startWithNotes(c.source, c.notes)
			size := len(ctx)
			if len(w.bridge) < 200 {
				size += (200 - len(w.bridge)) * strings.Count(ctx, w.bridge)
			}
			t.Logf("padded size %d, limit %d", size, manualLimit)
			if size >= manualLimit {
				t.Errorf("session start text is %d characters with a 200-character bridge path; the budget is %d", size, manualLimit)
			}
		})
	}
}
