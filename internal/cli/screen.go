// Full-screen mode in the OpenCode layout: scrolling conversation in the
// middle, side panel (title, context, MCPs), fixed input box at the bottom and
// a footer with directory, context and shortcuts.
//
// How it works: the usual REPL runs in a goroutine, reading from a pipe and
// writing to a writer whose lines become conversation lines. The keyboard is
// read in raw mode by the main goroutine, which edits the input box and sends
// the line to the pipe on Enter. That way every command — including the ones
// that ask something, like /model and /provider — works without a rewrite.
//
// Only enabled when stdin and stdout are real terminals; BH_SIMPLES=1 forces
// scrolling mode. Pipes, -p and tests never come through here.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"golang.org/x/term"
)

var ioPipe = io.Pipe

// Terminal modes OpenCode enables: alt screen, bracketed paste, and mouse with
// press + drag + any-motion reporting in SGR encoding (plus focus reporting).
// Everything is undone on exit, so the shell comes back exactly as it was.
const (
	escAlt        = "\033[?1049h"
	escNormal     = "\033[?1049l"
	escMouseOn    = "\033[?1000h\033[?1002h\033[?1003h\033[?1006h"
	escMouseOff   = "\033[?1006l\033[?1003l\033[?1002l\033[?1000l"
	escFocusOn    = "\033[?1004h"
	escFocusOff   = "\033[?1004l"
	escPasteOn    = "\033[?2004h" // bracketed paste: \033[200~...\033[201~
	escPasteOff   = "\033[?2004l"
	pasteStart    = "\033[200~"
	pasteEnd      = "\033[201~"
	escSyncStart  = "\033[?2026h" // paints the whole frame at once (no flicker)
	escSyncEnd    = "\033[?2026l"
	escClear      = "\033[2J"
	escClearLine  = "\033[K"
	escHideCursor = "\033[?25l"
	escShowCursor = "\033[?25h"
	maxLines      = 5000
)

// Box backgrounds from the theme: the sidebar and the user message use
// raised.base (#141414); their hover uses the prompt box colour (#1e1e1e).
const (
	bgPanel     = bgRaised
	bgUser      = bgRaised
	bgUserHover = bgBox
)

// sidebarWidth is OpenCode's SESSION_SIDEBAR_WIDTH (42 columns, docked flush).
const sidebarWidth = panelWidth

// panelWidth is the sidebar's width: OpenCode's SESSION_SIDEBAR_WIDTH, 42
// columns, docked flush against the right edge (bh used 40 at >=110).
const panelWidth = 42

// panePad is the session pane's left/right padding (OpenCode: 2, halved when
// the pane is narrower than 44 columns).
const panePad = 2

// paneFloor and panelThreshold implement the only breakpoint in OpenCode: the
// sidebar shows when the terminal is WIDER than 120 columns.
const (
	panelThreshold = 120
	paneFloor      = 44
)

// wheelLines is OpenCode's scroll speed (CustomSpeedScroll(3)).
const wheelLines = 3

// escInterruptWindow is the double-press window OpenCode uses (5 s).
const escInterruptWindow = 5 * time.Second

// footerCopyHint is what OpenCode points at once the command palette exists.
// bh has no palette yet (spec §3), so the same slot offers /help instead.
const footerCopyHint = "/help"

// panelState is what the side panel, the input box and the footer show.
type panelState struct {
	title     string
	model     string
	provider  string
	effort    string
	cwd       string
	ctxTokens int
	window    int
	mcps      []mcpEntry
	branch    string
}

// mcpEntry is one sidebar MCP row: the server name and whether its connection
// is alive (the dot is green when live, red when the client broke).
type mcpEntry struct {
	name  string
	falha bool
}

type screen struct {
	out io.Writer

	mu      sync.Mutex
	lines   []string
	partial string // line still without \n (e.g. "select model > ")
	transit string // transient spinner line
	// scroll is how many lines the view sits ABOVE the bottom. 0 means
	// pinned (sticky bottom, OpenCode's stickyScroll): new output scrolls
	// with it; >0 means the user read back, and new output must NOT pull the
	// view down again.
	scroll int
	// sticky records whether the view is pinned to the bottom. It differs
	// from scroll == 0 only while a jump is in flight (see toBottom).
	sticky bool
	w, h   int
	state  panelState
	input  string
	masked bool // the typed answer is a secret (API key)
	busy   bool // model turn in progress
	notice string
	// noticeAt is when notice was set; it clears after 2s.
	noticeAt time.Time
	// images pasted into the current input ([Image N]); imageCount numbers
	// them per message.
	images     []pastedImage
	imageCount int
	// sentImages holds the images of lines already sent and not yet taken by
	// the REPL, keyed by the trimmed line (see takeImages).
	sentImages map[string][][]string
	// lastTabTitle is the last title sent to the tab (resent only on change).
	lastTabTitle string
	spin         chan struct{}
	spinDone     chan struct{}

	// ── mouse ──
	// hits are the rectangles of the last painted frame, with their hover
	// style and click action; mouse events are dispatched through them.
	hits *hitList
	// sel is the in-app text selection (left-drag over the conversation). It
	// is copied on release/right-click/ctrl+c and suppresses click actions
	// while it holds text, as in OpenCode.
	sel   selRange
	selOn bool
	// selAnchor remembers the drag start so the first cells of a very short
	// drag still count as a selection anchor.
	selAnchor selPoint
	// promptCol is where the text starts inside the input box (1-based), saved
	// by the frame so a click moves the caret to the matching input column.
	promptCol int
	// thinking is the open/closed state of the "+ Thought" lines, keyed by the
	// line index in t.lines; thoughtBodies holds the reasoning text each of
	// them expands to.
	thinking      map[int]bool
	thoughtBodies map[int]string
	// mcpOpen is the sidebar's MCP collapse state (open by default).
	mcpOpen bool
	// interruptArmed is set by the first Esc while a turn runs; the second Esc
	// within escInterruptWindow actually interrupts. escAt is when the first
	// press happened.
	interruptArmed bool
	escAt          time.Time
	// spinnerSeq counts frames of the footer's busy spinner.
	spinnerSeqVal int
}

// interruptLabel is the footer text while a turn runs: "esc interrupt", and
// "esc again to interrupt" after the first Esc (OpenCode's PromptInterruptStatus).
const (
	interruptLabelArmed = "again to interrupt"
	interruptLabel      = "interrupt"
)

// currentSpin paints the footer's knight-rider spinner frame: 8 cells of ■/⬝
// in the agent colour, at 40 ms per frame, as OpenCode's ui/spinner.ts does.
func currentSpin(seq int, _ bool) string {
	width := 8
	total := width + 9 + (width - 1) + 30 // forward + holdEnd + backward + holdStart
	f := seq % total
	var pos int
	switch {
	case f < width:
		pos = f
	case f < width+9:
		pos = width - 1
	case f < width+9+width-1:
		pos = width - 2 - (f - width - 9)
	default:
		pos = 0
	}
	var sb strings.Builder
	for i := 0; i < width; i++ {
		if i == pos {
			sb.WriteString(bluef("■"))
			continue
		}
		// The trail dims by distance from the head, floored at 30% alpha.
		d := pos - i
		if d < 0 {
			d = -d
		}
		if d < 6 {
			sb.WriteString(color("\033[38;2;"+mixHex(92, 156, 245, float64(d)/6.0)+"m", "⬝"))
			continue
		}
		sb.WriteString(grayf("⬝"))
	}
	return sb.String()
}

// mixHex blends c toward the background at ratio f, keeping the 24-bit escape.
func mixHex(r, g, b int, f float64) string {
	const bg = 10
	clamp := func(v int) int { return clampInt(int(v), 0, 255) }
	if f > 1 {
		f = 1
	}
	return itoa(clamp(r+int(float64(bg-r)*f))) + ";" +
		itoa(clamp(g+int(float64(bg-g)*f))) + ";" +
		itoa(clamp(b+int(float64(bg-b)*f)))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var d []byte
	for v > 0 {
		d = append([]byte{byte('0' + v%10)}, d...)
		v /= 10
	}
	return string(d)
}

func newScreen(out io.Writer) *screen {
	fmt.Fprint(out, escAlt+escMouseOn+escFocusOn+escPasteOn+escClear)
	return &screen{out: out, hits: newHits(), sticky: true, mcpOpen: true, thinking: map[int]bool{}, thoughtBodies: map[int]string{}}
}

// takeImages returns (and forgets) the images sent with line: the REPL reads
// lines from a pipe, so the attachments travel beside it, keyed by the text.
func (t *screen) takeImages(line string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := strings.TrimSpace(line)
	q := t.sentImages[key]
	if len(q) == 0 {
		return nil
	}
	if len(q) == 1 {
		delete(t.sentImages, key)
	} else {
		t.sentImages[key] = q[1:]
	}
	return q[0]
}

