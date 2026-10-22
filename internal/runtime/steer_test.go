package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// steerRenderer records deliveries and, on the first tool start, injects steer
// messages into the running turn — the way the screen does from its key loop.
type steerRenderer struct {
	RendererPuro
	s         *Session
	once      sync.Once
	delivered []string
}

func (r *steerRenderer) Pensando()            {}
func (r *steerRenderer) Contexto(int)         {}
func (r *steerRenderer) Entregue(text string) { r.delivered = append(r.delivered, text) }
func (r *steerRenderer) ToolInicio(string, string) {
	r.once.Do(func() {
		r.s.AddSteer(Steer{Text: "also check B"})
		r.s.AddSteer(Steer{Text: "and C"})
	})
}

// A message sent while a tool runs reaches the model at the next request, in
// order, without cancelling the turn; the turn's final answer still lands.
func TestSteerDeliveredAtRoundBoundary(t *testing.T) {
	var mu sync.Mutex
	var requests [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var users []string
		toolDone := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolDone = true
			}
			if m.Role == "user" {
				b, _ := json.Marshal(m.Content)
				users = append(users, string(b))
			}
		}
		mu.Lock()
		requests = append(requests, users)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) {
			b, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		if !toolDone {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "shell", "arguments": `{"command":"echo hi"}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := NewSession(fakeConfig(t, srv.URL))
	var out strings.Builder
	r := &steerRenderer{RendererPuro: RendererPuro{out: &out}, s: s}
	if err := s.Send(context.Background(), r, t.TempDir(), "check A"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	second := strings.Join(requests[1], "|")
	if !strings.Contains(second, "also check B") || !strings.Contains(second, "and C") ||
		strings.Index(second, "also check B") > strings.Index(second, "and C") {
		t.Fatalf("second request users = %v", requests[1])
	}
	if strings.Contains(strings.Join(requests[0], "|"), "also check B") {
		t.Fatal("a steer must not leak into the request already in flight")
	}
	if strings.Join(r.delivered, "|") != "also check B|and C" {
		t.Fatalf("delivered = %v", r.delivered)
	}
	if !strings.Contains(out.String(), "done") {
		t.Fatalf("the turn must still finish: %q", out.String())
	}
	if left := s.TakeInbox(); len(left) != 0 {
		t.Fatalf("inbox left = %v", left)
	}
	// History keeps the steer in order after the tool result.
	h := s.History()
	var roles []string
	for _, m := range h {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,tool_call,tool_result,user,user,assistant" {
		t.Fatalf("history roles = %s", got)
	}
}

// A steer that arrives after the last request stays in the inbox for the caller
// (the screen sends it as the next prompt) instead of being dropped.
func TestSteerAfterLastRoundStaysQueued(t *testing.T) {
	srv := fakeProvider()
	defer srv.Close()
	s := NewSession(fakeConfig(t, srv.URL))
	s.AddSteer(Steer{Text: "late"})
	// No tool round ends before the answer here? fakeProvider makes one tool
	// round, so a steer queued BEFORE the turn is delivered at that boundary.
	if err := s.Send(context.Background(), nil, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	if left := s.TakeInbox(); len(left) != 0 {
		t.Fatalf("a queued steer must be delivered at the round boundary: %v", left)
	}
	s.AddSteer(Steer{Text: "after"})
	if left := s.TakeInbox(); len(left) != 1 || left[0].Text != "after" {
		t.Fatalf("take = %v", left)
	}
}
