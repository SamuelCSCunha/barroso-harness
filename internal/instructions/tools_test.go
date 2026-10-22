package instructions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, cwd, name string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	for _, s := range Especificacoes() {
		if s.Name == name {
			return s.Exec(context.Background(), cwd, raw)
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestInstructionsTools(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := call(t, cwd, "instructions_write", map[string]any{"content": "rule one\nrule two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, cwd, "instructions_edit", map[string]any{"oldString": "rule two", "newString": "rule 2"}); err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, cwd, "instructions_read", nil)
	if !strings.Contains(out, "rule 2") || !strings.Contains(out, "[global]") {
		t.Fatalf("read:\n%s", out)
	}
	if _, err := call(t, cwd, "instructions_write", map[string]any{"scope": "global", "content": "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, cwd, "instructions_delete", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "BARROSO.md")); err == nil {
		t.Fatal("project BARROSO.md must be gone")
	}
}
