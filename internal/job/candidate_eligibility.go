// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"papio/internal/store"
	"papio/internal/work"
)

// CandidateEligibleKind and CandidateEligibleStatus define the single durable
// predicate for the auto-bind pool: "still carrying an open manual_download
// action". Every pool query MUST reference these rather than restating the
// literals, so the veto and the pool agree by construction.
const (
	CandidateEligibleKind   = "manual_download"
	CandidateEligibleStatus = "open"
)

// AdoptAwaitingDetail is the house-voice remedy when a job stops awaiting a
// human download before bytes are adopted. No internals vocabulary.
const AdoptAwaitingDetail = "This choice is no longer available — please open the popup and choose the paper again."

// ErrAdoptNotAwaiting reports that adoption was refused because the job is
// not awaiting any human action. It is deliberately NOT wrapped around
// ErrCandidateNotEligible: the two answer different questions (may these
// bytes enter this job at all, versus may the auto-bind pool offer this job),
// so a caller that means one must not match the other by accident. Callers
// that treat a refusal as permanent check both explicitly.
var ErrAdoptNotAwaiting = errors.New("job is not awaiting a human handoff")

// ErrCandidateNotEligible reports that the auto-bind pool did not offer this
// job: not in StateAwaitingHuman or no open manual_download.
var ErrCandidateNotEligible = errors.New("candidate not eligible")

// CandidateNotEligibleDetail is the house-voice detail for ErrCandidateNotEligible.
// Kept as alias for callers that name that error specifically.
const CandidateNotEligibleDetail = AdoptAwaitingDetail

// AdoptEligible reports whether a job in StateAwaitingHuman still has at
// least one open human action of any kind. This is the broader adoption
// acceptance predicate: a job accepts bytes only while it is genuinely waiting
// for a human to supply them. It is deliberately weaker than
// CandidateEligible (which requires kind=manual_download). Two predicates for
// two different questions is intentional; what this tree forbids is two
// definitions of the same relation.
// See CandidateEligibleKind/CandidateEligibleStatus for the stricter pool
// predicate.
func (js *Store) AdoptEligible(ctx context.Context, jobID string) (bool, error) {
	var dummy int
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT 1 FROM jobs j WHERE j.id = ? AND j.state = ? AND EXISTS (SELECT 1 FROM human_actions a WHERE a.job_id = j.id AND a.status = 'open') LIMIT 1`,
		jobID, StateAwaitingHuman).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AdoptEligibleTx is the transaction-scoped variant of AdoptEligible.
func AdoptEligibleTx(ctx context.Context, tx *sql.Tx, jobID string) (bool, error) {
	var dummy int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM jobs j WHERE j.id = ? AND j.state = ? AND EXISTS (SELECT 1 FROM human_actions a WHERE a.job_id = j.id AND a.status = 'open') LIMIT 1`,
		jobID, StateAwaitingHuman).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TransitionAwaitingToValidatingIfAdoptEligible atomically verifies the
