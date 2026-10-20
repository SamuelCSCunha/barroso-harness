// Key decoding for the full screen: raw stdin bytes become named key events
// ("ctrl+p", "alt+b", "ctrl+left", "shift+enter"…), so the keymap can bind
// OpenCode's shortcuts. The old reader only knew a handful of control bytes and
// discarded everything else — Ctrl+P, the arrows sideways and every modifier
// combination simply vanished.
//
// Coverage: C0 control bytes, ESC-prefixed Alt chords, CSI sequences with xterm
// modifier parameters ("1;5D" = ctrl+left), the "~" family (Home/End/Delete/
// PgUp/PgDn, with modifiers), CSI-u (kitty / modifyOtherKeys: "13;2u" =
// shift+enter), SS3 arrows, SGR mouse and bracketed paste.
package cli

import (
	"bufio"
	"strconv"
	"strings"
	"time"
)

// leaderTimeout is how long OpenCode waits for the key after its leader
// (ctrl+x) before giving up (context/keymap.tsx:143).
const leaderTimeout = 2 * time.Second

// Action names kept from the original reader: the key loop and the tests
// already speak them.
const (
	keyEnter     = "enter"
	keyNewline   = "newline" // shift/ctrl/alt+enter and ctrl+j: a line break in the prompt
	keyBackspace = "backspace"
	keyEsc       = "esc"
	keyUp        = "cima"
	keyDown      = "baixo"
)

// ctrlNames maps C0 control bytes to their key names. 0x08 is ctrl+backspace:
// Windows Terminal and conhost with VT input send 0x7f for Backspace and 0x08
// for Ctrl+Backspace. Tab (0x09) and Enter (0x0d) are handled before this table.
var ctrlNames = map[byte]string{
	0x01: "ctrl+a", 0x02: "ctrl+b", 0x03: "ctrl+c", 0x04: "ctrl+d", 0x05: "ctrl+e",
	0x06: "ctrl+f", 0x07: "ctrl+g", 0x08: "ctrl+backspace", 0x0b: "ctrl+k",
	0x0c: "ctrl+l", 0x0e: "ctrl+n", 0x0f: "ctrl+o", 0x10: "ctrl+p", 0x11: "ctrl+q",
	0x12: "ctrl+r", 0x13: "ctrl+s", 0x14: "ctrl+t", 0x15: "ctrl+u", 0x17: "ctrl+w",
	0x18: "ctrl+x", 0x19: "ctrl+y", 0x1a: "ctrl+z", 0x1f: "ctrl+-",
}

// modPrefix turns an xterm modifier parameter (1 + shift·1 + alt·2 + ctrl·4)
// into the "ctrl+alt+shift+" prefix of a key name.
func modPrefix(m int) string {
	if m <= 1 {
		return ""
	}
	m--
	var p string
	if m&4 != 0 {
		p += "ctrl+"
	}
	if m&2 != 0 {
		p += "alt+"
	}
	if m&1 != 0 {
		p += "shift+"
	}
	return p
}

// arrowName names a cursor key: the original reader called up/down "cima"/
// "baixo", and those stay for the unmodified keys.
func arrowName(final byte, mods int) string {
	base := map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left", 'H': "home", 'F': "end"}[final]
	if base == "" {
		return ""
	}
	if mods <= 1 {
		switch base {
		case "up":
			return keyUp
		case "down":
			return keyDown
		}
		return base
	}
	return modPrefix(mods) + base
}

// tildeName names the "CSI n ~" family.
func tildeName(n, mods int) string {
	base := map[int]string{1: "home", 7: "home", 4: "end", 8: "end", 3: "delete", 5: "pgup", 6: "pgdn"}[n]
	if base == "" {
		return ""
	}
	return modPrefix(mods) + base
}

// csiUName names a CSI-u key (kitty keyboard / xterm modifyOtherKeys):
// "codepoint;mods u". Only the chords the keymap cares about get names; a plain
// printable with no modifier comes back as text.
func csiUName(code, mods int) (action, text string) {
	p := modPrefix(mods)
	switch code {
	case 13:
		if p == "" {
			return keyEnter, ""
		}
		return keyNewline, ""
	case 27:
		return p + "esc", ""
	case 127:
		return p + "backspace", ""
	case 9:
		return p + "tab", ""
	}
	if p == "" || p == "shift+" {
		return "", string(rune(code))
	}
	if code >= 'A' && code <= 'Z' {
		code += 'a' - 'A'
	}
	return p + string(rune(code)), ""
}

// splitParams parses "1;5" into ints; missing or bad fields are 0/1 as xterm
// defines them (the first parameter defaults to 1, modifiers to 1).
func splitParams(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]int, len(parts))
	for i, p := range parts {
		if j := strings.IndexByte(p, ':'); j >= 0 {
			p = p[:j] // kitty sub-parameters (alternate keys, event type)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			n = 1
		}
		out[i] = n
	}
	return out
}

