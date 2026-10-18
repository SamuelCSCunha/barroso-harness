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

func especificas(t *testing.T, busca *Busca) map[string]tools.Spec {
	t.Helper()
	out := map[string]tools.Spec{}
	for _, s := range Especificacoes(busca) {
		out[s.Name] = s
	}
	return out
}

func rodar(t *testing.T, spec tools.Spec, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return spec.Exec(context.Background(), t.TempDir(), raw)
}

// fixtureHTML é a página de teste: título, script e style que NÃO podem
// aparecer, entidade que DEVE ser decodificada, e blocos que precisam de
// fronteira de espaço entre si.
const fixtureHTML = `<!DOCTYPE html>
<html>
<head>
<title>Página de Teste &amp; Cia</title>
<style>body { color: red; } .x { display: none; }</style>
<script>var lixo = "NAO_DEVE_APARECER"; console.log("script_noise_1");</script>
</head>
<body>
<h1>Título Visível</h1>
<p>Primeiro parágrafo com acentuação: ação, coração.</p>
<p>Segundo &lt;parágrafo&gt; &amp; entidades.</p>
<script type="application/ld+json">{"@type":"script_noise_2"}</script>
<div>   espaços        demais
e quebra de linha   </div>
<!-- comentário oculto: comment_noise_3 -->
</body>
</html>`

// ── web_fetch: HTML ───────────────────────────────────────────────────────

func TestWebFetchHTMLExtraiTextoVisivel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(fixtureHTML))
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	out, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}

	// aviso de conteúdo não confiável, dentro da SAÍDA (não do schema).
	if !strings.HasPrefix(out, avisoNaoConfiavel+"\n") {
		t.Errorf("saída sem aviso de conteúdo externo:\n%s", out)
	}
	for _, quer := range []string{"Página de Teste & Cia", "Título Visível",
		"Primeiro parágrafo com acentuação", "Segundo <parágrafo> & entidades",
		"espaços demais e quebra de linha", avisoNaoConfiavel} {
		if !strings.Contains(out, quer) {
			t.Errorf("texto visível não encontrado: %q\nsaída:\n%s", quer, out)
		}
	}
	// ruído de script/style/comentário e markup não podem sobreviver.
	for _, naoQuer := range []string{"NAO_DEVE_APARECER", "script_noise_1", "script_noise_2",
		"comment_noise_3", "color: red", "<script", "<style", "</p>", "<h1>", "display: none"} {
		if strings.Contains(out, naoQuer) {
			t.Errorf("ruído sobrou na saída: %q\nsaída:\n%s", naoQuer, out)
		}
	}
	if len(out) > maxFetchPadrao {
		t.Errorf("saída maior que o teto padrão: %d", len(out))
	}
}

// Nenhum schema carrega o aviso de conteúdo externo: ele é prefixo da SAÍDA
// da tool, não do contrato exposto ao provider.
func TestWebFetchSchemaSemAviso(t *testing.T) {
	for _, s := range Especificacoes(nil) {
		b, _ := json.Marshal(s.Schema)
		if strings.Contains(strings.ToLower(s.Description), "é dado, não instrução") {
			t.Errorf("%s: aviso vazou na description: %s", s.Name, s.Description)
		}
		if strings.Contains(s.Description+string(b), avisoNaoConfiavel) {
			t.Errorf("%s: aviso vazou no schema: %s", s.Name, b)
		}
		// A descrição ainda precisa avisar que o conteúdo é de terceiros —
		// só que em palavras próprias, não na linha reservada à saída.
		if !strings.Contains(strings.ToLower(s.Description), "dado") {
			t.Errorf("%s: description não menciona que o conteúdo é dado: %s", s.Name, s.Description)
		}
	}
}

// ── web_fetch: JSON ───────────────────────────────────────────────────────

