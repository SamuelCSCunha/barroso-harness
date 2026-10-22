package tools

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// The agent once ran a cleanup script that killed the connector hosting its own
// terminal, and with it the agent itself. ProtectedPIDs lists this process and
// its ancestors (the shell, the terminal host, whatever spawned it); the system
// prompt names them and shell refuses a kill aimed at any of them.

// ProtectedPIDs is this process followed by its ancestors, nearest first, up to
// (not including) init. Where /proc is missing it stops at the parent.
func ProtectedPIDs() []int {
	pids := []int{os.Getpid()}
	pid := os.Getppid()
	for pid > 1 && len(pids) < 32 {
		pids = append(pids, pid)
		next, ok := parentOf(pid)
		if !ok || next == pid {
			break
		}
		pid = next
	}
	return pids
}

// parentOf reads PPid from /proc/<pid>/status (Linux, iSH).
func parentOf(pid int) (int, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "PPid:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			return n, err == nil
		}
	}
	return 0, false
}

// killArgs matches a kill invocation up to the end of its simple command.
var killArgs = regexp.MustCompile(`(?:^|[\s;&|(` + "`" + `])kill\s+([^;&|\n)` + "`" + `]*)`)

// killsProtected reports the first protected PID a literal `kill ... <pid>` in
// command targets. Negative PIDs (process groups) count by their absolute
// value. It only sees literal numbers: it is a seatbelt, not a sandbox.
func killsProtected(command string, protected []int) (int, bool) {
	set := map[int]bool{}
	for _, p := range protected {
		set[p] = true
	}
	for _, m := range killArgs.FindAllStringSubmatch(command, -1) {
		for _, f := range strings.Fields(m[1]) {
			n, err := strconv.Atoi(strings.TrimPrefix(f, "-"))
			if err == nil && n > 1 && set[n] {
				return n, true
			}
		}
	}
	return 0, false
}

// ProtectedNote is the system prompt line naming the protected processes.
func ProtectedNote() string {
	pids := ProtectedPIDs()
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("- Your own process is PID %d; it and its ancestors (PIDs %s) host this session — the shell, the terminal and the connector carrying it to the user. "+
		"Never kill, signal or restart them, directly or through a script, pkill/killall pattern or service restart: doing so ends this session mid-turn. "+
		"When cleaning up processes, exclude these PIDs explicitly.\n", pids[0], strings.Join(s, " "))
}
