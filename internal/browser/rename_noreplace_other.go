// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !linux && !darwin && !windows

package browser

import (
	"errors"
	"os"
)

// renameNoReplace has no atomic exclusive rename to call on this platform, so
// it reports errors.ErrUnsupported and callers fail closed rather than replace.
func renameNoReplace(oldpath, newpath string) error {
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.ErrUnsupported}
}
