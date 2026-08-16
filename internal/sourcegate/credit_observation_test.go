// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/store"
)

type recordingCredit struct {
	limitCalls []struct {
		identity string
		limit    int
		primary  bool
	}
	usedCalls    []int
	prepaidCalls []float64
	errLimit     error
	errUsed      error
	errPrepaid   error
}

func (r *recordingCredit) ObserveLimit(_ context.Context, _ string, identity string, limit int, primary bool) error {
	r.limitCalls = append(r.limitCalls, struct {
		identity string
		limit    int
		primary  bool
	}{identity, limit, primary})
	return r.errLimit
}

func (r *recordingCredit) ObserveCreditsUsed(_ context.Context, _ string, used int) error {
	r.usedCalls = append(r.usedCalls, used)
	return r.errUsed
}

func (r *recordingCredit) ObservePrepaidRemaining(_ context.Context, _ string, remainingUSD float64) error {
	r.prepaidCalls = append(r.prepaidCalls, remainingUSD)
	return r.errPrepaid
}

func testObserverWithCredit(t *testing.T, credit CreditObserver, headers map[string]string) (*Observer, *fakeQuotaFloor, *headerClient) {
	t.Helper()
	deferrer := &fakeDeferrer{}
	now := func() time.Time { return observerNow }
	floor := &fakeQuotaFloor{fakeDeferrer: deferrer, now: now}
	inner := &headerClient{status: http.StatusOK, headers: headers}
	observer, err := NewObserver(floor, credit, "openalex", config.Source{Enabled: true, APIKey: "private-key"}, inner)
	if err != nil {
		t.Fatal(err)
	}
	observer.now = now
	floor.now = now
	return observer, floor, inner
}

func bearerRequest(t *testing.T) *http.Request {
	t.Helper()
	req := observerRequest(t, "https://api.openalex.org/works")
	req.Header = make(http.Header)
	SetOpenAlexAuthorization(req, "private-key")
	return req
}

