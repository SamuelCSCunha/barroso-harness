// Tela cheia no layout do OpenCode: título da sessão no topo, conversa no meio
// com rolagem, painel lateral (título, contexto, MCPs), caixa de entrada fixa
// embaixo e rodapé com diretório, contexto e atalhos.
//
// Como funciona: o REPL de sempre roda numa goroutine, lendo de um pipe e
// escrevendo num escritor que vira linhas da conversa. O teclado é lido em raw
// mode pela goroutine principal, que edita a caixa de entrada e manda a linha
// para o pipe no Enter. Assim todos os comandos — inclusive os que perguntam
// algo, como /model e /provider — funcionam sem reescrita.
//
// Só liga com stdin e stdout em terminal de verdade; BH_SIMPLES=1 força o modo
// de rolagem. Pipe, -p e testes nunca passam por aqui.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"golang.org/x/term"
)

var ioPipe = io.Pipe

const (
	escAlt       = "\033[?1049h"
	escNormal    = "\033[?1049l"
	escSyncIni   = "\033[?2026h" // pinta o quadro inteiro de uma vez (sem piscar)
	escSyncFim   = "\033[?2026l"
	escLimpa     = "\033[2J"
	escApagaLin  = "\033[K"
	escCurOcult  = "\033[?25l"
	escCurVisiv  = "\033[?25h"
	fundoCaixa   = "\033[48;5;235m"
	fundoPainel  = "\033[48;5;234m"
	fundoUsuario = "\033[48;5;235m"
	larguraPain  = 40
	maxLinhas    = 5000
)

// estadoPainel é o que o painel lateral, a caixa e o rodapé mostram.
type estadoPainel struct {
	titulo   string
	modelo   string
	provider string
	effort   string
	cwd      string
	contexto int
	janela   int
	mcps     []string
}

type tela struct {
	out io.Writer

	mu      sync.Mutex
	linhas  []string
	parcial string // linha ainda sem \n (ex.: "selecionar modelo > ")
	transit string // linha transitória do spinner
	rolagem int
	w, h    int
	estado  estadoPainel
	entrada string
	ocultar bool // a resposta digitada é segredo (chave da API)
	ocupado bool // turno do modelo em curso
	aviso   string
	// tituloAba é o último título enviado à aba (só reenvia quando muda).
	tituloAba string
	spin      chan struct{}
	spinFim   chan struct{}
}

func novaTela(out io.Writer) *tela {
	fmt.Fprint(out, escAlt+escLimpa)
	return &tela{out: out}
}

func (t *tela) encerrar() {
	t.pararSpin()
	// A aba volta ao título neutro: o shell (cmd/PowerShell) não reescreve o
	// título sozinho, e o nome da sessão ficaria preso na aba depois do bh.
	fmt.Fprint(t.out, escCurVisiv+escNormal+tituloAba(""))
}

// tituloAba é a sequência OSC 0 que troca o nome da aba/janela do terminal.
// Caracteres de controle saem do título para não quebrar a sequência.
func tituloAba(sessao string) string {
	nome := "bh"
	if s := strings.TrimSpace(sessao); s != "" {
		limpo := strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return -1
			}
			return r
		}, s)
		if r := []rune(limpo); len(r) > 60 {
			limpo = string(r[:60]) + "…"
		}
		nome = "bh | " + limpo
	}
	return "\033]0;" + nome + "\a"
}

// ── API usada pelo REPL e pelo renderer do turno ──────────────────────────

// Adicionar acrescenta linhas à conversa e volta a rolagem para o fim.
func (t *tela) Adicionar(texto string) {
	t.mu.Lock()
	t.linhas = append(t.linhas, strings.Split(texto, "\n")...)
	t.aparar()
	t.rolagem = 0
	t.desenharTrancado()
	t.mu.Unlock()
}

