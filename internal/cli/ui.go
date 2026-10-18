package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// Camada fina de apresentação do REPL. Cores ANSI (o terminal moderno do
// Windows 10/11, macOS e Linux entende sem biblioteca extra); quem não entende
// ignora os escapes e vê o texto limpo. Nenhum texto verificado pelos testes
// é quebrado por escape no meio — a cor vai na linha inteira ou em rótulos,
// nunca dentro de uma substring confirmada.

const (
	reset = "\033[0m"
	neg   = "\033[1m"
	fraco = "\033[2m"
	verm  = "\033[31m"
	verd  = "\033[32m"
	amar  = "\033[33m"
	cian  = "\033[36m"
)

// coresLigadas fica false em teste/saída não-TTY, para a saída ficar limpa.
var coresLigadas = true

func cor(cod, s string) string {
	if !coresLigadas || cod == "" {
		return s
	}
	return cod + s + reset
}

func fracof(s string) string    { return cor(fraco, s) }
func verdof(s string) string    { return cor(verd, s) }
func amarelof(s string) string  { return cor(amar, s) }
func vermelhof(s string) string { return cor(verm, s) }
func cianof(s string) string    { return cor(cian, s) }
func negritof(s string) string  { return cor(neg, s) }

// banner é o cabeçalho de boas-vindas — uma linha, sem poluição.
func banner(out io.Writer, resumoLinha string) {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s  %s\n", negritof(cianof("Barroso Harness")), fracof(resumoLinha))
	fmt.Fprintf(out, "%s\n", fracof("digite /help para os comandos · texto livre vai ao modelo"))
	fmt.Fprintln(out)
}

// prompto é a linha de entrada: curta, com o modelo em destaque e o effort
// como marcador discreto. Termina em espaço para o cursor colar nela.
func prompto(out io.Writer, modelo, effort string) {
	seta := verdof(">")
	mod := negritof(modelo)
	if effort != "" {
		fmt.Fprintf(out, "%s %s %s ", seta, mod, amarelof("·"+effort))
		return
	}
	fmt.Fprintf(out, "%s %s ", seta, mod)
}

// despedida encerra limpo.
func despedida(out io.Writer) { fmt.Fprintln(out, fracof("ate")) }

// erroFmt imprime erro em vermelho, uma linha, sem markup cru.
func erroFmt(out io.Writer, prefixo string, err error) {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i] // erro é uma linha; o resto é detalhe de protocolo
	}
	fmt.Fprintf(out, "%s %s\n", vermelhof("x"), vermelhof(prefixo+": "+msg))
}

// okFmt imprime confirmação em verde.
func okFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", verdof("ok"), msg)
}

// avisoFmt imprime aviso em amarelo.
func avisoFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", amarelof("!"), msg)
}

// listarModelos mostra o catálogo em colunas alinhadas, numerado, com a
// janela de contexto quando o provider a publica. `atual` é marcado.
func listarModelos(out io.Writer, ls []dealer.CatalogModel, atual string) {
	if len(ls) == 0 {
		fmt.Fprintln(out, amarelof("catalogo vazio"))
		return
	}
	largura := 0
	for _, m := range ls {
		if len(m.ID) > largura {
			largura = len(m.ID)
		}
	}
	if largura > 44 {
		largura = 44
	}
	for i, m := range ls {
		marca := "  "
		nome := m.ID
		if m.ID == atual {
			marca = verdof("> ")
			nome = negritof(m.ID)
		}
		extra := ""
		if m.ContextLength > 0 {
			extra = fracof(fmt.Sprintf("  %dk tokens", m.ContextLength/1000))
		}
		fmt.Fprintf(out, "%s%s %-*s%s\n", marca, fracof(fmt.Sprintf("%3d", i+1)), largura, nome, extra)
	}
	fmt.Fprintln(out, fracof("digite o numero ou o id; enter cancela"))
}
