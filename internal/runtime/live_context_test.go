// Live context tests: the state machine (apply), the three specs and
// persistence. One test per behaviour the model would notice if it broke —
// the body that comes back from a write, the version that goes up, the limit
// that blocks.
package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── apply: read/write/append ────────────────────────────────────────────────

func TestLiveContextWriteThenReadReturnsExact(t *testing.T) {
	cx := &liveContext{}
	body := "# Goal\nMigrate the store to sqlite.\n\n## Pending\n- review indexes"
	output, err := cx.apply("write", body)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(output, "version 1") {
		t.Fatalf("write should announce version 1: %q", output)
	}
	read, err := cx.apply("read", "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read != body {
		t.Fatalf("roundtrip changed the body\n want: %q\n got:  %q", body, read)
	}
}

func TestLiveContextAppendJoinsWithDoubleNewline(t *testing.T) {
	cx := &liveContext{}
	if _, err := cx.apply("write", "first block"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := cx.apply("append", "second block"); err != nil {
		t.Fatalf("append: %v", err)
	}
	read, err := cx.apply("read", "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read != "first block\n\nsecond block" {
		t.Fatalf("append should join with \\n\\n: %q", read)
	}
}

func TestLiveContextAppendOnEmptySetsBody(t *testing.T) {
	cx := &liveContext{}
	// Without a previous write, append must not prefix newlines — otherwise the
	// file would be born with blank spacing lines and nothing before them.
	if _, err := cx.apply("append", "initial content"); err != nil {
		t.Fatalf("append on empty: %v", err)
	}
	read, _ := cx.apply("read", "")
	if read != "initial content" {
		t.Fatalf("append on empty should set the body: %q", read)
	}
}

func TestLiveContextWriteReplacesWhole(t *testing.T) {
	cx := &liveContext{}
	cx.apply("write", "old text that must disappear")
	cx.apply("write", "new text")
	read, _ := cx.apply("read", "")
	if read != "new text" {
		t.Fatalf("write should replace entirely: %q", read)
	}
	if strings.Contains(read, "old") {
		t.Fatalf("write left remains of the previous content: %q", read)
	}
}

// ── versions ────────────────────────────────────────────────────────────────

func TestLiveContextVersionIncrementsOnMutationNotOnRead(t *testing.T) {
	cx := &liveContext{}
	if cx.version() != 0 {
		t.Fatalf("new state should be at version 0, not %d", cx.version())
	}
	// Repeated reads on an empty state must not create a version: the counter
	// measures mutation, not curiosity.
	for i := 0; i < 3; i++ {
		cx.apply("read", "")
	}
	if cx.version() != 0 {
		t.Fatalf("read bumped the version: %d", cx.version())
	}
	cx.apply("write", "a")
	cx.apply("read", "")
	cx.apply("append", "b")
	cx.apply("read", "")
	if cx.version() != 2 {
		t.Fatalf("two mutations should give version 2, not %d", cx.version())
	}

	output, _ := cx.apply("write", "rewritten")
	if !strings.Contains(output, "version 3") {
		t.Fatalf("third mutation should announce version 3: %q", output)
	}
}

// ── 256 KiB limit ───────────────────────────────────────────────────────────

func TestLiveContextLimitOf256KiBIsExact(t *testing.T) {
	if liveContextBodyLimit != 256<<10 {
		t.Fatalf("limit should be 256 KiB, is %d", liveContextBodyLimit)
	}
	if liveContextReadLimit != 48<<10 {
		t.Fatalf("read should be capped at 48 KiB, is %d", liveContextReadLimit)
	}
}

func TestLiveContextWriteAboveLimitFailsWithExactMessage(t *testing.T) {
	cx := &liveContext{}
	large := strings.Repeat("x", liveContextBodyLimit+1)
	_, err := cx.apply("write", large)
	if err == nil {
		t.Fatal("write above the limit should fail")
	}
	if err.Error() != liveContextTooLarge {
		t.Fatalf("message should be %q, was %q", liveContextTooLarge, err.Error())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("limit error should be a single line: %q", err.Error())
	}
	// Failing must not leave the previous body half-written.
	if cx.body != "" {
		t.Fatalf("a rejected write should not change the body, it has %d bytes", len(cx.body))
	}
}

func TestLiveContextAppendBeyondLimitFails(t *testing.T) {
	cx := &liveContext{}
	half := strings.Repeat("a", liveContextBodyLimit/2)
	if _, err := cx.apply("write", half); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	_, err := cx.apply("append", strings.Repeat("b", liveContextBodyLimit/2+1))
	if err == nil {
		t.Fatal("an append that exceeds the limit should fail")
	}
	if err.Error() != liveContextTooLarge {
		t.Fatalf("append limit message should be %q, was %q", liveContextTooLarge, err.Error())
	}
	// A rejected append must not mutate: the version stays where it was.
	if cx.version() != 1 {
		t.Fatalf("a rejected append should not count a version, went to %d", cx.version())
	}
}

// write replaces, so a NEW body within the limit passes even if the previous
// one was large — the size that counts is the incoming content's.
func TestLiveContextCompactWritePassesAfterLargeBody(t *testing.T) {
	cx := &liveContext{}
	cx.apply("write", strings.Repeat("g", liveContextBodyLimit))
	if _, err := cx.apply("write", "compact"); err != nil {
		t.Fatalf("compact write should pass: %v", err)
	}
	if cx.body != "compact" {
		t.Fatalf("write should have replaced the body: %q", cx.body)
	}
}

// ── read cut at 48 KiB ──────────────────────────────────────────────────────

func TestLiveContextReadIsCutAt48KiB(t *testing.T) {
	cx := &liveContext{}
	cx.body = strings.Repeat("z", liveContextBodyLimit) // large body, small read
	read, err := cx.apply("read", "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(read) <= liveContextReadLimit {
		t.Fatalf("reading a large body should come back cut, got %d bytes", len(read))
	}
	if !strings.Contains(read, "live context cut at 49152 bytes") {
		t.Fatalf("missing cut marker: %q", read[liveContextReadLimit:])
	}
	if !strings.HasPrefix(read, strings.Repeat("z", 100)) {
		t.Fatal("the cut should preserve the body prefix")
	}
}

func TestLiveContextReadWithinLimitHasNoMarker(t *testing.T) {
	cx := &liveContext{}
	cx.body = strings.Repeat("z", liveContextReadLimit)
	read, _ := cx.apply("read", "")
	if strings.Contains(read, "cut at") {
		t.Fatal("a body exactly at the limit should not get a cut marker")
	}
}

// ── empty state ─────────────────────────────────────────────────────────────

func TestLiveContextEmptyReadInvitesWriting(t *testing.T) {
	cx := &liveContext{}
	read, err := cx.apply("read", "")
	if err != nil {
		t.Fatalf("read on empty should not fail: %v", err)
	}
	if read != liveContextEmpty {
		t.Fatalf("empty should return %q, returned %q", liveContextEmpty, read)
	}
	if !strings.Contains(read, "context_write") {
		t.Fatalf("the invitation should mention context_write: %q", read)
	}
}

// A whitespace-only body is empty: a model that "cleared" it with spaces still
// sees the invitation, not a blank screen without explanation.
func TestLiveContextWhitespaceOnlyBodyIsEmpty(t *testing.T) {
	cx := &liveContext{}
	cx.body = "   \n\t\n  "
	read, _ := cx.apply("read", "")
	if read != liveContextEmpty {
		t.Fatalf("a whitespace-only body should count as empty: %q", read)
	}
}

// ── operations outside the slice ────────────────────────────────────────────

func TestLiveContextUnknownOperationFails(t *testing.T) {
	cx := &liveContext{}
	_, err := cx.apply("edit", "something")
	if err == nil {
		t.Fatal("edit is outside the slice and should fail")
	}
	if !strings.Contains(err.Error(), "edit") {
		t.Fatalf("the error should name the rejected operation: %q", err.Error())
	}
}

// ── specs ───────────────────────────────────────────────────────────────────

func TestLiveContextSpecsContainTheThreeTools(t *testing.T) {
	list := EspecificacoesContextoVivo()
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	want := []string{"context_read", "context_write", "context_append"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("specs should be %v, were %v", want, names)
	}
	for _, s := range list {
		if s.Description == "" {
			t.Fatalf("%s without description: the provider does not know when to use it", s.Name)
		}
		if s.Exec == nil {
			t.Fatalf("%s without Exec: a decorative spec does not execute", s.Name)
		}
		if s.Schema == nil {
			t.Fatalf("%s without Schema", s.Name)
		}
	}
}

func TestLiveContextSpecsRequiredFields(t *testing.T) {
	for _, s := range EspecificacoesContextoVivo() {
		required, _ := s.Schema["required"].([]string)
		switch s.Name {
		case "context_read":
			if len(required) != 0 {
				t.Fatalf("context_read should require no argument, requires %v", required)
			}
		case "context_write", "context_append":
			if !reflect.DeepEqual(required, []string{"content"}) {
				t.Fatalf("%s should require exactly [content], requires %v", s.Name, required)
			}
			props, _ := s.Schema["properties"].(map[string]any)
			if props["content"] == nil {
				t.Fatalf("%s without the content property in the schema", s.Name)
			}
			if props["content"].(map[string]any)["type"] != "string" {
				t.Fatalf("%s: content should be a string", s.Name)
			}
		default:
			t.Fatalf("unexpected spec: %s", s.Name)
		}
	}
}

func TestLiveContextReadSpecOnEmptyState(t *testing.T) {
	cx := &liveContext{}
	specs := liveContextSpecs(cx)
	for _, s := range specs {
		if s.Name != "context_read" {
			continue
		}
		output, err := s.Exec(context.Background(), "", nil)
		if err != nil {
			t.Fatalf("context_read on empty: %v", err)
		}
		if output != liveContextEmpty {
			t.Fatalf("empty context_read should return %q, returned %q", liveContextEmpty, output)
		}
		return
	}
	t.Fatal("context_read missing from the specs")
}

func TestLiveContextWriteAndAppendSpecsExecute(t *testing.T) {
	cx := &liveContext{}
	specs := liveContextSpecs(cx)
	byName := make(map[string]tools.Spec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	// Raw JSON, as it comes from the provider — the path Exec actually gets.
	output, err := byName["context_write"].Exec(context.Background(), "", json.RawMessage(`{"content":"initial state"}`))
	if err != nil {
		t.Fatalf("context_write: %v", err)
	}
	if !strings.Contains(output, "live context rewritten (version 1") {
		t.Fatalf("context_write should confirm the version: %q", output)
	}
	if _, err := byName["context_append"].Exec(context.Background(), "", json.RawMessage(`{"content":"new pending item"}`)); err != nil {
		t.Fatalf("context_append: %v", err)
	}
	read, _ := cx.apply("read", "")
	if read != "initial state\n\nnew pending item" {
		t.Fatalf("write+append through the specs should compose the body: %q", read)
	}
}

func TestLiveContextSpecWithoutContentFailsClearly(t *testing.T) {
	cx := &liveContext{}
	specs := liveContextSpecs(cx)
	for _, name := range []string{"context_write", "context_append"} {
		for _, s := range specs {
			if s.Name != name {
				continue
			}
			_, err := s.Exec(context.Background(), "", json.RawMessage(`{"other":"x"}`))
			if err == nil {
				t.Fatalf("%s without content should fail", name)
			}
			if !strings.Contains(err.Error(), "content") {
				t.Fatalf("%s should explain that content is missing: %q", name, err.Error())
			}
			if strings.Contains(err.Error(), "\n") {
				t.Fatalf("%s error should be a single line: %q", name, err.Error())
			}
		}
	}
}

func TestLiveContextWriteSpecAboveLimitPropagatesError(t *testing.T) {
	cx := &liveContext{}
	output, err := cx.apply("write", strings.Repeat("x", liveContextBodyLimit+1))
	if err == nil || output != "" {
		t.Fatalf("apply should return only the error at the limit: %q / %v", output, err)
	}
	// And the tool must propagate it, not swallow it into "ok".
	for _, s := range liveContextSpecs(cx) {
		if s.Name != "context_write" {
			continue
		}
		if _, terr := s.Exec(context.Background(), "", json.RawMessage(`{"content":"`+strings.Repeat("x", liveContextBodyLimit+1)+`"}`)); terr == nil {
			t.Fatal("context_write above the limit should fail")
		}
		return
	}
	t.Fatal("context_write missing")
}

// ── persistence ─────────────────────────────────────────────────────────────

func TestLiveContextSaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	dataHome = func() string { return dir }
	t.Cleanup(func() { dataHome = defaultDataHome })

	cx := &liveContext{}
	cx.apply("write", "# Goal\nfinish the live context")
	cx.apply("append", "- task 1")
	if err := cx.save(dir, "sess-1"); err != nil {
		t.Fatalf("save: %v", err)
	}
	// The file has the promised name: whoever looks for it on disk finds it.
	if _, err := os.Stat(filepath.Join(dir, "sess-1.ctx-vivo.md")); err != nil {
		t.Fatalf("file should exist at %s: %v", filepath.Join(dir, "sess-1.ctx-vivo.md"), err)
	}
	back, err := loadLiveContext(dir, "sess-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	read, _ := back.apply("read", "")
	if read != "# Goal\nfinish the live context\n\n- task 1" {
		t.Fatalf("roundtrip changed the body:\n %q", read)
	}
	// The version must survive: without it the model would write "version 1"
	// over a third edit and lose the sense of progress.
	if back.version() != cx.version() {
		t.Fatalf("version should survive the roundtrip: %d → %d", cx.version(), back.version())
	}
	if next, _ := back.apply("append", "one more"); !strings.Contains(next, "version 3") {
		t.Fatalf("the next mutation should continue the count: %q", next)
	}
}

func TestLiveContextLoadMissingFileIsEmptyWithoutError(t *testing.T) {
	dir := t.TempDir()
	cx, err := loadLiveContext(dir, "never-written")
	if err != nil {
		t.Fatalf("a missing file should not fail: %v", err)
	}
	if cx == nil {
		t.Fatal("should return a usable state, not nil")
	}
	read, _ := cx.apply("read", "")
	if read != liveContextEmpty {
		t.Fatalf("a session without a file should be born empty: %q", read)
	}
	if cx.version() != 0 {
		t.Fatalf("a session without a file should be at version 0: %d", cx.version())
	}
}

// A model may write a body that starts exactly with the header prefix; and a
// crash may leave the header unclosed. In both cases the whole file is the
// context — never lose a line because of metadata.
func TestLiveContextLoadMalformedHeaderKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	cases := []string{
		"<!-- ctx-vivo:3 -->\nreal model body",
		"<!-- ctx-vivo: body without closing",
		"<!-- ctx-vivo:abc -->\nbody",
	}
	for i, content := range cases {
		id := "bad-" + string(rune('a'+i))
		if err := os.WriteFile(filepath.Join(dir, id+".ctx-vivo.md"), []byte(content), 0o600); err != nil {
			t.Fatalf("write case %d: %v", i, err)
		}
		cx, err := loadLiveContext(dir, id)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		read, _ := cx.apply("read", "")
		if !strings.Contains(read, "body") {
			t.Fatalf("case %d lost the content: %q", i, read)
		}
	}
	// The valid case, however, must split header from body: otherwise the model
	// would read the comment as if it were state it wrote.
	cx, _ := loadLiveContext(dir, "bad-a")
	read, _ := cx.apply("read", "")
	if strings.Contains(read, "ctx-vivo:") {
		t.Fatalf("a valid header should be stripped on read: %q", read)
	}
	if cx.version() != 3 {
		t.Fatalf("a valid header should restore version 3, got %d", cx.version())
	}
}

func TestLiveContextSaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	cx := &liveContext{}
	cx.apply("write", "something")
	if err := cx.save(dir, "sess-2"); err != nil {
		t.Fatalf("save should create the directory: %v", err)
	}
	back, err := loadLiveContext(dir, "sess-2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if read, _ := back.apply("read", ""); read != "something" {
		t.Fatalf("content lost: %q", read)
	}
}

