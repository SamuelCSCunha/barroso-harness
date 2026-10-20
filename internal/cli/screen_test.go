package cli

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// The session name goes to the terminal tab (OSC 0), without control
// characters that would break the sequence; without a name the tab is "BH".
func TestTabTitle(t *testing.T) {
	if got := tabTitle(""); got != "\033]0;BH\a" {
		t.Fatalf("empty tab = %q", got)
	}
	got := tabTitle("parser\a re\x1bview")
	if got != "\033]0;BH | parser review\a" {
		t.Fatalf("tab = %q", got)
	}
}

// The screen no longer draws a title line: the conversation starts at the
// top and the title only goes to the tab and the panel. The whole frame is
// painted over the screen background (#0a0a0a).
func TestScreenHasNoTitleLine(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	before := screenSize
	screenSize = func() (int, int) { return 140, 30 }
	defer func() { screenSize = before }()

	var sb strings.Builder
	tl := &screen{out: &sb}
	tl.Estado(panelState{title: "my session", model: "m"})
	tl.Adicionar("first conversation line")
	s := sb.String()
	if !strings.Contains(s, "\033]0;BH | my session\a") {
		t.Fatal("title did not go to the tab")
	}
	if !strings.Contains(s, "\033[2;1H"+bgScreen+"   first conversation line") {
		t.Fatalf("conversation should start on row 2, after the margin:\n%s", s)
	}
	if !strings.Contains(s, bgScreen+"\033[K") {
		t.Fatal("every painted row must carry the screen background")
	}
}

// On open, with no messages yet, the tab shows the harness name.
func TestTabShowsHarnessNameOnOpen(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	before := screenSize
	screenSize = func() (int, int) { return 140, 30 }
	defer func() { screenSize = before }()

	var sb strings.Builder
	tl := &screen{out: &sb}
	tl.Estado(panelState{model: "m"})
	if !strings.Contains(sb.String(), "\033]0;BH | BarrosoHarness\a") {
		t.Fatal("tab should open as 'BH | BarrosoHarness'")
	}

	// After the first message the tab shows the session title again.
	tl.Usuario("hello")
	tl.Estado(panelState{model: "m", title: "my session"})
	if !strings.Contains(sb.String(), "\033]0;BH | my session\a") {
		t.Fatal("a titled session must put its title on the tab")
	}
}

// The mouse wheel arrives as SGR and becomes scrolling. Clicks and drags are
// decoded too (press / release / motion), not discarded: the screen dispatches
// them through its hit regions.
func TestMouseWheel(t *testing.T) {
	ch := make(chan keyEvent, 16)
	in := "\x1b[<64;10;5M" + // wheel up
		"\x1b[<0;3;3M" + // left press
		"\x1b[<0;3;3m" + // left release
		"\x1b[<35;3;4M" + // hover: motion, no button held (32 | 3)
		"\x1b[<0;3;5M" + "\x1b[<32;4;5M" + // press, then drag with the left button
		"\x1b[<65;10;5M" + // wheel down
		"a"
	readKeys(bufio.NewReader(strings.NewReader(in)), ch)
	var got []string
	for k := range ch {
		got = append(got, k.action+k.text)
	}
	want := "rodacima,mousedown,mouseup,mousemove,mousedown,mousedrag,rodabaixo,a"
	if strings.Join(got, ",") != want {
		t.Fatalf("keys = %v", got)
	}
}

