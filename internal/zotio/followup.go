// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"papio/internal/job"
	"papio/internal/store"
)

// Event kinds of the two follow-ups Apply runs after an import, and of the
// marker that says it ran them.
const (
	followUpCollectionFiling = "zotio.collection_filing"
	followUpEnrich           = "zotio.enrich"
	// followUpsComplete is recorded once Apply has run both follow-ups,
	// whatever each one answered and even when neither applied. It is what
	// makes their absence readable: without it, an import with no enrichment
	// event looks the same whether a process death stopped the follow-ups or
	// auto-enrich was simply off at the time, and repairing the second kind
	// would rewrite the operator's library from today's configuration.
	followUpsComplete = "zotio.follow_ups"
)

// webAPINotSyncedHint explains a follow-up that the Zotero Web API answered
// with 404.
//
// Both follow-ups write through the Web API, and both read the parent item
// there first. An import through the desktop connector creates that parent on
// the desktop, which syncs it up afterwards: zotio measured 15-20s (its
// items delete comment), and Apply starts the follow-ups about a second after the
// import returns. On the operator's store on 2026-09-24, the only collection
// filing after a connector import had failed this way. Seven enrichments that
// ran seconds after a connector import had failed too, and each parent still
// had no abstract. The Web API had the parents later, but nothing asked again.
const webAPINotSyncedHint = "Zotero Web API does not have this item yet: Zotero desktop has not synced it"

// followUpRetryDelays is how long FollowUpRetrier waits after the Nth failed
// attempt of a follow-up before it tries again. The first wait is well past
// the measured sync lag; the later ones cover a desktop that was closed, had
// sync paused, or a Zotero outage that lasted. After the last one, the
// failure stands and its error event is the durable record of it.
var followUpRetryDelays = []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

const (
	// followUpScanLimit bounds the retryable follow-ups one pass reads.
	followUpScanLimit = 50
	// maxFollowUpsPerPass bounds the zotio runs in one pass. Every maintenance
	// runner shares one goroutine on a one-minute ticker, which is also why
	// internal/app's import retry has maxImportsPerPass.
	maxFollowUpsPerPass = 3
)

// FollowUpRetrier runs a collection filing or a metadata enrichment again
// when Zotero refused it for a reason that waiting can heal, and completes
// the ones a process death never ran at all. Apply is the only other caller
// of either, and it runs them once: the job is then imported, and nothing
// drives an imported job again. Repeating one is safe because both converge
// on the same state: filing reuses the named collection and does not add an
// item twice, and enrichment writes only fields that are still empty.
// pendingFollowUps holds the retryable set and says why each one qualifies.
type FollowUpRetrier struct{ service *Service }

// FollowUpRetrier returns the maintenance runner, or nil when the plan/apply
// integration is not configured. It satisfies daemon.MaintenanceRunner.
func (s *Service) FollowUpRetrier() *FollowUpRetrier {
	if s.requirePlanServices() != nil {
		return nil
	}
	return &FollowUpRetrier{service: s}
}

// pendingFollowUp is one follow-up whose latest attempt failed for a reason
// that waiting can heal.
type pendingFollowUp struct {
	jobID    string
	kind     string
	failedAt time.Time
	failures int
	detail   map[string]any
	apply    ApplyResult
}

// due reports whether the wait after the latest failure has passed.
func (p pendingFollowUp) due(now time.Time) bool {
	if p.failures < 1 || p.failures > len(followUpRetryDelays) {
		return false
	}
	return !now.Before(p.failedAt.Add(followUpRetryDelays[p.failures-1]))
}

// RunDue performs one bounded pass: it retries the follow-ups a retryable
// failure left behind, then repairs the ones a crash left unattempted. A
// follow-up that is not due yet stays selected for a later pass.
func (r *FollowUpRetrier) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if err := r.service.ensureFollowUpEpoch(ctx); err != nil {
		return fmt.Errorf("recording the Zotio follow-up epoch: %w", err)
	}
	pending, err := r.service.pendingFollowUps(ctx)
	if err != nil {
		return fmt.Errorf("reading Zotio follow-ups to retry: %w", err)
	}
	now := r.service.now()
	ran := 0
	for _, p := range pending {
		if ctx.Err() != nil || ran >= maxFollowUpsPerPass {
			return nil
		}
		if !p.due(now) {
			continue
		}
		if r.service.retryFollowUp(ctx, p) {
			ran++
		}
	}
	return r.service.repairMissingFollowUps(ctx, ran)
}