func (t *screen) shutdown() {
	t.stopSpin()
	// The tab goes back to a neutral title: the shell (cmd/PowerShell) does not
	// rewrite it, and the session name would stick to the tab after bh exits.
	fmt.Fprint(t.out, escPasteOff+escMouseOff+escFocusOff+escShowCursor+escNormal+tabTitle(""))
}

// tabTitle is the OSC 0 sequence that sets the terminal tab/window name.
// Control characters are stripped so they cannot break the sequence.
func tabTitle(session string) string {
	name := "BH"
	if s := strings.TrimSpace(session); s != "" {
		clean := strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return -1
			}
			return r
		}, s)
		if r := []rune(clean); len(r) > 60 {
			clean = string(r[:60]) + "…"
		}
		name = "BH | " + clean
	}
	return "\033]0;" + name + "\a"
}

// ── API used by the REPL and by the turn renderer ─────────────────────────

// Adicionar appends lines to the conversation. While the view is pinned to the
// bottom (sticky) the new lines scroll in with it; once the user read back,
// the view stays exactly where it is — new output must not yank it down.
func (t *screen) Adicionar(text string) {
	t.mu.Lock()
	t.lines = append(t.lines, strings.Split(text, "\n")...)
	t.trim()
	if t.scroll > 0 {
		// keep the offset above the bottom while content grows
		t.scroll += len(strings.Split(text, "\n"))
	}
	t.drawLocked()
	t.mu.Unlock()
}

// Usuario draws the sent message as a dark-background block with the blue
// bar, like OpenCode.
func (t *screen) Usuario(text string) {
	t.mu.Lock()
	// Same width and same text column as the input box: both boxes line up,
	// like in OpenCode. The bar is ┃ in the agent (primary blue) colour and the
	// box background is raised.base #141414, with one padding line above and
	// below; text two columns inside the bar.
	avail := t.conversationWidth()
	blank := withBg(bluef(bar), bgUser, avail)
	grown := 0
	if len(t.lines) > 0 {
		t.lines = append(t.lines, "")
		grown++
	}
	t.lines = append(t.lines, blank)
	grown++
	for _, l := range strings.Split(wrap(text, avail-5), "\n") {
		t.lines = append(t.lines, withBg(bluef(bar)+"  "+highlightImages(l, colorText), bgUser, avail))
		grown++
	}
	t.lines = append(t.lines, blank, "")
	grown += 2
	t.trim()
	if t.scroll > 0 {
		t.scroll += grown
	}
	t.drawLocked()
	t.mu.Unlock()
}

// AvisoSubagentes shows, in place of the user box, one line per finished
// sub-agent ("⚑ t1 completed · t2 failed").
func (t *screen) AvisoSubagentes(text string) {
	t.Adicionar("")
	t.Adicionar("  " + bluef("⚑") + " " + grayf(summarizeSubNotice(text)))
}

// summarizeSubNotice extracts the "## t1 completed (11s)" lines from the notice.
func summarizeSubNotice(text string) string {
	var parts []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "## ") {
			parts = append(parts, strings.TrimPrefix(l, "## "))
		}
	}
	if len(parts) == 0 {
		return "sub-agents finished"
	}
	return "sub-agents finished: " + strings.Join(parts, " · ")
}

func (t *screen) Limpar() {
	t.mu.Lock()
	t.lines, t.partial, t.scroll = nil, "", 0
	t.drawLocked()
	t.mu.Unlock()
}

func (t *screen) Estado(e panelState) {
	t.mu.Lock()
	t.state = e
	t.drawLocked()
	t.mu.Unlock()
}

func (t *screen) Ocupado(v bool) {
	t.mu.Lock()
	t.busy = v
	t.drawLocked()
	t.mu.Unlock()
}

func (t *screen) Ocultar(v bool) {
	t.mu.Lock()
	t.masked = v
	t.mu.Unlock()
}

func (t *screen) trim() {
	if n := len(t.lines); n > maxLines {
		t.lines = t.lines[n-maxLines:]
	}
}

// Pensar animates "⠋ <text> 3s" as the last conversation line until stopSpin.
func (t *screen) Pensar(text string) {
	t.stopSpin()
	t.mu.Lock()
	t.spin, t.spinDone = make(chan struct{}), make(chan struct{})
	stop, end := t.spin, t.spinDone
	t.mu.Unlock()
	go func() {
		defer close(end)
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			t.mu.Lock()
			// OpenCode's in-transcript spinner: glyph + space + label in the
			// warning colour, with NO elapsed seconds (they only appear at the
			// end, on the "+ Thought · 8.1s" line).
			t.transit = "  " + bluef(spinFrames[i%len(spinFrames)]) + " " +
				color(colorText, text)
			t.drawLocked()
			t.mu.Unlock()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *screen) stopSpin() {
	t.mu.Lock()
	stop, end := t.spin, t.spinDone
	t.spin, t.spinDone = nil, nil
	t.mu.Unlock()
	if stop != nil {
		close(stop)
		<-end
	}
	t.mu.Lock()
	t.transit = ""
	t.mu.Unlock()
}

// screenWriter is the REPL's io.Writer: each finished line becomes a
// conversation line; the rest stays as the partial line (a prompt awaiting input).
type screenWriter struct{ t *screen }

func (e screenWriter) Write(p []byte) (int, error) {
	t := e.t
	t.mu.Lock()
	parts := strings.Split(t.partial+string(p), "\n")
	for _, l := range parts[:len(parts)-1] {
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:]
		}
		t.lines = append(t.lines, l)
	}
	t.partial = parts[len(parts)-1]
	t.trim()
	// Stay-put scrolling: while the user reads back, the view keeps its offset
	// above the bottom as the conversation grows.
	if t.scroll > 0 {
		t.scroll += len(parts) - 1
	}
	t.drawLocked()
	t.mu.Unlock()
	return len(p), nil
}

// ── drawing ───────────────────────────────────────────────────────────────

// screenSize returns the terminal columns and rows (100x30 when unknown).
// A variable so the preview and the tests can pin a size.
var screenSize = func() (int, int) {
	if a, b, err := term.GetSize(int(os.Stdout.Fd())); err == nil && a >= 40 && b >= 12 {
		return a, b
	}
	return 100, 30
}

// measure reads the terminal size; returns true when it changed.
func (t *screen) measure() bool {
	w, h := screenSize()
	changed := w != t.w || h != t.h
	t.w, t.h = w, h
	return changed
}

// panelVisible implements OpenCode's only breakpoint: wide = width > 120.
func (t *screen) panelVisible() bool { return t.w > panelThreshold }

// panePadCols is the pane's left/right padding, halved below the floor width.
func (t *screen) panePadCols() int {
	if t.w < paneFloor {
		return 1
	}
	return panePad
}

// conversationWidth is the pane's content width (pane width minus padding).
func (t *screen) conversationWidth() int {
	pad := t.panePadCols()
	pane := t.w
	if t.panelVisible() {
		pane = t.w - panelWidth
	}
	return max(8, pane-2*pad)
}

// boxTop..boxBottom are the rows of the input box, whose height depends on how
// many lines the typed text wrapped into. Layout (OpenCode §1.3), for a
// terminal of H rows, input of n lines (grow = n-1):
//
//	1..H-7-grow      transcript
//	H-6-grow         status row ("Jump to latest ↓", right-aligned, or blank)
//	H-5-grow         ┃ + prompt padding row (paddingTop 1)
//	H-4-grow..H-3     ┃ + text lines
//	H-2              ┃ + metadata row (paddingTop 1)
//	H-1              bottom cap (╹ then ▀ × width)
//	H-1 → footer     footer row (OpenCode puts it on H-1, blade of the box)
//	H                blank (pane paddingBottom 1)
//
// frame describes one such layout so both drawing and hit-testing agree.
type frame struct {
	w, h       int
	convRow    int // first transcript row
	convHeight int // number of transcript rows
	statusRow  int
	boxTop     int // the padding row above the text
	boxTextRow int // first text row
	boxTextN   int // number of text lines
	boxMetaRow int // the model/provider/effort row
	boxGapRow  int // the blank metadata padding row (paddingTop 1)
	boxCapRow  int
	footerRow  int
	barCol     int // 1-based column of the ┃ of the box
	textCol    int // 1-based column where the box's text starts
	boxWidth   int // width of the ┃..end of the painted box
	paneCols   int // pane content width
}

