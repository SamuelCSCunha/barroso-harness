// Mouse support for the full screen, in the OpenCode model: the renderer
// enables the same terminal modes OpenCode does (1000 button reporting,
// 1002 drag reporting, 1003 any-motion reporting, 1006 SGR encoding), each
// frame records the rectangles it drew together with their hover style and
// click action, and clicks/hover are dispatched through those rectangles.
//
// A left-drag over the conversation is captured as an in-app selection: it is
// copied to the clipboard (OSC 52 plus the OS clipboard) on release /
// right-click / ctrl+c, and while it exists clicks are ignored, exactly as
// OpenCode does.
package cli

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// mouseSeq is one decoded SGR mouse event. Col and Row are 1-based terminal
// coordinates, as the terminal reports them.
type mouseSeq struct {
	Col, Row int
	Button   int  // button number of the event (0 left, 1 middle, 2 right, 3/4 wheel)
	Wheel    int  // 64 = wheel up, 65 = wheel down (SGR encodes them in Button)
	Release  bool // the sequence ended in 'm'
	Drag     bool // motion with a button held
	Motion   bool // motion without a button held (hover)
	Shift    bool
	Alt      bool
	Ctrl     bool
}

// parseMouseSeq decodes one SGR mouse sequence, i.e. "<code;col;rowM" (press)
// or "<code;col;rowm" (release). SGR encodes button+modifiers in one number:
// the low two bits are the button, 4 shift, 8 alt (meta), 16 control, 32
// motion, and the wheel is reported as 64/65 (the terminal sets bit 6 and
// reports button 4-7, which encode as 64 plus the low bits).
func parseMouseSeq(seq string) (mouseSeq, bool) {
	final := byte(0)
	if n := len(seq); n > 0 && (seq[n-1] == 'M' || seq[n-1] == 'm') {
		final = seq[n-1]
		seq = seq[:n-1]
	}
	parts := strings.Split(seq, ";")
	if len(parts) != 3 || final == 0 {
		return mouseSeq{}, false
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil || code < 0 {
		return mouseSeq{}, false
	}
	col, err := strconv.Atoi(parts[1])
	if err != nil || col < 1 {
		return mouseSeq{}, false
	}
	row, err := strconv.Atoi(parts[2])
	if err != nil || row < 1 {
		return mouseSeq{}, false
	}
	e := mouseSeq{Col: col, Row: row, Release: final == 'm'}
	if code&16 != 0 {
		e.Ctrl = true
	}
	if code&8 != 0 {
		e.Alt = true
	}
	if code&4 != 0 {
		e.Shift = true
	}
	b := code & 3
	e.Button = b
	switch {
	case code&32 != 0: // motion: bit 5 set, low bits tell what is held
		e.Motion = true
		e.Drag = b != 3
	case code&64 != 0: // wheel: 64 (+ low bits) up, 65 down; 66/67 horizontal
		e.Wheel = 64 + b
	}
	return e, true
}

// hitRegion is one interactive rectangle of the frame: the cells the last
// drawLocked painted, an optional hover style and what a click does. The
// action runs from the key loop with the screen lock held.
type hitRegion struct {
	row, col, width, height int
	// full is the region's own text, already coloured and padded, so a hover
	// repaint writes exactly what the plain frame wrote.
	plain, hover string
	id           string
	action       func()
	hovered      bool
}

// hitList is the set of regions of the last frame, later ones on top: the
// dispatch picks the first match from the end, like a z-index.
type hitList struct {
	regions []*hitRegion
}

func newHits() *hitList { return &hitList{} }

// add appends a region; a nil action still registers (it lets the region claim
// the click, e.g. the prompt, so a click inside the input box is not a
// conversation click).
func (h *hitList) add(region *hitRegion) { h.regions = append(h.regions, region) }

// at returns the topmost region containing (row, col), or nil.
func (h *hitList) at(row, col int) *hitRegion {
	for i := len(h.regions) - 1; i >= 0; i-- {
		r := h.regions[i]
		if row >= r.row && row < r.row+r.height && col >= r.col && col < r.col+r.width {
			return r
		}
	}
	return nil
}

// hoverRegion moves the hover highlight to (row, col) and returns the region
// that changed when a repaint is needed (nil means the hovered region did not
// change, so no redraw is needed). Motion is coalesced this way: moving the
// pointer inside one region costs nothing, only crossing a border repaints.
func (h *hitList) hoverRegion(row, col int) (*hitRegion, bool) {
	target := h.at(row, col)
	change := false
	for _, r := range h.regions {
		switch {
		case r == target && !r.hovered:
			r.hovered = true
			change = true
		case r != target && r.hovered:
			r.hovered = false
			change = true
		}
	}
	return target, change
}

// clear forgets the regions and their hovered state: called when the frame is
// rebuilt, so a repaint starts from a known state.
func (h *hitList) clear() { h.regions = nil }

// hovering returns the region currently highlighted, or nil.
func (h *hitList) hovering() *hitRegion {
	for _, r := range h.regions {
		if r.hovered {
			return r
		}
	}
	return nil
}

// selPoint is a cell of the selection anchor: line is the index in the wrapped
// transcript, col the visible column (0-based) inside that line.
type selPoint struct {
	line, col int
}

// selRange is an inclusive span between two cells.
type selRange struct {
	from, to selPoint
}

// normalize orders the endpoints, so callers always deal with from <= to.
func (s selRange) normalize() selRange {
	if s.from.line > s.to.line || (s.from.line == s.to.line && s.from.col > s.to.col) {
		s.from, s.to = s.to, s.from
	}
	return s
}

// inRange reports whether the cell falls inside the normalized span.
func (s selRange) inRange(p selPoint) bool {
	if p.line < s.from.line || p.line > s.to.line {
		return false
	}
	if p.line == s.from.line && p.col < s.from.col {
		return false
	}
	if p.line == s.to.line && p.col >= s.to.col {
		return false
	}
	return true
}

// extractSelection cuts the selected columns out of the transcript lines: one
// entry per touched line, joined by "\n" (trailing blanks collapse, so copying
// never ends with a dangling newline). Escapes are stripped: the clipboard text
// is plain. Wide glyphs are copied whole when the cut lands inside them, the
// way a terminal selection does.
func extractSelection(lines []string, sel selRange) string {
	first, last := sel.from, sel.to
	if first.line > last.line || (first.line == last.line && first.col > last.col) {
		first, last = last, first // normalize: a drag may be made either way
	}
	if first.line == last.line && first.col >= last.col {
		return "" // an empty span (from == to) selects nothing
	}
	var out []string
	for line := first.line; line <= last.line; line++ {
		if line < 0 || line >= len(lines) {
			continue
		}
		runes := []rune(stripANSI(lines[line]))
		from, to := 0, len(runes)
		if line == first.line {
			from = clampInt(first.col, 0, len(runes))
		}
		if line == last.line {
			to = clampInt(last.col, from, len(runes))
		}
		out = append(out, strings.TrimRight(string(runes[from:to]), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// stripANSI removes escape sequences, so a cut line never contains control
// bytes (they would break the clipboard text).
func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\033') {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			i = escapeEnd(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+size])
		i += size
	}
	return sb.String()
}

// ── mouse state machine ─────────────────────────────────────────────────────
//
// The states: idle (nothing held), selecting (left button held over the
// conversation), pressed (left button held elsewhere — a click only counts
// when the release lands on the same region).
const (
	mouseIdle = iota
	mouseSelecting
	mousePressed
)

// mouseState drives the left-drag selection and the click dispatch.
type mouseState struct {
	mode       int
	pressRow   int
	pressCol   int
	pressedHvr *hitRegion
}

// press handles a left press: over the conversation it starts a selection;
// anywhere else it arms the click, whose hover style the caller repaints.
func (m *mouseState) press(hits *hitList, row, col int) *hitRegion {
	r := hits.at(row, col)
	if r == nil || r.id == "conv" || r.hover == "" {
		if r == nil || r.id == "conv" {
			m.mode = mouseSelecting
			return nil
		}
		m.mode = mouseIdle
		return nil
	}
	m.mode = mousePressed
	m.pressRow, m.pressCol = row, col
	m.pressedHvr = r
	return r
}

// drag extends the selection while the button is held over the conversation;
// a drag that leaves it drops the pending click.
func (m *mouseState) drag(row, col, convRow, convCol int) *selPoint {
	if m.mode == mouseIdle {
		return nil
	}
	if m.mode == mouseSelecting {
		return &selPoint{line: row - convRow, col: col - convCol}
	}
	m.mode = mouseIdle
	m.pressedHvr = nil
	return nil
}

// release closes the interaction. A selection returns kept=true (the caller
// copies it and ignores the click); a click returns the region that was
// pressed, when the release lands on it.
func (m *mouseState) release(hits *hitList, row, col int) (*hitRegion, bool) {
	if m.mode == mouseIdle {
		return nil, false
	}
	if m.mode == mouseSelecting {
		m.mode = mouseIdle
		return nil, true
	}
	r := hits.at(row, col)
	kept := m.pressedHvr != nil && r == m.pressedHvr
	m.mode, m.pressedHvr = mouseIdle, nil
	return r, kept
}
