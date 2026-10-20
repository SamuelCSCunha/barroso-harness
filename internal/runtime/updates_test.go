package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A change made after the session's first request (by any process) comes
// back once as a [system update]; nothing changed means no update.
func TestPendingUpdate(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	cwd := t.TempDir()
	s := &Session{}
	if u := s.pendingUpdate(cwd); u != "" {
		t.Fatalf("baseline must be silent, got %q", u)
	}
	if err := os.WriteFile(filepath.Join(cwd, "BARROSO.md"), []byte("use tabs"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, ".barroso", "skills", "ship")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: ship\ndescription: release it\n---\nsteps"), 0o644)
	s.Observar(func() []string { return []string{"MCP server \"x\" added"} })
	u := s.pendingUpdate(cwd)
	for _, want := range []string{UpdatePrefix, "use tabs", "skill ship was created", "MCP server \"x\" added"} {
		if !strings.Contains(u, want) {
			t.Fatalf("update lacks %q:\n%s", want, u)
		}
	}
	s.watchers = nil
	if u := s.pendingUpdate(cwd); u != "" {
		t.Fatalf("second check must be silent, got %q", u)
	}
	_ = os.Remove(filepath.Join(cwd, "BARROSO.md"))
	if u := s.pendingUpdate(cwd); !strings.Contains(u, "BARROSO.md was deleted") {
		t.Fatalf("delete not reported: %q", u)
	}
}
