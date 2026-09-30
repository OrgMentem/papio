// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalexyield

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/store/storetest"
)

// OpenReadOnly reads a daemon's database without ever writing, migrating, or
// creating it: the measurement must leave the store exactly as it found it.
func TestOpenReadOnlyReadsAMigratedStoreAndRefusesWrites(t *testing.T) {
	path := filepath.Join(storetest.DataDir(t), "papio.db")
	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	var before int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&before); err != nil || before == 0 {
		t.Fatalf("user_version = %d, %v; want the migrated schema readable", before, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE measurement_leak (x INTEGER)`); err == nil {
		t.Fatal("read-only store accepted a schema write")
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 1`); err == nil {
		t.Fatal("read-only store accepted a user_version write")
	}
	var after int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&after); err != nil || after != before {
		t.Fatalf("user_version after refused writes = %d, %v; want %d", after, err, before)
	}
}

// A missing database is an error naming the path, and it is never created:
// the tool has no business making the store it only measures.
func TestOpenReadOnlyMissingStoreIsNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "papio.db")
	db, err := OpenReadOnly(path)
	if err == nil {
		_ = db.Close()
		t.Fatal("OpenReadOnly opened a database that does not exist")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "has the papio daemon run") {
		t.Fatalf("error = %v, want the path and the daemon hint", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat after a failed open = %v, want the database still absent", statErr)
	}
}