// Usuario desenha a mensagem enviada como bloco de fundo escuro com a barra
// azul, como o OpenCode.
func (t *tela) Usuario(texto string) {
	t.mu.Lock()
	larg := t.larguraConversa() - 2
	vazia := comFundo(azulf("▌"), fundoUsuario, larg)
	t.linhas = append(t.linhas, "", vazia)
	for _, l := range strings.Split(quebrar(texto, larg-6), "\n") {
		t.linhas = append(t.linhas, comFundo(azulf("▌")+"  "+cor(corTexto, l), fundoUsuario, larg))
	}
	t.linhas = append(t.linhas, vazia, "")
	t.aparar()
	t.rolagem = 0
	t.desenharTrancado()
	t.mu.Unlock()
}

func (t *tela) Limpar() {
	t.mu.Lock()
	t.linhas, t.parcial, t.rolagem = nil, "", 0
	t.desenharTrancado()
	t.mu.Unlock()
}

func (t *tela) Estado(e estadoPainel) {
	t.mu.Lock()
	t.estado = e
	t.desenharTrancado()
	t.mu.Unlock()
}

func (t *tela) Ocupado(v bool) {
	t.mu.Lock()
	t.ocupado = v
	t.desenharTrancado()
	t.mu.Unlock()
}

func (t *tela) Ocultar(v bool) {
	t.mu.Lock()
	t.ocultar = v
	t.mu.Unlock()
}

func (t *tela) aparar() {
	if n := len(t.linhas); n > maxLinhas {
		t.linhas = t.linhas[n-maxLinhas:]
	}
}

