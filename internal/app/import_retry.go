// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"log"
	"strings"

	"papio/internal/job"
)

// maxImportAttempts bounds how many times the import-retry pass re-drives one
// ready job's auto-import before giving up. A ready job is a validated artifact
// regardless of import outcome; this only governs the best-effort Zotero import
// that runs after acquisition, so a persistently failing import eventually
// surfaces as import_failed instead of re-driving Zotio forever.
const maxImportAttempts = 5

// readyImportScanLimit bounds one retry pass to the newest ready jobs, matching
// the daemon's other newest-first scans. A failed import ages out of the window
// only if this many newer ready jobs appear before it succeeds — far more than
// the bounded retry window admits.
const readyImportScanLimit = 200

// maxImportsPerPass bounds how many imports one maintenance pass performs.
//
// Two reasons, and the second is the one that bites. Each import drives Zotero's
// desktop connector, which leaks a progress window per invocation (see
// bootstrap.autoImportMinInterval), so an unbounded pass can wedge the
// operator's library application. And every maintenance runner shares ONE
// goroutine on a one-minute ticker (daemon.MaintenanceRunners.RunDue), so a long
// import pass starves its siblings — action reminders, handoff repair, offered
// recovery. That starvation predates the pacing: at readyImportScanLimit and the
// ~3.5s an apply measured on the operator's machine, a saturated pass already ran
// ~12 minutes inside a 60-second cadence. This bound is what makes the pass fit
// its cadence instead of merely being shorter than it was.
//
// Draining is not the goal of any single pass; the queue is durable and the next
// tick continues it. Three keeps a saturated pass near half its cadence even with
// the pacing floor applied between applies.
const maxImportsPerPass = 3

// EnsureCitationMetadata fills a ready job's citation identity from its DOI
// before an import is attempted, and is why an import can recover from a gap
// that used to be permanent.
//
// zotio's bundle validation requires a citation title and authors. A ready job
// that has neither fails it every time with "no citation record for this paper",
// and ready is TERMINAL: the scheduler never processes the job again, so the
// only code that fills those fields (enrichDOIWork, during resolve) can never
// run for it. The job then exhausts maxImportAttempts and importNeedsRetry
// stops selecting it, so it holds a validated PDF that papio will never file,
// with no operator-reachable way to fix it.
//
// Measured on the operator's machine 2026-08-22: 21 of 72 ready jobs had no
// citation title, 16 of them already at exactly 5 failed attempts, while papio's
// own configured discovery backend returned a title and authors for their DOIs
// on demand. The metadata was always available; nothing ever asked again.
//
// This runs on the shared import seam rather than inside the retry pass because
// the operator-driven backfill reaches the importer without passing through it,
// and the already-capped jobs are reachable ONLY through the backfill.
//
// Best-effort by construction: enrichment must never fail an import that could
// otherwise succeed, and enrichDOIWork already absorbs budget refusals and
// post-wire failures. It logs when the gap survives the attempt, because a
// paper that stays unfilable needs a reason the operator can read.
func (s *Service) EnsureCitationMetadata(ctx context.Context, jobID string) {
	if s == nil || s.Discovery == nil || s.Jobs == nil {
		return
	}
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil || row == nil {
		return
	}
	if strings.TrimSpace(row.Work.Title) != "" || strings.TrimSpace(row.Work.DOI) == "" {
		return
	}
	if _, err := s.enrichDOIWork(ctx, row); err != nil {
		log.Printf("papio: citation enrichment for job %s failed: %v", jobID, err)
		return
	}
	if strings.TrimSpace(row.Work.Title) == "" {
		log.Printf("papio: no citation metadata available for job %s (doi %s); import will fail bundle validation",
			jobID, row.Work.DOI)
	}
}

// ImportRetrier re-drives auto-import for ready jobs whose Zotero import has not
// succeeded. It exists because ready is a terminal state: the scheduler never
// revisits a ready job, so a transient Zotio outage during the inline import
// after validation would otherwise strand a validated PDF unimported forever
// (durability review #3). Each PlanAndApply is idempotent — the exports-ledger
// idempotency key plus zotio reservation reconciliation make a replay a no_op
// once the item exists — so re-driving is dedup-safe.
type ImportRetrier struct{ svc *Service }

