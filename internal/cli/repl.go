// Package cli holds the interactive mode: provider/model onboarding and the
// REPL with /model, /effort, /provider, /help and /exit. Key input is hidden
// (x/term); tests inject reader/writer and fake keys.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
	"golang.org/x/term"
)

// Esforcos are the common reasoning_effort levels. The provider may accept
// others; the real validation is its HTTP API, and the error reaches the user
// with the option to clear it via `/effort -` (no silent downgrade).
var Esforcos = []string{"low", "medium", "high", "xhigh", "max"}

// IO bundles the three streams so tests can inject reader/writer. The shared
// Reader avoids dropping bytes when the input is prebuffered.
type IO struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
	reader *bufio.Reader

	// screen != nil when the REPL runs behind the full screen (screen.go): Out
	// and ErrOut become conversation lines and cancelCh receives the turn's Esc/Ctrl+C.
	screen   *screen
	cancelCh chan struct{}
	// inject puts a line into the REPL input as if typed (full screen only):
	// it is how the finished sub-agent notice wakes a REPL that is waiting
	// for the user.
	inject func(string)
}

// subagentsLine is the synthetic line telling the REPL that sub-agents
// finished. It starts with a control character so it never collides with
// typed text.
const subagentsLine = "\x1esubagents"

func (io *IO) Reader() *bufio.Reader {
	if io.reader == nil {
		io.reader = bufio.NewReader(io.In)
	}
	return io.reader
}

