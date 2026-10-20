package cli

import (
	"bufio"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func readAllKeys(t *testing.T, in string) []keyEvent {
	t.Helper()
	ch := make(chan keyEvent, 16)
	readKeys(bufio.NewReader(strings.NewReader(in)), ch)
	var got []keyEvent
	for k := range ch {
		got = append(got, k)
	}
	return got
}

// A bracketed text paste arrives as one "paste" event; its newline does not
// become an Enter.
func TestBracketedTextPaste(t *testing.T) {
	got := readAllKeys(t, "a\x1b[200~line one\r\nline two\x1b[201~b")
	want := []keyEvent{{text: "a"}, {action: "paste", text: "line one\r\nline two"}, {text: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %#v", got)
	}
	if s := pasteText(got[1].text); s != "line one line two" {
		t.Fatalf("pasteText = %q", s)
	}
}

// An empty bracketed paste (Windows Terminal with an image-only clipboard)
// and a raw Ctrl+V both ask for the clipboard image.
func TestEmptyPasteAndCtrlVRequestImage(t *testing.T) {
	got := readAllKeys(t, "\x1b[200~\x1b[201~\x16\x1b[2~x")
	want := []keyEvent{{action: "pasteimage"}, {action: "pasteimage"}, {text: "x"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %#v", got)
	}
}

// The clipboard reader is injectable: an image becomes [Image N] in the
// input and travels with the sent line; text goes in as typed; a failure
// becomes a footer notice.
func TestPasteImageIntoInput(t *testing.T) {
	before := clipboardRead
	defer func() { clipboardRead = before }()
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())

	clipboardRead = func(dest string) (clipboardResult, error) { return clipboardResult{image: true}, nil }
	tl := &screen{out: &strings.Builder{}}
	path, text, err := pasteClipboard(1)
	if err != nil || path == "" || text != "" {
		t.Fatalf("paste = %q %q %v", path, text, err)
	}
	tl.editor().Insert("look ")
	tl.applyPaste(pasteResult{n: 1, path: path})
	tl.applyPaste(pasteResult{n: 2, path: "other.png"})
	if got := tl.editor().Text(); got != "look [Image 1][Image 2]" {
		t.Fatalf("input = %q", got)
	}
	// Deleting [Image 1] drops its image from the message (atomic backspace).
	tl.editor().LineStart()
	for range len("look [Image 1]") {
		tl.editor().Right()
	}
	tl.editor().Backspace()
	_, sent, imgs := tl.takeInput()
	tl.queueSent(sent, imgs)
	if tl.imageCount != 0 || !tl.editor().empty() {
		t.Fatal("the prompt and its numbering reset after send")
	}
	if got := tl.takeImages(" look [Image 2]\n"); !reflect.DeepEqual(got, []string{"other.png"}) {
		t.Fatalf("taken = %v", got)
	}
	if got := tl.takeImages("look [Image 2]"); got != nil {
		t.Fatalf("taken twice = %v", got)
	}

	// A two-line clipboard text keeps its line break in the prompt.
	clipboardRead = func(string) (clipboardResult, error) { return clipboardResult{text: "hi\r\nthere"}, nil }
	_, text, _ = pasteClipboard(1)
	tl.applyPaste(pasteResult{n: 1, text: text})
	if got := tl.editor().Text(); got != "hi\nthere" {
		t.Fatalf("text paste = %q", got)
	}

	tl.applyPaste(pasteResult{err: errors.New("boom")})
	if tl.toast == nil || tl.toast.variant != "error" || !strings.Contains(tl.toast.text, "Paste failed") {
		t.Fatalf("toast = %+v", tl.toast)
	}
}

// Attachment tokens are painted the way OpenCode's prompt does: inverted chip
// (background.base text on feedback.warning background), bold.
func TestHighlightImages(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	got := highlightImages("a [Image 1] b", "")
	want := boldf(bgWarning + colorText + "[Image 1]")
	if !strings.Contains(got, want) || !strings.HasPrefix(got, "a ") {
		t.Fatalf("highlight = %q", got)
	}
}

// Writing the clipboard goes through OSC 52 and the platform tool; the escape
// is plain base64 and never a raw control byte.
func TestOsc52(t *testing.T) {
	got := osc52("hi there")
	if got != "\033]52;c;aGkgdGhlcmU=\a" {
		t.Fatalf("osc52 = %q", got)
	}
}
