// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
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

// TestInterruptedMigrationReportsItsRollback checks the database after startup
// cancellation and preserves the interruption as the error's cause, without
// adding a redundant rollback error from database/sql or SQLite.
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
	var rollbackErr *sqlite.Error
	if errors.Is(err, sql.ErrTxDone) || errors.As(err, &rollbackErr) {
		t.Fatalf("Open error = %v, want only the interruption, not a redundant rollback error", err)
	}

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "papio.db")+"?mode=ro&_pragma=busy_timeout(5000)")
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

func TestAbortMigrationAfterSQLiteRollback(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("migration failed")} {
		t.Run(cause.Error(), func(t *testing.T) {
			db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "papio.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
					t.Errorf("cleanup rollback: %v", err)
				}
			}()
			if _, err := tx.Exec(`
				CREATE TABLE gated (x);
				CREATE TRIGGER abort_insert BEFORE INSERT ON gated
				BEGIN SELECT RAISE(ROLLBACK, 'forced rollback'); END;
				PRAGMA user_version = 1;`); err != nil {
				t.Fatal(err)
			}
			// RAISE(ROLLBACK) ends SQLite's transaction without telling
			// database/sql, just as a statement interrupt can do.
			if _, err := tx.Exec("INSERT INTO gated VALUES (1)"); err == nil {
				t.Fatal("trigger did not roll the transaction back")
			}
			err = abortMigration(tx, "applying fixture", cause)
			if !errors.Is(err, cause) {
				t.Fatalf("abort error = %v, want original cause %v", err, cause)
			}
			var rollbackErr *sqlite.Error
			interrupted := errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
			if errors.As(err, &rollbackErr) == interrupted {
				t.Fatalf("abort error = %v: redundant rollback must be ignored only after interruption", err)
			}
			var version, tables int
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'gated'").Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if version != 0 || tables != 0 {
				t.Fatalf("after SQLite rollback user_version = %d, gated tables = %d, want both 0", version, tables)
			}
		})
	}
}
