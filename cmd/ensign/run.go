package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Snapshot is the frozen input of a run. Hooks read only this, never the layers.
type Snapshot struct {
	Run          string               `json:"run"`
	Task         string               `json:"task"`
	Bridge       string               `json:"bridge"`
	ControlPlane string               `json:"control_plane"`
	Workspace    string               `json:"workspace"`
	WorkItem     string               `json:"work_item"`
	Start        string               `json:"start,omitempty"`
	Pipeline     *Pipeline            `json:"pipeline"`
	Bindings     map[string]Binding   `json:"bindings"`
	Briefs       map[string]string    `json:"briefs"` // relative path -> contents, frozen with the run
	Events       map[string][]Handler `json:"events,omitempty"`
	Bundles      []string             `json:"bundles,omitempty"`
	Created      string               `json:"created"`
}

type Dispatch struct {
	ID          string `json:"id"`
	Stage       string `json:"stage"`
	Scope       string `json:"scope"`
	Dim         string `json:"dim,omitempty"`
	Role        string `json:"role"`
	CallerAgent string `json:"caller_agent,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Done        bool   `json:"done"`
	Outcome     string `json:"outcome,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// Fields holds the report's other lines (files, rules, facts …).
	Fields     map[string][]string `json:"fields,omitempty"`
	StopBlocks int                 `json:"stop_blocks,omitempty"`
}

type ScopeState struct {
	Stage    string            `json:"stage"`              // current stage id; "" once the scope is finished
	Branches map[string]string `json:"branches,omitempty"` // dimension -> dispatch id, for a parallel stage
}

// ResumePoint is where an ask-user run picks up again.
type ResumePoint struct {
	Scope     string `json:"scope"`
	Stage     string `json:"stage"`
	Budget    string `json:"budget,omitempty"`     // the budget that ran out
	EdgeSpend bool   `json:"edge_spend,omitempty"` // it was spent by a transition, not by entering
	Reset     string `json:"reset,omitempty"`      // a reset still owed by that transition
}

