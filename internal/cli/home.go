// Home screen: what the full screen shows before the first message, in the
// layout of OpenCode's routes/home.tsx — the wordmark and the prompt box
// centered in the pane, and a footer with the MCP count and the version.
package cli

import (
	"fmt"
	"strings"
)

// The wordmark uses the same block font as OpenCode's logo.ts: each glyph is
// four columns wide and four rows tall ('_' is a cell filled with the shadow
// colour, '^' a ▀ over the shadow, '~' a ▀ in the shadow colour, ',' a ▄ in
// the shadow colour). "Barroso" is drawn muted and "Harness" bold, as OpenCode
// draws "open" and "code".
var logoGlyphs = map[rune][4]string{
	'B': {"    ", "█▀▀▄", "█^^▄", "▀▀▀ "},
	'a': {"    ", "▀▀▀█", "█▀▀█", "▀▀▀▀"},
	'r': {"    ", "█▀▀▀", "█   ", "▀   "},
	'o': {"    ", "█▀▀█", "█__█", "▀▀▀▀"},
	's': {"    ", "█▀▀▀", "▀▀▀█", "▀▀▀▀"},
	'H': {"    ", "█  █", "█▀▀█", "▀  ▀"},
	'n': {"    ", "█▀▀▄", "█__█", "▀  ▀"},
	'e': {"    ", "█▀▀█", "█^^^", "▀▀▀▀"},
}

// homeLogoLeft and homeLogoRight are the two halves of the wordmark.
const (
	homeLogoLeft  = "Barroso"
	homeLogoRight = "Harness"
)

// homePromptMax is the prompt box's max width on the home screen (maxWidth 75).
const homePromptMax = 75

// logoRows renders one word of the wordmark in the given foreground; bold
// marks the right half, as OpenCode's Logo does.
func logoRows(word, fg string, bold bool) [4]string {
	var rows [4]string
	shadow := "\033[48;2;" + mixHex(rgbOf(fg)[0], rgbOf(fg)[1], rgbOf(fg)[2], 0.75) + "m"
	shadowFg := "\033[38;2;" + mixHex(rgbOf(fg)[0], rgbOf(fg)[1], rgbOf(fg)[2], 0.75) + "m"
	attr := ""
	if bold {
		attr = "\033[1m"
	}
	for i, ch := range word {
		g := logoGlyphs[ch]
		for r := 0; r < 4; r++ {
			if i > 0 {
				rows[r] += " "
			}
			for _, c := range g[r] {
				switch c {
				case '_':
					rows[r] += shadow + " " + reset + bgScreen
				case '^':
					rows[r] += attr + fg + shadow + "▀" + reset + bgScreen
				case '~':
					rows[r] += shadowFg + "▀" + reset + bgScreen
				case ',':
					rows[r] += shadowFg + "▄" + reset + bgScreen
				case ' ':
					rows[r] += " "
				default:
					rows[r] += attr + fg + string(c) + reset + bgScreen
				}
			}
		}
	}
	return rows
}

// rgbOf extracts r, g, b from a 24-bit foreground escape ("\033[38;2;r;g;bm").
func rgbOf(esc string) [3]int {
	var r, g, b int
	_, _ = fmt.Sscanf(strings.TrimPrefix(esc, "\033[38;2;"), "%d;%d;%dm", &r, &g, &b)
	return [3]int{r, g, b}
}

// homeLogo is the full wordmark: both halves side by side with one column
// between them, or stacked when the pane is narrower than the wordmark.
func homeLogo(width int) []string {
	left := logoRows(homeLogoLeft, colorGray, false)
	right := logoRows(homeLogoRight, colorText, true)
	if visWidth(left[1])+1+visWidth(right[1]) <= width {
		out := make([]string, 4)
		for i := range out {
			out[i] = left[i] + " " + right[i]
		}
		return out
	}
	out := append([]string{}, left[1:]...)
	return append(out, right[:]...)
}

// isHome reports whether the home screen is showing: nothing has been said yet
// and no command prompt is waiting for an answer.
func (t *screen) isHome() bool {
	return len(t.lines) == 0 && len(t.items) == 0 && t.partial == "" && t.transit == "" &&
		!t.busy && len(t.queued) == 0
}

