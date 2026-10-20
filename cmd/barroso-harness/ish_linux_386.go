//go:build linux && 386

package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// linux/386 exists for iSH (Alpine x86 emulated on iOS). Its emulator corrupts
// Go's state when signal-based async preemption fires or when several threads
// run Go code at once: TLS verification fails with "unknown authority" and the
// runtime dies with "morestack on g0" or random SIGSEGVs. Measured on an
// iPhone: a JSON/fmt stress test crashed every run with 4 Ps and passed every
// run with GOMAXPROCS=1 + GODEBUG=asyncpreemptoff=1. That still crashed about
// once every few minutes, during garbage collection running alongside the
// program; with gcstoptheworld=1 (and GOGC=400, fewer collections) it stopped
// crashing in the same test.
//
// Both settings are only read at process start, so the binary re-executes
// itself once with them in the environment. GOMAXPROCS/GOGC the user already
// set are kept; the marker variable stops a second re-exec.
func init() {
	if os.Getenv("BH_ISH_RUNTIME") != "" {
		// iSH answers syscalls it does not implement (pidfd_open, used by
		// os/exec on every spawn) with SIGSYS, which the Go runtime treats as
		// fatal (signal.Ignore does not apply to it). Set to SIG_IGN at the
		// kernel level, the call fails with ENOSYS and os/exec falls back to
		// plain fork/exec, the same way Go handles Android < 12.
		ignoreSIGSYS()
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	env := append(os.Environ(), "BH_ISH_RUNTIME=1")
	if os.Getenv("GOMAXPROCS") == "" {
		env = append(env, "GOMAXPROCS=1")
	}
	if os.Getenv("GOGC") == "" {
		env = append(env, "GOGC=400")
	}
	d := os.Getenv("GODEBUG")
	for _, setting := range []string{"asyncpreemptoff=1", "gcstoptheworld=1"} {
		name := setting[:strings.IndexByte(setting, '=')+1]
		if !strings.Contains(d, name) {
			if d != "" {
				d += ","
			}
			d += setting
		}
	}
	env = append(env, "GODEBUG="+d)
	_ = syscall.Exec(self, os.Args, env) // on failure, keep running as is
}

// ignoreSIGSYS sets SIGSYS to SIG_IGN with a raw rt_sigaction, bypassing the
// runtime's handler. i386 kernel struct: handler, flags, restorer, 64-bit mask.
func ignoreSIGSYS() {
	var sa struct {
		handler  uintptr
		flags    uint32
		restorer uintptr
		mask     uint64
	}
	sa.handler = 1 // SIG_IGN
	syscall.RawSyscall6(syscall.SYS_RT_SIGACTION, uintptr(syscall.SIGSYS),
		uintptr(unsafe.Pointer(&sa)), 0, 8, 0, 0)
}
