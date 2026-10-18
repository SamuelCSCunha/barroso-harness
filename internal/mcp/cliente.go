package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Protocolo MCP implementado neste cliente.
const (
	// versaoProtocolo é a versão mais recente deste protocolo que o cliente
	// anuncia. Servidores antigos respondem success com a versão DELES no
	// result; o cliente aceita (MCP é retrocompatível) e não falha por isso.
	versaoProtocolo = "2025-06-18"

	// nomeCliente / versaoCliente vão no clientInfo do initialize.
	nomeCliente   = "barroso-harness"
	versaoCliente = "1.0"

	tetoLinhaStdio    = 1 << 20           // 1 MiB por linha JSON no scanner do stdio
	tetoBodyHTTP      = 32 << 20          // teto do corpo da resposta HTTP
	timeoutPorChamada = 120 * time.Second // teto padrão de cada chamada JSON-RPC
	prazoMataSub      = 5 * time.Second   // espera após fechar stdin antes do Kill
)

// ── JSON-RPC 2.0 ─────────────────────────────────────────────────────────

// mensagem é a peça comum do protocolo: request/response têm id, notificação
// não tem. Campos desconhecidos são ignorados no unmarshal — MCP acrescenta
// campos à vontade e o cliente precisa tolerar.
type mensagem struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *erroRPC        `json:"error,omitempty"`
}

// temID distingue request/response (id presente) de notificação (ausente).
// id:null não conta.
func (m mensagem) temID() bool {
	v := bytes.TrimSpace(m.ID)
	return len(v) > 0 && !bytes.Equal(v, []byte("null"))
}

// erroRPC é o objeto error do JSON-RPC (code + message obrigatórios, data é
// livre e de forma imprevisível entre servidores).
type erroRPC struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error formata em UMA linha: data cru nunca é despejado (pode ter segredo).
func (e *erroRPC) Error() string {
	if e == nil {
		return "erro do servidor MCP"
	}
	msg := umaLinha(e.Message)
	if msg == "" {
		msg = "(sem mensagem)"
	}
	if e.Code != 0 {
		return fmt.Sprintf("servidor MCP respondeu erro %d: %s", e.Code, msg)
	}
	return "servidor MCP respondeu erro: " + msg
}

// notificacao monta uma notificação JSON-RPC (sem id; sem resposta esperada).
func notificacao(metodo string, params any) mensagem {
	m := mensagem{JSONRPC: "2.0", Method: metodo}
	if params != nil {
		m.Params = jsonBytes(params)
	}
	return m
}

// requisicao empacota um request com id numérico e o corpo já serializado.
type requisicao struct {
	id     int64
	metodo string
	corpo  []byte
}

func novaRequisicao(id int64, metodo string, params any) requisicao {
	m := mensagem{JSONRPC: "2.0", ID: jsonBytes(id), Method: metodo}
	if params != nil {
		m.Params = jsonBytes(params)
	}
	b, err := json.Marshal(m)
	if err != nil {
		// Só acontece com valor impossível de serializar; params daqui são
		// structs internas. Devolve corpo vazio e o envio falha alto.
		b = nil
	}
	return requisicao{id: id, metodo: metodo, corpo: b}
}

// jsonBytes converte um valor interno em JSON; em impossibilidade devolve
// null (nunca panic num caminho de rede).
func jsonBytes(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// ── parâmetros e resultados MCP ──────────────────────────────────────────

// paramsInitialize é o que mandamos no initialize. Capabilities vazio = este
// cliente não oferece nada além de ferramentas (sampling/roots/prompts ficam
// para uma versão futura, e o servidor lida com a ausência).
type paramsInitialize struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      infoCliente    `json:"clientInfo"`
}

type infoCliente struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// resultInitialize é o que aceitamos de volta — tolerante a capabilities
// ausentes, serverInfo parcial e instruções longas.
type resultInitialize struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions,omitempty"`
}

// ferramentaMCP é uma entrada do tools/list.
type ferramentaMCP struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

type resultToolsList struct {
	Tools []ferramentaMCP `json:"tools"`
}

// paramsToolsCall é o que enviamos no tools/call. Arguments é um objeto: o
// argsJSON do chamador é parseado em mapa e reencaminhado como tal.
type paramsToolsCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// resultToolsCall é o resultado do tools/call: content é uma lista de blocos
// tipados e isError marca falha do lado do servidor.
type resultToolsCall struct {
	Content []blocoConteudo `json:"content"`
	IsError bool            `json:"isError"`
}

