package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// defaultKeepRuns is how many finished runs a bridge keeps when bridge.yaml has no keep-runs.
const defaultKeepRuns = 500

// sessionStartGCCap is how many runs one session start deletes, so a first cleanup of a long-neglected bridge cannot hold the start up.
const sessionStartGCCap = 50

// keepRuns is the bridge's limit on finished runs.
func (bf *BridgeFile) keepRuns() (int, error) {
	if bf.KeepRuns == nil {
		return defaultKeepRuns, nil
	}
	if *bf.KeepRuns < 0 {
		return 0, fmt.Errorf("bridge.yaml: keep-runs must not be negative")
	}
	return *bf.KeepRuns, nil
}

// gcResult is what a cleanup did.
type gcResult struct {
	Removed, Scratch, Links int
	More                    bool // candidates remain because the cap was reached
}

// finishedRun reports whether a run's state says success or error. Pending, running and ask-user runs are never finished.
func finishedRun(st *State) bool { return st.Status == "success" || st.Status == "error" }

// gcRuns deletes finished runs beyond keep, oldest first, at most max of them (max <= 0 means no cap). It then empties the scratch folders of the finished runs it keeps (runs an open sequence step still needs are left alone) and drops session links to runs that no longer exist.
func gcRuns(bridge string, keep, max int) gcResult {
	var res gcResult
	held, ok := heldRuns(bridge)
	if !ok {
		res.Links = dropDeadLinks(bridge)
		return res
	}
	entries, _ := os.ReadDir(runsDir(bridge))
	var finished []string // newest first
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].IsDir() || held[entries[i].Name()] {
			continue
		}
		var st State
		if readJSON(filepath.Join(runsDir(bridge), entries[i].Name(), "state.json"), &st) == nil && finishedRun(&st) {
			finished = append(finished, entries[i].Name())
		}
	}
	kept := finished
	var candidates []string
	if len(finished) > keep {
		kept, candidates = finished[:keep], finished[keep:]
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if max > 0 && res.Removed >= max {
			res.More = true
			break
		}
		r, err := openRun(bridge, candidates[i])
		if err != nil {
			continue
		}
		done := finishedRun(r.State)
		dir := r.Dir
		r.close()
		if !done {
			continue
		}
		removeRun(dir)
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			res.Removed++
		}
	}
	for _, id := range kept {
		if emptyScratchIfFinished(bridge, id) {
			res.Scratch++
		}
	}
	res.Links = dropDeadLinks(bridge)
	return res
}

// emptyScratchIfFinished empties a run's scratch folder when the run is finished and the folder holds something. It reports whether it emptied it.
func emptyScratchIfFinished(bridge, id string) bool {
	scratch := filepath.Join(runsDir(bridge), id, "scratch")
	if items, err := os.ReadDir(scratch); err != nil || len(items) == 0 {
		return false
	}
	r, err := openRun(bridge, id)
	if err != nil {
		return false
	}
	done := finishedRun(r.State)
	r.close()
	return done && emptyDir(scratch)
}

// emptyDir removes what is inside a folder and keeps the folder. A failure is only a warning.
func emptyDir(dir string) bool {
	items, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	ok := true
	for _, it := range items {
		if err := os.RemoveAll(filepath.Join(dir, it.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not empty %s: %v\n", dir, err)
			ok = false
		}
	}
	return ok && len(items) > 0
}

// heldRuns returns the runs that open sequence steps still need: a failed step waits on its run for a retry, and a step left active points at its run. ok is false when a sequence file can't be read, and then gc must not delete anything because it can't tell which runs are needed.
func heldRuns(bridge string) (held map[string]bool, ok bool) {
	held = map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(bridge, ".starship", "sessions", "*.sequence"))
	for _, f := range files {
		var sf seqFile
		if err := readJSON(f, &sf); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, false
		}
		for _, seq := range sf.Sequences {
			for _, st := range seq.Steps {
				if !st.open() {
					continue
				}
				for _, ru := range st.runs() {
					held[ru.Run] = true
				}
			}
		}
	}
	return held, true
}

// staleGCAge is how old a leftover <id>.gc file must be before a sweep removes it. A rename keeps the mtime, so this holds only because dropDeadLinks lets one sweeper run at a time.
const staleGCAge = 5 * time.Minute

