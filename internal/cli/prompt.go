// The prompt box on top of the editor: how the buffer wraps into the box's rows,
// where the caret lands on screen, how a click moves it, which keys edit it, and
// how a submitted prompt travels to the REPL.
package cli

import (
	"regexp"
	"strings"
)

// lineBreakMark carries a prompt's line breaks through the REPL's input pipe,
// which is read one line at a time: a multiline prompt would otherwise arrive as
// several separate submissions. The REPL turns it back into "\n" (decodeLine).
// It is a C0 control byte, so typed text can never contain it.
const lineBreakMark = "\x1c"

func encodeLine(s string) string { return strings.ReplaceAll(s, "\n", lineBreakMark) }
func decodeLine(s string) string { return strings.ReplaceAll(s, lineBreakMark, "\n") }

// tokenRe matches every attachment placeholder the box paints as a chip.
var tokenRe = regexp.MustCompile(`\[Image \d+\]|\[Pasted ~\d+ lines?\]`)

// editor returns the prompt buffer, creating it on first use (tests build the
// screen as a bare literal).
func (t *screen) editor() *editor {
	if t.ed == nil {
		t.ed = newEditor()
	}
	return t.ed
}

// boxSeg is one painted row of the box: which buffer line it shows and which
// rune columns of it.
type boxSeg struct{ row, start, end int }

// boxView is the visible part of the wrapped prompt and the caret inside it.
type boxView struct {
	lines              []string
	segs               []boxSeg
	caretRow, caretCol int
}

// wrapSegments splits a line into [start,end) rune ranges of at most n
// columns, breaking at the last space like wrapInput (the space at the break is
// dropped from both rows).
func wrapSegments(r []rune, n int) [][2]int {
	n = max(10, n)
	var segs [][2]int
	start := 0
	for len(r)-start > n {
		cut := n
		for i := n; i > 0; i-- {
			if r[start+i] == ' ' {
				cut = i
				break
			}
		}
		segs = append(segs, [2]int{start, start + cut})
		next := start + cut
		if next < len(r) && r[next] == ' ' {
			next++
		}
		start = next
	}
	return append(segs, [2]int{start, len(r)})
}

// boxTextWidth is the width the typed text wraps at: two columns inside the ┃
// and two of right padding.
func (t *screen) boxTextWidth() int {
	if t.isHome() {
		// The home prompt is at most 75 columns wide, centered (see homeFrame).
		return min(homePromptMax, t.w-2*t.panePadCols()) - 4
	}
	return t.boxWidthCols() - t.panePadCols() - 4
}

// boxView wraps the buffer for the box and keeps the caret's row inside the
// visible window of maxBox rows: past that height the box scrolls internally,
// following the caret (t.boxTop is the first visible row).
func (t *screen) boxView(maxBox int) boxView {
	var v boxView
	if t.partial != "" {
		// A command prompt is answered on its own transcript line; the box is idle.
		v.lines, v.segs = []string{""}, []boxSeg{{}}
		t.boxTop = 0
		return v
	}
	e := t.editor()
	width := t.boxTextWidth()
	for row, line := range e.lines {
		r := line
		if t.masked {
			r = []rune(strings.Repeat("•", len(line)))
		}
		segs := wrapSegments(r, width)
		for i, s := range segs {
			last := i == len(segs)-1
			if row == e.row && e.col >= s[0] && e.col <= s[1] && (last || e.col < segs[i+1][0]) {
				v.caretRow, v.caretCol = len(v.lines), e.col-s[0]
			}
			v.lines = append(v.lines, string(r[s[0]:s[1]]))
			v.segs = append(v.segs, boxSeg{row, s[0], s[1]})
		}
	}
	if len(v.lines) <= maxBox {
		t.boxTop = 0
		return v
	}
	top := clampInt(t.boxTop, 0, len(v.lines)-maxBox)
	if v.caretRow < top {
		top = v.caretRow
	}
	if v.caretRow >= top+maxBox {
		top = v.caretRow - maxBox + 1
	}
	t.boxTop = top
	v.lines = v.lines[top : top+maxBox]
	v.segs = v.segs[top : top+maxBox]
	v.caretRow -= top
	return v
}

