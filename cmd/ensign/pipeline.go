// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"strings"
	"time"
)

// machine applies pipeline transitions to a run's state.
type machine struct {
	snap *Snapshot
	st   *State
}

func (m *machine) stage(id string) *Stage {
	s, _, _ := m.snap.Pipeline.find(id)
	return s
}

// enter makes a stage the current one in its scope, spending its budget.
func (m *machine) enter(scope, id string) {
	sc := m.st.Scopes[scope]
	if sc == nil {
		sc = &ScopeState{}
		m.st.Scopes[scope] = sc
	}
	sc.Stage, sc.Branches = id, nil
	s := m.stage(id)
	if s.Spend != "" && !m.spend(s.Spend, scope, id, false) {
		return
	}
	m.st.log("enter", id, "", m.budgetLine())
	if why := skipReason(s, m.snap.Bindings, m.snap.Documentation); why != "" {
		m.st.note("stage " + id + " skipped: " + why)
		m.apply(scope, s, s.Skip.Outcome)
	} else if s.Parallel != nil && s.Parallel.Empty != "" && len(s.Dimensions) == 0 {
		m.st.log("empty", id, "", "no items; takes "+s.Parallel.Empty)
		m.apply(scope, s, s.Parallel.Empty)
	}
}

// skipReason says why a stage with a skip rule is not run, or "" if it runs.
func skipReason(s *Stage, bindings map[string]Binding, documentation string) string {
	if s.Skip == nil {
		return ""
	}
	if s.Skip.UnlessBinding == "docs" && documentation == "off" {
		return "documentation is turned off"
	}
	if len(bindings[s.Skip.UnlessBinding].Paths) == 0 {
		return "no @" + s.Skip.UnlessBinding + " paths declared"
	}
	return ""
}

// limit is a budget's size, including anything added when an ask-user run resumed.
func (m *machine) limit(budget string) int {
	return m.snap.Pipeline.Budgets[budget] + m.st.Extra[budget]
}

// spend charges one unit of a budget on the way into a stage — by entering it, or (edge)
// by the transition leading there. If that exceeds the budget, its on-exhausted
// transition ends the run and spend reports false. An ask-user there records where to
// resume, with the spend undone so the resume can make it against the larger budget.
func (m *machine) spend(budget, scope, into string, edge bool) bool {
	m.st.Spent[budget]++
	if m.st.Spent[budget] <= m.limit(budget) {
		return true
	}
	t := m.snap.Pipeline.OnExhausted[budget]
	if !t.terminal() {
		t = Transition{Error: "budget:" + budget}
	}
	if t.AskUser != "" {
		m.st.Spent[budget]--
		m.st.Resume = &ResumePoint{Scope: scope, Stage: into, Budget: budget, EdgeSpend: edge}
	}
	why := fmt.Sprintf("%s budget exhausted (%d)", strings.ReplaceAll(budget, "-", " "), m.limit(budget))
	if t.Finish != "" && m.hasLead() {
		m.toGate("", why)
		return false
	}
	m.finish(t, why)
	return false
}

// canonicalBudget maps the old budget name loop to coder-loop; any other name is unchanged.
func canonicalBudget(name string) string {
	if name == "loop" {
		return "coder-loop"
	}
	return name
}

// canonicalizeBudgets rewrites the old budget name loop to coder-loop throughout a pipeline:
// budgets, on-exhausted, sources, and every stage and transition spend or reset.
func (p *Pipeline) canonicalizeBudgets() error {
	if _, old := p.Budgets["loop"]; old {
		if _, cur := p.Budgets["coder-loop"]; cur {
			return fmt.Errorf("budgets declare both loop and coder-loop: loop is the old name of coder-loop")
		}
	}
	if _, old := p.OnExhausted["loop"]; old {
		if _, cur := p.OnExhausted["coder-loop"]; cur {
			return fmt.Errorf("on-exhausted declares both loop and coder-loop: loop is the old name of coder-loop")
		}
	}
	if v, ok := p.Budgets["loop"]; ok {
		delete(p.Budgets, "loop")
		p.Budgets["coder-loop"] = v
	}
	if t, ok := p.OnExhausted["loop"]; ok {
		delete(p.OnExhausted, "loop")
		p.OnExhausted["coder-loop"] = t
	}
	for _, prefix := range []string{"budgets.", "on-exhausted."} {
		if v, ok := p.Sources[prefix+"loop"]; ok {
			delete(p.Sources, prefix+"loop")
			p.Sources[prefix+"coder-loop"] = v
		}
	}
	for k, t := range p.OnExhausted {
		p.OnExhausted[k] = t.canonicalBudgets()
	}
	var walk func(list []*Stage)
	walk = func(list []*Stage) {
		for _, s := range list {
			s.Spend = canonicalBudget(s.Spend)
			for i := range s.Outcomes {
				s.Outcomes[i].Then = s.Outcomes[i].Then.canonicalBudgets()
			}
			walk(s.Stages)
		}
	}
	walk(p.Stages)
	return nil
}

