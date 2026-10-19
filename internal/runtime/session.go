// Package runtime implements a reusable conversation session. The transcript
// only commits a round after a valid response and, for tool_calls, after all
// results — errors/cancellations never leave orphaned calls.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/budget"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/instructions"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Session holds the transcript and reconfigurable state of a REPL.
type Session struct {
	config  config.Config
	dealer  *dealer.Cliente
	history []dealer.Mensagem
	est     budget.Estimador
	key     string // calibration per (normalized URL, model)

	// Persistence is OPTIONAL. A nil store means an in-memory-only session (the
	// NewSession path, used by -p mode and by tests). With a store, every
	// committed event of the turn goes into the log — and the session can be
	// resumed by Resume in a new process.
	store     *store.Store
	sessionID string

	// injected holds the specs of EXTERNAL subsystems (subagents, MCP), added
	// by the wiring through Injetar. They do not live in the static catalog
	// because those tools are owned by a process: see extras.go.
	//
	// muExtras only guards this field. The mutex is fine-grained on purpose —
	// the hot path (Send, building the turn's list) holds it only to copy the
	// slice, and never while talking to the provider.
	muExtras sync.Mutex
	injected []tools.Spec
}

// NewSession creates an empty session over the given configuration.
func NewSession(cfg config.Config) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	return s
}

// NewSessionWithStore creates an ALREADY persistent session; the id only
// enters the log with the first event. It is the REPL path, which knows up
// front where the database is.
func NewSessionWithStore(cfg config.Config, st *store.Store, sessionID string) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	s.Attach(st, sessionID)
	return s
}

// Attach binds the session to an existing store. It may be called after
// NewSession (when the database only opens later, e.g. a best-effort failure
// in the REPL); events of the next Send start being recorded.
func (s *Session) Attach(st *store.Store, sessionID string) {
	if st == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	s.store = st
	s.sessionID = sessionID
	// No indexing here: the session row is born with the first event
	// (StoreEvent) or with /rename. Opening and closing bh without talking
	// leaves no empty session in the database nor in the /resume list.
}

// ID returns the session id (empty when there is no persistence).
func (s *Session) ID() string { return s.sessionID }

// Persistente reports whether a log is recording (used by the CLI to show `sess`).
func (s *Session) Persistente() bool { return s.store != nil && s.sessionID != "" }

func (s *Session) Config() config.Config { return s.config }

// Configure updates provider/model/effort, clearing pending tool state (the
// full transcript only keeps committed turns; text stays compatible).
func (s *Session) Configure(cfg config.Config) {
	if s.config.Model != cfg.Model || s.config.CurrentProvider != cfg.CurrentProvider {
		// There are no pending tool calls thanks to Send's transaction; committed
		// textual history is valid across models and providers.
	}
	s.config = cfg
	s.rebuild()
}

func (s *Session) rebuild() {
	p := s.config.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	id := dealer.NormalizarURL(base) + "\x00" + s.config.Model
	if id != s.key {
		s.est = budget.Estimador{}
		if s.config.BytesPorToken > 0 {
			s.est.CharsPorToken = float64(s.config.BytesPorToken)
		}
		s.key = id
	}
	s.dealer = dealer.Novo(dealer.Config{
		BaseURL: base, APIKey: key, Model: s.config.Model,
		MaxTokens: s.config.MaxTokens, JanelaTokens: s.config.Janela,
		Effort: s.config.Effort,
	})
}

// History returns a copy for tests/safe observability; it does not expose the
// internal mutable slice.
func (s *Session) History() []dealer.Mensagem { return append([]dealer.Mensagem(nil), s.history...) }

// UltimoContexto returns the prompt tokens of the last request recorded in the
// transcript (0 if none): the context size of a resumed session, before any
// new turn.
func (s *Session) UltimoContexto() int {
	for i := len(s.history) - 1; i >= 0; i-- {
		if u := s.history[i].Usage; u != nil && u.PromptTokens > 0 {
			return u.PromptTokens
		}
	}
	return 0
}