// Pensar anima "⠋ <texto> 3s" como última linha da conversa até pararSpin.
func (t *tela) Pensar(texto string) {
	t.pararSpin()
	t.mu.Lock()
	t.spin, t.spinFim = make(chan struct{}), make(chan struct{})
	parar, fim := t.spin, t.spinFim
	t.mu.Unlock()
	go func() {
		defer close(fim)
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		desde := time.Now()
		for i := 0; ; i++ {
			t.mu.Lock()
			t.transit = "  " + azulf(quadrosSpin[i%len(quadrosSpin)]) + " " +
				cor(italico+corTexto, texto) + " " + cinzaf(fmt.Sprintf("%.0fs", time.Since(desde).Seconds()))
			t.desenharTrancado()
			t.mu.Unlock()
			select {
			case <-parar:
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *tela) pararSpin() {
	t.mu.Lock()
	parar, fim := t.spin, t.spinFim
	t.spin, t.spinFim = nil, nil
	t.mu.Unlock()
	if parar != nil {
		close(parar)
		<-fim
	}
	t.mu.Lock()
	t.transit = ""
	t.mu.Unlock()
}

// escritor é o io.Writer do REPL: cada linha terminada vira linha da conversa;
// o resto fica como linha parcial (um prompt esperando resposta).
type escritor struct{ t *tela }

func (e escritor) Write(p []byte) (int, error) {
	t := e.t
	t.mu.Lock()
	partes := strings.Split(t.parcial+string(p), "\n")
	for _, l := range partes[:len(partes)-1] {
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:]
		}
		t.linhas = append(t.linhas, l)
	}
	t.parcial = partes[len(partes)-1]
	t.aparar()
	t.rolagem = 0
	t.desenharTrancado()
	t.mu.Unlock()
	return len(p), nil
}

// ── desenho ───────────────────────────────────────────────────────────────

// tamanhoTela devolve colunas e linhas do terminal (100x30 quando não dá para
// saber). Variável para a prévia e os testes fixarem um tamanho.
var tamanhoTela = func() (int, int) {
	if a, b, err := term.GetSize(int(os.Stdout.Fd())); err == nil && a >= 40 && b >= 12 {
		return a, b
	}
	return 100, 30
}

// medir lê o tamanho do terminal; devolve true quando mudou.
func (t *tela) medir() bool {
	w, h := tamanhoTela()
	mudou := w != t.w || h != t.h
	t.w, t.h = w, h
	return mudou
}

func (t *tela) painelVisivel() bool { return t.w >= 110 }

func (t *tela) larguraConversa() int {
	if t.painelVisivel() {
		return t.w - larguraPain - 2
	}
	return t.w - 2
}

// desenharTrancado pinta o quadro inteiro. Chamado com t.mu travado.
//
// Linhas: 1..h-5 conversa · h-4 vazio/rolagem ·
// h-3..h-1 caixa de entrada · h rodapé. O painel ocupa as colunas da direita
// nas linhas 1..h-1; nada é escrito na última coluna da última linha (isso
// faria o terminal rolar).
func (t *tela) desenharTrancado() {
	limpar := t.medir()
	w, h := t.w, t.h
	larg := t.larguraConversa()
	altura := max(3, h-5)

	var sb strings.Builder
	sb.WriteString(escSyncIni + escCurOcult)
	if limpar {
		sb.WriteString(escLimpa)
	}
	linha := func(r int, s string) {
		fmt.Fprintf(&sb, "\033[%d;1H%s%s%s", r, reset, s, escApagaLin)
	}

	// Sem linha de título: o nome da sessão vai para a aba do terminal (OSC 0)
	// e para o painel. O bh tem uma sessão por terminal, então a barra de abas
	// do OpenCode não faz sentido aqui.
	// Sessão ainda sem nome aparece como "Nova sessão", igual ao painel. O
	// título é comparado já resolvido: comparar o cru deixava a aba sem
	// título na abertura (vazio == valor inicial de tituloAba).
	nomeAba := t.estado.titulo
	if strings.TrimSpace(nomeAba) == "" {
		nomeAba = "Nova sessão"
	}
	if nomeAba != t.tituloAba {
		t.tituloAba = nomeAba
		sb.WriteString(tituloAba(nomeAba))
	}

	// conversa
	var todas []string
	for _, l := range t.linhas {
		todas = append(todas, quebrarVis(l, larg-2)...)
	}
	if t.parcial != "" {
		todas = append(todas, quebrarVis(t.parcial+t.entradaVisivel(), larg-2)...)
	}
	if t.transit != "" {
		todas = append(todas, t.transit)
	}
	if t.rolagem > len(todas)-altura {
		t.rolagem = max(0, len(todas)-altura)
	}
	fim := len(todas) - t.rolagem
	vis := todas[max(0, fim-altura):fim]
	for i := 0; i < altura; i++ {
		s := ""
		if i < len(vis) {
			s = " " + vis[i]
		}
		linha(1+i, s)
	}
	if t.rolagem > 0 {
		linha(h-4, "  "+cinzaf(fmt.Sprintf("↓ %d linhas abaixo · PgDn volta", t.rolagem)))
	} else {
		linha(h-4, "")
	}

	// caixa de entrada; enquanto um prompt de comando espera resposta, o texto
	// digitado aparece na própria linha do prompt e a caixa fica vazia
	caixa := larg - 2
	texto := ""
	if t.parcial == "" {
		texto = t.entradaVisivel()
		if texto == "" && !t.ocupado {
			texto = cinzaf("Pergunte qualquer coisa…  /help lista os comandos")
		}
	}
	linha(h-3, "  "+comFundo(azulf("▌")+" "+texto, fundoCaixa, caixa))
	linha(h-2, "  "+comFundo(azulf("▌"), fundoCaixa, caixa))
	info := negritof(cor(corTexto, t.estado.modelo)) + " " + cinzaf(t.estado.provider)
	if t.estado.effort != "" {
		info += cinzaf(" · ") + laranjaf(t.estado.effort)
	}
	linha(h-1, "  "+comFundo(azulf("▌")+" "+info, fundoCaixa, caixa))

	// rodapé
	esq := "  " + cinzaf(t.estado.cwd)
	var dirP []string
	switch {
	case t.aviso != "":
		dirP = append(dirP, amarelof(t.aviso))
	case t.ocupado:
		dirP = append(dirP, negritof(cor(corTexto, "esc"))+" "+cinzaf("interrompe"))
	}
	if t.estado.contexto > 0 {
		dirP = append(dirP, fmtContexto(t.estado.contexto, t.estado.janela))
	}
	dirP = append(dirP, negritof(cor(corTexto, "/help"))+" "+cinzaf("comandos"))
	dir := strings.Join(dirP, "  ")
	linha(h, esq+strings.Repeat(" ", max(1, larg-larguraVis(esq)-larguraVis(dir)))+dir)

	// painel lateral
	if t.painelVisivel() {
		col := w - larguraPain + 1
		conteudo := t.linhasPainel()
		for r := 1; r <= h-1; r++ {
			s := ""
			if r-1 >= 0 && r-1 < len(conteudo) {
				s = conteudo[r-1]
			}
			fmt.Fprintf(&sb, "\033[%d;%dH%s%s", r, col, reset, comFundo(s, fundoPainel, larguraPain-1))
		}
	}

	// cursor: na caixa, ou no fim da linha do prompt de comando
	if !t.ocupado {
		if t.parcial == "" {
			fmt.Fprintf(&sb, "\033[%d;%dH", h-3, 5+larguraVis(t.entradaVisivel()))
		} else if len(vis) > 0 {
			fmt.Fprintf(&sb, "\033[%d;%dH", 1+len(vis)-1, 2+larguraVis(vis[len(vis)-1]))
		}
		sb.WriteString(escCurVisiv)
	}
	sb.WriteString(escSyncFim)
	fmt.Fprint(t.out, sb.String())
}

func (t *tela) entradaVisivel() string {
	if t.ocultar {
		return strings.Repeat("•", utf8.RuneCountInString(t.entrada))
	}
	return t.entrada
}

func (t *tela) linhasPainel() []string {
	e := t.estado
	titulo := e.titulo
	if titulo == "" {
		titulo = "Nova sessão"
	}
	var out []string
	for _, l := range strings.Split(quebrar(titulo, larguraPain-6), "\n") {
		out = append(out, "  "+negritof(cor(corTexto, l)))
	}
	out = append(out, "", "  "+negritof(cor(corTexto, "Contexto")))
	if e.contexto > 0 {
		out = append(out, "  "+cinzaf(milhar(e.contexto)+" tokens"))
		if e.janela > 0 {
			txt := fmt.Sprintf("%d%% usado", 100*e.contexto/e.janela)
			if 100*e.contexto/e.janela == 0 {
				txt = "<1% usado"
			}
			out = append(out, "  "+cinzaf(txt))
		}
	} else {
		out = append(out, "  "+cinzaf("—"))
	}
	out = append(out, "", "  "+negritof(cor(corTexto, "▼ MCP")))
	if len(e.mcps) == 0 {
		out = append(out, "  "+cinzaf("nenhum servidor"))
	}
	for _, m := range e.mcps {
		nome := cortarVis(m, larguraPain-18)
		pad := strings.Repeat(" ", max(1, larguraPain-15-larguraVis(nome)))
		out = append(out, "  "+verdof("•")+" "+negritof(cor(corTexto, nome))+pad+cinzaf("Conectado"))
	}
	return out
}

// ── teclado ───────────────────────────────────────────────────────────────

type tecla struct {
	texto string
	acao  string
}

// lerTeclas decodifica o stdin cru em teclas. Setas e PgUp/PgDn chegam como
// sequências de escape (o raw mode do Windows liga ENABLE_VIRTUAL_TERMINAL_INPUT);
// acentos chegam em UTF-8 de vários bytes e são remontados.
func lerTeclas(r *bufio.Reader, ch chan<- tecla) {
	defer close(ch)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		switch {
		case b == '\r' || b == '\n':
			ch <- tecla{acao: "enter"}
		case b == 127 || b == 8:
			ch <- tecla{acao: "backspace"}
		case b == 3:
			ch <- tecla{acao: "ctrl+c"}
		case b == 4:
			ch <- tecla{acao: "ctrl+d"}
		case b == 12:
			ch <- tecla{acao: "ctrl+l"}
		case b == 21:
			ch <- tecla{acao: "ctrl+u"}
		case b == 27:
			if r.Buffered() == 0 {
				ch <- tecla{acao: "esc"}
				continue
			}
			c, _ := r.ReadByte()
			if c != '[' && c != 'O' {
				continue
			}
			f, _ := r.ReadByte()
			switch f {
			case 'A':
				ch <- tecla{acao: "cima"}
			case 'B':
				ch <- tecla{acao: "baixo"}
			case '5', '6':
				_, _ = r.ReadByte() // '~'
				if f == '5' {
					ch <- tecla{acao: "pgup"}
				} else {
					ch <- tecla{acao: "pgdn"}
				}
			default:
				// demais sequências (setas laterais, F1…): descarta até o byte final
				for x := f; !(x >= 0x40 && x <= 0x7e) && r.Buffered() > 0; {
					x, _ = r.ReadByte()
				}
			}
		case b >= 0x80:
			n := 1
			switch {
			case b&0xE0 == 0xC0:
				n = 2
			case b&0xF0 == 0xE0:
				n = 3
			case b&0xF8 == 0xF0:
				n = 4
			}
			buf := []byte{b}
			for i := 1; i < n; i++ {
				c, err := r.ReadByte()
				if err != nil {
					return
				}
				buf = append(buf, c)
			}
			ch <- tecla{texto: string(buf)}
		case b >= 32 || b == '\t':
			ch <- tecla{texto: string(rune(b))}
		}
	}
}

