// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalexyield

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens the papio store at path (normally
// "<data_dir>/papio.db") strictly read-only: mode=ro at the SQLite driver
// level, so this package can never write to — or apply a migration against —
// the daemon's live database, and can safely run concurrently with a live
// daemon (WAL mode supports any number of concurrent readers alongside the
// daemon's one writer). Unlike internal/store.Open, this never calls
// os.MkdirAll and never runs a migration: a measurement tool has no business
// creating or upgrading the database it only reads.
func OpenReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening papio store %s read-only: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening papio store %s read-only: %w (has the papio daemon run at least once?)", path, err)
	}
	return db, nil
}
