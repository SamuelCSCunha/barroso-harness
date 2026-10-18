package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"golang.org/x/term"
)

// Camada de apresentação do REPL, no espírito do OpenCode: paleta ANSI-256,
// glifos UTF-8 e status line viva; nenhum framework de TUI (um binário, sem
// terceiros). Nenhum texto conferido por teste leva escape no meio — a cor
// envolve a linha inteira, e coresLigadas só fica ligada quando Out E ErrOut
// são terminais reais; pipes, redireções e buffers de teste (que não são
// *os.File) recebem texto cru.
const (
	reset = "\033[0m"
	neg   = "\033[1m"
)

// Paleta ANSI-256, tonalidade baixa — menos o cian de títulos e o verde/vermelho.
const (
	corDim  = "\033[38;5;244m"
	corCian = "\033[38;5;38m"
	corVerd = "\033[38;5;114m"
	corVerm = "\033[38;5;203m"
	corAmar = "\033[38;5;221m"
	corMag  = "\033[38;5;176m"
	corRox  = "\033[38;5;99m"
)

// Glifos: fontes de terminal modernas (Windows Terminal, conhost do 10/11,
// macOS, Linux) cobrem todos; com cores desligadas continuam sinais visuais.
const (
	glifoOk    = "✓"
	glifoErro  = "✗"
	glifoAviso = "⚠"
	glifoTool  = "▶"
	glifoGuard = "◆"
	glifoAtual = "▸"
)

// catalogoPorPagina é o tamanho da página do /model: 416 modelos não podem
// virar parede de texto.
const catalogoPorPagina = 50

// corCorMag está exposta para o teste afirmar que a paleta foi emitida (o
// próprio cor() devolve texto limpo com coresLigadas=false, então o teste liga
// cores e procura esta constante cruda).
const corCorMag = corMag

// coresLigadas decide se a paleta é emitida. Começa desligada; ligarCores
// determina o valor no início de cada RunInteractive.
var coresLigadas = false

func cor(cod, s string) string {
	if !coresLigadas || cod == "" {
		return s
	}
	return cod + s + reset
}

func dimf(s string) string      { return cor(corDim, s) }
func cianof(s string) string    { return cor(corCian, s) }
func verdof(s string) string    { return cor(corVerd, s) }
func vermelhof(s string) string { return cor(corVerm, s) }
func amarelof(s string) string  { return cor(corAmar, s) }
func magof(s string) string     { return cor(corMag, s) }
func roxof(s string) string     { return cor(corRox, s) }
func negritof(s string) string  { return cor(neg, s) }

func éTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// ligarCores liga a paleta somente com Out e ErrOut em terminal (x/term, já
// dependência da chave oculta), honrando NO_COLOR e TERM=dumb.
func ligarCores(io IO) {
	coresLigadas = éTerminal(io.Out) && éTerminal(io.ErrOut) &&
		os.Getenv("NO_COLOR") == "" &&
		os.Getenv("TERM") != "dumb"
}

// banner é a faixa de abertura: só a marca e a dica. O estado da sessão mora
// na status line, re-renderizada a cada iteração do REPL.
func banner(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, negritof(cianof("Barroso Harness")))
	fmt.Fprintln(out, dimf("digite /help para os comandos · texto livre vai ao modelo"))
	fmt.Fprintln(out)
}

// statusbar imprime a faixa de estado ANTES do prompt: provider, model,
// effort e janela — a cada iteração, para /effort e /model refletirem na hora.
// Compacta a janela em k tokens; a forma longa continua no /help e no Resumo.
func statusbar(out io.Writer, cfg *config.Config) {
	linha := "provider " + cfg.CurrentProvider
	if cfg.Effort != "" {
		linha += " · effort " + cfg.Effort
	}
	linha += " · model " + cfg.Model
	if cfg.Janela > 0 {
		linha += fmt.Sprintf(" · janela %dk", cfg.Janela/1000)
	}
	fmt.Fprintln(out, dimf(linha))
}

// prompto é a linha de entrada: seta, modelo em destaque e effort discreto.
// Termina em espaço para o cursor colar nela; há espaço entre o modelo e o marcador.
func prompto(out io.Writer, modelo, effort string) {
	seta := dimf(">")
	mod := negritof(modelo)
	if effort != "" {
		fmt.Fprintf(out, "%s %s %s ", seta, mod, amarelof("·"+effort))
		return
	}
	fmt.Fprintf(out, "%s %s ", seta, mod)
}

