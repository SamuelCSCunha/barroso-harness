package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── helpers ───────────────────────────────────────────────────────────────

// escrever cria o caminho (com os pais) e grava o conteúdo.
func escrever(t *testing.T, caminho, conteudo string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(caminho), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caminho, []byte(conteudo), 0o644); err != nil {
		t.Fatal(err)
	}
	return caminho
}

// skillDir monta `<raiz>/<nome>/SKILL.md` com frontmatter e corpo.
func skillDir(t *testing.T, raiz, nome, front, corpo string) string {
	t.Helper()
	return escrever(t, filepath.Join(raiz, nome, nomeArquivo), front+"\n"+corpo)
}

// chamar executa a spec de `nome` e devolve a saída.
func chamar(t *testing.T, nome string, specs []tools.Spec, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Name == nome {
			return s.Exec(context.Background(), "", raw)
		}
	}
	t.Fatalf("spec %s não encontrada", nome)
	return "", nil
}

// ── frontmatter ───────────────────────────────────────────────────────────

func TestFrontmatterComAspasESemNomeVemDoDiretorio(t *testing.T) {
	raiz := t.TempDir()
	corpo := "linha um\n\nlinha dois"
	p := skillDir(t, raiz, "minha-skill",
		"---\nname: \"Outro Nome\"\ndescription: 'Faz coisas úteis.'\n---", corpo)

	s, ok, err := carregarDe(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("carregarDe: ok=%v err=%v", ok, err)
	}
	if s.Nome != "Outro Nome" {
		t.Fatalf("nome do frontmatter (com aspas) devia vencer, veio %q", s.Nome)
	}
	if s.Descricao != "Faz coisas úteis." {
		t.Fatalf("descrição sem aspas simples devia ser exata, veio %q", s.Descricao)
	}
	if s.Corpo != corpo {
		t.Fatalf("corpo devia ser preservado verbatim, veio %q", s.Corpo)
	}
	if s.Fontes != "projeto" {
		t.Fatalf("rótulo da fonte devia ser mantido, veio %q", s.Fontes)
	}
}

func TestFrontmatterSemNameDerivaDoDiretorio(t *testing.T) {
	raiz := t.TempDir()
	corpo := "instruções"
	p := skillDir(t, raiz, "skill-derivada", "---\ndescription: só descrição\n---", corpo)

	s, ok, err := carregarDe(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("carregarDe: ok=%v err=%v", ok, err)
	}
	if s.Nome != "skill-derivada" {
		t.Fatalf("sem name, o nome do diretório devia valer, veio %q", s.Nome)
	}
	if s.Descricao != "só descrição" {
		t.Fatalf("descrição devia vir do frontmatter, veio %q", s.Descricao)
	}
	if s.Corpo != corpo {
		t.Fatalf("corpo devia ser preservado, veio %q", s.Corpo)
	}
}

func TestSemFrontmatterArquivoInteiroECorpo(t *testing.T) {
	raiz := t.TempDir()
	// arquivo sem bloco `---` nenhum: conteúdo gravado cru
	texto := "- passo um\n- passo dois"
	p := escrever(t, filepath.Join(raiz, "sem-front", nomeArquivo), texto)

	s, ok, err := carregarDe(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("carregarDe: ok=%v err=%v", ok, err)
	}
	if s.Nome != "sem-front" {
		t.Fatalf("nome devia vir do diretório, veio %q", s.Nome)
	}
	if s.Descricao != "" {
		t.Fatalf("sem frontmatter não há descrição, veio %q", s.Descricao)
	}
	if s.Corpo != texto {
		t.Fatalf("sem frontmatter o arquivo inteiro é o corpo, veio %q", s.Corpo)
	}
}

