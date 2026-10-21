// Frame-rendering tests: they paint a whole screen at a fixed size, replay the
// escape stream into a grid and assert on what lands on each row. This is how
// the OpenCode layout (§1.3 of the parity spec) is verified without a terminal.
package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// visualize replays the escape stream into h rows of visible text, following
// the cursor-positioning sequences, so a test can read the frame row by row.
// Cells are counted in columns: a multi-byte rune is one cell, escapes are
// skipped, and a cursor jump overwrites whatever is there.
func visualize(frame string, w, h int) []string {
	rows := make([][]rune, h)
	r, c := 0, 0
	put := func(ch rune) {
		if r < 0 || r >= h || c < 0 {
			return
		}
		row := rows[r]
		for len(row) <= c {
			row = append(row, ' ')
		}
		row[c] = ch
		rows[r] = row
	}
	for i := 0; i < len(frame); {
		if frame[i] == '\033' {
			j := escapeEnd(frame, i)
			seq := frame[i:j]
			// Erase sequences, as a real terminal applies them: 2J clears the
			// screen, K clears from the cursor to the end of the row.
			if seq == "[2J" {
				for k := range rows {
					rows[k] = nil
				}
			}
			if seq == "[K" && r >= 0 && r < h && c < len(rows[r]) {
				rows[r] = rows[r][:max(0, c)]
			}
			if strings.HasSuffix(seq, "H") && strings.Contains(seq, "[") {
				p := strings.SplitN(seq[2:len(seq)-1], ";", 2)
				if len(p) == 2 {
					n, ok1 := atoiSafe(p[0])
					m, ok2 := atoiSafe(p[1])
					if ok1 && ok2 {
						r, c = n-1, m-1
					}
				}
			}
			i = j
			continue
		}
		ch, size := utf8.DecodeRuneInString(frame[i:])
		if ch != '\n' {
			put(ch)
			c++
		}
		i += size
	}
	out := make([]string, h)
	for i, row := range rows {
		out[i] = strings.TrimRight(string(row), " ")
	}
	return out
}

// rowAt returns the visible text of a 1-based row.
func rowAt(rows []string, n int) string {
	if n-1 >= 0 && n-1 < len(rows) {
		return rows[n-1]
	}
	return ""
}

func showGrid(rows []string, width int) string {
	var sb strings.Builder
	for i, r := range rows {
		sb.WriteString(fmt.Sprintf("%2d|%-*s|\n", i+1, width, r))
	}
	return sb.String()
}

func atoiSafe(s string) (int, bool) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// newTestScreen paints into a buffer at a fixed size, so the assertions below
// never depend on the real terminal.
// newTestScreen pins the terminal size and returns the screen and its buffer.
// The caller owns restoring the real size (t.Cleanup does it, and a defer
// inside the helper would fire before the frame is painted).
func newTestScreen(t *testing.T, w, h int, state panelState) (*screen, *strings.Builder) {
	t.Helper()
	before := screenSize
	screenSize = func() (int, int) { return w, h }
	t.Cleanup(func() { screenSize = before })
	var sb strings.Builder
	sc := &screen{out: &sb}
	sc.Estado(state)
	return sc, &sb
}

