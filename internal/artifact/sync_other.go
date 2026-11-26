// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package artifact

import "os"

// syncRegularFile flushes one file's data and metadata to stable storage. A
// POSIX fsync works through a read-only descriptor, so the 0400 mode a failed
// earlier Promote left on the quarantine file does not get in the way.
func syncRegularFile(path string) error {
	file, err := os.Open(path)
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
