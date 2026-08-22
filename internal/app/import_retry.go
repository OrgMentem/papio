// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"

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
