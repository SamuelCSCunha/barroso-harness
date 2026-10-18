// Agente ACP v1 (Agent Client Protocol) do Barroso Harness, falado por stdio
// em JSON-RPC newline-delimited. O pacote é servidor: um CLIENTE (editor,
// Zed, CLI própria) lança o processo e dirige os turnos.
//
// Superfície implementada, e só ela — cada método existe porque um cliente ACP
// real o envia, e a ausência dos outros é decisão, não esquecimento:
//
//	initialize     → protocolVersion ecoado (o que o cliente mandou, se >0)
//	session/new    → cria sessão com cwd; id "s-<8hex>"
//	session/prompt → turno; notificações session/update durante, resposta no fim
//	session/cancel → cancela o turno em voo daquela sessão
//
// O resto do protocolo (session/load, session/list, session/resume,
// session/set_config_option, authenticate, request_permission) ficou fora de
// propósito: o harness ainda não persiste sessão, não tem dois modelos para
// trocar em runtime e roda em YOLO — não há o que aprovar. Acrescentar método
// novo é barato; fingir capacidade que o runtime não tem não é.
//
// Concorrência: o laço Handle lê sequencialmente, mas session/prompt despacha
// o turno num goroutine — é o que permite session/cancel chegar no meio. Toda
// escrita no fio passa por escMu, senão duas linhas JSON se misturariam.
package acp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// Turno é a costura testável do runtime: a única coisa que o agente precisa de
// uma sessão é rodar um turno. O runtime.Session satisfaz a interface sem
// adaptador (a assinatura é a mesma), e os testes injetam um falso que emite
// eventos determinísticos e obedece ao ctx.
type Turno interface {
	Send(ctx context.Context, r runtime.Renderer, cwd string, pedido string) error
}

// Metodos ACP atendidos. Exported para quem quiser rotear antes de Handle.
const (
	MetInitialize  = "initialize"
	MetSessionNovo = "session/new"
	MetPrompt      = "session/prompt"
	MetCancel      = "session/cancel"
)

// sessao guarda o turno de um cliente ACP e o cancelamento do turno em voo.
type sessao struct {
	id     string
	cwd    string
	turno  Turno
	cancel context.CancelFunc // nil enquanto nenhum turno corre
}

// Agente é o servidor ACP. Nasce com a config capturada em Novo (determinismo:
// session/new NÃO relê o disco, então os testes não dependem de ~/.barroso-harness)
// e uma fábrica de turnos — a de produção embrulha runtime.NewSession, a de
// testes devolve um falso.
type Agente struct {
	cfg     config.Config
	log     func(string) // opcional; nil = silencioso
	Fabrica func(config.Config) (Turno, string)

	mu      sync.Mutex
	sessoes map[string]*sessao

	escMu   sync.Mutex // serializa escritas no fio
	seq     int64      // ids de tool call únicos no processo
	rodando sync.WaitGroup
}

// Novo cria o agente sobre cfg, com a fábrica de produção.
func Novo(cfg config.Config) *Agente {
	a := &Agente{
		cfg:     cfg,
		sessoes: map[string]*sessao{},
		Fabrica: turnoDeRuntime,
	}
	return a
}

// NovoComFabrica cria o agente com outra fábrica de turnos (testes, harness
// embutida, motor alternativo).
func NovoComFabrica(cfg config.Config, fabrica func(config.Config) (Turno, string)) *Agente {
	a := Novo(cfg)
	if fabrica != nil {
		a.Fabrica = fabrica
	}
	return a
}

// RegistrarLog liga o log de diagnóstico (nil desliga).
func (a *Agente) RegistrarLog(f func(string)) { a.log = f }

// ComLog é a forma fluente de RegistrarLog.
func (a *Agente) ComLog(f func(string)) *Agente { a.RegistrarLog(f); return a }

func (a *Agente) logf(format string, args ...any) {
	if a.log == nil {
		return
	}
	a.log(fmt.Sprintf(format, args...))
}

