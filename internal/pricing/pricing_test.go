package pricing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCalcFullPromptNoCache(t *testing.T) {
	p := Price{InUSDPerM: 2.5, OutUSDPerM: 10}
	c := p.Calc(1_000_000, 0, 1_000_000)
	if !c.HasPrice || c.USD < 12.49 || c.USD > 12.51 {
		t.Fatalf("cost = %+v, want ~12.50", c)
	}
}

func TestCalcCacheSeparated(t *testing.T) {
	p := Price{InUSDPerM: 2.5, OutUSDPerM: 10, CacheUSDPerM: 1.25}
	// 500k fresh + 500k cached + 100k out
	c := p.Calc(500_000, 500_000, 100_000)
	want := 0.5*2.5 + 0.5*1.25 + 0.1*10
	if c.USD < want-0.001 || c.USD > want+0.001 {
		t.Fatalf("cost = %f, want %f", c.USD, want)
	}
}

func TestNoPriceNeverEstimates(t *testing.T) {
	c := Price{}.Calc(1_000_000, 0, 1_000_000)
	if c.HasPrice || c.USD != 0 {
		t.Fatalf("an unpriced model must have no cost: %+v", c)
	}
}

func TestCacheFallsBackToPromptPrice(t *testing.T) {
	p := Price{InUSDPerM: 3, OutUSDPerM: 12}
	// No cache price declared: the whole prompt is billed at In.
	c := p.Calc(0, 1_000_000, 0)
	if c.USD < 2.99 || c.USD > 3.01 {
		t.Fatalf("cache without a declared price = %f, want 3.00", c.USD)
	}
}

func TestPriceLookupWithProviderPrefix(t *testing.T) {
	tb := Tabela{Modelos: map[string]Price{"gpt-4o-mini": {InUSDPerM: 0.15, OutUSDPerM: 0.6}}}
	if _, ok := tb.Price("openai/gpt-4o-mini"); !ok {
		t.Fatal("the segment after the last / must resolve")
	}
	if _, ok := tb.Price("nope"); ok {
		t.Fatal("unknown model must not resolve")
	}
	if _, ok := tb.Price(""); ok {
		t.Fatal("empty model must not resolve")
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	tb, err := Load(dir)
	if err != nil || !tb.Empty() {
		t.Fatalf("no prices.json must load as empty: %+v %v", tb, err)
	}
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	body := `{"modelos":{"kimi-k2":{"in_usd_per_million":0.6,"out_usd_per_million":2.4,"cache_usd_per_million":0.15}}}`
	if err := os.WriteFile(filepath.Join(dir, "prices.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tb, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Price("kimi-k2")
	if !ok || p.InUSDPerM != 0.6 || p.CacheUSDPerM != 0.15 {
		t.Fatalf("price = %+v", p)
	}
}

func TestFormato(t *testing.T) {
	cases := map[float64]string{
		0:        "",
		0.004:    "$0.00",
		0.1249:   "$0.12",
		1.5:      "$1.50",
		12.3:     "$12.30",
		1234.5:   "$1.23k",
	}
	for usd, want := range cases {
		if got := Formato(usd); got != want {
			t.Errorf("Formato(%v) = %q, want %q", usd, got, want)
		}
	}
}

func TestFormatoNeverReadsZero(t *testing.T) {
	if got := Formato(0); strings.TrimSpace(got) != "" {
		t.Fatalf("zero cost must render empty, got %q", got)
	}
}
