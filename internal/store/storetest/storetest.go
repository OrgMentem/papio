// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package storetest hands tests a data directory whose database is already
// migrated, so store.Open finds user_version current and applies nothing.
//
// Migrations are the dominant cost in any package that builds a store per test:
// under -race one full migration run measures ~0.6s against ~0.045s for opening
// an already-migrated file. internal/browser has 285 tests, so it was spending
// roughly 170 of its 253 local seconds re-deriving schema no test varies — and
// on CI's shared runners that package reached 471s, then crossed `go test`'s
// 10-minute per-package timeout and failed the build.
//
// The template is produced once per process by the real migrations, so every
// test gets byte-identical schema to what it would have built itself.
package storetest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"papio/internal/store"
)

var (
	once     sync.Once
	template []byte
	buildErr error
)

// DataDir returns a fresh directory containing a migrated papio.db. Pass it
// where a test would otherwise pass t.TempDir(), then open it as usual.
func DataDir(t *testing.T) string {
	t.Helper()
	once.Do(build)
	if buildErr != nil {
		t.Fatalf("storetest: building migrated template: %v", buildErr)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "papio.db"), template, 0o600); err != nil {
		t.Fatalf("storetest: seeding %s: %v", dir, err)
	}
	return dir
}

// build runs the real migrations once and keeps the resulting file in memory.
// store.Close checkpoints the WAL, so the single papio.db file is the whole
// database — no sidecars to carry.
func build() {
	dir, err := os.MkdirTemp("", "papio-storetest-")
	if err != nil {
		buildErr = err
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		buildErr = err
		return
	}
	if err := db.Close(); err != nil {
		buildErr = err
		return
	}
	template, buildErr = os.ReadFile(filepath.Join(dir, "papio.db"))
}
