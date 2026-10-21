//go:build !(linux && 386)

package cli

// simpleOnly is false everywhere but the iSH build (see simple_ish.go): a real
// terminal always gets the full screen.
const simpleOnly = false
