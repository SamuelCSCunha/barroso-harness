// Barroso Harness — executável único, multiplataforma, provider-neutral.
// -p executa um turno não interativo com config já salva; sem argumentos
// inicia onboarding/REPL interativo.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ruanbarroso/barroso-harness/internal/cli"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// Versao é sobrescrita no link pelo deployer (-X main.Versao=vX.Y.Z-bh.N).
var Versao = "dev"

func main() {
	var (
		pedido = flag.String("p", "", "pedido de um único turno (modo print)")
		idInit = flag.Bool("init", false, "cria estrutura de dados em ~/.barroso-harness sem sobrescrever arquivos")
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
		fmt.Fprintln(os.Stderr, "uso: bh [-p \"pedido\"] [-init] [-version]")
		os.Exit(2)
	}
	if *pedido == "" {
		os.Exit(cli.RunInteractive(cli.IO{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}))
	}
	if err := Executar(os.Stdout, *pedido); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func Executar(out io.Writer, pedido string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return runtime.Turno(out, cfg, cwd, pedido)
}