// caretScreen is the terminal cell of the caret for frame f.
func (t *screen) caretScreen(f frame) (row, col int) {
	v := t.boxView(max(6, t.h/3))
	return f.boxTextRow + v.caretRow, f.textCol + v.caretCol
}

// clickBox moves the caret to a click inside the box's text rows, the way a
// click in OpenCode's textarea does. Reports whether the click landed there.
func (t *screen) clickBox(row, col int) bool {
	if t.partial != "" {
		return false
	}
	f := t.frameFor()
	if row < f.boxTextRow || row >= f.boxTextRow+f.boxTextN || col < f.barCol || col > f.boxWidth {
		return false
	}
	v := t.boxView(max(6, t.h/3))
	i := row - f.boxTextRow
	if i >= len(v.segs) {
		i = len(v.segs) - 1
	}
	s := v.segs[i]
	e := t.editor()
	e.row = s.row
	e.col = clampInt(s.start+col-f.textCol, s.start, s.end)
	e.wantCol, e.hasWantCol = e.col, true
	return true
}

// editKey applies a key that only edits the prompt. Reports whether name was one.
func (t *screen) editKey(name string) bool {
	e := t.editor()
	switch name {
	case "left", "ctrl+b":
		e.Left()
	case "right", "ctrl+f":
		e.Right()
	case "ctrl+a":
		e.LineStart()
	case "ctrl+e":
		e.LineEnd()
	case "alt+f", "ctrl+right":
		e.WordRight()
	case "alt+b", "ctrl+left":
		e.WordLeft()
	case keyBackspace:
		e.Backspace()
	case "ctrl+w", "ctrl+backspace", "alt+backspace":
		e.DeleteWordBack()
	case "ctrl+k":
		e.DeleteToLineEnd()
	case "ctrl+u":
		e.DeleteToLineStart()
	case "delete":
		e.DeleteForward()
	case "ctrl+-":
		e.Undo()
	case "ctrl+.":
		e.Redo()
	default:
		return false
	}
	return true
}

// cleanPaste normalizes pasted text: CRLF/CR become LF, tabs become spaces and
// other control bytes are dropped.
func cleanPaste(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 32 || r == 127:
			return -1
		}
		return r
	}, s)
}

// pasteInput puts pasted text into the prompt. A long or multi-line paste
// becomes one [Pasted ~N lines] token (OpenCode, P:1461-1473); the answer to a
// command prompt or a secret stays on one line.
func (t *screen) pasteInput(s string) {
	if t.partial != "" || t.masked {
		t.editor().Insert(pasteText(s))
		return
	}
	s = cleanPaste(s)
	if tk := pastedSummary(s); tk != nil {
		t.editor().AddToken(*tk)
		return
	}
	t.editor().Insert(s)
}

// historyEntry is one submitted prompt, with its attachment tokens so that
// recalling it brings the images and pasted blocks back too.
type historyEntry struct {
	text string
	toks []token
}

// recall loads a history entry into a fresh buffer.
func (t *screen) recall(h historyEntry, atStart bool) {
	e := newEditor()
	e.toks = append([]token(nil), h.toks...)
	e.lines = splitRunes(h.text)
	if atStart {
		e.BufferStart()
	} else {
		e.BufferEnd()
	}
	t.ed = e
}

// takeInput empties the prompt and returns what it held: the text as shown,
// the text the model receives (pasted blocks expanded) and the image paths.
func (t *screen) takeInput() (entry historyEntry, sent string, images []string) {
	e := t.editor()
	entry = historyEntry{text: e.Text(), toks: append([]token(nil), e.toks...)}
	sent, images = e.Expand(), e.SelectTokens(tokenImage)
	t.ed, t.imageCount, t.boxTop = newEditor(), 0, 0
	return entry, sent, images
}

// queueSent hands the images of a submitted prompt to the REPL, which picks
// them up with takeImages using the same text.
func (t *screen) queueSent(sent string, imgs []string) {
	if len(imgs) == 0 {
		return
	}
	if t.sentImages == nil {
		t.sentImages = map[string][][]string{}
	}
	key := strings.TrimSpace(sent)
	t.sentImages[key] = append(t.sentImages[key], imgs)
}
