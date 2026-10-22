package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── helpers ───────────────────────────────────────────────────────────────

// write creates the path (with parents) and writes the content.
func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// skillDir builds `<root>/<name>/SKILL.md` with frontmatter and body.
func skillDir(t *testing.T, root, name, front, body string) string {
	t.Helper()
	return write(t, filepath.Join(root, name, fileName), front+"\n"+body)
}

// call runs the spec named `name` and returns its output.
func call(t *testing.T, name string, specs []tools.Spec, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Name == name {
			return s.Exec(context.Background(), "", raw)
		}
	}
	t.Fatalf("spec %s not found", name)
	return "", nil
}

// ── frontmatter ───────────────────────────────────────────────────────────

func TestFrontmatterQuotedNameWinsOverDirectory(t *testing.T) {
	root := t.TempDir()
	body := "line one\n\nline two"
	p := skillDir(t, root, "my-skill",
		"---\nname: \"Other Name\"\ndescription: 'Does useful things.'\n---", body)

	s, ok, err := loadFrom(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("loadFrom: ok=%v err=%v", ok, err)
	}
	if s.Nome != "Other Name" {
		t.Fatalf("frontmatter name (quoted) should win, got %q", s.Nome)
	}
	if s.Descricao != "Does useful things." {
		t.Fatalf("description without single quotes should be exact, got %q", s.Descricao)
	}
	if s.Corpo != body {
		t.Fatalf("body should be preserved verbatim, got %q", s.Corpo)
	}
	if s.Fontes != "projeto" {
		t.Fatalf("source label should be kept, got %q", s.Fontes)
	}
}

func TestFrontmatterWithoutNameDerivesFromDirectory(t *testing.T) {
	root := t.TempDir()
	body := "instructions"
	p := skillDir(t, root, "derived-skill", "---\ndescription: description only\n---", body)

	s, ok, err := loadFrom(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("loadFrom: ok=%v err=%v", ok, err)
	}
	if s.Nome != "derived-skill" {
		t.Fatalf("without name, the directory name should apply, got %q", s.Nome)
	}
	if s.Descricao != "description only" {
		t.Fatalf("description should come from the frontmatter, got %q", s.Descricao)
	}
	if s.Corpo != body {
		t.Fatalf("body should be preserved, got %q", s.Corpo)
	}
}

func TestNoFrontmatterWholeFileIsBody(t *testing.T) {
	root := t.TempDir()
	// file without any `---` block: content written raw
	text := "- step one\n- step two"
	p := write(t, filepath.Join(root, "no-front", fileName), text)

	s, ok, err := loadFrom(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("loadFrom: ok=%v err=%v", ok, err)
	}
	if s.Nome != "no-front" {
		t.Fatalf("name should come from the directory, got %q", s.Nome)
	}
	if s.Descricao != "" {
		t.Fatalf("without frontmatter there is no description, got %q", s.Descricao)
	}
	if s.Corpo != text {
		t.Fatalf("without frontmatter the whole file is the body, got %q", s.Corpo)
	}
}

func TestUnclosedFrontmatterBlockIsWholeBody(t *testing.T) {
	root := t.TempDir()
	text := "name: lie\n\nstill body"
	p := write(t, filepath.Join(root, "unclosed", fileName), "---\n"+text)

	s, ok, err := loadFrom(p, "projeto")
	if err != nil || !ok {
		t.Fatalf("loadFrom: ok=%v err=%v", ok, err)
	}
	if s.Nome != "unclosed" || s.Descricao != "" {
		t.Fatalf("an unclosed block is not frontmatter, got %+v", s)
	}
	if s.Corpo != "---\n"+text {
		t.Fatalf("an unclosed block should stay in the body, got %q", s.Corpo)
	}
}

func TestEmptyBodyIsSkipped(t *testing.T) {
	root := t.TempDir()
	p := skillDir(t, root, "empty", "---\nname: empty\n---", "\n  \n")

	_, ok, err := loadFrom(p, "projeto")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a skill with an empty body should not be accepted")
	}

	// and it disappears from the catalog, without error
	out, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("empty skill should not appear, got %d", len(out))
	}
}

// ── discovery and precedence ──────────────────────────────────────────────

