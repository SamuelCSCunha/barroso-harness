package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHosted is a hosted MCP server: initialize hands out a session id,
// every later call must carry it, tools/list publishes the given tools and
// tools/call answers with callText (or calls onCall).
type fakeHosted struct {
	mu       sync.Mutex
	tools    []map[string]any
	callText string
	sse      bool
	status   int // non-zero: every request gets this status
	meta     map[string]any
	isError  bool
	expireAt int // tools/call number (1-based) answered with 404 once

	session  string
	sessions int
	calls    []map[string]any
	headers  []http.Header
	methods  []string
}

func (f *fakeHosted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, r.Header.Clone())
	if f.status != 0 {
		http.Error(w, "upstream exploded", f.status)
		return
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.methods = append(f.methods, m.Method)
	if m.Method == "initialize" {
		f.sessions++
		f.session = "sess-" + string(rune('0'+f.sessions))
		w.Header().Set("Mcp-Session-Id", f.session)
		f.reply(w, m.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
		return
	}
	if r.Header.Get("Mcp-Session-Id") != f.session {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	switch m.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.reply(w, m.ID, map[string]any{"tools": f.tools})
	case "tools/call":
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		f.calls = append(f.calls, p)
		if f.expireAt > 0 && len(f.calls) == f.expireAt {
			f.expireAt = 0
			f.session = "expired"
			http.Error(w, "session expired", http.StatusNotFound)
			return
		}
		res := map[string]any{"content": []any{map[string]any{"type": "text", "text": f.callText}}}
		if f.meta != nil {
			res["_meta"] = f.meta
		}
		if f.isError {
			res["isError"] = true
		}
		f.reply(w, m.ID, res)
	default:
		http.Error(w, "unknown method", http.StatusNotFound)
	}
}

func (f *fakeHosted) reply(w http.ResponseWriter, id json.RawMessage, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\nevent: message\ndata: " + string(b) + "\n\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (f *fakeHosted) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// Tools as published by the real servers on 2026-10-20 (schemas trimmed).
var (
	exaSearchTool = map[string]any{"name": "web_search_exa", "description": "Search the web for any topic",
		"inputSchema": map[string]any{"type": "object", "required": []any{"query", "objective"},
			"properties": map[string]any{"query": map[string]any{"type": "string"},
				"objective": map[string]any{"type": "string"}, "numResults": map[string]any{"type": "number"}}}}
	exaFetchTool = map[string]any{"name": "web_fetch_exa", "description": "Read a webpage's full content",
		"inputSchema": map[string]any{"type": "object", "required": []any{"urls"},
			"properties": map[string]any{"urls": map[string]any{"type": "array"},
				"maxCharacters": map[string]any{"type": "number"}}}}
	exaResearchTool = map[string]any{"name": "deep_researcher_start", "description": "Start a deep research web search",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}
	parSearchTool = map[string]any{"name": "web_search", "description": "Perform web searches",
		"inputSchema": map[string]any{"type": "object", "required": []any{"objective", "search_queries"},
			"properties": map[string]any{"objective": map[string]any{"type": "string"},
				"search_queries": map[string]any{"type": "array"}, "session_id": map[string]any{"type": "string"}}}}
	parFetchTool = map[string]any{"name": "web_fetch", "description": "Fetch content from web URLs",
		"inputSchema": map[string]any{"type": "object", "required": []any{"urls"},
			"properties": map[string]any{"urls": map[string]any{"type": "array"},
				"full_content": map[string]any{"type": "boolean"},
				"objective":    map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}}}}}
)

const exaSearchText = "Title: Release History - The Go Programming Language\nURL: https://go.dev/doc/devel/release\n" +
	"Published: N/A\nAuthor: N/A\nHighlights:\nThis page summarizes the changes between releases.\n\n" +
	"Title: Go 1.25 Release Notes\nURL: https://go.dev/doc/go1.25\nPublished: 2025-08-12\nHighlights:\nThe latest Go release."

const parSearchText = `{"search_id":"s1","results":[{"url":"https://go.dev/doc/devel/release","title":"Release History",` +
	`"excerpts":["go1.27.0 (released 2026-08-19)"]},{"url":"","title":"no url"}]}`

func newFakeProvider(t *testing.T, name string, f *fakeHosted) *ProvedorMCP {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newProvedor(name, srv.URL)
}

