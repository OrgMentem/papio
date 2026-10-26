//go:build windows

package app

import (
	"errors"

	"golang.org/x/sys/windows"
)

// hookOwnerAlive reports whether a hook reservation owner with pid may still
// run. Opening the process fails with ERROR_INVALID_PARAMETER when no process
// has the pid; anything else (including access denied) means it may exist,
// mirroring the daemon's processExited with the polarity flipped.
func hookOwnerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: a pid this code recorded fits in a DWORD.
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	event, err := windows.WaitForSingleObject(handle, 0)
	return err != nil || event != windows.WAIT_OBJECT_0
}
