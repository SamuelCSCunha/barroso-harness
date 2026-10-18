package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// TestTuiTurnoGlifosEPaleta: com cores ligadas, a facciata TUI do turno emite
// glifo e cor de uma vez; sem cores, rendererTurno devolve o texto cru que o
// runtime_test crava via Turno.
func TestTuiTurnoGlifosEPaleta(t *testing.T) {
	antiga := coresLigadas
	coresLigadas = true
	defer func() { coresLigadas = antiga }()

	var sb strings.Builder
	tui := tuiTurno{out: &sb}
	tui.Tool("terminal_run", 120*time.Millisecond, false)
	tui.Eco("exit_code: 0")
	tui.Guardrail(false)
	tui.Tokens(1200, 45, 890*time.Millisecond, true)
	saida := sb.String()

	for _, quer := range []string{
		glifoTool, "terminal_run", "120ms", corCorMag,
		glifoGuard, "tokens ↑1200 ↓45", "890ms", "compactado",
	} {
		if !strings.Contains(saida, quer) {
			t.Fatalf("TUI sem %q na saída:\n%q", quer, saida)
		}
	}
}

func TestPuroSemEscapes(t *testing.T) {
	antiga := coresLigadas
	coresLigadas = false
	defer func() { coresLigadas = antiga }()

	var sb strings.Builder
	rendererTurno(&sb).Tool("terminal_run", 120*time.Millisecond, false)
	if !strings.Contains(sb.String(), "[tool terminal_run ok em 120ms]") {
		t.Fatalf("modo puro perdeu o literal do runtime_test:\n%q", sb.String())
	}
	if strings.Contains(sb.String(), "\033") {
		t.Fatalf("escape ANSI à revelia no modo puro:\n%q", sb.String())
	}
}

// TestPaginacaoModelos: página 2 de 120 modelos abre no nº 51; o cabeçalho
// anuncia 3 páginas; página além do fim cai na última.
func TestPaginacaoModelos(t *testing.T) {
	antiga := coresLigadas
	coresLigadas = false
	defer func() { coresLigadas = antiga }()

	ls := make([]dealer.CatalogModel, 0, 120)
	for i := 0; i < 120; i++ {
		ls = append(ls, dealer.CatalogModel{ID: fmt.Sprintf("m-%03d", i), Input: []string{"text"}, ContextLength: 250000})
	}
	var sb strings.Builder
	listarModelos(&sb, ls, "", 2, catalogoPorPagina)
	saida := sb.String()
	if !strings.Contains(saida, "página 2/3") {
		t.Fatalf("cabeçalho de página errado:\n%s", saida)
	}
	linha50 := "  50 m-049"
	linha51 := "  51 m-050"
	if !strings.Contains(saida, linha51) {
		t.Fatalf("página 2 devia abrir no nº 51 (m-050):\n%s", saida)
	}
	if strings.Contains(saida, "m-050\n") && !strings.Contains(saida, linha50) {
		t.Fatalf("linha 50 no meio da página 2 deve preceder a 51:\n%s", saida)
	}
	if strings.Contains(saida, "m-100\n") {
		t.Fatalf("página 2 devia terminar no nº 100:\n%s", saida)
	}

	sb.Reset()
	listarModelos(&sb, ls, "", 99, catalogoPorPagina)
	if !strings.Contains(sb.String(), "página 3/3") {
		t.Fatalf("pg99 deveria fechar na última página, veio:\n%s", sb.String())
	}
}

// TestCoresDesligadasSemEscape: coresLigadas=false tira todo escape e o
// erroFmt mantém uma linha única (o detalhe de protocolo é descartado).
func TestCoresDesligadasSemEscape(t *testing.T) {
	antiga := coresLigadas
	defer func() { coresLigadas = antiga }()
	coresLigadas = false
	for _, f := range []func(string) string{verdof, vermelhof, amarelof, cianof, dimf, magof, roxof, negritof} {
		if strings.ContainsRune(f("x"), 0x1b) {
			t.Fatal("cor emita escape com coresLigadas=false")
		}
	}
	var sb strings.Builder
	erroFmt(&sb, "erro salvando", errors.New("disco cheio\ndetalhe de protocolo"))
	saida := sb.String()
	if !strings.Contains(saida, "disco cheio") || strings.ContainsRune(saida, 0x1b) {
		t.Fatalf("erroFmt com escape ou sem a mensagem: %q", saida)
	}
	if strings.Contains(saida, "detalhe de protocolo") {
		t.Fatalf("erroFmt devia descartar as linhas seguintes: %q", saida)
	}
}
