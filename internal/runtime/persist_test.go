// End-to-end tests of session persistence: writing to the log, resuming in a
// new process and recovering content the guardrail removed from the window.
package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Fechar() })
	return st
}

func fakeConfig(t *testing.T, url string) config.Config {
	t.Helper()
	return config.Config{BaseURL: url, Model: "fake", APIKey: "k", Janela: 100_000}
}

// ── NewSessionWithStore / Attach / ID ─────────────────────────────────────

func TestNewSessionWithStoreIndexesAndPersists(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101090000-abcd"
	s := NewSessionWithStore(config.Config{}, st, id)
	if s.ID() != id {
		t.Fatalf("ID() = %q, expected %q", s.ID(), id)
	}
	if !s.Persistente() {
		t.Fatal("a session with a store should be persistent")
	}
	// Without conversation, nothing goes to the log: the session is only born
	// with the first event.
	if _, err := st.GetSession(id); err == nil {
		t.Fatal("a session without events should not be indexed")
	}
	if _, err := st.StoreEvent(id, "user_message", map[string]any{"role": "user", "content": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(id); err != nil {
		t.Fatalf("session not indexed after the first event: %v", err)
	}
}

func TestAttachAfterNewSession(t *testing.T) {
	st := testStore(t)
	// NewSession keeps working WITHOUT a database (-p mode and tests).
	s := NewSession(config.Config{})
	if s.Persistente() {
		t.Fatal("NewSession should not have persistence")
	}
	if s.ID() != "" {
		t.Fatalf("ID() without store = %q", s.ID())
	}
	// Attach binds later: the REPL path when the database opens best-effort.
	s.Attach(st, "bh-20240101090000-ef01")
	if !s.Persistente() || s.ID() != "bh-20240101090000-ef01" {
		t.Fatalf("Attach did not bind: persistent=%v id=%q", s.Persistente(), s.ID())
	}
}

func TestAttachIgnoresBadInput(t *testing.T) {
	st := testStore(t)
	s := NewSession(config.Config{})
	s.Attach(nil, "bh-x") // nil store
	s.Attach(st, "   ")   // empty id
	if s.Persistente() || s.ID() != "" {
		t.Fatalf("Attach with bad input enabled persistence: %v %q", s.Persistente(), s.ID())
	}
}

// ── Send recording events ─────────────────────────────────────────────────

// single-turn provider: plain text, no tool.
func textProvider(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": text},
		}}})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestSendRecordsUserMessageAndAssistant(t *testing.T) {
	srv := textProvider(t, "model answer")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101100000-0001"

	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	var sb strings.Builder
	if err := s.Send(context.Background(), RendererDe(&sb), t.TempDir(), "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d; expected 2 (user_message + assistant): %+v", len(events), events)
	}
	if events[0].Kind != "user_message" {
		t.Fatalf("first event = %q, expected user_message", events[0].Kind)
	}
	if !strings.Contains(events[0].Payload, `"content":"hi"`) {
		t.Fatalf("user_message payload without the prompt: %s", events[0].Payload)
	}
	if events[1].Kind != "assistant" {
		t.Fatalf("second event = %q, expected assistant", events[1].Kind)
	}
	if !strings.Contains(events[1].Payload, `"content":"model answer"`) {
		t.Fatalf("assistant payload without the text: %s", events[1].Payload)
	}
}

// Pasted images go into the user_message payload and come back on resume.
func TestSendWithImagesPersistsAndResumes(t *testing.T) {
	srv := textProvider(t, "seen")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101100000-0009"
	img := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(img, []byte("\x89PNGfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	if err := s.SendWithImages(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "look [Image 1]", []string{img}); err != nil {
		t.Fatalf("SendWithImages: %v", err)
	}
	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || !strings.Contains(events[0].Payload, `"images"`) {
		t.Fatalf("user_message without images: %+v", events)
	}
	h := eventsToMessages(st, events)
	if len(h) == 0 || h[0].Role != "user" || len(h[0].Images) != 1 || h[0].Images[0] != img {
		t.Fatalf("resumed history lost the image: %+v", h)
	}
}

func TestSendWithoutStoreDoesNotRecord(t *testing.T) {
	srv := textProvider(t, "text")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101100000-0002"
	// Session WITHOUT a store: the turn works and the log stays untouched.
	s := NewSession(fakeConfig(t, srv.URL))
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hi"); err != nil {
		t.Fatalf("Send without store: %v", err)
	}
	if n, _ := st.ContarEventos(id); n != 0 {
		t.Fatalf("%d events recorded for a session without a store", n)
	}
}

func TestClearHistoryRecordsSessionReset(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101100000-0003"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.ClearHistory()
	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "session_reset" {
		t.Fatalf("ClearHistory should record session_reset, got %+v", events)
	}
}

// ── Resume rebuilding the transcript ──────────────────────────────────────

