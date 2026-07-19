// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestBackupFallsBackWhenHardLinksUnsupported(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	backupDir := t.TempDir()
	destination := filepath.Join(backupDir, "backup.db")
	raceDestination := filepath.Join(backupDir, "racing-backup.db")
	originalLink := linkFile
	linkFile = func(oldname, newname string) error {
		if newname == raceDestination {
			if err := os.WriteFile(newname, []byte("pre-existing backup"), 0o600); err != nil {
				return err
			}
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	t.Cleanup(func() { linkFile = originalLink })

	if err := db.Backup(ctx, destination); err != nil {
		t.Fatalf("backup without hard links: %v", err)
	}
	backup, err := sql.Open("sqlite", "file:"+destination)
	if err != nil {
		t.Fatalf("open fallback backup: %v", err)
	}
	defer backup.Close()
	var integrity string
	if err := backup.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("integrity check fallback backup: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("fallback backup integrity = %q, want ok", integrity)
	}

	if err := db.Backup(ctx, raceDestination); err == nil {
		t.Fatal("backup overwrote destination created during fallback")
	}
	contents, err := os.ReadFile(raceDestination)
	if err != nil {
		t.Fatalf("read racing destination: %v", err)
	}
	if string(contents) != "pre-existing backup" {
		t.Fatalf("racing destination was overwritten: %q", contents)
	}
	partials, err := filepath.Glob(filepath.Join(backupDir, ".papio-backup-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("fallback backup temporary files remain: %v", partials)
	}
}