func TestWebFetchJSONPassthrough(t *testing.T) {
	payload := `{"nome":"harness","versao":1,"itens":["a","b"],"aninhado":{"k":{"z":true}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(payload))
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	out, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	corpo := strings.TrimPrefix(out, avisoNaoConfiavel+"\n")
	// conteúdo integral, sem cortes.
	if !strings.Contains(corpo, "\n  \"nome\": \"harness\"") {
		t.Errorf("JSON não foi reindentado:\n%s", corpo)
	}
	var volta any
	if err := json.Unmarshal([]byte(corpo), &volta); err != nil {
		t.Errorf("saída não é JSON válido: %v\n%s", err, corpo)
	}
	if strings.Contains(out, "cortado em") {
		t.Errorf("JSON pequeno não devia ser cortado:\n%s", out)
	}
}

// ── web_fetch: content-type recusado ──────────────────────────────────────

func TestWebFetchRecusaOutroContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.7 lixo"))
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	out, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatalf("esperava recusa, voltou:\n%s", out)
	}
	linhas := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if len(linhas) != 1 {
		t.Errorf("erro não é de uma linha: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("erro não cita o content-type: %q", err.Error())
	}
}

func TestWebFetchRecusaEsquemaNaoHTTP(t *testing.T) {
	specs := especificas(t, nil)
	for _, alvo := range []string{"file:///c:/windows/win.ini", "/caminho/relativo", "exemplo.com"} {
		if _, err := rodar(t, specs["web_fetch"], map[string]any{"url": alvo}); err == nil {
			t.Errorf("esquema inválido aceito: %q", alvo)
		}
	}
	if _, err := rodar(t, specs["web_fetch"], map[string]any{"url": ""}); err == nil {
		t.Error("url vazia aceita")
	}
}

// ── web_fetch: status não-2XX ─────────────────────────────────────────────

func TestWebFetchNao2XXDevolveErroDeUmaLinha(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`<html><body><h1>404 Not Found</h1></body></html>`))
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	out, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatalf("esperava erro, voltou:\n%s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "404") {
		t.Errorf("mensagem sem o status: %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("erro não é de uma linha: %q", msg)
	}
}

// ── web_fetch: redirecionamentos ──────────────────────────────────────────

func TestWebFetchSegueRedirecionamentos(t *testing.T) {
	var saltos int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 3 saltos: dentro do limite de 5.
		saltos++
		switch r.URL.Path {
		case "/inicio":
			http.Redirect(w, r, "/meio", http.StatusFound)
		case "/meio":
			http.Redirect(w, r, "/fim", http.StatusFound)
		case "/fim":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("destino final alcançado"))
		default:
			t.Errorf("caminho inesperado: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	out, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL + "/inicio"})
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	if !strings.Contains(out, "destino final alcançado") {
		t.Errorf("redirecionamento não seguido:\n%s", out)
	}
	if saltos != 3 {
		t.Errorf("esperava 3 requisições, veio %d", saltos)
	}
}

func TestNovoHTTPLimitaRedirecionamentos(t *testing.T) {
	// Laço infinito de redirect: o CheckRedirect tem de estourar em 5.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = novoHTTP(5e9).Do(req)
	if err == nil {
		t.Fatal("esperava erro de excesso de redirecionamentos")
	}
	if !strings.Contains(err.Error(), "5") {
		t.Errorf("erro não menciona o limite: %v", err)
	}
}

// ── web_fetch: teto e marcador de corte ───────────────────────────────────

func TestWebFetchTruncaComMarcador(t *testing.T) {
	// 200 KB de texto: passa do teto de 2 MiB do corpo? Não — passa do teto
	// de caracteres, que é o que interessa aqui.
	grande := strings.Repeat("abcdefghij", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(grande))
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	out, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL, "max_chars": 500})
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	if len(out) > 500 {
		t.Errorf("saída passou do max_chars pedido: %d bytes", len(out))
	}
	if !strings.Contains(out, "(cortado em") || !strings.Contains(out, "bytes)") {
		t.Errorf("marcador de corte ausente:\n%s", out)
	}
	if !strings.Contains(out, "de 200000 bytes)") {
		t.Errorf("marcador não cita o total original:\n%s", out)
	}
}

// Teto padrão e hard cap: 20000 por omissão, 60000 no máximo.
func TestWebFetchTetoPadraoEHardCap(t *testing.T) {
	grande := strings.Repeat("x", 200000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(grande))
	}))
	defer srv.Close()

	specs := especificas(t, nil)
	semTeto, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(semTeto) > maxFetchPadrao {
		t.Errorf("teto padrão estourado: %d", len(semTeto))
	}
	if !strings.Contains(semTeto, "(cortado em") {
		t.Errorf("sem marcador de corte no teto padrão:\n%s", semTeto[:200])
	}

	alem, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL, "max_chars": 999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(alem) > tetoFetch {
		t.Errorf("hard cap de %d estourado: %d", tetoFetch, len(alem))
	}
}

// ── limparHTML: unidade ───────────────────────────────────────────────────

func TestLimparHTMLTitulo(t *testing.T) {
	titulo, _ := limparHTML([]byte(`<html><head><title>  Título &amp;   Subtítulo  </title></head><body>x</body></html>`))
	if titulo != "Título & Subtítulo" {
		t.Errorf("título = %q", titulo)
	}

	if t2, _ := limparHTML([]byte(`<body>sem titulo aqui</body>`)); t2 != "" {
		t.Errorf("título deveria ser vazio, veio %q", t2)
	}
}

func TestLimparHTMLRemoveScriptStyle(t *testing.T) {
	body := []byte(`<div>antes</div>
<script>var x = "<b>negrito falso</b>"; /* ruido */</script>
<style>.c{content:"nao aparece"} .d{font:bold}</style>
<div>depois</div>`)
	_, texto := limparHTML(body)
	for _, nao := range []string{"var x", "negrito falso", "content:", ".c{", ".d{", "nao aparece"} {
		if strings.Contains(texto, nao) {
			t.Errorf("script/style vazou (%q): %q", nao, texto)
		}
	}
	if !strings.Contains(texto, "antes") || !strings.Contains(texto, "depois") {
		t.Errorf("texto visível perdido: %q", texto)
	}
}

func TestLimparHTMLDecodificaEntidades(t *testing.T) {
	_, texto := limparHTML([]byte(`<p>a &amp; b &lt;c&gt; &quot;d&quot; &#39;e&#39; &nbsp; f &copy;</p>`))
	for _, quer := range []string{"a & b", "<c>", `"d"`, "'e'", "f ©"} {
		if !strings.Contains(texto, quer) {
			t.Errorf("entidade não decodificada: %q em %q", quer, texto)
		}
	}
	if strings.Contains(texto, "&amp;") || strings.Contains(texto, "&#39;") {
		t.Errorf("entidade crua sobrou: %q", texto)
	}
}

