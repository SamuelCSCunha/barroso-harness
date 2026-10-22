package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── helpers ───────────────────────────────────────────────────────────────

func specsByName(t *testing.T, search *Busca) map[string]tools.Spec {
	t.Helper()
	out := map[string]tools.Spec{}
	for _, s := range Especificacoes(search) {
		out[s.Name] = s
	}
	return out
}

func run(t *testing.T, spec tools.Spec, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return spec.Exec(context.Background(), t.TempDir(), raw)
}

// fixtureHTML is the test page: title, script and style that must NOT
// appear, entities that MUST be decoded, and blocks that need a space
// boundary between them.
const fixtureHTML = `<!DOCTYPE html>
<html>
<head>
<title>Café Page &amp; Co</title>
<style>body { color: red; } .x { display: none; }</style>
<script>var junk = "MUST_NOT_APPEAR"; console.log("script_noise_1");</script>
</head>
<body>
<h1>Visible Naïve Title</h1>
<p>First paragraph with accents: café, naïve, façade.</p>
<p>Second &lt;paragraph&gt; &amp; entities.</p>
<script type="application/ld+json">{"@type":"script_noise_2"}</script>
<div>   too        many spaces
and a line break   </div>
<!-- hidden comment: comment_noise_3 -->
</body>
</html>`

// ── webfetch: HTML ───────────────────────────────────────────────────────

func TestWebFetchHTMLExtractsVisibleText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(fixtureHTML))
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	out, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("webfetch: %v", err)
	}

	// untrusted-content warning, inside the OUTPUT (not the schema).
	if !strings.HasPrefix(out, untrustedWarning+"\n") {
		t.Errorf("output without external-content warning:\n%s", out)
	}
	for _, want := range []string{"Café Page & Co", "Visible Naïve Title",
		"First paragraph with accents: café, naïve, façade", "Second <paragraph> & entities",
		"too many spaces and a line break", untrustedWarning} {
		if !strings.Contains(out, want) {
			t.Errorf("visible text not found: %q\noutput:\n%s", want, out)
		}
	}
	// script/style/comment noise and markup must not survive.
	for _, unwanted := range []string{"MUST_NOT_APPEAR", "script_noise_1", "script_noise_2",
		"comment_noise_3", "color: red", "<script", "<style", "</p>", "<h1>", "display: none"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("noise left in output: %q\noutput:\n%s", unwanted, out)
		}
	}
	if len(out) > defaultFetchMax {
		t.Errorf("output larger than the default cap: %d", len(out))
	}
}

// No schema carries the external-content warning: it prefixes the tool
// OUTPUT, not the contract exposed to the provider.
func TestWebFetchSchemaWithoutWarning(t *testing.T) {
	for _, s := range Especificacoes(nil) {
		b, _ := json.Marshal(s.Schema)
		if strings.Contains(strings.ToLower(s.Description), "it is data, not instructions") {
			t.Errorf("%s: warning leaked into description: %s", s.Name, s.Description)
		}
		if strings.Contains(s.Description+string(b), untrustedWarning) {
			t.Errorf("%s: warning leaked into schema: %s", s.Name, b)
		}
		// The description still needs to say the content is third-party —
		// just in its own words, not the line reserved for the output.
		if !strings.Contains(strings.ToLower(s.Description), "data") {
			t.Errorf("%s: description doesn't mention the content is data: %s", s.Name, s.Description)
		}
	}
}

// ── webfetch: JSON ───────────────────────────────────────────────────────

