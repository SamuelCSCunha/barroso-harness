//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// watchEsc gives the scrolling mode (the iSH build) the full screen's Esc:
// while a turn runs the terminal leaves canonical mode, so a lone Esc arrives
// without Enter, and two within escInterruptWindow cancel the turn. ISIG stays
// on, so Ctrl+C still raises SIGINT. Other keys are dropped: this mode has no
// steering to send them to. The returned stop restores the terminal and only
// returns once the watcher is gone, so the next prompt reads in canonical mode
// again. Without a terminal, or where termios is refused, it does nothing.
func watchEsc(io IO, cancel context.CancelFunc) (stop func()) {
	in, ok := io.In.(*os.File)
	if !ok {
		return func() {}
	}
	fd := int(in.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return func() {}
	}
	cbreak := *old
	cbreak.Lflag &^= unix.ICANON | unix.ECHO
	cbreak.Cc[unix.VMIN], cbreak.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &cbreak); err != nil {
		return func() {}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var armedAt time.Time
		buf := make([]byte, 64)
		for {
			select {
			case <-quit:
				return
			default:
			}
			ready, err := readable(fd, 100)
			if err != nil {
				return // poll refused: give up quietly rather than spin
			}
			if !ready {
				continue
			}
			n, err := unix.Read(fd, buf)
			if err != nil || n == 0 {
				return
			}
			// A lone Esc is one byte with nothing behind it; arrows and other
			// keys send Esc followed by more bytes in the same burst.
			if n != 1 || buf[0] != 0x1b {
				continue
			}
			if more, _ := readable(fd, 30); more {
				continue
			}
			if !armedAt.IsZero() && time.Since(armedAt) < escInterruptWindow {
				// The spinner owns the current line: clear it first, or the
				// notice lands glued to "Thinking 5s".
				fmt.Fprint(io.Out, clearLine)
				warnFmt(io.Out, "canceling the turn")
				cancel()
				return
			}
			armedAt = time.Now()
			fmt.Fprint(io.Out, clearLine)
			warnFmt(io.Out, "press Esc again to cancel the turn")
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			<-done
			_ = unix.IoctlSetTermios(fd, ioctlSetTermios, old)
		})
	}
}

// clearLine returns to column 0 and erases the line.
const clearLine = "\r\033[K"

// readable waits up to ms milliseconds for input on fd. EINTR counts as "not
// yet"; any other error is returned.
func readable(fd, ms int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, ms)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return n > 0, err
}