// PromptKey reads the key WITHOUT echo (x/term on raw stdin). Tests override it.
var PromptKey = func(prompt string) (string, error) {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// fetchModels is the catalog's network seam: production calls dealer.Modelos,
// tests replace it to stay off the network. Separate from PromptKey because
// this replacement needs the URL and the key.
var fetchModels = func(ctx context.Context, base, key string) ([]dealer.CatalogModel, error) {
	return dealer.Modelos(ctx, base, key)
}

// RunInteractive is the path for `bh` with no arguments: onboarding if the
// config is incomplete, then the REPL.
func RunInteractive(io IO) int {
	if io.screen == nil && useScreen(io) {
		if c := runScreen(io); c >= 0 {
			return c
		}
	}
	if err := run(io); err != nil {
		fmt.Fprintf(io.ErrOut, "error: %v\n", err)
		return 1
	}
	return 0
}

func run(io IO) error {
	io.Reader() // initialize the buffer BEFORE any read: the IO copies
	// passed to Onboarding/repl must share the same *bufio.Reader,
	// otherwise the second reader sees EOF and the REPL gets no lines.
	if io.screen != nil {
		colorsOn = true // Out is the screen writer, which is a real terminal
	} else {
		enableColors(io)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Pronta() {
		fmt.Fprintln(io.Out, "Barroso Harness — first-time setup (provider, key and model; the key stays on this computer).")
		if err := Onboarding(io, &cfg); err != nil {
			if err == errCanceled || strings.Contains(strings.ToLower(err.Error()), "canceled") {
				fmt.Fprintln(io.Out, "onboarding canceled; nothing was changed or saved.")
				return nil
			}
			return err
		}
	} else if io.screen != nil {
		// the full screen already has title, panel and footer
	} else if colorsOn {
		headerTUI(io.Out, Versao)
	} else {
		banner(io.Out)
	}
	repl(io, &cfg)
	return nil
}

var errCanceled = fmt.Errorf("canceled")

// errUnchanged: model selection ended without a change (empty Enter).
var errUnchanged = fmt.Errorf("model unchanged")

// ── onboarding ───────────────────────────────────────────────────────────

// Onboarding walks through: base URL (default keys.barroso) → hidden key →
// validated /models → model choice. Reasoning effort starts off.
// Persists only on success; Ctrl+D/cancel returns errCanceled.
func Onboarding(io IO, cfg *config.Config) error {
	if io.reader == nil {
		io.reader = bufio.NewReader(io.In)
	}
	base := config.DefaultBaseURL
	if p := cfg.Ativo(); p != nil && p.BaseURL != "" {
		base = p.BaseURL
	}
	if v := ask(io, "base URL", base); strings.TrimSpace(v) != "" && strings.TrimSpace(v) != base {
		base = strings.TrimSpace(v)
	}

	key := ""
	if p := cfg.Ativo(); p != nil && p.APIKey != "" {
		key = p.APIKey
		fmt.Fprintln(io.Out, "current key kept; press enter to keep it or type a new one (hidden input).")
	}
	if newKey, err := PromptKey("api key: "); err == nil && strings.TrimSpace(newKey) != "" {
		key = strings.TrimSpace(newKey)
	}
	if key == "" {
		return fmt.Errorf("an API key is required to query /models and authenticate turns")
	}

	if err := configureProvider(io, cfg, base, key); err != nil {
		if _, ok := err.(*ForaDoCatalogo); !ok {
			return err
		}
	}
	if cfg.Model == "" {
		if err := selectModel(io, cfg); err != nil {
			return err
		}
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintln(io.Out, "saved to "+config.Dir()+"\\config.json — effort is off for now (/effort turns it on).")
	fmt.Fprintln(io.Out, cfg.Resumo())
	return nil
}

// ask reads one line of text input with a default suggestion.
func ask(io IO, fieldLabel, def string) string {
	fmt.Fprintf(io.Out, "%s [%s]: ", fieldLabel, def)
	line, _ := io.Reader().ReadString('\n')
	return strings.TrimSpace(line)
}

// configureProvider validates base+key AGAINST /models BEFORE touching memory
// or disk; on failure it returns an error and nothing changed (full rollback).
// When the stored model is not in the new catalog, the draft keeps the
// provider applied with Model="" and returns *ForaDoCatalogo (caller decides to persist).
func configureProvider(io IO, cfg *config.Config, base, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := fetchModels(ctx, base, key)
	if err != nil {
		return fmt.Errorf("provider rejected %s: %v (rolled back — nothing was saved)", base, err)
	}
	draft := *cfg
	draft = updateProvider(draft, providerName(base), base, key)

	notIn := ForaDoCatalogo{}
	if draft.Model != "" {
		if !inCatalog(draft.Model, ls) {
			notIn.Modelo = draft.Model
			draft.Model = "" // the new provider does not have the stored model
		}
	}
	if draft.Model != "" {
		// Model exists in the new catalog: the window follows what is stored
		// (manual source) or is re-derived from the catalog when derived.
		if draft.JanelaSource != config.FonteManual {
			if j := windowOf(draft.Model, ls); j > 0 {
				draft = applyWindow(draft, j)
			} else {
				draft = clearCatalogWindow(draft)
			}
		}
	}
	*cfg = draft
	if notIn.Modelo != "" || draft.Model == "" {
		return &notIn
	}
	return nil
}

type ForaDoCatalogo struct{ Modelo string }

func (e *ForaDoCatalogo) Error() string {
	return "model " + e.Modelo + " is not in this provider's catalog; pick another (/model)"
}

func providerName(base string) string {
	t := strings.TrimSuffix(strings.TrimSpace(base), "/")
	t = strings.TrimPrefix(strings.TrimPrefix(t, "https://"), "http://")
	if i := strings.IndexAny(t, "/:"); i >= 0 {
		t = t[:i]
	}
	return strings.NewReplacer(".", "-").Replace(t)
}

func inCatalog(id string, ls []dealer.CatalogModel) bool {
	if id == "" {
		return false
	}
	for _, m := range ls {
		if m.ID == id {
			return true
		}
	}
	return false
}

// applyWindow honours a manual override: if the user wrote
// context_window by hand, the catalog does not overwrite it.
func applyWindow(cfg config.Config, window int) config.Config {
	if cfg.JanelaSource == config.FonteManual && cfg.Janela > 0 {
		return cfg
	}
	cfg.Janela = window
	cfg.JanelaSource = config.FonteCatalogo
	return cfg
}

// clearCatalogWindow clears only the catalog-derived value; manual stays.
func clearCatalogWindow(cfg config.Config) config.Config {
	if cfg.JanelaSource == config.FonteManual && cfg.Janela > 0 {
		return cfg
	}
	cfg.Janela = 0
	cfg.JanelaSource = ""
	return cfg
}

func updateProvider(cfg config.Config, name, base, key string) config.Config {
	if cfg.Providers == nil {
		cfg.Providers = map[string]*config.Provider{}
	}
	cfg.Providers[name] = &config.Provider{BaseURL: base, APIKey: key}
	cfg.CurrentProvider = name
	cfg.BaseURL, cfg.APIKey = base, key
	return cfg
}

// windowOf returns the published context_length for the id (0 = unknown).
func windowOf(id string, ls []dealer.CatalogModel) int {
	for _, m := range ls {
		if m.ID == id {
			return m.ContextLength
		}
	}
	return 0
}

// ── model selection ──────────────────────────────────────────────────────

// selectModel lists the filtered (text-capable) catalog, alphabetically,
// numbered; accepts an index or an ID. Updates the derived window.
func selectModel(io IO, cfg *config.Config) error {
	p := cfg.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := fetchModels(ctx, base, key)
	if err != nil {
		return err
	}
	if len(ls) == 0 {
		return fmt.Errorf("catalog has no usable chat models")
	}
	listModels(io.Out, ls, cfg.Model)
	for {
		fmt.Fprint(io.Out, cyanf("select model > "))
		line, err := io.Reader().ReadString('\n')
		if err != nil {
			return errCanceled
		}
		v := strings.TrimSpace(line)
		if v == "" {
			// Empty Enter with a valid catalog model = exit without switching.
			// Without a model (onboarding, a new provider lacking the current one)
			// there is nothing to keep, so a choice is still required.
			if cfg.Model != "" && inCatalog(cfg.Model, ls) {
				return errUnchanged
			}
			fmt.Fprintln(io.Out, "pick an item (number or id) — no model, no turn.")
			continue
		}
		id := ""
		if n, errAtoi := strconv.Atoi(v); errAtoi == nil && n >= 1 && n <= len(ls) {
			id = ls[n-1].ID
		} else if inCatalog(v, ls) {
			id = v
		}
		if id == "" {
			fmt.Fprintf(io.Out, "%q is not in the catalog; try again.\n", v)
			continue
		}
		cfg.Model = id
		if j := windowOf(id, ls); j > 0 {
			*cfg = applyWindow(*cfg, j)
		} else {
			*cfg = clearCatalogWindow(*cfg)
		}
		return nil
	}
}

// ── REPL ─────────────────────────────────────────────────────────────────

// turnContext: the first Ctrl+C during a turn cancels the round, a second one
// ends the REPL (by closing quitCh).
func (io IO) turnContext(quitCh chan<- struct{}) (context.Context, context.CancelFunc) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	if io.cancelCh != nil {
		// Full screen: raw mode raises no signal; Esc/Ctrl+C come through here.
		// A request left over from a finished turn must not kill this one
		// (e.g. Enter right as the previous turn was ending).
		select {
		case <-io.cancelCh:
		default:
		}
		go func() {
			select {
			case <-io.cancelCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	go func() {
		<-sigs
		fmt.Fprintln(io.Out)
		warnFmt(io.Out, "canceling the turn — press again to exit")
		cancel()
		select {
		case <-sigs:
			close(quitCh)
		case <-ctx.Done():
		}
		signal.Stop(sigs)
	}()
	return ctx, cancel
}

func repl(io IO, cfg *config.Config) {
	// Best-effort persistence: without a database the REPL keeps going in memory
	// (the warning goes out once, on ErrOut, and never interrupts the conversation).
	db, activeID := openStore(io)
	var sess *runtime.Session
	if db != nil {
		sess = runtime.NewSessionWithStore(*cfg, db, activeID)
	} else {
		sess = runtime.NewSession(*cfg)
	}
	defer func() {
		if db != nil {
			_ = db.TouchSession(activeID)
			_ = db.Fechar()
		}
	}()
	br := io.Reader()
	cwd, _ := os.Getwd()
	quitCh := make(chan struct{})
	exiting := false
	lastContext := 0 // prompt tokens of the last request, for the context %

	// ── process-owned subsystems (MCP, sub-agents) ─────────────────────────
	// This ctx lives for the WHOLE PROCESS: sub-agents must outlive the turn
	// that created them, and the MCP handshake must not die with a round's
	// Ctrl+C. Best-effort — a failure of ONE server becomes a warning, not a
	// REPL crash; /model switching provider re-injects on the next line.
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	wired, wiringWarnings := wiring.Montar(processCtx, *cfg, sess, cwd)
	for _, notice := range wiringWarnings {
		warnFmt(io.Out, notice)
	}
	// Close on exit: the set's Fechar() stops MCP subprocesses; the sub-agent
	// manager is in-memory and dies with the process.
	defer func() { _ = wired.Fechar() }()
	if g := wired.Gerente(); g != nil && io.inject != nil {
		g.AoTerminar(func() { io.inject(subagentsLine) })
	}
	// The full screen's dialogs read the catalog, sessions and sub-agents
	// through this handle; it is refreshed before every prompt.
	var src *dialogSource
	if io.screen != nil {
		src = &dialogSource{db: db, gerente: wired.Gerente(), cwd: cwd}
		io.screen.bindSource(src)
	}

	for {
		// Exits requested by a turn (Ctrl+C twice) happen BEFORE drawing a new
		// prompt; EOF closes mid-line and asks for the line break.
		select {
		case <-quitCh:
			farewell(io.Out)
			return
		default:
		}
		if exiting {
			farewell(io.Out)
			return
		}
		if src != nil {
			src.update(*cfg, sess.ID())
		}
		if io.screen != nil {
			var mcps []mcpEntry
			for _, c := range wired.MCP() {
				mcps = append(mcps, mcpEntry{name: c.Fonte(), falha: c.Falhou()})
			}
			io.screen.Estado(panelState{
				title: currentTitle(db, sess), model: cfg.Model,
				provider: cfg.CurrentProvider, effort: cfg.Effort, cwd: shortCwd(cwd),
				ctxTokens: lastContext, window: cfg.Janela, mcps: mcps, branch: gitBranch(cwd),
			})
		} else if colorsOn {
			inputStatus(io.Out, cfg, sess.ID(), len(wired.MCP()), lastContext)
			printPromptTUI(io.Out)
		} else {
			sessionStatusbar(io.Out, cfg, sess.ID())
			printPrompt(io.Out, cfg.Model, cfg.Effort)
		}
		line, err := br.ReadString('\n')
		if err != nil { // EOF / Ctrl+D: exit cleanly
			fmt.Fprintln(io.Out)
			if db != nil {
				_ = db.TouchSession(sess.ID())
			}
			farewell(io.Out)
			return
		}
		// A multiline prompt from the full screen crosses the pipe on one line.
		line = decodeLine(line)
		// Sub-agents finished: the result becomes a model turn, without the
		// user having to ask.
		subNotice := ""
		if strings.TrimSpace(line) == subagentsLine {
			ts := wired.Gerente().Terminadas()
			if len(ts) == 0 {
				continue // already delivered (task_result or an earlier notice)
			}
			subNotice = subagent.AvisoTerminadas(ts, wired.Gerente().Ativas())
			line = subNotice
		}
		parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
		fields := strings.Fields(parts[0])
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}
		if len(fields) == 0 {
			continue
		}
		if subNotice != "" {
			fields = []string{""} // goes to the model turn, never to a command
		}
		switch fields[0] {
		case "/exit", "/quit":
			if db != nil {
				_ = db.TouchSession(sess.ID())
			}
			farewell(io.Out)
			return
		case "/new":
			// /new: clear the session transcript without switching provider (the
			// config stays as is). Useful after finishing something, so the context
			// is not carried into the next request.
			sess.ClearHistory()
			lastContext = 0
			if io.screen != nil {
				io.screen.Limpar()
			}
			okFmt(io.Out, "history cleared; new conversation")
		case "/resume":
			if db == nil {
				warnFmt(io.Out, "session not persisted: nothing to resume")
				continue
			}
			// "/resume <id>" (the sessions dialog) resumes directly; bare
			// "/resume" lists and asks.
			list := loadSessionRows(db, 100)
			choice := arg
			if choice == "" {
				list = list[:min(10, len(list))]
				if len(list) == 0 {
					printSessionList(io.Out, nil)
					continue
				}
				printSessionList(io.Out, list)
				fmt.Fprint(io.Out, greenf("❯")+" "+dimf("number or id (enter cancels)")+" ")
				var errRead error
				choice, errRead = br.ReadString('\n')
				if errRead != nil { // EOF midway: exit cleanly, as in the loop
					fmt.Fprintln(io.Out)
					_ = db.TouchSession(sess.ID())
					farewell(io.Out)
					return
				}
			}
			chosen := resolveSession(choice, list)
			if chosen == "" {
				warnFmt(io.Out, "empty or invalid choice; keeping the current session")
				continue
			}
			// Resume = new active id + transcript rebuilt from the log, in the
			// SAME session: tools injected by wiring (task_spawn, MCP) stay in the
			// session they were injected into. Swapping in a new session left the
			// resumed one without sub-agents and without MCP.
			activeID = chosen
			sess.Attach(db, activeID)
			_ = sess.ContextoVivoCarregar()
			if err := sess.Resume(); err != nil {
				errorFmt(io.ErrOut, "error resuming session", err)
				continue
			}
			lastContext = sess.UltimoContexto()
			if io.screen != nil {
				// Full screen: the resumed conversation comes back to the screen,
				// not just to the model.
				io.screen.Limpar()
				replayHistory(io.screen, sess.History())
				io.screen.Adicionar("") // spacing between the conversation and the notice
			}
			okFmt(io.Out, "session "+sess.ID()+" resumed")
		case "/save":
			if db == nil {
				warnFmt(io.Out, "session not persisted: nothing to save")
				continue
			}
			title := arg
			if title == "" {
				title = defaultTitle(sess)
			}
			if err := db.SetTitle(sess.ID(), title); err != nil {
				errorFmt(io.ErrOut, "error saving title", err)
				continue
			}
			okFmt(io.Out, "session "+sess.ID()+" saved — "+strings.TrimSpace(title))
		case "/rename":
			name := strings.Join(strings.Fields(arg), " ")
			if name == "" {
				warnFmt(io.Out, "usage: /rename <new session name>")
				continue
			}
			if db == nil {
				warnFmt(io.Out, "session not persisted: nowhere to store the name")
				continue
			}
			if err := db.SetTitle(sess.ID(), name); err != nil {
				errorFmt(io.ErrOut, "error renaming", err)
				continue
			}
			okFmt(io.Out, "session renamed: "+name)
		case "/help":
			printHelp(io)
		case "/update":
			cmdUpdate(io)
		case "/context", "/contexto": // /contexto: legacy alias, not in help
			cmdContext(io.Out, cfg, db)
		case "/skills":
			listSkills(io.Out, runtime.SkillsDisponiveis(cwd))
		case "/tasks":
			cmdTasks(io.Out, wired.Gerente(), arg)
		case "/mcps":
			listMCPs(io.Out, wired.MCP())
		case "/model":
			if arg != "" {
				// switch directly by id if it is in the current catalog
				if err := switchModelDirect(io, cfg, arg); err != nil {
					errorFmt(io.ErrOut, "error", err)
					continue
				}
			} else if err := selectModel(io, cfg); err == errUnchanged {
				okFmt(io.Out, "model unchanged: "+cfg.Model)
				continue
			} else if err != nil {
				errorFmt(io.ErrOut, "error", err)
				continue
			} else {
				if err := cfg.Save(); err != nil {
					errorFmt(io.ErrOut, "error saving", err)
					continue
				}
				sess.Configure(*cfg)
			}
			statusbar(io.Out, cfg)
		case "/effort":
			handleEffort(io, cfg, sess, arg)
		case "/provider":
			if err := cmdProvider(io, cfg); err != nil {
				errorFmt(io.ErrOut, "error", err)
				continue
			}
			sess.Configure(*cfg)
			statusbar(io.Out, cfg)
		default:
			request := strings.TrimSpace(line)
			if subNotice == "" {
				// A sub-agent result that finished while the user was typing
				// goes along with the request (non-full-screen mode, or a
				// notice not yet read).
				if g := wired.Gerente(); g != nil {
					if ts := g.Terminadas(); len(ts) > 0 {
						request = subagent.AvisoTerminadas(ts, g.Ativas()) + "\n---\n" + request
					}
				}
			}
			if strings.HasPrefix(fields[0], "/") && subNotice == "" {
				// /<skill-name> [args] runs the skill; any other / is an
				// unknown command.
				expanded, name, ok := runtime.ExpandirSkill(cwd, request)
				if !ok {
					fmt.Fprintf(io.Out, "unknown command: %s (/help lists commands; /skills lists skills)\n", fields[0])
					continue
				}
				okFmt(io.Out, "skill "+name)
				request = expanded
			}
			// model turn: Ctrl+C gives up the current round; a second
			// signal (or /exit) ends the REPL.
			ctx, cancel := io.turnContext(quitCh)
			rend := turnRenderer(io.Out)
			var tui *tuiTurn
			var tt *screenTurn
			if io.screen != nil && subNotice != "" {
				io.screen.AvisoSubagentes(subNotice)
				io.screen.Ocupado(true)
				tt = &screenTurn{t: io.screen, model: cfg.Model}
				rend = tt
			} else if io.screen != nil {
				io.screen.Usuario(strings.TrimSpace(line))
				io.screen.Ocupado(true)
				tt = &screenTurn{t: io.screen, model: cfg.Model}
				rend = tt
			} else if colorsOn {
				echoRequest(io.Out, strings.TrimSpace(line))
				tui = newTUITurn(io.Out, cfg)
				rend = tui
			}
			// Images pasted into the full screen ([Image N]) travel beside the line.
			var images []string
			if io.screen != nil && subNotice == "" {
				images = io.screen.takeImages(line)
			}
			errTurn := sess.SendWithImages(ctx, rend, cwd, request, images)
			if tt != nil {
				io.screen.stopSpin()
				io.screen.Ocupado(false)
				if tt.ctxTokens > 0 {
					lastContext = tt.ctxTokens
				}
			}
			if tui != nil {
				tui.stopSpin()
				tui.mu.Lock()
				if tui.ctxTokens > 0 {
					lastContext = tui.ctxTokens
				}
				tui.mu.Unlock()
			}
			cancel()
			if errTurn != nil {
				errorFmt(io.ErrOut, "error", errTurn)
			}
			select {
			case <-quitCh:
				exiting = true
			default:
			}
		}
	}
}

func printHelp(io IO) {
	fmt.Fprintln(io.Out, cyanf("commands"))
	printCommands(io.Out)
	fmt.Fprint(io.Out, dimf("plain text goes straight to the model; /<skill> [args] runs a skill; Esc twice cancels the turn.")+"\n\n")
}

// switchModelDirect resolves /model <index|id>: accepts the number from the
// alphabetical list or the ID; if neither is in the catalog, an actionable
// error and nothing changes.
func switchModelDirect(io IO, cfg *config.Config, input string) error {
	p := cfg.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := fetchModels(ctx, base, key)
	if err != nil {
		return err
	}
	id := ""
	if n, errAtoi := strconv.Atoi(input); errAtoi == nil && n >= 1 && n <= len(ls) {
		id = ls[n-1].ID
	} else if inCatalog(input, ls) {
		id = input
	}
	if id == "" {
		return fmt.Errorf("%q is not in the provider's catalog; run /model with no argument to list it", input)
	}
	draft := *cfg
	draft.Model = id
	if j := windowOf(id, ls); j > 0 {
		draft = applyWindow(draft, j)
	} else {
		draft = clearCatalogWindow(draft)
	}
	if err := draft.Save(); err != nil {
		return err
	}
	*cfg = draft
	return nil
}

// handleEffort processes /effort [level].
func handleEffort(io IO, cfg *config.Config, sess *runtime.Session, arg string) {
	current := "(off)"
	if cfg.Effort != "" {
		current = cfg.Effort
	}
	if arg == "" {
		fmt.Fprintf(io.Out, "effort: %s · levels: %s · `/effort -` clears\n", current, strings.Join(Esforcos, ", "))
		return
	}
	draft := *cfg
	if arg == "-" || arg == "off" || arg == "clear" {
		draft.Effort = ""
	} else {
		draft.Effort = arg
	}
	if err := draft.Save(); err != nil {
		fmt.Fprintf(io.ErrOut, "error saving: %v (effort stays %s)\n", err, current)
		return
	}
	*cfg = draft
	sess.Configure(*cfg)
	if cfg.Effort == "" {
		fmt.Fprintln(io.Out, "effort cleared; the next request goes out WITHOUT reasoning_effort.")
	} else {
		fmt.Fprintf(io.Out, "effort=%s; applies to the next request (provider rejects it? `/effort -` clears).\n", cfg.Effort)
	}
}

// cmdProvider asks for base URL and key (hidden), validates /models and persists.
// On validation failure the error propagates: neither memory nor disk changes.
func cmdProvider(io IO, cfg *config.Config) error {
	base := config.DefaultBaseURL
	if p := cfg.Ativo(); p != nil && p.BaseURL != "" {
		base = p.BaseURL
	}
	if v := ask(io, "base URL", base); v != "" && v != base {
		base = v
	}
	fmt.Fprintln(io.Out, "enter keeps the current key; the new key input is hidden.")
	newKey := ""
	if v, err := PromptKey("api key: "); err == nil && strings.TrimSpace(v) != "" {
		newKey = strings.TrimSpace(v)
	}
	key := newKey
	if key == "" {
		if p := cfg.Ativo(); p != nil {
			key = p.APIKey
		}
	}
	if key == "" {
		return fmt.Errorf("an API key is required to validate /models")
	}

	draft := *cfg
	err := configureProvider(io, &draft, base, key)
	// configureProvider returns ForaDoCatalogo when the stored model is not in
	// the new catalog: the provider is already applied to the draft, but the
	// model must be chosen BEFORE saving.
	var fc *ForaDoCatalogo
	if err != nil && !asNotInCatalog(err, &fc) {
		return err
	}
	if draft.Model == "" || fc != nil {
		fmt.Fprintln(io.Out, "the new provider does not have the current model — pick another:")
		if err := selectModel(io, &draft); err != nil {
			return fmt.Errorf("provider validated; run /provider again to pick a model (%w)", err)
		}
	}
	if err := draft.Save(); err != nil {
		return err
	}
	*cfg = draft
	return nil
}

func asNotInCatalog(err error, dst **ForaDoCatalogo) bool {
	if e, ok := err.(*ForaDoCatalogo); ok {
		*dst = e
		return true
	}
	return false
}
