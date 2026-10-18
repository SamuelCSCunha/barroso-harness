//go:build windows

package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arvoreWindowsFalsa monta o que a instalação no Windows cria: dois arquivos
// COPIADOS (não symlink), barroso-harness.exe e bh.exe, na mesma pasta.
func arvoreWindowsFassa(t *testing.T) (alvo, alias, staged string) {
	t.Helper()
	dir := t.TempDir()
	alvo = filepath.Join(dir, "barroso-harness.exe")
	alias = filepath.Join(dir, "bh.exe")
	staged = filepath.Join(dir, "staged.exe")
	for _, caminho := range []string{alvo, alias} {
		if err := os.WriteFile(caminho, []byte("versao-antiga"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	return alvo, alias, staged
}

func lerW(t *testing.T, caminho string) string {
	t.Helper()
	raw, err := os.ReadFile(caminho)
	if err != nil {
		return ""
	}
	return string(raw)
}

// O rename-dance: o alvo vai para .old-N e o staged toma o lugar. Um .exe em
// execução não pode ser sobrescrito, mas PODE ser renomeado — é o que torna
// essa troca possível.
func TestInstallWindowsTrocaComRenameDance(t *testing.T) {
	alvo, _, staged := arvoreWindowsFassa(t)

	resultado, err := installAt(alvo, staged)
	if err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if resultado.path != alvo {
		t.Fatalf("caminho %q, esperado %q", resultado.path, alvo)
	}
	if querido := "v0.1.0-bh.9"; lerW(t, alvo) != querido {
		t.Fatalf("alvo tem %q, esperado %q", lerW(t, alvo), querido)
	}
	backup := alvo + ".old-1"
	if querido := "versao-antiga"; lerW(t, backup) != querido {
		t.Fatalf("backup %q tem %q, esperado %q", backup, lerW(t, backup), querido)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged devia ter sido renomeado: %v", err)
	}
}

// Um segundo update não sobrescreve o backup do primeiro.
func TestInstallWindowsNaoSobrescreveBackup(t *testing.T) {
	alvo, _, staged := arvoreWindowsFassa(t)
	if err := os.WriteFile(alvo+".old-1", []byte("backup-original"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installAt(alvo, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if lerW(t, alvo+".old-1") != "backup-original" {
		t.Fatalf("backup original sobrescrito: %q", lerW(t, alvo+".old-1"))
	}
	if lerW(t, alvo+".old-2") != "versao-antiga" {
		t.Fatalf("anterior devia cair em .old-2, veio %q", lerW(t, alvo+".old-2"))
	}
}

// O segundo alias (bh.exe quando o alvo é barroso-harness.exe) é uma CÓPIA na
// instalação Windows, então ele também precisa ser trocado. Este teste fixa o
// defeito de passar o mesmo staged duas vezes: depois do primeiro rename o
// staged não existe mais, e o segundo installAt falharia.
func TestInstallWindowsTrocaOsDoisAliases(t *testing.T) {
	alvo, alias, staged := arvoreWindowsFassa(t)

	// Um staged por alias, que é o que a correção precisa produzir.
	stagedAlias := filepath.Join(filepath.Dir(staged), "staged-alias.exe")
	if err := os.WriteFile(stagedAlias, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := installAt(alvo, staged); err != nil {
		t.Fatalf("installAt alvo: %v", err)
	}
	if _, err := installAt(alias, stagedAlias); err != nil {
		t.Fatalf("installAt alias: %v", err)
	}
	if lerW(t, alvo) != "v0.1.0-bh.9" {
		t.Fatalf("alvo não foi trocado: %q", lerW(t, alvo))
	}
	if lerW(t, alias) != "v0.1.0-bh.9" {
		t.Fatalf("alias não foi trocado: %q", lerW(t, alias))
	}
}

// Um installBinary completo troca os dois arquivos com UM staged só. É aqui que
// o comportamento real aparece — inclusive o defeito, se ele existir.
func TestInstallWindowsInstallBinaryTrocaOsDois(t *testing.T) {
	alvo, alias, staged := arvoreWindowsFassa(t)

	resultado, err := installPair(alvo, staged)
	if err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	if resultado.path != alvo {
		t.Fatalf("caminho %q, esperado %q", resultado.path, alvo)
	}
	if lerW(t, alvo) != "v0.1.0-bh.9" {
		t.Fatalf("alvo tem %q", lerW(t, alvo))
	}
	if lerW(t, alias) != "v0.1.0-bh.9" {
		t.Fatalf("segundo alias ficou com %q — um staged só não pode trocar dois arquivos", lerW(t, alias))
	}
}

// Sem o segundo arquivo, o installBinary segue trocando o alvo: a ausência do
// alias não pode impedir a atualização principal.
func TestInstallWindowsSemSegundoAliasAtualizaSozinho(t *testing.T) {
	dir := t.TempDir()
	alvo := filepath.Join(dir, "barroso-harness.exe")
	staged := filepath.Join(dir, "staged.exe")
	if err := os.WriteFile(alvo, []byte("velho"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installPair(alvo, staged); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	if lerW(t, alvo) != "v0.1.0-bh.9" {
		t.Fatalf("alvo tem %q", lerW(t, alvo))
	}
}

// O snippet de PowerShell tem de citar os dois caminhos com -Force, para quem
// tiver de fazer a troca à mão poder colar.
func TestInstallWindowsSnippetPowerShell(t *testing.T) {
	comando := powershellReplace(`C:\bin\bh.exe`, `C:\tmp\staged.exe`)
	for _, querido := range []string{"Move-Item", "-Force", `C:\tmp\staged.exe`, `C:\bin\bh.exe`} {
		if !strings.Contains(comando, querido) {
			t.Fatalf("comando %q não tem %q", comando, querido)
		}
	}
}
