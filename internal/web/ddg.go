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

// endpointDDG é a versão "lite" do DuckDuckGo: HTML estático, sem JavaScript,
// uma tabela por resultado. É o fallback do web_search quando não há SearXNG
// configurado — a busca funciona de fábrica, e o search_url passa a ser
// melhoria (mais fontes, sem raspagem), não pré-requisito.
//
// Variável, não constante, só para o teste apontar para um servidor local.
var endpointDDG = "https://lite.duckduckgo.com/lite/"

// As duas marcas que importam na página lite, medidas em 2026-10-18:
//
//	<a rel="nofollow" href="//duckduckgo.com/l/?uddg=<URL codificada>&amp;rut=…" class='result-link'>Título</a>
//	<td class='result-snippet'>trecho com <b>destaque</b></td>
//
// O link real vem no parâmetro uddg do redirecionador; o snippet segue o link
// na mesma ordem. Regex basta: é uma página de formato fixo, e um parser de
// árvore seria dependência nova para ler duas classes.
var (
	reLinkDDG    = regexp.MustCompile(`(?s)<a[^>]*href=["']([^"']+)["'][^>]*class=['"]result-link['"][^>]*>(.*?)</a>`)
	reSnippetDDG = regexp.MustCompile(`(?s)<td[^>]*class=['"]result-snippet['"][^>]*>(.*?)</td>`)
)

// buscarDDG consulta o DuckDuckGo lite e devolve os resultados no mesmo
// formato do SearXNG, para a formatação ser uma só.
func buscarDDG(ctx context.Context, q string) ([]resultadoSearXNG, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	alvo := endpointDDG + "?" + url.Values{"q": []string{q}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
	if err != nil {
		return nil, fmt.Errorf("requisição: %w", err)
	}
	req.Header.Set("User-Agent", dealer.UserAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := novoHTTP(timeoutBusca).Do(req)
	if err != nil {
		return nil, fmt.Errorf("busca (DuckDuckGo) falhou: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("DuckDuckGo HTTP %d: %s", resp.StatusCode, resumoHTTP(b))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, tetoBodyBusca+1))
	if err != nil {
		return nil, fmt.Errorf("leitura do corpo: %w", err)
	}
	if len(b) > tetoBodyBusca {
		return nil, fmt.Errorf("resposta excedeu %d bytes", tetoBodyBusca)
	}
	return analisarDDG(string(b)), nil
}

// analisarDDG extrai (título, URL, snippet) da página lite. Links e snippets
// são pareados por ordem; um resultado sem snippet não derruba o par seguinte
// porque o snippet só é usado quando existe na mesma posição.
func analisarDDG(pagina string) []resultadoSearXNG {
	links := reLinkDDG.FindAllStringSubmatch(pagina, -1)
	snippets := reSnippetDDG.FindAllStringSubmatch(pagina, -1)
	out := make([]resultadoSearXNG, 0, len(links))
	for i, l := range links {
		u := urlRealDDG(html.UnescapeString(l[1]))
		if u == "" {
			continue
		}
		r := resultadoSearXNG{
			Titulo: textoDDG(l[2]),
			URL:    u,
		}
		if i < len(snippets) {
			r.Conteudo = textoDDG(snippets[i][1])
		}
		out = append(out, r)
	}
	return out
}

// urlRealDDG tira a URL de destino do redirecionador (//duckduckgo.com/l/?uddg=…).
// Link já direto (http/https) passa como está; anúncio e link interno do
// DuckDuckGo sem uddg são descartados.
func urlRealDDG(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if destino := u.Query().Get("uddg"); destino != "" {
		if strings.Contains(destino, "duckduckgo.com/y.js") {
			return "" // anúncio
		}
		return destino
	}
	if (u.Scheme == "http" || u.Scheme == "https") && !strings.HasSuffix(u.Host, "duckduckgo.com") {
		return href
	}
	return ""
}

// textoDDG tira tags e entidades e achata espaço.
func textoDDG(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(htmlTextoSemTags(s))), " ")
}
