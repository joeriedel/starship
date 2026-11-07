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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// defaultKeepRuns is how many finished runs a bridge keeps when bridge.yaml has no keep-runs.
const defaultKeepRuns = 500

// sessionStartGCCap is how many deletes one session start attempts, failed ones included, so neither a first cleanup of a long-neglected bridge nor a set of undeletable folders can hold the start up. The failed delete attempts share one retry budget of about 2s on Windows, so they cost about that in all, well inside the 600s default hook timeout. The cap limits attempts only, not reads: a persistently unreadable state.json or pipeline.json can cost up to 2s per read on Windows with no attempt counted. Only folders that keep pipeline.json between starts can use the cap repeatedly, and `ensign gc` has no cap, so it reaches what they hold back.
const sessionStartGCCap = 50

// pipelineOnlyAge is how old a run folder's pipeline.json must be, with no state.json beside it, before gc takes the folder for a leftover. createRun holds the run's lock while it writes the two files and gc skips a folder whose lock is held, so age is the backstop for a creation that crashed between them.
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

// The file operations of a cleanup go through these variables so a test can fail them.
var (
	gcRemove    = os.Remove
	gcRename    = os.Rename
	gcRemoveAll = os.RemoveAll
	gcReadDir   = os.ReadDir
	gcRemoveRun = removeRunBy
	gcStat      = os.Stat
	gcReadJSON  = readJSON
	gcTryLock   = func(l *flock.Flock) (bool, error) { return l.TryLock() }
)

// gcResult is what a cleanup did.
type gcResult struct {
	Removed, Scratch, Links int
	Records                 int  // publish records of closed or vanished tasks removed
	RecordsFailed           int  // publish records that could not be judged or removed
	Failed                  int  // delete attempts that left the folder in place
	OpenFailed              int  // run folders due for deletion that could not be locked or opened, so were not judged
	ScratchFailed           int  // kept finished runs whose scratch folder could not be read or fully emptied
	SessionFilesFailed      int  // failed operations on session files (lock, read, remove, rename or restore)
	ListFailed              bool // the runs folder could not be listed, so no run was considered for deletion
	HeldFailed              bool // the held sessions could not be listed, so no run was deleted and no scratch folder emptied
	SessionsListFailed      bool // the sessions folder could not be listed, so no session file was considered
	More                    bool // candidates remain because the cap on delete attempts was reached
}

// failed reports whether the cleanup left something undone that it should have done.
func (r gcResult) failed() bool {
	return r.Failed > 0 || r.OpenFailed > 0 || r.ScratchFailed > 0 || r.SessionFilesFailed > 0 || r.RecordsFailed > 0 || r.ListFailed || r.HeldFailed || r.SessionsListFailed
}

// addLinks copies a session sweep into the result.
func (r *gcResult) addLinks(sw linkSweep) {
	r.Links = sw.Links
	r.SessionFilesFailed = sw.Failed
	r.SessionsListFailed = sw.ListFailed
}

// addRecords copies a publish record sweep into the result.
func (r *gcResult) addRecords(sw recordSweep) {
	r.Records = sw.Removed
	r.RecordsFailed = sw.Failed
}

// finishedRun reports whether a run's state says success or error. Pending, running and ask-user runs are never finished.
func finishedRun(st *State) bool { return st.Status == "success" || st.Status == "error" }

