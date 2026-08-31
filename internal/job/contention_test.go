// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"testing"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/work"
	"time"
)

// contendingJob creates a job and drives it to state, returning its id.
func contendingJob(t *testing.T, js *Store, ctx context.Context, requestID, doi, state string) string {
	t.Helper()
	w := work.Work{DOI: doi, Title: "An Example Paper", Authors: []string{"Author, A."}, Year: 2020}
	id, err := js.CreateRequest(ctx, requestID, w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	if state == StateQueued {
		return id
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("queued->resolving: %v", err)
	}
	if state != StateResolving {
		if err := js.Transition(ctx, id, StateResolving, state, nil); err != nil {
			t.Fatalf("resolving->%s: %v", state, err)
		}
	}
	return id
}

func contentionBudget(t *testing.T, js *Store) *budget.Manager {
	t.Helper()
	fixed := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	m := budget.New(js.S,
		budget.WithNow(func() time.Time { return fixed }),
		budget.WithCreditPolicy(func(string) budget.CreditPolicy {
			return budget.CreditPolicy{DailyCreditFraction: 1, DailyCreditLimit: 100}
		}),
		budget.WithContentionProbe(js.OtherWorkWaiting),
	)
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 100, true); err != nil {
		t.Fatalf("observe source limit: %v", err)
	}
	return m
}

func spendJobShare(t *testing.T, m *budget.Manager, jobID string) {
	t.Helper()
	for range 25 {
		if err := m.CommitEgress(context.Background(), budget.EgressRequest{
			Source: config.SourceOpenAlex, Identity: "key-a", Credits: 1, JobID: jobID,
		}); err != nil {
			t.Fatalf("commit credit before share boundary: %v", err)
		}
	}
}

func TestOtherWorkWaitingSeesJobsThatWillMakeRequests(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	// One job, nothing else: no contention, so a per-job credit share must
	// not bind. An unspent allowance cannot be carried forward, so deferring
	// the only running job would cost throughput and buy nothing.
	mine := contendingJob(t, js, ctx, "wr_c_0001", "10.1002/mine", StateResolving)
	waiting, err := js.OtherWorkWaiting(ctx, mine)
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if waiting {
		t.Fatal("waiting = true with only the asking job present")
	}

	// A second job in retry_wait is exactly the victim this share exists to
	// protect: it was refused and is waiting for its turn.
	contendingJob(t, js, ctx, "wr_c_0002", "10.1002/other", StateRetryWait)
	waiting, err = js.OtherWorkWaiting(ctx, mine)
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if !waiting {
		t.Fatal("waiting = false with another job parked in retry_wait")
	}
}

func TestOtherWorkWaitingIgnoresJobsParkedOnPeople(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	mine := contendingJob(t, js, ctx, "wr_c_0010", "10.1002/mine", StateResolving)

	// Measured on the operator's store: of 139 jobs in awaiting_human, zero
	// had made a wire attempt since parking. Counting them would make
	// contention permanently true on any long-lived install, turning a share
	// that binds under pressure into one that always binds.
	for _, state := range []string{StateAwaitingHuman, StateReady, StateFailed, StateUnavailable, StateCancelled} {
		id := contendingJob(t, js, ctx, "wr_c_"+state, "10.1002/"+state, StateResolving)
		if err := js.Transition(ctx, id, StateResolving, state, nil); err != nil {
			t.Fatalf("cannot reach %s directly: %v", state, err)
		}
		waiting, err := js.OtherWorkWaiting(ctx, mine)
		if err != nil {
			t.Fatalf("OtherWorkWaiting: %v", err)
		}
		if waiting {
			t.Fatalf("a job in %s counted as waiting for an allowance", state)
		}
	}
}

func TestOtherWorkWaitingExcludesTheAskingJob(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	mine := contendingJob(t, js, ctx, "wr_c_0020", "10.1002/mine", StateResolving)

	// A job must never be its own contention: that would make the share bind
	// against the only work running, which is precisely the case the design
	// says must proceed.
	waiting, err := js.OtherWorkWaiting(ctx, mine)
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if waiting {
		t.Fatal("the asking job counted itself as contention")
	}

	// Asking on behalf of nobody (empty id) sees that same job, which is what
	// makes unattributed egress unable to accidentally exempt itself.
	waiting, err = js.OtherWorkWaiting(ctx, "")
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if !waiting {
		t.Fatal("waiting = false for an empty job id with a resolving job present")
	}
}

func TestCommitEgressChecksProductionContentionOutsideTransaction(t *testing.T) {
	js := testStore(t)
	mine := contendingJob(t, js, context.Background(), "wr_c_0030", "10.1002/mine", StateResolving)
	contendingJob(t, js, context.Background(), "wr_c_0031", "10.1002/other", StateQueued)
	m := contentionBudget(t, js)
	spendJobShare(t, m, mine)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := m.CommitEgress(ctx, budget.EgressRequest{
		Source: config.SourceOpenAlex, Identity: "key-a", Credits: 1, JobID: mine,
	})
	elapsed := time.Since(started)

	var exceeded *budget.ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Kind != budget.KindJobShare {
		t.Fatalf("CommitEgress = %v after %v, want a prompt job-share refusal", err, elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("CommitEgress took %v, want the contention probe to return promptly", elapsed)
	}
}

func TestCommitEgressCommitsAboveShareWithoutProductionContention(t *testing.T) {
	js := testStore(t)
	mine := contendingJob(t, js, context.Background(), "wr_c_0040", "10.1002/mine", StateResolving)
	m := contentionBudget(t, js)
	spendJobShare(t, m, mine)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.CommitEgress(ctx, budget.EgressRequest{
		Source: config.SourceOpenAlex, Identity: "key-a", Credits: 1, JobID: mine,
	}); err != nil {
		t.Fatalf("CommitEgress without other waiting work: %v", err)
	}

	assertCommitted := func(table, query string, args ...any) {
		t.Helper()
		var committed int
		if err := js.S.DB().QueryRow(query, args...).Scan(&committed); err != nil {
			t.Fatalf("read %s debit: %v", table, err)
		}
		if committed != 26 {
			t.Errorf("%s credits_committed = %d, want 26", table, committed)
		}
	}
	assertCommitted("source_credit_fuse",
		`SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, "2026-08-17")
	assertCommitted("job_credit_share",
		`SELECT credits_committed FROM job_credit_share WHERE job_id = ? AND source = ? AND utc_day = ?`,
		mine, config.SourceOpenAlex, "2026-08-17")
}