func (t Transition) canonicalBudgets() Transition {
	t.Spend, t.Reset = canonicalBudget(t.Spend), canonicalBudget(t.Reset)
	if t.Error == "budget:loop" {
		t.Error = "budget:coder-loop"
	}
	return t
}

func (m *machine) budgetLine() string {
	var parts []string
	for _, k := range sortedKeys(m.snap.Pipeline.Budgets) {
		parts = append(parts, fmt.Sprintf("%s %d/%d", k, m.st.Spent[k], m.limit(k)))
	}
	return strings.Join(parts, ", ")
}

// resetPosition drops where the run was: each scope's stage and branches, and the resume point.
func (st *State) resetPosition() {
	for _, sc := range st.Scopes {
		sc.Stage, sc.Branches = "", nil
	}
	st.Resume = nil
}

// hasLead reports a pipeline run by a lead, whose judgment closes the run at the gate.
func (m *machine) hasLead() bool { return m.snap.Pipeline.Stages[0].isLead() }

// toGate parks a passed run at the closing gate for its lead to judge.
func (m *machine) toGate(stage, why string) {
	m.st.Gate = why
	m.st.resetPosition()
	m.st.log("gate", stage, "", why)
}

// finish ends the run with a terminal transition.
func (m *machine) finish(t Transition, why string) {
	switch {
	case t.Finish != "":
		m.st.Status, m.st.Reason = "success", why
	case t.Error != "":
		m.st.Status, m.st.Reason = "error", t.Error+": "+why
	case t.AskUser != "":
		m.st.Status, m.st.Reason = "ask-user", t.AskUser
	}
	m.st.Gate = ""
	m.st.FinishedAt = time.Now().UnixNano()
	if m.st.Status != "ask-user" {
		m.st.resetPosition()
	}
	m.st.log("finish", "", "", m.st.Status+" — "+m.st.Reason)
}

// fail ends the run as an error with a reason code, e.g. "ensign" or "bad-report".
func (m *machine) fail(code, why string) { m.finish(Transition{Error: code}, why) }

// apply takes the transition for a stage's outcome.
func (m *machine) apply(scope string, s *Stage, outcome string) {
	t, _ := s.Outcomes.find(outcome)
	m.st.log("outcome", s.ID, "", outcome+" → "+t.String())
	into := t.Goto
	if t.Next {
		into = m.after(scope, s.ID)
	}
	if t.Spend != "" && !m.spend(t.Spend, scope, into, true) {
		if m.st.Resume != nil {
			m.st.Resume.Reset = t.Reset // the edge's reset still applies when it resumes
		}
		return
	}
	if t.Reset != "" {
		m.st.Spent[t.Reset] = 0
	}
	switch {
	case t.AskUser != "":
		m.st.Resume = &ResumePoint{Scope: scope, Stage: s.ID}
		m.finish(t, "")
	case t.Finish != "" && m.hasLead():
		// A pass does not end the run: its lead records whether the goal is met at the closing gate.
		m.toGate(s.ID, fmt.Sprintf("%s reported %s", s.ID, outcome))
	case t.terminal():
		m.finish(t, fmt.Sprintf("%s reported %s", s.ID, outcome))
	case t.Goto != "":
		m.enter(scope, t.Goto)
	case t.Next:
		if into == "" {
			m.fail("ensign", "next after the last stage "+s.ID)
			return
		}
		m.enter(scope, into)
	}
}