func TestLimparHTMLColapsaWhitespace(t *testing.T) {
	_, texto := limparHTML([]byte("<p>um\n\ttrês    quatro</p>\n\n\n<p>cinco</p>"))
	if texto != "um três quatro cinco" {
		t.Errorf("whitespace não colapsado: %q", texto)
	}
}

// Blocos vizinhos não podem colar palavras: </p><p> precisa virar espaço.
func TestLimparHTMLFronteiraDeTag(t *testing.T) {
	_, texto := limparHTML([]byte(`<p>foo</p><p>bar</p><ul><li>a</li><li>b</li></ul>`))
	if strings.Contains(texto, "foobar") {
		t.Errorf("palavras colaram: %q", texto)
	}
	for _, quer := range []string{"foo", "bar", "a", "b"} {
		if !strings.Contains(texto, quer) {
			t.Errorf("texto perdido (%q): %q", quer, texto)
		}
	}
}

func TestLimparHTMLRemoveComentarios(t *testing.T) {
	_, texto := limparHTML([]byte(`<p>vísivel</p><!-- invisivel_comentario --><p>também</p>`))
	if strings.Contains(texto, "invisivel_comentario") {
		t.Errorf("comentário vazou: %q", texto)
	}
	if !strings.Contains(texto, "vísivel") || !strings.Contains(texto, "também") {
		t.Errorf("texto ao redor do comentário perdido: %q", texto)
	}
}