type HistoryEntry struct {
	At       string `json:"at"`
	Kind     string `json:"kind"` // dispatch, denied, outcome, enter, finish, blocked
	Stage    string `json:"stage,omitempty"`
	Dispatch string `json:"dispatch,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type State struct {
	Run      string                 `json:"run"`
	Session  string                 `json:"session,omitempty"` // the Claude Code session that owns the run
	Status   string                 `json:"status"`            // pending, running, success, error, ask-user
	Reason   string                 `json:"reason,omitempty"`
	Approved bool                   `json:"approved"`
	Spent    map[string]int         `json:"spent"`
	Scopes   map[string]*ScopeState `json:"scopes"`
	// StartChild is the --start stage inside a lead, applied when the lead's scope opens.
	StartChild string         `json:"start_child,omitempty"`
	Started    bool           `json:"started,omitempty"` // run.start has fired
	Sync       bool           `json:"sync,omitempty"`    // run @sync before run.start
	Base       string         `json:"base,omitempty"`    // git-catalog: HEAD when the run started
	Extra      map[string]int `json:"extra,omitempty"`   // budget added on resume
	Resume     *ResumePoint   `json:"resume,omitempty"`  // where an ask-user run continues
	Answers    []string       `json:"answers,omitempty"` // the user's answers on resume
	Dispatches []*Dispatch    `json:"dispatches"`
	History    []HistoryEntry `json:"history"`
}

func (st *State) terminal() bool {
	return st.Status == "success" || st.Status == "error" || st.Status == "ask-user"
}

func (st *State) log(kind, stage, dispatch, detail string) {
	st.History = append(st.History, HistoryEntry{
		At: time.Now().Format(time.RFC3339), Kind: kind, Stage: stage, Dispatch: dispatch, Detail: detail,
	})
}

func (st *State) dispatch(id string) *Dispatch {
	for _, d := range st.Dispatches {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (st *State) byAgent(agentID string) *Dispatch {
	for _, d := range st.Dispatches {
		if agentID != "" && d.AgentID == agentID {
			return d
		}
	}
	return nil
}

func runsDir(bridge string) string { return filepath.Join(bridge, ".starship", "runs") }

func sessionFile(bridge, session string) string {
	return filepath.Join(bridge, ".starship", "sessions", session)
}

// RunFiles is an open run: its snapshot, its state, and the lock held while using them.
type RunFiles struct {
	Dir   string
	Snap  *Snapshot
	State *State
	lock  *flock.Flock
}

// openRun locks and loads one run.
func openRun(bridge, id string) (*RunFiles, error) {
	dir := filepath.Join(runsDir(bridge), id)
	l := flock.New(filepath.Join(dir, "lock"))
	if err := l.Lock(); err != nil {
		return nil, fmt.Errorf("locking run %s: %v", id, err)
	}
	r := &RunFiles{Dir: dir, lock: l, Snap: &Snapshot{}, State: &State{}}
	if err := readJSON(filepath.Join(dir, "pipeline.json"), r.Snap); err != nil {
		l.Unlock()
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "state.json"), r.State); err != nil {
		l.Unlock()
		return nil, err
	}
	return r, nil
}

// openSessionRun opens the run bound to a session. It returns nil, nil when there is none.
func openSessionRun(bridge, session string) (*RunFiles, error) {
	if session == "" {
		return nil, nil
	}
	data, err := os.ReadFile(sessionFile(bridge, session))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return openRun(bridge, strings.TrimSpace(string(data)))
}

// bindSession records that a run belongs to a session.
func bindSession(bridge, session, run string) error {
	f := sessionFile(bridge, session)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f, []byte(run+"\n"), 0o644)
}

type runInfo struct {
	id    string
	snap  Snapshot
	state State
}

// listRuns reads every run in the bridge, newest first, without locking.
func listRuns(bridge string) []runInfo {
	entries, _ := os.ReadDir(runsDir(bridge))
	var out []runInfo
	for i := len(entries) - 1; i >= 0; i-- {
		dir := filepath.Join(runsDir(bridge), entries[i].Name())
		var ri runInfo
		ri.id = entries[i].Name()
		if readJSON(filepath.Join(dir, "pipeline.json"), &ri.snap) == nil &&
			readJSON(filepath.Join(dir, "state.json"), &ri.state) == nil {
			out = append(out, ri)
		}
	}
	return out
}

func (r *RunFiles) save() error {
	return writeJSON(filepath.Join(r.Dir, "state.json"), r.State)
}

func (r *RunFiles) close() { r.lock.Unlock() }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// writeJSON replaces the file atomically, so a crash never leaves half a state file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cmdRun implements `ensign run <project>/<task> [--start <stage>] [--dry-run]`.
func cmdRun(args []string, out io.Writer) error {
	var task, start string
	dry, sync := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dry = true
		case "--sync":
			sync = true
		case "--start":
			if i+1 >= len(args) {
				return fmt.Errorf("--start needs a stage")
			}
			i++
			start = args[i]
		default:
			if strings.HasPrefix(args[i], "-") || task != "" {
				return fmt.Errorf("usage: ensign run <project>/<task> [--start <stage>] [--sync] [--dry-run]")
			}
			task = args[i]
		}
	}
	if task == "" {
		return fmt.Errorf("usage: ensign run <project>/<task> [--start <stage>] [--sync] [--dry-run]")
	}
	bridge, err := cwdBridge()
	if err != nil {
		return err
	}
	c, err := loadConfig(bridge, task)
	if err != nil {
		return err
	}
	errs := validate(c)
	if start != "" {
		if s, _, _ := c.Pipeline.find(start); s == nil {
			errs = append(errs, fmt.Sprintf("--start: no stage %q", start))
		}
	}
	if sync {
		if b, ok := c.Bindings["sync"]; !ok || (b.Command == "" && b.NotApplicable == "") {
			errs = append(errs, "--sync needs the @sync binding")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("the run is not valid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	printSummary(out, c, start)
	if dry {
		fmt.Fprintln(out, "\n(dry run: nothing created)")
		return nil
	}

	session := os.Getenv("STARSHIP_SESSION")
	if session != "" {
		if data, err := os.ReadFile(sessionFile(bridge, session) + ".pin"); err == nil {
			if started := strings.TrimSpace(string(data)); started != pinnedCommit(bridge) {
				return fmt.Errorf("the bridge was updated to a new starship release after this session started; restart claude before starting a run")
			}
		}
	}
	for _, ri := range listRuns(bridge) {
		if ri.state.terminal() {
			continue
		}
		if session != "" && ri.state.Session == session {
			return fmt.Errorf("this session already has an active run, %s", ri.id)
		}
		if ri.snap.Workspace == c.Workspace {
			return fmt.Errorf("run %s is still active on the same workspace (%s)", ri.id, c.Workspace)
		}
	}

	id, dir, err := newRunDir(bridge, time.Now().Format("20060102-150405")+"-"+strings.ReplaceAll(c.Task, "/", "-"))
	if err != nil {
		return err
	}
	snap := &Snapshot{
		Run:          id,
		Task:         c.Task,
		Bridge:       bridge,
		ControlPlane: c.ControlPlane,
		Workspace:    c.Workspace,
		WorkItem:     c.WorkItem,
		Start:        start,
		Pipeline:     c.Pipeline,
		Bindings:     c.Bindings,
		Briefs:       map[string]string{},
		Events:       c.Events,
		Bundles:      c.Bundles,
		Created:      time.Now().Format(time.RFC3339),
	}
	for _, hs := range snap.Events {
		for i := range hs {
			if hs[i].Kind == "augment" && !strings.HasPrefix(hs[i].Text, "@") {
				f := filepath.Join(hs[i].Dir, filepath.FromSlash(hs[i].Text))
				if _, err := os.Stat(f); err != nil {
					f, _ = c.findFile(hs[i].Text)
				}
				data, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				hs[i].Text = string(data)
			}
		}
	}
	if err := freezeBriefs(c, snap); err != nil {
		return err
	}
	st := &State{Run: snap.Run, Session: session, Status: "pending", Spent: map[string]int{}, Scopes: map[string]*ScopeState{},
		Sync: sync, Extra: map[string]int{}}
	first := c.Pipeline.Stages[0].ID
	if start != "" {
		_, _, scope := c.Pipeline.find(start)
		if scope == "" {
			first = start
		} else {
			first, st.StartChild = scope, start
		}
	}
	m := &machine{snap: snap, st: st}
	m.enter("", first)

	if err := os.Mkdir(filepath.Join(dir, "reports"), 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "pipeline.json"), snap); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "state.json"), st); err != nil {
		return err
	}
	if session != "" {
		if err := bindSession(bridge, session, snap.Run); err != nil {
			return err
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pipeline.json"))
	sum := sha256.Sum256(data)
	fmt.Fprintf(out, "\nrun %s created (pipeline sha %s), waiting for go\n", snap.Run, hex.EncodeToString(sum[:])[:12])
	return nil
}

// newRunDir creates a run's directory exclusively, adding -2, -3 … if the id is taken,
// so two runs started in the same second can never share one.
func newRunDir(bridge, base string) (string, string, error) {
	if err := os.MkdirAll(runsDir(bridge), 0o755); err != nil {
		return "", "", err
	}
	for n := 1; n < 100; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		dir := filepath.Join(runsDir(bridge), id)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return id, dir, nil
		}
		if !os.IsExist(err) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("could not allocate a run id for %s", base)
}

// freezeBriefs copies every brief the run can use into the snapshot.
func freezeBriefs(c *Config, snap *Snapshot) error {
	var walk func(list []*Stage) error
	walk = func(list []*Stage) error {
		for _, s := range list {
			paths := []string{}
			if s.Brief != "" {
				paths = append(paths, s.Brief)
			}
			for _, d := range s.Dimensions {
				paths = append(paths, "briefs/review/"+d+".md")
			}
			for _, rel := range paths {
				f, err := c.findFile(rel)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				snap.Briefs[rel] = string(data)
			}
			if err := walk(s.Stages); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(c.Pipeline.Stages)
}

func printSummary(out io.Writer, c *Config, start string) {
	p := c.Pipeline
	fmt.Fprintf(out, "task       %s\nworkspace  %s\npipeline   %s — %s\n", c.Task, c.Workspace, p.Name, p.Summary)
	if start != "" {
		fmt.Fprintf(out, "start at   %s\n", start)
	}
	fmt.Fprintln(out, "\nstages")
	var walk func(list []*Stage, indent string)
	walk = func(list []*Stage, indent string) {
		for _, s := range list {
			extra := ""
			if s.Spend != "" {
				extra += " spend:" + s.Spend
			}
			if s.Parallel != nil {
				extra += fmt.Sprintf(" parallel over %v", s.Dimensions)
			}
			fmt.Fprintf(out, "%s%-8s %-15s%s\n%s         from %s\n", indent, s.ID, s.Role, extra, indent, s.Source)
			for _, o := range s.Outcomes {
				fmt.Fprintf(out, "%s           %-9s → %s\n", indent, o.Name, o.Then)
			}
			walk(s.Stages, indent+"  ")
		}
	}
	walk(p.Stages, "  ")
	for k, v := range p.Sources {
		if strings.HasPrefix(k, "removed.") {
			fmt.Fprintf(out, "  (stage %s removed by %s)\n", strings.TrimPrefix(k, "removed."), v)
		}
	}

	fmt.Fprintln(out, "\nbudgets")
	for _, k := range sortedKeys(p.Budgets) {
		fmt.Fprintf(out, "  %-6s %d   from %s\n", k, p.Budgets[k], p.Sources["budgets."+k])
	}
	for _, k := range sortedKeys(p.OnExhausted) {
		fmt.Fprintf(out, "  on %s exhausted → %s   from %s\n", k, p.OnExhausted[k], p.Sources["on-exhausted."+k])
	}

	fmt.Fprintln(out, "\nbindings")
	for _, k := range sortedKeys(c.Bindings) {
		b := c.Bindings[k]
		v := b.Command
		switch {
		case b.NotApplicable != "":
			v = "not applicable: " + b.NotApplicable
		case b.Guidance != "":
			v = "guidance: " + b.Guidance
		}
		fmt.Fprintf(out, "  @%-8s %s   from %s\n", k, v, b.Source)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
