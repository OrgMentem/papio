// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package budget gates source calls with an in-memory token bucket plus a
// durable monthly spend/retry window keyed by (source, quota identity). The
// database is authoritative across daemon restarts; tokens are deliberately
// process-local (a restart may grant one fresh burst, never more than the
// configured monetary budget).
package budget

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"papio/internal/config"
	"papio/internal/store"
)

// ErrExceeded means the configured monthly source budget would be crossed.
type ErrExceeded struct {
	Source   string
	Identity string
	Spent    float64
	Limit    float64
	Attempt  float64
}

func (e *ErrExceeded) Error() string {
	return fmt.Sprintf("source %s (%s) monthly budget exceeded: spent $%.2f + request $%.2f > limit $%.2f", e.Source, e.Identity, e.Spent, e.Attempt, e.Limit)
}

// MaxInlineWait bounds how long Acquire will block a caller on the durable
// next_allowed_at gate. A source gate is set from a server Retry-After, and
// providers routinely express a *daily quota reset* that way — OpenAlex
// answers an exhausted daily quota with a Retry-After pointing at the next UTC
// midnight, up to 24 hours out. Sleeping that out in the caller parks an
// acquisition worker for the whole window while its scheduler heartbeat keeps
// renewing the job lease, so the claim never expires and the row can never be
// reclaimed: three workers on three gated jobs froze a 309-job cohort for a
// day. Anything past this bound is the job scheduler's problem, not the
// caller's; short blips are still absorbed inline so a two-second Retry-After
// does not burn a resolver out of the chain.
const MaxInlineWait = 5 * time.Second

// ErrDeferred means the source is gated by a durable next_allowed_at further
// out than MaxInlineWait. The caller must skip the source and park its work
// until Until rather than wait.
type ErrDeferred struct {
	Source string
	// Identity is the account the deferred call was made under. A durable
	// gate is stored against that account's row; a token-bucket deferral is
	// source-wide and merely reports who it turned away.
	Identity string
	Until    time.Time
}

func (e *ErrDeferred) Error() string {
	return fmt.Sprintf("source %s (%s) is deferred until %s", e.Source, e.Identity, e.Until.UTC().Format(time.RFC3339))
}

// Snapshot is safe diagnostic state; it never contains credentials. Identity
// is a non-secret fingerprint of the credential the counters were earned
// under, never the credential itself.
type Snapshot struct {
	Source           string     `json:"source"`
	Identity         string     `json:"identity"`
	WindowStart      string     `json:"window_start"`
	RequestsInWindow int        `json:"requests_in_window"`
	SpentUSD         float64    `json:"spent_usd"`
	NextAllowedAt    *time.Time `json:"next_allowed_at,omitempty"`
}

// Manager coordinates source gates for one daemon process.
type Manager struct {
	db *sql.DB

	mu sync.Mutex
	// limiters is keyed by source name alone, deliberately unlike the durable
	// rows which are keyed by (source, identity). A token bucket is politeness
	// toward a host, and that does not double because the operator happens to
	// hold two credentials for it; the durable row tracks a provider quota,
	// which the provider does meter per credential.
	limiters map[string]*tokenBucket
	now      func() time.Time
}

type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// New binds a manager to the papio store.
func New(s *store.Store) *Manager {
	return &Manager{db: s.DB(), limiters: make(map[string]*tokenBucket), now: time.Now}
}