func TestLiveContextSaveRejectsEmptySessionID(t *testing.T) {
	cx := &liveContext{}
	cx.apply("write", "something")
	// An empty sessionID would create a loose ".ctx-vivo.md" file at the home
	// root: garbage that no load ever finds again.
	if err := cx.save(t.TempDir(), "  "); err == nil {
		t.Fatal("an empty sessionID should be rejected")
	}
}

func TestLiveContextSaveOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	cx := &liveContext{}
	cx.apply("write", "first version")
	if err := cx.save(dir, "sess-3"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	cx.apply("write", "second version")
	if err := cx.save(dir, "sess-3"); err != nil {
		t.Fatalf("second save: %v", err)
	}
	back, _ := loadLiveContext(dir, "sess-3")
	if read, _ := back.apply("read", ""); read != "second version" {
		t.Fatalf("a repeated save should overwrite: %q", read)
	}
	// And it must not leave a temp file behind: each save leaves the directory
	// as it was, with only the final file.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("only the final file should remain, %d entries remain", len(ents))
	}
}

// ── per-session state ───────────────────────────────────────────────────────

func TestLiveContextSessionHasOwnState(t *testing.T) {
	a := NewSession(emptyLocalConfig())
	b := NewSession(emptyLocalConfig())
	defer func() {
		contextBySession.Delete(a)
		contextBySession.Delete(b)
	}()
	// Two sessions open at the same time must not share a body: the state of
	// one is the work of that conversation.
	contextOf(a).apply("write", "state of session A")
	contextOf(b).apply("write", "state of session B")
	if read, _ := contextOf(a).apply("read", ""); read != "state of session A" {
		t.Fatalf("session A should keep its state: %q", read)
	}
	if read, _ := contextOf(b).apply("read", ""); read != "state of session B" {
		t.Fatalf("session B should keep its state: %q", read)
	}
	// And the same session ALWAYS returns the same pointer: a spec opened in
	// one turn must see the next turn's mutation.
	if contextOf(a) != contextOf(a) {
		t.Fatal("contextOf should return the same state for the same session")
	}
}

