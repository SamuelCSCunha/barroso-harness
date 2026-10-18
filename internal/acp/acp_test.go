package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// ── infra de teste ─────────────────────────────────────────────────────────

// pipe é um writer sincronizado: o teste escreve requests, o agente escreve as
// respostas, e o teste lê o que já foi escrito. Diferente de io.Pipe — que
// bloquearia o leitor à espera do escritor e faria o teste depender de
// agendamento de goroutines.
type pipe struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (p *pipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Write(b)
}

func (p *pipe) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// linha é uma mensagem JSON-RPC já parseada, o suficiente para inspecionar
// resposta e notificação sem um tipo para cada.
type linha struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string   `json:"sessionUpdate"`
			ToolCallID    string   `json:"toolCallId"`
			Title         string   `json:"title"`
			Kind          string   `json:"kind"`
			Status        string   `json:"status"`
			Content       Conteudo `json:"content"`
		} `json:"update"`
	} `json:"params"`
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Conteudo decora o campo content de um update ACP, que NÃO tem forma única:
// agent_message_chunk usa um ÚNICO objeto de bloco ({type,text});
// tool_call_update usa um ARRAY de blocos. Aceitar as duas é o que permite
// inspecionar os dois tipos de update com o mesmo decodificador — e se o agente
// um dia emitir a forma errada para um deles, o teste quebra aqui em vez de
// passar em silêncio.
type Conteudo struct {
	Blocos []bloco
}

type bloco struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c *Conteudo) UnmarshalJSON(b []byte) error {
	c.Blocos = nil
	if len(trimSpaceJSON(b)) == 0 || string(trimSpaceJSON(b)) == "null" {
		return nil
	}
	if trimSpaceJSON(b)[0] == '[' {
		return json.Unmarshal(b, &c.Blocos)
	}
	var um bloco
	if err := json.Unmarshal(b, &um); err != nil {
		return err
	}
	c.Blocos = []bloco{um}
	return nil
}

func trimSpaceJSON(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}

// leiaLinhas devolve as linhas já escritas nopipe, parseadas.
func leiaLinhas(t *testing.T, p *pipe) []linha {
	t.Helper()
	cru := p.String()
	if cru == "" {
		return nil
	}
	partes := strings.Split(strings.TrimSuffix(cru, "\n"), "\n")
	out := make([]linha, 0, len(partes))
	for _, s := range partes {
		if strings.TrimSpace(s) == "" {
			continue
		}
		var l linha
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			t.Fatalf("linha não é JSON válido: %q: %v", s, err)
		}
		out = append(out, l)
	}
	return out
}