// ── entrada principal ─────────────────────────────────────────────────────

// usarTela diz se o REPL abre em tela cheia.
func usarTela(io IO) bool {
	if os.Getenv("BH_SIMPLES") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	in, okIn := io.In.(*os.File)
	out, okOut := io.Out.(*os.File)
	return okIn && okOut && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// runTela roda o REPL normal por trás da tela cheia. Devolve -1 quando não
// consegue o raw mode (o chamador cai no modo de rolagem).
func runTela(io IO) int {
	fd := int(os.Stdin.Fd())
	antigo, err := term.MakeRaw(fd)
	if err != nil {
		return -1
	}
	defer term.Restore(fd, antigo)

	t := novaTela(io.Out)
	defer t.encerrar()

	pr, pw := ioPipe()
	esc := escritor{t: t}
	sub := IO{In: pr, Out: esc, ErrOut: esc, tela: t, cancelar: make(chan struct{}, 1)}
	sub.Reader() // o bufio compartilhado nasce aqui, antes das cópias de IO

	// A chave da API não pode ser lida com term.ReadPassword (o stdin está em
	// raw mode na mão da tela): vem pela caixa, mascarada com •.
	antesKey := PromptKey
	PromptKey = func(prompt string) (string, error) {
		t.Ocultar(true)
		defer t.Ocultar(false)
		fmt.Fprint(esc, prompt)
		l, err := sub.Reader().ReadString('\n')
		return strings.TrimSpace(l), err
	}
	defer func() { PromptKey = antesKey }()

	codigo := make(chan int, 1)
	go func() {
		codigo <- RunInteractive(sub)
		_ = pr.Close()
	}()

	teclas := make(chan tecla, 64)
	go lerTeclas(bufio.NewReader(os.Stdin), teclas)
	tick := time.NewTicker(400 * time.Millisecond) // redimensionamento e avisos
	defer tick.Stop()

	var (
		historico []string
		posHist   = -1
		ultimoCC  time.Time
	)
	enviar := func(s string) {
		t.mu.Lock()
		if t.parcial != "" {
			// resposta a um prompt de comando fica registrada na conversa
			vis := s
			if t.ocultar {
				vis = strings.Repeat("•", utf8.RuneCountInString(s))
			}
			t.linhas = append(t.linhas, t.parcial+vis)
			t.parcial = ""
		}
		t.entrada = ""
		t.desenharTrancado()
		t.mu.Unlock()
		_, _ = pw.Write([]byte(s + "\n"))
	}
	editar := func(f func()) {
		t.mu.Lock()
		f()
		t.desenharTrancado()
		t.mu.Unlock()
	}

	for {
		select {
		case c := <-codigo:
			_ = pw.Close()
			return c
		case <-tick.C:
			editar(func() {
				if t.aviso != "" && time.Since(ultimoCC) > 2*time.Second {
					t.aviso = ""
				}
				// Reafirma o nome da aba: processos filhos que rodam pelo
				// cmd.exe (npx.cmd do MCP do Playwright, terminal_run com
				// cmd /C) sobrescrevem o título do console, e não há como o
				// bh impedir. Esvaziar força o reenvio no quadro seguinte.
				t.tituloAba = ""
			})
		case k, ok := <-teclas:
			if !ok {
				_ = pw.Close()
				return <-codigo
			}
			t.mu.Lock()
			ocupado, aguardando := t.ocupado, t.parcial != ""
			t.mu.Unlock()
			switch k.acao {
			case "":
				editar(func() { t.entrada += k.texto })
			case "backspace":
				editar(func() {
					if _, n := utf8.DecodeLastRuneInString(t.entrada); n > 0 {
						t.entrada = t.entrada[:len(t.entrada)-n]
					}
				})
			case "ctrl+u":
				editar(func() { t.entrada = "" })
			case "enter":
				t.mu.Lock()
				l := t.entrada
				t.mu.Unlock()
				if aguardando {
					enviar(l) // Enter vazio é resposta válida (ex.: mantém o modelo)
					continue
				}
				if strings.TrimSpace(l) == "" || ocupado {
					continue // um pedido por vez
				}
				historico = append(historico, l)
				posHist = -1
				enviar(l)
			case "cima", "baixo":
				if len(historico) == 0 {
					continue
				}
				if k.acao == "cima" {
					if posHist < 0 {
						posHist = len(historico) - 1
					} else if posHist > 0 {
						posHist--
					}
				} else if posHist >= 0 {
					posHist++
				}
				editar(func() {
					if posHist >= 0 && posHist < len(historico) {
						t.entrada = historico[posHist]
					} else {
						posHist, t.entrada = -1, ""
					}
				})
			case "pgup", "pgdn":
				editar(func() {
					passo := max(3, t.h-10)
					if k.acao == "pgup" {
						t.rolagem += passo
					} else {
						t.rolagem = max(0, t.rolagem-passo)
					}
				})
			case "ctrl+l":
				editar(func() { t.w = 0 }) // força limpar e repintar
			case "esc", "ctrl+c":
				if ocupado {
					select {
					case sub.cancelar <- struct{}{}:
					default:
					}
					continue
				}
				if k.acao == "esc" {
					continue
				}
				t.mu.Lock()
				vazia := t.entrada == ""
				t.mu.Unlock()
				if !vazia {
					editar(func() { t.entrada = "" })
					continue
				}
				if time.Since(ultimoCC) < 2*time.Second {
					enviar("/exit")
					continue
				}
				ultimoCC = time.Now()
				editar(func() { t.aviso = "ctrl+c de novo para sair" })
			case "ctrl+d":
				enviar("/exit")
			}
		}
	}
}

// ── texto com cor: largura visível, corte, quebra, fundo ──────────────────

// fimEscape devolve o índice logo após a sequência ANSI que começa em i.
func fimEscape(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
	}
	for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
		j++
	}
	return min(j+1, len(s))
}

