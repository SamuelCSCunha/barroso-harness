// Package update consulta o canal de distribuição e troca o binário instalado
// pela release mais nova.
//
// CANAL. A distribuição é Release de repositório PRIVADO no GitHub, publicada
// pelo deployer (ver .deploy/release.json no repo). Não existe — e não existia
// quando este pacote foi escrito — um dl.barroso.tec.br: o host não resolve DNS,
// e a primeira versão deste pacote apontava para lá. Consultar host morto daria
// "canal indisponível" em todos os /update, então a fonte é a API do GitHub.
//
// AUTENTICAÇÃO. Repositório privado exige token, e o token NUNCA mora na
// config.json nem no pacote: ele vem do ambiente (GITHUB_TOKEN ou TOKEN_GITHUB,
// neste ordem). Sem token, /update explica o que falta em vez de falhar com 404
// genérico — 404 de repo privado sem token é indistinguível de repo inexistente.
//
// VERIFICAÇÃO. O SHA256 de cada binário vem do SHA256SUMS publicado junto com
// a release, e o asset baixado é conferido antes de tocar no executável em uso.
// Só depois da verificação o binário staged é executado (:cmd -version) para
// provar que roda na plataforma atual. Nada é instalado sem essas três provas.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RepoSlug é o repositório privado das releases. Fixo de propósito: /update do
// barroso-harness só pode se atualizar a partir do canal dele.
const RepoSlug = "ruanbarroso/barroso-harness"

const (
	originGitHub = "https://api.github.com"
	// tetoManifest é o limite da resposta JSON da API. Uma release tem poucos
	// KB de metadados; 1 MiB é folga generosa que ainda evita leitura sem fim
	// se o endpoint um dia devolver algo inesperado.
	tetoManifest = 1 << 20
	// tetoAsset é o teto de um binário baixado. As releases atuais têm ~11 MB;
	// 64 MiB acomoda crescimento sem abrir brecha para descarregar o servidor.
	tetoAsset = 64 << 20
	// timeoutHTTP cobre o handshake e o download. Download de 11 MB em conexão
	// ruim precisa de folga; 5 min é o teto que não parece "travado".
	timeoutHTTP = 5 * time.Minute
)

// prefixoAsset é o começo do nome de todo binário de release. Usado para
// separar binário de metadado (SHA256SUMS) e para derivar a plataforma.
const prefixoAsset = "barroso-harness-"

// sha256SumsNome é o asset da lista de somas, publicada pelo deployer.
const sha256SumsNome = "SHA256SUMS"

// Lancamento é a release resolvida no canal: a tag e os assets que ela traz.
type Lancamento struct {
	Version string
	TagName string
	Assets  []Asset
}

// Asset é um arquivo da release. URL é a URL da API do GitHub, que exige o
// header Accept: application/octet-stream para devolver bytes em vez de JSON.
type Asset struct {
	Name  string
	URL   string
	Bytes int64
}

// Som traz a soma declarada de um asset, lida do SHA256SUMS. Vazia significa
// "o canal não publicou soma para este arquivo".
type Som struct {
	SHA256 string
}

// tokenDe retorna o token do ambiente, na ordem documentada no cabeçalho do
// pacote. String vazia quando não há nenhum.
func tokenDe() string {
	for _, nome := range []string{"GITHUB_TOKEN", "TOKEN_GITHUB"} {
		if v := strings.TrimSpace(os.Getenv(nome)); v != "" {
			return v
		}
	}
	return ""
}