// pidRecordAge is how old a <pid>.pid record must be before a sweep removes it. Age, not liveness, because a session idle past it loses only the link to its earlier id, which is how /clear behaved before the record existed.
const pidRecordAge = 30 * 24 * time.Hour

// sessionFileAge is how long a session must be untouched before a sweep removes its <id>.task, .pin, .env, .sequence and .topic-* files. Age, not liveness, because those files are never rewritten when a session ends; a session idle past it loses its task, topic memory, .pin (so the "bridge updated" check is skipped, run.go:952, sequence.go:1111) and .env until its next run rebinds it.
const sessionFileAge = 30 * 24 * time.Hour

// deadLink reports whether a session link names at least one run, none of the names is a path, and every named run's folder is gone. A link that is empty, unreadable or names a path is not dead. Links hold one run id per line.
func deadLink(bridge, file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	named := 0
	for _, id := range strings.Split(string(data), "\n") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if strings.ContainsAny(id, `/\`) {
			return false
		}
		if _, err := os.Stat(filepath.Join(runsDir(bridge), id)); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		named++
	}
	return named > 0
}

// sweptSession is one session's files that sweepSessionFiles may remove.
type sweptSession struct {
	files  []string // names in the sessions folder
	newest time.Time
	failed bool // a file's Info failed, so its age is unknown
	hasSeq bool
}

// sessionOfFile returns the session id a swept-type file name belongs to.
func sessionOfFile(name string) (string, bool) {
	if strings.HasSuffix(name, ".gc") {
		return "", false
	}
	for _, suf := range []string{".task", ".pin", ".env", ".sequence"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf), true
		}
	}
	if id, _, ok := strings.Cut(name, ".topic-"); ok && id != "" {
		return id, true
	}
	return "", false
}

// sweepSessionFiles removes the per-session files of sessions untouched for sessionFileAge. A session's age is the newest mtime among its swept files and its link, because .pin is rewritten at every SessionStart while .task is not. A session with a live run or an open sequence keeps all its files. It must run before dead links are dropped, so a link still shows which runs a session owned.
func sweepSessionFiles(bridge string, entries []os.DirEntry) {
	groups := map[string]*sweptSession{}
	note := func(g *sweptSession, e os.DirEntry) {
		info, err := e.Info()
		if err != nil {
			g.failed = true
			return
		}
		if info.ModTime().After(g.newest) {
			g.newest = info.ModTime()
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := sessionOfFile(e.Name())
		if !ok {
			continue
		}
		g := groups[id]
		if g == nil {
			g = &sweptSession{}
			groups[id] = g
		}
		g.files = append(g.files, e.Name())
		g.hasSeq = g.hasSeq || strings.HasSuffix(e.Name(), ".sequence")
		note(g, e)
	}
	for _, e := range entries {
		if g := groups[e.Name()]; g != nil && !e.IsDir() {
			note(g, e)
		}
	}
	var runs []runInfo
	listed := false
	for id, g := range groups {
		if g.failed || time.Since(g.newest) <= sessionFileAge {
			continue
		}
		if g.hasSeq {
			l := flock.New(seqPath(bridge, id) + ".lock")
			if ok, err := l.TryLock(); err != nil || !ok {
				continue
			}
			if !listed {
				runs, listed = listRuns(bridge), true
			}
			sweepSession(bridge, id, g, runs)
			l.Unlock()
			continue
		}
		if !listed {
			runs, listed = listRuns(bridge), true
		}
		sweepSession(bridge, id, g, runs)
	}
}

// sweepSession removes a stale session's files unless it is busy.
func sweepSession(bridge, id string, g *sweptSession, runs []runInfo) {
	if sessionBusy(bridge, id, runs) {
		return
	}
	dir := filepath.Join(bridge, ".starship", "sessions")
	reapSession(dir, id, g.files)
}

// sessionBusy reports whether a session has an open sequence or a run that has not finished, found through its link or through the run's owner. Anything unreadable counts as busy.
func sessionBusy(bridge, id string, runs []runInfo) bool {
	var sf seqFile
	if err := readJSON(seqPath(bridge, id), &sf); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	} else {
		for _, seq := range sf.Sequences {
			if seq.unfinished() {
				return true
			}
		}
	}
	linked, err := sessionRuns(bridge, id)
	if err != nil {
		return true
	}
	for _, run := range linked {
		var st State
		err := readJSON(filepath.Join(runsDir(bridge), run, "state.json"), &st)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !finishedRun(&st) {
			return true
		}
	}
	for _, ri := range runs {
		if ri.state.Session == id && !finishedRun(&ri.state) {
			return true
		}
	}
	return false
}

// reapSession removes a session's files the way a dead link is removed: it renames them all to <name>.gc and judges the session once more, so a write in between restores the whole session rather than only the file written.
func reapSession(dir, id string, files []string) {
	var renamed []string
	for _, name := range files {
		orig := filepath.Join(dir, name)
		if os.Rename(orig, orig+".gc") == nil {
			renamed = append(renamed, orig)
		}
	}
	settleSession(dir, id, files, renamed)
}

// settleSession judges a session whose files were renamed to .gc: it removes them all if the session is still stale, and otherwise restores each unless a newer file holds its name.
func settleSession(dir, id string, files, renamed []string) {
	fresh := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && time.Since(info.ModTime()) <= sessionFileAge
	}
	stale := true
	for _, orig := range renamed {
		stale = stale && !fresh(orig+".gc")
	}
	for _, name := range files {
		stale = stale && !fresh(filepath.Join(dir, name))
	}
	for _, suf := range []string{"", ".task", ".pin", ".env", ".sequence"} {
		stale = stale && !fresh(filepath.Join(dir, id+suf))
	}
	for _, orig := range renamed {
		if !stale {
			if _, err := os.Stat(orig); errors.Is(err, fs.ErrNotExist) && os.Rename(orig+".gc", orig) == nil {
				continue
			}
		}
		os.Remove(orig + ".gc")
	}
}

// dropDeadLinks removes session links (the bare <session-id> files) whose run folders are all gone, and returns how many. It also removes <pid>.pid records older than pidRecordAge, and the per-session files of sessions idle past sessionFileAge (see sweepSessionFiles); those are not counted. Live links are only read. A link that looks dead is renamed to <id>.gc and judged again, so a concurrent rebind is restored rather than deleted. Only one sweeper runs at a time, under a non-blocking bridge-wide .sweep.lock (never removed); a call that can't take it skips the .gc removal and sweepSessionFiles and does the rest, so it never blocks. Lock order: .sweep.lock, then a session's .sequence.lock.
func dropDeadLinks(bridge string) int {
	dir := filepath.Join(bridge, ".starship", "sessions")
	entries, _ := os.ReadDir(dir)
	sweeper := flock.New(filepath.Join(dir, ".sweep.lock"))
	swept := false
	if ok, err := sweeper.TryLock(); ok && err == nil {
		defer sweeper.Unlock()
		swept = true
		sweepSessionFiles(bridge, entries)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		maxAge := staleGCAge
		switch {
		case strings.HasSuffix(e.Name(), ".gc"):
			if !swept {
				continue
			}
		case strings.HasSuffix(e.Name(), ".pid"):
			maxAge = pidRecordAge
		default:
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	for _, e := range entries {
		if e.IsDir() || strings.Contains(e.Name(), ".") {
			continue
		}
		orig := filepath.Join(dir, e.Name())
		if !deadLink(bridge, orig) {
			continue
		}
		tmp := orig + ".gc"
		if os.Rename(orig, tmp) != nil {
			continue
		}
		if deadLink(bridge, tmp) {
			if os.Remove(tmp) == nil {
				n++
			}
			continue
		}
		// Rebound in between: restore it unless a newer binding is already in place.
		if _, err := os.Stat(orig); errors.Is(err, fs.ErrNotExist) {
			if os.Rename(tmp, orig) != nil {
				os.Remove(tmp)
			}
		} else {
			os.Remove(tmp)
		}
	}
	return n
}

// cmdGC implements `ensign gc [<keep>]`.
func cmdGC(args []string, out io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: ensign gc [<keep>]")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	var keep int
	if len(args) == 1 {
		keep, err = strconv.Atoi(args[0])
		if err != nil || keep < 0 {
			return fmt.Errorf("usage: ensign gc [<keep>]  (<keep> is a number, 0 or more)")
		}
	} else {
		bf, err := loadBridgeFile(bridge)
		if err != nil {
			return err
		}
		if keep, err = bf.keepRuns(); err != nil {
			return err
		}
	}
	res := gcRuns(bridge, keep, 0)
	fmt.Fprintf(out, "removed %d runs, emptied %d scratch folders, dropped %d session links\n", res.Removed, res.Scratch, res.Links)
	return nil
}