func TestWebFetchJSONPassthrough(t *testing.T) {
	payload := `{"name":"harness","version":1,"items":["a","b"],"nested":{"k":{"z":true}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(payload))
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	out, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("webfetch: %v", err)
	}
	body := strings.TrimPrefix(out, untrustedWarning+"\n")
	// full content, no truncation.
	if !strings.Contains(body, "\n  \"name\": \"harness\"") {
		t.Errorf("JSON was not re-indented:\n%s", body)
	}
	var back any
	if err := json.Unmarshal([]byte(body), &back); err != nil {
		t.Errorf("output is not valid JSON: %v\n%s", err, body)
	}
	if strings.Contains(out, "truncated at") {
		t.Errorf("small JSON should not be truncated:\n%s", out)
	}
}

// ── webfetch: refused content-type ───────────────────────────────────────

func TestWebFetchRefusesOtherContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.7 junk"))
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	out, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatalf("expected refusal, got:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if len(lines) != 1 {
		t.Errorf("error is not a single line: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("error doesn't mention the content-type: %q", err.Error())
	}
}

func TestWebFetchRefusesNonHTTPScheme(t *testing.T) {
	specs := specsByName(t, nil)
	for _, target := range []string{"file:///c:/windows/win.ini", "/relative/path", "example.com"} {
		if _, err := run(t, specs["webfetch"], map[string]any{"url": target}); err == nil {
			t.Errorf("invalid scheme accepted: %q", target)
		}
	}
	if _, err := run(t, specs["webfetch"], map[string]any{"url": ""}); err == nil {
		t.Error("empty url accepted")
	}
}

// ── webfetch: non-2XX status ─────────────────────────────────────────────

func TestWebFetchNon2XXReturnsOneLineError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`<html><body><h1>404 Not Found</h1></body></html>`))
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	out, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatalf("expected error, got:\n%s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "404") {
		t.Errorf("message without the status: %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("error is not a single line: %q", msg)
	}
}

// ── webfetch: redirects ──────────────────────────────────────────────────

func TestWebFetchFollowsRedirects(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 3 hops: within the limit of 5.
		hops++
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle", http.StatusFound)
		case "/middle":
			http.Redirect(w, r, "/end", http.StatusFound)
		case "/end":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("final destination reached"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	out, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL + "/start"})
	if err != nil {
		t.Fatalf("webfetch: %v", err)
	}
	if !strings.Contains(out, "final destination reached") {
		t.Errorf("redirect not followed:\n%s", out)
	}
	if hops != 3 {
		t.Errorf("expected 3 requests, got %d", hops)
	}
}

func TestNewHTTPClientLimitsRedirects(t *testing.T) {
	// Infinite redirect loop: CheckRedirect must give up at 5.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newHTTPClient(5e9).Do(req)
	if err == nil {
		t.Fatal("expected too-many-redirects error")
	}
	if !strings.Contains(err.Error(), "5") {
		t.Errorf("error doesn't mention the limit: %v", err)
	}
}

// ── webfetch: cap and truncation marker ──────────────────────────────────

func TestWebFetchTruncatesWithMarker(t *testing.T) {
	// 200 KB of text: over the 2 MiB body cap? No — over the character cap,
	// which is what matters here.
	big := strings.Repeat("abcdefghij", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(big))
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	out, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL, "maxChars": 500})
	if err != nil {
		t.Fatalf("webfetch: %v", err)
	}
	if len(out) > 500 {
		t.Errorf("output exceeded the requested maxChars: %d bytes", len(out))
	}
	if !strings.Contains(out, "(truncated at") || !strings.Contains(out, "bytes)") {
		t.Errorf("truncation marker missing:\n%s", out)
	}
	if !strings.Contains(out, "of 200000 bytes)") {
		t.Errorf("marker doesn't mention the original total:\n%s", out)
	}
}

// Default cap and hard cap: 20000 by default, 60000 at most.
func TestWebFetchDefaultCapAndHardCap(t *testing.T) {
	big := strings.Repeat("x", 200000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(big))
	}))
	defer srv.Close()

	specs := specsByName(t, nil)
	noCap, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(noCap) > defaultFetchMax {
		t.Errorf("default cap exceeded: %d", len(noCap))
	}
	if !strings.Contains(noCap, "(truncated at") {
		t.Errorf("no truncation marker at the default cap:\n%s", noCap[:200])
	}

	beyond, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL, "maxChars": 999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(beyond) > fetchCap {
		t.Errorf("hard cap of %d exceeded: %d", fetchCap, len(beyond))
	}
}

// ── cleanHTML: unit ───────────────────────────────────────────────────────

func TestCleanHTMLTitle(t *testing.T) {
	title, _ := cleanHTML([]byte(`<html><head><title>  Résumé &amp;   Sub-résumé  </title></head><body>x</body></html>`))
	if title != "Résumé & Sub-résumé" {
		t.Errorf("title = %q", title)
	}

	if t2, _ := cleanHTML([]byte(`<body>no title here</body>`)); t2 != "" {
		t.Errorf("title should be empty, got %q", t2)
	}
}

func TestCleanHTMLRemovesScriptStyle(t *testing.T) {
	body := []byte(`<div>before</div>
<script>var x = "<b>fake bold</b>"; /* noise */</script>
<style>.c{content:"does not appear"} .d{font:bold}</style>
<div>after</div>`)
	_, text := cleanHTML(body)
	for _, unwanted := range []string{"var x", "fake bold", "content:", ".c{", ".d{", "does not appear"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("script/style leaked (%q): %q", unwanted, text)
		}
	}
	if !strings.Contains(text, "before") || !strings.Contains(text, "after") {
		t.Errorf("visible text lost: %q", text)
	}
}

func TestCleanHTMLDecodesEntities(t *testing.T) {
	_, text := cleanHTML([]byte(`<p>a &amp; b &lt;c&gt; &quot;d&quot; &#39;e&#39; &nbsp; f &copy;</p>`))
	for _, want := range []string{"a & b", "<c>", `"d"`, "'e'", "f ©"} {
		if !strings.Contains(text, want) {
			t.Errorf("entity not decoded: %q in %q", want, text)
		}
	}
	if strings.Contains(text, "&amp;") || strings.Contains(text, "&#39;") {
		t.Errorf("raw entity left over: %q", text)
	}
}

