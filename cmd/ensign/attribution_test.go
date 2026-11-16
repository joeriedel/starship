// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttributionLine(t *testing.T) {
	for _, s := range []string{
		"Co-Authored-By: Someone <someone@example.com>",
		"fix supernova\n\nco-authored-by: A <a@example.com>",
		"  CO-AUTHORED-BY: A",
		"Claude-Session: https://example.com/session_1",
		"claude-session:x",
		"Generated with Claude Code",
		"generated with claude code",
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)",
		"Generated with [Claude Code](https://claude.com/claude-code)",
		"body\n\n  🤖 generated with Claude Code\n",
	} {
		if attributionLine(s) == "" {
			t.Errorf("not matched: %q", s)
		}
	}
	for _, s := range []string{
		"",
		"Add the supernova catalog",
		"fix the co-authored-by docs",
		"Mention co-authored work in the supernova notes\n\nSession handling: claude is a name",
		"The report was generated with a script",
	} {
		if l := attributionLine(s); l != "" {
			t.Errorf("matched %q in %q", l, s)
		}
	}
	if got := attributionLine("a\n  Co-Authored-By: X \nb"); got != "Co-Authored-By: X" {
		t.Errorf("line not trimmed: %q", got)
	}
	for _, v := range []string{"Co-Authored-By: X", "co-authored-by=X", "Claude-Session: s"} {
		if attributionTrailer(v) == "" {
			t.Errorf("trailer not matched: %q", v)
		}
	}
	for _, v := range []string{"Reviewed-by: X", "Signed-off-by: X", "plain"} {
		if attributionTrailer(v) != "" {
			t.Errorf("trailer matched: %q", v)
		}
	}
}

