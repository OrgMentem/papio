// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

// unavailableRecheckScanLimit bounds one maintenance pass. It matches the
// import-retry scan because both runners share the daemon's maintenance
// goroutine. The oldest-first cursor advances past exempt rows, so a permanent
// identity gap cannot starve newer unavailable outcomes.
const unavailableRecheckScanLimit = readyImportScanLimit

type unavailableRecheckCursor struct {
	updatedAt string
	id        string
}

// UnavailableRecheck re-submits old unavailable outcomes through the ordinary
// application boundary. The cursor makes each bounded pass continue through a
// large backlog before it wraps to the oldest still-eligible outcome.
type UnavailableRecheck struct {
	svc *Service

	mu     sync.Mutex
	cursor unavailableRecheckCursor
}

// UnavailableRechecker returns a maintenance runner for terminal unavailable
// outcomes. It satisfies daemon.MaintenanceRunner without importing that
// package.
func (s *Service) UnavailableRechecker() *UnavailableRecheck {
	return &UnavailableRecheck{svc: s}
}

// RunDue performs one bounded, best-effort unavailable re-check pass.
func (r *UnavailableRecheck) RunDue(ctx context.Context) error {
	if r == nil || r.svc == nil || r.svc.Jobs == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runDue(ctx)
}

func (r *UnavailableRecheck) runDue(ctx context.Context) error {
	days := r.svc.Config.Zotio.UnavailableRecheckDays
	if days <= 0 {
		return nil
	}
	now := time.Now
	if r.svc.Now != nil {
		now = r.svc.Now
	}
	cutoff := now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
	ids, cursor, err := r.selectDue(ctx, cutoff, r.cursor)
	if err != nil {
		return err
	}
	if len(ids) == 0 && r.cursor.updatedAt != "" {
		r.cursor = unavailableRecheckCursor{}
		ids, cursor, err = r.selectDue(ctx, cutoff, r.cursor)
		if err != nil {
			return err
		}
	}
	if len(ids) != 0 {
		r.cursor = cursor
	}

	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		row, err := r.svc.Jobs.Get(ctx, id)
		if err != nil || row.State != job.StateUnavailable || job.RecheckExempt(row.TerminalReason) {
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		if err != nil || !updatedAt.Before(now().UTC().Add(-time.Duration(days)*24*time.Hour)) {
			continue
		}
		blocked, err := r.sameWorkAlreadyActiveOrDelivered(ctx, row)
		if err != nil || blocked {
			continue
		}
		already, err := r.alreadyRechecked(ctx, row.ID)
		if err != nil || already {
			continue
		}

		request := unavailableWorkRequest(row)
		autoImport := row.Policy.AutoImport
		_, _ = r.svc.SubmitWithOptionsAs(ctx, job.PrincipalUnknown, request, SubmitOptions{
			AutoImport:        &autoImport,
			RecheckOf:         row.ID,
			RecheckWindowDays: days,
		})
	}
	return nil
}

func (r *UnavailableRecheck) selectDue(ctx context.Context, cutoff string, after unavailableRecheckCursor) ([]string, unavailableRecheckCursor, error) {
	query := `SELECT j.id, j.updated_at
		FROM jobs j
		WHERE j.state = ? AND julianday(j.updated_at) < julianday(?)
		  AND NOT EXISTS (
			SELECT 1 FROM events e
			WHERE e.job_id = j.id AND e.kind = 'unavailable.recheck'
		  )`
	args := []any{job.StateUnavailable, cutoff}
	if after.updatedAt != "" {
		query += ` AND (
			julianday(j.updated_at) > julianday(?)
			OR (
				julianday(j.updated_at) = julianday(?)
				AND (j.updated_at > ? OR (j.updated_at = ? AND j.id > ?))
			)
		)`
		args = append(args, after.updatedAt, after.updatedAt, after.updatedAt, after.updatedAt, after.id)
	}
	query += ` ORDER BY julianday(j.updated_at) ASC, j.updated_at ASC, j.id ASC LIMIT ?`
	args = append(args, unavailableRecheckScanLimit)

	rows, err := r.svc.Jobs.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, unavailableRecheckCursor{}, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, unavailableRecheckScanLimit)
	var cursor unavailableRecheckCursor
	for rows.Next() {
		var id, updatedAt string
		if err := rows.Scan(&id, &updatedAt); err != nil {
			_ = rows.Close()
			return nil, unavailableRecheckCursor{}, err
		}
		ids = append(ids, id)
		cursor = unavailableRecheckCursor{updatedAt: updatedAt, id: id}
	}
	if err := rows.Close(); err != nil {
		return nil, unavailableRecheckCursor{}, err
	}
	if err := rows.Err(); err != nil {
		return nil, unavailableRecheckCursor{}, err
	}
	return ids, cursor, nil
}

// sameWorkAlreadyActiveOrDelivered uses the same strongest canonical identity
// as job.Store's submit-time live-job lookup. Submit repeats the live check in
// its own transaction. This probe adds ready and imported, which are terminal
// for submission dedupe but must suppress a maintenance re-check.
func (r *UnavailableRecheck) sameWorkAlreadyActiveOrDelivered(ctx context.Context, row *job.Row) (bool, error) {
	kind, value, ok := strings.Cut(row.Work.Describe(), ":")
	if !ok {
		return false, nil
	}
	switch kind {
	case "doi", "pmid", "arxiv", "isbn", "openalex":
	default:
		return false, nil
	}
	var exists bool
	err := r.svc.Jobs.S.DB().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs j
			JOIN identifiers i ON i.work_request_id = j.work_request_id
			WHERE i.kind = ? AND i.value = ? AND j.id != ?
			  AND j.state NOT IN (?, ?, ?)
		)`, kind, value, row.ID, job.StateUnavailable, job.StateFailed, job.StateCancelled).Scan(&exists)
	return exists, err
}

func (r *UnavailableRecheck) alreadyRechecked(ctx context.Context, jobID string) (bool, error) {
	var exists bool
	err := r.svc.Jobs.S.DB().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM events WHERE job_id = ? AND kind = 'unavailable.recheck'
		)`, jobID).Scan(&exists)
	return exists, err
}

func unavailableWorkRequest(row *job.Row) protocol.WorkRequest {
	identifiers := &protocol.Identifiers{
		DOI: row.Work.DOI, PMID: row.Work.PMID, ArXiv: row.Work.ArXiv,
		ISBN: row.Work.ISBN, OpenAlex: row.Work.OpenAlex,
	}
	if identifiers.DOI == "" && identifiers.PMID == "" && identifiers.ArXiv == "" && identifiers.ISBN == "" && identifiers.OpenAlex == "" {
		identifiers = nil
	}
	return protocol.WorkRequest{
		SchemaVersion:      protocol.WorkRequestSchemaVersion,
		RequestID:          row.WorkRequestID,
		Identifiers:        identifiers,
		Title:              row.Work.Title,
		Authors:            append([]string(nil), row.Work.Authors...),
		Year:               row.Work.Year,
		ZotioItemKey:       row.ZotioItemKey,
		Collection:         row.Policy.Collection,
		DesiredVersion:     row.Policy.DesiredVersion,
		AccessModeOverride: row.Policy.AccessMode,
		Resolver:           row.Policy.Resolver,
		MaxCostUSD:         row.Policy.MaxCostUSD,
		SourcesAllow:       append([]string(nil), row.Policy.SourcesAllow...),
		SourcesDeny:        append([]string(nil), row.Policy.SourcesDeny...),
	}
}