// gcRuns first deletes pipeline-only run folders (pipeline.json but no state.json, see pipelineOnlyAge), then finished runs beyond keep, oldest first. It attempts at most max deletes in all, failed ones included (max <= 0 means no cap). It then empties the scratch folders of the finished runs it keeps (runs an open sequence step still needs are left alone) and drops session links to runs that no longer exist.
func gcRuns(bridge string, keep, max int) gcResult {
	var res gcResult
	held, err := heldRuns(bridge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list the held sessions, so no run was deleted and no scratch folder emptied: %v\n", err)
		res.HeldFailed = true
		res.addLinks(dropDeadLinks(bridge))
		res.addRecords(sweepClosedRecords(bridge))
		return res
	}
	entries, err := os.ReadDir(runsDir(bridge))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && plainFileOnPath(runsDir(bridge)) == "" {
		fmt.Fprintf(os.Stderr, "warning: could not list the runs in %s: %v\n", runsDir(bridge), err)
		res.ListFailed = true
	}
	var finished []string // newest first
	var pipelineOnly []string
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].IsDir() || held[entries[i].Name()] {
			continue
		}
		var st State
		dir := filepath.Join(runsDir(bridge), entries[i].Name())
		err := readJSON(filepath.Join(dir, "state.json"), &st)
		// A folder with neither file (a run creation that failed early, a crash mid-delete, or a failed delete, because os.RemoveAll removes all it can) is left alone on purpose. It costs no attempt. After a failed delete it holds only what couldn't be deleted, and reclaiming it would bring undeletable folders back into the cap at every start. A failed run creation or a crash mid-delete can leave deletable content (scratch, reports, skills) that stays until someone removes it by hand; that is accepted, because gc can't tell these apart from a failed delete, they are rare, and they cost no time. A state-only finished folder is retried in the second pass and counts as an attempt; one a failed delete left behind counts as Failed each time, under the cap and the budget.
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
	// Only failed deletes spend the budget, so a persistent failure can't hold the start up but a healthy cleanup never starves a later delete of its retries.
	budget := retryWindow
	remove := func(dir string) {
		attempts++
		start := time.Now()
		gcRemoveRun(dir, start.Add(budget))
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			res.Removed++
		} else {
			budget -= time.Since(start)
			res.Failed++
		}
	}
	// skipped counts a run folder that could not be locked or opened. It stays silent for a folder already gone; a folder caught mid-delete by another gc counts once and clears at the next gc.
	skipped := func(dir string, err error) {
		if _, serr := os.Stat(dir); errors.Is(serr, fs.ErrNotExist) {
			return
		}
		fmt.Fprintf(os.Stderr, "warning: could not open run folder %s: %v\n", dir, err)
		res.OpenFailed++
	}
	for i := len(pipelineOnly) - 1; i >= 0; i-- {
		if capped() {
			break
		}
		dir := filepath.Join(runsDir(bridge), pipelineOnly[i])
		l := flock.New(filepath.Join(dir, "lock"))
		if ok, err := l.TryLock(); err != nil || !ok {
			if err != nil {
				skipped(dir, err)
			}
			continue
		}
		// A save mid-replace holds this lock, so past TryLock state.json is settled and the unlocked first-pass read needs no not-found retry.
		_, err := gcStat(filepath.Join(dir, "state.json"))
		l.Unlock()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			skipped(dir, err)
			continue
		}
		if err == nil {
			continue
		}
		remove(dir)
	}
	kept := finished
	var candidates []string
	if len(finished) > keep {
		kept, candidates = finished[:keep], finished[keep:]
	}
	// A cleared run is held back while a .tested marker would let a landing pass over it; the markers are read once, when the first cleared candidate turns up.
	var markerRuns map[string]bool
	var markerErr error
	markersLoaded := false
	for i := len(candidates) - 1; i >= 0; i-- {
		if capped() {
			break
		}
		dir := filepath.Join(runsDir(bridge), candidates[i])
		var st State
		if r, err := openRun(bridge, candidates[i]); err == nil {
			st = *r.State
			r.close()
		} else {
			// A run whose files can't be used still goes when its state says it is finished.
			l := flock.New(filepath.Join(dir, "lock"))
			ok, lerr := l.TryLock()
			if lerr != nil {
				skipped(dir, lerr)
				continue
			}
			if !ok {
				continue
			}
			rerr := readRunState(dir, &st)
			l.Unlock()
			if rerr != nil {
				skipped(dir, rerr)
				continue
			}
		}
		if !finishedRun(&st) {
			continue
		}
		if st.Reason == clearedReason {
			if !markersLoaded {
				markerRuns, markerErr = markerCountedRuns(bridge)
				markersLoaded = true
			}
			if markerErr != nil {
				skipped(dir, fmt.Errorf("a cleared run is kept because the tested records could not be checked: %w", markerErr))
				continue
			}
			if markerRuns[candidates[i]] {
				continue
			}
		}
		remove(dir)
	}
	deadline := time.Now().Add(retryWindow)
	for _, id := range kept {
		emptied, failed := emptyScratchIfFinished(bridge, id, deadline)
		if emptied {
			res.Scratch++
		}
		if failed {
			res.ScratchFailed++
		}
	}
	res.addLinks(dropDeadLinks(bridge))
	res.addRecords(sweepClosedRecords(bridge))
	return res
}

