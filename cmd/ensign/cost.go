package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// What each agent of a run used, read from its transcript, and what that would cost at
// API prices. On a subscription nothing is billed per token; the figure still shows where
// a run spends.

// Usage is the token usage of one agent's transcript.
type Usage struct {
	Model      string  `json:"model,omitempty"` // the last model seen
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Cost       float64 `json:"cost"`               // USD at API prices, for the models priced below
	Unpriced   string  `json:"unpriced,omitempty"` // models seen that have no price here
}

func (u Usage) tokens() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// price is USD per million tokens. Cache writes cost 1.25× input for the 5-minute cache
// and 2× for the 1-hour cache.
type price struct{ in, out, read float64 }

var prices = map[string]price{
	"claude-fable-5-1":  {10, 50, 0.25},
	"claude-opus-5-5":   {4, 20, 0.20},
	"claude-opus-5":     {5, 25, 0.50},
	"claude-sonnet-5-5": {2, 10, 0.20},
	"claude-sonnet-5":   {2, 10, 0.20},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01},
	"claude-haiku-4-5":  {1, 5, 0.10},
}

// transcriptUsage adds up the usage of every model response in a transcript. A response
// is written once per content block, so each message id counts once, at its last record.
func transcriptUsage(path string) (Usage, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Usage{}, false
	}
	defer f.Close()
	type usage struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		ByTTL         struct {
			M5 int64 `json:"ephemeral_5m_input_tokens"`
			H1 int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	}
	type msg struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *usage `json:"usage"`
	}
	byID := map[string]msg{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			Type    string `json:"type"`
			Message msg    `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil || rec.Message.ID == "" {
			continue
		}
		if _, seen := byID[rec.Message.ID]; !seen {
			order = append(order, rec.Message.ID)
		}
		byID[rec.Message.ID] = rec.Message
	}
	var u Usage
	unpriced := map[string]bool{}
	for _, id := range order {
		m := byID[id]
		x := m.Usage
		u.Model = m.Model
		u.Input += x.Input
		u.Output += x.Output
		u.CacheRead += x.CacheRead
		u.CacheWrite += x.CacheCreation
		p, ok := prices[m.Model]
		if !ok {
			unpriced[m.Model] = true
			continue
		}
		w5, w1 := x.ByTTL.M5, x.ByTTL.H1
		if w5+w1 == 0 {
			w5 = x.CacheCreation
		}
		u.Cost += (float64(x.Input)*p.in + float64(x.Output)*p.out + float64(x.CacheRead)*p.read +
			float64(w5)*p.in*1.25 + float64(w1)*p.in*2) / 1e6
	}
	u.Unpriced = strings.Join(sortedStrings(unpriced), ", ")
	return u, len(order) > 0
}

// recordUsage refreshes a dispatch's usage from its agent's transcript.
func recordUsage(r *RunFiles, d *Dispatch) {
	if d.AgentID == "" || r.State.Transcript == "" {
		return
	}
	if u, ok := transcriptUsage(agentTranscript(r.State.Transcript, d.AgentID)); ok {
		d.Usage = &u
	}
}

// costLine sums a run's usage by stage, in the order stages first ran.
func costLine(r *RunFiles) string {
	type sum struct {
		cost   float64
		tokens int64
		n      int
	}
	by := map[string]*sum{}
	var order []string
	var total float64
	unpriced := map[string]bool{}
	for _, d := range r.State.Dispatches {
		recordUsage(r, d)
		if d.Usage == nil {
			continue
		}
		s := by[d.Stage]
		if s == nil {
			s = &sum{}
			by[d.Stage] = s
			order = append(order, d.Stage)
		}
		s.cost += d.Usage.Cost
		s.tokens += d.Usage.tokens()
		s.n++
		total += d.Usage.Cost
		for _, m := range strings.Split(d.Usage.Unpriced, ", ") {
			if m != "" {
				unpriced[m] = true
			}
		}
	}
	if len(order) == 0 {
		return ""
	}
	var parts []string
	for _, id := range order {
		s := by[id]
		p := fmt.Sprintf("%s $%.2f", id, s.cost)
		if s.n > 1 {
			p += fmt.Sprintf(" (%d agents)", s.n)
		}
		parts = append(parts, p)
	}
	line := fmt.Sprintf(" Cost at API prices: $%.2f — %s.", total, strings.Join(parts, ", "))
	if len(unpriced) > 0 {
		line += " Not priced: " + strings.Join(sortedStrings(unpriced), ", ") + "."
	}
	return line
}
