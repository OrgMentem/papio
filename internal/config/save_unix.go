// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

// SyncDir flushes dir's entries to stable storage, so a rename or create in it
// survives power loss. A filesystem that cannot sync a directory has nothing to
// flush and reports success.
func SyncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func normalizeConfigTarget(path string) string { return path }

func lockConfig(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("config lock must be a regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrConfigBusy
		}
		return nil, err
	}
	return file, nil
}