// noDDG fails the test if DuckDuckGo is reached.
func noDDG(t *testing.T) {
	t.Helper()
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("DuckDuckGo should not be called")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ddg.Close)
	prev := endpointDDG
	endpointDDG = ddg.URL
	t.Cleanup(func() { endpointDDG = prev })
}

func TestProviderSearchExaShape(t *testing.T) {
	noDDG(t)
	f := &fakeHosted{tools: []map[string]any{exaResearchTool, exaFetchTool, exaSearchTool}, callText: exaSearchText, sse: true}
	p := newFakeProvider(t, "exa", f)

	out, err := run(t, specsByName(t, &Busca{Providers: []*ProvedorMCP{p}})["websearch"],
		map[string]any{"query": "go release", "maxResults": 3})
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	if !strings.HasPrefix(out, untrustedWarning+"\n") {
		t.Errorf("missing untrusted warning:\n%s", out)
	}
	for _, want := range []string{"1. Release History - The Go Programming Language — https://go.dev/doc/devel/release",
		"This page summarizes", "2. Go 1.25 Release Notes — https://go.dev/doc/go1.25"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Published") || strings.Contains(out, "Author: N/A") {
		t.Errorf("metadata leaked into snippets:\n%s", out)
	}
	call := f.calls[0]
	if call["name"] != "web_search_exa" {
		t.Errorf("tool chosen = %v; want web_search_exa (not the research tool)", call["name"])
	}
	args := call["arguments"].(map[string]any)
	if args["query"] != "go release" || args["objective"] != "go release" || args["numResults"] != float64(3) {
		t.Errorf("arguments = %v", args)
	}
}

func TestProviderSearchParallelShape(t *testing.T) {
	noDDG(t)
	f := &fakeHosted{tools: []map[string]any{parFetchTool, parSearchTool}, callText: parSearchText}
	p := newFakeProvider(t, "parallel", f)

	out, err := run(t, specsByName(t, &Busca{Providers: []*ProvedorMCP{p}})["websearch"], map[string]any{"query": "go"})
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	if !strings.Contains(out, "1. Release History — https://go.dev/doc/devel/release") ||
		!strings.Contains(out, "go1.27.0 (released 2026-08-19)") {
		t.Errorf("normalized result missing:\n%s", out)
	}
	if strings.Contains(out, "no url") {
		t.Errorf("result without URL kept:\n%s", out)
	}
	args := f.calls[0]["arguments"].(map[string]any)
	if f.calls[0]["name"] != "web_search" || args["objective"] != "go" {
		t.Errorf("call = %v", f.calls[0])
	}
	if q, _ := args["search_queries"].([]any); len(q) != 1 || q[0] != "go" {
		t.Errorf("search_queries = %v", args["search_queries"])
	}
	if _, ok := args["session_id"]; ok {
		t.Errorf("optional session_id must not be invented: %v", args)
	}
}

// Requests are anonymous: no key, no Authorization, no cookie — and the
// session id the server handed out is sent back.
func TestProviderRequestsAreAnonymousAndKeepSession(t *testing.T) {
	f := &fakeHosted{tools: []map[string]any{parSearchTool}, callText: parSearchText}
	p := newFakeProvider(t, "parallel", f)
	if _, err := p.search(context.Background(), "x", 5); err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := strings.Join(f.methods, ","); got != "initialize,notifications/initialized,tools/list,tools/call" {
		t.Errorf("methods = %s", got)
	}
	for i, h := range f.headers {
		for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "Exa-Api-Key"} {
			if h.Get(k) != "" {
				t.Errorf("request %d carries %s", i, k)
			}
		}
		if h.Get("User-Agent") != "barroso-harness" {
			t.Errorf("request %d User-Agent = %q", i, h.Get("User-Agent"))
		}
		if i > 0 && h.Get("Mcp-Session-Id") != "sess-1" {
			t.Errorf("request %d session = %q", i, h.Get("Mcp-Session-Id"))
		}
	}
	// Discovery is cached: a second search is a single tools/call.
	before := len(f.methods)
	if _, err := p.search(context.Background(), "y", 5); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if got := f.methods[before:]; len(got) != 1 || got[0] != "tools/call" {
		t.Errorf("second search methods = %v; want only tools/call", got)
	}
}

