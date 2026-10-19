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

// Only images whose placeholder is still in the text are sent, in text order.
func TestSelectImages(t *testing.T) {
	imgs := []pastedImage{{1, "a.png"}, {2, "b.png"}, {3, "c.png"}}
	got := selectImages("see [Image 3] and [Image 1] and [Image 2", imgs)
	if !reflect.DeepEqual(got, []string{"c.png", "a.png"}) {
		t.Fatalf("selected = %v", got)
	}
	if got := selectImages("no images", imgs); got != nil {
		t.Fatalf("selected = %v", got)
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
	tl.input = "look "
	tl.applyPaste(pasteResult{n: 1, path: path})
	tl.applyPaste(pasteResult{n: 2, path: "other.png"})
	if tl.input != "look [Image 1][Image 2]" {
		t.Fatalf("input = %q", tl.input)
	}
	tl.queueImages("look [Image 2]")
	if tl.imageCount != 0 || tl.images != nil {
		t.Fatal("numbering should reset after send")
	}
	if got := tl.takeImages(" look [Image 2]\n"); !reflect.DeepEqual(got, []string{"other.png"}) {
		t.Fatalf("taken = %v", got)
	}
	if got := tl.takeImages("look [Image 2]"); got != nil {
		t.Fatalf("taken twice = %v", got)
	}

	clipboardRead = func(string) (clipboardResult, error) { return clipboardResult{text: "hi\nthere"}, nil }
	_, text, _ = pasteClipboard(1)
	tl.input = ""
	tl.applyPaste(pasteResult{n: 1, text: text})
	if tl.input != "hi there" {
		t.Fatalf("text paste = %q", tl.input)
	}

	tl.applyPaste(pasteResult{err: errors.New("boom")})
	if !strings.Contains(tl.notice, "paste failed") {
		t.Fatalf("notice = %q", tl.notice)
	}
}

// Placeholders are painted yellow.
func TestHighlightImages(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	got := highlightImages("a [Image 1] b", "")
	if !strings.Contains(got, yellowf("[Image 1]")) || !strings.HasPrefix(got, "a ") {
		t.Fatalf("highlight = %q", got)
	}
}
