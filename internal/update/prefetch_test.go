package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// usePrefetchDir points the cache at a TempDir for the test.
func usePrefetchDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := prefetchDir
	prefetchDir = func() string { return dir }
	t.Cleanup(func() { prefetchDir = prev })
	return dir
}

func TestTagFromLocation(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/releases/tag/v0.1.0-bh.74":       "v0.1.0-bh.74",
		"https://github.com/o/r/releases/tag/v0.1.0-bh.74/":      "v0.1.0-bh.74",
		"https://github.com/o/r/releases/tag/v0.1.0-bh.74?x=1#a": "v0.1.0-bh.74",
		"https://github.com/o/r/releases":                        "",
		"":                                                       "",
	}
	for in, want := range cases {
		if got := tagFromLocation(in); got != want {
			t.Errorf("tagFromLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

// The tag comes from the 302 itself: the client must not follow it.
func TestTagUltimaReadsRedirectWithoutFollowing(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/releases/tag/v0.1.0-bh.74", http.StatusFound)
	}))
	defer srv.Close()
	prev := latestURL
	latestURL = srv.URL + "/latest"
	defer func() { latestURL = prev }()

	tag, err := TagUltima(context.Background())
	if err != nil || tag != "v0.1.0-bh.74" {
		t.Fatalf("TagUltima = %q, %v; want v0.1.0-bh.74", tag, err)
	}
	if followed.Load() {
		t.Fatal("the redirect was followed")
	}
}

func TestTagUltimaRejectsNonRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	prev := latestURL
	latestURL = srv.URL
	defer func() { latestURL = prev }()
	if _, err := TagUltima(context.Background()); err == nil {
		t.Fatal("a 404 should be an error")
	}
}

// Up to date (or newer) means the tick stops after the free redirect: no API
// call, no download. The API origin is a const, so a call would hit the real
// network; the redirect server counting requests proves it stopped early.
func TestPrebaixarSkipsWhenNotNewer(t *testing.T) {
	usePrefetchDir(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/releases/tag/v0.1.0-bh.10", http.StatusFound)
	}))
	defer srv.Close()
	prev := latestURL
	latestURL = srv.URL
	defer func() { latestURL = prev }()

	for _, installed := range []string{"v0.1.0-bh.10", "v0.1.0-bh.11"} {
		got, err := Prebaixar(context.Background(), installed)
		if err != nil || got != "" {
			t.Fatalf("Prebaixar(%s) = %q, %v; want nothing to do", installed, got, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("redirect hits = %d, want 2", hits.Load())
	}
}

// Newer tag already in the cache: nothing to download and no API call.
func TestPrebaixarSkipsWhenAlreadyCached(t *testing.T) {
	usePrefetchDir(t)
	if err := os.WriteFile(prefetchPath("v0.1.0-bh.10"), []byte("bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v0.1.0-bh.10", http.StatusFound)
	}))
	defer srv.Close()
	prev := latestURL
	latestURL = srv.URL
	defer func() { latestURL = prev }()

	got, err := Prebaixar(context.Background(), "v0.1.0-bh.9")
	if err != nil || got != "" {
		t.Fatalf("Prebaixar = %q, %v; want nothing to do", got, err)
	}
}

// /update reuses the cached file only when its SHA256 is the published one;
// a mismatch deletes it so the next attempt downloads again.
func TestCachedForChecksSHA256(t *testing.T) {
	usePrefetchDir(t)
	content := []byte("barroso-harness binary")
	sum := sha256.Sum256(content)
	plan := Plano{Release: Lancamento{Version: "v0.1.0-bh.10"}, Sum: hex.EncodeToString(sum[:])}
	path := prefetchPath(plan.Release.Version)

	if _, ok := cachedFor(plan); ok {
		t.Fatal("empty cache reported a hit")
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := cachedFor(plan); !ok || got != path {
		t.Fatalf("cachedFor = %q, %v; want hit on %s", got, ok, path)
	}

	// Same tag, different bytes (republished release or truncated file).
	if err := os.WriteFile(path, content[:5], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedFor(plan); ok {
		t.Fatal("a file with another SHA256 was reused")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the mismatching file should have been deleted")
	}
}

// Another version's cache never answers for this one.
func TestCachedForIsPerVersion(t *testing.T) {
	usePrefetchDir(t)
	content := []byte("x")
	sum := sha256.Sum256(content)
	_ = os.WriteFile(prefetchPath("v0.1.0-bh.9"), content, 0o600)
	plan := Plano{Release: Lancamento{Version: "v0.1.0-bh.10"}, Sum: hex.EncodeToString(sum[:])}
	if _, ok := cachedFor(plan); ok {
		t.Fatal("bh.9's file answered for bh.10")
	}
}

func TestTakeLockIsExclusiveAndRecoversStale(t *testing.T) {
	dir := t.TempDir()
	release, ok := takeLock(dir, "v1.0.0")
	if !ok {
		t.Fatal("first lock refused")
	}
	if _, ok := takeLock(dir, "v1.0.0"); ok {
		t.Fatal("second lock granted while the first is held")
	}
	if _, ok := takeLock(dir, "v1.0.1"); !ok {
		t.Fatal("a lock for another version was refused")
	}
	release()
	if r, ok := takeLock(dir, "v1.0.0"); !ok {
		t.Fatal("lock not granted after release")
	} else {
		r()
	}

	// A lock left by a dead process is taken over once stale.
	name := filepath.Join(dir, ".lock.v2.0.0")
	_ = os.WriteFile(name, nil, 0o600)
	old := time.Now().Add(-2 * lockStale)
	_ = os.Chtimes(name, old, old)
	if _, ok := takeLock(dir, "v2.0.0"); !ok {
		t.Fatal("stale lock was not taken over")
	}
}

func TestPruneCacheKeepsOnlyCurrent(t *testing.T) {
	dir := usePrefetchDir(t)
	for _, v := range []string{"v0.1.0-bh.8", "v0.1.0-bh.9", "v0.1.0-bh.10"} {
		_ = os.WriteFile(prefetchPath(v), []byte(v), 0o600)
	}
	other := filepath.Join(dir, "unrelated")
	_ = os.WriteFile(other, nil, 0o600)

	pruneCache(dir, "v0.1.0-bh.10")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("cache has %d entries after prune, want 2 (current + unrelated)", len(entries))
	}
	if _, err := os.Stat(prefetchPath("v0.1.0-bh.10")); err != nil {
		t.Fatal("current version was pruned")
	}
}

// A dev build must not prefetch: every tag would look newer.
func TestLoopPrefetchOffForDev(t *testing.T) {
	done := make(chan struct{})
	go func() {
		LoopPrefetch(context.Background(), "dev")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("LoopPrefetch kept running for a dev build")
	}
}

// useLatest points the latest-release redirect at a server announcing tag.
func useLatest(t *testing.T, tag string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	prev := latestURL
	latestURL = srv.URL
	t.Cleanup(func() { latestURL = prev })
}
