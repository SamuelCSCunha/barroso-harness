// Input editor for the prompt box: a buffer with a caret, so the prompt behaves
// like OpenCode's textarea instead of an append-only string.
//
// Why a line model instead of a flat rune slice: every movement and deletion we
// need is defined in terms of lines (line start/end, up/down across a wrapped
// line, word boundaries, the box's own vertical scroll), and a flat slice makes
// each of those a reimplementation of "which line am I on". Storing lines keeps
// the caret honest and makes multiline input a first-class case rather than an
// accident of a newline escaping pasteText.
//
// Attachments are ATOMIC tokens: deleting with backspace removes the whole
// [Image N] / [Pasted ~N lines] instead of eating it one character at a time,
// which is OpenCode's behaviour (P:1461-1473).
package cli

import (
	"strings"
	"unicode/utf8"
)

// tokenKind is the type of an atomic attachment token.
type tokenKind int

const (
	tokenImage  tokenKind = iota // [Image N]
	tokenPasted                  // [Pasted ~N lines]
	tokenFile                    // @path
)

// token is an atomic run in the editor: the caret never lands inside one and a
// delete removes it whole, no matter which end the caret sits at.
type token struct {
	kind  tokenKind
	text  string // the visible placeholder, e.g. "[Image 1]"
	path  string // image path or file path ("" for a pasted block)
	body  string // the full text of a pasted block (sent on submit)
	lines int    // line count of a pasted block
}

// editor is the prompt's text buffer. Runes, not bytes: a caret measured in
// bytes breaks on the first accented character or emoji, and combining marks
// then split from their base.
type editor struct {
	lines  [][]rune
	row    int // caret line
	col    int // caret column, in runes, within lines[row]
	tokens map[int][]tokenAt
	// undo/redo stacks carry whole-buffer snapshots: the prompt is a few KB at
	// most, and a snapshot is the only history model that cannot corrupt itself
	// when an operation rewrites several lines at once.
	undo []editSnap
	redo []editSnap
	// wantCol is the column the caret keeps returning to while walking up/down
	// through short lines; without it, moving down off a long line and back up
	// lands in a different column than the user left from.
	wantCol    int
	hasWantCol bool
}

// editSnap is one undoable state of the buffer.
type editSnap struct {
	lines [][]rune
	toks  map[int][]tokenAt
}

func newEditor() *editor {
	e := &editor{tokens: map[int][]tokenAt{}}
	e.lines = [][]rune{{}}
	return e
}

// snapshot copies the buffer for the undo stack.
func (e *editor) snapshot() editSnap {
	cp := make([][]rune, len(e.lines))
	for i, l := range e.lines {
		cp[i] = append([]rune(nil), l...)
	}
	toks := make(map[int][]tokenAt, len(e.tokens))
	for k, v := range e.tokens {
		toks[k] = append([]tokenAt(nil), v...)
	}
	return editSnap{lines: cp, toks: toks}
}

// pushUndo records the state before a mutation and clears the redo stack:
// redo only applies to an unbroken undo chain, exactly like every editor.
func (e *editor) pushUndo() {
	e.undo = append(e.undo, e.snapshot())
	e.redo = nil
	const limit = 200
	if len(e.undo) > limit {
		e.undo = e.undo[len(e.undo)-limit:]
	}
}

// Undo restores the state before the last mutation.
func (e *editor) Undo() bool {
	if len(e.undo) == 0 {
		return false
	}
	e.redo = append(e.redo, e.snapshot())
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.restore(s)
	return true
}

// Redo reapplies an undone mutation.
func (e *editor) Redo() bool {
	if len(e.redo) == 0 {
		return false
	}
	e.undo = append(e.undo, e.snapshot())
	s := e.redo[len(e.redo)-1]
	e.redo = e.redo[:len(e.redo)-1]
	e.restore(s)
	return true
}

func (e *editor) restore(s editSnap) {
	e.lines = s.lines
	e.tokens = s.toks
	e.clamp()
}

// clamp keeps the caret inside the buffer after a structural change.
func (e *editor) clamp() {
	if e.row >= len(e.lines) {
		e.row = len(e.lines) - 1
	}
	if e.row < 0 {
		e.row = 0
	}
	if e.col > len(e.lines[e.row]) {
		e.col = len(e.lines[e.row])
	}
	if e.col < 0 {
		e.col = 0
	}
}

