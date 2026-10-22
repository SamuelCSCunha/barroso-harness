package runtime

import (
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

func TestLoopGuardNudgesOnFifthRepeat(t *testing.T) {
	var g loopGuard
	call := []dealer.ToolChamada{{Nome: "ctx_read", Args: "{}"}}
	for i := 1; i <= 4; i++ {
		if n := g.observe(call); n != "" {
			t.Fatalf("round %d nudged early: %s", i, n)
		}
	}
	if n := g.observe(call); !strings.Contains(n, "[loop]") || !strings.Contains(n, "ctx_read") {
		t.Fatalf("no nudge on 5th: %q", n)
	}
	// A different call resets the count.
	if n := g.observe([]dealer.ToolChamada{{Nome: "grep", Args: `{"pattern":"x"}`}}); n != "" {
		t.Fatal("reset failed")
	}
}

func TestLoopGuardIgnoresDifferentArgs(t *testing.T) {
	var g loopGuard
	for i := 0; i < 10; i++ {
		if n := g.observe([]dealer.ToolChamada{{Nome: "file_read", Args: `{"start":` + string(rune('0'+i)) + `}`}}); n != "" {
			t.Fatalf("nudged on distinct args: %s", n)
		}
	}
}
