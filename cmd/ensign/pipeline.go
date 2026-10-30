// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"fmt"
	"strings"
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
	m.finish(t, fmt.Sprintf("budget %s exhausted (%d)", budget, m.limit(budget)))
	return false
}

func (m *machine) budgetLine() string {
	var parts []string
	for _, k := range sortedKeys(m.snap.Pipeline.Budgets) {
		parts = append(parts, fmt.Sprintf("%s %d/%d", k, m.st.Spent[k], m.limit(k)))
	}
	return strings.Join(parts, ", ")
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
	if m.st.Status != "ask-user" {
		for _, sc := range m.st.Scopes {
			sc.Stage, sc.Branches = "", nil
		}
		m.st.Resume = nil
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
	st.Status, st.Reason, st.Approved = "running", "", true
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
		return fmt.Sprintf("The run is finished: %s (%s). Stop and report this.", m.st.Status, m.st.Reason)
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
