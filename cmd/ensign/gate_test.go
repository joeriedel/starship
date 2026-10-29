package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGates(t *testing.T) {
	needGit(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("STARSHIP_SKIP_REVIEW", "1")
	repo := filepath.Join(t.TempDir(), "starship")
	os.MkdirAll(repo, 0o755)
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "me@example.com")
	runGit(t, repo, "config", "user.name", "me")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(repo)
	var out bytes.Buffer
	if err := cmdGate([]string{"install", "--identity", "me@example.com"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git/hooks/pre-commit")); err != nil {
		t.Fatalf("pre-commit hook not installed")
	}

	// A registered bridge with an empty denylist fails closed.
	bridge := filepath.Join(t.TempDir(), "work")
	write(t, filepath.Join(bridge, "bridge.yaml"), "control-plane: { path: x }\n")
	write(t, filepath.Join(bridge, "denylist.txt"), "# nothing yet\n")
	if err := register(bridge); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "roles/coder.md"), "Write simple code.\n")
	runGit(t, repo, "add", "roles/coder.md")
	if err := preCommit(repo, &out); err == nil || !strings.Contains(err.Error(), "empty denylist") {
		t.Fatalf("empty denylist should refuse: %v", err)
	}
	write(t, filepath.Join(bridge, "denylist.txt"), "Zephyrcorp\n")
	if err := preCommit(repo, &out); err != nil {
		t.Fatalf("a clean change was refused: %v", err)
	}
	write(t, filepath.Join(repo, "roles/coder.md"), "Write simple code, as on the zephyrcorp ledger.\n")
	runGit(t, repo, "add", "roles/coder.md")
	if err := preCommit(repo, &out); err == nil || !strings.Contains(err.Error(), "zephyrcorp") {
		t.Fatalf("a denylisted term got through: %v", err)
	}

	// Identity.
	runGit(t, repo, "config", "user.email", "me@work.example")
	if err := preCommit(repo, &out); err == nil || !strings.Contains(err.Error(), "must be authored by me@example.com") {
		t.Fatalf("a work identity got through: %v", err)
	}

	// Attribution.
	msg := filepath.Join(t.TempDir(), "msg")
	write(t, msg, "Fix a role\n\nCo-Authored-By: Claude Opus <noreply@anthropic.com>\n")
	if err := cmdGate([]string{"commit-msg", msg}, &out); err == nil {
		t.Fatalf("an attribution trailer got through")
	}
	write(t, msg, "Fix a role\n\nTighten the coder's scope rules.\n")
	if err := cmdGate([]string{"commit-msg", msg}, &out); err != nil {
		t.Fatalf("a clean message was refused: %v", err)
	}

	// Proposals are checked against every denylist.
	prop := filepath.Join(t.TempDir(), "idea.md")
	write(t, prop, "When ZephyrCorp's ledger tests fail, …\n")
	if err := cmdProposal([]string{"check", prop}, &out); err == nil {
		t.Fatalf("a proposal with a denylisted term passed")
	}
}

func TestNeedsReview(t *testing.T) {
	if !needsReview("+++ b/roles/coder.md\n+x\n") || needsReview("+++ b/cmd/ensign/main.go\n+x\n") || needsReview("+++ b/pipelines/coding.yaml\n+x\n") {
		t.Fatalf("review tiers wrong")
	}
}

func TestCleanClaudeEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_PROJECT_DIR", "/somewhere")
	t.Setenv("ANTHROPIC_MODEL", "keep")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	env := strings.Join(cleanClaudeEnv(), "\n")
	for _, gone := range []string{"CLAUDECODE=", "CLAUDE_CODE_CHILD_SESSION=", "CLAUDE_PROJECT_DIR="} {
		if strings.Contains(env, gone) {
			t.Errorf("%s should be removed", gone)
		}
	}
	if !strings.Contains(env, "ANTHROPIC_MODEL=keep") || !strings.Contains(env, "CLAUDE_CODE_USE_BEDROCK=1") {
		t.Errorf("other variables should be kept")
	}
}
