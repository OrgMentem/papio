// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
)

func fetchPathFixture(t *testing.T, direct bool, mode string) (*Service, *job.Store, *job.Row) {
	t.Helper()
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = mode
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
			Source: "fixture", URL: "https://example.test/paper.pdf", Direct: direct,
			Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
			ReuseLicense: "unknown", IdentityConfidence: 1,
		}}},
		Policy: config.Source{Enabled: true},
	}}
	svc.Validate = passValidation()
	id, err := svc.Submit(context.Background(), doiRequest("wr_fetch_path"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return svc, jobs, row
}

func TestCancelDuringFetchDoesNotEscapeProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		mode string
	}{
		{name: "success", mode: config.ModeConservative},
		{name: "retry", err: &fetch.Error{Class: fetch.ClassRetryable, HTTPStatus: 503}, mode: config.ModeConservative},
		{name: "exhaustion", err: &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 404}, mode: config.ModeConservative},
		{name: "browser_park", err: &fetch.Error{Class: fetch.ClassBlocked, HTTPStatus: 403}, mode: config.ModeDelegated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, jobs, row := fetchPathFixture(t, true, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan string, 1)
			resume := make(chan struct{})
			calls := 0
			download := fakeDownload(&calls)
			svc.Fetch = func(ctx context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
				started <- path
				select {
				case <-resume:
				case <-ctx.Done():
					return fetch.Result{}, ctx.Err()
				}
				if tc.err != nil {
					return fetch.Result{}, tc.err
				}
				return download(ctx, candidate, path)
			}
			finished := make(chan error, 1)
			go func() { finished <- svc.Process(ctx, row) }()
			var path string
			select {
			case path = <-started:
			case err := <-finished:
				t.Fatalf("Process returned before fetching: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := svc.CancelJob(ctx, row.ID, job.TerminalReasonCancelledByUser); err != nil {
				t.Fatal(err)
			}
			close(resume)
			select {
			case err := <-finished:
				if err != nil {
					t.Fatalf("Process after cancellation: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := jobs.Get(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != job.StateCancelled {
				t.Fatalf("state = %q, want cancelled", current.State)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fetch file remains after cancellation: %v", err)
			}
			var ended, outcome sql.NullString
			if err := jobs.S.DB().QueryRowContext(ctx,
				`SELECT ended_at, outcome FROM attempts WHERE job_id = ? AND stage = 'fetch' ORDER BY id DESC LIMIT 1`, row.ID,
			).Scan(&ended, &outcome); err != nil {
				t.Fatal(err)
			}
			if !ended.Valid || outcome.String != "cancelled" {
				t.Fatalf("attempt ended=%v outcome=%v, want closed cancelled attempt", ended, outcome)
			}
		})
	}
}

