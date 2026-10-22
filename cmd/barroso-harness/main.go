// Barroso Harness — single executable, cross-platform, provider-neutral.
// -p runs one non-interactive turn with the already saved config; with no
// arguments it starts the interactive onboarding/REPL.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ruanbarroso/barroso-harness/internal/cli"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
	"golang.org/x/term"
)

// Versao is overridden at link time by the deployer (-X main.Versao=vX.Y.Z-bh.N).
var Versao = "dev"

func main() {
	// The REPL needs the version so /update can compare the installed build
	// with the published one. The deployer's -X reaches THIS variable, not the
	// cli package's, so main forwards it — otherwise /update would always see
	// itself as "dev" and treat any release as new.
	cli.Versao = Versao

	var (
		prompt      = flag.String("p", "", "single-turn prompt (print mode)")
		initFlag    = flag.Bool("init", false, "create the data directory in ~/.barroso-harness without overwriting files")
		acpFlag     = flag.Bool("acp", false, "serve the ACP protocol (JSON-RPC 2.0) over stdio for an editor/client")
		versionFlag = flag.Bool("version", false, "print the version")
		statsFlag   = flag.Int("stats", 0, "print cache/latency metrics of the latest N sessions (or of the session ids given as arguments)")
	)
	flag.Parse()

	if *statsFlag > 0 {
		if err := runStats(os.Stdout, flag.Args(), *statsFlag); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if *versionFlag {
		fmt.Println("barroso-harness " + Versao)
		return
	}
	if *initFlag {
		if err := config.Init(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("data directory created at", config.Dir())
		return
	}
	if flag.NArg() > 0 && *prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: bh [-p \"prompt\"] [-acp] [-init] [-version] [-stats N [session...]]")
		os.Exit(2)
	}
	if *acpFlag {
		serveACP()
		return
	}
	if *prompt == "" {
		os.Exit(cli.RunInteractive(cli.IO{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}))
	}
	sessionID := ""
	if err := Executar(os.Stdout, *prompt, &sessionID); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	// The id is only worth announcing when someone is watching: in a pipe, the
	// consumer on the other side would get an extra line mid-output.
	if sessionID != "" && isTerminal(os.Stderr) {
		fmt.Fprintf(os.Stderr, "session: %s\n", sessionID)
	}
}

// isTerminal reports whether the writer is a real terminal (x/term). Buffers,
// pipes and redirections are not — and the `session:` line is for humans.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Executar runs one turn. Persisting the log is best-effort: if the database
// does not open, the turn still happens and sessionID comes back empty (no
// `session:` line). Subsystem wiring (MCP, sub-agents) is best-effort too: a
// failing server does not abort the turn; the warning goes to stderr.
func Executar(out io.Writer, prompt string, sessionID *string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if err != nil || st == nil {
		return runWithWiring(out, cfg, cwd, prompt, nil, "")
	}
	id := runtime.NovoSessaoID()
	if sessionID != nil {
		*sessionID = id
	}
	err = runWithWiring(out, cfg, cwd, prompt, st, id)
	if closeErr := st.Fechar(); err == nil {
		err = closeErr
	}
	return err
}

// runWithWiring builds the session (persistent when there is a store), wires
// the process-owned subsystems and runs the turn. It is the single recipe for
// both -p paths: the id differs, everything else is the same.
func runWithWiring(out io.Writer, cfg config.Config, cwd, prompt string, st *store.Store, id string) error {
	var sess *runtime.Session
	if st != nil {
		sess = runtime.NewSessionWithStore(cfg, st, id)
	} else {
		sess = runtime.NewSession(cfg)
	}
	// Process-lifetime ctx: sub-agents outlive the turn that created them.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	set, warnings := wiring.Montar(ctx, cfg, sess, cwd)
	defer func() { _ = set.Fechar(); tools.KillJobs() }()
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	rend := runtime.RendererDe(out)
	if err := sess.Send(context.Background(), rend, cwd, prompt); err != nil {
		return err
	}
	// -p has no user to wake the REPL: while a sub-agent is running, the
	// process waits, and each batch that finishes becomes another turn with
	// the results — otherwise the process would exit, killing the sub-agents.
	m := set.Gerente()
	if m == nil {
		return nil
	}
	done := make(chan struct{}, 1)
	m.AoTerminar(func() {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	for {
		if fs := m.Terminadas(); len(fs) > 0 {
			// A dedicated ctx per turn: the one-trigger-per-turn limit compares
			// the ctx, and context.Background() would be the same for all.
			turn, end := context.WithCancel(context.Background())
			err := sess.Send(turn, rend, cwd, subagent.AvisoTerminadas(fs, m.Ativas()))
			end()
			if err != nil {
				return err
			}
			continue
		}
		if m.Ativas() == 0 {
			return nil
		}
		<-done
	}
}
