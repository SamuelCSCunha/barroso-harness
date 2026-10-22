//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// shell must hand the model's command line to cmd.exe AS WRITTEN.
// exec.Command's default escaping rewrites inner double quotes as \" and
// cmd.exe does not understand backslash escapes: the backslash leaked and the
// command arrived truncated at the first quote. These two cases both fail
// with the old spawn and pass with SysProcAttr.CmdLine.
func TestTerminalRunKeepsDoubleQuotes(t *testing.T) {
	spec := specTerminalRun()

	// 1. Quotes that carry meaning: cmd's if comparison only says SAME when
	// both quoted strings parse as single arguments.
	out, err := spec.Exec(context.Background(), ".",
		json.RawMessage(`{"command":"if \"a b\" == \"a b\" echo SAME"}`))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "SAME") {
		t.Fatalf("quoted comparison broke; output:\n%s", out)
	}
	if strings.Contains(out, `\"`) {
		t.Fatalf("backslash-quote leaked into the command; output:\n%s", out)
	}

	// 2. The reported reproduction: an inline script argument. Node is the
	// smallest reliable way to prove the child received the WHOLE argument.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	out, err = spec.Exec(context.Background(), ".",
		json.RawMessage(`{"command":"node -e \"console.log('argv ok', 6*7)\""}`))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "argv ok 42") {
		t.Fatalf("node did not receive the quoted script; output:\n%s", out)
	}
}
