// Package subagent — testes end-to-end com provider fake (SSE), no mesmo
// formato de runtime_test.go: pedaços com choices[].delta.content, chunk final
// com finish_reason e usage, e "data: [DONE]".
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── provider fake ──────────────────────────────────────────────────────────

// reqVista é o que o fake precisa enxergar do request: as tools oferecidas e a
// quantidade de mensagens (para saber se é a primeira rodada do sub-agente).
type reqVista struct {
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Messages []struct {
		Role string `json:"role"`
	} `json:"messages"`
}

// fakeProvider atende /chat/completions com um roteiro. resposta(o) devolve o
// texto da rodada; enquanto houver pedido de tool sem resultado de tool ainda,
// ele pede uma tool built-in de verdade (list_dir) para exercitar o loop.
type fakeProvider struct {
	mu        sync.Mutex
	chamadas  int
	vistas    []reqVista
	texto     string
	atraso    time.Duration
	pedirTool bool // se true, a 1ª rodada pede list_dir antes de responder
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var vista reqVista
	_ = json.NewDecoder(r.Body).Decode(&vista)
	f.mu.Lock()
	f.chamadas++
	f.vistas = append(f.vistas, vista)
	n := f.chamadas
	texto, atraso, pedirTool := f.texto, f.atraso, f.pedirTool
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	emitir := func(p any) {
		b, _ := json.Marshal(p)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}

	temResultadoTool := false
	for _, m := range vista.Messages {
		if m.Role == "tool" {
			temResultadoTool = true
		}
	}

	// Rodada 1 pede uma built-in; rodada 2 (já com tool_result) responde texto.
	if pedirTool && n == 1 && !temResultadoTool {
		emitir(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{
					"name":      "list_dir",
					"arguments": `{"depth":1}`,
				},
			}}},
		}}})
	} else {
		if atraso > 0 {
			time.Sleep(atraso)
		}
		for _, p := range []string{texto} {
			emitir(map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"content": p},
			}}})
		}
	}
	emitir(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 42, "completion_tokens": 7},
	})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (f *fakeProvider) Vistas() []reqVista {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reqVista(nil), f.vistas...)
}

func novoServidor(t *testing.T, texto string, atraso time.Duration, pedirTool bool) (*fakeProvider, config.Config) {
	t.Helper()
	f := &fakeProvider{texto: texto, atraso: atraso, pedirTool: pedirTool}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := config.Config{
		CurrentProvider: "fake",
		Providers: map[string]*config.Provider{
			"fake": {BaseURL: srv.URL, APIKey: "k"},
		},
		Model:     "fake-model",
		Janela:    100_000,
		MaxTokens: 512,
	}
	return f, cfg
}

