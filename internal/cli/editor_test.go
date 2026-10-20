// Editor tests: cursor movement, word/line deletion, multiline input, undo/redo
// and atomic attachment tokens. These are the primitives the prompt's keymap is
// built on, so each one is asserted at its boundary (start/end of buffer, empty
// lines, a caret that crosses from one line to the next).
package cli

import (
	"strings"
	"testing"
)

// typed inserts text through the same path a keypress would.
func typed(e *editor, s string) { e.Insert(s) }

// caret is the caret as "line:col" for a readable failure message.
func caret(e *editor) string { return itoa(e.row) + ":" + itoa(e.col) }

// Text round-trips whatever was typed, including newlines.
func TestEditorTextRoundTrip(t *testing.T) {
	e := newEditor()
	typed(e, "hello")
	if e.Text() != "hello" {
		t.Fatalf("text = %q", e.Text())
	}
	typed(e, " world")
	if e.Text() != "hello world" {
		t.Fatalf("text = %q", e.Text())
	}
	e.Insert("a\nb\nc")
	if e.Text() != "hello worlda\nb\nc" {
		t.Fatalf("multiline text = %q", e.Text())
	}
	if e.lineRowCount() != 3 {
		t.Fatalf("lines = %d", e.lineRowCount())
	}
}

// A caret measured in runes survives accented text and emoji.
func TestEditorRuneCaret(t *testing.T) {
	// "café" is 4 runes in 5 bytes: é is 2 bytes (U+00E9). A caret counted in
	// bytes would report 4 after typing all four, and a byte-wise backspace would
	// leave half of the é behind as a stray continuation byte.
	e := newEditor()
	typed(e, "café")
	if e.col != 4 {
		t.Fatalf("caret col after 4 runes (5 bytes) = %d, want 4", e.col)
	}
	e.Left() // the caret now sits between f and é
	if e.col != 3 {
		t.Fatalf("after left = %d", e.col)
	}
	e.Left() // now between a and f
	if e.col != 2 {
		t.Fatalf("second left = %d", e.col)
	}
	e.Backspace() // removes "a"; the é after the caret stays whole
	if e.Text() != "cfé" || e.col != 1 {
		t.Fatalf("backspace next to a multi-byte rune = %q col %d", e.Text(), e.col)
	}
	e.BufferEnd()
	e.Backspace() // removes the 2-byte é as one rune
	if e.Text() != "cf" || e.col != 2 {
		t.Fatalf("backspace a multi-byte rune = %q col %d", e.Text(), e.col)
	}

	// An emoji is one rune here: the caret must not land in its middle.
	e = newEditor()
	typed(e, "a\U0001F600b")
	if e.col != 3 {
		t.Fatalf("caret = %d, want 3", e.col)
	}
	e.Backspace()
	if e.Text() != "a\U0001F600" || e.col != 2 {
		t.Fatalf("emoji backspace = %q col %d", e.Text(), e.col)
	}
}

// Left/Right cross line boundaries: a caret at the start of a line moves to the
// end of the previous one.
func TestEditorMovementCrossLines(t *testing.T) {
	e := newEditor()
	typed(e, "ab\ncd")
	// caret is at the end of "cd" (1:2).
	if caret(e) != "1:2" {
		t.Fatalf("caret = %s", caret(e))
	}
	e.Left()
	if caret(e) != "1:1" {
		t.Fatalf("left = %s", caret(e))
	}
	e.Left()
	if caret(e) != "1:0" {
		t.Fatalf("left = %s", caret(e))
	}
	e.Left() // crosses to the end of the line above
	if caret(e) != "0:2" {
		t.Fatalf("cross-left = %s", caret(e))
	}
	e.Right() // crosses back down
	if caret(e) != "1:0" {
		t.Fatalf("cross-right = %s", caret(e))
	}
	e.Right()
	e.Right()
	if caret(e) != "1:2" {
		t.Fatalf("right to the end = %s", caret(e))
	}
	// The caret cannot leave the buffer in either direction.
	e.Left()
	e.Left()
	e.Left()
	e.Left()
	e.Left()
	e.Left()
	if caret(e) != "0:0" {
		t.Fatalf("clamped at the start = %s", caret(e))
	}
	for range 8 { // 5 reach the end (0:1 0:2 1:0 1:1 1:2); the rest must clamp
		e.Right()
	}
	if caret(e) != "1:2" {
		t.Fatalf("clamped at the end = %s", caret(e))
	}
}

