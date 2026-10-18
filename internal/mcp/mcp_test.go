package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── servidor MCP falso em HTTP ────────────────────────────────────────────

// fakeMCP serve os três métodos que o cliente usa: initialize, tools/list e
// tools/call. Responde no formato escolhido pelo teste (JSON puro ou SSE).
type fakeMCP struct {
	mu    sync.Mutex
	visto []string // métodos recebidos, em ordem

	respInicializar any
	respFerramentas any
	respChamada     any

	status int // != 0 força este status em todas as respostas (teste do 401)

	sse        bool // emoldura as respostas como eventos SSE
	batch      bool // responde tools/call como batch JSON-RPC
	semContent bool // Content-Type genérico no SSE (força sniffing)
}

// escreve coloca uma resposta no formato pedido pelo teste.
func (f *fakeMCP) escreve(w http.ResponseWriter, r *fakeMCP, resultado any, id any) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  resultado,
	}
	if f.sse {
		b, _ := json.Marshal(msg)
		if !f.semContent {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, ":ping\n\nevent: message\ndata: %s\n\n", b)
		return
	}
	if f.batch {
		b, _ := json.Marshal([]map[string]any{msg})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return
	}
	b, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	status := f.status
	f.mu.Unlock()

	// 401: autenticação recusada antes de qualquer processamento.
	if status != 0 {
		http.Error(w, "unauthorized", status)
		return
	}

	if req.Method != http.MethodPost {
		http.Error(w, "método não suportado", http.StatusMethodNotAllowed)
		return
	}

	var m mensagem
	if err := json.NewDecoder(req.Body).Decode(&m); err != nil {
		http.Error(w, "JSON inválido: "+err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.visto = append(f.visto, m.Method)
	f.mu.Unlock()

	// Notificação não tem id: nada a responder, só um 200/202.
	if !m.temID() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch m.Method {
	case "initialize":
		f.escreve(w, f, f.respInicializar, idParaResposta(m.ID))
	case "tools/list":
		if f.respFerramentas == nil {
			f.respFerramentas = map[string]any{"tools": []any{}}
		}
		f.escreve(w, f, f.respFerramentas, idParaResposta(m.ID))
	case "tools/call":
		if f.respChamada == nil {
			f.respChamada = map[string]any{"content": []any{}}
		}
		f.escreve(w, f, f.respChamada, idParaResposta(m.ID))
	default:
		http.Error(w, "método desconhecido: "+m.Method, http.StatusNotFound)
	}
}

// idParaResposta devolve o id cru do request para a resposta — JSON-RPC exige
// que a resposta carregue o id do request, não um inventado.
func idParaResposta(cru json.RawMessage) json.RawMessage {
	if len(cru) == 0 {
		return json.RawMessage("0")
	}
	return cru
}

// métodosVistos devolve a lista de métodos recebidos (cópia).
func (f *fakeMCP) metodosVistos() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.visto))
	copy(out, f.visto)
	return out
}

// novoFake sobe um httptest.Server e devolve o URL + o fake para inspeção.
func novoFake(t *testing.T, f *fakeMCP) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

// resultadoInicializar é o que um servidor MCP real devolve no initialize.
func resultadoInicializar() map[string]any {
	return map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": true},
		},
		"serverInfo": map[string]any{"name": "fake", "version": "0.1"},
	}
}

// ── testes HTTP ──────────────────────────────────────────────────────────

