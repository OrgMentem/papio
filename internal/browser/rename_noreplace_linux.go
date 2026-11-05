// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves oldpath to newpath only if newpath does not exist.
// renameat2(RENAME_NOREPLACE) makes the existence check and the rename one
// atomic step and fails with EEXIST (os.ErrExist) instead of replacing. A
// filesystem without the flag reports EINVAL, which is returned unchanged:
// callers fail closed rather than fall back to a replacing rename.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "renameat2", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
