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

// AssetPlataformaNome is this platform's binary name, in the deployer's
// naming (.deploy/release.json): barroso-harness-<goos>-<goarch>, with .exe
// on Windows. It lives here — not in update.go — so that install_*.go, which
// needs the same name, does not import a cross-platform symbol.
func AssetPlataformaNome() string {
	name := assetPrefix + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Baixar downloads an asset to staging, checking size and SHA256 while it
// writes. The expected checksum comes from the published SHA256SUMS (channel
// checksum), not from the asset metadata: SHA256SUMS is published by the same
// pipeline as the binary, and checking one against the other catches a
// truncated or corrupted download.
//
// The staged file is created 0750 — executable, but only the owner creates
// it. The process owner is whoever asked for the update.
func Baixar(ctx context.Context, asset Asset, sum string, staging string) (string, error) {
	if asset.URL == "" {
		return "", errors.New("asset has no URL")
	}
	if asset.Name != AssetPlataformaNome() &&
		!strings.HasPrefix(asset.Name, AssetPlataformaNome()+".") {
		return "", fmt.Errorf("asset %q is not the binary for this platform (%s)",
			asset.Name, AssetPlataformaNome())
	}
	if sum != "" && !isSHA256(sum) {
		return "", fmt.Errorf("declared checksum for %s is invalid: %q", asset.Name, sum)
	}

	client := &http.Client{Timeout: httpTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build download request for %s: %w", asset.Name, err)
	}
	// octet-stream is what the GitHub API wants in order to return BYTES
	// instead of the asset's JSON metadata. A token, when set, goes in the
	// header, NEVER in the query: a URL with credentials ends up in proxy and
	// server logs.
	req.Header.Set("Accept", "application/octet-stream")
	setAuth(req)

	resp, err := client.Do(req)
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
	file, err := os.OpenFile(staging, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o750)
	if err != nil {
		return "", fmt.Errorf("create staging %s: %w", staging, err)
	}

	written := false
	defer func() {
		_ = file.Close()
		if !written {
			removeIfExists(staging)
		}
	}()

	hasher := sha256.New()
	limit := int64(assetCap)
	if asset.Bytes > 0 && asset.Bytes < limit {
		limit = asset.Bytes
	}
	// +1 byte: if the server sends MORE than the limit, Copy writes the extra
	// byte and the comparison below catches it. Without the +1, the excess
	// would be silently cut and the checksum would be checked against a
	// truncated file.
	bytes, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("write %s: %w", asset.Name, err)
	}
	if bytes > limit {
		return "", fmt.Errorf("%s exceeds the %d-byte limit", asset.Name, assetCap)
	}
	if asset.Bytes > 0 && bytes != asset.Bytes {
		return "", fmt.Errorf("size mismatch for %s: got %d, expected %d",
			asset.Name, bytes, asset.Bytes)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if sum != "" && !strings.EqualFold(got, strings.ToLower(sum)) {
		return "", fmt.Errorf("SHA256 mismatch for %s: got %s, expected %s",
			asset.Name, got, sum)
	}
	if err := file.Chmod(0o750); err != nil {
		return "", fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync staged binary: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close staged binary: %w", err)
	}
	written = true
	return staging, nil
}

// isSHA256 recognizes 64 hex digits — the form SHA256SUMS publishes.
func isSHA256(s string) bool {
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

// clock exists only so tests can inject a clock without relying on sleep.
var clock = struct {
	Now func() time.Time
}{Now: time.Now}
