package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// gerenteComTarefa monta um gerente real com UMA tarefa que falha de imediato
// (config do filho incompleta) — sem rede, sem provider, e determinístico.
func gerenteComTarefa(t *testing.T) (*subagent.Gerente, string) {
	t.Helper()
	g := subagent.Novo(subagent.Config{})
	g.Vincular(context.Background())
	g.ConfigPai(config.Config{})
	id, err := g.Spawn(context.Background(), "investigar o bug do parser de datas")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return g, id
}

func TestTasksDesligadoExplicaComoLigar(t *testing.T) {
	semCores(t)
	var out bytes.Buffer
	cmdTasks(&out, nil, "")
	if !strings.Contains(out.String(), "subtasks") {
		t.Fatalf("deveria dizer como ligar: %q", out.String())
	}
}

func TestTasksVazioDiz(t *testing.T) {
	semCores(t)
	var out bytes.Buffer
	cmdTasks(&out, subagent.Novo(subagent.Config{}), "")
	if !strings.Contains(out.String(), "nenhuma tarefa") {
		t.Fatalf("saída: %q", out.String())
	}
}

func TestTasksListaEstadoEPedido(t *testing.T) {
	semCores(t)
	g, id := gerenteComTarefa(t)
	var out bytes.Buffer
	cmdTasks(&out, g, "")
	s := out.String()
	for _, quer := range []string{id, subagent.Failed, "investigar o bug", "1 failed"} {
		if !strings.Contains(s, quer) {
			t.Errorf("listagem sem %q:\n%s", quer, s)
		}
	}
}

func TestTasksDetalhaComErro(t *testing.T) {
	semCores(t)
	g, id := gerenteComTarefa(t)
	var out bytes.Buffer
	cmdTasks(&out, g, id)
	s := out.String()
	if !strings.Contains(s, "pedido: investigar o bug do parser de datas") {
		t.Errorf("detalhe sem o pedido:\n%s", s)
	}
	if !strings.Contains(s, "erro: config incompleta") {
		t.Errorf("detalhe sem o erro:\n%s", s)
	}
}

func TestTasksDetalheIdInexistente(t *testing.T) {
	semCores(t)
	g, _ := gerenteComTarefa(t)
	var out bytes.Buffer
	cmdTasks(&out, g, "t99")
	if !strings.Contains(out.String(), "não existe") {
		t.Fatalf("saída: %q", out.String())
	}
}

// Cancelar tarefa já terminada não pode fingir sucesso.
func TestTasksCancelTerminadaAvisa(t *testing.T) {
	semCores(t)
	g, id := gerenteComTarefa(t)
	var out bytes.Buffer
	cmdTasks(&out, g, "cancel "+id)
	if !strings.Contains(out.String(), "nada a cancelar") {
		t.Fatalf("saída: %q", out.String())
	}
	out.Reset()
	cmdTasks(&out, g, "cancel")
	if !strings.Contains(out.String(), "uso: /tasks cancel") {
		t.Fatalf("sem id deveria mostrar uso: %q", out.String())
	}
}

func TestReplHelpListaTasks(t *testing.T) {
	semCores(t)
	io, out, _ := ponteIO(t, "")
	ajuda(io)
	if !strings.Contains(out.String(), "/tasks") {
		t.Fatalf("/help sem /tasks:\n%s", out.String())
	}
}