// ── reading the buffer ──────────────────────────────────────────────────────

// Text is the buffer as a single string ("\n" between lines). This is what the
// REPL receives on submit, so it must stay a faithful round-trip.
func (e *editor) Text() string {
	parts := make([]string, len(e.lines))
	for i, l := range e.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

// SetText replaces the buffer, keeping (or clamping) the caret.
func (e *editor) SetText(s string) {
	e.pushUndo()
	e.lines = splitRunes(strings.ReplaceAll(s, "\r\n", "\n"))
	e.clamp()
}

// RuneCount is the caret's absolute offset in runes, from the start.
func (e *editor) RuneCount() int {
	n := 0
	for _, l := range e.lines {
		n += len(l) + 1 // the newline itself
	}
	return n - 1
}

// runeAt is the absolute caret offset: every line before the caret's line, plus
// the caret's column, plus the newline that ended each earlier line.
func (e *editor) absCaret() int {
	n := 0
	for i := 0; i < e.row; i++ {
		n += len(e.lines[i]) + 1
	}
	return n + e.col
}

// setRuneOffset places the caret at an absolute offset (used by a mouse click).
func (e *editor) setRuneOffset(off int) {
	e.row, e.col = 0, 0
	for e.row < len(e.lines)-1 && off > len(e.lines[e.row]) {
		off -= len(e.lines[e.row]) + 1
		e.row++
	}
	e.col = clampInt(off, 0, len(e.lines[e.row]))
	e.wantCol, e.hasWantCol = e.col, true
}

// lineRowCount is how many lines the buffer has (the box scrolls by rows).
func (e *editor) lineRowCount() int { return len(e.lines) }

// ── caret movement ──────────────────────────────────────────────────────────

// Left/Right move by one rune, crossing to the neighbouring line at a boundary
// (a caret at the start of a line moves to the end of the previous one, which is
// what every textarea does). The row delta is 0: the column does the moving, and
// the wrap is what changes the line.
func (e *editor) Left()  { e.moveTo(e.row, e.col, 0, -1) }
func (e *editor) Right() { e.moveTo(e.row, e.col, 0, 1) }

// moveTo applies a delta to the caret, clamping at both ends of the buffer.
//
// The wrap is DIRECTIONAL, and that is the whole subtlety: a Right that lands on
// a line's last column steps to the next line, but a Left that arrives at that
// same column must stop there. Without the direction check, a Left crossing out
// of line 1 lands at the end of line 0 and is immediately wrapped back down to
// line 1 — the caret never actually moves.
//
// Clamping happens before any read of e.lines[row], so a row that has gone
// negative is never indexed.
func (e *editor) moveTo(row, col, dr, dc int) {
	row += dr
	col += dc
	for col < 0 && row > 0 {
		row--
		col = len(e.lines[row])
	}
	if col < 0 {
		col = 0
	}
	if row < 0 {
		row, col = 0, 0
	}
	if row > len(e.lines)-1 {
		row = len(e.lines) - 1
		col = len(e.lines[row])
	}
	// Only a column PAST the end wraps: col == len is the valid caret position
	// after the line's last rune.
	if dc > 0 {
		for col > len(e.lines[row]) && row < len(e.lines)-1 {
			row++
			col = 0
		}
	}
	if col > len(e.lines[row]) {
		col = len(e.lines[row])
	}
	e.row, e.col = row, col
	e.wantCol, e.hasWantCol = e.col, true
}

// Up/Down move one line, keeping the column the caret was at before the move
// (wantCol): walking down off a long line and back up must return to the same
// column, not to wherever the shorter line's end happened to be.
func (e *editor) Up()   { e.vertical(-1) }
func (e *editor) Down() { e.vertical(1) }

func (e *editor) vertical(dr int) {
	target := e.row + dr
	if target < 0 || target >= len(e.lines) {
		return
	}
	col := e.col
	if e.hasWantCol {
		col = e.wantCol
	}
	e.row = target
	e.col = clampInt(col, 0, len(e.lines[target]))
}

// LineStart / LineEnd are ctrl+a and ctrl+e.
func (e *editor) LineStart() { e.col = 0; e.wantCol, e.hasWantCol = 0, true }
func (e *editor) LineEnd()   { e.col = len(e.lines[e.row]); e.wantCol, e.hasWantCol = e.col, true }

// BufferStart / BufferEnd are Home and End.
func (e *editor) BufferStart() { e.row, e.col = 0, 0; e.wantCol, e.hasWantCol = 0, true }

// BufferEnd is Home/End. The assignments are on separate lines on purpose: in a
// single multi-assign, the right-hand side is evaluated before EITHER left-hand
// variable is stored, so `e.col = len(e.lines[e.row])` would read the row the
// caret is leaving rather than the last line.
func (e *editor) BufferEnd() {
	e.row = len(e.lines) - 1
	e.col = len(e.lines[e.row])
	e.wantCol, e.hasWantCol = e.col, true
}

// WordRight / WordLeft jump a word, as OpenCode's alt+f / alt+b do.
//
// Both are readline-style: WordRight lands on the END of the next word (so a
// delete-word-pair cuts the word the caret sits in), and WordLeft lands on the
// START of the previous word (so a caret already at a start lands on the one
// before it). WordRight crosses to the next line when it runs off one.
func (e *editor) WordRight() {
	r := e.lines[e.row]
	for e.col < len(r) && !isWordRune(r[e.col]) {
		e.col++
	}
	for e.col < len(r) && isWordRune(r[e.col]) {
		e.col++
	}
	if e.col >= len(r) && e.row < len(e.lines)-1 {
		e.row++
		// Skip the leading separator of the next line so a WordRight on a line
		// that ends in a space lands on the next word, not on its first space.
		r2 := e.lines[e.row]
		for e.col < len(r2) && !isWordRune(r2[e.col]) {
			e.col++
		}
		for e.col < len(r2) && isWordRune(r2[e.col]) {
			e.col++
		}
	}
	e.wantCol, e.hasWantCol = e.col, true
}

// WordLeft moves to the start of the word to the left, crossing to the end of
// the previous line at the boundary.
//
// It moves exactly ONE word: skip the separators, then skip the word. The caret
// ending at a word start is "still in that word", which is what makes the pair
// alt+b / ctrl+w delete that word — the same convention every readline uses.
func (e *editor) WordLeft() {
	r := e.lines[e.row]
	for e.col > 0 && !isWordRune(r[e.col-1]) {
		e.col--
	}
	for e.col > 0 && isWordRune(r[e.col-1]) {
		e.col--
	}
	if e.col == 0 && e.row > 0 {
		e.row--
		e.col = len(e.lines[e.row])
	}
	e.wantCol, e.hasWantCol = e.col, true
}

func isWordRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') || r >= utf8.RuneSelf
}