// BuscarUltima busca a release publicada mais recente do canal.
//
// Pré-release e rascunho ficam de fora: /update entrega versão estável, e um
// rascunho nem sempre está completo. Se um dia o canal publicar só
// pré-release, a mensagem de erro explica — em vez de instalar algo instável.
func BuscarUltima(ctx context.Context) (Lancamento, error) {
	token := tokenDe()
	if token == "" {
		return Lancamento{}, errors.New(
			"canal de atualização precisa de token: defina GITHUB_TOKEN (ou TOKEN_GITHUB) " +
				"no ambiente — as releases do barroso-harness são privadas")
	}
	cliente := &http.Client{Timeout: timeoutHTTP}
	u := originGitHub + "/repos/" + RepoSlug + "/releases?per_page=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Lancamento{}, fmt.Errorf("montar consulta do canal: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := cliente.Do(req)
	if err != nil {
		return Lancamento{}, fmt.Errorf("consultar canal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Lancamento{}, fmt.Errorf("canal recusou o token (HTTP %s): verifique a permissão do token", resp.Status)
	}
	if resp.StatusCode == http.StatusNotFound {
		return Lancamento{}, fmt.Errorf("canal não encontrado (HTTP %s): token sem acesso a %s", resp.Status, RepoSlug)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Lancamento{}, fmt.Errorf("canal respondeu HTTP %s", resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, tetoManifest+1))
	if err != nil {
		return Lancamento{}, fmt.Errorf("ler resposta do canal: %w", err)
	}
	if len(raw) > tetoManifest {
		return Lancamento{}, fmt.Errorf("resposta do canal passa de %d bytes", tetoManifest)
	}

	var releases []releaseAPI
	if err := json.Unmarshal(raw, &releases); err != nil {
		return Lancamento{}, fmt.Errorf("resposta do canal inválida: %w", err)
	}

	return escolherMaisNova(releases)
}

// releaseAPI é a parte da árvore JSON de uma release que o pacote usa, e
// assetAPI um arquivo dentro dela. Ficam nomeados — e não structs anônimas
// dentro de BuscarUltima — para que o teste alimente escolherMaisNova sem
// subir servidor HTTP.
type releaseAPI struct {
	TagName    string     `json:"tag_name"`
	Draft      bool       `json:"draft"`
	Prerelease bool       `json:"prerelease"`
	Assets     []assetAPI `json:"assets"`
}

type assetAPI struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// releaseDe monta uma release mínima e válida para esta plataforma, para os
// testes não repetirem o asset obrigatório em cada caso.
func releaseDe(tag string) releaseAPI {
	return releaseAPI{
		TagName: tag,
		Assets: []assetAPI{
			{Name: AssetPlataformaNome(), Size: 1024, BrowserDownloadURL: "https://example.invalid/bin"},
		},
	}
}

// escolherMaisNova devolve a release de VERSÃO máxima entre as estáveis que
// têm binário desta plataforma. NÃO a primeira da lista: a ordem da API não é
// garantia de nada — ela vem em ordem decrescente de CRIAÇÃO, e uma
// republicação ou retag pode colocar uma versão mais antiga à frente da mais
// nova (observado no canal real: bh.9 listada antes de bh.10). Confiar na
// posição faria o /update oferecer DOWNGRADE a quem já está na mais nova.
//
// Rascunho e pré-release ficam de fora: /update entrega versão estável. Tag
// fora da forma vX.Y.Z[-sufixo] é ignorada, não aborta — um tag solto no canal
// não deve impedir a atualização de todos.
func escolherMaisNova(releases []releaseAPI) (Lancamento, error) {
	var melhor Lancamento
	temMelhor := false
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		// A tag é validada AQUI, antes de virar candidata — não só na
		// comparação. Sem isso, uma tag fora da forma que seja a PRIMEIRA
		// elegível vence por padrão (nenhuma comparação a derruba) e o canal
		// devolveria "nightly-build" como última versão.
		if _, err := partirVersao(r.TagName); err != nil {
			continue
		}
		if len(r.Assets) == 0 {
			continue
		}
		lanc := Lancamento{Version: r.TagName, TagName: r.TagName}
		for _, a := range r.Assets {
			// A URL de ASSET DA API vem primeiro, e a ordem não é detalhe: num
			// repositório PRIVADO o browser_download_url devolve 404 "Not Found"
			// no download de asset, enquanto a URL da API devolve os bytes com
			// Accept: application/octet-stream. Medido em A/B no canal real em
			// 2026-10-18, mesmo Accept e mesmo Bearer nas duas URLs, variando
			// só o tipo: browser 404, API 200. User-Agent não muda o resultado.
			//
			// NÃO registre aqui uma explicação de redirect: ela foi testada e é
			// FALSA. A URL da API também redireciona (302 para
			// release-assets.githubusercontent.com) e o cliente padrão do Go a
			// segue e recebe 200 — se o header morresse no salto, a URL da API
			// falharia igual. O 404 do browser fica sem mecanismo explicado, e
			// a escolha não depende dele.
			u := a.URL
			if u == "" {
				u = a.BrowserDownloadURL
			}
			lanc.Assets = append(lanc.Assets, Asset{Name: a.Name, URL: u, Bytes: a.Size})
		}
		if _, err := lanc.AssetDaPlataforma(); err != nil {
			continue
		}
		if !temMelhor {
			melhor, temMelhor = lanc, true
			continue
		}
		// A tag já foi validada acima, então compararVersoes não pode falhar
		// aqui; se falhasse, seria bug deste pacote, e ignorar silenciosamente
		// esconderia o problema. Continua valendo elegível anterior.
		cmp, err := compararVersoes(lanc.Version, melhor.Version)
		if err != nil {
			continue
		}
		if cmp > 0 {
			melhor = lanc
		}
	}
	if !temMelhor {
		return Lancamento{}, errors.New("canal não tem release publicada com binário para esta plataforma")
	}
	return melhor, nil
}

// MaisNovaQue informa se a release do canal é mais nova que a versão instalada.
// Exposta porque a CLI quer a resposta antes de propor qualquer download: se
// não é mais nova, /update não deve nem perguntar.
func (l Lancamento) MaisNovaQue(instalada string) (bool, error) {
	cmp, err := compararVersoes(l.Version, instalada)
	if err != nil {
		return false, err
	}
	return cmp > 0, nil
}

