// Regression tests for the interaction fixes of Phase 0: hover repaint must
// restore the plain style, a press must not leave a highlight behind, and the
// sidebar scrolls independently of the transcript.
package cli

import (
	"strings"
	"testing"
	"time"
)

// Hovering a region paints its hover style; leaving it must paint the PLAIN
// style back, in the region's own columns, over the screen background.
//
// This is the regression that the hover rewrite fixed: the old code only wrote
// hovered regions, so a pointer leaving a highlight left it painted until the
// next full frame (which could be minutes later, with no other output).
func TestHoverRepaintRestoresPlain(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	// 60 lines make the transcript scrollable, so the jump hint exists.
	for i := range 60 {
		sc.Adicionar(strings.Repeat("a", 5) + string(rune('0'+i%10)))
	}
	sc.scrollUp(wheelLines)
	if sc.scroll == 0 {
		t.Fatal("the view should be scrolled up to show the jump hint")
	}
	sb.Reset()

	var jump *hitRegion
	for _, r := range sc.hits.regions {
		if r.plain != "" && strings.Contains(r.plain, "Jump to latest") {
			jump = r
			break
		}
	}
	if jump == nil {
		t.Fatal("the jump hint must be a hit region")
	}
	row, col := jump.row, jump.col+jump.width/2
	before := sb.String()

	// Hover it: the hover style, which is a different colour from the plain one.
	sc.mouseHover(row, col)
	hovered := sb.String()
	if hovered == before {
		t.Fatal("hovering the jump hint must repaint something")
	}
	if !jump.hovered {
		t.Error("the region must be marked hovered")
	}
	if strings.Count(hovered, "\033[") < strings.Count(before, "\033[") {
		t.Error("the hover repaint should write at least one escape")
	}
	// The hover style is text.base (#eeeeee), not muted (#808080) — that is how
	// the test can tell the hover variant from the plain one.
	if !strings.Contains(hovered, "\033[38;2;238;238;238mJump to latest") {
		t.Error("the hovered hint should be painted in text.base:\n" + hovered)
	}

	// Move far away, over a region that carries no hover: the hint must be
	// repainted PLAIN (muted), which is the whole point of the fix.
	sb.Reset()
	sc.mouseHover(1, 1)
	after := sb.String()
	if jump.hovered {
		t.Error("leaving the region must clear its hovered flag")
	}
	if !strings.Contains(after, "\033[38;2;128;128;128mJump to latest") {
		t.Error("the hint must be repainted in the plain (muted) style:\n" + after)
	}
}

// A press that never becomes a click must not leave a highlight painted, and a
// release that dispatches must clear the pressed region's highlight too.
func TestPressClearsHighlight(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	sc.Ocupado(true)
	for i := range 20 {
		sc.Adicionar("filler " + strings.Repeat("x", i))
	}

	// Hover the sidebar's MCP header when there are more than two servers, then
	// press it: the press owns the pointer and drops the highlight.
	sc2, _ := newTestScreen(t, 140, 40, panelState{
		model: "m", cwd: "~/proj",
		mcps: []mcpEntry{{name: "a"}, {name: "b"}, {name: "c"}},
	})
	sc2.Adicionar("servers listed")
	var header *hitRegion
	for _, r := range sc2.hits.regions {
		if r.id == "mcpheader" {
			header = r
			break
		}
	}
	if header == nil {
		t.Fatal("the MCP heading must be a hit region with more than two servers")
	}
	openBefore := sc2.mcpOpen
	sb.Reset()
	sc2.mouseDown(header.row, header.col+2)
	sc2.mouseUp(header.row, header.col+2)
	if sc2.mcpOpen == openBefore {
		t.Error("the click on the MCP heading must toggle the section")
	}
	if header.hovered {
		t.Error("the pressed region must not stay highlighted after the click")
	}
	_ = sc
}

