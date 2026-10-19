package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

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

// banner é a faixa de abertura — limpa e estável, no espírito do OpenCode.
// Estado volátil fica na statusbar; a marca não se repete a cada turno.
func banner(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, cianof("╭─ ")+negritof(cianof("Barroso Harness")))
	fmt.Fprintln(out, cianof("╰─ ")+dimf("type /help for commands · plain text goes to the model"))
	fmt.Fprintln(out)
}

// statusbar imprime uma faixa compacta antes de cada prompt. Ela substitui o
// formato cru de Config.Resumo e torna mudanças de /model, /effort e /provider
// imediatamente visíveis sem empurrar um bloco de texto desnecessário.
func statusbar(out io.Writer, cfg *config.Config) {
	provider := cfg.CurrentProvider
	if provider == "" {
		provider = "no-provider"
	}
	modelo := cfg.Model
	if modelo == "" {
		modelo = "select a model"
	}
	linha := provider + " · " + modelo
	if cfg.Effort != "" {
		linha += " · effort " + cfg.Effort
	} else {
		linha += " · effort off"
	}
	if cfg.Janela > 0 {
		linha += fmt.Sprintf(" · %dk", cfg.Janela/1000)
	}
	fmt.Fprintln(out, dimf("╭─ "+linha))
}

// prompto é a linha de entrada: o chevron do OpenCode, modelo em destaque e
// effort discreto. Termina em espaço para o cursor colar nela.
func prompto(out io.Writer, modelo, effort string) {
	mod := modelo
	if mod == "" {
		mod = "barroso"
	}
	fmt.Fprint(out, verdof("❯")+" "+negritof(mod))
	if effort != "" {
		fmt.Fprint(out, " "+amarelof("·"+effort))
	}
	fmt.Fprint(out, " ")
}

// despedida encerra limpo.
func despedida(out io.Writer) {
	if coresLigadas {
		// Garantia: o cursor nunca fica escondido depois que o bh sai, mesmo
		// que o spinner tenha sido interrompido no meio.
		fmt.Fprint(out, mostrarCursor)
	}
	fmt.Fprintln(out, dimf("╰─ bye"))
}

// erroFmt imprime erro em vermelho, uma linha, sem markup cru.
func erroFmt(out io.Writer, prefixo string, err error) {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i] // erro é uma linha; o resto é detalhe de protocolo
	}
	recuo := ""
	if coresLigadas {
		recuo = "  " // alinhado com as linhas do turno na TUI
	}
	fmt.Fprintf(out, "%s%s %s\n", recuo, vermelhof(glifoErro), vermelhof(prefixo+": "+msg))
}

// okFmt imprime confirmação em verde.
func okFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", verdof(glifoOk), msg)
}

// avisoFmt imprime aviso em amarelo.
func avisoFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", amarelof(glifoAviso), msg)
}

// listarModelos mostra o catálogo INTEIRO, em ordem alfabética, numerado (o
// mesmo número que a seleção aceita), com a coluna da janela alinhada e o
// modelo atual marcado com ▸. Sem paginação: o terminal rola, e a lista
// completa é o que permite achar um modelo pelo olho.
func listarModelos(out io.Writer, ls []dealer.CatalogModel, atual string) {
	if len(ls) == 0 {
		fmt.Fprintln(out, amarelof("empty catalog"))
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, cianof(fmt.Sprintf("Models  %d available", len(ls))))
	fmt.Fprintln(out, dimf("────────────────────────────────────────────────────────────"))
	largura := 0
	for _, m := range ls {
		if n := utf8.RuneCountInString(m.ID); n > largura {
			largura = n
		}
	}
	if largura > 44 {
		largura = 44
	}
	for i, m := range ls {
		marca := "  "
		// O preenchimento é calculado sobre o id CRU: aplicar a largura depois
		// de colorir contaria os bytes do escape ANSI e desalinharia a coluna
		// justamente na linha do modelo atual.
		pad := strings.Repeat(" ", max(0, largura-utf8.RuneCountInString(m.ID)))
		nome := m.ID
		if m.ID == atual {
			marca = verdof("▸ ")
			nome = negritof(m.ID)
		}
		extra := ""
		if m.ContextLength > 0 {
			extra = dimf(fmt.Sprintf("  %dk", m.ContextLength/1000))
		}
		fmt.Fprintf(out, "%s%s %s%s%s\n", marca, dimf(fmt.Sprintf("%3d", i+1)), nome, pad, extra)
	}
	dica := "type the number or id"
	if atual != "" {
		dica += " · empty Enter keeps " + atual
	}
	fmt.Fprintln(out, dimf(dica))
}

// tuiTurno é a roupa TUI de um turno: glifo ▶ com nome de tool em magenta,
// duração dim, eco indentado 2, guardrail ◆ e rodapé de tokens.
type tuiTurno struct{ out io.Writer }

func (t tuiTurno) Resposta(texto string) { fmt.Fprintln(t.out, texto) }

func (t tuiTurno) Tool(nome string, dur time.Duration, erro bool) {
	estado, colore := "ok", verdof
	if erro {
		estado, colore = "error", vermelhof
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
	msg := "provider rejected the request size; reactive compaction"
	if proativa {
		msg = "proactive compaction (predicted overflow)"
	}
	fmt.Fprintf(t.out, "%s %s\n", roxof(glifoGuard), dimf(msg))
}

func (t tuiTurno) Tokens(prompt, saida int, dur time.Duration, compactou bool) {
	if prompt <= 0 && saida <= 0 {
		return
	}
	linha := fmt.Sprintf("tokens ↑%d ↓%d · %s", prompt, saida, dur.Round(time.Millisecond))
	if compactou {
		linha += " · compacted"
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

// larguraTerminal devolve as colunas do stdout; 100 quando não dá para saber.
func larguraTerminal() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		return w
	}
	return 100
}
