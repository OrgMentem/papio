// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

// shareManager builds a credit manager with a fixed clock, a known allowance and
// a settable contention answer, so a test can drive the share gate without a job
// table.
func shareManager(t *testing.T, limit int, waiting *bool) (*Manager, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	fixed := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	m := New(s,
		WithNow(func() time.Time { return fixed }),
		WithCreditPolicy(func(string) CreditPolicy {
			return CreditPolicy{DailyCreditFraction: 1, DailyCreditLimit: limit}
		}),
		WithContentionProbe(func(context.Context, string) (bool, error) { return *waiting, nil }),
	)
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", limit, true); err != nil {
		t.Fatal(err)
	}
	return m, s
}

func jobReq(jobID string, credits int) EgressRequest {
	return EgressRequest{Source: config.SourceOpenAlex, Identity: "key-a", Credits: credits, JobID: jobID}
}

// spendAll commits credits one request at a time and returns the first refusal.
func spendAll(t *testing.T, m *Manager, jobID string, requests, credits int) error {
	t.Helper()
	for i := 0; i < requests; i++ {
		if err := m.CommitEgress(context.Background(), jobReq(jobID, credits)); err != nil {
			return err
		}
	}
	return nil
}

func TestJobShareBoundsOneJobUnderContention(t *testing.T) {
	waiting := true
	m, _ := shareManager(t, 100, &waiting)

	// A quarter of 100 is 25, so the 26th credit is the one that must be
	// refused — the whole point being that the other 75 stay available to
	// the work that is waiting for them.
	err := spendAll(t, m, "job-hog", 26, 1)
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("CommitEgress = %v, want an ErrExceeded at the share boundary", err)
	}
	if exceeded.Kind != KindJobShare {
		t.Fatalf("kind = %v, want KindJobShare", exceeded.Kind)
	}
	if exceeded.JobID != "job-hog" {
		t.Fatalf("JobID = %q, want job-hog", exceeded.JobID)
	}
	if got := int(exceeded.Limit); got != 25 {
		t.Fatalf("share limit = %d, want 25", got)
	}
	if exceeded.Committed != 25 {
		t.Fatalf("committed = %d, want 25 (the job's own spend, not the source's)", exceeded.Committed)
	}
	// A per-job refusal must not read as a source-wide exhaustion: the day
	// still has three quarters of its allowance left.
	if exceeded.Window != WindowUTCDay {
		t.Fatalf("window = %v, want WindowUTCDay", exceeded.Window)
	}

	// The refusal is per JOB, so other work still gets served.
	if err := m.CommitEgress(context.Background(), jobReq("job-other", 1)); err != nil {
		t.Fatalf("second job refused after the first hit its share: %v", err)
	}
}

func TestJobShareDoesNotBindWithoutContention(t *testing.T) {
	waiting := false
	m, _ := shareManager(t, 100, &waiting)

	// Nothing else is waiting, so this job spends far past its quarter share
	// unrefused: an unspent allowance cannot be carried forward, and refusing
	// the only running job would cost throughput for nobody's benefit. It
	// stays under the source-wide day limit, which is checked first and is a
	// different refusal.
	if err := spendAll(t, m, "job-alone", 40, 1); err != nil {
		t.Fatalf("uncontended job refused past its share: %v", err)
	}

	// The moment other work appears, the share binds on the next request —
	// the counter was accumulating all along.
	waiting = true
	err := m.CommitEgress(context.Background(), jobReq("job-alone", 1))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Kind != KindJobShare {
		t.Fatalf("CommitEgress = %v, want KindJobShare once contention appears", err)
	}
	if exceeded.Committed != 40 {
		t.Fatalf("committed = %d, want the 40 already spent uncontended", exceeded.Committed)
	}
}

