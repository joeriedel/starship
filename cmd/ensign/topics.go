package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// topicNames lists the help topics linked into the bridge, sorted.
func topicNames(bridge string) ([]string, error) {
	dir := filepath.Join(bridge, ".starship", "topics")
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("no help topics in this bridge; run ensign link")
	}
	found, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	var names []string
	for _, f := range found {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".md"))
	}
	sort.Strings(names)
	return names, nil
}

// cmdHelp prints one help topic, or lists the topics.
func cmdHelp(args []string, out io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: ensign help [topic]")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	names, err := topicNames(bridge)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(out, strings.Join(names, "\n"))
		return nil
	}
	if !contains(names, args[0]) {
		return fmt.Errorf("unknown help topic %q; topics: %s", args[0], strings.Join(names, ", "))
	}
	data, err := os.ReadFile(filepath.Join(bridge, ".starship", "topics", args[0]+".md"))
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// deliverTopic prints a help topic to out the first time a session runs a command it belongs to.
func deliverTopic(bridge, name string, out io.Writer) {
	session := os.Getenv("STARSHIP_SESSION")
	marker := sessionFile(bridge, session) + ".topic-" + name
	if session != "" {
		if _, err := os.Stat(marker); err == nil {
			return
		}
	}
	data, err := os.ReadFile(filepath.Join(bridge, ".starship", "topics", name+".md"))
	if err != nil {
		fmt.Fprintf(out, "The %s help topic is missing; run ensign link.\n\n", name)
		return
	}
	fmt.Fprintf(out, "%s\n\n", strings.TrimRight(string(data), "\r\n"))
	if session != "" {
		writeFile(marker, "")
	}
}
