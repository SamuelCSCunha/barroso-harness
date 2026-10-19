// Turn visuals in the OpenCode style: a spinner while the model thinks, each
// tool on a "✓ name  summarized-argument" line with the output indented and
// dimmed, the answer with basic markdown (headings, bold, code, lists) and a
// "model · duration · tok/s · context %" footer.
//
// All of this only runs with colors on (real terminal). Pipes, -p and tests
// stay on RendererPuro, without a single escape.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// Extra palette from the redesign (the base lives in ui.go).
const (
	colorBlue   = "\033[38;5;75m"
	colorOrange = "\033[38;5;215m"
	colorGray   = "\033[38;5;240m"
	colorText   = "\033[38;5;252m"
	bgCode      = "\033[48;5;236m"
	italic      = "\033[3m"
)

func bluef(s string) string   { return color(colorBlue, s) }
func orangef(s string) string { return color(colorOrange, s) }
func grayf(s string) string   { return color(colorGray, s) }

// bar is the side border of the boxes (user request, input).
const bar = "▌"

// ── turn ──────────────────────────────────────────────────────────────────

// tuiTurn implements runtime.Renderer and runtime.RendererDetalhado.
type tuiTurn struct {
	out     io.Writer
	cfg     *config.Config
	started time.Time

	mu          sync.Mutex
	spin        chan struct{} // closed to stop the spinner
	spinDone    chan struct{}
	ctxTokens   int // prompt tokens of the last request
	pendingTool string
	pendingArgs string
}