// after is the stage following id in its scope, or "".
func (m *machine) after(scope, id string) string {
	list := m.snap.Pipeline.scopeList(scope)
	for i, x := range list {
		if x.ID == id && i+1 < len(list) {
			return list[i+1].ID
		}
	}
	return ""
}

// resume continues an ask-user run where it stopped, with the user's answer.
func (m *machine) resume(answer string, extra map[string]int) error {
	st := m.st
	if st.Status != "ask-user" || st.Resume == nil {
		return fmt.Errorf("run %s is %s; only a run waiting on a question can resume", st.Run, st.Status)
	}
	if st.Extra == nil {
		st.Extra = map[string]int{}
	}
	if len(extra) == 0 && st.Resume.Budget != "" {
		extra = map[string]int{st.Resume.Budget: 1}
	}
	for b, n := range extra {
		st.Extra[b] += n
	}
	question := st.Reason
	st.Status, st.Reason, st.Approved, st.Finished = "running", "", true, false
	st.Answers = append(st.Answers, fmt.Sprintf("Q: %s\nA: %s", question, answer))
	st.log("resumed", "", "", answer)
	p := st.Resume
	st.Resume = nil
	if p.EdgeSpend {
		st.Spent[p.Budget]++ // the transition's spend, now within the larger budget
	}
	if p.Reset != "" {
		st.Spent[p.Reset] = 0
	}
	m.enter(p.Scope, p.Stage)
	return nil
}

// join combines the branches of a parallel stage: the outcome listed first wins.
func join(s *Stage, outcomes []string) string {
	for _, o := range s.Outcomes {
		for _, got := range outcomes {
			if got == o.Name {
				return o.Name
			}
		}
	}
	return ""
}

// expect describes what a scope should dispatch next, in words an agent can act on.
func (m *machine) expect(scope string) string {
	if m.st.terminal() {
		msg := fmt.Sprintf("The run is finished: %s (%s).", m.st.Status, m.st.Reason)
		if len(m.st.Notes) > 0 {
			msg += " " + strings.Join(m.st.Notes, " ") + "."
		}
		return msg + " Stop and report this."
	}
	if m.st.Gate != "" {
		bridge, run := shellQuote(m.snap.Bridge), m.st.Run
		return fmt.Sprintf("The pipeline has passed (%s). The run is not finished until you record whether the work item's goal is met: run `ensign run finish --bridge %s --run %s --goal met` if it is, or `ensign run finish --bridge %s --run %s --goal not-met \"<why>\"` if it is not.",
			m.st.Gate, bridge, run, bridge, run)
	}
	sc := m.st.Scopes[scope]
	if sc == nil || sc.Stage == "" {
		return "Nothing to dispatch from here."
	}
	s := m.stage(sc.Stage)
	running := m.running(scope, s.ID)
	if s.Parallel != nil {
		left := len(s.Dimensions) - len(sc.Branches)
		if left > 0 {
			return fmt.Sprintf("Dispatch %d %s agent(s) for stage %q — one per focus (%s); each is assigned its focus automatically.",
				left, s.Role, s.ID, strings.Join(s.Dimensions, ", "))
		}
		return fmt.Sprintf("Wait: stage %q has %d of %d %s agents still running.", s.ID, running, len(s.Dimensions), s.Role)
	}
	if running > 0 {
		return fmt.Sprintf("Wait: stage %q (%s) is still running.", s.ID, s.Role)
	}
	if plainStage(m.snap, s) {
		return fmt.Sprintf("Stage %q is run by ensign itself; start no agent for it. Run `ensign run-stage --bridge %s --run %s` as a background shell command (Bash with run_in_background), then end your turn to wait for it. When it completes, rule on its output as you would a tester's report and run `ensign next`.",
			s.ID, shellQuote(m.snap.Bridge), m.st.Run)
	}
	return fmt.Sprintf("Dispatch stage %q: one %s agent.", s.ID, s.Role)
}

func (m *machine) running(scope, stage string) int {
	n := 0
	for _, d := range m.st.Dispatches {
		if d.Scope == scope && d.Stage == stage && !d.Done {
			n++
		}
	}
	return n
}

// runningIn names a stage in scope with a dispatched agent still working, or "".
func (m *machine) runningIn(scope string) string {
	for _, d := range m.st.Dispatches {
		if d.Scope == scope && !d.Done {
			return d.Stage
		}
	}
	return ""
}