// frameFor computes the layout for the current terminal size and input height.
// The transcript grows downward from row 2 (OpenCode's first message row has
// its own marginTop, so row 1 is blank but scrolls with the content).
//
// The prompt owns four fixed rows below the typing area: a blank metadata
// padding row (paddingTop 1), the metadata row, the ╹/▀ cap, and then the
// footer. Below the footer the last row of the screen stays blank (OpenCode's
// pane paddingBottom 1), so the footer lands on H−1.
func (t *screen) frameFor() frame {
	f := frame{w: t.w, h: t.h}
	boxN := len(t.boxLines(max(6, t.h/3))) // OpenCode: maxHeight = max(6, floor(H/3))
	// Rows are counted from the bottom so the block always ends on H−1 (footer)
	// with row H blank, no matter how tall the typing area is.
	f.footerRow = t.h - 1
	f.boxCapRow = f.footerRow - 1
	f.boxMetaRow = f.boxCapRow - 1
	f.boxGapRow = f.boxMetaRow - 1
	f.boxTextRow = f.boxGapRow - boxN
	f.boxTop = f.boxTextRow - 1
	f.statusRow = f.boxTop - 1
	f.convRow = 2
	f.convHeight = max(3, f.statusRow-f.convRow)
	f.boxTextN = boxN
	f.paneCols = t.conversationWidth()
	// The ┃ sits at the pane's left padding + 1 (1-based column), the text two
	// columns after it; the painted box spans the rest of the pane.
	f.barCol = t.panePadCols() + 1
	f.textCol = f.barCol + 1 + 2
	f.boxWidth = t.boxWidthCols()
	return f
}

// boxWidthCols is the last column (1-based) the prompt box paints: the pane's
// right edge minus its right padding.
func (t *screen) boxWidthCols() int {
	if t.panelVisible() {
		return t.w - panelWidth - t.panePadCols()
	}
	return t.w - t.panePadCols()
}

// wrapInputLimit wraps the typed text at width n and keeps the last maxLines
// lines (the box scrolls internally past its maximum height, caret visible).
func wrapInputLimit(s string, n, maxLines int) []string {
	out := wrapInput(s, n)
	if len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	return out
}

// fmtTokens formats a count like JavaScript's toLocaleString in en-US:
// 12345 -> "12,345" (OpenCode's sidebar and footer).
func fmtTokens(n int) string {
	return strings.ReplaceAll(thousands(n), ".", ",")
}

// boxLines wraps the visible input into the box's lines, capped at maxBox
// (the last lines are kept: the caret stays visible).
func (t *screen) boxLines(maxBox int) []string {
	text := ""
	if t.partial == "" {
		text = t.visibleInput()
	}
	// The text starts two columns inside the ┃, and the box has 2 columns of
	// right padding: that is the width the typed text wraps at.
	return wrapInputLimit(text, t.boxWidthCols()-t.panePadCols()-4, maxBox)
}

// ensure initializes the fields a zero-value screen (the tests build one
// directly, without newScreen) needs.
func (t *screen) ensure() {
	if t.hits == nil {
		t.hits = newHits()
	}
	if t.thinking == nil {
		t.thinking = map[int]bool{}
	}
	if t.thoughtBodies == nil {
		t.thoughtBodies = map[int]string{}
	}
}