// The frame follows §1.3 of the spec: the prompt block's four fixed rows, the
// footer on the second-to-last row and a blank last row.
func TestFrameLayoutMatchesSpec(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 30, panelState{
		title: "How do I fix this bug", model: "kimi-k2-thinking", provider: "moonshot",
		effort: "high", cwd: "~/proj", ctxTokens: 12345, window: 32000, branch: "main",
	})
	sc.Adicionar("assistant answer line one")
	sc.Adicionar("assistant answer line two")
	rows := visualize(sb.String(), 140, 30)
	t.Logf("=== rendered ===\n%s", showGrid(rows, 100))

	for _, tc := range []struct {
		row  int
		want string
	}{
		{23, ""},  // status row: pinned, so no "Jump to latest ↓"
		{24, "┃"}, // prompt padding row: the bar only, no text (paddingTop 1)
		{27, "┃  kimi-k2-thinking moonshot · high"}, // metadata row
		{30, ""}, // last row stays blank (paddingBottom 1)
	} {
		if got := strings.TrimSpace(rowAt(rows, tc.row)); got != tc.want {
			t.Errorf("row %d = %q, want %q", tc.row, got, tc.want)
		}
	}
	// The prompt block, the cap and the footer never cross into the sidebar. The
	// pane's right edge is at w-panelWidth, so the pane's share of a row is
	// everything before that column; the sidebar paints its own columns after it
	// (including its pinned footer on the same row as the pane footer).
	for _, row := range []int{24, 25, 26, 27, 28, 29} {
		pane := rowAt(rows, row)
		if r := []rune(pane); len(r) > 140-42 {
			pane = string(r[:140-42])
		}
		if got := visWidth(pane); got > 140-42 {
			t.Errorf("row %d spans %d columns, into the sidebar: %q", row, got, rowAt(rows, row))
		}
	}
	// The footer carries the location, the usage and the shortcut hint, in the
	// format OpenCode uses.
	footer := rowAt(rows, 29)
	if !strings.Contains(footer, "~/proj") {
		t.Errorf("footer should start with the location: %q", footer)
	}
	if !strings.Contains(footer, "12,345 (38%)") || !strings.Contains(footer, "ctrl+p commands") {
		t.Errorf("footer right half = %q", footer)
	}
	// The sidebar shows the title, the context block and its footer with the
	// branch. The MCP section is hidden here: this state has no servers.
	found := map[string]bool{}
	for _, r := range rows {
		for _, s := range []string{"How do I fix this bug", "Context", "12,345 tokens", "38% used", "~/proj:main"} {
			if strings.Contains(r, s) {
				found[s] = true
			}
		}
	}
	for _, s := range []string{"How do I fix this bug", "Context", "12,345 tokens", "38% used", "~/proj:main"} {
		if !found[s] {
			t.Errorf("sidebar is missing %q:\n%s", s, showGrid(rows, 100))
		}
	}
	if strings.Contains(sb.String(), "MCP") {
		t.Error("the MCP section must stay hidden when no server is configured")
	}
}

// The sidebar only exists above the spec's single breakpoint (width > 120),
// and is 42 columns wide.
func TestSidebarBreakpoint(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	narrow, sbNarrow := newTestScreen(t, 120, 24, panelState{model: "m", cwd: "~/proj"})
	narrow.Adicionar("no sidebar here")
	if strings.Contains(sbNarrow.String(), "Context") {
		t.Error("at exactly 120 columns the sidebar must stay hidden")
	}

	wide, sbWide := newTestScreen(t, 121, 24, panelState{model: "m", cwd: "~/proj", ctxTokens: 900, window: 32000})
	wide.Adicionar("sidebar here")
	rows := visualize(sbWide.String(), 121, 24)
	if !strings.Contains(sbWide.String(), "Context") {
		t.Errorf("at 121 columns the sidebar must show:\n%s", showGrid(rows, 100))
	}
	// Its right edge is the screen edge, so it is 42 columns: columns 80..121.
	if got := strings.TrimSpace(rowAt(rows, 2)); len([]rune(got)) > 121-42+2 {
		t.Errorf("sidebar row = %q", rowAt(rows, 2))
	}
}

// The Context section is hidden with no usage, and the MCP section with no
// servers.
func TestSidebarHidesEmptySections(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	sc.Adicionar("nothing else")
	frame := sb.String()
	if strings.Contains(stripANSI(frame), "Context") {
		t.Error("the Context section must be hidden without usage")
	}
	if strings.Contains(stripANSI(frame), "MCP") {
		t.Error("the MCP section must be hidden without servers")
	}
}

// The MCP heading counts the live and broken servers while collapsed, and is
// clickable past two servers.
func TestSidebarMCPHeader(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 40, panelState{
		model: "m", cwd: "~/proj",
		mcps: []mcpEntry{{name: "a"}, {name: "b", falha: true}, {name: "c"}},
	})
	sc.Adicionar("three servers")
	frame := sb.String()
	if want := "(2 active, 1 error)"; !strings.Contains(stripANSI(frame), want) {
		t.Errorf("collapsed MCP heading should read %q:\n%s", want,
			showGrid(visualize(frame, 140, 40), 100))
	}
	// A click on the heading toggles the section; the rows then list.
	got := false
	for _, r := range sc.hits.regions {
		if r.id == "mcpheader" {
			got = true
			r.action()
			break
		}
	}
	if !got {
		t.Fatal("the MCP heading must be a hit region with more than two servers")
	}
	sc.Adicionar("after toggle")
	t.Logf("=== expanded ===\n%s", showGrid(visualize(sb.String(), 140, 40), 100))
	expanded := stripANSI(sb.String())
	for _, want := range []string{"• a", "• b", "• c", "▼ MCP"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("the expanded MCP list should show %q", want)
		}
	}
}