// Tag auto-fechada e <script src=...></script> não devem engolir o resto.
func TestLimparHTMLTagsAutoFechadas(t *testing.T) {
	_, texto := limparHTML([]byte(`<div>a</div><img src="x.png"/><br/><div>b</div><script src="x.js"/>`))
	if !strings.Contains(texto, "a") || !strings.Contains(texto, "b") {
		t.Errorf("texto perdido com tags auto-fechadas: %q", texto)
	}
}

// ── web_search ────────────────────────────────────────────────────────────

// buscaFixture serve um envelope SearXNG com 3 resultados.
func buscaFixture(t *testing.T, alvo string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("sem format=json: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("q") != "harness barroso" {
			t.Errorf("query errada: %q", r.URL.Query().Get("q"))
		}
		if r.URL.Query().Get("language") != "pt-BR" {
			t.Errorf("language errado: %q", r.URL.Query().Get("language"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(alvo))
	}))
}

func TestWebSearchContraSearXNGFalso(t *testing.T) {
	srv := buscaFixture(t, `{"results":[
		{"title":"Primeiro &amp; Resultado","url":"https://a.example/1","content":"trecho um &lt;b&gt;ok&lt;/b&gt;"},
		{"title":"Segundo Resultado","url":"https://b.example/2","content":"trecho dois"},
		{"title":"Sem URL","content":"nao deve sair"}
	]}`)
	defer srv.Close()

	out, err := rodar(t, especificas(t, &Busca{Endpoint: srv.URL + "/search"})["web_search"],
		map[string]any{"query": "harness barroso"})
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}

	// formato numerado com título — URL e snippet indentado.
	if !strings.Contains(out, "1. Primeiro & Resultado — https://a.example/1") {
		t.Errorf("linha 1 fora do formato:\n%s", out)
	}
	if !strings.Contains(out, "2. Segundo Resultado — https://b.example/2") {
		t.Errorf("linha 2 fora do formato:\n%s", out)
	}
	if !strings.Contains(linhaDe(out, 1), "   trecho um <b>ok</b>") {
		t.Errorf("snippet ausente ou sem indentação:\n%s", out)
	}
	if !strings.Contains(out, "(2 de 3 resultados)") {
		t.Errorf("contador de descartados ausente:\n%s", out)
	}
	if strings.Contains(out, "Sem URL") {
		t.Errorf("resultado sem URL vazou:\n%s", out)
	}
	if !strings.HasPrefix(out, avisoNaoConfiavel+"\n") {
		t.Errorf("saída sem aviso de conteúdo externo:\n%s", out)
	}
	if len(out) > tetoBusca {
		t.Errorf("saída maior que o teto: %d", len(out))
	}
}

// linhaDe devolve a n-ésima linha (0-based) da saída, já sem o aviso.
func linhaDe(out string, n int) string {
	linhas := strings.Split(out, "\n")
	if n+1 >= len(linhas) {
		return ""
	}
	return linhas[n+1]
}

func TestWebSearchMaxResultados(t *testing.T) {
	var itens []string
	for i := 'a'; i <= 'j'; i++ {
		itens = append(itens, `{"title":"Item `+string(i)+`","url":"https://x.example/`+string(i)+`","content":"c"}`)
	}
	srv := buscaFixture(t, `{"results":[`+strings.Join(itens, ",")+`]}`)
	defer srv.Close()

	specs := especificas(t, &Busca{Endpoint: srv.URL})
	out, err := rodar(t, specs["web_search"], map[string]any{"query": "harness barroso", "max_resultados": 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3. Item c —") {
		t.Errorf("terceiro resultado ausente:\n%s", out)
	}
	if strings.Contains(out, "4. Item d") {
		t.Errorf("max_resultados ignorado:\n%s", out)
	}
	if len(linhasDe(out)) > 3 {
		t.Errorf("mais de 3 linhas de resultado: %d", len(linhasDe(out)))
	}
}

func linhasDe(out string) []string {
	var fora []string
	for _, l := range strings.Split(out, "\n")[1:] { // pula o aviso
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "(") {
			continue // snippet ou rodapé
		}
		fora = append(fora, l)
	}
	return fora
}

