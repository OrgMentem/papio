// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// BootstrapCreditCap is the bounded egress permitted before a primary identity
// establishes the day's denominator.
const BootstrapCreditCap = 50

// ColdStartCreditCap is the conservative absolute allowance used when the
// bootstrap window is exhausted but no denominator has been captured yet.
const ColdStartCreditCap = 500

// egressTestHooks toggles CommitEgress guards for mutation-resistant tests.
var egressTestHooks = struct {
	enforceDebitLimit  bool
	enforceEgressGates bool
}{
	enforceDebitLimit:  true,
	enforceEgressGates: true,
}

// BudgetKind names which local budget refused egress.
type BudgetKind int

const (
	KindCredits BudgetKind = iota
	KindUSD
)

func (k BudgetKind) String() string {
	switch k {
	case KindCredits:
		return "credits"
	case KindUSD:
		return "usd"
	default:
		return fmt.Sprintf("budget_kind(%d)", k)
	}
}

// Window names the reset horizon for a local budget refusal.
type Window int

const (
	WindowUTCDay Window = iota
	WindowMonth
	WindowSticky
)

func (w Window) String() string {
	switch w {
	case WindowUTCDay:
		return "utc_day"
	case WindowMonth:
		return "month"
	case WindowSticky:
		return "sticky"
	default:
		return fmt.Sprintf("window(%d)", w)
	}
}

// EgressRequest is the wire-side credit commit. Identity is the outgoing
// credential fingerprint, never a construction-time policy default.
type EgressRequest struct {
	Source   string
	Identity string
	Credits  int
}

type quotaLatch struct {
	until time.Time
}

type driftLatch struct {
	reason string
}

// CreditPolicy configures the source-wide daily credit fuse.
type CreditPolicy struct {
	DailyCreditFraction float64
	DailyCreditLimit    int // hard maximum; 0 = no absolute cap, not unmetered
}

// WithCreditPolicy supplies per-source fuse knobs for CommitEgress. When nil,
// fraction defaults to 0.5 with no hard maximum (limit 0).
func WithCreditPolicy(fn func(source string) CreditPolicy) Option {
	return func(m *Manager) {
		m.creditPolicy = fn
	}
}

func (m *Manager) creditPolicyFor(source string) CreditPolicy {
	if m.creditPolicy != nil {
		return m.creditPolicy(source)
	}
	return CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 0}
}

func utcDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func nextUTCMidnight(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
}

func nextUTCMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

func latchKey(source, identity string) string {
	return source + "\x00" + identity
}

// LatchQuota records a provider-reported near-exhaustion for one credential
// process-locally, immediately, before any durable write is attempted.
func (m *Manager) LatchQuota(source, identity string, until time.Time) {
	if source == "" || identity == "" {
		return
	}
	m.latchMu.Lock()
	defer m.latchMu.Unlock()
	if m.quotaLatches == nil {
		m.quotaLatches = make(map[string]quotaLatch)
	}
	m.quotaLatches[latchKey(source, identity)] = quotaLatch{until: until.UTC()}
}

func (m *Manager) latchedQuotaUntil(source, identity string) *time.Time {
	m.latchMu.Lock()
	defer m.latchMu.Unlock()
	entry, ok := m.quotaLatches[latchKey(source, identity)]
	if !ok {
		return nil
	}
	now := m.now().UTC()
	if !entry.until.After(now) {
		delete(m.quotaLatches, latchKey(source, identity))
		return nil
	}
	t := entry.until
	return &t
}

// QuotaLatchedUntil reports an active process-local quota latch.
func (m *Manager) QuotaLatchedUntil(source, identity string) (time.Time, bool) {
	if until := m.latchedQuotaUntil(source, identity); until != nil {
		return *until, true
	}
	return time.Time{}, false
}

func (m *Manager) setDriftLatch(source, reason string) {
	m.latchMu.Lock()
	defer m.latchMu.Unlock()
	if m.driftLatches == nil {
		m.driftLatches = make(map[string]driftLatch)
	}
	m.driftLatches[source] = driftLatch{reason: reason}
}

func (m *Manager) driftLatched(source string) (bool, string) {
	m.latchMu.Lock()
	defer m.latchMu.Unlock()
	entry, ok := m.driftLatches[source]
	return ok, entry.reason
}

type fuseRow struct {
	committed   int
	denominator sql.NullInt64
	driftClosed sql.NullString
}

func (m *Manager) allowanceFor(row fuseRow, policy CreditPolicy) (limit int, unmetered bool) {
	// 0 fraction disables the ceiling per plan (matching 0=unmetered for
	// budgets) — but every request still commits. Only DailyCreditLimit==0
	// is not unmetered; it is "no hard maximum".
	if policy.DailyCreditFraction == 0 {
		return 0, true
	}
	fraction := policy.DailyCreditFraction

	var basis int
	if row.denominator.Valid {
		basis = int(math.Round(fraction * float64(row.denominator.Int64)))
	} else if row.committed >= BootstrapCreditCap {
		basis = ColdStartCreditCap
	} else {
		basis = BootstrapCreditCap
	}
	limit = basis
	if policy.DailyCreditLimit != 0 && limit > policy.DailyCreditLimit {
		limit = policy.DailyCreditLimit
	}
	return limit, false
}

