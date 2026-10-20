// Prompt box tests: the editor as the screen shows it — the caret's cell, a
// multiline prompt growing the box, a click placing the caret, pastes, and the
// line-break transport to the REPL.
package cli

import (
	"strings"
	"testing"
)

// The terminal cursor sits on the caret, not at the end of the text.
func TestPromptCaretOnScreen(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	sc.editor().Insert("hello world")
	for range 6 {
		sc.editor().Left()
	}
	f := sc.frameFor()
	row, col := sc.caretScreen(f)
	if row != f.boxTextRow || col != f.textCol+5 {
		t.Fatalf("caret at %d:%d, want %d:%d", row, col, f.boxTextRow, f.textCol+5)
	}
	frame := sc.frame()
	if !strings.HasSuffix(frame, "\033["+itoa(row)+";"+itoa(col)+"H"+escShowCursor+escSyncEnd) {
		t.Fatalf("the frame must end with the cursor on the caret: %q", frame[len(frame)-40:])
	}
}

// A newline grows the box by a row, upward, and every line is painted.
func TestPromptMultilineBox(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	one := sc.frameFor()
	sc.editor().Insert("first line\nsecond line\nthird line")
	three := sc.frameFor()
	if three.boxTextN != 3 || three.boxTextRow != one.boxTextRow-2 {
		t.Fatalf("box rows = %d at %d (one-line box at %d)", three.boxTextN, three.boxTextRow, one.boxTextRow)
	}
	rows := visualize(sc.frame(), 100, 30)
	for i, want := range []string{"first line", "second line", "third line"} {
		if got := rowAt(rows, three.boxTextRow+i); !strings.Contains(got, want) {
			t.Errorf("box row %d = %q, want %q", i, got, want)
		}
	}
	// The caret is on the last line, after "third line".
	row, col := sc.caretScreen(three)
	if row != three.boxTextRow+2 || col != three.textCol+len("third line") {
		t.Errorf("caret at %d:%d", row, col)
	}
}

// Past its maximum height the box scrolls internally and keeps the caret
// visible, both at the bottom and after moving to the top.
func TestPromptBoxScrollsWithCaret(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 100, 24, panelState{model: "m"})
	maxBox := max(6, 24/3)
	var lines []string
	for i := range 20 {
		lines = append(lines, "line "+itoa(i))
	}
	sc.editor().Insert(strings.Join(lines, "\n"))
	v := sc.boxView(maxBox)
	if len(v.lines) != maxBox || v.lines[len(v.lines)-1] != "line 19" {
		t.Fatalf("bottom window = %v", v.lines)
	}
	sc.editor().BufferStart()
	v = sc.boxView(maxBox)
	if v.lines[0] != "line 0" || v.caretRow != 0 {
		t.Fatalf("top window = %v caret row %d", v.lines, v.caretRow)
	}
}

// A click in the box text moves the caret to the clicked cell.
func TestPromptClickPlacesCaret(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	sc.editor().Insert("alpha beta\ngamma")
	f := sc.frameFor()
	if !sc.clickBox(f.boxTextRow, f.textCol+3) {
		t.Fatal("a click on the first text row must hit the box")
	}
	if caret(sc.editor()) != "0:3" {
		t.Fatalf("caret = %s", caret(sc.editor()))
	}
	// Past the end of a line the caret clamps to that line's end.
	sc.clickBox(f.boxTextRow+1, f.textCol+40)
	if caret(sc.editor()) != "1:5" {
		t.Fatalf("caret = %s", caret(sc.editor()))
	}
	// Outside the text rows the click is not the box's.
	if sc.clickBox(f.convRow, f.textCol) {
		t.Fatal("a click on the transcript must not move the caret")
	}
}

// A long paste becomes one [Pasted ~N lines] chip and expands on submit.
func TestPromptPastedBlock(t *testing.T) {
	sc := &screen{out: &strings.Builder{}}
	sc.editor().Insert("review: ")
	sc.pasteInput("a\r\nb\r\nc")
	if got := sc.editor().Text(); got != "review: [Pasted ~3 lines]" {
		t.Fatalf("box = %q", got)
	}
	entry, sent, _ := sc.takeInput()
	if sent != "review: a\nb\nc" {
		t.Fatalf("sent = %q", sent)
	}
	// History keeps the chip, and recalling it brings the block back.
	sc.recall(entry, false)
	if _, s, _ := sc.takeInput(); s != "review: a\nb\nc" {
		t.Fatalf("recalled = %q", s)
	}
	// A short paste goes in as typed.
	sc.pasteInput("short")
	if sc.editor().Text() != "short" {
		t.Fatalf("short paste = %q", sc.editor().Text())
	}
}

// A secret's answer stays on one line and is masked in the box.
func TestPromptMaskedAnswer(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	sc.masked = true
	sc.pasteInput("sk-1\n2")
	if sc.editor().Text() != "sk-1 2" {
		t.Fatalf("secret = %q", sc.editor().Text())
	}
	if strings.Contains(stripANSI(sc.frame()), "sk-1") {
		t.Fatal("the secret must not be painted")
	}
}

// Multiline prompts cross the REPL pipe as one line.
func TestLineBreakTransport(t *testing.T) {
	in := "one\ntwo\n\nthree"
	enc := encodeLine(in)
	if strings.Contains(enc, "\n") {
		t.Fatalf("encoded = %q", enc)
	}
	if decodeLine(enc) != in {
		t.Fatalf("round trip = %q", decodeLine(enc))
	}
}

// ctrl+t cycles the effort levels, then back to off.
func TestNextEffort(t *testing.T) {
	if nextEffort("") != Esforcos[0] {
		t.Fatal("off -> first level")
	}
	if nextEffort(Esforcos[0]) != Esforcos[1] {
		t.Fatal("next level")
	}
	if nextEffort(Esforcos[len(Esforcos)-1]) != "-" {
		t.Fatal("last level -> off")
	}
}
