package cli

import (
	"strings"
	"testing"
)

// O nome da sessão vai para a aba do terminal (OSC 0), sem controles que
// quebrem a sequência; sem nome, a aba volta para "bh".
func TestTituloAba(t *testing.T) {
	if got := tituloAba(""); got != "\033]0;bh\a" {
		t.Fatalf("aba vazia = %q", got)
	}
	got := tituloAba("revisão\a do\x1b parser")
	if got != "\033]0;bh | revisão do parser\a" {
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
	if !strings.Contains(s, "\033]0;bh | minha sessão\a") {
		t.Fatal("título não foi para a aba")
	}
	if !strings.Contains(s, "\033[1;1H\033[0m primeira linha da conversa") {
		t.Fatalf("conversa deveria começar na linha 1")
	}
}

// Ao abrir, sem nenhuma mensagem, a aba já mostra "bh | Nova sessão".
func TestAbaNovaSessaoNaAbertura(t *testing.T) {
	coresLigadas = true
	defer func() { coresLigadas = false }()
	antes := tamanhoTela
	tamanhoTela = func() (int, int) { return 140, 30 }
	defer func() { tamanhoTela = antes }()

	var sb strings.Builder
	tl := &tela{out: &sb}
	tl.Estado(estadoPainel{modelo: "m"})
	if !strings.Contains(sb.String(), "\033]0;bh | Nova sessão\a") {
		t.Fatal("aba deveria abrir como 'bh | Nova sessão'")
	}
}