func TestBlocoFrontmatterNaoFechadoECorpoInteiro(t *testing.T) {
	raiz := t.TempDir()
	texto := "name: mentira\n\nainda corpo"
	p := escrever(t, filepath.Join(raiz, "nao-fechado", nomeArquivo), "---\n"+texto)

	s, ok, err := carregarDe(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("carregarDe: ok=%v err=%v", ok, err)
	}
	if s.Nome != "nao-fechado" || s.Descricao != "" {
		t.Fatalf("bloco não fechado não é frontmatter, veio %+v", s)
	}
	if s.Corpo != "---\n"+texto {
		t.Fatalf("bloco não fechado devia ficar no corpo, veio %q", s.Corpo)
	}
}

func TestCorpoVazioNaoEntra(t *testing.T) {
	raiz := t.TempDir()
	p := skillDir(t, raiz, "vazia", "---\nname: vazia\n---", "\n  \n")

	_, ok, err := carregarDe(p, "projeto")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("skill de corpo vazio não devia ser aceita")
	}

	// e some do catálogo, sem erro
	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fora) != 0 {
		t.Fatalf("skill vazia não devia aparecer, vieram %d", len(fora))
	}
}

// ── descoberta e precedência ──────────────────────────────────────────────

func TestPrecedenciaDedupDuasFontes(t *testing.T) {
	proj := t.TempDir()
	glob := t.TempDir()

	// mesmo nome nas duas fontes: a primeira (projeto) ganha
	skillDir(t, proj, "dup", "---\nname: dup\ndescription: do projeto\n---", "corpo do projeto")
	skillDir(t, glob, "dup", "---\nname: dup\ndescription: do global\n---", "corpo do global")
	// nomes exclusivos de cada lado
	skillDir(t, proj, "so-projeto", "---\nname: so-projeto\n---", "só no projeto")
	skillDir(t, glob, "so-global", "---\nname: so-global\n---", "só no global")

	fora, err := Descobrir([]Fonte{
		{Raiz: proj, Rotulo: "projeto"},
		{Raiz: glob, Rotulo: "global"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fora) != 3 {
		t.Fatalf("esperava 3 skills depois do dedupe, vieram %d", len(fora))
	}

	porNome := map[string]Skill{}
	for _, s := range fora {
		porNome[s.Nome] = s
	}
	if d := porNome["dup"]; d.Fontes != "projeto" || !strings.Contains(d.Corpo, "do projeto") {
		t.Fatalf("a fonte de maior precedência devia vencer o dedupe, veio %+v", d)
	}
	if porNome["so-projeto"].Fontes != "projeto" {
		t.Fatalf("rótulo errado: %+v", porNome["so-projeto"])
	}
	if porNome["so-global"].Fontes != "global" {
		t.Fatalf("rótulo errado: %+v", porNome["so-global"])
	}
}

func TestDescobrirIgnoraOcultosEOrdenaPorNome(t *testing.T) {
	raiz := t.TempDir()
	skillDir(t, raiz, "zeta", "---\nname: zeta\n---", "corpo z")
	skillDir(t, raiz, "alpha", "---\nname: alpha\n---", "corpo a")
	skillDir(t, raiz, "Beta", "---\nname: Beta\n---", "corpo b")
	// ocultos não entram
	skillDir(t, raiz, ".escondida", "---\nname: escondida\n---", "corpo oculto")

	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fora) != 3 {
		t.Fatalf("diretório oculto devia ser ignorado, vieram %d: %+v", len(fora), fora)
	}
	ordem := []string{fora[0].Nome, fora[1].Nome, fora[2].Nome}
	quer := []string{"alpha", "Beta", "zeta"}
	for i := range quer {
		if ordem[i] != quer[i] {
			t.Fatalf("ordem case-insensitive devia ser %v, veio %v", quer, ordem)
		}
	}
}

func TestDescobrirSkillSoltaNaRaiz(t *testing.T) {
	raiz := t.TempDir()
	escrever(t, filepath.Join(raiz, nomeArquivo), "---\nname: solta\ndescription: na raiz\n---\ncorpo da raiz")

	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fora) != 1 || fora[0].Nome != "solta" || fora[0].Corpo != "corpo da raiz" {
		t.Fatalf("SKILL.md solto na raiz devia entrar com nome do frontmatter, veio %+v", fora)
	}
}

