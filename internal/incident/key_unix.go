// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package incident

import (
	"fmt"
	"os"
)

func restrictIncidentKey(file *os.File) error { return file.Chmod(0o600) }

func syncIncidentKeyDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening incident key directory: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return fmt.Errorf("syncing incident key directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing incident key directory: %w", closeErr)
	}
	return nil
}
