// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"papio/internal/job"
)

// Event kinds of the two follow-ups Apply runs after an import.
const (
	followUpCollectionFiling = "zotio.collection_filing"
	followUpEnrich           = "zotio.enrich"
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
// the measured sync lag; the later ones cover a desktop that was closed or had
// sync paused. After the last one, the failure stands.
var followUpRetryDelays = []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

const (
	// followUpScanLimit bounds the retryable follow-ups one pass reads.
	followUpScanLimit = 50
	// maxFollowUpsPerPass bounds the zotio runs in one pass. Every maintenance
	// runner shares one goroutine on a one-minute ticker, which is also why
	// internal/app's import retry has maxImportsPerPass.
	maxFollowUpsPerPass = 3
)

// FollowUpRetrier runs a collection filing or a metadata enrichment again when
// the Zotero Web API refused it with 404, because the imported parent had not
// synced up yet. Apply is the only other caller of either, and it runs them
// once: the job is then imported, and nothing drives an imported job again.
// A retry is safe to repeat. Filing reuses the named collection and does not
// add an item twice, and enrichment writes only fields that are still empty.
type FollowUpRetrier struct{ service *Service }

// FollowUpRetrier returns the maintenance runner, or nil when the plan/apply
// integration is not configured. It satisfies daemon.MaintenanceRunner.
func (s *Service) FollowUpRetrier() *FollowUpRetrier {
	if s.requirePlanServices() != nil {
		return nil
	}
	return &FollowUpRetrier{service: s}
}

// pendingFollowUp is one follow-up whose latest attempt the Web API refused
// with 404.
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

// RunDue performs one bounded pass. A follow-up that is not due yet stays
// selected for a later pass.
func (r *FollowUpRetrier) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
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
	return nil
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

// pendingFollowUps returns imported jobs' follow-ups whose latest attempt the
// Web API answered with 404 and whose retries are not used up, newest first.
// A follow-up with no recorded import to repeat it from is left out.
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
				AND json_extract(e.detail_json, '$.error_http_status') = 404
		)
		WHERE apply_json IS NOT NULL AND failures <= ?
		ORDER BY seq DESC
		LIMIT ?`,
		job.StateImported, followUpCollectionFiling, followUpEnrich, len(followUpRetryDelays), followUpScanLimit)
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
