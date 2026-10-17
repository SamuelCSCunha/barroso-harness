// Package config — testes: default URL, migração do shape antigo, Save/Load
// canônico (providers/current_provider), effort vazio e segredo fora do
// output de erro/estado.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func home(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", dir)
	return dir, func() {}
}

func TestLoadSemArquivoDevolveVazioErroNil(t *testing.T) {
	home(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("sem config.json não deve ser erro: %v", err)
	}
	if c.Pronta() {
		t.Fatal("config vazia não está pronta")
	}
}

func TestSaveLoadRoundTripComProvider(t *testing.T) {
	home(t)
	c := Config{Model: "barroso-chat", Effort: ""}
	c = atualizarProviderTeste(c, "keys-barroso", DefaultBaseURL, "secret-key")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := got.Ativo()
	if p == nil || p.BaseURL != DefaultBaseURL || p.APIKey != "secret-key" {
		t.Fatalf("provider ativo errado: %+v", p)
	}
	if got.Model != "barroso-chat" || got.Effort != "" {
		t.Fatalf("model/effort errados: %+v", got)
	}
}

func atualizarProviderTeste(c Config, nome, base, key string) Config {
	if c.Providers == nil {
		c.Providers = map[string]*Provider{}
	}
	c.Providers[nome] = &Provider{BaseURL: base, APIKey: key}
	c.CurrentProvider = nome
	c.BaseURL, c.APIKey = base, key
	return c
}

func TestJSONDiscoComShapeCanonicoeSpoiler(t *testing.T) {
	home(t)
	c := atualizarProviderTeste(Config{Model: "m"}, "p", "https://x/v1", "k123")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(Dir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disco struct {
		CurrentProvider string               `json:"current_provider"`
		Providers       map[string]*Provider `json:"providers"`
		BaseURLRoot     string               `json:"base_url"`
	}
	if err := json.Unmarshal(b, &disco); err != nil {
		t.Fatal(err)
	}
	if disco.BaseURLRoot != "" {
		t.Fatalf("base_url não pode duplicar na raiz: %q", disco.BaseURLRoot)
	}
	if disco.CurrentProvider != "p" || disco.Providers["p"].BaseURL != "https://x/v1" {
		t.Fatalf("shape canônico violado: %s", b)
	}
}

func TestMigracaoShapeAntigo(t *testing.T) {
	home(t)
	legacy := []byte(`{"base_url":"https://keys.barroso.tec.br/v1","model":"barroso-chat"}`)
	if err := os.WriteFile(filepath.Join(Dir(), "config.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentProvider != "keys.barroso" {
		t.Fatalf("provider migrado errado: %q", c.CurrentProvider)
	}
	if c.Ativo().BaseURL != DefaultBaseURL {
		t.Fatalf("base migrada errada: %q", c.Ativo().BaseURL)
	}
	if c.Model != "barroso-chat" {
		t.Fatalf("model migrado perdido: %q", c.Model)
	}
	if c.Pronta() {
		t.Fatal("sem chave no disco não fica pronta (pede na interação)")
	}
	// persistir a migração mantém o shape novo
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, _ := Load()
	if got.CurrentProvider != "keys.barroso" || got.Model != "barroso-chat" {
		t.Fatalf("round trip da migração falhou: %+v", got)
	}
}

func TestMigracaoURLForaDaCasa(t *testing.T) {
	home(t)
	legacy := []byte(`{"base_url":"https://outro.example/v1","model":"m","context_window":16000}`)
	if err := os.WriteFile(filepath.Join(Dir(), "config.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := Load()
	if c.CurrentProvider != "default" {
		t.Fatalf("esperava provider default, veio %q", c.CurrentProvider)
	}
	if c.Janela != 16000 || c.JanelaSource != FonteManual {
		t.Fatalf("janela manual não preservada: %d/%s", c.Janela, c.JanelaSource)
	}
}

func TestResumoNuncaTemChave(t *testing.T) {
	c := atualizarProviderTeste(Config{Model: "m", Effort: "high"}, "p", "https://x/v1", "SEGREDO-123")
	s := c.Resumo()
	if strings.Contains(s, "SEGREDO") {
		t.Fatalf("Resumo vazou a chave: %s", s)
	}
	f := strings.Join(c.Faltas(), "; ")
	if strings.Contains(f, "SEGREDO") {
		t.Fatalf("Faltas vazou a chave: %s", f)
	}
}

func TestEsforcoVazioOMiteCampo(t *testing.T) {
	home(t)
	c := atualizarProviderTeste(Config{Model: "m"}, "p", "https://x/v1", "k")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(), "config.json"))
	if strings.Contains(string(b), `"effort"`) {
		t.Fatalf("effort vazio não deve aparecer no JSON: %s", b)
	}
}

func TestSelecionarProviderPreservaManual(t *testing.T) {
	c := Config{Janela: 32000, JanelaSource: FonteManual}
	c.SelecionarProvider("p", 250000)
	if c.Janela != 32000 {
		t.Fatalf("janela manual foi sobrescrita: %d", c.Janela)
	}
	c2 := Config{Janela: 1000, JanelaSource: FonteCatalogo}
	c2.SelecionarProvider("p", 250000)
	if c2.Janela != 250000 {
		t.Fatalf("janela de catálogo não atualizou: %d", c2.Janela)
	}
	c3 := Config{JanelaSource: FonteCatalogo}
	c3.SelecionarProvider("p", 0)
	if c3.Janela != 0 || c3.JanelaSource != "" {
		t.Fatalf("janela derivada deveria zerar: %+v", c3)
	}
}
