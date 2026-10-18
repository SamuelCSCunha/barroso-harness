// serveACP liga o modo bh -acp: o processo vira um servidor ACP (JSON-RPC 2.0
// newline-delimited sobre stdio) para um editor/cliente dirigir os turnos.
// stdout é fio de protocolo — nada de banner/log nele; diagnóstico vai para
// stderr. Persistência e subsistemas (MCP, sub-agentes) seguem best-effort:
// falha de um servidor vira aviso em stderr, não derrubada do protocolo.
package main

import (
	"os"
	"path/filepath"

	"github.com/ruanbarroso/barroso-harness/internal/acp"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
)

func serveACP() {
	cfg, err := config.Load()
	if err != nil {
		// Falha de config sai no stderr (log do protocolo), nunca no stdout —
		// o fio ainda é o(os.Stdout) do Sirve.
		os.Stderr.WriteString("erro: " + err.Error() + "\n")
		os.Exit(1)
	}
	cwd, _ := os.Getwd()

	// Log do ACP é o MESMO banco do REPL e do -p (mesma pasta, mesmo arquivo):
	// uma sessão aberta num cliente editável é retomável no terminal depois.
	// Best-effort — sem banco o protocolo segue, só sem histórico.
	loja, errBanco := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if errBanco != nil {
		os.Stderr.WriteString("aviso: sessão sem persistência: " + errBanco.Error() + "\n")
		loja = nil
	}
	defer func() {
		if loja != nil {
			_ = loja.Fechar()
		}
	}()

	var abertos []*wiring.Conjunto
	fabrica := func(c config.Config) (acp.Turno, string) {
		return novaSessaoACP(c, loja, cwd, &abertos)
	}
	defer func() {
		for _, conj := range abertos {
			_ = conj.Fechar()
		}
	}()
	ag := acp.NovoComFabrica(cfg, fabrica).ComLog(func(linha string) {
		os.Stderr.WriteString(linha + "\n")
	})
	// session/load só é anunciado quando há banco para ler: sem loja não há o
	// que reabrir, e o agente responde loadSession=false.
	if loja != nil {
		ag.Carregar = func(c config.Config, id string) (acp.Turno, error) {
			return carregarSessaoACP(c, loja, cwd, id, &abertos)
		}
	}
	if err := ag.Handle(nil, os.Stdin, os.Stdout); err != nil {
		os.Stderr.WriteString("erro: " + err.Error() + "\n")
		os.Exit(1)
	}
	ag.Esperar()
}

// novaSessaoACP monta uma runtime.Session por session/new: persistente quando
// há banco, com o wiring de subsistemas (MCP, sub-agentes) ligado — mesma
// receita do REPL e do -p, para o cliente ACP não ver um catálogo pobre.
//
// O id devolvido NUNCA é vazio, e isso não é detalhe: o agente ACP registra a
// sessão no mapa sob essa chave e responde session/new com ela. Id vazio
// registrava a sessão na chave "" e o cliente recebia sessionId vazio —
// incapaz de endereçar session/prompt, que rejeitava com "sessão desconhecida".
//
// O ctx do wiring é o de vida do processo: sub-agentes nascidos de um turno
// ACP precisam sobreviver a ele.
func novaSessaoACP(cfg config.Config, loja *store.Store, cwd string, abertos *[]*wiring.Conjunto) (acp.Turno, string) {
	id := runtime.NovoSessaoID()
	var sess *runtime.Session
	if loja != nil {
		sess = runtime.NewSessionWithStore(cfg, loja, id)
	} else {
		sess = runtime.NewSession(cfg)
	}
	conj, avisos := wiring.Montar(nil, cfg, sess, cwd)
	for _, aviso := range avisos {
		os.Stderr.WriteString("aviso: " + aviso + "\n")
	}
	if abertos != nil {
		*abertos = append(*abertos, conj)
	}
	return sess, id
}

// carregarSessaoACP reabre uma sessão do sessions.db para session/load: a
// mesma sessão que o REPL lista em /resume. Id que o banco não conhece é erro
// — criar uma sessão vazia com o id pedido faria o cliente achar que carregou
// uma conversa que não existe.
func carregarSessaoACP(cfg config.Config, loja *store.Store, cwd, id string, abertos *[]*wiring.Conjunto) (acp.Turno, error) {
	if loja == nil {
		return nil, store.ErrSessaoInexistente
	}
	if _, err := loja.GetSession(id); err != nil {
		return nil, err
	}
	sess := runtime.NewSessionWithStore(cfg, loja, id)
	if err := sess.Resume(); err != nil {
		return nil, err
	}
	conj, avisos := wiring.Montar(nil, cfg, sess, cwd)
	for _, aviso := range avisos {
		os.Stderr.WriteString("aviso: " + aviso + "\n")
	}
	if abertos != nil {
		*abertos = append(*abertos, conj)
	}
	return sess, nil
}
