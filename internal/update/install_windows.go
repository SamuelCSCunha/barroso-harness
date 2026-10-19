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

// installBinary é o caminho de produção: descobre o executável EM EXECUÇÃO e o
// troca. Fica fina de propósito — a lógica que importa está em installPair,
// que recebe o alvo como parâmetro e por isso é testável sem tocar no binário
// que está rodando os testes.
func installBinary(staged string) (installResult, error) {
	target, err := executablePath()
	if err != nil {
		return installResult{}, err
	}
	return installPair(target, staged)
}

// installPair troca o alvo e, quando ela existe, a segunda cópia do atalho.
//
// O segundo arquivo precisa do SEU PRÓPRIO staged: depois do primeiro rename o
// staged original já não existe mais, e reusá-lo faria o alias falhar — foi o
// que o teste TestInstallBinaryTrocaOsDois pegou.
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
	// A segunda cópia sai do ALVO já trocado, não do staged: depois do rename
	// inicial o staged não existe mais, e abri-lo de novo falharia. O alvo
	// recém-instalado é exatamente o conteúdo que o manifest conferiu.
	segundo, err := copyToStaging(alias, target)
	if err != nil {
		result.commands = append(result.commands, powershellReplace(alias, target))
		return result, nil
	}
	if _, aliasErr := installAt(alias, segundo); aliasErr != nil {
		result.commands = append(result.commands, powershellReplace(alias, segundo))
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
	// Aspas simples, não %q: no PowerShell o caminho vai entre aspas simples e
	// barras invertidas duplicadas seriam um caminho quebrado.
	return fmt.Sprintf("Move-Item -LiteralPath '%s' -Destination '%s' -Force", staged, target)
}

// copyToStaging prepara um segundo staged ao lado do alias, tirado do alvo que
// já foi trocado. O conteúdo é o mesmo que o manifest conferiu — o que evita um
// segundo download e mantém um único ponto de verificação.
func copyToStaging(target, origem string) (string, error) {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".barroso-harness-alias-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	in, err := os.Open(origem)
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
