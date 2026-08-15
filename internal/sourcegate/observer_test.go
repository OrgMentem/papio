// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"papio/internal/config"
)

type fakeQuotaFloor struct {
	*fakeDeferrer
	latches map[string]time.Time
	now     func() time.Time
}

func (f *fakeQuotaFloor) latchKey(source, identity string) string {
	return source + "\x00" + identity
}

func (f *fakeQuotaFloor) LatchQuota(source, identity string, until time.Time) {
	if f.latches == nil {
		f.latches = make(map[string]time.Time)
	}
	f.latches[f.latchKey(source, identity)] = until.UTC()
}

func (f *fakeQuotaFloor) QuotaLatchedUntil(source, identity string) (time.Time, bool) {
	if f.latches == nil {
		return time.Time{}, false
	}
	until, ok := f.latches[f.latchKey(source, identity)]
	now := observerNow
	if f.now != nil {
		now = f.now()
	}
	if !ok || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

type deferCall struct {
	source string
	policy config.Source
	until  time.Time
}

type fakeDeferrer struct {
	calls       []deferCall
	err         error
	deferCtxErr error
}

func (f *fakeDeferrer) Defer(ctx context.Context, source string, policy config.Source, until time.Time) error {
	f.deferCtxErr = ctx.Err()
	f.calls = append(f.calls, deferCall{source: source, policy: policy, until: until})
	return f.err
}

type headerClient struct {
	status  int
	headers map[string]string
	calls   int
}

func (c *headerClient) Do(*http.Request) (*http.Response, error) {
	c.calls++
	h := make(http.Header)
	for k, v := range c.headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: c.status, Header: h, Body: http.NoBody}, nil
}

var observerNow = time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)

func testObserver(t *testing.T, status int, headers map[string]string) (*Observer, *fakeQuotaFloor, *headerClient) {
	t.Helper()
	deferrer := &fakeDeferrer{}
	now := func() time.Time { return observerNow }
	floor := &fakeQuotaFloor{fakeDeferrer: deferrer, now: now}
	inner := &headerClient{status: status, headers: headers}
	observer, err := NewObserver(floor, nil, "openalex", config.Source{Enabled: true, APIKey: "private-key"}, inner)
	if err != nil {
		t.Fatal(err)
	}
	observer.now = now
	floor.now = now
	return observer, floor, inner
}

func observerRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{Method: http.MethodGet, URL: parsed}
}

func TestObserverDefersAtFloor(t *testing.T) {
	observer, floor, inner := testObserver(t, http.StatusOK, map[string]string{
		"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "400", "X-RateLimit-Reset": "3600",
	})
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want the request forwarded", inner.calls)
	}
	if len(floor.fakeDeferrer.calls) != 1 {
		t.Fatalf("Defer calls = %#v, want exactly one floor deferral", floor.fakeDeferrer.calls)
	}
	got := floor.fakeDeferrer.calls[0]
	if got.source != "openalex_quota" {
		t.Fatalf("source = %q, want openalex_quota: only the header signal writes that row", got.source)
	}
	if got.policy.APIKey != "private-key" {
		t.Fatalf("policy = %+v, want the keyed identity the request was served under", got.policy)
	}
	if want := observerNow.Add(time.Hour); !got.until.Equal(want) {
		t.Fatalf("until = %v, want the reported reset %v", got.until, want)
	}
}

func TestObserverIgnoresHealthyRemaining(t *testing.T) {
	observer, floor, _ := testObserver(t, http.StatusOK, map[string]string{
		"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "9000", "X-RateLimit-Reset": "3600",
	})
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 0 {
		t.Fatalf("Defer calls = %#v, want none with the budget healthy", floor.fakeDeferrer.calls)
	}
}

// A 429 alone is ambiguous: OpenAlex answers both an exhausted daily budget and
// a burst past its per-second ceiling that way. Only the daily-budget headers
// may trigger the quota floor.
func TestObserverIgnoresRateCeiling429(t *testing.T) {
	observer, floor, _ := testObserver(t, http.StatusTooManyRequests, map[string]string{
		"Retry-After": "120", "X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "9000", "X-RateLimit-Reset": "3600",
	})
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 0 {
		t.Fatalf("Defer calls = %#v, want none: a rate-ceiling 429 is not quota exhaustion", floor.fakeDeferrer.calls)
	}
}