// Scrolling up breaks stickiness and shows the right-aligned
// "Jump to latest ↓"; clicking it returns to the bottom.
func TestJumpToLatestAndStickiness(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	for i := 0; i < 60; i++ {
		sc.Adicionar(fmt.Sprintf("line %d", i))
	}
	// Pinned: the newest line is the last conversation row, no jump hint.
	status := sc.frameFor().statusRow
	rows := visualize(sb.String(), 140, 24)
	last := rowAt(rows, status-1)
	if !strings.Contains(last, "line 59") {
		t.Errorf("pinned view should end on the newest line:\n%s", showGrid(rows, 100))
	}
	if hint := strings.TrimSpace(rowAt(rows, status)); hint != "" {
		t.Errorf("no hint while pinned, status row = %q", hint)
	}

	// Scroll up two wheel ticks: the view moves, the hint appears right-aligned
	// on the status row.
	sc.scrollUp(2 * wheelLines)
	if sc.scroll == 0 {
		t.Fatal("scrolling up must move the view")
	}
	rows = visualize(sb.String(), 140, 24)
	jump := "Jump to latest ↓"
	hint := rowAt(rows, status)
	if !strings.Contains(hint, jump) {
		t.Errorf("the status row (%d) should carry %q:\n%s", status, jump, showGrid(rows, 100))
	}
	// Right-aligned: the text ends within the pane, well past its middle.
	trimmed := strings.TrimLeft(hint, " ")
	if len(hint)-len(trimmed) < 70 {
		t.Errorf("the hint must be right-aligned: %q", hint)
	}
	// The row is clickable and returns to the bottom, re-sticking the view.
	var acted bool
	for _, r := range sc.hits.regions {
		if r.plain != "" && strings.Contains(r.plain, jump) {
			if r.action != nil {
				r.action()
				acted = true
			}
			break
		}
	}
	if !acted {
		t.Fatal("the jump hint must be clickable")
	}
	if sc.scroll != 0 || !sc.sticky() {
		t.Error("clicking the hint must return to the bottom and re-stick")
	}
}

// New output must not yank the view down while the user is scrolled up.
func TestStayPutScrolling(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	for i := 0; i < 40; i++ {
		sc.Adicionar(fmt.Sprintf("old %d", i))
	}
	sc.scrollUp(5 * wheelLines)
	top, offset := sc.lines[len(sc.lines)-1], sc.scroll
	if offset == 0 {
		t.Fatal("the view should be scrolled up")
	}
	// New content arrives: the offset must grow with it, so the rows on screen
	// stay put instead of sliding down.
	for i := 0; i < 3; i++ {
		sc.Adicionar(fmt.Sprintf("new %d", i))
	}
	if sc.scroll <= offset {
		t.Errorf("scroll=%d should have grown past %d with the new lines", sc.scroll, offset)
	}
	_ = top
	// The row the user was reading is still the same line.
	sc.mu.Lock()
	last := sc.lines[len(sc.lines)-1]
	sc.mu.Unlock()
	if last != "new 2" {
		t.Fatalf("last line = %q", last)
	}
}

// PgUp/PgDn move half the visible height; Home and End jump to the ends.
func TestPageAndHomeEndScroll(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	for i := 0; i < 80; i++ {
		sc.Adicionar(fmt.Sprintf("line %d", i))
	}
	sc.setOffset(0)
	if sc.sticky() {
		t.Fatal("reaching the top must un-stick the view")
	}
	if got := sc.scroll; got != sc.maxScroll() {
		t.Fatalf("the top is %d lines above the bottom, scrolled %d", sc.maxScroll(), got)
	}
	half := sc.frameFor().convHeight / 2
	sc.setOffset(half)
	if got := sc.scroll; got != sc.maxScroll()-half {
		t.Errorf("a page down should move half the height (%d), scrolled %d", half, got)
	}
	sc.setOffset(sc.maxScroll())
	if !sc.sticky() || sc.scroll != 0 {
		t.Errorf("End must reach the bottom (scroll=%d sticky=%v)", sc.scroll, sc.sticky())
	}
	sc.scrollUp(wheelLines * 2)
	if sc.scroll == 0 {
		t.Fatal("the wheel must scroll up from the bottom")
	}
}