// readKeys decodes raw stdin into key events. Accented characters arrive as
// multi-byte UTF-8 and are reassembled.
func readKeys(r *bufio.Reader, ch chan<- keyEvent) {
	defer close(ch)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		switch {
		case b == '\r':
			ch <- keyEvent{action: keyEnter}
		case b == '\n':
			// ctrl+j: OpenCode's newline chord (raw-mode Enter sends \r).
			ch <- keyEvent{action: keyNewline}
		case b == 127:
			ch <- keyEvent{action: keyBackspace}
		case b == 0x16:
			// raw Ctrl+V (terminals without bracketed paste, or a keybinding
			// that sends it): bh reads the clipboard itself.
			ch <- keyEvent{action: "pasteimage"}
		case b == 0x1b:
			if !decodeEscape(r, ch) {
				return
			}
		case b == '\t':
			ch <- keyEvent{action: "tab"}
		case b < 0x20:
			if name, ok := ctrlNames[b]; ok {
				ch <- keyEvent{action: name}
			}
		case b >= 0x80:
			ch <- keyEvent{text: readUTF8(r, b)}
		default:
			ch <- keyEvent{text: string(rune(b))}
		}
	}
}

// readUTF8 reassembles a multi-byte rune whose first byte is b.
func readUTF8(r *bufio.Reader, b byte) string {
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
			break
		}
		buf = append(buf, c)
	}
	return string(buf)
}

// decodeEscape handles everything after an ESC byte. A lone ESC (nothing else
// buffered) is the Esc key; ESC + a byte is an Alt chord; ESC [ and ESC O open
// CSI and SS3 sequences. Returns false when the input ended.
func decodeEscape(r *bufio.Reader, ch chan<- keyEvent) bool {
	if r.Buffered() == 0 {
		ch <- keyEvent{action: keyEsc}
		return true
	}
	c, err := r.ReadByte()
	if err != nil {
		return false
	}
	switch {
	case c == '[':
		return decodeCSI(r, ch)
	case c == 'O':
		f, err := r.ReadByte()
		if err != nil {
			return false
		}
		if name := arrowName(f, 1); name != "" {
			ch <- keyEvent{action: name}
		}
		return true
	case c == '\r':
		ch <- keyEvent{action: keyNewline} // alt+enter
	case c == 127:
		ch <- keyEvent{action: "alt+backspace"}
	case c == 0x1b:
		ch <- keyEvent{action: keyEsc} // double Esc arrives in one read
		return decodeEscape(r, ch)
	case c < 0x20:
		if name, ok := ctrlNames[c]; ok {
			ch <- keyEvent{action: "ctrl+alt+" + strings.TrimPrefix(name, "ctrl+")}
		}
	case c >= 0x80:
		ch <- keyEvent{action: "alt+" + readUTF8(r, c)}
	default:
		ch <- keyEvent{action: "alt+" + strings.ToLower(string(rune(c)))}
	}
	return true
}

// decodeCSI reads a CSI sequence's parameters and final byte, and emits the key
// (or mouse event, or paste) it stands for.
func decodeCSI(r *bufio.Reader, ch chan<- keyEvent) bool {
	var params []byte
	var final byte
	for {
		x, err := r.ReadByte()
		if err != nil {
			return false
		}
		if x >= 0x40 && x <= 0x7e {
			final = x
			break
		}
		params = append(params, x)
	}
	ps := string(params)
	// SGR mouse: "<code;col;row" + M/m.
	if strings.HasPrefix(ps, "<") && (final == 'M' || final == 'm') {
		if e, ok := parseMouseSeq(ps[1:] + string(final)); ok {
			ch <- mouseEvent(e)
		}
		return true
	}
	nums := splitParams(ps)
	mods := 1
	if len(nums) >= 2 {
		mods = nums[1]
	}
	switch final {
	case 'A', 'B', 'C', 'D', 'H', 'F':
		if name := arrowName(final, mods); name != "" {
			ch <- keyEvent{action: name}
		}
	case 'Z':
		ch <- keyEvent{action: "shift+tab"}
	case '~':
		n := 0
		if len(nums) > 0 {
			n = nums[0]
		}
		if n == 200 {
			content, ok := readPaste(r)
			if !ok {
				return false
			}
			if content == "" {
				// Windows Terminal < 1.25: an image-only clipboard arrives as an
				// empty paste.
				ch <- keyEvent{action: "pasteimage"}
			} else {
				ch <- keyEvent{action: "paste", text: content}
			}
			return true
		}
		if name := tildeName(n, mods); name != "" {
			ch <- keyEvent{action: name}
		}
	case 'u':
		if len(nums) == 0 {
			return true
		}
		action, text := csiUName(nums[0], mods)
		ch <- keyEvent{action: action, text: text}
	}
	// Focus in/out (I/O) and anything unknown are dropped.
	return true
}

// leader tracks OpenCode's ctrl+x leader: after it, the next key within
// leaderTimeout is looked up in the leader table instead of the normal one.
type leader struct {
	armed bool
	at    time.Time
}

// arm starts waiting for the leader's second key.
func (l *leader) arm(now time.Time) { l.armed, l.at = true, now }

// take reports whether a key pressed at now completes a leader chord, and
// disarms the leader either way (a late key is an ordinary key).
func (l *leader) take(now time.Time) bool {
	ok := l.armed && now.Sub(l.at) < leaderTimeout
	l.armed = false
	return ok
}

// keyName is the canonical name of an event for the binding tables: the action
// when there is one, otherwise the typed character.
func keyName(k keyEvent) string {
	if k.action != "" {
		return k.action
	}
	return k.text
}
