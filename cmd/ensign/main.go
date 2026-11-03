// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

// Command ensign is starship's resolver, hook driver and CLI.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
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
	case "help":
		err = cmdHelp(os.Args[2:], os.Stdout)
	case "sequence":
		err = cmdSequence(os.Args[2:], os.Stdout)
	case "next":
		err = cmdNext(os.Args[2:], os.Stdout)
	case "dependency":
		err = cmdDependency(os.Args[2:], os.Stdout)
	case "run-stage":
		err = cmdRunStage(os.Args[2:], os.Stdout)
	case "init":
		err = cmdInit(os.Args[2:], os.Stdout)
	case "link":
		err = cmdLink(os.Args[2:], os.Stdout)
	case "project":
		err = cmdProject(os.Args[2:], os.Stdout)
	case "task":
		err = cmdTask(os.Args[2:], os.Stdout)
	case "journal":
		err = cmdJournal(os.Args[2:], os.Stdout)
	case "workspace":
		err = cmdWorkspace(os.Args[2:], os.Stdout)
	case "env":
		err = cmdEnv(os.Args[2:], os.Stdout)
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
	case "gc":
		err = cmdGC(os.Args[2:], os.Stdout)
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

// cmdLink implements `ensign link`. --migrate also moves the workspaces to the current layout first; update and setup pass it to the ensign they just fetched.
func cmdLink(args []string, out io.Writer) error {
	force, migrate := false, false
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		case "--migrate":
			migrate = true
		default:
			return fmt.Errorf("usage: ensign link [--force] [--migrate]")
		}
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	if migrate {
		if note := migrateWorkspaces(bridge); note != "" {
			fmt.Fprintln(out, note)
		}
	}
	return link(bridge, force, out)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ensign init <dir> (--control-plane <path> | --remote <url> --tag <tag>)
  ensign setup
  ensign workspace <project>[/<task>] [<path>]
  ensign env <project> [NAME=<value> | --path <folder> | --drop-path <folder>]
  ensign update [<tag>]
  ensign gc [<keep>]
  ensign link [--force] [--migrate]
  ensign selftest
  ensign help [topic]
  ensign project new <name> --workspace <path> | --locations <a,b> [--platforms <list>]
  ensign project reviewed <project>[/<task>]
  ensign task new <project>/<task> --workspace <path> [--branch <branch>]
  ensign task open <project>/<task>
  ensign task subtask <done|drop> <project>/<task> <name>
  ensign run <project>/<task> [--subtask <name>] [--start <stage>] [--sync] [--dependency <location>=<branch>] [--dry-run]
  ensign run --pipeline <name> [--project <p>] [--rebuild] [--dry-run]
  ensign run retire <run>
  ensign sequence [run <project>/<task> [run flags, e.g. --location <name> --sides <a,b>] | publish <project>/<task> | release <project> <version> [--location <name>] | do <instruction> | mode stop|keep-going | halt [--reason <text>] | discard [--reason <text>] | drop <n>... [--reason <text>] | retry <n> --diagnosis <why> --state <what is left> | accept <n> | done <n> [--note <text>] | fail <n> --reason <text>]
  ensign style ingest <project> <path-or-url> --trust authoritative|exemplar [--note <text>] [--location <name>]
  ensign style report <project>
  ensign skill new <name> --description <text> | ensign skill discard <name>
  ensign resume --answer <text> [--budget <name>=<n>] [--run <id>]
  ensign journal commit -m <message> [files…] | ensign journal push
  ensign git-catalog squash <project>/<task> [--location <name>] -m <message>
  ensign github-pr pull-request|merge|update <project>/<task> [--location <name>]
  ensign next [--bridge <dir>] [--run <id>]
  ensign run-stage [--bridge <dir>] [--run <id>]
  ensign dependency apply --side <location> | restore [--side <location>] [--bridge <dir>] [--run <id>]
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
		if strings.TrimSpace(in.Prompt) == "go" {
			text, ok, err := approveSequence(bridge, in.SessionID)
			if err != nil {
				return failHook(&in, nil, err, stderr)
			}
			if ok {
				json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
					"hookEventName": "UserPromptSubmit", "additionalContext": "starship: " + text}})
				return 0
			}
		}
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
				return 0
			}
			why, warn, err := editGuard(bridge, &in)
			if err != nil {
				return failHook(&in, nil, err, stderr)
			}
			out := map[string]any{}
			if why != "" {
				out["hookSpecificOutput"] = map[string]any{
					"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "starship: " + why}
			}
			if warn != nil {
				out["systemMessage"] = "starship: " + warn.Error()
			}
			if len(out) > 0 {
				json.NewEncoder(stdout).Encode(out)
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
	r, err := openHookRun(bridge, &in)
	if err != nil {
		return failHook(&in, nil, err, stderr)
	}
	if r == nil && event == "UserPromptSubmit" && strings.TrimSpace(in.Prompt) == "go" {
		if r, err = claimRun(bridge, in.SessionID); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	} else if r == nil {
		role, _ := in.ToolInput["subagent_type"].(string)
		if event == "PreToolUse" && in.AgentID != "" && stageRoleAnywhere(bridge, role) {
			json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "deny",
				"permissionDecisionReason": "starship: this agent's run can't be found: the session restarted and the agent could not be tied to any live run, so it can't dispatch stages"}})
		}
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
	if r != nil && in.AgentID != "" {
		if d := r.State.byAgent(in.AgentID); d != nil {
			d.LastSeen = time.Now().Format(time.RFC3339)
		}
	}
	if r != nil {
		if err := finishIfTerminal(r, wasTerminal, stderr); err != nil {
			return failHook(&in, nil, err, stderr)
		}
	}
	if r != nil {
		err := r.save()
		if errors.Is(err, fs.ErrNotExist) {
			return 0 // the run was replaced and deleted while this hook held it
		}
		if err != nil {
			return failHook(&in, nil, err, stderr)
		}
	}
	if r != nil && r.warn != nil && event != "UserPromptSubmit" {
		// systemMessage is common to every hook event's reply.
		m, isMap := out.(map[string]any)
		if out == nil {
			m, isMap = map[string]any{}, true
		}
		if isMap {
			m["systemMessage"] = "starship: " + r.warn.Error()
			out = m
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
	if r != nil && r.lock.Locked() && !r.State.terminal() { // unlocked: the state is stale
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
	carryErr := carryOverSession(bridge, in)
	var envNote string
	if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" && in.SessionID != "" {
		env, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			// The session's own commands run the bridge's ensign, found on PATH. Bash sources the setup file a run writes later; PowerShell, which takes only assignments, gets the environment now.
			var content string
			if plainEnvFile() {
				content, envNote = plainSessionEnv(bridge, in.SessionID)
			} else {
				content = fmt.Sprintf("export STARSHIP_SESSION=%s\nexport PATH=%s:\"$PATH\"\n",
					in.SessionID, shellQuote(posixPath(filepath.Join(bridge, ".starship", "bin"), hostOS == "windows"))) +
					sourceSetupLine(bridge, in.SessionID)
			}
			_, err = io.WriteString(env, content)
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
		if in.Source == "compact" || in.Source == "clear" {
			clearTopicMarkers(bridge, in.SessionID)
		}
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
	addGitattributes(bridge)
	dropSessionImport(bridge)
	updateHeartbeat(bridge)
	if note := splitHistory(bridge); note != "" {
		fmt.Fprintln(&ctx, note)
	}
	upgradeHookSettings(bridge)
	removeOldExe(bridge)
	fmt.Fprintf(&ctx, "STARSHIP-READY ensign %s bridge %s\n", version, bridge)
	if carryErr != nil {
		fmt.Fprintf(&ctx, "starship: could not carry the earlier session's runs over: %v\n", carryErr)
	}
	if envNote != "" {
		fmt.Fprintln(&ctx, envNote)
	}
	// The manual goes to the session through this hook, not CLAUDE.md, because Claude
	// Code gives CLAUDE.md to every subagent too. SessionStart also fires after a
	// compaction, so the manual comes back when the context is summarised.
	manual, err := os.ReadFile(filepath.Join(bridge, ".starship", "session.md"))
	if err != nil {
		fmt.Fprintln(&ctx, "starship: the session manual is missing; run ensign link")
		return reply("Starship is not ready in this bridge. Ask Claude to set it up.", ctx.String())
	}
	fmt.Fprintf(&ctx, "\n%s", manual)
	if requestCommands(bridge) != "" {
		fmt.Fprint(&ctx, "\nTo carry out a request like squashing commits or curating skills: read `ensign help requests` for the requests this bridge answers and the command for each.\n")
	}
	// A compaction or clear replaces the context, so the task's project notes, which
	// task open printed into it, are printed again.
	if task := sessionTask(bridge, in.SessionID); in.SessionID != "" && task != "" && (in.Source == "compact" || in.Source == "clear") {
		project, _, _ := strings.Cut(task, "/")
		if block, err := projectNotes(bridge, project); err == nil && block != "" {
			if ctx.Len()+len(block) < sessionContextLimit {
				ctx.WriteString(block)
			} else {
				fmt.Fprintf(&ctx, "\nnotes      projects/%s/project.md is too long to reprint here; run `ensign task open %s` to see it again\n", project, task)
			}
		}
	}
	show := "Starship ready · bridge " + filepath.Base(bridge)
	if version != "" && version != "dev" {
		show += " · " + version
	}
	if keep, err := bf.keepRuns(); err != nil {
		fmt.Fprintln(stderr, "starship:", err)
	} else if res := gcRuns(bridge, keep, sessionStartGCCap); res.Removed > 0 {
		show += fmt.Sprintf(" · cleaned up %d old runs", res.Removed)
		if res.More {
			show += " (more will be cleaned at the next session start)"
		}
	}
	return reply(show, ctx.String())
}

// sessionContextLimit is the length of additionalContext past which Claude Code cuts it
// to a preview.
const sessionContextLimit = 10000

// beforeHandback is settings as ensign wrote them before it registered PostToolUse and
// matched SubagentHandback.
func beforeHandback(settings string) string {
	i := strings.Index(settings, `    "PostToolUse"`)
	j := strings.Index(settings[i:], "\n")
	settings = settings[:i] + settings[i+j+1:]
	return strings.Replace(settings, "|SubagentHandback", "", 1)
}

// upgradeHookSettings brings a bridge's settings.json up to date (a timeout on the
// PreToolUse and SubagentStop hooks, the PostToolUse hook, SubagentHandback) when it is exactly what an
// older ensign wrote, and commits that. A settings file someone has edited is left alone.
func upgradeHookSettings(bridge string) {
	f := filepath.Join(bridge, ".claude", "settings.json")
	data, err := os.ReadFile(f)
	if err != nil {
		return
	}
	older := false
	for _, form := range []string{hookSettingsWith(0, 0), hookSettingsWith(hookTimeout, 0)} {
		older = older || string(data) == form || string(data) == beforeHandback(form)
	}
	if !older {
		return
	}
	if os.WriteFile(f, []byte(hookSettings()), 0o644) == nil && isGit(bridge) {
		git(bridge, "commit", "-q", "-m", "Update the starship hook settings", "--", ".claude/settings.json")
	}
}

// addGitattributes gives an existing bridge the .gitattributes that init writes, so its text files are LF on every machine, and commits just that file; committed blobs are not rewritten. Tracked files with uncommitted changes defer it to the next session. On a failure it removes and unstages the file unless HEAD has it, so the next session retries.
func addGitattributes(bridge string) {
	f := filepath.Join(bridge, ".gitattributes")
	if !isGit(bridge) {
		return
	}
	if dirty, err := gitOut(bridge, "status", "--porcelain", "--untracked-files=no"); err != nil || dirty != "" {
		return
	}
	// Exclusive create: only the session that makes the file may commit or undo it.
	file, err := os.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return
	}
	_, werr := file.WriteString(bridgeGitattributes)
	if cerr := file.Close(); werr != nil || cerr != nil {
		os.Remove(f)
		return
	}
	if git(bridge, "add", "--", ".gitattributes") != nil ||
		git(bridge, "commit", "-q", "-m", "Keep the bridge's text files at LF line endings", "--", ".gitattributes") != nil {
		// A concurrent session may have committed it already; then it is theirs to keep.
		if git(bridge, "cat-file", "-e", "HEAD:.gitattributes") != nil {
			os.Remove(f)
			git(bridge, "reset", "-q", "--", ".gitattributes")
		}
		return
	}
	// checkout-index skips files git thinks are unchanged, even with -f, so write the index's LF content over each CRLF working copy. Writing in place never leaves a file missing.
	eol, _ := gitOut(bridge, "ls-files", "-z", "--eol")
	var rewritten []string
	for _, rec := range strings.Split(eol, "\x00") {
		info, path, ok := strings.Cut(rec, "\t")
		if !ok || !strings.Contains(info, "w/crlf") || strings.Contains(info, "i/crlf") || strings.Contains(info, "i/mixed") {
			continue
		}
		if data, err := exec.Command("git", "-C", bridge, "cat-file", "--filters", ":"+path).Output(); err == nil {
			if os.WriteFile(filepath.Join(bridge, filepath.FromSlash(path)), data, 0o644) == nil {
				rewritten = append(rewritten, path)
			}
		}
	}
	// The index still records the CRLF size, which status reads as a change; re-hashing the same content as the index blob refreshes that without staging anything.
	if len(rewritten) > 0 {
		cmd := exec.Command("git", "-C", bridge, "update-index", "-z", "--stdin")
		cmd.Stdin = strings.NewReader(strings.Join(rewritten, "\x00") + "\x00")
		cmd.Run()
	}
}

// updateHeartbeat replaces the old heartbeat paragraph in a bridge's CLAUDE.md with one
// scoped to the session agent, and commits that. A CLAUDE.md with someone's uncommitted
// edits, or without the old paragraph, is left alone.
func updateHeartbeat(bridge string) {
	f := filepath.Join(bridge, "CLAUDE.md")
	data, err := os.ReadFile(f)
	if err != nil {
		return
	}
	// A checkout with CRLF endings (Windows, core.autocrlf) keeps them.
	from, to := oldHeartbeat, heartbeat
	if !strings.Contains(string(data), from) {
		from, to = strings.ReplaceAll(from, "\n", "\r\n"), strings.ReplaceAll(to, "\n", "\r\n")
	}
	if !strings.Contains(string(data), from) {
		return
	}
	tracked := isGit(bridge)
	if tracked {
		if dirty, err := gitOut(bridge, "status", "--porcelain", "--", "CLAUDE.md"); err != nil || dirty != "" {
			return
		}
	}
	updated := strings.Replace(string(data), from, to, 1)
	if os.WriteFile(f, []byte(updated), 0o644) == nil && tracked {
		git(bridge, "commit", "-q", "-m", "Scope the heartbeat rule to the session agent", "--", "CLAUDE.md")
	}
}

// splitHistory moves each task's "## Progress log" section out of task.md into a new
// history.md beside it, and commits that. In a git bridge, a task with uncommitted changes
// to either file is left alone, as is one that already has a history.md. It returns what
// the session needs to be told.
func splitHistory(bridge string) string {
	tracked := isGit(bridge)
	files, _ := filepath.Glob(filepath.Join(bridge, "projects", "*", "tasks", "*", "task.md"))
	var moved, notes []string
	for _, tf := range files {
		func() {
			dir := filepath.Dir(tf)
			task := filepath.Base(filepath.Dir(filepath.Dir(dir))) + "/" + filepath.Base(dir)
			lock := taskStateFile(bridge, task, "", ".journal.lock")
			if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
				notes = append(notes, fmt.Sprintf("starship: could not lock %s to split its progress log: %v", relTo(bridge, dir), err))
				return
			}
			l := flock.New(lock)
			if err := l.Lock(); err != nil {
				notes = append(notes, fmt.Sprintf("starship: could not lock %s to split its progress log: %v", relTo(bridge, dir), err))
				return
			}
			defer l.Unlock()
			hf := filepath.Join(dir, "history.md")
			data, err := os.ReadFile(tf)
			if err != nil {
				return
			}
			lines := strings.SplitAfter(string(data), "\n")
			start := -1
			for i, l := range lines {
				if strings.TrimSpace(l) == "## Progress log" {
					start = i
					break
				}
			}
			if start < 0 {
				return
			}
			rel := relTo(bridge, dir)
			if _, err := os.Stat(hf); err == nil {
				notes = append(notes, fmt.Sprintf("starship: %s has a progress log in task.md and a history.md; merge them by hand", rel))
				return
			}
			if tracked {
				dirty, err := gitOut(bridge, "status", "--porcelain", "--", tf, hf)
				if err != nil {
					notes = append(notes, fmt.Sprintf("starship: could not read the git status of %s (%v), so its progress log was not split", rel, err))
					return
				}
				if dirty != "" {
					return
				}
			}
			end := len(lines)
			for i := start + 1; i < len(lines); i++ {
				if strings.HasPrefix(lines[i], "## ") {
					end = i
					break
				}
			}
			entries := strings.ReplaceAll(strings.TrimSpace(strings.Join(lines[start+1:end], "")), "\r\n", "\n")
			rest := lines[:start]
			if end < len(lines) {
				rest = append(rest, lines[end:]...)
			} else {
				for len(rest) > 1 && strings.TrimSpace(rest[len(rest)-1]) == "" {
					rest = rest[:len(rest)-1]
				}
			}
			history := historyHeading(filepath.Base(dir)) + entries + "\n"
			if strings.Contains(string(data), "\r\n") {
				history = strings.ReplaceAll(history, "\n", "\r\n")
			}
			if err := os.WriteFile(hf, []byte(history), 0o644); err != nil {
				notes = append(notes, fmt.Sprintf("starship: could not write %s: %v", hf, err))
				return
			}
			if err := os.WriteFile(tf, []byte(strings.Join(rest, "")), 0o644); err != nil {
				os.Remove(hf)
				notes = append(notes, fmt.Sprintf("starship: could not write %s: %v", tf, err))
				return
			}
			moved = append(moved, tf, hf)
		}()
	}
	if len(moved) > 0 && tracked {
		if err := git(bridge, append([]string{"add", "--"}, moved...)...); err != nil {
			notes = append(notes, fmt.Sprintf("starship: split the progress logs but could not stage them: %v", err))
		} else if err := git(bridge, append([]string{"commit", "-q", "-m", "Move each task's progress log into history.md", "--"}, moved...)...); err != nil {
			notes = append(notes, fmt.Sprintf("starship: split the progress logs but could not commit them: %v", err))
		}
	}
	return strings.Join(notes, "\n")
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
		if dirty, err := gitOut(bridge, "status", "--porcelain", "--", "CLAUDE.md"); err != nil || dirty != "" {
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

// finishIfTerminal closes out a run that this call made terminal: it finishes the run, and for a sequence saves and advances to the next step. A failed advance is only reported.
func finishIfTerminal(r *RunFiles, wasTerminal bool, stderr io.Writer) error {
	if wasTerminal || !r.State.terminal() {
		return nil
	}
	finishRun(r)
	if r.State.Sequence {
		// Saved first: the next step reads this run's final state from disk.
		if err := r.save(); err != nil {
			return err
		}
		if _, err := advanceSequence(r.Snap.Bridge, r.State.Session); err != nil {
			fmt.Fprintln(stderr, "ensign: could not advance the sequence:", err)
		}
	}
	return nil
}
