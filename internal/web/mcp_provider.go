package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/jsonrpc"
)

// Hosted MCP providers that answer without an account or API key, checked on
// 2026-10-20 with initialize + tools/list from a clean client. The endpoints
// are CONSTANTS on purpose: nothing the model or the config writes turns into
// an MCP endpoint, so there is no new SSRF surface.
const (
	endpointExa      = "https://mcp.exa.ai/mcp"
	endpointParallel = "https://search.parallel.ai/mcp"
)

// DefaultProviders is the order used when search_providers is absent.
var DefaultProviders = []string{"exa", "parallel"}

const (
	// providerBodyCap limits one MCP response (search or fetch): the same
	// order as searchBodyCap, well below the MCP client's 32 MiB.
	providerBodyCap = 4 << 20
	// providerCooldown: after a provider fails (network, HTTP error, rate
	// limit, no compatible tool) it is skipped for a while, so every call
	// does not pay its timeout again before reaching the next source.
	providerCooldown = 2 * time.Minute
	// mcpProtocolVersion matches internal/mcp's announced version.
	mcpProtocolVersion = "2025-06-18"
)

// ProvedorMCP is ONE hosted MCP server used internally by web_search and
// web_fetch. Its tools never enter the catalog: the model keeps seeing only
// web_search/web_fetch, and the remote names cannot shadow anything.
//
// Discovery (initialize, notifications/initialized, tools/list) is lazy and
// cached; the search and fetch tools are resolved from the published names,
// descriptions and schemas, not from hard-coded names. Requests carry only
// Content-Type, Accept, User-Agent and the server's own Mcp-Session-Id —
// never a key, cookie or Authorization header.
type ProvedorMCP struct {
	Nome     string
	endpoint string

	mu        sync.Mutex
	transport *jsonrpc.HTTP
	ready     bool
	tools     []remoteTool
	until     time.Time    // cooldown after a failure
	nextID    atomic.Int64 // aligned on 386, unlike a bare int64
}

var (
	registryMu sync.Mutex
	registry   = map[string]*ProvedorMCP{}
)

// Provedores returns the providers for the given names, in order, as
// process-wide singletons (the discovery cache survives across turns).
// Unknown names are ignored, so ["off"] disables them all.
func Provedores(names []string) []*ProvedorMCP {
	registryMu.Lock()
	defer registryMu.Unlock()
	var out []*ProvedorMCP
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if seen[n] {
			continue
		}
		var endpoint string
		switch n {
		case "exa":
			endpoint = endpointExa
		case "parallel":
			endpoint = endpointParallel
		default:
			continue
		}
		seen[n] = true
		p := registry[n]
		if p == nil {
			p = newProvedor(n, endpoint)
			registry[n] = p
		}
		out = append(out, p)
	}
	return out
}

func newProvedor(name, endpoint string) *ProvedorMCP {
	return &ProvedorMCP{Nome: name, endpoint: endpoint}
}

// remoteTool is the part of a tools/list entry the matcher reads.
type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// callResult is the tools/call result. _meta is read only for the rate-limit
// flag Exa sets on an otherwise successful-looking answer.
type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
	IsError bool           `json:"isError"`
	Meta    map[string]any `json:"_meta,omitempty"`
}

// ── search / fetch ────────────────────────────────────────────────────────

// search runs the provider's search tool and normalizes the answer to the
// same shape as SearXNG. Zero parseable results is a failure (the next source
// gets its chance), not "(no results)".
func (p *ProvedorMCP) search(ctx context.Context, q string, limit int) ([]searxngResult, error) {
	text, err := p.call(ctx, searchTimeout, kindSearch, func(t remoteTool) (map[string]any, bool) {
		return searchArgs(t, q, limit)
	})
	if err != nil {
		return nil, err
	}
	rs := parseSearchText(text)
	if len(rs) == 0 {
		return nil, fmt.Errorf("%s: answer without parseable results: %s", p.Nome, oneLine(text, 200))
	}
	return rs, nil
}

// fetch runs the provider's fetch tool and returns the page text.
func (p *ProvedorMCP) fetch(ctx context.Context, target string, limit int) (string, error) {
	text, err := p.call(ctx, fetchTimeout, kindFetch, func(t remoteTool) (map[string]any, bool) {
		return fetchArgs(t, target, limit)
	})
	if err != nil {
		return "", err
	}
	content, err := parseFetchText(text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p.Nome, err)
	}
	return content, nil
}

