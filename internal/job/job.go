// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package job owns the durable acquisition state machine. Every transition is
// a compare-and-swap UPDATE plus an append-only event in one transaction;
// running work holds a lease; crash recovery expires leases and rewinds
// mid-flight stages to their last durable boundary (bearer URLs live only in
// the attempt's memory, so fetching/validating rewind to resolving).
package job

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"papio/internal/artifact"
	"papio/internal/store"
	"papio/internal/work"
)

// Job states (stack plan "Job states").
const (
	StateQueued        = "queued"
	StateResolving     = "resolving"
	StateFetching      = "fetching"
	StateValidating    = "validating"
	StateReady         = "ready"
	StateImported      = "imported"
	StateAwaitingHuman = "awaiting_human"
	StateRetryWait     = "retry_wait"
	StateNeedsReview   = "needs_review"
	StateUnavailable   = "unavailable"
	StateFailed        = "failed"
	StateCancelled     = "cancelled"
)

// RetryReason names a StateRetryWait reason that other packages must
// recognise by identity rather than by matching an ad hoc event-detail
// string. Ordinary resolver retry waits remain free-form reason strings
// recorded only in event detail (e.g. "resolver_temporarily_unavailable");
// a reason earns a constant here only when the wait is externally driven and
// another package needs to test for it.
const (
	// RetryReasonDocumentDeliveryPending marks StateRetryWait while
	// internal/delivery polls a submitted, not-yet-resolved provider request
	// (ADR-0017 Decision 4). Delivery polling draws on its own budget, never
	// the ordinary resolver/HTTP retry budget.
	RetryReasonDocumentDeliveryPending = "document_delivery_pending"
)

// ActionKind names a human_actions.kind value that other packages must
// recognise by identity. The kinds that predate this vocabulary
// (openurl_handoff, manual_download, openurl_available, verify_identity)
// remain free-form literals; ActionKindDocumentDelivery is named because
// internal/delivery and the CLI need to test for it without restating the
// string.
const (
	// ActionKindDocumentDelivery marks the human action opened only after
	// internal/delivery exhausts deterministic reconciliation for a lodged
	// delivery request (ADR-0017 Decision 4). Its three allowed operations
	// (open_request_history, confirm_request_exists, confirm_request_absent)
	// are validated by internal/delivery; it never offers retry_submission.
	ActionKindDocumentDelivery = "document_delivery"
	// ActionKindDownloadsAccessRequired marks the human action opened when a
	// completed browser download's adoption is deferred while the bridge's
	// adoption-scan latch is unhealthy (internal/browser's
	// adoptionScanSuspended — the macOS TCC consent-wall signature AGENTS.md
	// documents), rather than on an ordinary transient defer. Its detail
	// carries the adoption root path. It is deliberately absent from
	// dismissalCancelsParkedJob's awaiting_human list: the download itself is
	// fine, only the folder grant is missing, so dismissing it must never
	// cancel the job. It resolves the same way any other non-advisory action
	// does — the job's next terminal transition (see (*Store).transition).
	ActionKindDownloadsAccessRequired = "downloads_access_required"
)

// Candidate statuses. Only CandidateAccepted asserts that these bytes were
// fetched AND validated for the job that selected it, so it is the only status
// provenance may be read from (ADR-0007).
const (
	CandidatePending   = "pending"
	CandidateFetching  = "fetching"
	CandidateRetryable = "retryable"
	CandidateSkipped   = "skipped"
	CandidateInvalid   = "invalid"
	CandidateAccepted  = "accepted"
)

// TerminalReason is the closed, durable vocabulary for terminal acquisition
// outcomes. Persisted values are intentionally stable: use the constants rather
// than changing their text.
type TerminalReason string

const (
	TerminalReasonUnknown                               TerminalReason = "unknown"
	TerminalReasonNoLegalCandidates                     TerminalReason = "no legal candidates"
	TerminalReasonTemporarySourceFailuresDidNotClear    TerminalReason = "temporary source failures did not clear"
	TerminalReasonTemporaryCandidateFailuresDidNotClear TerminalReason = "temporary candidate failures did not clear"
	TerminalReasonCandidatesExhausted                   TerminalReason = "all candidates exhausted"
	TerminalReasonNoIdentifier                          TerminalReason = "no_identifier"
	TerminalReasonDOINotRegistered                      TerminalReason = "doi_not_registered"
	TerminalReasonNoEntitlement                         TerminalReason = "no_entitlement"
	TerminalReasonBrowserRejected                       TerminalReason = "browser_rejected"
	TerminalReasonDocumentDeliveryAvailable             TerminalReason = "document_delivery_available"
	TerminalReasonCancelledByUser                       TerminalReason = "cancelled by user"
	TerminalReasonBrowserCancelled                      TerminalReason = "browser_cancelled"
	TerminalReasonUserDismissed                         TerminalReason = "user_dismissed"
	TerminalReasonReviewRejected                        TerminalReason = "review_rejected"
)

// NormalizeTerminalReason converts a stored terminal reason to the durable
// vocabulary. Unknown values remain in old databases but are exposed as
// TerminalReasonUnknown rather than being re-persisted.
func NormalizeTerminalReason(reason string) TerminalReason {
	switch TerminalReason(reason) {
	case TerminalReasonNoLegalCandidates,
		TerminalReasonTemporarySourceFailuresDidNotClear,
		TerminalReasonTemporaryCandidateFailuresDidNotClear,
		TerminalReasonCandidatesExhausted,
		TerminalReasonNoIdentifier,
		TerminalReasonDOINotRegistered,
		TerminalReasonNoEntitlement,
		TerminalReasonBrowserRejected,
		TerminalReasonDocumentDeliveryAvailable,
		TerminalReasonCancelledByUser,
		TerminalReasonBrowserCancelled,
		TerminalReasonUserDismissed,
		TerminalReasonReviewRejected:
		return TerminalReason(reason)
	default:
		return TerminalReasonUnknown
	}
}

// Principal is the durable source whose entitlement initiated an acquisition.
// It is intentionally an opaque string: user identities and browser session
// identifiers are not available as durable, non-secret values.
type Principal string

const (
	PrincipalUnknown Principal = "unknown"
	PrincipalCLI     Principal = "cli"
	PrincipalMCP     Principal = "mcp"
)

// Attribution records who asked for an acquisition. Principal is the transport
// papio observed for itself; Consumer is the optional name a caller supplied
// for its own accounting, and is empty whenever none was given — a shared
// daemon partitions its totals by Consumer, never by Principal, which only
// distinguishes the socket from the in-process agent surface.
type Attribution struct {
	Principal Principal
	Consumer  string
}

// Terminal reports whether a state ends the acquisition attempt. ready is the
// acquisition terminal; imported additionally records a completed Zotero
// export; other exports remain separate idempotent records.
func Terminal(state string) bool {
	switch state {
	case StateReady, StateImported, StateUnavailable, StateFailed, StateCancelled:
		return true
	}
	return false
}

// allowed maps from-state -> to-states. Recovery rewinds (fetching/validating
// -> resolving) are legal because candidates re-rank deterministically and the
// artifact store is content-addressed (no duplicates on re-fetch).
var allowed = map[string]map[string]bool{
	StateQueued: {StateResolving: true, StateCancelled: true},
	StateResolving: {
		StateFetching: true, StateReady: true, StateAwaitingHuman: true, StateRetryWait: true,
		StateNeedsReview: true, StateUnavailable: true, StateFailed: true, StateCancelled: true,
	},
	StateFetching: {
		StateValidating: true, StateResolving: true, StateRetryWait: true,
		StateAwaitingHuman: true, StateNeedsReview: true, StateUnavailable: true,
		StateFailed: true, StateCancelled: true,
	},
	StateValidating: {
		StateReady: true, StateFetching: true, StateResolving: true,
		StateNeedsReview: true, StateFailed: true, StateCancelled: true,
		// Adoption re-parks here on a transient validation/store error so the
		// supplied download is preserved for the directory sweep to retry,
		// rather than being rewound to resolving and replaced by an OA fetch.
		StateAwaitingHuman: true,
	},
	StateAwaitingHuman: {
		StateResolving: true, StateFetching: true, StateCancelled: true, StateFailed: true,
		// Phase 2 browser bridge resumes a parked handoff directly: the extension's
		// terminal observations map to unavailable/needs_review/retry_wait, and an
		// adopted download re-enters validation. The adopting caller holds a lease so
		// the scheduler and RecoverStale cannot rewind the job mid-adoption.
		StateValidating: true, StateUnavailable: true, StateNeedsReview: true, StateRetryWait: true,
	},
	StateRetryWait:   {StateResolving: true, StateFetching: true, StateCancelled: true, StateFailed: true},
	StateNeedsReview: {StateResolving: true, StateFetching: true, StateAwaitingHuman: true, StateCancelled: true},
	// A successful zotio apply files the artifact in Zotero; imported is the
	// only edge out of ready and is itself fully terminal.
	StateReady: {StateImported: true},
}

// ErrConflict is returned when a CAS transition loses (state changed or the
// transition is not allowed).
var ErrConflict = errors.New("job state conflict")

// ErrHumanActionKind reports an action that cannot be resolved by the requested
// human workflow.
type ErrHumanActionKind struct {
	ActionID int64
	Kind     string
}

func (e *ErrHumanActionKind) Error() string {
	return fmt.Sprintf("human action %d has unsupported kind %q", e.ActionID, e.Kind)
}

// ErrCostExceeded means reserving a paid attempt would cross the job's
// explicit maximum. The reservation is atomic across daemon workers/restarts.
type ErrCostExceeded struct {
	JobID, Source    string
	Spent, Cost, Max float64
}

func (e *ErrCostExceeded) Error() string {
	return fmt.Sprintf("job %s cost limit exceeded for %s: $%.2f + $%.2f > $%.2f",
		e.JobID, e.Source, e.Spent, e.Cost, e.Max)
}

// Policy is the per-job policy snapshot stored in jobs.policy_json.
type Policy struct {
	AccessMode     string   `json:"access_mode"`
	DesiredVersion string   `json:"desired_version"`
	Resolver       string   `json:"resolver,omitempty"`
	MaxCostUSD     *float64 `json:"max_cost_usd,omitempty"`
	SourcesAllow   []string `json:"sources_allow,omitempty"`
	SourcesDeny    []string `json:"sources_deny,omitempty"`
	FetchMaxBytes  int64    `json:"fetch_max_bytes"`
	AutoImport     bool     `json:"auto_import,omitempty"`
	Collection     string   `json:"collection,omitempty"`
}

// SourceAllowed applies the allow/deny lists (deny wins; empty allow = all).
func (p Policy) SourceAllowed(name string) bool {
	for _, d := range p.SourcesDeny {
		if d == name {
			return false
		}
	}
	if len(p.SourcesAllow) == 0 {
		return true
	}
	for _, a := range p.SourcesAllow {
		if a == name {
			return true
		}
	}
	return false
}

// Row is one job with its request context.
type Row struct {
	ID                  string    `json:"id"`
	WorkRequestID       string    `json:"work_request_id"`
	State               string    `json:"state"`
	Policy              Policy    `json:"policy"`
	ArtifactSHA256      string    `json:"artifact_sha256,omitempty"`
	SelectedCandidateID int64     `json:"selected_candidate_id,omitempty"`
	SpentUSD            float64   `json:"spent_usd"`
	TerminalReason      string    `json:"terminal_reason,omitempty"`
	RetryAt             string    `json:"retry_at,omitempty"`
	CreatedAt           string    `json:"created_at"`
	UpdatedAt           string    `json:"updated_at"`
	Work                work.Work `json:"work"`
	ZotioItemKey        string    `json:"zotio_item_key,omitempty"`
	LeaseOwner          string    `json:"-"`
	LeaseExpiresAt      string    `json:"-"`
}

// Principal returns the acquiring principal recorded for a job's work request.
//
// Deliberately NOT a field on Row: Row is the result body of jobs.get, and the
// IPC envelope is decoded with DisallowUnknownFields, so widening it would make
// every older papio reject a newer daemon's response. New facts reach consumers
// through new methods (ADR-0007).
func (js *Store) Principal(ctx context.Context, jobID string) (Principal, error) {
	var principal sql.NullString
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT wr.requester FROM jobs j
		JOIN work_requests wr ON wr.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(&principal)
	if err != nil {
		return "", err
	}
	if principal.String == "" {
		return PrincipalUnknown, nil
	}
	return Principal(principal.String), nil
}

