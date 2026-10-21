package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// terminal_run streams its output to the context's sink while it runs, and
// still returns the whole output as its result.
func TestTerminalRunStreamsOutput(t *testing.T) {
	var mu sync.Mutex
	var got strings.Builder
	ctx := WithOutput(context.Background(), func(c string) {
		mu.Lock()
		got.WriteString(c)
		mu.Unlock()
	})
	out, err := specTerminalRun().Exec(ctx, t.TempDir(), json.RawMessage(`{"command":"echo one&& echo two"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Fatalf("result = %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got.String(), "one") || !strings.Contains(got.String(), "two") {
		t.Fatalf("streamed = %q", got.String())
	}
}