// identityFor names the provider account a call is made under. A provider
// meters by credential, not by source name: measured on one machine in the
// same second, OpenAlex anonymous read remaining 0 while the same source with
// an API key read 8792. Two budgets, so two rows.
//
// The fingerprint is a truncated SHA-256 of the credential and never the
// credential itself, because it is written to the database and read back in
// diagnostics. Sixty-four bits is far past collision range for the handful of
// credentials one machine holds, and short enough to read in a log line.
//
// The hash is deliberately unsalted, which is safe only because every
// credential papio accepts today is a high-entropy provider token: a bare
// 64-bit digest of a *guessable* string is reversible by dictionary, and this
// value is persisted and printed. A source whose "api_key" is low-entropy —
// an email, a username, an institution id — would need a per-install salt
// before it could be fingerprinted here.
//
// TrimSpace is not cosmetic: every client trims before deciding whether to
// send the credential, so trimming here is what keeps a whitespace-only key
// metered as the anonymous traffic it actually is.
func identityFor(policy config.Source) string {
	key := strings.TrimSpace(policy.APIKey)
	if key == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(key))
	return "key-" + hex.EncodeToString(sum[:8])
}

// Acquire waits for Retry-After and the in-memory token bucket, then atomically
// reserves one request and estimatedCost in the current UTC calendar month.
// A source with MaxCostUSD == 0 is unmetered monetarily; rate <= 0 is unthrottled.
// A durable gate further out than MaxInlineWait returns *ErrDeferred instead of
// blocking, so the caller can release its worker and park the work.
func (m *Manager) Acquire(ctx context.Context, source string, policy config.Source, estimatedCost float64) error {
	if source == "" {
		return errors.New("source name is required")
	}
	if estimatedCost < 0 || math.IsNaN(estimatedCost) || math.IsInf(estimatedCost, 0) {
		return fmt.Errorf("invalid estimated cost %.4f", estimatedCost)
	}
	if !policy.Enabled {
		return fmt.Errorf("source %s is disabled", source)
	}
	identity := identityFor(policy)

	// One deadline for the whole loop: a gate that keeps being pushed out
	// while we sleep must not extend the wait past a single MaxInlineWait.
	deadline := m.now().UTC().Add(MaxInlineWait)
	for {
		snap, err := m.Snapshot(ctx, source, policy)
		if err != nil {
			return err
		}
		now := m.now().UTC()
		if snap.NextAllowedAt == nil || !snap.NextAllowedAt.After(now) {
			break
		}
		if snap.NextAllowedAt.After(deadline) {
			return &ErrDeferred{Source: source, Identity: identity, Until: *snap.NextAllowedAt}
		}
		if err := sleepContext(ctx, snap.NextAllowedAt.Sub(now)); err != nil {
			return err
		}
	}
	if err := m.takeToken(ctx, source, identity, policy.RatePerSec, policy.Burst); err != nil {
		return err
	}
	return m.reserve(ctx, source, identity, policy.MaxCostUSD, estimatedCost)
}

// takeToken spends one token, waiting for the bucket to refill. Like the gate
// loop in Acquire it is bounded by MaxInlineWait, and for the same reason: the
// caller is usually a leased acquisition worker, and a slow refill would hold
// its claim while the heartbeat renews the lease. Contention makes the bound
// necessary rather than decorative — a waiter can lose each refilled token to
// another goroutine and never converge on its own.
func (m *Manager) takeToken(ctx context.Context, source, identity string, rate float64, burst int) error {
	if rate <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	m.mu.Lock()
	b := m.limiters[source]
	if b == nil || b.rate != rate || b.burst != float64(burst) {
		b = &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: m.now()}
		m.limiters[source] = b
	}
	m.mu.Unlock()

	deadline := m.now().Add(MaxInlineWait)
	for {
		b.mu.Lock()
		now := m.now()
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
			b.last = now
		}
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		// float64 nanoseconds overflow int64 at very low rates, which converted
		// to a non-positive Duration and turned this into a busy spin.
		seconds := (1 - b.tokens) / b.rate
		wait := MaxInlineWait
		if seconds < MaxInlineWait.Seconds() {
			wait = time.Duration(math.Ceil(seconds * float64(time.Second)))
		}
		b.mu.Unlock()
		if next := m.now().Add(wait); next.After(deadline) {
			// Same verdict as a durable gate from the caller's side: this source
			// cannot serve you now, release the worker and park. Deliberately
			// never persisted through Defer — a token is process-local and
			// advisory, and another caller may take it first.
			return &ErrDeferred{Source: source, Identity: identity, Until: next.UTC()}
		}
		if err := sleepContext(ctx, wait); err != nil {
			return err
		}
	}
}

