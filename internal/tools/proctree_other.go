//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// treeAttrs puts the command in its own process group: killing -pgid then
// reaches every descendant and never the agent.
func treeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree ends the process group led by pid.
func killTree(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if p, err := findProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}