func TestPrecedenceDedupTwoSources(t *testing.T) {
	proj := t.TempDir()
	glob := t.TempDir()

	// same name in both sources: the first (projeto) wins
	skillDir(t, proj, "dup", "---\nname: dup\ndescription: from project\n---", "project body")
	skillDir(t, glob, "dup", "---\nname: dup\ndescription: from global\n---", "global body")
	// names unique to each side
	skillDir(t, proj, "project-only", "---\nname: project-only\n---", "only in project")
	skillDir(t, glob, "global-only", "---\nname: global-only\n---", "only in global")

	out, err := Descobrir([]Fonte{
		{Raiz: proj, Rotulo: "projeto"},
		{Raiz: glob, Rotulo: "global"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 skills after dedupe, got %d", len(out))
	}

	byName := map[string]Skill{}
	for _, s := range out {
		byName[s.Nome] = s
	}
	if d := byName["dup"]; d.Fontes != "projeto" || !strings.Contains(d.Corpo, "project body") {
		t.Fatalf("the highest-precedence source should win the dedupe, got %+v", d)
	}
	if byName["project-only"].Fontes != "projeto" {
		t.Fatalf("wrong label: %+v", byName["project-only"])
	}
	if byName["global-only"].Fontes != "global" {
		t.Fatalf("wrong label: %+v", byName["global-only"])
	}
}

func TestDiscoverIgnoresHiddenAndSortsByName(t *testing.T) {
	root := t.TempDir()
	skillDir(t, root, "zeta", "---\nname: zeta\n---", "body z")
	skillDir(t, root, "alpha", "---\nname: alpha\n---", "body a")
	skillDir(t, root, "Beta", "---\nname: Beta\n---", "body b")
	// hidden ones are skipped
	skillDir(t, root, ".hidden", "---\nname: hidden\n---", "hidden body")

	out, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("hidden directory should be ignored, got %d: %+v", len(out), out)
	}
	order := []string{out[0].Nome, out[1].Nome, out[2].Nome}
	want := []string{"alpha", "Beta", "zeta"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("case-insensitive order should be %v, got %v", want, order)
		}
	}
}

func TestDiscoverLooseSkillInRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, fileName), "---\nname: loose\ndescription: in root\n---\nroot body")

	out, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Nome != "loose" || out[0].Corpo != "root body" {
		t.Fatalf("loose SKILL.md in root should be included with the frontmatter name, got %+v", out)
	}
}

func TestDiscoverEmptyDirectoriesNoError(t *testing.T) {
	dir := t.TempDir() // exists, empty
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Descobrir([]Fonte{{Raiz: filepath.Join(dir, "skills"), Rotulo: "projeto"}})
	if err != nil {
		t.Fatalf("an empty source is not an error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("no skill should appear, got %d", len(out))
	}
}

func TestDiscoverMissingSourceNoError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out, err := Descobrir([]Fonte{{Raiz: missing, Rotulo: "projeto"}})
	if err != nil {
		t.Fatalf("a missing source should be tolerated: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("nothing should be found, got %d", len(out))
	}
}

func TestDiscoverAllSourcesUnreadableErrors(t *testing.T) {
	base := t.TempDir()
	// a file in place of a directory: ReadDir fails
	file := write(t, filepath.Join(base, "file.txt"), "x")
	_, err := Descobrir([]Fonte{{Raiz: file, Rotulo: "projeto"}})
	if err == nil {
		t.Fatal("no readable source should return an error")
	}
	if !strings.Contains(err.Error(), "no skills source") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// one readable source is enough to not fail
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Descobrir([]Fonte{{Raiz: file, Rotulo: "projeto"}, {Raiz: empty, Rotulo: "projeto"}}); err != nil {
		t.Fatalf("with one readable source it should not fail: %v", err)
	}
}

// ── FontesPadrao ──────────────────────────────────────────────────────────

func TestDefaultSourcesComposition(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "me")
	cwd := filepath.Join(string(filepath.Separator), "home", "me", "proj")

	sources := FontesPadrao(home, cwd)
	if len(sources) != 3 {
		t.Fatalf("expected 3 sources, got %d: %+v", len(sources), sources)
	}

	want := []Fonte{
		{Raiz: filepath.Join(cwd, ".barroso", "skills"), Rotulo: "projeto"},
		{Raiz: filepath.Join(cwd, "skills"), Rotulo: "projeto"},
		{Raiz: filepath.Join(home, ".barroso-harness", "skills"), Rotulo: "global"},
	}
	for i, f := range want {
		if sources[i] != f {
			t.Fatalf("source %d should be %+v, got %+v", i, f, sources[i])
		}
	}
}

