package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

const (
	// tetoCorpo limita o corpo devolvido por skill_read. O catálogo inteiro de
	// skills é pontual, então o guardrail opera aqui e não no runtime.
	tetoCorpo = 48 << 10

	// descobertaFonte é o rótulo genérico quando a skill veio de um
	// Descobrir que não rotulou a fonte.
	descobertaFonte = "desconhecida"
)

// Especificacoes devolve as duas tools de skills: skills_list (catálogo) e
// skill_read (corpo inteiro de uma skill). Ambas compartilham o catálogo
// passado; uma lista vazia devolve as specs com mensagens de "sem skills".
func Especificacoes(hab []Skill) []tools.Spec {
	return []tools.Spec{
		specSkillsList(hab),
		specSkillRead(hab),
	}
}

// ── skills_list ───────────────────────────────────────────────────────────

func specSkillsList(hab []Skill) tools.Spec {
	return tools.Spec{
		Name: "skills_list",
		Description: "Lista as skills disponíveis (nome, descrição e origem), numeradas. " +
			"Para usar uma, chame skill_read com o nome exato — o corpo contém as instruções completas.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return catalogo(hab), nil
		},
	}
}

// catalogo monta as linhas numeradas do catálogo, na ordem já ordenada.
func catalogo(hab []Skill) string {
	if len(hab) == 0 {
		return "nenhuma skill disponível neste projeto."
	}
	var sb strings.Builder
	for i, s := range hab {
		fmt.Fprintf(&sb, "%d. %s — %s (%s)\n", i+1, s.Nome, descricaoCurta(s), fonteCurta(s))
	}
	sb.WriteString("\nUse skill_read com o nome exato para carregar as instruções completas.")
	return strings.TrimRight(sb.String(), "\n")
}

// descricaoCurta evita descrição vazia e achata a primeira linha.
func descricaoCurta(s Skill) string {
	d := strings.TrimSpace(primeiraLinha(s.Descricao))
	if d == "" {
		return "(sem descrição)"
	}
	if r := []rune(d); len(r) > 120 {
		d = string(r[:120]) + "…"
	}
	return d
}

// fonteCurta é o caminho impresso no catálogo: o diretório que contém o
// SKILL.md — um nível, legível e suficiente para achar a skill no disco.
func fonteCurta(s Skill) string {
	if s.Caminho == "" {
		if s.Fontes == "" {
			return descobertaFonte
		}
		return s.Fontes
	}
	dir := filepath.Dir(filepath.Clean(s.Caminho))
	if dir == "" || dir == "." {
		return s.Caminho
	}
	if s.Fontes != "" {
		return dir + " [" + s.Fontes + "]"
	}
	return dir
}

// primeiraLinha devolve a primeira linha não vazia do texto.
func primeiraLinha(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// ── skill_read ────────────────────────────────────────────────────────────

func specSkillRead(hab []Skill) tools.Spec {
	return tools.Spec{
		Name: "skill_read",
		Description: "Devolve o conteúdo completo (o markdown depois do frontmatter) de uma skill " +
			"pelo nome exato que aparece em skills_list. Siga as instruções recebidas.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "nome exato da skill, como em skills_list"},
			},
			"required": []string{"name"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			nome := strings.TrimSpace(c.Name)
			if nome == "" {
				return "", fmt.Errorf("skill não encontrada; use skills_list")
			}
			for _, s := range hab {
				if s.Nome != nome {
					continue
				}
				corpo := s.Corpo
				if len(corpo) > tetoCorpo {
					corpo = corpo[:tetoCorpo] + fmt.Sprintf(
						"\n\n[corpo cortado em %d KiB de %d; o resto está em %s]", tetoCorpo>>10, len(s.Corpo)>>10, s.Caminho)
				}
				return "# " + s.Nome + "\n\n" + corpo, nil
			}
			return "", fmt.Errorf("skill %s não encontrada; use skills_list", nome)
		},
	}
}