// ── editing ─────────────────────────────────────────────────────────────────

// Insert puts text at the caret; "\n" starts a new line.
func (e *editor) Insert(s string) {
	e.pushUndo()
	e.redo = nil
	e.insertNoUndo(s)
}

func (e *editor) insertNoUndo(s string) {
	if s == "" {
		return
	}
	parts := strings.Split(s, "\n")
	head, tail := e.lines[e.row][:e.col], e.lines[e.row][e.col:]
	if len(parts) == 1 {
		e.lines[e.row] = append(append([]rune{}, head...), append([]rune(parts[0]), tail...)...)
		e.col += len([]rune(parts[0]))
		return
	}
	// A multi-line insert: the current line's head, then each new line, then the
	// tail on the last one.
	var mid [][]rune
	for _, p := range parts[1 : len(parts)-1] {
		mid = append(mid, []rune(p))
	}
	first := append(append([]rune{}, head...), []rune(parts[0])...)
	last := append(append([]rune{}, []rune(parts[len(parts)-1])...), tail...)
	e.lines[e.row] = first
	out := append([][]rune{}, e.lines[:e.row+1]...)
	out = append(out, mid...)
	out = append(out, last)
	out = append(out, e.lines[e.row+1:]...)
	e.lines = out
	e.row += len(parts) - 1
	e.col = len([]rune(parts[len(parts)-1]))
	e.wantCol, e.hasWantCol = e.col, true
}

