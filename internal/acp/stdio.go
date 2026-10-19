// ACP server over stdio: how a real ACP client talks to the harness. main.go
// later calls Sirve (the integration belongs to another agent); this file is
// NOT wired anywhere yet.
//
// The stdio contract, and the reason for each precaution:
//   - stdout carries ONLY newline-delimited JSON-RPC. Any other write (banner,
//     log, runtime warning) corrupts the protocol — that is why the agent log
//     goes to stderr, never to the wire.
//   - the wire is closed on exit. ACP clients detect end of connection by EOF;
//     a descriptor left open after return leaves the client hanging.
package acp

import (
	"bufio"
	"context"
	"os"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// logToStderr is the agent's default logger on stdio: diagnostics go to
// stderr, never to the protocol wire.
func logToStderr(line string) {
	w := bufio.NewWriter(os.Stderr)
	_, _ = w.WriteString(line + "\n")
	_ = w.Flush()
}

// Sirve runs the ACP agent over the process stdin/stdout until the client
// closes, the ctx is cancelled or a write fails.
//
// The config is read ONCE here, not on every session/new: a client that
// changes the model in the file mid-conversation would see no effect in this
// process, and failing at that later is worse than making it clear now.
//
// On return, in-flight turns are awaited before closing stdout: a response cut
// in half would be worse than a few milliseconds of delay. Cancelling the ctx
// (host Ctrl+C, client shutdown) has already disarmed pending Sends, so this
// wait does not hang — unless a provider ignores the ctx, in which case the
// host closes stdin and the process dies with its parent.
func Sirve(ctx context.Context, cfg config.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a := Novo(cfg).ComLog(logToStderr)
	err := a.Handle(ctx, os.Stdin, os.Stdout)
	a.Esperar() // complete responses before closing
	_ = os.Stdout.Close()
	return err
}