// turnoDeRuntime é a fábrica de produção: uma runtime.Session por sessão ACP,
// com id novo. O par (Turno, id) deixa o teste escolher o id também.
func turnoDeRuntime(cfg config.Config) (Turno, string) {
	return runtime.NewSession(cfg), novaSessaoID()
}

// novaSessaoID devolve "s-" + 8 hexadecimais (32 bits de aleatoriedade).
func novaSessaoID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand do kernel é confiável nas plataformas alvo; se ainda assim
		// falhar, o relógio + contador mantêm unicidade sem pânico.
		binary.LittleEndian.PutUint32(b[:], uint32(time.Now().UnixNano()))
	}
	return "s-" + hex.EncodeToString(b[:])
}

// Handle serve o protocolo até o fim do reader, até o ctx ser cancelado ou até
// uma escrita falhar. EOF do cliente é encerramento LIMPO: devolve nil.
//
// Linha malformada não mata o laço (é logada e ignorada) — um cliente que
// mandou lixo numa linha não deve derrubar o agente inteiro. Só uma linha
// acima do teto de 8 MiB (ErrTooLong do scanner) encerra.
func (a *Agente) Handle(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sc := NovoScanner(r)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, ok, err := DecodificarLinha(sc.Bytes())
		if err != nil {
			// JSON inválido: não sabemos o id, então não há a quem responder.
			a.logf("acp: linha malformada ignorada: %v", err)
			continue
		}
		if !ok {
			continue
		}
		if err := a.despachar(ctx, w, req); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return ctx.Err()
}

// despacha um request. Notificação do cliente (sem id) é ignorada — o agente
// não define notificações de entrada neste recorte.
func (a *Agente) despachar(ctx context.Context, w io.Writer, req Request) error {
	if !req.TemID() {
		a.logf("acp: notificação do cliente ignorada: %s", req.Method)
		return nil
	}
	switch req.Method {
	case MetInitialize:
		return a.initialize(w, req)
	case MetSessionNovo:
		return a.novaSessao(w, req)
	case MetPrompt:
		return a.prompt(ctx, w, req)
	case MetCancel:
		return a.cancel(w, req)
	default:
		a.logf("acp: método desconhecido: %s", req.Method)
		return WriteError(w, req.ID, CodMetodo, "método não encontrado: "+req.Method)
	}
}

// ── initialize ─────────────────────────────────────────────────────────────

func (a *Agente) initialize(w io.Writer, req Request) error {
	// Params ausente é tolerável: cliente pode mandar {} ou nada. O que não se
	// tolera é protocolVersion irreconhecível — aí segue o default 1.
	var p struct {
		ProtocolVersion    any            `json:"protocolVersion"`
		ClientCapabilities map[string]any `json:"clientCapabilities"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "params inválidos: "+err.Error())
		}
	}
	v := 1
	if f, ok := p.ProtocolVersion.(float64); ok && int(f) > 0 {
		v = int(f)
	}
	return a.responder(w, NovaResposta(req.ID, map[string]any{
		"protocolVersion": v,
		"agentCapabilities": map[string]any{
			"loadSession": false,
		},
		"authMethods": []any{},
	}))
}

// ── session/new ────────────────────────────────────────────────────────────

func (a *Agente) novaSessao(w io.Writer, req Request) error {
	var p struct {
		CWD string `json:"cwd"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "params inválidos: "+err.Error())
		}
	}
	cwd := p.CWD
	if cwd == "" {
		// cwd vazio não é sessão inválida: o cliente pode não saber onde está.
		// Assume o diretório do processo — pior caso, o turno erra por conta
		// das tools e o cliente corrige com outra sessão.
		if d, err := os.Getwd(); err == nil {
			cwd = d
		} else {
			cwd = "."
		}
	}
	turno, id := a.Fabrica(a.cfg)
	a.mu.Lock()
	// Colisão de id é improvável (32 bits), mas um id repetido sobrescreveria
	// uma sessão viva; girar até achar livre é barato.
	for _, existe := a.sessoes[id]; existe; {
		id = novaSessaoID()
		_, existe = a.sessoes[id]
	}
	a.sessoes[id] = &sessao{id: id, cwd: cwd, turno: turno}
	a.mu.Unlock()
	a.logf("acp: sessão %s criada em %s", id, cwd)
	return a.responder(w, NovaResposta(req.ID, map[string]any{"sessionId": id}))
}