func TestDefaultSourcesEmptyStrings(t *testing.T) {
	if f := FontesPadrao("", ""); len(f) != 0 {
		t.Fatalf("without home and cwd there is no source, got %d", len(f))
	}
	if f := FontesPadrao("", filepath.Join("only", "cwd")); len(f) != 2 {
		t.Fatalf("without home only the project ones remain, got %d", len(f))
	}
	if f := FontesPadrao(filepath.Join("only", "home"), ""); len(f) != 1 || f[0].Rotulo != "global" {
		t.Fatalf("without cwd only the global one remains, got %+v", f)
	}
}

// ── tools ─────────────────────────────────────────────────────────────────

func TestSkillsListNumberedCatalog(t *testing.T) {
	root := t.TempDir()
	skillDir(t, root, "beta", "---\nname: beta\ndescription: does b\n---", "body b")
	skillDir(t, root, "alpha", "---\nname: alpha\ndescription: does a\n---", "body a")

	list, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	specs := Especificacoes(list)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if specs[0].Name != "skills" || specs[1].Name != "skill" {
		t.Fatalf("specs out of order: %s, %s", specs[0].Name, specs[1].Name)
	}

	out, err := call(t, "skills", specs, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(out, name) {
			t.Fatalf("catalog did not include %s:\n%s", name, out)
		}
	}
	// numbered and sorted
	if !strings.Contains(out, "1. alpha") || !strings.Contains(out, "2. beta") {
		t.Fatalf("catalog should be numbered and sorted:\n%s", out)
	}
	if !strings.Contains(out, "skill") {
		t.Fatalf("catalog should point to skill:\n%s", out)
	}
}

func TestSkillsListEmpty(t *testing.T) {
	out, err := call(t, "skills", Especificacoes(nil), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no skills") {
		t.Fatalf("empty catalog should say so, got %q", out)
	}
}

func TestSkillReadReturnsExactBody(t *testing.T) {
	root := t.TempDir()
	body := "# Title\n\nStep 1\nStep 2\n"
	skillDir(t, root, "exact", "---\nname: exact\ndescription: d\n---", body)

	list, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	specs := Especificacoes(list)
	out, err := call(t, "skill", specs, map[string]any{"id": "exact"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, body) {
		t.Fatalf("the exact body should end the output:\nexpected at end: %q\ngot: %q", body, out)
	}
	if !strings.HasPrefix(out, "# exact\n\n") {
		t.Fatalf("output should start with the skill name, got %q", out)
	}
}

func TestSkillReadUnknownNameErrorsInOneLine(t *testing.T) {
	root := t.TempDir()
	skillDir(t, root, "single", "---\nname: single\n---", "body")

	list, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	specs := Especificacoes(list)

	out, err := call(t, "skill", specs, map[string]any{"id": "does-not-exist"})
	if err == nil {
		t.Fatalf("unknown name should fail, got %q", out)
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") {
		t.Fatalf("error should be one line, got %q", msg)
	}
	if msg != "skill does-not-exist not found; use skills" {
		t.Fatalf("unexpected message: %q", msg)
	}

	// an empty name hits the same one-liner
	if _, err := call(t, "skill", specs, map[string]any{"id": "  "}); err == nil ||
		strings.Contains(err.Error(), "\n") {
		t.Fatalf("empty name should fail in one line, got %v", err)
	}
}

func TestSkillReadTruncatesLargeBody(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat("x", bodyCap+4096)
	skillDir(t, root, "large", "---\nname: large\n---", body)

	list, err := Descobrir([]Fonte{{Raiz: root, Rotulo: "projeto"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t, "skill", Especificacoes(list), map[string]any{"id": "large"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "body truncated") {
		t.Fatalf("a body above the cap should be truncated with a marker:\n%s", out[:200])
	}
	if len(out) >= len("# large\n\n")+len(body) {
		t.Fatalf("output should be smaller than the full body (%d vs %d)", len(out), len(body))
	}
}
