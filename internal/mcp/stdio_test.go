package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── subprocesso falso de MCP por stdio ───────────────────────────────────

// envHelper é a variável que liga o modo "servidor MCP falso" do subprocesso.
// O teste seta esta env no filho; sem ela, o binário de teste roda normalmente.
const envHelper = "BARROSO_MCP_TEST_HELPER"

// envMudo liga o modo "servidor que nunca responde" do subprocesso (usado no
// teste de handshake que estoura no tempo limite).
const envMudo = "BARROSO_MCP_TEST_MUDO"

// TestHelperProcess NÃO é um teste de verdade: é o processo servidor que os
// testes de stdio lançam. Roda fora do fluxo normal do `go test` (o binário
// pai só o invoca com -test.run=TestHelperProcess), então o guard por env
// evita que ele execute quando o pacote é testado no modo comum.
//
// Fala MCP por newline-delimited JSON em stdin/stdout — o mesmo formato que
// um servidor MCP de verdade usa. Não usa Content-Length: isso é do LSP, e MCP
// stdio deliberadamente adotou newline.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(envHelper) == "" {
		t.Skip("helper do teste de stdio; só roda como subprocesso")
	}

	// Modo "mudo": o processo lê o que entra e NUNCA responde. Serve para
	// exercitar o caminho de handshake que estoura no tempo limite — o cliente
	// tem de desistir sozinho e ainda assim limpar o subprocesso.
	if os.Getenv(envMudo) != "" {
		drenar := bufio.NewScanner(os.Stdin)
		for drenar.Scan() {
		}
		return
	}

	// Notificações emitidas ANTES da primeira resposta: o cliente tem de
	// correlacionar por id e simplesmente ignorá-las.
	emitirNotificacao("notifications/progress", map[string]any{
		"progressToken": "fake",
		"message":       "acordando",
	})

	in := bufio.NewScanner(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		linha := strings.TrimSpace(in.Text())
		if linha == "" {
			continue
		}
		var m mensagem
		if json.Unmarshal([]byte(linha), &m) != nil {
			continue
		}

		switch m.Method {
		case "initialize":
			responder(out, idDe(m.ID), map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-stdio", "version": "9"},
			})

		case "notifications/initialized":
			// Notificação: nada a responder.

		case "tools/list":
			// Ruído antes da resposta: linha não-JSON e uma notificação.
			fmt.Fprintln(out, "ruido nao-json do servidor")
			emitirNotificacao("notifications/message", map[string]any{"level": "info"})
			responder(out, idDe(m.ID), map[string]any{
				"tools": []any{
					map[string]any{
						"name":        "eco",
						"description": "Devolve o texto recebido",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"msg": map[string]any{"type": "string"},
							},
							"required": []string{"msg"},
						},
					},
					map[string]any{
						"name":        "falha",
						"description": "Sempre reporta erro",
						"inputSchema": map[string]any{"type": "object"},
					},
					map[string]any{
						"name":        "sem-schema",
						"description": "Sem inputSchema nenhum",
					},
				},
			})

		case "tools/call":
			var p paramsToolsCall
			_ = json.Unmarshal(m.Params, &p)
			switch p.Name {
			case "eco":
				msg := ""
				if v, ok := p.Arguments["msg"].(string); ok {
					msg = v
				}
				responder(out, idDe(m.ID), map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "eco: " + msg},
					},
				})
			case "falha":
				responder(out, idDe(m.ID), map[string]any{
					"isError": true,
					"content": []any{
						map[string]any{"type": "text", "text": "deu errado de propósito"},
					},
				})
			default:
				responderErro(out, idDe(m.ID), -32602, "ferramenta desconhecida: "+p.Name)
			}

		default:
			responderErro(out, idDe(m.ID), -32601, "método desconhecido: "+m.Method)
		}
	}
}

// idDe ecoa o id cru (string ou número) de volta, como manda o JSON-RPC.
// idDe ecoa o id cru do request (número ou string), como o JSON-RPC exige:
// a resposta carrega o id do request, não um inventado.
func idDe(cru json.RawMessage) any {
	return json.RawMessage(cru)
}

func responder(w *bufio.Writer, id any, resultado any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": resultado})
	// newline é o delimitador de mensagem do MCP stdio.
	fmt.Fprintf(w, "%s\n", b)
	_ = w.Flush()
}