func TestObserverLowRemaining429(t *testing.T) {
	observer, floor, _ := testObserver(t, http.StatusTooManyRequests, map[string]string{
		"Retry-After": "120", "X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "3", "X-RateLimit-Reset": "7200",
	})
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 1 {
		t.Fatalf("Defer calls = %#v, want the header-derived deferral", floor.fakeDeferrer.calls)
	}
	if want := observerNow.Add(2 * time.Hour); !floor.fakeDeferrer.calls[0].until.Equal(want) {
		t.Fatalf("until = %v, want the reported reset %v, never Retry-After", floor.fakeDeferrer.calls[0].until, want)
	}
}

func TestObserverAnonymousIdentity(t *testing.T) {
	observer, floor, _ := testObserver(t, http.StatusOK, map[string]string{
		"X-RateLimit-Limit": "1000", "X-RateLimit-Remaining": "10", "X-RateLimit-Reset": "600",
	})
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?mailto=reader@example.org")); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 1 {
		t.Fatalf("Defer calls = %#v, want one", floor.fakeDeferrer.calls)
	}
	if floor.fakeDeferrer.calls[0].policy.APIKey != "" {
		t.Fatalf("policy = %+v, want the anonymous identity: the request carried no api_key", floor.fakeDeferrer.calls[0].policy)
	}
}

func TestObserverRejectsMalformedReset(t *testing.T) {
	for _, reset := range []string{"-5", "10000000", "soon"} {
		t.Run(reset, func(t *testing.T) {
			observer, floor, _ := testObserver(t, http.StatusOK, map[string]string{
				"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "1", "X-RateLimit-Reset": reset,
			})
			if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
				t.Fatal(err)
			}
			if len(floor.fakeDeferrer.calls) != 0 {
				t.Fatalf("Defer calls = %#v, want none: a malformed reset cannot write an honest gate", floor.fakeDeferrer.calls)
			}
		})
	}
}

// The provider has already spoken by the time the floor is written, so a
// request context cancelled by a racing shutdown must not discard the only
// durable record that this identity has to stop.
func TestObserverFloorSurvivesRequestCancellation(t *testing.T) {
	observer, floor, _ := testObserver(t, http.StatusOK, map[string]string{
		"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "12", "X-RateLimit-Reset": "3600",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := observerRequest(t, "https://api.openalex.org/works?api_key=private-key").WithContext(ctx)
	if _, err := observer.Do(req); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 1 {
		t.Fatalf("Defer calls = %#v, want the floor recorded despite the cancelled request", floor.fakeDeferrer.calls)
	}
	if err := floor.fakeDeferrer.deferCtxErr; err != nil {
		t.Fatalf("Defer context error = %v, want a live context detached from the request", err)
	}
}

// A request bearing some third credential was served under an identity this
// observer cannot name. Attributing it to the configured key would gate the
// wrong pool, which is worse than gating none.
func TestObserverIgnoresUnknownCredential(t *testing.T) {
	observer, floor, _ := testObserver(t, http.StatusOK, map[string]string{
		"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "12", "X-RateLimit-Reset": "3600",
	})
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=someone-elses-key")); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 0 {
		t.Fatalf("Defer calls = %#v, want none for an unrecognised credential", floor.fakeDeferrer.calls)
	}
}

// A budget must be positive and a balance must lie inside it, or the floor's
// own one-credit minimum gates a healthy identity on a malformed response.
func TestObserverRejectsSelfInconsistentBudget(t *testing.T) {
	for _, test := range []struct {
		name      string
		limit     string
		remaining string
	}{
		{name: "zero limit", limit: "0", remaining: "0"},
		{name: "negative limit", limit: "-1", remaining: "0"},
		{name: "negative remaining", limit: "10000", remaining: "-5"},
		{name: "remaining exceeds limit", limit: "10", remaining: "99"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer, floor, _ := testObserver(t, http.StatusOK, map[string]string{
				"X-RateLimit-Limit": test.limit, "X-RateLimit-Remaining": test.remaining, "X-RateLimit-Reset": "3600",
			})
			if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
				t.Fatal(err)
			}
			if len(floor.fakeDeferrer.calls) != 0 {
				t.Fatalf("Defer calls = %#v, want none: the reported budget is not self-consistent", floor.fakeDeferrer.calls)
			}
		})
	}
}

