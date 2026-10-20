// Overlay tests: the palette, the dialogs' keyboard and mouse model, the
// geometry from the spec (§6), toasts and the prompt autocomplete.
package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// Ctrl+P opens the palette with every registry command, and choosing a
// non-dialog command hands it to the REPL.
func TestPaletteRunsRegistryCommands(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	sc.openPalette()
	if sc.dlg == nil || len(sc.dlg.items) != len(commands()) {
		t.Fatalf("palette = %+v", sc.dlg)
	}
	for _, r := range "new" {
		sc.dialogKey(keyEvent{text: string(r)})
	}
	if got := sc.dialogKey(keyEvent{action: keyEnter}); got != "/new" {
		t.Fatalf("enter = %q", got)
	}
	if sc.dlg != nil {
		t.Fatal("the palette closes after a choice")
	}
	// A dialog command opens its dialog instead of going to the REPL.
	sc.openPalette()
	for _, r := range "help" {
		sc.dialogKey(keyEvent{text: string(r)})
	}
	if got := sc.dialogKey(keyEvent{action: keyEnter}); got != "" || sc.dlg == nil || sc.dlg.title != "Help" {
		t.Fatalf("help = %q dlg %+v", got, sc.dlg)
	}
}

// The panel follows OpenCode's geometry: centered, top at H/4, 60 columns.
func TestDialogGeometry(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 140, 40, panelState{model: "m"})
	sc.openPalette()
	frame := sc.frame()
	g := sc.dlg.geo
	if g.width != 60 || g.left != (140-60)/2+1 || g.top != 40/4+1 {
		t.Fatalf("geo = %+v", g)
	}
	rows := visualize(frame, 140, 40)
	if !strings.Contains(rowAt(rows, g.titleRow), "Commands") || !strings.Contains(rowAt(rows, g.titleRow), "esc") {
		t.Fatalf("title row = %q", rowAt(rows, g.titleRow))
	}
	if !strings.Contains(frame, "\x1b[38;2;24;24;24m") && !strings.Contains(frame, "\x1b[48;2;4;4;4m") {
		t.Error("the frame underneath must be dimmed by the backdrop")
	}
	// Narrow terminals cap the panel at W-2.
	sc2, _ := newTestScreen(t, 50, 24, panelState{model: "m"})
	sc2.openPalette()
	sc2.frame()
	if sc2.dlg.geo.width != 48 {
		t.Fatalf("narrow width = %d", sc2.dlg.geo.width)
	}
}

// Mouse: motion highlights, mouse-up on the pressed row selects, a click on
// the backdrop closes.
func TestDialogMouse(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m", effort: "high"})
	sc.openEffortDialog()
	sc.frame()
	var row, idx int
	for r, i := range sc.dlg.geo.rowItem {
		if sc.dlg.items[i].title == "low" {
			row, idx = r, i
		}
	}
	col := sc.dlg.geo.left + 5
	sc.dialogMouse(keyEvent{mouse: true, action: "mousemove", row: row, col: col})
	if sc.dlg.sel != idx {
		t.Fatalf("hover sel = %d want %d", sc.dlg.sel, idx)
	}
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", row: row, col: col})
	cmd, _ := sc.dialogMouse(keyEvent{mouse: true, action: "mouseup", row: row, col: col})
	if cmd != "/effort low" || sc.dlg != nil {
		t.Fatalf("click = %q dlg %v", cmd, sc.dlg)
	}
	// Backdrop click closes without a command.
	sc.openEffortDialog()
	sc.frame()
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", row: 1, col: 1})
	cmd, _ = sc.dialogMouse(keyEvent{mouse: true, action: "mouseup", row: 1, col: 1})
	if cmd != "" || sc.dlg != nil {
		t.Fatalf("backdrop = %q dlg %v", cmd, sc.dlg)
	}
	// Choosing the current effort changes nothing.
	sc.openEffortDialog()
	if got := sc.dialogKey(keyEvent{action: keyEnter}); got != "" {
		t.Fatalf("current choice = %q", got)
	}
}

// Keyboard: arrows wrap, Esc closes, ctrl+c clears the search first.
func TestDialogKeys(t *testing.T) {
	sc := &screen{out: &strings.Builder{}}
	sc.openPalette()
	n := len(sc.dlg.items)
	sc.dialogKey(keyEvent{action: keyUp})
	if sc.dlg.sel != n-1 {
		t.Fatalf("up from the first row wraps: %d", sc.dlg.sel)
	}
	sc.dialogKey(keyEvent{text: "z"})
	sc.dialogKey(keyEvent{action: "ctrl+c"})
	if sc.dlg == nil || len(sc.dlg.query) != 0 {
		t.Fatal("ctrl+c clears the search first")
	}
	sc.dialogKey(keyEvent{action: keyEsc})
	if sc.dlg != nil {
		t.Fatal("esc closes")
	}
	// The rename dialog submits its text as /rename.
	sc.state.title = "old"
	sc.openRenameDialog()
	sc.dialogKey(keyEvent{action: "ctrl+u"})
	for _, r := range "new name" {
		sc.dialogKey(keyEvent{text: string(r)})
	}
	if got := sc.dialogKey(keyEvent{action: keyEnter}); got != "/rename new name" {
		t.Fatalf("rename = %q", got)
	}
}