// ── session/prompt ─────────────────────────────────────────────────────────

func (a *Agente) prompt(ctx context.Context, w io.Writer, req Request) error {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "params inválidos: "+err.Error())
		}
	}
	if p.SessionID == "" || len(p.Prompt) == 0 {
		return WriteError(w, req.ID, CodParams, "params inválidos: sessionId e prompt (ao menos um bloco) são obrigatórios")
	}
	s, ok := a.sessao(p.SessionID)
	if !ok {
		return WriteError(w, req.ID, CodErroAgente, "sessão desconhecida: "+p.SessionID)
	}

	// Texto do prompt = concatenação dos blocos type=text. Blocos de outro
	// tipo (imagem, áudio, recurso) não entram no pedido: o harness é textual
	// neste recorte, e silenciar seria pior que ignorar — o cliente vê o texto
	// que sobrou.
	pedido := ""
	for _, b := range p.Prompt {
		if b.Type != "" && b.Type != "text" {
			continue
		}
		pedido += b.Text
	}
	if pedido == "" {
		return WriteError(w, req.ID, CodParams, "params inválidos: prompt sem bloco de texto")
	}

	a.mu.Lock()
	if s.cancel != nil {
		a.mu.Unlock()
		return WriteError(w, req.ID, CodTurnoRodando, "turno já em andamento nesta sessão")
	}
	// ctx do turno morre por session/cancel OU pelo ctx do Handle, o que
	// vier primeiro.
	tctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	a.mu.Unlock()

	rend := &acpRenderer{a: a, w: w, sessao: s.id}
	a.rodando.Add(1)
	go func() {
		defer a.rodando.Done()
		err := s.turno.Send(tctx, rend, s.cwd, pedido)
		a.mu.Lock()
		s.cancel = nil
		a.mu.Unlock()
		cancel()
		if err != nil {
			a.responder(w, NovoErro(req.ID, CodErroAgente, mensagemErro(err)))
			a.logf("acp: turno da sessão %s falhou: %v", s.id, err)
			return
		}
		a.responder(w, NovaResposta(req.ID, map[string]any{}))
		a.logf("acp: turno da sessão %s concluído", s.id)
	}()
	return nil
}

// mensagemErro troca o seco "context canceled" do Go pela frase que o cliente
// ACP entende: quem cancelou foi ele.
func mensagemErro(err error) string {
	if errors.Is(err, context.Canceled) {
		return "turno cancelado pelo cliente (session/cancel)"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "turno expirou (prazo esgotado)"
	}
	return err.Error()
}

// ── session/cancel ─────────────────────────────────────────────────────────

func (a *Agente) cancel(w io.Writer, req Request) error {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "params inválidos: "+err.Error())
		}
	}
	if p.SessionID == "" {
		return WriteError(w, req.ID, CodParams, "params inválidos: sessionId é obrigatório")
	}
	s, ok := a.sessao(p.SessionID)
	if !ok {
		return WriteError(w, req.ID, CodErroAgente, "sessão desconhecida: "+p.SessionID)
	}
	a.mu.Lock()
	c := s.cancel
	a.mu.Unlock()
	if c != nil {
		c()
		a.logf("acp: cancelamento pedido para a sessão %s", s.id)
	} else {
		a.logf("acp: cancelamento sem turno em voo na sessão %s", s.id)
	}
	return a.responder(w, NovaResposta(req.ID, map[string]any{}))
}

// sessao busca uma sessão existente.
func (a *Agente) sessao(id string) (*sessao, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessoes[id]
	return s, ok
}

// responder escreve uma resposta serializada com as demais escritas.
func (a *Agente) responder(w io.Writer, r Response) error {
	a.escMu.Lock()
	defer a.escMu.Unlock()
	return EscreverLinha(w, r)
}

