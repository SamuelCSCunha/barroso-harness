// Package dealer — /models catalog tests (httptest): varied inputs, dedup,
// sorting, context_length, auth, trailing slash, body limit and requests
// with/without effort.
package dealer

import (
	"context"
	"encoding/base64"
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

func catalogBody(ifs []string, ctxLen int, ty string) []byte {
	_data := []map[string]any{
		{"id": "zz-last", "context_length": ctxLen, "input": ifs, "type": ty},
		{"id": "aa-first"},
		{"id": "aa-first"}, // duplicate
		{"id": "mm-audio", "input": []string{"audio"}},
		{"id": "tt-tts", "type": "tts"},
		{"id": "embed-model", "type": "embedding"},
	}
	d, _ := json.Marshal(map[string]any{"data": _data, "object": "list"})
	return d
}

func TestModelsSortDedupFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		w.Write(catalogBody([]string{"text"}, 250000, "chat"))
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
	// audio without text is dropped; tts/embedding are dropped; no-input is kept (conservative)
	if !reflect.DeepEqual(ids, []string{"aa-first", "zz-last"}) {
		t.Fatalf("wrong ids: %v", ids)
	}
	if ContextoDe(ls, "zz-last") != 250000 {
		t.Fatalf("context_length lost: %v", ls)
	}
}

func TestModelsWithoutInputAreKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"no-metadata"},{"id":"aa-first"}]}`))
	}))
	defer srv.Close()
	ls, err := Modelos(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 || ls[0].ID != "aa-first" || ls[1].ID != "no-metadata" {
		t.Fatalf("expected aa-first < no-metadata (alphabetical), got %v", ls)
	}
}

func TestCatalogRequiresAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k-true" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	if _, err := Modelos(context.Background(), srv.URL, "k-true"); err != nil {
		t.Fatalf("failed with the right key: %v", err)
	}
	if _, err := Modelos(context.Background(), srv.URL, ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("without a key it should give an actionable 401, got %v", err)
	}
}

func TestCatalogLimit1MiB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"` + strings.Repeat("x", 2<<20) + `"}]}`))
	}))
	defer srv.Close()
	_, err := Modelos(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected a body-larger-than-1-MiB error, got %v", err)
	}
}

func TestCatalogInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	_, err := Modelos(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected a JSON error, got %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://x.example/v1/":                 "https://x.example/v1",
		"https://x.example/v1/chat/completions": "https://x.example/v1",
		"https://x.example":                     "https://x.example",
		" https://x.example/v1/models ":         "https://x.example/v1",
	}
	for input, want := range cases {
		if got := NormalizarURL(input); got != want {
			t.Fatalf("NormalizarURL(%q) = %q; want %q", input, got, want)
		}
	}
}

func TestCatalogShortTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Catalogo(ctx, srv.URL, ""); err == nil {
		t.Fatal("expected a catalog timeout")
	}
}

// ── requests with/without effort ─────────────────────────────────────────

func requestBody(t *testing.T, cfg Config, tools []map[string]any) map[string]any {
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

func TestBodyWithEffortIncludesField(t *testing.T) {
	m := requestBody(t, Config{BaseURL: "https://x/v1", Model: "barroso-chat", Effort: "high", APIKey: "k"}, nil)
	if m["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort missing/wrong: %v", m["reasoning_effort"])
	}
}

func TestBodyWithoutEffortOmitsField(t *testing.T) {
	for _, e := range []string{"", "none"} {
		m := requestBody(t, Config{BaseURL: "https://x/v1", Model: "m", Effort: e}, nil)
		if _, ok := m["reasoning_effort"]; ok {
			t.Fatalf("effort=%q must omit reasoning_effort", e)
		}
	}
}

func TestRunSendsEffort(t *testing.T) {
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
		t.Fatalf("provider received reasoning_effort=%v", seen["reasoning_effort"])
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

// TestErrorSummaryHTMLDoesNotDumpMarkup pins the requested fix: when the
// provider returns HTML (a WAF/edge block page), the error that reaches the
// terminal is ONE useful line — never the whole markup burying the cause.
func TestErrorSummaryHTMLDoesNotDumpMarkup(t *testing.T) {
	blockPage := []byte(`<!DOCTYPE html>
<html lang="en-US">
<head><title>Attention Required! | Cloudflare</title></head>
<body><div class="cf-error-details"><h1>Sorry, you have been blocked</h1></div></body>
</html>`)
	got := errorSummary(blockPage, 300)
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Fatalf("error summary leaked markup: %q", got)
	}
	if strings.Contains(got, "Attention Required") == false && strings.Contains(got, "blocked") == false {
		t.Fatalf("summary lost the useful information: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("error summary must be one line: %q", got)
	}
}

// An OpenAI-format JSON error still yields the message, not the envelope.
func TestErrorSummaryJSONExtractsMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"Invalid or missing API key.","code":"invalid_api_key"}}`)
	got := errorSummary(body, 300)
	if got != "Invalid or missing API key." {
		t.Fatalf("JSON message was not extracted: %q", got)
	}
}

// A real block page served by a catalog 403: the error bubbling up to the CLI
// is short and readable, with no `<html>` on the way.
func TestCatalog403HTMLGivesShortError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Sorry, you have been blocked</title></head><body>security service</body></html>`))
	}))
	defer srv.Close()

	_, err := Catalogo(context.Background(), srv.URL, "wrong-key")
	if err == nil {
		t.Fatal("catalog 403 should fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "<html") || strings.Contains(msg, "<head") {
		t.Fatalf("catalog error leaked HTML:\n%s", msg)
	}
	if len(msg) > 400 {
		t.Fatalf("catalog error is too long (%d bytes): %s", len(msg), msg)
	}
	if !strings.Contains(msg, "403") {
		t.Fatalf("error should report the status: %s", msg)
	}
}