// larguraVis conta colunas visíveis (escapes ANSI não contam).
func larguraVis(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			i = fimEscape(s, i)
			continue
		}
		_, tam := utf8.DecodeRuneInString(s[i:])
		n++
		i += tam
	}
	return n
}

// cortarVis corta em n colunas visíveis preservando os escapes.
func cortarVis(s string, n int) string {
	if larguraVis(s) <= n {
		return s
	}
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := fimEscape(s, i)
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		if vis >= n-1 {
			sb.WriteString("…" + reset)
			return sb.String()
		}
		_, tam := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+tam])
		vis++
		i += tam
	}
	return sb.String()
}

// quebrarVis parte uma linha colorida em pedaços de n colunas visíveis.
func quebrarVis(s string, n int) []string {
	n = max(10, n)
	if larguraVis(s) <= n {
		return []string{s}
	}
	var out []string
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := fimEscape(s, i)
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		if vis == n {
			out = append(out, sb.String())
			sb.Reset()
			sb.WriteString("  ")
			vis = 2
		}
		_, tam := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+tam])
		vis++
		i += tam
	}
	return append(out, sb.String())
}

// comFundo pinta s com o fundo bg em exatamente n colunas. Cada reset dentro
// de s reaplica o fundo, para a cor do texto não abrir buraco na caixa.
func comFundo(s, bg string, n int) string {
	if !coresLigadas {
		bg = ""
	}
	s = strings.ReplaceAll(cortarVis(s, n), reset, reset+bg)
	return bg + s + strings.Repeat(" ", max(0, n-larguraVis(s))) + reset
}

