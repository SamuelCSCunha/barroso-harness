// Session live context: the file the model ITSELF edits to keep the working
// state between turns — goal, confirmed facts, decisions, constraints,
// artifacts/IDs, progress, pending items and open questions.
//
// It is not a log. The log (internal/store) is append-only and records what
// happened (user_message, assistant, tool_call...). The live context is the
// model-curated EXTRACT of what matters now: one describes the past, the other
// carries the present. That is why it has its own file and can be replaced
// wholesale — while the log only grows.
//
// The state lives in a map keyed by Session pointer (contextBySession), not in
// a struct field: Session is edited by other threads of work in this package,
// and the map keeps this file unaware of them. A Session pointer is stable for
// the life of the process, which makes it a safe key.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Live context limits. READS are cut at 48 KiB because they enter the model
// window on every ctx_read: the body may exceed 256 KiB on disk, but a single
// read cannot. The WRITE limit is what forces compaction — without it, a model
// that only appends bloats the file until it becomes noise that no longer fits
// in any read.
const (
	liveContextReadLimit = 48 << 10 // 48 KiB per ctx_read
	liveContextBodyLimit = 256 << 10
)

// Fixed texts. They are named because tools and tests compare them verbatim:
// changing the string changes what the model reads.
const (
	// liveContextEmpty is what ctx_read returns before the first write — an
	// invitation to act, not an error.
	liveContextEmpty = "(live context empty — use ctx_write to start)"

	// liveContextTooLarge is the write error: a single line, because a tool
	// message with a newline in the middle becomes a malformed log event.
	liveContextTooLarge = "live context exceeded 256 KiB; compact it with ctx_write"

	// liveContextCutMarker goes at the end of the body when the read was cut.
	liveContextCutMarker = "... (live context cut at %d bytes; use ctx_write to rewrite it compactly)"

	// liveContextVersionPrefix opens the file header line. It is an HTML
	// comment: invisible in any markdown rendering, and stripped on read — the
	// in-memory body never carries it. Kept as-is for on-disk compatibility.
	liveContextVersionPrefix = "<!-- ctx-vivo:"
)

// liveContext is the curated body + the version ledger. The version is not a
// content history (the log is for that): it is the count of committed
// mutations, which tells the model whether its write took and the UI whether
// something changed.
type liveContext struct {
	body     string
	versions []int
}

// version returns the current version (0 = no mutation yet).
func (cx *liveContext) version() int {
	if len(cx.versions) == 0 {
		return 0
	}
	return cx.versions[len(cx.versions)-1]
}

// bumpVersion counts one mutation. The ledger keeps the numbers in order, so
// the last element is always the current version.
func (cx *liveContext) bumpVersion() {
	cx.versions = append(cx.versions, len(cx.versions)+1)
}

// apply is the single state path — reads and mutations go through here. It
// returns the text for the model and, when it blocks the write, the error.
//
// The machine is deliberately dumb: it knows nothing about JSON, Session or
// disk. Translating tool arguments into calls is the spec layer's job.
func (cx *liveContext) apply(op, content string) (string, error) {
	switch op {
	case "read":
		// A whitespace-only body is empty: a blank file carries no state.
		if strings.TrimSpace(cx.body) == "" {
			return liveContextEmpty, nil
		}
		return truncateLiveContextRead(cx.body), nil

	case "write":
		// write REPLACES: only the new content's size matters — the old body
		// will be discarded, so it does not count against the limit.
		if len(content) > liveContextBodyLimit {
			return "", errors.New(liveContextTooLarge)
		}
		cx.body = content
		cx.bumpVersion()
		return fmt.Sprintf("ok: live context rewritten (version %d, %d bytes)", cx.version(), len(cx.body)), nil

	case "append":
		// Two newlines of spacing between what was there and the new excerpt:
		// markdown glues paragraphs without a gap and the model loses the
		// boundary.
		candidate := len(content)
		if cx.body != "" {
			candidate = len(cx.body) + 2 + len(content)
		}
		if candidate > liveContextBodyLimit {
			return "", errors.New(liveContextTooLarge)
		}
		if cx.body == "" {
			cx.body = content
		} else {
			cx.body += "\n\n" + content
		}
		cx.bumpVersion()
		return fmt.Sprintf("ok: appended to the live context (version %d)", cx.version()), nil

	default:
		// "edit" does not exist in this slice on purpose: it would replace the
		// discipline of rewriting compactly with incremental patches, which is
		// how the file rots.
		return "", fmt.Errorf("operation %q does not exist in the live context (use read, write or append)", op)
	}
}

