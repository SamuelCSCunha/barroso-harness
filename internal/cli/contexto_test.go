package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

func TestContextoAgregaPorPolitica(t *testing.T) {
	semCores(t)
	loja, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer loja.Fechar()
	_ = loja.NewSession("a")
	_ = loja.NewSession("b")
	_, _ = loja.StoreEvent("a", "compactacao", map[string]any{"politica": "agente", "bytes_antes": 40960, "bytes_depois": 10240})
	_, _ = loja.StoreEvent("b", "guardrail", map[string]any{"tipo": "proativa"}) // legado, sem política
	_, _ = loja.StoreEvent("b", "guardrail", map[string]any{"tipo": "reativa", "politica": "limiar"})

	var out bytes.Buffer
	cmdContexto(&out, &config.Config{}, loja)
	s := out.String()
	for _, quer := range []string{
		"política ativa: agente",
		"agente    1 sessões · 1 ctx_compact",
		"histórico 40→10 KiB (75% menor)",
		"limiar    1 sessões · 0 ctx_compact · 1 guardrail proativo · 1 reativo",
	} {
		if !strings.Contains(s, quer) {
			t.Errorf("saída sem %q:\n%s", quer, s)
		}
	}
}

func TestContextoSemLoja(t *testing.T) {
	semCores(t)
	var out bytes.Buffer
	cmdContexto(&out, &config.Config{PoliticaContexto: "limiar"}, nil)
	if !strings.Contains(out.String(), "política ativa: limiar") || !strings.Contains(out.String(), "sem persistência") {
		t.Fatalf("saída: %q", out.String())
	}
}
