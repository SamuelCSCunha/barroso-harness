// Barroso Harness — executável único, multiplataforma, provider-neutral.
// -p executa um turno não interativo com config já salva; sem argumentos
// inicia onboarding/REPL interativo.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ruanbarroso/barroso-harness/internal/cli"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
	"golang.org/x/term"
)

// Versao é sobrescrita no link pelo deployer (-X main.Versao=vX.Y.Z-bh.N).
var Versao = "dev"

func main() {
	// O REPL precisa da versão para o /update comparar o instalado com o
	// publicado. O -X do deployer alcança ESTA variável, não a do pacote cli,
	// então o main repassa — sem isso o /update sempre se veria como "dev" e
	// trataria qualquer release como novidade.
	cli.Versao = Versao

	var (
		pedido = flag.String("p", "", "pedido de um único turno (modo print)")
		idInit = flag.Bool("init", false, "cria estrutura de dados em ~/.barroso-harness sem sobrescrever arquivos")
		idACP  = flag.Bool("acp", false, "serve o protocolo ACP (JSON-RPC 2.0) por stdio para um editor/cliente")
		idVer  = flag.Bool("version", false, "mostra a versão")
	)
	flag.Parse()

	if *idVer {
		fmt.Println("barroso-harness " + Versao)
		return
	}
	if *idInit {
		if err := config.Init(); err != nil {
			fmt.Fprintln(os.Stderr, "erro:", err)
			os.Exit(1)
		}
		fmt.Println("estrutura criada em", config.Dir())
		return
	}
	if flag.NArg() > 0 && *pedido == "" {
		fmt.Fprintln(os.Stderr, "uso: bh [-p \"pedido\"] [-acp] [-init] [-version]")
		os.Exit(2)
	}
	if *idACP {
		serveACP()
		return
	}
	if *pedido == "" {
		os.Exit(cli.RunInteractive(cli.IO{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}))
	}
	idSessao := ""
	if err := Executar(os.Stdout, *pedido, &idSessao); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
	// O id só vale a pena anunciar quando alguém está olhando: em pipe, a
	// receita do outro lado receberia uma linha extra no meio da saída.
	if idSessao != "" && éTerminal(os.Stderr) {
		fmt.Fprintf(os.Stderr, "sessão: %s\n", idSessao)
	}
}

// éTerminal diz se o writer é um terminal de verdade (x/term). Buffers, pipes
// e redireções não são — e a linha `sessão:` é para humanos.
func éTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Executar roda um turno. Persistir o log é best-effort: se o banco não abrir,
// o turno acontece igual e idSessao volta vazia (sem linha `sessão:`).
// O wiring de subsistemas (MCP, sub-agentes) também é best-effort: falha de um
// servidor não derruba o turno; o aviso vai para stderr.
func Executar(out io.Writer, pedido string, idSessao *string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	loja, err := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if err != nil || loja == nil {
		return execComWiring(out, cfg, cwd, pedido, nil, "")
	}
	id := runtime.NovoSessaoID()
	if idSessao != nil {
		*idSessao = id
	}
	err = execComWiring(out, cfg, cwd, pedido, loja, id)
	if fecho := loja.Fechar(); err == nil {
		err = fecho
	}
	return err
}

// execComWiring monta a sessão (persistente quando há store), conecta os
// subsistemas com dono de processo e roda o turno. É a receita única dos dois
// caminhos de -p: o id difere, o resto é igual.
func execComWiring(out io.Writer, cfg config.Config, cwd, pedido string, loja *store.Store, id string) error {
	var sess *runtime.Session
	if loja != nil {
		sess = runtime.NewSessionWithStore(cfg, loja, id)
	} else {
		sess = runtime.NewSession(cfg)
	}
	// ctx de vida do processo: sub-agentes sobrevivem ao turno que os criou.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conjunto, avisos := wiring.Montar(ctx, cfg, sess, cwd)
	defer func() { _ = conjunto.Fechar() }()
	for _, aviso := range avisos {
		fmt.Fprintln(os.Stderr, "aviso:", aviso)
	}
	return sess.Send(context.Background(), runtime.RendererDe(out), cwd, pedido)
}
