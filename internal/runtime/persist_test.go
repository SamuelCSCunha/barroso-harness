// Testes de ponta a ponta da persistência da sessão: gravar no log, retomar
// num processo novo e recuperar conteúdo que o guardrail tirou da janela.
package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func lojaTeste(t *testing.T) *store.Store {
	t.Helper()
	loja, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("abrir store: %v", err)
	}
	t.Cleanup(func() { _ = loja.Fechar() })
	return loja
}

func cfgFake(t *testing.T, url string) config.Config {
	t.Helper()
	return config.Config{BaseURL: url, Model: "fake", APIKey: "k", Janela: 100_000}
}

// ── NewSessionWithStore / Attach / ID ─────────────────────────────────────

func TestNewSessionWithStoreIndexaEPersiste(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101090000-abcd"
	s := NewSessionWithStore(config.Config{}, loja, id)
	if s.ID() != id {
		t.Fatalf("ID() = %q, esperado %q", s.ID(), id)
	}
	if !s.Persistente() {
		t.Fatal("sessão com store deveria ser persistente")
	}
	// A sessão foi indexada no log, mas sem nenhum evento ainda.
	if n, _ := loja.ContarEventos(id); n != 0 {
		t.Fatalf("sessão nova com %d eventos", n)
	}
	if _, err := loja.GetSession(id); err != nil {
		t.Fatalf("sessão não indexada: %v", err)
	}
}

func TestAttachDepoisDeNewSession(t *testing.T) {
	loja := lojaTeste(t)
	// NewSession continua funcionando SEM banco (o modo -p e os testes).
	s := NewSession(config.Config{})
	if s.Persistente() {
		t.Fatal("NewSession não deveria ter persistência")
	}
	if s.ID() != "" {
		t.Fatalf("ID() sem store = %q", s.ID())
	}
	// Attach liga depois: é o caminho do REPL quando o banco abre em best-effort.
	s.Attach(loja, "bh-20240101090000-ef01")
	if !s.Persistente() || s.ID() != "bh-20240101090000-ef01" {
		t.Fatalf("Attach não ligou: persistente=%v id=%q", s.Persistente(), s.ID())
	}
}

func TestAttachIgnoraEntradaRuim(t *testing.T) {
	loja := lojaTeste(t)
	s := NewSession(config.Config{})
	s.Attach(nil, "bh-x") // store nil
	s.Attach(loja, "   ") // id vazio
	if s.Persistente() || s.ID() != "" {
		t.Fatalf("Attach com entrada ruim ligou persistência: %v %q", s.Persistente(), s.ID())
	}
}

// ── Send gravando eventos ─────────────────────────────────────────────────

// provider de um turno só: texto puro, sem tool.
func providerTexto(t *testing.T, texto string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": texto},
		}}})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestSendGravaUserMessageEAssistant(t *testing.T) {
	srv := providerTexto(t, "resposta do modelo")
	defer srv.Close()
	loja := lojaTeste(t)
	const id = "bh-20240101100000-0001"

	s := NewSessionWithStore(cfgFake(t, srv.URL), loja, id)
	var sb strings.Builder
	if err := s.Send(context.Background(), RendererDe(&sb), t.TempDir(), "oi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	eventos, err := loja.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventos) != 2 {
		t.Fatalf("eventos = %d; esperados 2 (user_message + assistant): %+v", len(eventos), eventos)
	}
	if eventos[0].Kind != "user_message" {
		t.Fatalf("primeiro evento = %q, esperado user_message", eventos[0].Kind)
	}
	if !strings.Contains(eventos[0].Payload, `"content":"oi"`) {
		t.Fatalf("payload do user_message sem o pedido: %s", eventos[0].Payload)
	}
	if eventos[1].Kind != "assistant" {
		t.Fatalf("segundo evento = %q, esperado assistant", eventos[1].Kind)
	}
	if !strings.Contains(eventos[1].Payload, `"content":"resposta do modelo"`) {
		t.Fatalf("payload do assistant sem o texto: %s", eventos[1].Payload)
	}
}