// Backspace deletes one rune, or a whole atomic token, before the caret.
func (e *editor) Backspace() bool {
	if e.col == 0 {
		if e.row == 0 {
			return false
		}
		e.pushUndo()
		// Join with the line above: its tokens move down by one line.
		prev := e.lines[e.row-1]
		e.col = len(prev)
		e.lines[e.row-1] = append(prev, e.lines[e.row]...)
		e.shiftTokens(e.row, -1)
		e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
		e.row--
		return true
	}
	if e.deleteTokenBefore() {
		return true
	}
	r := e.lines[e.row]
	if e.col > len(r) {
		return false
	}
	e.pushUndo()
	e.lines[e.row] = append(append([]rune{}, r[:e.col-1]...), r[e.col:]...)
	e.col--
	e.wantCol, e.hasWantCol = e.col, true
	return true
}

// DeleteWordBack removes the word before the caret (ctrl+w, ctrl+backspace):
// the caret moves to the word boundary and everything between is gone.
func (e *editor) DeleteWordBack() {
	end := e.absCaret()
	e.WordLeft()
	if e.absCaret() >= end {
		return // nothing to remove (the caret was already at a boundary)
	}
	e.deleteRange(e.absCaret(), end)
}

// DeleteToLineEnd / DeleteToLineStart are ctrl+k and ctrl+u.
func (e *editor) DeleteToLineEnd() {
	start := e.absCaret()
	e.LineEnd()
	e.deleteRange(start, e.absCaret())
}

func (e *editor) DeleteToLineStart() {
	end := e.absCaret()
	e.LineStart()
	e.deleteRange(e.absCaret(), end)
}

// DeleteForward removes one rune, or a whole token, after the caret (ctrl+d).
func (e *editor) DeleteForward() bool {
	r := e.lines[e.row]
	if e.col >= len(r) {
		if e.row >= len(e.lines)-1 {
			return false
		}
		e.pushUndo()
		e.lines[e.row] = append(r, e.lines[e.row+1]...)
		e.shiftTokens(e.row+1, -1)
		e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
		return true
	}
	if e.deleteTokenAfter() {
		return true
	}
	e.pushUndo()
	e.lines[e.row] = append(append([]rune{}, r[:e.col]...), r[e.col+1:]...)
	return true
}

// deleteRange removes the runes between two absolute offsets.
func (e *editor) deleteRange(from, to int) {
	if to <= from {
		return
	}
	e.pushUndo()
	e.setRuneOffset(from)
	// Re-read the buffer and drop the runes one by one in the caret's own line
	// terms: a simpler implementation would rebuild the line array from scratch,
	// which is where off-by-one bugs around the token map come from.
	for to > from {
		if e.col >= len(e.lines[e.row]) {
			// The range reaches past this line: join with the next one.
			if e.row >= len(e.lines)-1 {
				break
			}
			e.lines[e.row] = append(e.lines[e.row], e.lines[e.row+1]...)
			e.shiftTokens(e.row+1, -1)
			e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
			to--
			continue
		}
		e.lines[e.row] = append(append([]rune{}, e.lines[e.row][:e.col]...), e.lines[e.row][e.col+1:]...)
		to--
	}
	e.clamp()
}

// ── atomic tokens ───────────────────────────────────────────────────────────

// AddToken inserts an atomic token at the caret. The visible placeholder is what
// the box shows and what is deleted whole; body/path are what travels to the model.
func (e *editor) AddToken(tk token) {
	e.Insert(tk.text)
	row := e.row
	start := e.col - len([]rune(tk.text))
	e.tokens[row] = append(e.tokens[row], tokenAt{start: start, token: tk})
}

// tokenAt places a token inside one line.
type tokenAt struct {
	start int
	token token
}

// tokensOnLine returns the tokens of a line whose start column survived every
// edit, re-anchored against the line's actual text.
func (e *editor) tokensOnLine(row int) []token {
	// Tokens are matched against the live line text so an undo (which restores a
	// stale map) or a paste that shifted the columns still finds the right ones.
	var out []token
	for _, at := range e.tokens[row] {
		offset := at.start
		if offset < 0 || offset > len(e.lines[row]) {
			continue
		}
		if offset+len([]rune(at.token.text)) > len(e.lines[row]) {
			continue
		}
		if string(e.lines[row][offset:offset+len([]rune(at.token.text))]) == at.token.text {
			out = append(out, at.token)
		}
	}
	return out
}

