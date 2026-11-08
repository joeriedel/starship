// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"path/filepath"
	"runtime"
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

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestIsolatedClaudeEnv(t *testing.T) {
	root := filepath.Join("supernova", "root")
	base := []string{
		"PATH=/usr/bin", "LANG=C", "HTTPS_PROXY=http://proxy.supernova.test:3128",
		"HOME=/home/supernova-real", "USERPROFILE=C:/Users/supernova-real", "APPDATA=C:/Users/supernova-real/AppData/Roaming",
		"CLAUDE_CONFIG_DIR=/home/supernova-real/.claude", "XDG_CONFIG_HOME=/home/supernova-real/.config",
		"ANTHROPIC_API_KEY=sk-real", "ANTHROPIC_AUTH_TOKEN=real-token", "ANTHROPIC_BASE_URL=https://gateway.supernova.test",
		"ANTHROPIC_MODEL=real-model", "ANTHROPIC_DEFAULT_HAIKU_MODEL=real-haiku", "anthropic_custom_headers=x: y",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=1",
		"AWS_BEARER_TOKEN_BEDROCK=bearer", "CLAUDECODE=1", "CLAUDE_ENV_FILE=/x", "NO_PROXY=elsewhere.test", "no_proxy=elsewhere.test", "CLAUDE_CODE_GIT_BASH_PATH=C:/git/bash.exe",
	}
	env := isolatedClaudeEnv(base, root, "http://127.0.0.1:9")
	got := envMap(env)
	home := filepath.Join(root, "home")
	for k, want := range map[string]string{
		"HOME": home, "USERPROFILE": home, "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"APPDATA": filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "ANTHROPIC_BASE_URL": "http://127.0.0.1:9", "ANTHROPIC_API_KEY": dummyAPIKey,
		"CLAUDE_CODE_AUTO_MODE_SERVER": "0", "PATH": "/usr/bin", "LANG": "C", "HTTPS_PROXY": "http://proxy.supernova.test:3128",
		"CLAUDE_CODE_GIT_BASH_PATH": "C:/git/bash.exe",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if !strings.Contains(got["NO_PROXY"], "127.0.0.1") || strings.Contains(got["NO_PROXY"], "elsewhere") {
		t.Errorf("NO_PROXY = %q", got["NO_PROXY"])
	}
	if runtime.GOOS != "windows" && (!strings.Contains(got["no_proxy"], "127.0.0.1") || strings.Contains(got["no_proxy"], "elsewhere")) {
		t.Errorf("no_proxy = %q", got["no_proxy"])
	}
	for _, gone := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "anthropic_custom_headers", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "AWS_BEARER_TOKEN_BEDROCK", "CLAUDECODE", "CLAUDE_ENV_FILE"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s should be removed", gone)
		}
	}
	counts := map[string]int{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		counts[k]++
	}
	for k, n := range counts {
		if n > 1 {
			t.Errorf("%s is set %d times", k, n)
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, "supernova-real") || strings.Contains(kv, "sk-real") {
			t.Errorf("a real value survived: %s", kv)
		}
	}
}
