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
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// notaRenderer records Nota, the live tool events and Resposta in order.
type notaRenderer struct {
	recRenderer
	mu  sync.Mutex
	got []string
}

func (r *notaRenderer) note(e string) {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
}

func (r *notaRenderer) Nota(text string)                 { r.note("nota:" + text) }
func (r *notaRenderer) Resposta(text string)             { r.note("resposta:" + text) }
func (r *notaRenderer) ToolComecou(_, name, args string) { r.note("start:" + name + ":" + args) }
func (r *notaRenderer) ToolSaida(string, string)         {}
func (r *notaRenderer) ToolTerminou(_, name, _, _ string, _ time.Duration, _ bool) {
	r.note("end:" + name)
}

// textAndToolProvider answers the first round with text AND a tool call (the
// "let me check X" shape), the second with the final text.
func textAndToolProvider() *httptest.Server {
	var mu sync.Mutex
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1))
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) { b, _ := json.Marshal(p); _, _ = w.Write([]byte("data: " + string(b) + "\n\n")) }
		if first {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "let me check"}}}})
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "c0", "type": "function",
				"function": map[string]any{"name": "probe", "arguments": `{"path":"a.go"}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// Text written alongside tool calls reaches the renderer as Nota BEFORE the
// tool runs, and never as Resposta (which stays the final answer only).
func TestIntermediateTextBecomesNotaBeforeTools(t *testing.T) {
	srv := textAndToolProvider()
	defer srv.Close()
	s := NewSession(config.Config{BaseURL: srv.URL, Model: "fake", APIKey: "k", Janela: 100_000})
	s.Injetar(tools.Spec{Name: "probe", Schema: map[string]any{"type": "object"},
		Exec: func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil }})
	rr := &notaRenderer{}
	if err := s.Send(context.Background(), rr, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	want := `nota:let me check,start:probe:{"path":"a.go"},end:probe,resposta:done`
	if got := strings.Join(rr.got, ","); got != want {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
}
