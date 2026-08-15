// Package sourcegate binds an HTTP client to a source's budget, so that every
// request it makes is reserved and paced like any other call to that source.
//
// It exists because papio reached OpenAlex through two clients and only one of
// them was accounted for. The acquisition resolvers call budget.Acquire at the
// job level, but internal/discovery — used by search, MCP, watch digests and
// the DOI-only enrichment path — held a bare HTTP client and drew on the same
// provider quota invisibly. The budget manager therefore under-reported papio's
// own consumption by an unknown amount, and a durable next_allowed_at gate that
// paused acquisition did not pause discovery at all: it kept calling an API
// that had already said stop.
//
// The gate lives here rather than in internal/budget so that package stays
// free of net/http, and rather than in internal/discovery so it is not specific
// to one caller.
package sourcegate

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
)

// HTTPClient is the subset of an HTTP client this package wraps. It matches
// both *fetch.SecureHTTPClient and *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Reserver is the budget manager's reservation call. Taking an interface keeps
// the dependency one-way and makes the failure modes testable without a store.
type Reserver interface {
	Acquire(ctx context.Context, source string, policy config.Source, estimatedCost float64) error
}

// Client reserves against a source budget before every request it forwards.
type Client struct {
	inner    HTTPClient
	reserve  Reserver
	source   string
	policy   config.Source
	costEach float64
}

// New wraps inner so each request reserves one call against source's budget.
//
// A nil reserver is a programming error, not a supported mode. Returning inner
// unwrapped would leave a provider client that works perfectly and is invisible
// to accounting — the exact defect this package was written to end — so letting
// a wiring mistake reproduce it silently would be self-defeating. Tests that
// genuinely want no reservation pass a no-op Reserver and say so.
func New(reserve Reserver, source string, policy config.Source, costEach float64, inner HTTPClient) (HTTPClient, error) {
	if reserve == nil {
		return nil, fmt.Errorf("sourcegate: %s has no reserver; every provider client must be accounted for", source)
	}
	if inner == nil {
		return nil, fmt.Errorf("sourcegate: %s has no inner client", source)
	}
	return &Client{inner: inner, reserve: reserve, source: source, policy: policy, costEach: costEach}, nil
}

// Do reserves, then forwards. A refused reservation is returned unwrapped so
// callers can recognise *budget.ErrDeferred and *budget.ErrExceeded with
// errors.As and report a rate limit as such rather than as a transport fault.
//
// Reserving per HTTP REQUEST rather than per logical call is deliberate: a
// discovery search that resolves a seed DOI first issues two requests, and the
// provider counts two. Accounting for one is the under-reporting this package
// exists to end.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if err := c.reserve.Acquire(req.Context(), c.source, c.policy, c.costEach); err != nil {
		return nil, err
	}
	return c.inner.Do(req)
}

// Deferrer is the narrow capability NewObserver needs from a budget manager:
// durably push a source/identity's next-allowed-at forward. Kept as an
// interface for the same reason Reserver is one — one-way dependency,
// testable without a store.
type Deferrer interface {
	Defer(ctx context.Context, source string, policy config.Source, until time.Time) error
}

// Observer wraps an HTTP client and converts a provider's own daily-budget
// headers into a durable "<source>_quota" deferral, so a keyed identity lands
// softly at a floor instead of sprinting into a day-long block. Unlike Client
// it never refuses a request itself — only future ones.
//
// It reads the X-RateLimit-Remaining/Limit/Reset headers and nothing else. A
// bare 429 is deliberately ignored: OpenAlex answers both an exhausted daily
// budget and a burst past its ~100-requests-per-second ceiling with 429, and
// the two are indistinguishable from the status and Retry-After alone. A
// rate-ceiling 429 flows through the caller's ordinary TemporaryError/Defer
// path under the bare source name, like any other retryable HTTP failure.
type Observer struct {
	inner    HTTPClient
	deferrer Deferrer
	source   string
	keyed    config.Source
	now      func() time.Time

	// mu guards latched, the process-local fail-closed floor used when the
	// durable write fails. Keyed by the identity the response was served
	// under, because the two pools are separately budgeted: the keyed tier
	// being spent says nothing about the keyless one.
	mu      sync.Mutex
	latched map[string]time.Time
}

