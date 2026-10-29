//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// execReplace runs another program in this one's place and exits with its code; Windows
// has no exec, so this process waits for it.
func execReplace(path string, args []string) {
	cmd := exec.Command(path, args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "starship: could not hand off to", path+":", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// ensureOnPath adds dir to the user's PATH in the registry. New processes see it.
func ensureOnPath(dir string) (string, error) {
	if os.Getenv("STARSHIP_BIN_DIR") != "" {
		return "", nil // tests: never touch the real user PATH
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	cur, _, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return "", err
	}
	for _, p := range strings.Split(cur, ";") {
		if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
			return "", nil
		}
	}
	next := dir
	if cur != "" {
		next = strings.TrimRight(cur, ";") + ";" + dir
	}
	if err := k.SetExpandStringValue("Path", next); err != nil {
		return "", err
	}
	return fmt.Sprintf("added %s to your user PATH; open a new terminal for it to take effect", dir), nil
}
