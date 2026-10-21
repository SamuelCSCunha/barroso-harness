//go:build linux && 386

package cli

// simpleOnly fixes the scrolling mode on the linux/386 build, which exists
// only for iSH: its terminal sends no mouse wheel events, so the full screen
// could not scroll the conversation, while the scrolling mode leaves history
// to iSH's own touch scrolling.
const simpleOnly = true
