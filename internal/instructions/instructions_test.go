package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateHome points the global directory at a temp dir: tests must not
// depend on the machine's real BARROSO.md.
func isolateHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", h)
	return h
}

func TestResolveWithoutFileReturnsEmpty(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	s, err := Resolve(dir)
	if err != nil {
		t.Fatalf("without BARROSO.md it must return an empty string, not an error: %v", err)
	}
	if s != "" {
		t.Fatalf("expected empty, got %q", s)
	}
}

func TestResolveChildBeforeParent(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "proj", "sub")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	parentMD := "parent rule"
	childMD := "child rule"
	if err := os.WriteFile(filepath.Join(parent, "BARROSO.md"), []byte(parentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "BARROSO.md"), []byte(childMD), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(child)
	if err != nil {
		t.Fatal(err)
	}
	if iChild, iParent := strings.Index(got, childMD), strings.Index(got, parentMD); iChild > iParent {
		t.Fatalf("expected child before parent, got:\n%s", got)
	}
}

func TestResolveEmptyFileIsSkipped(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "BARROSO.md"), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s != "" {
		t.Fatalf("a blank BARROSO.md must not enter the instruction, got %q", s)
	}
}