// espereLinhas espera até haver n linhas escritas (ou estourar o prazo). Turno
// de session/prompt roda em goroutine, então a resposta não é síncrona com a
// escrita do request.
func espereLinhas(t *testing.T, p *pipe, n int) []linha {
	t.Helper()
	prazo := time.Now().Add(5 * time.Second)
	for {
		ls := leiaLinhas(t, p)
		if len(ls) >= n {
			return ls
		}
		if time.Now().After(prazo) {
			t.Fatalf("tempo esgotado: esperava %d linhas, vieram %d:\n%s", n, len(ls), p.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ── fakes ──────────────────────────────────────────────────────────────────

// fakeTurno é um Turno determinístico: emite eventos fixos na ordem do runtime
// (tools, guardrail, tokens, texto) e devolve o erro configurado. Respeita o
// ctx de propósito — se ele ignorasse o cancelamento, o teste de session/cancel
// passaria por acidente sem nunca ter exercitado o caminho real.
type fakeTurno struct {
	texto     string
	tools     []string      // tools que executam com sucesso
	falhas    []string      // tools que falham
	eco       string        // saída ecoada depois de cada tool
	erro      error         // erro devolvido no fim
	atraso    time.Duration // atraso antes de responder (cancelamento)
	compactar bool          // emite Guardrail(true)
	tokens    bool          // emite Tokens (não deve gerar notificação)

	iniciou chan struct{}
	umaVez  sync.Once

	mu       sync.Mutex
	ctxViu   bool
	pedido   string
	cwdVisto string
}

func newFakeTurno() *fakeTurno { return &fakeTurno{iniciou: make(chan struct{})} }

func (f *fakeTurno) Send(ctx context.Context, r runtime.Renderer, cwd, pedido string) error {
	f.umaVez.Do(func() { close(f.iniciou) })
	f.mu.Lock()
	f.pedido = pedido
	f.cwdVisto = cwd
	f.mu.Unlock()

	if f.atraso > 0 {
		select {
		case <-time.After(f.atraso):
		case <-ctx.Done():
			f.marcar()
		}
	}
	for _, nome := range f.tools {
		r.Tool(nome, 7*time.Millisecond, false)
		if f.eco != "" {
			r.Eco(f.eco)
		}
	}
	for _, nome := range f.falhas {
		r.Tool(nome, 3*time.Millisecond, true)
	}
	if f.compactar {
		r.Guardrail(true)
	}
	if f.tokens {
		r.Tokens(10, 20, time.Second, false)
	}
	if f.texto != "" {
		r.Resposta(f.texto)
	}
	if f.erro != nil {
		return f.erro
	}
	return nil
}

func (f *fakeTurno) marcar() {
	f.mu.Lock()
	f.ctxViu = true
	f.mu.Unlock()
}

func (f *fakeTurno) viuCtx() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxViu
}

func (f *fakeTurno) textoPedido() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pedido
}

func (f *fakeTurno) cwdRecebido() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cwdVisto
}

// turnoQueEspera bloqueia até o ctx ser cancelado e devolve o erro de
// cancelamento — o formato que o runtime real entrega quando o cliente pede
// session/cancel no meio de um turno.
type turnoQueEspera struct {
	iniciou chan struct{}
	livrei  chan struct{}
}

func newTurnoQueEspera() *turnoQueEspera {
	return &turnoQueEspera{iniciou: make(chan struct{}), livrei: make(chan struct{})}
}

func (t *turnoQueEspera) Send(ctx context.Context, _ runtime.Renderer, _, _ string) error {
	close(t.iniciou)
	<-ctx.Done()
	close(t.livrei)
	return ctx.Err()
}

// fabrica devolve uma fábrica determinística: sempre o mesmo turno e sempre o
// mesmo id, para os testes não dependerem de aleatoriedade.
func fabrica(f Turno) func(config.Config) (Turno, string) {
	return func(config.Config) (Turno, string) { return f, "s-deadbeef" }
}

// arranjo monta agente + pipe de saída + escritor de requests, e liga o Handle
// num goroutine com ctx do teste.
type arranjo struct {
	a        *Agente
	out      *pipe
	entrada  *io.PipeWriter
	cancelar context.CancelFunc
}

func arranjar(t *testing.T, fake Turno) *arranjo {
	t.Helper()
	a := Novo(config.Config{})
	a.Fabrica = fabrica(fake)
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Handle(ctx, pr, out) }()
	return &arranjo{a: a, out: out, entrada: pw, cancelar: cancel}
}

// envia escreve um request no fio de entrada.
func (ar *arranjo) envia(t *testing.T, method string, id any, params any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ar.entrada.Write(append(b, '\n')); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("escrita do request %s: %v", method, err)
	}
}

// ── jsonrpc.go ─────────────────────────────────────────────────────────────

func TestEscreverLinhaEmiteUmaLinhaJSON(t *testing.T) {
	var b strings.Builder
	if err := EscreverLinha(&b, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "\n") {
		t.Fatalf("linha sem terminador: %q", b.String())
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Fatalf("esperava exatamente uma linha, veio %q", b.String())
	}
}

