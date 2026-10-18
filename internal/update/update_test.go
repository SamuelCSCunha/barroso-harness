package update

import (
	"context"
	"strings"
	"testing"
)

// ── compararVersoes ────────────────────────────────────────────────────────
//
// A tabela abaixo é a razão deste pacote ter sido reescrito: a comparação
// original ordenava o sufixo como TEXTO, então "bh.10" < "bh.9" ('1' < '9' no
// primeiro caractere). O sintoma seria o /update recusando a release mais nova
// — ou, com o sinal trocado, oferecendo downgrade.

func TestCompararVersoesSufixoNumerico(t *testing.T) {
	casos := []struct {
		a, b string
		quer int
	}{
		{"v0.1.0-bh.9", "v0.1.0-bh.10", -1},
		{"v0.1.0-bh.10", "v0.1.0-bh.9", 1},
		{"v0.1.0-bh.2", "v0.1.0-bh.10", -1},
		{"v0.1.0-bh.10", "v0.1.0-bh.10", 0},
		// Estável beats prévia: a versão sem sufixo é mais nova que ela mesma
		// com sufixo. É a convenção do semver, e o deployer publica as duas
		// formas quando fecha a versão.
		{"v0.1.0", "v0.1.0-bh.10", 1},
		{"v0.1.0-bh.10", "v0.1.0", -1},
		// Sufixo não numérico compara lexicograficamente, como no semver.
		{"v0.1.0-alpha", "v0.1.0-beta", -1},
		{"v0.1.0-rc.2", "v0.1.0-rc.10", -1},
	}
	for _, c := range casos {
		obt, err := compararVersoes(c.a, c.b)
		if err != nil {
			t.Fatalf("compararVersoes(%q,%q): %v", c.a, c.b, err)
		}
		if obt != c.quer {
			t.Errorf("compararVersoes(%q,%q) = %d, queria %d", c.a, c.b, obt, c.quer)
		}
	}
}

func TestCompararVersoesRejeitaFormaInvalida(t *testing.T) {
	for _, v := range []string{"", "dev", "1.2", "vX.Y.Z", "v0.1.0-", "qualquer_coisa"} {
		if _, err := partirVersao(v); err == nil {
			t.Errorf("partirVersao(%q) deveria recusar", v)
		}
	}
}

// ── token ──────────────────────────────────────────────────────────────────

// Sem token no ambiente, o erro precisa DIZER o que falta. Um 404 genérico de
// repo privado é indistinguível de repo inexistente — foi isso que fez a
// primeira versão deste pacote parecer "canal morto" quando o canal existia.
func TestTokenAusenteExplica(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("TOKEN_GITHUB", "")
	if tok := tokenDe(); tok != "" {
		t.Fatalf("tokenDe devolveu %q com ambiente vazio", tok)
	}
	_, err := BuscarUltima(context.Background())
	if err == nil {
		t.Fatal("BuscarUltima sem token deveria falhar")
	}
	msg := err.Error()
	for _, quer := range []string{"token", "GITHUB_TOKEN", "privadas"} {
		if !strings.Contains(msg, quer) {
			t.Errorf("mensagem de erro %q deveria mencionar %q", msg, quer)
		}
	}
}

