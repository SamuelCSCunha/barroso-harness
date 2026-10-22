//go:build windows

package tools

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// treeAttrs puts the command in its own process group, so killing the tree
// does not reach the agent.
func treeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killTree ends pid and every descendant. cmd /C spawns the real program as a
// child; killing only cmd.exe left it (and its pipe handles) alive.
func killTree(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if k.Run() != nil {
		if p, err := findProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}
