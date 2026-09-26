// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/livecohort"
)

func writeStore(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "papio.db"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("seeding papio.db: %v", err)
	}
}

// Without the store lookup the run cannot name a committed job, so wiring
// must refuse before the first submission rather than measure unprotected.
func TestWireSafetyRefusesWithoutTheStore(t *testing.T) {
	var opts livecohort.Options
	closeSafety, err := wireSafety(&opts, t.TempDir(), filepath.Join(t.TempDir(), "j.json"), false)
	if err == nil {
		closeSafety()
		t.Fatal("wireSafety opened a run with no read-only store")
	}
	if !strings.Contains(err.Error(), "store") {
		t.Fatalf("error = %v, want the missing store named", err)
	}
	if opts.Lookup != nil || opts.Journal != nil {
		t.Fatal("wireSafety left safety dependencies half-wired after refusing")
	}
}

// Without the journal a later run cannot reconcile a lost submission, so
// wiring must refuse rather than submit jobs no rerun can account for.
func TestWireSafetyRefusesWithoutTheJournal(t *testing.T) {
	dataDir := t.TempDir()
	writeStore(t, dataDir)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding blocker: %v", err)
	}
	var opts livecohort.Options
	closeSafety, err := wireSafety(&opts, dataDir, filepath.Join(blocker, "journal.json"), false)
	if err == nil {
		closeSafety()
		t.Fatal("wireSafety opened a run with no submission journal")
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Fatalf("error = %v, want the missing journal named", err)
	}
	if opts.Lookup != nil || opts.Journal != nil {
		t.Fatal("wireSafety left safety dependencies half-wired after refusing")
	}
}

// -no-store drops the untried-candidate column, which is evidence. The
// safety lookup is not evidence, so it stays wired.
func TestWireSafetyKeepsTheLookupUnderNoStore(t *testing.T) {
	dataDir := t.TempDir()
	writeStore(t, dataDir)
	var opts livecohort.Options
	closeSafety, err := wireSafety(&opts, dataDir, filepath.Join(t.TempDir(), "j.json"), true)
	if err != nil {
		t.Fatalf("wireSafety: %v", err)
	}
	defer closeSafety()
	if opts.Lookup == nil {
		t.Fatal("lookup is nil: -no-store must only drop the candidate column")
	}
	if opts.Inspector != nil {
		t.Fatal("inspector is wired: -no-store must leave the candidate column unfilled")
	}
	if opts.Journal == nil {
		t.Fatal("journal is nil: the safety record must always be wired")
	}
}
