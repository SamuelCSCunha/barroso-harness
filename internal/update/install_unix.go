//go:build !windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type installResult struct {
	path     string
	commands []string
}

func executablePath() (string, error) {
	name, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", fmt.Errorf("resolver caminho do executável: %w", err)
	}
	return filepath.Abs(resolved)
}

func installBinary(staged string) (installResult, error) {
	target, err := executablePath()
	if err != nil {
		return installResult{}, err
	}
	return installAt(target, staged)
}

// installAt is factored out so tests can replace only files inside TempDir.
func installAt(target, staged string) (installResult, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return installResult{}, err
	}
	staged, err = filepath.Abs(staged)
	if err != nil {
		return installResult{}, err
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".barroso-harness-update-*")
	if err != nil {
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, fmt.Errorf("criar staging ao lado do binário: %w", err)
	}
	tmpName := tmp.Name()
	defer removeIfExists(tmpName)

	in, err := os.Open(staged)
	if err != nil {
		_ = tmp.Close()
		return installResult{}, err
	}
	_, copyErr := tmp.ReadFrom(in)
	closeInErr := in.Close()
	if copyErr != nil {
		_ = tmp.Close()
		if isPermissionError(copyErr) {
			return permissionFallback(staged, target)
		}
		return installResult{}, copyErr
	}
	if closeInErr != nil {
		_ = tmp.Close()
		return installResult{}, closeInErr
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return installResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return installResult{}, err
	}

	backup := target + ".old"
	for i := 1; ; i++ {
		if _, err := os.Lstat(backup); os.IsNotExist(err) {
			break
		}
		backup = fmt.Sprintf("%s.old-%d", target, i)
	}
	if err := os.Rename(target, backup); err != nil && !os.IsNotExist(err) {
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, fmt.Errorf("guardar binário anterior: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Rename(backup, target)
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, fmt.Errorf("instalar binário novo: %w", err)
	}
	return installResult{path: target}, nil
}

func permissionFallback(staged, target string) (installResult, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return installResult{}, err
	}
	fallback := filepath.Join(cache, "barroso-harness", "updates", filepath.Base(staged))
	if err := os.MkdirAll(filepath.Dir(fallback), 0o700); err != nil {
		return installResult{}, err
	}
	if err := copyFile(staged, fallback); err != nil {
		return installResult{}, err
	}
	alias := filepath.Join(filepath.Dir(target), "bh")
	return installResult{path: fallback, commands: []string{
		fmt.Sprintf("sudo install -m 0755 %q %q", fallback, target),
		fmt.Sprintf("sudo ln -sf %q %q", target, alias),
	}}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Chmod(0o755); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

func verifyStaged(staged, version string) error {
	cmd := exec.Command(staged, "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("executar binário staged: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return validateVersionOutput(string(output), version)
}

func installCommands(result installResult) string { return strings.Join(result.commands, "\n") }