func responderErro(w *bufio.Writer, id any, code int, msg string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	fmt.Fprintf(w, "%s\n", b)
	_ = w.Flush()
}

// emitirNotificacao escreve uma notificação (sem id) — o cliente deve ignorar.
func emitirNotificacao(metodo string, params any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": metodo, "params": params})
	fmt.Printf("%s\n", b)
}

// servidorStdioFalso monta o Servidor que aponta para este mesmo binário de
// teste, no modo helper. O env do filho carrega envHelper — e um marcador que
// o helper usa para saber que está no clima de teste.
func servidorStdioFalso(nome string) Servidor {
	return Servidor{
		Nome:    nome,
		Tipo:    "stdio",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			envHelper:                "1",
			"GO_WANT_HELPER_PROCESS": "1",
		},
		Timeout: 10,
	}
}

// ── testes do transporte stdio ───────────────────────────────────────────

// Handshake completo: initialize (com notificações antes), tools/list.
func TestStdioHandshakeELista(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre (stdio): %v", err)
	}
	defer c.Fecha()

	if c.Transporto() != "stdio" {
		t.Errorf("Transporto() = %q, queria stdio", c.Transporto())
	}
	info := c.InfoServidor()
	if info.ServerInfo.Name != "fake-stdio" {
		t.Errorf("ServerInfo.Name = %q; queria fake-stdio", info.ServerInfo.Name)
	}

	especificacoes, err := c.Ferramentas(ctx)
	if err != nil {
		t.Fatalf("Ferramentas (stdio): %v", err)
	}
	if len(especificacoes) != 3 {
		t.Fatalf("Ferramentas devolveu %d; queria 3", len(especificacoes))
	}
	nomes := map[string]bool{}
	for _, e := range especificacoes {
		nomes[e.Name] = true
	}
	for _, q := range []string{"eco", "falha", "sem-schema"} {
		if !nomes[q] {
			t.Errorf("ferramenta %q ausente do catálogo", q)
		}
	}
	// A que não tinha schema recebeu o objeto livre.
	for _, e := range especificacoes {
		if e.Name == "sem-schema" && e.Schema["type"] != "object" {
			t.Errorf("sem-schema.Schema = %#v", e.Schema)
		}
	}
}

// tools/call pelo caminho da Spec (o que o runtime realmente executa).
func TestStdioChamarEcoaArgumentos(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	out, err := c.Chamar(ctx, "eco", `{"msg":"olá stdio"}`)
	if err != nil {
		t.Fatalf("Chamar: %v", err)
	}
	if out != "eco: olá stdio" {
		t.Errorf("Chamar = %q; queria %q", out, "eco: olá stdio")
	}

	// Argumentos vazios viram {} e o servidor responde "eco: ".
	out, err = c.Chamar(ctx, "eco", "")
	if err != nil {
		t.Fatalf("Chamar com args vazios: %v", err)
	}
	if out != "eco: " {
		t.Errorf("Chamar vazio = %q", out)
	}
}

// isError do servidor vira erro Go carregando a mensagem dele.
func TestStdioIsError(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	out, err := c.Chamar(ctx, "falha", "{}")
	if err == nil {
		t.Fatalf("Chamar deveria devolver erro")
	}
	if !strings.Contains(err.Error(), "deu errado de propósito") {
		t.Errorf("erro = %q; queria a mensagem do servidor", err.Error())
	}
	if !strings.Contains(out, "deu errado de propósito") {
		t.Errorf("saída = %q", out)
	}
}

// Erro JSON-RPC (diferente de isError) também vira erro Go.
func TestStdioErroJSONRPC(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	_, err = c.Chamar(ctx, "nao-existe", "{}")
	if err == nil {
		t.Fatalf("Chamar em ferramenta inexistente deveria falhar")
	}
	if !strings.Contains(err.Error(), "ferramenta desconhecida") {
		t.Errorf("erro = %q", err.Error())
	}
}

// Várias chamadas em sequência: cada uma com id próprio, resposta casada.
func TestStdioChamadasSequenciais(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	for i := 0; i < 6; i++ {
		querido := fmt.Sprintf("eco: msg-%d", i)
		out, err := c.Chamar(ctx, "eco", fmt.Sprintf(`{"msg":"msg-%d"}`, i))
		if err != nil {
			t.Fatalf("chamada %d: %v", i, err)
		}
		if out != querido {
			t.Errorf("chamada %d = %q; queria %q", i, out, querido)
		}
	}
}

