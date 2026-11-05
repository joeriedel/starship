// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const copyrightHeader = "// Copyright (c) 2026 Joseph Riedel\n// Licensed under the MIT License. See LICENSE.\n\n"

func TestEveryGoFileCarriesTheCopyrightHeader(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		seen++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(data), copyrightHeader) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatalf("no Go files found under %s", root)
	}
	if len(offenders) > 0 {
		t.Errorf("%d Go files lack the copyright header; each must begin with these lines and a blank line:\n%s\n%s", len(offenders), copyrightHeader, strings.Join(offenders, "\n"))
	}
}