// ErrQuotaLatched refuses a request because the provider reported this
// identity's daily budget nearly spent and the durable floor could not be
// written. It is deliberately its own type rather than a budget error: the
// observer knows nothing about admission ledgers, and the seam between them is
// the narrow Deferrer interface.
type ErrQuotaLatched struct {
	Source   string
	Identity string
	Until    time.Time
}

func (e *ErrQuotaLatched) Error() string {
	return fmt.Sprintf("source %s (%s) quota floor is latched in this process until %s",
		e.Source, e.Identity, e.Until.UTC().Format(time.RFC3339))
}

// quotaFloorDivisor sets the floor at 1/20th (5%) of the reported daily
// budget. The reported quantity is CREDITS, not requests, and the two are
// priced an order of magnitude apart — a singleton entity GET costs 1 while a
// search costs 10 — so the floor leaves ~500 credits of headroom on a
// 10,000-credit day, which is between 50 and 500 requests depending on shape.
// That is what keeps requests already in flight when the gate lands from
// overrunning the budget.
const quotaFloorDivisor = 20

// maxQuotaResetSeconds range-guards X-RateLimit-Reset before it is multiplied
// into a Duration. No daily reset is ever two days out; a negative or absurd
// value is malformed, and converting it either overflows or yields a past
// instant that budget.Defer's clamp does not repair — writing no future gate
// at the exact moment the quota is lowest.
const maxQuotaResetSeconds = 48 * 3600

// quotaDeferTimeout bounds the floor write. It is generous for one local
// SQLite upsert and short enough that a shutdown is not held open by it.
const quotaDeferTimeout = 5 * time.Second

// NewObserver wraps inner so each response's daily-budget headers can defer
// future calls. A nil deferrer or inner client is a wiring error.
func NewObserver(deferrer Deferrer, source string, keyed config.Source, inner HTTPClient) (*Observer, error) {
	if deferrer == nil {
		return nil, fmt.Errorf("sourcegate: %s observer has no deferrer", source)
	}
	if inner == nil {
		return nil, fmt.Errorf("sourcegate: %s observer has no inner client", source)
	}
	// Canonicalize the credential once, here, because every other layer already
	// does: the OpenAlex resolver trims the configured key before putting it on
	// the wire, and budget.identityFor trims it before deriving the identity. An
	// untrimmed copy here compared an outgoing "key" against a configured
	// " key ", matched neither arm of observe's switch, and silently dropped the
	// low-quota floor — so a configuration the rest of the stack deliberately
	// treats as equivalent defeated the 5% stop entirely.
	keyed.APIKey = strings.TrimSpace(keyed.APIKey)
	return &Observer{inner: inner, deferrer: deferrer, source: source, keyed: keyed, now: time.Now}, nil
}

// Do refuses a request whose identity is latched, forwards otherwise, then
// floor-defers the identity it was served under when the provider reports its
// daily budget nearly spent. Unparseable or self-inconsistent headers are a
// no-op: the observer never fails a request that already succeeded at the
// transport level. A failed Defer cannot fail that request either — it has
// already been served — but it latches this process closed for the identity so
// the NEXT one is refused here, before the wire, rather than being permitted by
// the absence of a record papio failed to write.
func (o *Observer) Do(req *http.Request) (*http.Response, error) {
	served, known := o.servedIdentity(req)
	if known {
		if until, latched := o.latchedUntil(served); latched {
			return nil, &ErrQuotaLatched{Source: o.source, Identity: poolName(served), Until: until}
		}
	}
	resp, err := o.inner.Do(req)
	if resp == nil {
		return resp, err
	}
	o.observe(req, resp)
	return resp, err
}

// servedIdentity names the identity a request goes out under, read from the
// OUTGOING request rather than from configuration: this same client serves both
// the keyed and the keyless tier, and a gate earned by one credential must
// never be attributed to the other. The comparison is against the configured
// key by VALUE, not by presence: a request bearing some third key was served
// under an identity this observer cannot name, so it reports not-known and
// callers neither latch it nor obey a latch on its behalf — guessing would
// bind the wrong pool, which is worse than binding none.
func (o *Observer) servedIdentity(req *http.Request) (config.Source, bool) {
	served := o.keyed
	sent := ""
	if req != nil && req.URL != nil {
		sent = req.URL.Query().Get("api_key")
	}
	switch sent {
	case o.keyed.APIKey:
		// served as configured, keyed or keyless alike
	case "":
		served.APIKey = ""
	default:
		return config.Source{}, false
	}
	return served, true
}