// truncateLiveContextRead applies the read limit. A body within the limit
// comes back whole, without a marker — a marker on a small read is noise.
func truncateLiveContextRead(s string) string {
	if len(s) <= liveContextReadLimit {
		return s
	}
	return s[:liveContextReadLimit] + "\n\n" + fmt.Sprintf(liveContextCutMarker, liveContextReadLimit)
}

// ── per-session state ──────────────────────────────────────────────────────

// contextBySession holds one *liveContext per Session pointer. sync.Map
// because tool calls of one turn run sequentially, but nothing stops an
// integrator from using the session from different goroutines. The value is
// never removed: the session dies with the process, and what must survive is
// in the file.
var contextBySession sync.Map

// contextOf returns the session's live context, creating an empty one the
// first time. A nil Session returns a throwaway state: an orphan spec keeps
// answering instead of bringing down the turn.
func contextOf(s *Session) *liveContext {
	if s == nil {
		return &liveContext{}
	}
	if v, ok := contextBySession.Load(s); ok {
		return v.(*liveContext)
	}
	cx := &liveContext{}
	v, _ := contextBySession.LoadOrStore(s, cx)
	return v.(*liveContext)
}

// sessionlessContext is the state used when the tool does not know which
// session it comes from (-p mode, tests, and EspecificacoesContextoVivo
// without a session). A persistent session has its own in contextBySession —
// the global exists for the Session-less path, and the session is always the
// preferred state.
var sessionlessContext = &liveContext{}

// ── persistence ────────────────────────────────────────────────────────────

// dataHome resolves the data directory. It is a var, not a plain function, so
// tests can inject a temporary directory without depending on package config.
var dataHome = func() string {
	if v := os.Getenv("BARROSO_HARNESS_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.Getwd()
	}
	return filepath.Join(home, ".barroso-harness")
}

// liveContextPath builds <dir>/<sessionID>.ctx-vivo.md. The on-disk name is
// kept unchanged so existing sessions keep their live context.
func liveContextPath(dir, sessionID string) string {
	return filepath.Join(dir, sessionID+".ctx-vivo.md")
}

// save writes atomically: it writes to a temp file IN THE SAME directory and
// renames it over the target. A rename within the same filesystem is atomic —
// a crash midway leaves the previous file intact instead of a half-written one
// the next Resume would read as legitimate context.
func (cx *liveContext) save(dir, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("empty sessionID")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf("%s%d -->\n%s", liveContextVersionPrefix, cx.version(), cx.body)
	target := liveContextPath(dir, sessionID)
	tmp, err := os.CreateTemp(dir, ".ctx-vivo-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// The temp file is born 0600 (CreateTemp) and rename preserves the mode:
	// session state is not a world-readable file.
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// splitLiveContextVersion strips the version header from the file. A
// malformed header (or a model body that happens to start with the prefix) is
// returned WHOLE: losing a line of context is worse than restarting the
// version count from zero.
func splitLiveContextVersion(b []byte) (string, int) {
	s := string(b)
	if !strings.HasPrefix(s, liveContextVersionPrefix) {
		return s, 0
	}
	end := strings.IndexByte(s, '\n')
	line := s
	if end >= 0 {
		line = s[:end]
	}
	if !strings.HasSuffix(line, "-->") {
		return s, 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, liveContextVersionPrefix), "-->")))
	if err != nil || n < 0 {
		return s, 0
	}
	if end < 0 {
		return "", n
	}
	return s[end+1:], n
}

// loadLiveContext reads the session file. A MISSING file is not an error: it
// is a session that has not written anything yet, and the empty state is the
// right answer.
func loadLiveContext(dir, sessionID string) (*liveContext, error) {
	b, err := os.ReadFile(liveContextPath(dir, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return &liveContext{}, nil
		}
		return nil, err
	}
	body, versions := splitLiveContextVersion(b)
	cx := &liveContext{body: body}
	// The version ledger is rebuilt from the header: without it, the first
	// ctx_write after a Resume would announce "version 1" for a third edit of
	// the file.
	for i := 1; i <= versions; i++ {
		cx.bumpVersion()
	}
	return cx, nil
}

// ContextoVivoSalvar writes this session's live context to
// <home>/<sessionID>.ctx-vivo.md. Silent without persistence: an in-memory
// session (-p mode) has nowhere to store it, and that must not bring down a
// turn.
func (s *Session) ContextoVivoSalvar() error {
	if !s.Persistente() {
		return nil
	}
	return contextOf(s).save(dataHome(), s.sessionID)
}

