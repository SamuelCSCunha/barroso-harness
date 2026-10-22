package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSpec(t *testing.T, name string, roots map[string]string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	for _, s := range EspecificacoesGestao(roots) {
		if s.Name == name {
			return s.Exec(context.Background(), "", raw)
		}
	}
	t.Fatalf("no spec %s", name)
	return "", nil
}

// create refuses an existing skill; edit changes a passage, renames and moves
// it between scopes.
func TestSkillCreateEditRemove(t *testing.T) {
	dir := t.TempDir()
	roots := map[string]string{"projeto": filepath.Join(dir, "p"), "global": filepath.Join(dir, "g")}
	if _, err := runSpec(t, "skill_create", roots, map[string]any{"name": "deploy", "description": "d", "body": "step one\nstep two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runSpec(t, "skill_create", roots, map[string]any{"name": "deploy", "description": "d", "body": "x"}); err == nil || !strings.Contains(err.Error(), "skill_edit") {
		t.Fatalf("create over an existing skill must point to skill_edit, got %v", err)
	}
	if _, err := runSpec(t, "skill_edit", roots, map[string]any{"name": "deploy", "oldString": "step two", "newString": "step 2", "description": "new desc"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(roots["projeto"], "deploy", fileName))
	if !strings.Contains(string(b), "step 2") || !strings.Contains(string(b), "description: new desc") {
		t.Fatalf("edit not applied:\n%s", b)
	}
	if _, err := runSpec(t, "skill_edit", roots, map[string]any{"name": "deploy", "new_name": "ship", "new_scope": "global"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(roots["global"], "ship", fileName)); err != nil {
		t.Fatal("rename+move did not land in global/ship")
	}
	if _, err := os.Stat(filepath.Join(roots["projeto"], "deploy")); err == nil {
		t.Fatal("old directory must be gone")
	}
	if _, err := runSpec(t, "skill_remove", roots, map[string]any{"name": "ship", "scope": "global"}); err != nil {
		t.Fatal(err)
	}
}
