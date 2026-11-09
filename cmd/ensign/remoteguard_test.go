// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRemoteChangeRefused(t *testing.T) {
	for _, c := range []string{
		// git
		"git push",
		"git push origin main",
		"git push https://example.invalid/supernova.git main",
		"git push $DEST main",
		"git push --dry-run origin main",
		"git -C /work/supernova push",
		"git -c user.name=x --no-pager --git-dir=/x/.git push origin",
		"git --git-dir /x/.git --work-tree /x push",
		"git --attr-source HEAD push origin main",
		"git --attr-source=HEAD push origin",
		"git --no-lazy-fetch push",
		"git --future-option value push origin",
		"git --future-flag push origin",
		"git --future-a x --future-b y push",
		"git --attr-source HEAD lfs push origin",
		"git lfs push origin main",
		"git lfs lock a.bin",
		"git lfs unlock a.bin",
		"git subtree push --prefix=lib lib main",
		"git svn dcommit",
		"git p4 submit",
		"/usr/bin/git push",
		`"C:\Program Files\Git\bin\git.exe" push`,
		`C:\git\git.exe push`,
		"GIT.EXE push",
		// wrappers and separators
		"FOO=1 git push",
		"env -i FOO=1 git push",
		"env -u HOME git push",
		"sudo -u bob git push",
		"command git push",
		"builtin exec git push",
		"nohup git push",
		"nice -n 10 git push",
		"timeout 30 git push",
		"timeout -s KILL 30 git push",
		"echo x | xargs git push",
		"echo x | xargs -I {} git push {}",
		`find . -name x -exec git push \;`,
		"find . -execdir gh pr merge 1 +",
		"if true; then git push; fi",
		"! git push",
		"time git push",
		"ls; git push",
		"ls && git push",
		"ls || git push",
		"ls | git push",
		"ls |& git push",
		"ls & git push",
		"ls\ngit push",
		"git \\\npush",
		"echo $(git push)",
		"echo `git push`",
		`echo "$(git push)"`,
		"echo \"`git push`\"",
		"(git push)",
		"{ git push; }",
		"cat <(git push)",
		"git push 2>&1",
		"git push >/dev/null",
		"cat <<EOF\nbody\nEOF\ngit push",
		"cat <<-EOF\n\tbody\n\tEOF\ngit push",
		"sh -c 'git push'",
		"bash -lc \"git push\"",
		"zsh -c 'cd x && git push'",
		"eval git push",
		"eval 'sh -c \"git push\"'",
		"cmd /c git push",
		`cmd /c "git push"`,
		"powershell -NoProfile -Command \"git push\"",
		"pwsh -c 'gh pr merge 1'",
		"git push 'unterminated",
		"echo \"$(git push",
		// gh
		"gh pr create",
		"gh pr merge 12",
		"gh pr comment 12 -b hi",
		"gh issue create",
		"gh release create v1",
		"gh repo create supernova",
		"gh workflow run ci",
		"gh run rerun 1",
		"gh -R acme/supernova pr merge 1",
		"gh pr --repo acme/supernova close 1",
		"gh.exe pr merge 1",
		"gh sync",
		"gh pr",
		"gh api repos/acme/supernova/issues -f title=x",
		"gh api repos/acme/supernova/issues -F title=x",
		"gh api repos/acme/supernova/issues -ftitle=x",
		"gh api repos/acme/supernova/issues --field title=x",
		"gh api repos/acme/supernova/issues --raw-field title=x",
		"gh api repos/acme/supernova/issues --input body.json",
		"gh api -X POST repos/acme/supernova/issues",
		"gh api -XPOST repos/acme/supernova/issues",
		"gh api --method POST repos/acme/supernova/issues",
		"gh api --method=delete repos/acme/supernova/issues/1",
		"gh api repos/acme/supernova -X PATCH",
		"gh api graphql -f query='mutation { x }'",
		"gh api graphql --input q.json",
		// curl and wget
		"curl -X POST https://example.invalid",
		"curl -XPUT https://example.invalid",
		"curl --request DELETE https://example.invalid",
		"curl --request=POST https://example.invalid",
		"curl -d a=b https://example.invalid",
		"curl --data-binary @f https://example.invalid",
		"curl --data-raw x https://example.invalid",
		"curl --json '{}' https://example.invalid",
		"curl -F a=b https://example.invalid",
		"curl --form-string a=b https://example.invalid",
		"curl -T f https://example.invalid",
		"curl --upload-file f https://example.invalid",
		"curl -sXPOST https://example.invalid",
		"curl -sSd x https://example.invalid",
		"curl -s -o out -d x https://example.invalid",
		"wget --post-data=a=b https://example.invalid",
		"wget --post-file f https://example.invalid",
		"wget --body-data x https://example.invalid",
		"wget --body-file=f https://example.invalid",
		"wget --method=POST https://example.invalid",
		"wget --method PUT https://example.invalid",
		// ensign
		"ensign github-pr merge",
		".starship/bin/ensign github-pr pull-request",
		"ensign journal push",
		"ensign journal commit",
		"ensign update",
		"ensign run supernova",
		"ensign run-stage",
		"ensign resume",
		"ensign sequence release-run",
		"ENSIGN.EXE update",
		// registries
		"npm publish",
		"pnpm publish",
		"yarn publish",
		"npm --registry=x publish",
		"cargo publish",
		"docker push acme/supernova",
		"twine upload dist/*",
		"gem push supernova.gem",
		// positional beats --repo, shells with option values, subtree options, more publishes, paths . and ..
		"git push --repo=/tmp/x origin main",
		"bash -o pipefail -c 'git push'",
		"bash -euo pipefail -c 'git push'",
		"bash -ceo pipefail 'git push'",
		"bash -c -o pipefail 'git push'",
		"bash -eoc pipefail 'git push'",
		"bash -oe pipefail -c 'git push'",
		"bash +x -c 'git push'",
		"bash +o history -c 'git push'",
		"ensign run --help",
		"ensign run-stage --help",
		"ensign journal commit -m -h",
		"git subtree -P lib push origin main",
		"git subtree --prefix=lib push origin main",
		"git subtree --prefix lib push origin main",
		"yarn npm publish",
		"python -m twine upload dist/*",
		"python3 -m twine upload dist/*",
		"py -m twine upload dist/*",
		"docker image push acme/supernova",
		"gh repo deploy-key add key.pub",
		"gh repo deploy-key delete 1",
		"gh repo license create",
		"cmd //c git push",
		"CMD //K git push",
		"cmd /k git push",
	} {
		if remoteChange(c) == "" {
			t.Errorf("not refused: %q", c)
		}
	}
}