func TestFetchFailureReturnsCandidateAndAttemptWriteErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		direct bool
		err    error
		status string
	}{
		{name: "non_direct", status: "skipped"},
		{name: "suppressed", direct: true, err: fetch.Suppressed("example.test"), status: "skipped"},
		{name: "backoff", direct: true, err: fetch.BackedOff("example.test", time.Minute), status: "retryable"},
		{name: "retryable", direct: true, err: &fetch.Error{Class: fetch.ClassRetryable, HTTPStatus: 503}, status: "retryable"},
		{name: "blocked", direct: true, err: &fetch.Error{Class: fetch.ClassBlocked, HTTPStatus: 403}, status: "skipped"},
		{name: "invalid", direct: true, err: &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 404}, status: "invalid"},
	} {
		for _, write := range []string{"candidate", "attempt"} {
			if !tc.direct && write == "attempt" {
				continue
			}
			t.Run(tc.name+"/"+write, func(t *testing.T) {
				svc, jobs, row := fetchPathFixture(t, tc.direct, config.ModeConservative)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				calls := 0
				svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
					calls++
					return fetch.Result{}, tc.err
				}
				trigger := fmt.Sprintf(`CREATE TRIGGER fail_fetch_write BEFORE UPDATE OF status ON candidates
					WHEN NEW.status = '%s'
					BEGIN SELECT RAISE(ABORT, 'injected fetch write failure'); END`, tc.status)
				if write == "attempt" {
					trigger = `CREATE TRIGGER fail_fetch_write BEFORE UPDATE OF ended_at ON attempts
						WHEN OLD.stage = 'fetch'
						BEGIN SELECT RAISE(ABORT, 'injected fetch write failure'); END`
				}
				if _, err := jobs.S.DB().ExecContext(ctx, trigger); err != nil {
					t.Fatal(err)
				}
				if err := svc.Process(ctx, row); err == nil || !strings.Contains(err.Error(), "injected fetch write failure") {
					t.Fatalf("Process error = %v, want injected store error", err)
				}
				wantCalls := 0
				if tc.direct {
					wantCalls = 1
				}
				if calls != wantCalls {
					t.Fatalf("fetch calls = %d, want %d", calls, wantCalls)
				}
				current, err := jobs.Get(ctx, row.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.State != job.StateFetching {
					t.Fatalf("state = %q, want fetching after store failure", current.State)
				}
				if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_fetch_write`); err != nil {
					t.Fatal(err)
				}
				if err := jobs.ResetCandidates(ctx, row.ID); err != nil {
					t.Fatal(err)
				}
				pending, err := jobs.NextPendingCandidate(ctx, row.ID)
				if err != nil || pending == nil {
					t.Fatalf("candidate after retry reset = %+v, %v; want runnable candidate", pending, err)
				}
				if write == "candidate" && tc.direct {
					var ended sql.NullString
					if err := jobs.S.DB().QueryRowContext(ctx,
						`SELECT ended_at FROM attempts WHERE job_id = ? AND stage = 'fetch'`, row.ID,
					).Scan(&ended); err != nil {
						t.Fatal(err)
					}
					if !ended.Valid {
						t.Fatal("candidate write failure left the completed fetch attempt open")
					}
				}
			})
		}
	}
}

func TestRederivableLandingSeedsReturnsLookupErrors(t *testing.T) {
	for _, column := range []string{"id", "source"} {
		t.Run(column, func(t *testing.T) {
			svc, jobs, row := fetchPathFixture(t, true, config.ModeConservative)
			ctx := context.Background()
			candidate := resolver.Candidate{
				Source: "fixture", URL: "https://example.test/derived.pdf", Direct: true,
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
				ReuseLicense: "unknown", IdentityConfidence: 1,
			}
			persisted, live := candidateRows(row, []resolver.Candidate{candidate}, []string{""})
			if _, err := jobs.InsertCandidates(ctx, row.ID, persisted); err != nil {
				t.Fatal(err)
			}
			key := persisted[0].URLKey
			id, err := jobs.CandidateIDByKey(ctx, row.ID, key)
			if err != nil {
				t.Fatal(err)
			}
			if err := jobs.MarkCandidate(ctx, id, "retryable"); err != nil {
				t.Fatal(err)
			}
			if err := jobs.RecordEvent(ctx, row.ID, landingDerivedEventKind, map[string]any{
				"parent_url_key": "parent", "url_key": key,
			}); err != nil {
				t.Fatal(err)
			}
			// A temporary view injects a scan failure in either lookup without
			// changing the durable candidate or preventing the event read.
			if _, err := jobs.S.DB().ExecContext(ctx, fmt.Sprintf(
				`CREATE TEMP VIEW candidates AS SELECT NULL AS %s, c.* FROM main.candidates c`, column,
			)); err != nil {
				t.Fatal(err)
			}
			_, _, lookupErr := svc.rederivableLandingSeeds(ctx, row, live)
			if _, err := jobs.S.DB().ExecContext(ctx, `DROP VIEW temp.candidates`); err != nil {
				t.Fatal(err)
			}
			if lookupErr == nil || errors.Is(lookupErr, sql.ErrNoRows) {
				t.Fatalf("lookup error = %v, want operational store error", lookupErr)
			}
			stored, err := jobs.GetCandidate(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != "retryable" {
				t.Fatalf("candidate status = %q, want retryable", stored.Status)
			}
			_, pending, err := svc.rederivableLandingSeeds(ctx, row, live)
			if err != nil || !pending[key] {
				t.Fatalf("recovered pending set = %v, %v; want derived candidate", pending, err)
			}
		})
	}
}

func TestFetchPreservesNonterminalTransitionConflict(t *testing.T) {
	svc, jobs, row := fetchPathFixture(t, true, config.ModeConservative)
	ctx := context.Background()
	calls := 0
	download := fakeDownload(&calls)
	svc.Fetch = func(ctx context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
		if err := jobs.Transition(ctx, row.ID, job.StateFetching, job.StateResolving, nil); err != nil {
			return fetch.Result{}, err
		}
		return download(ctx, candidate, path)
	}
	if err := svc.Process(ctx, row); !errors.Is(err, job.ErrConflict) {
		t.Fatalf("Process error = %v, want nonterminal conflict", err)
	}
	current, err := jobs.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != job.StateResolving {
		t.Fatalf("state = %q, want resolving", current.State)
	}
}

func TestRederivableLandingSeedsIgnoresMissingCandidate(t *testing.T) {
	svc, jobs, row := fetchPathFixture(t, true, config.ModeConservative)
	ctx := context.Background()
	if err := jobs.RecordEvent(ctx, row.ID, landingDerivedEventKind, map[string]any{
		"parent_url_key": "parent", "url_key": "missing",
	}); err != nil {
		t.Fatal(err)
	}
	seeds, pending, err := svc.rederivableLandingSeeds(ctx, row, nil)
	if err != nil || len(seeds) != 0 || len(pending) != 0 {
		t.Fatalf("missing candidate: seeds=%v pending=%v err=%v", seeds, pending, err)
	}
}
