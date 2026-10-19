package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AssetPlataformaNome é o nome do binário desta plataforma, na nomenclatura do
// deployer (.deploy/release.json): barroso-harness-<goos>-<goarch>, com .exe
// no Windows. Fica aqui — e não em update.go — para que install_*.go, que
// precisa do mesmo nome, não importe símbolo de plataforma cruzada.
func AssetPlataformaNome() string {
	nome := prefixoAsset + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		nome += ".exe"
	}
	return nome
}

// Baixar baixa um asset para staging, conferindo tamanho e SHA256 enquanto
// escreve. A soma esperada vem do SHA256SUMS publicado (soma canal), não do
// metadata do asset: o SHA256SUMS é publicado pelo mesmo pipeline do binário,
// e conferir os dois entre si pega download truncado ou corrompido.
//
// O arquivo staged sai com 0750 — é executável, mas só o dono o cria. O dono
// do processo é quem mandou atualizar.
func Baixar(ctx context.Context, asset Asset, soma string, staging string) (string, error) {
	if asset.URL == "" {
		return "", errors.New("asset has no URL")
	}
	if asset.Name != AssetPlataformaNome() &&
		!strings.HasPrefix(asset.Name, AssetPlataformaNome()+".") {
		return "", fmt.Errorf("asset %q is not the binary for this platform (%s)",
			asset.Name, AssetPlataformaNome())
	}
	if soma != "" && !éSHA256(soma) {
		return "", fmt.Errorf("declared checksum for %s is invalid: %q", asset.Name, soma)
	}

	cliente := &http.Client{Timeout: timeoutHTTP}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build download request for %s: %w", asset.Name, err)
	}
	// octet-stream é o que a API do GitHub quer para devolver BYTES em vez do
	// metadata JSON do asset. O token vai no header, NUNCA na query: URL com
	// credencial acaba em log de proxy e de servidor.
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+tokenDe())

	resp, err := cliente.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download %s: HTTP %s", asset.Name, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "text/html") {
		return "", fmt.Errorf("download %s: channel returned HTML (%s) instead of the binary", asset.Name, ct)
	}

	if err := os.MkdirAll(filepath.Dir(staging), 0o750); err != nil {
		return "", fmt.Errorf("create staging: %w", err)
	}
	arquivo, err := os.OpenFile(staging, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o750)
	if err != nil {
		return "", fmt.Errorf("create staging %s: %w", staging, err)
	}

	gravado := false
	defer func() {
		_ = arquivo.Close()
		if !gravado {
			removeIfExists(staging)
		}
	}()

	hasher := sha256.New()
	limite := int64(tetoAsset)
	if asset.Bytes > 0 && asset.Bytes < limite {
		limite = asset.Bytes
	}
	// +1 byte: se o servidor mandar MAIS que o limite, o Copy grava o byte
	// extra e a comparação abaixo detecta. Sem o +1, o excesso seria cortado
	// silenciosamente e a soma conferiria contra um arquivo truncado.
	bytes, err := io.Copy(io.MultiWriter(arquivo, hasher), io.LimitReader(resp.Body, limite+1))
	if err != nil {
		return "", fmt.Errorf("write %s: %w", asset.Name, err)
	}
	if bytes > limite {
		return "", fmt.Errorf("%s exceeds the %d-byte limit", asset.Name, tetoAsset)
	}
	if asset.Bytes > 0 && bytes != asset.Bytes {
		return "", fmt.Errorf("size mismatch for %s: got %d, expected %d",
			asset.Name, bytes, asset.Bytes)
	}
	obtida := hex.EncodeToString(hasher.Sum(nil))
	if soma != "" && !strings.EqualFold(obtida, strings.ToLower(soma)) {
		return "", fmt.Errorf("SHA256 mismatch for %s: got %s, expected %s",
			asset.Name, obtida, soma)
	}
	if err := arquivo.Chmod(0o750); err != nil {
		return "", fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := arquivo.Sync(); err != nil {
		return "", fmt.Errorf("sync staged binary: %w", err)
	}
	if err := arquivo.Close(); err != nil {
		return "", fmt.Errorf("close staged binary: %w", err)
	}
	gravado = true
	return staging, nil
}

// éSHA256 reconhece 64 hexadecimais — a forma que o SHA256SUMS publica.
func éSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// camadaTempo só existe para os testes injetarem relógio sem depender de sleep.
var camadaTempo = struct {
	Agora func() time.Time
}{Agora: time.Now}
