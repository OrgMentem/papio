// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package filing

import (
	"golang.org/x/sys/windows"
	"os"
)

func openRegular(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrArtifact
	}
	return f, nil
}

// Windows does not support directory fsync. The artifact file itself is synced.
func syncDirectory(string) error { return nil }