func TestSendSemStoreNaoGrava(t *testing.T) {
	srv := providerTexto(t, "texto")
	defer srv.Close()
	loja := lojaTeste(t)
	const id = "bh-20240101100000-0002"
	// Sessão SEM store: o turno funciona e o log fica intacto.
	s := NewSession(cfgFake(t, srv.URL))
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "oi"); err != nil {
		t.Fatalf("Send sem store: %v", err)
	}
	if n, _ := loja.ContarEventos(id); n != 0 {
		t.Fatalf("gravaram %d eventos numa sessão sem store", n)
	}
}

func TestClearHistoryGravaSessionReset(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101100000-0003"
	s := NewSessionWithStore(config.Config{}, loja, id)
	s.ClearHistory()
	eventos, err := loja.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventos) != 1 || eventos[0].Kind != "session_reset" {
		t.Fatalf("ClearHistory deveria gravar session_reset, veio %+v", eventos)
	}
}

// ── Resume reconstruindo o transcript ─────────────────────────────────────

func TestResumeReconstroiTranscript(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101110000-0001"
	// Primeiro processo: conversa com tool call + resultado. Send só grava
	// assistant quando há texto confirmado (o evento vazio de uma rodada que
	// terminou em tool call nunca entra no log), então a conversa aqui usa
	// essa mesma regra.
	s := NewSessionWithStore(config.Config{}, loja, id)
	eventos := []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "leia o arquivo"}},
		{"tool_call", map[string]any{
			"role": "tool_call", "tool_id": "call_1", "tool_name": "file_read",
			"tool_args": `{"path":"x.txt"}`,
		}},
		{"tool_result", map[string]any{
			"role": "tool_result", "tool_id": "call_1", "tool_name": "file_read",
			"content": "1: conteúdo",
		}},
		{"assistant", map[string]any{"role": "assistant", "content": "pronto, li"}},
	}
	for _, e := range eventos {
		s.registrar(e.kind, e.payload)
	}

	// Segundo processo: mesma sessão, transcript reconstruído do log.
	s2 := NewSessionWithStore(config.Config{}, loja, id)
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := s2.History()
	if len(h) != 4 {
		t.Fatalf("história reconstruída com %d mensagens; esperadas 4:\n%+v", len(h), h)
	}
	if h[0].Role != "user" || h[0].Conteudo != "leia o arquivo" {
		t.Fatalf("mensagem 0 errada: %+v", h[0])
	}
	if h[1].Role != "tool_call" || h[1].ToolName != "file_read" || h[1].ToolArgs != `{"path":"x.txt"}` {
		t.Fatalf("tool_call não voltou íntegro: %+v", h[1])
	}
	if h[2].Role != "tool_result" || h[2].Conteudo != "1: conteúdo" {
		t.Fatalf("tool_result não voltou íntegro: %+v", h[2])
	}
	if h[3].Role != "assistant" || h[3].Conteudo != "pronto, li" {
		t.Fatalf("assistant final errado: %+v", h[3])
	}
}

// Eventos de observabilidade (guardrail, session_reset, metadado) NÃO entram
// no transcript: são comentários sobre a conversa, não a conversa.
func TestResumeIgnoraEventosDeObservabilidade(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101110000-0009"
	s := NewSessionWithStore(config.Config{}, loja, id)
	s.registrar("user_message", map[string]any{"role": "user", "content": "oi"})
	s.registrar("guardrail", map[string]any{"tipo": "proativa", "janela": 100000})
	s.registrar("session_reset", map[string]any{"role": "system", "content": "história limpa"})
	s.registrar("assistant", map[string]any{"role": "assistant", "content": "olá"})

	s2 := NewSessionWithStore(config.Config{}, loja, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 2 {
		t.Fatalf("história = %d mensagens; esperadas 2 (só conteúdo): %+v", len(h), h)
	}
	if h[0].Role != "user" || h[1].Role != "assistant" {
		t.Fatalf("ordem/papéis errados: %+v", h)
	}
}

func TestResumeSemEventosDeixaVazio(t *testing.T) {
	loja := lojaTeste(t)
	s := NewSessionWithStore(config.Config{}, loja, "bh-20240101110000-0002")
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume de sessão sem eventos: %v", err)
	}
	if len(s.History()) != 0 {
		t.Fatalf("história devia estar vazia, veio %d", len(s.History()))
	}
}