// drawLocked paints the whole frame and rebuilds the hit regions. Called with
// t.mu held.
func (t *screen) drawLocked() {
	t.ensure()
	t.hits.clear()
	fullClear := t.measure()
	f := t.frameFor()

	var sb strings.Builder
	sb.WriteString(escSyncStart + escHideCursor)
	if fullClear {
		sb.WriteString(escClear)
	}
	line := func(r int, s string) {
		fmt.Fprintf(&sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}

	// No title line: the session name goes to the terminal tab (OSC 0) and to
	// the panel. bh has one session per terminal, so OpenCode's tab bar makes
	// no sense here.
	tabName := t.state.title
	if strings.TrimSpace(tabName) == "" {
		tabName = "New session"
	}
	if tabName != t.lastTabTitle {
		t.lastTabTitle = tabName
		sb.WriteString(tabTitle(tabName))
	}

	// conversation: every wrapped line, the visible window taken from the end
	// only while the view is pinned; once the user scrolled up, new content
	// must NOT yank the view down (OpenCode's sticky scroll).
	all := t.wrapped(f.paneCols - 2)
	height := f.convHeight
	if t.scroll > len(all)-height {
		t.scroll = max(0, len(all)-height)
	}
	end := len(all) - t.scroll
	vis := all[max(0, end-height):end]
	t.paintTranscript(&sb, f, vis, height)

	// status row: "Jump to latest ↓" while the view sits above the bottom.
	if t.scroll > 0 {
		jump := grayf("Jump to latest ↓")
		row := padding(f.w) + jump
		t.hits.add(&hitRegion{
			row: f.statusRow, col: f.w - visWidth(jump) + 1,
			width: visWidth(jump), height: 1,
			plain: row, hover: color(colorText, "Jump to latest ↓") + " ↓",
			action: func() { t.toBottom() },
		})
		line(f.statusRow, row)
	} else {
		line(f.statusRow, "")
	}

	t.drawBox(&sb, f)
	t.drawFooter(&sb, f)
	if t.panelVisible() {
		t.drawPanel(&sb, f)
	}

	// cursor: in the box (also during the turn, to type the next message right
	// away), or at the end of the command prompt line
	if t.partial == "" {
		t.promptCol = f.textCol + visWidth(t.boxLines(max(6, t.h/3))[len(t.boxLines(max(6, t.h/3)))-1])
		fmt.Fprintf(&sb, "\033[%d;%dH", f.boxTextRow+f.boxTextN-1, t.promptCol)
		sb.WriteString(escShowCursor)
	} else if len(vis) > 0 {
		last := vis[len(vis)-1]
		fmt.Fprintf(&sb, "\033[%d;%dH", f.convRow+len(vis)-1, 1+visWidth(conversationMargin(last)+last))
		sb.WriteString(escShowCursor)
	}
	sb.WriteString(escSyncEnd)
	fmt.Fprint(t.out, sb.String())
}

// wrapped returns the whole transcript wrapped to width (each entry already
// colored), the way the frame shows it.
func (t *screen) wrapped(width int) []string {
	var all []string
	for _, l := range t.lines {
		all = append(all, wrapVis(l, width)...)
	}
	if t.partial != "" {
		all = append(all, wrapVis("  "+t.partial+t.visibleInput(), width)...)
	}
	if t.transit != "" {
		all = append(all, t.transit)
	}
	return all
}

// paintTranscript paints the visible conversation rows, applying the in-app
// selection over them. Each row is registered as a selection source, and the
// rows that carry a collapsible Thought line are registered with their click
// action and hover style, as OpenCode's message-parts do.
func (t *screen) paintTranscript(sb *strings.Builder, f frame, vis []string, height int) {
	line := func(r int, s string) {
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	// Total wrapped lines before the window, so a mouse row maps back to the
	// index in t.lines (which line of the transcript is on screen).
	base := len(t.wrapped(f.paneCols-2)) - t.scroll - len(vis)
	// Row 1 is the pane's top margin; the messages start on row 2.
	for i := 0; i < height; i++ {
		row := f.convRow + i
		s := ""
		if i < len(vis) {
			visRow := vis[i]
			if t.selOn {
				visRow = t.applySelection(visRow, base+i)
			}
			s = conversationMargin(visRow) + visRow
		}
		line(row, s)
		// A drag starts on any conversation cell; when the row holds a Thought
		// line it also answers a click (and shows the hover variant).
		region := &hitRegion{row: row, col: 1, width: f.paneCols, height: 1, id: "conv"}
		idx := base + i
		if idx >= 0 && idx < len(t.lines) {
			if body, ok := t.thoughtBodies[idx]; ok && body != "" {
				head := t.lines[idx]
				region.id = "thought"
				region.plain = conversationMargin(head) + head
				region.hover = thoughtHover(head, t.thinking[idx])
				region.action = func() { t.expandThought(idx) }
			}
		}
		t.hits.add(region)
	}
}

// thoughtHover paints a Thought header's hover variant: the collapsed line
// flips its "+" to "-" on hover, as OpenCode previews the expansion.
func thoughtHover(head string, open bool) string {
	if open {
		return head
	}
	return strings.Replace(head, "+ Thought", "- Thought", 1)
}

// applySelection paints the selected segment of one wrapped line.
func (t *screen) applySelection(l string, idx int) string {
	plain := stripANSI(l)
	runes := []rune(plain)
	n := t.sel.normalize()
	// A line takes part in the selection when it lies between the endpoints
	// (inclusive); the endpoints then clamp the painted span.
	if idx < n.from.line || idx > n.to.line {
		return l
	}
	from, to := 0, len(runes)
	if idx == n.from.line {
		from = clampInt(n.from.col, 0, len(runes))
	}
	if idx == n.to.line {
		to = clampInt(n.to.col, from, len(runes))
	}
	if from >= to {
		return l
	}
	return color(colorText, string(runes[:from])) +
		bgSelection + fgSelected + string(runes[from:to]) + reset +
		color(colorText, string(runes[to:]))
}

// drawBox paints the input box: the ┃ gutter, the padded text rows, the
// metadata row, and the bottom cap. No bottom padding row inside the box.
func (t *screen) drawBox(sb *strings.Builder, f frame) {
	line := func(r int, s string) {
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	bar := bluef(bar)
	gutter := strings.Repeat(" ", f.barCol-1) + bar
	blank := strings.Repeat(" ", f.barCol-1) + withBg(bar, bgBox, f.boxWidth-f.barCol+1)
	// padding row above the text (paddingTop 1)
	line(f.boxTop, blank)
	// text rows
	maxBox := max(6, t.h/3)
	boxLines := t.boxLines(maxBox)
	for i, l := range boxLines {
		painted := l
		if !t.masked {
			painted = highlightImages(l, "")
		}
		line(f.boxTextRow+i, gutter+"  "+withBg(painted, bgBox, f.boxWidth-f.textCol+1-2))
	}
	// metadata row: a blank padding row first (paddingTop 1), then the model,
	// provider and effort (the effort in bold warning), as OpenCode does.
	line(f.boxGapRow, blank)
	info := color(colorText, t.state.model) + " " + grayf(t.state.provider)
	if t.state.effort != "" {
		info += grayf(" · ") + boldf(orangef(t.state.effort))
	}
	line(f.boxMetaRow, gutter+"  "+withBg(info, bgBox, f.boxWidth-f.textCol+1-2))
	// bottom cap: ╹ in the bar colour, then ▀ across the box
	capRow := strings.Repeat(" ", f.barCol-1) + bar + strings.Repeat(barEdge, f.boxWidth-f.barCol)
	line(f.boxCapRow, withBg(capRow, bgBox, f.boxWidth))
	// the whole box is clickable: a click inside it must not reach the
	// conversation underneath.
	t.hits.add(&hitRegion{row: f.boxTop, col: 1, width: f.boxWidth,
		height: f.boxCapRow - f.boxTop + 1, id: "box"})
	_ = bar
}

// drawFooter paints the footer on the row right below the cap; the last row
// stays blank (OpenCode's pane paddingBottom 1).
func (t *screen) drawFooter(sb *strings.Builder, f frame) {
	line := func(r int, s string) {
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	// The footer is aligned to the prompt box: it starts at the pane's left
	// padding and never crosses into the sidebar.
	width := f.boxWidth
	left, right := t.footerParts(width)
	gap := max(1, width-visWidth(left)-visWidth(right))
	line(f.footerRow, left+strings.Repeat(" ", gap)+truncateVis(right, width-visWidth(left)-gap))
	line(f.h, "")
}

// footerParts builds the left (location or interrupt state) and right (usage
// and shortcut hint) half of the footer, following OpenCode's fit rules: the
// shortcuts are dropped before the usage, and the right side disappears
// entirely while an interrupt is armed on a narrow screen.
func (t *screen) footerParts(w int) (string, string) {
	var left string
	if t.busy {
		label := color(colorText, "esc") + " " + grayf(interruptLabel)
		if t.interruptArmed {
			label = orangef("esc " + interruptLabelArmed)
		}
		left = "   " + currentSpin(t.spinnerSeqVal, t.interruptArmed) + " " + label
	} else if t.state.cwd != "" {
		left = "  " + grayf(t.state.cwd)
	}
	usage := ""
	if t.state.ctxTokens > 0 {
		usage = fmtContext(t.state.ctxTokens, t.state.window)
	}
	// OpenCode's fit rule: available = (W-8) - min(28, (W-8)/2).
	available := max(0, w-8-min(28, (w-8)/2))
	hint := boldf(color(colorText, footerCopyHint)) + " " + grayf("commands")
	fits := func(parts ...string) bool {
		return visWidth(strings.Join(parts, " · ")) <= available
	}
	var right []string
	switch {
	case usage != "" && hint != "" && fits(usage, hint):
		right = []string{usage, hint}
	case usage != "" && fits(usage):
		right = []string{usage}
	case hint != "" && fits(hint):
		right = []string{hint}
	}
	// Interrupt armed on a narrow screen hides the right side entirely.
	if t.busy && t.interruptArmed && w < 80 {
		right = nil
	}
	if t.notice != "" {
		right = []string{yellowf(t.notice)}
	}
	return left, strings.Join(right, grayf(" · "))
}

func (t *screen) visibleInput() string {
	if t.masked {
		return strings.Repeat("•", utf8.RuneCountInString(t.input))
	}
	return t.input
}

// sidebarScroll is the sidebar's own scroll offset (its content grows taller
// than the pane).
func (t *screen) drawPanel(sb *strings.Builder, f frame) {
	col := t.w - panelWidth + 1
	panel, n := t.panelLines()
	avail := f.h - 2 // top/bottom padding of 1
	// off is how many leading lines the sidebar clips (its content can grow
	// taller than the pane); the visible slice starts there.
	off := 0
	if d := n - avail; d > 0 {
		off = d
	}
	visible := panel[min(off, len(panel)):]
	// The MCP heading, when it is one of the visible lines and there are more
	// than two servers: a mouse-down collapses the section.
	mcpVisible := -1
	for i, l := range visible {
		if strings.Contains(stripANSI(l), "MCP") {
			mcpVisible = i
			break
		}
	}
	for i := 0; i < avail; i++ {
		row := 2 + i
		s := ""
		if i < len(visible) {
			s = visible[i]
		}
		fmt.Fprintf(sb, "\033[%d;%dH%s%s", row, col, reset, withBg(s, bgPanel, panelWidth-2))
		if i == mcpVisible && len(t.state.mcps) > 2 {
			t.hits.add(&hitRegion{row: row, col: col, width: panelWidth - 2, height: 1,
				id: "mcpheader", action: func() { t.mcpOpen = !t.mcpOpen; t.drawLocked() }})
			continue // the region is registered; nothing else to do on this row
		}
	}
}

// panelLines renders the sidebar: title, Context (hidden with no usage) and MCP
// (hidden with no servers, collapsed past two rows when closed).
func (t *screen) panelLines() ([]string, int) {
	e := t.state
	title := e.title
	if title == "" {
		title = "New session"
	}
	var out []string
	out = append(out, "") // paddingTop 1
	for _, l := range strings.Split(wrap(title, panelWidth-6), "\n") {
		out = append(out, "  "+boldf(color(colorText, l)))
	}
	// Context: hidden entirely while there is no usage (OpenCode's
	// <Show when={state()}>), not a placeholder row.
	if e.ctxTokens > 0 {
		out = append(out, "", "  "+boldf(color(colorText, "Context")))
		out = append(out, "  "+grayf(fmtTokens(e.ctxTokens)+" tokens"))
		if e.window > 0 {
			pct := 100 * e.ctxTokens / e.window
			txt := fmt.Sprintf("%d%% used", pct)
			if pct == 0 {
				txt = "<1% used"
			}
			out = append(out, "  "+grayf(txt))
		}
	}
	// MCP: hidden when there are no servers; the header opens/collapses past
	// two servers, exactly like OpenCode's sidebar/mcp.tsx.
	if len(e.mcps) > 0 {
		out = append(out, "", "  "+mcpHeader(e.mcps, t.mcpOpen))
		if t.mcpOpen || len(e.mcps) <= 2 {
			for _, m := range e.mcps {
				name := truncateVis(m.name, panelWidth-18)
				dot := greenf("•")
				status := grayf("Connected")
				if m.falha {
					dot, status = redf("•"), redf("Error")
				}
				out = append(out, "  "+dot+" "+boldf(color(colorText, name))+"  "+status)
			}
		}
	}
	out = append(out, "")
	// footer: the working directory and its git branch.
	loc := e.cwd
	if e.branch != "" {
		loc += ":" + e.branch
	}
	out = append(out, "  "+grayf(truncateVis(loc, panelWidth-6)))
	return out, len(out)
}

// mcpHeader renders the sidebar's MCP heading: "▼ MCP" open, and
// "▶ MCP 2 active, 1 error" collapsed (OpenCode counts the live and the broken
// servers so the hidden section still reports them).
func mcpHeader(mcps []mcpEntry, open bool) string {
	arrow := "▼"
	if open {
		return boldf(color(colorText, arrow+" MCP"))
	}
	arrow = "▶"
	live, bad := 0, 0
	for _, m := range mcps {
		if m.falha {
			bad++
			continue
		}
		live++
	}
	tail := fmt.Sprintf(" (%d active", live)
	if bad > 0 {
		tail += fmt.Sprintf(", %d error", bad)
		if bad > 1 {
			tail += "s"
		}
	}
	tail += ")"
	return boldf(color(colorText, arrow+" MCP")) + grayf(tail)
}

// padding pads a footer/status row's left side to width columns.
func padding(width int) string { return strings.Repeat(" ", max(1, width)) }

// conversationMargin: boxes (user messages) start at the input box column;
// the rest of the conversation starts at the column of the TEXT inside them,
// like in OpenCode.
func conversationMargin(l string) string {
	if colorsOn && strings.HasPrefix(l, bgUser) {
		return "  "
	}
	return "   "
}

// ── scroll: sticky bottom that new output must not break ───────────────────

// t.scroll is how many lines the view sits ABOVE the bottom: 0 is the pinned
// (sticky) position, maxScroll() the very top. The helpers below work with the
// offset FROM THE TOP, which is what the keys mean (Home = 0, End = max).

// maxScroll is the furthest the view can sit above the bottom.
func (t *screen) maxScroll() int {
	return max(0, len(t.wrapped(t.conversationWidth()-2))-t.frameFor().convHeight)
}

// offsetFromTop is the view's position measured from the top of the transcript.
func (t *screen) offsetFromTop() int { return t.maxScroll() - t.scroll }

// setOffset pins the view at n lines from the top and re-sticks it when that is
// the bottom, exactly like OpenCode's sticky scroll.
func (t *screen) setOffset(n int) {
	t.scroll = clampInt(t.maxScroll()-n, 0, t.maxScroll())
	t.sticky = t.scroll == 0
	t.drawLocked()
}

// scrollUp/scrollDown move by n lines: "up" means towards the older content
// (the top), which is a LARGER t.scroll and a smaller offset from the top.
// Moving up un-sticks the view — the status row then shows "Jump to latest ↓"
// and new output no longer pulls it down — and reaching the bottom re-sticks it.
func (t *screen) scrollUp(n int)   { t.setOffset(t.offsetFromTop() - n) }
func (t *screen) scrollDown(n int) { t.setOffset(t.offsetFromTop() + n) }

// scrollPage moves by a fraction of the visible height: half for PgUp/PgDn
// (OpenCode's page keys), a quarter for its "half page".
func (t *screen) scrollPage(fraction int, up bool) {
	step := t.frameFor().convHeight / fraction
	if up {
		t.scrollUp(step)
		return
	}
	t.scrollDown(step)
}

// toBottom re-pins the view to the bottom, like OpenCode's toBottom().
func (t *screen) toBottom() {
	t.scroll, t.sticky = 0, true
	t.drawLocked()
}

// ── mouse dispatch ─────────────────────────────────────────────────────────

// mouse is the screen's left-button state (idle / selecting / pressed).
var mouse = mouseState{}

// wheelScroll routes a wheel event by the region the pointer is over: the
// sidebar scrolls its own list, everything else scrolls the transcript, at
// OpenCode's 3 lines per tick.
func (t *screen) wheelScroll(row, col, dir int) {
	if t.panelVisible() && col > t.w-panelWidth {
		t.drawLocked()
		return
	}
	// dir is +1 for a wheel-up event: the view moves UP (older content).
	if dir > 0 {
		t.scrollUp(wheelLines)
		return
	}
	t.scrollDown(wheelLines)
}

// mouseHover repaints the hovered region, but only when the hovered region
// changed: motion inside one region costs no redraw.
func (t *screen) mouseHover(row, col int) {
	if _, change := t.hits.hoverRegion(row, col); !change {
		return
	}
	t.repaintHover()
}

// repaintHover writes just the row of the region being (un)hovered, instead of
// the whole frame: a hover must not cost a full repaint.
func (t *screen) repaintHover() {
	var sb strings.Builder
	sb.WriteString(escHideCursor)
	for _, r := range t.hits.regions {
		if r.hover == "" || !r.hovered {
			continue
		}
		text := r.plain
		if r.hovered {
			text = r.hover
		}
		fmt.Fprintf(&sb, "\033[%d;%dH%s%s%s%s", r.row, r.col, reset, truncateVis(text, r.width),
			escClearLine, reset)
	}
	sb.WriteString(escShowCursor)
	fmt.Fprint(t.out, sb.String())
}

// mouseDown handles a left press: it starts a selection over the conversation
// or arms the click of the region underneath.
func (t *screen) mouseDown(row, col int) {
	if r := mouse.press(t.hits, row, col); r != nil {
		t.repaintHover()
	}
}

// mouseDrag extends the selection (or cancels the click). The drag is measured
// in transcript coordinates: the row maps to the wrapped line (the frame's
// first conversation row), the column subtracts the pane's left padding.
func (t *screen) mouseDrag(row, col int) {
	p := mouse.drag(row, col, t.frameFor().convRow, t.panePadCols())
	if p == nil {
		return
	}
	if !t.selOn {
		t.selOn = true
		t.sel = selRange{from: *p}
	}
	t.sel.to = *p
	t.drawLocked()
}

// mouseUp closes the interaction: a selection is kept and copied (clicks are
// then ignored while it holds text, as OpenCode does); a click dispatches the
// region's action.
func (t *screen) mouseUp(row, col int) {
	r, kept := mouse.release(t.hits, row, col)
	if kept && t.selOn {
		t.copySelection()
		return
	}
	if r == nil || r.action == nil {
		return
	}
	r.action()
}

// rightClick copies the current selection, as OpenCode does on Windows (where
// copy-on-select is "manual" by default).
func (t *screen) rightClick() {
	if t.selOn {
		t.copySelection()
	}
}

// selectionText is the plain text of the current in-app selection.
func (t *screen) selectionText() string {
	return extractSelection(t.wrapped(t.conversationWidth()-2), t.sel)
}

// hasSelection reports whether the in-app selection holds any text.
func (t *screen) hasSelection() bool {
	return t.selOn && t.selectionText() != ""
}

// clearSelection drops the in-app selection, as OpenCode's Esc does.
func (t *screen) clearSelection() {
	if !t.selOn {
		return
	}
	t.selOn = false
	t.drawLocked()
}

// copySelection copies the selection to the terminal clipboard (OSC 52) and to
// the OS clipboard, and shows the "Copied to clipboard" notice.
func (t *screen) copySelection() {
	text := t.selectionText()
	if text == "" {
		return
	}
	fmt.Fprint(t.out, osc52(text))
	if clipboardWrite == nil || clipboardWrite(text) != nil {
		t.notice, t.noticeAt = "copied to clipboard", time.Now()
	}
	t.drawLocked()
}

type keyEvent struct {
	text   string
	action string
	// mouse is set when the event came from an SGR mouse sequence.
	mouse bool
	col   int
	row   int
}

// readKeys decodes raw stdin into keys. Arrows and PgUp/PgDn arrive as escape
// sequences (Windows raw mode enables ENABLE_VIRTUAL_TERMINAL_INPUT);
// accented characters arrive as multi-byte UTF-8 and are reassembled.
func readKeys(r *bufio.Reader, ch chan<- keyEvent) {
	defer close(ch)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		switch {
		case b == '\r' || b == '\n':
			ch <- keyEvent{action: "enter"}
		case b == 127 || b == 8:
			ch <- keyEvent{action: "backspace"}
		case b == 3:
			ch <- keyEvent{action: "ctrl+c"}
		case b == 4:
			ch <- keyEvent{action: "ctrl+d"}
		case b == 12:
			ch <- keyEvent{action: "ctrl+l"}
		case b == 21:
			ch <- keyEvent{action: "ctrl+u"}
		case b == 22:
			// raw Ctrl+V (terminals without bracketed paste, or a keybinding
			// that sends it): bh reads the clipboard itself.
			ch <- keyEvent{action: "pasteimage"}
		case b == 27:
			if r.Buffered() == 0 {
				ch <- keyEvent{action: "esc"}
				continue
			}
			c, _ := r.ReadByte()
			if c != '[' && c != 'O' {
				continue
			}
			f, _ := r.ReadByte()
			switch f {
			case 'A':
				ch <- keyEvent{action: "cima"}
			case 'B':
				ch <- keyEvent{action: "baixo"}
			case '<':
				// SGR mouse: "<code;col;rowM" (press) / "m" (release).
				var seq []byte
				for {
					x, err := r.ReadByte()
					if err != nil {
						break
					}
					seq = append(seq, x)
					if x == 'M' || x == 'm' {
						break
					}
				}
				if e, ok := parseMouseSeq(string(seq)); ok {
					ch <- mouseEvent(e)
				}
			case '2':
				// "[200~" opens a bracketed paste; other "2…" sequences
				// (Insert, F9…) are discarded up to their final byte.
				params := []byte{f}
				for {
					x, err := r.ReadByte()
					if err != nil {
						return
					}
					if x >= 0x40 && x <= 0x7e {
						if x == '~' && string(params) == "200" {
							content, ok := readPaste(r)
							if !ok {
								return
							}
							if content == "" {
								// Windows Terminal < 1.25: an image-only clipboard
								// arrives as an empty paste.
								ch <- keyEvent{action: "pasteimage"}
							} else {
								ch <- keyEvent{action: "paste", text: content}
							}
						}
						break
					}
					params = append(params, x)
				}
			case '5', '6':
				_, _ = r.ReadByte() // '~'
				if f == '5' {
					ch <- keyEvent{action: "pgup"}
				} else {
					ch <- keyEvent{action: "pgdn"}
				}
			case 'H':
				ch <- keyEvent{action: "home"}
			case 'F':
				ch <- keyEvent{action: "end"}
			default:
				// other sequences (left/right arrows, F1…): discard up to the final byte
				for x := f; !(x >= 0x40 && x <= 0x7e) && r.Buffered() > 0; {
					x, _ = r.ReadByte()
				}
			}
		case b >= 0x80:
			n := 1
			switch {
			case b&0xE0 == 0xC0:
				n = 2
			case b&0xF0 == 0xE0:
				n = 3
			case b&0xF8 == 0xF0:
				n = 4
			}
			buf := []byte{b}
			for i := 1; i < n; i++ {
				c, err := r.ReadByte()
				if err != nil {
					return
				}
				buf = append(buf, c)
			}
			ch <- keyEvent{text: string(buf)}
		case b >= 32 || b == '\t':
			ch <- keyEvent{text: string(rune(b))}
		}
	}
}

// mouseEvent maps a decoded SGR event onto a keyEvent action, carrying the
// cell it happened on for the dispatcher.
func mouseEvent(e mouseSeq) keyEvent {
	k := keyEvent{mouse: true, col: e.Col, row: e.Row}
	switch {
	case e.Wheel == 64:
		k.action = "rodacima"
	case e.Wheel == 65:
		k.action = "rodabaixo"
	case e.Drag && e.Button == 0:
		k.action = "mousedrag"
	case e.Drag:
		k.action = "mousemove"
	case e.Motion:
		k.action = "mousemove"
	case e.Release && e.Button == 0:
		k.action = "mouseup"
	case e.Release && e.Button == 2:
		k.action = "rightclick"
	case e.Release:
		k.action = "mouserelease"
	default:
		k.action = "mousedown"
	}
	return k
}

// pasteResult is the outcome of reading the clipboard for image n.
type pasteResult struct {
	n          int
	path, text string
	err        error
}

// applyPaste puts a clipboard read into the input: an image becomes the
// [Image N] placeholder, text goes in as typed, a failure becomes a footer
// notice. Called with t.mu held.
func (t *screen) applyPaste(p pasteResult) {
	switch {
	case p.err != nil:
		if errors.Is(p.err, errNoClipboard) {
			t.notice = "nothing to paste"
		} else {
			t.notice = "paste failed: " + oneLineErr(p.err)
		}
		t.noticeAt = time.Now()
	case p.path != "":
		if p.n <= t.imageCount {
			p.n = t.imageCount + 1 // the input was sent/cleared meanwhile
		}
		t.imageCount = p.n
		t.images = append(t.images, pastedImage{n: p.n, path: p.path})
		t.input += imageLabel(p.n)
	default:
		t.input += pasteText(p.text)
	}
}

// queueImages moves the pasted images still referenced by line to the queue
// the REPL drains with takeImages, and resets the per-message numbering.
// Called with t.mu held.
func (t *screen) queueImages(line string) {
	imgs := selectImages(line, t.images)
	t.images, t.imageCount = nil, 0
	if len(imgs) == 0 {
		return
	}
	if t.sentImages == nil {
		t.sentImages = map[string][][]string{}
	}
	key := strings.TrimSpace(line)
	t.sentImages[key] = append(t.sentImages[key], imgs)
}

func oneLineErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	return s
}

// readPaste reads a bracketed paste's content up to "\033[201~". ok is false
// when the input ends before the closing sequence.
func readPaste(r *bufio.Reader) (string, bool) {
	var buf []byte
	for {
		x, err := r.ReadByte()
		if err != nil {
			return "", false
		}
		buf = append(buf, x)
		if x == '~' && len(buf) >= len(pasteEnd) && string(buf[len(buf)-len(pasteEnd):]) == pasteEnd {
			return string(buf[:len(buf)-len(pasteEnd)]), true
		}
	}
}

// ── main entry ────────────────────────────────────────────────────────────

// useScreen reports whether the REPL opens in full-screen mode.
func useScreen(io IO) bool {
	if os.Getenv("BH_SIMPLES") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	in, okIn := io.In.(*os.File)
	out, okOut := io.Out.(*os.File)
	return okIn && okOut && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// runScreen runs the normal REPL behind the full screen. Returns -1 when raw
// mode is unavailable (the caller falls back to scrolling mode).
func runScreen(io IO) int {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return -1
	}
	defer term.Restore(fd, old)

	t := newScreen(io.Out)
	defer t.shutdown()

	pr, pw := ioPipe()
	esc := screenWriter{t: t}
	injected := make(chan string, 16)
	sub := IO{In: pr, Out: esc, ErrOut: esc, screen: t, cancelCh: make(chan struct{}, 1),
		inject: func(s string) {
			select {
			case injected <- s:
			default: // queue full: a notice is already pending, and one is enough
			}
		}}
	sub.Reader() // the shared bufio is created here, before IO is copied

	// The API key cannot be read with term.ReadPassword (stdin is in raw mode,
	// owned by the screen): it comes through the box, masked with •.
	prevPromptKey := PromptKey
	PromptKey = func(prompt string) (string, error) {
		t.Ocultar(true)
		defer t.Ocultar(false)
		fmt.Fprint(esc, prompt)
		l, err := sub.Reader().ReadString('\n')
		return strings.TrimSpace(l), err
	}
	defer func() { PromptKey = prevPromptKey }()

	exitCode := make(chan int, 1)
	go func() {
		exitCode <- RunInteractive(sub)
		_ = pr.Close()
	}()

	keys := make(chan keyEvent, 64)
	go readKeys(bufio.NewReader(os.Stdin), keys)
	tick := time.NewTicker(400 * time.Millisecond) // resize and notices
	defer tick.Stop()

	var (
		history   []string
		histPos   = -1
		lastCtrlC time.Time
		// noticePending: a sub-agent finished while a prompt was waiting.
		noticePending bool
	)
	// The pipe is synchronous: writing directly would block keys while the REPL
	// is still finishing an interrupted turn. The queue keeps send order.
	output := make(chan string, 16)
	go func() {
		for s := range output {
			_, _ = pw.Write([]byte(s + "\n"))
		}
	}()
	defer close(output)
	send := func(s string) {
		t.mu.Lock()
		if t.partial != "" {
			// the answer to a command prompt is recorded in the conversation
			vis := s
			if t.masked {
				vis = strings.Repeat("•", utf8.RuneCountInString(s))
			}
			t.lines = append(t.lines, t.partial+vis)
			t.partial = ""
		}
		t.input = ""
		t.drawLocked()
		t.mu.Unlock()
		output <- s
	}
	edit := func(f func()) {
		t.mu.Lock()
		f()
		t.drawLocked()
		t.mu.Unlock()
	}
	// The clipboard is read off the key loop (PowerShell takes a second to
	// start); the result comes back through pasted.
	pasted := make(chan pasteResult, 4)
	pasting := false

	for {
		select {
		case c := <-exitCode:
			_ = pw.Close()
			return c
		case p := <-pasted:
			pasting = false
			edit(func() { t.applyPaste(p) })
		case s := <-injected:
			// With a command prompt awaiting an answer (e.g. /model), the synthetic
			// line would be read as the answer: hold it until the prompt closes.
			t.mu.Lock()
			waiting := t.partial != ""
			t.mu.Unlock()
			if waiting {
				noticePending = true
				continue
			}
			output <- s
		case <-tick.C:
			t.mu.Lock()
			idle := t.partial == ""
			t.mu.Unlock()
			if noticePending && idle {
				noticePending = false
				output <- subagentsLine
			}
			edit(func() {
				if t.notice != "" && time.Since(t.noticeAt) > 2*time.Second {
					t.notice = ""
				}
				// Reassert the tab name: child processes run through cmd.exe
				// (npx.cmd for the Playwright MCP, terminal_run with cmd /C) overwrite
				// the console title, and bh cannot prevent it. Clearing forces a
				// resend on the next frame.
				t.lastTabTitle = ""
			})
		case k, ok := <-keys:
			if !ok {
				_ = pw.Close()
				return <-exitCode
			}
			t.mu.Lock()
			busy, awaitingReply := t.busy, t.partial != ""
			t.mu.Unlock()
			switch k.action {
			case "":
				edit(func() { t.input += k.text })
			case "paste":
				edit(func() { t.input += pasteText(k.text) })
			case "pasteimage":
				if pasting {
					continue
				}
				pasting = true
				t.mu.Lock()
				n := t.imageCount + 1
				t.mu.Unlock()
				go func() {
					path, text, err := pasteClipboard(n)
					pasted <- pasteResult{n: n, path: path, text: text, err: err}
				}()
			case "backspace":
				edit(func() {
					if _, n := utf8.DecodeLastRuneInString(t.input); n > 0 {
						t.input = t.input[:len(t.input)-n]
					}
				})
			case "ctrl+u":
				edit(func() { t.input = "" })
			case "enter":
				t.mu.Lock()
				l := t.input
				t.mu.Unlock()
				if awaitingReply {
					send(l) // an empty Enter is a valid answer (e.g. keeps the model)
					continue
				}
				if strings.TrimSpace(l) == "" {
					continue
				}
				if busy {
					// New message mid-turn: interrupts the thinking or the running
					// tool; the message goes in as soon as the canceled turn hands
					// control back to the REPL.
					select {
					case sub.cancelCh <- struct{}{}:
					default:
					}
				}
				history = append(history, l)
				histPos = -1
				t.mu.Lock()
				t.queueImages(l)
				t.mu.Unlock()
				send(l)
			case "cima", "baixo":
				if len(history) == 0 {
					continue
				}
				if k.action == "cima" {
					if histPos < 0 {
						histPos = len(history) - 1
					} else if histPos > 0 {
						histPos--
					}
				} else if histPos >= 0 {
					histPos++
				}
				edit(func() {
					if histPos >= 0 && histPos < len(history) {
						t.input = history[histPos]
					} else {
						histPos, t.input = -1, ""
					}
				})
			case "pgup", "pgdn", "rodacima", "rodabaixo", "mousedrag", "mousemove",
				"mousedown", "mouseup", "mouserelease", "rightclick":
				// A selection suppresses every other input while it holds text,
				// as OpenCode does: the drag is being made or read.
				t.mu.Lock()
				selecting := t.hasSelection()
				t.mu.Unlock()
				if selecting && k.action != "mouseup" && k.action != "mouserelease" && k.action != "rightclick" {
					continue
				}
				edit(func() {
					switch k.action {
					case "rodacima":
						t.wheelScroll(k.row, k.col, 1)
					case "rodabaixo":
						t.wheelScroll(k.row, k.col, -1)
					case "mousemove":
						t.mouseHover(k.row, k.col)
					case "mousedown":
						t.mouseDown(k.row, k.col)
					case "mousedrag":
						t.mouseDrag(k.row, k.col)
					case "mouseup":
						t.mouseUp(k.row, k.col)
					case "mouserelease":
						// a region without an action only needs the hover
						// cleared, which the next frame does
					case "rightclick":
						t.rightClick()
					case "pgup": // half the visible height (OpenCode's page keys)
						t.scrollPage(2, true)
					case "pgdn":
						t.scrollPage(2, false)
					}
				})
			case "home", "end", "ctrl+g":
				edit(func() {
					// Home and ctrl+g jump to the first message; end and
					// ctrl+alt+g to the last, as OpenCode binds them.
					if k.action == "home" || k.action == "ctrl+g" {
						t.setOffset(0)
						return
					}
					t.setOffset(t.maxScroll())
				})
			case "ctrl+l":
				edit(func() { t.w = 0 }) // forces a clear and repaint
			case "esc", "ctrl+c":
				// A selection cancels itself on Esc before any other action,
				// as OpenCode does.
				t.mu.Lock()
				selecting := t.hasSelection()
				t.mu.Unlock()
				if selecting && k.action == "esc" {
					edit(func() { t.clearSelection() })
					continue
				}
				if k.action == "ctrl+c" && selecting {
					edit(func() { t.copySelection() })
					continue
				}
				if busy {
					if k.action == "ctrl+c" {
						select {
						case sub.cancelCh <- struct{}{}:
						default:
						}
						continue
					}
					// Esc arms the interrupt; a second press within the window
					// actually interrupts, exactly as OpenCode does.
					t.mu.Lock()
					armed := t.interruptArmed && time.Since(t.escAt) < escInterruptWindow
					t.mu.Unlock()
					if armed {
						t.mu.Lock()
						t.interruptArmed = false
						t.mu.Unlock()
						select {
						case sub.cancelCh <- struct{}{}:
						default:
						}
						continue
					}
					edit(func() {
						t.mu.Lock()
						t.interruptArmed, t.escAt = true, time.Now()
						t.mu.Unlock()
					})
					continue
				}
				if k.action == "esc" {
					continue
				}
				t.mu.Lock()
				blank := t.input == ""
				t.mu.Unlock()
				if !blank {
					edit(func() { t.input = "" })
					continue
				}
				if time.Since(lastCtrlC) < 2*time.Second {
					send("/exit")
					continue
				}
				lastCtrlC = time.Now()
				edit(func() { t.notice, t.noticeAt = "press ctrl+c again to exit", lastCtrlC })
			case "ctrl+d":
				if busy {
					continue // deleting forward / exiting is OpenCode's; bh keeps exiting
				}
				send("/exit")
			}
		}
	}
}

// ── colored text: visible width, truncation, wrapping, background ─────────

// escapeEnd returns the index right after the ANSI sequence starting at i.
func escapeEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
	}
	for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
		j++
	}
	return min(j+1, len(s))
}