// blocoConteudo é um item de content. Tipos que não são "text" entram no
// resumo pelo Type — não derrubam a chamada.
type blocoConteudo struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ── Cliente ──────────────────────────────────────────────────────────────

// Cliente é UMA conexão com UM servidor MCP, pelos dois transportes do
// protocolo: stdio (subprocesso, JSON-RPC delimitado por newline em
// stdin/stdout) e HTTP streamable (POST do JSON-RPC; resposta em JSON puro
// ou SSE).
//
// As ferramentas são expostas com o NOME CRU que o servidor publica — este
// pacote não prefixa nada. Namespacing para evitar colisão entre servidores
// (ex.: "github__criar_issue") é trabalho de quem integra, e Fonte() entrega
// o nome do servidor exatamente para isso.
//
// respRouter distribui as respostas lidas do transporte para QUEM está
// esperando aquele id. Sem ele, duas chamadas concorrentes disputariam uma
// mensagem por vez por um canal compartilhado: cada uma descarta a resposta
// que não casa — e às vezes descarta justamente a que a outra precisava, e aí
// ela estoura no tempo limite sem ninguém entregar.
type respRouter struct {
	mu     sync.Mutex
	espera map[string]chan mensagem
}

func novoRespRouter() *respRouter {
	return &respRouter{espera: map[string]chan mensagem{}}
}

// registrar cria o canal de entrega de um id e o devolve.
func (r *respRouter) registrar(id string) chan mensagem {
	ch := make(chan mensagem, 1)
	r.mu.Lock()
	r.espera[id] = ch
	r.mu.Unlock()
	return ch
}

// esquecer tira o canal do mapa (chamador desistiu: timeout ou ctx morto).
func (r *respRouter) esquecer(id string) {
	r.mu.Lock()
	delete(r.espera, id)
	r.mu.Unlock()
}

// entregar encaminha a resposta para o dono do id. Resposta órfã (ninguém
// esperando mais) é descartada sem travar o leitor.
func (r *respRouter) entregar(id string, m mensagem) {
	r.mu.Lock()
	ch, ok := r.espera[id]
	r.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- m: // buffer 1: nunca trava o leitor
	default:
	}
}

// Cliente é seguro para uso concorrente: cada chamada reserva um id próprio
// e a resposta é roteada por id, então goroutines diferentes podem chamar
// ferramentas ao mesmo tempo (stdio usa um writer único + um reader único).
type Cliente struct {
	serv  Servidor
	tipo  string
	fonte string

	// http
	url   string
	httpc *http.Client

	// stdio
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	saida    chan []byte   // fila de escritas (um escritor só)
	roteador *respRouter   // resposta → quem espera aquele id
	fim      chan struct{} // fechado no encerramento: desbloqueia os esperadores
	escErr   chan error    // primeiro erro de escrita (não fatal por si)

	mu        sync.Mutex
	falhaLeit error

	inicializado bool
	info         resultInitialize
	caps         map[string]any

	proxID   int64
	timeout  time.Duration
	fechaUma sync.Once
}

// encerrar fecha o canal fim UMA única vez — chamado tanto pelo Fecha (decisão
// local) quanto pelo reader quando o stdout do subprocesso acaba (decisão do
// servidor). Fechar duas vezes seria panic, daí o Once.
func (c *Cliente) encerrar() {
	c.fechaUma.Do(func() { close(c.fim) })
}

// Novo valida a configuração e devolve um cliente AINDA NÃO conectado. Abrir
// é passo separado (Abre) porque a conexão pode falhar e o chamador decide o
// que fazer — inclusive seguir sem este servidor.
func Novo(s Servidor) (*Cliente, error) {
	if err := s.validar(); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	tipo, err := normalizarTipo(s.Tipo)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	return &Cliente{
		serv:     s,
		tipo:     tipo,
		fonte:    s.Nome,
		saida:    make(chan []byte, 32),
		roteador: novoRespRouter(),
		fim:      make(chan struct{}),
		escErr:   make(chan error, 1),
		timeout:  s.timeoutDuracao(),
	}, nil
}

// Fonte devolve Servidor.Nome — o nome do servidor, para prefixo/namespace.
func (c *Cliente) Fonte() string { return c.fonte }

