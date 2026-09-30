// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/job"
	"papio/internal/redact"
	"papio/internal/store"
	"papio/internal/store/storetest"
	paperwork "papio/internal/work"
)

var inspectorPolicy = job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}

// seedStore writes jobs through the real job store into a migrated data
// directory, then closes it so the inspector reads what a daemon committed.
func seedStore(t *testing.T, seed func(ctx context.Context, js *job.Store)) string {
	t.Helper()
	dataDir := storetest.DataDir(t)
	s, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	seed(context.Background(), &job.Store{S: s})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func openInspector(t *testing.T, dataDir string) *StoreInspector {
	t.Helper()
	inspector, err := OpenStoreInspector(dataDir)
	if err != nil {
		t.Fatalf("OpenStoreInspector: %v", err)
	}
	t.Cleanup(func() { _ = inspector.Close() })
	return inspector
}

func createInspectorJob(t *testing.T, ctx context.Context, js *job.Store, requestID string, who job.Attribution, force bool) string {
	t.Helper()
	created, err := js.CreateRequestForWork(ctx, requestID, paperwork.Work{DOI: "10.1000/" + requestID, Title: "Paper " + requestID},
		"", "", inspectorPolicy, nil, who, force)
	if err != nil {
		t.Fatal(err)
	}
	return created.JobID
}

// The untried column answers "did papio stop while still holding something
// it never attempted": only pending counts, and a job with no candidates is
// zero of zero rather than an error.
func TestStoreInspectorCountsOnlyPendingCandidatesAsUntried(t *testing.T) {
	var withCandidates, without string
	dataDir := seedStore(t, func(ctx context.Context, js *job.Store) {
		withCandidates = createInspectorJob(t, ctx, js, "wr-candidates", job.Attribution{}, false)
		without = createInspectorJob(t, ctx, js, "wr-empty", job.Attribution{}, false)
		statuses := []string{"pending", "pending", "retryable", "invalid", "skipped"}
		var cands []job.Candidate
		for i, status := range statuses {
			key := withCandidates + "-" + status + "-" + string(rune('a'+i))
			cands = append(cands, job.Candidate{
				JobID: withCandidates, Source: "unpaywall", URLRedacted: redact.URL("https://example.test/" + key + ".pdf"), URLKey: key,
				Version: "published", AccessBasis: "open_access", ExpectedMIME: "application/pdf", Direct: true, Rank: i,
			})
		}
		if _, err := js.InsertCandidates(ctx, withCandidates, cands); err != nil {
			t.Fatal(err)
		}
		rows, err := js.S.DB().QueryContext(ctx, `SELECT id FROM candidates WHERE job_id = ? ORDER BY rank`, withCandidates)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if len(ids) != len(statuses) {
			t.Fatalf("inserted %d candidates, want %d", len(ids), len(statuses))
		}
		for i, status := range statuses {
			if status == "pending" {
				continue
			}
			if err := js.MarkCandidate(ctx, ids[i], status); err != nil {
				t.Fatal(err)
			}
		}
	})
	inspector := openInspector(t, dataDir)
	if want := filepath.Join(dataDir, "papio.db"); inspector.Path() != want {
		t.Fatalf("Path() = %q, want %q", inspector.Path(), want)
	}

	untried, total, err := inspector.UntriedCandidates(context.Background(), withCandidates)
	if err != nil || untried != 2 || total != 5 {
		t.Fatalf("UntriedCandidates = %d/%d, %v; want 2 pending of 5", untried, total, err)
	}
	untried, total, err = inspector.UntriedCandidates(context.Background(), without)
	if err != nil || untried != 0 || total != 0 {
		t.Fatalf("UntriedCandidates(no candidates) = %d/%d, %v; want 0/0", untried, total, err)
	}
}

// The safety lookups must name a committed job even once it is terminal:
// resubmitting to discover it would duplicate the acquisition. A force
// submission stores a generated request id, so only its consumer names it.
func TestStoreInspectorFindsCommittedJobsIncludingTerminalAndForced(t *testing.T) {
	var cancelled, forced string
	dataDir := seedStore(t, func(ctx context.Context, js *job.Store) {
		cancelled = createInspectorJob(t, ctx, js, "wr-cancelled", job.Attribution{}, false)
		if err := js.Transition(ctx, cancelled, job.StateQueued, job.StateCancelled, nil); err != nil {
			t.Fatal(err)
		}
		forced = createInspectorJob(t, ctx, js, "wr-forced", job.Attribution{Consumer: "livecohort-run-forced"}, true)
	})
	inspector := openInspector(t, dataDir)
	ctx := context.Background()

	jobID, state, found, err := inspector.JobForRequest(ctx, "wr-cancelled")
	if err != nil || !found || jobID != cancelled || state != job.StateCancelled {
		t.Fatalf("JobForRequest(terminal) = %q %q %v %v; want %q cancelled", jobID, state, found, err, cancelled)
	}
	if jobID, _, found, err := inspector.JobForRequest(ctx, "wr-forced"); err != nil || found {
		t.Fatalf("JobForRequest(forced) = %q %v %v; a force submission must not be found by its supplied request id", jobID, found, err)
	}
	jobID, state, found, err = inspector.JobForConsumer(ctx, "livecohort-run-forced")
	if err != nil || !found || jobID != forced || state != job.StateQueued {
		t.Fatalf("JobForConsumer = %q %q %v %v; want %q queued", jobID, state, found, err, forced)
	}
	if _, _, found, err := inspector.JobForRequest(ctx, "wr-never"); err != nil || found {
		t.Fatalf("JobForRequest(unknown) found=%v err=%v; want a clean not-found", found, err)
	}
	if _, _, found, err := inspector.JobForConsumer(ctx, "never"); err != nil || found {
		t.Fatalf("JobForConsumer(unknown) found=%v err=%v; want a clean not-found", found, err)
	}
}

// A measurement must never write the store it measures.
func TestStoreInspectorRefusesWrites(t *testing.T) {
	dataDir := seedStore(t, func(context.Context, *job.Store) {})
	inspector := openInspector(t, dataDir)
	if _, err := inspector.db.ExecContext(context.Background(), `CREATE TABLE measurement_leak (x INTEGER)`); err == nil {
		t.Fatal("read-only inspector accepted a write")
	}
}

// A missing store is named, not created: the run refuses rather than measure
// against an empty database it made itself.
func TestOpenStoreInspectorMissingStoreIsNotCreated(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := OpenStoreInspector(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenStoreInspector(missing) error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "papio.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat papio.db after a failed open = %v, want it still absent", err)
	}
}

// A file that is not a papio database fails its reads rather than reporting
// zero untried candidates, which would read as "papio held nothing back".
func TestStoreInspectorMalformedStoreFailsReads(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "papio.db"), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspector := openInspector(t, dataDir)
	if untried, total, err := inspector.UntriedCandidates(context.Background(), "job"); err == nil {
		t.Fatalf("UntriedCandidates on a malformed store = %d/%d, nil; want an error", untried, total)
	}
	if _, _, _, err := inspector.JobForRequest(context.Background(), "wr"); err == nil {
		t.Fatal("JobForRequest on a malformed store returned no error")
	}
}