func TestEscreverLinhaFazFlush(t *testing.T) {
	// O host pode passar um *bufio.Writer; sem flush a resposta morreria no
	// buffer e o cliente nunca a veria.
	var bruta strings.Builder
	bw := bufio.NewWriter(&bruta)
	if err := EscreverLinha(bw, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if bruta.String() == "" {
		t.Fatal("mensagem não chegou ao writer (flush ausente)")
	}
}

func TestDecodificarLinhaLinhaVaziaNaoEhMensagem(t *testing.T) {
	if _, ok, err := DecodificarLinha([]byte("   \n")); ok || err != nil {
		t.Fatalf("linha vazia deveria ser silenciosa: ok=%v err=%v", ok, err)
	}
}

func TestTemIDDistingueRequestDeNotificacao(t *testing.T) {
	req := Request{ID: json.RawMessage("1")}
	if !req.TemID() {
		t.Fatal("id numérico deveria contar como request")
	}
	notif := Request{ID: json.RawMessage("null")}
	if notif.TemID() {
		t.Fatal("id null deveria contar como notificação")
	}
	vazio := Request{}
	if vazio.TemID() {
		t.Fatal("id ausente deveria contar como notificação")
	}
}

func TestDecodificarLinhaJSONInvalidoDevolveErro(t *testing.T) {
	if _, _, err := DecodificarLinha([]byte("{nao json")); err == nil {
		t.Fatal("JSON inválido deveria devolver erro")
	}
}

// ── initialize ─────────────────────────────────────────────────────────────

func TestInitializeEcoAVersaoDoCliente(t *testing.T) {
	f := newFakeTurno()
	ar := arranjar(t, f)
	ar.envia(t, MetInitialize, 1, map[string]any{"protocolVersion": 5})
	ls := espereLinhas(t, ar.out, 1)
	r := ls[0]
	if r.Error != nil {
		t.Fatalf("initialize falhou: %+v", r.Error)
	}
	if v, ok := r.Result["protocolVersion"].(float64); !ok || int(v) != 5 {
		t.Fatalf("protocolVersion deveria ecoar 5, veio %v", r.Result["protocolVersion"])
	}
}

func TestInitializeSemParamsUsaVersaoUm(t *testing.T) {
	// Cliente que manda initialize sem params (ou sem protocolVersion) precisa
	// de resposta utilizável, não de erro: a versão 1 é o default do protocolo.
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, 7, map[string]any{})
	ls := espereLinhas(t, ar.out, 1)
	if v, ok := ls[0].Result["protocolVersion"].(float64); !ok || int(v) != 1 {
		t.Fatalf("protocolVersion default deveria ser 1, veio %v", ls[0].Result["protocolVersion"])
	}
}

func TestInitializeVersaoZeroCaiNoDefault(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, 8, map[string]any{"protocolVersion": 0})
	ls := espereLinhas(t, ar.out, 1)
	if v, _ := ls[0].Result["protocolVersion"].(float64); int(v) != 1 {
		t.Fatalf("protocolVersion 0 deveria cair no default 1, veio %v", ls[0].Result["protocolVersion"])
	}
}

func TestInitializeInformaCapacidadesEMetodosDeAuth(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, "abc", map[string]any{"protocolVersion": 1})
	ls := espereLinhas(t, ar.out, 1)
	res := ls[0].Result
	if _, ok := res["agentCapabilities"]; !ok {
		t.Fatalf("resposta sem agentCapabilities: %v", res)
	}
	if _, ok := res["authMethods"]; !ok {
		t.Fatalf("resposta sem authMethods: %v", res)
	}
}

func TestInitializePreservaIdNumericoEString(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, 42, nil)
	ar.envia(t, MetInitialize, "id-texto", nil)
	ls := espereLinhas(t, ar.out, 2)
	if string(ls[0].ID) != "42" {
		t.Fatalf("id numérico deveria voltar como 42, veio %s", ls[0].ID)
	}
	if string(ls[1].ID) != `"id-texto"` {
		t.Fatalf("id string deveria voltar como \"id-texto\", veio %s", ls[1].ID)
	}
}

// ── session/new ────────────────────────────────────────────────────────────

func TestSessionNewDevolveSessionId(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, 1, nil)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": `C:\tmp\projeto`})
	ls := espereLinhas(t, ar.out, 2)
	id, _ := ls[1].Result["sessionId"].(string)
	if id != "s-deadbeef" {
		t.Fatalf("sessionId inesperado: %q", id)
	}
}

