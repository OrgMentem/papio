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
	"time"

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

// Do forwards the request, then floor-defers the identity it was served under
// when the provider reports its daily budget nearly spent. Unparseable or
// self-inconsistent headers are a no-op: the observer never fails a request
// that already succeeded at the transport level. A failed Defer cannot fail
// the request either, but it is logged loudly — it is the only durable record
// that the provider asked papio to stop, and losing it silently is how a
// quota gets spent twice.
func (o *Observer) Do(req *http.Request) (*http.Response, error) {
	resp, err := o.inner.Do(req)
	if resp == nil {
		return resp, err
	}
	o.observe(req, resp)
	return resp, err
}

func (o *Observer) observe(req *http.Request, resp *http.Response) {
	// The identity is read from the OUTGOING request, not from configuration:
	// this same client serves both the keyed and the keyless tier, and a gate
	// earned by one credential must never be written against the other. The
	// comparison is against the configured key by VALUE, not by presence: a
	// request bearing some third key was served under an identity this
	// observer cannot name, and guessing would write the gate against the
	// wrong pool — worse than writing none.
	served := o.keyed
	sent := ""
	if req.URL != nil {
		sent = req.URL.Query().Get("api_key")
	}
	switch sent {
	case o.keyed.APIKey:
		// served as configured, keyed or keyless alike
	case "":
		served.APIKey = ""
	default:
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
	// The provider has already spoken, so this write must not inherit the
	// cancellation of the request that carried the news: a shutdown racing a
	// low-quota response would otherwise drop the gate precisely when the
	// budget is lowest.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), quotaDeferTimeout)
	defer cancel()
	if err := o.deferrer.Defer(ctx, o.source+"_quota", served, until); err != nil {
		log.Printf("papio: %s quota low (%s pool, remaining=%d/%d) but the floor could not be recorded: %v",
			o.source, poolName(served), remaining, limit, err)
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