// Every SGR field decodes: the button in the low two bits, shift 4, alt 8,
// ctrl 16, motion 32, wheel 64.
func TestParseMouseSeq(t *testing.T) {
	tests := []struct {
		in   string
		want mouseSeq
		ok   bool
	}{
		{"0;6;2M", mouseSeq{Col: 6, Row: 2, Button: 0}, true},
		{"0;6;2m", mouseSeq{Col: 6, Row: 2, Button: 0, Release: true}, true},
		// left button held, moving: 32 | 0
		{"32;10;4M", mouseSeq{Col: 10, Row: 4, Button: 0, Drag: true, Motion: true}, true},
		// hover, nothing held: 35 = 32 | 3
		{"35;10;4M", mouseSeq{Col: 10, Row: 4, Button: 3, Motion: true}, true},
		// wheel up / down (the terminal reports 64+low bits: 0 → 64 up, 1 → 65 down)
		{"64;1;1M", mouseSeq{Col: 1, Row: 1, Button: 0, Wheel: 64}, true},
		{"65;1;1M", mouseSeq{Col: 1, Row: 1, Button: 1, Wheel: 65}, true},
		// modifiers: shift 4 + alt 8 + ctrl 16 = 28
		{"28;5;5M", mouseSeq{Col: 5, Row: 5, Button: 0, Shift: true, Alt: true, Ctrl: true}, true},
		// right button
		{"2;7;7m", mouseSeq{Col: 7, Row: 7, Button: 2, Release: true}, true},
	}
	for _, tc := range tests {
		got, ok := parseMouseSeq(tc.in)
		if ok != tc.ok {
			t.Fatalf("parseMouseSeq(%q) ok = %v", tc.in, ok)
		}
		if tc.ok && got != tc.want {
			t.Fatalf("parseMouseSeq(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if _, ok := parseMouseSeq("0;6"); ok {
		t.Fatal("a truncated sequence must not decode")
	}
	if _, ok := parseMouseSeq("x;6;2M"); ok {
		t.Fatal("a non-numeric payload must not decode")
	}
}

// A cancel request left over from a finished turn does not kill the next
// turn; a request made during the turn does.
func TestTurnContextIgnoresStaleCancel(t *testing.T) {
	io := IO{cancelCh: make(chan struct{}, 1)}
	io.cancelCh <- struct{}{}
	ctx, cancel := io.turnContext(make(chan struct{}))
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("stale request canceled the new turn")
	case <-time.After(50 * time.Millisecond):
	}
	io.cancelCh <- struct{}{}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("request during the turn did not cancel")
	}
}

// Long text in the input box wraps by word; a word longer than the line
// breaks in the middle.
func TestWrapInput(t *testing.T) {
	got := wrapInput("one two thre four five", 12)
	if strings.Join(got, "|") != "one two thre|four five" {
		t.Fatalf("wrap = %q", got)
	}
	got = wrapInput(strings.Repeat("x", 25), 10)
	if strings.Join(got, "|") != "xxxxxxxxxx|xxxxxxxxxx|xxxxx" {
		t.Fatalf("long word = %q", got)
	}
	if got := wrapInput("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty = %q", got)
	}
}

// /resume in full screen redraws the conversation: question, each tool next
// to its result, and the answer.
func TestReplayHistory(t *testing.T) {
	before := screenSize
	screenSize = func() (int, int) { return 140, 40 }
	defer func() { screenSize = before }()
	var sb strings.Builder
	tl := &screen{out: &sb}
	tl.Estado(panelState{model: "m"}) // measures the screen, as in the REPL
	replayHistory(tl, []dealer.Mensagem{
		{Role: "user", Conteudo: "list the files"},
		{Role: "assistant", Conteudo: ""},
		{Role: "tool_call", ToolID: "1", ToolName: "terminal_run", ToolArgs: `{"command":"dir"}`},
		{Role: "tool_call", ToolID: "2", ToolName: "file_read", ToolArgs: `{"path":"a.go"}`},
		{Role: "tool_result", ToolID: "1", ToolName: "terminal_run", Conteudo: "exit_code: 0\nx"},
		{Role: "tool_result", ToolID: "2", ToolName: "file_read", Conteudo: "package a", IsErro: true},
		{Role: "assistant", Conteudo: "done, two files"},
	})
	// The shell call is a "$ command" block with its output; the failed read is
	// an inline row (red, its error behind a click), as OpenCode draws them.
	joined := tl.transcriptText()
	order := []string{"list the files", "$ dir", "x", "→ Read a.go", "done, two files"}
	pos := 0
	for _, s := range order {
		i := strings.Index(joined[pos:], s)
		if i < 0 {
			t.Fatalf("missing %q in the right order:\n%s", s, joined)
		}
		pos += i + len(s)
	}
	failed := false
	for _, it := range tl.items {
		for _, m := range append([]*item{it}, it.members...) {
			if m.name == "file_read" && m.failed {
				failed = true
			}
		}
	}
	if !failed {
		t.Fatal("the failed read must be marked as failed")
	}
}
