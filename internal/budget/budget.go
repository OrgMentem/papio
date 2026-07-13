// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package budget gates source calls with an in-memory token bucket plus a
// durable monthly spend/retry window. The database is authoritative across
// daemon restarts; tokens are deliberately process-local (a restart may grant
// one fresh burst, never more than the configured monetary budget).
package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"papio/internal/config"
	"papio/internal/store"
)

// ErrExceeded means the configured monthly source budget would be crossed.
type ErrExceeded struct {
	Source  string
	Spent   float64
	Limit   float64
	Attempt float64
}

func (e *ErrExceeded) Error() string {
	return fmt.Sprintf("source %s monthly budget exceeded: spent $%.2f + request $%.2f > limit $%.2f", e.Source, e.Spent, e.Attempt, e.Limit)
}

// Snapshot is safe diagnostic state; it never contains credentials.
type Snapshot struct {
	Source           string     `json:"source"`
	WindowStart      string     `json:"window_start"`
	RequestsInWindow int        `json:"requests_in_window"`
	SpentUSD         float64    `json:"spent_usd"`
	NextAllowedAt    *time.Time `json:"next_allowed_at,omitempty"`
}

// Manager coordinates source gates for one daemon process.
type Manager struct {
	db *sql.DB

	mu       sync.Mutex
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

// Acquire waits for Retry-After and the in-memory token bucket, then atomically
// reserves one request and estimatedCost in the current UTC calendar month.
// A source with MaxCostUSD == 0 is unmetered monetarily; rate <= 0 is unthrottled.
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

	for {
		snap, err := m.Snapshot(ctx, source)
		if err != nil {
			return err
		}
		if snap.NextAllowedAt == nil || !snap.NextAllowedAt.After(m.now().UTC()) {
			break
		}
		if err := sleepContext(ctx, time.Until(*snap.NextAllowedAt)); err != nil {
			return err
		}
	}
	if err := m.takeToken(ctx, source, policy.RatePerSec, policy.Burst); err != nil {
		return err
	}
	return m.reserve(ctx, source, policy.MaxCostUSD, estimatedCost)
}

func (m *Manager) takeToken(ctx context.Context, source string, rate float64, burst int) error {
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
		wait := time.Duration(math.Ceil((1 - b.tokens) / b.rate * float64(time.Second)))
		b.mu.Unlock()
		if err := sleepContext(ctx, wait); err != nil {
			return err
		}
	}
}

func (m *Manager) reserve(ctx context.Context, source string, limit, cost float64) error {
	now := m.now().UTC()
	month := now.Format("2006-01")

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_budgets(source, window_start) VALUES(?, ?)
		ON CONFLICT(source) DO NOTHING`, source, month); err != nil {
		return err
	}
	var window string
	var requests int
	var spent float64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(window_start,''), requests_in_window, spent_usd
		FROM source_budgets WHERE source = ?`, source).Scan(&window, &requests, &spent); err != nil {
		return err
	}
	if window != month {
		window, requests, spent = month, 0, 0
	}
	if limit > 0 && spent+cost > limit+1e-9 {
		return &ErrExceeded{Source: source, Spent: spent, Limit: limit, Attempt: cost}
	}
	_, err = tx.ExecContext(ctx, `UPDATE source_budgets
		SET window_start = ?, requests_in_window = ?, spent_usd = ?,
		    next_allowed_at = CASE WHEN next_allowed_at <= ? THEN NULL ELSE next_allowed_at END
		WHERE source = ?`, window, requests+1, spent+cost, store.Now(), source)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Defer persists a server-requested next allowed time (usually Retry-After).
// It never shortens an existing later gate.
func (m *Manager) Defer(ctx context.Context, source string, until time.Time) error {
	until = until.UTC()
	_, err := m.db.ExecContext(ctx, `INSERT INTO source_budgets(source, window_start, next_allowed_at)
		VALUES(?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET next_allowed_at =
		CASE WHEN next_allowed_at IS NULL OR next_allowed_at < excluded.next_allowed_at
		     THEN excluded.next_allowed_at ELSE next_allowed_at END`,
		source, m.now().UTC().Format("2006-01"), until.Format(time.RFC3339Nano))
	return err
}

// ClearDefer removes a persisted Retry-After gate after an operator-approved
// reset or successful call.
func (m *Manager) ClearDefer(ctx context.Context, source string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE source_budgets SET next_allowed_at = NULL WHERE source = ?`, source)
	return err
}

// Snapshot returns one source's durable counters. A missing row is zero state.
func (m *Manager) Snapshot(ctx context.Context, source string) (Snapshot, error) {
	var out Snapshot
	out.Source = source
	var next sql.NullString
	err := m.db.QueryRowContext(ctx, `SELECT COALESCE(window_start,''), requests_in_window, spent_usd, next_allowed_at
		FROM source_budgets WHERE source = ?`, source).Scan(&out.WindowStart, &out.RequestsInWindow, &out.SpentUSD, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if next.Valid && next.String != "" {
		t, err := time.Parse(time.RFC3339Nano, next.String)
		if err != nil {
			return out, fmt.Errorf("source %s has invalid next_allowed_at: %w", source, err)
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
