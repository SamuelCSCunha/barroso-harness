// Decoder tests: every byte sequence the keymap binds must come out as its
// named key. Ctrl+P is the regression that started this: the old reader dropped
// it as an unknown control byte.
package cli

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// decode runs the reader over in and returns one name per event.
func decode(in string) []string {
	ch := make(chan keyEvent, 64)
	readKeys(bufio.NewReader(strings.NewReader(in)), ch)
	var out []string
	for k := range ch {
		out = append(out, keyName(k))
	}
	return out
}

func TestDecodeKeys(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\x10", "ctrl+p"},
		{"\x0e", "ctrl+n"},
		{"\x18", "ctrl+x"},
		{"\x01", "ctrl+a"},
		{"\x05", "ctrl+e"},
		{"\x0b", "ctrl+k"},
		{"\x15", "ctrl+u"},
		{"\x17", "ctrl+w"},
		{"\x14", "ctrl+t"},
		{"\x12", "ctrl+r"},
		{"\x07", "ctrl+g"},
		{"\x1f", "ctrl+-"},
		{"\x08", "ctrl+backspace"},
		{"\x7f", "backspace"},
		{"\r", "enter"},
		{"\n", "newline"}, // ctrl+j
		{"\t", "tab"},
		{"\x1b", "esc"},
		{"\x1b\r", "newline"}, // alt+enter
		{"\x1bb", "alt+b"},
		{"\x1bf", "alt+f"},
		{"\x1b\x7f", "alt+backspace"},
		{"\x1b[A", "cima"},
		{"\x1b[B", "baixo"},
		{"\x1b[C", "right"},
		{"\x1b[D", "left"},
		{"\x1bOA", "cima"},
		{"\x1bOD", "left"},
		{"\x1b[H", "home"},
		{"\x1b[F", "end"},
		{"\x1b[1~", "home"},
		{"\x1b[4~", "end"},
		{"\x1b[3~", "delete"},
		{"\x1b[5~", "pgup"},
		{"\x1b[6~", "pgdn"},
		{"\x1b[1;5D", "ctrl+left"},
		{"\x1b[1;5C", "ctrl+right"},
		{"\x1b[1;3H", "alt+home"},
		{"\x1b[1;3F", "alt+end"},
		{"\x1b[1;2A", "shift+up"},
		{"\x1b[3;5~", "ctrl+delete"},
		{"\x1b[Z", "shift+tab"},
		{"\x1b[13;2u", "newline"}, // shift+enter (CSI-u)
		{"\x1b[13;5u", "newline"}, // ctrl+enter
		{"\x1b[13u", "enter"},
		{"\x1b[112;5u", "ctrl+p"},
		{"\x1b[103;7u", "ctrl+alt+g"},
		{"\x1b\x07", "ctrl+alt+g"},
		{"\x1b\x02", "ctrl+alt+b"},
		{"\x1b[I", ""}, // focus in: dropped
	}
	for _, c := range cases {
		got := decode(c.in)
		joined := strings.Join(got, ",")
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%q should be dropped, got %v", c.in, got)
			}
			continue
		}
		if joined != c.want {
			t.Errorf("%q = %q, want %q", c.in, joined, c.want)
		}
	}
}

// Text, UTF-8 and pastes still arrive as before.
func TestDecodeTextAndPaste(t *testing.T) {
	got := decode("aé😀")
	if strings.Join(got, "") != "aé😀" {
		t.Fatalf("text = %v", got)
	}
	ch := make(chan keyEvent, 8)
	readKeys(bufio.NewReader(strings.NewReader("\x1b[200~line1\nline2\x1b[201~x")), ch)
	k := <-ch
	if k.action != "paste" || k.text != "line1\nline2" {
		t.Fatalf("paste = %+v", k)
	}
	if k := <-ch; k.text != "x" {
		t.Fatalf("after paste = %+v", k)
	}
	if got := decode("\x1b[200~\x1b[201~"); strings.Join(got, ",") != "pasteimage" {
		t.Fatalf("empty paste = %v", got)
	}
}

// Several keys in one read are split correctly, mouse included.
func TestDecodeSequenceStream(t *testing.T) {
	got := decode("\x10ab\x1b[1;5D\x1b[<0;3;4M\x1b[<0;3;4m\x1b\x1b")
	want := "ctrl+p,a,b,ctrl+left,mousedown,mouseup,esc,esc"
	if strings.Join(got, ",") != want {
		t.Fatalf("stream = %v", got)
	}
}

// The leader waits two seconds for its second key.
func TestLeaderTimeout(t *testing.T) {
	var l leader
	now := time.Now()
	if l.take(now) {
		t.Fatal("an unarmed leader completes nothing")
	}
	l.arm(now)
	if !l.take(now.Add(time.Second)) {
		t.Fatal("a key within 2s completes the chord")
	}
	if l.take(now.Add(time.Second)) {
		t.Fatal("the leader disarms after one key")
	}
	l.arm(now)
	if l.take(now.Add(leaderTimeout)) {
		t.Fatal("a key after 2s is an ordinary key")
	}
}
