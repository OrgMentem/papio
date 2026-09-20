// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package doctor

import (
	"context"
	"errors"
	"os"
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

func checkWorkerExecutable(_ context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("papio worker executable is not runnable")
	}
	return nil
}
