// Session-log retrieval tools. They live in runtime, not in package tools,
// because they close over the Session: they need the store to read the log,
// and package tools cannot import runtime (it would be a cycle). Send appends
// these specs to the built-ins before building the schemas, so the model sees
// them like any other tool.
//
// The problem they solve: the guardrail compacts large tool results so the
// turn fits in the window, and the transcript drops the rest. Without
// retrieval, the content is gone — the agent knows something was there but
// cannot read it again. These two tools provide the way back.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

const defaultSearchLimit = 10
const defaultRetrieveLimit = 8 << 10 // 8 KiB: retrieve must not blow the window

// SessaoSpecs returns the session tools for the given Session. Without
// persistence they stay in the list (the schema is the same), but they return
// a message explaining that the log is not active — better than the tool not
// existing and the model retrying.
func SessaoSpecs(s *Session) []tools.Spec {
	return []tools.Spec{
		sessionSearchSpec(s),
		sessionRetrieveSpec(s),
	}
}

func sessionSearchSpec(s *Session) tools.Spec {
	return tools.Spec{
		Name: "session_search",
		Description: "Searches the current session's history (substring, case-insensitive) and returns " +
			"the matching excerpts, with the event number (seq). Use it to find again content " +
			"that guardrail compaction removed from the window or that left the transcript: large " +
			"tool outputs, old decisions, paths already read. Combined with session_retrieve it " +
			"recovers the whole text. Only sees this session.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "text to search for (substring, case-insensitive)"},
				"limit": map[string]any{"type": "integer", "description": "how many events to return; default 10"},
			},
			"required": []string{"query"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if strings.TrimSpace(c.Query) == "" {
				return "", fmt.Errorf("empty query")
			}
			if !s.Persistente() {
				return "(session without persistence: the log is not recording, there is nothing to search)", nil
			}
			limit := c.Limit
			if limit <= 0 {
				limit = defaultSearchLimit
			}
			hits := s.Search(c.Query, limit)
			if len(hits) == 0 {
				return fmt.Sprintf("no excerpt with %q in this session's history", c.Query), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%d excerpt(s) with %q — use session_retrieve with the seq for the whole text:\n", len(hits), c.Query)
			for _, h := range hits {
				fmt.Fprintf(&sb, "seq %d [%s] %s\n", h.Seq, h.Kind, h.Snippet)
			}
			return sb.String(), nil
		},
	}
}

func sessionRetrieveSpec(s *Session) tools.Spec {
	return tools.Spec{
		Name: "session_retrieve",
		Description: "Returns ONE event of the current session's history by number (seq), with its " +
			"full content — including large tool outputs that were archived in a blob " +
			"during compaction. The seq comes from session_search. Use it when an excerpt is not " +
			"enough and you need the whole text back.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"seq":   map[string]any{"type": "integer", "description": "event number (seq), as returned by session_search"},
				"limit": map[string]any{"type": "integer", "description": "byte limit of the returned content; default 8192"},
			},
			"required": []string{"seq"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Seq   int `json:"seq"`
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if !s.Persistente() {
				return "(session without persistence: the log is not recording, there is no event to retrieve)", nil
			}
			if c.Seq <= 0 {
				return "", fmt.Errorf("invalid seq: %d", c.Seq)
			}
			limit := c.Limit
			if limit <= 0 {
				limit = defaultRetrieveLimit
			}
			e, err := s.Retrieve(c.Seq)
			if err != nil {
				return "", fmt.Errorf("event %d not retrieved: %w", c.Seq, err)
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "seq %d [%s] %s\n", e.Seq, e.Kind, e.Ts)
			body := e.Payload
			if len(body) > limit {
				body = body[:limit] + fmt.Sprintf("\n\n[truncated at %d of %d bytes; ask for a larger limit]", limit, len(e.Payload))
			}
			sb.WriteString(body)
			return sb.String(), nil
		},
	}
}