// AssetDaPlataforma escolhe o asset desta plataforma pelo NOME — a API do
// GitHub não declara os/arch por asset, então a plataforma está no nome:
// barroso-harness-<goos>-<goarch>[.exe].
func (l Lancamento) AssetDaPlataforma() (Asset, error) {
	alvo := AssetPlataformaNome()
	// Coincidência exata primeiro; se algum dia o nome ganhar sufixo extra,
	// o prefixo ainda casa sem aceitar binário de outra plataforma.
	for _, a := range l.Assets {
		if a.Name == alvo {
			return a, nil
		}
	}
	for _, a := range l.Assets {
		if strings.HasPrefix(a.Name, alvo+".") {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s não tem binário para esta plataforma (%s)", l.Version, alvo)
}

// AssetSomas devolve o asset SHA256SUMS, se a release o publicou.
func (l Lancamento) AssetSomas() (Asset, bool) {
	for _, a := range l.Assets {
		if a.Name == sha256SumsNome {
			return a, true
		}
	}
	return Asset{}, false
}

// ListarSomas baixa e interpreta o SHA256SUMS. Formato: "<hex>  <nome>" por
// linha, o mesmo que sha256sum emite. Linha mal formada é ignorada — um
// metadata futuro a mais não deve derrubar a atualização.
func ListarSomas(ctx context.Context, asset Asset) (map[string]string, error) {
	if asset.URL == "" {
		return nil, errors.New("asset de somas sem URL")
	}
	cliente := &http.Client{Timeout: timeoutHTTP}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tokenDe())
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := cliente.Do(req)
	if err != nil {
		return nil, fmt.Errorf("baixar SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("SHA256SUMS respondeu HTTP %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	somas := map[string]string{}
	for _, linha := range strings.Split(string(raw), "\n") {
		campos := strings.Fields(linha)
		if len(campos) < 2 {
			continue
		}
		somas[strings.TrimPrefix(campos[len(campos)-1], "*")] = strings.ToLower(campos[0])
	}
	return somas, nil
}

// comparável é a versão quebrada em partes para comparação: três números e o
// sufixo após o hífen, quebrando em identificadores (o deployer publica
// v0.1.0-bh.10; "bh" e "10" são identificadores separados).
type comparavel struct {
	nums      [3]int
	sufixo    []string
	temSufixo bool
}

var versaoForma = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// partirVersao valida e quebra uma tag. Tag fora da forma é recusada: comparar
// o incomparável daria update para baixo ou recusa de update legítimo.
func partirVersao(v string) (comparavel, error) {
	v = strings.TrimSpace(v)
	if !versaoForma.MatchString(v) {
		return comparavel{}, fmt.Errorf("versão %q fora da forma vX.Y.Z[-sufixo]", v)
	}
	var c comparavel
	corpo := strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(corpo, '-'); i >= 0 {
		c.temSufixo = true
		c.sufixo = strings.Split(corpo[i+1:], ".")
		corpo = corpo[:i]
	}
	for i, p := range strings.SplitN(corpo, ".", 3) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return comparavel{}, fmt.Errorf("versão %q tem número inválido %q", v, p)
		}
		c.nums[i] = n
	}
	return c, nil
}

// compararVersoes devolve -1, 0 ou 1.
//
// A correção que esta função existe para: comparar o sufixo como TEXTO diz que
// "bh.10" < "bh.9", porque '1' < '9' no primeiro caractere. O resultado é o
// programa recusando a atualização mais nova (ou pior, oferecendo downgrade).
// Sufixo é comparado identificador a identificador, numericamente quando os
// dois lados são números.
func compararVersoes(a, b string) (int, error) {
	ca, err := partirVersao(a)
	if err != nil {
		return 0, err
	}
	cb, err := partirVersao(b)
	if err != nil {
		return 0, err
	}
	for i := range ca.nums {
		if ca.nums[i] != cb.nums[i] {
			if ca.nums[i] < cb.nums[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	// Sem sufixo beats com sufixo: v0.1.0 > v0.1.0-bh.10 (a estável é mais nova
	// que a sua própria prévia). É a convenção do semver.
	if ca.temSufixo != cb.temSufixo {
		if ca.temSufixo {
			return -1, nil
		}
		return 1, nil
	}
	for i := 0; i < len(ca.sufixo) || i < len(cb.sufixo); i++ {
		var sa, sb string
		if i < len(ca.sufixo) {
			sa = ca.sufixo[i]
		}
		if i < len(cb.sufixo) {
			sb = cb.sufixo[i]
		}
		if sa == sb {
			continue
		}
		na, errA := strconv.Atoi(sa)
		nb, errB := strconv.Atoi(sb)
		if errA == nil && errB == nil {
			if na < nb {
				return -1, nil
			}
			return 1, nil
		}
		if sa < sb {
			return -1, nil
		}
		return 1, nil
	}
	return 0, nil
}

// ── helpers compartilhados pelos dois install_*.go ─────────────────────────

func isPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission)
}

func removeIfExists(name string) {
	if name != "" {
		_ = os.Remove(name)
	}
}

// validateVersionOutput confere que o binário staged respondeu a versão
// esperada. Sem essa prova, um download que devolveu HTML (página de erro)
// seria instalado como se fosse executável.
func validateVersionOutput(output, expected string) error {
	if !strings.Contains(output, expected) {
		return fmt.Errorf("binário staged respondeu %q, esperado %s",
			strings.TrimSpace(output), expected)
	}
	return nil
}
