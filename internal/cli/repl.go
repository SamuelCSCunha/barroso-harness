// Package cli abriga o modo interativo: onboarding de provider/model e o
// REPL com /model, /effort, /provider, /help e /exit. A leitura da chave é
// oculta (x/term); testes injetam reader/writer e chaves fictícias.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
	"golang.org/x/term"
)

// Esforcos são os níveis comuns de reasoning_effort. O provider pode aceitar
// outros; validação real é o HTTP dele, e o erro volta para o usuário com a
// chance de limpar via `/effort -` (nada de downgrade silencioso).
var Esforcos = []string{"low", "medium", "high", "xhigh", "max"}

// IO agrega os três fluxos, para testes injetarem reader/writer. O Reader
// compartilhado evita descartar bytes quando a entrada é prebuffered.
type IO struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
	reader *bufio.Reader

	// tela != nil quando o REPL roda por trás da tela cheia (tela.go): Out e
	// ErrOut viram linhas da conversa e cancelar recebe o Esc/Ctrl+C do turno.
	tela     *tela
	cancelar chan struct{}
}

func (io *IO) Reader() *bufio.Reader {
	if io.reader == nil {
		io.reader = bufio.NewReader(io.In)
	}
	return io.reader
}

// PromptKey lê a chave SEM eco (x/term no stdin cru). Testes sobrescrevem.
var PromptKey = func(prompt string) (string, error) {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// buscarModelos é a costura de rede do catálogo: produção chama
// dealer.Modelos, testes substituem para não sair da máquina. Separado do
// PromptKey porque aqui a substituição precisa de URL e chave.
var buscarModelos = func(ctx context.Context, base, key string) ([]dealer.CatalogModel, error) {
	return dealer.Modelos(ctx, base, key)
}

// RunInteractive é o caminho de `bh` sem argumentos: onboarding se a config
// estiver incompleta, REPL em seguida.
func RunInteractive(io IO) int {
	if io.tela == nil && usarTela(io) {
		if c := runTela(io); c >= 0 {
			return c
		}
	}
	if err := run(io); err != nil {
		fmt.Fprintf(io.ErrOut, "erro: %v\n", err)
		return 1
	}
	return 0
}

func run(io IO) error {
	io.Reader() // inicializa o buffer ANTES de qualquer leitura: as cópias de IO
	// passadas a Onboarding/repl precisam compartilhar o mesmo *bufio.Reader,
	// senão o segundo leitor vê EOF e o REPL não recebe linha nenhuma.
	if io.tela != nil {
		coresLigadas = true // Out é o escritor da tela, que é terminal de verdade
	} else {
		ligarCores(io)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Pronta() {
		fmt.Fprintln(io.Out, "Barroso Harness — primeira configuração (provider, chave e modelo; a chave fica só neste computador).")
		if err := Onboarding(io, &cfg); err != nil {
			if err == errCancelar || strings.Contains(strings.ToLower(err.Error()), "cancelado") {
				fmt.Fprintln(io.Out, "onboarding cancelado; nada foi alterado ou salvo.")
				return nil
			}
			return err
		}
	} else if io.tela != nil {
		// a tela cheia já tem título, painel e rodapé
	} else if coresLigadas {
		cabecalhoTUI(io.Out, Versao)
	} else {
		banner(io.Out)
	}
	repl(io, &cfg)
	return nil
}

var errCancelar = fmt.Errorf("cancelado")

// errMantido: a seleção de modelo terminou sem troca (Enter vazio).
var errMantido = fmt.Errorf("modelo mantido")

// ── onboarding ───────────────────────────────────────────────────────────

// Onboarding guia: base URL (default keys.barroso) → chave oculta →
// /models validado → escolha de modelo. Reasoning effort inicia desativado.
// Persiste somente no sucesso; Ctrl+D/cancelamento devolve errCancelar.
func Onboarding(io IO, cfg *config.Config) error {
	if io.reader == nil {
		io.reader = bufio.NewReader(io.In)
	}
	base := config.DefaultBaseURL
	if p := cfg.Ativo(); p != nil && p.BaseURL != "" {
		base = p.BaseURL
	}
	if v := perguntar(io, "base URL", base); strings.TrimSpace(v) != "" && strings.TrimSpace(v) != base {
		base = strings.TrimSpace(v)
	}

	key := ""
	if p := cfg.Ativo(); p != nil && p.APIKey != "" {
		key = p.APIKey
		fmt.Fprintln(io.Out, "chave atual preservada; digite enter para mantê-la ou uma nova (input oculto).")
	}
	if nova, err := PromptKey("api key: "); err == nil && strings.TrimSpace(nova) != "" {
		key = strings.TrimSpace(nova)
	}
	if key == "" {
		return fmt.Errorf("chave obrigatória para consultar /models e autenticar os turnos")
	}

	if err := configurarProvider(io, cfg, base, key); err != nil {
		if _, ok := err.(*ForaDoCatalogo); !ok {
			return err
		}
	}
	if cfg.Model == "" {
		if err := selecionarModelo(io, cfg); err != nil {
			return err
		}
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("salvar config: %w", err)
	}
	fmt.Fprintln(io.Out, "salvo em "+config.Dir()+"\\config.json — effort desativado por enquanto (/effort liga).")
	fmt.Fprintln(io.Out, cfg.Resumo())
	return nil
}

// perguntar lê uma linha de entrada textual com sugestão default.
func perguntar(io IO, rótulo, def string) string {
	fmt.Fprintf(io.Out, "%s [%s]: ", rótulo, def)
	linha, _ := io.Reader().ReadString('\n')
	return strings.TrimSpace(linha)
}

// configurarProvider valida base+chave NO /models ANTES de alterar memória ou
// disco; em falha devolve erro — nada mudou (rollback integral). Quando o
// model guardado não existe no catálogo novo, a cópia fica com o provider
// aplicado e Model="" e devolve *ForaDoCatalogo (caller decide persistir).
func configurarProvider(io IO, cfg *config.Config, base, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := buscarModelos(ctx, base, key)
	if err != nil {
		return fmt.Errorf("provider recusou %s: %v (rollback — nada foi salvo)", base, err)
	}
	copia := *cfg
	copia = atualizarProvider(copia, definirNomeProvider(base), base, key)

	fora := ForaDoCatalogo{}
	if copia.Model != "" {
		if !catalogoTem(copia.Model, ls) {
			fora.Modelo = copia.Model
			copia.Model = "" // provider novo não tem o modelo guardado
		}
	}
	if copia.Model != "" {
		// Model existente no catálogo novo: a janela segue o que está gravado
		// (fonte manual) ou rederiva do catálogo quando derivada.
		if copia.JanelaSource != config.FonteManual {
			if j := janelaDe(copia.Model, ls); j > 0 {
				copia = aplicarJanela(copia, j)
			} else {
				copia = limparJanelaCatalogo(copia)
			}
		}
	}
	*cfg = copia
	if fora.Modelo != "" || copia.Model == "" {
		return &fora
	}
	return nil
}

type ForaDoCatalogo struct{ Modelo string }

func (e *ForaDoCatalogo) Error() string {
	return "model " + e.Modelo + " fora do catálogo deste provider; escolha outro (/model)"
}

func definirNomeProvider(base string) string {
	t := strings.TrimSuffix(strings.TrimSpace(base), "/")
	t = strings.TrimPrefix(strings.TrimPrefix(t, "https://"), "http://")
	if i := strings.IndexAny(t, "/:"); i >= 0 {
		t = t[:i]
	}
	return strings.NewReplacer(".", "-").Replace(t)
}

func catalogoTem(id string, ls []dealer.CatalogModel) bool {
	if id == "" {
		return false
	}
	for _, m := range ls {
		if m.ID == id {
			return true
		}
	}
	return false
}

// aplicarJanela respeita override manual: se o usuário escreveu
// context_window à mão, catálogo não sobrescreve.
func aplicarJanela(cfg config.Config, janela int) config.Config {
	if cfg.JanelaSource == config.FonteManual && cfg.Janela > 0 {
		return cfg
	}
	cfg.Janela = janela
	cfg.JanelaSource = config.FonteCatalogo
	return cfg
}

// limparJanelaCatalogo zera somente o valor derivado do catálogo; manual fica.
func limparJanelaCatalogo(cfg config.Config) config.Config {
	if cfg.JanelaSource == config.FonteManual && cfg.Janela > 0 {
		return cfg
	}
	cfg.Janela = 0
	cfg.JanelaSource = ""
	return cfg
}

func atualizarProvider(cfg config.Config, nome, base, key string) config.Config {
	if cfg.Providers == nil {
		cfg.Providers = map[string]*config.Provider{}
	}
	cfg.Providers[nome] = &config.Provider{BaseURL: base, APIKey: key}
	cfg.CurrentProvider = nome
	cfg.BaseURL, cfg.APIKey = base, key
	return cfg
}

// janelaDe devolve o context_length publicado para o id (0 = desconhecida).
func janelaDe(id string, ls []dealer.CatalogModel) int {
	for _, m := range ls {
		if m.ID == id {
			return m.ContextLength
		}
	}
	return 0
}

// ── seleção de model ─────────────────────────────────────────────────────

// selecionarModelo: lista o catálogo filtrado (text-capable), ordem
// alfabética, numerado; aceita índice ou ID. Atualiza janela derivada.
func selecionarModelo(io IO, cfg *config.Config) error {
	p := cfg.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := buscarModelos(ctx, base, key)
	if err != nil {
		return err
	}
	if len(ls) == 0 {
		return fmt.Errorf("catálogo sem modelos plausíveis de chat")
	}
	listarModelos(io.Out, ls, cfg.Model)
	for {
		fmt.Fprint(io.Out, cianof("selecionar modelo > "))
		linha, err := io.Reader().ReadString('\n')
		if err != nil {
			return errCancelar
		}
		v := strings.TrimSpace(linha)
		if v == "" {
			// Enter vazio com um modelo válido do catálogo = sair sem trocar.
			// Sem modelo (onboarding, provider novo que não tem o atual) não
			// há o que manter, então a escolha continua obrigatória.
			if cfg.Model != "" && catalogoTem(cfg.Model, ls) {
				return errMantido
			}
			fmt.Fprintln(io.Out, "escolha um item (número ou id) — sem modelo não há turno.")
			continue
		}
		id := ""
		if n, errA := strconv.Atoi(v); errA == nil && n >= 1 && n <= len(ls) {
			id = ls[n-1].ID
		} else if catalogoTem(v, ls) {
			id = v
		}
		if id == "" {
			fmt.Fprintf(io.Out, "%q não está no catálogo; repita.\n", v)
			continue
		}
		cfg.Model = id
		if j := janelaDe(id, ls); j > 0 {
			*cfg = aplicarJanela(*cfg, j)
		} else {
			*cfg = limparJanelaCatalogo(*cfg)
		}
		return nil
	}
}

// ── REPL ─────────────────────────────────────────────────────────────────

// sinais é preenchido por signal.Notify; o primeiro Ctrl+C durante um turno
// cancela a rodada, um segundo encerra o REPL (via canal fechar).
func (io IO) cancelarTurno(fechar chan<- struct{}) (context.Context, context.CancelFunc) {
	signais := make(chan os.Signal, 2)
	signal.Notify(signais, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	if io.cancelar != nil {
		// Tela cheia: o raw mode não gera sinal; Esc/Ctrl+C chegam por aqui.
		// Um pedido que sobrou de um turno já encerrado não pode derrubar
		// este (ex.: Enter no instante em que o turno anterior terminava).
		select {
		case <-io.cancelar:
		default:
		}
		go func() {
			select {
			case <-io.cancelar:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	go func() {
		<-signais
		fmt.Fprintln(io.Out)
		avisoFmt(io.Out, "cancelando a rodada — de novo para sair")
		cancel()
		select {
		case <-signais:
			close(fechar)
		case <-ctx.Done():
		}
		signal.Stop(signais)
	}()
	return ctx, cancel
}

func repl(io IO, cfg *config.Config) {
	// Persistência best-effort: sem banco, o REPL continua em memória (o aviso
	// sai uma vez, em ErrOut, e nunca interrompe a conversa).
	loja, idAtivo := abrirStore(io)
	var sess *runtime.Session
	if loja != nil {
		sess = runtime.NewSessionWithStore(*cfg, loja, idAtivo)
	} else {
		sess = runtime.NewSession(*cfg)
	}
	defer func() {
		if loja != nil {
			_ = loja.TouchSession(idAtivo)
			_ = loja.Fechar()
		}
	}()
	br := io.Reader()
	cwd, _ := os.Getwd()
	fechar := make(chan struct{})
	sair := false
	ultimoContexto := 0 // prompt tokens do último request, para o % de contexto

	// ── subsistemas com dono de processo (MCP, sub-agentes) ───────────────
	// O ctx aqui é de VIDA DO PROCESSO: os sub-agentes precisam sobreviver ao
	// turno que os criou, e o handshake do MCP não pode morrer com um Ctrl+C
	// de rodada. Persistentes best-effort — falha de UM servidor vira aviso,
	// não derrubada do REPL; /model trocando provider reinjeta na próxima linha.
	ctxProcesso, cancelaProcesso := context.WithCancel(context.Background())
	defer cancelaProcesso()
	conjunto, avisosWiring := wiring.Montar(ctxProcesso, *cfg, sess, cwd)
	for _, aviso := range avisosWiring {
		avisoFmt(io.Out, aviso)
	}
	// Fechar ao sair: Fecha() do conjunto encerra subprocessos MCP; o gerente
	// de sub-agentes é in-memory e morre com o processo.
	defer func() { _ = conjunto.Fechar() }()

	for {
		// Encerramentos vindos de um turno (Ctrl+C duas vezes) saem ANTES de
		// desenhar prompt novo; o EOF fecha no meio da linha e pede a quebra.
		select {
		case <-fechar:
			despedida(io.Out)
			return
		default:
		}
		if sair {
			despedida(io.Out)
			return
		}
		if io.tela != nil {
			var mcps []string
			for _, c := range conjunto.MCP() {
				mcps = append(mcps, c.Fonte())
			}
			io.tela.Estado(estadoPainel{
				titulo: tituloAtual(loja, sess), modelo: cfg.Model,
				provider: cfg.CurrentProvider, effort: cfg.Effort, cwd: cwdCurto(cwd),
				contexto: ultimoContexto, janela: cfg.Janela, mcps: mcps,
			})
		} else if coresLigadas {
			statusEntrada(io.Out, cfg, sess.ID(), len(conjunto.MCP()), ultimoContexto)
			promptoTUI(io.Out)
		} else {
			statusbarSessao(io.Out, cfg, sess.ID())
			prompto(io.Out, cfg.Model, cfg.Effort)
		}
		linha, err := br.ReadString('\n')
		if err != nil { // EOF / Ctrl+D: sair limpo
			fmt.Fprintln(io.Out)
			if loja != nil {
				_ = loja.TouchSession(sess.ID())
			}
			despedida(io.Out)
			return
		}
		partes := strings.SplitN(strings.TrimSpace(linha), " ", 2)
		campos := strings.Fields(partes[0])
		arg := ""
		if len(partes) > 1 {
			arg = strings.TrimSpace(partes[1])
		}
		if len(campos) == 0 {
			continue
		}
		switch campos[0] {
		case "/exit", "/quit":
			if loja != nil {
				_ = loja.TouchSession(sess.ID())
			}
			despedida(io.Out)
			return
		case "/new":
			// A caixa faia do /new: limpar o transcript da sessão sem trocar
			// provider (CONFIG fica onde está). Pode ser usado após terminar algo
			// pra não carregar o contexto pro próximo pedido.
			sess.ClearHistory()
			ultimoContexto = 0
			if io.tela != nil {
				io.tela.Limpar()
			}
			okFmt(io.Out, "história limpa; conversa nova")
		case "/resume":
			if loja == nil {
				avisoFmt(io.Out, "sessão sem persistência: nada para retomar")
				continue
			}
			lista := listarSessoesMonta(loja, 10)
			if len(lista) == 0 {
				listarSessaoResumo(io.Out, nil)
				continue
			}
			listarSessaoResumo(io.Out, lista)
			fmt.Fprint(io.Out, verdof("❯")+" "+dimf("número ou id (enter cancela)")+" ")
			escolha, errLeitura := br.ReadString('\n')
			if errLeitura != nil { // EOF no meio: sai limpo como no loop
				fmt.Fprintln(io.Out)
				_ = loja.TouchSession(sess.ID())
				despedida(io.Out)
				return
			}
			escolhido := resolverSessao(escolha, lista)
			if escolhido == "" {
				avisoFmt(io.Out, "escolha vazia ou inválida; sessão atual mantida")
				continue
			}
			// Retomar = novo id ativo + transcript reconstruído do log.
			idAtivo = escolhido
			sess = runtime.NewSessionWithStore(*cfg, loja, idAtivo)
			if err := sess.Resume(); err != nil {
				erroFmt(io.ErrOut, "erro retomando sessão", err)
				continue
			}
			okFmt(io.Out, "sessão "+sess.ID()+" retomada")
		case "/save":
			if loja == nil {
				avisoFmt(io.Out, "sessão sem persistência: nada para salvar")
				continue
			}
			titulo := arg
			if titulo == "" {
				titulo = tituloPadrao(sess)
			}
			if err := loja.SetTitle(sess.ID(), titulo); err != nil {
				erroFmt(io.ErrOut, "erro salvando título", err)
				continue
			}
			okFmt(io.Out, "sessão "+sess.ID()+" salva — "+strings.TrimSpace(titulo))
		case "/rename":
			nome := strings.Join(strings.Fields(arg), " ")
			if nome == "" {
				avisoFmt(io.Out, "uso: /rename novo nome da sessão")
				continue
			}
			if loja == nil {
				avisoFmt(io.Out, "sessão sem persistência: não há onde gravar o nome")
				continue
			}
			if err := loja.SetTitle(sess.ID(), nome); err != nil {
				erroFmt(io.ErrOut, "erro renomeando", err)
				continue
			}
			okFmt(io.Out, "sessão renomeada: "+nome)
		case "/help":
			ajuda(io)
		case "/update":
			cmdUpdate(io)
		case "/contexto":
			cmdContexto(io.Out, cfg, loja)
		case "/skills":
			listarSkills(io.Out, runtime.SkillsDisponiveis(cwd))
		case "/tasks":
			cmdTasks(io.Out, conjunto.Gerente(), arg)
		case "/model":
			if arg != "" {
				// troca direta por id se ele existir no catálogo atual
				if err := trocarModeloDireto(io, cfg, arg); err != nil {
					erroFmt(io.ErrOut, "erro", err)
					continue
				}
			} else if err := selecionarModelo(io, cfg); err == errMantido {
				okFmt(io.Out, "modelo mantido: "+cfg.Model)
				continue
			} else if err != nil {
				erroFmt(io.ErrOut, "erro", err)
				continue
			} else {
				if err := cfg.Save(); err != nil {
					erroFmt(io.ErrOut, "erro salvando", err)
					continue
				}
				sess.Configure(*cfg)
			}
			statusbar(io.Out, cfg)
		case "/effort":
			procEffort(io, cfg, sess, arg)
		case "/provider":
			if err := cmdProvider(io, cfg); err != nil {
				erroFmt(io.ErrOut, "erro", err)
				continue
			}
			sess.Configure(*cfg)
			statusbar(io.Out, cfg)
		default:
			pedido := strings.TrimSpace(linha)
			if strings.HasPrefix(campos[0], "/") {
				// /<nome-de-skill> [args] executa a skill; qualquer outro / é
				// comando desconhecido.
				expandido, nome, ok := runtime.ExpandirSkill(cwd, pedido)
				if !ok {
					fmt.Fprintf(io.Out, "comando desconhecido: %s (/help lista; /skills lista as skills)\n", campos[0])
					continue
				}
				okFmt(io.Out, "skill "+nome)
				pedido = expandido
			}
			// turno do modelo: Ctrl+C abre mão da rodada corrente; um segundo
			// sinal (ou /exit) encerra o REPL.
			ctx, cancel := io.cancelarTurno(fechar)
			rend := rendererTurno(io.Out)
			var tui *turnoTUI
			var tt *turnoTela
			if io.tela != nil {
				io.tela.Usuario(strings.TrimSpace(linha))
				io.tela.Ocupado(true)
				tt = &turnoTela{t: io.tela, modelo: cfg.Model}
				rend = tt
			} else if coresLigadas {
				ecoPedido(io.Out, strings.TrimSpace(linha))
				tui = novoTurnoTUI(io.Out, cfg)
				rend = tui
			}
			errTurno := sess.Send(ctx, rend, cwd, pedido)
			if tt != nil {
				io.tela.pararSpin()
				io.tela.Ocupado(false)
				if tt.contexto > 0 {
					ultimoContexto = tt.contexto
				}
			}
			if tui != nil {
				tui.pararSpin()
				tui.mu.Lock()
				if tui.contexto > 0 {
					ultimoContexto = tui.contexto
				}
				tui.mu.Unlock()
			}
			cancel()
			if errTurno != nil {
				erroFmt(io.ErrOut, "erro", errTurno)
			}
			select {
			case <-fechar:
				sair = true
			default:
			}
		}
	}
}

func ajuda(io IO) {
	fmt.Fprint(io.Out, cianof("comandos")+`
  /model            lista o catálogo completo (alfabético) e troca o modelo
  /model <id>       troca direta quando o id está no catálogo
  /effort           mostra effort atual e níveis comuns
  /effort <nivel>   grava e entra na próxima requisição
  /effort -         limpa (request volta sem reasoning_effort)
  /provider         troca base URL + chave; valida /models antes de salvar
  /new              limpa a história da sessão (conversa nova)
  /resume           lista as últimas sessões e retoma uma
  /save [título]    dá título à sessão atual e grava o log
  /rename <nome>    renomeia a sessão atual (aba do terminal e painel)
  /update           baixa e instala a release mais nova do canal
  /contexto         política de compactação ativa e o que cada uma já fez
  /skills           lista as skills; /<nome> [args] executa uma
  /tasks            lista os sub-agentes; /tasks <id> detalha; /tasks cancel <id>
  /help             esta ajuda
  /exit, /quit      sair
`)
	fmt.Fprint(io.Out, dimf("texto livre vai direto ao modelo; Ctrl+C cancela a rodada em curso.")+"\n\n")
}

// trocarModeloDireto resolve /model <índice|id>: aceita número da lista
// alfabética ou o ID; se nenhum dos dois existir no catálogo, erro acionável
// e nada muda.
func trocarModeloDireto(io IO, cfg *config.Config, entrada string) error {
	p := cfg.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := buscarModelos(ctx, base, key)
	if err != nil {
		return err
	}
	id := ""
	if n, errA := strconv.Atoi(entrada); errA == nil && n >= 1 && n <= len(ls) {
		id = ls[n-1].ID
	} else if catalogoTem(entrada, ls) {
		id = entrada
	}
	if id == "" {
		return fmt.Errorf("%q não está no catálogo do provider; use /model sem argumento para listar", entrada)
	}
	copia := *cfg
	copia.Model = id
	if j := janelaDe(id, ls); j > 0 {
		copia = aplicarJanela(copia, j)
	} else {
		copia = limparJanelaCatalogo(copia)
	}
	if err := copia.Save(); err != nil {
		return err
	}
	*cfg = copia
	return nil
}

// procEffort processa /effort [nivel].
func procEffort(io IO, cfg *config.Config, sess *runtime.Session, arg string) {
	atual := "(desativado)"
	if cfg.Effort != "" {
		atual = cfg.Effort
	}
	if arg == "" {
		fmt.Fprintf(io.Out, "effort: %s · níveis: %s · `/effort -` limpa\n", atual, strings.Join(Esforcos, ", "))
		return
	}
	copia := *cfg
	if arg == "-" || arg == "off" || arg == "clear" {
		copia.Effort = ""
	} else {
		copia.Effort = arg
	}
	if err := copia.Save(); err != nil {
		fmt.Fprintf(io.ErrOut, "erro salvando: %v (effort atual continua %s)\n", err, atual)
		return
	}
	*cfg = copia
	sess.Configure(*cfg)
	if cfg.Effort == "" {
		fmt.Fprintln(io.Out, "effort limpo; a próxima requisição sai SEM reasoning_effort.")
	} else {
		fmt.Fprintf(io.Out, "effort=%s; entra na próxima requisição (provider recusa? `/effort -` limpa).\n", cfg.Effort)
	}
}

// cmdProvider pergunta base URL e chave (oculta), valida /models e persiste.
// Em falha de validação o erro sobe: nada de memória nem disco é alterado.
func cmdProvider(io IO, cfg *config.Config) error {
	base := config.DefaultBaseURL
	if p := cfg.Ativo(); p != nil && p.BaseURL != "" {
		base = p.BaseURL
	}
	if v := perguntar(io, "base URL", base); v != "" && v != base {
		base = v
	}
	fmt.Fprintln(io.Out, "enter mantém a chave atual; input da nova é oculto.")
	nova := ""
	if v, err := PromptKey("api key: "); err == nil && strings.TrimSpace(v) != "" {
		nova = strings.TrimSpace(v)
	}
	key := nova
	if key == "" {
		if p := cfg.Ativo(); p != nil {
			key = p.APIKey
		}
	}
	if key == "" {
		return fmt.Errorf("chave obrigatória para validar /models")
	}

	copia := *cfg
	err := configurarProvider(io, &copia, base, key)
	// configurarProvider devolve ForaDoCatalogo quando o model guardado não
	// existe no catálogo novo: o provider já foi aplicado na cópia, mas a
	// escolha de model deve acontecer ANTES de salvar.
	var fc *ForaDoCatalogo
	if err != nil && !asForaCatalogo(err, &fc) {
		return err
	}
	if copia.Model == "" || fc != nil {
		fmt.Fprintln(io.Out, "provider novo não tem o model atual — escolha outro:")
		if err := selecionarModelo(io, &copia); err != nil {
			return fmt.Errorf("provider validado; corra /provider de novo para a escolha de model (%w)", err)
		}
	}
	if err := copia.Save(); err != nil {
		return err
	}
	*cfg = copia
	return nil
}

func asForaCatalogo(err error, destino **ForaDoCatalogo) bool {
	if e, ok := err.(*ForaDoCatalogo); ok {
		*destino = e
		return true
	}
	return false
}
