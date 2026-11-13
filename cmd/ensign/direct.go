// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The phases of a direct edit: asked for, shown to the user by a Stop, and opened by the user's go.
const (
	directPending = "pending"
	directShown   = "shown"
	directOpen    = "open"
)

// directFile is a session's direct edit: the one code folder the session may edit itself for one piece of work, with a one-line description of the work.
type directFile struct {
	Folder string `json:"folder"`
	Work   string `json:"work"`
	Phase  string `json:"phase"`
	// Spelling is the folder as the editor spelled it; Folder is its resolved form, used for matching.
	Spelling string `json:"spelling,omitempty"`
}

// shown is the folder as the editor spelled it, or the resolved folder for a record without a spelling.
func (df *directFile) shown() string {
	if df.Spelling != "" {
		return df.Spelling
	}
	return df.Folder
}

func directPath(bridge, session string) string { return sessionFile(bridge, session) + ".direct" }

// readDirect reads the session's direct edit; it is nil when there is none.
func readDirect(bridge, session string) (*directFile, error) {
	df := &directFile{}
	if err := readJSON(directPath(bridge, session), df); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return df, nil
}

func closeDirect(bridge, session string) error {
	if err := os.Remove(directPath(bridge, session)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// runHolding says which approved, unfinished run holds a code folder that p is in, whether it works there or only reads it; "" when none does. A direct edit in a read folder changes what that run reads. With queued, a queued run of a sequence counts too, approved or not: it starts there when its turn comes.
func runHolding(bridge, p, goos string, queued bool) (string, error) {
	live, err := liveRuns(bridge)
	if err != nil {
		return "", err
	}
	for _, ri := range live {
		if !(ri.state.Approved || queued && ri.state.queued()) || ri.snap.Workspace == "" {
			continue
		}
		works, reads := folderHolds(&ri.snap)
		for i, f := range append(works, reads...) {
			if !pathWithin(p, resolveEditFolder(f, goos), goos) {
				continue
			}
			verb := "works in"
			if i >= len(works) {
				verb = "reads"
			}
			if ri.state.queued() {
				return fmt.Sprintf("run %s is queued and will start there: it %s the code folder %s", ri.id, verb, f), nil
			}
			return fmt.Sprintf("run %s (%s) %s the code folder %s", ri.id, ri.state.Status, verb, f), nil
		}
	}
	return "", nil
}

// landingAround reports the first of folder and its ancestors that a landing or an update holds, and which ("a landing" or "an update"); "" when none does. A landing or an update locks the exact folder it works in, and one in a folder around a direct edit's folder reaches it too. The caller holds lockFolders.
func landingAround(bridge, folder string) (string, string, error) {
	for p := resolveEditFolder(folder, runtime.GOOS); ; {
		if landing, err := landingUnderWay(bridge, p); err != nil || landing {
			return p, "a landing", err
		}
		if updating, err := updateUnderWay(bridge, p); err != nil || updating {
			return p, "an update", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", "", nil
		}
		p = parent
	}
}

// directFolderBusy is why a direct edit can't be asked for or opened in folder, with the folder a landing or an update holds named in spelling, the editor's form of folder: a live or queued run holds it or a folder around it, or a landing or an update is under way in it or a folder around it. The caller holds lockFolders. It returns "" when the folder is free.
func directFolderBusy(bridge, folder, spelling string) (string, error) {
	if why, err := runHolding(bridge, folder, runtime.GOOS, true); why != "" || err != nil {
		return why, err
	}
	around, what, err := landingAround(bridge, folder)
	if err != nil || around == "" {
		return "", err
	}
	return fmt.Sprintf("%s is under way in the code folder %s", what, inRunsSpelling(spelling, around)), nil
}

// directEdit decides a session's edit of resolved, a target outside the folders it may always edit: allowed inside the folder of its open direct edit unless a live run holds the target or a landing or an update is under way around it. It returns the refusal text in held when the edit is within the open folder and a run, a landing or an update holds the target.
func directEdit(bridge, session, resolved, goos string) (allowed bool, held string, err error) {
	if session == "" {
		return false, "", nil
	}
	df, rerr := readDirect(bridge, session)
	if rerr != nil || df == nil || df.Phase != directOpen {
		return false, "", nil
	}
	if !pathWithin(resolved, resolveEditFolder(df.Folder, goos), goos) {
		return false, "", nil
	}
	l, err := lockFolders(bridge)
	if err != nil {
		return false, "", err
	}
	defer l.Unlock()
	held, err = runHolding(bridge, resolved, goos, false)
	if err != nil || held != "" {
		return false, held, err
	}
	around, what, err := landingAround(bridge, filepath.Dir(resolved))
	if err != nil || around != "" {
		if around != "" {
			held = fmt.Sprintf("%s is under way in the code folder %s", what, around)
		}
		return false, held, err
	}
	return true, "", nil
}

// promptDirect runs at every UserPromptSubmit that promptWrites did not handle, after promptWrites has voided unused approvals; a go that promptWrites handled never reaches it, and closeOpenDirect covers that path. A new prompt is new work, so an open direct edit closes, and a request the prompt does not approve is dropped, except by a word the session answers (status, help), which leaves a waiting request alone. It is handled when the prompt is go and a direct edit was waiting for it: text then says what happened, and the run and sequence paths must not see this go. note reports a request that was closed or dropped.
func promptDirect(bridge string, in *HookInput) (note, text string, handled bool, err error) {
	if in.SessionID == "" {
		return
	}
	df, rerr := readDirect(bridge, in.SessionID)
	if df == nil && rerr == nil {
		return
	}
	if df != nil && df.Phase != directOpen && isSessionWord(in.Prompt) {
		return
	}
	if err = closeDirect(bridge, in.SessionID); err != nil {
		return
	}
	if rerr != nil {
		note = "starship: the session's direct edit state could not be read and was dropped; the guard is shut."
		return
	}
	isGo := strings.TrimSpace(in.Prompt) == "go"
	if df.Phase == directOpen {
		note = fmt.Sprintf("starship: the direct edit in %s (%s) is closed: a new prompt is new work. Ask again (ensign direct ask) if it is needed.", df.shown(), df.Work)
		return
	}
	if !isGo {
		note = fmt.Sprintf("starship: the request for a direct edit in %s (%s) was not approved with go and is dropped.", df.shown(), df.Work)
		return
	}
	handled = true
	if df.Phase != directShown {
		text = fmt.Sprintf("this go opened nothing: the direct edit request for %s was not shown to the user at the end of a turn, so it is dropped. Tell the user the folder and the work and ask for go again with ensign direct ask.", df.shown())
		return
	}
	waiting, err := goWaiting(bridge, in.SessionID)
	if err != nil {
		return
	}
	if waiting {
		text = fmt.Sprintf("a direct edit in %s and a run or sequence both wait for go, so this go approved neither and the direct edit request is dropped. Tell the user; discard the run or sequence if the direct edit is wanted, then ask again.", df.shown())
		return
	}
	l, err := lockFolders(bridge)
	if err != nil {
		if errors.As(err, new(*lockBusyError)) {
			// The request stays for the next go.
			if werr := writeJSON(directPath(bridge, in.SessionID), df); werr != nil {
				err = werr
			}
		}
		return
	}
	defer l.Unlock()
	why, err := directFolderBusy(bridge, df.Folder, df.shown())
	if err != nil {
		return
	}
	if why != "" {
		text = fmt.Sprintf("the direct edit in %s was not opened: %s. The request is dropped; tell the user.", df.shown(), why)
		return
	}
	df.Phase = directOpen
	if err = writeJSON(directPath(bridge, in.SessionID), df); err != nil {
		return
	}
	text = fmt.Sprintf("the guard is open for %s for this turn only, for: %s. Do only what the user asked, and run no builds or tests unless they asked. Commits and pushes still need the user's explicit approval. Run `ensign direct close` when done.", df.shown(), df.Work)
	return
}

// closeOpenDirect closes the session's open direct edit for a prompt that another step handles before promptDirect, and returns the note for the reply. Nothing else changes.
func closeOpenDirect(bridge, session string) (string, error) {
	if session == "" {
		return "", nil
	}
	df, rerr := readDirect(bridge, session)
	if rerr != nil || df == nil || df.Phase != directOpen {
		return "", nil
	}
	if err := closeDirect(bridge, session); err != nil {
		return "", err
	}
	return fmt.Sprintf("starship: the direct edit in %s (%s) is closed: a new prompt is new work. Ask again (ensign direct ask) if it is needed.", df.shown(), df.Work), nil
}

// directStop ends a turn's direct edit: an open one closes, and a pending request counts as shown to the user, which go needs.
func directStop(bridge string, in *HookInput) any {
	if in.SessionID == "" {
		return nil
	}
	df, rerr := readDirect(bridge, in.SessionID)
	if df == nil && rerr == nil {
		return nil
	}
	var err error
	if rerr != nil || df.Phase == directOpen {
		err = closeDirect(bridge, in.SessionID)
	} else if df.Phase == directPending {
		df.Phase = directShown
		err = writeJSON(directPath(bridge, in.SessionID), df)
	}
	if err != nil && !in.StopHookActive {
		return block(fmt.Sprintf("the session's direct edit state could not be updated: %v", err))
	}
	return nil
}

// cmdDirect implements `ensign direct ask <folder> "<what>" | close`. Nothing here approves: only the user's go does.
func cmdDirect(args []string, out io.Writer) error {
	usage := errors.New(`usage: ensign direct ask <folder> "<what the user asked for>" | ensign direct close`)
	session := os.Getenv("STARSHIP_SESSION")
	if session == "" {
		return errors.New("a direct edit belongs to a session, and STARSHIP_SESSION is not set")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 1 && args[0] == "close":
		df, err := readDirect(bridge, session)
		if err != nil {
			fmt.Fprintf(out, "the direct edit state could not be read (%v); closing it\n", err)
		}
		if err := closeDirect(bridge, session); err != nil {
			return err
		}
		switch {
		case df == nil:
			fmt.Fprintln(out, "no direct edit to close")
		case df.Phase == directOpen:
			fmt.Fprintf(out, "closed the direct edit in %s\n", df.shown())
		default:
			fmt.Fprintf(out, "dropped the pending request for a direct edit in %s\n", df.shown())
		}
		return nil
	case len(args) >= 3 && args[0] == "ask":
		work := strings.Join(strings.Fields(strings.Join(args[2:], " ")), " ")
		if work == "" {
			return usage
		}
		return directAsk(bridge, session, args[1], work, out)
	}
	return usage
}

func directAsk(bridge, session, folder, work string, out io.Writer) error {
	if err := movedRefusal(bridge, session); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	expanded := claudeToolPath(folder, home, runtime.GOOS)
	if windowsUnresolvable(expanded, runtime.GOOS) {
		return errors.New("on Windows, give the full `C:\\...` path of the folder")
	}
	cwd, _ := os.Getwd()
	resolved := resolveEditTarget(expanded, cwd, runtime.GOOS)
	if fi, err := os.Stat(resolved); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not an existing folder", folder)
	}
	if waiting, err := goWaiting(bridge, session); err != nil {
		return err
	} else if waiting {
		return errors.New("a run or sequence is waiting for go, and a direct edit can't wait with it; have the user approve or discard it first")
	}
	if err := writesPendingRefusal(bridge, session); err != nil {
		return err
	}
	l, err := lockFolders(bridge)
	if err != nil {
		return err
	}
	defer l.Unlock()
	if why, err := directFolderBusy(bridge, resolved, expanded); err != nil {
		return err
	} else if why != "" {
		return fmt.Errorf("a direct edit is refused in a folder a live run holds: %s", why)
	}
	if err := os.MkdirAll(filepath.Dir(directPath(bridge, session)), 0o755); err != nil {
		return err
	}
	if err := writeJSON(directPath(bridge, session), &directFile{Folder: resolved, Spelling: expanded, Work: work, Phase: directPending}); err != nil {
		return err
	}
	fmt.Fprintf(out, "direct edit requested in %s for: %s\nIt waits for the user's go; nothing here approves it. Tell the user the folder and the work in your reply and ask them to type go, then end your turn. A go opens the guard for that folder for the next turn only; any other reply drops the request, except status and help, which leave it waiting.\n", expanded, work)
	return nil
}

// dirtyFolderError is a run that can't start because a code folder it works in has uncommitted changes: the run's commit would sweep them in. A retry is exempt, since its dirty tree is the previous attempt's carried work.
type dirtyFolderError struct {
	folder string
	paths  []string
}

func (e *dirtyFolderError) Error() string {
	shown := e.paths[:min(len(e.paths), 5)]
	more := ""
	if len(e.paths) > len(shown) {
		more = ", ..."
	}
	return fmt.Sprintf("the code folder %s has %d uncommitted path(s): %s%s. A run's commit would sweep them in: ask the user to commit, stash or discard them", e.folder, len(e.paths), strings.Join(shown, ", "), more)
}

// dirtyFolder returns a *dirtyFolderError for the first folder the snapshot's run works in that has uncommitted changes. A folder that is not a git repository, or whose status can't be read, is left out, and a p4 task is never checked: its workspace may sit inside an unrelated git repository. The task's vcs is read from bridge and task; when that fails (or there is no task) the git check runs.
func dirtyFolder(snap *Snapshot, bridge, task string) error {
	if task != "" {
		if v, err := taskVcsOf(bridge, task); err == nil && v.Kind == "p4" {
			return nil
		}
	}
	works, _ := folderHolds(snap)
	for _, ws := range works {
		if paths, err := gitDirtyPaths(ws); err == nil && len(paths) > 0 {
			return &dirtyFolderError{folder: ws, paths: paths}
		}
	}
	return nil
}