func TestRemoteChangePasses(t *testing.T) {
	for _, c := range []string{
		"",
		"ls -la",
		"bash script.sh -c 'git push'",
		"bash +c script.sh git",
		"bash --norc script.sh git",
		"echo \"git push\"",
		"echo 'git push'",
		"grep -rn \"git push\" .",
		"git log --grep 'push'",
		"rg 'gh pr merge'",
		"git show HEAD:push",
		"git log push",
		"git --attr-source HEAD status",
		"git --attr-source push log",
		"git --no-pager log push",
		"git --bare status",
		"git --attr-source HEAD push /tmp/supernova.git",
		"git --attr-source",
		"ls push",
		"cat <<EOF\ngit push\nEOF",
		"cat <<-EOF\n\tgit push\n\tEOF",
		"cat <<'EOF'\ngit push\nEOF",
		"cat <<\"EOF\"\ngit push\nEOF\nls",
		"cat <<EOF | grep x\ngit push origin\nEOF",
		"# git push",
		"ls # git push",
		"echo a#b",
		"git status\n# git push\nls",
		"command -v git push",
		"git fetch origin",
		"git pull",
		"git ls-remote origin",
		"git remote -v",
		"git config remote.origin.url",
		"git diff HEAD~1",
		"git lfs ls-files",
		// local-path pushes, for scratch repositories
		"git push /tmp/supernova-scratch/remote.git main",
		"git push ./remote.git main",
		"git push ../remote.git",
		"git push ~/remote.git",
		"git push file:///tmp/remote.git main",
		"git push --repo=/tmp/remote.git",
		"git push --repo /tmp/remote.git",
		"git push -u /tmp/remote.git main",
		`git push C:\Temp\remote.git main`,
		"git push C:/Temp/remote.git main",
		// gh reads
		"gh pr list",
		"gh pr view 1",
		"gh pr status",
		"gh pr diff 1",
		"gh pr checks 1",
		"gh run watch 1",
		"gh run download 1",
		"gh release list",
		"gh attestation verify x",
		"gh search issues supernova",
		"gh status",
		"gh browse",
		"gh help",
		"gh version",
		"gh completion -s bash",
		"gh config get editor",
		"gh alias list",
		"gh auth status",
		"gh extension list",
		"gh repo clone acme/supernova",
		"gh pr checkout 1",
		"gh --version",
		"gh --help",
		"gh",
		"gh -R acme/supernova pr view 1",
		"gh api repos/acme/supernova/pulls",
		"gh api repos/acme/supernova/issues -X GET",
		"gh api repos/acme/supernova/issues -XGET -f state=open",
		"gh api --method head repos/acme/supernova",
		"gh api graphql -f query='query { viewer { login } }'",
		// curl and wget reads
		"curl https://example.invalid",
		"curl -sSL -o out https://example.invalid",
		"curl -X GET https://example.invalid",
		"curl -X HEAD https://example.invalid",
		"curl -I https://example.invalid",
		"curl -G -d a=b https://example.invalid",
		"curl --get --data-urlencode a=b https://example.invalid",
		"curl --url-query a=b https://example.invalid",
		"curl -oXfile https://example.invalid",
		"curl -H 'X: y' https://example.invalid",
		"wget https://example.invalid",
		"wget --method=GET https://example.invalid",
		"wget --method HEAD https://example.invalid",
		// ensign and registries
		"ensign help run",
		"ensign task list",
		"ensign sequence publish",
		"ensign sequence release",
		"ensign journal status",
		"npm install",
		"npm view supernova",
		"cargo build",
		"docker ps",
		"gem list",
		// help, more gh reads, paths . and ..
		"git push --help",
		"git push -h",
		"gh pr merge --help",
		"gh pr --help",
		"npm publish --help",
		"gh run get 1",
		"gh pr check 1",
		"gh cache dummy-list",
		"gh extension search supernova",
		"gh repo deploy-key list",
		"gh repo gitignore list",
		"gh repo gitignore view Go",
		"gh repo license list",
		"gh repo license view mit",
		"git push . main",
		"git push .. main",
		"git push --repo=origin ../x main",
	} {
		if got := remoteChange(c); got != "" {
			t.Errorf("refused %q: %q", c, got)
		}
	}
}

