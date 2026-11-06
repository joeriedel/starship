// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// applyDependencyBinding is the per-location binding that wires another location's unlanded branch into a side's workspace.
const applyDependencyBinding = "apply-dependency"

// appliedRecord is the name, in the workspace's git dir, of the record of what @apply-dependency touched.
const appliedRecord = "starship-dependency-applied"

// Dependency is another location's unlanded branch that a run builds and tests against.
type Dependency struct {
	Location string   `json:"location"`
	Branch   string   `json:"branch,omitempty"` // empty: the branch the run's own workspace is on when the dependency is applied
	Path     string   `json:"path"`             // the dependency's checkout on this machine
	Sides    []string `json:"sides"`            // the sides it is applied on
	Auto     bool     `json:"auto,omitempty"`   // added for the run's subtask, not named with --dependency
}

// label names the branch for people.
func (d Dependency) label() string {
	if d.Branch == "" {
		return "this run's branch"
	}
	return d.Branch
}

// parseDependencyFlag splits a --dependency value.
func parseDependencyFlag(v string) (location, branch string, err error) {
	location, branch, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(location) == "" || strings.TrimSpace(branch) == "" {
		return "", "", fmt.Errorf("--dependency needs <location>=<branch>, got %q", v)
	}
	return strings.TrimSpace(location), strings.TrimSpace(branch), nil
}

// hasApplyBinding says a binding set declares @apply-dependency in any form ensign can act on.
func hasApplyBinding(bs map[string]Binding) bool {
	b, ok := bs[applyDependencyBinding]
	return ok && (b.Command != "" || b.NotApplicable != "")
}

// checkDependencyCheckout refuses a dependency whose checkout is not on the named branch: the branch is applied from what that folder holds.
func checkDependencyCheckout(d Dependency) error {
	cur := currentBranch(d.Path)
	if cur == d.Branch {
		return nil
	}
	on := "on branch " + cur
	if cur == "" {
		on = "on a detached HEAD"
	}
	return fmt.Errorf("dependency %s: the checkout %s is %s, not on %s; check out %s there first", d.Location, d.Path, on, d.Branch, d.Branch)
}

// resolveDependencies works out the dependencies of the run: the branches named with --dependency, the automatic ones (the same form, found for the run's subtask), then the run's own location for each dependent side.
func (c *Config) resolveDependencies(bridge string, flags, auto []string) error {
	if len(flags) > 0 && c.Location == "" {
		return fmt.Errorf("--dependency has no meaning for a run without a location: the project declares none, or the run names none")
	}
	seen := map[string]bool{}
	for _, f := range append(slices.Clone(flags), auto...) {
		loc, branch, err := parseDependencyFlag(f)
		if err != nil {
			return err
		}
		dependents := c.Locations.Dependents(loc)
		switch {
		case !c.Locations.Has(loc):
			return fmt.Errorf("--dependency: project %s declares no location %q (it declares %s)", c.Project, loc, strings.Join(c.Locations.Names(), ", "))
		case loc == c.Location:
			return fmt.Errorf("--dependency: %s is the location this run edits; name a location it depends on", loc)
		case !contains(dependents, c.Location):
			return fmt.Errorf("--dependency: %s does not depend on %s, so its branch is not part of this run's build", c.Location, loc)
		case seen[loc]:
			return fmt.Errorf("--dependency: %s is named twice", loc)
		}
		seen[loc] = true
		p, err := locationPathFor(bridge, c.Project, loc)
		if err != nil {
			return err
		}
		if _, err := gitOut(p, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
			return fmt.Errorf("--dependency: location %s has no branch %q in %s", loc, branch, p)
		}
		dep := Dependency{Location: loc, Branch: branch, Path: p, Auto: slices.Contains(auto, f)}
		if err := checkDependencyCheckout(dep); err != nil {
			return err
		}
		for _, sd := range c.Sides {
			if contains(dependents, sd.Location) {
				dep.Sides = append(dep.Sides, sd.Location)
			}
		}
		c.Dependencies = append(c.Dependencies, dep)
	}
	if len(c.Sides) > 1 {
		own := Dependency{Location: c.Location, Path: c.Workspace}
		for _, sd := range c.Sides[1:] {
			own.Sides = append(own.Sides, sd.Location)
		}
		c.Dependencies = append(c.Dependencies, own)
	}
	var lacking []string
	for _, sd := range c.Sides {
		mine := dependenciesFor(c.Dependencies, sd.Location)
		if len(mine) == 0 || hasApplyBinding(sd.Bindings) {
			continue
		}
		var names []string
		for _, d := range mine {
			names = append(names, "the branch of "+d.Location)
		}
		lacking = append(lacking, fmt.Sprintf("%s (needs %s)", sd.Location, strings.Join(names, ", ")))
	}
	if len(lacking) > 0 {
		return fmt.Errorf("these sides need another location's branch applied but have no @%s binding: %s; add @%s to that location (or `not-applicable: <reason>` when it already builds from the local checkout), or narrow --sides", applyDependencyBinding, strings.Join(lacking, ", "), applyDependencyBinding)
	}
	return nil
}