// The resolver trims the configured key before putting it on the wire and
// budget.identityFor trims it before deriving the identity, so a configured
// " key " must not read as a credential this observer cannot name. It used to:
// the untrimmed comparison matched neither arm and silently dropped the floor.
func TestObserverCanonicalizesTheConfiguredCredential(t *testing.T) {
	deferrer := &fakeDeferrer{}
	inner := &headerClient{status: http.StatusOK, headers: map[string]string{
		"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "12", "X-RateLimit-Reset": "3600",
	}}
	observer, err := NewObserver(&fakeQuotaFloor{fakeDeferrer: deferrer}, nil, "openalex", config.Source{Enabled: true, APIKey: "  private-key\t"}, inner)
	if err != nil {
		t.Fatal(err)
	}
	observer.now = func() time.Time { return observerNow }
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if len(deferrer.calls) != 1 {
		t.Fatalf("Defer calls = %#v, want the floor recorded for the trimmed credential", deferrer.calls)
	}
	if got := deferrer.calls[0].policy.APIKey; got != "private-key" {
		t.Fatalf("floor identity key = %q, want the canonical %q", got, "private-key")
	}
}

// A failed floor write must not hand back permission. The provider has already
// said this identity is nearly spent; losing the durable record is a reason to
// stop, not a reason to keep sending.
func TestFailedFloorWriteLatchesEgressClosed(t *testing.T) {
	headers := map[string]string{
		"X-RateLimit-Remaining": "100",
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Reset":     "3600",
	}
	observer, floor, inner := testObserver(t, 200, headers)
	floor.fakeDeferrer.err = errors.New("database is locked")

	// The response that carried the news is served: it already happened.
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatalf("first call = %v, want the already-served response", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}

	// The next one is refused here, before the wire.
	_, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key"))
	var latched *ErrQuotaLatched
	if !errors.As(err, &latched) {
		t.Fatalf("second call = %v, want *ErrQuotaLatched", err)
	}
	if want := observerNow.Add(time.Hour); !latched.Until.Equal(want) {
		t.Fatalf("latched until %s, want the provider's own reset %s", latched.Until, want)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want no second request", inner.calls)
	}
}

// The two pools are separately budgeted, so a keyed latch must not close the
// keyless tier that the fallback path exists to reach.
func TestLatchIsPerIdentity(t *testing.T) {
	headers := map[string]string{
		"X-RateLimit-Remaining": "100",
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Reset":     "3600",
	}
	observer, floor, inner := testObserver(t, 200, headers)
	floor.fakeDeferrer.err = errors.New("disk full")
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works")); err != nil {
		t.Fatalf("keyless call = %v, want it unaffected by the keyed latch", err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls = %d, want the keyless request forwarded", inner.calls)
	}
}

// A latch expires at the provider's reset, not at process exit: a stale past
// instant read as "still latched" would close egress forever.
func TestLatchExpiresAtReset(t *testing.T) {
	headers := map[string]string{
		"X-RateLimit-Remaining": "100",
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Reset":     "3600",
	}
	observer, floor, inner := testObserver(t, 200, headers)
	floor.fakeDeferrer.err = errors.New("database is locked")
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	observer.now = func() time.Time { return observerNow.Add(2 * time.Hour) }
	floor.now = observer.now
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatalf("post-reset call = %v, want the latch expired", err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls = %d, want the post-reset request forwarded", inner.calls)
	}
}

// An unnameable third credential is neither latched nor governed by a latch:
// the observer refuses to attribute it, and attributing it either way is wrong.
func TestLatchIgnoresUnnameableCredential(t *testing.T) {
	headers := map[string]string{
		"X-RateLimit-Remaining": "100",
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Reset":     "3600",
	}
	observer, floor, inner := testObserver(t, 200, headers)
	floor.fakeDeferrer.err = errors.New("database is locked")
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=someone-elses")); err != nil {
		t.Fatalf("third-credential call = %v, want no latch applied to an identity we cannot name", err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls = %d, want the unnameable request forwarded", inner.calls)
	}
}

// The latch must not be conditional on the write FAILING. A successful durable
// write and a slow one are indistinguishable to the caller that arrives during
// it, so the process-local stop is set from the parsed header. Here Defer
// succeeds, and the next request is still refused before the wire.
func TestFloorLatchesOnTheParsedHeaderNotOnAFailedWrite(t *testing.T) {
	headers := map[string]string{
		"X-RateLimit-Remaining": "100",
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Reset":     "3600",
	}
	observer, floor, inner := testObserver(t, 200, headers)
	if _, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if len(floor.fakeDeferrer.calls) != 1 {
		t.Fatalf("Defer calls = %d, want the durable floor written too", len(floor.fakeDeferrer.calls))
	}
	_, err := observer.Do(observerRequest(t, "https://api.openalex.org/works?api_key=private-key"))
	var latched *ErrQuotaLatched
	if !errors.As(err, &latched) {
		t.Fatalf("second call = %v, want *ErrQuotaLatched even though the write succeeded", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want no second request", inner.calls)
	}
}
