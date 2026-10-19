package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execManagement(t *testing.T, roots map[string]string, name string, args map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	for _, s := range EspecificacoesGestao(roots) {
		if s.Name == name {
			return s.Exec(context.Background(), "", b)
		}
	}
	t.Fatalf("spec %s missing", name)
	return "", nil
}

func TestSkillCreateWritesAndIsDiscovered(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	r := map[string]string{"projeto": root, "global": t.TempDir()}
	out, err := execManagement(t, r, "skill_create", map[string]any{"name": "review", "description": "reviews\ncode", "body": "1. read\n2. point out"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/review") {
		t.Errorf("output: %q", out)
	}
	list, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil || len(list) != 1 || list[0].Nome != "review" || list[0].Descricao != "reviews code" || !strings.Contains(list[0].Corpo, "2. point out") {
		t.Fatalf("created skill is not discovered correctly: %+v %v", list, err)
	}
	if _, err := execManagement(t, r, "skill_remove", map[string]any{"name": "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "review")); !os.IsNotExist(err) {
		t.Fatal("directory should have been removed")
	}
}

func TestSkillCreateRejectsDangerousNameAndBadScope(t *testing.T) {
	r := map[string]string{"projeto": t.TempDir()}
	for _, n := range []string{"../outside", "a/b", "", "with space"} {
		if _, err := execManagement(t, r, "skill_create", map[string]any{"name": n, "description": "d", "body": "b"}); err == nil {
			t.Errorf("name %q should be rejected", n)
		}
	}
	if _, err := execManagement(t, r, "skill_create", map[string]any{"name": "x", "description": "d", "body": "b", "scope": "global"}); err == nil {
		t.Error("scope without root should fail")
	}
	if _, err := execManagement(t, r, "skill_remove", map[string]any{"name": "missing"}); err == nil {
		t.Error("removing a nonexistent skill should fail")
	}
}
