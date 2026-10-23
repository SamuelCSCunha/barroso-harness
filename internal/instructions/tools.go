package instructions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// BARROSO.md management by the agent: read, write, edit and delete, in the
// global scope (next to config.json) or the project scope (the nearest
// BARROSO.md from the working directory up, or a new one in the working
// directory). Changes apply to every open session on its next request.

var scopeEnum = []string{"project", "global"}

// pathFor resolves the file of a scope. For project it is the nearest existing
// BARROSO.md from cwd upward (the one that already applies), else cwd's.
func pathFor(cwd, scope string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", "project":
		dir := cwd
		for {
			p := filepath.Join(dir, "BARROSO.md")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		return filepath.Join(cwd, "BARROSO.md"), nil
	case "global":
		return filepath.Join(config.Dir(), "BARROSO.md"), nil
	}
	return "", fmt.Errorf("invalid scope %q: use project or global", scope)
}

// Especificacoes returns the instructions_* tools.
func Especificacoes() []tools.Spec {
	scopeProp := map[string]any{"type": "string", "enum": scopeEnum, "description": "project (default) or global"}
	parse := func(raw json.RawMessage, v any) error {
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("invalid input: %w", err)
		}
		return nil
	}
	return []tools.Spec{
		{
			Name: "instructions_read",
			Description: "Shows the BARROSO.md files that apply here (system instructions), in precedence order, with their paths. " +
				"Use it before editing them.",
			Schema: map[string]any{"type": "object", "properties": map[string]any{}},
			Exec: func(_ context.Context, cwd string, _ json.RawMessage) (string, error) {
				var sb strings.Builder
				for _, scope := range scopeEnum {
					p, _ := pathFor(cwd, scope)
					b, err := os.ReadFile(p)
					if err != nil {
						fmt.Fprintf(&sb, "[%s] %s — does not exist\n\n", scope, p)
						continue
					}
					fmt.Fprintf(&sb, "[%s] %s\n%s\n\n", scope, p, strings.TrimSpace(string(b)))
				}
				return strings.TrimSpace(sb.String()), nil
			},
		},
		{
			Name: "instructions_write",
			Description: "Creates or fully rewrites a BARROSO.md (system instructions for every session). " +
				"For a small change prefer instructions_edit. Applies to every open session on its next request.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope":   scopeProp,
					"content": map[string]any{"type": "string", "description": "full markdown content"},
				},
				"required": []string{"content"},
			},
			Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
				var c struct {
					Scope   string `json:"scope"`
					Content string `json:"content"`
				}
				if err := parse(raw, &c); err != nil {
					return "", err
				}
				if strings.TrimSpace(c.Content) == "" {
					return "", fmt.Errorf("empty content: use instructions_delete to remove the file")
				}
				p, err := pathFor(cwd, c.Scope)
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(p, []byte(strings.TrimSpace(c.Content)+"\n"), 0o644); err != nil {
					return "", err
				}
				return "wrote " + p, nil
			},
		},
		{
			Name: "instructions_edit",
			Description: "Replaces one exact, unique passage of a BARROSO.md (like the edit tool). " +
				"Read it first with instructions_read. Applies to every open session on its next request.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope":     scopeProp,
					"oldString": map[string]any{"type": "string", "description": "exact passage to replace (must be unique)"},
					"newString": map[string]any{"type": "string", "description": "replacement"},
				},
				"required": []string{"oldString", "newString"},
			},
			Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
				var c struct {
					Scope     string `json:"scope"`
					OldString string `json:"oldString"`
					NewString string `json:"newString"`
				}
				if err := parse(raw, &c); err != nil {
					return "", err
				}
				p, err := pathFor(cwd, c.Scope)
				if err != nil {
					return "", err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return "", fmt.Errorf("%s does not exist: create it with instructions_write", p)
				}
				text := string(b)
				switch n := strings.Count(text, c.OldString); {
				case c.OldString == "":
					return "", fmt.Errorf("oldString is empty")
				case n == 0:
					return "", fmt.Errorf("oldString not found in %s; read it with instructions_read", p)
				case n > 1:
					return "", fmt.Errorf("oldString occurs %d times in %s: include more context", n, p)
				}
				text = strings.Replace(text, c.OldString, c.NewString, 1)
				if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
					return "", err
				}
				return "edited " + p, nil
			},
		},
		{
			Name:        "instructions_delete",
			Description: "Deletes a BARROSO.md. Applies to every open session on its next request.",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"scope": scopeProp},
			},
			Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
				var c struct {
					Scope string `json:"scope"`
				}
				if err := parse(raw, &c); err != nil {
					return "", err
				}
				p, err := pathFor(cwd, c.Scope)
				if err != nil {
					return "", err
				}
				if err := os.Remove(p); err != nil {
					if os.IsNotExist(err) {
						return "", fmt.Errorf("%s does not exist", p)
					}
					return "", err
				}
				return "deleted " + p, nil
			},
		},
	}
}