func TestCleanHTMLCollapsesWhitespace(t *testing.T) {
	_, text := cleanHTML([]byte("<p>one\n\tthree    four</p>\n\n\n<p>five</p>"))
	if text != "one three four five" {
		t.Errorf("whitespace not collapsed: %q", text)
	}
}

// Adjacent blocks must not glue words together: </p><p> must become a space.
func TestCleanHTMLTagBoundary(t *testing.T) {
	_, text := cleanHTML([]byte(`<p>foo</p><p>bar</p><ul><li>a</li><li>b</li></ul>`))
	if strings.Contains(text, "foobar") {
		t.Errorf("words glued together: %q", text)
	}
	for _, want := range []string{"foo", "bar", "a", "b"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lost (%q): %q", want, text)
		}
	}
}

func TestCleanHTMLRemovesComments(t *testing.T) {
	_, text := cleanHTML([]byte(`<p>café</p><!-- invisible_comment --><p>also</p>`))
	if strings.Contains(text, "invisible_comment") {
		t.Errorf("comment leaked: %q", text)
	}
	if !strings.Contains(text, "café") || !strings.Contains(text, "also") {
		t.Errorf("text around the comment lost: %q", text)
	}
}

// Self-closing tags and <script src=...></script> must not swallow the rest.
func TestCleanHTMLSelfClosingTags(t *testing.T) {
	_, text := cleanHTML([]byte(`<div>a</div><img src="x.png"/><br/><div>b</div><script src="x.js"/>`))
	if !strings.Contains(text, "a") || !strings.Contains(text, "b") {
		t.Errorf("text lost with self-closing tags: %q", text)
	}
}

// ── websearch ────────────────────────────────────────────────────────────