func TestResumeRebuildsTranscript(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0001"
	// First process: conversation with a tool call + result. Send only records
	// assistant when there is committed text (the empty event of a round that
	// ended in a tool call never enters the log), so the conversation here
	// follows that same rule.
	s := NewSessionWithStore(config.Config{}, st, id)
	events := []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "read the file"}},
		{"tool_call", map[string]any{
			"role": "tool_call", "tool_id": "call_1", "tool_name": "file_read",
			"tool_args": `{"path":"x.txt"}`,
		}},
		{"tool_result", map[string]any{
			"role": "tool_result", "tool_id": "call_1", "tool_name": "file_read",
			"content": "1: content",
		}},
		{"assistant", map[string]any{"role": "assistant", "content": "done, read it"}},
	}
	for _, e := range events {
		s.record(e.kind, e.payload)
	}

	// Second process: same session, transcript rebuilt from the log.
	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := s2.History()
	if len(h) != 4 {
		t.Fatalf("history rebuilt with %d messages; expected 4:\n%+v", len(h), h)
	}
	if h[0].Role != "user" || h[0].Conteudo != "read the file" {
		t.Fatalf("message 0 wrong: %+v", h[0])
	}
	if h[1].Role != "tool_call" || h[1].ToolName != "file_read" || h[1].ToolArgs != `{"path":"x.txt"}` {
		t.Fatalf("tool_call did not come back intact: %+v", h[1])
	}
	if h[2].Role != "tool_result" || h[2].Conteudo != "1: content" {
		t.Fatalf("tool_result did not come back intact: %+v", h[2])
	}
	if h[3].Role != "assistant" || h[3].Conteudo != "done, read it" {
		t.Fatalf("final assistant wrong: %+v", h[3])
	}
}

// Observability events (guardrail, session_reset, metadata) do NOT enter the
// transcript: they are comments about the conversation, not the conversation.
func TestResumeIgnoresObservabilityEvents(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0009"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.record("user_message", map[string]any{"role": "user", "content": "hi"})
	s.record("guardrail", map[string]any{"tipo": "proativa", "janela": 100000})
	s.record("session_reset", map[string]any{"role": "system", "content": "history cleared"})
	s.record("assistant", map[string]any{"role": "assistant", "content": "hello"})

	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 2 {
		t.Fatalf("history = %d messages; expected 2 (content only): %+v", len(h), h)
	}
	if h[0].Role != "user" || h[1].Role != "assistant" {
		t.Fatalf("wrong order/roles: %+v", h)
	}
}

func TestResumeWithoutEventsLeavesEmpty(t *testing.T) {
	st := testStore(t)
	s := NewSessionWithStore(config.Config{}, st, "bh-20240101110000-0002")
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume of a session without events: %v", err)
	}
	if len(s.History()) != 0 {
		t.Fatalf("history should be empty, got %d", len(s.History()))
	}
}

func TestResumeRehydratesBlob(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0003"
	large := strings.Repeat("tool output line\n", 2000) // > 16 KiB

	s := NewSessionWithStore(config.Config{}, st, id)
	s.recordResult("call_9", "terminal_run", large, false)

	// The recorded event is short: preview + blob reference.
	events, _ := st.ListEvents(id, 0, 0)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if !strings.Contains(events[0].Payload, `"blob":`) {
		t.Fatalf("large result did not become a blob: %s", events[0].Payload)
	}
	if len(events[0].Payload) > 1024 {
		t.Fatalf("payload with blob has %d bytes", len(events[0].Payload))
	}

	// Resume returns the WHOLE content — that is the rehydration promise.
	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 1 {
		t.Fatalf("history = %d messages", len(h))
	}
	if h[0].Conteudo != large {
		t.Fatalf("blob not rehydrated: got %d of %d bytes", len(h[0].Conteudo), len(large))
	}
}

// ── Search / Retrieve ─────────────────────────────────────────────────────

func TestSearchAndRetrieve(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101120000-0001"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.record("user_message", map[string]any{"role": "user", "content": "I need the path of FileX"})
	toolSeq, _ := st.StoreEvent(id, "tool_result", map[string]any{
		"role": "tool_result", "tool_name": "file_read", "content": "C:\\data\\FileX.txt",
	})

	// Case-insensitive search over the content field.
	hits := s.Search("filex", 10)
	if len(hits) == 0 {
		t.Fatal("Search found nothing")
	}
	var foundTool bool
	for _, h := range hits {
		if h.Kind == "tool_result" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("Search did not find the tool_result: %+v", hits)
	}

	// Retrieve returns the full event.
	e, err := s.Retrieve(toolSeq)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if e.Seq != toolSeq {
		t.Fatalf("Retrieve returned seq %d, expected %d", e.Seq, toolSeq)
	}
	if !strings.Contains(e.Payload, `C:\\data\\FileX.txt`) {
		t.Fatalf("Retrieve lost the content: %s", e.Payload)
	}
	if _, err := s.Retrieve(99999); err == nil {
		t.Fatal("Retrieve of a missing seq should fail")
	}
}

func TestSearchWithoutPersistenceReturnsNil(t *testing.T) {
	s := NewSession(config.Config{})
	if hits := s.Search("anything", 10); hits != nil {
		t.Fatalf("Search without store returned %+v", hits)
	}
	if _, err := s.Retrieve(1); err == nil {
		t.Fatal("Retrieve without store should fail")
	}
	// Resume without store is a no-op, not an error.
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume without store: %v", err)
	}
}