// adoption acceptance predicate (StateAwaitingHuman + at least one open human
// action of any kind) and moves the job to validating with the given
// candidate. The verification and the state change share one transaction on
// the same connection, so a concurrent DismissHumanAction commit cannot slip
// between them. Reads inside go through tx only.
func (js *Store) TransitionAwaitingToValidatingIfAdoptEligible(ctx context.Context, jobID string, candidateID int64) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	eligible, err := AdoptEligibleTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if !eligible {
		return fmt.Errorf("%w: %s", ErrAdoptNotAwaiting, AdoptAwaitingDetail)
	}
	now := store.Now()
	detail := map[string]any{"reason": "adopt_browser_download", "source": "browser", "from": StateAwaitingHuman, "to": StateValidating}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	releaseLease := releasesLease(StateValidating)
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ?, selected_candidate_id = COALESCE(?, selected_candidate_id), lease_owner = CASE WHEN ? THEN NULL ELSE lease_owner END, lease_expires_at = CASE WHEN ? THEN NULL ELSE lease_expires_at END WHERE id = ? AND state = ?`,
		StateValidating, now, candidateID, releaseLease, releaseLease, jobID, StateAwaitingHuman)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		eligibleNow, _ := AdoptEligibleTx(ctx, tx, jobID)
		if !eligibleNow {
			return fmt.Errorf("%w: %s", ErrAdoptNotAwaiting, AdoptAwaitingDetail)
		}
		return fmt.Errorf("%w: job %s not in state %s", ErrConflict, jobID, StateAwaitingHuman)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`, jobID, now, string(detailJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

// CandidateEligible reports whether a single job currently satisfies the
// auto-bind pool predicate (StateAwaitingHuman + open manual_download). This
// is the stricter pool predicate; AdoptEligible is the broader acceptance one.
func (js *Store) CandidateEligible(ctx context.Context, jobID string) (bool, error) {
	var dummy int
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT 1 FROM jobs j WHERE j.id = ? AND j.state = ? AND EXISTS (SELECT 1 FROM human_actions a WHERE a.job_id = j.id AND a.status = ? AND a.kind = ?) LIMIT 1`,
		jobID, StateAwaitingHuman, CandidateEligibleStatus, CandidateEligibleKind).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CandidateEligibleJob is a job that inbound PDF bytes may be correlated with:
// live, awaiting a human download, and still carrying an open manual_download
// action.
type CandidateEligibleJob struct {
	JobID     string
	Work      work.Work
	BoundDOIs []string
}

// ListCandidateEligibleJobs returns every candidate-eligible job, oldest action
// first. A job qualifies iff it is live (not Terminal), is in
// StateAwaitingHuman, and has an open human_actions row of kind
// manual_download.
//
// The Terminal exclusion is implied by StateAwaitingHuman — every terminal
// state is distinct from awaiting_human, so filtering on the state already
// excludes terminal jobs without an additional Terminal check — but that
// implication is not obvious from the query alone, hence this note.
//
// Ordering is deterministic (oldest open action first, tie-broken by job id)
// so callers and tests never depend on SQLite row order. If a job somehow
// carries two open manual_download actions it appears once.
func (js *Store) ListCandidateEligibleJobs(ctx context.Context) ([]CandidateEligibleJob, error) {
	jobs, err := queryCandidateEligibleJobs(ctx, js.S.DB())
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		anchor, err := fetchSubmittedIdentity(ctx, js.S.DB(), jobs[i].JobID)
		if err != nil {
			return nil, err
		}
		jobs[i].BoundDOIs = BoundDOIs(anchor, jobs[i].Work)
	}
	return jobs, nil
}

// ListCandidateEligibleJobsTx is the transaction-scoped variant.
func ListCandidateEligibleJobsTx(ctx context.Context, tx *sql.Tx) ([]CandidateEligibleJob, error) {
	jobs, err := queryCandidateEligibleJobs(ctx, tx)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		anchor, err := fetchSubmittedIdentity(ctx, tx, jobs[i].JobID)
		if err != nil {
			return nil, err
		}
		jobs[i].BoundDOIs = BoundDOIs(anchor, jobs[i].Work)
	}
	return jobs, nil
}

func queryCandidateEligibleJobs(ctx context.Context, q handoffQueryer) ([]CandidateEligibleJob, error) {
	query := `
		SELECT j.id,
		       COALESCE(w.title,''),
		       COALESCE(w.authors_json,'[]'),
		       COALESCE(w.year,0),
		       COALESCE(w.zotio_item_key,''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'doi'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'pmid'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'arxiv'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'isbn'),''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = w.id AND kind = 'openalex'),'')
		FROM human_actions a
		JOIN jobs j ON j.id = a.job_id
		JOIN work_requests w ON w.id = j.work_request_id
		WHERE a.status = ? AND a.kind = ? AND j.state = ?
		GROUP BY j.id
		ORDER BY MIN(a.created_at) ASC, MIN(a.id) ASC, j.id ASC`

	rows, err := q.QueryContext(ctx, query, CandidateEligibleStatus, CandidateEligibleKind, StateAwaitingHuman)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CandidateEligibleJob
	for rows.Next() {
		var item CandidateEligibleJob
		var authorsJSON string
		var zotioKey string
		var doi, pmid, arxiv, isbn, openalex string
		var year int
		if err := rows.Scan(
			&item.JobID,
			&item.Work.Title,
			&authorsJSON,
			&year,
			&zotioKey,
			&doi, &pmid, &arxiv, &isbn, &openalex,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		item.Work.Year = year
		item.Work.DOI = doi
		item.Work.PMID = pmid
		item.Work.ArXiv = arxiv
		item.Work.ISBN = isbn
		item.Work.OpenAlex = openalex
		_ = zotioKey
		if err := json.Unmarshal([]byte(authorsJSON), &item.Work.Authors); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("job %s authors: %w", item.JobID, err)
		}
		out = append(out, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
