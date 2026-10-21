package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newestCached picks the newest cached version above the installed one and
// ignores everything else in the folder.
func TestNewestCached(t *testing.T) {
	dir := usePrefetchDir(t)
	for _, v := range []string{"v0.1.0-bh.8", "v0.1.0-bh.10", "v0.1.0-bh.9"} {
		_ = os.WriteFile(prefetchPath(v), []byte(v), 0o600)
	}
	_ = os.WriteFile(filepath.Join(dir, ".lock.install"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(dir, AssetPlataformaNome()+".garbage"), nil, 0o600)

	cases := map[string]string{
		"v0.1.0-bh.7":  "v0.1.0-bh.10",
		"v0.1.0-bh.9":  "v0.1.0-bh.10",
		"v0.1.0-bh.10": "",
		"v0.1.0-bh.11": "",
	}
	for installed, want := range cases {
		got, err := newestCached(installed)
		if err != nil || got != want {
			t.Errorf("newestCached(%s) = %q, %v; want %q", installed, got, err, want)
		}
	}
}

// An empty or missing cache is not an error: there is just nothing to install.
func TestNewestCachedMissingDir(t *testing.T) {
	dir := usePrefetchDir(t)
	_ = os.Remove(dir)
	if got, err := newestCached("v0.1.0-bh.1"); got != "" || err != nil {
		t.Fatalf("newestCached on a missing dir = %q, %v", got, err)
	}
}

// Up to date: the tick stops after the free redirect and installs nothing.
func TestAutoInstalarSkipsWhenNotNewer(t *testing.T) {
	usePrefetchDir(t)
	useLatest(t, "v0.1.0-bh.10")
	prev := installOverride
	installOverride = func(string) error {
		t.Fatal("installed while up to date")
		return nil
	}
	defer func() { installOverride = prev }()
	for _, installed := range []string{"v0.1.0-bh.10", "v0.1.0-bh.11"} {
		if got, err := AutoInstalar(context.Background(), installed); got != "" || err != nil {
			t.Fatalf("AutoInstalar(%s) = %q, %v; want nothing to do", installed, got, err)
		}
	}
}

func TestAliasOf(t *testing.T) {
	dir := filepath.Join("x", "bin")
	cases := map[string]string{
		filepath.Join(dir, "bh.exe"):              filepath.Join(dir, "barroso-harness.exe"),
		filepath.Join(dir, "barroso-harness.exe"): filepath.Join(dir, "bh.exe"),
		filepath.Join(dir, "bh"):                  filepath.Join(dir, "barroso-harness"),
		filepath.Join(dir, "barroso-harness"):     filepath.Join(dir, "bh"),
		filepath.Join(dir, "update.test"):         "",
	}
	for in, want := range cases {
		if got := aliasOf(in); got != want {
			t.Errorf("aliasOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A failed release is retried only once the channel publishes another one.
func TestChannelMovedPast(t *testing.T) {
	useLatest(t, "v0.1.0-bh.10")
	if channelMovedPast(context.Background(), "v0.1.0-bh.10") {
		t.Fatal("same release reported as moved")
	}
	if !channelMovedPast(context.Background(), "v0.1.0-bh.9") {
		t.Fatal("newer release not reported as moved")
	}
}

// A dev build must not auto-update: every tag would look newer.
func TestLoopAutoOffForDev(t *testing.T) {
	done := make(chan struct{})
	go func() {
		LoopAuto(context.Background(), "dev", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("LoopAuto kept running for a dev build")
	}
}
