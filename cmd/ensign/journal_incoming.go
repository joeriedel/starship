// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Commits another session or machine pushed reach the bridge by a pull in pushBridge, by the Stop hook's fast-forward, or by the session-start sync. Each intake is appended to an untracked log under .starship, and each session keeps a mark of how far in that log it has been told, so a Stop hook can tell a session what arrived without knowing which command pulled it.

const (
	incomingLogMax  = 200 // lines the log may reach before it is cut back
	incomingLogKeep = 100
	incomingListMax = 8 // files named per group in a report
)

func incomingLogPath(bridge string) string {
	return filepath.Join(bridge, ".starship", "incoming.log")
}

// incomingIntake is one intake: the commits in To that the bridge's HEAD did not have, as the diff from their merge base.
type incomingIntake struct {
	ID       int64
	Base, To string
}

// readIncomingLog returns the log's intakes, oldest first. A missing or unreadable log is empty.
func readIncomingLog(bridge string) []incomingIntake {
	data, err := os.ReadFile(incomingLogPath(bridge))
	if err != nil {
		return nil
	}
	var out []incomingIntake
	for _, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		id, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, incomingIntake{id, f[1], f[2]})
	}
	return out
}

func incomingEnd(bridge string) int64 {
	log := readIncomingLog(bridge)
	if len(log) == 0 {
		return 0
	}
	return log[len(log)-1].ID
}

// recordIntake logs the commits of newHead that oldHead lacked and returns their count and files. Recording is best effort: a log that can't be written loses only the Stop hook's notice.
func recordIntake(bridge, oldHead, newHead string) (int, []string) {
	if oldHead == "" || newHead == "" || oldHead == newHead {
		return 0, nil
	}
	base, err := gitOut(bridge, "merge-base", oldHead, newHead)
	if err != nil || base == newHead {
		return 0, nil
	}
	files, err := gitZ(bridge, "diff", "--name-only", "--no-renames", "-z", base, newHead)
	if err != nil {
		return 0, nil
	}
	count := 0
	if n, err := gitOut(bridge, "rev-list", "--count", newHead, "^"+oldHead); err == nil {
		count, _ = strconv.Atoi(n)
	}
	if err := os.MkdirAll(filepath.Dir(incomingLogPath(bridge)), 0o755); err == nil {
		appendIncoming(bridge, fmt.Sprintf("%d %s %s\n", time.Now().UnixNano(), base, newHead))
	}
	return count, files
}

func appendIncoming(bridge, line string) {
	p := incomingLogPath(bridge)
	if data, err := os.ReadFile(p); err == nil {
		if lines := strings.SplitAfter(string(data), "\n"); len(lines) > incomingLogMax {
			os.WriteFile(p, []byte(strings.Join(lines[len(lines)-incomingLogKeep-1:], "")), 0o644)
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}

func incomingMarkPath(bridge, session string) string {
	return sessionFile(bridge, session) + ".incoming"
}

func readIncomingMark(bridge, session string) (int64, bool) {
	data, err := os.ReadFile(incomingMarkPath(bridge, session))
	if err != nil {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return id, err == nil
}

func setIncomingMark(bridge, session string, id int64) {
	writeFile(incomingMarkPath(bridge, session), strconv.FormatInt(id, 10)+"\n")
}

// sessionProject is the project of the session's task, or "" for none.
func sessionProject(bridge, session string) string {
	if session == "" {
		return ""
	}
	project, _, _ := strings.Cut(sessionTask(bridge, session), "/")
	return project
}

// incomingFiles is the intake's changed files, grouped for a session working on project ("" for none).
type incomingFiles struct {
	Project  []string // the current project's files
	Wide     []string // bridge.yaml and notes/
	Others   int      // files of other projects
	Projects int      // how many other projects
	Rest     []string // anything else
}

func groupIncoming(files []string, project string) incomingFiles {
	var g incomingFiles
	seen := map[string]bool{}
	others := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if p, ok := strings.CutPrefix(f, "projects/"); ok {
			name, _, _ := strings.Cut(p, "/")
			if project != "" && name == project {
				g.Project = append(g.Project, f)
			} else {
				g.Others++
				others[name] = true
			}
			continue
		}
		if f == "bridge.yaml" || strings.HasPrefix(f, "notes/") {
			g.Wide = append(g.Wide, f)
		} else {
			g.Rest = append(g.Rest, f)
		}
	}
	g.Projects = len(others)
	return g
}

// needs is the files a session must re-read: the current project's project.md and tasks, and everything bridge-wide.
func (g incomingFiles) needs(project string) []string {
	var out []string
	for _, f := range g.Project {
		if f == "projects/"+project+"/project.md" || strings.HasPrefix(f, "projects/"+project+"/tasks/") {
			out = append(out, f)
		}
	}
	return append(out, g.Wide...)
}

func listFiles(files []string) string {
	if len(files) > incomingListMax {
		return strings.Join(files[:incomingListMax], ", ") + fmt.Sprintf(" and %d more", len(files)-incomingListMax)
	}
	return strings.Join(files, ", ")
}

func (g incomingFiles) describe(project string) string {
	var parts []string
	if len(g.Project) > 0 {
		parts = append(parts, fmt.Sprintf("project %s: %s", project, listFiles(g.Project)))
	}
	if len(g.Wide) > 0 {
		parts = append(parts, "bridge-wide: "+listFiles(g.Wide))
	}
	if g.Others > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) in %d other project(s)", g.Others, g.Projects))
	}
	if len(g.Rest) > 0 {
		parts = append(parts, "other: "+listFiles(g.Rest))
	}
	return strings.Join(parts, "; ")
}

// incomingReport words an intake for the session working on project; empty when nothing came in.
func incomingReport(commits int, files []string, project string) string {
	if commits == 0 && len(files) == 0 {
		return ""
	}
	return fmt.Sprintf("the bridge took in %d commit(s) from its remote: %s", commits, sortedGroups(files, project))
}

func sortedGroups(files []string, project string) string {
	files = append([]string(nil), files...)
	sort.Strings(files)
	return groupIncoming(files, project).describe(project)
}

// incomingStop tells the session once about intakes since its mark that touch what it works on. A session with no mark gets one without a notice.
func incomingStop(bridge string, in *HookInput) any {
	if in.SessionID == "" || in.StopHookActive {
		return nil
	}
	mark, ok := readIncomingMark(bridge, in.SessionID)
	if !ok {
		setIncomingMark(bridge, in.SessionID, incomingEnd(bridge))
		return nil
	}
	project := sessionProject(bridge, in.SessionID)
	var files []string
	end := mark
	for _, it := range readIncomingLog(bridge) {
		if it.ID <= mark {
			continue
		}
		end = it.ID
		if fs, err := gitZ(bridge, "diff", "--name-only", "--no-renames", "-z", it.Base, it.To); err == nil {
			files = append(files, fs...)
		}
	}
	if end == mark {
		return nil
	}
	setIncomingMark(bridge, in.SessionID, end)
	sort.Strings(files)
	g := groupIncoming(files, project)
	needs := g.needs(project)
	if len(needs) == 0 {
		return map[string]any{"systemMessage": fmt.Sprintf("starship: the bridge took in changes from its remote: %s", g.describe(project))}
	}
	return block(fmt.Sprintf("other sessions or machines pushed changes to the bridge that touch your work (%s). Re-read those files, and tell the user about anything that affects the work in progress.", g.describe(project)))
}
