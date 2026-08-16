// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"papio/internal/work"
)

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
	// Populate BoundDOIs for every candidate. One SubmittedIdentity load per
	// candidate is acceptable: the pool is a human's manual-download backlog,
	// bounded and small, and the alternative is a second SQL expression of the
	// same bound-DOI rule.
	for i := range jobs {
		anchor, err := fetchSubmittedIdentity(ctx, js.S.DB(), jobs[i].JobID)
		if err != nil {
			return nil, err
		}
		jobs[i].BoundDOIs = BoundDOIs(anchor, jobs[i].Work)
	}
	return jobs, nil
}

// ListCandidateEligibleJobsTx is the transaction-scoped variant of
// ListCandidateEligibleJobs. It MUST be used by callers already inside a
// transaction: db.SetMaxOpenConns(1) means the transaction holds the ONLY
// connection, so any query issued through the pool (rather than through that
// tx) inside the transaction DEADLOCKS. Reading through the tx observes the
// transaction's own view, which is the whole point of the fence.
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

// queryCandidateEligibleJobs shares ONE SQL body between the pool and
// transaction entry points. It reuses the existing handoffQueryer interface
// that already covers QueryContext for *sql.DB/*sql.Tx, checked before
// inventing a new one.
func queryCandidateEligibleJobs(ctx context.Context, q handoffQueryer) ([]CandidateEligibleJob, error) {
	// One joined query so the pool cannot observe a half-written action/job
	// pair between separate reads, and so work metadata arrives without N+1
	// lookups. Work hydration mirrors the neighbouring joined queries (see
	// queryOpenHandoffJobs): title/authors/year/zotio key from work_requests
	// with COALESCE for nullable columns, and identifiers via scalar
	// sub-selects, scanning with the same sql.NullString/COALESCE and
	// jsonScanner style.
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
		WHERE a.status = 'open' AND a.kind = 'manual_download' AND j.state = ?
		GROUP BY j.id
		ORDER BY MIN(a.created_at) ASC, MIN(a.id) ASC, j.id ASC`

	rows, err := q.QueryContext(ctx, query, StateAwaitingHuman)
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
		_ = zotioKey // work.Work has no zotio field; scanned to keep column list aligned with Get/queryOpenHandoffJobs
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
