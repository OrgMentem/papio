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
	"time"

	"papio/internal/store"
	"papio/internal/work"
)

// Job states (stack plan "Job states").
const (
	StateQueued       = "queued"
	StateResolving    = "resolving"
	StateFetching     = "fetching"
	StateValidating   = "validating"
	StateReady        = "ready"
	StateAwaitingHuman = "awaiting_human"
	StateRetryWait    = "retry_wait"
	StateNeedsReview  = "needs_review"
	StateUnavailable  = "unavailable"
	StateFailed       = "failed"
	StateCancelled    = "cancelled"
)

// Terminal reports whether a state ends the acquisition attempt. ready is the
// acquisition terminal; exports are separate idempotent records.
func Terminal(state string) bool {
	switch state {
	case StateReady, StateUnavailable, StateFailed, StateCancelled:
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
		StateFetching: true, StateAwaitingHuman: true, StateRetryWait: true,
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
	},
	StateAwaitingHuman: {StateResolving: true, StateFetching: true, StateCancelled: true, StateFailed: true},
	StateRetryWait:     {StateResolving: true, StateFetching: true, StateCancelled: true, StateFailed: true},
	StateNeedsReview:   {StateResolving: true, StateFetching: true, StateCancelled: true},
}

// ErrConflict is returned when a CAS transition loses (state changed or the
// transition is not allowed).
var ErrConflict = errors.New("job state conflict")

// Policy is the per-job policy snapshot stored in jobs.policy_json.
type Policy struct {
	AccessMode     string   `json:"access_mode"`
	DesiredVersion string   `json:"desired_version"`
	MaxCostUSD     *float64 `json:"max_cost_usd,omitempty"`
	SourcesAllow   []string `json:"sources_allow,omitempty"`
	SourcesDeny    []string `json:"sources_deny,omitempty"`
	FetchMaxBytes  int64    `json:"fetch_max_bytes"`
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
	ID             string
	WorkRequestID  string
	State          string
	Policy         Policy
	ArtifactSHA256 string
	TerminalReason string
	RetryAt        string
	CreatedAt      string
	UpdatedAt      string
	Work           work.Work
	ZotioItemKey   string
}

// Candidate is one ranked acquisition option. URL is never stored; only the
// redacted form persists, so a crash discards bearer URLs by construction.
type Candidate struct {
	ID                 int64
	JobID              string
	Source             string
	URLRedacted        string
	URLKey             string
	LandingRedacted    string
	Version            string
	AccessBasis        string
	ReuseLicense       string
	ExpectedMIME       string
	CostUSD            float64
	Direct             bool
	IdentityConfidence float64
	RankEvidence       string
	Rank               int
	Status             string
}

// Store layers job semantics over the shared SQLite store.
type Store struct{ S *store.Store }