// notificar escreve uma notificação de mão única (session/update).
func (a *Agente) notificar(w io.Writer, n Notification) error {
	a.escMu.Lock()
	defer a.escMu.Unlock()
	return EscreverLinha(w, n)
}

// proximoID dá ids de tool call únicos no processo (clientes indexam por eles).
func (a *Agente) proximoID() string {
	return fmt.Sprintf("tool-%d", atomic.AddInt64(&a.seq, 1))
}

// Esperar espera os turnos em voo. Serve ao host que quer encerrar limpo:
// Sirve a chama antes de fechar os descritores, para nenhuma resposta ficar
// pela metade. Turnos pendurados em I/O de rede só saem com ctx cancelado.
func (a *Agente) Esperar() { a.rodando.Wait() }

// ── acpRenderer ────────────────────────────────────────────────────────────

// acpRenderer traduz o Renderer do runtime para notificações ACP. Regra de
// tradução, escolhida para o cliente não perder informação sem inventar
// eventos que o protocolo não tem:
//
//	Resposta  → agent_message_chunk (o texto do modelo; runtime entrega inteiro)
//	Tool      → tool_call, kind "other", status final (completed/failed)
//	Eco       → tool_call_update do último call, com a saída como conteúdo
//	Guardrail → agent_message_chunk com o aviso de compactação
//	Tokens    → nada: rodapé de consumo é assunto de UI local, não de protocolo
type acpRenderer struct {
	a      *Agente
	w      io.Writer
	sessao string

	mu     sync.Mutex
	ultimo string // toolCallId do último tool_call, para o Eco se anexar
}

func (r *acpRenderer) Resposta(texto string) {
	// O conteúdo do chunk é um OBJETO de bloco único ({type,text}), não dois
	// campos soltos no update: é o formato que o cliente ACP valida.
	r.emitir("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": texto},
	})
}

func (r *acpRenderer) Tool(nome string, dur time.Duration, erro bool) {
	estado := "completed"
	if erro {
		estado = "failed"
	}
	id := r.a.proximoID()
	r.mu.Lock()
	r.ultimo = id
	r.mu.Unlock()
	// Uma única notificação com o estado FINAL: o runtime só informa a tool
	// depois de executada (nome + duração + falha juntos), então um par
	// in_progress/completed seria teatro — o cliente receberia dois eventos
	// para algo que já acabou.
	r.emitir("tool_call", map[string]any{
		"toolCallId": id,
		"title":      nome,
		"kind":       "other",
		"status":     estado,
	})
}

func (r *acpRenderer) Eco(saida string) {
	if saida == "" {
		return
	}
	r.mu.Lock()
	id := r.ultimo
	r.mu.Unlock()
	if id == "" {
		return // eco solto, sem tool call a quem se anexar
	}
	r.emitir("tool_call_update", map[string]any{
		"toolCallId": id,
		"content":    []map[string]any{{"type": "text", "text": saida}},
	})
}

func (r *acpRenderer) Guardrail(proativa bool) {
	texto := "[guardrail] compactação reativa: o provider recusou por contexto"
	if proativa {
		texto = "[guardrail] compactação proativa por previsão de estouro"
	}
	r.emitir("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": texto},
	})
}

// Tokens é deliberadamente silencioso: ver o comentário do tipo.
func (r *acpRenderer) Tokens(int, int, time.Duration, bool) {}

// emitir emite uma session/update. Falha de escrita é só logada: quem trata o
// fio é o laço Handle, e uma tool não pode abortar o turno porque o cliente
// fechou a leitura — o turno acabou, a resposta vai falhar sozinha.
func (r *acpRenderer) emitir(sessionUpdate string, update map[string]any) {
	update["sessionUpdate"] = sessionUpdate
	n := NovaNotificacao("session/update", map[string]any{
		"sessionId": r.sessao,
		"update":    update,
	})
	// Campos ausentes têm de sair omitidos, não null: cliente ACP valida o
	// formato do update.
	if err := r.a.notificar(r.w, n); err != nil {
		r.a.logf("acp: falha ao emitir %s: %v", sessionUpdate, err)
	}
}
