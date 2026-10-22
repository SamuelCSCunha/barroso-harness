//go:build !windows

package tools

import (
	"context"
	"os/exec"
)

// terminalCmd is the non-Windows half of shell's spawn. On Unix the
// child is bash -c with a single argument, which Go's argv passing delivers
// verbatim — there is no escaping layer to work around. Minimal systems
// without bash (Alpine, iSH, BusyBox images) fall back to POSIX sh.
func terminalCmd(ctx context.Context, command string) *exec.Cmd {
	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	return exec.CommandContext(ctx, shell, "-c", command)
}
