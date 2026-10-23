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
		sessionsListSpec(s),
	}
}

func sessionSearchSpec(s *Session) tools.Spec {
	return tools.Spec{
		Name: "session_search",
		Description: "THIS SESSION ONLY by default: searches the history (substring, case-insensitive) and " +
			"returns the matching excerpts, each with its event number (seq) and the session it came from. " +
			"Use it to find again content that guardrail compaction removed from the window or that left the " +
			"transcript: large tool outputs, old decisions, paths already read. Combined with session_retrieve it " +
			"recovers the whole text. To search ANOTHER session, pass its id from session_list; the " +
			"current session's id is the first line of every result. When you do not know which session the " +
			"user means, ask instead of guessing.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":     map[string]any{"type": "string", "description": "text to search for (substring, case-insensitive)"},
				"limit":     map[string]any{"type": "integer", "description": "how many events to return; default 10"},
				"sessionID": map[string]any{"type": "string", "description": "ANOTHER session's id, from session_list; omit for THIS session"},
			},
			"required": []string{"query"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Query     string `json:"query"`
				Limit     int    `json:"limit"`
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if strings.TrimSpace(c.Query) == "" {
				return "", fmt.Errorf("empty query")
			}
			target := strings.TrimSpace(c.SessionID)
			if target == "" {
				if !s.Persistente() {
					return "(session without persistence: the log is not recording, there is nothing to search)", nil
				}
				target = s.sessionID
			} else if !s.Persistente() {
				return "(session without persistence: another session's log cannot be read)", nil
			}
			if !s.store.Known(target) {
				return fmt.Sprintf("no session with id %q (see session_list)", target), nil
			}
			limit := c.Limit
			if limit <= 0 {
				limit = defaultSearchLimit
			}
			hits, err := s.store.Buscar(target, c.Query, limit)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return fmt.Sprintf("no excerpt with %q in %s", c.Query, sessionLabel(s, target)), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "session %s: %d excerpt(s) with %q — use session_retrieve with the seq for the whole text:\n", sessionLabel(s, target), len(hits), c.Query)
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
		Description: "Returns ONE event by its number (seq) with its FULL content — including large tool " +
			"outputs archived in a blob during compaction. THIS SESSION ONLY by default: the seq comes from " +
			"session_search, which states the session it belongs to; pass the same sessionID to read an event of " +
			"ANOTHER session. Use it when an excerpt is not enough and you need the whole text back.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"seq":       map[string]any{"type": "integer", "description": "event number (seq), as returned by session_search"},
				"limit":     map[string]any{"type": "integer", "description": "byte limit of the returned content; default 8192"},
				"sessionID": map[string]any{"type": "string", "description": "ANOTHER session's id, from session_list; omit for THIS session"},
			},
			"required": []string{"seq"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Seq       int    `json:"seq"`
				Limit     int    `json:"limit"`
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Seq <= 0 {
				return "", fmt.Errorf("invalid seq: %d", c.Seq)
			}
			target := strings.TrimSpace(c.SessionID)
			if target == "" {
				if !s.Persistente() {
					return "(session without persistence: the log is not recording, there is no event to retrieve)", nil
				}
				target = s.sessionID
			} else if !s.Persistente() {
				return "(session without persistence: another session's log cannot be read)", nil
			}
			limit := c.Limit
			if limit <= 0 {
				limit = defaultRetrieveLimit
			}
			e, err := s.store.GetEvent(target, c.Seq)
			if err != nil {
				return "", fmt.Errorf("event %d not retrieved from %s: %w", c.Seq, sessionLabel(s, target), err)
			}
			if full, rerr := s.store.Rehydrate(e.Payload); rerr == nil {
				e.Payload = full
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "session %s — seq %d [%s] %s\n", sessionLabel(s, target), e.Seq, e.Kind, e.Ts)
			body := e.Payload
			if len(body) > limit {
				body = body[:limit] + fmt.Sprintf("\n\n[truncated at %d of %d bytes; ask for a larger limit]", limit, len(e.Payload))
			}
			sb.WriteString(body)
			return sb.String(), nil
		},
	}
}

// sessionLabel names a session in a result: "<title> (<id>)", or only the id
// when there is no title. The CURRENT session is marked, so the model can tell
// its own log from another one's without a second call.
func sessionLabel(s *Session, id string) string {
	title := ""
	if row, err := s.store.GetSession(id); err == nil {
		title = strings.TrimSpace(row.Title)
	}
	if title == "" {
		if id == s.sessionID {
			return "current (" + id + ")"
		}
		return id
	}
	if id == s.sessionID {
		return title + " (current: " + id + ")"
	}
	return title + " (" + id + ")"
}

// sessionsListSpec lists the sessions recorded in this machine's database:
// id, title and when each was last updated. METADATA ONLY — no content, so a
// result answers "which session was that?" without pulling another
// conversation's text into this one.
func sessionsListSpec(s *Session) tools.Spec {
	return tools.Spec{
		Name: "session_list",
		Description: "Lists the sessions recorded on THIS machine (id, title, last update), most recent first. " +
			"Metadata only: it returns no conversation content. Use it to find the id of a session the user " +
			"mentions (\"the one about the connector\"), then read that session with session_search/session_retrieve " +
			"passing the id. The current session is marked. Sessions from other machines or accounts are not here.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "how many sessions to list; default 20"},
			},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if !s.Persistente() {
				return "(session without persistence: the log is not recording, there is no session list)", nil
			}
			limit := c.Limit
			if limit <= 0 {
				limit = 20
			}
			rows, err := s.store.ListSessions(limit)
			if err != nil {
				return "", err
			}
			if len(rows) == 0 {
				return "no session recorded yet.", nil
			}
			var sb strings.Builder
			for _, r := range rows {
				// Title only: an untitled session shows "(untitled)" and NOT
				// its first message, which is conversation content and would
				// leak another session's text into this one.
				name := strings.TrimSpace(r.Title)
				if name == "" {
					name = "(untitled)"
				}
				marker := ""
				if r.ID == s.sessionID {
					marker = " [current]"
				}
				fmt.Fprintf(&sb, "%s — %s — updated %s%s\n", r.ID, name, r.UpdatedAt, marker)
			}
			return sb.String(), nil
		},
	}
}
