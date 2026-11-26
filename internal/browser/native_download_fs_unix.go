// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package browser

import (
	"errors"
	"os"
	"syscall"
)

func nativeSingleLink(_ *os.File, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

// syncNativeRootDir flushes a pinned directory's entries so a link or create
// in it survives power loss. A filesystem that cannot sync a directory has
// nothing to flush and reports success, as config.SyncDir does for a path.
func syncNativeRootDir(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