// latch closes this process's egress for one identity until the provider's own
// reset. A later, further-out floor extends it; an earlier one never shortens
// it, because the reason the latch exists is that papio could not record what
// it knows and must therefore keep the most conservative fact it has.
func (o *Observer) latch(served config.Source, until time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.latched == nil {
		o.latched = make(map[string]time.Time)
	}
	key := poolName(served)
	if existing, ok := o.latched[key]; ok && !until.After(existing) {
		return
	}
	o.latched[key] = until
}

// latchedUntil reports an active latch for served. An expired entry is deleted
// rather than left to accumulate: the map holds at most two keys, but a stale
// past instant read as "latched" would close egress forever.
func (o *Observer) latchedUntil(served config.Source) (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := poolName(served)
	until, ok := o.latched[key]
	if !ok {
		return time.Time{}, false
	}
	if !until.After(o.now()) {
		delete(o.latched, key)
		return time.Time{}, false
	}
	return until, true
}

func (o *Observer) observe(req *http.Request, resp *http.Response) {
	served, known := o.servedIdentity(req)
	if !known {
		return
	}
	remaining, remErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	limit, limErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	resetSeconds, resetErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset"))
	if remErr != nil || limErr != nil || resetErr != nil {
		return
	}
	if resetSeconds < 0 || resetSeconds > maxQuotaResetSeconds {
		return
	}
	// A budget must be positive and a balance must lie inside it. Without this
	// a malformed "limit: 0, remaining: 0" response satisfies the floor test
	// against the floor's own 1-credit minimum and gates a healthy identity
	// for a whole reset period.
	if limit <= 0 || remaining < 0 || remaining > limit {
		return
	}
	floor := limit / quotaFloorDivisor
	if floor < 1 {
		floor = 1
	}
	if remaining > floor {
		return
	}
	until := o.now().Add(time.Duration(resetSeconds) * time.Second)
	// Latch FIRST, on the parsed header, before attempting persistence. Latching
	// only after Defer fails leaves a window the width of the write — up to
	// quotaDeferTimeout — in which this process has already been told to stop and
	// keeps sending anyway, and "the durable write and the local state fail
	// together" is too narrow an assumption: a slow write blocks while a healthy
	// one commits. The latch is never cleared on success, because a successful
	// write and the latch say the same thing until the same instant, and the
	// conservative duplicate costs nothing.
	o.latch(served, until)
	// The provider has already spoken, so this write must not inherit the
	// cancellation of the request that carried the news: a shutdown racing a
	// low-quota response would otherwise drop the gate precisely when the
	// budget is lowest.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), quotaDeferTimeout)
	defer cancel()
	if err := o.deferrer.Defer(ctx, budget.QuotaSourceName(o.source), served, until); err != nil {
		// The durable row is what other processes and later restarts read, so
		// losing it matters — but it is not what makes THIS process stop, and the
		// earlier version of this code made exactly that mistake: it logged and
		// returned, converting a fact the process already possessed back into
		// permission, so a busy or full SQLite turned the safety floor into more
		// wire traffic at the moment the budget was lowest. No retry or
		// settlement machinery: losing availability for one reset period is the
		// safe side of this trade, and spending someone's prepaid balance is not.
		log.Printf("papio: %s quota low (%s pool, remaining=%d/%d) but the floor could not be recorded: %v; %s egress stays closed in this process until %s",
			o.source, poolName(served), remaining, limit, err, o.source, until.Format(time.RFC3339))
		return
	}
	log.Printf("papio: %s quota low (%s pool, remaining=%d/%d); deferred until %s",
		o.source, poolName(served), remaining, limit, until.Format(time.RFC3339))
}

// poolName names the identity a request was served under, for logs only.
func poolName(served config.Source) string {
	if served.APIKey != "" {
		return "keyed"
	}
	return "anonymous"
}