// despedida encerra limpo.
func despedida(out io.Writer) { fmt.Fprintln(out, dimf("ate")) }

// erroFmt imprime erro em vermelho, uma linha, sem markup cru.
func erroFmt(out io.Writer, prefixo string, err error) {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i] // erro é uma linha; o resto é detalhe de protocolo
	}
	fmt.Fprintf(out, "%s %s\n", vermelhof(glifoErro), vermelhof(prefixo+": "+msg))
}

// okFmt imprime confirmação em verde.
func okFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", verdof(glifoOk), msg)
}

// avisoFmt imprime aviso em amarelo.
func avisoFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", amarelof(glifoAviso), msg)
}

// listarModelos mostra UMA página do catálogo, em ordem alfabética, numerado
// com índice GLOBAL (o mesmo número que /model <n> aceita), coluna da janela
// alinhada e o modelo atual marcado com ▸.
func listarModelos(out io.Writer, ls []dealer.CatalogModel, atual string, pagina, porPagina int) {
	if len(ls) == 0 {
		fmt.Fprintln(out, amarelof("catalogo vazio"))
		return
	}
	totalPaginas := (len(ls) + porPagina - 1) / porPagina
	if pagina < 1 {
		pagina = 1
	}
	if pagina > totalPaginas {
		pagina = totalPaginas
	}
	inicio := (pagina - 1) * porPagina
	fim := inicio + porPagina
	if fim > len(ls) {
		fim = len(ls)
	}
	fmt.Fprintln(out, cianof("catálogo")+dimf(fmt.Sprintf(" %d modelos · página %d/%d · pg<N> navega", len(ls), pagina, totalPaginas)))
	largura := 0
	for _, m := range ls[inicio:fim] {
		if len(m.ID) > largura {
			largura = len(m.ID)
		}
	}
	if largura > 44 {
		largura = 44
	}
	for i, m := range ls[inicio:fim] {
		num := inicio + i + 1
		marca := "  "
		nome := m.ID
		if m.ID == atual {
			marca = verdof("▸ ")
			nome = negritof(m.ID)
		}
		extra := ""
		if m.ContextLength > 0 {
			extra = dimf(fmt.Sprintf("  %dk", m.ContextLength/1000))
		}
		fmt.Fprintf(out, "%s%s %-*s%s\n", marca, dimf(fmt.Sprintf("%3d", num)), largura, nome, extra)
	}
	fmt.Fprintln(out, dimf("digite o numero ou o id · pg<N> troca de página"))
}

// tuiTurno é a roupa TUI de um turno: glifo ▶ com nome de tool em magenta,
// duração dim, eco indentado 2, guardrail ◆ e rodapé de tokens.
type tuiTurno struct{ out io.Writer }

func (t tuiTurno) Resposta(texto string) { fmt.Fprintln(t.out, texto) }

func (t tuiTurno) Tool(nome string, dur time.Duration, erro bool) {
	estado, colore := "ok", verdof
	if erro {
		estado, colore = "erro", vermelhof
	}
	fmt.Fprintf(t.out, "%s %s %s %s\n",
		cianof(glifoTool), magof(nome),
		dimf(dur.Round(time.Millisecond).String()), colore(estado))
}

func (t tuiTurno) Eco(saida string) {
	if saida == "" {
		return
	}
	for _, linha := range strings.Split(saida, "\n") {
		fmt.Fprintln(t.out, dimf("  "+linha))
	}
}

func (t tuiTurno) Guardrail(proativa bool) {
	msg := "provider recusou por contexto; compactação reativa"
	if proativa {
		msg = "compactação proativa por previsão de estouro"
	}
	fmt.Fprintf(t.out, "%s %s\n", roxof(glifoGuard), dimf(msg))
}

func (t tuiTurno) Tokens(prompt, saida int, dur time.Duration, compactou bool) {
	if prompt <= 0 && saida <= 0 {
		return
	}
	linha := fmt.Sprintf("tokens ↑%d ↓%d · %s", prompt, saida, dur.Round(time.Millisecond))
	if compactou {
		linha += " · compactado"
	}
	fmt.Fprintln(t.out, dimf(linha))
}

// rendererTurno escolhe a facciata: glifos+paleta quando a sessão ligou cores
// (terminal real), texto cru caso contrário — inclusive nos buffers dos testes,
// que então veem a mesma linha literal que runtime_test crava no modo puro.
func rendererTurno(out io.Writer) runtime.Renderer {
	if coresLigadas {
		return tuiTurno{out: out}
	}
	return runtime.RendererDe(out)
}