func newTUITurn(out io.Writer, cfg *config.Config) *tuiTurn {
	return &tuiTurn{out: out, cfg: cfg, started: time.Now()}
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner cursor sequences.
const (
	hideCursor = "\033[?25l"
	showCursor = "\033[?25h"
)

// Pensando animates "⠋ Thinking" on the current line until the turn's next output.
func (t *tuiTurn) Pensando() {
	t.stopSpin()
	t.mu.Lock()
	t.spin = make(chan struct{})
	t.spinDone = make(chan struct{})
	stop, end := t.spin, t.spinDone
	t.mu.Unlock()
	// The cursor is hidden while animating: blinking next to "Thinking" it
	// looks like an input field. It comes back when the spinner stops.
	fmt.Fprint(t.out, hideCursor)
	go func() {
		defer close(end)
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		i := 0
		since := time.Now()
		for {
			fmt.Fprintf(t.out, "\r  %s %s %s\033[K", bluef(spinFrames[i%len(spinFrames)]),
				color(italic+colorText, "Thinking"), grayf(fmt.Sprintf("%.0fs", time.Since(since).Seconds())))
			i++
			select {
			case <-stop:
				fmt.Fprint(t.out, "\r\033[K"+showCursor)
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *tuiTurn) stopSpin() {
	t.mu.Lock()
	stop, end := t.spin, t.spinDone
	t.spin, t.spinDone = nil, nil
	t.mu.Unlock()
	if stop != nil {
		close(stop)
		<-end
	}
}

func (t *tuiTurn) Contexto(promptTokens int) {
	t.mu.Lock()
	t.ctxTokens = promptTokens
	t.mu.Unlock()
}

// ToolInicio stores the summarized argument and shows the tool running.
func (t *tuiTurn) ToolInicio(name, args string) {
	t.stopSpin()
	summary := summarizeArgs(args)
	t.mu.Lock()
	t.pendingTool, t.pendingArgs = name, summary
	t.mu.Unlock()
	fmt.Fprintf(t.out, "\r  %s %s %s\033[K", yellowf("●"), color(colorText, name), grayf(summary))
}

func (t *tuiTurn) Tool(name string, dur time.Duration, failed bool) {
	t.stopSpin()
	t.mu.Lock()
	summary := t.pendingArgs
	t.pendingTool, t.pendingArgs = "", ""
	t.mu.Unlock()
	mark := greenf("✓")
	if failed {
		mark = redf("✗")
	}
	fmt.Fprintf(t.out, "\r  %s %s %s %s\033[K\n", mark, color(colorText, name), grayf(summary), grayf(fmtDur(dur)))
}

// Eco: tool output indented under "›", at most 6 lines.
func (t *tuiTurn) Eco(output string) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return
	}
	lines := strings.Split(output, "\n")
	const max = 6
	extra := 0
	if len(lines) > max {
		extra = len(lines) - max
		lines = lines[:max]
	}
	for i, l := range lines {
		pre := "    "
		if i == 0 {
			pre = "  " + grayf("›") + " "
		}
		fmt.Fprintln(t.out, pre+grayf(truncate(l, terminalWidth()-6)))
	}
	if extra > 0 {
		fmt.Fprintln(t.out, "    "+grayf(fmt.Sprintf("… +%d lines", extra)))
	}
}

func (t *tuiTurn) Pensou(_ string, dur time.Duration) {
	t.stopSpin()
	fmt.Fprintf(t.out, "  %s%s\n", orangef("+ Thought"), grayf(" · "+fmtDur(dur)))
}

func (t *tuiTurn) Cortada(ceiling int, gaveUp bool) {
	t.stopSpin()
	msg := fmt.Sprintf("response cut at the output limit (%d tokens); continuing", ceiling)
	if gaveUp {
		msg = fmt.Sprintf("response cut at the output limit (%d tokens) again; stopped", ceiling)
	}
	fmt.Fprintf(t.out, "  %s %s\n", yellowf("⚠"), grayf(msg))
}

func (t *tuiTurn) Guardrail(proactive bool) {
	t.stopSpin()
	msg := "context compacted by the guardrail (provider rejected the request size)"
	if proactive {
		msg = "context compacted"
	}
	fmt.Fprintf(t.out, "  %s %s\n", purplef("◆"), grayf(msg))
}

func (t *tuiTurn) Resposta(text string) {
	t.stopSpin()
	fmt.Fprintln(t.out)
	fmt.Fprintln(t.out, renderMarkdown(text, terminalWidth()))
}

// Tokens is the turn footer: "model · 12.3s · 25.3 tok/s · 9.8K (5%)". No
// mode label: BH has no modes, it is always YOLO.
func (t *tuiTurn) Tokens(prompt, output int, dur time.Duration, compacted bool) {
	t.stopSpin()
	parts := []string{color(colorText, t.cfg.Model), fmtDur(dur)}
	if output > 0 && dur > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", float64(output)/dur.Seconds()))
	}
	t.mu.Lock()
	ctx := t.ctxTokens
	t.mu.Unlock()
	if ctx > 0 {
		parts = append(parts, fmtContext(ctx, t.cfg.Janela))
	}
	if compacted {
		parts = append(parts, purplef("compacted"))
	}
	fmt.Fprintln(t.out)
	fmt.Fprintln(t.out, "  "+strings.Join(parts, grayf(" · ")))
}

// ── boxes: header, input, request ─────────────────────────────────────────

// headerTUI is the opening: the block logo and a hint line.
func headerTUI(out io.Writer, version string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+grayf("▄▄ ▄▄")+"  "+boldf(color(colorText, "barroso"))+boldf(bluef(" harness")))
	fmt.Fprintln(out, "  "+grayf("▀▀ ▀▀")+"  "+grayf("v"+strings.TrimPrefix(version, "v")+" · /help commands · ctrl+c interrupt"))
	fmt.Fprintln(out)
}

// inputStatus is the line under the input box: model, provider, effort,
// context and MCPs. No mode label like OpenCode's "Build": BH is always
// YOLO.
func inputStatus(out io.Writer, cfg *config.Config, session string, mcps int, ctxTokens int) {
	model := cfg.Model
	if model == "" {
		model = "no model"
	}
	parts := []string{boldf(color(colorText, model)), grayf(cfg.CurrentProvider)}
	if cfg.Effort != "" {
		parts = append(parts, orangef(cfg.Effort))
	}
	left := "  " + bluef(bar) + " " + strings.Join(parts, grayf(" · "))
	var dir []string
	if ctxTokens > 0 {
		dir = append(dir, fmtContext(ctxTokens, cfg.Janela))
	}
	if mcps > 0 {
		dir = append(dir, greenf("●")+grayf(fmt.Sprintf(" %d MCP", mcps)))
	}
	if session != "" {
		dir = append(dir, grayf("sess "+shortSession(session)))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, left+"   "+strings.Join(dir, grayf("  ")))
}

// printPromptTUI is the typing line, inside the blue-bordered "box".
func printPromptTUI(out io.Writer) {
	fmt.Fprint(out, "  "+bluef(bar)+" "+bluef("›")+" ")
}

