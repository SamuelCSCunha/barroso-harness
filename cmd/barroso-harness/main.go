// Barroso Harness — executável único, multiplataforma, provider-neutral.
// Slice 0: um turno completo por invocação (-p "pedido"), com streaming de
// texto, tools locais em modo YOLO, orçamento de janela como guardrail e
// compactação proativa/reativa.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// Versao é sobrescrita no link pelo deployer (-X main.Versao=vX.Y.Z-bh.N).
var Versao = "dev"

func main() {
	var (
		pedido  = flag.String("p", "", "pedido de um único turno (modo print); modo interativo vem no slice 1")
		idInit  = flag.Bool("init", false, "cria config.json e BARROSO.md de exemplo em ~/.barroso-harness")
		idVer   = flag.Bool("version", false, "mostra a versão")
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
		fmt.Println("arquivos de exemplo criados em", config.Dir())
		return
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