// ClearHistory clears the transcript. Used only when it makes sense to start a
// new conversation explicitly; it is not called when switching models.
//
// With persistence, the break also enters the log (session_reset): the log is
// append-only and cannot "undo" earlier events, but it must record that the
// conversation was cut — otherwise a Resume would rebuild a transcript the
// user thinks they discarded.
func (s *Session) ClearHistory() {
	s.history = nil
	s.record("session_reset", map[string]any{"role": "system", "content": "history cleared; new conversation"})
}

// Send runs a turn and keeps the transcript between calls. The user message
// and the rounds are appended to a private buffer and only committed on full
// success; YOLO tool execution already performed cannot be reverted, but the
// provider history is never left malformed.
func (s *Session) Send(ctx context.Context, r Renderer, cwd, prompt string) error {
	if r == nil {
		r = RendererDe(&strings.Builder{})
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("empty prompt")
	}
	if !s.config.Pronta() {
		return fmt.Errorf("incomplete configuration (%s); run `bh` with no arguments to set it up", strings.Join(s.config.Faltas(), ", "))
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
	}
	start := time.Now()
	baroMD, err := instructions.Resolve(cwd)
	if err != nil {
		return err
	}
	working := append([]dealer.Mensagem(nil), s.history...)
	working = append(working, dealer.Mensagem{Role: "user", Conteudo: prompt})
	floor := 0
	// Consecutive cuts at the output limit, and the limit of the retry when the
	// cut came back empty (the model spent everything thinking).
	cuts, extraLimit := 0, 0
	reactiveDone := false
	compacted := false
	// Whole-turn totals: each tool cycle spends request and response, and the
	// UI footer shows the total, not the last cycle's.
	promptUsage, outputUsage := 0, 0

	// The user prompt enters the log BEFORE the loop: if the turn fails midway,
	// the log still records what was asked.
	s.record("user_message", map[string]any{"role": "user", "content": prompt})

	for cycle := 0; cycle < 64; cycle++ {
		// Catalog rebuilt on EVERY cycle: mcp_add, mcp_remove and skill_create
		// change the tools mid-turn, and the next round must already see them.
		// Built only once, a freshly connected tool stayed invisible until the
		// next prompt — the model saw the server in mcp_list and could not call
		// its tool.
		list := s.toolCatalog(cwd)
		schemas := toolSchemas(list)
		system := "You are the Barroso Harness agent, running locally on the user's machine in YOLO mode: tools execute without asking for approval.\n" +
			"Use tools to act (terminal, files) and TEXT to talk to the user. When the task is done, reply with plain text.\n"
		if baroMD != "" {
			system += "\n## BARROSO.md (system instructions)\n" + baroMD
		}
		system += "\n## Environment\n- cwd: " + cwd + "\n- platform: use tools to find out; do not assume a shell.\n"

		body, err := s.dealer.Corpo(system, working, schemas)
		if err != nil {
			return err
		}
		ev := budget.Avaliar(string(body), s.config.Janela, s.est)
		if !ev.Cabe {
			pruned := prune(working, &floor, ev.TokensEntrada+ev.TokensSaida-s.config.Janela)
			if pruned == nil {
				return fmt.Errorf("guardrail: request does not fit (%s) and pruning did not free enough", ev.Veredito())
			}
			r.Guardrail(true)
			// Persisted kind/keys/values stay in their original (Portuguese)
			// form: sessions.db readers (cli context stats) match on them.
			s.record("guardrail", map[string]any{
				"tipo":       "proativa",
				"politica":   policy(s.config.PoliticaContexto),
				"veredito":   ev.Veredito(),
				"janela":     s.config.Janela,
				"estimativa": ev.TokensEntrada + ev.TokensSaida,
			})
			compacted = true
			working = pruned
			continue
		}

		if rd, ok := r.(RendererDetalhado); ok {
			rd.Pensando()
		}
		cli := s.dealer
		limit := cli.MaxTokens()
		if extraLimit > 0 {
			limit = extraLimit
		}
		if s.config.Janela > 0 {
			// The limit cannot exceed what is left in the window (as OpenCode and
			// pi do), with a floor so the model can still answer.
			limit = max(1024, min(limit, s.config.Janela-ev.TokensEntrada))
		}
		if limit != cli.MaxTokens() {
			cli = cli.ComTeto(limit)
		}
		round, err := cli.RodarContext(ctx, system, working, schemas)
		if err != nil {
			if contextOverflow(err) && s.config.Janela > 0 && !reactiveDone {
				reactiveDone = true
				r.Guardrail(false)
				s.record("guardrail", map[string]any{
					"tipo":     "reativa",
					"politica": policy(s.config.PoliticaContexto),
					"motivo":   oneLine(err.Error()),
				})
				pruned := prune(working, &floor, budget.ReservaOperacional)
				if pruned != nil {
					compacted = true
					working = pruned
					continue
				}
			}
			return err
		}
		if rr, ok := r.(RendererRaciocinio); ok && strings.TrimSpace(round.Raciocinio) != "" {
			rr.Pensou(round.Raciocinio, round.DurRaciocinio)
		}
		if round.Usage != nil {
			if rd, ok := r.(RendererDetalhado); ok {
				rd.Contexto(round.Usage.PromptTokens)
			}
			s.est.Calibrar(len(body), round.Usage.PromptTokens)
			if round.Usage.PromptTokens > 0 {
				promptUsage += round.Usage.PromptTokens
			}
			outputUsage += round.Usage.OutputTokens
		}
		if round.Cortada {
			// A response cut at the output limit is never a final answer: no
			// harness studied (OpenCode, Codex, Claude Code, Gemini CLI, dsh, pi,
			// Crush, Aider) silently ends the turn in that case.
			cuts++
			rc, _ := r.(RendererCorte)
			if cuts > maxCuts {
				if rc != nil {
					rc.Cortada(limit, true)
				}
				r.Tokens(promptUsage, outputUsage, time.Since(start), compacted)
				return fmt.Errorf("response cut at the output limit (%d tokens) %d times in a row; send \"continue\" to resume", limit, cuts)
			}
			if rc != nil {
				rc.Cortada(limit, false)
			}
			if strings.TrimSpace(round.Texto) == "" && len(round.Tools) == 0 {
				// Only thought: retry with twice the limit.
				extraLimit = limit * 2
				continue
			}
			// Half-written text (or tool call): the text stays, the tools do not
			// run (their arguments may be truncated) and the model is told to
			// continue in smaller pieces.
			if round.Texto != "" {
				r.Resposta(round.Texto)
				working = append(working, dealer.Mensagem{Role: "assistant", Conteudo: round.Texto, Usage: round.Usage})
				s.record("assistant", map[string]any{"role": "assistant", "content": round.Texto})
			}
			notice := cutNotice
			if len(round.Tools) > 0 {
				notice += " " + cutNoticeTools
			}
			working = append(working, dealer.Mensagem{Role: "user", Conteudo: notice})
			s.record("user_message", map[string]any{"role": "user", "content": notice})
			continue
		}
		cuts, extraLimit = 0, 0
		if len(round.Tools) == 0 {
			r.Resposta(round.Texto)
			r.Tokens(promptUsage, outputUsage, time.Since(start), compacted)
			working = append(working, dealer.Mensagem{Role: "assistant", Conteudo: round.Texto, Usage: round.Usage})
			s.history = working
			// Committed text enters the log AFTER the transcript commit: a failed
			// round leaves no trace of a response that never existed.
			assistant := map[string]any{"role": "assistant", "content": round.Texto}
			if round.Usage != nil {
				assistant["usage"] = map[string]any{
					"prompt_tokens": round.Usage.PromptTokens,
					"output_tokens": round.Usage.OutputTokens,
				}
			}
			s.record("assistant", assistant)
			return nil
		}

		// Build the assistant tool-call + results in a separate transaction; only
		// add them to working when every execution has finished.
		turn := make([]dealer.Mensagem, 0, len(round.Tools)+1)
		if round.Texto != "" {
			turn = append(turn, dealer.Mensagem{Role: "assistant", Conteudo: round.Texto, Usage: round.Usage})
		}
		results := make([]dealer.Mensagem, 0, len(round.Tools))
		for _, call := range round.Tools {
			turn = append(turn, dealer.Mensagem{Role: "tool_call", ToolID: call.ID, ToolName: call.Nome, ToolArgs: call.Args})
			// The call enters the log here, before executing: if execution hangs
			// or blows up, the log still shows what was requested and with which
			// arguments.
			s.record("tool_call", map[string]any{
				"role":      "tool_call",
				"tool_id":   call.ID,
				"tool_name": call.Nome,
				"tool_args": call.Args,
			})
		}
		for _, call := range round.Tools {
			if rd, ok := r.(RendererDetalhado); ok {
				rd.ToolInicio(call.Nome, call.Args)
			}
			toolStart := time.Now()
			output, execErr := runTool(ctx, list, cwd, call)
			dur := time.Since(toolStart)
			isErr := execErr != nil
			if isErr {
				output = "error: " + execErr.Error()
			}
			results = append(results, dealer.Mensagem{Role: "tool_result", Conteudo: output, ToolID: call.ID, ToolName: call.Nome, IsErro: isErr})
			// Outputs above blobThreshold become blob + preview.
			s.recordResult(call.ID, call.Nome, output, isErr)
			r.Tool(call.Nome, dur, isErr)
			r.Eco(echoExcerpt(output))
		}
		working = append(working, turn...)
		working = append(working, results...)
		if state := compactedState(round.Tools, results); state != "" {
			before := historyBytes(working)
			working = compactHistory(working, state)
			floor = 0 // the history changed shape; pruning restarts from the beginning
			compacted = true
			r.Guardrail(true)
			// Kind and keys kept as originally persisted (see persist.go).
			s.record(eventCompaction, map[string]any{
				"politica":     PoliticaAgente,
				"ciclo":        cycle,
				"bytes_antes":  before,
				"bytes_depois": historyBytes(working),
				"estado_bytes": len(state),
				"estado":       state,
			})
		}
	}
	return fmt.Errorf("turn exceeded 64 cycles; configurable technical limit (not a permission check)")
}

func toolSchemas(list []tools.Spec) []map[string]any {
	schemas := make([]map[string]any, 0, len(list))
	for _, s := range list {
		schemas = append(schemas, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": s.Name, "description": s.Description, "parameters": s.Schema},
		})
	}
	return schemas
}

func runTool(ctx context.Context, list []tools.Spec, cwd string, call dealer.ToolChamada) (string, error) {
	for _, s := range list {
		if s.Name == call.Nome {
			return s.Exec(ctx, cwd, json.RawMessage(call.Args))
		}
	}
	return "", fmt.Errorf("unknown tool: %q (check the exact name)", call.Nome)
}

// maxCuts: how many consecutive cuts at the output limit the turn tolerates
// before stopping and reporting (OpenCode uses 3).
const maxCuts = 3

// Synthetic continuation messages after a cut. The "[output limit]" prefix
// lets the screen recognize them and not draw them as user speech.
const (
	cutNotice      = "[output limit] Your last response hit the output token limit and was cut off. Continue exactly where you stopped, without repeating what you already wrote, and break the remaining work into smaller pieces."
	cutNoticeTools = "Tool calls in the cut response were NOT executed because their arguments may be incomplete; call them again, with smaller arguments if needed."
)
