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

// Skill management by the agent itself: skill_create, skill_edit and
// skill_remove. Previously the model could only READ skills someone had
// written to disk.

// validName: the name becomes a directory and a command (/<name>), so only
// letters, digits, hyphen and underscore — no path separators.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// EspecificacoesGestao returns skill_create, skill_edit and skill_remove.
// roots maps the scope ("projeto" | "global") to that scope's skills
// directory.
func EspecificacoesGestao(roots map[string]string) []tools.Spec {
	return []tools.Spec{specSkillCreate(roots), specSkillEdit(roots), specSkillRemove(roots)}
}

// scopeEnum is what the tools accept; "project" is the English name of the
// internal "projeto" key.
var scopeEnum = []string{"project", "global"}

// The scope values "projeto"/"global" are a data contract shared with
// internal/runtime (map keys and source labels), so they stay as-is; the
// tools accept "project" for "projeto".
func rootFor(roots map[string]string, scope string) (string, string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" || scope == "project" {
		scope = "projeto"
	}
	r := strings.TrimSpace(roots[scope])
	if r == "" {
		return "", "", fmt.Errorf("invalid scope %q: use project or global", scope)
	}
	return r, scope, nil
}

func specSkillCreate(roots map[string]string) tools.Spec {
	return tools.Spec{
		Name: "skill_create",
		Description: "Creates a NEW skill: writes <scope>/<name>/SKILL.md with frontmatter. " +
			"The body is the instructions the agent follows when running it; the user invokes it with /<name>. " +
			"Scope project (default, .barroso/skills in the current directory) or global (~/.barroso-harness/skills). " +
			"Fails if the skill already exists: change an existing one with skill_edit.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "letters, digits, - and _"},
				"description": map[string]any{"type": "string", "description": "one line: when to use it"},
				"body":        map[string]any{"type": "string", "description": "instructions in markdown"},
				"scope":       map[string]any{"type": "string", "enum": scopeEnum},
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
			content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body + "\n"
			path := filepath.Join(dir, fileName)
			if _, err := os.Stat(path); err == nil {
				return "", fmt.Errorf("skill %q already exists (%s) at %s: use skill_edit to change it", name, scope, path)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
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
				"scope": map[string]any{"type": "string", "enum": scopeEnum},
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

func specSkillEdit(roots map[string]string) tools.Spec {
	return tools.Spec{
		Name: "skill_edit",
		Description: "Edits an existing skill (<scope>/<name>/SKILL.md) in place. Give only what changes: " +
			"description (new one-line description); oldString + newString (replace one exact, unique passage of the body, like the edit tool); " +
			"body (replace the whole body); new_name (rename: the command becomes /<new_name>); new_scope (move between project and global). " +
			"The change applies to every open session on their next request.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "current skill name"},
				"scope":       map[string]any{"type": "string", "enum": scopeEnum, "description": "where it is now (default project)"},
				"description": map[string]any{"type": "string", "description": "new one-line description"},
				"oldString":  map[string]any{"type": "string", "description": "exact passage of the body to replace (must be unique)"},
				"newString":  map[string]any{"type": "string", "description": "replacement for oldString"},
				"body":        map[string]any{"type": "string", "description": "new full body (instead of oldString/newString)"},
				"new_name":    map[string]any{"type": "string", "description": "rename to this name"},
				"new_scope":   map[string]any{"type": "string", "enum": scopeEnum, "description": "move to this scope"},
			},
			"required": []string{"name"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name        string  `json:"name"`
				Scope       string  `json:"scope"`
				Description *string `json:"description"`
				OldString   string  `json:"oldString"`
				NewString   *string `json:"newString"`
				Body        *string `json:"body"`
				NewName     string  `json:"new_name"`
				NewScope    string  `json:"new_scope"`
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
			path := filepath.Join(root, name, fileName)
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("skill %q does not exist in scope %s (create it with skill_create)", name, scope)
			}
			_, desc, body := splitFrontmatter(string(data))
			var changed []string
			if c.Description != nil {
				desc = strings.Join(strings.Fields(*c.Description), " ")
				changed = append(changed, "description")
			}
			switch {
			case c.Body != nil && c.OldString != "":
				return "", fmt.Errorf("give either body or oldString/newString, not both")
			case c.Body != nil:
				if strings.TrimSpace(*c.Body) == "" {
					return "", fmt.Errorf("empty body: the skill needs instructions")
				}
				body = *c.Body
				changed = append(changed, "body")
			case c.OldString != "":
				if c.NewString == nil {
					return "", fmt.Errorf("oldString needs newString")
				}
				switch n := strings.Count(body, c.OldString); n {
				case 0:
					return "", fmt.Errorf("oldString not found in the body of %q; read it with skill", name)
				case 1:
					body = strings.Replace(body, c.OldString, *c.NewString, 1)
				default:
					return "", fmt.Errorf("oldString occurs %d times in the body: include more context to make it unique", n)
				}
				changed = append(changed, "body passage")
			}
			newName := name
			if nn := strings.TrimSpace(c.NewName); nn != "" && nn != name {
				if !validName.MatchString(nn) {
					return "", fmt.Errorf("invalid new_name %q: only letters, digits, - and _ (up to 64)", nn)
				}
				newName = nn
				changed = append(changed, "name")
			}
			newRoot, newScope := root, scope
			if strings.TrimSpace(c.NewScope) != "" {
				if newRoot, newScope, err = rootFor(roots, c.NewScope); err != nil {
					return "", err
				}
				if newScope != scope {
					changed = append(changed, "scope")
				}
			}
			if len(changed) == 0 {
				return "", fmt.Errorf("nothing to change: give description, body, oldString/newString, new_name or new_scope")
			}
			dest := filepath.Join(newRoot, newName, fileName)
			if dest != path {
				if _, err := os.Stat(dest); err == nil {
					return "", fmt.Errorf("a skill already exists at %s", dest)
				}
			}
			content := "---\nname: " + newName + "\ndescription: " + desc + "\n---\n" + strings.TrimSpace(body) + "\n"
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
				return "", err
			}
			if dest != path {
				if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("skill %q edited (%s) in %s scope: %s", newName, strings.Join(changed, ", "), newScope, dest), nil
		},
	}
}
