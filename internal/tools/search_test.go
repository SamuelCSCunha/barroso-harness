package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func call(t *testing.T, exec func(context.Context, string, json.RawMessage) (string, error), cwd string, args map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	return exec(context.Background(), cwd, b)
}

func grepTree(t *testing.T) string {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a\nfunc Alpha() {}\n// TODO alpha\n")
	writeFile(t, root, "sub/b.go", "package b\nfunc Beta() {}\nfunc alphaHelper() {}\n")
	writeFile(t, root, "sub/c.txt", "Alpha in text\n")
	writeFile(t, root, "bin.dat", "Alpha\x00binary")
	writeFile(t, root, "node_modules/x.go", "func Alpha() {}\n")
	writeFile(t, root, ".git/config", "Alpha\n")
	return root
}

func TestGrepContent(t *testing.T) {
	root := grepTree(t)
	out, err := call(t, execGrep, root, map[string]any{"pattern": `func Alpha`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2: func Alpha() {}") {
		t.Fatalf("missing content match:\n%s", out)
	}
	for _, bad := range []string{"node_modules", ".git", "bin.dat"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%s should be skipped:\n%s", bad, out)
		}
	}
}

func TestGrepSkipsBinary(t *testing.T) {
	root := grepTree(t)
	out, err := call(t, execGrep, root, map[string]any{"pattern": "Alpha", "output": "files"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "bin.dat") {
		t.Fatalf("binary file should be skipped:\n%s", out)
	}
	if !strings.Contains(out, "sub/c.txt") {
		t.Fatalf("text file should match:\n%s", out)
	}
}

func TestGrepFilesSortedAndGlob(t *testing.T) {
	root := grepTree(t)
	out, err := call(t, execGrep, root, map[string]any{"pattern": "alpha", "caseSensitive": false, "include": "*.go", "output": "files"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "a.go\nsub/b.go\n" {
		t.Fatalf("unexpected files output: %q", out)
	}
	out, err = call(t, execGrep, root, map[string]any{"pattern": "Alpha", "include": "sub/**/*.txt", "output": "files"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "sub/c.txt\n" {
		t.Fatalf("path glob: %q", out)
	}
}

func TestGrepCount(t *testing.T) {
	root := grepTree(t)
	out, err := call(t, execGrep, root, map[string]any{"pattern": "(?i)alpha", "glob": "*.go", "output": "count"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go: 2\n") || !strings.Contains(out, "sub/b.go: 1\n") {
		t.Fatalf("unexpected count output:\n%s", out)
	}
}

func TestGrepContextAndNoMatch(t *testing.T) {
	root := grepTree(t)
	out, err := call(t, execGrep, root, map[string]any{"pattern": "TODO", "context": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2- func Alpha() {}") || !strings.Contains(out, "a.go:3: // TODO alpha") {
		t.Fatalf("context lines missing:\n%s", out)
	}
	out, _ = call(t, execGrep, root, map[string]any{"pattern": "zzz_nothing"})
	if out != "no matches" {
		t.Fatalf("got %q", out)
	}
	if _, err := call(t, execGrep, root, map[string]any{"pattern": "("}); err == nil {
		t.Fatal("invalid regexp should fail")
	}
}

func TestGlobDoubleStarAndMtimeOrder(t *testing.T) {
	root := t.TempDir()
	old := writeFile(t, root, "internal/cli/screen.go", "")
	writeFile(t, root, "internal/cli/screen_test.go", "")
	writeFile(t, root, "internal/runtime/session.go", "")
	writeFile(t, root, "main.go", "")
	writeFile(t, root, "vendor/x/y.go", "")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	out, err := call(t, execGlob, root, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "vendor") {
		t.Fatalf("vendor should be skipped:\n%s", out)
	}
	if !strings.Contains(out, "main.go\n") || !strings.Contains(out, "(4 file(s))") {
		t.Fatalf("** should match all depths:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[3] != "internal/cli/screen.go" {
		t.Fatalf("oldest file should come last, got:\n%s", out)
	}

	out, err = call(t, execGlob, root, map[string]any{"pattern": "internal/**/screen*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "internal/cli/screen_test.go\ninternal/cli/screen.go\n") {
		t.Fatalf("unexpected:\n%s", out)
	}
	out, _ = call(t, execGlob, root, map[string]any{"pattern": "*.go"})
	if !strings.HasPrefix(out, "main.go\n(1 file(s))") {
		t.Fatalf("pattern without ** should stay at top level:\n%s", out)
	}
}

func TestEditErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "f.txt", "one\ntwo\none\n")
	if _, err := call(t, execEdit, root, map[string]any{"path": "f.txt", "oldString": "three", "newString": "x"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found, got %v", err)
	}
	_, err := call(t, execEdit, root, map[string]any{"path": "f.txt", "oldString": "one", "newString": "x"})
	if err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestEditReplaceAllAndSingle(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "f.txt", "one\ntwo\none\n")
	out, err := call(t, execEdit, root, map[string]any{"path": "f.txt", "oldString": "one", "newString": "1", "replaceAll": true})
	if err != nil || out != "ok: 2 replacement(s) in f.txt" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, err = call(t, execEdit, root, map[string]any{"path": "f.txt", "oldString": "two\n", "newString": ""})
	if err != nil || out != "ok: 1 replacement(s) in f.txt" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "1\n1\n" {
		t.Fatalf("content %q", b)
	}
}

func TestEditPreservesCRLF(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "w.txt", "alpha\r\nbeta\r\ngamma\r\n")
	_, err := call(t, execEdit, root, map[string]any{"path": "w.txt", "oldString": "alpha\nbeta", "newString": "alpha\nBETA\nextra"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "alpha\r\nBETA\r\nextra\r\ngamma\r\n" {
		t.Fatalf("CRLF not preserved: %q", b)
	}
}

func TestBuiltinsIncludeSearchTools(t *testing.T) {
	names := map[string]bool{}
	for _, s := range SpecsBuiltins() {
		names[s.Name] = true
	}
	for _, n := range []string{"grep", "glob", "edit", "edit_many"} {
		if !names[n] {
			t.Fatalf("%s missing from SpecsBuiltins", n)
		}
	}
}

// A walk over more files than maxWalkFiles stops and says the result is
// partial, instead of running for minutes.
func TestGrepStopsAtFileLimit(t *testing.T) {
	old := maxWalkFiles
	maxWalkFiles = 3
	t.Cleanup(func() { maxWalkFiles = old })
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, root, n+".txt", "needle\n")
	}
	out, err := call(t, execGrep, root, map[string]any{"pattern": "needle", "output": "files"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "search stopped early: file limit of 3") {
		t.Fatalf("no partial note:\n%s", out)
	}
	out, err = call(t, execGlob, root, map[string]any{"pattern": "*.txt"})
	if err != nil || !strings.Contains(out, "results are partial") {
		t.Fatalf("glob: err=%v\n%s", err, out)
	}
}

func TestGrepStopsAtDeadline(t *testing.T) {
	old := searchDeadline
	searchDeadline = -time.Second // already past: stops at the first entry
	t.Cleanup(func() { searchDeadline = old })
	root := t.TempDir()
	writeFile(t, root, "sub/a.txt", "needle\n")
	out, err := call(t, execGrep, root, map[string]any{"pattern": "needle"})
	if err != nil || !strings.Contains(out, "time limit") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

// Under the limits nothing changes: no note.
func TestGrepNoNoteWhenComplete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "needle\n")
	out, _ := call(t, execGrep, root, map[string]any{"pattern": "needle"})
	if strings.Contains(out, "stopped early") {
		t.Fatalf("unexpected note:\n%s", out)
	}
}

// ── edit_many ──────────────────────────────────────────────────────────────

// The whole point of the tool: many files change in ONE call, with the same
// contract as `edit`.
func TestEditManyAcrossFiles(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, root, "a.go", "package main\nfunc oldName() {}\n")
	b := writeFile(t, root, "pkg/b.go", "package pkg\nfunc oldName() {}\n// keep\n")
	c := writeFile(t, root, "pkg/c.go", "package pkg\n// only a comment\n")

	out, err := call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "a.go", "oldString": "func oldName()", "newString": "func newName()"},
		{"path": "pkg/b.go", "oldString": "func oldName()", "newString": "func newName()"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "pkg/b.go") || !strings.Contains(out, "(2 file(s) changed)") {
		t.Fatalf("unexpected result:\n%s", out)
	}
	for _, p := range []string{a, b} {
		got, _ := os.ReadFile(p)
		if !strings.Contains(string(got), "func newName()") || strings.Contains(string(got), "oldName") {
			t.Fatalf("%s not rewritten: %q", p, got)
		}
	}
	// c.go was never mentioned.
	got, _ := os.ReadFile(c)
	if string(got) != "package pkg\n// only a comment\n" {
		t.Fatalf("untouched file changed: %q", got)
	}
}

// Two edits on the SAME file compose: the second sees the first's result.
func TestEditManyComposesOnSameFile(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "f.txt", "alpha beta gamma\n")
	out, err := call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "f.txt", "oldString": "alpha", "newString": "alphaX"},
		{"path": "f.txt", "oldString": "beta", "newString": "betaX"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok: 2 replacement(s) in f.txt") {
		t.Fatalf("replacements on one file should be counted together:\n%s", out)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "alphaX betaX gamma\n" {
		t.Fatalf("content %q", got)
	}
}

// All-or-nothing: ONE unmatched edit and NOTHING is written, not even the
// edits before it. A half-applied refactor is worse than a failed one.
func TestEditManyWritesNothingWhenAnyEditFails(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, root, "a.txt", "one\n")
	b := writeFile(t, root, "b.txt", "safe\n")

	_, err := call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "a.txt", "oldString": "one", "newString": "ONE"},
		{"path": "b.txt", "oldString": "not-there", "newString": "x"},
	}})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected the failing edit to be reported, got %v", err)
	}
	for _, p := range []string{a, b} {
		got, _ := os.ReadFile(p)
		if string(got) != "one\n" && string(got) != "safe\n" {
			t.Fatalf("%s was written despite a later failure: %q", p, got)
		}
	}
}

func TestEditManyErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "f.txt", "dup\ndup\n")
	if _, err := call(t, execEditMany, root, map[string]any{"edits": []map[string]any{}}); err == nil {
		t.Fatal("empty edits should fail")
	}
	// Ambiguity is the same error `edit` gives: replaceAll is the answer.
	_, err := call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "f.txt", "oldString": "dup", "newString": "x"},
	}})
	if err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	// A missing file names the path.
	_, err = call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "nope.txt", "oldString": "x", "newString": "y"},
	}})
	if err == nil || !strings.Contains(err.Error(), "nope.txt") {
		t.Fatalf("expected the path in the error, got %v", err)
	}
	// An empty oldString points at write, like `edit`.
	_, err = call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "f.txt", "oldString": "", "newString": "y"},
	}})
	if err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("expected the write hint, got %v", err)
	}
}

func TestEditManyPreservesCRLF(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "w.txt", "alpha\r\nbeta\r\n")
	_, err := call(t, execEditMany, root, map[string]any{"edits": []map[string]any{
		{"path": "w.txt", "oldString": "alpha\nbeta", "newString": "alpha\nBETA"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "alpha\r\nBETA\r\n" {
		t.Fatalf("CRLF not preserved: %q", got)
	}
}
