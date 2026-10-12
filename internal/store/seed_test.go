// Copyright 2026 OrgMentem. Licensed under MIT.

package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var (
	seedOnce     sync.Once
	seedTemplate []byte
	seedErr      error
)

// seededDataDir returns a fresh directory holding an already-migrated papio.db,
// for tests that need the current schema but do not exercise migration. It is
// storetest.DataDir for this package's own tests: internal/store/storetest
// imports this package, so an in-package test cannot import it without a cycle.
// Tests that pin a migration or an older schema keep building their own.
func seededDataDir(t *testing.T) string {
	t.Helper()
	seedOnce.Do(buildSeed)
	if seedErr != nil {
		t.Fatalf("building migrated template: %v", seedErr)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "papio.db"), seedTemplate, 0o600); err != nil {
		t.Fatalf("seeding %s: %v", dir, err)
	}
	return dir
}

// buildSeed runs the real migrations once. Close checkpoints the WAL, so the
// single papio.db file is the whole database.
func buildSeed() {
	dir, err := os.MkdirTemp("", "papio-store-seed-")
	if err != nil {
		seedErr = err
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	db, err := Open(context.Background(), dir)
	if err != nil {
		seedErr = err
		return
	}
	if err := db.Close(); err != nil {
		seedErr = err
		return
	}
	seedTemplate, seedErr = os.ReadFile(filepath.Join(dir, "papio.db"))
}
