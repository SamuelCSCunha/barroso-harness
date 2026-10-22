//go:build windows

package tools

import (
	"io/fs"
	"syscall"
)

// Attributes of OneDrive/Files On-Demand placeholders: reading one makes
// Windows download it, and the read can block for minutes.
const (
	fileAttributeOffline            = 0x1000
	fileAttributeRecallOnOpen       = 0x40000
	fileAttributeRecallOnDataAccess = 0x400000
)

// cloudOnly reports a file whose content is not on the local disk.
func cloudOnly(d fs.DirEntry) bool {
	info, err := d.Info()
	if err != nil {
		return false
	}
	a, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return a.FileAttributes&(fileAttributeOffline|fileAttributeRecallOnOpen|fileAttributeRecallOnDataAccess) != 0
}
