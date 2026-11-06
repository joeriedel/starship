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
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// defaultKeepRuns is how many finished runs a bridge keeps when bridge.yaml has no keep-runs.
const defaultKeepRuns = 500

// sessionStartGCCap is how many deletes one session start attempts, failed ones included, so neither a first cleanup of a long-neglected bridge nor a set of undeletable folders can hold the start up. The delete attempts cost at most about 2s each on Windows, so 100s, well inside the 600s default hook timeout. The cap limits attempts only, not reads: a persistently unreadable state.json or pipeline.json can cost up to 2s per read on Windows with no attempt counted. Only folders that keep pipeline.json between starts can use the cap repeatedly, and `ensign gc` has no cap, so it reaches what they hold back.
const sessionStartGCCap = 50

// pipelineOnlyAge is how old a run folder's pipeline.json must be, with no state.json beside it, before gc takes the folder for a leftover. Age, because createRun writes pipeline.json and then state.json with no lock held between them, and five minutes outlasts that gap.
const pipelineOnlyAge = 5 * time.Minute

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
	Failed                  int  // delete attempts that left the folder in place
	More                    bool // candidates remain because the cap on delete attempts was reached
}

// finishedRun reports whether a run's state says success or error. Pending, running and ask-user runs are never finished.
func finishedRun(st *State) bool { return st.Status == "success" || st.Status == "error" }

// gcRuns first deletes pipeline-only run folders (pipeline.json but no state.json, see pipelineOnlyAge), then finished runs beyond keep, oldest first. It attempts at most max deletes in all, failed ones included (max <= 0 means no cap). It then empties the scratch folders of the finished runs it keeps (runs an open sequence step still needs are left alone) and drops session links to runs that no longer exist.
func gcRuns(bridge string, keep, max int) gcResult {
	var res gcResult
	held, ok := heldRuns(bridge)
	if !ok {
		res.Links = dropDeadLinks(bridge)
		return res
	}
	entries, _ := os.ReadDir(runsDir(bridge))
	var finished []string // newest first
	var pipelineOnly []string
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].IsDir() || held[entries[i].Name()] {
			continue
		}
		var st State
		dir := filepath.Join(runsDir(bridge), entries[i].Name())
		err := readJSON(filepath.Join(dir, "state.json"), &st)
		// A folder with neither file (a run creation that failed early, a crash mid-delete, or a failed delete, because os.RemoveAll removes all it can) is left alone on purpose. It costs no attempt. After a failed delete it holds only what couldn't be deleted, and reclaiming it would bring undeletable folders back into the cap at every start. A failed run creation or a crash mid-delete can leave deletable content (scratch, reports, skills) that stays until someone removes it by hand; that is accepted, because gc can't tell these apart from a failed delete, they are rare, and they cost no time. A state-only folder fails openRun in the second pass, likewise without an attempt.
		if err == nil && finishedRun(&st) {
			finished = append(finished, entries[i].Name())
		} else if errors.Is(err, fs.ErrNotExist) {
			if info, err := os.Stat(filepath.Join(dir, "pipeline.json")); err == nil && time.Since(info.ModTime()) > pipelineOnlyAge {
				pipelineOnly = append(pipelineOnly, entries[i].Name())
			}
		}
	}
	attempts := 0
	capped := func() bool {
		if max > 0 && attempts >= max {
			res.More = true
			return true
		}
		return false
	}
	remove := func(dir string) {
		attempts++
		removeRun(dir)
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			res.Removed++
		} else {
			res.Failed++
		}
	}
	for i := len(pipelineOnly) - 1; i >= 0; i-- {
		if capped() {
			break
		}
		dir := filepath.Join(runsDir(bridge), pipelineOnly[i])
		l := flock.New(filepath.Join(dir, "lock"))
		if ok, err := l.TryLock(); err != nil || !ok {
			continue
		}
		_, err := os.Stat(filepath.Join(dir, "state.json"))
		l.Unlock()
		if !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		remove(dir)
	}
	kept := finished
	var candidates []string
	if len(finished) > keep {
		kept, candidates = finished[:keep], finished[keep:]
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if capped() {
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
		remove(dir)
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
	files, err := filesIn(filepath.Join(bridge, ".starship", "sessions"), ".sequence")
	if err != nil {
		return nil, false
	}
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

// sessionFileAge is how long a session must be untouched before a sweep removes its <id>.task, .pin, .env, .sequence, .writes, .moved, .topic-*, .lock, .sequence.lock, .writes.lock, .sequence.tmp, .writes.tmp and .tmp files. Age, not liveness, because those files are never rewritten when a session ends; a session idle past it loses its task, topic memory, .pin (so the "bridge updated" check is skipped in createRun and startRun) and .env until its next run rebinds it.
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
	for _, suf := range []string{".task", ".pin", ".env", ".sequence", ".writes", ".moved"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf), true
		}
	}
	if id, _, ok := strings.Cut(name, ".topic-"); ok && id != "" {
		return id, true
	}
	return "", false
}

// sweepSessionFiles removes the per-session files of sessions untouched for sessionFileAge, then their lock and tmp files (see sweepLeftovers). A session's age is the newest mtime among its swept files and its link, because .pin is rewritten at every SessionStart while .task is not. A session with a live run or an open sequence keeps all its files. It must run before dead links are dropped, so a link still shows which runs a session owned.
func sweepSessionFiles(bridge string, entries []os.DirEntry) {
	leftovers := leftoverGroups(entries)
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
	sweepLeftovers(bridge, leftovers, func() []runInfo {
		if !listed {
			runs, listed = listRuns(bridge), true
		}
		return runs
	})
}

