package cli

import (
	"bufio"
	"strings"
	"testing"
	"time"
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
