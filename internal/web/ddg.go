package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// endpointDDG is DuckDuckGo's "lite" version: static HTML, no JavaScript,
// one table per result. It is websearch's fallback when no SearXNG is
// configured — search works out of the box, and search_url becomes an
// improvement (more sources, no scraping), not a prerequisite.
//
// A variable, not a constant, only so tests can point it at a local server.
var endpointDDG = "https://lite.duckduckgo.com/lite/"

// The two markers that matter on the lite page, measured on 2026-10-18:
//
//	<a rel="nofollow" href="//duckduckgo.com/l/?uddg=<encoded URL>&amp;rut=…" class='result-link'>Title</a>
//	<td class='result-snippet'>snippet with <b>highlight</b></td>
//
// The real link comes in the redirector's uddg parameter; the snippet follows
// the link in the same order. Regex is enough: the page has a fixed format,
// and a tree parser would be a new dependency just to read two classes.
var (
	reLinkDDG    = regexp.MustCompile(`(?s)<a[^>]*href=["']([^"']+)["'][^>]*class=['"]result-link['"][^>]*>(.*?)</a>`)
	reSnippetDDG = regexp.MustCompile(`(?s)<td[^>]*class=['"]result-snippet['"][^>]*>(.*?)</td>`)
)

// searchDDG queries DuckDuckGo lite and returns results in the same shape as
// SearXNG, so formatting happens in one place.
func searchDDG(ctx context.Context, q string) ([]searxngResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target := endpointDDG + "?" + url.Values{"q": []string{q}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("User-Agent", dealer.UserAgent())
	req.Header.Set("Accept", "text/html")

	resp, err := newHTTPClient(searchTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("search (DuckDuckGo) failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("DuckDuckGo HTTP %d: %s", resp.StatusCode, summarizeHTTP(b))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, searchBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if len(b) > searchBodyCap {
		return nil, fmt.Errorf("response exceeded %d bytes", searchBodyCap)
	}
	return parseDDG(string(b)), nil
}

// parseDDG extracts (title, URL, snippet) from the lite page. Links and
// snippets are paired by order; a result without a snippet doesn't break the
// next pair because the snippet is only used when present at the same index.
func parseDDG(page string) []searxngResult {
	links := reLinkDDG.FindAllStringSubmatch(page, -1)
	snippets := reSnippetDDG.FindAllStringSubmatch(page, -1)
	out := make([]searxngResult, 0, len(links))
	for i, l := range links {
		u := realURLDDG(html.UnescapeString(l[1]))
		if u == "" {
			continue
		}
		r := searxngResult{
			Title: textDDG(l[2]),
			URL:   u,
		}
		if i < len(snippets) {
			r.Content = textDDG(snippets[i][1])
		}
		out = append(out, r)
	}
	return out
}

// realURLDDG extracts the destination URL from the redirector (//duckduckgo.com/l/?uddg=…).
// An already-direct link (http/https) passes through as is; ads and internal
// DuckDuckGo links without uddg are dropped.
func realURLDDG(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if dest := u.Query().Get("uddg"); dest != "" {
		if strings.Contains(dest, "duckduckgo.com/y.js") {
			return "" // ad
		}
		return dest
	}
	if (u.Scheme == "http" || u.Scheme == "https") && !strings.HasSuffix(u.Host, "duckduckgo.com") {
		return href
	}
	return ""
}

// textDDG strips tags and entities and collapses whitespace.
func textDDG(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(stripHTMLTags(s))), " ")
}