// NewID returns a 26-hex-char random identifier with a type prefix.
func NewID(prefix string) string {
	var b [13]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// CreateRequest inserts a work request, its identifiers, and a queued job in
// one transaction. Resubmitting the same requestID returns the existing live
// job (idempotent submission).
func (js *Store) CreateRequest(ctx context.Context, requestID string, w work.Work, zotioKey, collection string, pol Policy, rawIDs map[string]string) (string, error) {
	if requestID == "" {
		requestID = NewID("wr")
	}
	db := js.S.DB()

	// Idempotent resubmission: return the live job for this request if any.
	var existing string
	err := db.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE work_request_id = ? AND state NOT IN ('failed','cancelled','unavailable') ORDER BY created_at DESC LIMIT 1`,
		requestID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	polJSON, err := json.Marshal(pol)
	if err != nil {
		return "", err
	}
	authorsJSON, _ := json.Marshal(w.Authors)
	now := store.Now()
	jobID := NewID("job")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO work_requests (id, created_at, requester, zotio_item_key, collection_key, title, authors_json, year, desired_version, max_cost_usd)
		 VALUES (?, ?, 'cli', ?, ?, ?, ?, ?, ?, ?)`,
		requestID, now, nullable(zotioKey), nullable(collection), nullable(w.Title), string(authorsJSON), nullableInt(w.Year), pol.DesiredVersion, pol.MaxCostUSD); err != nil {
		return "", fmt.Errorf("inserting work request: %w", err)
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
			return "", fmt.Errorf("inserting identifier %s: %w", kind, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?, ?)`,
		jobID, requestID, string(polJSON), now, now); err != nil {
		return "", fmt.Errorf("inserting job: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.created', ?)`,
		jobID, now, fmt.Sprintf(`{"request_id":%q,"work":%q}`, requestID, w.Describe())); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return jobID, nil
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
// terminalReason applies to terminal states.
func (js *Store) Transition(ctx context.Context, jobID, from, to string, detail map[string]any, opts ...TransitionOpt) error {
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
	defer tx.Rollback()

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
func WithTerminalReason(reason string) TransitionOpt {
	return func(c *transitionCfg) { c.terminalReason = reason }
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
// due. Stages mid-flight (resolving/fetching/validating) are only claimable
// when their lease has expired (crash recovery path).
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
		     OR (state IN ('resolving','fetching','validating') AND lease_expires_at IS NOT NULL AND lease_expires_at < ?)
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
	type stale struct{ id, state string }
	var found []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.state); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var recovered []string
	for _, s := range found {
		if s.state == StateResolving {
			// Already at the durable boundary; just clear the lease.
			if err := js.Release(ctx, s.id, ""); err == nil {
				if _, err := js.S.DB().ExecContext(ctx,
					`UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL WHERE id = ?`, s.id); err != nil {
					return recovered, err
				}
			}
			recovered = append(recovered, s.id)
			continue
		}
		if err := js.Transition(ctx, s.id, s.state, StateResolving, map[string]any{"reason": "crash_recovery"}); err != nil && !errors.Is(err, ErrConflict) {
			return recovered, err
		}
		recovered = append(recovered, s.id)
	}
	return recovered, nil
}

// Get loads one job row with its work-request identity.
func (js *Store) Get(ctx context.Context, jobID string) (*Row, error) {
	db := js.S.DB()
	var r Row
	var polJSON string
	var artifact, terminal, retryAt sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT j.id, j.work_request_id, j.state, j.policy_json, j.artifact_sha256, j.terminal_reason, j.retry_at, j.created_at, j.updated_at,
		       COALESCE(w.title,''), COALESCE(w.authors_json,'[]'), COALESCE(w.year,0), COALESCE(w.zotio_item_key,'')
		FROM jobs j JOIN work_requests w ON w.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(
		&r.ID, &r.WorkRequestID, &r.State, &polJSON, &artifact, &terminal, &retryAt, &r.CreatedAt, &r.UpdatedAt,
		&r.Work.Title, &jsonScanner{&r.Work.Authors}, &r.Work.Year, &r.ZotioItemKey)
	if err != nil {
		return nil, err
	}
	r.ArtifactSHA256, r.TerminalReason, r.RetryAt = artifact.String, terminal.String, retryAt.String
	if err := json.Unmarshal([]byte(polJSON), &r.Policy); err != nil {
		return nil, fmt.Errorf("job %s policy: %w", jobID, err)
	}
	ids, err := db.QueryContext(ctx, `SELECT kind, value FROM identifiers WHERE work_request_id = ?`, r.WorkRequestID)
	if err != nil {
		return nil, err
	}
	defer ids.Close()
	for ids.Next() {
		var kind, value string
		if err := ids.Scan(&kind, &value); err != nil {
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
	return &r, ids.Err()
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

// List returns jobs, optionally filtered by state, newest first.
func (js *Store) List(ctx context.Context, state string, limit int) ([]Row, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id FROM jobs`
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := js.S.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		idList = append(idList, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range idList {
		r, err := js.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// InsertCandidates stores ranked candidates (redacted URLs only), deduplicated
// per job by url_key. Returns the number inserted.
func (js *Store) InsertCandidates(ctx context.Context, jobID string, cands []Candidate) (int, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := store.Now()
	inserted := 0
	for _, c := range cands {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO candidates
			  (job_id, source, url_redacted, url_key, landing_redacted, version, access_basis, reuse_license,
			   expected_mime, cost_usd, direct, identity_confidence, rank_evidence, rank, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
			jobID, c.Source, c.URLRedacted, c.URLKey, nullable(c.LandingRedacted), c.Version, c.AccessBasis,
			c.ReuseLicense, nullable(c.ExpectedMIME), c.CostUSD, boolInt(c.Direct), c.IdentityConfidence,
			nullable(c.RankEvidence), c.Rank, now)
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
		SELECT id, job_id, source, url_redacted, url_key, COALESCE(landing_redacted,''), version, access_basis,
		       reuse_license, COALESCE(expected_mime,''), cost_usd, direct, identity_confidence,
		       COALESCE(rank_evidence,''), COALESCE(rank,0), status
		FROM candidates WHERE job_id = ? AND status = 'pending' ORDER BY rank ASC, id ASC LIMIT 1`, jobID)
	var c Candidate
	var direct int
	err := row.Scan(&c.ID, &c.JobID, &c.Source, &c.URLRedacted, &c.URLKey, &c.LandingRedacted, &c.Version,
		&c.AccessBasis, &c.ReuseLicense, &c.ExpectedMIME, &c.CostUSD, &direct, &c.IdentityConfidence,
		&c.RankEvidence, &c.Rank, &c.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Direct = direct == 1
	return &c, nil
}

// MarkCandidate updates one candidate's status.
func (js *Store) MarkCandidate(ctx context.Context, id int64, status string) error {
	_, err := js.S.DB().ExecContext(ctx, `UPDATE candidates SET status = ? WHERE id = ?`, status, id)
	return err
}

// ResetCandidates returns all candidates of a job to pending (used when a
// recovered job re-enters fetching with a fresh resolution pass).
func (js *Store) ResetCandidates(ctx context.Context, jobID string) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE candidates SET status = 'pending' WHERE job_id = ? AND status IN ('fetching')`, jobID)
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
	SHA256           string
	SizeBytes        int64
	MIME             string
	PageCount        int
	TextChars        int64
	OCRUsed          bool
	Encrypted        bool
	HasActiveContent bool
	IdentityResult   string
	Path             string
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

// GetArtifact loads one artifact row by hash.
func (js *Store) GetArtifact(ctx context.Context, sha string) (*Artifact, error) {
	var a Artifact
	var ocr, enc, active int
	var identity sql.NullString
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT sha256, size_bytes, mime, COALESCE(page_count,0), COALESCE(text_chars,0), ocr_used, encrypted, has_active_content, identity_result, path
		FROM artifacts WHERE sha256 = ?`, sha).Scan(
		&a.SHA256, &a.SizeBytes, &a.MIME, &a.PageCount, &a.TextChars, &ocr, &enc, &active, &identity, &a.Path)
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

// FindArtifactByDOI returns a prior validated artifact for the same DOI, if
// any job with that DOI reached ready (resolver order step 1: local cache).
func (js *Store) FindArtifactByDOI(ctx context.Context, doi string) (*Artifact, error) {
	var sha string
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT j.artifact_sha256 FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = 'doi' AND i.value = ? AND j.state = 'ready' AND j.artifact_sha256 IS NOT NULL
		ORDER BY j.updated_at DESC LIMIT 1`, doi).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return js.GetArtifact(ctx, sha)
}

// OpenHumanAction records a required human step for a job.
func (js *Store) OpenHumanAction(ctx context.Context, jobID, kind, detail string) (int64, error) {
	res, err := js.S.DB().ExecContext(ctx,
		`INSERT INTO human_actions (job_id, kind, status, detail, created_at) VALUES (?, ?, 'open', ?, ?)`,
		jobID, kind, nullable(detail), store.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// HumanAction is one pending or resolved human step.
type HumanAction struct {
	ID        int64
	JobID     string
	Kind      string
	Status    string
	Detail    string
	CreatedAt string
}

// ListHumanActions returns actions, optionally only open ones.
func (js *Store) ListHumanActions(ctx context.Context, openOnly bool) ([]HumanAction, error) {
	q := `SELECT id, job_id, kind, status, COALESCE(detail,''), created_at FROM human_actions`
	if openOnly {
		q += ` WHERE status = 'open'`
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := js.S.DB().QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HumanAction
	for rows.Next() {
		var h HumanAction
		if err := rows.Scan(&h.ID, &h.JobID, &h.Kind, &h.Status, &h.Detail, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Events returns a job's event stream in order.
func (js *Store) Events(ctx context.Context, jobID string) ([]map[string]any, error) {
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT seq, at, kind, detail_json FROM events WHERE job_id = ? ORDER BY seq ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var seq int64
		var at, kind, detail string
		if err := rows.Scan(&seq, &at, &kind, &detail); err != nil {
			return nil, err
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(detail), &d)
		out = append(out, map[string]any{"seq": seq, "at": at, "kind": kind, "detail": d})
	}
	return out, rows.Err()
}
