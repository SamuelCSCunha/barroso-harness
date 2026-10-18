package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

func TestCompactarHistoricoPreservaPedidoAtualERodada(t *testing.T) {
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "pedido antigo"},
		{Role: "assistant", Conteudo: "resposta antiga"},
		{Role: "user", Conteudo: "pedido atual"},
		{Role: "tool_call", ToolID: "c1", ToolName: "ctx_compact", ToolArgs: `{"estado":"x"}`},
		{Role: "tool_result", ToolID: "c1", ToolName: "ctx_compact", Conteudo: "ok"},
	}
	out := compactarHistorico(h, "objetivo: X")
	if len(out) != 4 {
		t.Fatalf("esperava resumo + 3 mensagens do pedido atual, veio %d: %+v", len(out), out)
	}
	if out[0].Role != "user" || !strings.HasPrefix(out[0].Conteudo, prefixoResumo) || !strings.Contains(out[0].Conteudo, "objetivo: X") {
		t.Fatalf("primeira mensagem deveria ser o estado: %+v", out[0])
	}
	if out[1].Conteudo != "pedido atual" || out[2].Role != "tool_call" || out[3].Role != "tool_result" {
		t.Fatalf("pedido atual e rodada deveriam ficar intactos: %+v", out[1:])
	}
}

// Sem nada antes do pedido atual não há o que compactar.
func TestCompactarHistoricoSemAnteriorNaoMexe(t *testing.T) {
	h := []dealer.Mensagem{{Role: "user", Conteudo: "único"}}
	if out := compactarHistorico(h, "e"); len(out) != 1 || out[0].Conteudo != "único" {
		t.Fatalf("não deveria mudar: %+v", out)
	}
}

// Compactar de novo substitui o resumo anterior em vez de empilhar.
func TestCompactarHistoricoRecompactaSemEmpilhar(t *testing.T) {
	h := compactarHistorico([]dealer.Mensagem{
		{Role: "user", Conteudo: "a"}, {Role: "assistant", Conteudo: "b"}, {Role: "user", Conteudo: "c"},
	}, "e1")
	h = append(h, dealer.Mensagem{Role: "assistant", Conteudo: "d"}, dealer.Mensagem{Role: "user", Conteudo: "f"})
	out := compactarHistorico(h, "e2")
	if len(out) != 2 || !strings.Contains(out[0].Conteudo, "e2") || strings.Contains(out[0].Conteudo, "e1") || out[1].Conteudo != "f" {
		t.Fatalf("recompactação errada: %+v", out)
	}
}

func TestEstadoCompactadoIgnoraChamadaComErro(t *testing.T) {
	calls := []dealer.ToolChamada{{ID: "1", Nome: nomeCompactar, Args: `{"estado":"bom"}`}}
	if e := estadoCompactado(calls, []dealer.Mensagem{{IsErro: true}}); e != "" {
		t.Fatalf("chamada com erro não compacta, veio %q", e)
	}
	if e := estadoCompactado(calls, []dealer.Mensagem{{}}); e != "bom" {
		t.Fatalf("estado = %q", e)
	}
}

func TestSpecCompactarValida(t *testing.T) {
	sp := specCompactar()
	if _, err := sp.Exec(context.Background(), "", json.RawMessage(`{"estado":"  "}`)); err == nil {
		t.Fatal("estado vazio deveria falhar")
	}
	if _, err := sp.Exec(context.Background(), "", json.RawMessage(`{"estado":"`+strings.Repeat("x", tetoResumo+1)+`"}`)); err == nil {
		t.Fatal("estado acima do teto deveria falhar")
	}
}

// A política escolhe se o modelo vê ctx_compact.
func TestPoliticaControlaCatalogo(t *testing.T) {
	tem := func(p string) bool {
		s := NewSession(config.Config{PoliticaContexto: p})
		for _, sp := range s.catalogoTools(t.TempDir()) {
			if sp.Name == nomeCompactar {
				return true
			}
		}
		return false
	}
	if !tem("") || !tem("agente") {
		t.Error("política padrão/agente deveria expor ctx_compact")
	}
	if tem("limiar") || tem("LIMIAR") {
		t.Error("política limiar não deveria expor ctx_compact")
	}
}

// Ponta a ponta: o modelo chama ctx_compact no meio do segundo pedido; o
// request seguinte já vai com o histórico compactado, e a sessão termina com
// o estado no lugar da conversa antiga.
func TestSendAplicaCompactacaoDoAgente(t *testing.T) {
	var (
		mu       sync.Mutex
		chamadas int
		ultimo   []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		chamadas++
		n := chamadas
		ultimo = req.Messages
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emitir := func(p any) {
			b, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		if n == 2 { // segundo pedido, primeira rodada: compacta
			emitir(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "cmp1", "type": "function",
				"function": map[string]any{"name": "ctx_compact", "arguments": `{"estado":"objetivo: bug do parser; causa em parse.go:42"}`},
			}}}}}})
		} else {
			emitir(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "feito"}}}})
		}
		emitir(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := config.Config{BaseURL: srv.URL, Model: "fake", APIKey: "k", Janela: 100_000}
	s := NewSession(cfg)
	cwd := t.TempDir()
	if err := s.Send(context.Background(), nil, cwd, "explore o parser"); err != nil {
		t.Fatalf("pedido 1: %v", err)
	}
	if err := s.Send(context.Background(), nil, cwd, "agora corrija"); err != nil {
		t.Fatalf("pedido 2: %v", err)
	}

	// O 3º request (depois do resultado de ctx_compact) já foi compactado:
	// system, estado, pedido atual, tool_call, tool_result.
	mu.Lock()
	msgs := ultimo
	mu.Unlock()
	if len(msgs) != 5 {
		t.Fatalf("request pós-compactação deveria ter 5 mensagens, veio %d: %v", len(msgs), msgs)
	}
	if c, _ := msgs[1]["content"].(string); !strings.Contains(c, "causa em parse.go:42") {
		t.Fatalf("estado ausente do request: %v", msgs[1])
	}
	for _, m := range msgs {
		if c, _ := m["content"].(string); strings.Contains(c, "explore o parser") {
			t.Fatalf("pedido antigo deveria ter saído do request: %v", msgs)
		}
	}
	h := s.History()
	if !strings.HasPrefix(h[0].Conteudo, prefixoResumo) || h[len(h)-1].Conteudo != "feito" {
		t.Fatalf("histórico final inesperado: %+v", h)
	}
}

// Retomar uma sessão compactada traz o estado, não a conversa antiga.
func TestResumeReaplicaCompactacao(t *testing.T) {
	st := lojaTeste(t)
	s := NewSessionWithStore(config.Config{}, st, "bh-cmp")
	s.registrar("user_message", map[string]any{"role": "user", "content": "pedido antigo"})
	s.registrar("assistant", map[string]any{"role": "assistant", "content": "resposta antiga"})
	s.registrar("user_message", map[string]any{"role": "user", "content": "pedido atual"})
	s.registrar("compactacao", map[string]any{"politica": PoliticaAgente, "estado": "objetivo: Y"})
	s.registrar("assistant", map[string]any{"role": "assistant", "content": "ok"})

	r := NewSessionWithStore(config.Config{}, st, "bh-cmp")
	if err := r.Resume(); err != nil {
		t.Fatal(err)
	}
	h := r.History()
	if len(h) != 3 || !strings.Contains(h[0].Conteudo, "objetivo: Y") || h[1].Conteudo != "pedido atual" || h[2].Conteudo != "ok" {
		t.Fatalf("resume deveria reaplicar a compactação: %+v", h)
	}
}