// emptyScratchIfFinished empties a run's scratch folder when the run is finished and the folder holds something. It reports whether it emptied it and whether the folder could not be read or fully emptied. A run that can't be opened, or isn't finished, is busy, not a failure.
func emptyScratchIfFinished(bridge, id string, deadline time.Time) (emptied, failed bool) {
	scratch := filepath.Join(runsDir(bridge), id, "scratch")
	items, err := os.ReadDir(scratch)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false
		}
		fmt.Fprintf(os.Stderr, "warning: could not empty %s: %v\n", scratch, err)
		return false, true
	}
	if len(items) == 0 {
		return false, false
	}
	r, err := openRun(bridge, id)
	if err != nil {
		return false, false
	}
	done := finishedRun(r.State)
	r.close()
	if !done {
		return false, false
	}
	return emptyDir(scratch, deadline)
}

// emptyDir removes what is inside a folder and keeps the folder. It reports whether it emptied something and whether anything failed (a missing folder is not a failure). Each failure also warns on stderr. A transient failure is retried until deadline, which callers share across folders so a persistent failure on Windows cannot stall them; after it each item gets one try.
func emptyDir(dir string, deadline time.Time) (emptied, failed bool) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false
		}
		fmt.Fprintf(os.Stderr, "warning: could not empty %s: %v\n", dir, err)
		return false, true
	}
	for _, it := range items {
		path := filepath.Join(dir, it.Name())
		if err := retry(time.Until(deadline), isTransientFileError, func() error { return gcRemoveAll(path) }); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not empty %s: %v\n", dir, err)
			failed = true
		}
	}
	return !failed && len(items) > 0, failed
}

// heldRuns returns the runs that open sequence steps still need: a failed step waits on its run for a retry, and a step left active points at its run. The error names what could not be read, and then gc must not delete anything because it can't tell which runs are needed.
func heldRuns(bridge string) (held map[string]bool, err error) {
	held = map[string]bool{}
	files, err := filesIn(filepath.Join(bridge, ".starship", "sessions"), ".sequence")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		var sf seqFile
		if err := readSeqFile(f, &sf); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
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
	return held, nil
}

// staleGCAge is how old a leftover <id>.gc file must be before a sweep removes it. A rename keeps the mtime, so this holds only because dropDeadLinks lets one sweeper run at a time. It applies to dead-link and unknown .gc files; a session file's .gc is judged by its session instead (see retryKeptGC).
const staleGCAge = 5 * time.Minute

// pidRecordAge is how old a <pid>.pid record must be before a sweep removes it. Age, not liveness, because a session idle past it loses only the link to its earlier id, which is how /clear behaved before the record existed.
const pidRecordAge = 30 * 24 * time.Hour

// sessionFileAge is how long a session must be untouched before a sweep removes its <id>.task, .pin, .env, .sequence, .sequence.corrupt, .writes, .direct, .moved, .topic-*, .lock, .sequence.lock, .writes.lock, .sequence.tmp, .writes.tmp and .tmp files. Age, not liveness, because those files are never rewritten when a session ends; a session idle past it loses its task, topic memory, .pin (so the "bridge updated" check is skipped in createRun and startRun) and .env until its next run rebinds it.
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
		runPath := filepath.Join(runsDir(bridge), id)
		if _, err := gcStat(runPath); !reallyMissing(runPath, err) {
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
	for _, suf := range []string{".task", ".pin", ".env", ".lines", ".sequence", ".sequence.corrupt", ".writes", ".direct", ".moved"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf), true
		}
	}
	if id, _, ok := strings.Cut(name, ".topic-"); ok && id != "" {
		return id, true
	}
	return "", false
}

// linkSweep is what dropDeadLinks did.
type linkSweep struct {
	Links      int  // dead session links removed
	Failed     int  // failed operations on session files (lock, read, remove, rename or restore)
	ListFailed bool // the sessions folder could not be listed
}

