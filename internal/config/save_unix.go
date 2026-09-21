// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

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
