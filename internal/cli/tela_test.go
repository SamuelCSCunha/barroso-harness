package cli

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// O nome da sessão vai para a aba do terminal (OSC 0), sem controles que
// quebrem a sequência; sem nome, a aba volta para "BH".
func TestTituloAba(t *testing.T) {
	if got := tituloAba(""); got != "\033]0;BH\a" {
		t.Fatalf("aba vazia = %q", got)
	}
	got := tituloAba("revisão\a do\x1b parser")
	if got != "\033]0;BH | revisão do parser\a" {
		t.Fatalf("aba = %q", got)
	}
}

// A tela não desenha mais a linha de título: a conversa começa na linha 1 e o
// título só vai para a aba e o painel.
func TestTelaSemLinhaDeTitulo(t *testing.T) {
	coresLigadas = true
	defer func() { coresLigadas = false }()
	antes := tamanhoTela
	tamanhoTela = func() (int, int) { return 140, 30 }
	defer func() { tamanhoTela = antes }()

	var sb strings.Builder
	tl := &tela{out: &sb}
	tl.Estado(estadoPainel{titulo: "minha sessão", modelo: "m"})
	tl.Adicionar("primeira linha da conversa")
	s := sb.String()
	if !strings.Contains(s, "\033]0;BH | minha sessão\a") {
		t.Fatal("título não foi para a aba")
	}
	if !strings.Contains(s, "\033[2;1H\033[0m   primeira linha da conversa") {
		t.Fatalf("conversa deveria começar na linha 2, depois da margem")
	}
}

// Ao abrir, sem nenhuma mensagem, a aba já mostra "BH | Nova sessão".
func TestAbaNovaSessaoNaAbertura(t *testing.T) {
	coresLigadas = true
	defer func() { coresLigadas = false }()
	antes := tamanhoTela
	tamanhoTela = func() (int, int) { return 140, 30 }
	defer func() { tamanhoTela = antes }()

	var sb strings.Builder
	tl := &tela{out: &sb}
	tl.Estado(estadoPainel{modelo: "m"})
	if !strings.Contains(sb.String(), "\033]0;BH | New session\a") {
		t.Fatal("aba deveria abrir como 'BH | New session'")
	}
}

// A roda do mouse chega como SGR e vira rolagem; clique é ignorado.
func TestRodaDoMouse(t *testing.T) {
	ch := make(chan tecla, 8)
	lerTeclas(bufio.NewReader(strings.NewReader("\x1b[<64;10;5M\x1b[<0;3;3M\x1b[<0;3;3m\x1b[<65;10;5Ma")), ch)
	var got []string
	for k := range ch {
		got = append(got, k.acao+k.texto)
	}
	if strings.Join(got, ",") != "rodacima,rodabaixo,a" {
		t.Fatalf("teclas = %v", got)
	}
}

// Um pedido de interrupção que sobrou de um turno já encerrado não derruba o
// turno seguinte; um pedido feito durante o turno, sim.
func TestCancelarTurnoIgnoraPedidoVelho(t *testing.T) {
	io := IO{cancelar: make(chan struct{}, 1)}
	io.cancelar <- struct{}{}
	ctx, cancel := io.cancelarTurno(make(chan struct{}))
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("pedido velho cancelou o turno novo")
	case <-time.After(50 * time.Millisecond):
	}
	io.cancelar <- struct{}{}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pedido durante o turno não cancelou")
	}
}

// Texto longo na caixa de entrada quebra por palavra; palavra maior que a
// linha quebra no meio.
func TestQuebrarEntrada(t *testing.T) {
	got := quebrarEntrada("um dois tres quatro cinco", 12)
	if strings.Join(got, "|") != "um dois tres|quatro cinco" {
		t.Fatalf("quebra = %q", got)
	}
	got = quebrarEntrada(strings.Repeat("x", 25), 10)
	if strings.Join(got, "|") != "xxxxxxxxxx|xxxxxxxxxx|xxxxx" {
		t.Fatalf("palavra longa = %q", got)
	}
	if got := quebrarEntrada("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("vazio = %q", got)
	}
}

// /resume na tela cheia redesenha a conversa: pergunta, tool junto do seu
// resultado e resposta.
func TestReproduzirHistorico(t *testing.T) {
	antes := tamanhoTela
	tamanhoTela = func() (int, int) { return 140, 40 }
	defer func() { tamanhoTela = antes }()
	var sb strings.Builder
	tl := &tela{out: &sb}
	tl.Estado(estadoPainel{modelo: "m"}) // mede a tela, como no REPL
	reproduzirHistorico(tl, []dealer.Mensagem{
		{Role: "user", Conteudo: "liste os arquivos"},
		{Role: "assistant", Conteudo: ""},
		{Role: "tool_call", ToolID: "1", ToolName: "terminal_run", ToolArgs: `{"command":"dir"}`},
		{Role: "tool_call", ToolID: "2", ToolName: "file_read", ToolArgs: `{"path":"a.go"}`},
		{Role: "tool_result", ToolID: "1", ToolName: "terminal_run", Conteudo: "exit_code: 0\nx"},
		{Role: "tool_result", ToolID: "2", ToolName: "file_read", Conteudo: "package a", IsErro: true},
		{Role: "assistant", Conteudo: "pronto, dois arquivos"},
	})
	tudo := strings.Join(tl.linhas, "\n")
	ordem := []string{"liste os arquivos", "terminal_run", "exit_code: 0", "file_read", "package a", "pronto, dois arquivos"}
	pos := 0
	for _, s := range ordem {
		i := strings.Index(tudo[pos:], s)
		if i < 0 {
			t.Fatalf("faltou %q na ordem certa:\n%s", s, tudo)
		}
		pos += i + len(s)
	}
	if !strings.Contains(tudo, "✗") {
		t.Fatal("tool com erro deveria sair com ✗")
	}
}
