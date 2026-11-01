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

// gcRuns deletes finished runs beyond keep, oldest first, at most max of them (max <= 0 means no cap). It then empties the scratch folders of the finished runs that remain and drops session links to runs that no longer exist.
func gcRuns(bridge string, keep, max int) gcResult {
	var res gcResult
	held, ok := heldRuns(bridge)
	if !ok {
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
	if res.Removed > 0 {
		res.Links = dropDeadLinks(bridge)
	}
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
				if st.open() && st.Run != "" {
					held[st.Run] = true
				}
			}
		}
	}
	return held, true
}

// staleGCAge is how old a leftover <id>.gc file must be before a sweep removes it, so a concurrent pass's in-flight file is never touched.
const staleGCAge = 5 * time.Minute

// deadLinkRun returns the run a session link names and whether that run's folder is gone. A link that is empty, unreadable or names a path is not dead.
func deadLinkRun(bridge, file string) (run string, dead bool) {
	data, err := os.ReadFile(file)
	run = strings.TrimSpace(string(data))
	if err != nil || run == "" || strings.ContainsAny(run, `/\`) {
		return run, false
	}
	_, err = os.Stat(filepath.Join(runsDir(bridge), run))
	return run, errors.Is(err, fs.ErrNotExist)
}

// dropDeadLinks removes session links (the bare <session-id> files) whose run folder is gone, and returns how many. Live links are only read. A link that looks dead is renamed to <id>.gc and judged again, so a concurrent rebind is restored rather than deleted.
func dropDeadLinks(bridge string) int {
	dir := filepath.Join(bridge, ".starship", "sessions")
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gc") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleGCAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	for _, e := range entries {
		if e.IsDir() || strings.Contains(e.Name(), ".") {
			continue
		}
		orig := filepath.Join(dir, e.Name())
		if _, dead := deadLinkRun(bridge, orig); !dead {
			continue
		}
		tmp := orig + ".gc"
		if os.Rename(orig, tmp) != nil {
			continue
		}
		if _, dead := deadLinkRun(bridge, tmp); dead {
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
