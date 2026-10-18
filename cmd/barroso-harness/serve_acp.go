// serveACP liga o modo bh -acp: o processo vira um servidor ACP (JSON-RPC 2.0
// newline-delimited sobre stdio) para um editor/cliente dirigir os turnos.
// stdout é fio de protocolo — nada de banner/log nele; diagnóstico vai para
// stderr. Persistência e subsistemas (MCP, sub-agentes) seguem best-effort:
// falha de um servidor vira aviso em stderr, não derrubada do protocolo.
package main

import (
	"os"

	"github.com/ruanbarroso/barroso-harness/internal/acp"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
)

func serveACP() {
	cfg, err := config.Load()
	if err != nil {
		// Falha de config sai no stderr (log do protocolo), nunca no stdout —
		// o fio ainda é o(stdout) do Sirve.
		os.Stderr.WriteString("erro: " + err.Error() + "\n")
		os.Exit(1)
	}
	cwd, _ := os.Getwd()
	// Fábrica de turnos real: cada session/new ganha uma runtime.Session com o
	// wiring montado (MCP conectado, gerente de sub-agentes). O conjunto é
	// guardado por sessão e fechado quando o cliente encerra o processo.
	var abertos []*wiring.Conjunto
	fabrica := func(c config.Config) (acp.Turno, string) {
		sess := runtime.NewSession(c)
		conj, avisos := wiring.Montar(nil, c, sess, cwd)
		for _, aviso := range avisos {
			os.Stderr.WriteString("aviso: " + aviso + "\n")
		}
		abertos = append(abertos, conj)
		return sess, ""
	}
	defer func() {
		for _, conj := range abertos {
			_ = conj.Fechar()
		}
	}()
	ag := acp.NovoComFabrica(cfg, fabrica).ComLog(func(linha string) {
		os.Stderr.WriteString(linha + "\n")
	})
	if err := ag.Handle(nil, os.Stdin, os.Stdout); err != nil {
		os.Stderr.WriteString("erro: " + err.Error() + "\n")
		os.Exit(1)
	}
	ag.Esperar()
}
