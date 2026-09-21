// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package app

import (
	"os"
	"testing"
)

func failAdoptionRootResolution(t *testing.T, _ *Service, root string) (error, func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := os.Chmod(root, info.Mode().Perm()); err != nil {
			t.Errorf("restore adoption root mode: %v", err)
		}
	}
	t.Cleanup(restore)
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	return os.ErrPermission, restore
}