// searchFixture serves a SearXNG envelope with the given body.
func searchFixture(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("missing format=json: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("q") != "harness barroso" {
			t.Errorf("wrong query: %q", r.URL.Query().Get("q"))
		}
		if r.URL.Query().Get("language") != "pt-BR" {
			t.Errorf("wrong language: %q", r.URL.Query().Get("language"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func TestWebSearchAgainstFakeSearXNG(t *testing.T) {
	srv := searchFixture(t, `{"results":[
		{"title":"First &amp; Result","url":"https://a.example/1","content":"snippet one &lt;b&gt;ok&lt;/b&gt;"},
		{"title":"Second Result","url":"https://b.example/2","content":"snippet two"},
		{"title":"No URL","content":"must not appear"}
	]}`)
	defer srv.Close()

	out, err := run(t, specsByName(t, &Busca{Endpoint: srv.URL + "/search"})["websearch"],
		map[string]any{"query": "harness barroso"})
	if err != nil {
		t.Fatalf("websearch: %v", err)
	}

	// numbered format with title — URL and an indented snippet.
	if !strings.Contains(out, "1. First & Result — https://a.example/1") {
		t.Errorf("line 1 has the wrong format:\n%s", out)
	}
	if !strings.Contains(out, "2. Second Result — https://b.example/2") {
		t.Errorf("line 2 has the wrong format:\n%s", out)
	}
	if !strings.Contains(lineAt(out, 1), "   snippet one <b>ok</b>") {
		t.Errorf("snippet missing or not indented:\n%s", out)
	}
	if !strings.Contains(out, "(2 of 3 results)") {
		t.Errorf("dropped-results counter missing:\n%s", out)
	}
	if strings.Contains(out, "No URL") {
		t.Errorf("result without URL leaked:\n%s", out)
	}
	if !strings.HasPrefix(out, untrustedWarning+"\n") {
		t.Errorf("output without external-content warning:\n%s", out)
	}
	if len(out) > searchCap {
		t.Errorf("output larger than the cap: %d", len(out))
	}
}

// lineAt returns the n-th line (0-based) of the output, skipping the warning.
func lineAt(out string, n int) string {
	lines := strings.Split(out, "\n")
	if n+1 >= len(lines) {
		return ""
	}
	return lines[n+1]
}

func TestWebSearchMaxResults(t *testing.T) {
	var items []string
	for i := 'a'; i <= 'j'; i++ {
		items = append(items, `{"title":"Item `+string(i)+`","url":"https://x.example/`+string(i)+`","content":"c"}`)
	}
	srv := searchFixture(t, `{"results":[`+strings.Join(items, ",")+`]}`)
	defer srv.Close()

	specs := specsByName(t, &Busca{Endpoint: srv.URL})
	out, err := run(t, specs["websearch"], map[string]any{"query": "harness barroso", "maxResults": 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3. Item c —") {
		t.Errorf("third result missing:\n%s", out)
	}
	if strings.Contains(out, "4. Item d") {
		t.Errorf("maxResults ignored:\n%s", out)
	}
	if len(resultLines(out)) > 3 {
		t.Errorf("more than 3 result lines: %d", len(resultLines(out)))
	}
}

func resultLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n")[1:] { // skip the warning
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "(") {
			continue // snippet or footer
		}
		lines = append(lines, l)
	}
	return lines
}

func TestWebSearchNon2XX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	// A failing SearXNG now falls back to DuckDuckGo; keep the test offline by
	// making the fallback fail too, so the error carries both reasons.
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ddg.Close()
	prev := endpointDDG
	endpointDDG = ddg.URL
	t.Cleanup(func() { endpointDDG = prev })

	specs := specsByName(t, &Busca{Endpoint: srv.URL})
	out, err := run(t, specs["websearch"], map[string]any{"query": "x"})
	if err == nil {
		t.Fatalf("expected error, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("message without the status: %q", err.Error())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error is not a single line: %q", err.Error())
	}
}

// fixtureDDG reproduces the lite page markup measured on 2026-10-18: a link
// through the redirector (uddg), an ad (y.js) that must disappear, and a
// result without a snippet to prove that pairing by order doesn't misalign.
const fixtureDDG = `<table>
<tr><td>1.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Fdevel%2Frelease&amp;rut=abc" class='result-link'>Release History - The <b>Go</b> Programming Language</a></td></tr>
<tr><td>&nbsp;</td><td class='result-snippet'>Go 1.3 is a major <b>release</b> &amp; more.</td></tr>
<tr><td>2.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fduckduckgo.com%2Fy.js%3Fad%3D1&amp;rut=x" class='result-link'>Sponsored Ad</a></td></tr>
<tr><td>&nbsp;</td><td class='result-snippet'>buy now</td></tr>
<tr><td>3.&nbsp;</td><td><a rel="nofollow" href="https://example.org/direct" class='result-link'>Direct</a></td></tr>
</table>`

// Without search_url, search does NOT fail: it falls back to DuckDuckGo lite.
// Before this fallback the tool returned a configuration error and search was
// effectively disabled for every fresh install.
func TestWebSearchWithoutEndpointUsesDuckDuckGo(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(fixtureDDG))
	}))
	defer srv.Close()
	prev := endpointDDG
	endpointDDG = srv.URL + "/lite/"
	t.Cleanup(func() { endpointDDG = prev })

	for name, search := range map[string]*Busca{
		"nil":    nil,
		"empty":  {},
		"spaces": {Endpoint: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := run(t, specsByName(t, search)["websearch"], map[string]any{"query": "go release"})
			if err != nil {
				t.Fatalf("without search_url it should fall back to DuckDuckGo, got error: %v", err)
			}
			if query != "go release" {
				t.Errorf("query sent = %q", query)
			}
			if !strings.Contains(out, "https://go.dev/doc/devel/release") {
				t.Errorf("real URL (decoded from uddg) missing:\n%s", out)
			}
			if !strings.Contains(out, "Release History - The Go Programming Language") {
				t.Errorf("tag-free title missing:\n%s", out)
			}
			if !strings.Contains(out, "Go 1.3 is a major release & more.") {
				t.Errorf("tag/entity-free snippet missing:\n%s", out)
			}
			if strings.Contains(out, "y.js") || strings.Contains(out, "Sponsored Ad") {
				t.Errorf("ad should be dropped:\n%s", out)
			}
			if !strings.Contains(out, "https://example.org/direct") {
				t.Errorf("direct link should pass through:\n%s", out)
			}
		})
	}
}