func TestResumeReidrataBlob(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101110000-0003"
	grande := strings.Repeat("linha de saída de tool\n", 2000) // > 16 KiB

	s := NewSessionWithStore(config.Config{}, loja, id)
	s.registrarResultado("call_9", "terminal_run", grande, false)

	// O evento gravado é curto: preview + referência ao blob.
	eventos, _ := loja.ListEvents(id, 0, 0)
	if len(eventos) != 1 {
		t.Fatalf("eventos = %d", len(eventos))
	}
	if !strings.Contains(eventos[0].Payload, `"blob":`) {
		t.Fatalf("resultado grande não virou blob: %s", eventos[0].Payload)
	}
	if len(eventos[0].Payload) > 1024 {
		t.Fatalf("payload com blob ficou com %d bytes", len(eventos[0].Payload))
	}

	// Resume devolve o conteúdo INTEIRO — é a promessa da reidratação.
	s2 := NewSessionWithStore(config.Config{}, loja, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 1 {
		t.Fatalf("história = %d mensagens", len(h))
	}
	if h[0].Conteudo != grande {
		t.Fatalf("blob não reidratou: vieram %d de %d bytes", len(h[0].Conteudo), len(grande))
	}
}

// ── Search / Retrieve ─────────────────────────────────────────────────────

func TestSearchERetrieve(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101120000-0001"
	s := NewSessionWithStore(config.Config{}, loja, id)
	s.registrar("user_message", map[string]any{"role": "user", "content": "preciso do caminho do FicheiroX"})
	seqTool, _ := loja.StoreEvent(id, "tool_result", map[string]any{
		"role": "tool_result", "tool_name": "file_read", "content": "C:\\dados\\FicheiroX.txt",
	})

	// Busca sem distinguir maiúscula, sobre o campo content.
	hits := s.Search("ficheirox", 10)
	if len(hits) == 0 {
		t.Fatal("Search não achou nada")
	}
	var achouTool bool
	for _, h := range hits {
		if h.Kind == "tool_result" {
			achouTool = true
		}
	}
	if !achouTool {
		t.Fatalf("Search não achou o tool_result: %+v", hits)
	}

	// Retrieve devolve o evento completo.
	e, err := s.Retrieve(seqTool)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if e.Seq != seqTool {
		t.Fatalf("Retrieve devolveu seq %d, esperado %d", e.Seq, seqTool)
	}
	if !strings.Contains(e.Payload, `C:\\dados\\FicheiroX.txt`) {
		t.Fatalf("Retrieve perdeu o conteúdo: %s", e.Payload)
	}
	if _, err := s.Retrieve(99999); err == nil {
		t.Fatal("Retrieve de seq inexistente deveria falhar")
	}
}

func TestSearchSemPersistenciaDevolveNil(t *testing.T) {
	s := NewSession(config.Config{})
	if hits := s.Search("qualquer", 10); hits != nil {
		t.Fatalf("Search sem store devolveu %+v", hits)
	}
	if _, err := s.Retrieve(1); err == nil {
		t.Fatal("Retrieve sem store deveria falhar")
	}
	// Resume sem store é no-op, não erro.
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume sem store: %v", err)
	}
}

// ── as tools do modelo ────────────────────────────────────────────────────

