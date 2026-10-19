//go:build !windows

package tools

import (
	"context"
	"os/exec"
)

// terminalCmd is the non-Windows half of terminal_run's spawn. On Unix the
// child is bash -c with a single argument, which Go's argv passing delivers
// verbatim — there is no escaping layer to work around.
func terminalCmd(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "bash", "-c", command)
}
