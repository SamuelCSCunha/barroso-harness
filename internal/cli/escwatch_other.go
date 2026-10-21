//go:build !linux && !darwin

package cli

import "context"

// watchEsc is a no-op here: the scrolling mode on this platform keeps the
// terminal in line mode and only Ctrl+C cancels a turn.
func watchEsc(io IO, cancel context.CancelFunc) (stop func()) { return func() {} }
