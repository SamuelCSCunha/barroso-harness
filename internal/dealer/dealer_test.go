// Package dealer — testes do catálogo /models (httptest): inputs variados,
// dedup, ordenação, context_length, auth, trailing slash, limite de body e
// request com/sem effort.
package dealer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func catalogo(ifs []string, ctxLen int, ty string) []byte {
	_data := []map[string]any{
		{"id": "zz-last", "context_length": ctxLen, "input": ifs, "type": ty},
		{"id": "aa-first"},
		{"id": "aa-first"}, // duplicata
		{"id": "mm-audio", "input": []string{"audio"}},
		{"id": "tt-tts", "type": "tts"},
		{"id": "embed-model", "type": "embedding"},
	}
	d, _ := json.Marshal(map[string]any{"data": _data, "object": "list"})
	return d
}

func TestModelosOrdenaDedupFiltra(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("caminho errado: %s", r.URL.Path)
		}
		w.Write(catalogo([]string{"text"}, 250000, "chat"))
	}))
	defer srv.Close()
	ls, err := Modelos(context.Background(), srv.URL+"/v1/", "k")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ls {
		ids = append(ids, m.ID)
	}
	// audio sem text sai; tts/embedding saem; sem-input entra (conservador)
	if !reflect.DeepEqual(ids, []string{"aa-first", "zz-last"}) {
		t.Fatalf("ids errados: %v", ids)
	}
	if ContextoDe(ls, "zz-last") != 250000 {
		t.Fatalf("context_length perdido: %v", ls)
	}
}

func TestModelosSemInputMantem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"sem-metadata"},{"id":"aa-first"}]}`))
	}))
	defer srv.Close()
	ls, err := Modelos(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 || ls[0].ID != "aa-first" || ls[1].ID != "sem-metadata" {
		t.Fatalf("esperava aa-first < sem-metadata (alfabética), veio %v", ls)
	}
}

func TestCatalogoExigeAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k-true" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	if _, err := Modelos(context.Background(), srv.URL, "k-true"); err != nil {
		t.Fatalf("com chave certa falhou: %v", err)
	}
	if _, err := Modelos(context.Background(), srv.URL, ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("sem chave devia dar 401 acionável, veio %v", err)
	}
}

func TestCatalogoLimite1MiB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"` + strings.Repeat("x", 2<<20) + `"}]}`))
	}))
	defer srv.Close()
	_, err := Modelos(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "excedeu") {
		t.Fatalf("esperava erro de corpo maior que 1 MiB, veio %v", err)
	}
}