// Up/Down keep the column they left from (wantCol), so walking off a long line
// and back returns to the same column instead of the shorter line's end.
func TestEditorVerticalKeepsColumn(t *testing.T) {
	e := newEditor()
	typed(e, "longer line here\nhi\nthird line ok")
	e.BufferStart()
	for range 11 {
		e.Right()
	}
	if caret(e) != "0:11" {
		t.Fatalf("caret = %s", caret(e))
	}
	e.Down() // short line: the column is clamped to its end (2)
	if caret(e) != "1:2" {
		t.Fatalf("down onto a short line = %s", caret(e))
	}
	e.Up() // back: the remembered column, not the short line's end
	if caret(e) != "0:11" {
		t.Fatalf("up must restore column 11, got %s", caret(e))
	}
	e.Down()
	e.Down()
	if caret(e) != "2:11" {
		t.Fatalf("down to the third line = %s", caret(e))
	}
	// At the first line, Up does nothing; at the last, Down does nothing.
	e.BufferStart()
	e.Up()
	if caret(e) != "0:0" {
		t.Fatalf("up at the top = %s", caret(e))
	}
	e.BufferEnd()
	e.Down()
	if !e.caretAtEnd() {
		t.Fatalf("down at the bottom = %s", caret(e))
	}
}

// Line start/end (ctrl+a/ctrl+e) and buffer start/end.
func TestEditorLineAndBuffer(t *testing.T) {
	e := newEditor()
	typed(e, "one\ntwo\nthree")
	e.LineEnd()
	if caret(e) != "2:5" {
		t.Fatalf("line end = %s", caret(e))
	}
	e.LineStart()
	if caret(e) != "2:0" {
		t.Fatalf("line start = %s", caret(e))
	}
	e.LineStart() // already at the start: stays
	if caret(e) != "2:0" {
		t.Fatalf("line start twice = %s", caret(e))
	}
	e.BufferEnd()
	if !e.caretAtEnd() {
		t.Fatalf("buffer end = %s", caret(e))
	}
	e.BufferStart()
	if !e.caretAtStart() {
		t.Fatalf("buffer start = %s", caret(e))
	}
}

// Word movement is readline-style: alt+f lands on the END of the next word and
// alt+b on the START of the previous one, so the two together bracket a word that
// ctrl+w can delete.
func TestEditorWordMovement(t *testing.T) {
	e := newEditor()
	typed(e, "one two   three")
	e.BufferStart()
	e.WordRight() // to the end of "one"
	if caret(e) != "0:3" {
		t.Fatalf("first word = %s", caret(e))
	}
	e.WordRight() // the run of spaces is skipped with the next word
	if caret(e) != "0:7" {
		t.Fatalf("second word = %s", caret(e))
	}
	e.WordRight()
	if caret(e) != "0:15" {
		t.Fatalf("third word = %s", caret(e))
	}
	// Back: one word at a time, to its start.
	e.WordLeft()
	if caret(e) != "0:10" {
		t.Fatalf("back to the start of three = %s", caret(e))
	}
	e.WordLeft()
	if caret(e) != "0:4" {
		t.Fatalf("back to the start of two = %s", caret(e))
	}
	e.WordLeft()
	if caret(e) != "0:0" {
		t.Fatalf("back to the start = %s", caret(e))
	}
	e.WordLeft() // at the start: stays
	if caret(e) != "0:0" {
		t.Fatalf("word-left at the start = %s", caret(e))
	}
	// A caret parked at the end of the text does not move forward.
	e.BufferEnd()
	e.WordRight()
	if caret(e) != "0:15" {
		t.Fatalf("word-right at the end = %s", caret(e))
	}
	// ctrl+w after alt+f deletes the word the caret just walked over.
	e.BufferStart()
	e.WordRight()
	e.DeleteWordBack()
	if e.Text() != " two   three" {
		t.Fatalf("bracketed delete = %q", e.Text())
	}
}