// Handshake + tools/list com DUAS ferramentas, uma com schema completo.
func TestHTTPHandshakeELista(t *testing.T) {
	f := &fakeMCP{
		respInicializar: resultadoInicializar(),
		respFerramentas: map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "eco",
					"description": "Devolve o que entrou",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"msg": map[string]any{"type": "string"},
						},
						"required": []string{"msg"},
					},
				},
				map[string]any{
					"name":        "soma",
					"description": "Soma dois números",
					// sem inputSchema: o cliente deve inventar um objeto livre
				},
			},
		},
	}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	if got := c.Fonte(); got != "fake" {
		t.Errorf("Fonte() = %q, want %q", got, "fake")
	}
	if got := c.Transporto(); got != "http" {
		t.Errorf("Transporto() = %q, want %q", got, "http")
	}

	especificacoes, err := c.Ferramentas(ctx)
	if err != nil {
		t.Fatalf("Ferramentas: %v", err)
	}
	if len(especificacoes) != 2 {
		t.Fatalf("Ferramentas devolveu %d specs, queria 2", len(especificacoes))
	}
	if especificacoes[0].Name != "eco" || especificacoes[1].Name != "soma" {
		t.Fatalf("nomes = %q, %q; queria eco, soma", especificacoes[0].Name, especificacoes[1].Name)
	}
	if especificacoes[0].Description != "Devolve o que entrou" {
		t.Errorf("descrição = %q", especificacoes[0].Description)
	}

	// O schema da primeira ferramenta tem de ser o do servidor, intacto.
	props, ok := especificacoes[0].Schema["properties"].(map[string]any)
	if !ok || len(props) != 1 {
		t.Fatalf("Schema[properties] = %#v", especificacoes[0].Schema["properties"])
	}
	if _, ok := props["msg"]; !ok {
		t.Errorf("Schema sem a propriedade msg: %#v", props)
	}
	if especificacoes[0].Schema["type"] != "object" {
		t.Errorf("Schema[type] = %v", especificacoes[0].Schema["type"])
	}

	// A segunda, sem schema, recebe o objeto livre.
	if especificacoes[1].Schema["type"] != "object" {
		t.Errorf("Schema padrão[type] = %v, queria object", especificacoes[1].Schema["type"])
	}
	if _, ok := especificacoes[1].Schema["properties"].(map[string]any); !ok {
		t.Errorf("Schema padrão sem properties: %#v", especificacoes[1].Schema)
	}

	// initialize + notifications/initialized + tools/list, nessa ordem.
	vistos := f.metodosVistos()
	queridos := []string{"initialize", "notifications/initialized", "tools/list"}
	if len(vistos) < len(queridos) {
		t.Fatalf("métodos vistos = %v", vistos)
	}
	for i, q := range queridos {
		if vistos[i] != q {
			t.Fatalf("método %d = %q, queria %q (vistos: %v)", i, vistos[i], q, vistos)
		}
	}

	// O handshake gravou o que o servidor negociou.
	info := c.InfoServidor()
	if info.ServerInfo.Name != "fake" || info.ProtocolVersion != "2025-06-18" {
		t.Errorf("InfoServidor = %+v", info)
	}
	if caps := c.Capacidades(); len(caps) == 0 {
		t.Errorf("Capabilities vazia; queria o mapa do servidor")
	}
}

// tools/call com eco dos argumentos, via Spec (o caminho que o runtime usa).
func TestHTTPChamarEcoaArgumentos(t *testing.T) {
	f := &fakeMCP{
		respInicializar: resultadoInicializar(),
		respFerramentas: map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "eco",
					"description": "Devolve o que entrou",
					"inputSchema": map[string]any{"type": "object"},
				},
			},
		},
		respChamada: map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "primeiro"},
				map[string]any{"type": "text", "text": "segundo"},
			},
		},
	}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	// Múltiplos blocos de texto são concatenados.
	out, err := c.Chamar(context.Background(), "eco", `{"msg":"oi"}`)
	if err != nil {
		t.Fatalf("Chamar: %v", err)
	}
	if out != "primeiro\nsegundo" {
		t.Errorf("Chamar = %q, queria %q", out, "primeiro\nsegundo")
	}

	// Argumentos vazios e "null" ambos viram {} sem estourar.
	if _, err := c.Chamar(context.Background(), "eco", ""); err != nil {
		t.Errorf("Chamar com args vazios: %v", err)
	}
	if _, err := c.Chamar(context.Background(), "eco", "null"); err != nil {
		t.Errorf("Chamar com args null: %v", err)
	}

	// Argumentos que não são JSON têm de dar erro de UMA linha, não pânico.
	if _, err := c.Chamar(context.Background(), "eco", "{"); err == nil {
		t.Errorf("Chamar com JSON quebrado deveria falhar")
	} else if strings.Contains(err.Error(), "\n") {
		t.Errorf("erro multi-linha: %q", err.Error())
	}
}

