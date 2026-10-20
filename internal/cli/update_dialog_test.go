package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/update"
)

// waitDialog waits until the update dialog leaves the given state.
func waitUpd(t *testing.T, sc *screen, not string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		sc.mu.Lock()
		k := ""
		if sc.dlg != nil && sc.dlg.upd != nil {
			k = sc.dlg.upd.kind
		}
		sc.mu.Unlock()
		if k != not {
			return k
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("update dialog stuck in %q", not)
	return ""
}

// /update opens a popup over the screen (the home screen stays home), checks,
// and offers Skip/Update; Update installs and offers Restart, which exits.
func TestUpdateDialogFlow(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	prevC, prevI := updateCheck, updateInstall
	defer func() { updateCheck, updateInstall = prevC, prevI }()
	updateCheck = func(context.Context, string) (update.Plano, error) {
		return update.Plano{Release: update.Lancamento{Version: "v9"}}, nil
	}
	installed := false
	updateInstall = func(context.Context, update.Plano) error { installed = true; return nil }

	sc, sb := newTestScreen(t, 160, 40, panelState{model: "m"})
	sc.mu.Lock()
	cmd := sc.runCommand("/update")
	sc.drawLocked()
	sc.mu.Unlock()
	if cmd != "" || sc.dlg == nil || !sc.isHome() {
		t.Fatalf("/update must open a popup over home, cmd=%q", cmd)
	}
	if k := waitUpd(t, sc, updChecking); k != updAvailable {
		t.Fatalf("state = %q", k)
	}
	sc.mu.Lock()
	sc.drawLocked()
	all := strings.Join(visualize(sb.String(), 160, 40), "\n")
	if !strings.Contains(all, "An update is available") || !strings.Contains(all, " Update ") {
		sc.mu.Unlock()
		t.Fatalf("frame:\n%s", all)
	}
	sc.updateKey(keyEnter) // Update is the focused (last) button
	sc.mu.Unlock()
	if k := waitUpd(t, sc, updInstalling); k != updInstalled || !installed {
		t.Fatalf("after install state = %q installed=%v", k, installed)
	}
	sc.mu.Lock()
	cmd = sc.updateKey(keyEnter)
	sc.mu.Unlock()
	if cmd != "/exit" || sc.dlg != nil {
		t.Fatalf("Restart must exit, cmd=%q", cmd)
	}
}

// Up to date and failures are shown in the popup with only Skip.
func TestUpdateDialogCurrentAndFailed(t *testing.T) {
	prevC := updateCheck
	defer func() { updateCheck = prevC }()
	for _, tc := range []struct {
		err  error
		want string
	}{{nil, updCurrent}, {errors.New("channel down"), updFailed}} {
		updateCheck = func(context.Context, string) (update.Plano, error) {
			return update.Plano{Current: tc.err == nil}, tc.err
		}
		sc, _ := newTestScreen(t, 120, 30, panelState{model: "m"})
		sc.mu.Lock()
		sc.runCommand("/update")
		sc.mu.Unlock()
		if k := waitUpd(t, sc, updChecking); k != tc.want {
			t.Fatalf("state = %q, want %q", k, tc.want)
		}
		sc.mu.Lock()
		if b := sc.dlg.upd.buttons(); len(b) != 1 || b[0] != "Skip" {
			t.Fatalf("buttons = %v", b)
		}
		sc.updateKey(keyEnter)
		closed := sc.dlg == nil
		sc.mu.Unlock()
		if !closed {
			t.Fatal("Skip must close the popup")
		}
	}
}