// visWidth counts visible columns (ANSI escapes do not count).
func visWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			i = escapeEnd(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// truncateVis cuts to n visible columns, keeping the escapes.
func truncateVis(s string, n int) string {
	if visWidth(s) <= n {
		return s
	}
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := escapeEnd(s, i)
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		if vis >= n-1 {
			sb.WriteString("…" + reset)
			return sb.String()
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+size])
		vis++
		i += size
	}
	return sb.String()
}

// wrapVis splits a colored line into chunks of n visible columns.
func wrapVis(s string, n int) []string {
	n = max(10, n)
	if visWidth(s) <= n {
		return []string{s}
	}
	var out []string
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := escapeEnd(s, i)
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		if vis == n {
			out = append(out, sb.String())
			sb.Reset()
			sb.WriteString("  ")
			vis = 2
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+size])
		vis++
		i += size
	}
	return append(out, sb.String())
}

// wrapInput wraps the typed text into lines of up to n columns, preferring
// the last space on the line (a word that is too long breaks in the middle).
// The box is one column wider than n: the cursor fits after a full line.
func wrapInput(s string, n int) []string {
	n = max(10, n)
	var out []string
	r := []rune(s)
	for len(r) > n {
		cut := n
		for i := n; i > 0; i-- {
			if r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		if cut < len(r) && r[cut] == ' ' {
			cut++
		}
		r = r[cut:]
	}
	return append(out, string(r))
}

// withBg paints s with background bg in exactly n columns. Each reset
// inside s reapplies the background so text color cannot punch a hole in the box.
func withBg(s, bg string, n int) string {
	if !colorsOn {
		bg = ""
	}
	s = strings.ReplaceAll(truncateVis(s, n), reset, reset+bg)
	return bg + s + strings.Repeat(" ", max(0, n-visWidth(s))) + reset
}

func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ".")
}

