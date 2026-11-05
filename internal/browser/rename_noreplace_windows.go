// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"os"

	"golang.org/x/sys/windows"
)

// renameNoReplace moves oldpath to newpath only if newpath does not exist.
// os.Rename passes MOVEFILE_REPLACE_EXISTING; MoveFileEx without it leaves the
// existence check to the filesystem's rename and fails with
// ERROR_ALREADY_EXISTS (os.ErrExist) instead of replacing. No
// MOVEFILE_COPY_ALLOWED: a copy across volumes is not atomic, so that fails
// too, and callers fail closed.
func renameNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return &os.LinkError{Op: "movefileex", Old: oldpath, New: newpath, Err: err}
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return &os.LinkError{Op: "movefileex", Old: oldpath, New: newpath, Err: err}
	}
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return &os.LinkError{Op: "movefileex", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
