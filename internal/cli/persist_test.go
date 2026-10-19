// REPL persistence tests: /resume (list, pick, resume), /save (title), the
// statusbar's `· sess <4hex>` suffix and the best-effort fallback when the
// database does not open.
package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

func TestIDFormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := newSessionID()
		parts := strings.Split(id, "-")
		if len(parts) != 3 {
			t.Fatalf("id should have 3 parts separated by '-': %q", id)
		}
		if len(parts[1]) != 14 {
			t.Fatalf("timestamp YYYYMMDDHHMMSS should have 14 digits, got %q", parts[1])
		}
		if len(parts[2]) != 4 {
			t.Fatalf("random suffix should have 4 hex digits, got %q", parts[2])
		}
		if seen[id] {
			t.Fatalf("repeated id: %q", id)
		}
		seen[id] = true
	}
	if short := shortSession(newSessionID()); len(short) != 4 {
		t.Fatalf("shortSession should return the last 4 hex digits, got %q", short)
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		ago       time.Duration
		contains  string
		wantExact string
	}{
		{10 * time.Second, "", "now"},
		{5 * time.Minute, "min", ""},
		{3 * time.Hour, "h", ""},
		{48 * time.Hour, "d", ""},
	}
	for _, c := range cases {
		ts := now.Add(-c.ago).Format(store.TsFormato)
		res := formatAge(ts)
		if c.wantExact != "" && res != c.wantExact {
			t.Fatalf("age of %v = %q, want %q", c.ago, res, c.wantExact)
		}
		if c.contains != "" && !strings.Contains(res, c.contains) {
			t.Fatalf("age of %v = %q, should contain %q", c.ago, res, c.contains)
		}
	}
	if formatAge("garbage") != "?" {
		t.Fatal("an invalid timestamp should return '?'")
	}
}

func TestResolveSessionNumberAndPrefix(t *testing.T) {
	ls := []sessionRow{
		{Session: store.Session{ID: "bh-20240101100000-aaaa", Title: "one"}},
		{Session: store.Session{ID: "bh-20240101110000-bbbb", Title: "two"}},
		{Session: store.Session{ID: "bh-20240101120000-cccc", Title: "three"}},
	}
	// List number (1-based).
	if id := resolveSession("2", ls); id != "bh-20240101110000-bbbb" {
		t.Fatalf("resolve 2 = %q", id)
	}
	// Id prefix and full id.
	if id := resolveSession("bh-20240101120000", ls); id != "bh-20240101120000-cccc" {
		t.Fatalf("resolve prefix = %q", id)
	}
	if id := resolveSession("bh-20240101100000-aaaa", ls); id != "bh-20240101100000-aaaa" {
		t.Fatalf("resolve full id = %q", id)
	}
	// Out of the list or empty: nothing chosen.
	for _, input := range []string{"", "  ", "99", "0", "-1", "does-not-exist"} {
		if id := resolveSession(input, ls); id != "" {
			t.Fatalf("resolve %q should fail, got %q", input, id)
		}
	}
}

func TestSessionStatusbarSuffix(t *testing.T) {
	cfg := &config.Config{CurrentProvider: "p1", Model: "mod"}
	var sb strings.Builder
	sessionStatusbar(&sb, cfg, "bh-20240101100000-abcd")
	output := sb.String()
	if !strings.Contains(output, "· sess abcd") {
		t.Fatalf("statusbar missing the session suffix:\n%s", output)
	}
	// The base line stays the same as in ui.go (provider · model · effort).
	for _, want := range []string{"p1", "mod", "effort off"} {
		if !strings.Contains(output, want) {
			t.Fatalf("statusbar lost %q:\n%s", want, output)
		}
	}
	// Without an active session, no suffix.
	sb.Reset()
	sessionStatusbar(&sb, cfg, "")
	if strings.Contains(sb.String(), "sess") {
		t.Fatalf("without a session there should be no suffix:\n%s", sb.String())
	}
}