// call resolves the tool of the given kind, builds its arguments and runs
// tools/call within the timeout. An expired session re-handshakes ONCE.
func (p *ProvedorMCP) call(ctx context.Context, timeout time.Duration, kind toolKind,
	build func(remoteTool) (map[string]any, bool)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := p.cooling(); err != nil {
		return "", err
	}
	text, err := p.callOnce(ctx, kind, build)
	if errors.Is(err, jsonrpc.ErrSessionExpired) {
		p.invalidate()
		text, err = p.callOnce(ctx, kind, build)
	}
	if err != nil {
		p.fail()
		return "", fmt.Errorf("%s: %w", p.Nome, err)
	}
	return text, nil
}

func (p *ProvedorMCP) callOnce(ctx context.Context, kind toolKind,
	build func(remoteTool) (map[string]any, bool)) (string, error) {
	t, list, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	tool, args, ok := pickTool(list, kind, build)
	if !ok {
		return "", fmt.Errorf("no compatible %s tool published", kind)
	}
	res, err := t.Call(ctx, jsonrpc.NewRequest(p.id(), "tools/call", map[string]any{
		"name": tool.Name, "arguments": args,
	}))
	if err != nil {
		return "", err
	}
	var out callResult
	if err := jsonrpc.DecodeResult(res.Result, &out); err != nil {
		return "", fmt.Errorf("tools/call %s: malformed result: %w", tool.Name, err)
	}
	var parts []string
	for _, b := range out.Content {
		if t := strings.ToLower(b.Type); (t == "" || t == "text") && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if out.IsError {
		return "", fmt.Errorf("%s reported an error: %s", tool.Name, oneLine(orEmpty(text), 300))
	}
	if rateLimited(out.Meta) {
		return "", fmt.Errorf("anonymous rate limit reached")
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s returned no text", tool.Name)
	}
	return text, nil
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no detail)"
	}
	return s
}

// rateLimited reads a provider _meta flag such as {"ai.exa/rateLimited": true}.
func rateLimited(meta map[string]any) bool {
	for k, v := range meta {
		k = strings.ToLower(k)
		if b, ok := v.(bool); ok && b && (strings.Contains(k, "ratelimit") || strings.Contains(k, "rate_limit")) {
			return true
		}
	}
	return false
}

// ── discovery ─────────────────────────────────────────────────────────────

// discover returns the transport and the tool list, running the handshake
// and tools/list the first time (or after invalidate).
func (p *ProvedorMCP) discover(ctx context.Context) (*jsonrpc.HTTP, []remoteTool, error) {
	p.mu.Lock()
	if p.ready {
		t, list := p.transport, p.tools
		p.mu.Unlock()
		return t, list, nil
	}
	if p.transport == nil {
		p.transport = &jsonrpc.HTTP{
			URL:       p.endpoint,
			UserAgent: dealer.UserAgent,
			Client:    newHTTPClient(fetchTimeout),
			MaxBody:   providerBodyCap,
		}
	}
	t := p.transport
	p.mu.Unlock()

	t.ResetSession()
	if _, err := t.Call(ctx, jsonrpc.NewRequest(p.id(), "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": dealer.UserAgent, "version": "1.0"},
	})); err != nil {
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}
	if err := t.Notify(ctx, jsonrpc.NewNotification("notifications/initialized", nil)); err != nil {
		return nil, nil, fmt.Errorf("notifications/initialized: %w", err)
	}
	res, err := t.Call(ctx, jsonrpc.NewRequest(p.id(), "tools/list", nil))
	if err != nil {
		return nil, nil, fmt.Errorf("tools/list: %w", err)
	}
	var list struct {
		Tools []remoteTool `json:"tools"`
	}
	if err := jsonrpc.DecodeResult(res.Result, &list); err != nil {
		return nil, nil, fmt.Errorf("tools/list: malformed result: %w", err)
	}

	p.mu.Lock()
	p.tools, p.ready = list.Tools, true
	p.mu.Unlock()
	return t, list.Tools, nil
}

func (p *ProvedorMCP) id() int64 { return p.nextID.Add(1) }

func (p *ProvedorMCP) invalidate() {
	p.mu.Lock()
	p.ready = false
	p.mu.Unlock()
}

// fail starts the cooldown and drops the cached discovery: the next attempt
// after the pause starts from a fresh session.
func (p *ProvedorMCP) fail() {
	p.mu.Lock()
	p.ready = false
	p.until = time.Now().Add(providerCooldown)
	p.mu.Unlock()
}

func (p *ProvedorMCP) cooling() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rest := time.Until(p.until); rest > 0 {
		return fmt.Errorf("%s: skipped for %s after a recent failure", p.Nome, rest.Round(time.Second))
	}
	return nil
}

// ── tool matching ─────────────────────────────────────────────────────────

type toolKind string

const (
	kindSearch toolKind = "search"
	kindFetch  toolKind = "fetch"
)