func TestSessionNewCriaIdsDistintosPorSessao(t *testing.T) {
	f := newFakeTurno()
	n := 0
	var mu sync.Mutex
	a := Novo(config.Config{})
	a.Fabrica = func(config.Config) (Turno, string) {
		mu.Lock()
		defer mu.Unlock()
		n++
		switch n {
		case 1:
			return f, "s-primeira"
		case 2:
			return f, "s-segunda"
		default:
			return f, "s-outra"
		}
	}
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _ = a.Handle(context.Background(), pr, out) }()

	for i, id := range []int{10, 11} {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": MetSessionNovo, "params": map[string]any{"cwd": "."}})
		_, _ = pw.Write(append(b, '\n'))
		_ = i
	}
	ls := espereLinhas(t, out, 2)
	if ls[0].Result["sessionId"] == ls[1].Result["sessionId"] {
		t.Fatalf("duas sessões dividiram o mesmo id: %v", ls[0].Result["sessionId"])
	}
}

func TestSessionNewCwdVazioUsaDiretorioDoProcesso(t *testing.T) {
	// cwd vazio não invalida a sessão: o harness precisa de um diretório para
	// as tools, e o do processo é a melhor suposição disponível.
	f := &fakeTurno{iniciou: make(chan struct{}), texto: "ok"}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 3, map[string]any{})
	ls := espereLinhas(t, ar.out, 1)
	id, _ := ls[0].Result["sessionId"].(string)
	if id == "" {
		t.Fatal("session/new sem cwd deveria criar sessão normalmente")
	}
	ar.envia(t, MetPrompt, 4, map[string]any{"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": "oi"}}})
	espereLinhas(t, ar.out, 2)
	if f.cwdRecebido() == "" {
		t.Fatal("cwd ficou vazio no turno")
	}
}

// ── session/prompt ─────────────────────────────────────────────────────────

func TestPromptHappyPathEmiteChunkEDepoisRespostaVazia(t *testing.T) {
	f := &fakeTurno{iniciou: make(chan struct{}), texto: "turno ok"}
	ar := arranjar(t, f)

	ar.envia(t, MetInitialize, 1, map[string]any{"protocolVersion": 1})
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": `C:\tmp`})
	ls := espereLinhas(t, ar.out, 2)
	id := ls[1].Result["sessionId"].(string)

	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt": []map[string]string{
			{"type": "text", "text": "bom dia"},
			{"type": "text", "text": " como vai"},
		},
	})
	ls = espereLinhas(t, ar.out, 4)

	// 1 notificação agent_message_chunk, depois a resposta {} — nessa ordem.
	not := ls[2]
	if not.Method != "session/update" {
		t.Fatalf("esperava notificação session/update, veio %q", not.Method)
	}
	if not.Params.Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("sessionUpdate inesperado: %q", not.Params.Update.SessionUpdate)
	}
	if n := len(not.Params.Update.Content.Blocos); n != 1 || not.Params.Update.Content.Blocos[0].Text != "turno ok" {
		t.Fatalf("conteúdo do chunk inesperado: %+v", not.Params.Update.Content)
	}
	if not.Params.Update.Content.Blocos[0].Type != "text" {
		t.Fatalf("bloco do chunk deveria ser type=text: %q", not.Params.Update.Content.Blocos[0].Type)
	}
	if not.Params.SessionID != id {
		t.Fatalf("notificação deveria citar a sessão %q, veio %q", id, not.Params.SessionID)
	}

	resp := ls[3]
	if resp.Error != nil {
		t.Fatalf("prompt deveria ter sucesso: %+v", resp.Error)
	}
	if len(resp.Result) != 0 {
		t.Fatalf("resultado de prompt deveria ser {} vazio, veio %v", resp.Result)
	}
	if string(resp.ID) != "3" {
		t.Fatalf("resposta deveria citar o id 3, veio %s", resp.ID)
	}

	// O pedido enviado ao runtime é a concatenação dos blocos de texto.
	select {
	case <-f.iniciou:
	case <-time.After(time.Second):
		t.Fatal("turno nunca rodou")
	}
}

