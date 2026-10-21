// Prefetch: the interactive bh checks the channel in the background,
// downloads a newer release into the cache and installs NOTHING. /update then
// reuses the cached binary when it is the same release the channel publishes.
//
// COST. Each tick starts at https://github.com/<repo>/releases/latest, which
// answers 302 with the newest tag in Location. Requests to github.com do not
// count against the API's anonymous 60/h limit — measured on 2026-10-21:
// X-RateLimit-Remaining stayed put across the 302s, while API calls (even
// conditional ones answered 304 Not Modified) spent one each. The API is only
// called when the tag is newer than the installed version AND not cached yet,
// so an up-to-date bh polling every 2 min spends nothing from the budget the
// manual /update needs.
//
// TRUST. The cache is an optimization, never a source of truth: /update still
// resolves the release through the API and reuses the cached file only when
// its SHA256 matches the release's SHA256SUMS. The exec check (-version) runs
// on the cached file like on a fresh download.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IntervaloPrefetch is the pause between background checks.
const IntervaloPrefetch = 2 * time.Minute

// lockStale is when a download lock is considered abandoned. A process that
// died mid-download leaves its lock behind; past the download timeout nobody
// can still be holding it legitimately.
const lockStale = httpTimeout + time.Minute

// latestURL answers 302 with the newest release tag, outside the API limit.
var latestURL = "https://github.com/" + RepoSlug + "/releases/latest"

// prefetchDir is where downloaded-but-not-installed binaries wait. A var so
// tests point it at a TempDir.
var prefetchDir = func() string { return filepath.Join(stagingRoot(), "prefetch") }

// tagFromLocation extracts the tag from a redirect such as
// https://github.com/owner/repo/releases/tag/v0.1.0-bh.74 . Query and
// fragment are dropped so a tracking parameter does not become part of the tag.
func tagFromLocation(location string) string {
	if i := strings.IndexAny(location, "?#"); i >= 0 {
		location = location[:i]
	}
	i := strings.LastIndex(location, "/tag/")
	if i < 0 {
		return ""
	}
	return strings.Trim(location[i+len("/tag/"):], "/")
}

// TagUltima returns the newest release tag through the github.com redirect.
// The redirect is NOT followed: Location is the answer, and following it
// would only fetch an HTML page.
func TagUltima(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "barroso-harness")
	client := &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", errors.New("latest-release redirect responded " + resp.Status)
	}
	tag := tagFromLocation(resp.Header.Get("Location"))
	if _, err := parseVersion(tag); err != nil {
		return "", errors.New("latest-release redirect carries no valid tag")
	}
	return tag, nil
}

// prefetchPath is the final name of a cached binary for a version.
func prefetchPath(version string) string {
	return filepath.Join(prefetchDir(), AssetPlataformaNome()+"."+version)
}

// cachedFor returns the cached binary of the plan's release, if one exists
// AND its SHA256 matches the published one. A mismatch (truncated file, tag
// republished with other bytes) deletes the file so it is downloaded again.
func cachedFor(p Plano) (string, bool) {
	path := prefetchPath(p.Release.Version)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	if p.Sum == "" || !fileHasSHA256(path, p.Sum) {
		removeIfExists(path)
		return "", false
	}
	return path, true
}

func fileHasSHA256(path, sum string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), strings.TrimSpace(sum))
}

// takeLock creates the version's lock exclusively, so two bh windows do not
// download the same 11 MB at once. A lock older than lockStale is abandoned
// (its owner died) and is taken over.
func takeLock(dir, version string) (release func(), ok bool) {
	name := filepath.Join(dir, ".lock."+version)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(name) }, true
		}
		info, statErr := os.Stat(name)
		if statErr != nil || clock.Now().Sub(info.ModTime()) < lockStale {
			return nil, false
		}
		_ = os.Remove(name)
	}
	return nil, false
}

// Prebaixar is one background tick: if the channel has a release newer than
// installed and it is not cached yet, download it into the cache. Returns the
// version that became ready, or "" when there was nothing to do.
func Prebaixar(ctx context.Context, installed string) (string, error) {
	tag, err := TagUltima(ctx)
	if err != nil {
		return "", err
	}
	if newer, err := compareVersions(tag, installed); err != nil || newer <= 0 {
		return "", err
	}
	// Already cached: no API call. The checksum is checked by /update, which
	// has SHA256SUMS at hand anyway.
	if _, err := os.Stat(prefetchPath(tag)); err == nil {
		return "", nil
	}
	plan, err := Verificar(ctx, installed)
	if err != nil || plan.Current {
		return "", err
	}
	if _, ok := cachedFor(plan); ok {
		return "", nil
	}
	dir := prefetchDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	release, ok := takeLock(dir, plan.Release.Version)
	if !ok {
		return "", nil
	}
	defer release()

	// Download to a per-process temp name and rename at the end: the final
	// name only ever appears complete and verified.
	tmp := filepath.Join(dir, ".tmp."+strconv.Itoa(os.Getpid())+"."+plan.Asset.Name)
	if _, err := Baixar(ctx, plan.Asset, plan.Sum, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, prefetchPath(plan.Release.Version)); err != nil {
		removeIfExists(tmp)
		return "", err
	}
	pruneCache(dir, plan.Release.Version)
	return plan.Release.Version, nil
}

// pruneCache deletes cached binaries of other versions: the cache holds the
// next update, not an archive of releases.
func pruneCache(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := AssetPlataformaNome() + "."
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, prefix) && name != prefix+keep {
			removeIfExists(filepath.Join(dir, name))
		}
	}
}

// LoopPrefetch runs Prebaixar now and then every IntervaloPrefetch until ctx
// is canceled. It is silent: network errors, an offline channel and a
// rate-limited API are normal, and the next tick retries. A build without a
// release version ("dev") does not prefetch at all — every tag would look
// newer and the loop would download on every developer's machine.
func LoopPrefetch(ctx context.Context, installed string) {
	if _, err := parseVersion(installed); err != nil {
		return
	}
	for {
		_, _ = Prebaixar(ctx, installed)
		select {
		case <-ctx.Done():
			return
		case <-time.After(IntervaloPrefetch):
		}
	}
}