// shortCwd replaces the home directory with ~ in the footer path.
func shortCwd(cwd string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return "~" + string(filepath.Separator) + rel
		}
	}
	return cwd
}

// gitBranch returns the current git branch of dir (for OpenCode's
// "~/path:branch" location label), reading .git/HEAD of the nearest repository
// without running git. Empty when dir is not in a repository or HEAD is
// detached.
func gitBranch(dir string) string {
	for d := dir; d != ""; {
		gitPath := filepath.Join(d, ".git")
		if st, err := os.Stat(gitPath); err == nil {
			if !st.IsDir() {
				// worktree/submodule: ".git" is a file "gitdir: <path>"
				b, err := os.ReadFile(gitPath)
				if err != nil {
					return ""
				}
				g := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
				if !filepath.IsAbs(g) {
					g = filepath.Join(d, g)
				}
				gitPath = g
			}
			head, err := os.ReadFile(filepath.Join(gitPath, "HEAD"))
			if err != nil {
				return ""
			}
			ref := strings.TrimSpace(string(head))
			if !strings.HasPrefix(ref, "ref: refs/heads/") {
				return ""
			}
			return strings.TrimPrefix(ref, "ref: refs/heads/")
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return ""
}

// isWorkingState reports whether a user message is the compaction working
// state. "[estado de trabalho" is the legacy marker still found in old logs.
func isWorkingState(s string) bool {
	return strings.HasPrefix(s, "[working state") || strings.HasPrefix(s, "[estado de trabalho")
}

// sessionTitle is the user's first message, as OpenCode names the session.
func sessionTitle(h []dealer.Mensagem) string {
	for _, m := range h {
		if m.Role != "user" || isWorkingState(m.Conteudo) ||
			strings.HasPrefix(m.Conteudo, "[subagents]") || strings.HasPrefix(m.Conteudo, "[output limit]") {
			continue
		}
		t := strings.Join(strings.Fields(m.Conteudo), " ")
		if r := []rune(t); len(r) > 60 {
			t = string(r[:60]) + "…"
		}
		return t
	}
	return ""
}

// ── turn renderer in full-screen mode ─────────────────────────────────────

type screenTurn struct {
	t           *screen
	model       string
	pendingArgs string
	ctxTokens   int
}

func (r *screenTurn) Pensando() { r.t.Pensar("Thinking") }

func (r *screenTurn) ToolInicio(name, args string) {
	r.pendingArgs = summarizeArgs(args)
	r.t.Pensar(strings.TrimSpace(name + " " + r.pendingArgs))
}

// Contexto arrives on every request of the turn: the panel and footer update
// immediately, without waiting for the turn to end (a tool-heavy turn takes minutes).
func (r *screenTurn) Contexto(n int) {
	r.ctxTokens = n
	r.t.mu.Lock()
	r.t.state.ctxTokens = n
	r.t.drawLocked()
	r.t.mu.Unlock()
}

func (r *screenTurn) Tool(name string, dur time.Duration, failed bool) {
	r.t.stopSpin()
	mark := greenf("✓")
	if failed {
		mark = redf("✗")
	}
	args := r.pendingArgs
	r.pendingArgs = ""
	r.t.Adicionar("  " + mark + " " + color(colorText, name) + " " + grayf(args) + " " + grayf(fmtDur(dur)))
}

func (r *screenTurn) Eco(output string) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return
	}
	// Compact like OpenCode: ONE preview line, truncated to the width. The
	// whole output already went to the model; on screen, a 2 KB JSON became a
	// wall of text.
	first := ""
	total := 0
	for _, l := range strings.Split(output, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		total++
		if first == "" {
			first = strings.Join(strings.Fields(l), " ")
		}
	}
	if first == "" {
		return
	}
	r.t.mu.Lock()
	avail := r.t.conversationWidth() - 10
	r.t.mu.Unlock()
	if total > 1 {
		first += fmt.Sprintf("  (+%d lines)", total-1)
	}
	r.t.Adicionar("  " + grayf("› "+truncateVis(first, avail)))
}