// Backspace deletes a rune, joins lines at a line start, and never leaves the
// buffer empty (a prompt with one empty line is still a valid buffer).
func TestEditorBackspace(t *testing.T) {
	e := newEditor()
	typed(e, "abc")
	e.Backspace()
	if e.Text() != "ab" {
		t.Fatalf("backspace = %q", e.Text())
	}
	e.Insert("\nsecond")
	e.LineStart()
	e.Backspace() // joins with the line above
	if e.Text() != "absecond" || e.lineRowCount() != 1 {
		t.Fatalf("join = %q lines %d", e.Text(), e.lineRowCount())
	}
	if caret(e) != "0:2" {
		t.Fatalf("caret after join = %s", caret(e))
	}
	// Deleting everything leaves one empty line, not a broken buffer.
	e.BufferEnd()
	for e.Backspace() {
	}
	if e.lineRowCount() != 1 || e.Text() != "" || !e.caretAtStart() {
		t.Fatalf("emptied buffer: %q lines %d", e.Text(), e.lineRowCount())
	}
	if e.Backspace() {
		t.Fatal("backspace on an empty buffer must report no change")
	}
}

// Word, line-end and line-start deletions.
func TestEditorDeletions(t *testing.T) {
	// Delete to the end of the line: ctrl+k.
	e := newEditor()
	typed(e, "keep this\nall of that goes")
	e.LineStart() // caret at the start of the second line
	e.DeleteToLineEnd()
	if e.Text() != "keep this\n" {
		t.Fatalf("ctrl+k = %q", e.Text())
	}
	// Delete to the start of the line: ctrl+u.
	e = newEditor()
	typed(e, "keep this\ndrop all of that")
	for range 3 {
		e.Down()
	}
	e.DeleteToLineStart()
	if e.Text() != "keep this\n" {
		t.Fatalf("ctrl+u = %q", e.Text())
	}
	// Delete a word back.
	e = newEditor()
	typed(e, "one two three")
	e.DeleteWordBack() // caret at the end: removes "three"
	if e.Text() != "one two " {
		t.Fatalf("ctrl+w = %q", e.Text())
	}
}

// DeleteForward removes a rune, or joins the next line at a line end (ctrl+d).
func TestEditorDeleteForward(t *testing.T) {
	e := newEditor()
	typed(e, "abc")
	e.BufferStart()
	e.DeleteForward()
	if e.Text() != "bc" {
		t.Fatalf("ctrl+d = %q", e.Text())
	}
	e = newEditor()
	typed(e, "one\ntwo")
	e.BufferStart()
	for range 3 {
		e.Right()
	}
	e.DeleteForward() // at the end of "one": joins with "two"
	if e.Text() != "onetwo" {
		t.Fatalf("ctrl+d join = %q", e.Text())
	}
	// At the very end it reports no change.
	e.BufferEnd()
	if e.DeleteForward() {
		t.Fatal("ctrl+d at the end must report no change")
	}
}

// Undo/redo cover every mutating operation, not just insertion.
func TestEditorUndoRedo(t *testing.T) {
	e := newEditor()
	if e.Undo() || e.Redo() {
		t.Fatal("an untouched buffer has nothing to undo or redo")
	}
	typed(e, "hello")
	e.Undo()
	if e.Text() != "" {
		t.Fatalf("undo insert = %q", e.Text())
	}
	e.Redo()
	if e.Text() != "hello" {
		t.Fatalf("redo insert = %q", e.Text())
	}
	// Undo of a deletion restores the text.
	e = newEditor()
	typed(e, "abcdef")
	e.BufferStart()
	e.DeleteForward()
	if e.Text() != "bcdef" {
		t.Fatalf("setup = %q", e.Text())
	}
	e.Undo()
	if e.Text() != "abcdef" {
		t.Fatalf("undo delete = %q", e.Text())
	}
	// A new mutation clears the redo stack: redo only follows an unbroken chain.
	typed(e, "!")
	e.Redo()
	if e.Redo() {
		t.Fatal("typing after an undo must drop the redo history")
	}
	// A structural change (a line join) survives undo.
	e = newEditor()
	typed(e, "ab\ncd")
	e.LineStart() // caret at the start of "cd"
	e.Backspace() // joins the lines
	if e.lineRowCount() != 1 {
		t.Fatalf("setup join = %d lines", e.lineRowCount())
	}
	e.Undo()
	if e.lineRowCount() != 2 || e.Text() != "ab\ncd" {
		t.Fatalf("undo join = %q lines %d", e.Text(), e.lineRowCount())
	}
}

