// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spend appends model responses to an agent's transcript. Each response is written twice,
// as Claude Code does for a message with two content blocks.
func (w *world) spend(agent, model string, input, output, read, write5m int64) {
	w.t.Helper()
	f, err := os.OpenFile(agentTranscript(w.session, agent), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		w.t.Fatal(err)
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{
		"id": "msg_" + agent + model, "model": model, "usage": map[string]any{
			"input_tokens": input, "output_tokens": output, "cache_read_input_tokens": read,
			"cache_creation_input_tokens": write5m,
			"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": write5m, "ephemeral_1h_input_tokens": 0},
		}}})
	f.Write(append(rec, '\n'))
	f.Write(append(rec, '\n'))
}

func TestTranscriptUsageCountsEachMessageOnce(t *testing.T) {
	w := newWorld(t)
	write(t, agentTranscript(w.session, "a1"), "")
	w.spend("a1", "claude-opus-5-5", 1_000_000, 100_000, 2_000_000, 400_000)
	u, ok := transcriptUsage(agentTranscript(w.session, "a1"))
	// 4 + 2 (output at 20) + 0.40 (reads at 0.20) + 2 (writes at 5)
	if !ok || u.Input != 1_000_000 || math.Abs(u.Cost-8.40) > 1e-9 {
		t.Fatalf("usage %+v", u)
	}
	w.spend("a1", "claude-unknown-9", 10, 10, 0, 0)
	if u, _ = transcriptUsage(agentTranscript(w.session, "a1")); u.Unpriced != "claude-unknown-9" || math.Abs(u.Cost-8.40) > 1e-9 {
		t.Fatalf("an unknown model should be counted but not priced: %+v", u)
	}
}

func TestRunCostIsLoggedByStage(t *testing.T) {
	w := newWorld(t)
	lead := w.startLead()
	w.spend(lead, "claude-opus-5-5", 0, 50_000, 0, 0) // $1.00
	coder, _ := w.spawn(lead, "orchestrator", "coder")
	w.spend(coder, "claude-sonnet-5-5", 0, 50_000, 0, 0) // $0.50
	w.stop(coder, "coder", report("compiled"), false)
	for range 2 {
		r, _ := w.spawn(lead, "orchestrator", "reviewer")
		w.spend(r, "claude-opus-5-5", 0, 10_000, 0, 0) // $0.20 each
		w.stop(r, "reviewer", report("clean"), false)
	}
	w.leaf(lead, "style-reviewer", "done")
	w.leaf(lead, "tester", "pass")
	data, _ := os.ReadFile(filepath.Join(w.bridge, "projects/supernova/tasks/auth-refactor/history.md"))
	want := "Cost at API prices: $1.90 — run $1.00, code $0.50, review $0.40 (2 agents)."
	if !strings.Contains(string(data), want) {
		t.Fatalf("history.md should carry the cost by stage:\n%s", data)
	}
}
