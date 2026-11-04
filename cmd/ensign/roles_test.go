package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestRoleAndBriefDocumentationRules(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	doc := read("roles/documentation.md")
	for _, re := range []string{
		`(?m)^name: documentation$`,
		`(?m)^tools: .*\bWrite\b.*\bEdit\b|(?m)^tools: .*\bEdit\b.*\bWrite\b`,
		`(?i)change only paths declared in .@docs.`,
		`(?i)never survey the codebase`,
		`(?i)built from the task's own record`,
		`(?i)check the declared docs against what the run changed`,
		`(?i)edits are neither tested nor reviewed`,
		`(?i)report .updated. when you changed docs and .unchanged. when they were already accurate`,
		`(?i)report .blocked. only when you could not do the check at all`,
		`(?i)are not .blocked.: report them alongside`,
		`(?i)the .files. line lists the docs you changed`,
	} {
		if !regexp.MustCompile(re).MatchString(doc) {
			t.Errorf("roles/documentation.md does not match %q", re)
		}
	}
	checks := map[string][]string{
		"roles/coder.md":               {`(?i)write code, not documentation`, `(?i)don't change paths declared in .@docs.`},
		"roles/reviewer.md":            {`(?i)review code only, not documentation`, `(?i)even when it is in the diff`},
		"briefs/review/correctness.md": {`(?i)documentation .*isn't reviewed`, `(?i)even when it is in the diff`},
		"briefs/review/public.md":      {`(?i)documentation .*isn't reviewed`, `(?i)even when it is in the diff`},
		"roles/orchestrator.md":        {`(?i)documentation isn't reviewed`, `(?i)even when it is in the diff`},
	}
	for rel, res := range checks {
		body := read(rel)
		for _, re := range res {
			if !regexp.MustCompile(re).MatchString(body) {
				t.Errorf("%s does not match %q", rel, re)
			}
		}
	}
}