// sessionSweep carries the retry deadline of one dropDeadLinks pass, shared so a persistent Windows failure cannot stall the pass, and counts the failures.
type sessionSweep struct {
	deadline time.Time
	failed   int
	readWarn map[string]bool // entries whose unreadable Info was already reported
}

// tryLock takes a lock without blocking and reports whether it is held. A lock that is busy is quiet; an error is reported and counted, except a missing sessions folder.
func (s *sessionSweep) tryLock(l *flock.Flock) bool {
	ok, err := gcTryLock(l)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: could not lock %s: %v\n", l.Path(), err)
			s.failed++
		}
		return false
	}
	return ok
}

// entryInfo is e.Info() for an entry of dir. An error other than a vanished file is reported and counted once per entry in a sweep, however many groups look at it.
func (s *sessionSweep) entryInfo(dir string, e os.DirEntry) (fs.FileInfo, error) {
	info, err := e.Info()
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !s.readWarn[e.Name()] {
		if s.readWarn == nil {
			s.readWarn = map[string]bool{}
		}
		s.readWarn[e.Name()] = true
		fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", filepath.Join(dir, e.Name()), err)
		s.failed++
	}
	return info, err
}

// remove removes a file, retrying a transient failure until the deadline. A file that is already gone (another gc got there first) is not a failure. It reports whether the file was removed.
func (s *sessionSweep) remove(path string) bool {
	err := retry(time.Until(s.deadline), isTransientFileError, func() error { return gcRemove(path) })
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", path, err)
		s.failed++
	}
	return false
}

// rename is remove for os.Rename.
func (s *sessionSweep) rename(oldpath, newpath string) bool {
	err := retry(time.Until(s.deadline), isTransientFileError, func() error { return gcRename(oldpath, newpath) })
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warning: could not rename %s: %v\n", oldpath, err)
		s.failed++
	}
	return false
}

// taskOfFile is the <project>/<task> name of a task.md path.
func taskOfFile(f string) string {
	d := filepath.Dir(f)
	return filepath.Base(filepath.Dir(filepath.Dir(d))) + "/" + filepath.Base(d)
}

// taskIsClosed reports whether the task.md at f has the status closed.
func taskIsClosed(f string) (bool, error) {
	var tf struct {
		Status string `yaml:"status"`
	}
	front, _, err := splitFrontmatter(f)
	if err == nil {
		err = decodeFrontmatter(front, &tf)
	}
	return tf.Status == "closed", err
}

// recordSweep is what sweepClosedRecords did.
type recordSweep struct {
	Removed int // record files removed
	Failed  int // listings, reads, locks and removals that went wrong
}