// AttemptedTiers reports which access bases this job actually reached, in
// candidate-rank order and deduplicated. It reads append-only attempt evidence,
// rather than mutable candidate status, because ResetCandidates changes fetching
// and retryable candidates back to pending and would otherwise erase a tier from
// the lifetime record. Accepted candidates are also included: acceptance proves
// their tier was reached even if an older path recorded no candidate attempt.
func (js *Store) AttemptedTiers(ctx context.Context, jobID string) ([]string, error) {
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT c.access_basis
		FROM candidates c
		WHERE c.job_id = ?
		  AND (
			c.status = 'accepted'
			OR EXISTS (
				SELECT 1
				FROM attempts a
				WHERE a.job_id = c.job_id AND a.candidate_id = c.id
			)
		  )
		ORDER BY c.rank ASC, c.id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	seen := map[string]bool{}
	for rows.Next() {
		var basis string
		if err := rows.Scan(&basis); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !seen[basis] {
			seen[basis] = true
			out = append(out, basis)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// Candidate is one ranked acquisition option. URL is never stored; only the
// redacted form persists, so a crash discards bearer URLs by construction.
type Candidate struct {
	ID                 int64   `json:"id"`
	JobID              string  `json:"job_id"`
	Source             string  `json:"source"`
	URLRedacted        string  `json:"url_redacted"`
	URLKey             string  `json:"url_key"`
	LandingRedacted    string  `json:"landing_redacted,omitempty"`
	BrowserRoute       string  `json:"browser_route,omitempty"`
	SessionEvidence    string  `json:"session_evidence,omitempty"`
	Version            string  `json:"version"`
	AccessBasis        string  `json:"access_basis"`
	ReuseLicense       string  `json:"reuse_license"`
	ExpectedMIME       string  `json:"expected_mime,omitempty"`
	CostUSD            float64 `json:"cost_usd"`
	Direct             bool    `json:"direct"`
	IdentityConfidence float64 `json:"identity_confidence"`
	RankEvidence       string  `json:"rank_evidence,omitempty"`
	Rank               int     `json:"rank"`
	Status             string  `json:"status"`
	ReviewOverride     bool    `json:"review_override"`
}

// Store layers job semantics over the shared SQLite store.
type Store struct {
	S *store.Store

	oldestMu      sync.Mutex
	oldestCursors map[string]oldestListCursor
}

type oldestListCursor struct {
	createdAt string
	id        string
}

// NewID returns a 26-hex-char random identifier with a type prefix.
func NewID(prefix string) string {
	var b [13]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// CreateResult preserves convergence information so callers can distinguish
// a reused live job from a new queue entry.
type CreateResult struct {
	JobID    string
	Existing bool
}

// CreateRequest inserts a work request, its identifiers, and a queued job in
// one transaction. Resubmitting the same requestID returns its existing live
// job; terminal attempts permit a new job.
func (js *Store) CreateRequest(ctx context.Context, requestID string, w work.Work, zotioKey, collection string, pol Policy, rawIDs map[string]string, principal Principal) (string, error) {
	result, err := js.createRequest(ctx, requestID, w, zotioKey, collection, pol, rawIDs, Attribution{Principal: principal}, false, false)
	if err != nil {
		return "", err
	}
	return result.JobID, nil
}

// CreateRequestForWork creates a job or returns a live job with the same
// work.Work.Describe identity. It deliberately excludes title-only matching:
// titles describe works rather than assert identity, so merging on one could
// silently discard a distinct acquisition.
//
// It takes the full Attribution because it is the production submit path, the
// only place a caller-supplied consumer can be recorded. A reused live job
// keeps the attribution it was queued with: the second submitter did not create
// this acquisition, and rewriting the row would hand one consumer another's
// work.
func (js *Store) CreateRequestForWork(ctx context.Context, requestID string, w work.Work, zotioKey, collection string, pol Policy, rawIDs map[string]string, who Attribution, force bool) (CreateResult, error) {
	return js.createRequest(ctx, requestID, w, zotioKey, collection, pol, rawIDs, who, force, true)
}

func (js *Store) createRequest(ctx context.Context, requestID string, w work.Work, zotioKey, collection string, pol Policy, rawIDs map[string]string, who Attribution, force, deduplicateWork bool) (CreateResult, error) {
	if who.Principal == "" {
		who.Principal = PrincipalUnknown
	}
	if requestID == "" || force {
		// A force request needs its own work_request row so the live-job
		// invariant cannot collapse the explicitly requested fresh attempt.
		requestID = NewID("wr")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return CreateResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if !force {
		existing, err := liveJobForRequest(ctx, tx, requestID)
		if err != nil {
			return CreateResult{}, err
		}
		if existing != "" {
			return CreateResult{JobID: existing, Existing: true}, nil
		}
		if deduplicateWork {
			existing, err = liveJobForCanonicalWork(ctx, tx, w)
			if err != nil {
				return CreateResult{}, err
			}
			if existing != "" {
				return CreateResult{JobID: existing, Existing: true}, nil
			}
		}
	}

	polJSON, err := json.Marshal(pol)
	if err != nil {
		return CreateResult{}, err
	}
	authorsJSON, _ := json.Marshal(w.Authors)
	now := store.Now()
	jobID := NewID("job")

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO work_requests (id, created_at, requester, zotio_item_key, collection_key, title, authors_json, year, desired_version, max_cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		requestID, now, who.Principal, nullable(zotioKey), nullable(collection), nullable(w.Title), string(authorsJSON), nullableInt(w.Year), pol.DesiredVersion, pol.MaxCostUSD); err != nil {
		return CreateResult{}, fmt.Errorf("inserting work request: %w", err)
	}
	for kind, value := range map[string]string{"doi": w.DOI, "pmid": w.PMID, "arxiv": w.ArXiv, "isbn": w.ISBN, "openalex": w.OpenAlex} {
		if value == "" {
			continue
		}
		raw := rawIDs[kind]
		if raw == "" {
			raw = value
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO identifiers (work_request_id, kind, value, raw) VALUES (?, ?, ?, ?)`,
			requestID, kind, value, raw); err != nil {
			return CreateResult{}, fmt.Errorf("inserting identifier %s: %w", kind, err)
		}
	}
	// consumer is written here, on the job, rather than on the work_request the
	// INSERT OR IGNORE above may have reused: the submitter asked for THIS
	// acquisition. A resubmitted request id whose earlier jobs are terminal
	// creates a new job, and it must carry the name of whoever resubmitted it
	// rather than inheriting the first submitter's.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (id, work_request_id, state, policy_json, consumer, created_at, updated_at)
		 VALUES (?, ?, 'queued', ?, ?, ?, ?)`,
		jobID, requestID, string(polJSON), nullable(who.Consumer), now, now); err != nil {
		return CreateResult{}, fmt.Errorf("inserting job: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.created', ?)`,
		jobID, now, fmt.Sprintf(`{"request_id":%q,"work":%q}`, requestID, w.Describe())); err != nil {
		return CreateResult{}, err
	}
	if force && deduplicateWork {
		// A force submission is the operator withdrawing a verdict this work
		// already received. Retry out of unavailable cancels the conservative
		// advisory for the same reason (see Retry): left open it outlives its
		// own remedy and keeps advising an action that has now been taken —
		// here on a fresh job rather than the same one. Without this, every
		// resubmission double-counts the work's institutional opportunity
		// against a job that no longer represents it.
		superseded, err := supersededJobsForCanonicalWork(ctx, tx, w)
		if err != nil {
			return CreateResult{}, err
		}
		for _, oldID := range superseded {
			if _, err := tx.ExecContext(ctx,
				`UPDATE human_actions SET status = 'cancelled', resolved_at = ?
				  WHERE job_id = ? AND kind = ? AND status = 'open'`,
				now, oldID, informationalActionKind); err != nil {
				return CreateResult{}, err
			}
			// The durable trace that this verdict was withdrawn, and by what.
			// A terminal papio record is not necessarily final, and a consumer
			// that cached the old outcome has no other way to learn that.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.superseded', ?)`,
				oldID, now, fmt.Sprintf(`{"superseded_by":%q,"work":%q}`, jobID, w.Describe())); err != nil {
				return CreateResult{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{JobID: jobID}, nil
}

func liveJobForRequest(ctx context.Context, tx *sql.Tx, requestID string) (string, error) {
	return firstLiveJob(ctx, tx,
		`SELECT id, state FROM jobs WHERE work_request_id = ? ORDER BY created_at DESC`, requestID)
}

func liveJobForCanonicalWork(ctx context.Context, tx *sql.Tx, w work.Work) (string, error) {
	kind, value, ok := strings.Cut(w.Describe(), ":")
	if !ok {
		return "", nil
	}
	switch kind {
	case "doi", "pmid", "arxiv", "isbn", "openalex":
	default:
		return "", nil
	}
	return firstLiveJob(ctx, tx, `
		SELECT j.id, j.state FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = ? AND i.value = ?
		ORDER BY j.created_at DESC`, kind, value)
}

// RecordDuplicateWork notes that jobID now provably names the same work as
// another job that is still live, and returns that job's id ("" if none).
//
// It records; it does not converge. papio deduplicates at submit, where a
// title-only request correctly matches nothing because liveJobForCanonicalWork
// keys on strong identifiers. Enrichment can discover a DOI much later, and at
// that instant the duplication becomes knowable — but the handle was already
// issued. ADR-0010 promises a consumer that `existing: true` answers a question
// asked at submit, and consumers persist the returned job_id on that basis;
// merging afterwards would redefine a handle they are already polling.
//
// The asymmetry decides it, the same one ADR-0007 and ADR-0008 turn on. A
// duplicate costs one wasted fetch and, because artifacts are content
// addressed, one stored file either way. A false merge breaks a live handle.
// So the relationship is written down and left for a consumer to act on
// knowingly, rather than acted on by papio behind their back.
func (js *Store) RecordDuplicateWork(ctx context.Context, jobID string, w work.Work) (string, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	other, err := liveJobForCanonicalWorkExcept(ctx, tx, w, jobID)
	if err != nil || other == "" {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.duplicate_work_detected', ?)`,
		jobID, store.Now(), fmt.Sprintf(`{"duplicate_of":%q,"work":%q}`, other, w.Describe())); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return other, nil
}

func liveJobForCanonicalWorkExcept(ctx context.Context, tx *sql.Tx, w work.Work, exclude string) (string, error) {
	kind, value, ok := strings.Cut(w.Describe(), ":")
	if !ok {
		return "", nil
	}
	switch kind {
	case "doi", "pmid", "arxiv", "isbn", "openalex":
	default:
		return "", nil
	}
	return firstLiveJob(ctx, tx, `
		SELECT j.id, j.state FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = ? AND i.value = ? AND j.id != ?
		ORDER BY j.created_at DESC`, kind, value, exclude)
}

// supersededJobsForCanonicalWork lists terminal jobs that already answered this
// work. A force submission is a deliberate second opinion on a verdict those
// jobs published, so their advisories must not outlive it.
//
// Unlike liveJobForCanonicalWork this matches on EVERY strong identifier the
// request carries, not just Describe's highest-priority one. A request may
// supply several, so an older PMID-only job is invisible to a DOI-keyed lookup
// when the replacement supplies both — and it would keep its advisory open with
// no supersession recorded, which is the exact leak this function exists to
// close. The breadth is safe here and not in the live-dedup path because the
// stakes differ: merging two LIVE acquisitions can discard work in flight,
// whereas this only retires an advisory on a job that already settled. Title is
// still excluded — it describes a work rather than asserting its identity.
func supersededJobsForCanonicalWork(ctx context.Context, tx *sql.Tx, w work.Work) ([]string, error) {
	identifiers := map[string]string{
		"doi": w.DOI, "pmid": w.PMID, "arxiv": w.ArXiv, "isbn": w.ISBN, "openalex": w.OpenAlex,
	}
	seen := make(map[string]bool)
	var ids []string
	for kind, value := range identifiers {
		if value == "" {
			continue
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT j.id, j.state FROM jobs j
			JOIN identifiers i ON i.work_request_id = j.work_request_id
			WHERE i.kind = ? AND i.value = ?
			ORDER BY j.created_at DESC`, kind, value)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if Terminal(state) && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func firstLiveJob(ctx context.Context, tx *sql.Tx, query string, args ...any) (string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return "", err
		}
		if !Terminal(state) {
			return id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// FillWorkMetadata fills fields absent from the original request using
// resolver-observed metadata. Request values remain authoritative; conflicting
// identifiers fail closed rather than silently changing the requested work.
func (js *Store) FillWorkMetadata(ctx context.Context, jobID string, discovered work.Work) (*Row, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var requestID string
	var title, authorsJSON sql.NullString
	var year sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT w.id, w.title, w.authors_json, w.year
		FROM jobs j JOIN work_requests w ON w.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(&requestID, &title, &authorsJSON, &year); err != nil {
		return nil, err
	}
	var authors []string
	if authorsJSON.Valid && authorsJSON.String != "" {
		if err := json.Unmarshal([]byte(authorsJSON.String), &authors); err != nil {
			return nil, fmt.Errorf("request %s authors: %w", requestID, err)
		}
	}
	if !title.Valid || title.String == "" {
		title.String, title.Valid = discovered.Title, discovered.Title != ""
	}
	if len(authors) == 0 && len(discovered.Authors) > 0 {
		authors = append([]string(nil), discovered.Authors...)
	}
	if !year.Valid || year.Int64 == 0 {
		year.Int64, year.Valid = int64(discovered.Year), discovered.Year != 0
	}
	encodedAuthors, err := json.Marshal(authors)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_requests SET title = ?, authors_json = ?, year = ? WHERE id = ?`,
		nullable(title.String), string(encodedAuthors), nullableInt(int(year.Int64)), requestID); err != nil {
		return nil, err
	}

	observed := map[string]string{
		"doi": discovered.DOI, "pmid": discovered.PMID, "arxiv": discovered.ArXiv,
		"isbn": discovered.ISBN, "openalex": discovered.OpenAlex,
	}
	for kind, value := range observed {
		if value == "" {
			continue
		}
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT value FROM identifiers WHERE work_request_id = ? AND kind = ?`, requestID, kind).Scan(&existing)
		switch {
		case err == nil && existing != value:
			return nil, fmt.Errorf("resolver metadata conflicts with requested %s: %q != %q", kind, value, existing)
		case err == nil:
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO identifiers(work_request_id, kind, value, raw) VALUES(?, ?, ?, ?)`,
			requestID, kind, value, value); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.metadata_enriched', ?)`,
		jobID, store.Now(), `{"source":"resolver","policy":"fill_missing_only"}`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return js.Get(ctx, jobID)
}

// ReserveCost atomically charges one paid source attempt to a job. A nil limit
// tracks spend without imposing a ceiling. Zero-cost calls are not recorded.
func (js *Store) ReserveCost(ctx context.Context, jobID, source string, cost float64, limit *float64) error {
	if cost < 0 {
		return fmt.Errorf("negative job cost %.4f", cost)
	}
	if cost == 0 {
		return nil
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var spent float64
	if err := tx.QueryRowContext(ctx, `SELECT spent_usd FROM jobs WHERE id = ?`, jobID).Scan(&spent); err != nil {
		return err
	}
	if limit != nil && spent+cost > *limit+1e-9 {
		return &ErrCostExceeded{JobID: jobID, Source: source, Spent: spent, Cost: cost, Max: *limit}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET spent_usd = spent_usd + ?, updated_at = ? WHERE id = ?`,
		cost, store.Now(), jobID); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"source": source, "cost_usd": cost})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.cost_reserved', ?)`,
		jobID, store.Now(), string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseReservedCost reverses a reservation when the paid source call did not
// start (for example, its durable monthly budget closed between checks).
func (js *Store) ReleaseReservedCost(ctx context.Context, jobID, source string, cost float64) error {
	if cost <= 0 {
		return nil
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx,
		`UPDATE jobs SET spent_usd = spent_usd - ?, updated_at = ?
		 WHERE id = ? AND spent_usd + 1e-9 >= ?`,
		cost, store.Now(), jobID, cost)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("cannot release unreserved job cost %.4f for %s", cost, jobID)
	}
	detail, _ := json.Marshal(map[string]any{"source": source, "cost_usd": cost})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.cost_released', ?)`,
		jobID, store.Now(), string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// Transition CAS-moves a job from -> to, appending an event in the same
// transaction. detail must be pre-redacted. retryAt applies to retry_wait;
// terminalReason applies to terminal states. Reaching any terminal state
// closes the job's open human actions ('resolved' on ready, 'cancelled'
// otherwise) so a finished job never strands an open action.
func (js *Store) Transition(ctx context.Context, jobID, from, to string, detail map[string]any, opts ...TransitionOpt) error {
	return js.transition(ctx, jobID, from, to, detail, opts...)
}

func (js *Store) transition(ctx context.Context, jobID, from, to string, detail map[string]any, opts ...TransitionOpt) error {
	if !allowed[from][to] {
		return fmt.Errorf("%w: %s -> %s not allowed", ErrConflict, from, to)
	}
	var cfg transitionCfg
	for _, o := range opts {
		o(&cfg)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["from"], detail["to"] = from, to
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	now := store.Now()
	// A parked or terminal job is owned by nobody: release the lease so the
	// scheduler can re-claim it when it becomes runnable again.
	releaseLease := Terminal(to) || to == StateRetryWait || to == StateAwaitingHuman || to == StateNeedsReview

	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ?,
		        retry_at = ?,
		        terminal_reason = COALESCE(?, terminal_reason),
		        artifact_sha256 = COALESCE(?, artifact_sha256),
		        selected_candidate_id = COALESCE(?, selected_candidate_id),
		        lease_owner = CASE WHEN ? THEN NULL ELSE lease_owner END,
		        lease_expires_at = CASE WHEN ? THEN NULL ELSE lease_expires_at END
		 WHERE id = ? AND state = ?`,
		to, now, nullable(cfg.retryAt), nullable(cfg.terminalReason), nullable(cfg.artifactSHA), cfg.candidateID,
		releaseLease, releaseLease, jobID, from)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("%w: job %s not in state %s", ErrConflict, jobID, from)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		jobID, now, string(detailJSON)); err != nil {
		return err
	}
	// Record the acquisition edge whenever this transition attaches an artifact.
	// Inside the same transaction, and reading identity_result here rather than
	// from a caller argument, because artifacts.identity_result was just written
	// by THIS job's validation: capturing it now is what stops a later
	// acquisition of the same bytes from silently rewriting this job's finding
	// (ADR-0007). role is 'main' — components other than the main file are
	// recorded by AddComponent.
	if cfg.artifactSHA != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO job_artifacts (job_id, artifact_sha256, role, candidate_id, identity_result, created_at)
			SELECT ?, ?, 'main', j.selected_candidate_id, a.identity_result, ?
			  FROM jobs j JOIN artifacts a ON a.sha256 = ?
			 WHERE j.id = ?
			ON CONFLICT(job_id, role, artifact_sha256) DO UPDATE SET
			        candidate_id = excluded.candidate_id,
			        identity_result = excluded.identity_result`,
			jobID, cfg.artifactSHA, now, cfg.artifactSHA, jobID); err != nil {
			return err
		}
	}
	if Terminal(to) {
		if err := closeTerminalHumanActions(ctx, tx, jobID, to, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// closeTerminalHumanActions is the one transaction-local closure used by all
// terminal transitions. openurl_available is advisory and intentionally
// survives; every other open action belongs to the completed job and closes
// with it.
func closeTerminalHumanActions(ctx context.Context, tx *sql.Tx, jobID, to, now string) error {
	actionStatus := "cancelled"
	if to == StateReady || to == StateImported {
		actionStatus = "resolved"
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE human_actions SET status = ?, resolved_at = ?
		 WHERE job_id = ? AND status = 'open' AND kind != ?`,
		actionStatus, now, jobID, informationalActionKind)
	return err
}

// RepairAwaitingHuman atomically resolves a repair snapshot's open actions and
// returns an unleased parked job to resolving. The lease predicate must share
// this transaction: adoption can acquire its awaiting_human lease after
// maintenance read the page, and closing that handoff would lose the browser
// download that lease protects.
func (js *Store) RepairAwaitingHuman(ctx context.Context, jobID string, actionIDs []int64, detail map[string]any) error {
	expected := make(map[int64]struct{}, len(actionIDs))
	for _, actionID := range actionIDs {
		if _, duplicate := expected[actionID]; duplicate {
			return fmt.Errorf("%w: duplicate human action %d", ErrConflict, actionID)
		}
		expected[actionID] = struct{}{}
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["from"], detail["to"] = StateAwaitingHuman, StateResolving
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, retry_at = NULL,
		        lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND state = ?
		   AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		StateResolving, now, jobID, StateAwaitingHuman, now)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return fmt.Errorf("%w: job %s is not an unleased awaiting_human job", ErrConflict, jobID)
	}

	openRows, err := tx.QueryContext(ctx,
		`SELECT id FROM human_actions WHERE job_id = ? AND status = 'open'`, jobID)
	if err != nil {
		return err
	}
	open := make(map[int64]struct{}, len(expected))
	for openRows.Next() {
		var actionID int64
		if err := openRows.Scan(&actionID); err != nil {
			_ = openRows.Close()
			return err
		}
		if _, expectedAction := expected[actionID]; !expectedAction {
			_ = openRows.Close()
			return fmt.Errorf("%w: open actions changed for job %s", ErrConflict, jobID)
		}
		open[actionID] = struct{}{}
	}
	if err := openRows.Err(); err != nil {
		_ = openRows.Close()
		return err
	}
	if err := openRows.Close(); err != nil {
		return err
	}
	if len(open) != len(expected) {
		return fmt.Errorf("%w: open actions changed for job %s", ErrConflict, jobID)
	}

	if len(actionIDs) != 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(actionIDs)), ",")
		args := make([]any, 0, len(actionIDs)+2)
		args = append(args, now, jobID)
		for _, actionID := range actionIDs {
			args = append(args, actionID)
		}
		res, err = tx.ExecContext(ctx,
			`UPDATE human_actions SET status = 'resolved', resolved_at = ?
			 WHERE job_id = ? AND status = 'open' AND id IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return err
		}
		if changed, _ := res.RowsAffected(); changed != int64(len(actionIDs)) {
			return fmt.Errorf("%w: open actions changed for job %s", ErrConflict, jobID)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		jobID, now, string(detailJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

// RepairParkWithAction atomically moves an unleased human park only when its
// open actions still match the maintenance snapshot, then opens the replacement
// human action. The lease and action predicates share one transaction so a
// browser adopter that wins after maintenance reads cannot lose its download.
func (js *Store) RepairParkWithAction(ctx context.Context, jobID, from, to string, actionIDs []int64,
	actionKind, actionDetail string, detail map[string]any, access AccessClassification, opts ...OpenHumanActionOption,
) error {
	if !allowed[from][to] {
		return fmt.Errorf("%w: %s -> %s not allowed", ErrConflict, from, to)
	}
	var options openHumanActionOptions
	if err := access.apply(&options); err != nil {
		return err
	}
	for _, option := range opts {
		if option == nil {
			continue
		}
		if err := option(&options); err != nil {
			return err
		}
	}
	if options.binding != nil && actionKind != "verify_identity" {
		return errors.New("human action binding is only valid for verify_identity")
	}
	expected := make(map[int64]struct{}, len(actionIDs))
	for _, actionID := range actionIDs {
		if _, duplicate := expected[actionID]; duplicate {
			return fmt.Errorf("%w: duplicate human action %d", ErrConflict, actionID)
		}
		expected[actionID] = struct{}{}
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["from"], detail["to"] = from, to
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, retry_at = NULL,
		        lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND state = ?
		   AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		to, now, jobID, from, now)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return fmt.Errorf("%w: job %s is not an unleased %s job", ErrConflict, jobID, from)
	}

	openRows, err := tx.QueryContext(ctx,
		`SELECT id FROM human_actions WHERE job_id = ? AND status = 'open'`, jobID)
	if err != nil {
		return err
	}
	open := make(map[int64]struct{}, len(expected))
	for openRows.Next() {
		var actionID int64
		if err := openRows.Scan(&actionID); err != nil {
			_ = openRows.Close()
			return err
		}
		if _, expectedAction := expected[actionID]; !expectedAction {
			_ = openRows.Close()
			return fmt.Errorf("%w: open actions changed for job %s", ErrConflict, jobID)
		}
		open[actionID] = struct{}{}
	}
	if err := openRows.Err(); err != nil {
		_ = openRows.Close()
		return err
	}
	if err := openRows.Close(); err != nil {
		return err
	}
	if len(open) != len(expected) {
		return fmt.Errorf("%w: open actions changed for job %s", ErrConflict, jobID)
	}

	if options.inheritResolvedHandoffAuth {
		err = tx.QueryRowContext(ctx, `
			SELECT requires_auth FROM human_actions
			 WHERE job_id = ? AND kind = 'openurl_handoff' AND status = 'resolved'
			 ORDER BY resolved_at DESC, id DESC
			 LIMIT 1`, jobID).Scan(&options.requiresAuth)
		switch {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			// A missing handoff cannot prove the work is open access, so do not
			// send a user toward a provider page with false no-login guidance.
			options.requiresAuth = true
		default:
			return err
		}
	}

	var actionID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM human_actions
		 WHERE job_id = ? AND kind = ? AND status = 'open'
		 ORDER BY id ASC LIMIT 1`, jobID, actionKind).Scan(&actionID)
	switch {
	case err == nil:
		if options.binding == nil {
			_, err = tx.ExecContext(ctx,
				`UPDATE human_actions SET detail = ?, requires_auth = ?, blocked_by = ?, revision = revision + 1 WHERE id = ?`,
				nullable(actionDetail), options.requiresAuth, options.blockedBy, actionID)
		} else {
			_, err = tx.ExecContext(ctx, `
				UPDATE human_actions
				SET detail = ?, requires_auth = ?, blocked_by = ?, candidate_id = ?, quarantine_path = ?, quarantine_sha256 = ?,
					revision = revision + 1
				WHERE id = ?`,
				nullable(actionDetail), options.requiresAuth, options.blockedBy, options.binding.CandidateID, options.binding.QuarantinePath,
				options.binding.QuarantineSHA256, actionID)
		}
	case errors.Is(err, sql.ErrNoRows):
		binding := options.binding
		candidateID, path, sha := any(nil), "", ""
		if binding != nil {
			candidateID, path, sha = binding.CandidateID, binding.QuarantinePath, binding.QuarantineSHA256
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO human_actions
				(job_id, kind, status, detail, requires_auth, blocked_by, candidate_id, quarantine_path, quarantine_sha256, revision, created_at)
			VALUES (?, ?, 'open', ?, ?, ?, ?, ?, ?, 1, ?)`,
			jobID, actionKind, nullable(actionDetail), options.requiresAuth, options.blockedBy, candidateID, path, sha, now)
	default:
		return err
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		jobID, now, string(detailJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

type transitionCfg struct {
	retryAt        string
	terminalReason string
	artifactSHA    string
	candidateID    any
}

// TransitionOpt customizes a transition.
type TransitionOpt func(*transitionCfg)

// WithRetryAt schedules the next attempt for a retry_wait transition.
func WithRetryAt(t time.Time) TransitionOpt {
	return func(c *transitionCfg) { c.retryAt = t.UTC().Format(time.RFC3339Nano) }
}

// WithTerminalReason records why a job ended.
func WithTerminalReason(reason TerminalReason) TransitionOpt {
	return func(c *transitionCfg) { c.terminalReason = string(reason) }
}

// WithArtifact links the accepted artifact.
func WithArtifact(sha string) TransitionOpt {
	return func(c *transitionCfg) { c.artifactSHA = sha }
}

// WithCandidate records the selected candidate.
func WithCandidate(id int64) TransitionOpt {
	return func(c *transitionCfg) { c.candidateID = id }
}

// ClaimNext leases the oldest runnable job: queued always; retry_wait when
// due. Mid-flight stages are claimable when unowned (the durable result of
// RecoverStale) or when their prior lease expired.
func (js *Store) ClaimNext(ctx context.Context, owner string, lease time.Duration) (*Row, error) {
	now := store.Now()
	expires := time.Now().UTC().Add(lease).Format(time.RFC3339Nano)
	db := js.S.DB()

	var id string
	err := db.QueryRowContext(ctx, `
		SELECT id FROM jobs
		WHERE (
		        (state = 'queued' AND (lease_owner IS NULL OR lease_expires_at < ?))
		     OR (state = 'retry_wait' AND retry_at <= ? AND (lease_owner IS NULL OR lease_expires_at < ?))
		     OR (state IN ('resolving','fetching','validating') AND (lease_owner IS NULL OR lease_expires_at < ?))
		      )
		ORDER BY created_at ASC LIMIT 1`, now, now, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := db.ExecContext(ctx,
		`UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ? AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		owner, expires, id, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil // lost the race; caller loops
	}
	return js.Get(ctx, id)
}

// Heartbeat extends a held lease.
func (js *Store) Heartbeat(ctx context.Context, jobID, owner string, lease time.Duration) error {
	expires := time.Now().UTC().Add(lease).Format(time.RFC3339Nano)
	res, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = ? WHERE id = ? AND lease_owner = ?`, expires, jobID, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: lease on %s not held by %s", ErrConflict, jobID, owner)
	}
	return nil
}

// Release drops a lease without changing state (job becomes claimable).
func (js *Store) Release(ctx context.Context, jobID, owner string) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL WHERE id = ? AND lease_owner = ?`, jobID, owner)
	return err
}

// Cancel moves a nonterminal job to cancelled. Repeated cancellation and
// cancellation after any terminal result are idempotent no-ops.
func (js *Store) Cancel(ctx context.Context, jobID string, reason TerminalReason) error {
	for {
		row, err := js.Get(ctx, jobID)
		if err != nil {
			return err
		}
		if Terminal(row.State) {
			return nil
		}
		err = js.transition(ctx, jobID, row.State, StateCancelled,
			map[string]any{"reason": string(reason)}, WithTerminalReason(reason))
		if errors.Is(err, ErrConflict) {
			continue
		}
		return err
	}
}

// Retry explicitly reopens a retry-wait, failed, or unavailable job at the
// durable resolving boundary. Ready, cancelled, active, and human-parked jobs
// require their dedicated command instead of silently changing meaning.
func (js *Store) Retry(ctx context.Context, jobID string) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var from string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, jobID).Scan(&from); err != nil {
		return err
	}
	switch from {
	case StateRetryWait, StateFailed, StateUnavailable:
	default:
		return fmt.Errorf("%w: %s cannot be retried", ErrConflict, from)
	}
	now := store.Now()
	result, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'resolving', updated_at = ?, lease_owner = NULL,
		        lease_expires_at = NULL, retry_at = NULL, terminal_reason = NULL,
		        selected_candidate_id = NULL, artifact_sha256 = NULL
		  WHERE id = ? AND state = ?`, now, jobID, from)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrConflict
	}
	// Every other open action is closed by the terminal transition itself, but
	// the conservative-mode advisory is exempt there and in the startup sweep so
	// its trace survives on a job that stayed unavailable. Retry is the only edge
	// out of unavailable (the allowed table gives that state no outbound edge),
	// and it is the user acting on exactly the advice the advisory gives - switch
	// access mode and retry. Left open it outlives its own remedy and keeps
	// telling the user to do the thing they just did, even once the job reaches
	// ready. Re-exhausting in conservative mode opens it again.
	cancelled, err := tx.ExecContext(ctx,
		`UPDATE human_actions SET status = 'cancelled', resolved_at = ?
		  WHERE job_id = ? AND kind = ? AND status = 'open'`,
		now, jobID, informationalActionKind)
	if err != nil {
		return err
	}
	advisories, err := cancelled.RowsAffected()
	if err != nil {
		return err
	}
	// Releasing the pinned access mode is the other half of cancelling that
	// advisory, and without it the remedy the advisory names cannot work. The
	// advisory says "a route exists; this mode will not take it", the operator
	// widens access_mode and retries — but the job snapshots its mode at submit
	// time and the decision path reads that snapshot, so the retry would
	// re-exhaust under the same conservative mode and reopen the same advisory
	// forever. Clearing it lets the job follow the operator's current
	// configuration from here, which is precisely the decision they just made.
	// Scoped to jobs that actually carry the advisory: an ordinary failed or
	// retry-wait retry keeps whatever mode it was submitted with.
	if advisories > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET policy_json = json_set(policy_json, '$.access_mode', '') WHERE id = ?`,
			jobID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE candidates SET status = 'pending'
		  WHERE job_id = ? AND status IN ('fetching','retryable')`, jobID); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"from": from, "to": StateResolving, "reason": "explicit_retry"})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.retry_requested', ?)`,
		jobID, now, string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverStale rewinds expired mid-flight jobs to resolving: bearer URLs and
// quarantine temp files are per-attempt, so the durable boundary is the
// candidate set, which re-ranks deterministically. Content addressing makes
// re-fetches duplicate-free. Returns the rewound job IDs.
func (js *Store) RecoverStale(ctx context.Context) ([]string, error) {
	now := store.Now()
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT id, state FROM jobs WHERE state IN ('resolving','fetching','validating') AND (lease_expires_at IS NULL OR lease_expires_at < ?)`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type stale struct{ id, state string }
	var found []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.state); err != nil {
			_ = rows.Close()
			return nil, err
		}
		found = append(found, s)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var recovered []string
	for _, s := range found {
		if s.state == StateResolving {
			// Already at the durable boundary; clear only a still-stale lease.
			result, err := js.S.DB().ExecContext(ctx,
				`UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL
				 WHERE id = ? AND state = 'resolving'
				   AND (lease_expires_at IS NULL OR lease_expires_at < ?)`, s.id, now)
			if err != nil {
				return recovered, err
			}
			if changed, _ := result.RowsAffected(); changed == 1 {
				recovered = append(recovered, s.id)
			}
			continue
		}
		if err := js.Transition(ctx, s.id, s.state, StateResolving, map[string]any{"reason": "crash_recovery"}); err != nil && !errors.Is(err, ErrConflict) {
			return recovered, err
		}
		recovered = append(recovered, s.id)
	}
	return recovered, nil
}

// informationalActionKind marks the one advisory action that legitimately
// stays open on a terminal job: conservative mode records that an
// institutional OpenURL exists without opening it, and that trace must
// survive both the terminal transition and the startup sweep.
const informationalActionKind = "openurl_available"

// CloseStaleHumanActions cancels open non-advisory actions for jobs that have
// already reached a terminal state. It repairs rows left by older daemon
// versions.
func (js *Store) CloseStaleHumanActions(ctx context.Context) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET status = 'cancelled', resolved_at = ?
		 WHERE status = 'open'
		   AND kind != ?
		   AND EXISTS (
		       SELECT 1 FROM jobs
		       WHERE jobs.id = human_actions.job_id
		         AND jobs.state IN ('ready', 'imported', 'unavailable', 'failed', 'cancelled')
		   )`, store.Now(), informationalActionKind)
	return err
}

// SweepTerminalQuarantine removes abandoned per-job download files only after
// their jobs become terminal. Human-review states deliberately retain their
// quarantine directory because action details point users to those files.
func (js *Store) SweepTerminalQuarantine(ctx context.Context) error {
	if js == nil || js.S == nil {
		return errors.New("job store is not initialized")
	}
	artifacts, err := artifact.New(filepath.Dir(js.S.Path()))
	if err != nil {
		return fmt.Errorf("open artifact layout: %w", err)
	}
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT id FROM jobs
		 WHERE state IN ('ready', 'imported', 'unavailable', 'failed', 'cancelled')`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var cleanupErr error
	for _, id := range ids {
		if err := artifacts.CleanQuarantine(id); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clean terminal quarantine for %s: %w", id, err))
		}
	}
	return cleanupErr
}

// Get loads one job row with its work-request identity.
func (js *Store) Get(ctx context.Context, jobID string) (*Row, error) {
	db := js.S.DB()
	var r Row
	var polJSON string
	var artifact, terminal, retryAt sql.NullString
	var selected sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT j.id, j.work_request_id, j.state, j.policy_json, j.artifact_sha256, j.selected_candidate_id,
		       j.spent_usd, j.terminal_reason, j.retry_at, j.created_at, j.updated_at,
		       COALESCE(j.lease_owner,''), COALESCE(j.lease_expires_at,''),
		       COALESCE(w.title,''), COALESCE(w.authors_json,'[]'), COALESCE(w.year,0), COALESCE(w.zotio_item_key,'')
		FROM jobs j JOIN work_requests w ON w.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(
		&r.ID, &r.WorkRequestID, &r.State, &polJSON, &artifact, &selected, &r.SpentUSD, &terminal, &retryAt, &r.CreatedAt, &r.UpdatedAt,
		&r.LeaseOwner, &r.LeaseExpiresAt, &r.Work.Title, &jsonScanner{&r.Work.Authors}, &r.Work.Year, &r.ZotioItemKey)
	if err != nil {
		return nil, err
	}
	r.ArtifactSHA256, r.SelectedCandidateID, r.RetryAt = artifact.String, selected.Int64, retryAt.String
	if terminal.Valid && terminal.String != "" {
		r.TerminalReason = string(NormalizeTerminalReason(terminal.String))
	}
	if err := json.Unmarshal([]byte(polJSON), &r.Policy); err != nil {
		return nil, fmt.Errorf("job %s policy: %w", jobID, err)
	}
	ids, err := db.QueryContext(ctx, `SELECT kind, value FROM identifiers WHERE work_request_id = ?`, r.WorkRequestID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ids.Close() }()
	for ids.Next() {
		var kind, value string
		if err := ids.Scan(&kind, &value); err != nil {
			_ = ids.Close()
			return nil, err
		}
		switch kind {
		case "doi":
			r.Work.DOI = value
		case "pmid":
			r.Work.PMID = value
		case "arxiv":
			r.Work.ArXiv = value
		case "isbn":
			r.Work.ISBN = value
		case "openalex":
			r.Work.OpenAlex = value
		}
	}
	if err := ids.Close(); err != nil {
		return nil, err
	}
	if err := ids.Err(); err != nil {
		return nil, err
	}
	return &r, nil
}

// LeaseActive reports whether the row is still protected from a competing
// state transition. A malformed persisted lease is treated as active so repair
// code cannot discard a browser download while ownership is uncertain.
func (r Row) LeaseActive(now time.Time) bool {
	if r.LeaseOwner == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, r.LeaseExpiresAt)
	return err != nil || !expires.Before(now)
}

// jsonScanner scans a JSON array column into a []string.
type jsonScanner struct{ dst *[]string }

func (j *jsonScanner) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		return nil
	case string:
		return json.Unmarshal([]byte(v), j.dst)
	case []byte:
		return json.Unmarshal(v, j.dst)
	default:
		return fmt.Errorf("unexpected authors column type %T", src)
	}
}

// ListLimitMax and ListLimitDefault bound Store.List's limit parameter: a
// caller-supplied limit outside (0, ListLimitMax] resets to ListLimitDefault.
// Exported so internal/cli can compute the same effective limit the daemon
// will actually use instead of comparing a returned row count against the
// raw --limit flag (which is a different number whenever it is out of
// range) — see internal/agentjson.Capped and its callers.
const (
	ListLimitMax     = 500
	ListLimitDefault = 100
)

// List returns jobs, optionally filtered by state, newest first.
func (js *Store) List(ctx context.Context, state string, limit int) ([]Row, error) {
	rows, _, err := js.listJobs(ctx, state, "", EffectiveListLimit(limit), false)
	return rows, err
}

// ListPage returns one bounded page of jobs and whether more rows exist behind
// it. The flag is a proof, not an inference: the query reaches one row past the
// limit and reports whether it was there, so a cohort-scale consumer can tell a
// full page from a complete list (ADR-0007). Comparing len(rows) against the
// requested limit cannot — an exactly-full final page is indistinguishable from
// a truncated one.
func (js *Store) ListPage(ctx context.Context, state string, limit int) ([]Row, bool, error) {
	return js.listJobs(ctx, state, "", EffectiveListLimit(limit), true)
}

// ListPageFor is ListPage narrowed to one consumer's submissions. An empty
// consumer is unfiltered, exactly as an empty state is.
//
// The filter is applied in SQL rather than to a fetched page because the page
// bound and its truncation proof have to describe the rows the caller asked
// for: filtering afterwards would return a short page and claim it was
// complete.
func (js *Store) ListPageFor(ctx context.Context, state, consumer string, limit int) ([]Row, bool, error) {
	return js.listJobs(ctx, state, consumer, EffectiveListLimit(limit), true)
}

// EffectiveListLimit resolves a caller-supplied limit the way the store applies
// it, so a caller never has to reimplement the clamp.
//
// An over-large limit clamps DOWN TO THE MAXIMUM rather than resetting to the
// default. Resetting made the function non-monotonic — asking for 600 returned
// 100, fewer rows than asking for 500 — and it did so silently, in the
// direction of under-reporting, which is the worst way to be wrong for the
// people who pass a large limit: they are counting. Two separate consumers
// walked into it on the same day, each concluding the daemon held a fraction
// of the jobs it actually held. Unspecified still means the default.
func EffectiveListLimit(limit int) int {
	if limit <= 0 {
		return ListLimitDefault
	}
	if limit > ListLimitMax {
		return ListLimitMax
	}
	return limit
}

// listJobs fetches limit rows, or limit+1 when probing so the caller learns
// whether the page is partial. limit must already be effective.
func (js *Store) listJobs(ctx context.Context, state, consumer string, limit int, probe bool) ([]Row, bool, error) {
	fetch := limit
	if probe {
		fetch++
	}
	q := `SELECT id FROM jobs`
	args := []any{}
	var where []string
	if state != "" {
		where = append(where, `state = ?`)
		args = append(args, state)
	}
	if consumer != "" {
		where = append(where, `consumer = ?`)
		args = append(args, consumer)
	}
	if len(where) != 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, fetch)
	rows, err := js.S.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		idList = append(idList, id)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(idList) > limit {
		idList, truncated = idList[:limit], true
	}
	out := make([]Row, 0, len(idList))
	for _, id := range idList {
		r, err := js.Get(ctx, id)
		if err != nil {
			return nil, false, err
		}
		out = append(out, *r)
	}
	return out, truncated, nil
}

// ListOldest returns the next bounded maintenance page in stable oldest-first
// order. It resumes after the prior page for the same state set and wraps only
// after reaching the end, so permanently parked jobs cannot hide every newer
// repair or reminder.
func (js *Store) ListOldest(ctx context.Context, states []string, limit int) ([]Row, error) {
	if len(states) == 0 {
		return nil, nil
	}
	limit = EffectiveListLimit(limit)
	key := strings.Join(states, "\x00")
	js.oldestMu.Lock()
	defer js.oldestMu.Unlock()
	if js.oldestCursors == nil {
		js.oldestCursors = make(map[string]oldestListCursor)
	}
	cursor := js.oldestCursors[key]
	out, err := js.listOldestAfter(ctx, states, limit, cursor)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 && cursor.createdAt != "" {
		delete(js.oldestCursors, key)
		out, err = js.listOldestAfter(ctx, states, limit, oldestListCursor{})
		if err != nil {
			return nil, err
		}
	}
	if len(out) != 0 {
		last := out[len(out)-1]
		js.oldestCursors[key] = oldestListCursor{createdAt: last.CreatedAt, id: last.ID}
	}
	return out, nil
}

func (js *Store) listOldestAfter(ctx context.Context, states []string, limit int, after oldestListCursor) ([]Row, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, 0, len(states)+4)
	for _, state := range states {
		args = append(args, state)
	}
	q := `SELECT id FROM jobs WHERE state IN (` + placeholders + `)`
	if after.createdAt != "" {
		q += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, after.createdAt, after.createdAt, after.id)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := js.S.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(ids))
	for _, id := range ids {
		row, err := js.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *row)
	}
	return out, nil
}

// validBrowserRoutes and validSessionEvidences mirror the CHECK constraints
// migration 0019 added to candidates.browser_route/session_evidence.
var (
	validBrowserRoutes    = map[string]bool{"resolver": true, "direct": true, "oa": true}
	validSessionEvidences = map[string]bool{"fresh_auth": true, "warm": true, "none": true}
)

// validateCandidateEnums fails closed on an out-of-enum browser_route or
// session_evidence before the row reaches SQLite. InsertCandidates uses
// INSERT OR IGNORE so its (job_id, url_key) dedupe can skip a row without
// erroring — but SQLite treats a CHECK violation exactly the same way under
// OR IGNORE, silently dropping the candidate instead of failing the insert.
// No current writer sets these fields before insert (browser adoption
// inserts with them empty and applies route/evidence afterward via
// ApplyBrowserDeliveryContextToCandidate), so this is currently unreachable
// in practice, but a future writer that does set them must not lose a
// candidate without a trace.
func validateCandidateEnums(c Candidate) error {
	if c.BrowserRoute != "" && !validBrowserRoutes[c.BrowserRoute] {
		return fmt.Errorf("candidate %s: invalid browser_route %q", c.URLKey, c.BrowserRoute)
	}
	if c.SessionEvidence != "" && !validSessionEvidences[c.SessionEvidence] {
		return fmt.Errorf("candidate %s: invalid session_evidence %q", c.URLKey, c.SessionEvidence)
	}
	return nil
}

// InsertCandidates stores ranked candidates (redacted URLs only), deduplicated
// per job by url_key. Returns the number inserted.
func (js *Store) InsertCandidates(ctx context.Context, jobID string, cands []Candidate) (int, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	inserted := 0
	for _, c := range cands {
		if err := validateCandidateEnums(c); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO candidates
			  (job_id, source, url_redacted, url_key, landing_redacted, browser_route, session_evidence, version, access_basis, reuse_license,
			   expected_mime, cost_usd, direct, identity_confidence, rank_evidence, rank, status, review_override, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			jobID, c.Source, c.URLRedacted, c.URLKey, nullable(c.LandingRedacted), nullable(c.BrowserRoute), nullable(c.SessionEvidence),
			c.Version, c.AccessBasis, c.ReuseLicense, nullable(c.ExpectedMIME), c.CostUSD, boolInt(c.Direct), c.IdentityConfidence,
			nullable(c.RankEvidence), c.Rank, boolInt(c.ReviewOverride), now)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NextPendingCandidate returns the best-ranked candidate still pending, or nil.
func (js *Store) NextPendingCandidate(ctx context.Context, jobID string) (*Candidate, error) {
	row := js.S.DB().QueryRowContext(ctx, `
		SELECT id, job_id, source, url_redacted, url_key, COALESCE(landing_redacted,''), COALESCE(browser_route,''), COALESCE(session_evidence,''), version, access_basis,
		       reuse_license, COALESCE(expected_mime,''), cost_usd, direct, identity_confidence,
		       COALESCE(rank_evidence,''), COALESCE(rank,0), status, review_override
		FROM candidates WHERE job_id = ? AND status = 'pending' ORDER BY rank ASC, id ASC LIMIT 1`, jobID)
	var c Candidate
	var direct, override int
	err := row.Scan(&c.ID, &c.JobID, &c.Source, &c.URLRedacted, &c.URLKey, &c.LandingRedacted, &c.BrowserRoute,
		&c.SessionEvidence, &c.Version, &c.AccessBasis, &c.ReuseLicense, &c.ExpectedMIME, &c.CostUSD, &direct, &c.IdentityConfidence,
		&c.RankEvidence, &c.Rank, &c.Status, &override)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Direct, c.ReviewOverride = direct == 1, override == 1
	return &c, nil
}

// MarkCandidate updates one candidate's status.
func (js *Store) MarkCandidate(ctx context.Context, id int64, status string) error {
	_, err := js.S.DB().ExecContext(ctx, `UPDATE candidates SET status = ? WHERE id = ?`, status, id)
	return err
}

// MarkCandidateVersionUnobserved records that nobody observed which version this
// candidate's bytes are. It exists for browser adoption, whose candidate rows are
// keyed by content hash and inserted with INSERT OR IGNORE: a row created before
// papio stopped synthesizing versions survives re-adoption and would otherwise be
// re-read with its old `published` claim (ADR-0007). Deliberately one-way — there
// is no setter that can put a concrete version back, because no caller is ever
// entitled to invent one.
func (js *Store) MarkCandidateVersionUnobserved(ctx context.Context, id int64) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE candidates SET version = 'unknown' WHERE id = ? AND version <> 'unknown'`, id)
	return err
}

// GetCandidate loads one candidate by its durable ID.
func (js *Store) GetCandidate(ctx context.Context, id int64) (*Candidate, error) {
	row := js.S.DB().QueryRowContext(ctx, `
		SELECT id, job_id, source, url_redacted, url_key, COALESCE(landing_redacted,''), COALESCE(browser_route,''), COALESCE(session_evidence,''), version, access_basis,
		       reuse_license, COALESCE(expected_mime,''), cost_usd, direct, identity_confidence,
		       COALESCE(rank_evidence,''), COALESCE(rank,0), status, review_override
		FROM candidates WHERE id = ?`, id)
	var c Candidate
	var direct, override int
	if err := row.Scan(&c.ID, &c.JobID, &c.Source, &c.URLRedacted, &c.URLKey, &c.LandingRedacted, &c.BrowserRoute,
		&c.SessionEvidence, &c.Version, &c.AccessBasis, &c.ReuseLicense, &c.ExpectedMIME, &c.CostUSD, &direct, &c.IdentityConfidence,
		&c.RankEvidence, &c.Rank, &c.Status, &override); err != nil {
		return nil, err
	}
	c.Direct, c.ReviewOverride = direct == 1, override == 1
	return &c, nil
}

// BrowserAccessBasis derives the conservative access claim from observed
// browser-delivery provenance. Missing or incomplete context is deliberately
// manual: an adopted file must never become institutional on an inference.
func BrowserAccessBasis(route, sessionEvidence string) (string, error) {
	switch route {
	case "oa":
		if sessionEvidence != "none" {
			return "", fmt.Errorf("oa browser delivery requires session_evidence none")
		}
		return "open_access", nil
	case "resolver", "direct":
		switch sessionEvidence {
		case "fresh_auth", "warm":
			return "institutional", nil
		case "none":
			return "manual", nil
		default:
			return "", fmt.Errorf("invalid browser session_evidence %q", sessionEvidence)
		}
	default:
		return "", fmt.Errorf("invalid browser route %q", route)
	}
}

// BrowserSessionFreshlyEvidenced reports whether recorded delivery context
// carries recent positive evidence that the operator's institutional session
// was authenticated, as opposed to merely reaching an institutional route.
//
// It answers a narrower question than BrowserAccessBasis and defers to it for
// the route/evidence lattice rather than restating it, so the two cannot drift.
// "warm" still derives an institutional basis — a session evidenced at some
// point is a real one — but the extension's currentSessionEvidence tiers these
// two purely on the AGE of that origin's evidence, so "warm" means the evidence
// aged past its TTL with nothing confirming the session since, and "fresh_auth"
// means something confirmed it recently. This predicate is therefore about
// recency of confirmation, NOT about observing a login: a keepalive probe
// committing "in" mints fresh_auth without any login, and the extension reports
// that same observation to the daemon as "warm_verified". Do not rename this to
// imply a witnessed login; an earlier name did and it was wrong.
//
// Callers that publish a rights claim about the session want this predicate;
// callers recording how the bytes were obtained want the basis.
//
// Empty evidence is false by construction, which is what keeps every adoption
// with no recorded context out — whether it predates migration 0019 or arrived
// through a path that carried no context at all (a directory-scan adoption
// always does, and a delivery context can be pruned by its TTL before the
// completion frame lands).
func BrowserSessionFreshlyEvidenced(route, sessionEvidence string) bool {
	if sessionEvidence != "fresh_auth" {
		return false
	}
	basis, err := BrowserAccessBasis(route, sessionEvidence)
	return err == nil && basis == "institutional"
}

// ApplyBrowserDeliveryContextToCandidate records route/session evidence on the
// candidate created by one browser adoption. The durable candidate ID is the
// binding: a prior browser candidate for the same job can never receive a
// later download's provenance.
func (js *Store) ApplyBrowserDeliveryContextToCandidate(ctx context.Context, jobID string, candidateID int64, route, sessionEvidence, landingRedacted string) (bool, error) {
	accessBasis, err := BrowserAccessBasis(route, sessionEvidence)
	if err != nil {
		return false, err
	}
	var landing any
	if landingRedacted != "" {
		landing = landingRedacted
	}
	res, err := js.S.DB().ExecContext(ctx, `
		UPDATE candidates
		SET browser_route = ?, session_evidence = ?, access_basis = ?,
		    landing_redacted = CASE WHEN ? IS NULL THEN landing_redacted ELSE ? END
		WHERE id = ? AND job_id = ? AND source = 'browser'`,
		route, sessionEvidence, accessBasis, landing, landing, candidateID, jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ApplyBrowserDeliveryContext is retained for callers compiled against the
// pre-binding API, but deliberately fails closed: without the candidate ID it
// cannot prove which download produced a browser row.
func (js *Store) ApplyBrowserDeliveryContext(ctx context.Context, jobID, route, sessionEvidence, landingRedacted string) (bool, error) {
	return false, nil
}

// ResetCandidates makes interrupted and retryable candidates runnable for a
// fresh resolution pass. Invalid/skipped candidates stay exhausted.
func (js *Store) ResetCandidates(ctx context.Context, jobID string) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE candidates SET status = 'pending' WHERE job_id = ? AND status IN ('fetching','retryable')`, jobID)
	return err
}

// Attempt records one resolve/fetch/validate execution.
func (js *Store) StartAttempt(ctx context.Context, jobID string, candidateID int64, stage, source string) (int64, error) {
	var cand any
	if candidateID > 0 {
		cand = candidateID
	}
	res, err := js.S.DB().ExecContext(ctx,
		`INSERT INTO attempts (job_id, candidate_id, stage, source, started_at) VALUES (?, ?, ?, ?, ?)`,
		jobID, cand, stage, nullable(source), store.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishAttempt closes an attempt with its outcome. detail must be redacted.
func (js *Store) FinishAttempt(ctx context.Context, attemptID int64, outcome string, httpStatus int, detail string) error {
	var status any
	if httpStatus > 0 {
		status = httpStatus
	}
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE attempts SET ended_at = ?, outcome = ?, http_status = ?, detail = ? WHERE id = ?`,
		store.Now(), outcome, status, nullable(detail), attemptID)
	return err
}

// UpsertArtifact records a validated artifact (content-addressed; idempotent).
type Artifact struct {
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	MIME             string `json:"mime"`
	PageCount        int    `json:"page_count"`
	TextChars        int64  `json:"text_chars"`
	OCRUsed          bool   `json:"ocr_used"`
	Encrypted        bool   `json:"encrypted"`
	HasActiveContent bool   `json:"has_active_content"`
	IdentityResult   string `json:"identity_result,omitempty"`
	Path             string `json:"path"`
	CreatedAt        string `json:"created_at"`
}

// UpsertArtifact inserts the artifact row if new.
func (js *Store) UpsertArtifact(ctx context.Context, a Artifact) error {
	_, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO artifacts (sha256, size_bytes, mime, page_count, text_chars, ocr_used, encrypted, has_active_content, identity_result, path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sha256) DO UPDATE SET identity_result = excluded.identity_result`,
		a.SHA256, a.SizeBytes, a.MIME, a.PageCount, a.TextChars, boolInt(a.OCRUsed), boolInt(a.Encrypted),
		boolInt(a.HasActiveContent), nullable(a.IdentityResult), a.Path, store.Now())
	return err
}

// Component roles. A role is acquisition-local: identical bytes can be one job's
// main file and another job's supplement, so the role cannot live on the shared
// artifacts row (ADR-0007).
const (
	ComponentMain         = "main"
	ComponentHTMLFullText = "html_fulltext"
	ComponentSupplement   = "supplement"
	ComponentAppendix     = "appendix"
)

// Component is one artifact a job obtained, in the role it was obtained as, with
// the identity finding THAT acquisition recorded. Identity lives here rather than
// on the artifact because it is computed against a per-job target: on the shared
// artifacts row it is last-writer-wins across every job holding the same digest.
type Component struct {
	Role           string `json:"role"`
	SHA256         string `json:"sha256"`
	CandidateID    int64  `json:"candidate_id,omitempty"`
	IdentityResult string `json:"identity_result,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// AddComponent records a non-main component. The main component is written by
// the state transition that attaches the artifact, so callers never race it.
func (js *Store) AddComponent(ctx context.Context, jobID, sha, role string, candidateID int64, identityResult string) error {
	switch role {
	case ComponentHTMLFullText, ComponentSupplement, ComponentAppendix:
	case ComponentMain:
		return fmt.Errorf("%w: the main component is recorded by the artifact transition", ErrConflict)
	default:
		return fmt.Errorf("%w: unknown component role %q", ErrConflict, role)
	}
	var candidate any
	if candidateID != 0 {
		candidate = candidateID
	}
	_, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO job_artifacts (job_id, artifact_sha256, role, candidate_id, identity_result, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id, role, artifact_sha256) DO UPDATE SET
		        candidate_id = excluded.candidate_id,
		        identity_result = excluded.identity_result`,
		jobID, sha, role, candidate, nullable(identityResult), store.Now())
	return err
}

// Components lists everything one job obtained, main first. An empty result means
// the job holds no artifact; it never means "unknown".
func (js *Store) Components(ctx context.Context, jobID string) ([]Component, error) {
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT role, artifact_sha256, COALESCE(candidate_id, 0), COALESCE(identity_result, ''), created_at
		  FROM job_artifacts WHERE job_id = ?
		 ORDER BY CASE role WHEN 'main' THEN 0 ELSE 1 END, role, artifact_sha256`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Component
	for rows.Next() {
		var c Component
		if err := rows.Scan(&c.Role, &c.SHA256, &c.CandidateID, &c.IdentityResult, &c.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// AcquisitionIdentity returns the identity finding this job's own acquisition of
// its main component recorded, which is the only per-acquisition identity on
// record. Falls back to the shared artifact row only when no edge exists, which
// can only happen for a job whose artifact predates the acquisition edge and was
// somehow missed by its backfill.
func (js *Store) AcquisitionIdentity(ctx context.Context, jobID, sha string) (string, error) {
	var identity sql.NullString
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT identity_result FROM job_artifacts WHERE job_id = ? AND artifact_sha256 = ? AND role = 'main'`,
		jobID, sha).Scan(&identity)
	if err == nil {
		return identity.String, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	artifact, err := js.GetArtifact(ctx, sha)
	if err != nil || artifact == nil {
		return "", err
	}
	return artifact.IdentityResult, nil
}

// GetArtifact loads one artifact row by hash.
func (js *Store) GetArtifact(ctx context.Context, sha string) (*Artifact, error) {
	var a Artifact
	var ocr, enc, active int
	var identity sql.NullString
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT sha256, size_bytes, mime, COALESCE(page_count,0), COALESCE(text_chars,0), ocr_used, encrypted,
		       has_active_content, identity_result, path, created_at
		FROM artifacts WHERE sha256 = ?`, sha).Scan(
		&a.SHA256, &a.SizeBytes, &a.MIME, &a.PageCount, &a.TextChars, &ocr, &enc, &active, &identity, &a.Path, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.OCRUsed, a.Encrypted, a.HasActiveContent = ocr == 1, enc == 1, active == 1
	a.IdentityResult = identity.String
	return &a, nil
}

// CandidateForArtifact returns the accepted candidate provenance for one job's
// artifact. The job's own selection wins, but only when it is genuinely
// accepted; otherwise a content-hash scan recovers the acquisition that did
// produce these bytes.
//
// Preferring the job's own row is load-bearing, not an optimization. artifacts
// are content-addressed and shared, so two acquisitions can hold identical bytes
// under different reuse licences and access bases; resolving provenance by hash
// alone reports some other job's terms, which is first-writer-wins rights
// attribution on a digest (ADR-0007).
//
// The accepted check is equally load-bearing. selected_candidate_id is written
// when a fetch starts, before validation, and the transition SQL COALESCEs it
// forward — so a job can carry a REJECTED selection through crash recovery or a
// scheduler retry and then complete from the local cache, whose transition only
// records the artifact. Trusting the raw pointer would publish the licence of a
// file papio actually threw away.
func (js *Store) CandidateForArtifact(ctx context.Context, jobID, sha string) (*Candidate, error) {
	var own sql.NullInt64
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT selected_candidate_id FROM jobs WHERE id = ?`, jobID).Scan(&own)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if own.Valid {
		candidate, err := js.GetCandidate(ctx, own.Int64)
		if err != nil {
			return nil, err
		}
		if candidate != nil && candidate.JobID == jobID && candidate.Status == CandidateAccepted {
			return candidate, nil
		}
	}
	var id sql.NullInt64
	err = js.S.DB().QueryRowContext(ctx, `
		SELECT j.selected_candidate_id FROM jobs j
		JOIN candidates c ON c.id = j.selected_candidate_id
		WHERE j.artifact_sha256 = ? AND j.state IN ('ready','imported') AND c.status = ?
		ORDER BY j.updated_at ASC LIMIT 1`, sha, CandidateAccepted).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return js.GetCandidate(ctx, id.Int64)
}

// FindArtifactByDOI returns a prior validated artifact for the same DOI, if any
// job with that DOI reached ready (resolver order step 1: local cache), together
// with the accepted candidate of the acquisition that produced it. The caller
// must record that candidate on the cache transition: the bytes are that
// acquisition's, so its licence and access basis are the honest provenance, and
// without it the completing job has no candidate of its own and provenance has
// to be guessed from the digest later (ADR-0007).
func (js *Store) FindArtifactByDOI(ctx context.Context, doi string) (*Artifact, *Candidate, error) {
	var sha string
	var candidateID sql.NullInt64
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT j.artifact_sha256, j.selected_candidate_id FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = 'doi' AND i.value = ? AND j.state IN ('ready','imported') AND j.artifact_sha256 IS NOT NULL
		ORDER BY j.updated_at DESC LIMIT 1`, doi).Scan(&sha, &candidateID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	artifact, err := js.GetArtifact(ctx, sha)
	if err != nil || artifact == nil {
		return nil, nil, err
	}
	if !candidateID.Valid {
		return artifact, nil, nil
	}
	candidate, err := js.GetCandidate(ctx, candidateID.Int64)
	if err != nil {
		return nil, nil, err
	}
	if candidate != nil && candidate.Status != CandidateAccepted {
		// The source job carried a stale selection forward; its digest is still
		// valid but its pointer is not provenance.
		candidate = nil
	}
	return artifact, candidate, nil
}

// HumanActionBinding ties an identity-review action to the exact candidate and
// quarantined file the reviewer sees.
type HumanActionBinding struct {
	CandidateID      int64
	QuarantinePath   string
	QuarantineSHA256 string
}

// AcceptedReviewBinding returns the pending candidate and quarantined file
// preserved by the latest accepted identity review, if it remains reusable.
func (js *Store) AcceptedReviewBinding(ctx context.Context, jobID string) (*HumanActionBinding, error) {
	var binding HumanActionBinding
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT ha.candidate_id, ha.quarantine_path, ha.quarantine_sha256
		FROM human_actions ha
		JOIN candidates c ON c.id = ha.candidate_id AND c.job_id = ha.job_id
		WHERE ha.job_id = ?
		  AND ha.kind = 'verify_identity'
		  AND ha.status = 'resolved'
		  AND c.review_override = 1
		  AND c.status = 'pending'
		ORDER BY ha.resolved_at DESC, ha.id DESC
		LIMIT 1`, jobID).Scan(&binding.CandidateID, &binding.QuarantinePath, &binding.QuarantineSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading accepted review binding: %w", err)
	}
	return &binding, nil
}

type openHumanActionOptions struct {
	binding                    *HumanActionBinding
	requiresAuth               bool
	blockedBy                  string
	inheritResolvedHandoffAuth bool
}

// OpenHumanActionOption configures one human action.
type OpenHumanActionOption func(*openHumanActionOptions) error

// WithHumanActionBinding persists the immutable identity-review inputs used by
// preview and compare-and-swap review resolution.
func WithHumanActionBinding(binding HumanActionBinding) OpenHumanActionOption {
	return func(options *openHumanActionOptions) error {
		if binding.CandidateID <= 0 {
			return errors.New("human action binding requires a candidate ID")
		}
		if strings.TrimSpace(binding.QuarantinePath) == "" {
			return errors.New("human action binding requires a quarantine path")
		}
		if !validSHA256(binding.QuarantineSHA256) {
			return errors.New("human action binding requires a SHA-256")
		}
		binding.QuarantinePath = strings.TrimSpace(binding.QuarantinePath)
		binding.QuarantineSHA256 = strings.ToLower(strings.TrimSpace(binding.QuarantineSHA256))
		options.binding = &binding
		return nil
	}
}

// AccessClassification records why a human action exists and whether finishing
// it needs an authenticated institutional session.
//
// It is a required argument to OpenHumanAction rather than an option, and that
// is the point of the type. As an option it was supplied at four of twelve call
// sites; the rest silently took Go's zero value, which reads as "no sign-in
// needed" — the one answer the access-mode safety check most needs to be right,
// and it was wrong at two sites including the action the extension had
// explicitly reported as auth-walled. Omission is now a compile error.
type AccessClassification struct {
	requiresAuth bool
	blockedBy    string
	inherit      bool
}

// Access states the classification outright. blockedBy is one of "anti_bot",
// "paywall", "landing_page", or "".
func Access(requiresAuth bool, blockedBy string) AccessClassification {
	return AccessClassification{requiresAuth: requiresAuth, blockedBy: blockedBy}
}

// AccessInheritedFromResolvedHandoff keeps replacement guidance honest after a
// browser handoff has already closed. A landing page can block papio
// differently than the paywall did, but it cannot establish that the user's
// institutional sign-in is no longer needed, so the prior handoff's answer is
// carried forward and a missing one fails closed.
func AccessInheritedFromResolvedHandoff(blockedBy string) AccessClassification {
	return AccessClassification{blockedBy: blockedBy, inherit: true}
}

func (a AccessClassification) apply(options *openHumanActionOptions) error {
	switch a.blockedBy {
	case "", "anti_bot", "paywall", "landing_page":
	default:
		return fmt.Errorf("invalid human action blocked_by %q", a.blockedBy)
	}
	options.requiresAuth = a.requiresAuth
	options.blockedBy = a.blockedBy
	options.inheritResolvedHandoffAuth = a.inherit
	return nil
}

func validSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// OpenHumanAction records a required human step for a job. Re-parking the
// same job and action kind refreshes the existing action rather than creating
// another open prompt.
func (js *Store) OpenHumanAction(ctx context.Context, jobID, kind, detail string, access AccessClassification, opts ...OpenHumanActionOption) (int64, error) {
	var options openHumanActionOptions
	if err := access.apply(&options); err != nil {
		return 0, err
	}
	for _, option := range opts {
		if option == nil {
			continue
		}
		if err := option(&options); err != nil {
			return 0, err
		}
	}
	if options.binding != nil && kind != "verify_identity" {
		return 0, errors.New("human action binding is only valid for verify_identity")
	}

	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var actionID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM human_actions
		 WHERE job_id = ? AND kind = ? AND status = 'open'
		 ORDER BY id ASC LIMIT 1`, jobID, kind).Scan(&actionID)
	switch {
	case err == nil:
		if options.binding == nil {
			_, err = tx.ExecContext(ctx,
				`UPDATE human_actions SET detail = ?, requires_auth = ?, blocked_by = ?, revision = revision + 1 WHERE id = ?`,
				nullable(detail), options.requiresAuth, options.blockedBy, actionID)
		} else {
			_, err = tx.ExecContext(ctx, `
				UPDATE human_actions
				SET detail = ?, requires_auth = ?, blocked_by = ?, candidate_id = ?, quarantine_path = ?, quarantine_sha256 = ?,
					revision = revision + 1
				WHERE id = ?`,
				nullable(detail), options.requiresAuth, options.blockedBy, options.binding.CandidateID, options.binding.QuarantinePath,
				options.binding.QuarantineSHA256, actionID)
		}
		if err != nil {
			return 0, err
		}
	case errors.Is(err, sql.ErrNoRows):
		binding := options.binding
		candidateID, path, sha := any(nil), "", ""
		if binding != nil {
			candidateID, path, sha = binding.CandidateID, binding.QuarantinePath, binding.QuarantineSHA256
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO human_actions
				(job_id, kind, status, detail, requires_auth, blocked_by, candidate_id, quarantine_path, quarantine_sha256, revision, created_at)
			VALUES (?, ?, 'open', ?, ?, ?, ?, ?, ?, 1, ?)`,
			jobID, kind, nullable(detail), options.requiresAuth, options.blockedBy, candidateID, path, sha, store.Now())
		if err != nil {
			return 0, err
		}
		actionID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	default:
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return actionID, nil
}

// ResolveHumanAction closes one open action with a compare-and-swap update.
func (js *Store) ResolveHumanAction(ctx context.Context, actionID int64, status string) error {
	if status != "resolved" && status != "cancelled" {
		return fmt.Errorf("invalid human action status %q", status)
	}
	res, err := js.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = 'open'`,
		status, store.Now(), actionID)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		var exists int
		if err := js.S.DB().QueryRowContext(ctx, `SELECT 1 FROM human_actions WHERE id = ?`, actionID).Scan(&exists); err != nil {
			return err
		}
		return fmt.Errorf("%w: human action %d is not open", ErrConflict, actionID)
	}
	return nil
}

// ResolveReview applies a human accept or reject verdict to a parked identity
// review. It atomically closes the action and moves the job to its next durable
// boundary, leaving no interval in which a closed action still parks a job.
// ReviewOutcome reports the result of a compare-and-swap identity review.
type ReviewOutcome string

const (
	ReviewApplied        ReviewOutcome = "applied"
	ReviewAlreadyApplied ReviewOutcome = "already_applied"
	ReviewConflict       ReviewOutcome = "conflict"
)

// ResolveReviewInput supplies the immutable snapshot fields required for a
// modern review transition.
type ResolveReviewInput struct {
	ActionID         int64
	Verdict          string
	ExpectedRevision int64
	ExpectedSHA256   string
}

// ReviewResolution is the durable result of resolving an identity review.
type ReviewResolution struct {
	Outcome ReviewOutcome
	JobID   string
	State   string
}

// ResolveReview preserves the legacy action-resolution API for CLI callers
// that predate review bindings. New callers must use ResolveReviewCAS.
func (js *Store) ResolveReview(ctx context.Context, actionID int64, verdict string) (string, string, error) {
	resolution, err := js.resolveReview(ctx, ResolveReviewInput{
		ActionID: actionID, Verdict: verdict,
	}, true)
	if err != nil {
		return "", "", err
	}
	if resolution.Outcome != ReviewApplied {
		return "", "", fmt.Errorf("%w: human action %d is not open", ErrConflict, actionID)
	}
	return resolution.JobID, resolution.State, nil
}

// ResolveReviewCAS atomically applies a review only when its rendered binding
// still identifies the same open action and quarantined file.
func (js *Store) ResolveReviewCAS(ctx context.Context, input ResolveReviewInput) (ReviewResolution, error) {
	if input.ExpectedRevision <= 0 {
		return ReviewResolution{}, errors.New("expected review revision is required")
	}
	if input.Verdict == "accept" {
		if !validSHA256(input.ExpectedSHA256) {
			return ReviewResolution{}, errors.New("expected SHA-256 is required for accept")
		}
		input.ExpectedSHA256 = strings.ToLower(strings.TrimSpace(input.ExpectedSHA256))
	}
	return js.resolveReview(ctx, input, false)
}

func (js *Store) resolveReview(ctx context.Context, input ResolveReviewInput, legacy bool) (ReviewResolution, error) {
	if input.ActionID <= 0 || (input.Verdict != "accept" && input.Verdict != "reject") {
		return ReviewResolution{}, fmt.Errorf("invalid review action or verdict")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	defer func() { _ = tx.Rollback() }()

	var action HumanAction
	err = tx.QueryRowContext(ctx, `
		SELECT id, job_id, kind, status, COALESCE(detail,''), created_at,
			COALESCE(candidate_id, 0), quarantine_path, quarantine_sha256, revision
		FROM human_actions WHERE id = ?`, input.ActionID).Scan(
		&action.ID, &action.JobID, &action.Kind, &action.Status, &action.Detail, &action.CreatedAt,
		&action.CandidateID, &action.QuarantinePath, &action.QuarantineSHA256, &action.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		if legacy {
			return ReviewResolution{}, err
		}
		return ReviewResolution{Outcome: ReviewConflict}, nil
	}
	if err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	if action.Kind != "verify_identity" {
		return ReviewResolution{}, &ErrHumanActionKind{ActionID: input.ActionID, Kind: action.Kind}
	}
	if action.Status != "open" {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, action.JobID).Scan(&state); err != nil {
			return ReviewResolution{}, normalizeReviewBusy(err)
		}
		var applied int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM events
			WHERE job_id = ? AND kind = 'human_action.resolve'
				AND detail_json LIKE ? AND detail_json LIKE ?
			ORDER BY seq DESC LIMIT 1`,
			action.JobID,
			fmt.Sprintf("%%\"action_id\":%d%%", action.ID),
			fmt.Sprintf("%%\"verdict\":\"%s\"%%", input.Verdict),
		).Scan(&applied)
		switch {
		case err == nil:
			return ReviewResolution{Outcome: ReviewAlreadyApplied, JobID: action.JobID, State: state}, nil
		case errors.Is(err, sql.ErrNoRows):
			return ReviewResolution{Outcome: ReviewConflict, JobID: action.JobID, State: state}, nil
		default:
			return ReviewResolution{}, normalizeReviewBusy(err)
		}
	}
	if !legacy && action.Revision != input.ExpectedRevision {
		return ReviewResolution{Outcome: ReviewConflict, JobID: action.JobID}, nil
	}

	var from string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, action.JobID).Scan(&from); err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	if from != StateNeedsReview {
		return ReviewResolution{Outcome: ReviewConflict, JobID: action.JobID, State: from}, nil
	}

	now := store.Now()
	query := `UPDATE human_actions SET status = 'resolved', resolved_at = ? WHERE id = ? AND status = 'open'`
	args := []any{now, action.ID}
	if !legacy {
		query += ` AND revision = ?`
		args = append(args, input.ExpectedRevision)
		if input.Verdict == "accept" {
			query += ` AND quarantine_sha256 = ?`
			args = append(args, input.ExpectedSHA256)
		}
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	if changed, err := res.RowsAffected(); err != nil {
		return ReviewResolution{}, err
	} else if changed != 1 {
		return ReviewResolution{Outcome: ReviewConflict, JobID: action.JobID}, nil
	}

	to, reason := StateCancelled, "review_rejected"
	terminalReason := TerminalReasonReviewRejected
	if input.Verdict == "accept" {
		candidateID := action.CandidateID
		if candidateID == 0 {
			var candidate sql.NullInt64
			err := tx.QueryRowContext(ctx, `
				SELECT candidate_id FROM attempts
				WHERE job_id = ? AND stage = 'validate' AND outcome = 'needs_review'
				ORDER BY id DESC LIMIT 1`, action.JobID).Scan(&candidate)
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
			if err != nil {
				return ReviewResolution{}, normalizeReviewBusy(err)
			}
			if candidate.Valid {
				candidateID = candidate.Int64
			}
		}
		if candidateID > 0 {
			res, err := tx.ExecContext(ctx,
				`UPDATE candidates SET review_override = 1, status = 'pending' WHERE id = ? AND job_id = ?`,
				candidateID, action.JobID)
			if err != nil {
				return ReviewResolution{}, normalizeReviewBusy(err)
			}
			if changed, err := res.RowsAffected(); err != nil {
				return ReviewResolution{}, err
			} else if changed != 1 {
				return ReviewResolution{Outcome: ReviewConflict, JobID: action.JobID}, nil
			}
			to = StateFetching
		} else {
			to = StateResolving
		}
		reason = "review_accepted"
		terminalReason = ""
	}
	detail, err := json.Marshal(map[string]any{"from": from, "to": to, "reason": reason})
	if err != nil {
		return ReviewResolution{}, err
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, lease_owner = NULL, lease_expires_at = NULL,
		        retry_at = NULL, terminal_reason = ?, selected_candidate_id = NULL, artifact_sha256 = NULL
		WHERE id = ? AND state = ?`,
		to, now, nullable(string(terminalReason)), action.JobID, from)
	if err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	if changed, err := res.RowsAffected(); err != nil {
		return ReviewResolution{}, err
	} else if changed != 1 {
		return ReviewResolution{Outcome: ReviewConflict, JobID: action.JobID}, nil
	}
	if Terminal(to) {
		if err := closeTerminalHumanActions(ctx, tx, action.JobID, to, now); err != nil {
			return ReviewResolution{}, normalizeReviewBusy(err)
		}
	}
	resolutionDetail, err := json.Marshal(map[string]any{"action_id": action.ID, "verdict": input.Verdict})
	if err != nil {
		return ReviewResolution{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'human_action.resolve', ?)`,
		action.JobID, now, string(resolutionDetail)); err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		action.JobID, now, string(detail)); err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	if err := tx.Commit(); err != nil {
		return ReviewResolution{}, normalizeReviewBusy(err)
	}
	return ReviewResolution{Outcome: ReviewApplied, JobID: action.JobID, State: to}, nil
}

func normalizeReviewBusy(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "sqlite_busy") || strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") {
		return fmt.Errorf("%w: review transaction busy", ErrConflict)
	}
	return err
}

// DismissHumanAction atomically closes an open human action (compare-and-swap
// on revision). It cancels the job only when that job is currently parked on
// the dismissed action: awaiting_human for openurl_handoff, manual_download,
// openurl_available, or document_delivery; or needs_review for
// verify_identity. A stale action from another state is closed without
// disturbing the job's live work. downloads_access_required is deliberately
// excluded from the awaiting_human list even though that action also parks a
// job there: the pending download itself is fine, only the folder grant is
// missing, so dismissing it must never cancel the acquisition.
func (js *Store) DismissHumanAction(ctx context.Context, actionID, expectedRevision int64) (jobID string, err error) {
	if actionID <= 0 || expectedRevision <= 0 {
		return "", errors.New("dismiss requires a positive action ID and revision")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", normalizeReviewBusy(err)
	}
	defer func() { _ = tx.Rollback() }()

	var actionKind, state string
	if err := tx.QueryRowContext(ctx, `
		SELECT human_actions.job_id, human_actions.kind, jobs.state
		FROM human_actions
		JOIN jobs ON jobs.id = human_actions.job_id
		WHERE human_actions.id = ?`, actionID).Scan(&jobID, &actionKind, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: human action %d does not exist", ErrConflict, actionID)
		}
		return "", normalizeReviewBusy(err)
	}
	now := store.Now()
	res, err := tx.ExecContext(ctx,
		`UPDATE human_actions SET status = 'cancelled', resolved_at = ? WHERE id = ? AND status = 'open' AND revision = ?`,
		now, actionID, expectedRevision)
	if err != nil {
		return "", normalizeReviewBusy(err)
	}
	if changed, err := res.RowsAffected(); err != nil {
		return "", err
	} else if changed != 1 {
		return "", fmt.Errorf("%w: human action %d is not open at revision %d", ErrConflict, actionID, expectedRevision)
	}
	resolutionDetail, err := json.Marshal(map[string]any{"action_id": actionID, "verdict": "dismiss"})
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'human_action.resolve', ?)`,
		jobID, now, string(resolutionDetail)); err != nil {
		return "", normalizeReviewBusy(err)
	}
	if err := tx.Commit(); err != nil {
		return "", normalizeReviewBusy(err)
	}
	if dismissalCancelsParkedJob(actionKind, state) {
		return jobID, js.Cancel(ctx, jobID, TerminalReasonUserDismissed)
	}
	return jobID, nil
}

func dismissalCancelsParkedJob(actionKind, state string) bool {
	switch state {
	case StateAwaitingHuman:
		// downloads_access_required is awaiting_human too, but intentionally
		// absent here — see DismissHumanAction's doc comment.
		return actionKind == "openurl_handoff" || actionKind == "manual_download" || actionKind == "openurl_available" || actionKind == ActionKindDocumentDelivery
	case StateNeedsReview:
		return actionKind == "verify_identity"
	default:
		return false
	}
}

// HumanAction is one pending or resolved human step.
type HumanAction struct {
	ID               int64  `json:"id"`
	JobID            string `json:"job_id"`
	Kind             string `json:"kind"`
	Status           string `json:"status"`
	Detail           string `json:"detail,omitempty"`
	RequiresAuth     bool   `json:"requires_auth"`
	BlockedBy        string `json:"blocked_by,omitempty"`
	CreatedAt        string `json:"created_at"`
	CandidateID      int64  `json:"candidate_id,omitempty"`
	QuarantinePath   string `json:"quarantine_path,omitempty"`
	QuarantineSHA256 string `json:"quarantine_sha256,omitempty"`
	Revision         int64  `json:"revision"`
}

// QuiesceAfter is how long an open human action keeps drawing papio's own
// initiative — automatic browser offers and escalating reminders — before it
// goes quiet.
//
// It exists because "open forever" and "notified forever" were the same thing.
// The reminder backoff caps the interval at 24h but never the count, and the
// bridge re-offers every open handoff whenever a browser session goes live, so
// one handoff nobody could complete produced roughly sixty tabs and seven
// notifications over three days. Some of those are papio's fault and get fixed
// at the source (a DOI that does not exist is now caught before the park); the
// rest are not, and never will be — a title the library simply does not hold,
// a provider that changed its login, a job the user has decided to ignore.
//
// Going quiet is deliberately NOT expiry. The action stays open, stays listed,
// and stays openable: `papio actions open` drives a quiesced handoff on
// demand, because an explicit command is user intent in a way a session-live
// tick is not. papio simply stops volunteering. `papio doctor` reports the
// count so the queue does not go silently stale.
//
// Seven days is one working week: long enough that an ordinary "I'll get to it
// after the weekend" handoff is untouched, short enough that a dead one stops
// costing a tab per session.
const QuiesceAfter = 7 * 24 * time.Hour

// Quiesced reports whether this action has been open long enough that papio
// should stop acting on it unprompted. An unparseable timestamp reports false:
// the visible, noisy behaviour is the safe failure here, because the quiet one
// would strand an action with nothing pointing at it.
func (a HumanAction) Quiesced(now time.Time) bool {
	created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
	if err != nil {
		return false
	}
	return now.Sub(created) >= QuiesceAfter
}

// HandoffAcceptedLease bounds how long a single accepted browser drive can
// justify holding an epoch open before the absence of any terminal or
// progress event marks that drive fruitless. It is deliberately longer than
// the extension's own 3-minute drive timeout so one physical drive — which
// may re-acknowledge across a service-worker restart — is never split into
// two counted epochs.
//
// It must also exceed the longest an accepted handoff can sit WITHOUT being
// driven at all, because the extension sends job_accept on its queued path
// too (background.ts queues under governor pressure, then releases after
// QUEUED_HANDOFF_RELEASE_MS; ADR-0013's 2026-08-06 addendum ratifies that
// 45-second evidence-free release). Nothing daemon-side distinguishes queued
// from driving, so a lease shorter than the queue wait would charge a job for
// waiting its turn and quiesce a healthy backlog — the same mistake as
// counting raw offers, one layer down. The bound is the extension's own
// arithmetic: at most maxOutstandingOffers (4) accepted handoffs against
// HANDOFF_DRIVE_LIMIT (2) slots, each held up to the 3-minute drive timeout,
// plus the 45-second release, so 6m45s worst case. Ten minutes clears that
// with margin and still settles the incident this exists for, whose epochs
// were hours to days apart: replayed over its real event history it yields
// 15 fruitless epochs against a bound of 3.
const HandoffAcceptedLease = 10 * time.Minute

// MaxAutomaticHandoffEpochs is how many fruitless drive epochs an open
// handoff tolerates before papio stops re-offering it on its own initiative.
// In the verified field incident a single openurl_handoff for a paper that
// needed no institution was offered 38 times over three days with zero
// terminal browser.provider_outcome events, pinning both of the extension's
// handoff drive slots and spawning a duplicate tab per service-worker
// restart — and the action was only 3.07 days old the whole time, well
// inside QuiesceAfter's seven-day fence. Time alone was never going to catch
// this; it needed evidence from what each accepted drive actually did.
//
// These are safety invariants, not operator tuning: a config field would
// force a config/binary lockstep deploy for a number that should never need
// per-deployment adjustment.
const MaxAutomaticHandoffEpochs = 3

// HandoffOfferState is the automatic-offer decision folded from a job's
// persisted event history: how many browser drives in a row produced no
// terminal or progress signal, and whether that streak should stop papio
// from re-offering unprompted.
type HandoffOfferState struct {
	FruitlessEpochs int
	Quiesced        bool
}

// terminalHandoffEvent reports whether an event proves the drive produced
// something. It is the single definition the fold consults twice: once to
// exempt a late signal from the lease boundary, and once to close its epoch.
// Keep it beside the switch that uses it — two drifting notions of "terminal"
// would resurrect the boundary bug it exists to prevent.
func terminalHandoffEvent(kind string, event map[string]any) bool {
	switch kind {
	case "browser.provider_outcome", "browser.download_started", "browser.download_complete":
		return true
	case "job.transition":
		detail, _ := event["detail"].(map[string]any)
		from, _ := detail["from"].(string)
		to, _ := detail["to"].(string)
		return from == StateAwaitingHuman && to != StateAwaitingHuman
	}
	return false
}

// ProjectHandoffOfferState folds a job's event history into the automatic-offer
// decision. events are the `[]map[string]any` rows returned by the store, oldest
// first; each has a "kind" string and a "detail" map. actionCreatedAt scopes the
// fold to the currently open human action: a job that re-entered awaiting_human
// carries its whole history in events, and a prior, already-resolved action's
// drives must not count against a fresh one.
//
// An unparseable actionCreatedAt, or an unparseable event timestamp, fails
// open (not quiesced) for the same reason Quiesced does: the noisy failure is
// the safe one here, because the quiet one strands the action.
func ProjectHandoffOfferState(events []map[string]any, actionCreatedAt string, now time.Time) HandoffOfferState {
	created, err := time.Parse(time.RFC3339Nano, actionCreatedAt)
	if err != nil {
		return HandoffOfferState{}
	}

	var epochStart time.Time
	open := false
	fruitless := 0
	closeEpoch := func(fruitlessClose bool) {
		if !open {
			return
		}
		open = false
		if fruitlessClose {
			fruitless++
		} else {
			fruitless = 0
		}
	}

	for _, event := range events {
		atStr, _ := event["at"].(string)
		at, err := time.Parse(time.RFC3339Nano, atStr)
		if err != nil {
			return HandoffOfferState{}
		}
		if at.Before(created) {
			continue // belongs to a prior, already-resolved human action
		}
		// Classify before the lease boundary is applied. A terminal signal
		// closes the epoch it belongs to however late it lands: the lease
		// bounds SILENCE, and a drive that finally reported is not silent. An
		// SSO or 2FA detour can easily outrun ten minutes, and force-closing
		// that epoch as fruitless first would both charge a successful drive
		// and swallow its reset, since closeEpoch no-ops once the epoch is
		// shut. Three slow-but-successful drives would then quiesce a healthy
		// job — the false-quiescing this fence exists to prevent.
		kind, _ := event["kind"].(string)
		if open && !terminalHandoffEvent(kind, event) && at.Sub(epochStart) >= HandoffAcceptedLease {
			closeEpoch(true) // lease elapsed before anything terminal arrived
		}
		switch {
		case kind == "browser.handoff_offered" || kind == "browser.job_accept":
			// Reconnect re-acknowledgements land here too; if an epoch is
			// already open and still within lease, this is the SAME epoch —
			// counting raw offers would turn one stuck drive into dozens.
			// browser.job_reject and send failures are transport problems,
			// not a fruitless drive: they match nothing here and fall through.
			if !open {
				epochStart = at
				open = true
			}
		case terminalHandoffEvent(kind, event):
			closeEpoch(false)
		}
	}
	if open && now.Sub(epochStart) >= HandoffAcceptedLease {
		closeEpoch(true)
	}
	return HandoffOfferState{
		FruitlessEpochs: fruitless,
		Quiesced:        fruitless >= MaxAutomaticHandoffEpochs,
	}
}

// OpenHandoffJob binds one open institutional handoff action to its awaiting
// job row. The bridge uses this joined view to drain arbitrarily large
// handoff backlogs without one Get query per action.
type OpenHandoffJob struct {
	Row    Row
	Action HumanAction
}

// ListOpenHandoffJobs returns all open institutional handoffs and their
// awaiting-human jobs in one joined query. Identifiers are selected through
// indexed scalar lookups so the returned rows remain complete without a
// follow-up query per job.
func (js *Store) ListOpenHandoffJobs(ctx context.Context) ([]OpenHandoffJob, error) {
	rows, _, err := js.listOpenHandoffJobs(ctx, 0, false)
	return rows, err
}

// ListOpenHandoffJobsPage returns one bounded oldest-first page of open
// institutional handoffs and whether another page exists behind it.
func (js *Store) ListOpenHandoffJobsPage(ctx context.Context, limit int) ([]OpenHandoffJob, bool, error) {
	return js.listOpenHandoffJobs(ctx, EffectiveListLimit(limit), true)
}

func (js *Store) listOpenHandoffJobs(ctx context.Context, limit int, probe bool) ([]OpenHandoffJob, bool, error) {
	fetch := limit
	if probe && limit > 0 {
		fetch++
	}
	query := `
		SELECT j.id, j.work_request_id, j.state, j.policy_json,
		       COALESCE(j.artifact_sha256,''), j.selected_candidate_id,
		       j.spent_usd, COALESCE(j.terminal_reason,''), COALESCE(j.retry_at,''),
		       j.created_at, j.updated_at, COALESCE(j.lease_owner,''), COALESCE(j.lease_expires_at,''),
		       COALESCE(w.title,''), COALESCE(w.authors_json,'[]'), COALESCE(w.year,0), COALESCE(w.zotio_item_key,''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'doi'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'pmid'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'arxiv'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'isbn'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'openalex'),''),
		       ha.id, ha.job_id, ha.kind, ha.status, COALESCE(ha.detail,''), ha.requires_auth,
		       COALESCE(ha.blocked_by,''), ha.created_at, COALESCE(ha.candidate_id,0),
		       COALESCE(ha.quarantine_path,''), COALESCE(ha.quarantine_sha256,''), ha.revision
		FROM human_actions ha
		JOIN jobs j ON j.id = ha.job_id
		JOIN work_requests w ON w.id = j.work_request_id
		WHERE ha.status = 'open' AND ha.kind = 'openurl_handoff' AND j.state = 'awaiting_human'
		ORDER BY j.created_at ASC, j.id ASC, ha.id ASC`
	var args []any
	if fetch > 0 {
		query += ` LIMIT ?`
		args = append(args, fetch)
	}
	result, err := js.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = result.Close() }()
	var out []OpenHandoffJob
	for result.Next() {
		var item OpenHandoffJob
		var policyJSON, authorsJSON string
		var artifact sql.NullString
		var selectedID sql.NullInt64
		var doi, pmid, arxiv, isbn, openalex string
		if err := result.Scan(
			&item.Row.ID, &item.Row.WorkRequestID, &item.Row.State, &policyJSON,
			&artifact, &selectedID, &item.Row.SpentUSD, &item.Row.TerminalReason, &item.Row.RetryAt,
			&item.Row.CreatedAt, &item.Row.UpdatedAt, &item.Row.LeaseOwner, &item.Row.LeaseExpiresAt,
			&item.Row.Work.Title, &authorsJSON, &item.Row.Work.Year, &item.Row.ZotioItemKey,
			&doi, &pmid, &arxiv, &isbn, &openalex,
			&item.Action.ID, &item.Action.JobID, &item.Action.Kind, &item.Action.Status,
			&item.Action.Detail, &item.Action.RequiresAuth, &item.Action.BlockedBy,
			&item.Action.CreatedAt, &item.Action.CandidateID, &item.Action.QuarantinePath,
			&item.Action.QuarantineSHA256, &item.Action.Revision,
		); err != nil {
			_ = result.Close()
			return nil, false, err
		}
		item.Row.ArtifactSHA256 = artifact.String
		item.Row.SelectedCandidateID = selectedID.Int64
		item.Row.Work.DOI, item.Row.Work.PMID, item.Row.Work.ArXiv = doi, pmid, arxiv
		item.Row.Work.ISBN, item.Row.Work.OpenAlex = isbn, openalex
		if err := json.Unmarshal([]byte(policyJSON), &item.Row.Policy); err != nil {
			_ = result.Close()
			return nil, false, fmt.Errorf("job %s policy: %w", item.Row.ID, err)
		}
		if err := json.Unmarshal([]byte(authorsJSON), &item.Row.Work.Authors); err != nil {
			_ = result.Close()
			return nil, false, fmt.Errorf("job %s authors: %w", item.Row.ID, err)
		}
		if item.Row.TerminalReason != "" {
			item.Row.TerminalReason = string(NormalizeTerminalReason(item.Row.TerminalReason))
		}
		out = append(out, item)
	}
	if err := result.Close(); err != nil {
		return nil, false, err
	}
	if err := result.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if limit > 0 && len(out) > limit {
		out, more = out[:limit], true
	}
	return out, more, nil
}

// ListHumanActions returns actions, optionally only open ones. Unbounded: the
// inbox and maintenance callers need the whole set.
func (js *Store) ListHumanActions(ctx context.Context, openOnly bool) ([]HumanAction, error) {
	rows, _, err := js.listHumanActions(ctx, openOnly, nil, "", 0)
	return bareActions(rows), err
}

// ListHumanActionsPage returns one bounded page of actions and whether more
// exist behind it, proven the same way as ListPage.
func (js *Store) ListHumanActionsPage(ctx context.Context, openOnly bool, limit int) ([]HumanAction, bool, error) {
	rows, truncated, err := js.listHumanActions(ctx, openOnly, nil, "", EffectiveListLimit(limit))
	return bareActions(rows), truncated, err
}

// ListHumanActionsPageFor returns one bounded page of actions carrying each
// row's consumer attribution, optionally narrowed to a single consumer. An
// empty consumer is unfiltered.
func (js *Store) ListHumanActionsPageFor(ctx context.Context, openOnly bool, consumer string, limit int) ([]AttributedAction, bool, error) {
	return js.listHumanActions(ctx, openOnly, nil, consumer, EffectiveListLimit(limit))
}

// ListHumanActionsForJob returns every action recorded for one job, open or
// resolved, with its attribution. Unbounded like ListHumanActions: a single
// job's action history is small, and a bound here could hide the open handoff a
// caller is asking about.
func (js *Store) ListHumanActionsForJob(ctx context.Context, jobID string) ([]AttributedAction, error) {
	if jobID == "" {
		return nil, nil
	}
	rows, _, err := js.listHumanActions(ctx, false, []string{jobID}, "", 0)
	return rows, err
}

// ListOpenHumanActionsForJobs returns open actions for the supplied bounded job
// page. Maintenance callers use it rather than materializing every historic
// open action, including terminal advisory rows.
func (js *Store) ListOpenHumanActionsForJobs(ctx context.Context, jobIDs []string) ([]HumanAction, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	rows, _, err := js.listHumanActions(ctx, true, jobIDs, "", 0)
	return bareActions(rows), err
}

// AttributedAction pairs one human action with the consumer that submitted its
// job. Consumer is empty when the submission recorded none, which is the honest
// answer for every request queued before attribution existed: it is not
// backfilled and never defaulted.
//
// It exists because Consumer must not become a HumanAction field. HumanAction is
// the row body of actions.list, decoded with DisallowUnknownFields, so widening
// it would make every older papio reject a newer daemon's listing.
type AttributedAction struct {
	Action   HumanAction
	Consumer string
}

func bareActions(rows []AttributedAction) []HumanAction {
	if len(rows) == 0 {
		return nil
	}
	out := make([]HumanAction, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Action)
	}
	return out
}

// listHumanActions returns every matching action when limit is 0, otherwise one
// page plus a proof of whether more exist: it fetches limit+1 rows and reports
// whether the extra one was there.
//
// The jobs reach is a LEFT JOIN on purpose: an action whose parent job row has
// gone must still be listed and closable, so a missing attribution can never make
// an open action disappear from the inbox.
func (js *Store) listHumanActions(ctx context.Context, openOnly bool, jobIDs []string, consumer string, limit int) ([]AttributedAction, bool, error) {
	q := `SELECT a.id, a.job_id, a.kind, a.status, COALESCE(a.detail,''), a.requires_auth, a.blocked_by, a.created_at,
		COALESCE(a.candidate_id, 0), a.quarantine_path, a.quarantine_sha256, a.revision, j.consumer
		FROM human_actions a
		LEFT JOIN jobs j ON j.id = a.job_id`
	var where []string
	var args []any
	if openOnly {
		where = append(where, `a.status = 'open'`)
	}
	if jobIDs != nil {
		where = append(where, `a.job_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",")+`)`)
		for _, id := range jobIDs {
			args = append(args, id)
		}
	}
	if consumer != "" {
		where = append(where, `j.consumer = ?`)
		args = append(args, consumer)
	}
	if len(where) != 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY a.id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := js.S.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var out []AttributedAction
	for rows.Next() {
		var h HumanAction
		var consumer sql.NullString
		if err := rows.Scan(
			&h.ID, &h.JobID, &h.Kind, &h.Status, &h.Detail, &h.RequiresAuth, &h.BlockedBy, &h.CreatedAt,
			&h.CandidateID, &h.QuarantinePath, &h.QuarantineSHA256, &h.Revision, &consumer,
		); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		out = append(out, AttributedAction{Action: h, Consumer: consumer.String})
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if limit > 0 && len(out) > limit {
		out, truncated = out[:limit], true
	}
	return out, truncated, nil
}

// RecordEvent appends a durable event to a job's ordered event stream.
func (js *Store) RecordEvent(ctx context.Context, jobID, kind string, detail map[string]any) error {
	if jobID == "" || kind == "" {
		return errors.New("job event requires job ID and kind")
	}
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshaling job event: %w", err)
	}
	_, err = js.S.DB().ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, ?, ?)`,
		jobID, store.Now(), kind, string(encoded))
	if err != nil {
		return fmt.Errorf("recording job event: %w", err)
	}
	return nil
}

// Events returns a job's event stream in order.
func (js *Store) Events(ctx context.Context, jobID string) ([]map[string]any, error) {
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT seq, at, kind, detail_json FROM events WHERE job_id = ? ORDER BY seq ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var seq int64
		var at, kind, detail string
		if err := rows.Scan(&seq, &at, &kind, &detail); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(detail), &d)
		out = append(out, map[string]any{"seq": seq, "at": at, "kind": kind, "detail": d})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}
