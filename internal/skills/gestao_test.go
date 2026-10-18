package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execGestao(t *testing.T, raizes map[string]string, nome string, args map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	for _, s := range EspecificacoesGestao(raizes) {
		if s.Name == nome {
			return s.Exec(context.Background(), "", b)
		}
	}
	t.Fatalf("spec %s ausente", nome)
	return "", nil
}

func TestSkillCreateGravaEDescobre(t *testing.T) {
	raiz := filepath.Join(t.TempDir(), "skills")
	r := map[string]string{"projeto": raiz, "global": t.TempDir()}
	out, err := execGestao(t, r, "skill_create", map[string]any{"name": "revisar", "description": "revisa\ncódigo", "body": "1. leia\n2. aponte"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/revisar") {
		t.Errorf("saída: %q", out)
	}
	hab, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil || len(hab) != 1 || hab[0].Nome != "revisar" || hab[0].Descricao != "revisa código" || !strings.Contains(hab[0].Corpo, "2. aponte") {
		t.Fatalf("skill criada não é descoberta direito: %+v %v", hab, err)
	}
	if _, err := execGestao(t, r, "skill_remove", map[string]any{"name": "revisar"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(raiz, "revisar")); !os.IsNotExist(err) {
		t.Fatal("diretório deveria ter saído")
	}
}

func TestSkillCreateRecusaNomePerigosoEEscopoRuim(t *testing.T) {
	r := map[string]string{"projeto": t.TempDir()}
	for _, n := range []string{"../fora", "a/b", "", "com espaço"} {
		if _, err := execGestao(t, r, "skill_create", map[string]any{"name": n, "description": "d", "body": "b"}); err == nil {
			t.Errorf("nome %q deveria ser recusado", n)
		}
	}
	if _, err := execGestao(t, r, "skill_create", map[string]any{"name": "x", "description": "d", "body": "b", "scope": "global"}); err == nil {
		t.Error("escopo sem raiz deveria falhar")
	}
	if _, err := execGestao(t, r, "skill_remove", map[string]any{"name": "nao-ha"}); err == nil {
		t.Error("remover inexistente deveria falhar")
	}
}
