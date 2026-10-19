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
// top and the title only goes to the tab and the panel.
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
	if !strings.Contains(s, "\033[2;1H\033[0m   first conversation line") {
		t.Fatalf("conversation should start on row 2, after the margin")
	}
}

// On open, with no messages yet, the tab already shows "BH | New session".
func TestTabShowsNewSessionOnOpen(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	before := screenSize
	screenSize = func() (int, int) { return 140, 30 }
	defer func() { screenSize = before }()

	var sb strings.Builder
	tl := &screen{out: &sb}
	tl.Estado(panelState{model: "m"})
	if !strings.Contains(sb.String(), "\033]0;BH | New session\a") {
		t.Fatal("tab should open as 'BH | New session'")
	}
}

// The mouse wheel arrives as SGR and becomes scrolling; clicks are ignored.
func TestMouseWheel(t *testing.T) {
	ch := make(chan keyEvent, 8)
	readKeys(bufio.NewReader(strings.NewReader("\x1b[<64;10;5M\x1b[<0;3;3M\x1b[<0;3;3m\x1b[<65;10;5Ma")), ch)
	var got []string
	for k := range ch {
		got = append(got, k.action+k.text)
	}
	if strings.Join(got, ",") != "rodacima,rodabaixo,a" {
		t.Fatalf("keys = %v", got)
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
	joined := strings.Join(tl.lines, "\n")
	order := []string{"list the files", "terminal_run", "exit_code: 0", "file_read", "package a", "done, two files"}
	pos := 0
	for _, s := range order {
		i := strings.Index(joined[pos:], s)
		if i < 0 {
			t.Fatalf("missing %q in the right order:\n%s", s, joined)
		}
		pos += i + len(s)
	}
	if !strings.Contains(joined, "✗") {
		t.Fatal("a failed tool should show ✗")
	}
}
