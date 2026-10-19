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

// providerCorte responde, em ordem, com os passos dados: cada passo é o texto,
// se tem tool call e o finish_reason. Guarda o max_tokens e as mensagens de
// cada request.
type pedidoVisto struct {
	MaxTokens int
	Ultima    string
}

func providerCorte(t *testing.T, passos []struct {
	texto  string
	tool   bool
	motivo string
}) (*httptest.Server, *[]pedidoVisto) {
	var mu sync.Mutex
	var vistos []pedidoVisto
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
		i := len(vistos)
		ult := ""
		if n := len(req.Messages); n > 0 && req.Messages[n-1].Content != nil {
			ult = *req.Messages[n-1].Content
		}
		vistos = append(vistos, pedidoVisto{MaxTokens: req.MaxTokens, Ultima: ult})
		mu.Unlock()
		if i >= len(passos) {
			i = len(passos) - 1
		}
		p := passos[i]
		emitir := func(v any) { b, _ := json.Marshal(v); _, _ = w.Write([]byte("data: " + string(b) + "\n\n")) }
		delta := map[string]any{}
		if p.texto != "" {
			delta["content"] = p.texto
		}
		if p.tool {
			delta["tool_calls"] = []map[string]any{{"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "terminal_run", "arguments": `{"command":"echo nao-deve-rodar"`}}}
		}
		emitir(map[string]any{"choices": []map[string]any{{"delta": delta}}})
		emitir(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": p.motivo}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 50}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &vistos
}

type passo = struct {
	texto  string
	tool   bool
	motivo string
}

// Resposta cortada e vazia (só pensou): tenta de novo com o dobro do teto.
func TestCorteVazioTentaComTetoMaior(t *testing.T) {
	srv, vistos := providerCorte(t, []passo{{"", false, "length"}, {"pronto", false, "stop"}})
	var sb strings.Builder
	err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k", Effort: "max"}, t.TempDir(), "oi")
	if err != nil {
		t.Fatal(err)
	}
	v := *vistos
	if len(v) != 2 || v[1].MaxTokens != 2*v[0].MaxTokens || v[0].MaxTokens < 32<<10 {
		t.Fatalf("tetos = %+v", v)
	}
	if !strings.Contains(sb.String(), "pronto") || !strings.Contains(sb.String(), "[output limit]") {
		t.Fatalf("saída:\n%s", sb.String())
	}
}

// Texto cortado: fica, e o modelo recebe o pedido de continuar; tool call
// cortada não roda.
func TestCorteComTextoContinua(t *testing.T) {
	srv, vistos := providerCorte(t, []passo{{"parte um ", true, "length"}, {"parte dois", false, "stop"}})
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "oi"); err != nil {
		t.Fatal(err)
	}
	v := *vistos
	if len(v) != 2 || !strings.HasPrefix(v[1].Ultima, "[output limit]") || !strings.Contains(v[1].Ultima, "NOT executed") {
		t.Fatalf("pedidos = %+v", v)
	}
	out := sb.String()
	if strings.Contains(out, "[tool terminal_run") || !strings.Contains(out, "parte um") || !strings.Contains(out, "parte dois") {
		t.Fatalf("saída:\n%s", out)
	}
}

// Cortes seguidos demais: para com erro em vez de encerrar calado.
func TestCorteDesistePorLimite(t *testing.T) {
	srv, vistos := providerCorte(t, []passo{{"", false, "length"}})
	var sb strings.Builder
	err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "oi")
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("erro = %v", err)
	}
	if n := len(*vistos); n != maxCortes+1 {
		t.Fatalf("requests = %d", n)
	}
}

type rendPensou struct {
	*RendererPuro
	pensou string
}

func (r *rendPensou) Pensou(texto string, _ time.Duration) { r.pensou += texto }

// O raciocínio que o provider expõe (reasoning / reasoning_content) chega ao
// renderer, e não entra na resposta.
func TestRaciocinioVaiProRenderer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, d := range []map[string]any{
			{"reasoning": "pensa ", "reasoning_content": "pensa "},
			{"reasoning_content": "mais"},
			{"content": "resposta"},
		} {
			b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": d}}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	var sb strings.Builder
	r := &rendPensou{RendererPuro: NovoRendererPuro(&sb)}
	s := NewSession(config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"})
	if err := s.Send(context.Background(), r, t.TempDir(), "oi"); err != nil {
		t.Fatal(err)
	}
	if r.pensou != "pensa mais" || strings.Contains(sb.String(), "pensa") || !strings.Contains(sb.String(), "resposta") {
		t.Fatalf("pensou=%q saída=%q", r.pensou, sb.String())
	}
}