// ContextoVivoCarregar brings this session's live context from disk into
// memory. A session without persistence has nothing to read and stays empty.
func (s *Session) ContextoVivoCarregar() error {
	if !s.Persistente() {
		return nil
	}
	cx, err := loadLiveContext(dataHome(), s.sessionID)
	if err != nil {
		return err
	}
	contextBySession.Store(s, cx)
	return nil
}

// ── specs ──────────────────────────────────────────────────────────────────

// EspecificacoesContextoVivo returns the THREE live context tools, bound to
// the session-less state. It is the catalog for callers without a Session at
// hand (-p mode, tests); an integrator with a session should prefer
// EspecificacoesContextoVivoSessao, which binds the same specs to that
// session's state.
//
// NOTE FOR INTEGRATORS: the static tool catalog (SpecsBuiltins) still carries
// a placeholder ctx_read that reads BARROSO.md. The lists must be merged
// WITHOUT duplicating names — two ctx_read in the schema is an ambiguous call
// for the provider.
func EspecificacoesContextoVivo() []tools.Spec {
	return liveContextSpecs(sessionlessContext)
}

// EspecificacoesContextoVivoSessao returns the same three tools bound to the
// given session's live context — the form used by the REPL, where the state
// must survive the turn and be saved/loaded by sessionID.
func EspecificacoesContextoVivoSessao(s *Session) []tools.Spec {
	return liveContextSpecs(contextOf(s))
}

func liveContextSpecs(cx *liveContext) []tools.Spec {
	return []tools.Spec{
		ctxReadSpec(cx),
		ctxWriteSpec(cx),
		ctxAppendSpec(cx),
	}
}

// ctxReadSpec reads the current body, cut at 48 KiB. It takes no argument: the
// state is already known to the spec.
func ctxReadSpec(cx *liveContext) tools.Spec {
	return tools.Spec{
		Name: "ctx_read",
		Description: "Reads the session's live context: the file YOU maintain between turns with the " +
			"working state (goal, confirmed facts, decisions, constraints, artifacts, " +
			"progress, pending items). Read it at the start of a long task to pick up the thread, and " +
			"after writing to check what was kept. Returns up to 48 KiB.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return cx.apply("read", "")
		},
	}
}

// ctxWriteSpec REPLACES the whole body. The description carries the recipe of
// what to keep and the provenance notice: since the file is replaceable, an
// event that only existed in the log needs its seq/path noted HERE to be found
// again later through session_search/session_retrieve.
func ctxWriteSpec(cx *liveContext) tools.Spec {
	return tools.Spec{
		Name: "ctx_write",
		Description: "REWRITES IN FULL the session's live context (the working-state file " +
			"you maintain between turns). The new content completely replaces the previous one — it does " +
			"not append. Keep: goal and completion criteria; confirmed facts WITH references " +
			"(file path, event seq in the log); decisions and their rationale; constraints " +
			"discovered; relevant artifacts and IDs; progress; pending items and open questions. " +
			"References matter because the log is append-only and large results may leave the " +
			"window: noting the seq/path lets you find everything again with session_search and " +
			"session_retrieve. Also rewrite to COMPACT — drop what no longer matters — whenever " +
			"the file grows.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "the whole live context, in markdown"},
			},
			"required": []string{"content"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			content, err := liveContextContent(raw)
			if err != nil {
				return "", err
			}
			return cx.apply("write", content)
		},
	}
}

// ctxAppendSpec appends to the end. It is the shortcut to record a fact
// without rewriting everything — and the path that bloats the file, which is
// why the description says to compact with ctx_write when it gets heavy.
func ctxAppendSpec(cx *liveContext) tools.Spec {
	return tools.Spec{
		Name: "ctx_append",
		Description: "APPENDS an excerpt to the end of the session's live context, preserving what was already there. " +
			"Use it to record a new fact, a decision made or a pending item without rewriting the " +
			"whole file. If the file is already large, prefer ctx_write: appending without compacting " +
			"is what makes the live context stop fitting in a read. Include references (path/seq) " +
			"when the fact comes from something that may leave the window later.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "the excerpt to append, in markdown"},
			},
			"required": []string{"content"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			content, err := liveContextContent(raw)
			if err != nil {
				return "", err
			}
			return cx.apply("append", content)
		},
	}
}

// liveContextContent translates the call JSON into content. It lives in the
// spec layer, not in apply, because malformed JSON is a tool problem — the
// state machine does not need to know about it.
func liveContextContent(raw json.RawMessage) (string, error) {
	var c struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(c.Content) == "" {
		return "", fmt.Errorf("empty content: write the working state (or ctx_read to see the current one)")
	}
	return c.Content, nil
}
