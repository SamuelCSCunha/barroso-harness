package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

func provider(models string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fake-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, models)
	}))
}

func TestConfigurarProviderRollback401(t *testing.T) {
	old := config.Config{CurrentProvider: "old", Model: "before", Effort: "high",
		Providers: map[string]*config.Provider{"old": {BaseURL: "https://old/v1", APIKey: "secret-old"}}}
	cfg := old
	srv := provider(`{"error":"bad"}`, http.StatusUnauthorized)
	defer srv.Close()
	var out, errOut strings.Builder
	err := configurarProvider(IO{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}, &cfg, srv.URL+"/v1", "fake-key")
	if err == nil {
		t.Fatal("401 deve falhar")
	}
	if cfg.CurrentProvider != old.CurrentProvider || cfg.Model != old.Model || cfg.Effort != old.Effort || cfg.Ativo().APIKey != old.Ativo().APIKey {
		t.Fatalf("config alterada apesar de 401: %+v", cfg)
	}
}

func TestConfigurarProviderRollbackJSONInvalido(t *testing.T) {
	old := config.Config{CurrentProvider: "old", Model: "before", Providers: map[string]*config.Provider{"old": {BaseURL: "https://old/v1", APIKey: "secret-old"}}}
	cfg := old
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "oops") }))
	defer srv.Close()
	var out, errOut strings.Builder
	err := configurarProvider(IO{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}, &cfg, srv.URL, "fake-key")
	if err == nil {
		t.Fatal("JSON inválido deve falhar")
	}
	if cfg.CurrentProvider != old.CurrentProvider || cfg.Model != old.Model || cfg.Ativo().APIKey != old.Ativo().APIKey {
		t.Fatalf("config alterada apesar de JSON inválido: %+v", cfg)
	}
}

func TestOnboardingEscolheModeloEEsforcoVazio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") != "Bearer fake-key" {
				http.Error(w, "unauthorized", 401)
				return
			}
			io.WriteString(w, `{"data":[{"id":"zeta","input":["text"]},{"id":"barroso-chat","input":["text","image"],"context_length":250000},{"id":"audio","input":["audio"]}]}`)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if _, exists := req["reasoning_effort"]; exists {
				t.Errorf("reasoning_effort deve ficar omitido no default: %v", req["reasoning_effort"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	oldPrompt := PromptKey
	PromptKey = func(string) (string, error) { return "fake-key", nil }
	defer func() { PromptKey = oldPrompt }()

	var out, errOut strings.Builder
	iofake := IO{In: strings.NewReader(srv.URL + "/v1\n2\n"), Out: &out, ErrOut: &errOut}
	cfg := config.Config{}
	if err := Onboarding(iofake, &cfg); err != nil {
		t.Fatalf("onboarding falhou: %v; stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
	if cfg.Model != "zeta" { // IDs ordenados alfab.: barroso-chat, zeta => 2 deveria ser zeta
		t.Fatalf("model selecionado inesperado: %q; output=%s", cfg.Model, out.String())
	}
	if cfg.Effort != "" || cfg.Janela != 0 || cfg.JanelaSource != "" {
		// selecionou zeta, que não publica context_length; janela derivada vazia
		t.Fatalf("effort/janela default inesperados: %+v", cfg)
	}
	loaded, err := config.Load()
	if err != nil || !loaded.Pronta() {
		t.Fatalf("config não persistida: %+v err=%v", loaded, err)
	}
	if strings.Contains(out.String()+errOut.String(), "fake-key") {
		t.Fatal("saída vazou a chave fake")
	}

	// A primeira mensagem com effort vazio não envia a chave reasoning_effort.
	if err := runtime.NewSession(loaded).Send(nil, io.Discard, t.TempDir(), "teste"); err != nil {
		t.Fatalf("turno teste: %v", err)
	}
}

func TestProcEffortSetEClear(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	cfg := config.Config{CurrentProvider: "p", Model: "m", Providers: map[string]*config.Provider{"p": {BaseURL: "https://example.test/v1", APIKey: "fake"}}}
	sess := runtime.NewSession(cfg)
	var out, errOut strings.Builder
	iofake := IO{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}
	procEffort(iofake, &cfg, sess, "high")
	if cfg.Effort != "high" {
		t.Fatalf("effort não setado: %q", cfg.Effort)
	}
	procEffort(iofake, &cfg, sess, "-")
	if cfg.Effort != "" {
		t.Fatalf("effort não limpou: %q", cfg.Effort)
	}
	if !strings.Contains(out.String(), "SEM reasoning_effort") {
		t.Fatalf("mensagem de clear ausente: %s", out.String())
	}
}
