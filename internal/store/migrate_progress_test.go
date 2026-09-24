// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"modernc.org/sqlite"
)

// migrationGate holds a fixture migration inside its own SQL statement. The
// statement calls papio_test_gate(), which reports that the migration is
// running and then waits until the test releases it: a slow migration whose
// length the test decides, with no timing assumption.
var migrationGate struct {
	entered chan struct{}
	release chan struct{}
}

func init() {
	sqlite.MustRegisterScalarFunction("papio_test_gate", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		migrationGate.entered <- struct{}{}
		<-migrationGate.release
		return int64(1), nil
	})
}

// useMigrations replaces the embedded migration set with files for the
// length of the test.
func useMigrations(t *testing.T, files fstest.MapFS) {
	t.Helper()
	previous := migrations
	migrations = files
	t.Cleanup(func() { migrations = previous })
}

// useGatedMigration replaces the migration set with one migration that stops
// inside its statement until the test releases it.
func useGatedMigration(t *testing.T) {
	t.Helper()
	useMigrations(t, fstest.MapFS{
		"migrations/0001_gated.sql": {Data: []byte("CREATE TABLE gated (x); INSERT INTO gated SELECT papio_test_gate();")},
	})
	migrationGate.entered = make(chan struct{}, 1)
	migrationGate.release = make(chan struct{})
}

// TestOpenTraceReportsOnlyPendingMigrations pins what a daemon tells the
// commands waiting for it: an upgrade when the database is behind this binary,
// from the version it is at, and never on an ordinary start, which would
// announce an upgrade on every start. The integrity check runs every time.
func TestOpenTraceReportsOnlyPendingMigrations(t *testing.T) {
	useMigrations(t, fstest.MapFS{
		// open() adds a partial index on pdf_grabs after migrating, so the
		// fixture needs that table.
		"migrations/0001_base.sql": {Data: []byte("CREATE TABLE pdf_grabs (url_host TEXT, title TEXT, state TEXT);")},
		"migrations/0002_next.sql": {Data: []byte("CREATE TABLE next_step (x);")},
	})
	ctx := context.Background()
	dataDir := t.TempDir()
	older, err := open(ctx, dataDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := older.Close(); err != nil {
		t.Fatal(err)
	}

	var migrating [][2]int
	checks := 0
	traced := WithOpenTrace(ctx, &OpenTrace{
		Migrating: func(from, to int) { migrating = append(migrating, [2]int{from, to}) },
		Checking:  func() { checks++ },
	})
	for range 2 {
		s, err := Open(traced, dataDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if len(migrating) != 1 || migrating[0] != [2]int{1, 2} {
		t.Fatalf("Migrating calls = %v, want one call from 1 to 2", migrating)
	}
	if checks != 2 {
		t.Fatalf("Checking calls = %d, want one for each open", checks)
	}
}

// TestInterruptedMigrationReportsItsRollback reproduces a daemon whose startup
// context ends while a migration is running, as a signal ends it. database/sql
// rolls the transaction back as soon as the context ends, so the explicit
// rollback that follows finds the transaction already done. That is the
// rollback having happened, and the log must say so, not report a second
// failure ("rollback also failed: sql: transaction has already been committed
// or rolled back") that reads as a damaged database.
func TestInterruptedMigrationReportsItsRollback(t *testing.T) {
	useGatedMigration(t)
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan error, 1)
	go func() {
		s, err := Open(ctx, dataDir)
		if s != nil {
			_ = s.Close()
		}
		opened <- err
	}()
	<-migrationGate.entered
	cancel()
	close(migrationGate.release)
	err := <-opened
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "rollback also failed") {
		t.Fatalf("Open error = %q: the migration was rolled back, so the rollback must not be reported as failing", err)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Open error = %q, want it to say the migration was rolled back", err)
	}

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "papio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, tables int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'gated'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != 0 || tables != 0 {
		t.Fatalf("after the interrupted migration user_version = %d and gated tables = %d, want both 0", version, tables)
	}
}