// pickTool returns the best-scoring tool of the kind whose schema the
// arguments can satisfy. Scoring is by name and description; a tool whose
// required fields cannot be filled is never chosen.
func pickTool(list []remoteTool, kind toolKind, build func(remoteTool) (map[string]any, bool)) (remoteTool, map[string]any, bool) {
	type cand struct {
		t     remoteTool
		args  map[string]any
		score int
	}
	var cands []cand
	for _, t := range list {
		s := scoreTool(t, kind)
		if s <= 0 {
			continue
		}
		args, ok := build(t)
		if !ok {
			continue
		}
		cands = append(cands, cand{t, args, s})
	}
	if len(cands) == 0 {
		return remoteTool{}, nil, false
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	return cands[0].t, cands[0].args, true
}

func scoreTool(t remoteTool, kind toolKind) int {
	name := strings.ToLower(t.Name)
	desc := strings.ToLower(t.Description)
	score := 0
	switch kind {
	case kindSearch:
		switch {
		case strings.Contains(name, "web_search"), strings.Contains(name, "search_web"):
			score = 10
		case strings.Contains(name, "search"):
			score = 5
		}
		// Specialized or long-running variants are worse than plain web search.
		for _, bad := range []string{"research", "deep", "code", "company", "people", "linkedin", "fetch", "crawl"} {
			if strings.Contains(name, bad) {
				score -= 6
			}
		}
	case kindFetch:
		switch {
		case strings.Contains(name, "web_fetch"), strings.Contains(name, "fetch"):
			score = 10
		case strings.Contains(name, "crawl"), strings.Contains(name, "extract"), strings.Contains(name, "contents"):
			score = 6
		case strings.Contains(name, "read") && strings.Contains(desc, "url"):
			score = 4
		}
		if strings.Contains(name, "search") {
			score -= 6
		}
	}
	if score > 0 && strings.Contains(desc, "web") {
		score++
	}
	return score
}

// schemaProps returns the properties and the required set of a schema.
func schemaProps(t remoteTool) (map[string]map[string]any, map[string]bool) {
	props := map[string]map[string]any{}
	if raw, ok := t.InputSchema["properties"].(map[string]any); ok {
		for k, v := range raw {
			if m, ok := v.(map[string]any); ok {
				props[k] = m
			} else {
				props[k] = map[string]any{}
			}
		}
	}
	req := map[string]bool{}
	if raw, ok := t.InputSchema["required"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				req[s] = true
			}
		}
	}
	return props, req
}

// propType reads "type" (or the first non-null type of anyOf).
func propType(p map[string]any) string {
	if s, ok := p["type"].(string); ok {
		return s
	}
	if list, ok := p["anyOf"].([]any); ok {
		for _, v := range list {
			if m, ok := v.(map[string]any); ok {
				if s, ok := m["type"].(string); ok && s != "null" {
					return s
				}
			}
		}
	}
	return ""
}

func normKey(k string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
}

// searchArgs fills a search schema: query/q/objective take the text, an
// array of queries takes [q], a count field takes the limit. ok=false when a
// required field has no meaning to us.
func searchArgs(t remoteTool, q string, limit int) (map[string]any, bool) {
	props, req := schemaProps(t)
	args := map[string]any{}
	hasText := false
	for k, p := range props {
		typ := propType(p)
		switch nk := normKey(k); {
		case (nk == "query" || nk == "q" || nk == "objective" || nk == "searchquery") && (typ == "string" || typ == ""):
			args[k] = q
			hasText = true
		case (nk == "searchqueries" || nk == "queries") && typ == "array":
			args[k] = []string{q}
			hasText = true
		case (nk == "numresults" || nk == "maxresults" || nk == "limit" || nk == "count") &&
			(typ == "number" || typ == "integer"):
			args[k] = limit
		}
	}
	return args, hasText && coversRequired(args, req)
}

// fetchArgs fills a fetch schema: urls takes [u], url takes u; a character
// cap takes the limit; full_content asks for the page rather than excerpts
// (the output is capped by wrap anyway).
func fetchArgs(t remoteTool, target string, limit int) (map[string]any, bool) {
	props, req := schemaProps(t)
	args := map[string]any{}
	hasURL := false
	for k, p := range props {
		typ := propType(p)
		switch nk := normKey(k); {
		case nk == "urls" && typ == "array":
			args[k] = []string{target}
			hasURL = true
		case (nk == "url" || nk == "uri") && (typ == "string" || typ == ""):
			args[k] = target
			hasURL = true
		case (nk == "maxcharacters" || nk == "maxchars" || nk == "maxlength") && (typ == "number" || typ == "integer"):
			args[k] = limit
		case nk == "fullcontent" && typ == "boolean":
			args[k] = true
		}
	}
	return args, hasURL && coversRequired(args, req)
}

func coversRequired(args map[string]any, req map[string]bool) bool {
	for k := range req {
		if _, ok := args[k]; !ok {
			return false
		}
	}
	return true
}

