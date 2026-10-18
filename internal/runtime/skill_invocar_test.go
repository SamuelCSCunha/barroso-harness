package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skillNoProjeto(t *testing.T, nome, corpo string) string {
	t.Helper()
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".barroso", "skills", nome)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conteudo := "---\nname: " + nome + "\ndescription: teste\n---\n" + corpo + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(conteudo), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir()) // isola a fonte global
	return cwd
}

func TestExpandirSkillMontaPedidoComCorpoEArgs(t *testing.T) {
	cwd := skillNoProjeto(t, "revisar-pr", "1. leia o diff\n2. aponte bugs")
	pedido, nome, ok := ExpandirSkill(cwd, "/revisar-pr  #42 com foco em segurança")
	if !ok || nome != "revisar-pr" {
		t.Fatalf("skill não reconhecida: ok=%v nome=%q", ok, nome)
	}
	for _, quer := range []string{"Execute agora a skill `revisar-pr`", "1. leia o diff", "2. aponte bugs", "Argumentos do usuário: #42 com foco em segurança"} {
		if !strings.Contains(pedido, quer) {
			t.Errorf("pedido sem %q:\n%s", quer, pedido)
		}
	}
}

func TestExpandirSkillSemArgsNaoInventaLinha(t *testing.T) {
	cwd := skillNoProjeto(t, "deploy", "rode o deploy")
	pedido, _, ok := ExpandirSkill(cwd, "/DEPLOY")
	if !ok {
		t.Fatal("nome deveria casar sem diferenciar maiúscula")
	}
	if strings.Contains(pedido, "Argumentos") {
		t.Errorf("sem argumentos não deveria haver a linha:\n%s", pedido)
	}
}

func TestExpandirSkillNaoCasaNomeDesconhecidoNemTexto(t *testing.T) {
	cwd := skillNoProjeto(t, "deploy", "x")
	for _, l := range []string{"/nao-existe", "deploy", "/", "  "} {
		if _, _, ok := ExpandirSkill(cwd, l); ok {
			t.Errorf("%q não deveria virar skill", l)
		}
	}
}
