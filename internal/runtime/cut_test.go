package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// seenRequest records the max_tokens and the last message of each request.
type seenRequest struct {
	MaxTokens int
	Last      string
}

// cutProvider answers, in order, with the given steps: each step is the text,
// whether it has a tool call and the finish_reason.
func cutProvider(t *testing.T, steps []struct {
	text   string
	tool   bool
	reason string
}) (*httptest.Server, *[]seenRequest) {
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
			Messages  []struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		i := len(seen)
		last := ""
		if n := len(req.Messages); n > 0 && req.Messages[n-1].Content != nil {
			last = *req.Messages[n-1].Content
		}
		seen = append(seen, seenRequest{MaxTokens: req.MaxTokens, Last: last})
		mu.Unlock()
		if i >= len(steps) {
			i = len(steps) - 1
		}
		p := steps[i]
		emit := func(v any) { b, _ := json.Marshal(v); _, _ = w.Write([]byte("data: " + string(b) + "\n\n")) }
		delta := map[string]any{}
		if p.text != "" {
			delta["content"] = p.text
		}
		if p.tool {
			delta["tool_calls"] = []map[string]any{{"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "terminal_run", "arguments": `{"command":"echo must-not-run"`}}}
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": delta}}})
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": p.reason}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 50}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

type step = struct {
	text   string
	tool   bool
	reason string
}

// Cut and empty response (only thought): retry with twice the limit.
func TestEmptyCutRetriesWithLargerLimit(t *testing.T) {
	srv, seen := cutProvider(t, []step{{"", false, "length"}, {"done", false, "stop"}})
	var sb strings.Builder
	err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k", Effort: "max"}, t.TempDir(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	v := *seen
	if len(v) != 2 || v[1].MaxTokens != 2*v[0].MaxTokens || v[0].MaxTokens < 32<<10 {
		t.Fatalf("limits = %+v", v)
	}
	if !strings.Contains(sb.String(), "done") || !strings.Contains(sb.String(), "[output limit]") {
		t.Fatalf("output:\n%s", sb.String())
	}
}

// Cut text: it stays, and the model is asked to continue; a cut tool call
// does not run.
func TestCutWithTextContinues(t *testing.T) {
	srv, seen := cutProvider(t, []step{{"part one ", true, "length"}, {"part two", false, "stop"}})
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "hi"); err != nil {
		t.Fatal(err)
	}
	v := *seen
	if len(v) != 2 || !strings.HasPrefix(v[1].Last, "[output limit]") || !strings.Contains(v[1].Last, "NOT executed") {
		t.Fatalf("requests = %+v", v)
	}
	out := sb.String()
	if strings.Contains(out, "[tool terminal_run") || !strings.Contains(out, "part one") || !strings.Contains(out, "part two") {
		t.Fatalf("output:\n%s", out)
	}
}

// Too many consecutive cuts: stop with an error instead of ending silently.
func TestCutGivesUpAtLimit(t *testing.T) {
	srv, seen := cutProvider(t, []step{{"", false, "length"}})
	var sb strings.Builder
	err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "hi")
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("error = %v", err)
	}
	if n := len(*seen); n != maxCuts+1 {
		t.Fatalf("requests = %d", n)
	}
}

type thinkingRenderer struct {
	*RendererPuro
	thought string
}

func (r *thinkingRenderer) Pensou(text string, _ time.Duration) { r.thought += text }

// The reasoning the provider exposes (reasoning / reasoning_content) reaches
// the renderer and does not enter the answer.
func TestReasoningGoesToRenderer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, d := range []map[string]any{
			{"reasoning": "think ", "reasoning_content": "think "},
			{"reasoning_content": "more"},
			{"content": "answer"},
		} {
			b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": d}}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	var sb strings.Builder
	r := &thinkingRenderer{RendererPuro: NovoRendererPuro(&sb)}
	s := NewSession(config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"})
	if err := s.Send(context.Background(), r, t.TempDir(), "hi"); err != nil {
		t.Fatal(err)
	}
	if r.thought != "think more" || strings.Contains(sb.String(), "think") || !strings.Contains(sb.String(), "answer") {
		t.Fatalf("thought=%q output=%q", r.thought, sb.String())
	}
}