func (m *Manager) reserve(ctx context.Context, source, identity string, limit, cost float64) error {
	now := m.now().UTC()
	month := now.Format("2006-01")

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_budgets(source, identity, window_start) VALUES(?, ?, ?)
		ON CONFLICT(source, identity) DO NOTHING`, source, identity, month); err != nil {
		return err
	}
	var window string
	var requests int
	var spent float64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(window_start,''), requests_in_window, spent_usd
		FROM source_budgets WHERE source = ? AND identity = ?`, source, identity).Scan(&window, &requests, &spent); err != nil {
		return err
	}
	if window != month {
		window, requests, spent = month, 0, 0
	}
	if limit > 0 && spent+cost > limit+1e-9 {
		return &ErrExceeded{Source: source, Identity: identity, Spent: spent, Limit: limit, Attempt: cost}
	}
	_, err = tx.ExecContext(ctx, `UPDATE source_budgets
		SET window_start = ?, requests_in_window = ?, spent_usd = ?,
		    next_allowed_at = CASE WHEN next_allowed_at <= ? THEN NULL ELSE next_allowed_at END
		WHERE source = ? AND identity = ?`, window, requests+1, spent+cost, store.Now(), source, identity)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// MaxDeferHorizon bounds how far a server may push a source out. Every real
// quota resets within a day — OpenAlex answers an exhausted daily allowance
// with a Retry-After pointing at the next UTC midnight — so anything beyond
// this is a clock skew, a malformed header or a bug, and papio believing it
// would park every job needing that source for as long as the provider said,
// with no way back except editing the database. Clamping costs one wasted
// request when the gate turns out to still be closed; not clamping costs the
// source until someone notices.
const MaxDeferHorizon = 24 * time.Hour

// Defer persists a server-requested next allowed time (usually Retry-After)
// against the quota identity in policy, so a gate earned under one credential
// never gates another. It never shortens an existing later gate, and never
// honours one beyond MaxDeferHorizon.
func (m *Manager) Defer(ctx context.Context, source string, policy config.Source, until time.Time) error {
	until = until.UTC()
	if horizon := m.now().UTC().Add(MaxDeferHorizon); until.After(horizon) {
		until = horizon
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO source_budgets(source, identity, window_start, next_allowed_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(source, identity) DO UPDATE SET next_allowed_at =
		CASE WHEN next_allowed_at IS NULL OR next_allowed_at < excluded.next_allowed_at
		     THEN excluded.next_allowed_at ELSE next_allowed_at END`,
		source, identityFor(policy), m.now().UTC().Format("2006-01"), until.Format(time.RFC3339Nano))
	return err
}

// Snapshot returns one source's durable counters for the quota identity in
// policy. A missing row is zero state.
func (m *Manager) Snapshot(ctx context.Context, source string, policy config.Source) (Snapshot, error) {
	var out Snapshot
	out.Source = source
	out.Identity = identityFor(policy)
	var next sql.NullString
	err := m.db.QueryRowContext(ctx, `SELECT COALESCE(window_start,''), requests_in_window, spent_usd, next_allowed_at
		FROM source_budgets WHERE source = ? AND identity = ?`, source, out.Identity).Scan(&out.WindowStart, &out.RequestsInWindow, &out.SpentUSD, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if next.Valid && next.String != "" {
		t, err := time.Parse(time.RFC3339Nano, next.String)
		if err != nil {
			return out, fmt.Errorf("source %s (%s) has invalid next_allowed_at: %w", source, out.Identity, err)
		}
		out.NextAllowedAt = &t
	}
	return out, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
