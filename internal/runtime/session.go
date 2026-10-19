// Package runtime implements a reusable conversation session. The transcript
// only commits a round after a valid response and, for tool_calls, after all
// results — errors/cancellations never leave orphaned calls.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
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
	return s.SendWithImages(ctx, r, cwd, prompt, nil)
}

// SendWithImages is Send with image files attached to the user message (the
// full screen's pasted [Image N]). The paths go into the transcript and the
// log; the dealer inlines the bytes on each request, so the model must
// support vision.
func (s *Session) SendWithImages(ctx context.Context, r Renderer, cwd, prompt string, images []string) error {
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
	working = append(working, dealer.Mensagem{Role: "user", Conteudo: prompt, Images: images})
	floor := 0
	// Consecutive cuts at the output limit, and the limit of the retry when the
	// cut came back empty (the model spent everything thinking).
	extraLimit := 0
	reactiveDone := false
	compacted := false
	// Whole-turn totals: each tool cycle spends request and response, and the
	// UI footer shows the total, not the last cycle's.
	promptUsage, outputUsage := 0, 0

	// The user prompt enters the log BEFORE the loop: if the turn fails midway,
	// the log still records what was asked.
	userEvent := map[string]any{"role": "user", "content": prompt}
	if len(images) > 0 {
		userEvent["images"] = images
	}
	s.record("user_message", userEvent)

	// No cycle limit: the turn runs until the model answers with text or the
	// user interrupts it (Esc / Ctrl+C cancels ctx).
	for cycle := 0; ; cycle++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Catalog rebuilt on EVERY cycle: mcp_add, mcp_remove and skill_create
		// change the tools mid-turn, and the next round must already see them.
		// Built only once, a freshly connected tool stayed invisible until the
		// next prompt — the model saw the server in mcp_list and could not call
		// its tool.
		list := s.toolCatalog(cwd)
		schemas := toolSchemas(list)
		system := systemPrompt(cwd, baroMD, hasTool(list, "task_spawn"), time.Now())

		// Measured without the image bytes: see dealer.CorpoEstimativa.
		body, err := s.dealer.CorpoEstimativa(system, working, schemas)
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
			// No give-up count: the turn keeps continuing until the model
			// finishes or the user interrupts.
			rc, _ := r.(RendererCorte)
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
		extraLimit = 0
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
		for _, out := range s.runRound(ctx, r, list, cwd, round.Tools) {
			results = append(results, dealer.Mensagem{Role: "tool_result", Conteudo: out.output, ToolID: out.call.ID, ToolName: out.call.Nome, IsErro: out.isErr})
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

// toolOutcome is one executed call of a round.
type toolOutcome struct {
	call   dealer.ToolChamada
	output string
	isErr  bool
	dur    time.Duration
}

// sequentialTools mutate the catalog or the session itself; a round that
// contains any of them runs one call at a time, in order.
var sequentialTools = map[string]bool{
	"mcp_add":      true,
	"mcp_remove":   true,
	"skill_create": true,
	"skill_remove": true,
	"ctx_compact":  true,
	"ctx_write":    true,
	"ctx_append":   true,
	"task_spawn":   true,
}

// runRound executes the round's tool calls and returns their outcomes in the
// ORIGINAL order. Independent calls run concurrently; the renderer and the log
// (recordResult) are only touched from this goroutine, in call order: every
// ToolInicio is announced up front, and each call's recordResult/Tool/Eco is
// emitted as soon as it and all earlier calls are done. Cancellation reaches
// the tools through ctx; waiting for every goroutine keeps the transcript free
// of calls without results.
func (s *Session) runRound(ctx context.Context, r Renderer, list []tools.Spec, cwd string, calls []dealer.ToolChamada) []toolOutcome {
	outcomes := make([]toolOutcome, len(calls))
	run := func(i int) {
		call := calls[i]
		start := time.Now()
		output, execErr := runTool(ctx, list, cwd, call)
		o := toolOutcome{call: call, output: output, dur: time.Since(start)}
		if execErr != nil {
			o.isErr = true
			o.output = "error: " + execErr.Error()
		}
		outcomes[i] = o
	}
	report := func(i int) {
		o := outcomes[i]
		// Outputs above blobThreshold become blob + preview.
		s.recordResult(o.call.ID, o.call.Nome, o.output, o.isErr)
		r.Tool(o.call.Nome, o.dur, o.isErr)
		r.Eco(echoExcerpt(o.output))
	}
	rd, detailed := r.(RendererDetalhado)

	parallel := len(calls) > 1
	for _, c := range calls {
		if sequentialTools[c.Nome] {
			parallel = false
			break
		}
	}
	if !parallel {
		for i, c := range calls {
			if detailed {
				rd.ToolInicio(c.Nome, c.Args)
			}
			run(i)
			report(i)
		}
		return outcomes
	}

	done := make([]chan struct{}, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		if detailed {
			rd.ToolInicio(c.Nome, c.Args)
		}
		done[i] = make(chan struct{})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer close(done[i])
			run(i)
		}(i)
	}
	for i := range calls {
		<-done[i]
		report(i)
	}
	wg.Wait()
	return outcomes
}

func hasTool(list []tools.Spec, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

// systemPrompt builds the turn's system message: identity, real environment
// facts, tool policy and working style, then BARROSO.md.
func systemPrompt(cwd, baroMD string, canSpawn bool, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("You are the Barroso Harness agent, a coding agent running locally on the user's machine. " +
		"Your tools execute immediately, without asking for approval (YOLO mode): act decisively, but think before destructive actions. " +
		"You help with software engineering tasks: reading and changing code, running builds and tests, using git, investigating problems.\n")

	sb.WriteString("\n## Environment\n")
	osName := goruntime.GOOS
	switch osName {
	case "windows":
		osName = "Windows"
	case "darwin":
		osName = "macOS"
	case "linux":
		osName = "Linux"
	}
	fmt.Fprintf(&sb, "- OS: %s (%s/%s)\n", osName, goruntime.GOOS, goruntime.GOARCH)
	fmt.Fprintf(&sb, "- Shell used by terminal_run: %s\n", tools.ShellName())
	fmt.Fprintf(&sb, "- Working directory (cwd): %s\n", cwd)
	fmt.Fprintf(&sb, "- Today's date: %s\n", now.Format("2006-01-02"))
	if goruntime.GOOS == "windows" {
		sb.WriteString("- This is Windows: do not use bash/POSIX syntax or tools (grep, sed, ls, cat, rm -rf) in terminal_run. " +
			"File tools accept both backslashes and forward slashes in paths.\n")
	}

	sb.WriteString("\n## Tool policy\n" +
		"- Find things with grep (file contents) and glob (file names). Do not search through terminal_run (findstr, git grep, dir /s, find).\n" +
		"- Read files with file_read; read whole files when reasonably sized rather than many small windows.\n" +
		"- Change existing files with edit (exact string replacement of only the changed region). Use file_write only for new files or full rewrites.\n" +
		"- Use terminal_run for builds, tests, git, package managers and running programs.\n" +
		"- PARALLEL CALLS: when several tool calls are independent (reading several files, several searches, a build plus a git status), " +
		"ISSUE THEM ALL IN THE SAME RESPONSE: they run in parallel. Only chain calls across responses when one needs the result of another.\n" +
		"- If a call fails, read the error and adapt. Never repeat a failing command unchanged.\n" +
		"- Avoid exploratory loops: decide what you need, gather it in as few rounds as possible, then act.\n")
	if canSpawn {
		sb.WriteString("- task_spawn is available: use it for independent work that can proceed in parallel, at most once per turn, and end your turn right after spawning.\n")
	}

	sb.WriteString("\n## Working style\n" +
		"- Be direct. Do the task end to end instead of describing what you would do; ask the user only when truly blocked or when the request is ambiguous in a way that matters.\n" +
		"- Follow the conventions of the existing code (style, libraries, structure) and keep changes focused on the request.\n" +
		"- After changing code, verify it: build and run the relevant tests, and fix what you broke.\n" +
		"- Use tools to act and TEXT to talk to the user. When the task is done, reply with a short plain-text answer: what changed and what the user must know. No long recaps.\n")

	if baroMD != "" {
		sb.WriteString("\n## BARROSO.md (system instructions)\n" + baroMD)
	}
	return sb.String()
}

func runTool(ctx context.Context, list []tools.Spec, cwd string, call dealer.ToolChamada) (string, error) {
	for _, s := range list {
		if s.Name == call.Nome {
			return s.Exec(ctx, cwd, json.RawMessage(call.Args))
		}
	}
	return "", fmt.Errorf("unknown tool: %q (check the exact name)", call.Nome)
}

// Synthetic continuation messages after a cut. The "[output limit]" prefix
// lets the screen recognize them and not draw them as user speech.
const (
	cutNotice      = "[output limit] Your last response hit the output token limit and was cut off. Continue exactly where you stopped, without repeating what you already wrote, and break the remaining work into smaller pieces."
	cutNoticeTools = "Tool calls in the cut response were NOT executed because their arguments may be incomplete; call them again, with smaller arguments if needed."
)
