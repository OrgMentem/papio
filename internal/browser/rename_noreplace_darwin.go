// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves oldpath to newpath only if newpath does not exist.
// renamex_np(RENAME_EXCL) makes the existence check and the rename one atomic
// step and fails with EEXIST (os.ErrExist) instead of replacing. A volume
// without exclusive rename reports ENOTSUP, which is returned unchanged:
// callers fail closed rather than fall back to a replacing rename.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
