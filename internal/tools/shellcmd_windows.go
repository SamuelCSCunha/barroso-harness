//go:build windows

package tools

import (
	"context"
	"os/exec"
	"syscall"
)

// terminalCmd builds the command cmd /C runs WITHOUT letting Go escape the
// arguments. exec.Command assembles the child's command line with
// syscall.EscapeArg, which rewrites the model's inner double quotes as \" —
// and cmd.exe does not understand backslash escapes, so the backslash leaks
// into the command and it arrives truncated at the first quote (e.g.
// `node -e "console.log(1)"` reached node as `-e` plus an unterminated
// string). Setting SysProcAttr.CmdLine hands the raw line straight to
// CreateProcess instead; cmd.exe then parses the quotes itself, the way a
// human typing in a terminal would.
//
// Args is still set so cmd.Path resolution and cmd.String() stay sane, but
// the spawn uses CmdLine, which wins over anything built from Args.
func terminalCmd(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd", "/C", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "/C " + command}
	return cmd
}
