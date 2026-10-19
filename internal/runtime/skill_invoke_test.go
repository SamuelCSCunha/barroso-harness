package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectSkill(t *testing.T, name, body string) string {
	t.Helper()
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".barroso", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: test\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir()) // isolate the global source
	return cwd
}

func TestExpandSkillBuildsPromptWithBodyAndArgs(t *testing.T) {
	cwd := projectSkill(t, "review-pr", "1. read the diff\n2. point out bugs")
	prompt, name, ok := ExpandirSkill(cwd, "/review-pr  #42 focusing on security")
	if !ok || name != "review-pr" {
		t.Fatalf("skill not recognized: ok=%v name=%q", ok, name)
	}
	for _, want := range []string{"Run the skill `review-pr`", "1. read the diff", "2. point out bugs", "User arguments: #42 focusing on security"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt without %q:\n%s", want, prompt)
		}
	}
}

func TestExpandSkillWithoutArgsAddsNoLine(t *testing.T) {
	cwd := projectSkill(t, "deploy", "run the deploy")
	prompt, _, ok := ExpandirSkill(cwd, "/DEPLOY")
	if !ok {
		t.Fatal("name should match case-insensitively")
	}
	if strings.Contains(prompt, "arguments") {
		t.Errorf("without arguments there should be no such line:\n%s", prompt)
	}
}

func TestExpandSkillIgnoresUnknownNameAndPlainText(t *testing.T) {
	cwd := projectSkill(t, "deploy", "x")
	for _, l := range []string{"/does-not-exist", "deploy", "/", "  "} {
		if _, _, ok := ExpandirSkill(cwd, l); ok {
			t.Errorf("%q should not become a skill", l)
		}
	}
}