// ensureFollowUpEpoch records the instant this store began marking finished
// follow-ups, once, if no marker exists yet. It dates the repair scan on a
// store that has imports from before papio wrote markers: those imports are
// older than the epoch, so their missing follow-ups are read as the policy of
// the day rather than as a crash. Without it, the first import after an
// upgrade would set the epoch itself and any import that crashed before it
// would fall outside the scan for good. The insert is one statement, so two
// daemons cannot write two epochs.
func (s *Service) ensureFollowUpEpoch(ctx context.Context) error {
	_, err := s.Store.DB().ExecContext(ctx, `
		INSERT INTO events (job_id, at, kind, detail_json)
		SELECT NULL, ?, ?, '{"status":"epoch"}'
		WHERE NOT EXISTS (SELECT 1 FROM events WHERE kind = ?)`,
		store.Now(), followUpsComplete, followUpsComplete)
	return err
}

// retryFollowUp repeats exactly what failed: the same collection name, or the
// same parent, for the import the exports ledger recorded.
func (s *Service) retryFollowUp(ctx context.Context, p pendingFollowUp) bool {
	plan := &Plan{JobID: p.jobID}
	switch p.kind {
	case followUpCollectionFiling:
		plan.Collection = stringField(p.detail, "collection")
		return s.fileCollection(ctx, plan, &p.apply)
	case followUpEnrich:
		return s.enrichAutoImportedParent(ctx, plan, &p.apply)
	default:
		return false
	}
}

// pendingFollowUps returns imported jobs' follow-ups whose latest attempt
// failed for a reason that waiting can heal, and whose retries are not used
// up, newest first. A follow-up with no recorded import to repeat it from is
// left out.
//
// The retryable reasons are the Web API's 404 (the parent has not synced up
// yet), its 429 and its 5xx (rate limit and outage), and a network failure
// such as a refused connection. A 5xx does not prove the write never
// happened — Zotero can commit and then lose the response — so what makes
// the repeat safe is that both follow-ups converge on the same state rather
// than adding to it. Filing resolves the collection by name across every
// page, creates one only when absent with a fixed write token, reconciles an
// ambiguous create by re-reading the list, and then sets membership by item
// key, which zotio answers "no_op" the second time; enrichment takes its work
// queue from the items whose DOI or abstract is still empty, so a field that
// the lost response had already written is no longer proposed.
//
// Everything else stays out: a timeout or a cancellation gives no reason to
// expect a different answer, and the remaining 4xx say the request itself is
// wrong, which waiting does not change.
func (s *Service) pendingFollowUps(ctx context.Context) ([]pendingFollowUp, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `
		SELECT job_id, kind, at, detail_json, apply_json, failures FROM (
			SELECT e.seq, e.job_id, e.kind, e.at, e.detail_json,
				(SELECT x.result_json FROM exports x
					WHERE x.job_id = e.job_id AND x.kind = 'zotio_apply'
						AND json_extract(x.result_json, '$.status') IN ('applied', 'no_op')
					ORDER BY x.id DESC LIMIT 1) AS apply_json,
				(SELECT COUNT(*) FROM events f
					WHERE f.job_id = e.job_id AND f.kind = e.kind
						AND json_extract(f.detail_json, '$.status') = 'error') AS failures
			FROM jobs j
			JOIN events e ON e.job_id = j.id
			WHERE j.state = ?
				AND e.kind IN (?, ?)
				AND e.seq = (SELECT MAX(l.seq) FROM events l WHERE l.job_id = e.job_id AND l.kind = e.kind)
				AND json_extract(e.detail_json, '$.status') = 'error'
				AND (json_extract(e.detail_json, '$.error_http_status') IN (404, 429)
					OR json_extract(e.detail_json, '$.error_http_status') BETWEEN 500 AND 599
					OR json_extract(e.detail_json, '$.error_class') = ?)
		)
		WHERE apply_json IS NOT NULL AND failures <= ?
		ORDER BY seq DESC
		LIMIT ?`,
		job.StateImported, followUpCollectionFiling, followUpEnrich, ErrorClassNetwork,
		len(followUpRetryDelays), followUpScanLimit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var pending []pendingFollowUp
	for rows.Next() {
		var p pendingFollowUp
		var at, detailJSON, applyJSON string
		if err := rows.Scan(&p.jobID, &p.kind, &at, &detailJSON, &applyJSON, &p.failures); err != nil {
			return nil, err
		}
		failedAt, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			log.Printf("papio: Zotio follow-up for job %s has an unreadable time %q", p.jobID, at)
			continue
		}
		p.failedAt = failedAt
		if err := json.Unmarshal([]byte(detailJSON), &p.detail); err != nil {
			log.Printf("papio: Zotio follow-up for job %s has an unreadable detail: %v", p.jobID, err)
			continue
		}
		if err := json.Unmarshal([]byte(applyJSON), &p.apply); err != nil {
			log.Printf("papio: recorded Zotio import for job %s is unreadable: %v", p.jobID, err)
			continue
		}
		pending = append(pending, p)
	}
	return pending, rows.Err()
}