// leftoverOfFile returns the session id a <id>.lock, <id>.sequence.lock, <id>.writes.lock, <id>.sequence.tmp, <id>.writes.tmp or <id>.tmp file name belongs to. Names of the swept types, the bridge-wide .sweep.lock and files such as <id>.task.tmp (whose stem is itself a session file) are not leftovers.
func leftoverOfFile(name string) (string, bool) {
	if _, ok := sessionOfFile(name); ok || name == ".sweep.lock" {
		return "", false
	}
	for _, suf := range []string{".sequence.lock", ".writes.lock", ".sequence.tmp", ".writes.tmp", ".lock", ".tmp"} {
		id, found := strings.CutSuffix(name, suf)
		if !found {
			continue
		}
		if _, isSession := sessionOfFile(id); isSession && suf != ".sequence.lock" && suf != ".writes.lock" && suf != ".sequence.tmp" && suf != ".writes.tmp" {
			return "", false
		}
		return id, id != ""
	}
	return "", false
}

// sweptLeftovers is one session's lock and tmp files.
type sweptLeftovers struct {
	seqLock, writesLock, lock, tmp, seqTmp, writesTmp bool
	newest                                            time.Time
	failed                                            bool
}

// leftoverGroups finds the sessions with lock or tmp files in the snapshot and reads their ages. It runs before the swept files are renamed, because an entry's Info fails once its file is gone.
func leftoverGroups(entries []os.DirEntry) map[string]*sweptLeftovers {
	groups := map[string]*sweptLeftovers{}
	note := func(g *sweptLeftovers, e os.DirEntry) {
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
		id, ok := leftoverOfFile(e.Name())
		if !ok {
			continue
		}
		g := groups[id]
		if g == nil {
			g = &sweptLeftovers{}
			groups[id] = g
		}
		switch {
		case strings.HasSuffix(e.Name(), ".sequence.tmp"):
			g.seqTmp = true
		case strings.HasSuffix(e.Name(), ".writes.tmp"):
			g.writesTmp = true
		case strings.HasSuffix(e.Name(), ".sequence.lock"):
			g.seqLock = true
		case strings.HasSuffix(e.Name(), ".writes.lock"):
			g.writesLock = true
		case strings.HasSuffix(e.Name(), ".lock"):
			g.lock = true
		default:
			g.tmp = true
		}
		note(g, e)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := sessionOfFile(e.Name())
		if !ok {
			id = e.Name()
		}
		if g := groups[id]; g != nil {
			note(g, e)
		}
	}
	return groups
}

// sweepLeftovers removes the <id>.lock, <id>.sequence.lock, <id>.writes.lock, <id>.sequence.tmp, <id>.writes.tmp and <id>.tmp files of sessions whose newest file, link included, is older than sessionFileAge and that are not busy. A lock's own mtime is its creation time, so the session's other files and link show its use. Every lock is taken without blocking and the session is left for a later sweep if one is held; <id>.lock is also taken for a <id>.tmp, because its only writer holds it. <id>.sequence.lock is taken for a <id>.sequence.tmp as well, because its only writer holds it; it is created if missing and removed again with the other locks, and a .sequence.tmp alone never takes <id>.lock. <id>.writes.lock is taken the same way for a <id>.writes.tmp, because withWrites holds it across the write.
func sweepLeftovers(bridge string, groups map[string]*sweptLeftovers, listRunsOnce func() []runInfo) {
	dir := filepath.Join(bridge, ".starship", "sessions")
	for id, g := range groups {
		if g.failed || time.Since(g.newest) <= sessionFileAge || sessionBusy(bridge, id, listRunsOnce()) {
			continue
		}
		var locks []*flock.Flock
		var paths []string
		if g.seqLock || g.seqTmp {
			paths = append(paths, filepath.Join(dir, id+".sequence.lock"))
		}
		if g.writesLock || g.writesTmp {
			paths = append(paths, filepath.Join(dir, id+".writes.lock"))
		}
		if g.lock || g.tmp {
			paths = append(paths, filepath.Join(dir, id+".lock"))
		}
		held := true
		for _, path := range paths {
			l := flock.New(path)
			if ok, err := l.TryLock(); err != nil || !ok {
				held = false
				break
			}
			locks = append(locks, l)
		}
		if !held {
			for _, l := range locks {
				l.Unlock()
			}
			continue
		}
		if g.tmp {
			os.Remove(filepath.Join(dir, id+".tmp"))
		}
		if g.seqTmp {
			os.Remove(filepath.Join(dir, id+".sequence.tmp"))
		}
		if g.writesTmp {
			os.Remove(filepath.Join(dir, id+".writes.tmp"))
		}
		var left []string
		for _, path := range paths {
			if os.Remove(path) != nil {
				left = append(left, path)
			}
		}
		for _, l := range locks {
			l.Unlock()
		}
		// Windows refuses to remove a file we still hold open, so those are retried once the locks are closed.
		for _, path := range left {
			os.Remove(path)
		}
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
	for _, suf := range []string{"", ".task", ".pin", ".env", ".sequence", ".writes", ".moved"} {
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

// dropDeadLinks removes session links (the bare <session-id> files) whose run folders are all gone, and returns how many. It also removes <pid>.pid records older than pidRecordAge, and the per-session files, locks included, of sessions idle past sessionFileAge (see sweepSessionFiles); those are not counted. Live links are only read. A link that looks dead is renamed to <id>.gc and judged again, so a concurrent rebind is restored rather than deleted. Only one sweeper runs at a time, under a non-blocking bridge-wide .sweep.lock (never removed); a call that can't take it skips the .gc removal and sweepSessionFiles and does the rest, so it never blocks. Lock order: .sweep.lock, then a session's .sequence.lock, then its .lock.
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
		case strings.HasSuffix(e.Name(), ".pid"), strings.HasSuffix(e.Name(), ".connectors"):
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