// Transporto devolve a forma canônica do transporte: "stdio" ou "http".
func (c *Cliente) Transporto() string { return c.tipo }

// InfoServidor devolve o que o initialize negociou (cópia Defensiva). Antes
// de abrir, devolve o valor zero.
func (c *Cliente) InfoServidor() resultInitialize {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Capacidades devolve o mapa de capabilities que o servidor anunciou no
// initialize (cópia defensiva; nil antes da abertura ou se não anunciou).
func (c *Cliente) Capacidades() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.caps) == 0 {
		return nil
	}
	out := make(map[string]any, len(c.caps))
	for k, v := range c.caps {
		out[k] = v
	}
	return out
}

// proximoID reserva um id monótono (correlação id-based, exigida porque o
// servidor pode emitir notificações entre respostas).
func (c *Cliente) proximoID() int64 { return atomic.AddInt64(&c.proxID, 1) }

// Abre conecta e faz o handshake (initialize + notifications/initialized).
// Depois de Abre o cliente está pronto para Ferramentas e Chamar.
func (c *Cliente) Abre(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch c.tipo {
	case tipoStdio:
		return c.abreStdio(ctx)
	case tipoHTTP:
		return c.abreHTTP(ctx)
	default:
		return fmt.Errorf("mcp: servidor %q: transporte %q não implementado", c.fonte, c.tipo)
	}
}

// ── stdio ────────────────────────────────────────────────────────────────

func (c *Cliente) abreStdio(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.serv.Command, c.serv.Args...)
	cmd.Dir = cwdDoServidor()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: servidor %q: stdin: %w", c.fonte, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp: servidor %q: stdout: %w", c.fonte, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("mcp: servidor %q: stderr: %w", c.fonte, err)
	}
	// Ambiente = o do processo + o do servidor. Em Windows o formato já é o
	// esperado (blocos "K=V" separados por NUL); nada a converter.
	cmd.Env = append(os.Environ(), envSlice(c.serv.Env)...)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp: servidor %q: start: %w", c.fonte, err)
	}
	c.cmd = cmd
	c.stdin = stdin

	// stderr é drenado e descartado: servidor MCP usa o canal de erro como
	// log de debug e, se ninguém lesse, o pipe encheria e travaria o
	// subprocesso. Nunca imprimimos — o ruído pode conter segredo.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	// Um reader (newline-delimited JSON) e um writer: sem interleaving.
	go c.leLoopStdio(stdout)
	go c.escreveLoop()

	if err := c.handshake(ctx); err != nil {
		_ = c.Fecha() // não deixar subprocesso zumbi em meia-conexão
		return err
	}
	return nil
}

// leLoopStdio lê uma mensagem JSON por linha. Buffer de até 1 MiB — MCP stdio
// é newline-delimited JSON-RPC (o framing Content-Length do LSP NÃO é parte
// deste protocolo).
//
// Notificações entre respostas são descartadas AQUI: o cliente se correlaciona
// por id, e notificação não tem id — guardá-la só encheria o canal e confundiria
// o esperador. (O cliente não assina nem precisa assinar notificações MCP.)
func (c *Cliente) leLoopStdio(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), tetoLinhaStdio)
	for sc.Scan() {
		linha := bytes.TrimSpace(sc.Bytes())
		if len(linha) == 0 {
			continue
		}
		var m mensagem
		if json.Unmarshal(linha, &m) != nil {
			continue // ruído não-JSON: ignorado, não mata a sessão
		}
		if !m.temID() {
			continue // notificação: sem id a correlacionar
		}
		// Rotear por id: a mensagem vai direto para o dono, sem passar por
		// canal compartilhado (outras chamadas concorrentes nunca a pegam).
		c.roteador.entregar(nomeID(m.ID), m)
	}
	// EOF do stdout (ou linha acima do buffer): o servidor se foi. Marca a
	// falha e sinaliza o fim para os esperadores pendentes.
	c.marcaFalhaLeitura(sc.Err())
	c.encerrar()
}