// Pensou closes "Thinking" like OpenCode: a "+ Thought: <title> · 8.1s" line
// in place of the spinner, clickable to expand the reasoning.
func (r *screenTurn) Pensou(text string, dur time.Duration) {
	r.t.stopSpin()
	r.t.thought(dur, text)
}

// thought appends the collapsed Thought line and remembers the reasoning text
// so a click can expand it: OpenCode's reasoningSummary turns a "**Title**"
// prefix into "Thought: Title".
func (t *screen) thought(dur time.Duration, reasoning string) {
	title, body := reasoningSummary(reasoning)
	t.mu.Lock()
	t.thoughtBodies[len(t.lines)] = strings.TrimRight(body, "\n")
	t.mu.Unlock()
	mid := ""
	label := "+ Thought"
	if title != "" {
		label, mid = "+ Thought: "+title, " · "
	}
	t.Adicionar("  " + orangef(label) + mid + grayf(fmtDur(dur)))
}

// reasoningSummary mirrors OpenCode's context/thinking.ts: a leading
// "**Title**" paragraph becomes the title and the rest is the body.
func reasoningSummary(text string) (title, body string) {
	content := strings.TrimSpace(text)
	if !strings.HasPrefix(content, "**") {
		return "", content
	}
	end := strings.Index(content, "**")
	if end < 0 {
		return "", content
	}
	rest := content[end+2:]
	// The title ends at the closing "**", and the body starts after the blank
	// line that follows it (or at the end).
	stop := strings.Index(rest, "**")
	if stop < 0 {
		return "", content
	}
	title = strings.TrimSpace(rest[:stop])
	body = strings.TrimSpace(strings.TrimPrefix(rest[stop+2:], "\n"))
	return title, body
}

