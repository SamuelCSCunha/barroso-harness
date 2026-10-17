package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// smokeServer cobre /v1/models e /v1/chat/completions com roteiro previsível
// e devolve a lista de requests capturados (para inspecionar reasoning_effort).
func smokeServer(t *testing.T, catálogo string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	vistos := &[]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer fake-key" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, catálogo)
		case "/v1/chat/completions":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			*vistos = append(*vistos, req)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"turno ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, vistos
}

const catálogoSmoke = `{"data":[
	{"id":"barroso-chat","input":["text","image"],"context_length":250000},
	{"id":"zz-ultimo","input":["text"]},
	{"id":"audio-x","input":["audio"]}
]}`

// TestSmokeOnboardingTurnoModelEffort cobre o fluxo completo pedido no plano:
// onboarding, turno, /effort high (entra no request), /effort - (sai do
// request), /model por índice e /exit — nada de chave na saída.
func TestSmokeOnboardingTurnoModelEffort(t *testing.T) {
	srv, vistos := smokeServer(t, catálogoSmoke)
	defer srv.Close()
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	antigo := PromptKey
	PromptKey = func(string) (string, error) { return "fake-key", nil }
	defer func() { PromptKey = antigo }()

	entrada := strings.Join([]string{
		srv.URL + "/v1", // base URL
		"1",             // modelo nº 1 da lista alfabética = barroso-chat
		"oi",            // turno 1: effort desligado
		"/effort high",
		"de novo", // turno 2: reasoning_effort=high
		"/effort -",
		"terceiro", // turno 3: campo volta a ser omitido
		"/model 2", // zz-ultimo, sem context_length publicada
		"/exit",
	}, "\n") + "\n"

	var out, errOut strings.Builder
	if code := RunInteractive(IO{In: strings.NewReader(entrada), Out: &out, ErrOut: &errOut}); code != 0 {
		t.Fatalf("exit %d\nout=%s\nerr=%s", code, out.String(), errOut.String())
	}
	tela := out.String() + errOut.String()
	if strings.Contains(tela, "fake-key") {
		t.Fatalf("a chave fictícia vazou na saída:\n%s", tela)
	}
	if n := len(*vistos); n != 3 {
		t.Fatalf("esperava 3 turnos no provider, vieram %d", n)
	}
	if _, ok := (*vistos)[0]["reasoning_effort"]; ok {
		t.Fatal("turno 1 deveria sair sem reasoning_effort (default desativado)")
	}
	if (*vistos)[1]["reasoning_effort"] != "high" {
		t.Fatalf("turno 2 deveria ter effort high, veio %v", (*vistos)[1]["reasoning_effort"])
	}
	if _, ok := (*vistos)[2]["reasoning_effort"]; ok {
		t.Fatal("turno 3 deveria voltar sem reasoning_effort depois de /effort -")
	}

	disco, err := os.ReadFile(filepath.Join(config.Dir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
		Window int    `json:"context_window"`
	}
	if err := json.Unmarshal(disco, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "zz-ultimo" || saved.Effort != "" {
		t.Fatalf("disco deveria ter model zz-ultimo e effort limpo: %+v", saved)
	}
	if saved.Window != 0 {
		t.Fatalf("troca para modelo sem janela publicada deveria limpar a derivada, veio %d", saved.Window)
	}
}

// TestSmokeProviderRollbackDepoisTroca: 401 em /provider não altera nada em
// memória nem disco; o provider seguinte grava normalmente.
func TestSmokeProviderRollbackDepoisTroca(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	antigo := PromptKey
	PromptKey = func(string) (string, error) { return "fake-key", nil }
	defer func() { PromptKey = antigo }()

	ruim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer ruim.Close()
	bom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m2","input":["text"],"context_length":128000}]}`)
	}))
	defer bom.Close()

	// config já pronta (evita onboarding): provider local gravado manualmente.
	cfg := configNovoTeste(bom.URL+"/v1", "fake-key", "m2")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	entrada := strings.Join([]string{
		"/provider", ruim.URL + "/v1", // falha com 401 → rollback
		"/provider", bom.URL + "/v1", "1", // sucesso, escolhe m2
		"/exit",
	}, "\n") + "\n"

	var out, errOut strings.Builder
	if code := RunInteractive(IO{In: strings.NewReader(entrada), Out: &out, ErrOut: &errOut}); code != 0 {
		t.Fatalf("exit %d\nout=%s\nerr=%s", code, out.String(), errOut.String())
	}
	tela := out.String() + errOut.String()
	if !strings.Contains(tela, "401") {
		t.Fatalf("o 401 do provider ruim deveria aparecer na saída:\n%s", tela)
	}
	disco, err := os.ReadFile(filepath.Join(config.Dir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disco), strings.TrimPrefix(ruim.URL, "http://")) {
		t.Fatalf("provider que falhou foi gravado em disco:\n%s", disco)
	}
	var saved struct {
		Model           string `json:"model"`
		CurrentProvider string `json:"current_provider"`
		Providers       map[string]*struct {
			BaseURL string `json:"base_url"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(disco, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "m2" {
		t.Fatalf("model esperado m2, veio %q", saved.Model)
	}
	if p := saved.Providers[saved.CurrentProvider]; p == nil || !strings.Contains(p.BaseURL, "127.0.0.1") {
		t.Fatalf("provider ativo deveria ser o servidor bom: %s", disco)
	}
}