func TestFirstErrLine(t *testing.T) {
	res := firstErrLine(testErr{"first line\nprotocol detail\none more"})
	if strings.Contains(res, "detail") || strings.Contains(res, "\n") {
		t.Fatalf("firstErrLine should keep only the first line: %q", res)
	}
	if res != "first line" {
		t.Fatalf("firstErrLine = %q", res)
	}
}

type testErr struct{ s string }

func (e testErr) Error() string { return e.s }

// ── end to end through the REPL ───────────────────────────────────────────

// noColors turns the palette off so asserts see plain text.
func noColors(t *testing.T) {
	t.Helper()
	prevColors := colorsOn
	colorsOn = false
	t.Cleanup(func() { colorsOn = prevColors })
}

// /resume lists the stored sessions and resumes the chosen one.
func TestResumeRestoresSessionFromLog(t *testing.T) {
	home := fakeHome(t)
	noColors(t)
	// A previous session with content in the log. readyConfig creates its own
	// data directory, so it comes first and is the test's REAL home.
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	home = readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	db, err := store.Open(filepath.Join(home, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	const prevSession = "bh-20240101100000-abcd"
	if err := db.NewSession(prevSession); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "pergunta anterior"}},
		{"assistant", map[string]any{"role": "assistant", "content": "resposta anterior"}},
	} {
		if _, err := db.StoreEvent(prevSession, ev.kind, ev.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetTitle(prevSession, "old conversation"); err != nil {
		t.Fatal(err)
	}
	if err := db.Fechar(); err != nil {
		t.Fatal(err)
	}

	// New REPL: /resume, pick by number, /exit.
	io, outIO, _ := bridgeIO(t, "/resume\n1\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("repl exited with %d: %s", code, io.ErrOut)
	}
	screen := outIO.String()
	for _, want := range []string{prevSession, "old conversation", "2 events", "resumed"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("/resume missing %q:\n%s", want, screen)
		}
	}

	// The log is append-only: the previous session stays whole, with its title.
	db2, err := store.Open(filepath.Join(home, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Fechar()
	se, err := db2.GetSession(prevSession)
	if err != nil {
		t.Fatalf("previous session vanished from the log: %v", err)
	}
	if se.Title != "old conversation" {
		t.Fatalf("title = %q", se.Title)
	}
	if n, _ := db2.ContarEventos(prevSession); n != 2 {
		t.Fatalf("previous session events = %d; the log is append-only", n)
	}
}

// /save stores the title; the next listing shows it.
func TestSaveStoresTitle(t *testing.T) {
	noColors(t)
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := bridgeIO(t, "/save my test conversation\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("exit code %d: %s", code, io.ErrOut)
	}
	if !strings.Contains(outIO.String(), "saved") {
		t.Fatalf("/save did not confirm:\n%s", outIO.String())
	}

	// The database is the one in the active config.Dir() (the same the REPL used).
	db, err := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Fechar()
	ls, err := db.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(ls) != 1 {
		t.Fatalf("sessions = %d; want 1: %+v", len(ls), ls)
	}
	if ls[0].Title != "my test conversation" {
		t.Fatalf("stored title = %q", ls[0].Title)
	}
}

// An empty session does not show in /resume — there is no conversation to resume.
func TestResumeWithoutSessionsWarns(t *testing.T) {
	noColors(t)
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := bridgeIO(t, "/resume\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("exit code %d: %s", code, io.ErrOut)
	}
	if !strings.Contains(outIO.String(), "no previous sessions") {
		t.Fatalf("empty /resume did not warn:\n%s", outIO.String())
	}
}

// /rename stores the session name; without an argument, it shows usage.
func TestRenameStoresName(t *testing.T) {
	noColors(t)
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := bridgeIO(t, "/rename\n/rename   refactor   the parser\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	output := outIO.String()
	if !strings.Contains(output, "usage: /rename") || !strings.Contains(output, "session renamed: refactor the parser") {
		t.Fatalf("output:\n%s", output)
	}
	db, err := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Fechar()
	ls, _ := db.ListSessions(10)
	if len(ls) != 1 || ls[0].Title != "refactor the parser" {
		t.Fatalf("stored title: %+v", ls)
	}
}
