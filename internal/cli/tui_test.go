package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// The spinner hides the cursor and restores it when stopping — a cursor
// blinking next to "Thinking" looked like an input field.
func TestSpinnerHidesAndRestoresCursor(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	var sb strings.Builder
	tui := newTUITurn(&sb, &config.Config{Model: "m"})
	tui.Pensando()
	time.Sleep(20 * time.Millisecond)
	tui.stopSpin()
	s := sb.String()
	i, j := strings.Index(s, hideCursor), strings.LastIndex(s, showCursor)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("cursor was not hidden and restored in that order: %q", s)
	}
}

// BH has no modes: neither the status line nor the footer carries "Build".
func TestNoModeLabel(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	var sb strings.Builder
	cfg := &config.Config{CurrentProvider: "p", Model: "m", Janela: 1000}
	inputStatus(&sb, cfg, "", 0, 0)
	newTUITurn(&sb, cfg).Tokens(10, 10, time.Second, false)
	if strings.Contains(sb.String(), "Build") {
		t.Fatalf("mode label should not appear: %q", sb.String())
	}
}
