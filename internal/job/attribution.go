// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"papio/internal/store"
)

// Consumer returns the consumer name recorded for a job's work request and
// whether one was recorded at all.
//
// The second return value is the whole point: a job with no attribution is a
// different fact from a job attributed to the empty string, and a shared
// daemon's accounting has to be able to say "unattributed" rather than quietly
// folding those rows into some default owner. Same reason it is not a Row field
// as Principal is not: Row is the body of jobs.get, decoded with
// DisallowUnknownFields, so new facts arrive by new method (ADR-0007).
func (js *Store) Consumer(ctx context.Context, jobID string) (string, bool, error) {
	var consumer sql.NullString
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT wr.consumer FROM jobs j
		JOIN work_requests wr ON wr.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(&consumer)
	if err != nil {
		return "", false, err
	}
	if !consumer.Valid || consumer.String == "" {
		return "", false, nil
	}
	return consumer.String, true, nil
}

// ConsumersFor returns the recorded consumer for each of the supplied jobs.
// Jobs with no attribution are absent from the map rather than present with an
// empty value, so a caller cannot mistake "nobody claimed this" for a name.
//
// One query for the whole page: decorating a 500-row listing with a per-row
// lookup would multiply the page's cost by its own length.
func (js *Store) ConsumersFor(ctx context.Context, jobIDs []string) (map[string]string, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	q := `SELECT j.id, wr.consumer FROM jobs j
		JOIN work_requests wr ON wr.id = j.work_request_id
		WHERE wr.consumer IS NOT NULL AND j.id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",") + `)`
	args := make([]any, 0, len(jobIDs))
	for _, id := range jobIDs {
		args = append(args, id)
	}
	rows, err := js.S.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string, len(jobIDs))
	for rows.Next() {
		var jobID, consumer string
		if err := rows.Scan(&jobID, &consumer); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if consumer != "" {
			out[jobID] = consumer
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// ValidationRecord is one durable validation verdict: which candidate's bytes
// were judged, what the pipeline decided, and the full stage-by-stage evidence
// as a versioned JSON document.
//
// Document is text rather than a decoded struct for the reason bundle.document
// is: it carries its own schema_version, so the evidence can grow a field
// without forcing a new RPC method on every reader.
type ValidationRecord struct {
	JobID       string `json:"job_id"`
	CandidateID int64  `json:"candidate_id"`
	SHA256      string `json:"sha256"`
	Outcome     string `json:"outcome"`
	RecordedAt  string `json:"recorded_at"`
	Document    string `json:"document"`
}

// RecordValidationReport persists one validation verdict, replacing any earlier
// report for the same candidate — a retry that re-fetches and re-validates the
// same candidate produces a new verdict, and the current one is the truth.
//
// It is keyed by (job, candidate), never by content hash: two jobs that obtain
// identical bytes each reach their own identity decision against their own
// requested work, and ADR-0007 forbids projecting one onto the other.
func (js *Store) RecordValidationReport(ctx context.Context, record ValidationRecord) error {
	if record.JobID == "" || record.CandidateID == 0 {
		return errors.New("validation report requires a job and candidate")
	}
	if record.Outcome == "" || record.Document == "" {
		return errors.New("validation report requires an outcome and document")
	}
	if record.RecordedAt == "" {
		record.RecordedAt = store.Now()
	}
	_, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO validation_reports (job_id, candidate_id, sha256, outcome, recorded_at, document)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id, candidate_id) DO UPDATE SET
			sha256 = excluded.sha256,
			outcome = excluded.outcome,
			recorded_at = excluded.recorded_at,
			document = excluded.document`,
		record.JobID, record.CandidateID, record.SHA256, record.Outcome, record.RecordedAt, record.Document)
	return err
}

// ValidationReports returns every validation verdict recorded for one job,
// newest first. A job validated before this evidence was persisted returns no
// rows: there is nothing to reconstruct it from, and a synthesized report would
// be a guess presented as provenance.
func (js *Store) ValidationReports(ctx context.Context, jobID string) ([]ValidationRecord, error) {
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT job_id, candidate_id, sha256, outcome, recorded_at, document
		FROM validation_reports WHERE job_id = ?
		ORDER BY recorded_at DESC, candidate_id DESC`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ValidationRecord
	for rows.Next() {
		var record ValidationRecord
		if err := rows.Scan(&record.JobID, &record.CandidateID, &record.SHA256,
			&record.Outcome, &record.RecordedAt, &record.Document); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}
