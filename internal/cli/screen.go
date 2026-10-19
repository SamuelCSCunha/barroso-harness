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

const (
	escAlt        = "\033[?1049h"
	escNormal     = "\033[?1049l"
	escMouseOn    = "\033[?1000h\033[?1006h" // mouse wheel arrives as SGR (\033[<64;x;yM)
	escMouseOff   = "\033[?1006l\033[?1000l"
	escPasteOn    = "\033[?2004h" // bracketed paste: a paste arrives as \033[200~...\033[201~
	escPasteOff   = "\033[?2004l"
	pasteStart    = "\033[200~"
	pasteEnd      = "\033[201~"
	escSyncStart  = "\033[?2026h" // paints the whole frame at once (no flicker)
	escSyncEnd    = "\033[?2026l"
	escClear      = "\033[2J"
	escClearLine  = "\033[K"
	escHideCursor = "\033[?25l"
	escShowCursor = "\033[?25h"
	bgBox         = "\033[48;5;235m"
	bgPanel       = "\033[48;5;234m"
	bgUser        = "\033[48;5;235m"
	panelWidth    = 40
	maxLines      = 5000
)

// panelState is what the side panel, the input box and the footer show.
type panelState struct {
	title     string
	model     string
	provider  string
	effort    string
	cwd       string
	ctxTokens int
	window    int
	mcps      []string
}

type screen struct {
	out io.Writer

	mu      sync.Mutex
	lines   []string
	partial string // line still without \n (e.g. "select model > ")
	transit string // transient spinner line
	scroll  int
	w, h    int
	state   panelState
	input   string
	masked  bool // the typed answer is a secret (API key)
	busy    bool // model turn in progress
	notice  string
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
}

