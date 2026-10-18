// Package subagent implementa o runner de tarefas irmãs: sub-agentes autônomos
// que rodam em segundo plano com TODAS as ferramentas built-in, EXCETO
// delegação, e um registro em memória das tarefas exposto ao agente pai por
// três tools (spawn/status/result).
//
// A invariante "filho não delega" vale POR CONSTRUÇÃO, não por filtro:
// runtime.Session.Send monta a lista de ferramentas de cada turno chamando
// tools.SpecsBuiltins() (session.go: `lista := tools.SpecsBuiltins()`), e as
// specs deste pacote são entregues SOMENTE à lista do pai pelo wiring do
// caller. Nenhum caminho de código injeta <prefixo>_spawn numa sessão filha —
// o sub-agente nasce sem ela —, e o prompt da tarefa é passado ao filho TAL
// QUAL, sem wrapper: quem quiser instrução extra escreve no prompt.
//
// Persistência FORA DE ESCOPO: o registro vive só em memória e as tarefas
// morrem com o processo. O slice de sessões pluga o store depois; Tarefa já
// carrega ID determinístico e ciclo de vida explícito, que é tudo de que um
// registro persistido precisa.
//
// TEMPO DE VIDA: Spawn deriva o contexto da sessão filha do ctx informado
// (context.WithCancel), então cancelar o pai cancela a tarefa. O REPL cria um
// ctx POR TURNO e o cancela no fim da linha — passar esse ctx enterraria a
// tarefa assim que o turno acabasse. Para tarefas que sobrevivem ao turno, o
// wiring deve usar um ctx de vida longa (ex.: g.Vincular(context.Background())
// uma vez, e Spawn com ctx desse nível); Cancelar continua sendo a parada por
// tarefa. Ver Vincular.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

const (
	// PrefixoPadrao é o prefixo das tools quando Config.Nome vem vazio:
	// task_spawn / task_status / task_result.
	PrefixoPadrao = "task"

	// MaxPadrao é o teto de sub-agentes rodando ao mesmo tempo.
	MaxPadrao = 3

	// TamanhoFila limita a profundidade da fila de queued: além disso o Spawn
	// falha com erro explícito em vez de crescer sem limite.
	TamanhoFila = 256

	// TetoTranscript é o teto de linhas do rastro de tools por tarefa.
	TetoTranscript = 200
)