// esperarEstado consulta até que a tarefa chegue a um estado terminal (ou o
// prazo estoure). Evita sleep fixo — a suíte precisa ser rápida e estável.
func esperarEstado(t *testing.T, g *Gerente, id string, aceita ...string) Tarefa {
	t.Helper()
	prazo := time.Now().Add(5 * time.Second)
	for time.Now().Before(prazo) {
		cp, ok := g.snapshot(id)
		if !ok {
			t.Fatalf("tarefa %s não existe", id)
		}
		for _, a := range aceita {
			if cp.Estado == a {
				return cp
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	cp, _ := g.snapshot(id)
	t.Fatalf("tarefa %s não chegou a %v (estado atual: %s, erro: %s)", id, aceita, cp.Estado, cp.Erro)
	return Tarefa{}
}

// executarTool roda uma spec do catálogo com o argumento JSON dado.
func executarTool(t *testing.T, s tools.Spec, args string) (string, error) {
	t.Helper()
	cwd := t.TempDir()
	return s.Exec(context.Background(), cwd, []byte(args))
}

// ── Testes ─────────────────────────────────────────────────────────────────

// Test A: o sub-agente roda o loop completo (tool + texto), o resultado volta
// pelo task_result e — invariante central — o provider NUNCA vê uma tool de
// delegação (task_spawn/task_status/task_result) no catálogo enviado.
func TestSpawnExecutaEResultadoVoltaSemToolsDeDelegacao(t *testing.T) {
	f, cfg := novoServidor(t, "tarefa feita", 0, true)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), especStatus(g, "task"), especResult(g, "task"))

	nomos := map[string]tools.Spec{}
	for _, s := range specs {
		nomos[s.Name] = s
	}
	if len(nomos) != 3 {
		t.Fatalf("esperava 3 tools, vieram %d", len(specs))
	}
	for _, nome := range []string{"task_spawn", "task_status", "task_result"} {
		if _, ok := nomos[nome]; !ok {
			t.Fatalf("tool %q ausente do catálogo: %v", nome, chaves(nomos))
		}
	}

	saida, err := executarTool(t, nomos["task_spawn"], `{"prompt":"liste o diretório e responda"}`)
	if err != nil {
		t.Fatalf("spawn falhou: %v", err)
	}
	if !strings.Contains(saida, "disparado(s): t1") {
		t.Fatalf("resposta do spawn inesperada: %q", saida)
	}

	var id string
	for _, tk := range g.Tarefas() {
		id = tk.ID
	}
	if id != "t1" {
		t.Fatalf("id determinístico deveria ser t1, veio %q", id)
	}

	cp := esperarEstado(t, g, id, Completed, Failed)
	if cp.Estado != Completed {
		t.Fatalf("tarefa deveria completar, estado %s erro %q", cp.Estado, cp.Erro)
	}
	if cp.Resultado != "tarefa feita" {
		t.Fatalf("resultado inesperado: %q", cp.Resultado)
	}
	if len(cp.Transcricao()) != 1 || !strings.Contains(cp.Transcricao()[0], "list_dir") {
		t.Fatalf("rastro de tools deveria ter list_dir: %v", cp.Transcricao())
	}

	// Resultado pela tool.
	res, err := executarTool(t, nomos["task_result"], fmt.Sprintf(`{"id":%q}`, id))
	if err != nil {
		t.Fatalf("result falhou: %v", err)
	}
	if res != "tarefa feita" {
		t.Fatalf("task_result deveria devolver o texto final, veio %q", res)
	}

	// INVARIANTE: nenhum request ao provider levou tool de delegação.
	for i, v := range f.Vistas() {
		if len(v.Tools) == 0 {
			t.Fatalf("request %d não ofereceu tools — o filho deveria ter as built-in", i)
		}
		for _, tl := range v.Tools {
			nome := tl.Function.Name
			if strings.HasPrefix(nome, "task_") {
				t.Fatalf("request %d ofereceu %q: sub-agente NÃO pode ter tools de delegação", i, nome)
			}
		}
		if !temBuiltin(v, "list_dir") {
			t.Fatalf("request %d deveria oferecer as built-in do runtime (list_dir entre elas)", i)
		}
	}
}

// Test B: sem teto — N prompts numa chamada disparam N sub-agentes, todos
// rodando ao mesmo tempo, e o pai é avisado quando terminam.
func TestSpawnVariosSemTetoEAvisa(t *testing.T) {
	_, cfg := novoServidor(t, "ok", 400*time.Millisecond, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	avisos := make(chan struct{}, 16)
	g.AoTerminar(func() { avisos <- struct{}{} })
	specs := append(Especificacoes(g, cfg), especStatus(g, "task"), especResult(g, "task"))
	var spawn tools.Spec
	for _, s := range specs {
		if s.Name == "task_spawn" {
			spawn = s
		}
	}
	saida, err := executarTool(t, spawn, `{"prompts":["a","b","c","d","e"]}`)
	if err != nil || !strings.Contains(saida, "5 sub-agente(s) disparado(s): t1, t2, t3, t4, t5") {
		t.Fatalf("spawn = %q %v", saida, err)
	}
	rodando := 0
	for _, tk := range g.Fotos() {
		if tk.Estado == Running {
			rodando++
		}
	}
	if rodando != 5 {
		t.Fatalf("todas deveriam rodar juntas, rodando=%d", rodando)
	}
	if g.Ativas() != 5 {
		t.Fatalf("ativas = %d", g.Ativas())
	}
	entregues := 0
	prazo := time.After(5 * time.Second)
	for entregues < 5 {
		select {
		case <-avisos:
			entregues += len(g.Terminadas())
		case <-prazo:
			t.Fatalf("só %d entregues", entregues)
		}
	}
	if len(g.Terminadas()) != 0 {
		t.Fatal("Terminadas não pode repetir tarefa já entregue")
	}
	msg := AvisoTerminadas(g.Fotos(), 0)
	if !strings.HasPrefix(msg, "[subagents]") || !strings.Contains(msg, "## t5 completed") || !strings.Contains(msg, "ok") {
		t.Fatalf("aviso:\n%s", msg)
	}
}

// Test C: cancelar uma tarefa em execução a leva a cancelled e o status mostra.
func TestCancelarTarefaEmExecucao(t *testing.T) {
	_, cfg := novoServidor(t, "nunca vou responder", 2*time.Second, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), especStatus(g, "task"), especResult(g, "task"))
	nomos := map[string]tools.Spec{}
	for _, s := range specs {
		nomos[s.Name] = s
	}

	id, err := g.Spawn(context.Background(), "demora bastante")
	if err != nil {
		t.Fatalf("spawn falhou: %v", err)
	}

	// Espera a sessão filha de fato começar (o provider já registrou chamada).
	prazo := time.Now().Add(2 * time.Second)
	for {
		cp, _ := g.snapshot(id)
		if cp.Estado == Running {
			break
		}
		if time.Now().After(prazo) {
			t.Fatalf("tarefa não iniciou (estado %s)", cp.Estado)
		}
		time.Sleep(2 * time.Millisecond)
	}

	if !g.Cancelar(id) {
		t.Fatal("Cancelar deveria devolver true para tarefa em execução")
	}

	cp := esperarEstado(t, g, id, Cancelled, Completed)
	if cp.Estado != Cancelled {
		t.Fatalf("estado deveria ser cancelled, veio %s", cp.Estado)
	}

	// Só depois do terminal é que não há mais o que cancelar: chamar de novo
	// devolve false. (Antes disso o worker ainda não gravou o estado.)
	if g.Cancelar(id) {
		t.Fatal("Cancelar de tarefa já cancelada deveria devolver false")
	}

	saida, err := executarTool(t, nomos["task_result"], fmt.Sprintf(`{"id":%q}`, id))
	if err != nil {
		t.Fatalf("result falhou: %v", err)
	}
	if !strings.HasPrefix(saida, "tarefa "+id+" cancelada") {
		t.Fatalf("result de cancelada deveria ser uma linha de estado: %q", saida)
	}

	status, _ := executarTool(t, nomos["task_status"], `{}`)
	if !strings.Contains(status, Cancelled) {
		t.Fatalf("status deveria mostrar cancelled:\n%s", status)
	}
}

// Test D: task_result com id inexistente devolve erro de UMA linha; cancelar
// id desconhecido devolve false; e prompt vazio não cria tarefa.
func TestResultadoIdInexistenteEErrosDeEntrada(t *testing.T) {
	_, cfg := novoServidor(t, "ok", 0, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), especStatus(g, "task"), especResult(g, "task"))
	nomos := map[string]tools.Spec{}
	for _, s := range specs {
		nomos[s.Name] = s
	}

	_, err := executarTool(t, nomos["task_result"], `{"id":"t99"}`)
	if err == nil {
		t.Fatal("task_result de id inexistente deveria errar")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("erro deveria ser de UMA linha: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "t99") {
		t.Fatalf("erro deveria citar o id: %q", err.Error())
	}

	if g.Cancelar("t99") {
		t.Fatal("Cancelar de id inexistente deveria devolver false")
	}

	if _, err := g.Spawn(context.Background(), "   "); err == nil {
		t.Fatal("spawn com prompt vazio deveria errar")
	}

	// spawn sem argumentos também erreia (prompt obrigatório).
	if _, err := executarTool(t, nomos["task_spawn"], `{}`); err == nil {
		t.Fatal("spawn sem prompt deveria errar")
	}

	// Status sem tarefas é uma linha informativa, não um erro.
	saida, err := executarTool(t, nomos["task_status"], `{}`)
	if err != nil {
		t.Fatalf("status sem tarefas não deveria errar: %v", err)
	}
	if !strings.Contains(saida, "nenhuma") {
		t.Fatalf("status vazio inesperado: %q", saida)
	}
}

// Test E: sem config pronta a tarefa nasce failed com erro de uma linha
// (o gerente não entra em pânico nem trava a fila).
func TestSpawnSemConfigFalhaLimpo(t *testing.T) {
	g := Novo(Config{})
	g.Vincular(context.Background())
	id, err := g.Spawn(context.Background(), "algo")
	if err != nil {
		t.Fatalf("spawn não deveria errar por config, apenas falhar a tarefa: %v", err)
	}
	cp := esperarEstado(t, g, id, Failed)
	if cp.Erro == "" || strings.Contains(cp.Erro, "\n") {
		t.Fatalf("erro deveria ser preenchido e de uma linha: %q", cp.Erro)
	}
}

// Test F: o resultado volta cortado em 16 KiB quando o sub-agente escreve muito.
func TestResultadoCortadoNoTeto(t *testing.T) {
	grande := strings.Repeat("x", TetoResultado+5000)
	_, cfg := novoServidor(t, grande, 0, false)
	g := Novo(Config{})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), especStatus(g, "task"), especResult(g, "task"))
	nomos := map[string]tools.Spec{}
	for _, s := range specs {
		nomos[s.Name] = s
	}
	id, err := g.Spawn(context.Background(), "escreva muito")
	if err != nil {
		t.Fatalf("spawn falhou: %v", err)
	}
	esperarEstado(t, g, id, Completed, Failed)
	saida, err := executarTool(t, nomos["task_result"], fmt.Sprintf(`{"id":%q}`, id))
	if err != nil {
		t.Fatalf("result falhou: %v", err)
	}
	if len(saida) <= TetoResultado || !strings.Contains(saida, "cortado em") {
		t.Fatalf("resultado deveria vir cortado no teto (veio %d bytes)", len(saida))
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func chaves(m map[string]tools.Spec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func temBuiltin(v reqVista, nome string) bool {
	for _, tl := range v.Tools {
		if tl.Function.Name == nome {
			return true
		}
	}
	return false
}

// O modelo só recebe task_spawn: sem status/result ele não tem como ficar
// consultando em loop, e os resultados chegam pelo aviso automático.
func TestModeloSoRecebeSpawn(t *testing.T) {
	specs := Especificacoes(Novo(Config{Nome: "task"}), config.Config{})
	if len(specs) != 1 || specs[0].Name != "task_spawn" {
		t.Fatalf("tools do modelo = %v", specs)
	}
}