func TestPromptConcatenaBlocosDeTexto(t *testing.T) {
	f := &fakeTurno{iniciou: make(chan struct{}), texto: "ok"}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt": []map[string]string{
			{"type": "text", "text": "um"},
			{"type": "text", "text": " dois"},
		},
	})
	espereLinhas(t, ar.out, 3)
	if got := f.textoPedido(); got != "um dois" {
		t.Fatalf("pedido concatenado deveria ser \"um dois\", veio %q", got)
	}
}

func TestPromptIgnoraBlocosQueNaoSaoTexto(t *testing.T) {
	// Imagem/áudio/recurso não entram no pedido: o harness é textual neste
	// recorte. Ignorar é melhor que inventar transcrição.
	f := &fakeTurno{iniciou: make(chan struct{}), texto: "ok"}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt": []map[string]string{
			{"type": "image", "text": "ignorado"},
			{"type": "text", "text": "vale"},
		},
	})
	espereLinhas(t, ar.out, 3)
	if got := f.textoPedido(); got != "vale" {
		t.Fatalf("só o bloco de texto deveria entrar, veio %q", got)
	}
}

func TestPromptSessaoDesconhecidaDevolveErroDoAgente(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetPrompt, 9, map[string]any{
		"sessionId": "s-naoexiste",
		"prompt":    []map[string]string{{"type": "text", "text": "oi"}},
	})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodErroAgente {
		t.Fatalf("sessão desconhecida deveria dar -32000, veio %+v", ls[0])
	}
	if !strings.Contains(ls[0].Error.Message, "s-naoexiste") {
		t.Fatalf("mensagem deveria citar a sessão: %q", ls[0].Error.Message)
	}
}

func TestPromptArrayVazioEhParamsInvalidos(t *testing.T) {
	f := newFakeTurno()
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	id := ls[0].Result["sessionId"].(string)

	ar.envia(t, MetPrompt, 3, map[string]any{"sessionId": id, "prompt": []map[string]string{}})
	ls = espereLinhas(t, ar.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodParams {
		t.Fatalf("prompt vazio deveria dar -32602, veio %+v", ls[1])
	}
	select {
	case <-f.iniciou:
		t.Fatal("turno não deveria nem começar com prompt vazio")
	default:
	}
}

func TestPromptSemNenhumBlocoDeTextoEhParamsInvalidos(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	id := ls[0].Result["sessionId"].(string)

	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "image", "text": "x"}},
	})
	ls = espereLinhas(t, ar.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodParams {
		t.Fatalf("prompt sem bloco de texto deveria dar -32602, veio %+v", ls[1])
	}
}

func TestPromptSemSessionIdEhParamsInvalidos(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetPrompt, 3, map[string]any{
		"prompt": []map[string]string{{"type": "text", "text": "oi"}},
	})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("falta de sessionId deveria dar -32602, veio %+v", ls[0])
	}
}

func TestPromptParamsMalformadosEhParamsInvalidos(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	// params que não é objeto: o unmarshal para o struct tipado falha.
	b := []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":[1,2,3]}` + "\n")
	if _, err := ar.entrada.Write(b); err != nil {
		t.Fatal(err)
	}
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("params malformado deveria dar -32602, veio %+v", ls[0])
	}
}

func TestPromptComTurnoJaEmAndamentoDevolve32001(t *testing.T) {
	// Um turno em voo por sessão: o protocolo ACP não enfileira. O segundo
	// prompt leva erro explícito em vez de esperar indefinidamente.
	f := newTurnoQueEspera()
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	id := ls[0].Result["sessionId"].(string)

	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "primeiro"}},
	})
	select {
	case <-f.iniciou:
	case <-time.After(2 * time.Second):
		t.Fatal("primeiro turno não começou")
	}

	ar.envia(t, MetPrompt, 4, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "segundo"}},
	})
	ls = espereLinhas(t, ar.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodTurnoRodando {
		t.Fatalf("prompt concorrente deveria dar -32001, veio %+v", ls[1])
	}
	if string(ls[1].ID) != "4" {
		t.Fatalf("o erro deveria citar o id do prompt rejeitado, veio %s", ls[1].ID)
	}
}