// isError do servidor vira erro Go com a mensagem DELE.
func TestHTTPChamarMapeiaIsError(t *testing.T) {
	f := &fakeMCP{
		respInicializar: resultadoInicializar(),
		respFerramentas: map[string]any{
			"tools": []any{
				map[string]any{"name": "quebra", "inputSchema": map[string]any{"type": "object"}},
			},
		},
		respChamada: map[string]any{
			"isError": true,
			"content": []any{
				map[string]any{"type": "text", "text": "falha catastrófica no servidor"},
			},
		},
	}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	out, err := c.Chamar(context.Background(), "quebra", "{}")
	if err == nil {
		t.Fatalf("Chamar deveria devolver erro quando isError=true")
	}
	if !strings.Contains(err.Error(), "falha catastrófica no servidor") {
		t.Errorf("erro = %q; queria a mensagem do servidor", err.Error())
	}
	if !strings.Contains(out, "falha catastrófica no servidor") {
		t.Errorf("saída = %q; queria o texto do servidor junto", out)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("erro multi-linha: %q", err.Error())
	}
}

// 401: handshake falha com status e mensagem úteis.
func TestHTTP401NoHandshake(t *testing.T) {
	f := &fakeMCP{status: http.StatusUnauthorized}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err == nil {
		t.Fatalf("Abre deveria falhar com 401")
	} else {
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("erro = %q; queria o status 401", err.Error())
		}
		if !strings.Contains(err.Error(), "unauthorized") {
			t.Errorf("erro = %q; queria o corpo do servidor", err.Error())
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("erro multi-linha: %q", err.Error())
		}
	}
	c.Fecha() // idempotente, mesmo sem conexão de fato
}

// Resposta em SSE: o cliente extrai o JSON-RPC das linhas data:.
func TestHTTPRespostaSSE(t *testing.T) {
	f := &fakeMCP{
		sse:             true,
		respInicializar: resultadoInicializar(),
		respFerramentas: map[string]any{
			"tools": []any{
				map[string]any{"name": "sse", "inputSchema": map[string]any{"type": "object"}},
			},
		},
		respChamada: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "resposta via stream"}},
		},
	}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre (SSE): %v", err)
	}
	defer c.Fecha()

	especificacoes, err := c.Ferramentas(ctx)
	if err != nil {
		t.Fatalf("Ferramentas (SSE): %v", err)
	}
	if len(especificacoes) != 1 || especificacoes[0].Name != "sse" {
		t.Fatalf("Ferramentas (SSE) = %+v", especificacoes)
	}
	out, err := c.Chamar(ctx, "sse", `{"x":1}`)
	if err != nil {
		t.Fatalf("Chamar (SSE): %v", err)
	}
	if out != "resposta via stream" {
		t.Errorf("Chamar (SSE) = %q", out)
	}
}

// SSE com Content-Type genérico: o cliente descobre pelo corpo.
func TestHTTPRespostaSSESemContentType(t *testing.T) {
	f := &fakeMCP{
		sse:             true,
		semContent:      true,
		respInicializar: resultadoInicializar(),
		respFerramentas: map[string]any{
			"tools": []any{map[string]any{"name": "sniff", "inputSchema": map[string]any{"type": "object"}}},
		},
	}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre (SSE sniffado): %v", err)
	}
	defer c.Fecha()
	if _, err := c.Ferramentas(context.Background()); err != nil {
		t.Fatalf("Ferramentas (SSE sniffado): %v", err)
	}
}

// Resposta em batch JSON-RPC: a resposta de id casante é achada no array.
func TestHTTPRespostaBatch(t *testing.T) {
	f := &fakeMCP{
		batch:           true,
		respInicializar: resultadoInicializar(),
		respChamada: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "veio num batch"}},
		},
	}
	_, url := novoFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre (batch): %v", err)
	}
	defer c.Fecha()
	out, err := c.Chamar(context.Background(), "qualquer", "{}")
	if err != nil {
		t.Fatalf("Chamar (batch): %v", err)
	}
	if out != "veio num batch" {
		t.Errorf("Chamar (batch) = %q", out)
	}
}

// A conexão HTTP tem de anunciar Accept conforme a especificação MCP exige.
func TestHTTPHeaderAcceptExigido(t *testing.T) {
	var aceito, ua, contentType string
	f := &fakeMCP{respInicializar: resultadoInicializar()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		aceito = req.Header.Get("Accept")
		ua = req.Header.Get("User-Agent")
		contentType = req.Header.Get("Content-Type")
		f.ServeHTTP(w, req)
	}))
	defer srv.Close()

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: srv.URL, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	if !strings.Contains(aceito, "application/json") || !strings.Contains(aceito, "text/event-stream") {
		t.Errorf("Accept = %q; a especificação MCP exige ambos", aceito)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if !strings.Contains(ua, "barroso-harness") {
		t.Errorf("User-Agent = %q; queria o do dealer", ua)
	}
}