// expandThought toggles a Thought line's body in place, the way OpenCode's
// click does: the "+"/"-" prefix flips and the reasoning shows below the header,
// indented, all in the muted colour.
func (t *screen) expandThought(idx int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if idx < 0 || idx >= len(t.lines) {
		return
	}
	body, ok := t.thoughtBodies[idx]
	if !ok {
		return
	}
	open := t.thinking[idx]
	t.thinking[idx] = !open
	head := t.lines[idx]
	switch {
	case open: // collapse: "+ Thought[: title] · dur"
		t.lines[idx] = strings.Replace(head, "- Thought", "+ Thought", 1)
	case strings.HasPrefix(stripANSI(head), "  + Thought"):
		t.lines[idx] = strings.Replace(head, "+ Thought", "- Thought", 1)
		width := t.conversationWidth() - 2
		var shown []string
		for _, l := range strings.Split(body, "\n") {
			shown = append(shown, wrapVis("     "+grayf(l), width)...)
		}
		t.lines = append(t.lines[:idx+1], append(shown, t.lines[idx+1:]...)...)
	}
	t.drawLocked()
}

func (r *screenTurn) Cortada(ceiling int, gaveUp bool) {
	r.t.stopSpin()
	msg := fmt.Sprintf("response cut at the output limit (%s tokens); continuing", thousands(ceiling))
	if gaveUp {
		msg = fmt.Sprintf("response cut at the output limit (%s tokens) again; stopped", thousands(ceiling))
	}
	r.t.Adicionar("  " + yellowf("⚠") + " " + grayf(msg))
}

func (r *screenTurn) Guardrail(proactive bool) {
	r.t.stopSpin()
	msg := "context compacted by the guardrail (provider rejected the request size)"
	if proactive {
		msg = "context compacted"
	}
	r.t.Adicionar("  " + purplef("◆") + " " + grayf(msg))
}

func (r *screenTurn) Resposta(text string) {
	r.t.stopSpin()
	r.t.mu.Lock()
	avail := r.t.conversationWidth() - 4
	r.t.mu.Unlock()
	r.t.Adicionar("")
	r.t.Adicionar(renderMarkdown(text, avail))
}

func (r *screenTurn) Tokens(_, output int, dur time.Duration, compacted bool) {
	r.t.stopSpin()
	parts := []string{color(colorText, r.model), fmtDur(dur)}
	if output > 0 && dur > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", float64(output)/dur.Seconds()))
	}
	if compacted {
		parts = append(parts, purplef("compacted"))
	}
	r.t.Adicionar("")
	r.t.Adicionar("  " + strings.Join(parts, grayf(" · ")))
}

// currentTitle is the session name: the stored one (/rename, /save) or,
// without it, the user's first message, as OpenCode names the session.
func currentTitle(db *store.Store, sess *runtime.Session) string {
	if db != nil && sess.ID() != "" {
		if se, err := db.GetSession(sess.ID()); err == nil && strings.TrimSpace(se.Title) != "" {
			return strings.TrimSpace(se.Title)
		}
	}
	// The first message comes from the log, not the in-memory history: after
	// a compaction the history starts at the working state, and the title
	// became the next message (e.g. "...").
	if db != nil && sess.ID() != "" {
		if p := strings.Join(strings.Fields(db.PrimeiraFala(sess.ID())), " "); p != "" {
			if r := []rune(p); len(r) > 60 {
				p = string(r[:60]) + "…"
			}
			return p
		}
	}
	return sessionTitle(sess.History())
}

// replayHistory redraws a resumed session's conversation on screen in the
// same format as a live turn: user box, tools with the result preview and
// the answer in markdown. Tool durations are not in the log, so the tool
// line has none. In the transcript a round's calls all come before the
// results; each tool is drawn next to its result, as live.
func replayHistory(t *screen, h []dealer.Mensagem) {
	calls := map[string]dealer.Mensagem{}
	r := &screenTurn{t: t}
	for _, m := range h {
		switch m.Role {
		case "user":
			if isWorkingState(m.Conteudo) {
				t.Adicionar("  " + purplef("◆") + " " + grayf("context compacted"))
				continue
			}
			if strings.HasPrefix(m.Conteudo, "[subagents]") {
				t.AvisoSubagentes(m.Conteudo)
				continue
			}
			if strings.HasPrefix(m.Conteudo, "[output limit]") {
				t.Adicionar("  " + yellowf("⚠") + " " + grayf("response cut at the output limit; continued"))
				continue
			}
			t.Usuario(strings.TrimSpace(m.Conteudo))
		case "tool_call":
			calls[m.ToolID] = m
		case "tool_result":
			mark := greenf("✓")
			if m.IsErro {
				mark = redf("✗")
			}
			c := calls[m.ToolID]
			t.Adicionar("  " + mark + " " + color(colorText, m.ToolName) + " " + grayf(summarizeArgs(c.ToolArgs)))
			r.Eco(m.Conteudo)
		case "assistant":
			if strings.TrimSpace(m.Conteudo) != "" {
				r.Resposta(m.Conteudo)
			}
		}
	}
}
