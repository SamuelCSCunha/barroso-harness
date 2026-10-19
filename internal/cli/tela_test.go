package cli

import (
	"strings"
	"testing"
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
	if !strings.Contains(sb.String(), "\033]0;BH | Nova sessão\a") {
		t.Fatal("aba deveria abrir como 'BH | Nova sessão'")
	}
}