func TestLiveContextSessionSpecsCloseOverItsState(t *testing.T) {
	s := NewSession(emptyLocalConfig())
	defer contextBySession.Delete(s)
	for _, sp := range EspecificacoesContextoVivoSessao(s) {
		if sp.Name == "context_write" {
			if _, err := sp.Exec(context.Background(), "", json.RawMessage(`{"content":"state of this session"}`)); err != nil {
				t.Fatalf("session context_write: %v", err)
			}
		}
	}
	if read, _ := contextOf(s).apply("read", ""); read != "state of this session" {
		t.Fatalf("the spec should have written to the session state: %q", read)
	}
	// And the session-less catalog stays independent.
	if read, _ := sessionlessContext.apply("read", ""); read != liveContextEmpty {
		t.Fatalf("the session-less state should remain empty: %q", read)
	}
}

func TestLiveContextSessionWithoutPersistenceSavesWithoutError(t *testing.T) {
	s := NewSession(emptyLocalConfig())
	// No file should be created: without a sessionID there is nowhere to store.
	if err := s.ContextoVivoSalvar(); err != nil {
		t.Fatalf("save without persistence should be silent: %v", err)
	}
	if err := s.ContextoVivoCarregar(); err != nil {
		t.Fatalf("load without persistence should be silent: %v", err)
	}
}

