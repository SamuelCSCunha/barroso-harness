//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// atomicReplace overwrites the target on Windows. MoveFileEx with
// MOVEFILE_REPLACE_EXISTING fails if the target is read-only; in that case we
// remove the target and fall back to a plain rename.
func atomicReplace(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	const movefileReplaceExisting = 0x1
	if err := windows.MoveFileEx(fromPtr, toPtr, movefileReplaceExisting); err == nil {
		return nil
	}
	if err := os.Remove(to); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(from, to)
}
