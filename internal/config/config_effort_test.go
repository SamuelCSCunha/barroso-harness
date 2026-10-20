package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config without "effort" gets DefaultEffort; an explicit value (including
// "none") is kept.
func TestDefaultEffort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", dir)
	p := filepath.Join(dir, "config.json")
	_ = os.WriteFile(p, []byte(`{"model":"m"}`), 0o644)
	if c, _ := Load(); c.Effort != DefaultEffort {
		t.Fatalf("absent effort: %q", c.Effort)
	}
	_ = os.WriteFile(p, []byte(`{"model":"m","effort":"none"}`), 0o644)
	if c, _ := Load(); c.Effort != "none" {
		t.Fatalf("explicit none: %q", c.Effort)
	}
}
