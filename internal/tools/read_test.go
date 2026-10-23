package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTool(t *testing.T) Spec {
	t.Helper()
	for _, s := range SpecsBuiltins() {
		if s.Name == "read" {
			return s
		}
	}
	t.Fatal("read is not in the catalog")
	return Spec{}
}

// Regression (session bh-20261023110329-0a62): read dumped a PNG as text
// lines, and two reads of it filled a 250k-token window.
func TestReadImageIsAttachedNotDumped(t *testing.T) {
	dir := t.TempDir()
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 4096)...)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := readTool(t).Exec(nil, dir, json.RawMessage(`{"path":"shot.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := ImageAttached + filepath.Join(dir, "shot.png") + "]\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("want prefix %q, got %q", want, out)
	}
	if strings.Contains(out, "PNG\r") || len(out) > 300 {
		t.Fatalf("image bytes leaked into the result: %q", out)
	}
}

func TestReadRefusesOtherBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("ab\x00\x01\x02cd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := readTool(t).Exec(nil, dir, json.RawMessage(`{"path":"a.bin"}`)); err == nil {
		t.Fatalf("binary read should fail, got %q", out)
	}
}

// Text whose 8 KiB sample ends inside a multi-byte rune is still text.
func TestReadTextWithRuneAtSampleEdge(t *testing.T) {
	dir := t.TempDir()
	text := strings.Repeat("a", 8<<10-1) + "é\nsegunda linha\n"
	if err := os.WriteFile(filepath.Join(dir, "t.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := readTool(t).Exec(nil, dir, json.RawMessage(`{"path":"t.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2: segunda linha") {
		t.Fatalf("text file not read as text: %q", out[:80])
	}
}