// An HTTP error from the fallback is still one line and names the source.
func TestWebSearchDuckDuckGoHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	prev := endpointDDG
	endpointDDG = srv.URL
	t.Cleanup(func() { endpointDDG = prev })

	_, err := run(t, specsByName(t, nil)["websearch"], map[string]any{"query": "x"})
	if err == nil {
		t.Fatal("expected HTTP 429 error")
	}
	if !strings.Contains(err.Error(), "DuckDuckGo") || !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention DuckDuckGo and the status: %q", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error is not a single line: %q", err)
	}
}

// With search_url configured, SearXNG remains the path — the fallback must
// not steal searches from whoever configured it.
func TestWebSearchWithEndpointDoesNotUseDuckDuckGo(t *testing.T) {
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("DuckDuckGo should not be called with search_url configured")
	}))
	defer ddg.Close()
	prev := endpointDDG
	endpointDDG = ddg.URL
	t.Cleanup(func() { endpointDDG = prev })

	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://sx.example/a","content":"c"}]}`))
	}))
	defer sx.Close()

	out, err := run(t, specsByName(t, &Busca{Endpoint: sx.URL})["websearch"], map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("SearXNG: %v", err)
	}
	if !strings.Contains(out, "https://sx.example/a") {
		t.Errorf("SearXNG result missing:\n%s", out)
	}
}

// The tool must be in the catalog even without Busca — the model needs to
// see the schema in order to then get the configuration error.
func TestEspecificacoesIncludesSearchWithoutConfig(t *testing.T) {
	specs := specsByName(t, nil)
	s, ok := specs["websearch"]
	if !ok {
		t.Fatal("websearch missing from the catalog with nil Busca")
	}
	if len(s.Schema) == 0 {
		t.Error("websearch without schema")
	}
	if _, ok := s.Schema["properties"].(map[string]any)["query"]; !ok {
		t.Errorf("schema without the query property: %v", s.Schema)
	}
	if _, ok := specs["webfetch"]; !ok {
		t.Error("webfetch missing from the catalog")
	}
}

// ── spec contract ─────────────────────────────────────────────────────────

func TestEspecificacoesSchemaContract(t *testing.T) {
	for _, s := range Especificacoes(nil) {
		if s.Name == "" || s.Description == "" {
			t.Errorf("spec %q without name or description", s.Name)
		}
		sch, ok := s.Schema["type"].(string)
		if !ok || sch != "object" {
			t.Errorf("%s: schema type = %v", s.Name, s.Schema["type"])
		}
		props, ok := s.Schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: schema without properties", s.Name)
			continue
		}
		req, ok := s.Schema["required"].([]string)
		if !ok || len(req) == 0 {
			t.Errorf("%s: schema without required", s.Name)
		}
		for _, r := range req {
			if _, exists := props[r]; !exists {
				t.Errorf("%s: required %q not in properties", s.Name, r)
			}
		}
		if s.Exec == nil {
			t.Errorf("%s: Exec nil", s.Name)
		}
	}
}

// No tool returns more than its declared cap.
func TestNoOutputExceedsCap(t *testing.T) {
	// fetch with a large HTML page.
	bigHTML := "<html><head><title>g</title></head><body>" +
		strings.Repeat("<p>paragraph with plenty of text inside to count</p>", 3000) +
		"</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(bigHTML))
	}))
	defer srv.Close()

	specs := specsByName(t, &Busca{Endpoint: srv.URL})
	for _, tc := range []struct {
		name  string
		args  map[string]any
		limit int
	}{
		{"webfetch", map[string]any{"url": srv.URL}, fetchCap},
		{"webfetch", map[string]any{"url": srv.URL, "maxChars": 1000}, 1000},
		{"websearch", map[string]any{"query": "x"}, searchCap},
	} {
		out, err := run(t, specs[tc.name], tc.args)
		if err != nil {
			continue // an error is not output
		}
		if len(out) > tc.limit {
			t.Errorf("%s: %d bytes > cap %d", tc.name, len(out), tc.limit)
		}
	}
}

// Harness UA in both tools.
func TestHarnessUserAgent(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		if r.URL.Query().Get("format") == "json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[]}`))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	if dealer.UserAgent != "barroso-harness" {
		t.Fatalf("dealer UserAgent changed: %q", dealer.UserAgent)
	}
	specs := specsByName(t, &Busca{Endpoint: srv.URL})
	if _, err := run(t, specs["webfetch"], map[string]any{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, specs["websearch"], map[string]any{"query": "x"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	for i, ua := range seen {
		if ua != dealer.UserAgent {
			t.Errorf("request %d: User-Agent = %q, want %q", i, ua, dealer.UserAgent)
		}
	}
}
