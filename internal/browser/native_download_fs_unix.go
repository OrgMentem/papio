// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package browser

import (
	"os"
	"syscall"
)

func nativeSingleLink(_ *os.File, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
