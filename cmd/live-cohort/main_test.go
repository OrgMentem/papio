// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/bench"
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

// A run that measured work before failing must still emit its report: the
// settled rows and cleanup it holds are real, and dropping them repeats the
// invisible situation the instrument exists to prevent. Fail-first: the old
// call site exited before rendering, so the measurements never reached the
// operator.
func TestFinishEmitsReportAlongsideRunError(t *testing.T) {
	report := livecohort.Report{
		CohortID: "test",
		RunID:    "testrun",
		Results: []livecohort.Result{{
			Key:        "w",
			Request:    "doi:10.1/w",
			Expected:   bench.AutonomousReady,
			Outcome:    livecohort.SubmitFailed,
			Verdict:    livecohort.VerdictMissed,
			StopDetail: "disk full for second",
			RequestID:  "livecohort-testrun-w",
		}},
		JournalFailures: []string{"livecohort: recording request livecohort-testrun-w: disk full for second"},
	}
	var stdout, stderr bytes.Buffer
	outPath := filepath.Join(t.TempDir(), "report.txt")
	code := finish(&stdout, &stderr, report, errors.New("disk full for second"), false, outPath)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 alongside the run error", code)
	}
	if !strings.Contains(stdout.String(), "JOURNAL FAILURES") || !strings.Contains(stdout.String(), "disk full for second") {
		t.Fatalf("stdout = %q, want the measured report with its journal failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "disk full for second") {
		t.Fatalf("stderr = %q, want the run error named", stderr.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading the written report: %v", err)
	}
	if string(data) != stdout.String() {
		t.Fatal("the file report differs from the printed report")
	}
}

// A complete run still exits 0 with no error on stderr.
func TestFinishSucceedsWithoutRunError(t *testing.T) {
	report := livecohort.Report{CohortID: "test", RunID: "testrun"}
	var stdout, stderr bytes.Buffer
	if code := finish(&stdout, &stderr, report, nil, true, ""); code != 0 {
		t.Fatalf("exit = %d, want 0 for a complete run (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"cohort_id"`) {
		t.Fatalf("stdout = %q, want the JSON report", stdout.String())
	}
}