func TestPromptComErroDoTurnoDevolve32000(t *testing.T) {
	f := &fakeTurno{iniciou: make(chan struct{}), erro: errors.New("provider 502")}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "oi"}},
	})
	ls = espereLinhas(t, ar.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodErroAgente {
		t.Fatalf("erro do turno deveria dar -32000, veio %+v", ls[1])
	}
	if !strings.Contains(ls[1].Error.Message, "502") {
		t.Fatalf("mensagem de erro deveria carregar a falha do turno: %q", ls[1].Error.Message)
	}
}

// ── session/cancel ─────────────────────────────────────────────────────────

func TestCancelInterrompeTurnoEmVoo(t *testing.T) {
	f := newTurnoQueEspera()
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	id := ls[0].Result["sessionId"].(string)

	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "demora"}},
	})
	select {
	case <-f.iniciou:
	case <-time.After(2 * time.Second):
		t.Fatal("turno não começou")
	}

	ar.envia(t, MetCancel, 4, map[string]any{"sessionId": id})

	// O prompt em voo devolve erro de cancelamento, e a resposta ao cancel
	// também chega — as duas linhas podem vir em qualquer ordem entre si.
	ls = espereLinhas(t, ar.out, 3)

	var respPrompt, respCancel *linha
	for i := range ls {
		if string(ls[i].ID) == "3" {
			respPrompt = &ls[i]
		}
		if string(ls[i].ID) == "4" {
			respCancel = &ls[i]
		}
	}
	if respPrompt == nil || respPrompt.Error == nil || respPrompt.Error.Code != CodErroAgente {
		t.Fatalf("prompt cancelado deveria dar erro -32000, veio %+v", respPrompt)
	}
	if !strings.Contains(strings.ToLower(respPrompt.Error.Message), "cancel") {
		t.Fatalf("mensagem deveria mencionar cancelamento: %q", respPrompt.Error.Message)
	}
	if respCancel == nil || respCancel.Error != nil {
		t.Fatalf("session/cancel deveria responder {} com sucesso, veio %+v", respCancel)
	}

	select {
	case <-f.livrei:
	case <-time.After(2 * time.Second):
		t.Fatal("o turno deveria ter observado o cancelamento")
	}
}

func TestCancelSemTurnoEmVooRespondeSucesso(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetCancel, 3, map[string]any{"sessionId": ls[0].Result["sessionId"]})
	ls = espereLinhas(t, ar.out, 2)
	if ls[1].Error != nil {
		t.Fatalf("cancel sem turno deveria ser um no-op com sucesso, veio %+v", ls[1].Error)
	}
}

func TestCancelSessaoDesconhecidaDevolveErro(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetCancel, 3, map[string]any{"sessionId": "s-fantasma"})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodErroAgente {
		t.Fatalf("cancel em sessão inexistente deveria dar -32000, veio %+v", ls[0])
	}
}

func TestCancelSemSessionIdEhParamsInvalidos(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetCancel, 3, map[string]any{})
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("cancel sem sessionId deveria dar -32602, veio %+v", ls[0])
	}
}

func TestCancelDepoisDoTurnoPermiteNovoPrompt(t *testing.T) {
	// O cancelamento limpa o estado da sessão: sem isso, todo prompt seguinte
	// na mesma sessão levaria -32001 para sempre.
	f := newTurnoQueEspera()
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	id := ls[0].Result["sessionId"].(string)

	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "trava"}},
	})
	<-f.iniciou
	ar.envia(t, MetCancel, 4, map[string]any{"sessionId": id})
	<-f.livrei

	// troca o turno por um que responde normalmente
	ar.a.mu.Lock()
	ar.a.sessoes[id].turno = &fakeTurno{iniciou: make(chan struct{}), texto: "depois"}
	ar.a.mu.Unlock()

	ar.envia(t, MetPrompt, 5, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "de novo"}},
	})
	ls = espereLinhas(t, ar.out, 5)
	var resposta *linha
	for i := range ls {
		if string(ls[i].ID) == "5" {
			resposta = &ls[i]
		}
	}
	if resposta == nil || resposta.Error != nil {
		t.Fatalf("prompt após cancelamento deveria funcionar, veio %+v", resposta)
	}
}