func milhar(n int) string {
	s := fmt.Sprintf("%d", n)
	var partes []string
	for len(s) > 3 {
		partes = append([]string{s[len(s)-3:]}, partes...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, partes...), ".")
}

// cwdCurto troca o home por ~ no diretório do rodapé.
func cwdCurto(cwd string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return "~" + string(filepath.Separator) + rel
		}
	}
	return cwd
}

// tituloSessao é a primeira fala do usuário, como o OpenCode nomeia a sessão.
func tituloSessao(h []dealer.Mensagem) string {
	for _, m := range h {
		if m.Role != "user" || strings.HasPrefix(m.Conteudo, "[estado de trabalho") {
			continue
		}
		t := strings.Join(strings.Fields(m.Conteudo), " ")
		if r := []rune(t); len(r) > 60 {
			t = string(r[:60]) + "…"
		}
		return t
	}
	return ""
}

// ── renderer do turno na tela cheia ───────────────────────────────────────

type turnoTela struct {
	t        *tela
	modelo   string
	argsPend string
	contexto int
}

func (r *turnoTela) Pensando() { r.t.Pensar("Pensando") }

func (r *turnoTela) ToolInicio(nome, args string) {
	r.argsPend = resumoArgs(args)
	r.t.Pensar(strings.TrimSpace(nome + " " + r.argsPend))
}

