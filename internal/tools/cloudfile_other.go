//go:build !windows

package tools

import "io/fs"

// cloudOnly: placeholders that download on read are a Windows matter.
func cloudOnly(fs.DirEntry) bool { return false }