// Headers configurados no Servidor chegam ao request.
func TestHTTPHeadersDoServidor(t *testing.T) {
	var authVisto string
	f := &fakeMCP{respInicializar: resultadoInicializar()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authVisto = req.Header.Get("Authorization")
		f.ServeHTTP(w, req)
	}))
	defer srv.Close()

	c, err := Novo(Servidor{
		Nome:    "fake",
		Tipo:    "http",
		URL:     srv.URL,
		Timeout: 5,
		Headers: map[string]string{"Authorization": "Bearer teste-token"},
	})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()
	if authVisto != "Bearer teste-token" {
		t.Errorf("Authorization = %q", authVisto)
	}
}

// Servidor URL morto: erro de UMA linha, sem pânico.
func TestHTTPServidorInacessivel(t *testing.T) {
	c, err := Novo(Servidor{Nome: "morto", Tipo: "http", URL: "http://127.0.0.1:1/mcp", Timeout: 2})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Abre(ctx); err == nil {
		t.Fatalf("Abre deveria falhar com servidor inacessível")
	} else if strings.Contains(err.Error(), "\n") {
		t.Errorf("erro multi-linha: %q", err.Error())
	}
}

// ── validação da config ──────────────────────────────────────────────────

func TestValidacaoServidor(t *testing.T) {
	casos := []struct {
		nome       string
		serv       Servidor
		deveFalhar bool // true = Novo deve devolver erro
	}{
		{"stdio valido", Servidor{Nome: "s", Tipo: "stdio", Command: "go"}, false},
		{"http valido", Servidor{Nome: "h", Tipo: "http", URL: "http://x"}, false},
		{"http com maiuscula", Servidor{Nome: "h", Tipo: "HTTP", URL: "http://x"}, false},
		{"streamable-http", Servidor{Nome: "h", Tipo: "streamable-http", URL: "http://x"}, false},
		{"sem nome", Servidor{Tipo: "http", URL: "http://x"}, true},
		{"tipo desconhecido", Servidor{Nome: "x", Tipo: "websocket", URL: "http://x"}, true},
		{"stdio sem comando", Servidor{Nome: "x", Tipo: "stdio"}, true},
		{"http sem url", Servidor{Nome: "x", Tipo: "http"}, true},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			_, err := Novo(c.serv)
			if !c.deveFalhar && err != nil {
				t.Errorf("Novo(%+v) falhou: %v", c.serv, err)
			}
			if c.deveFalhar && err == nil {
				t.Errorf("Novo(%+v) deveria falhar", c.serv)
			}
		})
	}
}

// envSlice classifica e formata pares K=V.
func TestEnvSlice(t *testing.T) {
	got := envSlice(map[string]string{"B": "2", "A": "1"})
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("envSlice = %v; queria [A=1 B=2] ordenado", got)
	}
	if envSlice(nil) != nil {
		t.Errorf("envSlice(nil) deveria ser nil")
	}
}

// Timeout por servidor: default quando não configurado.
func TestTimeoutDoServidor(t *testing.T) {
	if got := (Servidor{Timeout: 30}).timeoutDuracao(); got != 30*time.Second {
		t.Errorf("timeoutDuracao = %v", got)
	}
	if got := (Servidor{}).timeoutDuracao(); got != timeoutPorChamada {
		t.Errorf("timeoutDuracao padrão = %v; queria %v", got, timeoutPorChamada)
	}
	if got := (Servidor{Timeout: -5}).timeoutDuracao(); got != timeoutPorChamada {
		t.Errorf("timeoutDuracao negativo = %v; queria o padrão", got)
	}
}

// ── correlação de ids ────────────────────────────────────────────────────

func TestCorrelacaoDeIDs(t *testing.T) {
	casos := []struct {
		a, b string
		quer bool
	}{
		{"1", "1", true},
		{"1", "2", false},
		{`"1"`, "1", true}, // número vs string com mesmo conteúdo
		{`"abc"`, `"abc"`, true},
		{"", "", true},
		{"1", "", false},
		{"null", "null", true},
	}
	for _, c := range casos {
		got := mesmoID(json.RawMessage(c.a), json.RawMessage(c.b))
		if got != c.quer {
			t.Errorf("mesmoID(%s, %s) = %v; queria %v", c.a, c.b, got, c.quer)
		}
	}
}

