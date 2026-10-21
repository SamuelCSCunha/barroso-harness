// /update: downloads the newest release from the channel and replaces the installed binary.
//
// The command deliberately runs OUTSIDE the REPL loop: installing replaces
// the running executable, and on Windows that requires the process to exit
// first. Doing the swap inside the interactive loop would inherit a lock that
// never resolves — the handler only orchestrates, and internal/update decides
// what is possible.
//
// The question comes BEFORE the download (via Confirmar), so nobody waits
// for 11 MB to find out they did not want to update.
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/update"
)

// Versao is this binary's version, injected by main (-X main.Versao). It
// lives here as a package variable because the REPL does not receive the
// version as an argument, and an /update that does not know its own version
// would compare against an empty string.
var Versao = "dev"

// timeoutUpdate is the channel deadline: query + ~11 MB download + exec check.
// A bad connection needs slack; 10 min is the ceiling that still does not
// look stuck to someone watching the terminal.
const timeoutUpdate = 10 * time.Minute

// cmdUpdate runs the full cycle. A channel failure NEVER brings the REPL
// down: the error comes back as a message and the session goes on — updating is a convenience.
// It reports true when a new binary was installed: the REPL then exits on its
// own, since the running process is still the old version.
func cmdUpdate(io IO) bool {
	version := strings.TrimSpace(Versao)
	ctx, cancel := context.WithTimeout(context.Background(), timeoutUpdate)
	defer cancel()

	installed, err := update.Rodar(ctx, io.Out, version, func() bool {
		return confirm(io, "update barroso-harness now? [y/N]")
	})
	if err != nil {
		errorFmt(io.ErrOut, "update failed", err)
		return false
	}
	return installed
}

// confirm asks a yes/no question and returns true only for an explicit yes.
// An empty Enter is NO: whoever did not answer did not consent, and
// replacing the binary by omission would be the most dangerous default.
//
// EOF (Ctrl+D) midway is also a no — the line comes back clean, as in the
// rest of the REPL. "s"/"sim" are still accepted as yes (Portuguese input).
func confirm(io IO, question string) bool {
	fmt.Fprintf(io.Out, "%s %s ", greenf("❯"), question)
	line, err := io.Reader().ReadString('\n')
	if err != nil {
		fmt.Fprintln(io.Out)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "s" || answer == "sim" || answer == "y" || answer == "yes"
}