// CommitEgress is the egress authority. It revalidates EVERY durable no-egress
// authority for the outgoing identity and debits the request shape's
// conservative credit cost in ONE transaction. Returns *ErrDeferred for a gate,
// *ErrExceeded for the fuse. No blocking wait may follow a nil return.
func (m *Manager) CommitEgress(ctx context.Context, req EgressRequest) error {
	if req.Source == "" {
		return errors.New("source name is required")
	}
	if req.Identity == "" {
		return errors.New("egress identity is required")
	}
	if req.Credits < 1 {
		return fmt.Errorf("invalid egress credit cost %d", req.Credits)
	}
	if latched, _ := m.driftLatched(req.Source); latched {
		return &ErrExceeded{
			Source:   req.Source,
			Identity: req.Identity,
			Kind:     KindCredits,
			Window:   WindowSticky,
		}
	}
	now := m.now().UTC()
	day := utcDay(now)
	policy := m.creditPolicyFor(req.Source)

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	row, err := m.readFuseRow(ctx, tx, req.Source, day)
	if err != nil {
		return err
	}
	if row.driftClosed.Valid && row.driftClosed.String != "" {
		return &ErrExceeded{
			Source:   req.Source,
			Identity: req.Identity,
			Kind:     KindCredits,
			Window:   WindowSticky,
		}
	}

	if egressTestHooks.enforceEgressGates {
		if err := m.checkEgressGates(ctx, tx, req, now); err != nil {
			return err
		}
	}

	limit, unmetered := m.allowanceFor(row, policy)
	if egressTestHooks.enforceDebitLimit && !unmetered && row.committed+req.Credits > limit {
		return creditExceeded(req, row.committed, req.Credits, limit, now)
	}

	debitUnmetered := unmetered || !egressTestHooks.enforceDebitLimit
	affected, err := m.debitCredits(ctx, tx, req.Source, day, req.Credits, limit, debitUnmetered)
	if err != nil {
		return err
	}
	if affected == 0 {
		row, err = m.readFuseRow(ctx, tx, req.Source, day)
		if err != nil {
			return err
		}
		return creditExceeded(req, row.committed, req.Credits, limit, now)
	}
	return tx.Commit()
}

func creditExceeded(req EgressRequest, committed, attempt, limit int, now time.Time) *ErrExceeded {
	return &ErrExceeded{
		Source:    req.Source,
		Identity:  req.Identity,
		Kind:      KindCredits,
		Window:    WindowUTCDay,
		Until:     nextUTCMidnight(now),
		Committed: committed,
		Attempt:   float64(attempt),
		Limit:     float64(limit),
	}
}

func (m *Manager) readFuseRow(ctx context.Context, tx *sql.Tx, source, day string) (fuseRow, error) {
	var row fuseRow
	err := tx.QueryRowContext(ctx, `SELECT credits_committed, denominator, drift_closed_at
		FROM source_credit_fuse WHERE source = ? AND utc_day = ?`, source, day).
		Scan(&row.committed, &row.denominator, &row.driftClosed)
	if errors.Is(err, sql.ErrNoRows) {
		return fuseRow{}, nil
	}
	return row, err
}

func (m *Manager) checkEgressGates(ctx context.Context, tx *sql.Tx, req EgressRequest, now time.Time) error {
	if until := m.latchedQuotaUntil(req.Source, req.Identity); until != nil {
		return &ErrDeferred{Source: req.Source, Identity: req.Identity, Until: *until, Quota: true}
	}

	if err := m.checkNextAllowed(ctx, tx, req.Source, req.Source, req.Identity, now, false); err != nil {
		return err
	}
	if err := m.checkNextAllowed(ctx, tx, req.Source, PacingSourceName(req.Source), pacingIdentity(), now, false); err != nil {
		return err
	}
	if err := m.checkNextAllowed(ctx, tx, req.Source, QuotaSourceName(req.Source), req.Identity, now, true); err != nil {
		return err
	}
	return nil
}

