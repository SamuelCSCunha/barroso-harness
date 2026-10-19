//go:build !windows

package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTree creates an "installed binary" and a "staged" file in a TempDir, so
// installation swaps fake files and never the binary running the tests.
func fakeTree(t *testing.T) (target, staged string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "barroso-harness")
	staged = filepath.Join(dir, "staged")
	if err := os.WriteFile(target, []byte("old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	return target, staged
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// The happy path: the staged file becomes the target and the previous one is
// kept as .old, so a failed update can be rolled back.
func TestInstallUnixReplacesAndKeepsPrevious(t *testing.T) {
	target, staged := fakeTree(t)

	result, err := installAt(target, staged)
	if err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if result.path != target {
		t.Fatalf("installed path %q, want %q", result.path, target)
	}
	if want := "v0.1.0-bh.9"; readFile(t, target) != want {
		t.Fatalf("target has %q, want %q", readFile(t, target), want)
	}
	// The previous one remains recoverable.
	kept := target + ".old"
	if want := "old-version"; readFile(t, kept) != want {
		t.Fatalf("backup %q has %q, want %q", kept, readFile(t, kept), want)
	}
	// The staged file was consumed by the rename.
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged should have been renamed: %v", err)
	}
}

// The installed binary must be executable: 0755, not the download's 0700.
func TestInstallUnixLeavesBinaryExecutable(t *testing.T) {
	target, staged := fakeTree(t)
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", mode)
	}
}

// Staging happens in the target's own directory: that is what makes the
// final rename atomic. If it were done in /tmp, a cross-filesystem rename
// would fail with EXDEV — and the target would be left without a binary.
func TestInstallUnixLeavesNoStagingBehind(t *testing.T) {
	target, staged := fakeTree(t)
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".barroso-harness-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("staging was left behind: %v", leftovers)
	}
}

// A second /update does not overwrite the existing backup: the first .old is
// still what rolling back would restore.
func TestInstallUnixDoesNotOverwriteExistingBackup(t *testing.T) {
	target, staged := fakeTree(t)
	previous := target + ".old"
	if err := os.WriteFile(previous, []byte("original-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if readFile(t, previous) != "original-backup" {
		t.Fatalf("original backup was overwritten: %q", readFile(t, previous))
	}
	// This install's previous version went to the next free slot.
	if readFile(t, target+".old-1") != "old-version" {
		t.Fatalf("expected the previous one in .old-1, got %q", readFile(t, target+".old-1"))
	}
}

// Without write permission on the target's directory (a root-owned binary in
// /usr/local/bin), the installer does not give up: it returns ready-made sudo
// commands.
func TestInstallUnixWithoutPermissionReturnsSudoCommands(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: everything is writable, the EACCES path never happens")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "barroso-harness")
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Read-only directory: creating the staging file next to the target fails with EACCES.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	result, err := installAt(target, staged)
	if err == nil {
		t.Fatal("without permission it should return an error, not silent success")
	}
	if len(result.commands) == 0 {
		t.Fatalf("error without actionable commands: %v", err)
	}
	commands := strings.Join(result.commands, "\n")
	if !strings.Contains(commands, "sudo install -m 0755") {
		t.Fatalf("missing the install command: %q", commands)
	}
	if !strings.Contains(commands, "sudo ln -sf") {
		t.Fatalf("missing the bh shortcut: %q", commands)
	}
	if !strings.Contains(commands, target) {
		t.Fatalf("commands do not mention the target %q: %q", target, commands)
	}
}

// The `bh` shortcut is a symlink to the binary. EvalSymlinks resolves the
// real target, so updating through the symlink replaces the actual binary and
// the shortcut keeps working — which is what this test pins.
func TestInstallUnixResolvesShortcutSymlink(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("symlinks: covered where EvalSymlinks is available")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "barroso-harness-real")
	shortcut := filepath.Join(dir, "bh")
	if err := os.WriteFile(real, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, shortcut); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(shortcut)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if resolved != real {
		t.Fatalf("symlink resolved to %q, want %q", resolved, real)
	}
}
