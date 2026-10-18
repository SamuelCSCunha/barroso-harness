// Package skills resolve as skills do agente a partir do disco. Uma skill é um
// diretório contendo um SKILL.md (forma preferida, o nome do diretório batiza
// a skill) ou um SKILL.md solto na raiz de uma fonte (aí o nome vem do
// frontmatter). O frontmatter é o primeiro bloco delimitado por linhas `---`;
// só linhas `key: value` simples são lidas — YAML aninhado, listas e blocos
// não são interpretados.
package skills

import (
	"os"
	"path/filepath"
	"strings"
)

// nomeArquivo é o nome fixo do arquivo que materializa uma skill.
const nomeArquivo = "SKILL.md"

// Skill é uma skill já lida do disco, pronta para entrar no catálogo.
type Skill struct {
	Nome      string // frontmatter `name`; sem ele, o nome do diretório/arquivo
	Descricao string // frontmatter `description`
	Caminho   string // caminho absoluto do SKILL.md
	Corpo     string // o markdown DEPOIS do frontmatter, verbatim
	Fontes    string // rótulo da fonte de onde veio ("global"|"projeto")
}

// analisar separa frontmatter de corpo e devolve a skill. ok=false quando o
// corpo fica vazio — skill sem conteúdo não entra no catálogo.
func analisar(conteudo, caminho, fonte string) (Skill, bool) {
	nome, descricao, corpo := separarFrontmatter(conteudo)
	if strings.TrimSpace(nome) == "" {
		nome = nomeDoDisco(caminho)
	}
	s := Skill{
		Nome:      nome,
		Descricao: descricao,
		Caminho:   caminho,
		Corpo:     corpo,
		Fontes:    fonte,
	}
	if strings.TrimSpace(corpo) == "" {
		return s, false
	}
	return s, true
}

// separarFrontmatter devolve name, description e o corpo. Sem frontmatter (ou
// com o bloco não fechado) o arquivo inteiro é o corpo e o nome vem do disco.
// O corpo começa na primeira linha depois do `---` de fechamento: a quebra de
// linha que encerra o bloco é consumida, o resto é preservado verbatim.
func separarFrontmatter(conteudo string) (nome, descricao, corpo string) {
	linhas := strings.Split(conteudo, "\n")

	abre := -1
	for i, l := range linhas {
		t := semCR(l)
		if t == "" {
			continue
		}
		if t == "---" {
			abre = i
		}
		break
	}
	if abre < 0 {
		return "", "", conteudo
	}

	fecha := -1
	for i := abre + 1; i < len(linhas); i++ {
		if semCR(linhas[i]) == "---" {
			fecha = i
			break
		}
	}
	if fecha < 0 {
		return "", "", conteudo
	}

	for i := abre + 1; i < fecha; i++ {
		l := strings.TrimSpace(semCR(linhas[i]))
		chave, valor, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(chave) {
		case "name":
			nome = desaspas(strings.TrimSpace(valor))
		case "description":
			descricao = desaspas(strings.TrimSpace(valor))
		}
	}

	if fecha+1 < len(linhas) {
		corpo = strings.Join(linhas[fecha+1:], "\n")
	}
	return nome, descricao, corpo
}

// semCR tira o \r que sobra da quebra de linha em arquivos CRLF.
func semCR(l string) string { return strings.TrimSuffix(l, "\r") }

// desaspas remove aspas (duplas ou simples) envolvendo o valor inteiro.
func desaspas(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// nomeDoDisco deriva o nome pelo caminho: o diretório pai quando o arquivo é o
// SKILL.md canônico (`<raiz>/<skill>/SKILL.md` → `<skill>`); senão, o nome do
// arquivo sem extensão.
func nomeDoDisco(caminho string) string {
	limpo := filepath.Clean(caminho)
	base := filepath.Base(limpo)
	if strings.EqualFold(base, nomeArquivo) {
		pai := filepath.Base(filepath.Dir(limpo))
		if pai == "" || pai == "." || pai == string(filepath.Separator) {
			return ""
		}
		return pai
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// acharSkillMD procura o SKILL.md dentro de dir (sem descer um nível). A
// comparação ignora caixa, para conviver com filesystems case-insensitive.
func acharSkillMD(dir string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range ents {
		if e.IsDir() || !strings.EqualFold(e.Name(), nomeArquivo) {
			continue
		}
		return filepath.Join(dir, e.Name()), true
	}
	return "", false
}