func TestObserverPrimaryIdentityRecordsDenominator(t *testing.T) {
	rec := &recordingCredit{}
	observer, _, inner := testObserverWithCredit(t, rec, map[string]string{
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Remaining": "9000",
		"X-RateLimit-Reset":     "3600",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
	if len(rec.limitCalls) != 1 {
		t.Fatalf("ObserveLimit calls = %d, want 1", len(rec.limitCalls))
	}
	if !rec.limitCalls[0].primary {
		t.Fatal("ObserveLimit primary = false, want true for configured primary identity")
	}
	if rec.limitCalls[0].limit != 10000 {
		t.Fatalf("limit = %d, want 10000", rec.limitCalls[0].limit)
	}
}

func TestObserverNonPrimaryIdentityDoesNotEstablishDenominator(t *testing.T) {
	rec := &recordingCredit{}
	observer, _, _ := testObserverWithCredit(t, rec, map[string]string{
		"X-RateLimit-Limit":     "1000",
		"X-RateLimit-Remaining": "900",
		"X-RateLimit-Reset":     "3600",
	})
	if err := doAndClose(observer, observerRequest(t, "https://api.openalex.org/works?mailto=reader@example.org")); err != nil {
		t.Fatal(err)
	}
	if len(rec.limitCalls) != 1 {
		t.Fatalf("ObserveLimit calls = %d, want 1", len(rec.limitCalls))
	}
	if rec.limitCalls[0].primary {
		t.Fatal("ObserveLimit primary = true, want false for anonymous fallback on keyed install")
	}
}

func TestObserverCreditsUsedSeedsCounter(t *testing.T) {
	rec := &recordingCredit{}
	observer, _, _ := testObserverWithCredit(t, rec, map[string]string{
		"X-RateLimit-Credits-Used": "240",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	if len(rec.usedCalls) != 1 || rec.usedCalls[0] != 240 {
		t.Fatalf("usedCalls = %v, want [240]", rec.usedCalls)
	}
}

func TestObserverCreditsUsedSeedsDatabase(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	m := budget.New(s, budget.WithNow(func() time.Time { return observerNow }), budget.WithCreditPolicy(func(string) budget.CreditPolicy {
		return budget.CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000}
	}))
	observer, _, _ := testObserverWithCredit(t, m, map[string]string{
		"X-RateLimit-Credits-Used": "240",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	day := observerNow.UTC().Format("2006-01-02")
	var committed int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed < 240 {
		t.Fatalf("credits_committed = %d, want at least seeded 240", committed)
	}
}

type prepaidLatchCredit struct {
	*budget.Manager
	failPersist error
}

func (p *prepaidLatchCredit) ObservePrepaidRemaining(ctx context.Context, source string, remainingUSD float64) error {
	err := p.Manager.ObservePrepaidRemaining(ctx, source, remainingUSD)
	if p.failPersist != nil {
		return p.failPersist
	}
	return err
}

func TestObserverPrepaidDropTriggersStickyClosure(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	m := budget.New(s, budget.WithNow(func() time.Time { return observerNow }), budget.WithCreditPolicy(func(string) budget.CreditPolicy {
		return budget.CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000}
	}))
	wrapped := &prepaidLatchCredit{Manager: m}
	observer, _, _ := testObserverWithCredit(t, wrapped, map[string]string{
		"X-RateLimit-Prepaid-Remaining-USD": "1.0",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	wrapped.failPersist = errors.New("disk full")
	observer2, _, _ := testObserverWithCredit(t, wrapped, map[string]string{
		"X-RateLimit-Prepaid-Remaining-USD": "0.5",
	})
	if err := doAndClose(observer2, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	err = m.CommitEgress(context.Background(), budget.EgressRequest{
		Source:   config.SourceOpenAlex,
		Identity: budget.IdentityFor(config.Source{APIKey: "private-key"}),
		Credits:  1,
	})
	var exceeded *budget.ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Window != budget.WindowSticky {
		t.Fatalf("CommitEgress = %v, want sticky refusal after prepaid drop", err)
	}
}

func TestObserverFailedPrepaidPersistLeavesLatch(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	m := budget.New(s, budget.WithNow(func() time.Time { return observerNow }), budget.WithCreditPolicy(func(string) budget.CreditPolicy {
		return budget.CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000}
	}))
	wrapped := &prepaidLatchCredit{Manager: m, failPersist: errors.New("disk full")}
	observer, _, inner := testObserverWithCredit(t, wrapped, map[string]string{
		"X-RateLimit-Prepaid-Remaining-USD": "1.0",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	observer2, _, inner2 := testObserverWithCredit(t, wrapped, map[string]string{
		"X-RateLimit-Prepaid-Remaining-USD": "0.5",
	})
	if err := doAndClose(observer2, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 || inner2.calls != 1 {
		t.Fatalf("in-flight requests must succeed: calls %d and %d", inner.calls, inner2.calls)
	}
	err = m.CommitEgress(context.Background(), budget.EgressRequest{
		Source:   config.SourceOpenAlex,
		Identity: budget.IdentityFor(config.Source{APIKey: "private-key"}),
		Credits:  1,
	})
	var exceeded *budget.ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Window != budget.WindowSticky {
		t.Fatalf("CommitEgress = %v, want sticky closure latch without durable prepaid row", err)
	}
}

func TestObserverSkipsCreditObservationWithoutObserver(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	observer, _, _ := testObserverWithCredit(t, nil, map[string]string{
		"X-RateLimit-Limit":                 "10000",
		"X-RateLimit-Credits-Used":          "240",
		"X-RateLimit-Prepaid-Remaining-USD": "1.0",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	day := observerNow.UTC().Format("2006-01-02")
	var denom sql.NullInt64
	err = s.DB().QueryRowContext(context.Background(),
		`SELECT denominator FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&denom)
	if err == nil && denom.Valid {
		t.Fatalf("denominator = %d, want unset when credit observer is nil", denom.Int64)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}

func TestObserverPrimaryRecordsDenominator_guardRequired(t *testing.T) {
	rec := &recordingCredit{}
	observer, _, _ := testObserverWithCredit(t, rec, map[string]string{
		"X-RateLimit-Limit": "10000",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	if len(rec.limitCalls) != 1 {
		t.Fatalf("ObserveLimit calls = %d, want 1 with credit observer wired", len(rec.limitCalls))
	}
}

func TestObserverCreditsUsed_guardRequired(t *testing.T) {
	rec := &recordingCredit{}
	observer, _, _ := testObserverWithCredit(t, rec, map[string]string{
		"X-RateLimit-Credits-Used": "240",
	})
	if err := doAndClose(observer, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	if len(rec.usedCalls) != 1 {
		t.Fatalf("usedCalls = %v, want one ObserveCreditsUsed with credit observer wired", rec.usedCalls)
	}
	observerNil, _, _ := testObserverWithCredit(t, nil, map[string]string{
		"X-RateLimit-Credits-Used": "240",
	})
	if err := doAndClose(observerNil, bearerRequest(t)); err != nil {
		t.Fatal(err)
	}
	if len(rec.usedCalls) != 1 {
		t.Fatal("nil credit observer must not call ObserveCreditsUsed")
	}
}

// doAndClose performs the request and closes the body. The observer reads its
// credit headers off the response, so a test that leaks the body is testing the
// same code with a resource the production caller does not leak.
func doAndClose(c interface {
	Do(*http.Request) (*http.Response, error)
}, req *http.Request) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
