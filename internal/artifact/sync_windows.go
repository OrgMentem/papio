// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// syncRegularFile flushes one file's data and metadata to stable storage.
// FlushFileBuffers needs write access, and a Promote that failed after its
// chmod left the quarantine file read-only, so a refused open lifts the
// attribute and retries once.
//
// The attribute belongs to the file, not to a name (see reassertReadOnly), so
// when an earlier Promote already linked the artifact, lifting it also unseals
// the published artifact. It is therefore restored on EVERY path out of the
// lift: a failed reopen, a failed flush and a successful flush alike. A restore
// that fails is returned, because the artifact would otherwise stay writable.
func syncRegularFile(path string) (err error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrPermission) {
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return err
		}
		defer func() {
			if restoreErr := os.Chmod(path, 0o400); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restoring read-only attribute: %w", restoreErr))
			}
		}()
		file, err = os.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
