package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSemArquivoDevolveVazio(t *testing.T) {
	dir := t.TempDir()
	s, err := Resolve(dir)
	if err != nil {
		t.Fatalf("sem BARROSO.md deve devolver string vazia, não erro: %v", err)
	}
	if s != "" {
		t.Fatalf("esperava vazio, veio %q", s)
	}
}

func TestResolveFilhoAntesDoPai(t *testing.T) {
	pai := t.TempDir()
	filho := filepath.Join(pai, "proj", "sub")
	if err := os.MkdirAll(filho, 0o755); err != nil {
		t.Fatal(err)
	}
	paiMD := "regra do pai"
	filhoMD := "regra do filho"
	if err := os.WriteFile(filepath.Join(pai, "BARROSO.md"), []byte(paiMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filho, "BARROSO.md"), []byte(filhoMD), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(filho)
	if err != nil {
		t.Fatal(err)
	}
	if iFilho, iPai := strings.Index(got, filhoMD), strings.Index(got, paiMD); iFilho > iPai {
		t.Fatalf("esperava filho antes do pai, veio:\n%s", got)
	}
}

func TestResolveArquivoVazioNaoEntra(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "BARROSO.md"), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s != "" {
		t.Fatalf("BARROSO.md em branco não deve entrar na instrução, veio %q", s)
	}
}
