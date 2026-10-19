//go:build windows

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
		return "", err
	}
	return filepath.Abs(resolved)
}

// installBinary is the production path: it finds the RUNNING executable and
// replaces it. It is thin on purpose — the logic that matters is in
// installPair, which takes the target as a parameter and is therefore
// testable without touching the binary running the tests.
func installBinary(staged string) (installResult, error) {
	target, err := executablePath()
	if err != nil {
		return installResult{}, err
	}
	return installPair(target, staged)
}

// installPair replaces the target and, when it exists, the second copy (the
// shortcut).
//
// The second file needs ITS OWN staged copy: after the first rename the
// original staged file no longer exists, and reusing it would make the alias
// fail — which is what TestInstallWindowsInstallBinaryReplacesBoth caught.
func installPair(target, staged string) (installResult, error) {
	result, err := installAt(target, staged)
	if err != nil {
		return result, err
	}
	alias := ""
	base := strings.ToLower(filepath.Base(target))
	if base == "bh.exe" {
		alias = filepath.Join(filepath.Dir(target), "barroso-harness.exe")
	} else if base == "barroso-harness.exe" {
		alias = filepath.Join(filepath.Dir(target), "bh.exe")
	}
	if alias == "" {
		return result, nil
	}
	if _, statErr := os.Lstat(alias); statErr != nil {
		return result, nil
	}
	// The second copy comes from the already-replaced TARGET, not from the
	// staged file: after the initial rename the staged file no longer exists,
	// and opening it again would fail. The freshly installed target is
	// exactly the content the manifest verified.
	second, err := copyToStaging(alias, target)
	if err != nil {
		result.commands = append(result.commands, powershellReplace(alias, target))
		return result, nil
	}
	if _, aliasErr := installAt(alias, second); aliasErr != nil {
		result.commands = append(result.commands, powershellReplace(alias, second))
	}
	return result, nil
}

// installAt is factored out so tests swap only files in TempDir, never the
// binary running the tests itself.
func installAt(target, staged string) (installResult, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return installResult{}, err
	}
	staged, err = filepath.Abs(staged)
	if err != nil {
		return installResult{}, err
	}
	backup := fmt.Sprintf("%s.old-1", target)
	for i := 1; ; i++ {
		backup = fmt.Sprintf("%s.old-%d", target, i)
		if _, err := os.Lstat(backup); os.IsNotExist(err) {
			break
		}
	}
	if err := os.Rename(target, backup); err != nil {
		return installResult{commands: []string{powershellReplace(target, staged)}}, fmt.Errorf("rename running executable: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return installResult{commands: []string{powershellReplace(target, staged)}}, fmt.Errorf("installing staged binary failed (%v) and rollback failed (%w)", err, rollbackErr)
		}
		return installResult{commands: []string{powershellReplace(target, staged)}}, fmt.Errorf("install staged binary: %w", err)
	}
	return installResult{path: target}, nil
}

func powershellReplace(target, staged string) string {
	// Single quotes, not %q: in PowerShell the path goes in single quotes and
	// doubled backslashes would be a broken path.
	return fmt.Sprintf("Move-Item -LiteralPath '%s' -Destination '%s' -Force", staged, target)
}

// copyToStaging prepares a second staged file next to the alias, taken from
// the already-replaced target. The content is what the manifest verified —
// which avoids a second download and keeps a single verification point.
func copyToStaging(target, source string) (string, error) {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".barroso-harness-alias-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if _, err := tmp.ReadFrom(in); err != nil {
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

func verifyStaged(staged, version string) error {
	cmd := exec.Command(staged, "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run staged binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return validateVersionOutput(string(output), version)
}

func installCommands(result installResult) string { return strings.Join(result.commands, "\n") }
