// The update dialog: /update (or "update" in the command palette) opens a
// popup over the current screen, as OpenCode's component/dialog-update.tsx
// does, instead of printing the check into the transcript. It checks the
// channel, offers Skip/Update, installs, and asks for a restart.
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/update"
)

// Update dialog states, OpenCode's UpdateState types.
const (
	updChecking   = "checking"
	updCurrent    = "current"
	updAvailable  = "available"
	updInstalling = "installing"
	updInstalled  = "installed"
	updFailed     = "failed"
)

// updateState is the dialog's body: what the check found, what is happening.
type updateState struct {
	kind    string
	message string // the one-line text of current/failed
	plan    update.Plano
	active  int // the focused button
	spin    int // spinner frame
	cancel  context.CancelFunc
	// button cells of the last frame, for clicks: [row, col, width] each.
	btnGeo [][3]int
}

// Variables so the tests can drive the dialog without a network.
var (
	updateCheck   = update.Verificar
	updateInstall = update.Instalar
)

// updateButtons are the dialog's buttons in order; the last one is the
// confirm action (Enter), as in OpenCode.
func (u *updateState) buttons() []string {
	switch u.kind {
	case updInstalling:
		return nil
	case updAvailable:
		return []string{"Skip", "Update"}
	case updInstalled:
		return []string{"Skip", "Restart"}
	}
	return []string{"Skip"}
}

// openUpdateDialog opens the popup and starts the check off the key loop.
func (t *screen) openUpdateDialog() {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutUpdate)
	u := &updateState{kind: updChecking, cancel: cancel}
	d := &dialog{title: "Update", size: dialogSize, upd: u}
	t.openDialog(d)
	t.spinUpdate(d)
	go func() {
		plan, err := updateCheck(ctx, strings.TrimSpace(Versao))
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.dlg != d {
			return
		}
		switch {
		case err != nil:
			u.kind, u.message = updFailed, oneLineErr(err)
		case plan.Current:
			u.kind = updCurrent
		default:
			u.kind, u.plan = updAvailable, plan
		}
		u.active = max(0, len(u.buttons())-1)
		t.drawLocked()
	}()
}

// spinUpdate animates the spinner while checking or installing.
func (t *screen) spinUpdate(d *dialog) {
	go func() {
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for range tick.C {
			t.mu.Lock()
			if t.dlg != d {
				t.mu.Unlock()
				return
			}
			if d.upd.kind == updChecking || d.upd.kind == updInstalling {
				d.upd.spin++
				t.drawLocked()
			}
			t.mu.Unlock()
		}
	}()
}

// updatePress runs a button: Skip closes, Update installs, Restart exits so
// the new binary starts next time (bh cannot replace itself while running).
// Returns a slash command for the REPL ("/exit" for Restart).
func (t *screen) updatePress(label string) string {
	d := t.dlg
	u := d.upd
	switch label {
	case "Skip":
		if u.cancel != nil {
			u.cancel()
		}
		t.closeDialog()
	case "Update":
		u.kind, u.btnGeo = updInstalling, nil
		plan := u.plan
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeoutUpdate)
			defer cancel()
			err := updateInstall(ctx, plan)
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.dlg != d {
				return
			}
			if err != nil {
				u.kind, u.message = updFailed, oneLineErr(err)
			} else {
				u.kind = updInstalled
			}
			u.active = max(0, len(u.buttons())-1)
			t.drawLocked()
		}()
	case "Restart":
		t.closeDialog()
		return "/exit"
	}
	return ""
}

// updateKey handles the dialog's keys: Enter confirms the focused button,
// left/right/tab move between them, Esc closes.
func (t *screen) updateKey(name string) string {
	u := t.dlg.upd
	btns := u.buttons()
	switch name {
	case keyEnter:
		if len(btns) == 0 {
			return ""
		}
		return t.updatePress(btns[clampInt(u.active, 0, len(btns)-1)])
	case "left", "shift+tab":
		if n := len(btns); n > 0 {
			u.active = (u.active + n - 1) % n
		}
	case "right", "tab":
		if n := len(btns); n > 0 {
			u.active = (u.active + 1) % n
		}
	}
	return ""
}

// updateMouse handles a click on a button (mouse-up).
func (t *screen) updateMouse(k keyEvent) (string, bool) {
	u := t.dlg.upd
	for i, g := range u.btnGeo {
		if k.row == g[0] && k.col >= g[1] && k.col < g[1]+g[2] {
			switch k.action {
			case "mousemove":
				if u.active != i {
					u.active = i
					return "", true
				}
				return "", false
			case "mouseup":
				return t.updatePress(u.buttons()[i]), true
			}
		}
	}
	return "", false
}

// drawUpdateBody paints the dialog's message and buttons from row on; returns
// the next free row.
func (t *screen) drawUpdateBody(put func(string), row *int, left, width int, sb *strings.Builder) {
	u := t.dlg.upd
	inner := width - 6
	spin := bluef(spinFrames[u.spin%len(spinFrames)])
	var body []string
	switch u.kind {
	case updChecking:
		body = []string{spin + " " + color(colorText, "Checking for updates…")}
	case updAvailable:
		body = strings.Split(wrap("An update is available ("+u.plan.Release.Version+
			"). After installing, you'll be prompted to restart BarrosoHarness.", inner), "\n")
		for i := range body {
			body[i] = grayf(body[i])
		}
	case updInstalling:
		body = []string{spin + " " + color(colorText, "Installing BarrosoHarness "+u.plan.Release.Version+"…")}
	case updInstalled:
		body = strings.Split(wrap("Update successful! A restart is required: Restart exits bh; "+
			"open it again to use the new version.", inner), "\n")
		for i := range body {
			body[i] = grayf(body[i])
		}
	case updCurrent:
		body = []string{grayf("BarrosoHarness is already up to date.")}
	case updFailed:
		for _, l := range strings.Split(wrap(u.message, inner), "\n") {
			body = append(body, color(fgError, l))
		}
	}
	for _, l := range body {
		put("   " + l)
	}
	btns := u.buttons()
	u.btnGeo = nil
	if len(btns) == 0 {
		return
	}
	put("")
	// Buttons right-aligned: " Skip " " Update ", the focused one inverted in
	// the accent colour (OpenCode's legacy footer).
	total := 0
	for _, b := range btns {
		total += visWidth(b) + 2
	}
	col := left + width - 3 - total
	fmt.Fprintf(sb, "\033[%d;%dH%s", *row, left, withBg("", bgScreen, width))
	for i, b := range btns {
		label := " " + b + " "
		if i == u.active {
			fmt.Fprintf(sb, "\033[%d;%dH%s%s%s%s", *row, col, bgAccent, fgSelected, label, reset)
		} else {
			fmt.Fprintf(sb, "\033[%d;%dH%s%s", *row, col, bgScreen, grayf(label))
		}
		u.btnGeo = append(u.btnGeo, [3]int{*row, col, visWidth(label)})
		col += visWidth(label)
	}
	*row++
}