func TestJournalCommitRefusesAttribution(t *testing.T) {
	needGit(t)
	w := newWorld(t)
	journalFixture(t, w)
	write(t, notesPath(w, "notes/profile.md"), "Maintainer of supernova.\n")
	before := runGit(t, w.bridge, "rev-parse", "HEAD")
	var buf bytes.Buffer
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(w.bridge)
	err := cmdJournal([]string{"commit", "-m", "Notes\n\nCo-Authored-By: Someone <someone@example.com>"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "Co-Authored-By: Someone") || !strings.Contains(err.Error(), "never added") {
		t.Fatalf("expected a refusal naming the line, got %v", err)
	}
	if after := runGit(t, w.bridge, "rev-parse", "HEAD"); after != before {
		t.Errorf("a commit was made: %s -> %s", before, after)
	}
	if st := runGit(t, w.bridge, "diff", "--cached", "--name-only"); st != "" {
		t.Errorf("files were staged: %s", st)
	}
}

func TestSquashAndPassCommitRefuseAttribution(t *testing.T) {
	if _, err := squash("/nonexistent/supernova", "abc", "Add supernova\n\nGenerated with Claude Code"); err == nil || !strings.Contains(err.Error(), "attribution line") {
		t.Errorf("squash: %v", err)
	}
	ws := newRepo(t, filepath.Join(t.TempDir(), "supernova"))
	r := unitRun(t, ws)
	d := &Dispatch{ID: "d1", Summary: "done\nClaude-Session: https://example.com/s", Fields: map[string][]string{"files": {"lib.txt"}}}
	if err := catalogCommitPass(r, d); err != nil {
		t.Errorf("commit-pass with nothing to commit: %v", err)
	}
	write(t, filepath.Join(ws, "lib.txt"), "work\n")
	if err := catalogCommitPass(r, d); err == nil || !strings.Contains(err.Error(), "attribution line") {
		t.Errorf("commit-pass: %v", err)
	}
	if err := commitDocsPaths("/nonexistent/supernova", []string{"README.md"}, true, "-m", "docs", "-m", "Co-Authored-By: X"); err == nil || !strings.Contains(err.Error(), "attribution line") {
		t.Errorf("docs commit: %v", err)
	}
}

func TestBashHookRefusesAttribution(t *testing.T) {
	dir := t.TempDir()
	msgFile := filepath.Join(dir, "msg.txt")
	if err := os.WriteFile(msgFile, []byte("Add supernova\n\nCo-Authored-By: X <x@example.com>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanFile := filepath.Join(dir, "clean.txt")
	if err := os.WriteFile(cleanFile, []byte("Add supernova\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused := []string{
		"git commit -m \"$(cat <<'EOF'\nAdd supernova\n\nCo-Authored-By: X <x@example.com>\nEOF\n)\"",
		"git commit -F - <<'EOF'\nAdd supernova\n\nCo-Authored-By: X <x@example.com>\nEOF",
		"cat <<'EOF' | git commit -F -\nAdd supernova\n\nCo-Authored-By: X <x@example.com>\nEOF",
		"cat <<EOF | gh pr create --body-file -\nSummary\n\nGenerated with Claude Code\nEOF",
		"cat <<'EOF' |\nAdd supernova\n\nCo-Authored-By: X <x@example.com>\nEOF\ngit commit -F -",
		"cat <<'EOF' |&\nAdd supernova\n\nCo-Authored-By: X <x@example.com>\nEOF\ngit commit -F -",
		"cat <<EOF |\nSummary\n\nGenerated with Claude Code\nEOF\ngh pr create --body-file -",
		"git commit -m \"Add supernova\" --trailer \"Co-Authored-By: X <x@example.com>\"",
		"git commit --trailer=Claude-Session=s -m x",
		"git commit -F " + msgFile,
		"git commit --file=" + msgFile,
		"git commit --amend -m 'x\n\nCo-Authored-By: X'",
		"git commit -am 'x\n\nCo-Authored-By: X'",
		"git commit --message='x\n\nCo-Authored-By: X'",
		"git commit -m 'x' -m 'Co-Authored-By: X'",
		"git -C /tmp/supernova -c user.name=a commit -m 'x\nCo-Authored-By: X'",
		"cd supernova && git add -A && git commit -m \"$(printf 'x\\n')\" -m \"$(cat <<EOF\nGenerated with Claude Code\nEOF\n)\"",
		"bash -c \"git commit -m 'x\n\nCo-Authored-By: X'\"",
		"gh pr create --title t --body \"Summary\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\"",
		"gh pr create --body-file " + msgFile,
		"gh pr edit 3 -b 'x\n\nGenerated with Claude Code'",
		"gh pr create --title t --body \"$(cat <<'EOF'\nSummary\n\nGenerated with Claude Code\nEOF\n)\"",
	}
	allowed := []string{
		"git commit -m 'Add supernova'",
		"cat > CONTRIBUTING.md <<'EOF' && git add CONTRIBUTING.md && git commit -m \"Document trailer policy\"\nExample:\nCo-Authored-By: Name <email>\nEOF",
		"cat > F <<'EOF'\nExample:\nCo-Authored-By: Name <email>\nEOF\ngit commit -m \"Add supernova\"",
		"git commit -m \"$(cat <<'EOF'\nAdd supernova\n\nBody with details\nEOF\n)\"",
		"git commit -m 'fix the co-authored-by docs'",
		"git commit -F " + cleanFile,
		"git commit -F " + filepath.Join(dir, "missing.txt"),
		"git commit --trailer 'Reviewed-by: X' -m x",
		"git log --format=%B",
		"git push origin main",
		"echo 'Co-Authored-By: X'",
		"gh pr create --title t --body 'Summary of supernova'",
		"gh pr list",
	}
	for _, who := range []struct{ id, typ string }{{"", ""}, {"a1", "coder"}} {
		for _, c := range refused {
			denied, reason := bashHook(t, true, who.id, who.typ, jsonStr2(c))
			if !denied || !strings.Contains(reason, "never added") {
				t.Errorf("%q %q: not refused (%v, %q)", who.typ, c, denied, reason)
			}
		}
		for _, c := range allowed {
			if who.id == "" && strings.HasPrefix(c, "git push") {
				continue
			}
			if denied, reason := bashHook(t, true, who.id, who.typ, jsonStr2(c)); denied {
				t.Errorf("%q %q: refused: %s", who.typ, c, reason)
			}
		}
	}
}

func jsonStr2(cmd string) string { return `{"command":` + jsonStr(cmd) + `}` }
