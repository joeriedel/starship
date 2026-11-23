package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// readerFactsMigrationHow is the instruction to run the facts migration, shared by the session-start line, the dispatch refusal, the gather and the linked-item look-up.
const readerFactsMigrationHow = "dispatch the reader with a brief that says it is a facts migration, to move reader/facts.md and reader/history.md into reader/facts/ (one file per subject)"

// readerFactsMigrated reports whether the reader's facts are in one file per subject under reader/facts/. An old file holding only whitespace is removed first, since the reader can't delete; the facts are migrated exactly when reader/facts.md does not exist. Nothing else records the state.
func readerFactsMigrated(bridge string) (bool, error) {
	dir := filepath.Join(bridge, "reader")
	for _, name := range []string{"history.md", "facts.md"} {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(data)) == "" {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
		}
	}
	_, err := os.Stat(filepath.Join(dir, "facts.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// readerFactsPending reports whether the bridge declares the reader and its facts are still in the old layout.
func readerFactsPending(bridge string) (bool, error) {
	declared, err := readerDeclared(bridge)
	if err != nil || !declared {
		return false, err
	}
	migrated, err := readerFactsMigrated(bridge)
	return !migrated, err
}

// readerFactsRefusal is the reason a dispatch of the reader is refused while the facts are not migrated, or "" when it is allowed. Only a prompt that says it is a facts migration passes.
func readerFactsRefusal(bridge string, in *HookInput) string {
	if sub, _ := in.ToolInput["subagent_type"].(string); sub != "reader" {
		return ""
	}
	if pending, err := readerFactsPending(bridge); err != nil || !pending {
		return ""
	}
	if prompt, _ := in.ToolInput["prompt"].(string); strings.Contains(strings.ToLower(prompt), "facts migration") {
		return ""
	}
	return "the reader's facts are still in the old layout, so run the facts migration first: " + readerFactsMigrationHow
}

// readerFactsLine is the session-start line while the facts are in the old layout.
func readerFactsLine() string {
	return fmt.Sprintf("starship: the reader's facts are in the old layout. Before anything else, %s. Until then ensign refuses gathers and linked-item look-ups.", readerFactsMigrationHow)
}
