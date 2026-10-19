// REPL persistence: opens the session log under config.Dir() and keeps ONE
// active id per process. Best-effort — if the database does not open, the
// REPL keeps going in memory with a discreet warning. A persistent session is
// a convenience (resume, search, recover), never a prerequisite for a turn.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// dbFileName is the log file, inside the harness data directory — the same
// home as config.json, never loose in the user's cwd.
const dbFileName = "sessions.db"

// sessionRow is a session with its event count already resolved: /resume
// needs to show both together, and querying the database row by row would
// cost another round trip per session.
type sessionRow struct {
	store.Session
	Eventos int
	Previa  string // the user's first message, for when there is no title
}

// openStore opens the log. Returns (nil, "") when it cannot — permissions,
// full disk, unavailable driver — and the REPL keeps going in memory.
func openStore(io IO) (*store.Store, string) {
	dbPath := filepath.Join(config.Dir(), dbFileName)
	db, err := store.Open(dbPath)
	if err != nil {
		warnFmt(io.ErrOut, "session not persisted: "+firstErrLine(err))
		return nil, ""
	}
	return db, newSessionID()
}

// newSessionID: bh-<YYYYMMDDHHMMSS>-<4hex>. The timestamp makes the order
// readable by eye; the 4 random hex digits keep ids unique when two REPLs
// start in the same second.
func newSessionID() string {
	return fmt.Sprintf("bh-%s-%s", time.Now().Format("20060102150405"), hex4())
}

func hex4() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Without entropy, the clock becomes the fallback source of randomness.
		return fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)
	}
	return hex.EncodeToString(b[:])
}

// shortSession is the statusbar suffix: the last 4 hex digits of the id.
func shortSession(id string) string {
	if i := strings.LastIndexByte(id, '-'); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// formatAge summarizes how long ago the session was used ("now", "12min",
// "3h", "5d"). One line and no seconds: nobody picks a session by the second.
func formatAge(ts string) string {
	t, err := time.Parse(store.TsFormato, ts)
	if err != nil {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// loadSessionRows summarizes the most recent sessions with their event
// count — what /resume lists and what the user's choice consumes.
//
// Sessions with zero events are left out: they have no conversation to
// resume. Without this filter a freshly opened REPL (which already indexes
// its own empty id) shows at the top of its own list, an option that yields nothing.
func loadSessionRows(db *store.Store, limit int) []sessionRow {
	ls, err := db.ListSessions(limit)
	if err != nil {
		return nil
	}
	out := make([]sessionRow, 0, len(ls))
	for _, se := range ls {
		n, _ := db.ContarEventos(se.ID)
		if n == 0 {
			continue
		}
		l := sessionRow{Session: se, Eventos: n}
		if strings.TrimSpace(se.Title) == "" {
			l.Previa = db.PrimeiraFala(se.ID)
		}
		out = append(out, l)
	}
	return out
}

// printSessionList prints the numbered /resume list.
func printSessionList(out io.Writer, ls []sessionRow) {
	if len(ls) == 0 {
		fmt.Fprintln(out, dimf("no previous sessions found"))
		return
	}
	fmt.Fprintln(out, dimf("previous sessions — number to resume:"))
	for i, l := range ls {
		title := strings.TrimSpace(l.Title)
		if title == "" {
			preview := strings.Join(strings.Fields(l.Previa), " ")
			if r := []rune(preview); len(r) > 60 {
				preview = string(r[:60]) + "…"
			}
			if preview == "" {
				preview = "(untitled)"
			}
			title = dimf(preview)
		} else {
			title = boldf(title)
		}
		fmt.Fprintf(out, "%s %s %s %s %s\n",
			dimf(fmt.Sprintf("%3d", i+1)),
			cyanf(l.ID),
			title,
			dimf(formatAge(l.UpdatedAt)),
			dimf(fmt.Sprintf("%d events", l.Eventos)),
		)
	}
}

// resolveSession turns the user's answer into an id: list number (1-based)
// or id prefix. "" when nothing matches.
func resolveSession(input string, ls []sessionRow) string {
	v := strings.TrimSpace(input)
	if v == "" {
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n >= 1 && n <= len(ls) {
			return ls[n-1].ID
		}
		return ""
	}
	for _, l := range ls {
		if l.ID == v || strings.HasPrefix(l.ID, v) {
			return l.ID
		}
	}
	return ""
}

// firstErrLine cuts the message at the first line break: the persistence
// warning is ONE line in the REPL.
func firstErrLine(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const ceiling = 120
	if len(msg) > ceiling {
		msg = msg[:ceiling] + "…"
	}
	return msg
}

// sessionStatusbar is the status strip with the session suffix. The base
// line (provider · model · effort · window) is the SAME as statusbar in ui.go;
// it is repeated here because that strip cannot change there, and the
// `· sess <4hex>` suffix only shows when a log is active. If the base strip
// ever changes in ui.go, this mirror must follow.
func sessionStatusbar(out io.Writer, cfg *config.Config, id string) {
	provider := cfg.CurrentProvider
	if provider == "" {
		provider = "no-provider"
	}
	model := cfg.Model
	if model == "" {
		model = "select a model"
	}
	line := provider + " · " + model
	if cfg.Effort != "" {
		line += " · effort " + cfg.Effort
	} else {
		line += " · effort off"
	}
	if cfg.Janela > 0 {
		line += fmt.Sprintf(" · %dk", cfg.Janela/1000)
	}
	if id != "" {
		line += " · sess " + shortSession(id)
	}
	fmt.Fprintln(out, dimf("╭─ "+line))
}

// defaultTitle gives the session a provisional name when `/save` has no
// argument: the first 60 characters of the user's last message. An untitled
// session is indistinguishable from another in the /resume list.
func defaultTitle(sess *runtime.Session) string {
	h := sess.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role != "user" {
			continue
		}
		t := strings.TrimSpace(h[i].Conteudo)
		if t == "" {
			continue
		}
		t = strings.Join(strings.Fields(t), " ")
		const ceiling = 60
		if len(t) > ceiling {
			t = t[:ceiling] + "…"
		}
		return t
	}
	return "untitled session"
}