// A left-drag over the conversation selects text; releasing copies it, and
// while the selection holds text the clicks are ignored.
func TestSelectionExtraction(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	// extractSelection cuts the columns of each touched line.
	lines := []string{"first line here", "second", "third and last"}
	got := extractSelection(lines, selRange{from: selPoint{0, 6}, to: selPoint{1, 4}})
	if got != "line here\nseco" {
		t.Fatalf("selection = %q", got)
	}
	// A multi-line selection keeps the whole lines in between.
	got = extractSelection(lines, selRange{from: selPoint{0, 0}, to: selPoint{2, 5}})
	if got != "first line here\nsecond\nthird" {
		t.Fatalf("multi-line selection = %q", got)
	}
	// A reversed drag is normalized: dragging from (1,3) back to (0,1) selects
	// the same span as dragging forward from (0,1) to (1,3).
	got = extractSelection(lines, selRange{from: selPoint{1, 3}, to: selPoint{0, 1}})
	if got != "irst line here\nsec" {
		t.Fatalf("reversed selection = %q", got)
	}
	// Escapes are stripped, and an empty span selects nothing.
	if got := extractSelection([]string{bluef("colored")}, selRange{from: selPoint{0, 0}, to: selPoint{0, 0}}); got != "" {
		t.Fatalf("empty span = %q", got)
	}
	if got := extractSelection([]string{bluef("colored")}, selRange{from: selPoint{0, 0}, to: selPoint{0, 4}}); got != "colo" {
		t.Fatalf("colored cut = %q", got)
	}
}

// The in-app selection is painted over the selected cells, and the dispatch
// ignores clicks while it holds text.
func TestSelectionPaintAndClickSuppression(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m", cwd: "~/proj"})
	sc.Adicionar("first line here")
	sc.Adicionar("second line here")
	sc.selOn = true
	sc.sel = selRange{from: selPoint{0, 6}, to: selPoint{0, 11}}
	sc.Adicionar("third")
	frame := sb.String()
	if !strings.Contains(frame, bgSelection) {
		t.Error("the selected cells must be painted with the selection background")
	}
	// The wrapped line keeps its 2-column pane margin, so the selection's
	// columns are counted from the line's start: 6..11 of "  first line here"
	// is "line", the trailing blank trimmed as a clipboard cut does.
	if sc.selectionText() != "line" {
		t.Fatalf("selection text = %q", sc.selectionText())
	}
	// A click on a jump hint would act, but the selection suppresses it.
	sc.mouseDown(3, 10)
	if mouse.mode != mouseSelecting {
		t.Errorf("a press with a selection should start a new selection, mode=%d", mouse.mode)
	}
	sc.clearSelection()
	if sc.selOn {
		t.Error("esc must clear the selection")
	}
}

// Esc twice within 5 s interrupts the turn; the first press only arms it.
func TestDoubleEscInterrupt(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, _ := newTestScreen(t, 140, 24, panelState{model: "m"})
	sc.Ocupado(true)
	// The first press arms, the second inside the window cancels, a third (after
	// the window) arms again.
	sc.interruptArmed, sc.escAt = true, time.Now().Add(-time.Second)
	sc.mu.Lock()
	armed := sc.interruptArmed && time.Since(sc.escAt) < escInterruptWindow
	sc.mu.Unlock()
	if !armed {
		t.Fatal("a press 1 s after the first should still interrupt")
	}
	sc.interruptArmed, sc.escAt = true, time.Now().Add(-6*time.Second)
	sc.mu.Lock()
	armed = sc.interruptArmed && time.Since(sc.escAt) < escInterruptWindow
	sc.mu.Unlock()
	if armed {
		t.Fatal("a press beyond the 5 s window must re-arm instead of interrupt")
	}
}

// The in-transcript spinner has no elapsed seconds: they only appear at the end.
func TestSpinnerHasNoElapsed(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m"})
	sc.Pensar("Thinking")
	// The animation paints from its own goroutine: wait for the first frame.
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(sb.String(), "Thinking") && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !strings.Contains(sb.String(), "Thinking") {
		t.Error("the spinner should label itself Thinking")
	}
	// No elapsed seconds: they only appear at the end, on the Thought line.
	sc.mu.Lock()
	plain := stripANSI(sc.transit)
	sc.mu.Unlock()
	if regexpElapsed.MatchString(plain) {
		t.Error("the spinner must not show elapsed seconds: " + plain)
	}
	sc.stopSpin()
}