// The model dialog loads the real catalog and marks the active model.
func TestModelDialogLoadsCatalog(t *testing.T) {
	before := fetchModels
	defer func() { fetchModels = before }()
	fetchModels = func(context.Context, string, string) ([]dealer.CatalogModel, error) {
		return []dealer.CatalogModel{{ID: "a", ContextLength: 128000}, {ID: "b"}}, nil
	}
	sc := &screen{out: &strings.Builder{}}
	sc.state.model = "b"
	sc.src = &dialogSource{}
	sc.openModelDialog()
	deadline := time.Now().Add(2 * time.Second)
	for {
		sc.mu.Lock()
		loading := sc.dlg.loading
		sc.mu.Unlock()
		if !loading || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(sc.dlg.items) != 2 || !sc.dlg.items[1].current || sc.dlg.sel != 1 {
		t.Fatalf("items = %+v sel %d", sc.dlg.items, sc.dlg.sel)
	}
	sc.dlg.sel = 0
	if got := sc.dialogKey(keyEvent{action: keyEnter}); got != "/model a" {
		t.Fatalf("choice = %q", got)
	}
}

// A bare dialog command typed in the prompt opens the dialog; with an argument
// it goes to the REPL.
func TestDialogCommand(t *testing.T) {
	if c, ok := dialogCommand("/model"); !ok || c != "/model" {
		t.Fatal("/model alone opens the dialog")
	}
	if _, ok := dialogCommand("/model gpt"); ok {
		t.Fatal("/model with an id goes to the REPL")
	}
	if _, ok := dialogCommand("/new"); ok {
		t.Fatal("/new has no dialog")
	}
}

// Every registry command has a REPL handler: the palette never shows a dead row.
func TestRegistryCommandsAreHandled(t *testing.T) {
	src := readFile(t, "repl.go")
	for _, c := range commands() {
		if !strings.Contains(src, `"`+c.name+`"`) {
			t.Errorf("%s has no case in the REPL switch", c.name)
		}
	}
}

// Toasts land top-right and expire after 5 s.
func TestToast(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	sc.toastf("success", "Copied to clipboard")
	rows := visualize(sc.frame(), 100, 30)
	if !strings.Contains(rowAt(rows, 3), "Copied to clipboard") {
		t.Fatalf("toast row = %q", rowAt(rows, 3))
	}
	g := sc.toast.geo
	if g[1]+g[2]-1 != 100-2 {
		t.Fatalf("toast right edge at %d", g[1]+g[2]-1)
	}
	if !sc.toastHit(3, g[1]+2) || sc.toastHit(3, 1) {
		t.Fatal("hit-test")
	}
}

// "/" at the prompt start lists commands; "@" at a word start lists files;
// Tab inserts the choice.
func TestAutocomplete(t *testing.T) {
	before := listFiles
	defer func() { listFiles = before }()
	listFiles = func(string) []string { return []string{"internal/cli/screen.go", "README.md"} }
	sc := &screen{out: &strings.Builder{}}
	sc.src = &dialogSource{cwd: "."}
	sc.editor().Insert("/mod")
	sc.refreshAC()
	if !sc.acOpen() || sc.ac.items[0].text != "/model" {
		t.Fatalf("slash = %+v", sc.ac)
	}
	sc.acKey("tab")
	if sc.editor().Text() != "/model " || sc.acOpen() {
		t.Fatalf("after tab = %q", sc.editor().Text())
	}
	sc.ed = newEditor()
	sc.editor().Insert("look at @scr")
	sc.refreshAC()
	if !sc.acOpen() || sc.ac.items[0].text != "@internal/cli/screen.go" {
		t.Fatalf("at = %+v", sc.ac)
	}
	sc.acKey(keyEsc)
	if sc.acOpen() {
		t.Fatal("esc hides")
	}
	// Hidden stays hidden while the user keeps typing the same token.
	sc.editor().Insert("e")
	sc.refreshAC()
	if sc.acOpen() {
		t.Fatal("esc hides the popup for the rest of the token")
	}
	sc.ac.hidden = false
	sc.acKey(keyEnter)
	if sc.editor().Text() != "look at @internal/cli/screen.go " {
		t.Fatalf("after enter = %q", sc.editor().Text())
	}
	// "/" after text is not a command.
	sc.ed = newEditor()
	sc.editor().Insert("a /b")
	sc.refreshAC()
	if sc.ac != nil {
		t.Fatal("a slash mid-prompt is plain text")
	}
}

func TestFuzzy(t *testing.T) {
	if fuzzyScore("mdl", "model") == 0 || fuzzyScore("xyz", "model") != 0 {
		t.Fatal("subsequence")
	}
	if fuzzyScore("mo", "model") <= fuzzyScore("mo", "tasks monitor") {
		t.Fatal("prefix wins")
	}
}

func TestSessionDay(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.Local)
	if got := sessionDay(now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00"), now); got != "Today" {
		t.Fatalf("today = %q", got)
	}
	if got := sessionDay(time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local).UTC().Format("2006-01-02T15:04:05.000Z07:00"), now); got != "Tue Oct 13 2026" {
		t.Fatalf("older = %q", got)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A click on a sidebar MCP row opens the MCP dialog with the real status.
func TestSidebarMCPRowOpensDialog(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 140, 40, panelState{model: "m",
		mcps: []mcpEntry{{name: "alpha"}, {name: "beta", falha: true}}})
	sc.Adicionar("x")
	var row *hitRegion
	for _, r := range sc.hits.regions {
		if r.id == "mcprow" {
			row = r
			break
		}
	}
	if row == nil {
		t.Fatal("MCP rows must be hit regions")
	}
	sc.mouseDown(row.row, row.col+3)
	sc.mouseUp(row.row, row.col+3)
	if sc.dlg == nil || sc.dlg.title != "MCP servers" || sc.dlg.items[1].footer != "Failed !" {
		t.Fatalf("dialog = %+v", sc.dlg)
	}
}
