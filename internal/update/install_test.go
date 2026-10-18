//go:build !windows

package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// arvoreFalsa cria um "binário instalado" e um "staged" num TempDir, para a
// instalação trocar arquivo de mentira e nunca o binário que roda os testes.
func arvoreFalsa(t *testing.T) (alvo, staged string) {
	t.Helper()
	dir := t.TempDir()
	alvo = filepath.Join(dir, "barroso-harness")
	staged = filepath.Join(dir, "staged")
	if err := os.WriteFile(alvo, []byte("versao-antiga"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	return alvo, staged
}

func ler(t *testing.T, caminho string) string {
	t.Helper()
	raw, err := os.ReadFile(caminho)
	if err != nil {
		return ""
	}
	return string(raw)
}

// O caminho feliz: o staged passa a ser o alvo e o anterior fica preservado em
// .old, para uma atualização que deu errado poder voltar atrás.
func TestInstallUnixTrocaEPreservaAnterior(t *testing.T) {
	alvo, staged := arvoreFalsa(t)

	resultado, err := installAt(alvo, staged)
	if err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if resultado.path != alvo {
		t.Fatalf("caminho instalado %q, esperado %q", resultado.path, alvo)
	}
	if querido := "v0.1.0-bh.9"; ler(t, alvo) != querido {
		t.Fatalf("alvo tem %q, esperado %q", ler(t, alvo), querido)
	}
	// O anterior continua recuperável.
	preservado := alvo + ".old"
	if querido := "versao-antiga"; ler(t, preservado) != querido {
		t.Fatalf("backup %q tem %q, esperado %q", preservado, ler(t, preservado), querido)
	}
	// O staged foi consumido pelo rename.
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged devia ter sido renomeado: %v", err)
	}
}

// O binário instalado tem de ser executável: 0755, não o 0700 do download.
func TestInstallUnixDeixaBinarioExecutavel(t *testing.T) {
	alvo, staged := arvoreFalsa(t)
	if _, err := installAt(alvo, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	info, err := os.Stat(alvo)
	if err != nil {
		t.Fatal(err)
	}
	if modo := info.Mode().Perm(); modo&0o111 == 0 {
		t.Fatalf("binário instalado não é executável: %v", modo)
	}
}

// O staging acontece no próprio diretório do alvo: é o que garante que o rename
// final é atômico. Se ele fosse feito em /tmp, um rename cruzando filesystems
// falharia com EXDEV — e aí o alvo ficaria sem binário.
func TestInstallUnixNaoDeixaStagingSobrando(t *testing.T) {
	alvo, staged := arvoreFalsa(t)
	if _, err := installAt(alvo, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	sobras, err := filepath.Glob(filepath.Join(filepath.Dir(alvo), ".barroso-harness-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sobras) != 0 {
		t.Fatalf("staging ficou para trás: %v", sobras)
	}
}

// Um segundo /update não sobrescreve o backup existente: o .old do primeiro
// continua sendo o que voltar para trás levaria.
func TestInstallUnixNaoSobrescreveBackupExistente(t *testing.T) {
	alvo, staged := arvoreFalsa(t)
	anterior := alvo + ".old"
	if err := os.WriteFile(anterior, []byte("backup-original"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installAt(alvo, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if ler(t, anterior) != "backup-original" {
		t.Fatalf("backup original foi sobrescrito: %q", ler(t, anterior))
	}
	// A versão anterior deste install foi para o próximo slot livre.
	if ler(t, alvo+".old-1") != "versao-antiga" {
		t.Fatalf("esperava o anterior em .old-1, veio %q", ler(t, alvo+".old-1"))
	}
}

// Sem permissão de escrita no diretório do alvo (binário root-owned em
// /usr/local/bin), o instalador não desiste: devolve os comandos sudo prontos.
func TestInstallUnixSemPermissaoDevolveComandosSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("rodando como root: tudo é escrevível, o caminho EACCES não acontece")
	}
	dir := t.TempDir()
	alvo := filepath.Join(dir, "barroso-harness")
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(alvo, []byte("velho"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("novo"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Diretório sem escrita: criar o staging ao lado do alvo falha com EACCES.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	resultado, err := installAt(alvo, staged)
	if err == nil {
		t.Fatal("sem permissão devia devolver erro, não sucesso silencioso")
	}
	if len(resultado.commands) == 0 {
		t.Fatalf("erro sem comandos acionáveis: %v", err)
	}
	comandos := strings.Join(resultado.commands, "\n")
	if !strings.Contains(comandos, "sudo install -m 0755") {
		t.Fatalf("falta o install: %q", comandos)
	}
	if !strings.Contains(comandos, "sudo ln -sf") {
		t.Fatalf("falta o atalho bh: %q", comandos)
	}
	if !strings.Contains(comandos, alvo) {
		t.Fatalf("comandos não citam o alvo %q: %q", alvo, comandos)
	}
}

// O atalho `bh` é um symlink para o binário. EvalSymlinks resolve o alvo real,
// então atualizar pelo symlink troca o binário de verdade e o atalho continua
// valendo — é o que este teste fixa.
func TestInstallUnixResolveSymlinkDoAtalho(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("symlinks: coberto onde o import de EvalSymlinks está disponível")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "barroso-harness-real")
	atalho := filepath.Join(dir, "bh")
	if err := os.WriteFile(real, []byte("velho"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, atalho); err != nil {
		t.Skipf("symlink indisponível: %v", err)
	}

	resolvido, err := filepath.EvalSymlinks(atalho)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if resolvido != real {
		t.Fatalf("symlink resolveu para %q, esperado %q", resolvido, real)
	}
}