// TestRequestsSendUserAgent pins the client contract: the official client's
// UA (zen.go) goes on BOTH paths — catalog and chat completions, both
// gateway or none. Go's default "Go-http-client/1.1" is 403 bait on WAFs.
func TestRequestsSendUserAgent(t *testing.T) {
	t.Setenv("ZEN_USER_AGENT", "opencode/latest/9.9.9/test")
	var catalogUA, chatUA, sessao string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/models":
			catalogUA = r.Header.Get("User-Agent")
			io.WriteString(w, `{"data":[{"id":"m1","input":["text"]}]}`)
		case r.URL.Path == "/chat/completions":
			chatUA = r.Header.Get("User-Agent")
			sessao = r.Header.Get("x-opencode-session")
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// catalog
	ls, err := Modelos(context.Background(), srv.URL, "k")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(ls) == 0 {
		t.Fatal("empty catalog")
	}
	if catalogUA != UserAgent() {
		t.Fatalf("catalog sent User-Agent %q, want %q", catalogUA, UserAgent())
	}

	// chat completions
	c := Novo(Config{BaseURL: srv.URL, Model: "m1", APIKey: "k"}).ComConversa("sessao-teste")
	if _, err := c.Rodar("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if chatUA != UserAgent() {
		t.Fatalf("chat sent User-Agent %q, want %q", chatUA, UserAgent())
	}
	// canonical session headers travel on the chat route (and never on the
	// catalog one-shot... they do travel as a fresh mint there too; here the
	// chat assertion pins the stable value for the conversation).
	if !strings.HasPrefix(sessao, "ses_") || len(sessao) != 30 {
		t.Fatalf("x-opencode-session = %q; want the canonical ses_ shape", sessao)
	}

	if catalogUA == "Go-http-client/1.1" || chatUA == "Go-http-client/1.1" {
		t.Fatal("request went out with Go's default User-Agent — it would break on WAFs again")
	}
}

// ── multimodal user content ──────────────────────────────────────────────

// A user message with an image becomes content parts: the text (with an
// "[Image N: path]" line) and the image as a base64 data URL. A missing file
// is dropped with a note instead of failing.
func TestBodyUserMessageWithImage(t *testing.T) {
	dir := t.TempDir()
	img := dir + string(os.PathSeparator) + "shot.png"
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(img, png, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := dir + string(os.PathSeparator) + "gone.png"
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m"})
	b, err := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "what is [Image 1] vs [Image 2]?", Images: []string{img, missing}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	user := -1
	for i, m := range body.Messages {
		if m.Role == "user" {
			user = i
			break
		}
	}
	if user < 0 {
		t.Fatal("no user message in the body")
	}
	if err := json.Unmarshal(body.Messages[user].Content, &parts); err != nil {
		t.Fatalf("user content is not parts: %s", body.Messages[user].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[1].Type != "image_url" {
		t.Fatalf("parts = %+v", parts)
	}
	if !strings.Contains(parts[0].Text, "[Image 1: "+img+"]") || !strings.Contains(parts[0].Text, "[Image 2: "+missing+" (file missing") {
		t.Fatalf("text = %q", parts[0].Text)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != want {
		t.Fatalf("image url = %+v", parts[1].ImageURL)
	}
	// The budget estimate does not carry the image bytes; plain messages stay strings.
	e, _ := c.CorpoEstimativa("sys", []Mensagem{{Role: "user", Conteudo: "x", Images: []string{img}}}, nil)
	if strings.Contains(string(e), base64.StdEncoding.EncodeToString(png)) {
		t.Fatal("estimate body should not inline the image")
	}
	p, _ := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "plain"}}, nil)
	if !strings.Contains(string(p), `"content":"plain"`) {
		t.Fatalf("plain user message changed: %s", p)
	}
}