// ── the model's tools ─────────────────────────────────────────────────────

func TestSessionSpecsSearchAndRetrieve(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101130000-0001"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.record("tool_result", map[string]any{
		"role": "tool_result", "tool_name": "terminal_run",
		"content": "exit_code: 0\ngit status --short",
	})

	specs := SessaoSpecs(s)
	if len(specs) != 2 {
		t.Fatalf("SessaoSpecs returned %d specs; expected 2", len(specs))
	}
	byName := map[string]tools.Spec{}
	for _, sp := range specs {
		byName[sp.Name] = sp
	}
	search, ok := byName["session_search"]
	if !ok {
		t.Fatal("session_search is not in the specs")
	}
	if !strings.Contains(strings.ToLower(search.Description), "compact") {
		t.Fatalf("session_search description does not mention compaction:\n%s", search.Description)
	}
	rtr, ok := byName["session_retrieve"]
	if !ok {
		t.Fatal("session_retrieve is not in the specs")
	}
	if !strings.Contains(strings.ToLower(rtr.Description), "seq") {
		t.Fatalf("session_retrieve description does not mention seq:\n%s", rtr.Description)
	}
	// Schemas with the promised parameters.
	if _, ok := search.Schema["properties"].(map[string]any)["query"]; !ok {
		t.Fatal("session_search without query parameter")
	}
	if _, ok := search.Schema["properties"].(map[string]any)["limit"]; !ok {
		t.Fatal("session_search without limit parameter")
	}
	if _, ok := rtr.Schema["properties"].(map[string]any)["seq"]; !ok {
		t.Fatal("session_retrieve without seq parameter")
	}
	if _, ok := rtr.Schema["properties"].(map[string]any)["limit"]; !ok {
		t.Fatal("session_retrieve without limit parameter")
	}

	// Exec of session_search finds the recorded content.
	output, err := search.Exec(nil, t.TempDir(), json.RawMessage(`{"query":"git status"}`))
	if err != nil {
		t.Fatalf("exec session_search: %v", err)
	}
	if !strings.Contains(output, "git status") {
		t.Fatalf("session_search did not return the excerpt:\n%s", output)
	}

	// Exec of session_retrieve returns the whole event.
	output, err = rtr.Exec(nil, t.TempDir(), json.RawMessage(`{"seq":1}`))
	if err != nil {
		t.Fatalf("exec session_retrieve: %v", err)
	}
	if !strings.Contains(output, "terminal_run") {
		t.Fatalf("session_retrieve did not return the event:\n%s", output)
	}
}

func TestSessionSpecsWithoutPersistenceExplain(t *testing.T) {
	s := NewSession(config.Config{})
	for _, sp := range SessaoSpecs(s) {
		args := `{}`
		if sp.Name == "session_search" {
			args = `{"query":"x"}`
		} else if sp.Name == "session_retrieve" {
			args = `{"seq":1}`
		}
		output, err := sp.Exec(nil, t.TempDir(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s without store returned an error: %v", sp.Name, err)
		}
		if !strings.Contains(output, "persistence") {
			t.Fatalf("%s without store should explain there is no log:\n%s", sp.Name, output)
		}
	}
}

// Send includes the session tools in the catalog sent to the provider.
func TestSendExposesSessionToolsInSchema(t *testing.T) {
	var sawSearch, sawRetrieve bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, tl := range req.Tools {
			switch tl.Function.Name {
			case "session_search":
				sawSearch = true
			case "session_retrieve":
				sawRetrieve = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	st := testStore(t)
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, "bh-20240101130000-0002")
	_ = s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hi")
	if !sawSearch || !sawRetrieve {
		t.Fatalf("provider did not see the session tools (search=%v retrieve=%v)", sawSearch, sawRetrieve)
	}
}

// A resumed session already knows the context size: it is the prompt_tokens
// of the last request left in the transcript.
func TestLastContext(t *testing.T) {
	s := NewSession(config.Config{})
	if s.UltimoContexto() != 0 {
		t.Fatal("an empty session should have context 0")
	}
	s.history = []dealer.Mensagem{
		{Role: "user", Conteudo: "hi"},
		{Role: "assistant", Conteudo: "a", Usage: &dealer.Usage{PromptTokens: 100}},
		{Role: "user", Conteudo: "again"},
		{Role: "assistant", Conteudo: "b", Usage: &dealer.Usage{PromptTokens: 15139}},
		{Role: "user", Conteudo: "question without an answer yet"},
	}
	if got := s.UltimoContexto(); got != 15139 {
		t.Fatalf("UltimoContexto = %d", got)
	}
}

// A large result in a session without a database (the subagent case) must
// not crash the process.
func TestLargeResultWithoutStoreDoesNotCrash(t *testing.T) {
	s := NewSession(config.Config{})
	if p := s.recordResult("c1", "file_read", strings.Repeat("x", blobThreshold+10), false); p != nil {
		t.Fatalf("without store it should not record: %v", p)
	}
}