func TestCatalogoJSONInvalido(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{não é json`))
	}))
	defer srv.Close()
	_, err := Modelos(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "JSON inválido") {
		t.Fatalf("esperava erro de JSON, veio %v", err)
	}
}

func TestNormalizarURL(t *testing.T) {
	casos := map[string]string{
		"https://x.example/v1/":                 "https://x.example/v1",
		"https://x.example/v1/chat/completions": "https://x.example/v1",
		"https://x.example":                     "https://x.example",
		" https://x.example/v1/models ":         "https://x.example/v1",
	}
	for entrada, esperado := range casos {
		if got := NormalizarURL(entrada); got != esperado {
			t.Fatalf("NormalizarURL(%q) = %q; esperava %q", entrada, got, esperado)
		}
	}
}

func TestCatalogoTimeoutCurto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Catalogo(ctx, srv.URL, ""); err == nil {
		t.Fatal("esperava timeout do catálogo")
	}
}

// ── request com/sem effort ───────────────────────────────────────────────

func corpoDe(t *testing.T, cfg Config, tools []map[string]any) map[string]any {
	t.Helper()
	c := Novo(cfg)
	b, err := c.Corpo("sys", nil, tools)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCorpoComEffortIncluiCampo(t *testing.T) {
	m := corpoDe(t, Config{BaseURL: "https://x/v1", Model: "barroso-chat", Effort: "high", APIKey: "k"}, nil)
	if m["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort ausente/errado: %v", m["reasoning_effort"])
	}
}

func TestCorpoSemEffortOMiteCampo(t *testing.T) {
	for _, e := range []string{"", "none"} {
		m := corpoDe(t, Config{BaseURL: "https://x/v1", Model: "m", Effort: e}, nil)
		if _, ok := m["reasoning_effort"]; ok {
			t.Fatalf("effort=%q deve omitir reasoning_effort", e)
		}
	}
}

func TestRodarEnviaEffort(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "ok"}}}})
		w.Write([]byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := Novo(Config{BaseURL: srv.URL, Model: "m", APIKey: "k", Effort: "low"})
	if _, err := c.Rodar("sys", nil, nil); err != nil {
		t.Fatal(err)
	}
	if seen["reasoning_effort"] != "low" {
		t.Fatalf("provider recebeu reasoning_effort=%v", seen["reasoning_effort"])
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

// TestResumoErroHTMLNaoDespejaMarkup é a cravação da correção pedida: quando
// o provider devolve HTML (página de bloqueio do WAF/edge), o erro que chega
// ao terminal é UMA linha útil — nunca o markup inteiro enterrando a causa.
func TestResumoErroHTMLNaoDespejaMarkup(t *testing.T) {
	htmlBloqueio := []byte(`<!DOCTYPE html>
<html lang="en-US">
<head><title>Attention Required! | Cloudflare</title></head>
<body><div class="cf-error-details"><h1>Sorry, you have been blocked</h1></div></body>
</html>`)
	got := resumoErro(htmlBloqueio, 300)
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Fatalf("resumo de erro vazou markup: %q", got)
	}
	if strings.Contains(got, "Attention Required") == false && strings.Contains(got, "blocked") == false {
		t.Fatalf("resumo perdeu a informação útil: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("resumo de erro deve ser uma linha: %q", got)
	}
}

// JSON de erro no formato OpenAI continua entregando a mensagem, não o envelope.
func TestResumoErroJSONExtraiMensagem(t *testing.T) {
	corpo := []byte(`{"error":{"message":"Chave de API inválida ou ausente.","code":"invalid_api_key"}}`)
	got := resumoErro(corpo, 300)
	if got != "Chave de API inválida ou ausente." {
		t.Fatalf("mensagem do JSON não foi extraída: %q", got)
	}
}

// Página de bloqueio real servida por um catálogo 403: o erro que sobe para a
// CLI é curto e legível, sem `<html>` no caminho.
func TestCatalogo403HTMLGeraErroCurto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Sorry, you have been blocked</title></head><body>security service</body></html>`))
	}))
	defer srv.Close()

	_, err := Catalogo(context.Background(), srv.URL, "chave-errada")
	if err == nil {
		t.Fatal("catálogo 403 deveria errar")
	}
	msg := err.Error()
	if strings.Contains(msg, "<html") || strings.Contains(msg, "<head") {
		t.Fatalf("erro do catálogo vazou HTML:\n%s", msg)
	}
	if len(msg) > 400 {
		t.Fatalf("erro do catálogo ficou longo demais (%d bytes): %s", len(msg), msg)
	}
	if !strings.Contains(msg, "403") {
		t.Fatalf("erro deveria informar o status: %s", msg)
	}
}

// TestRequestsMandamUserAgent é a cravação da causa raiz de um 403 falso:
// Go manda "Go-http-client/1.1" por padrão e há WAF (Cloudflare entre eles)
// que bloqueia isso ANTES de olhar a chave. O dealer precisa mandar um UA
// nomeado nos DOIS caminhos — catálogo e chat completions.
func TestRequestsMandamUserAgent(t *testing.T) {
	var uasCatalogo, uasChat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/models":
			uasCatalogo = r.Header.Get("User-Agent")
			io.WriteString(w, `{"data":[{"id":"m1","input":["text"]}]}`)
		case r.URL.Path == "/chat/completions":
			uasChat = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// catálogo
	ls, err := Modelos(context.Background(), srv.URL, "k")
	if err != nil {
		t.Fatalf("catálogo: %v", err)
	}
	if len(ls) == 0 {
		t.Fatal("catálogo vazio")
	}
	if uasCatalogo != UserAgent {
		t.Fatalf("catálogo mandou User-Agent %q, esperado %q", uasCatalogo, UserAgent)
	}

	// chat completions
	c := Novo(Config{BaseURL: srv.URL, Model: "m1", APIKey: "k"})
	if _, err := c.Rodar("sys", []Mensagem{{Role: "user", Conteudo: "oi"}}, nil); err != nil {
		t.Fatalf("rodar: %v", err)
	}
	if uasChat != UserAgent {
		t.Fatalf("chat mandou User-Agent %q, esperado %q", uasChat, UserAgent)
	}

	if uasCatalogo == "Go-http-client/1.1" || uasChat == "Go-http-client/1.1" {
		t.Fatal("request saiu com o User-Agent padrão do Go — volta a quebrar em WAF")
	}
}