// escreveLoop é o ÚNICO escritor do stdin: pega o corpo já serializado e põe
// um newline no fim (delimitador de mensagem). Nunca morre no meio: quando o
// Fecha fecha o canal de saída, o range drena os pendentes e encerra.
// escreveLoop é o ÚNICO escritor do stdin. O canal c.saida NUNCA é fechado:
// fechá-lo faria panic ("send on closed channel") em qualquer chamador
// concorrente que estivesse enviando. O sinal de parada é c.fim, que o Fecha
// fecha — aí o escritor drena o que resta e sai.
func (c *Cliente) escreveLoop() {
	stdin := c.stdin
	if stdin == nil {
		return
	}
	for {
		select {
		case corpo := <-c.saida:
			if len(corpo) == 0 {
				continue
			}
			if _, err := stdin.Write(append(corpo, '\n')); err != nil {
				// Pipe quebrado: sinaliza quem espera e para de escrever.
				// stdin continua o mesmo — o Fecha é quem o fecha.
				c.sinalizaEscrita(err)
				return
			}
		case <-c.fim:
			return
		}
	}
}

func (c *Cliente) sinalizaEscrita(err error) {
	select {
	case c.escErr <- err:
	default:
	}
}

// marcaFalhaLeitura guarda o primeiro erro/EOF do reader.
func (c *Cliente) marcaFalhaLeitura(err error) {
	c.mu.Lock()
	if c.falhaLeit == nil {
		c.falhaLeit = err
	}
	c.mu.Unlock()
}

func (c *Cliente) falhaAtual() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.falhaLeit
}

// cwdDoServidor: o subprocesso MCP roda no diretório do agente, não no da
// harness — servidores stdio costumam resolver caminhos relativos ao projeto.
func cwdDoServidor() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// ── http ─────────────────────────────────────────────────────────────────

func (c *Cliente) abreHTTP(_ context.Context) error {
	c.url = strings.TrimSpace(c.serv.URL)
	c.httpc = &http.Client{Timeout: c.timeout}
	return c.handshake(context.Background())
}

// ── handshake ────────────────────────────────────────────────────────────

// handshake faz initialize (com correlacão por id, ignorando notificações)
// e notifications/initialized. Idempotente: chamadas seguintes são no-op.
func (c *Cliente) handshake(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	ja := c.inicializado
	c.mu.Unlock()
	if ja {
		return nil
	}

	req := novaRequisicao(c.proximoID(), "initialize", paramsInitialize{
		ProtocolVersion: versaoProtocolo,
		Capabilities:    map[string]any{},
		ClientInfo:      infoCliente{Name: nomeCliente, Version: versaoCliente},
	})
	res, err := c.enviar(ctx, req)
	if err != nil {
		return fmt.Errorf("mcp: servidor %q: initialize: %w", c.fonte, err)
	}
	var init resultInitialize
	if err := decodificarResultado(res.Result, &init); err != nil {
		return fmt.Errorf("mcp: servidor %q: initialize: %w", c.fonte, err)
	}
	c.mu.Lock()
	c.info = init
	c.caps = init.Capabilities
	c.inicializado = true
	c.mu.Unlock()

	if err := c.enviarBruto(ctx, notificacao("notifications/initialized", nil)); err != nil {
		return fmt.Errorf("mcp: servidor %q: notifications/initialized: %w", c.fonte, err)
	}
	return nil
}

// ── request → response ───────────────────────────────────────────────────

// enviar dispara o request e devolve a resposta de id casante.
func (c *Cliente) enviar(ctx context.Context, r requisicao) (mensagem, error) {
	if len(r.corpo) == 0 {
		return mensagem{}, fmt.Errorf("mcp: request %s não serializou", r.metodo)
	}
	if c.tipo == tipoHTTP {
		return c.requestHTTP(ctx, r)
	}
	return c.requestStdio(ctx, r)
}