func TestLiveContextSessionSavesAndLoadsWithInjectedHome(t *testing.T) {
	dir := t.TempDir()
	dataHome = func() string { return dir }
	t.Cleanup(func() { dataHome = defaultDataHome })

	s := NewSession(emptyLocalConfig())
	defer contextBySession.Delete(s)
	s.sessionID = "sess-inj"
	s.store = nil
	// Persistente() requires a store; what matters here is the save/load pair
	// reading the injected home.
	if s.Persistente() {
		t.Fatal("precondition: session should be without persistence")
	}
	contextOf(s).apply("write", "injected state")
	contextOf(s).save(dir, s.sessionID)

	// Fresh state, like a Resume in a new process.
	other := NewSession(emptyLocalConfig())
	defer contextBySession.Delete(other)
	other.sessionID = "sess-inj"
	other.store = nil
	if err := other.ContextoVivoCarregar(); err == nil {
		// Without a store, the wrapper is silent; the direct read is the real path.
		t.Log("wrapper without store does not load — expected")
	}
	cx, err := loadLiveContext(dataHome(), other.sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if read, _ := cx.apply("read", ""); read != "injected state" {
		t.Fatalf("the state should come back from disk: %q", read)
	}
}

// defaultDataHome keeps the original implementation of dataHome, so a test
// that injected a temporary directory does not leak the injection into the
// following ones.
var defaultDataHome = func() string {
	if v := os.Getenv("BARROSO_HARNESS_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.Getwd()
	}
	return filepath.Join(home, ".barroso-harness")
}

// emptyLocalConfig builds a configuration without provider/model: the sessions
// in these tests never call Send, they only need a stable pointer to serve as
// a key in contextBySession.
func emptyLocalConfig() config.Config { return config.Config{} }