func TestJobShareCounterIsMonotone(t *testing.T) {
	waiting := true
	m, s := shareManager(t, 100, &waiting)
	if err := spendAll(t, m, "job-a", 10, 1); err != nil {
		t.Fatal(err)
	}

	// There is no reset path, and that is the design: the earlier proposal
	// reset the counter on "progress", which let a source dribbling novelty
	// keep one job alive forever. Only a human resubmission resets a share,
	// and that produces a different job id.
	var taken int
	if err := s.DB().QueryRow(
		`SELECT credits_committed FROM job_credit_share WHERE job_id = ?`, "job-a").Scan(&taken); err != nil {
		t.Fatal(err)
	}
	if taken != 10 {
		t.Fatalf("counter = %d, want 10", taken)
	}
	if err := spendAll(t, m, "job-a", 5, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(
		`SELECT credits_committed FROM job_credit_share WHERE job_id = ?`, "job-a").Scan(&taken); err != nil {
		t.Fatal(err)
	}
	if taken != 15 {
		t.Fatalf("counter = %d after more spend, want 15 (monotone)", taken)
	}
}

func TestJobShareChargesInTheEgressTransaction(t *testing.T) {
	waiting := true
	m, s := shareManager(t, 10, &waiting)

	// The source-wide fuse refuses this: 20 > the 10-credit day. The job's
	// share must not record credits the source never spent, because the two
	// numbers are written in one transaction.
	if err := m.CommitEgress(context.Background(), jobReq("job-a", 20)); err == nil {
		t.Fatal("over-allowance request was admitted")
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM job_credit_share WHERE job_id = ?`, "job-a").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("job_credit_share rows = %d after a refused commit, want 0", rows)
	}
}

func TestJobShareSkippedWithoutAttribution(t *testing.T) {
	waiting := true
	m, s := shareManager(t, 100, &waiting)

	// An unattributed commit (no job on the context) must not be charged to
	// a guessed row, and must not be refused either: the share is an
	// accounting rule, not a permission system.
	for i := 0; i < 30; i++ {
		if err := m.CommitEgress(context.Background(), EgressRequest{
			Source: config.SourceOpenAlex, Identity: "key-a", Credits: 1,
		}); err != nil {
			t.Fatalf("unattributed commit %d refused: %v", i, err)
		}
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM job_credit_share`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("job_credit_share rows = %d, want 0 for unattributed egress", rows)
	}
}

func TestJobShareTreatsProbeFailureAsNoContention(t *testing.T) {
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fixed := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	m := New(s,
		WithNow(func() time.Time { return fixed }),
		WithCreditPolicy(func(string) CreditPolicy { return CreditPolicy{DailyCreditFraction: 1, DailyCreditLimit: 100} }),
		WithContentionProbe(func(context.Context, string) (bool, error) {
			return false, errors.New("jobs table unreadable")
		}),
	)
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 100, true); err != nil {
		t.Fatal(err)
	}

	// A share must never refuse work because the signal that would justify
	// refusing it could not be read. Availability is the safe failure mode.
	if err := spendAll(t, m, "job-a", 40, 1); err != nil {
		t.Fatalf("commit refused on an unreadable contention probe: %v", err)
	}
}

func TestJobShareGuardRequired(t *testing.T) {
	old := egressTestHooks.enforceJobShare
	egressTestHooks.enforceJobShare = false
	t.Cleanup(func() { egressTestHooks.enforceJobShare = old })

	waiting := true
	m, _ := shareManager(t, 100, &waiting)
	if err := spendAll(t, m, "job-hog", 26, 1); err != nil {
		t.Fatalf("with the share guard disabled the 26th credit should pass — proves TestJobShareBoundsOneJobUnderContention exercises the guard: %v", err)
	}
}

func TestJobShareLimitFloorsAtOne(t *testing.T) {
	// A quarter of the bootstrap allowance rounds toward zero. Refusing every
	// request — including the only job's — because the share computed to 0
	// would make a cold start unable to do anything at all.
	if got := jobShareLimit(2); got != 1 {
		t.Fatalf("jobShareLimit(2) = %d, want 1", got)
	}
	if got := jobShareLimit(0); got != 1 {
		t.Fatalf("jobShareLimit(0) = %d, want 1", got)
	}
	if got := jobShareLimit(10000); got != 2500 {
		t.Fatalf("jobShareLimit(10000) = %d, want 2500", got)
	}
}

func TestJobShareErrorNamesTheJobNotDollars(t *testing.T) {
	// Error()'s default branch prints money. A new kind that fell through to
	// it would report a credit refusal in dollars, which is why KindJobShare
	// is a distinct kind rather than a reused KindCredits.
	err := &ErrExceeded{
		Source: config.SourceOpenAlex, Identity: "key-a", JobID: "job-a",
		Kind: KindJobShare, Window: WindowUTCDay,
		Until:     time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		Committed: 2500, Attempt: 1, Limit: 2500,
	}
	got := err.Error()
	for _, want := range []string{"job-a", "share", "other work is waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "$") {
		t.Errorf("Error() = %q, want no dollar amount in a credit refusal", got)
	}
	if got, want := KindJobShare.String(), "job_share"; got != want {
		t.Errorf("KindJobShare.String() = %q, want %q", got, want)
	}
}
