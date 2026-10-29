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
	if s.Spend != "" && !m.spend(s.Spend) {
		return
	}
	m.st.log("enter", id, "", m.budgetLine())
}

// spend charges one unit of a budget. If that exceeds it, the budget's on-exhausted
// transition ends the run and spend reports false.
func (m *machine) spend(budget string) bool {
	m.st.Spent[budget]++
	if m.st.Spent[budget] <= m.snap.Pipeline.Budgets[budget] {
		return true
	}
	t := m.snap.Pipeline.OnExhausted[budget]
	if !t.terminal() {
		t = Transition{Error: "budget:" + budget}
	}
	m.finish(t, fmt.Sprintf("budget %s exhausted (%d)", budget, m.snap.Pipeline.Budgets[budget]))
	return false
}

func (m *machine) budgetLine() string {
	var parts []string
	for _, k := range sortedKeys(m.snap.Pipeline.Budgets) {
		parts = append(parts, fmt.Sprintf("%s %d/%d", k, m.st.Spent[k], m.snap.Pipeline.Budgets[k]))
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
	for _, sc := range m.st.Scopes {
		sc.Stage, sc.Branches = "", nil
	}
	m.st.log("finish", "", "", m.st.Status+" — "+m.st.Reason)
}

// fail ends the run as an error with a reason code, e.g. "ensign" or "bad-report".
func (m *machine) fail(code, why string) { m.finish(Transition{Error: code}, why) }

// apply takes the transition for a stage's outcome.
func (m *machine) apply(scope string, s *Stage, outcome string) {
	t, _ := s.Outcomes.find(outcome)
	m.st.log("outcome", s.ID, "", outcome+" → "+t.String())
	if t.Spend != "" && !m.spend(t.Spend) {
		return
	}
	if t.Reset != "" {
		m.st.Spent[t.Reset] = 0
	}
	switch {
	case t.terminal():
		m.finish(t, fmt.Sprintf("%s reported %s", s.ID, outcome))
	case t.Goto != "":
		m.enter(scope, t.Goto)
	case t.Next:
		list := m.snap.Pipeline.scopeList(scope)
		for i, x := range list {
			if x.ID == s.ID && i+1 < len(list) {
				m.enter(scope, list[i+1].ID)
				return
			}
		}
		m.fail("ensign", "next after the last stage "+s.ID)
	}
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