func (m *Manager) checkNextAllowed(ctx context.Context, tx *sql.Tx, displaySource, rowSource, identity string, now time.Time, quota bool) error {
	var next sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT next_allowed_at FROM source_budgets
		WHERE source = ? AND identity = ?`, rowSource, identity).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !next.Valid || next.String == "" {
		return nil
	}
	gate, err := time.Parse(time.RFC3339Nano, next.String)
	if err != nil {
		return fmt.Errorf("source %s (%s) has invalid next_allowed_at: %w", rowSource, identity, err)
	}
	if gate.After(now) {
		return &ErrDeferred{Source: displaySource, Identity: identity, Until: gate, Quota: quota}
	}
	return nil
}

func (m *Manager) debitCredits(ctx context.Context, tx *sql.Tx, source, day string, credits, limit int, unmetered bool) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO source_credit_fuse (source, utc_day) VALUES (?, ?)`, source, day); err != nil {
		return 0, err
	}
	if unmetered {
		res, err := tx.ExecContext(ctx, `UPDATE source_credit_fuse
			SET credits_committed = credits_committed + ?
			WHERE source = ? AND utc_day = ? AND drift_closed_at IS NULL`,
			credits, source, day)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	res, err := tx.ExecContext(ctx, `UPDATE source_credit_fuse
		SET credits_committed = credits_committed + ?
		WHERE source = ? AND utc_day = ?
		  AND drift_closed_at IS NULL
		  AND credits_committed + ? <= ?`,
		credits, source, day, credits, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ObserveLimit records a provider-reported daily limit for the day's basis.
// Monotone: it may lower the captured denominator, never raise it, and only the
// configured primary identity establishes it.
func (m *Manager) ObserveLimit(ctx context.Context, source, identity string, limit int, primary bool) error {
	if !primary || source == "" || limit < 1 {
		return nil
	}
	day := utcDay(m.now())
	_, err := m.db.ExecContext(ctx, `INSERT INTO source_credit_fuse (source, utc_day, denominator)
		VALUES (?, ?, ?)
		ON CONFLICT(source, utc_day) DO UPDATE SET denominator = CASE
			WHEN denominator IS NULL THEN excluded.denominator
			WHEN excluded.denominator < denominator THEN excluded.denominator
			ELSE denominator
		END`, source, day, limit)
	return err
}

// ObserveCreditsUsed seeds the day's counter from the provider's credits-used
// header on first observation.
func (m *Manager) ObserveCreditsUsed(ctx context.Context, source string, used int) error {
	if source == "" || used < 0 {
		return nil
	}
	day := utcDay(m.now())
	_, err := m.db.ExecContext(ctx, `INSERT INTO source_credit_fuse (source, utc_day, credits_committed, credits_used_seed)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(source, utc_day) DO UPDATE SET
			credits_used_seed = COALESCE(source_credit_fuse.credits_used_seed, excluded.credits_used_seed),
			credits_committed = MAX(source_credit_fuse.credits_committed, excluded.credits_used_seed)`,
		source, day, used, used)
	return err
}

// ObservePrepaidRemaining records prepaid balance; egress closes when it falls
// below the first value observed for the day.
func (m *Manager) ObservePrepaidRemaining(ctx context.Context, source string, remainingUSD float64) error {
	if source == "" || math.IsNaN(remainingUSD) || math.IsInf(remainingUSD, 0) {
		return nil
	}
	day := utcDay(m.now())
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_credit_fuse (source, utc_day, prepaid_baseline_usd)
		VALUES (?, ?, ?)
		ON CONFLICT(source, utc_day) DO UPDATE SET
			prepaid_baseline_usd = COALESCE(prepaid_baseline_usd, excluded.prepaid_baseline_usd)`,
		source, day, remainingUSD); err != nil {
		return err
	}
	var baseline sql.NullFloat64
	if err := tx.QueryRowContext(ctx, `SELECT prepaid_baseline_usd FROM source_credit_fuse
		WHERE source = ? AND utc_day = ?`, source, day).Scan(&baseline); err != nil {
		return err
	}
	if baseline.Valid && remainingUSD < baseline.Float64-1e-12 {
		reason := "prepaid balance below starting observation"
		m.setDriftLatch(source, reason)
		if _, err := tx.ExecContext(ctx, `UPDATE source_credit_fuse SET drift_closed_at = ?, drift_reason = ?
			WHERE source = ? AND utc_day = ? AND drift_closed_at IS NULL`,
			m.now().UTC().Format(time.RFC3339Nano), reason, source, day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DriftClose durably closes all egress for a source with no timed reopen.
func (m *Manager) DriftClose(ctx context.Context, source, reason string) error {
	if source == "" {
		return errors.New("source name is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "pricing drift"
	}
	m.setDriftLatch(source, reason)
	day := utcDay(m.now())
	_, err := m.db.ExecContext(ctx, `INSERT INTO source_credit_fuse (source, utc_day, drift_closed_at, drift_reason)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(source, utc_day) DO UPDATE SET
			drift_closed_at = COALESCE(drift_closed_at, excluded.drift_closed_at),
			drift_reason = COALESCE(drift_reason, excluded.drift_reason)`,
		source, day, m.now().UTC().Format(time.RFC3339Nano), reason)
	return err
}

// EgressTestDisableGates turns off CommitEgress gate checks for mutation tests
// in other packages.
func EgressTestDisableGates(t interface{ Cleanup(func()) }) {
	old := egressTestHooks.enforceEgressGates
	egressTestHooks.enforceEgressGates = false
	t.Cleanup(func() { egressTestHooks.enforceEgressGates = old })
}
