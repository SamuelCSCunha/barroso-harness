package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

func TestContextAggregatesByPolicy(t *testing.T) {
	noColors(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Fechar()
	_ = db.NewSession("a")
	_ = db.NewSession("b")
	_, _ = db.StoreEvent("a", "compactacao", map[string]any{"politica": "agente", "bytes_antes": 40960, "bytes_depois": 10240})
	_, _ = db.StoreEvent("b", "guardrail", map[string]any{"tipo": "proativa"}) // legacy, no policy
	_, _ = db.StoreEvent("b", "guardrail", map[string]any{"tipo": "reativa", "politica": "limiar"})

	var out bytes.Buffer
	cmdContext(&out, &config.Config{}, db)
	s := out.String()
	for _, want := range []string{
		"active policy: agente",
		"agente    1 sessions · 1 ctx_compact",
		"history 40→10 KiB (75% smaller)",
		"limiar    1 sessions · 0 ctx_compact · 1 proactive guardrail · 1 reactive",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestContextWithoutStore(t *testing.T) {
	noColors(t)
	var out bytes.Buffer
	cmdContext(&out, &config.Config{PoliticaContexto: "limiar"}, nil)
	if !strings.Contains(out.String(), "active policy: limiar") || !strings.Contains(out.String(), "not persisted") {
		t.Fatalf("output: %q", out.String())
	}
}