// sweepClosedRecords removes the .publish, .tested and @<location>.tested records of closed tasks, which close-time cleanup missed or a run finishing after the close recreated, and the .publish, .tested and .base records (with @<location> forms) of tasks that no longer exist. A task name can't be recovered from an encoded file name, so each record is matched forward from the tasks on disk. An encoding is swept only when every task encoding to it is closed, none has a live run, and its publish lock is free; a busy lock is retried by the next gc. A closed task's .base stays, because nothing stops a squash after the close. A vanished task's records are swept only once the task has been missing for sessionFileAge, because a task can be absent briefly (a bridge checkout of another commit) and this also runs at session start; a stale .publish or .base would otherwise change a task re-created under that name. Absence is timed by an empty <enc>.vanished marker, created when the records are first seen without a task (nothing is swept on that pass) and removed with the records (kept while any record's removal failed, so the next gc retries it), or when the task is back or the records are gone; a record's own age says nothing about how long its task has been missing. Lock files (.publish.lock, .journal.lock, the landings' and the updates') are never removed: unlinking a flock file can give two holders, other code re-creates them, and they are empty.
func sweepClosedRecords(bridge string) recordSweep {
	var sw recordSweep
	dir := filepath.Join(bridge, ".starship", "tasks")
	entries, err := gcReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && plainFileOnPath(dir) == "" {
			fmt.Fprintf(os.Stderr, "warning: could not list the publish records in %s: %v\n", dir, err)
			sw.Failed++
		}
		return sw
	}
	records := map[string][]string{} // encoded task name -> its record file names
	markers := map[string]bool{}     // encoded task names with a .vanished marker
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var enc string
		if base, ok := strings.CutSuffix(name, ".vanished"); ok {
			markers[base] = true
			continue
		}
		if base, ok := strings.CutSuffix(name, ".publish"); ok {
			enc = base
		} else {
			base, ok := strings.CutSuffix(name, ".tested")
			if !ok {
				base, ok = strings.CutSuffix(name, ".base")
			}
			if !ok {
				continue
			}
			var loc string
			var hasLoc bool
			if enc, loc, hasLoc = strings.Cut(base, "@"); hasLoc && !validName(loc) {
				continue
			}
		}
		records[enc] = append(records[enc], name)
	}
	if len(records) == 0 && len(markers) == 0 {
		return sw
	}
	s := &sessionSweep{deadline: time.Now().Add(retryWindow)}
	marker := func(enc string) string { return filepath.Join(dir, enc+".vanished") }
	for enc := range markers {
		if records[enc] == nil && s.remove(marker(enc)) {
			delete(markers, enc)
		}
	}
	if len(records) == 0 {
		sw.Failed += s.failed
		return sw
	}
	files, err := taskFiles(bridge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list every task, so no publish record was removed: %v\n", err)
		sw.Failed++
		sw.Failed += s.failed
		return sw
	}
	tasks := map[string]string{} // encoded task name -> a task encoding to it
	open := map[string]bool{}
	for _, f := range files {
		task := taskOfFile(f)
		enc := strings.ReplaceAll(task, "/", "--")
		if records[enc] == nil {
			continue
		}
		tasks[enc] = task
		closed, err := taskIsClosed(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read the status of %s, so its publish records are kept: %v\n", task, err)
			sw.Failed++
			open[enc] = true
		} else if !closed {
			open[enc] = true
		}
	}
	var encs, orphans []string
	for enc := range records {
		if _, ok := tasks[enc]; !ok {
			orphans = append(orphans, enc)
		} else if !open[enc] {
			encs = append(encs, enc)
		}
	}
	s.deadline = time.Now().Add(retryWindow)
	for enc := range markers {
		if tasks[enc] != "" {
			s.remove(marker(enc))
		}
	}
	if len(encs) == 0 && len(orphans) == 0 {
		sw.Failed += s.failed
		return sw
	}
	sort.Strings(encs)
	sort.Strings(orphans)
	live, err := liveRuns(bridge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list the live runs, so no publish record was removed: %v\n", err)
		sw.Failed++
		sw.Failed += s.failed
		return sw
	}
	liveEnc := map[string]bool{}
	for _, ri := range live {
		liveEnc[strings.ReplaceAll(ri.snap.Task, "/", "--")] = true
	}
	s.deadline = time.Now().Add(retryWindow)
	for _, enc := range encs {
		if liveEnc[enc] {
			continue
		}
		l := flock.New(taskStateFile(bridge, tasks[enc], "", ".publish.lock"))
		if !s.tryLock(l) {
			continue
		}
		for _, name := range records[enc] {
			if !strings.HasSuffix(name, ".base") && s.remove(filepath.Join(dir, name)) {
				sw.Removed++
			}
		}
		l.Unlock()
	}
	// vanishedLongEnough reports whether enc's marker is older than sessionFileAge; an unreadable marker counts as not.
	vanishedLongEnough := func(enc string) bool {
		info, err := gcStat(marker(enc))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", marker(enc), err)
				s.failed++
			}
			return false
		}
		return time.Since(info.ModTime()) > sessionFileAge
	}
	for _, enc := range orphans {
		if !markers[enc] {
			f, err := os.OpenFile(marker(enc), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err == nil {
				f.Close()
			} else if !errors.Is(err, fs.ErrExist) {
				fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", marker(enc), err)
				s.failed++
			}
			continue
		}
		if liveEnc[enc] || !vanishedLongEnough(enc) {
			continue
		}
		l := flock.New(taskStateFile(bridge, enc, "", ".publish.lock"))
		if !s.tryLock(l) {
			continue
		}
		if vanishedLongEnough(enc) {
			failedBefore := s.failed
			for _, name := range records[enc] {
				if s.remove(filepath.Join(dir, name)) {
					sw.Removed++
				}
			}
			if s.failed == failedBefore {
				s.remove(marker(enc))
			}
		}
		l.Unlock()
	}
	sw.Failed += s.failed
	return sw
}

