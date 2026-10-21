//go:build !(linux && 386)

package update

// AutoDisponivel is true everywhere but the iSH build (see auto_ish.go).
const AutoDisponivel = true
