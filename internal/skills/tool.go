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
	// bodyCap limits the body returned by skill. The skills catalog as a
	// whole is small, so the guardrail lives here and not in the runtime.
	bodyCap = 48 << 10

	// unknownSource is the generic label when the skill came from a Descobrir
	// call that did not label the source.
	unknownSource = "unknown"
)

// Especificacoes returns the two skills tools: skills (catalog) and
// skill (full body of one skill). Both share the given catalog; an empty
// list yields specs that report "no skills".
func Especificacoes(list []Skill) []tools.Spec {
	return []tools.Spec{
		specSkillsList(list),
		specSkillRead(list),
	}
}

// ── skills ───────────────────────────────────────────────────────────

func specSkillsList(list []Skill) tools.Spec {
	return tools.Spec{
		Name: "skills",
		Description: "Lists the available skills (name, description and origin), numbered. " +
			"To use one, call skill with its exact id (shown below) — the body contains the full instructions.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return catalog(list), nil
		},
	}
}

// catalog builds the numbered catalog lines, in the already sorted order.
func catalog(list []Skill) string {
	if len(list) == 0 {
		return "no skills available in this project."
	}
	var sb strings.Builder
	for i, s := range list {
		fmt.Fprintf(&sb, "%d. %s — %s (%s)\n", i+1, s.Nome, shortDescription(s), shortSource(s))
	}
	sb.WriteString("\nUse skill with the exact id (shown above) to load the full instructions.")
	return strings.TrimRight(sb.String(), "\n")
}

// shortDescription avoids an empty description and flattens the first line.
func shortDescription(s Skill) string {
	d := strings.TrimSpace(firstLine(s.Descricao))
	if d == "" {
		return "(no description)"
	}
	if r := []rune(d); len(r) > 120 {
		d = string(r[:120]) + "…"
	}
	return d
}

// shortSource is the path printed in the catalog: the directory containing the
// SKILL.md — one level, readable and enough to find the skill on disk.
func shortSource(s Skill) string {
	if s.Caminho == "" {
		if s.Fontes == "" {
			return unknownSource
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

// firstLine returns the first non-empty line of the text.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// ── skill ────────────────────────────────────────────────────────────

func specSkillRead(list []Skill) tools.Spec {
	return tools.Spec{
		Name: "skill",
		Description: "Load a specialized skill's instructions into the conversation when the task at hand " +
			"matches its description. Returns the full content (the markdown after the frontmatter) of " +
			"a skill by the exact id shown in skills. Follow the instructions you receive.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "skill id (its name), as shown in skills"},
			},
			"required": []string{"id"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name string `json:"id"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			name := strings.TrimSpace(c.Name)
			if name == "" {
				return "", fmt.Errorf("skill not found; use skills")
			}
			for _, s := range list {
				if s.Nome != name {
					continue
				}
				body := s.Corpo
				if len(body) > bodyCap {
					body = body[:bodyCap] + fmt.Sprintf(
						"\n\n[body truncated at %d KiB of %d; the rest is in %s]", bodyCap>>10, len(s.Corpo)>>10, s.Caminho)
				}
				return "# " + s.Nome + "\n\n" + body, nil
			}
			return "", fmt.Errorf("skill %s not found; use skills", name)
		},
	}
}