// AllTokens returns every token in the buffer, in reading order.
func (e *editor) AllTokens() []token {
	var out []token
	for row := range e.lines {
		out = append(out, e.tokensOnLine(row)...)
	}
	return out
}

// SelectTokens returns the paths of the token kinds the caller asks for, so the
// REPL can attach images and files to the message it sends.
func (e *editor) SelectTokens(kind tokenKind) []string {
	var out []string
	for _, t := range e.AllTokens() {
		if t.kind == kind && t.path != "" {
			out = append(out, t.path)
		}
	}
	return out
}

// deleteTokenBefore removes the whole token that ends at the caret, if any.
func (e *editor) deleteTokenBefore() bool {
	for _, at := range e.tokens[e.row] {
		end := at.start + len([]rune(at.token.text))
		if end == e.col {
			e.pushUndo()
			e.deleteRange(at.start, e.col)
			e.removeTokenAt(e.row, at.start)
			return true
		}
	}
	return false
}

// deleteTokenAfter removes the whole token that starts at the caret, if any.
func (e *editor) deleteTokenAfter() bool {
	for _, at := range e.tokens[e.row] {
		if at.start == e.col {
			e.pushUndo()
			e.deleteRange(e.col, e.col+len([]rune(at.token.text)))
			e.removeTokenAt(e.row, at.start)
			return true
		}
	}
	return false
}

func (e *editor) removeTokenAt(row, start int) {
	kept := e.tokens[row][:0]
	for _, at := range e.tokens[row] {
		if at.start != start {
			kept = append(kept, at)
		}
	}
	e.tokens[row] = kept
}

// shiftTokens moves every token at lines >= from by delta rows, so a line join
// or split keeps the map aligned with the text.
func (e *editor) shiftTokens(from, delta int) {
	next := make(map[int][]tokenAt, len(e.tokens))
	for row, toks := range e.tokens {
		if row >= from && delta < 0 {
			row += delta
		} else if row >= from && delta > 0 {
			row += delta
		}
		next[row] = toks
	}
	e.tokens = next
}

// splitRunes splits text into lines of runes, guaranteeing at least one line.
func splitRunes(s string) [][]rune {
	parts := strings.Split(s, "\n")
	out := make([][]rune, 0, len(parts))
	for _, p := range parts {
		out = append(out, []rune(p))
	}
	return out
}

// empty reports whether the buffer has no text at all.
func (e *editor) empty() bool {
	for _, l := range e.lines {
		if len(l) > 0 {
			return false
		}
	}
	return true
}

// pastedSummary summarizes a multi-line paste into one atomic token, the way
// OpenCode's prompt does (P:1461-1473): text with three or more lines, or more
// than 150 characters, becomes "[Pasted ~N lines]" instead of flooding the box.
// The whole text travels in the token's body and is sent on submit.
//
// Short single-line text is not summarized: it is inserted as typed.
func pastedSummary(s string) *token {
	if s == "" {
		return nil
	}
	lines := strings.Count(s, "\n") + 1
	if utf8.RuneCountInString(s) > 150 {
		if lines > 1 {
			return &token{kind: tokenPasted, text: pastedLabel(lines), body: s, lines: lines}
		}
		return &token{kind: tokenPasted, text: "[Pasted ~1 line]", body: s, lines: 1}
	}
	if lines < 3 {
		return nil
	}
	return &token{kind: tokenPasted, text: pastedLabel(lines), body: s, lines: lines}
}

// pastedLabel is the placeholder of a summarized paste.
func pastedLabel(lines int) string {
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	return "[Pasted ~" + itoa(lines) + " " + unit + "]"
}

// caretAtStart reports whether the caret sits at the very beginning of the
// buffer: history-up only applies there, so ↑ inside a line moves the caret
// instead of pulling a previous prompt back (OpenCode's rule, spec §7).
func (e *editor) caretAtStart() bool { return e.row == 0 && e.col == 0 }

// caretAtEnd reports whether the caret sits at the very end of the buffer.
func (e *editor) caretAtEnd() bool {
	return e.row == len(e.lines)-1 && e.col == len(e.lines[e.row])
}