func TestSessaoSpecsBuscaERetrieve(t *testing.T) {
	loja := lojaTeste(t)
	const id = "bh-20240101130000-0001"
	s := NewSessionWithStore(config.Config{}, loja, id)
	s.registrar("tool_result", map[string]any{
		"role": "tool_result", "tool_name": "terminal_run",
		"content": "exit_code: 0\ngit status --short",
	})

	specs := SessaoSpecs(s)
	if len(specs) != 2 {
		t.Fatalf("SessaoSpecs devolveu %d specs; esperadas 2", len(specs))
	}
	porNome := map[string]tools.Spec{}
	for _, sp := range specs {
		porNome[sp.Name] = sp
	}
	busca, ok := porNome["session_search"]
	if !ok {
		t.Fatal("session_search não está nas specs")
	}
	if !strings.Contains(strings.ToLower(busca.Description), "compact") {
		t.Fatalf("descrição de session_search não menciona compactação:\n%s", busca.Description)
	}
	rtr, ok := porNome["session_retrieve"]
	if !ok {
		t.Fatal("session_retrieve não está nas specs")
	}
	if !strings.Contains(strings.ToLower(rtr.Description), "seq") {
		t.Fatalf("descrição de session_retrieve não menciona seq:\n%s", rtr.Description)
	}
	// Schemas com os parâmetros prometidos.
	if _, ok := busca.Schema["properties"].(map[string]any)["query"]; !ok {
		t.Fatal("session_search sem parâmetro query")
	}
	if _, ok := busca.Schema["properties"].(map[string]any)["limit"]; !ok {
		t.Fatal("session_search sem parâmetro limit")
	}
	if _, ok := rtr.Schema["properties"].(map[string]any)["seq"]; !ok {
		t.Fatal("session_retrieve sem parâmetro seq")
	}
	if _, ok := rtr.Schema["properties"].(map[string]any)["limit"]; !ok {
		t.Fatal("session_retrieve sem parâmetro limit")
	}

	// Exec de session_search acha o conteúdo gravado.
	saida, err := busca.Exec(nil, t.TempDir(), json.RawMessage(`{"query":"git status"}`))
	if err != nil {
		t.Fatalf("exec session_search: %v", err)
	}
	if !strings.Contains(saida, "git status") {
		t.Fatalf("session_search não devolveu o trecho:\n%s", saida)
	}

	// Exec de session_retrieve devolve o evento inteiro.
	saida, err = rtr.Exec(nil, t.TempDir(), json.RawMessage(`{"seq":1}`))
	if err != nil {
		t.Fatalf("exec session_retrieve: %v", err)
	}
	if !strings.Contains(saida, "terminal_run") {
		t.Fatalf("session_retrieve não devolveu o evento:\n%s", saida)
	}
}

func TestSessaoSpecsSemPersistenciaExplica(t *testing.T) {
	s := NewSession(config.Config{})
	for _, sp := range SessaoSpecs(s) {
		args := `{}`
		if sp.Name == "session_search" {
			args = `{"query":"x"}`
		} else if sp.Name == "session_retrieve" {
			args = `{"seq":1}`
		}
		saida, err := sp.Exec(nil, t.TempDir(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s sem store devolveu erro: %v", sp.Name, err)
		}
		if !strings.Contains(saida, "persistência") {
			t.Fatalf("%s sem store deveria explicar que não há log:\n%s", sp.Name, saida)
		}
	}
}

// Send inclui as tools de sessão no catálogo enviado ao provider.
func TestSendExpoeToolsDeSessaoNoSchema(t *testing.T) {
	var viuBusca, viuRetrieve bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, tl := range req.Tools {
			switch tl.Function.Name {
			case "session_search":
				viuBusca = true
			case "session_retrieve":
				viuRetrieve = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	loja := lojaTeste(t)
	s := NewSessionWithStore(cfgFake(t, srv.URL), loja, "bh-20240101130000-0002")
	_ = s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "oi")
	if !viuBusca || !viuRetrieve {
		t.Fatalf("provider não viu as tools de sessão (busca=%v retrieve=%v)", viuBusca, viuRetrieve)
	}
}
