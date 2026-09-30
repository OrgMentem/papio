// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func checkPathPrivacy(path string, info os.FileInfo) *privacyError {
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if info.IsDir() {
		return &privacyError{"data directory is not private", "chmod 0700 " + path}
	}
	return &privacyError{"configuration is readable by group or others", "chmod 600 " + path}
}

// checkWorkerExecutable asks the kernel whether THIS process may execute the
// worker. A mode with some execute bit is not enough: a root-owned 0700 file,
// or one of ours with only group/other execute bits, stats fine and has an
// execute bit, yet execve fails with EACCES for the current user.
func checkWorkerExecutable(_ context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("papio worker executable is not runnable")
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return fmt.Errorf("papio worker executable is not runnable by the current user: %w", err)
	}
	return nil
}