// ImportRetrier returns a maintenance runner that retries pending imports. It
// satisfies daemon.MaintenanceRunner without importing that package.
func (s *Service) ImportRetrier() *ImportRetrier { return &ImportRetrier{svc: s} }

// RunDue performs one bounded, best-effort import-retry pass. Its error is
// best-effort maintenance and never terminates acquisition workers.
func (r *ImportRetrier) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.svc.retryPendingImports(ctx)
}

func (s *Service) retryPendingImports(ctx context.Context) error {
	if s == nil || s.AutoImporter == nil {
		return nil
	}
	rows, err := s.Jobs.List(ctx, job.StateReady, readyImportScanLimit)
	if err != nil {
		return err
	}
	applied := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return nil
		}
		if !row.Policy.AutoImport {
			continue
		}
		events, err := s.Jobs.Events(ctx, row.ID)
		if err != nil {
			continue // best-effort per job; the next pass retries this one
		}
		if !importNeedsRetry(events) {
			// A delivered paper whose lifecycle never advanced stays in ready
			// forever, because every reader of that state agrees there is
			// nothing to do: this pass sees a successful outcome and skips it,
			// and doctor's undelivered_zotero_imports excludes it for the same
			// reason. Measured on the operator's store, 26 papers had sat that
			// way for 34-40 days with the PDF attached in Zotero. The stale
			// state is not cosmetic - a later browser download was refused
			// adoption with "job is not awaiting a human handoff (state
			// ready)", so the row also blocks work that arrives after it.
			s.reconcileDeliveredReady(ctx, &row, events)
			continue
		}
		s.autoImportReady(ctx, &row)
		applied++
		if applied >= maxImportsPerPass {
			return nil
		}
	}
	return nil
}

// reconcileDeliveredReady advances a ready job whose durable auto-import
// outcome already succeeded. Apply does this inline through markImported, so
// this is the repair path for rows that missed it - a dropped transition, or a
// success recorded by a build that predates the lifecycle advance. The keys
// come from the durable event rather than a fresh Zotio call: the delivery is
// already a fact, and re-driving the connector to re-learn it would leak a
// progress window per row (see maxImportsPerPass).
//
// A conflict means another actor moved the row first, which is the desired end
// state either way.
func (s *Service) reconcileDeliveredReady(ctx context.Context, row *job.Row, events []map[string]any) {
	status, parentKey, attachmentKey := settledImport(events)
	switch status {
	case "applied", "no_op", "duplicate":
	default:
		return
	}
	detail := map[string]any{
		"status": status,
		"reason": "import_reconciled",
	}
	if parentKey != "" {
		detail["parent_key"] = parentKey
	}
	if attachmentKey != "" {
		detail["attachment_key"] = attachmentKey
	}
	if err := s.Jobs.Transition(ctx, row.ID, job.StateReady, job.StateImported, detail); err != nil {
		if !errors.Is(err, job.ErrConflict) {
			log.Printf("papio: reconciling delivered import for job %s: %v", row.ID, err)
		}
	}
}

// settledImport returns the latest durable auto-import outcome and its item
// keys. The keys are read from the same event that carries the status, because
// an earlier failed attempt can carry a parent key from a partial write.
func settledImport(events []map[string]any) (status, parentKey, attachmentKey string) {
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "zotio.auto_import" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		status, _ = detail["status"].(string)
		parentKey, _ = detail["parent_key"].(string)
		attachmentKey, _ = detail["attachment_key"].(string)
	}
	return status, parentKey, attachmentKey
}

// importNeedsRetry reports whether a ready job's latest durable auto-import
// outcome warrants another attempt. A successful outcome (applied, no_op, or
// duplicate) is done. An error outcome retries until maxImportAttempts distinct
// failures accumulate, after which the job is left import_failed. A missing or
// skipped outcome retries: the inline import never recorded a result (a dropped
// event insert) or ran before Zotio was configured.
func importNeedsRetry(events []map[string]any) bool {
	var status string
	errorCount := 0
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "zotio.auto_import" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		status, _ = detail["status"].(string)
		if status == "error" {
			errorCount++
		}
	}
	switch status {
	case "applied", "no_op", "duplicate":
		return false
	}
	return errorCount < maxImportAttempts
}