func TestRemoteChangeBounds(t *testing.T) {
	deep := "git push"
	for i := 0; i < shellDepthLimit+2; i++ {
		deep = "sh -c '" + strings.ReplaceAll(deep, "'", `'\''`) + "'"
	}
	if got := remoteChange(deep); got != "" {
		t.Errorf("recursion past the limit refused: %q", got)
	}
	if got := remoteChange("git push origin main"); got != "git push origin main" {
		t.Errorf("label = %q", got)
	}
	long := remoteChange("git push " + strings.Repeat("x", 300))
	if len(long) > 120 || !strings.HasSuffix(long, "...") {
		t.Errorf("long label not cut: %d", len(long))
	}
	for _, c := range []string{"git push \\", "git push <<", "git push <<EOF", "echo \"git", "echo `git push", "echo $(git push", "git push 2>", "(", "$(", "<(git push"} {
		remoteChange(c)
	}
}

func bashHook(t *testing.T, env bool, agentID, agentType, input string) (denied bool, reason string) {
	t.Helper()
	if !env {
		t.Setenv("CLAUDE_PROJECT_DIR", "")
	}
	data := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/nonexistent/supernova","tool_name":"Bash","tool_input":` + input
	data += `,"agent_id":` + jsonStr(agentID) + `,"agent_type":` + jsonStr(agentType) + `}`
	var out, errb bytes.Buffer
	if code := cmdHook("PreToolUse", strings.NewReader(data), &out, &errb); code != 0 {
		t.Fatalf("PreToolUse exited %d: %s", code, errb.String())
	}
	if out.Len() == 0 {
		return false, ""
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return dig(res, "hookSpecificOutput", "permissionDecision") == "deny", dig(res, "hookSpecificOutput", "permissionDecisionReason")
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestBashHookRefusesRemoteChangesForReadOnlyRoles(t *testing.T) {
	push := `{"command":"git push origin main"}`
	for _, role := range []string{"explorer", "reviewer"} {
		for _, env := range []bool{true, false} {
			denied, reason := bashHook(t, env, "a1", role, push)
			if !denied {
				t.Errorf("%s (env %v) not denied", role, env)
			}
			if !strings.HasPrefix(reason, "starship: ") || !strings.Contains(reason, "git push origin main") || !strings.Contains(reason, "scratch repository") {
				t.Errorf("reason = %q", reason)
			}
		}
		if denied, _ := bashHook(t, false, "a1", role, `{"command":"git log --oneline"}`); denied {
			t.Errorf("%s: git log denied", role)
		}
		if denied, _ := bashHook(t, false, "a1", role, `{"command":"git push /tmp/supernova/remote.git main"}`); denied {
			t.Errorf("%s: push to a local path denied", role)
		}
		for _, input := range []string{`{}`, `{"command":42}`, `{"command":null}`} {
			if denied, _ := bashHook(t, false, "a1", role, input); denied {
				t.Errorf("%s: %s denied", role, input)
			}
		}
	}
	for _, c := range []struct{ id, role string }{{"a1", "coder"}, {"a1", "tester"}, {"a1", "orchestrator"}, {"a1", "supernova-scout"}, {"a1", ""}, {"", "explorer"}, {"", "reviewer"}, {"", ""}} {
		if denied, _ := bashHook(t, false, c.id, c.role, push); denied {
			t.Errorf("agent %q role %q denied", c.id, c.role)
		}
	}
}
