package runtime

import "testing"

func TestHookContextClaudeCodeShape(t *testing.T) {
	out := []byte(`{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"<memoria>x</memoria>"}}`)
	if got := hookContext(out); got != "<memoria>x</memoria>" {
		t.Fatalf("got %q", got)
	}
}

func TestHookContextPlainAndEmpty(t *testing.T) {
	if got := hookContext([]byte("  texto puro \n")); got != "texto puro" {
		t.Fatalf("got %q", got)
	}
	if got := hookContext([]byte("\n")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestWithHookContext(t *testing.T) {
	if withHookContext("oi", "") != "oi" {
		t.Fatal("empty context changed the prompt")
	}
	if withHookContext("oi", "ctx") != "oi\n\nctx" {
		t.Fatal("context not appended")
	}
}
