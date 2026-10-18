// Testes da fábrica de sessões do modo ACP.
//
// O bug que estes testes cravam: a fábrica devolvia "" como id da sessão. O
// agente ACP registrava a sessão no mapa sob essa chave e respondia
// session/new com sessionId vazio — e session/prompt, que exige o id,
// rejeitava tudo com "sessão desconhecida". Nenhum teste pegou porque o
// defeito estava no chamador (cmd/), não no pacote internal/acp, cuja própria
// fábrica de produção gera id correto.
package main

import (
	"path/filepath"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/acp"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
)

// novaFabricaACPDeTeste devolve (fabrica, loja) com o banco num diretório
// temporário. A loja fica aberta de propósito: fechar antes do fim do teste
// perderia o log.
func novaFabricaACPDeTeste(t *testing.T) (func(config.Config) (acp.Turno, string), *store.Store) {
	t.Helper()
	loja, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = loja.Fechar() })
	var abertos []*wiring.Conjunto
	return func(c config.Config) (acp.Turno, string) {
		return novaSessaoACP(c, loja, t.TempDir(), &abertos)
	}, loja
}

// A regressão que importa: o id tem de sair preenchido.
func TestNovaSessaoACPDevolveIdNaoVazio(t *testing.T) {
	fabrica, _ := novaFabricaACPDeTeste(t)
	turno, id := fabrica(config.Config{})
	if turno == nil {
		t.Fatal("turno não pode ser nil")
	}
	if id == "" {
		t.Fatal("id da sessão ACP veio vazio: session/new responderia sessionId vazio e session/prompt rejeitaria com \"sessão desconhecida\"")
	}
}

// Duas sessões não podem dividir id: a segunda sobrescreveria a primeira no
// mapa do agente, e o cliente veria o histórico da outra conversa.
func TestNovaSessaoACPDevolveIdsDistintos(t *testing.T) {
	fabrica, _ := novaFabricaACPDeTeste(t)
	_, a := fabrica(config.Config{})
	_, b := fabrica(config.Config{})
	if a == b {
		t.Fatalf("duas sessões ACP dividiram o id %q", a)
	}
}

// A configuração passada tem de chegar à sessão: o agente ACP captura a config
// em session/new e a fábrica é quem a entrega ao runtime.
func TestNovaSessaoACPPropagaConfig(t *testing.T) {
	fabrica, _ := novaFabricaACPDeTeste(t)
	cfg := config.Config{Model: "modelo-de-teste", Effort: "high"}
	turno, _ := fabrica(cfg)
	sess, ok := turno.(*runtime.Session)
	if !ok {
		t.Fatalf("turno deveria ser *runtime.Session, veio %T", turno)
	}
	if got := sess.Config().Model; got != "modelo-de-teste" {
		t.Fatalf("modelo na sessão = %q, queria \"modelo-de-teste\"", got)
	}
}

// Com banco aberto, a sessão é persistente: é isso que permite retomar no
// terminal uma conversa começada num cliente ACP.
func TestNovaSessaoACPPersisteQuandoHaLoja(t *testing.T) {
	fabrica, loja := novaFabricaACPDeTeste(t)
	turno, id := fabrica(config.Config{})
	sess := turno.(*runtime.Session)
	if !sess.Persistente() {
		t.Fatal("sessão ACP deveria ser persistente com loja disponível")
	}
	if sess.ID() != id {
		t.Fatalf("id da sessão = %q, id devolvido pela fábrica = %q", sess.ID(), id)
	}
	// A sessão tem de estar indexada no banco, não só amarrada em memória.
	if err := loja.NewSession(id); err != nil {
		t.Fatalf("NewSession(%q) após fabrica: %v", id, err)
	}
}

// Sem banco, a fábrica ainda devolve id válido e o turno funciona — queda de
// persistência é degradação, não falha.
func TestNovaSessaoACPFuncionaSemLoja(t *testing.T) {
	fabrica, _ := novaFabricaACPDeTeste(t)
	var abertos []*wiring.Conjunto
	turno, id := novaSessaoACP(config.Config{}, nil, t.TempDir(), &abertos)
	if turno == nil {
		t.Fatal("turno não pode ser nil sem loja")
	}
	if id == "" {
		t.Fatal("id vazio sem loja: a ausência do banco não pode derrubar o id")
	}
	_ = fabrica
	if turno.(*runtime.Session).Persistente() {
		t.Fatal("sem loja a sessão não deveria reportar persistência")
	}
}
