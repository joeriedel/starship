package main

import (
	"strings"
	"testing"
)

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