func TestDescobrirDiretoriosVaziosSemErro(t *testing.T) {
	dir := t.TempDir() // existe, vazio
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	fora, err := Descobrir([]Fonte{{Raiz: filepath.Join(dir, "skills"), Rotulo: "projeto"}})
	if err != nil {
		t.Fatalf("fonte vazia não é erro: %v", err)
	}
	if len(fora) != 0 {
		t.Fatalf("nenhuma skill devia surgir, vieram %d", len(fora))
	}
}

func TestDescobrirFonteInexistenteNaoErra(t *testing.T) {
	ausente := filepath.Join(t.TempDir(), "nao-existe")
	fora, err := Descobrir([]Fonte{{Raiz: ausente, Rotulo: "projeto"}})
	if err != nil {
		t.Fatalf("fonte inexistente devia ser tolerada: %v", err)
	}
	if len(fora) != 0 {
		t.Fatalf("nada devia ser encontrado, vieram %d", len(fora))
	}
}

func TestDescobrirTodasFontesIlegiveisErra(t *testing.T) {
	base := t.TempDir()
	// arquivo no lugar de diretório: ReadDir falha
	arquivo := escrever(t, filepath.Join(base, "arquivo.txt"), "x")
	_, err := Descobrir([]Fonte{{Raiz: arquivo, Rotulo: "projeto"}})
	if err == nil {
		t.Fatal("nenhuma fonte legível devia devolver erro")
	}
	if !strings.Contains(err.Error(), "nenhuma fonte") {
		t.Fatalf("mensagem de erro inesperada: %v", err)
	}

	// uma fonte legível basta para não errar
	vazia := filepath.Join(base, "vazia")
	if err := os.MkdirAll(vazia, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Descobrir([]Fonte{{Raiz: arquivo, Rotulo: "projeto"}, {Raiz: vazia, Rotulo: "projeto"}}); err != nil {
		t.Fatalf("com uma fonte legível não devia errar: %v", err)
	}
}

// ── FontesPadrao ──────────────────────────────────────────────────────────

func TestFontesPadraoComposicao(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "eu")
	cwd := filepath.Join(string(filepath.Separator), "home", "eu", "proj")

	fontes := FontesPadrao(home, cwd)
	if len(fontes) != 3 {
		t.Fatalf("esperava 3 fontes, vieram %d: %+v", len(fontes), fontes)
	}

	quer := []Fonte{
		{Raiz: filepath.Join(cwd, ".barroso", "skills"), Rotulo: "projeto"},
		{Raiz: filepath.Join(cwd, "skills"), Rotulo: "projeto"},
		{Raiz: filepath.Join(home, ".barroso-harness", "skills"), Rotulo: "global"},
	}
	for i, f := range quer {
		if fontes[i] != f {
			t.Fatalf("fonte %d devia ser %+v, veio %+v", i, f, fontes[i])
		}
	}
}

func TestFontesPadraoStringsVazias(t *testing.T) {
	if f := FontesPadrao("", ""); len(f) != 0 {
		t.Fatalf("sem home e sem cwd não há fonte, vieram %d", len(f))
	}
	if f := FontesPadrao("", filepath.Join("so", "cwd")); len(f) != 2 {
		t.Fatalf("sem home sobram só as de projeto, vieram %d", len(f))
	}
	if f := FontesPadrao(filepath.Join("so", "home"), ""); len(f) != 1 || f[0].Rotulo != "global" {
		t.Fatalf("sem cwd sobra só a global, vieram %+v", f)
	}
}

// ── tools ─────────────────────────────────────────────────────────────────