// An expired session (404 with a session id) re-handshakes once and the
// call goes through.
func TestProviderReinitializesExpiredSession(t *testing.T) {
	f := &fakeHosted{tools: []map[string]any{parSearchTool}, callText: parSearchText, expireAt: 1}
	p := newFakeProvider(t, "parallel", f)
	if _, err := p.search(context.Background(), "x", 5); err != nil {
		t.Fatalf("search after expiry: %v", err)
	}
	if f.sessions != 2 {
		t.Errorf("initialize count = %d; want 2", f.sessions)
	}
}

// A failing first provider moves on to the second; a provider that has no
// compatible tool is skipped without calling anything.
func TestProviderFallbackChain(t *testing.T) {
	noDDG(t)
	broken := &fakeHosted{status: http.StatusInternalServerError}
	noTool := &fakeHosted{tools: []map[string]any{exaResearchTool}}
	good := &fakeHosted{tools: []map[string]any{parSearchTool}, callText: parSearchText}
	b := &Busca{Providers: []*ProvedorMCP{
		newFakeProvider(t, "broken", broken), newFakeProvider(t, "notool", noTool), newFakeProvider(t, "good", good)}}

	out, err := run(t, specsByName(t, b)["websearch"], map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	if !strings.Contains(out, "https://go.dev/doc/devel/release") {
		t.Errorf("result from the working provider missing:\n%s", out)
	}
	if noTool.callCount() != 0 {
		t.Error("provider without a compatible tool was called")
	}
}

// SearXNG keeps precedence: when it answers, no provider is touched.
func TestSearXNGPrecedesProviders(t *testing.T) {
	noDDG(t)
	tripwire := &fakeHosted{tools: []map[string]any{parSearchTool}, callText: parSearchText}
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://sx.example/a"}]}`))
	}))
	defer sx.Close()
	b := &Busca{Endpoint: sx.URL, Providers: []*ProvedorMCP{newFakeProvider(t, "trip", tripwire)}}
	out, err := run(t, specsByName(t, b)["websearch"], map[string]any{"query": "x"})
	if err != nil || !strings.Contains(out, "https://sx.example/a") {
		t.Fatalf("SearXNG result: %v\n%s", err, out)
	}
	if len(tripwire.methods) != 0 {
		t.Errorf("provider contacted although SearXNG answered: %v", tripwire.methods)
	}
}

