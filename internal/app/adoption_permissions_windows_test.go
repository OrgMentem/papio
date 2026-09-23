// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package app

import (
	"path/filepath"
	"syscall"
	"testing"
)

func failAdoptionRootResolution(t *testing.T, svc *Service, _ string) (func(), error) {
	t.Helper()
	// Windows metadata lookup can succeed despite a read-denying DACL. Use an
	// invalid configured root to exercise the same operational-error branch
	// through real filesystem resolution, without changing token privileges.
	// Keep effective and legacy roots equal so a valid drain root cannot hide
	// the configured-root fault. Jobs and artifacts retain their original stores;
	// neither the existing component nor any on-disk directory is modified.
	dataDir, adoptionRoot := svc.Config.DataDir, svc.Config.Browser.AdoptionRoot
	restore := func() {
		svc.Config.DataDir = dataDir
		svc.Config.Browser.AdoptionRoot = adoptionRoot
	}
	t.Cleanup(restore)
	svc.Config.DataDir = dataDir + "\x00"
	svc.Config.Browser.AdoptionRoot = filepath.Join(svc.Config.DataDir, "adoptions")
	return restore, syscall.EINVAL
}