func TestSkillsListCatalogaNumerado(t *testing.T) {
	raiz := t.TempDir()
	skillDir(t, raiz, "beta", "---\nname: beta\ndescription: faz b\n---", "corpo b")
	skillDir(t, raiz, "alpha", "---\nname: alpha\ndescription: faz a\n---", "corpo a")

	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	specs := Especificacoes(fora)
	if len(specs) != 2 {
		t.Fatalf("eram esperadas 2 specs, vieram %d", len(specs))
	}
	if specs[0].Name != "skills_list" || specs[1].Name != "skill_read" {
		t.Fatalf("specs fora de ordem: %s, %s", specs[0].Name, specs[1].Name)
	}

	out, err := chamar(t, "skills_list", specs, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, nome := range []string{"alpha", "beta"} {
		if !strings.Contains(out, nome) {
			t.Fatalf("catálogo não trouxe %s:\n%s", nome, out)
		}
	}
	// numerado e ordenado
	if !strings.Contains(out, "1. alpha") || !strings.Contains(out, "2. beta") {
		t.Fatalf("catálogo devia sair numerado e ordenado:\n%s", out)
	}
	if !strings.Contains(out, "skill_read") {
		t.Fatalf("catálogo devia mandar chamar skill_read:\n%s", out)
	}
}

func TestSkillsListVazio(t *testing.T) {
	out, err := chamar(t, "skills_list", Especificacoes(nil), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nenhuma skill") {
		t.Fatalf("catálogo vazio devia dizer isso, veio %q", out)
	}
}

func TestSkillReadDevolveCorpoExato(t *testing.T) {
	raiz := t.TempDir()
	corpo := "# Título\n\nPasso 1\nPasso 2\n"
	skillDir(t, raiz, "exata", "---\nname: exata\ndescription: d\n---", corpo)

	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	specs := Especificacoes(fora)
	out, err := chamar(t, "skill_read", specs, map[string]any{"name": "exata"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, corpo) {
		t.Fatalf("corpo exato devia fechar a saída:\nesperado no fim: %q\nveio: %q", corpo, out)
	}
	if !strings.HasPrefix(out, "# exata\n\n") {
		t.Fatalf("saída devia abrir com o nome da skill, veio %q", out)
	}
}

func TestSkillReadNomeDesconhecidoErraEmUmaLinha(t *testing.T) {
	raiz := t.TempDir()
	skillDir(t, raiz, "unica", "---\nname: unica\n---", "corpo")

	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	specs := Especificacoes(fora)

	out, err := chamar(t, "skill_read", specs, map[string]any{"name": "nao-existe"})
	if err == nil {
		t.Fatalf("nome desconhecido devia errar, saiu %q", out)
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") {
		t.Fatalf("erro devia ser de uma linha, veio %q", msg)
	}
	if msg != "skill nao-existe não encontrada; use skills_list" {
		t.Fatalf("mensagem inesperada: %q", msg)
	}

	// nome vazio cai no mesmo one-liner
	if _, err := chamar(t, "skill_read", specs, map[string]any{"name": "  "}); err == nil ||
		strings.Contains(err.Error(), "\n") {
		t.Fatalf("nome vazio devia errar em uma linha, veio %v", err)
	}
}

func TestSkillReadCortaCorpoGrande(t *testing.T) {
	raiz := t.TempDir()
	corpo := strings.Repeat("x", tetoCorpo+4096)
	skillDir(t, raiz, "grande", "---\nname: grande\n---", corpo)

	fora, err := Descobrir([]Fonte{{Raiz: raiz, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := chamar(t, "skill_read", Especificacoes(fora), map[string]any{"name": "grande"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "corpo cortado") {
		t.Fatalf("corpo acima do teto devia sair cortado com marcador:\n%s", out[:200])
	}
	if len(out) >= len("# grande\n\n")+len(corpo) {
		t.Fatalf("saída deveria ser menor que o corpo íntegro (%d vs %d)", len(out), len(corpo))
	}
}
