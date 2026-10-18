package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// O spinner esconde o cursor e o devolve ao parar — o cursor piscando ao lado
// de "Pensando" parecia campo de digitação.
func TestSpinnerEscondeEDevolveCursor(t *testing.T) {
	coresLigadas = true
	defer func() { coresLigadas = false }()
	var sb strings.Builder
	tui := novoTurnoTUI(&sb, &config.Config{Model: "m"})
	tui.Pensando()
	time.Sleep(20 * time.Millisecond)
	tui.pararSpin()
	s := sb.String()
	i, j := strings.Index(s, esconderCursor), strings.LastIndex(s, mostrarCursor)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("cursor não foi escondido e devolvido nessa ordem: %q", s)
	}
}

// O BH não tem modos: nem a linha de status nem o rodapé levam "Build".
func TestSemRotuloDeModo(t *testing.T) {
	coresLigadas = true
	defer func() { coresLigadas = false }()
	var sb strings.Builder
	cfg := &config.Config{CurrentProvider: "p", Model: "m", Janela: 1000}
	statusEntrada(&sb, cfg, "", 0, 0)
	novoTurnoTUI(&sb, cfg).Tokens(10, 10, time.Second, false)
	if strings.Contains(sb.String(), "Build") {
		t.Fatalf("rótulo de modo não deveria aparecer: %q", sb.String())
	}
}