// homeFrame lays out the home screen: the pane is a column centered in the
// terminal, flexible space above and below (OpenCode puts 3 extra rows above
// the logo), the logo, one row, then the prompt (maxWidth 75, paddingTop 1).
// The footer row keeps its usual place at the bottom.
func (t *screen) homeFrame() (frame, int) {
	f := frame{w: t.w, h: t.h}
	pad := t.panePadCols()
	boxW := min(homePromptMax, t.w-2*pad)
	left := (t.w-boxW)/2 + 1
	boxN := len(t.boxLines(max(6, t.h/3)))
	logoN := len(homeLogo(t.w - 2*pad))
	if t.h < 12 {
		logoN = 0
	}
	// logo + 1 blank + prompt (padding, text, gap, meta, cap) + footer row
	block := logoN + 1 + 1 + boxN + 1 + 1 + 1 + 1
	top := max(1, (t.h-block-3)/2+3)
	logoRow := top
	f.boxTop = logoRow + logoN + 1 + 1
	f.boxTextRow = f.boxTop + 1
	f.boxTextN = boxN
	f.boxGapRow = f.boxTextRow + boxN
	f.boxMetaRow = f.boxGapRow + 1
	f.boxCapRow = f.boxMetaRow + 1
	f.footerRow = f.boxCapRow + 1
	f.barCol = left
	f.textCol = f.barCol + 3
	f.boxWidth = left + boxW - 1
	f.paneCols = boxW
	f.statusRow, f.convRow, f.convHeight = 0, 0, 0
	return f, logoRow
}

// drawHome paints the home screen.
func (t *screen) drawHome(sb *strings.Builder) frame {
	f, logoRow := t.homeFrame()
	line := func(r int, s string) {
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	for r := 1; r <= t.h; r++ {
		line(r, "")
	}
	if t.h >= 12 {
		for i, l := range homeLogo(t.w - 2*t.panePadCols()) {
			col := max(1, (t.w-visWidth(l))/2+1)
			fmt.Fprintf(sb, "\033[%d;%dH%s%s", logoRow+i, col, bgScreen, l)
		}
	}
	t.drawBox(sb, f)
	// The location and the shortcut hint sit under the prompt, aligned to it.
	left, right := t.footerParts(f.boxWidth - f.barCol + 1)
	gap := max(1, f.boxWidth-f.barCol+1-visWidth(left)-visWidth(right))
	fmt.Fprintf(sb, "\033[%d;%dH%s%s", f.footerRow, f.barCol-1, bgScreen,
		strings.TrimPrefix(left, " ")+strings.Repeat(" ", gap)+right)
	t.drawHomeFooter(sb)
	return f
}

// drawHomeFooter paints OpenCode's home footer: "⊙ N MCP /mcps" on the left
// (clickable, opens the MCP dialog) and the version on the right.
func (t *screen) drawHomeFooter(sb *strings.Builder) {
	if t.h < 14 || t.w < 44 {
		return
	}
	row := t.h - 1
	if t.h < 16 {
		row = t.h
	}
	left := ""
	if n := len(t.state.mcps); n > 0 {
		live, bad := 0, 0
		for _, m := range t.state.mcps {
			if m.falha {
				bad++
				continue
			}
			live++
		}
		switch {
		case bad > 0:
			left = redf("⊙ ") + color(colorText, fmt.Sprintf("%d MCP failed", bad))
		case live > 0:
			left = greenf("⊙ ") + color(colorText, fmt.Sprintf("%d MCP", live))
		default:
			left = grayf("⊙ ") + color(colorText, fmt.Sprintf("%d MCP", live))
		}
		if t.w >= 64 {
			left += " " + grayf("/mcps")
		}
		t.hits.add(&hitRegion{row: row, col: 3, width: visWidth(left), height: 1, id: "homemcp",
			plain: left, hover: left, action: func() { t.openMCPDialog(); t.drawLocked() }})
	}
	right := ""
	if t.w >= 64 {
		right = grayf(Versao)
	}
	gap := max(1, t.w-4-visWidth(left)-visWidth(right))
	fmt.Fprintf(sb, "\033[%d;1H%s  %s%s%s%s", row, bgScreen, left, strings.Repeat(" ", gap), right, escClearLine)
}

// drawHomeLocked paints a whole home-screen frame (the home counterpart of
// drawLocked). Called with t.mu held.
func (t *screen) drawHomeLocked(fullClear bool) {
	var sb strings.Builder
	sb.WriteString(escSyncStart + escHideCursor)
	if fullClear {
		sb.WriteString(escClear)
	}
	if t.lastTabTitle != "New session" {
		t.lastTabTitle = "New session"
		sb.WriteString(tabTitle("New session"))
	}
	f := t.drawHome(&sb)
	t.drawAC(&sb, f)
	t.drawToast(&sb)
	if t.dlg != nil {
		dimmed := dimFrame(sb.String())
		sb.Reset()
		sb.WriteString(dimmed)
		if row, col := t.drawDialog(&sb); row > 0 {
			fmt.Fprintf(&sb, "\033[%d;%dH%s", row, col, escShowCursor)
		}
		sb.WriteString(escSyncEnd)
		fmt.Fprint(t.out, sb.String())
		return
	}
	row, col := t.caretScreen(f)
	t.promptCol = col
	fmt.Fprintf(&sb, "\033[%d;%dH%s%s", row, col, escShowCursor, escSyncEnd)
	fmt.Fprint(t.out, sb.String())
}