// Estados de uma tarefa.
const (
	Queued    = "queued"
	Running   = "running"
	Completed = "completed"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Config é a configuração do gerente. Nome é o prefixo das tools expostas ao
// pai ("task" → task_spawn/task_status/task_result); MaxConcorrentes é o teto
// de sub-agentes rodando ao mesmo tempo (<=0 → MaxPadrao).
type Config struct {
	Nome            string
	MaxConcorrentes int
}

// Tarefa é uma tarefa irmã: o que foi pedido, em que estado está e o que o
// sub-agente produziu. Resultado é o texto FINAL do filho (o que <prefixo>_result
// devolve); Erro é UMA linha de falha; o rastro de tools fica em transcript
// (teto TetoTranscript linhas).
//
// Os campos de ciclo de vida são escritos pelo worker e lidos por qualquer um,
// sempre sob Gerente.mu — a tarefa NÃO tem lock próprio, e é por isso que pode
// ser copiada (snapshot, testes) sem acionar o vet.
type Tarefa struct {
	ID          string
	Prompt      string
	Estado      string
	Criada      time.Time
	Inicada     time.Time
	TerminadaEm time.Time
	Resultado   string
	Erro        string

	cancel     context.CancelFunc
	ctx        context.Context // sessão filha; nasce junto com o par (ctx, cancel)
	transcript []string
}

// Transcricao devolve uma cópia do rastro de tools (observabilidade/testes).
func (t *Tarefa) Transcricao() []string {
	return append([]string(nil), t.transcript...)
}

// coletor é o runtime.Renderer de um sub-agente: não desenha nada, apenas COLHE
// a saída para dentro da Tarefa. Resposta acumula o texto final em Resultado;
// Tool registra uma linha no rastro; Eco/Guardrail/Tokens são no-op — quem só
// quer o resultado não precisa de eco nem de rodapé de tokens.
//
// Ele segura o lock do gerente só para escrever na tarefa (nunca atravessa a
// chamada de rede), e é o ÚNICO escritor de Resultado/transcript enquanto a
// tarefa roda.
type coletor struct {
	g *Gerente
	t *Tarefa
}

func (c *coletor) Resposta(texto string) {
	if texto == "" {
		return
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	c.t.anexarResultado(texto)
}

func (c *coletor) Tool(nome string, _ time.Duration, erro bool) {
	linha := "[tool " + nome + "]"
	if erro {
		linha = "[tool " + nome + " erro]"
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	c.t.anexarTranscript(linha)
}

func (c *coletor) Eco(string)                           {}
func (c *coletor) Guardrail(bool)                       {}
func (c *coletor) Tokens(int, int, time.Duration, bool) {}

// Gerente registra e executa as tarefas irmãs. Thread-safe: o chamador cria e
// consulta tarefas enquanto os workers rodam em segundo plano.
type Gerente struct {
	nome string
	max  int
	cwd  string

	mu      sync.Mutex
	tarefas map[string]*Tarefa
	ordem   []string
	seq     int
	rodando int

	cfgFilho config.Config   // config herdada por TODAS as tarefas (provider/modelo/chave)
	pai      context.Context // contexto de vida longa; ver Vincular

	fila        chan string   // ids queued aguardando vaga
	despachando bool          // dispatcher já subiu (goroutine única)
	vagas       chan struct{} // tokens de concorrência: capacidade = max
}

// Novo cria um gerente de tarefas.
func Novo(cfg Config) *Gerente {
	nome := strings.TrimSpace(cfg.Nome)
	if nome == "" {
		nome = PrefixoPadrao
	}
	max := cfg.MaxConcorrentes
	if max <= 0 {
		max = MaxPadrao
	}
	cwd, _ := os.Getwd()
	return &Gerente{
		nome:    nome,
		max:     max,
		cwd:     cwd,
		tarefas: map[string]*Tarefa{},
		fila:    make(chan string, TamanhoFila),
		vagas:   make(chan struct{}, max),
		pai:     context.Background(),
	}
}

// Prefixo devolve o prefixo das tools deste gerente.
func (g *Gerente) Prefixo() string { return g.nome }

// Teto devolve o máximo de sub-agentes concorrentes.
func (g *Gerente) Teto() int { return g.max }

// Cwd devolve o diretório em que as tarefas rodam por padrão.
func (g *Gerente) Cwd() string { return g.cwd }

// DefinirCwd troca o diretório de execução das tarefas futuras.
func (g *Gerente) DefinirCwd(cwd string) { g.mu.Lock(); g.cwd = cwd; g.mu.Unlock() }

// ConfigPai guarda a configuração que toda tarefa filha herda (mesmo
// provider/modelo/chave do pai). Chamada pelo wiring — normalmente por
// Especificacoes, que já recebe a config do pai.
func (g *Gerente) ConfigPai(cfg config.Config) {
	g.mu.Lock()
	g.cfgFilho = cfg
	g.mu.Unlock()
}

// Vincular troca o contexto-pai usado quando Spawn receber ctx nil. O REPL
// chama uma vez com context.Background() para que as tarefas irmãs sobrevivam
// ao turno que as criou (o ctx por turno é cancelado no fim da linha).
func (g *Gerente) Vincular(pai context.Context) {
	if pai == nil {
		pai = context.Background()
	}
	g.mu.Lock()
	g.pai = pai
	g.mu.Unlock()
}

// Tarefas devolve snapshot das tarefas na ordem de criação (o mapa interno
// nunca escapa).
func (g *Gerente) Tarefas() []*Tarefa {
	g.mu.Lock()
	out := make([]*Tarefa, 0, len(g.ordem))
	for _, id := range g.ordem {
		if t := g.tarefas[id]; t != nil {
			out = append(out, t)
		}
	}
	g.mu.Unlock()
	return out
}

// Fotos devolve CÓPIAS de todas as tarefas, na ordem de criação, lidas sob o
// mesmo lock que os workers usam para escrever. É o que a UI deve usar: os
// ponteiros de Tarefas são vivos, e ler Estado/Resultado deles enquanto um
// worker escreve é corrida de dados.
func (g *Gerente) Fotos() []Tarefa {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Tarefa, 0, len(g.ordem))
	for _, id := range g.ordem {
		t := g.tarefas[id]
		if t == nil {
			continue
		}
		cp := *t
		cp.transcript = append([]string(nil), t.transcript...)
		cp.cancel, cp.ctx = nil, nil
		out = append(out, cp)
	}
	return out
}

// snapshot devolve uma cópia consistente de uma tarefa (a leitura inteira
// acontece sob o mesmo lock que os workers usam para escrever).
func (g *Gerente) snapshot(id string) (Tarefa, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tarefas[id]
	if !ok || t == nil {
		return Tarefa{}, false
	}
	cp := *t
	cp.transcript = append([]string(nil), t.transcript...)
	return cp, true
}

// finalizar fecha o ciclo de vida de uma tarefa (chamado sob o lock do
// gerente). erro vazio ganha um texto padrão por estado, para que
// <prefixo>_result nunca devolva estado sem explicação.
func (t *Tarefa) finalizar(estado, erro string) {
	t.Estado = estado
	if erro == "" {
		switch estado {
		case Cancelled:
			erro = "cancelada"
		}
	}
	if erro != "" {
		t.Erro = erro
	}
	t.TerminadaEm = time.Now()
	t.cancel = nil
	t.ctx = nil
}

// iniciarMarca que a tarefa saiu da fila e começou a rodar.
func (t *Tarefa) iniciarMarca() {
	t.Estado = Running
	t.Inicada = time.Now()
}

// anexarResultado soma o texto do modelo ao resultado final.
func (t *Tarefa) anexarResultado(texto string) {
	if t.Resultado != "" {
		t.Resultado += "\n"
	}
	t.Resultado += texto
}

// anexarTranscript registra uma tool executada, respeitando o teto de linhas.
func (t *Tarefa) anexarTranscript(linha string) {
	if len(t.transcript) >= TetoTranscript {
		return
	}
	t.transcript = append(t.transcript, linha)
}

// Spawn cria uma tarefa e a entrega ao runner. O prompt vai ao filho TAL QUAL.
// O ctx informado (ou o vinculado, se vier nil) gera o contexto da sessão
// filha: cancelá-lo cancela a tarefa. Acima do teto de concorrência a tarefa
// fica queued e o dispatcher a promove quando uma vaga abrir — Spawn nunca
// bloqueia. Retorna o id determinístico (t1, t2, …).
func (g *Gerente) Spawn(ctx context.Context, pedido string) (string, error) {
	if strings.TrimSpace(pedido) == "" {
		return "", fmt.Errorf("prompt vazio")
	}
	g.mu.Lock()
	if ctx == nil {
		ctx = g.pai
	}
	cfg := g.cfgFilho
	cwd := g.cwd
	g.seq++
	t := &Tarefa{ID: fmt.Sprintf("t%d", g.seq), Prompt: pedido, Estado: Queued, Criada: time.Now()}
	g.tarefas[t.ID] = t
	g.ordem = append(g.ordem, t.ID)
	id := t.ID
	g.mu.Unlock()

	if ctx.Err() != nil {
		// Pai já morto: a tarefa nasce cancelada em vez de fingir que roda.
		g.mu.Lock()
		t.finalizar(Cancelled, "contexto do pai já estava encerrado")
		g.mu.Unlock()
		return id, nil
	}
	if !cfg.Pronta() {
		g.mu.Lock()
		t.finalizar(Failed, "config incompleta: "+strings.Join(cfg.Faltas(), ", "))
		g.mu.Unlock()
		return id, nil
	}

	// O par (ctx, cancel) da sessão filha nasce JÁ aqui, derivado do ctx do
	// pai — e não dentro do worker. Criá-lo sob o MESMO lock que marca running
	// fecha a janela em que um Cancelar veria a tarefa rodando com cancel nil
	// e recusaria o cancelamento.
	g.mu.Lock()
	filhoCtx, cancel := context.WithCancel(ctx)
	t.ctx = filhoCtx
	t.cancel = cancel
	cfg, cwd = g.cfgFilho, g.cwd
	g.mu.Unlock()

	// Vaga livre entra já; sem vaga vai para a fila e o dispatcher cuida.
	select {
	case g.vagas <- struct{}{}:
		g.mu.Lock()
		t.iniciarMarca()
		g.rodando++
		g.mu.Unlock()
		go g.executar(ctx, t, cfg, cwd)
		return id, nil
	default:
	}

	select {
	case g.fila <- id:
		g.subirDispatcher()
		return id, nil
	default:
		// Fila cheia: falha explícita em vez de sumir com o pedido. O ctx
		// derivado é liberado aqui para não vazar o watch do pai.
		cancel()
		g.mu.Lock()
		t.finalizar(Failed, "fila de tarefas cheia")
		g.mu.Unlock()
		return id, nil
	}
}

// subirDispatcher sobe a goroutine única de promoção queued→running (lazy).
func (g *Gerente) subirDispatcher() {
	g.mu.Lock()
	if g.despachando {
		g.mu.Unlock()
		return
	}
	g.despachando = true
	g.mu.Unlock()
	go g.despachar()
}

// despachar espera ids na fila e inicia cada um assim que houver vaga. A
// aquisição da vaga é bloqueante de propósito: é ela que segura a fila quando
// o teto estoura, preservando a ordem FIFO sem busy-wait. A fila nunca é
// fechada, então a goroutine dura a vida do gerente.
//
// O ctx usado é o da sessão filha (criado em Spawn, derivado do ctx do pai):
// cancelar o pai continua derrubando a fila, e Cancelar(id) segue valendo para
// quem ainda não começou.
func (g *Gerente) despachar() {
	for id := range g.fila {
		g.vagas <- struct{}{} // espera uma vaga abrir
		g.mu.Lock()
		t := g.tarefas[id]
		if t == nil {
			g.mu.Unlock()
			<-g.vagas
			continue
		}
		if t.Estado != Queued { // cancelada enquanto esperava na fila
			g.mu.Unlock()
			<-g.vagas
			// O par (ctx, cancel) nunca foi usado; libera o watch do pai.
			if t.cancel != nil {
				t.cancel()
			}
			continue
		}
		ctx, cfg, cwd := t.ctx, g.cfgFilho, g.cwd
		t.iniciarMarca()
		g.rodando++
		g.mu.Unlock()
		go g.executar(ctx, t, cfg, cwd)
	}
}

// executar roda a sessão filha: mesmas ferramentas built-in do pai, sem as de
// delegação (ver doc do pacote), transcript zerado e saída colhida pelo
// coletor. Ao terminar (ou falhar, ou ser cancelada) devolve a vaga.
//
// O par (ctx, cancel) da sessão filha já foi criado em Spawn; aqui ele é usado
// e devolvido.
func (g *Gerente) executar(ctx context.Context, t *Tarefa, cfg config.Config, cwd string) {
	defer func() {
		g.mu.Lock()
		if g.rodando > 0 {
			g.rodando--
		}
		g.mu.Unlock()
		<-g.vagas
	}()

	g.mu.Lock()
	filhoCtx, cancel := t.ctx, t.cancel
	g.mu.Unlock()
	if filhoCtx == nil {
		// Sem sessão filha (estado inesperado): falha em vez de entrar em
		// pânico, e devolve a vaga pelo defer acima.
		g.mu.Lock()
		t.finalizar(Failed, "sessão filha não inicializada")
		g.mu.Unlock()
		return
	}
	defer cancel()

	// TRANSCRIPT PRÓPRIO, não há herança: NewSession já nasce com history
	// vazia; ClearHistory é só o reforço explícito da invariante (o sub-agente
	// não enxerga o histórico do pai, nem o pai o do filho).
	sess := runtime.NewSession(cfg)
	sess.ClearHistory()

	// Sessão filha = tools.SpecsBuiltins() SOMENTE (session.go monta a lista
	// internamente). Nenhuma spec de delegação chega aqui: ver doc do pacote.
	err := sess.Send(filhoCtx, &coletor{g: g, t: t}, cwd, t.Prompt)

	g.mu.Lock()
	switch {
	case err == nil:
		t.finalizar(Completed, "")
	case filhoCtx.Err() != nil || ctx.Err() != nil || errors.Is(err, context.Canceled):
		t.finalizar(Cancelled, "")
	default:
		t.finalizar(Failed, umaLinha(err))
	}
	g.mu.Unlock()
}

// Cancelar encerra uma tarefa. Queued vira cancelled na hora (o dispatcher a
// ignora quando for promovê-la); running tem o cancel() da sessão chamado e o
// worker a marca cancelled. Devolve true só quando havia o que cancelar.
func (g *Gerente) Cancelar(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tarefas[id]
	if t == nil {
		return false
	}
	switch t.Estado {
	case Queued:
		t.finalizar(Cancelled, "cancelada antes de iniciar")
		return true
	case Running:
		if t.cancel != nil {
			t.cancel() // o worker escreve cancelled ao desenrolar o cancel
			return true
		}
		return false
	default:
		return false
	}
}

// umaLinha colapsa um erro em UMA linha curta (o campo Erro é de leitura rápida
// e vai direto para a tool <prefixo>_result).
func umaLinha(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