// ── conector ─────────────────────────────────────────────────────────────

func TestConectarTodosErroPorServidor(t *testing.T) {
	fOk := &fakeMCP{respInicializar: resultadoInicializar(), respFerramentas: map[string]any{"tools": []any{}}}
	_, urlOk := novoFake(t, fOk)

	ctx := context.Background()
	clientes, erros := ConectarTodos(ctx, []Servidor{
		{Nome: "bom", Tipo: "http", URL: urlOk, Timeout: 5},
		{Nome: "ruim-sem-url", Tipo: "http", Timeout: 5},
		{Nome: "ruim-morto", Tipo: "http", URL: "http://127.0.0.1:1/mcp", Timeout: 1},
	})

	// Um servidor bom conecta; os ruins voltam como itens de erro.
	if len(clientes) != 1 {
		t.Fatalf("clientes = %d; queria 1 (só o bom)", len(clientes))
	}
	if clientes[0].Fonte() != "bom" {
		t.Errorf("cliente = %q; queria bom", clientes[0].Fonte())
	}
	if len(erros) != 2 {
		t.Fatalf("erros = %d; queria 2 (%+v)", len(erros), erros)
	}
	for _, e := range erros {
		if e.Nome == "" {
			t.Errorf("ErroServidor sem nome: %+v", e)
		}
		if e.Err == nil {
			t.Errorf("ErroServidor sem erro: %+v", e)
		}
		msg := e.Error()
		if strings.Contains(msg, "\n") {
			t.Errorf("erro multi-linha: %q", msg)
		}
		if !strings.Contains(msg, e.Nome) {
			t.Errorf("erro %q não cita o nome do servidor", msg)
		}
	}

	// Ordem estável: os erros vêm na ordem dos servidores dados.
	if erros[0].Nome != "ruim-sem-url" || erros[1].Nome != "ruim-morto" {
		t.Errorf("ordem dos erros = %q, %q; queria ruim-sem-url, ruim-morto",
			erros[0].Nome, erros[1].Nome)
	}

	// ErroServidor abre para errors.Is/As no erro original.
	alvo := erros[0]
	var como ErroServidor
	if !errosComo(alvo, &como) {
		t.Errorf("ErroServidor não respeita Unwrap")
	}
}

func TestConectarTodosVazio(t *testing.T) {
	clientes, erros := ConectarTodos(context.Background(), nil)
	if clientes != nil || erros != nil {
		t.Errorf("ConectarTodos(nil) = %v, %v; queria nil, nil", clientes, erros)
	}
}

func TestFechaTodos(t *testing.T) {
	f := &fakeMCP{respInicializar: resultadoInicializar()}
	_, url := novoFake(t, f)
	c, err := Novo(Servidor{Nome: "x", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	// FechaTodos é idempotente no cliente (Fecha já é).
	if err := FechaTodos([]*Cliente{c, nil}); err != nil {
		t.Errorf("FechaTodos: %v", err)
	}
	if err := FechaTodos([]*Cliente{c}); err != nil {
		t.Errorf("FechaTodos repetido: %v", err)
	}
}

// errosComo confere que ErroServidor abre para o erro original (Unwrap).
func errosComo(err error, alvo *ErroServidor) bool {
	var e ErroServidor
	if !asErroServidor(err, &e) {
		return false
	}
	*alvo = e
	return true
}

// asErroServidor é o errors.As sem importar errors neste arquivo de teste
// (mantém o conjunto de importações enxuto).
func asErroServidor(err error, alvo *ErroServidor) bool {
	for err != nil {
		if e, ok := err.(ErroServidor); ok {
			*alvo = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// garantir que o corpo da resposta não vaze no erro: só o necessário entra.
func TestResumoHTTP(t *testing.T) {
	if got := resumoHTTP([]byte("  linha\n\noutra  "), 100); got != "linha outra" {
		t.Errorf("resumoHTTP = %q; queria uma linha só", got)
	}
	if got := resumoHTTP(nil, 100); got != "(corpo vazio)" {
		t.Errorf("resumoHTTP(vazio) = %q", got)
	}
	longo := bytes.Repeat([]byte("x"), 500)
	if got := resumoHTTP(longo, 10); len([]rune(got)) > 11 {
		t.Errorf("resumoHTTP não cortou: %d runas", len([]rune(got)))
	}
}