func TestWebSearchNao2XX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	specs := especificas(t, &Busca{Endpoint: srv.URL})
	out, err := rodar(t, specs["web_search"], map[string]any{"query": "x"})
	if err == nil {
		t.Fatalf("esperava erro, voltou:\n%s", out)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("mensagem sem o status: %q", err.Error())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("erro não é de uma linha: %q", err.Error())
	}
}

// fixtureDDG reproduz a marcação da página lite medida em 2026-10-18: link pelo
// redirecionador (uddg), um anúncio (y.js) que tem de sumir, e um resultado
// sem snippet para provar que o pareamento por ordem não desalinha.
const fixtureDDG = `<table>
<tr><td>1.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Fdevel%2Frelease&amp;rut=abc" class='result-link'>Release History - The <b>Go</b> Programming Language</a></td></tr>
<tr><td>&nbsp;</td><td class='result-snippet'>Go 1.3 is a major <b>release</b> &amp; more.</td></tr>
<tr><td>2.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fduckduckgo.com%2Fy.js%3Fad%3D1&amp;rut=x" class='result-link'>Anúncio</a></td></tr>
<tr><td>&nbsp;</td><td class='result-snippet'>compre já</td></tr>
<tr><td>3.&nbsp;</td><td><a rel="nofollow" href="https://example.org/direto" class='result-link'>Direto</a></td></tr>
</table>`

// Sem search_url a busca NÃO falha: cai no DuckDuckGo lite. Antes deste
// fallback a tool devolvia erro de configuração e a busca ficava desligada na
// prática para qualquer instalação nova.
func TestWebSearchSemEndpointUsaDuckDuckGo(t *testing.T) {
	var consulta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		consulta = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(fixtureDDG))
	}))
	defer srv.Close()
	antes := endpointDDG
	endpointDDG = srv.URL + "/lite/"
	t.Cleanup(func() { endpointDDG = antes })

	for nome, busca := range map[string]*Busca{
		"nil":    nil,
		"vazio":  {},
		"espaco": {Endpoint: "   "},
	} {
		t.Run(nome, func(t *testing.T) {
			out, err := rodar(t, especificas(t, busca)["web_search"], map[string]any{"query": "go release"})
			if err != nil {
				t.Fatalf("sem search_url deveria cair no DuckDuckGo, veio erro: %v", err)
			}
			if consulta != "go release" {
				t.Errorf("consulta enviada = %q", consulta)
			}
			if !strings.Contains(out, "https://go.dev/doc/devel/release") {
				t.Errorf("URL real (decodificada do uddg) ausente:\n%s", out)
			}
			if !strings.Contains(out, "Release History - The Go Programming Language") {
				t.Errorf("título sem tags ausente:\n%s", out)
			}
			if !strings.Contains(out, "Go 1.3 is a major release & more.") {
				t.Errorf("snippet sem tags/entidades ausente:\n%s", out)
			}
			if strings.Contains(out, "y.js") || strings.Contains(out, "Anúncio") {
				t.Errorf("anúncio deveria ser descartado:\n%s", out)
			}
			if !strings.Contains(out, "https://example.org/direto") {
				t.Errorf("link direto deveria passar:\n%s", out)
			}
		})
	}
}

// Erro HTTP do fallback continua sendo de uma linha e cita a origem.
func TestWebSearchDuckDuckGoErroHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	antes := endpointDDG
	endpointDDG = srv.URL
	t.Cleanup(func() { endpointDDG = antes })

	_, err := rodar(t, especificas(t, nil)["web_search"], map[string]any{"query": "x"})
	if err == nil {
		t.Fatal("esperava erro do HTTP 429")
	}
	if !strings.Contains(err.Error(), "DuckDuckGo") || !strings.Contains(err.Error(), "429") {
		t.Errorf("erro deveria citar DuckDuckGo e o status: %q", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("erro não é de uma linha: %q", err)
	}
}

// Com search_url configurado, o SearXNG continua sendo o caminho — o fallback
// não pode roubar a busca de quem configurou.
func TestWebSearchComEndpointNaoUsaDuckDuckGo(t *testing.T) {
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("DuckDuckGo não deveria ser chamado com search_url configurado")
	}))
	defer ddg.Close()
	antes := endpointDDG
	endpointDDG = ddg.URL
	t.Cleanup(func() { endpointDDG = antes })

	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://sx.example/a","content":"c"}]}`))
	}))
	defer sx.Close()

	out, err := rodar(t, especificas(t, &Busca{Endpoint: sx.URL})["web_search"], map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("SearXNG: %v", err)
	}
	if !strings.Contains(out, "https://sx.example/a") {
		t.Errorf("resultado do SearXNG ausente:\n%s", out)
	}
}