// Attachment tokens are atomic: a backspace removes the whole placeholder, and
// the caret can never be left sitting inside one.
func TestEditorAtomicTokens(t *testing.T) {
	e := newEditor()
	typed(e, "look ")
	e.AddToken(token{kind: tokenImage, text: "[Image 1]", path: "a.png"})
	if e.Text() != "look [Image 1]" {
		t.Fatalf("text = %q", e.Text())
	}
	// The caret is after the token; one backspace removes it whole.
	e.Backspace()
	if e.Text() != "look " {
		t.Fatalf("atomic backspace = %q", e.Text())
	}
	// Undo brings the token back.
	e.Undo()
	if e.Text() != "look [Image 1]" {
		t.Fatalf("undo token = %q", e.Text())
	}
	// ctrl+d with the caret before the token removes it whole too.
	e.LineStart()
	for range 5 {
		e.Right()
	}
	if !e.DeleteForward() {
		t.Fatal("delete-forward on a token must report a change")
	}
	if e.Text() != "look " {
		t.Fatalf("atomic delete-forward = %q", e.Text())
	}
	// Only the tokens still present in the text are selected.
	e.Undo()
	got := e.SelectTokens(tokenImage)
	if len(got) != 1 || got[0] != "a.png" {
		t.Fatalf("select = %v", got)
	}
}

// A token is dropped from the selection once its placeholder is deleted, so the
// model never receives an image the user removed.
func TestEditorTokenSelectionFollowsText(t *testing.T) {
	e := newEditor()
	e.AddToken(token{kind: tokenImage, text: "[Image 1]", path: "a.png"})
	typed(e, " ")
	e.AddToken(token{kind: tokenImage, text: "[Image 2]", path: "b.png"})
	if got := e.SelectTokens(tokenImage); len(got) != 2 {
		t.Fatalf("select = %v", got)
	}
	// Delete "[Image 2]" and only the first path survives.
	e.Backspace()
	got := e.SelectTokens(tokenImage)
	if len(got) != 1 || got[0] != "a.png" {
		t.Fatalf("select after delete = %v", got)
	}
}

// A multi-line paste inserts as several lines, and its lines travel as one token.
func TestEditorMultilinePaste(t *testing.T) {
	e := newEditor()
	typed(e, "run this\n")
	e.Insert("first pasted\nsecond pasted\nthird pasted")
	if e.lineRowCount() != 4 {
		t.Fatalf("lines = %d", e.lineRowCount())
	}
	e.Insert("\nafter")
	if e.Text() != "run this\nfirst pasted\nsecond pasted\nthird pasted\nafter" {
		t.Fatalf("text = %q", e.Text())
	}
	e.BufferEnd()
	e.LineStart()
	e.Backspace() // joins "after" onto the pasted block
	if e.Text() != "run this\nfirst pasted\nsecond pasted\nthird pastedafter" {
		t.Fatalf("join = %q", e.Text())
	}
}

// Caret positions used by the history rule: ↑/↓ only leave the buffer at its
// very start/end.
func TestEditorCaretBoundariesMultiline(t *testing.T) {
	e := newEditor()
	typed(e, "one\ntwo")
	e.BufferStart()
	if !e.caretAtStart() {
		t.Fatal("buffer start")
	}
	for range 3 {
		e.Right()
	}
	if e.caretAtStart() {
		t.Fatal("a caret inside the first line is not at the start")
	}
	e.BufferStart()
	e.Down()
	if e.caretAtStart() {
		t.Fatal("a caret on line 1 of 2 is not at the buffer start")
	}
	e.BufferEnd()
	if !e.caretAtEnd() {
		t.Fatal("buffer end")
	}
	e.Left()
	if e.caretAtEnd() {
		t.Fatal("a caret before the last rune is not at the end")
	}
	// A single empty line is both the start and the end.
	e = newEditor()
	if !e.caretAtStart() || !e.caretAtEnd() {
		t.Fatal("an empty buffer is both boundaries at once")
	}
}

// Pasted blocks are summarized into one atomic token, as OpenCode does for text
// with three or more lines (or over 150 characters).
func TestPastedSummary(t *testing.T) {
	if got := pastedSummary(""); got != nil {
		t.Fatal("empty input yields no token")
	}
	if got := pastedSummary("just one line"); got != nil {
		t.Fatal("short single-line input is not summarized")
	}
	tok := pastedSummary("a\nb\nc")
	if tok == nil || tok.text != "[Pasted ~3 lines]" || !strings.Contains(tok.body, "c") {
		t.Fatalf("three-line paste = %+v", tok)
	}
	long := strings.Repeat("x", 151)
	tok = pastedSummary(long)
	if tok == nil || !strings.Contains(tok.text, "Pasted") {
		t.Fatalf("long single-line paste = %+v", tok)
	}
}
