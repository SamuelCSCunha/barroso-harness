// The prompt's autocomplete popup (OpenCode's component/prompt/autocomplete.tsx):
// "/" at the very start of the prompt lists commands and skills, "@" at the
// start of a word lists files under the working directory. The popup sits right
// above the prompt, at most 10 rows; up/down (ctrl+p/ctrl+n) move, Enter or Tab
// insert, Esc hides, and a mouse-up on a row picks it.
package cli

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// acState is the open popup.
type acState struct {
	trigger byte // '/' or '@'
	start   int  // rune column of the trigger on the caret's line
	query   string
	items   []completionEntry
	sel     int
	hidden  bool // Esc hid it; it stays hidden until the token changes
	geo     map[int]int
	press   int
}

const acMaxRows = 10

// skipComplete are directories the "@" completion never descends into.
var skipComplete = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true}

// walkLimit caps how many files "@" lists, so a home directory cannot stall the
// key loop.
const walkLimit = 5000

// listFiles returns the paths under root, slash-separated and relative, up to
// walkLimit, the way the glob tool walks them. A variable so tests can pin it.
var listFiles = func(root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil || len(out) >= walkLimit {
			if p == root || err == nil {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			if p != root && (skipComplete[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, rerr := filepath.Rel(root, p); rerr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// acToken finds the token the caret is completing: "/xyz" when it is the whole
// prompt so far (no whitespace), or "@xyz" starting a word.
func acToken(e *editor) (trigger byte, start int, query string, ok bool) {
	line := e.lines[e.row][:e.col]
	if e.row == 0 && len(line) > 0 && line[0] == '/' && !strings.ContainsAny(string(line), " \t") {
		return '/', 0, string(line), true
	}
	i := len(line)
	for i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
		i--
	}
	if i < len(line) && line[i] == '@' {
		return '@', i, string(line[i+1:]), true
	}
	return 0, 0, "", false
}

// refreshAC recomputes the popup after an edit. cwd is where "@" looks.
func (t *screen) refreshAC() {
	if t.partial != "" || t.masked || t.dlg != nil {
		t.ac = nil
		return
	}
	trig, start, q, ok := acToken(t.editor())
	if !ok {
		t.ac = nil
		return
	}
	if t.ac != nil && t.ac.trigger == trig && t.ac.start == start && t.ac.query == q {
		return
	}
	hidden := t.ac != nil && t.ac.hidden && t.ac.trigger == trig && t.ac.start == start
	cwd := ""
	if t.src != nil {
		cwd = t.src.cwd
	}
	var items []completionEntry
	if trig == '/' {
		items = slashCompletions(cwd, q)
	} else {
		files := t.fileCache(cwd)
		for _, j := range fuzzyRank(len(files), q, func(i int) string { return files[i] }) {
			items = append(items, completionEntry{text: "@" + files[j]})
			if len(items) == 20 {
				break
			}
		}
	}
	t.ac = &acState{trigger: trig, start: start, query: q, items: items, hidden: hidden, press: -1}
}

// fileCache lists the files once per prompt (a walk per keystroke would make
// typing lag in a large tree).
func (t *screen) fileCache(cwd string) []string {
	if t.files == nil || t.filesRoot != cwd {
		if cwd == "" {
			return nil
		}
		t.files, t.filesRoot = listFiles(cwd), cwd
	}
	return t.files
}

// acOpen reports whether the popup is showing.
func (t *screen) acOpen() bool { return t.ac != nil && !t.ac.hidden }

// acPick inserts the selected entry: "/name " replaces the prompt start,
// "@path " replaces the "@…" word.
func (t *screen) acPick() {
	a := t.ac
	if a == nil || len(a.items) == 0 {
		return
	}
	it := a.items[clampInt(a.sel, 0, len(a.items)-1)]
	e := t.editor()
	e.pushUndo()
	line := e.lines[e.row]
	ins := []rune(it.text + " ")
	rest := line[e.col:]
	e.lines[e.row] = append(append(append([]rune{}, line[:a.start]...), ins...), rest...)
	e.col = a.start + len(ins)
	e.wantCol, e.hasWantCol = e.col, true
	t.ac = nil
}

// acKey handles a key while the popup is showing; reports whether it used it.
func (t *screen) acKey(name string) bool {
	if !t.acOpen() {
		return false
	}
	a := t.ac
	n := len(a.items)
	switch name {
	case keyUp, "ctrl+p":
		if n > 0 {
			a.sel = (a.sel - 1 + n) % n
		}
	case keyDown, "ctrl+n":
		if n > 0 {
			a.sel = (a.sel + 1) % n
		}
	case keyEnter, "tab":
		if n == 0 {
			return false
		}
		// Enter on a command that takes no argument runs it: the prompt becomes
		// the bare command and the key falls through to the normal send.
		if it := a.items[clampInt(a.sel, 0, n-1)]; name == keyEnter && a.trigger == '/' && noArgCommand(it.text) {
			e := t.editor()
			e.pushUndo()
			e.lines[e.row] = []rune(it.text)
			e.col = len(e.lines[e.row])
			e.wantCol, e.hasWantCol = e.col, true
			t.ac = nil
			return false
		}
		t.acPick()
	case keyEsc:
		a.hidden = true
	default:
		return false
	}
	return true
}

// acMouse handles a mouse event over the popup: motion highlights, mouse-up on
// the pressed row picks it. Reports whether the event was the popup's.
func (t *screen) acMouse(k keyEvent) bool {
	if !t.acOpen() {
		return false
	}
	a := t.ac
	i, on := a.geo[k.row]
	switch k.action {
	case "mousemove":
		if on {
			a.sel = i
		}
		return on
	case "mousedown":
		if on {
			a.press, a.sel = i, i
		}
		return on
	case "mouseup":
		p := a.press
		a.press = -1
		if on && i == p {
			a.sel = i
			t.acPick()
			return true
		}
		return on
	}
	return false
}

// drawAC paints the popup directly above the prompt box, over the transcript.
func (t *screen) drawAC(sb *strings.Builder, f frame) {
	if !t.acOpen() {
		return
	}
	a := t.ac
	a.geo = map[int]int{}
	width := f.boxWidth - f.barCol + 1
	rows := min(acMaxRows, max(1, len(a.items)))
	rows = min(rows, f.boxTop-f.convRow)
	if rows <= 0 {
		return
	}
	a.sel = clampInt(a.sel, 0, max(0, len(a.items)-1))
	off := clampInt(a.sel-rows+1, 0, max(0, len(a.items)-rows))
	pad := 0
	for _, it := range a.items {
		pad = max(pad, visWidth(it.text))
	}
	top := f.boxTop - rows
	edge := strings.Repeat(" ", f.barCol-1) + color(fgBorder, bar)
	for i := 0; i < rows; i++ {
		row := top + i
		var body string
		switch {
		case len(a.items) == 0 && a.trigger == '/':
			body = grayf(" No matching commands")
		case len(a.items) == 0:
			body = grayf(" No matching files")
		default:
			idx := off + i
			it := a.items[idx]
			a.geo[row] = idx
			name := it.text + strings.Repeat(" ", pad-visWidth(it.text)+2)
			if idx == a.sel {
				body = withBg(fgSelected+" "+name+it.desc, bgAccent, width-1)
			} else {
				body = " " + color(colorText, name) + grayf(it.desc)
			}
		}
		fmt.Fprintf(sb, "\033[%d;1H%s%s", row, bgScreen+edge, withBg(body, "\033[48;2;30;30;30m", width-1))
	}
}