func (r *turnoTela) Contexto(n int) { r.contexto = n }

func (r *turnoTela) Tool(nome string, dur time.Duration, erro bool) {
	r.t.pararSpin()
	marca := verdof("✓")
	if erro {
		marca = vermelhof("✗")
	}
	args := r.argsPend
	r.argsPend = ""
	r.t.Adicionar("  " + marca + " " + cor(corTexto, nome) + " " + cinzaf(args) + " " + cinzaf(fmtDur(dur)))
}

func (r *turnoTela) Eco(saida string) {
	saida = strings.TrimRight(saida, "\n")
	if saida == "" {
		return
	}
	ls := strings.Split(saida, "\n")
	extra := 0
	if len(ls) > 6 {
		extra, ls = len(ls)-6, ls[:6]
	}
	for i, l := range ls {
		pre := "    "
		if i == 0 {
			pre = "  " + cinzaf("›") + " "
		}
		r.t.Adicionar(pre + cinzaf(l))
	}
	if extra > 0 {
		r.t.Adicionar("    " + cinzaf(fmt.Sprintf("… +%d linhas", extra)))
	}
}

func (r *turnoTela) Guardrail(proativa bool) {
	r.t.pararSpin()
	msg := "contexto compactado pelo guardrail (o provider recusou por tamanho)"
	if proativa {
		msg = "contexto compactado"
	}
	r.t.Adicionar("  " + roxof("◆") + " " + cinzaf(msg))
}

func (r *turnoTela) Resposta(texto string) {
	r.t.pararSpin()
	r.t.mu.Lock()
	larg := r.t.larguraConversa() - 4
	r.t.mu.Unlock()
	r.t.Adicionar("")
	r.t.Adicionar(renderMarkdown(texto, larg))
}

func (r *turnoTela) Tokens(_, saida int, dur time.Duration, compactou bool) {
	r.t.pararSpin()
	partes := []string{cor(corTexto, r.modelo), fmtDur(dur)}
	if saida > 0 && dur > 0 {
		partes = append(partes, fmt.Sprintf("%.1f tok/s", float64(saida)/dur.Seconds()))
	}
	if compactou {
		partes = append(partes, roxof("compactado"))
	}
	r.t.Adicionar("")
	r.t.Adicionar("  " + strings.Join(partes, cinzaf(" · ")))
}

// tituloAtual é o nome da sessão: o gravado (/rename, /save) ou, sem ele, a
// primeira fala do usuário, como o OpenCode nomeia a sessão.
func tituloAtual(loja *store.Store, sess *runtime.Session) string {
	if loja != nil && sess.ID() != "" {
		if se, err := loja.GetSession(sess.ID()); err == nil && strings.TrimSpace(se.Title) != "" {
			return strings.TrimSpace(se.Title)
		}
	}
	return tituloSessao(sess.History())
}