// requestStdio põe o corpo na fila do escritor único e espera a resposta.
func (c *Cliente) requestStdio(ctx context.Context, r requisicao) (mensagem, error) {
	espera := c.prazo(ctx)
	select {
	case c.saida <- r.corpo:
	case <-time.After(espera):
		return mensagem{}, fmt.Errorf("mcp: servidor %q: fila de escrita cheia ao enviar %s", c.fonte, r.metodo)
	case <-ctx.Done():
		return mensagem{}, ctx.Err()
	case <-c.fim:
		return mensagem{}, fmt.Errorf("mcp: servidor %q: conexão encerrada ao enviar %s", c.fonte, r.metodo)
	}

	idCrua := jsonBytes(r.id)
	chave := nomeID(idCrua)
	minha := c.roteador.registrar(chave)
	defer c.roteador.esquecer(chave)

	timer := time.NewTimer(espera)
	defer timer.Stop()
	for {
		select {
		case m := <-minha:
			if m.Error != nil {
				return mensagem{}, m.Error
			}
			return m, nil
		case err := <-c.escErr:
			if err != nil {
				return mensagem{}, fmt.Errorf("mcp: servidor %q: escrita: %w", c.fonte, err)
			}
		case <-timer.C:
			return mensagem{}, fmt.Errorf("mcp: servidor %q: tempo de %s esgotado aguardando resposta de %s", c.fonte, espera, r.metodo)
		case <-ctx.Done():
			return mensagem{}, ctx.Err()
		case <-c.fim:
			return mensagem{}, c.erroConexao(r.metodo)
		}
	}
}

// erroConexao explica o fim da conexão: EOF puro, erro de IO ou encerramento
// local pelo Fecha.
func (c *Cliente) erroConexao(metodo string) error {
	select {
	case <-c.fim:
	default:
		return fmt.Errorf("mcp: servidor %q: cliente fechado antes de %s", c.fonte, metodo)
	}
	if err := c.falhaAtual(); err != nil {
		return fmt.Errorf("mcp: servidor %q: leitura: %w", c.fonte, err)
	}
	return fmt.Errorf("mcp: servidor %q: servidor encerrou a conexão durante %s", c.fonte, metodo)
}