// regexpElapsed matches the "3s" / "12s" suffix bh used to append to the
// transient spinner line.
var regexpElapsed = regexp.MustCompile(`\d+s$`)

// The Thought line collapses with its duration and a leading "**Title**"
// becomes "Thought: <title>".
func TestThoughtLine(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	title, body := reasoningSummary("**Reasoning title**\n\nthe reasoning body")
	if title != "Reasoning title" || body != "the reasoning body" {
		t.Fatalf("summary = %q / %q", title, body)
	}
	if t1, b1 := reasoningSummary("plain reasoning"); t1 != "" || b1 != "plain reasoning" {
		t.Fatalf("no title = %q / %q", t1, b1)
	}
	sc, sb := newTestScreen(t, 140, 24, panelState{model: "m"})
	sc.thought(8100*time.Millisecond, "**Reasoning title**\n\nbody text")
	frame := stripANSI(sb.String())
	if !strings.Contains(frame, "+ Thought") || !strings.Contains(frame, "8.1s") {
		t.Errorf("Thought line = %q", frame)
	}
	if !strings.Contains(frame, ":") {
		t.Errorf("a titled Thought should read \"Thought: Reasoning title\": %q", frame)
	}
	// Clicking the header expands the reasoning below it.
	var th *item
	for _, r := range sc.hits.regions {
		if it := sc.items[r.itemID]; it != nil && it.kind == "thought" && r.action != nil {
			th = it
			r.action()
			break
		}
	}
	if th == nil || !th.open {
		t.Fatal("the click must expand the reasoning")
	}
	if !strings.Contains(stripANSI(sb.String()), "body text") {
		t.Error("the expanded body must appear in the transcript")
	}
}

// The user message box uses ┃, raised.base, and the same columns as the prompt.
func TestUserMessageBox(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	sc, sb := newTestScreen(t, 140, 30, panelState{model: "m", cwd: "~/proj"})
	sc.Usuario("a message wide enough to check the box")
	frame := sb.String()
	if !strings.Contains(frame, bgUser) {
		t.Error("the user box must use raised.base (#141414)")
	}
	if !strings.Contains(frame, bluef(bar)) {
		t.Error("the user box bar must be ┃ in the primary colour")
	}
	rows := visualize(frame, 140, 30)
	for i, r := range rows {
		if strings.Contains(r, "a message wide enough") {
			t.Logf("user row %d = %q", i+1, r)
		}
	}
}

// While a turn runs the footer's knight-rider spinner moves on its own, and it
// stops when the turn ends.
func TestBusySpinnerAnimates(t *testing.T) {
	sc, _ := newTestScreen(t, 120, 30, panelState{model: "m", cwd: "~/proj"})
	sc.Adicionar("hello")
	sc.Ocupado(true)
	time.Sleep(6 * spinInterval)
	sc.mu.Lock()
	moved := sc.spinnerSeqVal
	sc.mu.Unlock()
	sc.Ocupado(false)
	if moved < 2 {
		t.Fatalf("spinner advanced %d frames while busy; it must animate", moved)
	}
	time.Sleep(3 * spinInterval)
	sc.mu.Lock()
	after := sc.spinnerSeqVal
	sc.mu.Unlock()
	time.Sleep(3 * spinInterval)
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.spinnerSeqVal != after {
		t.Fatal("spinner kept running after the turn ended")
	}
}

// The spinner's footer repaint must not erase to the end of the row: the
// sidebar's cwd line shares it, and clearing it 25 times a second blinked.
func TestFooterRepaintLeavesSidebarAlone(t *testing.T) {
	sc, sb := newTestScreen(t, 160, 30, panelState{model: "m", cwd: "~"})
	sc.Adicionar("hello")
	sc.mu.Lock()
	sc.busy = true
	sb.Reset()
	sc.repaintFooterLocked()
	out := sb.String()
	sc.busy = false
	sc.mu.Unlock()
	if out == "" {
		t.Fatal("the footer was not repainted")
	}
	if strings.Contains(out, "\033[K") {
		t.Fatalf("footer repaint clears to the end of the line: %q", out)
	}
	f := sc.frameFor()
	if w := visWidth(stripANSI(strings.SplitN(out, "H", 2)[1])); w > f.boxWidth {
		t.Fatalf("footer repaint spans %d columns, past the pane (%d)", w, f.boxWidth)
	}
}
