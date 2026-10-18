package skills

import (
	"fmt"
	"os"
)

// Carregar lê o SKILL.md em `caminho` e devolve a skill. Erra se o arquivo não
// existe ou se o corpo (depois do frontmatter) está vazio.
func Carregar(caminho string) (*Skill, error) {
	s, ok, err := carregarDe(caminho, "")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s: SKILL.md sem corpo", caminho)
	}
	return &s, nil
}

// carregarDe lê e analisa um SKILL.md; ok=false quando o corpo fica vazio.
func carregarDe(caminho, fonte string) (Skill, bool, error) {
	b, err := os.ReadFile(caminho)
	if err != nil {
		return Skill{}, false, err
	}
	s, ok := analisar(string(b), caminho, fonte)
	return s, ok, nil
}