// ── erros de protocolo ─────────────────────────────────────────────────────

func TestMetodoDesconhecidoDevolve32601(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, "session/list", 5, nil)
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodMetodo {
		t.Fatalf("método desconhecido deveria dar -32601, veio %+v", ls[0])
	}
	if !strings.Contains(ls[0].Error.Message, "session/list") {
		t.Fatalf("mensagem deveria citar o método: %q", ls[0].Error.Message)
	}
}

func TestLinhaMalformadaNaoMataOLaco(t *testing.T) {
	// Um cliente que mandou lixo numa linha não pode derrubar o agente: a
	// linha é ignorada e o request seguinte é atendido normalmente.
	ar := arranjar(t, newFakeTurno())
	if _, err := ar.entrada.Write([]byte("{isso nao eh json\n")); err != nil {
		t.Fatal(err)
	}
	ar.envia(t, MetInitialize, 6, nil)
	ls := espereLinhas(t, ar.out, 1)
	if ls[0].Error != nil {
		t.Fatalf("initialize após linha malformada deveria funcionar: %+v", ls[0].Error)
	}
	if v, _ := ls[0].Result["protocolVersion"].(float64); int(v) != 1 {
		t.Fatalf("protocolVersion inesperado: %v", ls[0].Result["protocolVersion"])
	}
}

func TestNotificacaoDoClienteNaoGeraResposta(t *testing.T) {
	// Notificação (sem id) é mão única: responder seria violar o JSON-RPC 2.0.
	ar := arranjar(t, newFakeTurno())
	b := []byte(`{"jsonrpc":"2.0","method":"initialized","params":{}}` + "\n")
	if _, err := ar.entrada.Write(b); err != nil {
		t.Fatal(err)
	}
	ar.envia(t, MetInitialize, 12, nil)
	ls := espereLinhas(t, ar.out, 1)
	if len(ls) != 1 {
		t.Fatalf("notificação do cliente não deveria gerar linha, vieram %d:\n%s", len(ls), ar.out.String())
	}
	if string(ls[0].ID) != "12" {
		t.Fatalf("a única linha deveria ser a resposta do initialize, veio %s", ls[0].ID)
	}
}

func TestHandleEOFDevolveNil(t *testing.T) {
	a := Novo(config.Config{})
	a.Fabrica = fabrica(newFakeTurno())
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	if err := a.Handle(context.Background(), strings.NewReader(""), out); err != nil {
		t.Fatalf("EOF deveria ser encerramento limpo, veio %v", err)
	}
}

func TestHandleLinhaAcimaDoTetoDevolveErro(t *testing.T) {
	// Linha maior que o teto do scanner encerra o transporte: fingir que leu
	// corromperia o protocolo. O erro precisa sair, não ser engolido.
	a := Novo(config.Config{})
	a.Fabrica = fabrica(newFakeTurno())
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	gigante := strings.Repeat("x", MaxLinha+1024)
	if err := a.Handle(context.Background(), strings.NewReader(gigante+"\n"), out); err == nil {
		t.Fatal("linha acima do teto deveria devolver erro")
	}
}

func TestCadaRespostaEUmaLinhaComNewline(t *testing.T) {
	ar := arranjar(t, newFakeTurno())
	ar.envia(t, MetInitialize, 1, nil)
	ar.envia(t, MetInitialize, 2, nil)
	espereLinhas(t, ar.out, 2)
	s := ar.out.String()
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("saída deveria terminar em newline: %q", s)
	}
	// Cada linha tem de ser um JSON completo: nenhum newline no meio dos
	// objetos. Se um dia a escrita for dividida em duas chamadas de Write, duas
	// respostas se misturariam na mesma linha e isso apareceria aqui.
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		var probe map[string]any
		if err := json.Unmarshal([]byte(l), &probe); err != nil {
			t.Fatalf("linha não é JSON completo (newline no meio?): %q: %v", l, err)
		}
	}
	if n := strings.Count(s, "\n"); n != 2 {
		t.Fatalf("dois requests deveriam gerar exatamente 2 linhas, veio %d: %q", n, s)
	}
}

