// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func setSchemaVersion(t *testing.T, ctx context.Context, dataDir string, version int) {
	t.Helper()
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
		_ = raw.Close()
		t.Fatalf("set future schema version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close future schema database: %v", err)
	}
}

func TestGuardCapableSchema33RefusesSchema34(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	setSchemaVersion(t, ctx, dataDir, 34)

	opened, err := open(ctx, dataDir, 33)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("schema-33 guard returned a store for schema 34")
	}
	if err == nil {
		t.Fatal("schema-33 guard accepted schema version 34")
	}
	if got, want := err.Error(), "database schema version 34 is newer than this binary supports (33); refusing to open"; !strings.Contains(got, want) {
		t.Fatalf("schema-33 guard error = %q, want containing %q", got, want)
	}
}

func TestOpenRefusesSchemaNewerThanBinary(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	setSchemaVersion(t, ctx, dataDir, 39)

	opened, err := Open(ctx, dataDir)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("Open returned a store for a future schema")
	}
	if err == nil {
		t.Fatal("Open accepted schema version 39 with latest embedded migration 38")
	}
	if got, want := err.Error(), "database schema version 39 is newer than this binary supports (38); refusing to open"; !strings.Contains(got, want) {
		t.Fatalf("Open error = %q, want containing %q", got, want)
	}
}