// automaticDependencies names, as --dependency values, the task branches that earlier subtasks left in the locations this run depends on. doneLocs are the locations of the task's earlier done subtasks; flags are the dependencies the session already named, which win. A branch that is missing (published) or has no commits its base lacks is left out.
func (c *Config) automaticDependencies(bridge string, doneLocs, flags []string) []string {
	if c.Locations == nil || c.Location == "" {
		return nil
	}
	branch := taskBranch(c.Task)
	named := map[string]bool{}
	for _, f := range flags {
		if loc, _, err := parseDependencyFlag(f); err == nil {
			named[loc] = true
		}
	}
	var out []string
	for _, loc := range doneLocs {
		if named[loc] || loc == c.Location || !contains(c.Locations.Dependents(loc), c.Location) {
			continue
		}
		named[loc] = true
		p, err := locationPathFor(bridge, c.Project, loc)
		if err != nil {
			continue
		}
		if _, err := gitOut(p, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil && branchAhead(p, branch) {
			out = append(out, loc+"="+branch)
		}
	}
	return out
}

// branchAhead says whether a local branch has commits the default branch lacks. When the default branch can't be told, a branch that exists counts as ahead.
func branchAhead(ws, branch string) bool {
	base, err := defaultBranch(ws)
	if err != nil {
		return true
	}
	ref := "refs/heads/" + base
	if _, err := gitOut(ws, "rev-parse", "--verify", "--quiet", ref); err != nil {
		ref = "origin/" + base
	}
	n, err := gitOut(ws, "rev-list", "--count", ref+".."+"refs/heads/"+branch)
	return err != nil || n != "0"
}

// dependenciesFor lists the dependencies applied on a side.
func dependenciesFor(deps []Dependency, location string) []Dependency {
	var out []Dependency
	for _, d := range deps {
		if contains(d.Sides, location) {
			out = append(out, d)
		}
	}
	return out
}

// sidePlan says what a side's dependencies need: apply runs @apply-dependency for them, and na is the reason the side needs no wiring. A side that needs a dependency has a binding, or the run is not created.
type sidePlan struct {
	apply []Dependency
	na    string
}

func planSide(sd Side, deps []Dependency) sidePlan {
	mine := dependenciesFor(deps, sd.Location)
	b := sd.Bindings[applyDependencyBinding]
	switch {
	case len(mine) == 0:
		return sidePlan{}
	case b.Command != "":
		return sidePlan{apply: mine}
	}
	return sidePlan{na: b.NotApplicable}
}

// dependencyLines describes the run's dependencies for the summary: what is applied where and what needs no wiring.
func dependencyLines(sides []Side, deps []Dependency) []string {
	var out []string
	for _, d := range deps {
		var applied []string
		for _, sd := range sides {
			if len(planSide(sd, []Dependency{d}).apply) > 0 {
				applied = append(applied, sd.Location)
			}
		}
		line := fmt.Sprintf("dependency %s @ %s (%s)", d.Location, d.label(), d.Path)
		if d.Auto {
			line += " automatic: an earlier subtask of the task is done there"
		}
		if len(applied) > 0 {
			line += " applied on " + strings.Join(applied, ", ")
		}
		out = append(out, line)
		for _, sd := range sides {
			p := planSide(sd, []Dependency{d})
			if p.na != "" {
				out = append(out, fmt.Sprintf("side %s: @%s is not applicable (%s); tested as it is", sd.Location, applyDependencyBinding, p.na))
			}
		}
	}
	return out
}

// absentHash stands for a file that does not exist, where a blob hash would otherwise be.
const absentHash = "absent"

// appliedEntry is one file @apply-dependency touched. Target is the blob restore puts back (absentHash for no file); empty means what HEAD has. Post is the blob the file held right after the apply. Left marks a git-ignored file that existed before the apply: restore never deletes or rewrites it, and reports it.
type appliedEntry struct {
	Path   string `json:"path"`
	Target string `json:"target,omitempty"`
	Post   string `json:"post"`
	Left   bool   `json:"left,omitempty"`
}

type appliedFiles struct {
	Files []appliedEntry `json:"files"`
}

// appliedRecordPath is where the workspace's record lives: inside its git dir, so it stays out of the tree and survives across runs.
func appliedRecordPath(ws string) (string, error) {
	p, err := gitOut(ws, "rev-parse", "--git-path", appliedRecord)
	if err != nil {
		return "", fmt.Errorf("%s is not a git workspace: %v", ws, err)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(ws, p)
	}
	return p, nil
}

func loadApplied(ws string) (appliedFiles, string, error) {
	var rec appliedFiles
	p, err := appliedRecordPath(ws)
	if err != nil {
		return rec, "", err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return rec, p, nil
	}
	if err != nil {
		return rec, p, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, p, fmt.Errorf("%s: %v", p, err)
	}
	return rec, p, nil
}

// parseAppliedList reads the files @apply-dependency wrote: one path per line, relative to the workspace, with / or \ and either line ending. On a bad line it returns the valid paths before it together with the error.
func parseAppliedList(data string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		slash := strings.ReplaceAll(line, `\`, "/")
		if strings.HasPrefix(slash, "/") || filepath.IsAbs(line) || (len(slash) > 1 && slash[1] == ':') {
			return out, fmt.Errorf("@%s listed %q: applied files must be relative to the workspace", applyDependencyBinding, line)
		}
		clean := path.Clean(slash)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return out, fmt.Errorf("@%s listed %q: it is not a file inside the workspace", applyDependencyBinding, line)
		}
		out = append(out, clean)
	}
	return out, nil
}

// gitZ runs git and splits its NUL-separated output.
func gitZ(ws string, args ...string) ([]string, error) {
	out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).Output()
	if err != nil {
		return nil, err
	}
	var toks []string
	for _, t := range strings.Split(string(out), "\x00") {
		if t != "" {
			toks = append(toks, t)
		}
	}
	return toks, nil
}

// dirtyBefore lists the paths git status reports as changed or untracked, as git spells them (slash form, unquoted).
func dirtyBefore(ws string) ([]string, error) {
	toks, err := gitZ(ws, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var out []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		out = append(out, t[3:])
		if strings.ContainsAny(t[:2], "RC") {
			i++ // the original path follows
		}
	}
	return out, nil
}

// fileHash is the blob hash of the file as it is on disk, without line-ending conversion, or absentHash.
func fileHash(ws, p string) string {
	if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(p))); err != nil {
		return absentHash
	}
	h, err := gitOut(ws, "hash-object", "--no-filters", "--", p)
	if err != nil {
		return absentHash
	}
	return h
}

// storeFile writes the file's content into the object database and returns its hash, so it can be put back later.
func storeFile(ws, p string) string {
	if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(p))); err != nil {
		return absentHash
	}
	h, err := gitOut(ws, "hash-object", "-w", "--no-filters", "--", p)
	if err != nil {
		return absentHash
	}
	return h
}

// targetHash is what restore puts back for the entry.
func targetHash(ws string, e appliedEntry) string {
	if e.Target != "" {
		return e.Target
	}
	h, err := gitOut(ws, "rev-parse", "--verify", "--quiet", "HEAD:"+e.Path)
	if err != nil || h == "" {
		return absentHash
	}
	return h
}

// writeTarget puts the entry's file back to its target, byte for byte.
func writeTarget(ws string, e appliedEntry, target string) error {
	abs := filepath.Join(ws, filepath.FromSlash(e.Path))
	if target == absentHash {
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		for dir := filepath.Dir(abs); dir != filepath.Clean(ws) && os.Remove(dir) == nil; dir = filepath.Dir(dir) {
		}
		if e.Target == "" {
			return git(ws, "rm", "-q", "--cached", "-f", "--ignore-unmatch", "--", ":(literal)"+e.Path)
		}
		return nil
	}
	data, err := exec.Command("git", "-C", ws, "cat-file", "blob", target).Output()
	if err != nil {
		return fmt.Errorf("reading %s for %s: %v", target, e.Path, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, data, mode); err != nil {
		return err
	}
	if e.Target == "" {
		return git(ws, "reset", "-q", "HEAD", "--", ":(literal)"+e.Path)
	}
	return nil
}

// saveApplied writes the record, or deletes it when nothing is left in it.
func saveApplied(recPath string, files []appliedEntry) error {
	if len(files) == 0 {
		if err := os.Remove(recPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	enc, _ := json.Marshal(appliedFiles{Files: files})
	return os.WriteFile(recPath, enc, 0o644)
}

// resolveDependencyBranch is the branch to apply: the one named, or what the run's workspace is on now.
func resolveDependencyBranch(d Dependency) (string, error) {
	if d.Branch != "" {
		return d.Branch, nil
	}
	b := currentBranch(d.Path)
	if b == "" {
		return "", fmt.Errorf("%s is on a detached HEAD, so there is no branch of this run to apply", d.Path)
	}
	return b, nil
}

// listIgnored lists the untracked ignored files of ws one by one, so whether a file existed before an apply is decided per file and not per folder.
func listIgnored(ws string) ([]string, error) {
	toks, err := gitZ(ws, "ls-files", "-o", "-i", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("listing the ignored files of %s: %v", ws, err)
	}
	return toks, nil
}

// gitIgnoresCase reports whether git treats file names case-insensitively in ws: git probes the filesystem at init and clone and records it in core.ignorecase.
func gitIgnoresCase(ws string) bool {
	v, err := gitOut(ws, "config", "--bool", "core.ignorecase")
	return err == nil && v == "true"
}

// gitSpelling is the spelling git knows f by when it differs from the reported one only in case: an exact match wins, then the record, the paths dirty before the apply, the ignored files, and the tracked files, in that order (each sorted). A path git does not know keeps its reported spelling.
func gitSpelling(ws, f string, rec []appliedEntry, before map[string]string, ignored []string) string {
	var cands []string
	for _, e := range rec {
		cands = append(cands, e.Path)
	}
	dirty := make([]string, 0, len(before))
	for p := range before {
		dirty = append(dirty, p)
	}
	slices.Sort(dirty)
	cands = append(cands, dirty...)
	cands = append(cands, ignored...)
	if slices.Contains(cands, f) {
		return f
	}
	for _, c := range cands {
		if strings.EqualFold(c, f) {
			return c
		}
	}
	tracked, err := gitZ(ws, "ls-files", "-z")
	if err != nil {
		return f
	}
	// git's icase pathspec folds ASCII only, so the tracked files are listed whole and folded here like the other sources.
	slices.Sort(tracked)
	if slices.Contains(tracked, f) {
		return f
	}
	for _, c := range tracked {
		if strings.EqualFold(c, f) {
			return c
		}
	}
	return f
}

// applyDependency runs @apply-dependency for one dependency in the side's workspace and records the files it touched. ignoredBefore is listIgnored from before the side's first apply.
func applyDependency(r *RunFiles, d *Dispatch, sd Side, dep Dependency, ignoredBefore []string) (string, error) {
	b := sd.Bindings[applyDependencyBinding]
	if b.Command == "" {
		return "", fmt.Errorf("side %s has no @%s command", sd.Location, applyDependencyBinding)
	}
	branch, err := resolveDependencyBranch(dep)
	if err != nil {
		return "", err
	}
	if dep.Branch != "" {
		if err := checkDependencyCheckout(dep); err != nil {
			return "", err
		}
	}
	rec, recPath, err := loadApplied(sd.Workspace)
	if err != nil {
		return "", err
	}
	settle := settleCommand(r, sd.Location)
	var unsettled []string
	for _, e := range rec.Files {
		if cur := fileHash(sd.Workspace, e.Path); !e.Left && cur != e.Post && cur != targetHash(sd.Workspace, e) {
			unsettled = append(unsettled, e.Path)
		}
	}
	if len(unsettled) > 0 {
		return "", fmt.Errorf("not applying again: %s changed since the wiring was applied, and restore keeps it; once the wiring is out of it by hand, run %s", strings.Join(unsettled, ", "), settleHints(settle, unsettled))
	}
	before := map[string]string{}
	dirty, err := dirtyBefore(sd.Workspace)
	if err != nil {
		return "", fmt.Errorf("reading the status of %s: %v", sd.Workspace, err)
	}
	for _, p := range dirty {
		before[p] = storeFile(sd.Workspace, p)
	}
	listFile := filepath.Join(r.Dir, "applied-"+sd.Location+"-"+dep.Location+".txt")
	if err := os.WriteFile(listFile, nil, 0o644); err != nil {
		return "", err
	}
	env := append(handlerEnv(r, d),
		"STARSHIP_DEPENDENCY="+dep.Location,
		"STARSHIP_DEPENDENCY_BRANCH="+branch,
		"STARSHIP_DEPENDENCY_PATH="+dep.Path,
		"STARSHIP_APPLIED_FILES="+listFile,
	)
	limit := b.Timeout
	if limit == 0 {
		limit = defaultBindingTimeout
	}
	out, runErr := shellFor(sd.Workspace, b.Command, env, limit)
	data, _ := os.ReadFile(listFile)
	files, perr := parseAppliedList(string(data))
	foldCase := gitIgnoresCase(sd.Workspace)
	for _, f := range files {
		if foldCase {
			f = gitSpelling(sd.Workspace, f, rec.Files, before, ignoredBefore)
		}
		post := fileHash(sd.Workspace, f)
		known := false
		for i := range rec.Files {
			if rec.Files[i].Path == f {
				rec.Files[i].Post = post
				known = true
			}
		}
		if !known {
			_, wasDirty := before[f]
			_, headErr := gitOut(sd.Workspace, "rev-parse", "--verify", "--quiet", "HEAD:"+f)
			// The "./" keeps a name starting ":" from being read as pathspec magic; ":(literal)" is rejected by check-ignore.
			left := !wasDirty && headErr != nil && slices.Contains(ignoredBefore, f) && exec.Command("git", "-C", sd.Workspace, "check-ignore", "-q", "--", "./"+f).Run() == nil
			rec.Files = append(rec.Files, appliedEntry{Path: f, Target: before[f], Post: post, Left: left})
		}
	}
	if len(files) > 0 {
		if err := saveApplied(recPath, rec.Files); err != nil {
			return out, err
		}
	}
	if perr != nil {
		return out, perr
	}
	if runErr != nil {
		return out, fmt.Errorf("@%s for %s failed on side %s: %v", applyDependencyBinding, dep.Location, sd.Location, runErr)
	}
	return out, nil
}

// applyDependencies applies every dependency the side needs, in order.
func applyDependencies(r *RunFiles, d *Dispatch, sd Side, deps []Dependency) (string, error) {
	ignoredBefore, err := listIgnored(sd.Workspace)
	if err != nil {
		return "", err
	}
	var log []string
	for _, dep := range deps {
		out, err := applyDependency(r, d, sd, dep, ignoredBefore)
		log = append(log, fmt.Sprintf("$ @%s %s (%s)\n%s", applyDependencyBinding, dep.Location, sd.Location, out))
		if err != nil {
			return strings.Join(log, "\n"), err
		}
	}
	return strings.Join(log, "\n"), nil
}

// settleCommand is the command that drops a file from the record of a side's applied wiring.
func settleCommand(r *RunFiles, location string) string {
	return fmt.Sprintf("ensign dependency restore --bridge %s --run %s --side %s", shellQuote(r.Snap.Bridge), r.State.Run, location)
}

func settleHints(settle string, files []string) string {
	var cmds []string
	for _, f := range files {
		cmds = append(cmds, "`"+settle+" --done "+f+"`")
	}
	return strings.Join(cmds, ", ")
}

// restoreDependencies returns a plain notice (not an error) for files it left in place. It puts back what @apply-dependency touched in ws: each file goes to its target (HEAD, or what it held before the apply) and leaves the record. A file edited since the apply is left alone and named, with the settle command (a prefix of `--done <file>`) that drops it once it has been put right by hand.
func restoreDependencies(ws, settle string) (string, error) {
	rec, recPath, err := loadApplied(ws)
	if err != nil {
		return "", err
	}
	var kept []appliedEntry
	var changed, left []string
	for _, e := range rec.Files {
		target := targetHash(ws, e)
		cur := fileHash(ws, e.Path)
		switch {
		case e.Left:
			left = append(left, e.Path)
		case cur == target:
		case cur != e.Post:
			changed = append(changed, e.Path)
			kept = append(kept, e)
		default:
			if err := writeTarget(ws, e, target); err != nil {
				return "", err
			}
		}
	}
	if err := saveApplied(recPath, kept); err != nil {
		return "", err
	}
	notice := ""
	if len(left) > 0 {
		notice = fmt.Sprintf("dependency restore left %s in place: ignored by git and present before the wiring was applied, so it cannot reach a commit; remove the wiring from it by hand", strings.Join(left, ", "))
	}
	if len(changed) > 0 {
		return notice, fmt.Errorf("dependency restore left %s as it is: changed since the wiring was applied; once the wiring is out of it by hand, run %s", strings.Join(changed, ", "), settleHints(settle, changed))
	}
	return notice, nil
}

// settleApplied drops files from the record of ws's applied wiring, for files whose wiring was removed by hand.
func settleApplied(ws string, files []string) error {
	rec, recPath, err := loadApplied(ws)
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, f := range files {
		f = path.Clean(strings.ReplaceAll(f, `\`, "/"))
		found := false
		for _, e := range rec.Files {
			found = found || e.Path == f
		}
		if !found {
			return fmt.Errorf("%s is not in the record of applied wiring", f)
		}
		drop[f] = true
	}
	var kept []appliedEntry
	for _, e := range rec.Files {
		if !drop[e.Path] {
			kept = append(kept, e)
		}
	}
	return saveApplied(recPath, kept)
}

// checkNoDependencyWiring refuses a commit while @apply-dependency's changes are in the workspace; files that are back at their target leave the record.
func checkNoDependencyWiring(r *RunFiles) error {
	ws := r.Snap.Workspace
	rec, recPath, err := loadApplied(ws)
	if err != nil {
		return err
	}
	var kept []appliedEntry
	var wired, changed []string
	for _, e := range rec.Files {
		cur := fileHash(ws, e.Path)
		switch {
		case e.Left:
			kept = append(kept, e)
			continue
		case cur == targetHash(ws, e):
			continue
		case cur != e.Post:
			changed = append(changed, e.Path)
		default:
			wired = append(wired, e.Path)
		}
		kept = append(kept, e)
	}
	if len(kept) < len(rec.Files) {
		if err := saveApplied(recPath, kept); err != nil {
			return err
		}
	}
	var parts []string
	if len(wired) > 0 {
		parts = append(parts, fmt.Sprintf("dependency wiring from @%s is still in the tree: %s; run `ensign dependency restore --bridge %s --run %s`", applyDependencyBinding, strings.Join(wired, ", "), shellQuote(r.Snap.Bridge), r.State.Run))
	}
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("%s changed since the dependency wiring was applied, so restore leaves it; once the wiring is out of it by hand, run %s", strings.Join(changed, ", "), settleHints(settleCommand(r, r.Snap.Location), changed)))
	}
	if len(parts) > 0 {
		return errors.New(strings.Join(parts, "; "))
	}
	return nil
}

// cmdDependency implements `ensign dependency apply --side <location>` and `ensign dependency restore [--side <location>]`, for agents that build and test themselves.
func cmdDependency(args []string, out io.Writer) error {
	const usage = "ensign dependency apply --side <location> [--bridge <dir>] [--run <id>] | ensign dependency restore [--side <location>] [--bridge <dir>] [--run <id>] | ensign dependency restore --side <location> --done <file> [--done <file>...] [--bridge <dir>] [--run <id>]"
	if len(args) == 0 || (args[0] != "apply" && args[0] != "restore") {
		return fmt.Errorf("usage: %s", usage)
	}
	verb, side := args[0], ""
	var rest, done []string
	for i := 1; i < len(args); i++ {
		if args[i] == "--done" && i+1 < len(args) {
			done = append(done, args[i+1])
			i++
			continue
		}
		if args[i] == "--side" && i+1 < len(args) {
			side = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	r, err := openRunByFlags(rest, usage)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("there is no active run")
	}
	r.close() // the run is only read, and the binding may take a while
	if len(r.Snap.Dependencies) == 0 {
		return fmt.Errorf("run %s has no dependencies to %s", r.State.Run, verb)
	}
	var sides []Side
	for _, sd := range r.Snap.sides() {
		if side == "" || sd.Location == side {
			sides = append(sides, sd)
		}
	}
	if len(sides) == 0 {
		return fmt.Errorf("run %s has no side %q (its sides are %s)", r.State.Run, side, strings.Join(sideNames(r.Snap.sides()), ", "))
	}
	if len(done) > 0 && (verb != "restore" || side == "") {
		return fmt.Errorf("--done goes with `ensign dependency restore --side <location>`")
	}
	if verb == "apply" {
		if side == "" {
			return fmt.Errorf("ensign dependency apply needs --side <location>")
		}
		sd := sides[0]
		p := planSide(sd, r.Snap.Dependencies)
		if len(p.apply) == 0 {
			fmt.Fprintf(out, "side %s needs no dependency applied; test it as it is\n", sd.Location)
			return nil
		}
		log, err := applyDependencies(r, nil, sd, p.apply)
		fmt.Fprintln(out, log)
		if err == nil {
			fmt.Fprintf(out, "applied on side %s; run `ensign dependency restore --side %s` before you finish\n", sd.Location, sd.Location)
		}
		return err
	}
	if len(done) > 0 {
		if err := settleApplied(sides[0].Workspace, done); err != nil {
			return err
		}
		fmt.Fprintln(out, "settled")
		return nil
	}
	for _, sd := range sides {
		notice, err := restoreDependencies(sd.Workspace, settleCommand(r, sd.Location))
		if notice != "" {
			fmt.Fprintf(out, "side %s: %s\n", sd.Location, notice)
		}
		if err != nil {
			return fmt.Errorf("side %s: %v", sd.Location, err)
		}
	}
	fmt.Fprintln(out, "restored")
	return nil
}