// Chamadas em paralelo: ids distintos precisam correlacionar sem trocar.
func TestStdioChamadasConcorrentes(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	const n = 12
	erros := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			querido := fmt.Sprintf("eco: par-%d", i)
			out, err := c.Chamar(ctx, "eco", fmt.Sprintf(`{"msg":"par-%d"}`, i))
			if err != nil {
				erros <- fmt.Errorf("par-%d: %w", i, err)
				return
			}
			if out != querido {
				erros <- fmt.Errorf("par-%d: resposta trocada: %q", i, out)
				return
			}
			erros <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-erros; err != nil {
			t.Errorf("%v", err)
		}
	}
}

// Fecha encerra o subprocesso: EOF no stdin faz o helper sair; o Wait
// acontece dentro do prazo (5 s), sem depender do Kill.
func TestStdioFechaTerminaSubprocesso(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}

	pid := c.cmd.Process.Pid
	inicio := time.Now()
	if err := c.Fecha(); err != nil {
		t.Errorf("Fecha: %v", err)
	}
	if decorrido := time.Since(inicio); decorrido > prazoMataSub+3*time.Second {
		t.Errorf("Fecha demorou %v; queria sair pelo EOF, não pelo Kill", decorrido)
	}

	// Fecha duas vezes é no-op.
	if err := c.Fecha(); err != nil {
		t.Errorf("Fecha repetido: %v", err)
	}

	// O processo não existe mais.
	if err := sinalVivo(pid); err == nil {
		t.Errorf("subprocesso %d ainda vivo depois do Fecha", pid)
	}

	// Chamar depois de fechar devolve erro, não trava nem panic.
	_, err = c.Chamar(context.Background(), "eco", "{}")
	if err == nil {
		t.Errorf("Chamar depois do Fecha deveria falhar")
	}
}

// Comando inexistente: erro de UMA linha, sem subprocesso para limpar.
func TestStdioComandoInexistente(t *testing.T) {
	c, err := Novo(Servidor{
		Nome: "fantasma",
		Tipo: "stdio",
		// Caminho absoluto que não existe: falha no Start, não no PATH.
		Command: filepath.Join(t.TempDir(), "nao-existe-abc123"),
		Timeout: 5,
	})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err == nil {
		t.Fatalf("Abre deveria falhar com comando inexistente")
	} else if strings.Contains(err.Error(), "\n") {
		t.Errorf("erro multi-linha: %q", err.Error())
	}
}

// Contexto já cancelado: Abre devolve o erro do ctx antes de subir processo.
func TestStdioContextoCancelado(t *testing.T) {
	c, err := Novo(servidorStdioFalso("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Abre(ctx); err == nil {
		t.Errorf("Abre com ctx cancelado deveria falhar")
	}
}

// Handshake falho: o subprocesso tem de ser limpo (sem zumbi).
func TestStdioHandshakeFalhoLimpaSubprocesso(t *testing.T) {
	// Subprocesso que não fala MCP: lê uma linha e sai sem responder nada.
	c, err := Novo(Servidor{
		Nome:    "mudo",
		Tipo:    "stdio",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			envHelper:                "1",
			"GO_WANT_HELPER_PROCESS": "1",
			"BARROSO_MCP_TEST_MUDO":  "1",
		},
		Timeout: 3,
	})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := c.Abre(ctx); err == nil {
		t.Fatalf("Abre deveria falhar com servidor mudo")
	}
	// Depois do Abre falhar, não pode sobrar processo: Fecha no caminho de
	// erro do handshake garante isso.
	if err := sinalVivo(c.cmd.Process.Pid); err == nil {
		t.Errorf("subprocesso zumbi depois do handshake falho")
	}
}

// sinalVivo checa se o pid existe. Em Windows não há sinal 0: usa OpenProcess
// via tasklist, que é portável o bastante para o teste.
func sinalVivo(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("pid inválido")
	}
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH")
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if strings.Contains(string(out), fmt.Sprintf("%d", pid)) {
		return nil
	}
	return fmt.Errorf("sem processo %d", pid)
}