// requestHTTP faz o POST e extrai a resposta do JSON puro ou do stream SSE.
func (c *Cliente) requestHTTP(ctx context.Context, r requisicao) (mensagem, error) {
	espera := c.prazo(ctx)
	hctx, cancel := context.WithTimeout(ctx, espera)
	defer cancel()

	req, err := http.NewRequestWithContext(hctx, http.MethodPost, c.url, bytes.NewReader(r.corpo))
	if err != nil {
		return mensagem{}, fmt.Errorf("mcp: servidor %q: request: %w", c.fonte, err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Mesmo UA do dealer: há WAF que barra o "Go-http-client/1.1" padrão antes
	// de olhar credencial — e gateway MCP costuma ficar atrás de um deles.
	req.Header.Set("User-Agent", dealer.UserAgent)
	// A especificação do MCP streamable HTTP EXIGE este Accept: o servidor
	// escolhe entre JSON puro e SSE conforme o que o cliente declara aceitar.
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.serv.Headers {
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return mensagem{}, fmt.Errorf("mcp: servidor %q: %w", c.fonte, err)
	}
	defer resp.Body.Close()
	corpo, err := io.ReadAll(io.LimitReader(resp.Body, tetoBodyHTTP))
	if err != nil {
		return mensagem{}, fmt.Errorf("mcp: servidor %q: lendo resposta: %w", c.fonte, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mensagem{}, fmt.Errorf("mcp: servidor %q: HTTP %d: %s", c.fonte, resp.StatusCode, resumoHTTP(corpo, 300))
	}
	m, err := respostaDe(corpo, resp.Header.Get("Content-Type"), jsonBytes(r.id))
	if err != nil {
		return mensagem{}, fmt.Errorf("mcp: servidor %q: %s: %w", c.fonte, r.metodo, err)
	}
	if m.Error != nil {
		return mensagem{}, m.Error
	}
	return m, nil
}

// enviarBruto manda uma notificação (sem resposta a esperar). Em stdio vai
// para o escritor; em HTTP é o POST.
//
// Em HTTP o servidor pode responder 202/200 com o corpo VAZIO (e deve: a
// especificação do MCP streamable permite notificação sem resposta JSON-RPC),
// ou com um JSON-RPC de resposta, ou com outro de erro. Todos os três são
// válidos; o parse do corpo é tolerante a isso.
func (c *Cliente) enviarBruto(ctx context.Context, m mensagem) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if c.tipo == tipoHTTP {
		r := requisicao{metodo: m.Method, corpo: b}
		// Notificação: o que interessa é só falha de TRANSPORTE (rede, status
		// HTTP, timeout). Corpo vazio não é falha.
		_, err := c.requestHTTP(ctx, r)
		if err != nil {
			var vazio erroRespostaVazia
			if erroComo(err, &vazio) {
				return nil
			}
			return err
		}
		return nil
	}
	espera := c.prazo(ctx)
	select {
	case c.saida <- b:
		return nil
	case <-time.After(espera):
		return fmt.Errorf("fila de escrita cheia")
	case <-ctx.Done():
		return ctx.Err()
	case <-c.fim:
		return fmt.Errorf("mcp: servidor %q: conexão encerrada ao notificar %s", c.fonte, m.Method)
	}
}

// prazo casa o timeout do servidor com o do ctx (o menor vence).
func (c *Cliente) prazo(ctx context.Context) time.Duration {
	d := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		if rest := time.Until(dl); rest < d {
			d = rest
		}
	}
	if d <= 0 {
		d = timeoutPorChamada
	}
	return d
}

// erroRespostaVazia marca corpo ausente numa resposta 2xx: válido em
// notificação (o servidor pode responder 202/200 sem corpo), inválido em
// request,onde uma resposta JSON-RPC é obrigatória.
type erroRespostaVazia struct{}

func (erroRespostaVazia) Error() string { return "resposta sem corpo" }

// erroComo é o errors.As sem importar errors aqui (o arquivo não precisa do
// pacote para mais nada).
func erroComo(err error, alvo any) bool {
	for err != nil {
		if e, ok := err.(erroRespostaVazia); ok {
			if a, ok := alvo.(*erroRespostaVazia); ok {
				*a = e
				return true
			}
			return false
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// respostaDe acha a mensagem de id casante: aceita JSON-RPC único, batch
// (array) e SSE (linhas data:).
func respostaDe(corpo []byte, contentType string, id json.RawMessage) (mensagem, error) {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/event-stream") {
		return respostaSSE(corpo, id)
	}
	ini := bytes.TrimSpace(corpo)
	if bytes.HasPrefix(ini, []byte("data:")) || bytes.HasPrefix(ini, []byte("event:")) ||
		bytes.HasPrefix(ini, []byte("id:")) || bytes.HasPrefix(ini, []byte(":")) {
		// SSE sem Content-Type informativo. O ":" fecha um COMENTÁRIO SSE
		// (":ping"), que vem antes do data: em streams reais — sniffar só
		// pelas outras três prefixos trataria esse stream como JSON e falhava.
		return respostaSSE(corpo, id)
	}
	return respostaJSON(corpo, id)
}

// respostaJSON extrai de um objeto JSON-RPC ou de um array (batch).
func respostaJSON(corpo []byte, id json.RawMessage) (mensagem, error) {
	corpo = bytes.TrimSpace(corpo)
	if len(corpo) == 0 {
		return mensagem{}, erroRespostaVazia{}
	}
	if corpo[0] == '[' {
		var lote []mensagem
		if err := json.Unmarshal(corpo, &lote); err != nil {
			return mensagem{}, fmt.Errorf("resposta JSON inválida: %w", err)
		}
		return acha(lote, id)
	}
	var m mensagem
	if err := json.Unmarshal(corpo, &m); err != nil {
		return mensagem{}, fmt.Errorf("resposta JSON inválida: %w", err)
	}
	if !m.temID() {
		return mensagem{}, fmt.Errorf("servidor respondeu notificação, não um resultado")
	}
	if !mesmoID(m.ID, id) {
		return mensagem{}, fmt.Errorf("resposta com id inesperado: %s", nomeID(m.ID))
	}
	return m, nil
}

// respostaSSE lê as linhas data: — uma mensagem JSON-RPC por evento, ou um
// batch. Comentários (":ping"), event: e "[DONE]" são ignorados.
func respostaSSE(corpo []byte, id json.RawMessage) (mensagem, error) {
	var achadas []mensagem
	for _, bruta := range strings.Split(string(corpo), "\n") {
		linha := strings.TrimRight(bruta, "\r")
		if !strings.HasPrefix(linha, "data:") {
			continue
		}
		dados := strings.TrimSpace(strings.TrimPrefix(linha, "data:"))
		if dados == "" || dados == "[DONE]" {
			continue
		}
		if dados[0] == '[' {
			var lote []mensagem
			if json.Unmarshal([]byte(dados), &lote) == nil {
				for _, m := range lote {
					if m.temID() {
						achadas = append(achadas, m)
					}
				}
				continue
			}
		}
		var m mensagem
		if json.Unmarshal([]byte(dados), &m) != nil {
			continue // keep-alive ou evento não-JSON
		}
		if m.temID() {
			achadas = append(achadas, m)
		}
	}
	return acha(achadas, id)
}

// acha procura a resposta de id casante numa lista (batch ou eventos SSE).
func acha(lista []mensagem, id json.RawMessage) (mensagem, error) {
	for _, m := range lista {
		if mesmoID(m.ID, id) {
			return m, nil
		}
	}
	return mensagem{}, fmt.Errorf("resposta com id %s não veio no corpo", nomeID(id))
}

// mesmoID compara ids tolerando número vs string.
func mesmoID(a, b json.RawMessage) bool { return nomeID(a) == nomeID(b) }

// nomeID reduz o id a uma forma comparável estável (n:1, s:abc).
func nomeID(id json.RawMessage) string {
	v := bytes.TrimSpace(id)
	if len(v) == 0 {
		return ""
	}
	var num json.Number
	if json.Unmarshal(v, &num) == nil {
		if i, err := num.Int64(); err == nil {
			return "n:" + fmt.Sprint(i)
		}
		return "s:" + num.String()
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return "s:" + s
	}
	return string(v)
}

// resumoHTTP reduz o corpo ao essencial em UMA linha (HTML de WAF, JSON de
// erro ou texto puro) — nunca despeja o corpo cru no terminal.
func resumoHTTP(b []byte, max int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	if s == "" {
		return "(corpo vazio)"
	}
	return s
}

// umaLinha colapsa espaços e quebras: mensagens numa linha só.
func umaLinha(s string) string { return resumoHTTP([]byte(s), 400) }

// decodificarResultado faz unmarshal tolerante: resultado ausente ou null
// devolve o valor zero do alvo em vez de erro.
func decodificarResultado(raw json.RawMessage, alvo any) error {
	limpo := bytes.TrimSpace(raw)
	if len(limpo) == 0 || bytes.Equal(limpo, []byte("null")) {
		return nil
	}
	return json.Unmarshal(limpo, alvo)
}

// ── chamadas MCP de alto nível ───────────────────────────────────────────

// Ferramentas faz o handshake (se ainda não houve) e o tools/list; cada
// ferramenta do servidor vira uma tools.Spec cujo Exec chama Chamar com o
// nome cru. Servidor sem ferramentas devolve lista vazia — não é erro.
func (c *Cliente) Ferramentas(ctx context.Context) ([]tools.Spec, error) {
	if err := c.handshake(ctx); err != nil {
		return nil, err
	}
	res, err := c.enviar(ctx, novaRequisicao(c.proximoID(), "tools/list", nil))
	if err != nil {
		return nil, fmt.Errorf("mcp: servidor %q: tools/list: %w", c.fonte, err)
	}
	var lista resultToolsList
	if err := decodificarResultado(res.Result, &lista); err != nil {
		return nil, fmt.Errorf("mcp: servidor %q: tools/list: %w", c.fonte, err)
	}
	specs := make([]tools.Spec, 0, len(lista.Tools))
	for _, f := range lista.Tools {
		nome := strings.TrimSpace(f.Name)
		if nome == "" {
			continue
		}
		specs = append(specs, c.specDe(f, nome))
	}
	return specs, nil
}

// specDe converte uma ferramenta MCP em Spec da harness. Schema é o
// inputSchema do servidor; se ele não publicou (ou veio vazio), cai para um
// objeto sem restrições — provider nenhum aceita parameters ausente.
func (c *Cliente) specDe(f ferramentaMCP, nome string) tools.Spec {
	schema := f.InputSchema
	if len(schema) == 0 {
		schema = schemaObjetoLivre()
	}
	desc := umaLinha(f.Description)
	if desc == "" {
		desc = fmt.Sprintf("Ferramenta MCP %s do servidor %s", nome, c.fonte)
	}
	serv := c.fonte
	return tools.Spec{
		Name:        nome,
		Description: desc,
		Schema:      schema,
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			args := string(bytes.TrimSpace(raw))
			if args == "" || args == "null" {
				args = "{}"
			}
			out, err := c.Chamar(ctx, nome, args)
			if err != nil {
				// Erro do servidor vira texto + erro Go: o provider vê a falha
				// e pode reagir, e o runtime não trata como crash da sessão.
				if out == "" {
					return "", fmt.Errorf("mcp %s.%s: %w", serv, nome, err)
				}
				return out, fmt.Errorf("mcp %s.%s: %w", serv, nome, err)
			}
			return out, nil
		},
	}
}

func schemaObjetoLivre() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// Chamar executa tools/call. argsJSON é o JSON dos argumentos; vazio ou
// "null" viram {}. isError do servidor vira erro Go com a mensagem dele.
func (c *Cliente) Chamar(ctx context.Context, nome, argsJSON string) (string, error) {
	if err := c.handshake(ctx); err != nil {
		return "", err
	}
	args := map[string]any{}
	limpo := strings.TrimSpace(argsJSON)
	if limpo != "" && limpo != "null" {
		if err := json.Unmarshal([]byte(limpo), &args); err != nil {
			return "", fmt.Errorf("mcp: argumentos inválidos para %q: %w", nome, err)
		}
	}
	res, err := c.enviar(ctx, novaRequisicao(c.proximoID(), "tools/call", paramsToolsCall{Name: nome, Arguments: args}))
	if err != nil {
		return "", fmt.Errorf("mcp: servidor %q: tools/call %s: %w", c.fonte, nome, err)
	}
	var out resultToolsCall
	if err := decodificarResultado(res.Result, &out); err != nil {
		return "", fmt.Errorf("mcp: servidor %q: tools/call %s: %w", c.fonte, nome, err)
	}
	texto := juntarConteudo(out.Content)
	if out.IsError {
		if texto == "" {
			texto = "(servidor reportou erro sem conteúdo)"
		}
		return texto, fmt.Errorf("servidor MCP %q reportou erro em %s: %s", c.fonte, nome, umaLinha(texto))
	}
	return texto, nil
}

// juntarConteudo concatena os blocos do resultado. Blocos de texto são
// separados por newline; blocos de outro tipo viram marcador mínimo entre
// colchetes (imagem e recurso embutido não têm texto para o provider ler).
//
// O conteúdo de texto entra CRU. Um servidor pode responder "eco: " com o
// espaço importando (valor formatado), e cortar esse espaço apagaria
// informação da ferramenta.
func juntarConteudo(blocos []blocoConteudo) string {
	if len(blocos) == 0 {
		return ""
	}
	partes := make([]string, 0, len(blocos))
	for _, b := range blocos {
		switch strings.ToLower(strings.TrimSpace(b.Type)) {
		case "", "text":
			partes = append(partes, b.Text) // cru: sem trim
		case "image":
			partes = append(partes, fmt.Sprintf("[imagem %s omitida]", ouPadrao(b.MimeType, "image/*")))
		case "resource":
			alvo := strings.TrimSpace(b.URI)
			if alvo == "" {
				alvo = ouPadrao(b.Name, "recurso")
			}
			partes = append(partes, fmt.Sprintf("[recurso %s]", alvo))
		default:
			partes = append(partes, fmt.Sprintf("[bloco %s omitido]", ouPadrao(b.Type, "desconhecido")))
		}
	}
	return strings.Join(partes, "\n")
}

func ouPadrao(v, fb string) string {
	if strings.TrimSpace(v) == "" {
		return fb
	}
	return v
}

// ── fechamento ───────────────────────────────────────────────────────────

// Fecha encerra a conexão. Em stdio: fecha stdin (o servidor bem-comportado
// sai ao ver EOF), espera até 5 s e só então manda Kill. Idempotente.
//
// A ordem importa: o canal de saída é fechado DEPOIS do stdin, para que o
// escritor únidrone não escreva num pipe fechado; e o canal fim é sinalizado
// antes, para que remetentes pendentes desistam no lugar de travar.
func (c *Cliente) Fecha() error {
	c.fechaUma.Do(func() {
		close(c.fim) // quem espera resposta acorda aqui

		if c.stdin != nil {
			_ = c.stdin.Close() // EOF: servidor encerra sozinho
		}
		// c.saida NAO é fechado: tem vários remetentes concorrentes, e fechar
		// canal com remetente ativo é panic em Go. O escritor percebe o fim por
		// c.fim; o canal fica para o GC.

		if c.cmd == nil || c.cmd.Process == nil {
			return
		}
		feito := make(chan error, 1)
		go func() { feito <- c.cmd.Wait() }()
		select {
		case <-feito:
			return // saiu com o EOF — caminho normal
		case <-time.After(prazoMataSub):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-feito
		}
	})
	return nil
}
