package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestListaModelosCompleta: o catálogo inteiro sai numa lista só, sem
// paginação, e a coluna da janela fica alinhada também na linha do modelo
// atual (o negrito não pode entrar na conta do preenchimento).
func TestListaModelosCompleta(t *testing.T) {
	antiga := coresLigadas
	defer func() { coresLigadas = antiga }()

	ls := make([]dealer.CatalogModel, 0, 120)
	for i := 0; i < 120; i++ {
		ls = append(ls, dealer.CatalogModel{ID: fmt.Sprintf("m-%03d", i), Input: []string{"text"}, ContextLength: 250000})
	}
	ls = append(ls, dealer.CatalogModel{ID: "z-modelo-longo", ContextLength: 1048000})

	coresLigadas = false
	var sb strings.Builder
	listarModelos(&sb, ls, "m-050")
	saida := sb.String()
	for _, quer := range []string{"121 disponíveis", "  1 m-000", " 51 m-050", "120 m-119", "121 z-modelo-longo"} {
		if !strings.Contains(saida, quer) {
			t.Fatalf("lista sem %q:\n%s", quer, saida)
		}
	}
	if strings.Contains(saida, "página") || strings.Contains(saida, "pg<") {
		t.Fatalf("não deveria haver paginação:\n%s", saida)
	}

	// Com cores, a coluna da janela começa na mesma posição visível na linha
	// do modelo atual e numa linha comum.
	coresLigadas = true
	sb.Reset()
	listarModelos(&sb, ls, "m-050")
	sem := regexp.MustCompile("" + `\[[0-9;]*m`)
	col := func(id string) int {
		for _, l := range strings.Split(sb.String(), "\n") {
			l = sem.ReplaceAllString(l, "")
			if strings.Contains(l, id+" ") {
				// Coluna em CARACTERES, não bytes: o ▸ do modelo atual tem 3
				// bytes e ocupa uma coluna, igual a um espaço.
				if k := strings.Index(l, "250k"); k >= 0 {
					return utf8.RuneCountInString(l[:k])
				}
				return -1
			}
		}
		return -1
	}
	if a, b := col("m-050"), col("m-051"); a < 0 || a != b {
		t.Fatalf("coluna desalinhada no modelo atual: %d vs %d", a, b)
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
