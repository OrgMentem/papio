// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
	"papio/internal/store"
)

// Source identifies the bounded origin of a cohort. Browser sources retain
// only their bare origin; CLI sources use a human-readable, non-URL label.
type Source struct {
	Kind, Label, Detector, ScanID string
}

type ChunkRequest struct {
	RequestID, CohortID     string
	Source                  Source
	CohortTotal, ChunkIndex int
	FinalChunk              bool
	CanonicalKeys           []string
}

type MemberOutcome struct {
	CanonicalKey, JobID, Outcome string
}

type ChunkResult struct {
	BatchID, Membership                                        string
	CohortTotal                                                *int
	PersistedMembers, Submitted, Joined, AlreadyOwned, Invalid int
}

type ConflictError struct{ Reason string }

func (e *ConflictError) Error() string {
	if e == nil || e.Reason == "" {
		return "acquisition cohort conflict"
	}
	return "acquisition cohort conflict: " + e.Reason
}

type Cohorts struct {
	S   *store.Store
	Now func() time.Time
	mu  sync.Mutex
}

func New(s *store.Store) *Cohorts {
	return &Cohorts{S: s, Now: func() time.Time { return time.Now().UTC() }}
}

const maxCohortTotal = 200
const chunkSize = 50

var (
	cohortIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	dnsHostRE   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

func conflictf(format string, args ...any) error {
	return &ConflictError{Reason: fmt.Sprintf(format, args...)}
}

func validateSource(s Source) error {
	if s.Kind == "" || len(s.Kind) > 64 || strings.ContainsAny(s.Kind, "\x00\r\n") {
		return errors.New("source kind is invalid")
	}
	if len([]rune(s.Label)) == 0 || len([]rune(s.Label)) > 300 || strings.ContainsAny(s.Label, "\x00\r\n") {
		return errors.New("source label is invalid")
	}
	if s.Kind == "browser_page" {
		if !validBareOrigin(s.Label) {
			return errors.New("browser source label must be a bare lowercase https origin")
		}
		if s.Detector == "" || len([]rune(s.Detector)) > 128 || strings.ContainsAny(s.Detector, "\x00\r\n") {
			return errors.New("browser source detector is invalid")
		}
	} else if strings.ContainsAny(s.Label, "/?#@\\") {
		return errors.New("non-browser source label must not contain URL data")
	}
	if len([]rune(s.Detector)) > 128 || strings.ContainsAny(s.Detector, "\x00\r\n") {
		return errors.New("source detector is invalid")
	}
	if len([]rune(s.ScanID)) > 64 || strings.ContainsAny(s.ScanID, "\x00\r\n") {
		return errors.New("source scan id is invalid")
	}
	return nil
}

func validBareOrigin(raw string) bool {
	if !strings.HasPrefix(raw, "https://") || strings.ContainsAny(raw, "?#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return false
	}
	host := u.Hostname()
	if !dnsHostRE.MatchString(host) || host != strings.ToLower(host) {
		return false
	}
	if port := strings.TrimPrefix(u.Host, host); port != "" {
		if !strings.HasPrefix(port, ":") || len(port[1:]) < 1 || len(port[1:]) > 5 {
			return false
		}
		for _, r := range port[1:] {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func validateChunkRequest(r ChunkRequest) error {
	if !requestIDRE.MatchString(r.RequestID) || !cohortIDRE.MatchString(r.CohortID) {
		return errors.New("request and cohort ids are invalid")
	}
	if err := validateSource(r.Source); err != nil {
		return err
	}
	if r.CohortTotal < 1 || r.CohortTotal > maxCohortTotal {
		return errors.New("cohort total is out of range")
	}
	chunks := (r.CohortTotal + chunkSize - 1) / chunkSize
	if r.ChunkIndex < 0 || r.ChunkIndex >= chunks {
		return errors.New("chunk index is out of range")
	}
	want := chunkSize
	if r.ChunkIndex == chunks-1 {
		want = r.CohortTotal - r.ChunkIndex*chunkSize
	}
	if len(r.CanonicalKeys) != want {
		return fmt.Errorf("chunk has %d keys, want %d", len(r.CanonicalKeys), want)
	}
	if r.FinalChunk != (r.ChunkIndex == chunks-1) {
		return errors.New("final chunk flag does not match manifest")
	}
	seen := make(map[string]struct{}, len(r.CanonicalKeys))
	for _, k := range r.CanonicalKeys {
		if k == "" || len([]rune(k)) > 300 || strings.ContainsRune(k, '\x00') {
			return errors.New("canonical key is invalid")
		}
		if _, ok := seen[k]; ok {
			return errors.New("chunk contains duplicate canonical key")
		}
		seen[k] = struct{}{}
	}
	return nil
}

func newBatchID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ab_" + hex.EncodeToString(b[:]), nil
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func sameSource(a, b Source) bool {
	return a.Kind == b.Kind && a.Label == b.Label && a.Detector == b.Detector && a.ScanID == b.ScanID
}

type storedChunk struct {
	CanonicalKeys []string `json:"canonical_keys"`
}

func (c *Cohorts) SubmitChunk(ctx context.Context, req ChunkRequest, submit func(context.Context, []string) ([]MemberOutcome, error)) (ChunkResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.S == nil || c.S.DB() == nil {
		return ChunkResult{}, errors.New("cohort store is unavailable")
	}
	if req.ChunkIndex < 0 || req.ChunkIndex >= (req.CohortTotal+chunkSize-1)/chunkSize {
		return ChunkResult{}, conflictf("chunk index %d is beyond the cohort manifest", req.ChunkIndex)
	}
	if req.FinalChunk != (req.ChunkIndex == (req.CohortTotal+chunkSize-1)/chunkSize-1) {
		return ChunkResult{}, conflictf("final chunk flag does not match the manifest")
	}
	if err := validateChunkRequest(req); err != nil {
		return ChunkResult{}, err
	}
	now := c.Now().UTC()
	db := c.S.DB()
	var oldBatch, oldCohort, oldRequest string
	var oldIndex int
	err := db.QueryRowContext(ctx, `SELECT b.id,b.cohort_id,c.chunk_index,c.request_id FROM acquisition_batch_chunks c JOIN acquisition_batches b ON b.id=c.batch_id WHERE c.request_id=?`, req.RequestID).Scan(&oldBatch, &oldCohort, &oldIndex, &oldRequest)
	if err == nil {
		if oldCohort != req.CohortID || oldIndex != req.ChunkIndex {
			return ChunkResult{}, conflictf("request id already belongs to cohort %q chunk %d", oldCohort, oldIndex)
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ChunkResult{}, err
	}

	var batchID, kind, label, detector, scanID string
	var expected int
	var membership string
	err = db.QueryRowContext(ctx, `SELECT id,source_kind,source_label,COALESCE(source_detector,''),COALESCE(source_scan_id,''),expected_total,membership_state FROM acquisition_batches WHERE cohort_id=?`, req.CohortID).Scan(&batchID, &kind, &label, &detector, &scanID, &expected, &membership)
	if errors.Is(err, sql.ErrNoRows) {
		batchID, err = newBatchID()
		if err != nil {
			return ChunkResult{}, err
		}
		membership = "open"
	} else if err != nil {
		return ChunkResult{}, err
	}
	if batchID != "" && expected != 0 {
		if expected != req.CohortTotal || !sameSource(Source{Kind: kind, Label: label, Detector: detector, ScanID: scanID}, req.Source) {
			return ChunkResult{}, conflictf("cohort manifest identity differs")
		}
	}

	// An existing chunk is a replay only if every semantic field matches. The
	// result is returned from its cache and submit is never called.
	var chunkJSON, resultJSON, storedRequest string
	var storedFinal int
	err = db.QueryRowContext(ctx, `SELECT c.request_id,c.canonical_keys_json,c.result_json,c.final_chunk FROM acquisition_batch_chunks c WHERE c.batch_id=? AND c.chunk_index=?`, batchID, req.ChunkIndex).Scan(&storedRequest, &chunkJSON, &resultJSON, &storedFinal)
	if err == nil {
		var stored storedChunk
		if storedRequest != req.RequestID || storedFinal != boolInt(req.FinalChunk) || json.Unmarshal([]byte(chunkJSON), &stored) != nil || !sameStrings(stored.CanonicalKeys, req.CanonicalKeys) {
			return ChunkResult{}, conflictf("chunk manifest identity differs")
		}
		var out ChunkResult
		if json.Unmarshal([]byte(resultJSON), &out) != nil {
			return ChunkResult{}, errors.New("stored cohort result is corrupt")
		}
		return out, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ChunkResult{}, err
	}
	if membership == "partial" && expected != 0 {
		var updated string
		if err := db.QueryRowContext(ctx, `SELECT updated_at FROM acquisition_batches WHERE id=?`, batchID).Scan(&updated); err != nil {
			return ChunkResult{}, err
		}
		if now.Sub(parseTime(updated)) > 24*time.Hour {
			return ChunkResult{}, conflictf("cohort replay window expired")
		}
	}

	var next int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM acquisition_batch_chunks WHERE batch_id=?`, batchID).Scan(&next); err != nil {
		return ChunkResult{}, err
	}
	if req.ChunkIndex != next {
		return ChunkResult{}, conflictf("chunk index %d is not next index %d", req.ChunkIndex, next)
	}
	var duplicate string
	err = db.QueryRowContext(ctx, `SELECT canonical_key FROM acquisition_batch_members WHERE batch_id=? AND canonical_key IN (`+placeholders(len(req.CanonicalKeys))+`) LIMIT 1`, append([]any{batchID}, stringsToAny(req.CanonicalKeys)...)...).Scan(&duplicate)
	if err == nil {
		return ChunkResult{}, conflictf("canonical key %q already belongs to cohort", duplicate)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ChunkResult{}, err
	}
	outcomes, err := submit(ctx, append([]string(nil), req.CanonicalKeys...))
	if err != nil {
		return ChunkResult{}, err
	}
	ordered, counts, err := validateOutcomes(req.CanonicalKeys, outcomes)
	if err != nil {
		return ChunkResult{}, err
	}
	res := ChunkResult{BatchID: batchID, Membership: "open", PersistedMembers: next*chunkSize + len(req.CanonicalKeys), Submitted: counts["submitted"], Joined: counts["joined"], AlreadyOwned: counts["already_owned"], Invalid: counts["invalid"]}
	if membership == "partial" {
		// A failed membership write leaves the domain submission durable but no
		// trustworthy denominator. The page-bulk submitter is idempotent on
		// canonical work identity, so replay may legitimately call submit again
		// and receive joined/already_owned instead of submitted.
		res.Membership = "partial"
	}
	if membership != "partial" && req.FinalChunk {
		res.Membership = "complete"
		n := req.CohortTotal
		res.CohortTotal = &n
	}
	resultBytes, _ := json.Marshal(res)
	keysBytes, _ := json.Marshal(storedChunk{CanonicalKeys: req.CanonicalKeys})
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ChunkResult{}, err
	}
	created := timestamp(now)
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_batches(id,cohort_id,source_kind,source_label,source_detector,source_scan_id,expected_total,created_at,updated_at,membership_state) VALUES(?,?,?,?,?,?,?,?,?,?)`, batchID, req.CohortID, req.Source.Kind, req.Source.Label, nullable(req.Source.Detector), nullable(req.Source.ScanID), req.CohortTotal, created, created, res.Membership)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE acquisition_batches SET updated_at=?,membership_state=? WHERE id=?`, created, res.Membership, batchID)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_batch_chunks(batch_id,chunk_index,request_id,final_chunk,canonical_keys_json,result_json,created_at) VALUES(?,?,?,?,?,?,?)`, batchID, req.ChunkIndex, req.RequestID, boolInt(req.FinalChunk), keysBytes, resultBytes, created)
	}
	if err == nil {
		for i, m := range ordered {
			_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_batch_members(batch_id,ordinal,canonical_key,job_id,submission_outcome,created_at) VALUES(?,?,?,?,?,?)`, batchID, req.ChunkIndex*chunkSize+i, m.CanonicalKey, nullable(m.JobID), m.Outcome, created)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		_ = tx.Rollback()
		// Submission has already crossed the domain boundary. Preserve that fact
		// without inventing a denominator from telemetry.
		if batchID != "" {
			_, _ = db.ExecContext(ctx, `UPDATE acquisition_batches SET membership_state='partial' WHERE id=?`, batchID)
		}
		return ChunkResult{}, fmt.Errorf("persist cohort membership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_, _ = db.ExecContext(ctx, `UPDATE acquisition_batches SET membership_state='partial' WHERE id=?`, batchID)
		return ChunkResult{}, err
	}
	return res, nil
}

func validateOutcomes(keys []string, outcomes []MemberOutcome) ([]MemberOutcome, map[string]int, error) {
	if len(outcomes) != len(keys) {
		return nil, nil, errors.New("submit returned one outcome per canonical key")
	}
	byKey := make(map[string]MemberOutcome, len(outcomes))
	counts := map[string]int{}
	for _, o := range outcomes {
		if o.CanonicalKey == "" || byKey[o.CanonicalKey].CanonicalKey != "" {
			return nil, nil, errors.New("submit returned duplicate or empty canonical key")
		}
		switch o.Outcome {
		case "submitted", "joined", "already_owned", "invalid":
		default:
			return nil, nil, fmt.Errorf("unknown submission outcome %q", o.Outcome)
		}
		if (o.Outcome == "submitted" || o.Outcome == "joined") && o.JobID == "" {
			return nil, nil, errors.New("submitted and joined outcomes require a job id")
		}
		byKey[o.CanonicalKey] = o
		counts[o.Outcome]++
	}
	ordered := make([]MemberOutcome, len(keys))
	for i, key := range keys {
		o, ok := byKey[key]
		if !ok {
			return nil, nil, fmt.Errorf("submit omitted canonical key %q", key)
		}
		ordered[i] = o
	}
	if counts["submitted"]+counts["joined"]+counts["already_owned"]+counts["invalid"] != len(keys) {
		return nil, nil, errors.New("submission outcome counts do not sum to chunk")
	}
	return ordered, counts, nil
}

func (c *Cohorts) Latest(ctx context.Context, now time.Time) (*Projection, error) {
	var id string
	err := c.S.DB().QueryRowContext(ctx, `SELECT id FROM acquisition_batches ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c.Projection(ctx, id, now)
}

func (c *Cohorts) Projection(ctx context.Context, batchID string, now time.Time) (*Projection, error) {
	var p Projection
	var created, updated, closed string
	var expected int
	if err := c.S.DB().QueryRowContext(ctx, `SELECT source_label,created_at,updated_at,COALESCE(closed_at,''),expected_total,membership_state FROM acquisition_batches WHERE id=?`, batchID).Scan(&p.Label, &created, &updated, &closed, &expected, &p.Membership); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	p.BatchID, p.StartedAt = batchID, parseTime(created)
	if closed != "" {
		t := parseTime(closed)
		p.SettledAt = &t
	}
	if p.Membership == "open" && now.Sub(parseTime(updated)) >= 10*time.Minute {
		if _, err := c.S.DB().ExecContext(ctx, `UPDATE acquisition_batches SET membership_state='partial' WHERE id=? AND membership_state='open'`, batchID); err != nil {
			return nil, err
		}
		p.Membership = "partial"
	}
	if p.Membership != "complete" {
		p.ProjectionComplete = false
		return &p, nil
	}
	members, err := c.S.DB().QueryContext(ctx, `SELECT canonical_key,COALESCE(job_id,''),submission_outcome FROM acquisition_batch_members WHERE batch_id=? ORDER BY ordinal`, batchID)
	if err != nil {
		return nil, err
	}
	defer members.Close()
	type member struct{ key, job, outcome string }
	var rows []member
	for members.Next() {
		var m member
		if err := members.Scan(&m.key, &m.job, &m.outcome); err != nil {
			return nil, err
		}
		rows = append(rows, m)
	}
	if err := members.Err(); err != nil {
		return nil, err
	}
	if len(rows) != expected {
		p.ProjectionComplete = false
		return &p, nil
	}
	inFlight, scheduled, continuing, waiting, stalled, settled, unavailable := 0, 0, 0, 0, 0, 0, 0
	complete := true
	var terminalAt *time.Time
	for _, m := range rows {
		if m.outcome == "already_owned" || m.outcome == "invalid" {
			settled++
			if m.outcome == "invalid" {
				unavailable++
			}
			continue
		}
		var state, leaseOwner, leaseExpires, retryAt string
		if err := c.S.DB().QueryRowContext(ctx, `SELECT state,COALESCE(lease_owner,''),COALESCE(lease_expires_at,''),COALESCE(retry_at,'') FROM jobs WHERE id=?`, m.job).Scan(&state, &leaseOwner, &leaseExpires, &retryAt); err != nil {
			complete = false
			continue
		}
		if job.Terminal(state) {
			var raw string
			if err := c.S.DB().QueryRowContext(ctx, `SELECT MAX(at) FROM events WHERE job_id=? AND kind='job.transition' AND json_extract(detail_json,'$.to') IN ('ready','imported','unavailable','failed','cancelled')`, m.job).Scan(&raw); err != nil || raw == "" {
				complete = false
				continue
			}
			t, ok := parseTimeChecked(raw)
			if !ok {
				complete = false
				continue
			}
			if terminalAt == nil || t.After(*terminalAt) {
				terminalAt = &t
			}
			settled++
			if state == job.StateUnavailable || state == job.StateFailed || state == job.StateCancelled {
				unavailable++
			}
			continue
		}
		if leaseOwner != "" {
			t, ok := parseTimeChecked(leaseExpires)
			if !ok {
				complete = false
				continue
			}
			if !t.Before(now) {
				inFlight++
				continue
			}
		}
		if retryAt != "" {
			t, ok := parseTimeChecked(retryAt)
			if !ok {
				complete = false
				continue
			}
			if t.After(now) {
				scheduled++
				continue
			}
		}
		if state == job.StateAwaitingHuman || state == job.StateNeedsReview {
			var n int
			if err := c.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM human_actions WHERE job_id=? AND status='open'`, m.job).Scan(&n); err != nil {
				complete = false
				continue
			}
			if n > 0 {
				waiting++
				continue
			}
			complete = false
			continue
		}
		switch state {
		case job.StateQueued, job.StateResolving, job.StateFetching, job.StateValidating:
			continuing++
		default:
			complete = false
		}
	}
	if !complete {
		p.ProjectionComplete = false
		return &p, nil
	}
	p.ProjectionComplete = true
	p.Total = intPtr(expected)
	p.Settled = intPtr(settled)
	p.NonterminalTotal = intPtr(len(rows) - settled)
	p.InFlight = intPtr(inFlight)
	p.Scheduled = intPtr(scheduled)
	p.Continuing = intPtr(continuing)
	p.WaitingRequired = intPtr(waiting)
	p.Stalled = intPtr(stalled)
	p.Unavailable = intPtr(unavailable)
	if settled == len(rows) && terminalAt != nil {
		p.SettledAt = terminalAt
		_, _ = c.S.DB().ExecContext(ctx, `UPDATE acquisition_batches SET closed_at=COALESCE(closed_at,?),updated_at=? WHERE id=?`, timestamp(*terminalAt), timestamp(*terminalAt), batchID)
	}
	return &p, nil
}

type Projection struct {
	BatchID, Label, Membership                                                                               string
	StartedAt                                                                                                time.Time
	SettledAt                                                                                                *time.Time
	ProjectionComplete                                                                                       bool
	Total, Settled, NonterminalTotal, InFlight, Scheduled, Continuing, WaitingRequired, Stalled, Unavailable *int
}

func (c *Cohorts) RecordCLIBatch(ctx context.Context, label string, outcomes []MemberOutcome) (ChunkResult, error) {
	if len(outcomes) == 0 || len(outcomes) > maxCohortTotal {
		return ChunkResult{}, errors.New("CLI cohort must contain 1..200 outcomes")
	}
	if strings.TrimSpace(label) == "" || len([]rune(label)) > 256 || strings.ContainsAny(label, "\x00\r\n/?#@\\") {
		return ChunkResult{}, errors.New("CLI cohort label is invalid")
	}
	cohortID, err := newBatchID()
	if err != nil {
		return ChunkResult{}, err
	}
	keys := make([]string, len(outcomes))
	for i, o := range outcomes {
		keys[i] = o.CanonicalKey
	}
	ordered, counts, err := validateOutcomes(keys, outcomes)
	if err != nil {
		return ChunkResult{}, err
	}
	batchID, err := newBatchID()
	if err != nil {
		return ChunkResult{}, err
	}
	now := timestamp(c.Now())
	expected := len(keys)
	result := ChunkResult{BatchID: batchID, Membership: "complete", CohortTotal: &expected, PersistedMembers: expected, Submitted: counts["submitted"], Joined: counts["joined"], AlreadyOwned: counts["already_owned"], Invalid: counts["invalid"]}
	resultJSON, _ := json.Marshal(result)
	keysJSON, _ := json.Marshal(storedChunk{CanonicalKeys: keys})
	tx, err := c.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ChunkResult{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_batches(id,cohort_id,source_kind,source_label,expected_total,created_at,updated_at,membership_state) VALUES(?,?,?,?,?,?,?,?)`, batchID, cohortID, "cli", label, expected, now, now, "complete")
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_batch_chunks(batch_id,chunk_index,request_id,final_chunk,canonical_keys_json,result_json,created_at) VALUES(?,?,?,?,?,?,?)`, batchID, 0, cohortID, 1, keysJSON, resultJSON, now)
	}
	for i, m := range ordered {
		if err != nil {
			break
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_batch_members(batch_id,ordinal,canonical_key,job_id,submission_outcome,created_at) VALUES(?,?,?,?,?,?)`, batchID, i, m.CanonicalKey, nullable(m.JobID), m.Outcome, now)
	}
	if err != nil {
		_ = tx.Rollback()
		_, _ = c.S.DB().ExecContext(ctx, `UPDATE acquisition_batches SET membership_state='partial' WHERE id=?`, batchID)
		return ChunkResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChunkResult{}, err
	}
	return result, nil
}

func parseTime(raw string) time.Time { t, _ := time.Parse(time.RFC3339Nano, raw); return t }
func parseTimeChecked(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	return t, err == nil
}
func intPtr(v int) *int { return &v }
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
func stringsToAny(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}