// ── tradução do renderer ───────────────────────────────────────────────────

func TestPromptTraduzToolEEcoParaNotificacoes(t *testing.T) {
	f := &fakeTurno{
		iniciou: make(chan struct{}),
		tools:   []string{"bash"},
		eco:     "saida da tool",
		texto:   "pronto",
	}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "roda"}},
	})
	// tool_call + tool_call_update + agent_message_chunk + resposta = 4 linhas
	ls = espereLinhas(t, ar.out, 4)

	tool := ls[1]
	if tool.Method != "session/update" || tool.Params.Update.SessionUpdate != "tool_call" {
		t.Fatalf("esperava tool_call, veio %+v", tool)
	}
	if tool.Params.Update.Title != "bash" {
		t.Fatalf("título deveria ser o nome da tool: %q", tool.Params.Update.Title)
	}
	if tool.Params.Update.Kind != "other" {
		t.Fatalf("kind deveria ser \"other\": %q", tool.Params.Update.Kind)
	}
	if tool.Params.Update.Status != "completed" {
		t.Fatalf("status deveria ser completed: %q", tool.Params.Update.Status)
	}
	if tool.Params.Update.ToolCallID == "" {
		t.Fatal("tool_call sem toolCallId")
	}

	eco := ls[2]
	if eco.Params.Update.SessionUpdate != "tool_call_update" {
		t.Fatalf("esperava tool_call_update, veio %+v", eco)
	}
	if eco.Params.Update.ToolCallID != tool.Params.Update.ToolCallID {
		t.Fatalf("eco deveria se anexar ao tool_call anterior: %q vs %q",
			eco.Params.Update.ToolCallID, tool.Params.Update.ToolCallID)
	}
	if n := len(eco.Params.Update.Content.Blocos); n != 1 || eco.Params.Update.Content.Blocos[0].Text != "saida da tool" {
		t.Fatalf("conteúdo do eco inesperado: %+v", eco.Params.Update.Content)
	}
}

func TestRenderToolComErroViraStatusFailed(t *testing.T) {
	f := &fakeTurno{iniciou: make(chan struct{}), falhas: []string{"bash"}, texto: "ok"}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "roda"}},
	})
	ls = espereLinhas(t, ar.out, 3)
	if ls[1].Params.Update.Status != "failed" {
		t.Fatalf("tool que falhou deveria virar status failed, veio %q", ls[1].Params.Update.Status)
	}
}

func TestRenderGuardrailViraChunkDeTexto(t *testing.T) {
	f := &fakeTurno{iniciou: make(chan struct{}), compactar: true, texto: "ok"}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "roda"}},
	})
	ls = espereLinhas(t, ar.out, 4)
	if ls[1].Params.Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("guardrail deveria virar chunk de texto, veio %+v", ls[1])
	}
	if n := len(ls[1].Params.Update.Content.Blocos); n != 1 {
		t.Fatalf("chunk do guardrail deveria ter 1 bloco, veio %+v", ls[1].Params.Update.Content)
	}
	if !strings.Contains(ls[1].Params.Update.Content.Blocos[0].Text, "guardrail") {
		t.Fatalf("texto do chunk deveria mencionar o guardrail: %q", ls[1].Params.Update.Content.Blocos[0].Text)
	}
}

func TestRenderTokensNaoEmiteNada(t *testing.T) {
	// Consumo de tokens é rodapé de UI local: não existe evento ACP para isso,
	// e inventar um poluiria o protocolo.
	f := &fakeTurno{iniciou: make(chan struct{}), tokens: true, texto: "ok"}
	ar := arranjar(t, f)
	ar.envia(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := espereLinhas(t, ar.out, 1)
	ar.envia(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "roda"}},
	})
	// chunk da resposta + resposta {} = 2 linhas. Nada de tokens no meio.
	ls = espereLinhas(t, ar.out, 3)
	if len(ls) != 3 {
		t.Fatalf("Tokens não deveria emitir nada; vieram %d linhas:\n%s", len(ls), ar.out.String())
	}
}
