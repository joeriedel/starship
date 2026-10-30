// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

// Command ensign is starship's resolver, hook driver and CLI.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// version and releaseRepo are set at release build time with -ldflags
// "-X main.version=… -X main.releaseRepo=…": the release tag, and the repo it came from.
var (
	version     = "dev"
	releaseRepo = ""
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:], os.Stdout)
	case "next":
		err = cmdNext(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:], os.Stdout)
	case "link":
		var bridge string
		if bridge, err = cwdBridge(); err == nil {
			err = link(bridge, len(os.Args) > 2 && os.Args[2] == "--force", os.Stdout)
		}
	case "project":
		err = cmdProject(os.Args[2:], os.Stdout)
	case "task":
		err = cmdTask(os.Args[2:], os.Stdout)
	case "journal":
		err = cmdJournal(os.Args[2:], os.Stdout)
	case "workspace":
		err = cmdWorkspace(os.Args[2:], os.Stdout)
	case "github-pr":
		err = cmdGithubPR(os.Args[2:], os.Stdout)
	case "git-catalog":
		err = cmdCatalog(os.Args[2:], os.Stdout)
	case "resume":
		err = cmdResume(os.Args[2:], os.Stdout)
	case "selftest":
		err = cmdSelftest(os.Stdout)
	case "selftest-hook":
		if len(os.Args) == 3 {
			os.Exit(cmdSelftestHook(os.Args[2], os.Stdin, os.Stdout))
		}
	case "style":
		err = cmdStyle(os.Args[2:], os.Stdout)
	case "skill":
		err = cmdSkill(os.Args[2:], os.Stdout)
	case "setup":
		err = cmdSetup(os.Args[2:], os.Stdout)
	case "update":
		err = cmdUpdate(os.Args[2:], os.Stdout)
	case "version", "--version":
		fmt.Println("ensign", version)
	case "hook":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		os.Exit(cmdHook(os.Args[2], os.Stdin, os.Stdout, os.Stderr))
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ensign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ensign init <dir> (--control-plane <path> | --remote <url> --tag <tag>)
  ensign setup
  ensign workspace <project>[/<task>] [<path>]
  ensign update [<tag>]
  ensign link [--force]
  ensign selftest
  ensign project new <name>
  ensign task new <project>/<task> --workspace <path> [--branch <branch>]
  ensign task open <project>/<task>
  ensign run <project>/<task> [--start <stage>] [--sync] [--dry-run]
  ensign run --pipeline <name> [--project <p>] [--rebuild] [--dry-run]
  ensign style ingest <project> <path-or-url> --trust authoritative|exemplar [--note <text>]
  ensign style report <project>
  ensign skill new <name> --description <text> | ensign skill discard <name>
  ensign resume --answer <text> [--budget <name>=<n>] [--run <id>]
  ensign journal commit -m <message> [files…] | ensign journal push
  ensign git-catalog squash <project>/<task> -m <message>
  ensign github-pr pull-request|merge <project>/<task>
  ensign next [--bridge <dir>] [--run <id>]
  ensign hook <SessionStart|UserPromptSubmit|PreToolUse|PostToolUse|SubagentStop|Stop>`)
}

// cmdHook runs one hook event and returns the exit code for Claude Code.
// Internal errors exit 2, which Claude Code treats as blocking: ensign fails closed.
func cmdHook(event string, stdin io.Reader, stdout, stderr io.Writer) int {
	var in HookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		fmt.Fprintln(stderr, "starship: ensign could not read the hook input:", err)
		return 2
	}
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		dir = in.Cwd
	}
	bridge, err := findBridge(dir)
	if err != nil {
		return 0 // outside a bridge, hook events are no-ops
	}
	switch event {
	case "SessionStart":
		return hookSessionStart(&in, bridge, stdout, stderr)
	case "UserPromptSubmit":
		if text := sessionWord(bridge, in.SessionID, in.Prompt); text != "" {
			// continue:false, not decision:block: the prompt is kept as sent and the input
			// box clears, where a block leaves the word in the box under an "operation
			// blocked" warning. The text starts on its own line, under the harness's label.
			json.NewEncoder(stdout).Encode(map[string]any{"continue": false, "stopReason": "\n" + text})
			return 0
		}
	case "PreToolUse":
		if in.ToolName != "Agent" && in.ToolName != "Task" && in.ToolName != "SubagentHandback" {
			if why := controlPlaneEdit(bridge, &in); why != "" {
				json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
					"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "starship: " + why}})
			}
			return 0
		}
	case "PostToolUse":
		// Only a background launch is recorded; taking the run lock for any other call
		// would make every agent's Bash wait on whichever hook holds it.
		if backgroundLaunch(&in) == "" {
			return 0
		}
	case "Stop":
		if out := hookStop(bridge, &in); out != nil {
			json.NewEncoder(stdout).Encode(out)
		}
		return 0
	}
	r, err := openSessionRun(bridge, in.SessionID)
	if err != nil {
		return failHook(&in, nil, err, stderr)
	}
	if r == nil && event == "UserPromptSubmit" && strings.TrimSpace(in.Prompt) == "go" {
		if r, err = claimRun(bridge, in.SessionID); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	} else if r == nil {
		return 0
	}
	if r != nil {
		defer r.close()
		if r.State.Transcript == "" && in.TranscriptPath != "" {
			r.State.Transcript = in.TranscriptPath
		}
	}

	wasTerminal := r != nil && r.State.terminal()
	var out any
	switch event {
	case "PreToolUse":
		out, err = hookPreToolUse(&in, r)
	case "PostToolUse":
		out, err = hookPostToolUse(&in, r)
	case "SubagentStop":
		out, err = hookSubagentStop(&in, r)
	case "UserPromptSubmit":
		if strings.TrimSpace(in.Prompt) == "go" {
			out, err = hookUserPromptSubmit(&in, r)
		}
	default:
		err = fmt.Errorf("unknown hook event %q", event)
	}
	if err != nil {
		return failHook(&in, r, err, stderr)
	}
	if r != nil && !wasTerminal && r.State.terminal() {
		finishRun(r)
	}
	if r != nil {
		if err := r.save(); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	}
	if out != nil {
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	}
	return 0
}

// failHook ends the run as error: ensign, and blocks the action. A stop that is already
// being forced to continue is let through, so a persistent fault can't trap an agent.
func failHook(in *HookInput, r *RunFiles, err error, stderr io.Writer) int {
	if r != nil && !r.State.terminal() {
		m := &machine{snap: r.Snap, st: r.State}
		m.fail("ensign", err.Error())
		r.save()
	}
	fmt.Fprintln(stderr, "starship: ensign error, the run has stopped:", err)
	if in.StopHookActive {
		return 0
	}
	return 2
}

// hookSessionStart gives the session's commands its id, so `ensign run` can bind the run
// to this session, and prints the heartbeat the bridge's CLAUDE.md checks for.
func hookSessionStart(in *HookInput, bridge string, stdout, stderr io.Writer) int {
	if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" && in.SessionID != "" {
		env, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			// The session's own commands run the bridge's ensign, found on PATH.
			_, err = fmt.Fprintf(env, "export STARSHIP_SESSION=%s\nexport PATH=%s:\"$PATH\"\n",
				in.SessionID, shellQuote(filepath.ToSlash(filepath.Join(bridge, ".starship", "bin"))))
			env.Close()
		}
		if err != nil {
			fmt.Fprintln(stderr, "starship: could not record the session id:", err)
			return 2
		}
	}
	// The reply is JSON: what Claude reads goes in additionalContext, and one line for
	// the user goes in systemMessage, shown at startup without calling the model.
	reply := func(show, context string) int {
		json.NewEncoder(stdout).Encode(map[string]any{
			"systemMessage":      show,
			"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": context},
		})
		return 0
	}
	bf, err := loadBridgeFile(bridge)
	if err == nil {
		_, err = controlPlaneDir(bridge, bf)
	}
	if err != nil {
		return reply("Starship is not ready in this bridge. Ask Claude to set it up.",
			fmt.Sprintf("starship: this bridge is not ready: %v", err))
	}
	var ctx strings.Builder
	if in.SessionID != "" {
		writeFile(sessionFile(bridge, in.SessionID)+".pin", pinnedCommit(bridge)+"\n")
	}
	if note := syncBridge(bridge); note != "" {
		fmt.Fprintln(&ctx, note)
	}
	if note := migrateWorkspaces(bridge); note != "" {
		fmt.Fprintln(&ctx, note)
	}
	if linkStale(bridge) && len(liveRuns(bridge)) == 0 {
		if err := link(bridge, false, nil); err != nil {
			return reply("Starship is not ready in this bridge. Ask Claude to set it up.",
				fmt.Sprintf("starship: this bridge is not ready: %v", err))
		}
	}
	dropSessionImport(bridge)
	upgradeHookSettings(bridge)
	fmt.Fprintf(&ctx, "STARSHIP-READY ensign %s bridge %s\n", version, bridge)
	// The manual goes to the session through this hook, not CLAUDE.md, because Claude
	// Code gives CLAUDE.md to every subagent too. SessionStart also fires after a
	// compaction, so the manual comes back when the context is summarised.
	manual, err := os.ReadFile(filepath.Join(bridge, ".starship", "session.md"))
	if err != nil {
		fmt.Fprintln(&ctx, "starship: the session manual is missing; run ensign link")
		return reply("Starship is not ready in this bridge. Ask Claude to set it up.", ctx.String())
	}
	fmt.Fprintf(&ctx, "\n%s", manual)
	if cmds := requestCommands(bridge); cmds != "" {
		fmt.Fprintf(&ctx, "\n%s", cmds)
	}
	show := "Starship ready · bridge " + filepath.Base(bridge)
	if version != "" && version != "dev" {
		show += " · " + version
	}
	return reply(show, ctx.String())
}

// beforeHandback is settings as ensign wrote them before it registered PostToolUse and
// matched SubagentHandback.
func beforeHandback(settings string) string {
	i := strings.Index(settings, `    "PostToolUse"`)
	j := strings.Index(settings[i:], "\n")
	settings = settings[:i] + settings[i+j+1:]
	return strings.Replace(settings, "|SubagentHandback", "", 1)
}

// upgradeHookSettings brings a bridge's settings.json up to date (a timeout on the
// PreToolUse hook, the PostToolUse hook, SubagentHandback) when it is exactly what an
// older ensign wrote, and commits that. A settings file someone has edited is left alone.
func upgradeHookSettings(bridge string) {
	f := filepath.Join(bridge, ".claude", "settings.json")
	data, err := os.ReadFile(f)
	if err != nil || (string(data) != hookSettingsWith(0) && string(data) != beforeHandback(hookSettingsWith(0)) &&
		string(data) != beforeHandback(hookSettings())) {
		return
	}
	if os.WriteFile(f, []byte(hookSettings()), 0o644) == nil && isGit(bridge) {
		git(bridge, "commit", "-q", "-m", "Update the starship hook settings", "--", ".claude/settings.json")
	}
}

// dropSessionImport removes the manual's import from a bridge's CLAUDE.md, left there by
// older versions of ensign init, and commits that. A CLAUDE.md with someone's uncommitted
// edits is left alone.
func dropSessionImport(bridge string) {
	f := filepath.Join(bridge, "CLAUDE.md")
	data, err := os.ReadFile(f)
	if err != nil || !strings.Contains(string(data), sessionImport) {
		return
	}
	tracked := isGit(bridge)
	if tracked {
		if dirty, _ := gitOut(bridge, "status", "--porcelain", "--", "CLAUDE.md"); dirty != "" {
			return
		}
	}
	var keep []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != sessionImport {
			keep = append(keep, line)
		}
	}
	if os.WriteFile(f, []byte(strings.Join(keep, "\n")), 0o644) == nil && tracked {
		git(bridge, "commit", "-q", "-m", "Load the session manual from the SessionStart hook", "--", "CLAUDE.md")
	}
}