// followUpsOutstanding says which follow-ups of one import have never been
// attempted, and whether the completion marker is already recorded. A filing
// that moved from the desktop save to the Web API (filingDeferred) has not
// been attempted either.
type followUpsOutstanding struct {
	filing bool
	enrich bool
	marked bool
}

// outstandingFollowUps reads one job's follow-up history. Each follow-up
// records an event whatever its outcome, so the absence of an event is what
// distinguishes "never ran" from "ran and failed", which the retry schedule
// above owns.
func (s *Service) outstandingFollowUps(ctx context.Context, jobID string) (followUpsOutstanding, error) {
	var filingStatus sql.NullString
	var enrichEvents, marks int
	err := s.Store.DB().QueryRowContext(ctx, `
		SELECT
			(SELECT json_extract(e.detail_json, '$.status') FROM events e
				WHERE e.job_id = ? AND e.kind = ? ORDER BY e.seq DESC LIMIT 1),
			(SELECT COUNT(*) FROM events e WHERE e.job_id = ? AND e.kind = ?),
			(SELECT COUNT(*) FROM events e WHERE e.job_id = ? AND e.kind = ?)`,
		jobID, followUpCollectionFiling, jobID, followUpEnrich, jobID, followUpsComplete).
		Scan(&filingStatus, &enrichEvents, &marks)
	if err != nil {
		return followUpsOutstanding{}, err
	}
	return followUpsOutstanding{
		filing: !filingStatus.Valid || filingStatus.String == filingDeferred,
		enrich: enrichEvents == 0,
		marked: marks > 0,
	}, nil
}

// recordFollowUpsComplete marks an import's follow-ups as run, which is what
// takes it out of the repair scan for good.
func (s *Service) recordFollowUpsComplete(ctx context.Context, jobID string) {
	_ = s.Bundle.Jobs.RecordEvent(context.WithoutCancel(ctx), jobID, followUpsComplete, map[string]any{"status": "done"})
}

// completeFollowUps runs the follow-ups of a recorded import that have never
// been attempted, and only those. Apply reaches it when it replays an import
// the exports ledger already holds, which is what a process death between
// recording the import and running its follow-ups leaves behind. Repeating a
// follow-up that did run would write a second event for work already done,
// and the retry schedule, not this path, owns a follow-up that failed.
func (s *Service) completeFollowUps(ctx context.Context, plan *Plan, result *ApplyResult) {
	if plan == nil || result == nil {
		return
	}
	outstanding, err := s.outstandingFollowUps(ctx, plan.JobID)
	if err != nil {
		log.Printf("papio: reading Zotio follow-ups of job %s: %v", plan.JobID, err)
		return
	}
	if outstanding.filing && !filedWithImport(plan, result) {
		s.fileCollection(ctx, plan, result)
	}
	if outstanding.enrich {
		s.enrichAutoImportedParent(ctx, plan, result)
	}
	if !outstanding.marked {
		s.recordFollowUpsComplete(ctx, plan.JobID)
	}
}

// missingFollowUp is one recorded import that carries no completion marker,
// which is what a process death between recording the import and running its
// follow-ups leaves behind. Nothing drives an imported job again, so without
// this the paper stays outside its collection and without its metadata.
type missingFollowUp struct {
	jobID string
	apply ApplyResult
}