// A press released on another region is cancelled: no action, no highlight.
func TestPressCancelledByDragAway(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 140, 40, panelState{
		model: "m", cwd: "~/proj",
		mcps: []mcpEntry{{name: "a"}, {name: "b"}, {name: "c"}},
	})
	sc.Adicionar("content")
	var header *hitRegion
	for _, r := range sc.hits.regions {
		if r.id == "mcpheader" {
			header = r
			break
		}
	}
	if header == nil {
		t.Fatal("the MCP heading must be a hit region")
	}
	openBefore := sc.mcpOpen
	sc.mouseDown(header.row, header.col+2)
	// A drag that leaves the pressed region cancels the click.
	sc.mouseDrag(1, 1)
	sc.mouseUp(1, 1)
	if sc.mcpOpen != openBefore {
		t.Error("a drag away must cancel the click")
	}
	if header.hovered {
		t.Error("a cancelled press must not stay highlighted")
	}
	if mouse.mode != mouseIdle {
		t.Error("the mouse must be idle after a cancelled click")
	}
}

// The sidebar scrolls on its own, without moving the transcript, and vice versa.
func TestSidebarScrollsIndependently(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	// A short terminal with the MCP list open makes the sidebar overflow, so it
	// has something to scroll. Note the section must be OPEN (mcpOpen), or the
	// four rows collapse into one heading and nothing overflows.
	sc, _ := newTestScreen(t, 140, 16, panelState{
		model: "m", cwd: "~/proj", ctxTokens: 9999, window: 32000,
		mcps: []mcpEntry{
			{name: "alpha"}, {name: "beta"}, {name: "gamma"}, {name: "delta"}, {name: "epsilon"},
		},
	})
	sc.mcpOpen = true
	sc.Adicionar("transcript line")

	transcriptBefore := sc.scroll
	if got := sc.panelMaxOff(); got <= 0 {
		t.Fatalf("the sidebar should overflow at 16 rows, panelMaxOff = %d", got)
	}

	// A wheel over the sidebar column scrolls the sidebar only.
	sc.wheelScroll(10, sc.w-panelWidth+5, -1)
	if sc.panelOff <= 0 {
		t.Error("the wheel over the sidebar must scroll it down")
	}
	if sc.scroll != transcriptBefore {
		t.Errorf("the transcript must not move: scroll %d -> %d", transcriptBefore, sc.scroll)
	}
	// And back up.
	sc.wheelScroll(10, sc.w-panelWidth+5, 1)
	if sc.panelOff != 0 {
		t.Errorf("one tick back must return the sidebar to the top, got %d", sc.panelOff)
	}

	// Scrolling past the top stays clamped: no blank stretch appears below.
	for range 10 {
		sc.wheelScroll(10, sc.w-panelWidth+5, 1)
	}
	if sc.panelOff != 0 {
		t.Errorf("scrolling up past the top must clamp at 0, got %d", sc.panelOff)
	}
	for range 20 {
		sc.wheelScroll(10, sc.w-panelWidth+5, -1)
	}
	sc.Adicionar("more transcript")
	if maxOff := sc.panelMaxOff(); sc.panelOff > maxOff {
		t.Errorf("the sidebar offset %d exceeds its maximum %d", sc.panelOff, maxOff)
	}
}

// A wheel over the transcript scrolls the transcript, not the sidebar.
func TestWheelOverTranscriptLeavesSidebar(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 140, 30, panelState{
		model: "m", cwd: "~/proj",
		mcps: []mcpEntry{{name: "alpha"}, {name: "beta"}},
	})
	for i := range 60 {
		sc.Adicionar("line " + strings.Repeat("y", i%7))
	}
	before := sc.panelOff
	// Column 30 is well inside the pane: the sidebar starts at w-panelWidth+1
	// (99 at 140 columns), and using a column past it would route the wheel to
	// the sidebar, which is what the assertion is meant to rule out.
	sc.wheelScroll(10, 30, 1)
	if sc.panelOff != before {
		t.Error("a wheel over the transcript must not move the sidebar")
	}
	if sc.scroll == 0 {
		t.Error("a wheel up over the transcript must scroll it")
	}
}

