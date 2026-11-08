// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// probeHost is the place the selftest's probe sessions run in: one temp folder holding every probe project and the home and Claude Code config the sessions see, and the stand-in Messages API they talk to. Nothing in the user's real home, config or credentials is read, written or removed, and a session never reaches a real provider.
type probeHost struct {
	root string
	mock *messagesMock
	env  []string
}

// newProbeHost makes the temp root, with an empty home and config, and starts the stand-in API.
func newProbeHost() (*probeHost, error) {
	root, err := os.MkdirTemp("", "ensign-selftest-")
	if err != nil {
		return nil, err
	}
	mock, err := startMessagesMock(nil)
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	h := &probeHost{root: root, mock: mock, env: isolatedClaudeEnv(os.Environ(), root, mock.URL)}
	if err := os.MkdirAll(filepath.Join(root, "home", ".claude"), 0o755); err != nil {
		h.close(io.Discard)
		return nil, err
	}
	return h, nil
}

// close stops the stand-in API and removes the root, which holds the probe folders, the isolated config and the session records in it. A removal that fails is noted on out. A process of a session that outlived it can hold a folder open on Windows, so the removal retries for a while.
func (h *probeHost) close(out io.Writer) {
	h.mock.Close()
	if err := retry(5*time.Second, isTransientFileError, func() error { return os.RemoveAll(h.root) }); err != nil {
		fmt.Fprintf(out, "could not remove the selftest folder %s: %v\n", h.root, err)
	}
}

// run is runProbeSession in the isolated environment.
func (h *probeHost) run(dir, prompt string, claudeArgs ...string) ([]map[string]any, string, string, error) {
	return runProbeSession(h.env, dir, prompt, claudeArgs...)
}

// dummyAPIKey is the key the probe sessions carry; the stand-in API ignores it.
const dummyAPIKey = "sk-ant-selftest-dummy-key"

// credentialPrefixes start the names of the variables that pick a provider or carry a credential: the API key and auth token, a gateway or model override (ANTHROPIC_), Bedrock, Vertex and Foundry selection (CLAUDE_CODE_USE_), the OAuth token (CLAUDE_CODE_OAUTH), skipping a provider's auth (CLAUDE_CODE_SKIP_), the key helper's settings (CLAUDE_CODE_API_KEY) and the Bedrock bearer token. A model override is stripped too: the stand-in answers to any model name, and the probes pass --model themselves.
var credentialPrefixes = []string{"ANTHROPIC_", "CLAUDE_CODE_USE_", "CLAUDE_CODE_OAUTH", "CLAUDE_CODE_SKIP_", "CLAUDE_CODE_API_KEY", "AWS_BEARER_TOKEN"}

// isolatedClaudeEnv is base as the probe sessions see it: without the variables a running session sets about itself or that could route to a real credential or provider, with a home and config folder inside root, the stand-in API as the only API, and a dummy key. Windows and the XDG-aware tools look elsewhere than HOME, so those folders are inside root too. Nothing is copied from the real config; the paths in base that are replaced are never opened.
func isolatedClaudeEnv(base []string, root, baseURL string) []string {
	home := filepath.Join(root, "home")
	set := map[string]string{
		"HOME":               home,
		"USERPROFILE":        home,
		"APPDATA":            filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA":       filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"XDG_DATA_HOME":      filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":     filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":     filepath.Join(home, ".cache"),
		"CLAUDE_CONFIG_DIR":  filepath.Join(home, ".claude"),
		"ANTHROPIC_BASE_URL": baseURL,
		"ANTHROPIC_API_KEY":  dummyAPIKey,
		"NO_PROXY":           "127.0.0.1,localhost",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_AUTO_MODE_SERVER":             "0",
	}
	var env []string
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		if sessionVars[name] || upper == "NO_PROXY" || isCredentialVar(upper) {
			continue
		}
		if _, replaced := set[upper]; replaced {
			continue
		}
		env = append(env, kv)
	}
	for name, v := range set {
		env = append(env, name+"="+v)
	}
	if runtime.GOOS != "windows" {
		env = append(env, "no_proxy=127.0.0.1,localhost")
	}
	return env
}

func isCredentialVar(upper string) bool {
	for _, p := range credentialPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}