func TestTokenPreferenciaAmbiente(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_primario")
	t.Setenv("TOKEN_GITHUB", "ghp_secundario")
	if got := tokenDe(); got != "ghp_primario" {
		t.Errorf("tokenDe = %q, queria ghp_primario (GITHUB_TOKEN ganha)", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := tokenDe(); got != "ghp_secundario" {
		t.Errorf("tokenDe sem GITHUB_TOKEN = %q, queria ghp_secundario", got)
	}
}

// ── nome do asset ──────────────────────────────────────────────────────────

// O nome do asset é a ÚNICA forma de saber a plataforma: a API do GitHub não
// declara os/arch por asset. Se esta função derivar errado, o update baixa o
// binário da plataforma errada e o exec-check falha com mensagem confusa.
func TestAssetPlataformaNomeTemFormaDoDeployer(t *testing.T) {
	nome := AssetPlataformaNome()
	if !strings.HasPrefix(nome, "barroso-harness-") {
		t.Errorf("asset %q deveria começar com barroso-harness-", nome)
	}
	if !strings.Contains(strings.TrimPrefix(nome, "barroso-harness-"), "-") {
		t.Errorf("asset %q deveria ter goos-goarch", nome)
	}
}

func TestAssetDaPlataformaExigeNomeExato(t *testing.T) {
	l := Lancamento{Version: "v0.1.0-bh.10"}
	// Asset de outra plataforma NÃO pode casar: prefixo comum
	// "barroso-harness-linux-amd64" vs "barroso-harness-linux-amd64-v2" à parte,
	// o que importa é rejeitar "barroso-harness-linux-arm64" quando a plataforma
	// é amd64.
	l.Assets = []Asset{
		{Name: "barroso-harness-linux-arm64"},
		{Name: "barroso-harness-darwin-arm64"},
	}
	if _, err := l.AssetDaPlataforma(); err == nil {
		t.Fatal("release sem binário da plataforma deveria dar erro")
	}
}

// ── SHA256SUMS ─────────────────────────────────────────────────────────────

// Um asset sem soma no SHA256SUMS não pode ser instalado. Este teste crava a
// decisão do Rodar: soma ausente aborta, em vez de seguir "confiando".
func TestEBaixarRejeitaSomaInvalida(t *testing.T) {
	for _, s := range []string{"abc", "zz" + strings.Repeat("0", 62), strings.Repeat("a", 63)} {
		if éSHA256(s) {
			t.Errorf("éSHA256(%q) deveria ser falso", s)
		}
	}
	if !éSHA256(strings.Repeat("a", 64)) {
		t.Error("éSHA256 de 64 hex deveria ser verdadeiro")
	}
}

// ── Rodar: fluxo sem canal ─────────────────────────────────────────────────

// Sem token, Rodar falha ANTES de tocar em qualquer arquivo. Um update que
// erra nunca deixa a máquina sem harness — é a propriedade que os outros
// testes assumem.
func TestRodarSemTokenNaoInstalaNada(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("TOKEN_GITHUB", "")
	var saida strings.Builder
	err := Rodar(context.Background(), &saida, "v0.1.0-bh.9", nil)
	if err == nil {
		t.Fatal("Rodar sem token deveria falhar")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("erro %q deveria mencionar token", err)
	}
}

// Versão local "dev" é build sem release: qualquer versão publicada é novidade
// e a comparação não pode travar nem recusar.
func TestNormalizarVersaoDev(t *testing.T) {
	for entrada, quer := range map[string]string{
		"":              "dev",
		"  ":            "dev",
		"dev":           "dev",
		" v0.1.0-bh.9 ": "v0.1.0-bh.9",
	} {
		if got := normalizarVersao(entrada); got != quer {
			t.Errorf("normalizarVersao(%q) = %q, queria %q", entrada, got, quer)
		}
	}
}

// ── escolherMaisNova ───────────────────────────────────────────────────────
//
// Estes testes existem por causa de um bug REAL, pego por uma sonda contra o
// canal de produção antes de qualquer pessoa ver o sintoma: a API devolveu
// v0.1.0-bh.9 ANTES de v0.1.0-bh.10 na lista de releases. A primeira versão
// deste pacote pegava a primeira release estável da lista, então um usuário
// com bh.10 instalada receberia oferta de DOWNGRADE para bh.9.

// O CASO QUE MOTIVOU A CORREÇÃO: a lista traz a versão mais ANTIGA antes da
// mais nova. A escolha tem de olhar a versão, não a posição — senão quem está
// na bh.10 recebe oferta de downgrade para bh.9.
func TestEscolherMaisNovaIgnoraOrdemDaLista(t *testing.T) {
	lista := []releaseAPI{
		releaseDe("v0.1.0-bh.9"),
		releaseDe("v0.1.0-bh.10"),
		releaseDe("v0.1.0-bh.8"),
	}
	lanc, err := escolherMaisNova(lista)
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	if lanc.Version != "v0.1.0-bh.10" {
		t.Fatalf("escolheu %q, queria v0.1.0-bh.10 (a mais nova, não a primeira)", lanc.Version)
	}
}

// Mais de dez versões: "bh.10" > "bh.9" precisa da comparação NUMÉRICA do
// sufixo. Comparação textual daria bh.9 como maior e o teste falharia.
func TestEscolherMaisNovaComSufixoDeDoisDigitos(t *testing.T) {
	lista := []releaseAPI{releaseDe("v0.1.0-bh.9"), releaseDe("v0.1.0-bh.11"), releaseDe("v0.1.0-bh.10")}
	lanc, err := escolherMaisNova(lista)
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	if lanc.Version != "v0.1.0-bh.11" {
		t.Fatalf("escolheu %q, queria v0.1.0-bh.11", lanc.Version)
	}
}

// Rascunho e pré-release são ignorados mesmo sendo a maior versão da lista:
// /update entrega estável.
func TestEscolherMaisNovaIgnoraRascunhoEPreRelease(t *testing.T) {
	maior := releaseDe("v0.2.0-rc.1")
	maior.Prerelease = true
	rascunho := releaseDe("v0.3.0")
	rascunho.Draft = true
	lista := []releaseAPI{maior, rascunho, releaseDe("v0.1.0-bh.10")}
	lanc, err := escolherMaisNova(lista)
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	if lanc.Version != "v0.1.0-bh.10" {
		t.Fatalf("escolheu %q; rascunho/pré-release não deveria vencer", lanc.Version)
	}
}

// Tag fora da forma (solto no canal) não pode abortar nem vencer: é ignorada e
// a melhor válida segue vencedora.
func TestEscolherMaisNovaIgnoraTagInvalida(t *testing.T) {
	solto := releaseDe("nightly-build")
	lista := []releaseAPI{solto, releaseDe("v0.1.0-bh.10")}
	lanc, err := escolherMaisNova(lista)
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	if lanc.Version != "v0.1.0-bh.10" {
		t.Fatalf("escolheu %q; tag inválida não deveria vencer", lanc.Version)
	}
}

// Só tag inválida na lista = nada instalável, e o erro precisa dizer isso em
// vez de devolver lançamento vazio.
func TestEscolherMaisNovaSoTagInvalidaErra(t *testing.T) {
	_, err := escolherMaisNova([]releaseAPI{releaseDe("lixo")})
	if err == nil {
		t.Fatal("só tag inválida deveria dar erro")
	}
}

// Release sem binário DESTA plataforma não serve: é ignorada, mesmo sendo a
// mais nova. Sem esse filtro, o update baixaria um asset que não existe aqui.
func TestEscolherMaisNovaIgnoraSemBinarioDaPlataforma(t *testing.T) {
	outra := releaseAPI{
		TagName: "v0.1.0-bh.10",
		Assets: []assetAPI{
			{Name: "barroso-harness-planeta-9-ppc64", Size: 10, BrowserDownloadURL: "https://example.invalid/x"},
		},
	}
	lista := []releaseAPI{outra, releaseDe("v0.1.0-bh.9")}
	lanc, err := escolherMaisNova(lista)
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	if lanc.Version != "v0.1.0-bh.9" {
		t.Fatalf("escolheu %q; release sem binário da plataforma não deveria vencer", lanc.Version)
	}
}

// Lista vazia (canal sem release publicada) devolve erro explícito.
func TestEscolherMaisNovaListaVazia(t *testing.T) {
	if _, err := escolherMaisNova(nil); err == nil {
		t.Fatal("lista vazia deveria dar erro")
	}
}

// A URL escolhida tem de ser a de ASSET DA API, não a de browser.
//
// Não é gosto: num repositório privado o browser_download_url redireciona para
// objects.githubusercontent.com e o header Authorization não sobrevive ao
// salto — o download dá 404 "Not Found" com token válido. A URL da API aceita
// o Bearer e devolve bytes com Accept: application/octet-stream. Se este teste
// falhar, todo /update quebra em produção sem que nada mais denuncie.
func TestEscolherMaisNovaPrefereURLDeAssetDaAPI(t *testing.T) {
	r := releaseDe("v0.1.0-bh.11")
	r.Assets[0].URL = "https://api.github.com/repos/ruanbarroso/barroso-harness/releases/assets/653642039"
	r.Assets[0].BrowserDownloadURL = "https://github.com/ruanbarroso/barroso-harness/releases/download/v0.1.0-bh.11/barroso-harness-windows-amd64.exe"

	lanc, err := escolherMaisNova([]releaseAPI{r})
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	asset, err := lanc.AssetDaPlataforma()
	if err != nil {
		t.Fatalf("AssetDaPlataforma: %v", err)
	}
	if !strings.HasPrefix(asset.URL, "https://api.github.com/") {
		t.Fatalf("URL escolhida %q deveria ser a de asset da API (browser_download_url devolve 404 em repo privado; a URL da API devolve os bytes)", asset.URL)
	}
}

// A única release sem URL de API cai no browser: degradação, não falha.
func TestEscolherMaisNovaCaiNoBrowserSemURLDaAPI(t *testing.T) {
	r := releaseDe("v0.1.0-bh.11")
	r.Assets[0].URL = ""
	r.Assets[0].BrowserDownloadURL = "https://github.com/ruanbarroso/barroso-harness/releases/download/v0.1.0-bh.11/binario"

	lanc, err := escolherMaisNova([]releaseAPI{r})
	if err != nil {
		t.Fatalf("escolherMaisNova: %v", err)
	}
	asset, _ := lanc.AssetDaPlataforma()
	if asset.URL != "https://github.com/ruanbarroso/barroso-harness/releases/download/v0.1.0-bh.11/binario" {
		t.Fatalf("sem URL da API deveria usar a de browser, veio %q", asset.URL)
	}
}
