package cli

import (
	"strings"
	"testing"
)

// Before the first message the screen shows the wordmark and a prompt at most
// 75 columns wide, centered; the first message switches to the conversation.
func TestHomeScreen(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, sb := newTestScreen(t, 160, 40, panelState{model: "m", provider: "p"})
	if !sc.isHome() {
		t.Fatal("an empty screen must show home")
	}
	f := sc.frameFor()
	width := f.boxWidth - f.barCol + 1
	if width != homePromptMax || f.barCol != (160-homePromptMax)/2+1 {
		t.Fatalf("home box at col %d width %d", f.barCol, width)
	}
	rows := visualize(sb.String(), 160, 40)
	all := strings.Join(rows, "\n")
	if !strings.Contains(all, "█▀▀▄") || !strings.Contains(rowAt(rows, f.boxMetaRow), "m p") {
		t.Fatalf("home frame:\n%s", showGrid(rows, 160))
	}
	sc.Usuario("hello")
	if sc.isHome() || sc.frameFor().barCol != 3 {
		t.Fatal("the first message must switch to the conversation layout")
	}
}