// repairMissingFollowUps finishes the imports whose follow-ups never ran,
// sharing the pass budget with the retry schedule. It repeats nothing: each
// follow-up that ran recorded an event, and an event takes that follow-up out
// of outstandingFollowUps. Every job it reads leaves the scan in the same
// pass, because it records the completion marker whether or not a follow-up
// applied to that job.
func (s *Service) repairMissingFollowUps(ctx context.Context, ran int) error {
	if ran >= maxFollowUpsPerPass || ctx.Err() != nil {
		return nil
	}
	missing, err := s.missingFollowUps(ctx)
	if err != nil {
		return fmt.Errorf("reading Zotio imports with no follow-up: %w", err)
	}
	for _, m := range missing {
		if ctx.Err() != nil || ran >= maxFollowUpsPerPass {
			return nil
		}
		row, err := s.Bundle.Jobs.Get(ctx, m.jobID)
		if err != nil {
			continue // best-effort per job; the next pass reads it again
		}
		outstanding, err := s.outstandingFollowUps(ctx, m.jobID)
		if err != nil {
			continue
		}
		// The plan the import ran under is gone once it succeeded, so the
		// filing is rebuilt from the same policy fields PlanJobs reads.
		// CollectionIsKey repeats Apply's own reading of a key-shaped
		// collection on a job that names a Zotero item: there the collection
		// is a missing-PDF queue filter, so it is a key and not a name, and
		// filing accepts names only (fileCollection). That is a filing-input
		// decision about this one write, not an ownership answer: the job's
		// ownership was settled before the import this repair completes.
		collection := strings.TrimSpace(row.Policy.Collection)
		plan := &Plan{
			JobID:           m.jobID,
			Collection:      collection,
			CollectionIsKey: row.ZotioItemKey != "" && keyRE.MatchString(collection),
		}
		did := false
		if outstanding.filing {
			did = s.fileCollection(ctx, plan, &m.apply)
		}
		if outstanding.enrich && s.enrichAutoImportedParent(ctx, plan, &m.apply) {
			did = true
		}
		s.recordFollowUpsComplete(ctx, m.jobID)
		if did {
			ran++
		}
	}
	return nil
}

// missingFollowUps returns imports, newest first, that recorded a successful
// apply and no completion marker. It reads the exports ledger rather than the
// job list because the recorded apply is the import the follow-ups belong to.
//
// There is no age cutoff: a crash victim is repaired however long the daemon
// stayed down. The scan is bounded instead by the oldest marker this store
// holds, which ensureFollowUpEpoch writes on the first maintenance pass. An
// import older than that instant has no marker for a reason that is not a
// crash — papio was not writing markers yet — and its follow-ups remain the
// operator's to run. Every import after it carries a marker unless a process
// death stopped the follow-ups.
func (s *Service) missingFollowUps(ctx context.Context) ([]missingFollowUp, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `
		SELECT job_id, apply_json FROM (
			SELECT x.id AS export_id, x.job_id AS job_id, x.result_json AS apply_json,
				x.created_at AS imported_at,
				(SELECT COUNT(*) FROM events e WHERE e.job_id = x.job_id AND e.kind = ?) AS marks
			FROM exports x JOIN jobs j ON j.id = x.job_id
			WHERE x.kind = 'zotio_apply' AND j.state = ?
				AND json_extract(x.result_json, '$.status') IN ('applied', 'no_op')
				AND COALESCE(json_extract(x.result_json, '$.parent_key'), '') <> ''
				AND (COALESCE(json_extract(j.policy_json, '$.collection'), '') <> ''
					OR COALESCE(json_extract(j.policy_json, '$.auto_import'), 0) <> 0)
		)
		WHERE marks = 0 AND imported_at >= (SELECT MIN(e.at) FROM events e WHERE e.kind = ?)
		ORDER BY export_id DESC
		LIMIT ?`,
		followUpsComplete, job.StateImported, followUpsComplete, followUpScanLimit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var missing []missingFollowUp
	seen := make(map[string]bool)
	for rows.Next() {
		var m missingFollowUp
		var applyJSON string
		if err := rows.Scan(&m.jobID, &applyJSON); err != nil {
			return nil, err
		}
		if seen[m.jobID] {
			// A replanned job can hold more than one recorded apply; the
			// newest one is the import the follow-ups belong to.
			continue
		}
		seen[m.jobID] = true
		if err := json.Unmarshal([]byte(applyJSON), &m.apply); err != nil {
			log.Printf("papio: recorded Zotio import for job %s is unreadable: %v", m.jobID, err)
			continue
		}
		missing = append(missing, m)
	}
	return missing, rows.Err()
}
