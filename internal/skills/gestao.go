package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Gestão de skills pelo próprio agente: skill_create e skill_remove. Antes o
// modelo só LIA skills que alguém tinha escrito no disco.

// nomeValido: o nome vira diretório e comando (/<nome>), então só letras,
// dígitos, hífen e sublinhado — nada de separador de caminho.
var nomeValido = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// EspecificacoesGestao devolve skill_create e skill_remove. raizes mapeia o
// escopo ("projeto" | "global") para o diretório de skills daquele escopo.
func EspecificacoesGestao(raizes map[string]string) []tools.Spec {
	return []tools.Spec{specSkillCreate(raizes), specSkillRemove(raizes)}
}

func raizDe(raizes map[string]string, escopo string) (string, string, error) {
	escopo = strings.ToLower(strings.TrimSpace(escopo))
	if escopo == "" {
		escopo = "projeto"
	}
	r := strings.TrimSpace(raizes[escopo])
	if r == "" {
		return "", "", fmt.Errorf("escopo %q inválido: use projeto ou global", escopo)
	}
	return r, escopo, nil
}

func specSkillCreate(raizes map[string]string) tools.Spec {
	return tools.Spec{
		Name: "skill_create",
		Description: "Cria (ou substitui) uma skill: grava <escopo>/<name>/SKILL.md com frontmatter. " +
			"O corpo são as instruções que o agente segue ao executá-la; o usuário invoca com /<name>. " +
			"Escopo projeto (padrão, .barroso/skills do diretório atual) ou global (~/.barroso-harness/skills).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "letras, dígitos, - e _"},
				"description": map[string]any{"type": "string", "description": "uma linha: quando usar"},
				"body":        map[string]any{"type": "string", "description": "instruções em markdown"},
				"scope":       map[string]any{"type": "string", "enum": []string{"projeto", "global"}},
			},
			"required": []string{"name", "description", "body"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Body        string `json:"body"`
				Scope       string `json:"scope"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			nome := strings.TrimSpace(c.Name)
			if !nomeValido.MatchString(nome) {
				return "", fmt.Errorf("name %q inválido: só letras, dígitos, - e _ (até 64)", nome)
			}
			corpo := strings.TrimSpace(c.Body)
			if corpo == "" {
				return "", fmt.Errorf("body vazio: a skill precisa de instruções")
			}
			raiz, escopo, err := raizDe(raizes, c.Scope)
			if err != nil {
				return "", err
			}
			desc := strings.Join(strings.Fields(c.Description), " ")
			dir := filepath.Join(raiz, nome)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			conteudo := "---\nname: " + nome + "\ndescription: " + desc + "\n---\n" + corpo + "\n"
			caminho := filepath.Join(dir, nomeArquivo)
			if err := os.WriteFile(caminho, []byte(conteudo), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("skill %q criada (%s) em %s; invoque com /%s", nome, escopo, caminho, nome), nil
		},
	}
}

func specSkillRemove(raizes map[string]string) tools.Spec {
	return tools.Spec{
		Name:        "skill_remove",
		Description: "Remove uma skill criada como <escopo>/<name>/SKILL.md (apaga o diretório dela).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string"},
				"scope": map[string]any{"type": "string", "enum": []string{"projeto", "global"}},
			},
			"required": []string{"name"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name  string `json:"name"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			nome := strings.TrimSpace(c.Name)
			if !nomeValido.MatchString(nome) {
				return "", fmt.Errorf("name %q inválido", nome)
			}
			raiz, escopo, err := raizDe(raizes, c.Scope)
			if err != nil {
				return "", err
			}
			dir := filepath.Join(raiz, nome)
			if _, err := os.Stat(filepath.Join(dir, nomeArquivo)); err != nil {
				return "", fmt.Errorf("skill %q não existe no escopo %s", nome, escopo)
			}
			if err := os.RemoveAll(dir); err != nil {
				return "", err
			}
			return fmt.Sprintf("skill %q removida (%s)", nome, escopo), nil
		},
	}
}