// A tool tem de existir no catálogo mesmo sem Busca — o modelo precisa ver
// o schema para então receber o erro de configuração.
func TestEspecificacoesIncluiBuscaSemConfigurar(t *testing.T) {
	specs := especificas(t, nil)
	s, ok := specs["web_search"]
	if !ok {
		t.Fatal("web_search ausente do catálogo com Busca nil")
	}
	if len(s.Schema) == 0 {
		t.Error("web_search sem schema")
	}
	if _, ok := s.Schema["properties"].(map[string]any)["query"]; !ok {
		t.Errorf("schema sem a propriedade query: %v", s.Schema)
	}
	if _, ok := specs["web_fetch"]; !ok {
		t.Error("web_fetch ausente do catálogo")
	}
}

// ── contrato dos specs ────────────────────────────────────────────────────

func TestEspecificacoesPrefixoETeto(t *testing.T) {
	for _, s := range Especificacoes(nil) {
		if s.Name == "" || s.Description == "" {
			t.Errorf("spec %q sem nome ou descrição", s.Name)
		}
		sch, ok := s.Schema["type"].(string)
		if !ok || sch != "object" {
			t.Errorf("%s: schema type = %v", s.Name, s.Schema["type"])
		}
		props, ok := s.Schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: schema sem properties", s.Name)
			continue
		}
		req, ok := s.Schema["required"].([]string)
		if !ok || len(req) == 0 {
			t.Errorf("%s: schema sem required", s.Name)
		}
		for _, r := range req {
			if _, existe := props[r]; !existe {
				t.Errorf("%s: required %q sem properties", s.Name, r)
			}
		}
		if s.Exec == nil {
			t.Errorf("%s: Exec nil", s.Name)
		}
	}
}

// Nenhuma tool devolve mais que o teto declarado.
func TestNenhumaSaidaPassaDoTeto(t *testing.T) {
	// fetch com HTML grande.
	grandeHTML := "<html><head><title>g</title></head><body>" +
		strings.Repeat("<p>parágrafo com bastante texto dentro para contar</p>", 3000) +
		"</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(grandeHTML))
	}))
	defer srv.Close()

	specs := especificas(t, &Busca{Endpoint: srv.URL})
	for _, caso := range []struct {
		nome string
		args map[string]any
		teto int
	}{
		{"web_fetch", map[string]any{"url": srv.URL}, tetoFetch},
		{"web_fetch", map[string]any{"url": srv.URL, "max_chars": 1000}, 1000},
		{"web_search", map[string]any{"query": "x"}, tetoBusca},
	} {
		out, err := rodar(t, specs[caso.nome], caso.args)
		if err != nil {
			continue // erro não é saída
		}
		if len(out) > caso.teto {
			t.Errorf("%s: %d bytes > teto %d", caso.nome, len(out), caso.teto)
		}
	}
}

// UA do harness em ambas as ferramentas.
func TestUserAgentDoHarness(t *testing.T) {
	var vistos []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vistos = append(vistos, r.Header.Get("User-Agent"))
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
		t.Fatalf("UserAgent do dealer mudou: %q", dealer.UserAgent)
	}
	specs := especificas(t, &Busca{Endpoint: srv.URL})
	if _, err := rodar(t, specs["web_fetch"], map[string]any{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := rodar(t, specs["web_search"], map[string]any{"query": "x"}); err != nil {
		t.Fatal(err)
	}
	if len(vistos) != 2 {
		t.Fatalf("esperava 2 requisições, veio %d", len(vistos))
	}
	for i, ua := range vistos {
		if ua != dealer.UserAgent {
			t.Errorf("requisição %d: User-Agent = %q, queria %q", i, ua, dealer.UserAgent)
		}
	}
}
