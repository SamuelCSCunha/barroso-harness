// Visual do turno no estilo do OpenCode: spinner enquanto o modelo pensa,
// cada tool numa linha "✓ nome  argumento-resumido" com a saída recuada e
// apagada, a resposta com markdown básico (títulos, negrito, código, listas)
// e um rodapé "Build · modelo · duração · tok/s · contexto %".
//
// Tudo isto só roda com cores ligadas (terminal real). Pipe, -p e testes
// continuam no RendererPuro, sem um escape sequer.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// Paleta extra do redesenho (a base fica em ui.go).
const (
	corAzul  = "\033[38;5;75m"
	corLaran = "\033[38;5;215m"
	corCinza = "\033[38;5;240m"
	corTexto = "\033[38;5;252m"
	fundoCod = "\033[48;5;236m"
	italico  = "\033[3m"
)

func azulf(s string) string    { return cor(corAzul, s) }
func laranjaf(s string) string { return cor(corLaran, s) }
func cinzaf(s string) string   { return cor(corCinza, s) }

// barra é a borda lateral das caixas (pedido do usuário, entrada).
const barra = "▌"

// ── turno ─────────────────────────────────────────────────────────────────

// turnoTUI implementa runtime.Renderer e runtime.RendererDetalhado.
type turnoTUI struct {
	out    io.Writer
	cfg    *config.Config
	inicio time.Time

	mu       sync.Mutex
	spin     chan struct{} // fecha para parar o spinner
	spinFim  chan struct{}
	contexto int // prompt tokens do último request
	pendente string
	argsPend string
}

func novoTurnoTUI(out io.Writer, cfg *config.Config) *turnoTUI {
	return &turnoTUI{out: out, cfg: cfg, inicio: time.Now()}
}

