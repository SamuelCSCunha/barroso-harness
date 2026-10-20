package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// roundStats is what one "round" event records (see runtime.Session).
type roundStats struct {
	PromptTokens int   `json:"prompt_tokens"`
	CachedTokens int   `json:"cached_tokens"`
	OutputTokens int   `json:"output_tokens"`
	TTFTms       int64 `json:"ttft_ms"`
	DurationMs   int64 `json:"duration_ms"`
	ToolsBytes   int   `json:"tools_bytes"`
	SystemBytes  int   `json:"system_bytes"`
	ToolCalls    int   `json:"tool_calls"`
}

// sessionTotals sums the rounds of one session.
type sessionTotals struct {
	rounds, calls           int
	prompt, cached, output  int
	ttft, dur               time.Duration
	toolsBytes, systemBytes int
}

func (t *sessionTotals) add(r roundStats) {
	t.rounds++
	t.calls += r.ToolCalls
	t.prompt += r.PromptTokens
	t.cached += r.CachedTokens
	t.output += r.OutputTokens
	t.ttft += time.Duration(r.TTFTms) * time.Millisecond
	t.dur += time.Duration(r.DurationMs) * time.Millisecond
	t.toolsBytes, t.systemBytes = r.ToolsBytes, r.SystemBytes
}

// readTotals reads every round event of a session.
func readTotals(st *store.Store, id string) (sessionTotals, error) {
	var t sessionTotals
	from := 0
	for {
		evs, err := st.ListEvents(id, from, 500)
		if err != nil {
			return t, err
		}
		if len(evs) == 0 {
			return t, nil
		}
		for _, e := range evs {
			from = e.Seq + 1
			if e.Kind != "round" {
				continue
			}
			var r roundStats
			if json.Unmarshal([]byte(e.Payload), &r) == nil {
				t.add(r)
			}
		}
	}
}

// printTotals writes one session's line: rounds, cache hit rate, latency and
// the size of what goes out on every request.
func printTotals(out io.Writer, label string, t sessionTotals) {
	if t.rounds == 0 {
		fmt.Fprintf(out, "%s  no measured rounds\n", label)
		return
	}
	hit := 0
	if t.prompt > 0 {
		hit = 100 * t.cached / t.prompt
	}
	fmt.Fprintf(out, "%s  rounds %d  tool calls %d  prompt %d  cached %d (%d%%)  output %d  model time %s  avg ttft %s  tools %d B  system %d B\n",
		label, t.rounds, t.calls, t.prompt, t.cached, hit, t.output,
		t.dur.Round(time.Second), (t.ttft / time.Duration(t.rounds)).Round(10*time.Millisecond),
		t.toolsBytes, t.systemBytes)
}

// runStats prints the metrics of the given sessions, or of the latest n
// sessions when none is given.
func runStats(out io.Writer, ids []string, n int) error {
	st, err := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Fechar() }()
	if len(ids) == 0 {
		rows, err := st.ListSessions(n)
		if err != nil {
			return err
		}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		t, err := readTotals(st, id)
		if err != nil {
			return err
		}
		printTotals(out, id, t)
	}
	return nil
}
