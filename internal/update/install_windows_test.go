//go:build windows

package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWindowsTree builds what the Windows installation creates: two COPIED
// files (not symlinks), barroso-harness.exe and bh.exe, in the same folder.
func fakeWindowsTree(t *testing.T) (target, alias, staged string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "barroso-harness.exe")
	alias = filepath.Join(dir, "bh.exe")
	staged = filepath.Join(dir, "staged.exe")
	for _, path := range []string{target, alias} {
		if err := os.WriteFile(path, []byte("old-version"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	return target, alias, staged
}

func readFileW(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// The rename dance: the target goes to .old-N and the staged file takes its
// place. A running .exe cannot be overwritten, but it CAN be renamed — which
// is what makes this swap possible.
func TestInstallWindowsSwapsWithRenameDance(t *testing.T) {
	target, _, staged := fakeWindowsTree(t)

	result, err := installAt(target, staged)
	if err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if result.path != target {
		t.Fatalf("path %q, want %q", result.path, target)
	}
	if want := "v0.1.0-bh.9"; readFileW(t, target) != want {
		t.Fatalf("target has %q, want %q", readFileW(t, target), want)
	}
	backup := target + ".old-1"
	if want := "old-version"; readFileW(t, backup) != want {
		t.Fatalf("backup %q has %q, want %q", backup, readFileW(t, backup), want)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged should have been renamed: %v", err)
	}
}

// A second update does not overwrite the first one's backup.
func TestInstallWindowsDoesNotOverwriteBackup(t *testing.T) {
	target, _, staged := fakeWindowsTree(t)
	if err := os.WriteFile(target+".old-1", []byte("original-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if readFileW(t, target+".old-1") != "original-backup" {
		t.Fatalf("original backup overwritten: %q", readFileW(t, target+".old-1"))
	}
	if readFileW(t, target+".old-2") != "old-version" {
		t.Fatalf("previous one should land in .old-2, got %q", readFileW(t, target+".old-2"))
	}
}

// The second alias (bh.exe when the target is barroso-harness.exe) is a COPY
// in the Windows installation, so it must be replaced too. This test pins
// the defect of passing the same staged file twice: after the first rename
// the staged file no longer exists, and the second installAt would fail.
func TestInstallWindowsReplacesBothAliases(t *testing.T) {
	target, alias, staged := fakeWindowsTree(t)

	// One staged file per alias, which is what the fix must produce.
	stagedAlias := filepath.Join(filepath.Dir(staged), "staged-alias.exe")
	if err := os.WriteFile(stagedAlias, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt target: %v", err)
	}
	if _, err := installAt(alias, stagedAlias); err != nil {
		t.Fatalf("installAt alias: %v", err)
	}
	if readFileW(t, target) != "v0.1.0-bh.9" {
		t.Fatalf("target was not replaced: %q", readFileW(t, target))
	}
	if readFileW(t, alias) != "v0.1.0-bh.9" {
		t.Fatalf("alias was not replaced: %q", readFileW(t, alias))
	}
}

// A full installBinary replaces both files with a SINGLE staged file. This is
// where the real behavior shows up — including the defect, if it exists.
func TestInstallWindowsInstallBinaryReplacesBoth(t *testing.T) {
	target, alias, staged := fakeWindowsTree(t)

	result, err := installPair(target, staged)
	if err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	if result.path != target {
		t.Fatalf("path %q, want %q", result.path, target)
	}
	if readFileW(t, target) != "v0.1.0-bh.9" {
		t.Fatalf("target has %q", readFileW(t, target))
	}
	if readFileW(t, alias) != "v0.1.0-bh.9" {
		t.Fatalf("second alias still has %q — a single staged file cannot replace two files", readFileW(t, alias))
	}
}

// Without the second file, installBinary still replaces the target: a missing
// alias must not block the main update.
func TestInstallWindowsWithoutSecondAliasUpdatesAlone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "barroso-harness.exe")
	staged := filepath.Join(dir, "staged.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installPair(target, staged); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	if readFileW(t, target) != "v0.1.0-bh.9" {
		t.Fatalf("target has %q", readFileW(t, target))
	}
}

// The PowerShell snippet must mention both paths with -Force, so whoever has
// to do the swap by hand can paste it.
func TestInstallWindowsPowerShellSnippet(t *testing.T) {
	command := powershellReplace(`C:\bin\bh.exe`, `C:\tmp\staged.exe`)
	for _, want := range []string{"Move-Item", "-Force", `C:\tmp\staged.exe`, `C:\bin\bh.exe`} {
		if !strings.Contains(command, want) {
			t.Fatalf("command %q lacks %q", command, want)
		}
	}
}
