// Package runtime — end-to-end tests with a fake provider (SSE): tool loop,
// text streaming, guardrail with a small window and tools.
package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func toolList() []tools.Spec { return tools.SpecsBuiltins() }

// fake httptest provider: a two-round script — asks for terminal_run, then
// closes with text streamed piece by piece with usage at the end.
func fakeProvider() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		wantTool := true
		for _, m := range req.Messages {
			if m.Role == "tool" {
				wantTool = false
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if wantTool {
			emit(map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"tool_calls": []map[string]any{{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{
						"name":      "terminal_run",
						"arguments": `{"command":"echo loop-tuple","timeout_secs":10}`,
					},
				}}},
			}}})
		} else {
			for _, p := range []string{"result ", "received ", "and closing."} {
				emit(map[string]any{"choices": []map[string]any{{
					"delta": map[string]any{"content": p},
				}}})
			}
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 30}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestTurnWithToolAndStreaming(t *testing.T) {
	srv := fakeProvider()
	defer srv.Close()
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "fake", APIKey: "k", Janela: 100_000}, t.TempDir(), "show that the loop runs"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"loop-tuple", "[tool terminal_run ok", "result received and closing."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output without %q:\n%s", want, out)
		}
	}
}

func TestTurnRequiresConfig(t *testing.T) {
	var sb strings.Builder
	err := Turno(&sb, config.Config{}, t.TempDir(), "hi")
	if err == nil {
		t.Fatal("turn without config must fail")
	}
}

func TestFileReadTool(t *testing.T) {
	cwd := t.TempDir()
	list := toolList()
	found := false
	for _, s := range list {
		if s.Name == "file_read" {
			found = true
			output, err := s.Exec(nil, cwd, json.RawMessage(`{"path":"missing.txt"}`))
			if err == nil {
				t.Fatalf("file_read of a missing file should fail, got %q", output)
			}
		}
	}
	if !found {
		t.Fatal("file_read is not in the tool list")
	}
}

func TestPrunePreservesTail(t *testing.T) {
	var h []historyMessage
	for i := 0; i < 40; i++ {
		h = append(h, historyMessage{Role: "tool_result", Conteudo: strings.Repeat("x", 4096), ToolName: "file_read"})
	}
	floor := 0
	pruned := prune(h, &floor, 2000)
	if pruned == nil {
		t.Fatal("pruning should free 2 KiB across 40 results of 4 KiB")
	}
	for i := len(pruned) - 24; i < len(pruned); i++ {
		if len(pruned[i].Conteudo) != 4096 {
			t.Fatalf("untouched tail of 24 was violated at index %d", i)
		}
	}
	if len(pruned[0].Conteudo) >= 4096 {
		t.Fatal("expected the oldest one to be pruned")
	}
}
