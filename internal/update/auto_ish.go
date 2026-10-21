//go:build linux && 386

package update

// AutoDisponivel is false on the linux/386 build, which exists only for iSH:
// there the binary is replaced by hand, through /update, never in the
// background.
const AutoDisponivel = false