var quadrosSpin = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Pensando anima "⠋ Thinking" na linha atual até a próxima saída do turno.
func (t *turnoTUI) Pensando() {
	t.pararSpin()
	t.mu.Lock()
	t.spin = make(chan struct{})
	t.spinFim = make(chan struct{})
	parar, fim := t.spin, t.spinFim
	t.mu.Unlock()
	go func() {
		defer close(fim)
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		i := 0
		desde := time.Now()
		for {
			fmt.Fprintf(t.out, "\r  %s %s %s\033[K", azulf(quadrosSpin[i%len(quadrosSpin)]),
				cor(italico+corTexto, "Pensando"), cinzaf(fmt.Sprintf("%.0fs", time.Since(desde).Seconds())))
			i++
			select {
			case <-parar:
				fmt.Fprint(t.out, "\r\033[K")
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *turnoTUI) pararSpin() {
	t.mu.Lock()
	parar, fim := t.spin, t.spinFim
	t.spin, t.spinFim = nil, nil
	t.mu.Unlock()
	if parar != nil {
		close(parar)
		<-fim
	}
}

func (t *turnoTUI) Contexto(promptTokens int) {
	t.mu.Lock()
	t.contexto = promptTokens
	t.mu.Unlock()
}

// ToolInicio guarda o argumento resumido e mostra a tool rodando.
func (t *turnoTUI) ToolInicio(nome, args string) {
	t.pararSpin()
	resumo := resumoArgs(args)
	t.mu.Lock()
	t.pendente, t.argsPend = nome, resumo
	t.mu.Unlock()
	fmt.Fprintf(t.out, "\r  %s %s %s\033[K", amarelof("●"), cor(corTexto, nome), cinzaf(resumo))
}

func (t *turnoTUI) Tool(nome string, dur time.Duration, erro bool) {
	t.pararSpin()
	t.mu.Lock()
	resumo := t.argsPend
	t.pendente, t.argsPend = "", ""
	t.mu.Unlock()
	marca := verdof("✓")
	if erro {
		marca = vermelhof("✗")
	}
	fmt.Fprintf(t.out, "\r  %s %s %s %s\033[K\n", marca, cor(corTexto, nome), cinzaf(resumo), cinzaf(fmtDur(dur)))
}

// Eco: saída da tool recuada sob o "›", no máximo 6 linhas.
func (t *turnoTUI) Eco(saida string) {
	saida = strings.TrimRight(saida, "\n")
	if saida == "" {
		return
	}
	linhas := strings.Split(saida, "\n")
	const max = 6
	extra := 0
	if len(linhas) > max {
		extra = len(linhas) - max
		linhas = linhas[:max]
	}
	for i, l := range linhas {
		pre := "    "
		if i == 0 {
			pre = "  " + cinzaf("›") + " "
		}
		fmt.Fprintln(t.out, pre+cinzaf(cortarLargura(l, larguraTerminal()-6)))
	}
	if extra > 0 {
		fmt.Fprintln(t.out, "    "+cinzaf(fmt.Sprintf("… +%d linhas", extra)))
	}
}

func (t *turnoTUI) Guardrail(proativa bool) {
	t.pararSpin()
	msg := "contexto compactado pelo guardrail (o provider recusou por tamanho)"
	if proativa {
		msg = "contexto compactado"
	}
	fmt.Fprintf(t.out, "  %s %s\n", roxof("◆"), cinzaf(msg))
}

func (t *turnoTUI) Resposta(texto string) {
	t.pararSpin()
	fmt.Fprintln(t.out)
	fmt.Fprintln(t.out, renderMarkdown(texto, larguraTerminal()))
}

// Tokens é o rodapé do OpenCode: "Build · modelo · 12.3s · 25.3 tok/s · 9.8K (5%)".
func (t *turnoTUI) Tokens(prompt, saida int, dur time.Duration, compactou bool) {
	t.pararSpin()
	partes := []string{azulf("Build"), cor(corTexto, t.cfg.Model), fmtDur(dur)}
	if saida > 0 && dur > 0 {
		partes = append(partes, fmt.Sprintf("%.1f tok/s", float64(saida)/dur.Seconds()))
	}
	t.mu.Lock()
	ctx := t.contexto
	t.mu.Unlock()
	if ctx > 0 {
		partes = append(partes, fmtContexto(ctx, t.cfg.Janela))
	}
	if compactou {
		partes = append(partes, roxof("compactado"))
	}
	fmt.Fprintln(t.out)
	fmt.Fprintln(t.out, "  "+strings.Join(partes, cinzaf(" · ")))
}

// ── caixas: cabeçalho, entrada, pedido ────────────────────────────────────

// cabecalhoTUI é a abertura: a marca em bloco e uma linha de dica.
func cabecalhoTUI(out io.Writer, versao string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+cinzaf("▄▄ ▄▄")+"  "+negritof(cor(corTexto, "barroso"))+negritof(azulf(" harness")))
	fmt.Fprintln(out, "  "+cinzaf("▀▀ ▀▀")+"  "+cinzaf("v"+strings.TrimPrefix(versao, "v")+" · /help comandos · ctrl+c interrompe"))
	fmt.Fprintln(out)
}

// statusEntrada é a linha sob a caixa de entrada: modelo, provider, effort,
// contexto e MCPs — no lugar do "Build auto · GLM · max" do OpenCode.
func statusEntrada(out io.Writer, cfg *config.Config, sessao string, mcps int, contexto int) {
	modelo := cfg.Model
	if modelo == "" {
		modelo = "sem modelo"
	}
	partes := []string{azulf("Build"), negritof(cor(corTexto, modelo)), cinzaf(cfg.CurrentProvider)}
	if cfg.Effort != "" {
		partes = append(partes, laranjaf(cfg.Effort))
	}
	esq := "  " + azulf(barra) + " " + strings.Join(partes, cinzaf(" · "))
	var dir []string
	if contexto > 0 {
		dir = append(dir, fmtContexto(contexto, cfg.Janela))
	}
	if mcps > 0 {
		dir = append(dir, verdof("●")+cinzaf(fmt.Sprintf(" %d MCP", mcps)))
	}
	if sessao != "" {
		dir = append(dir, cinzaf("sess "+sessaoCurta(sessao)))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, esq+"   "+strings.Join(dir, cinzaf("  ")))
}

// promptoTUI é a linha de digitação, dentro da "caixa" de borda azul.
func promptoTUI(out io.Writer) {
	fmt.Fprint(out, "  "+azulf(barra)+" "+azulf("›")+" ")
}

// ecoPedido redesenha o pedido do usuário como bloco com borda, como o
// OpenCode mostra a mensagem enviada: apaga a linha de status e a(s) linha(s)
// digitada(s) — uma linha longa quebra em várias no terminal — e reescreve o
// pedido sozinho na caixa, com uma linha de respiro antes do turno.
func ecoPedido(out io.Writer, pedido string) {
	largura := larguraTerminal()
	digitadas := (larguraPrompto + utf8.RuneCountInString(pedido) + largura - 1) / largura
	if digitadas < 1 {
		digitadas = 1
	}
	// +1: a linha de status logo acima da entrada.
	for i := 0; i < digitadas+1; i++ {
		fmt.Fprint(out, "\033[1A\r\033[K")
	}
	for _, l := range strings.Split(quebrar(pedido, largura-6), "\n") {
		fmt.Fprintln(out, "  "+azulf(barra)+" "+cor(corTexto, l))
	}
	fmt.Fprintln(out)
}

// larguraPrompto: colunas de "  ▌ › " antes do texto digitado.
const larguraPrompto = 6

// ── markdown ──────────────────────────────────────────────────────────────

var (
	reNegrito = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reItalico = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
	reCodigo  = regexp.MustCompile("`([^`\n]+)`")
)

// renderMarkdown converte o markdown mais comum em cor: títulos, negrito,
// itálico, código em linha, blocos de código e listas. O resto passa cru.
func renderMarkdown(texto string, largura int) string {
	var out []string
	emCodigo := false
	for _, l := range strings.Split(strings.TrimRight(texto, "\n"), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			emCodigo = !emCodigo
			if emCodigo {
				if lang := strings.TrimPrefix(t, "```"); lang != "" {
					out = append(out, "  "+cinzaf(lang))
				}
			}
			continue
		}
		if emCodigo {
			out = append(out, "  "+cor(fundoCod+corTexto, " "+l+" "))
			continue
		}
		switch {
		case strings.HasPrefix(t, "#"):
			titulo := strings.TrimSpace(strings.TrimLeft(t, "#"))
			out = append(out, "  "+negritof(azulf(inline(titulo))))
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			recuo := strings.Repeat(" ", len(l)-len(strings.TrimLeft(l, " ")))
			out = append(out, "  "+recuo+azulf("•")+" "+inline(t[2:]))
		case t == "---" || t == "***":
			out = append(out, "  "+cinzaf(strings.Repeat("─", min(largura-4, 60))))
		default:
			for _, q := range strings.Split(quebrar(l, largura-4), "\n") {
				out = append(out, "  "+inline(q))
			}
		}
	}
	return strings.Join(out, "\n")
}

func inline(s string) string {
	if !coresLigadas {
		return s
	}
	s = reCodigo.ReplaceAllString(s, corLaran+"$1"+reset)
	s = reNegrito.ReplaceAllString(s, neg+"$1"+reset)
	s = reItalico.ReplaceAllString(s, "$1"+italico+"$2"+reset)
	return s
}

// ── utilitários ──────────────────────────────────────────────────────────

// resumoArgs pega o argumento mais informativo da chamada (comando, caminho,
// url, query…) para a linha da tool, como "✓ read ~/x.go" do OpenCode.
func resumoArgs(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || len(m) == 0 {
		return ""
	}
	for _, k := range []string{"command", "path", "url", "query", "name", "prompt", "pattern", "id", "estado", "content"} {
		if v, ok := m[k]; ok {
			s := strings.Join(strings.Fields(fmt.Sprint(v)), " ")
			return cortarLargura(s, 70)
		}
	}
	return ""
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// fmtContexto: "9.8K (5%)" — o mesmo formato do canto do OpenCode.
func fmtContexto(tokens, janela int) string {
	k := fmt.Sprintf("%.1fK", float64(tokens)/1000)
	if janela <= 0 {
		return cinzaf(k)
	}
	pct := 100 * tokens / janela
	txt := fmt.Sprintf("%s (%d%%)", k, pct)
	if pct == 0 {
		txt = k + " (<1%)"
	}
	if pct == 0 {
		txt = k + " (<1%)"
	}
	switch {
	case pct >= 80:
		return vermelhof(txt)
	case pct >= 50:
		return amarelof(txt)
	default:
		return cinzaf(txt)
	}
}

func cortarLargura(s string, max int) string {
	if max < 8 {
		max = 8
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// quebrar parte por palavras numa largura; linha vazia é preservada.
func quebrar(s string, largura int) string {
	if largura < 20 {
		largura = 20
	}
	var out []string
	for _, linha := range strings.Split(s, "\n") {
		recuo := linha[:len(linha)-len(strings.TrimLeft(linha, " "))]
		palavras := strings.Fields(linha)
		if len(palavras) == 0 {
			out = append(out, "")
			continue
		}
		atual := recuo
		for _, p := range palavras {
			if utf8.RuneCountInString(atual)+utf8.RuneCountInString(p)+1 > largura && strings.TrimSpace(atual) != "" {
				out = append(out, atual)
				atual = recuo
			}
			if strings.TrimSpace(atual) != "" {
				atual += " "
			}
			atual += p
		}
		out = append(out, atual)
	}
	return strings.Join(out, "\n")
}