// Every provider failing (rate limit, isError) ends at DuckDuckGo, and with
// DuckDuckGo failing too the error is ONE line naming every source.
func TestProvidersAllFailFallsToDDG(t *testing.T) {
	rl := &fakeHosted{tools: []map[string]any{exaSearchTool}, callText: "You've hit the free rate limit",
		meta: map[string]any{"ai.exa/rateLimited": true}}
	ie := &fakeHosted{tools: []map[string]any{parSearchTool}, callText: "quota", isError: true}
	ddgHits := 0
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ddgHits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ddg.Close()
	prev := endpointDDG
	endpointDDG = ddg.URL
	t.Cleanup(func() { endpointDDG = prev })

	b := &Busca{Providers: []*ProvedorMCP{newFakeProvider(t, "exa", rl), newFakeProvider(t, "parallel", ie)}}
	_, err := run(t, specsByName(t, b)["websearch"], map[string]any{"query": "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"exa", "rate limit", "parallel", "quota", "DuckDuckGo", "429"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "\n") || ddgHits != 1 {
		t.Errorf("multi-line error or DDG hits=%d: %q", ddgHits, msg)
	}
	// The failed providers are now cooling down: the next call skips them
	// without any request.
	before := len(rl.methods)
	_, _ = run(t, specsByName(t, b)["websearch"], map[string]any{"query": "x"})
	if len(rl.methods) != before {
		t.Error("provider in cooldown was contacted again")
	}
}

func TestProviderFetchPathsAndFallback(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("direct body"))
	}))
	defer page.Close()

	t.Run("exa markdown", func(t *testing.T) {
		f := &fakeHosted{tools: []map[string]any{exaSearchTool, exaFetchTool},
			callText: "# Example Domain\nURL: https://example.com\n\nExample text."}
		p := newFakeProvider(t, "exa", f)
		out, err := run(t, specsByName(t, &Busca{Providers: []*ProvedorMCP{p}})["webfetch"],
			map[string]any{"url": "https://example.com", "maxChars": 500})
		if err != nil {
			t.Fatalf("web_fetch: %v", err)
		}
		if !strings.HasPrefix(out, untrustedWarning+"\n") || !strings.Contains(out, "Example text.") {
			t.Errorf("output:\n%s", out)
		}
		args := f.calls[0]["arguments"].(map[string]any)
		if f.calls[0]["name"] != "web_fetch_exa" || args["maxCharacters"] != float64(500) {
			t.Errorf("call = %v", f.calls[0])
		}
	})
	t.Run("parallel json and cap", func(t *testing.T) {
		long := strings.Repeat("palavra ", 2000)
		f := &fakeHosted{tools: []map[string]any{parFetchTool},
			callText: `{"results":[{"url":"https://example.com","title":"Example Domain","excerpts":["` + long + `"]}]}`}
		p := newFakeProvider(t, "parallel", f)
		out, err := run(t, specsByName(t, &Busca{Providers: []*ProvedorMCP{p}})["webfetch"],
			map[string]any{"url": "https://example.com", "maxChars": 1000})
		if err != nil {
			t.Fatalf("web_fetch: %v", err)
		}
		if len(out) > 1000 || !strings.Contains(out, "Example Domain\nhttps://example.com") || !strings.Contains(out, "truncated") {
			t.Errorf("len=%d output:\n%.300s", len(out), out)
		}
		if f.calls[0]["arguments"].(map[string]any)["full_content"] != true {
			t.Errorf("full_content not requested: %v", f.calls[0])
		}
	})
	t.Run("provider fails, direct GET answers", func(t *testing.T) {
		f := &fakeHosted{status: http.StatusInternalServerError}
		p := newFakeProvider(t, "broken", f)
		// httptest is loopback, so publicURL keeps it local; exercise the
		// fallback through the provider call directly, then the spec.
		if _, err := p.fetch(context.Background(), "https://example.com", 100); err == nil ||
			!strings.Contains(err.Error(), "500") {
			t.Errorf("provider error = %v", err)
		}
		out, err := run(t, specsByName(t, &Busca{Providers: []*ProvedorMCP{p}})["webfetch"], map[string]any{"url": page.URL})
		if err != nil || !strings.Contains(out, "direct body") {
			t.Errorf("direct GET: %v\n%s", err, out)
		}
	})
	t.Run("private URL never reaches the provider", func(t *testing.T) {
		f := &fakeHosted{tools: []map[string]any{parFetchTool}, callText: `{"results":[]}`}
		p := newFakeProvider(t, "trip", f)
		out, err := run(t, specsByName(t, &Busca{Providers: []*ProvedorMCP{p}})["webfetch"], map[string]any{"url": page.URL})
		if err != nil || !strings.Contains(out, "direct body") {
			t.Errorf("direct GET: %v\n%s", err, out)
		}
		if len(f.methods) != 0 {
			t.Errorf("loopback URL sent to provider: %v", f.methods)
		}
	})
}

// A provider that hangs is bounded by the call's ctx: the chain moves on.
func TestProviderTimeoutMovesOn(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	p := newProvedor("slow", slow.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.search(ctx, "x", 3); err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout not honoured: %s", time.Since(start))
	}
}

func TestPublicURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://example.com/x":   true,
		"http://go.dev":           true,
		"http://127.0.0.1:8080/":  false,
		"http://localhost/":       false,
		"http://10.0.0.5/":        false,
		"http://192.168.1.1/":     false,
		"http://[::1]/":           false,
		"http://169.254.169.254/": false,
		"http://intranet/":        false,
		"http://nas.local/":       false,
		"ftp://example.com/":      false,
	} {
		if got := publicURL(u); got != want {
			t.Errorf("publicURL(%q) = %v; want %v", u, got, want)
		}
	}
}

func TestProvedoresRegistry(t *testing.T) {
	ps := Provedores([]string{"Parallel", "exa", "parallel", "bogus"})
	if len(ps) != 2 || ps[0].Nome != "parallel" || ps[1].Nome != "exa" {
		t.Fatalf("Provedores = %v", ps)
	}
	if ps[0].endpoint != endpointParallel || ps[1].endpoint != endpointExa {
		t.Errorf("endpoints = %s, %s", ps[0].endpoint, ps[1].endpoint)
	}
	if again := Provedores([]string{"exa"}); again[0] != ps[1] {
		t.Error("providers are not singletons")
	}
	if len(Provedores([]string{"off"})) != 0 {
		t.Error(`["off"] should disable all providers`)
	}
}