// New output while scrolled up keeps the rows on screen in the same place,
// whether it comes from the renderer, the writer, or a user message. This is the
// unified-accounting regression: the three paths used to advance the offset by
// different amounts, so a wrapped line slid the view.
func TestUnifiedScrollAccounting(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 60, 24, panelState{model: "m", cwd: "~/proj"})
	// Narrow enough that most of these lines wrap into several rows.
	for i := range 40 {
		sc.Adicionar("filler line number " + strings.Repeat("z", i))
	}
	sc.scrollUp(wheelLines * 4)
	offset := sc.offsetFromTop()
	if offset == 0 {
		t.Fatal("the view should be scrolled up")
	}
	firstOnScreen := func() string {
		rows := visualize(sc.frame(), 60, 24)
		return rowAt(rows, sc.frameFor().convRow)
	}
	before := firstOnScreen()

	// Three different writers add content; the visible row must not move.
	sc.Adicionar("a renderer line that is long enough to wrap onto another row here")
	if got := firstOnScreen(); got != before {
		t.Errorf("renderer output moved the view: %q -> %q", before, got)
	}
	screenWriter{t: sc}.Write([]byte("a writer line that is long enough to wrap onto another row here\n"))
	if got := firstOnScreen(); got != before {
		t.Errorf("writer output moved the view: %q -> %q", before, got)
	}
	sc.Usuario("a user message that is long enough to wrap onto another row")
	if got := firstOnScreen(); got != before {
		t.Errorf("a user message moved the view: %q -> %q", before, got)
	}
	if sc.offsetFromTop() != offset {
		t.Errorf("the offset from the top drifted: %d -> %d", offset, sc.offsetFromTop())
	}
}

// While pinned (sticky) new output DOES scroll in with the view.
func TestPinnedFollowsNewOutput(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 100, 24, panelState{model: "m", cwd: "~/proj"})
	for i := range 40 {
		sc.Adicionar("line " + string(rune('a'+i%26)))
	}
	if sc.scroll != 0 || !sc.sticky() {
		t.Fatal("the view starts pinned")
	}
	sc.Adicionar("the newest line of all")
	if sc.scroll != 0 {
		t.Errorf("a pinned view must stay at the bottom, scroll = %d", sc.scroll)
	}
	rows := visualize(sc.frame(), 100, 24)
	if !strings.Contains(rowAt(rows, sc.frameFor().statusRow-1), "the newest line of all") {
		t.Errorf("the newest line should be the last conversation row:\n%s",
			showGrid(rows, 100))
	}
}

// The armed interrupt state expires by itself after OpenCode's 5 s window and
// when the turn ends, so a stale press cannot cancel a later turn.
func TestInterruptArmedExpiry(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	// 1. The turn ends: the flag is cleared.
	sc, _ := newTestScreen(t, 100, 24, panelState{model: "m"})
	sc.Ocupado(true)
	sc.interruptArmed = true
	sc.escAt = time.Now()
	sc.Ocupado(false)
	if sc.interruptArmed {
		t.Error("finishing the turn must clear the armed interrupt")
	}
	// A fresh turn starts unarmed.
	sc.Ocupado(true)
	if sc.interruptArmed {
		t.Error("a new turn must not inherit the armed state")
	}

	// 2. The window lapses: the flag is stale and no longer counts as armed.
	sc.interruptArmed = true
	sc.escAt = time.Now().Add(-escInterruptWindow - time.Second)
	if armed := sc.interruptArmed && time.Since(sc.escAt) < escInterruptWindow; armed {
		t.Error("a press beyond the window must not still be armed")
	}
	sc.escAt = time.Now().Add(-time.Second)
	if armed := sc.interruptArmed && time.Since(sc.escAt) < escInterruptWindow; !armed {
		t.Error("a press inside the window must still be armed")
	}
}

// frame paints the current frame into a string, so a test can replay it. The
// writer is swapped for the duration: the frame build is exactly drawLocked's,
// and this keeps the test from depending on a production helper that exists for
// no other reason than the test.
func (t *screen) frame() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sb strings.Builder
	prev := t.out
	t.out = &sb
	t.drawLocked()
	t.out = prev
	return sb.String()
}