// echoRequest redraws the user's request as a bordered block, the way
// OpenCode shows the sent message: it erases the status line and the typed
// line(s) — a long line wraps into several on the terminal — and rewrites
// the request alone in the box, with a padding line before the turn.
func echoRequest(out io.Writer, request string) {
	width := terminalWidth()
	typedLines := (promptWidth + utf8.RuneCountInString(request) + width - 1) / width
	if typedLines < 1 {
		typedLines = 1
	}
	// +1: the status line right above the input.
	for i := 0; i < typedLines+1; i++ {
		fmt.Fprint(out, "\033[1A\r\033[K")
	}
	for _, l := range strings.Split(wrap(request, width-6), "\n") {
		fmt.Fprintln(out, "  "+bluef(bar)+" "+color(colorText, l))
	}
	fmt.Fprintln(out)
}

// promptWidth: columns of "  ▌ › " before the typed text.
const promptWidth = 6

// ── markdown ──────────────────────────────────────────────────────────────

var (
	reBold   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reItalic = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
	reCode   = regexp.MustCompile("`([^`\n]+)`")
)

// renderMarkdown turns the most common markdown into color: headings, bold,
// italic, inline code, code blocks and lists. Everything else passes through.
func renderMarkdown(text string, width int) string {
	var out []string
	inCode := false
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
			if inCode {
				if lang := strings.TrimPrefix(t, "```"); lang != "" {
					out = append(out, "  "+grayf(lang))
				}
			}
			continue
		}
		if inCode {
			out = append(out, "  "+color(bgCode+colorText, " "+l+" "))
			continue
		}
		switch {
		case strings.HasPrefix(t, "#"):
			title := strings.TrimSpace(strings.TrimLeft(t, "#"))
			out = append(out, "  "+boldf(bluef(inline(title))))
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			indent := strings.Repeat(" ", len(l)-len(strings.TrimLeft(l, " ")))
			out = append(out, "  "+indent+bluef("•")+" "+inline(t[2:]))
		case t == "---" || t == "***":
			out = append(out, "  "+grayf(strings.Repeat("─", min(width-4, 60))))
		default:
			for _, q := range strings.Split(wrap(l, width-4), "\n") {
				out = append(out, "  "+inline(q))
			}
		}
	}
	return strings.Join(out, "\n")
}

func inline(s string) string {
	if !colorsOn {
		return s
	}
	s = reCode.ReplaceAllString(s, colorOrange+"$1"+reset)
	s = reBold.ReplaceAllString(s, bold+"$1"+reset)
	s = reItalic.ReplaceAllString(s, "$1"+italic+"$2"+reset)
	return s
}

// ── utilities ─────────────────────────────────────────────────────────────

// summarizeArgs picks the most informative argument of the call (command,
// path, url, query…) for the tool line, like OpenCode's "✓ read ~/x.go".
func summarizeArgs(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || len(m) == 0 {
		return ""
	}
	for _, k := range []string{"command", "path", "url", "query", "name", "prompt", "pattern", "id", "estado", "content"} {
		if v, ok := m[k]; ok {
			s := strings.Join(strings.Fields(fmt.Sprint(v)), " ")
			return truncate(s, 70)
		}
	}
	return ""
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// fmtContexto: "9.8K (5%)" — the same format as OpenCode's corner.
func fmtContext(tokens, window int) string {
	k := fmt.Sprintf("%.1fK", float64(tokens)/1000)
	if window <= 0 {
		return grayf(k)
	}
	pct := 100 * tokens / window
	txt := fmt.Sprintf("%s (%d%%)", k, pct)
	if pct == 0 {
		txt = k + " (<1%)"
	}
	if pct == 0 {
		txt = k + " (<1%)"
	}
	switch {
	case pct >= 80:
		return redf(txt)
	case pct >= 50:
		return yellowf(txt)
	default:
		return grayf(txt)
	}
}

func truncate(s string, max int) string {
	if max < 8 {
		max = 8
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// wrap splits by words at a width; empty lines are preserved.
func wrap(s string, width int) string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		words := strings.Fields(line)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		current := indent
		for _, p := range words {
			if utf8.RuneCountInString(current)+utf8.RuneCountInString(p)+1 > width && strings.TrimSpace(current) != "" {
				out = append(out, current)
				current = indent
			}
			if strings.TrimSpace(current) != "" {
				current += " "
			}
			current += p
		}
		out = append(out, current)
	}
	return strings.Join(out, "\n")
}