// sweepSessionFiles removes the per-session files of sessions untouched for sessionFileAge, then their lock and tmp files (see sweepLeftovers). A session's age is the newest mtime among its swept files and its link, because .pin is rewritten at every SessionStart while .task is not. A session with a live run or an open sequence keeps all its files. It must run before dead links are dropped, so a link still shows which runs a session owned.
func sweepSessionFiles(bridge string, entries []os.DirEntry, s *sessionSweep) {
	dir := filepath.Join(bridge, ".starship", "sessions")
	leftovers := leftoverGroups(dir, entries, s)
	groups := map[string]*sweptSession{}
	note := func(g *sweptSession, e os.DirEntry) {
		info, err := s.entryInfo(dir, e)
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
	var listErr error
	listed := false
	for id, g := range groups {
		if g.failed || time.Since(g.newest) <= sessionFileAge {
			continue
		}
		if g.hasSeq {
			l := flock.New(seqPath(bridge, id) + ".lock")
			if !s.tryLock(l) {
				continue
			}
			if !listed {
				runs, listErr = listRuns(bridge)
				listed = true
			}
			if listErr == nil {
				sweepSession(bridge, id, g, runs, s)
			}
			l.Unlock()
			continue
		}
		if !listed {
			runs, listErr = listRuns(bridge)
			listed = true
		}
		if listErr == nil {
			sweepSession(bridge, id, g, runs, s)
		}
	}
	sweepLeftovers(bridge, leftovers, s, func() ([]runInfo, error) {
		if !listed {
			runs, listErr = listRuns(bridge)
			listed = true
		}
		return runs, listErr
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
func leftoverGroups(dir string, entries []os.DirEntry, s *sessionSweep) map[string]*sweptLeftovers {
	groups := map[string]*sweptLeftovers{}
	note := func(g *sweptLeftovers, e os.DirEntry) {
		info, err := s.entryInfo(dir, e)
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
func sweepLeftovers(bridge string, groups map[string]*sweptLeftovers, s *sessionSweep, listRunsOnce func() ([]runInfo, error)) {
	dir := filepath.Join(bridge, ".starship", "sessions")
	for id, g := range groups {
		if g.failed || time.Since(g.newest) <= sessionFileAge {
			continue
		}
		// A session is only swept once the runs are known not to hold it.
		runs, err := listRunsOnce()
		if err != nil || sessionBusy(bridge, id, runs) {
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
			if !s.tryLock(l) {
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
			s.remove(filepath.Join(dir, id+".tmp"))
		}
		if g.seqTmp {
			s.remove(filepath.Join(dir, id+".sequence.tmp"))
		}
		if g.writesTmp {
			s.remove(filepath.Join(dir, id+".writes.tmp"))
		}
		var left []string
		for _, path := range paths {
			if gcRemove(path) != nil {
				left = append(left, path)
			}
		}
		for _, l := range locks {
			l.Unlock()
		}
		// Windows refuses to remove a file we still hold open, so those are retried once the locks are closed.
		for _, path := range left {
			s.remove(path)
		}
	}
}

// sweepSession removes a stale session's files unless it is busy.
func sweepSession(bridge, id string, g *sweptSession, runs []runInfo, s *sessionSweep) {
	if sessionBusy(bridge, id, runs) {
		return
	}
	dir := filepath.Join(bridge, ".starship", "sessions")
	reapSession(dir, id, g.files, s)
}

// sessionBusy reports whether a session has an open sequence or a run that has not finished, found through its link or through the run's owner. Anything unreadable counts as busy.
func sessionBusy(bridge, id string, runs []runInfo) bool {
	var sf seqFile
	if err := readSeqFile(seqPath(bridge, id), &sf); err != nil {
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
		statePath := filepath.Join(runsDir(bridge), run, "state.json")
		err := gcReadJSON(statePath, &st)
		if reallyMissing(statePath, err) {
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
func reapSession(dir, id string, files []string, s *sessionSweep) {
	var renamed []string
	for _, name := range files {
		orig := filepath.Join(dir, name)
		if s.rename(orig, orig+".gc") {
			renamed = append(renamed, orig)
		}
	}
	settleSession(dir, id, files, renamed, s)
}

// settleSession judges a session whose files were renamed to .gc: it removes them all if the session is still stale, and otherwise restores each unless a newer file holds its name.
func settleSession(dir, id string, files, renamed []string, s *sessionSweep) {
	var paths []string
	for _, orig := range renamed {
		paths = append(paths, orig+".gc")
	}
	for _, name := range files {
		paths = append(paths, filepath.Join(dir, name))
	}
	paths = append(paths, sessionIDPaths(dir, id)...)
	stale := staleSession(paths, s)
	for _, orig := range renamed {
		if stale {
			s.remove(orig + ".gc")
			continue
		}
		restoreGC(orig, s)
	}
}

// sessionIDPaths is the bare id and the fixed suffixes of a session's files.
func sessionIDPaths(dir, id string) []string {
	var paths []string
	for _, suf := range []string{"", ".task", ".pin", ".env", ".sequence", ".sequence.corrupt", ".writes", ".direct", ".moved"} {
		paths = append(paths, filepath.Join(dir, id+suf))
	}
	return paths
}

// staleSession reports whether none of the paths was modified within sessionFileAge. A path that is missing is not fresh. A path that can't be read is reported and counted, and the session is not stale, so nothing is removed on a failed read.
func staleSession(paths []string, s *sessionSweep) bool {
	for _, p := range paths {
		info, err := gcStat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", p, err)
			s.failed++
			return false
		}
		if time.Since(info.ModTime()) <= sessionFileAge {
			return false
		}
	}
	return true
}

// restoreGC puts orig.gc back unless a newer file holds the name, in which case the .gc is dropped.
func restoreGC(orig string, s *sessionSweep) {
	_, err := gcStat(orig)
	switch {
	case err == nil:
		s.remove(orig + ".gc")
	case errors.Is(err, fs.ErrNotExist):
		// A failed restore keeps the .gc, the only copy of a file in a session just judged live, unlike the dead-link restore, which drops it when the rename fails. A later sweep retries it (see retryKeptGC).
		s.rename(orig+".gc", orig)
	default:
		fmt.Fprintf(os.Stderr, "warning: could not restore %s: %v\n", orig, err)
		s.failed++
	}
}

// retryKeptGC judges a session that has session-file .gc files older than staleGCAge, left by an earlier sweep that kept them, and removes them if the session is now stale or restores them if it is not. The judgment covers every .gc and file of the session in the snapshot and its fixed names, read now because sweepSessionFiles may have changed them. A session with a .sequence is judged under its .sequence.lock, the lock sweepSessionFiles holds while reaping it; if that is held the .gc files are left for a later sweep.
func retryKeptGC(dir, id string, old []string, entries []os.DirEntry, s *sessionSweep) {
	var paths []string
	hasSeq := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if got, ok := sessionOfFile(strings.TrimSuffix(e.Name(), ".gc")); !ok || got != id {
			continue
		}
		hasSeq = hasSeq || e.Name() == id+".sequence" || e.Name() == id+".sequence.gc"
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	paths = append(paths, sessionIDPaths(dir, id)...)
	if hasSeq {
		l := flock.New(filepath.Join(dir, id+".sequence.lock"))
		if !s.tryLock(l) {
			return
		}
		defer l.Unlock()
	}
	stale := staleSession(paths, s)
	for _, gc := range old {
		if stale {
			s.remove(gc)
			continue
		}
		restoreGC(strings.TrimSuffix(gc, ".gc"), s)
	}
}

// dropDeadLinks removes session links (the bare <session-id> files) whose run folders are all gone, and returns how many, with how many operations on session files (lock, read, remove, rename or restore) failed. It also removes <pid>.pid records older than pidRecordAge, and the per-session files, locks included, of sessions idle past sessionFileAge (see sweepSessionFiles); those files do not count as links, but failures on them do. Live links are only read. A link that looks dead is renamed to <id>.gc and judged again, so a concurrent rebind is restored rather than deleted. Only one sweeper runs at a time, under a non-blocking bridge-wide .sweep.lock (never removed); a call that can't take it skips the .gc removal and sweepSessionFiles and does the rest, so it never blocks. A .gc of a session file older than staleGCAge is not removed on age: retryKeptGC judges its session and restores or removes it. Lock order: .sweep.lock, then a session's .sequence.lock, then its .lock.
func dropDeadLinks(bridge string) linkSweep {
	dir := filepath.Join(bridge, ".starship", "sessions")
	var sw linkSweep
	s := &sessionSweep{deadline: time.Now().Add(retryWindow)}
	entries, err := gcReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && plainFileOnPath(dir) == "" {
		fmt.Fprintf(os.Stderr, "warning: could not list the session files in %s: %v\n", dir, err)
		sw.ListFailed = true
	}
	sweeper := flock.New(filepath.Join(dir, ".sweep.lock"))
	swept := false
	if ok, err := gcTryLock(sweeper); err != nil {
		if !errors.Is(err, fs.ErrNotExist) && plainFileOnPath(dir) == "" {
			fmt.Fprintf(os.Stderr, "warning: could not lock %s: %v\n", sweeper.Path(), err)
			s.failed++
		}
	} else if ok {
		defer sweeper.Unlock()
		swept = true
		sweepSessionFiles(bridge, entries, s)
	}
	keptGC := map[string][]string{} // session id -> its old .gc files
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
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", filepath.Join(dir, e.Name()), err)
				s.failed++
			}
			continue
		}
		if time.Since(info.ModTime()) > maxAge {
			if id, ok := sessionOfFile(strings.TrimSuffix(e.Name(), ".gc")); ok && strings.HasSuffix(e.Name(), ".gc") {
				keptGC[id] = append(keptGC[id], filepath.Join(dir, e.Name()))
				continue
			}
			s.remove(filepath.Join(dir, e.Name()))
		}
	}
	for id, old := range keptGC {
		retryKeptGC(dir, id, old, entries, s)
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
		if !s.rename(orig, tmp) {
			continue
		}
		if deadLink(bridge, tmp) {
			if s.remove(tmp) {
				sw.Links++
			}
			continue
		}
		// Rebound in between: restore it unless a newer binding is already in place.
		_, err := gcStat(orig)
		switch {
		case err == nil:
			s.remove(tmp)
		case errors.Is(err, fs.ErrNotExist):
			err := retry(time.Until(s.deadline), isTransientFileError, func() error { return gcRename(tmp, orig) })
			if err != nil {
				before := s.failed
				s.remove(tmp)
				s.failed = before
				if !errors.Is(err, fs.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "warning: could not restore session link %s: %v\n", orig, err)
					s.failed++
				}
			}
		default:
			// The tmp stays, possibly the only copy of a live binding; as a bare-id .gc it is not retried by retryKeptGC but removed by the age sweep once older than staleGCAge.
			fmt.Fprintf(os.Stderr, "warning: could not restore session link %s: %v\n", orig, err)
			s.failed++
		}
	}
	sw.Failed = s.failed
	return sw
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
	fmt.Fprintf(out, "removed %d runs, emptied %d scratch folders, dropped %d session links", res.Removed, res.Scratch, res.Links)
	if res.Failed > 0 {
		fmt.Fprintf(out, ", could not delete %d runs", res.Failed)
	}
	if res.OpenFailed > 0 {
		fmt.Fprintf(out, ", could not open %d old runs", res.OpenFailed)
	}
	if res.ScratchFailed > 0 {
		fmt.Fprintf(out, ", could not empty %d scratch folders", res.ScratchFailed)
	}
	if res.ListFailed {
		fmt.Fprint(out, ", could not list the runs")
	}
	if res.HeldFailed {
		fmt.Fprint(out, ", could not list the held sessions")
	}
	if res.SessionsListFailed {
		fmt.Fprint(out, ", could not list the session files")
	}
	if res.SessionFilesFailed > 0 {
		fmt.Fprintf(out, ", could not sweep %d session files", res.SessionFilesFailed)
	}
	if res.Records > 0 {
		fmt.Fprintf(out, ", removed %d publish records of closed or vanished tasks", res.Records)
	}
	if res.RecordsFailed > 0 {
		fmt.Fprintf(out, ", could not sweep %d publish records of closed or vanished tasks", res.RecordsFailed)
	}
	fmt.Fprintln(out)
	if res.failed() {
		return errors.New("cleanup did not finish: see the warnings and counts above")
	}
	return nil
}
