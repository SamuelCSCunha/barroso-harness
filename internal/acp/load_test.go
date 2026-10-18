package acp

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// turnoComHistorico é um Turno que também expõe History — o que session/load
// usa para reproduzir a conversa ao cliente.
type turnoComHistorico struct {
	*fakeTurno
	h []dealer.Mensagem
}

func (t *turnoComHistorico) History() []dealer.Mensagem { return t.h }

var _ Turno = (*turnoComHistorico)(nil)
var _ runtime.Renderer = (*acpRenderer)(nil)

// arranjarComCarga monta o agente com um Carregador, ANTES de ligar o Handle
// (o campo é lido pelo laço; escrevê-lo depois seria corrida).
func arranjarComCarga(t *testing.T, carregar Carregador) *arranjo {
	t.Helper()
	a := Novo(config.Config{})
	a.Fabrica = fabrica(newFakeTurno())
	a.Carregar = carregar
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Handle(ctx, pr, out) }()
	return &arranjo{a: a, out: out, entrada: pw, cancelar: cancel}
}

func historicoDeTeste() []dealer.Mensagem {
	return []dealer.Mensagem{
		{Role: "user", Conteudo: "liste os arquivos"},
		{Role: "tool_call", ToolID: "c1", ToolName: "list_dir", ToolArgs: `{}`},
		{Role: "tool_result", ToolID: "c1", ToolName: "list_dir", Conteudo: "a.go\nb.go"},
		{Role: "assistant", Conteudo: "há dois arquivos"},
	}
}

// Sem Carregador o agente não pode prometer o que não faz.
func TestInitializeSemCarregadorNaoAnunciaLoad(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, 1, nil)
	ls := espereLinhas(t, ar.out, 1)
	caps := ls[0].Result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != false {
		t.Fatalf("sem Carregador loadSession deveria ser false: %v", caps)
	}
}

func TestInitializeComCarregadorAnunciaLoad(t *testing.T) {
	ar := arranjarComCarga(t, func(config.Config, string) (Turno, error) { return newFakeTurno(), nil })
	ar.envia(t, MetInitialize, 1, nil)
	ls := espereLinhas(t, ar.out, 1)
	caps := ls[0].Result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("com Carregador loadSession deveria ser true: %v", caps)
	}
}

func TestSessionLoadSemCarregadorRecusa(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetLoad, 1, map[string]any{"sessionId": "bh-x"})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodMetodo {
		t.Fatalf("esperava erro de método, veio %+v", ls[0])
	}
}

// O caminho feliz: reproduz o histórico na ordem e só então responde {}.
func TestSessionLoadReproduzHistoricoEResponde(t *testing.T) {
	var pedido string
	ar := arranjarComCarga(t, func(_ config.Config, id string) (Turno, error) {
		pedido = id
		return &turnoComHistorico{fakeTurno: newFakeTurno(), h: historicoDeTeste()}, nil
	})
	ar.envia(t, MetLoad, 7, map[string]any{"sessionId": "bh-antiga", "cwd": `C:\tmp`})
	// user + tool_call + tool_call_update + agent_message + resposta
	ls := espereLinhas(t, ar.out, 5)

	if pedido != "bh-antiga" {
		t.Fatalf("Carregador recebeu id %q", pedido)
	}
	tipos := []string{}
	for _, l := range ls[:4] {
		if l.Method != "session/update" || l.Params.SessionID != "bh-antiga" {
			t.Fatalf("esperava session/update da sessão carregada, veio %+v", l)
		}
		tipos = append(tipos, l.Params.Update.SessionUpdate)
	}
	quer := []string{"user_message_chunk", "tool_call", "tool_call_update", "agent_message_chunk"}
	for i := range quer {
		if tipos[i] != quer[i] {
			t.Fatalf("ordem da reprodução = %v, queria %v", tipos, quer)
		}
	}
	if ls[0].Params.Update.Content.Blocos[0].Text != "liste os arquivos" {
		t.Errorf("texto do usuário errado: %+v", ls[0].Params.Update.Content)
	}
	if ls[1].Params.Update.Title != "list_dir" {
		t.Errorf("tool_call sem o nome da tool: %+v", ls[1].Params.Update)
	}
	if ls[2].Params.Update.ToolCallID != ls[1].Params.Update.ToolCallID {
		t.Errorf("update não referencia o tool_call: %q vs %q", ls[2].Params.Update.ToolCallID, ls[1].Params.Update.ToolCallID)
	}
	if ls[2].Params.Update.Status != "completed" {
		t.Errorf("status do resultado = %q", ls[2].Params.Update.Status)
	}
	resp := ls[4]
	if resp.Error != nil || string(resp.ID) != "7" {
		t.Fatalf("resposta final inesperada: %+v", resp)
	}
}

// Depois do load a sessão é endereçável por session/prompt.
func TestSessionLoadDeixaSessaoUtilizavel(t *testing.T) {
	f := &fakeTurno{iniciou: make(chan struct{}), texto: "continuando"}
	ar := arranjarComCarga(t, func(config.Config, string) (Turno, error) {
		return &turnoComHistorico{fakeTurno: f}, nil
	})
	ar.envia(t, MetLoad, 1, map[string]any{"sessionId": "bh-antiga"})
	espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 2, map[string]any{
		"sessionId": "bh-antiga",
		"prompt":    []map[string]string{{"type": "text", "text": "e agora?"}},
	})
	ls := espereLinhas(t, ar.out, 3)
	if ls[1].Params.Update.SessionUpdate != "agent_message_chunk" || ls[2].Error != nil {
		t.Fatalf("prompt na sessão carregada falhou: %+v", ls)
	}
	if f.textoPedido() != "e agora?" {
		t.Errorf("turno recebeu %q", f.textoPedido())
	}
}

func TestSessionLoadIdInexistente(t *testing.T) {
	ar := arranjarComCarga(t, func(config.Config, string) (Turno, error) {
		return nil, errors.New("sessão inexistente")
	})
	ar.envia(t, MetLoad, 1, map[string]any{"sessionId": "bh-nao-ha"})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("esperava -32602, veio %+v", ls[0])
	}
}

func TestSessionLoadSemSessionId(t *testing.T) {
	ar := arranjarComCarga(t, func(config.Config, string) (Turno, error) { return newFakeTurno(), nil })
	ar.envia(t, MetLoad, 1, map[string]any{})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("esperava -32602, veio %+v", ls[0])
	}
}
