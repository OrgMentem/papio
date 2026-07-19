// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile attempts a non-blocking exclusive lock on file using LockFileEx,
// the Windows analog of flock. It reports (false, nil) when another holder owns
// the lock so the caller can retry. A single byte is locked; unlockFile must
// release the identical range.
func tryLockFile(file *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped),
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// unlockFile releases the lock acquired by tryLockFile.
func unlockFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, new(windows.Overlapped))
}