// ── normalization ─────────────────────────────────────────────────────────

// providerResult covers the JSON shape {"results":[{url,title,...}]}
// (Parallel). Text fields vary by provider; all are optional.
type providerResult struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Excerpts    []string `json:"excerpts"`
	Highlights  []string `json:"highlights"`
	Content     string   `json:"content"`
	Text        string   `json:"text"`
	Snippet     string   `json:"snippet"`
	FullContent string   `json:"full_content"`
}

// body picks the result text: for a search the short fields come first, for
// a fetch the fullest one does.
func (r providerResult) body(full bool) string {
	excerpts := strings.Join(r.Excerpts, "\n\n")
	highlights := strings.Join(r.Highlights, "\n\n")
	order := []string{r.Content, r.Text, r.Snippet, excerpts, highlights, r.FullContent}
	if full {
		order = []string{r.FullContent, r.Text, r.Content, excerpts, highlights, r.Snippet}
	}
	for _, s := range order {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

type providerEnvelope struct {
	Results []providerResult `json:"results"`
	Errors  []any            `json:"errors"`
}

// decodeEnvelope parses text as {"results":[...]} when it is JSON.
func decodeEnvelope(text string) (providerEnvelope, bool) {
	var env providerEnvelope
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "{") || json.Unmarshal([]byte(s), &env) != nil {
		return env, false
	}
	return env, true
}

// parseSearchText accepts the JSON envelope or the "Title:/URL:" text blocks
// (Exa). Anything else yields no results.
func parseSearchText(text string) []searxngResult {
	if env, ok := decodeEnvelope(text); ok {
		out := make([]searxngResult, 0, len(env.Results))
		for _, r := range env.Results {
			if strings.TrimSpace(r.URL) == "" {
				continue
			}
			out = append(out, searxngResult{Title: r.Title, URL: r.URL, Content: r.body(false)})
		}
		return out
	}
	var (
		out []searxngResult
		cur *searxngResult
		sb  strings.Builder
	)
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.URL) != "" {
			cur.Content = sb.String()
			out = append(out, *cur)
		}
		cur = nil
		sb.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "Title:"):
			flush()
			cur = &searxngResult{Title: strings.TrimSpace(strings.TrimPrefix(l, "Title:"))}
		case strings.HasPrefix(l, "URL:"):
			if cur != nil && cur.URL != "" {
				flush() // a second URL without a Title starts a new result
			}
			if cur == nil {
				cur = &searxngResult{}
			}
			cur.URL = strings.TrimSpace(strings.TrimPrefix(l, "URL:"))
		case cur == nil:
		case strings.HasPrefix(l, "Published:"), strings.HasPrefix(l, "Author:"),
			strings.HasPrefix(l, "Highlights:"), strings.HasPrefix(l, "Text:"), strings.HasPrefix(l, "Summary:"):
			if rest := strings.TrimSpace(l[strings.Index(l, ":")+1:]); rest != "" &&
				!strings.HasPrefix(l, "Published:") && !strings.HasPrefix(l, "Author:") {
				sb.WriteString(rest + " ")
			}
		default:
			if sb.Len() < 2000 {
				sb.WriteString(l + " ")
			}
		}
	}
	flush()
	return out
}

// parseFetchText turns a fetch answer into page text: the JSON envelope
// becomes "title\nURL\n\nbody", plain text/markdown passes through trimmed.
func parseFetchText(text string) (string, error) {
	env, ok := decodeEnvelope(text)
	if !ok {
		if s := strings.TrimSpace(text); s != "" {
			return s, nil
		}
		return "", fmt.Errorf("empty page")
	}
	var parts []string
	for _, r := range env.Results {
		body := strings.TrimSpace(r.body(true))
		if body == "" {
			continue
		}
		head := strings.TrimSpace(r.Title)
		if u := strings.TrimSpace(r.URL); u != "" {
			head = strings.TrimSpace(head + "\n" + u)
		}
		parts = append(parts, strings.TrimSpace(head+"\n\n"+body))
	}
	if len(parts) == 0 {
		if len(env.Errors) > 0 {
			b, _ := json.Marshal(env.Errors)
			return "", fmt.Errorf("fetch failed: %s", oneLine(string(b), 300))
		}
		return "", fmt.Errorf("answer without page content")
	}
	return strings.Join(parts, "\n\n"), nil
}

// ── privacy guard ─────────────────────────────────────────────────────────

// publicURL reports whether a URL may be handed to a third-party provider.
// Loopback, private, link-local and single-label hosts stay on the direct
// GET: the provider could not reach them anyway, and the address itself
// would leak internal topology.
func publicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast())
	}
	return strings.Contains(host, ".")
}
