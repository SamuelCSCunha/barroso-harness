// Package runtime — testes end-to-end com provider fake (SSE): loop com
// tool, streaming de texto, guardrail com janela pequena e ferramentas.
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

func toolsLista() []tools.Spec { return tools.SpecsBuiltins() }

// provider fake em httptest: roteiro de duas rodadas — pede terminal_run,
// depois fecha com texto streaming pedaço a pedaço com usage no fim.
func providerFake() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		pedeTool := true
		for _, m := range req.Messages {
			if m.Role == "tool" {
				pedeTool = false
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emitir := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if pedeTool {
			emitir(map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"tool_calls": []map[string]any{{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{
						"name":      "terminal_run",
						"arguments": `{"command":"echo tuple-do-loop","timeout_secs":10}`,
					},
				}}},
			}}})
		} else {
			for _, p := range []string{"resultado ", "recebido ", "e encerrando."} {
				emitir(map[string]any{"choices": []map[string]any{{
					"delta": map[string]any{"content": p},
				}}})
			}
		}
		emitir(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 30}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestTurnoComToolEStreaming(t *testing.T) {
	srv := providerFake()
	defer srv.Close()
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "fake", APIKey: "k", Janela: 100_000}, t.TempDir(), "mostre que o loop roda"); err != nil {
		t.Fatalf("turno falhou: %v", err)
	}
	out := sb.String()
	for _, esperado := range []string{"tuple-do-loop", "[tool terminal_run ok", "resultado recebido e encerrando."} {
		if !strings.Contains(out, esperado) {
			t.Fatalf("saída sem %q:\n%s", esperado, out)
		}
	}
}

func TestTurnoExigeConfig(t *testing.T) {
	var sb strings.Builder
	err := Turno(&sb, config.Config{}, t.TempDir(), "oi")
	if err == nil {
		t.Fatal("turno sem config deve falhar")
	}
}

func TestFileReadTool(t *testing.T) {
	cwd := t.TempDir()
	lista := toolsLista()
	achou := false
	for _, s := range lista {
		if s.Name == "file_read" {
			achou = true
			saída, err := s.Exec(nil, cwd, json.RawMessage(`{"path":"inexistente.txt"}`))
			if err == nil {
				t.Fatalf("file_read de inexistente deveria errar, veio %q", saída)
			}
		}
	}
	if !achou {
		t.Fatal("file_read não está na lista de tools")
	}
}

func TestPodarPreservaCauda(t *testing.T) {
	var h []MensagemHist
	for i := 0; i < 40; i++ {
		h = append(h, MensagemHist{Role: "tool_result", Conteudo: strings.Repeat("x", 4096), ToolName: "file_read"})
	}
	piso := 0
	podado := podar(h, &piso, 2000)
	if podado == nil {
		t.Fatal("poda deveria aliviar 2 KiB em 40 resultados de 4 KiB")
	}
	for i := len(podado) - 24; i < len(podado); i++ {
		if len(podado[i].Conteudo) != 4096 {
			t.Fatalf("cauda de 24 intocada foi violada no índice %d", i)
		}
	}
	if len(podado[0].Conteudo) >= 4096 {
		t.Fatal("esperava o mais antigo podado")
	}
}