func newScreen(out io.Writer) *screen {
	fmt.Fprint(out, escAlt+escMouseOn+escPasteOn+escClear)
	return &screen{out: out}
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
	fmt.Fprint(t.out, escPasteOff+escMouseOff+escShowCursor+escNormal+tabTitle(""))
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

// Adicionar appends lines to the conversation and scrolls back to the end.
func (t *screen) Adicionar(text string) {
	t.mu.Lock()
	t.lines = append(t.lines, strings.Split(text, "\n")...)
	t.trim()
	t.scroll = 0
	t.drawLocked()
	t.mu.Unlock()
}

// Usuario draws the sent message as a dark-background block with the blue
// bar, like OpenCode.
func (t *screen) Usuario(text string) {
	t.mu.Lock()
	// Same width and same text column as the input box: both boxes line up,
	// like in OpenCode.
	avail := t.conversationWidth() - 2
	blank := withBg(bluef("▌"), bgUser, avail)
	if len(t.lines) > 0 {
		t.lines = append(t.lines, "")
	}
	t.lines = append(t.lines, blank)
	for _, l := range strings.Split(wrap(text, avail-5), "\n") {
		t.lines = append(t.lines, withBg(bluef("▌")+"  "+highlightImages(l, colorText), bgUser, avail))
	}
	t.lines = append(t.lines, blank, "")
	t.trim()
	t.scroll = 0
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
		since := time.Now()
		for i := 0; ; i++ {
			t.mu.Lock()
			t.transit = "  " + bluef(spinFrames[i%len(spinFrames)]) + " " +
				color(italic+colorText, text) + " " + grayf(fmt.Sprintf("%.0fs", time.Since(since).Seconds()))
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
	t.scroll = 0
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

func (t *screen) panelVisible() bool { return t.w >= 110 }

func (t *screen) conversationWidth() int {
	if t.panelVisible() {
		return t.w - panelWidth - 2
	}
	return t.w - 2
}

// drawLocked paints the whole frame. Called with t.mu held.
//
// Rows: 1 margin · 2..h-7 conversation · h-6 blank/scroll ·
// h-5..h-1 input box (with padding) · h footer. The panel takes the right-hand
// columns on rows 1..h-1; nothing is written to the last column of the last
// row (that would scroll the terminal).
func (t *screen) drawLocked() {
	fullClear := t.measure()
	w, h := t.w, t.h
	avail := t.conversationWidth()
	// Long text in the input box wraps into several lines, and the box grows
	// upwards (up to a third of the screen), like in OpenCode.
	boxWidth := avail - 2
	boxText := ""
	if t.partial == "" {
		boxText = t.visibleInput()
	}
	boxLines := wrapInput(boxText, boxWidth-4)
	if ceiling := max(1, h/3); len(boxLines) > ceiling {
		boxLines = boxLines[len(boxLines)-ceiling:]
	}
	grow := len(boxLines) - 1
	// Row 1 is margin; the conversation goes from row 2 to h-7-grow.
	height := max(3, h-8-grow)

	var sb strings.Builder
	sb.WriteString(escSyncStart + escHideCursor)
	if fullClear {
		sb.WriteString(escClear)
	}
	line := func(r int, s string) {
		fmt.Fprintf(&sb, "\033[%d;1H%s%s%s", r, reset, s, escClearLine)
	}

	// No title line: the session name goes to the terminal tab (OSC 0) and to
	// the panel. bh has one session per terminal, so OpenCode's tab bar makes
	// no sense here.
	// An unnamed session shows as "New session", same as the panel. The title
	// is compared after resolving: comparing the raw one left the tab untitled
	// on open (empty == initial value of lastTabTitle).
	tabName := t.state.title
	if strings.TrimSpace(tabName) == "" {
		tabName = "New session"
	}
	if tabName != t.lastTabTitle {
		t.lastTabTitle = tabName
		sb.WriteString(tabTitle(tabName))
	}

	// conversation
	var all []string
	for _, l := range t.lines {
		all = append(all, wrapVis(l, avail-2)...)
	}
	if t.partial != "" {
		all = append(all, wrapVis("  "+t.partial+t.visibleInput(), avail-2)...)
	}
	if t.transit != "" {
		all = append(all, t.transit)
	}
	if t.scroll > len(all)-height {
		t.scroll = max(0, len(all)-height)
	}
	end := len(all) - t.scroll
	vis := all[max(0, end-height):end]
	// 2-column left margin, the same as the input box.
	line(1, "")
	for i := 0; i < height; i++ {
		s := ""
		if i < len(vis) {
			s = conversationMargin(vis[i]) + vis[i]
		}
		line(2+i, s)
	}
	if t.scroll > 0 {
		line(h-6-grow, "   "+grayf(fmt.Sprintf("↓ %d lines below · PgDn to return", t.scroll)))
	} else {
		line(h-6-grow, "")
	}

	// input box; while a command prompt awaits an answer, the typed text shows
	// on the prompt line itself and the box stays empty.
	// One line of padding above and below, and text 2 spaces from the bar,
	// like OpenCode's box.
	blankBox := "  " + withBg(bluef("▌"), bgBox, boxWidth)
	line(h-5-grow, blankBox)
	for i, l := range boxLines {
		if !t.masked {
			l = highlightImages(l, "") // pasted [Image N] in yellow
		}
		line(h-4-grow+i, "  "+withBg(bluef("▌")+"  "+l, bgBox, boxWidth))
	}
	line(h-3, blankBox)
	info := boldf(color(colorText, t.state.model)) + " " + grayf(t.state.provider)
	if t.state.effort != "" {
		info += grayf(" · ") + orangef(t.state.effort)
	}
	line(h-2, "  "+withBg(bluef("▌")+"  "+info, bgBox, boxWidth))
	line(h-1, blankBox)

	// footer
	left := "  " + grayf(t.state.cwd)
	var rightParts []string
	switch {
	case t.notice != "":
		rightParts = append(rightParts, yellowf(t.notice))
	case t.busy:
		rightParts = append(rightParts, boldf(color(colorText, "esc"))+" "+grayf("interrupt"))
	}
	if t.state.ctxTokens > 0 {
		rightParts = append(rightParts, fmtContext(t.state.ctxTokens, t.state.window))
	}
	rightParts = append(rightParts, boldf(color(colorText, "/help"))+" "+grayf("commands"))
	dir := strings.Join(rightParts, "  ")
	line(h, left+strings.Repeat(" ", max(1, avail-visWidth(left)-visWidth(dir)))+dir)

	// side panel
	if t.panelVisible() {
		col := w - panelWidth + 1
		panel := t.panelLines()
		// Panel with a top margin (row 1 free) and one inner line of padding
		// before the title, aligned with the top of the conversation.
		for r := 2; r <= h-1; r++ {
			s := ""
			if r-3 >= 0 && r-3 < len(panel) {
				s = panel[r-3]
			}
			fmt.Fprintf(&sb, "\033[%d;%dH%s%s", r, col, reset, withBg(s, bgPanel, panelWidth-2))
		}
	}

	// cursor: in the box (also during the turn, to type the next message right
	// away), or at the end of the command prompt line
	if t.partial == "" {
		fmt.Fprintf(&sb, "\033[%d;%dH", h-4, 6+visWidth(boxLines[len(boxLines)-1]))
		sb.WriteString(escShowCursor)
	} else if len(vis) > 0 {
		last := vis[len(vis)-1]
		fmt.Fprintf(&sb, "\033[%d;%dH", 2+len(vis)-1, 1+visWidth(conversationMargin(last)+last))
		sb.WriteString(escShowCursor)
	}
	sb.WriteString(escSyncEnd)
	fmt.Fprint(t.out, sb.String())
}

// conversationMargin: boxes (user messages) start at the input box column;
// the rest of the conversation starts at the column of the TEXT inside them,
// like in OpenCode.
func conversationMargin(l string) string {
	if colorsOn && strings.HasPrefix(l, bgUser) {
		return "  "
	}
	return "   "
}

func (t *screen) visibleInput() string {
	if t.masked {
		return strings.Repeat("•", utf8.RuneCountInString(t.input))
	}
	return t.input
}

func (t *screen) panelLines() []string {
	e := t.state
	title := e.title
	if title == "" {
		title = "New session"
	}
	var out []string
	for _, l := range strings.Split(wrap(title, panelWidth-6), "\n") {
		out = append(out, "  "+boldf(color(colorText, l)))
	}
	out = append(out, "", "  "+boldf(color(colorText, "Context")))
	if e.ctxTokens > 0 {
		out = append(out, "  "+grayf(thousands(e.ctxTokens)+" tokens"))
		if e.window > 0 {
			txt := fmt.Sprintf("%d%% used", 100*e.ctxTokens/e.window)
			if 100*e.ctxTokens/e.window == 0 {
				txt = "<1% used"
			}
			out = append(out, "  "+grayf(txt))
		}
	} else {
		out = append(out, "  "+grayf("—"))
	}
	out = append(out, "", "  "+boldf(color(colorText, "▼ MCP")))
	if len(e.mcps) == 0 {
		out = append(out, "  "+grayf("no servers"))
	}
	for _, m := range e.mcps {
		name := truncateVis(m, panelWidth-18)
		pad := strings.Repeat(" ", max(1, panelWidth-17-visWidth(name)))
		out = append(out, "  "+greenf("•")+" "+boldf(color(colorText, name))+pad+grayf("Connected"))
	}
	return out
}

// ── keyboard ──────────────────────────────────────────────────────────────

type keyEvent struct {
	text   string
	action string
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
				// SGR mouse: "<button;col;rowM". 64/65 are wheel up/down;
				// clicks and drags are ignored.
				var seq []byte
				for {
					x, err := r.ReadByte()
					if err != nil || x == 'M' || x == 'm' {
						break
					}
					seq = append(seq, x)
				}
				button, _, _ := strings.Cut(string(seq), ";")
				switch button {
				case "64":
					ch <- keyEvent{action: "rodacima"}
				case "65":
					ch <- keyEvent{action: "rodabaixo"}
				}
			case '2':
				// "\033[200~" opens a bracketed paste; other "2…" sequences
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
			case "pgup", "pgdn", "rodacima", "rodabaixo":
				edit(func() {
					step := max(3, t.h-10)
					if strings.HasPrefix(k.action, "roda") {
						step = 3
					}
					if k.action == "pgup" || k.action == "rodacima" {
						t.scroll += step
					} else {
						t.scroll = max(0, t.scroll-step)
					}
				})
			case "ctrl+l":
				edit(func() { t.w = 0 }) // forces a clear and repaint
			case "esc", "ctrl+c":
				if busy {
					select {
					case sub.cancelCh <- struct{}{}:
					default:
					}
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

// Pensou closes "Thinking" like OpenCode: a "+ Thought · 8.1s" line in place
// of the spinner.
func (r *screenTurn) Pensou(_ string, dur time.Duration) {
	r.t.stopSpin()
	r.t.Adicionar("  " + orangef("+ Thought") + grayf(" · "+fmtDur(dur)))
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
