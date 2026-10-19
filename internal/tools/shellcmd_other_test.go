//go:build !windows

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// On Unix the child is bash -c with a single argv entry, delivered verbatim;
// the contract to pin is that the whole line arrives as ONE argument.
func TestTerminalRunPassesWholeLine(t *testing.T) {
	spec := specTerminalRun()
	out, err := spec.Exec(context.Background(), ".",
		json.RawMessage(`{"command":"printf '[%s]' \"$0\"\" ok\"\" 42\""}`))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "[bash ok 42]") {
		t.Fatalf("bash did not receive the whole line as one argument; output:\n%s", out)
	}
}
