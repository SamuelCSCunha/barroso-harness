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

// Skill management by the agent itself: skill_create and skill_remove.
// Previously the model could only READ skills someone had written to disk.

// validName: the name becomes a directory and a command (/<name>), so only
// letters, digits, hyphen and underscore — no path separators.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// EspecificacoesGestao returns skill_create and skill_remove. roots maps the
// scope ("projeto" | "global") to that scope's skills directory.
func EspecificacoesGestao(roots map[string]string) []tools.Spec {
	return []tools.Spec{specSkillCreate(roots), specSkillRemove(roots)}
}

// The scope values "projeto"/"global" are a data contract shared with
// internal/runtime (map keys and source labels), so they stay as-is.
func rootFor(roots map[string]string, scope string) (string, string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = "projeto"
	}
	r := strings.TrimSpace(roots[scope])
	if r == "" {
		return "", "", fmt.Errorf("invalid scope %q: use projeto or global", scope)
	}
	return r, scope, nil
}

func specSkillCreate(roots map[string]string) tools.Spec {
	return tools.Spec{
		Name: "skill_create",
		Description: "Creates (or replaces) a skill: writes <scope>/<name>/SKILL.md with frontmatter. " +
			"The body is the instructions the agent follows when running it; the user invokes it with /<name>. " +
			"Scope projeto (default, .barroso/skills in the current directory) or global (~/.barroso-harness/skills).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "letters, digits, - and _"},
				"description": map[string]any{"type": "string", "description": "one line: when to use it"},
				"body":        map[string]any{"type": "string", "description": "instructions in markdown"},
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
				return "", fmt.Errorf("invalid input: %w", err)
			}
			name := strings.TrimSpace(c.Name)
			if !validName.MatchString(name) {
				return "", fmt.Errorf("invalid name %q: only letters, digits, - and _ (up to 64)", name)
			}
			body := strings.TrimSpace(c.Body)
			if body == "" {
				return "", fmt.Errorf("empty body: the skill needs instructions")
			}
			root, scope, err := rootFor(roots, c.Scope)
			if err != nil {
				return "", err
			}
			desc := strings.Join(strings.Fields(c.Description), " ")
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body + "\n"
			path := filepath.Join(dir, fileName)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("skill %q created (%s) at %s; invoke it with /%s", name, scope, path, name), nil
		},
	}
}

func specSkillRemove(roots map[string]string) tools.Spec {
	return tools.Spec{
		Name:        "skill_remove",
		Description: "Removes a skill created as <scope>/<name>/SKILL.md (deletes its directory).",
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
				return "", fmt.Errorf("invalid input: %w", err)
			}
			name := strings.TrimSpace(c.Name)
			if !validName.MatchString(name) {
				return "", fmt.Errorf("invalid name %q", name)
			}
			root, scope, err := rootFor(roots, c.Scope)
			if err != nil {
				return "", err
			}
			dir := filepath.Join(root, name)
			if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
				return "", fmt.Errorf("skill %q does not exist in scope %s", name, scope)
			}
			if err := os.RemoveAll(dir); err != nil {
				return "", err
			}
			return fmt.Sprintf("skill %q removed (%s)", name, scope), nil
		},
	}
}
