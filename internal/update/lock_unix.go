// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package update

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile attempts a non-blocking exclusive lock on file. It reports
// (false, nil) when another holder owns the lock so the caller can retry.
// The lock lives on the open file description, so every papio process, and
// every Checker that opens the lock file separately, contends for it.
func tryLockFile(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// unlockFile releases the lock acquired by tryLockFile.
func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
