// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"papio/internal/job"
)

const (
	ImportBackfillLimitDefault = 50
	importBackfillLimitMax     = 50
)

// ImportBackfillRequest configures one bounded import-backfill pass.
type ImportBackfillRequest struct {
	Apply               bool   `json:"apply"`
	IncludeNotRequested bool   `json:"include_not_requested"`
	Limit               int    `json:"limit"`
	Cursor              string `json:"cursor,omitempty"`
}

// ImportBackfillItem is one job in a backfill breakdown or apply outcome.
type ImportBackfillItem struct {
	JobID     string `json:"job_id"`
	CreatedAt string `json:"created_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status,omitempty"`
	ParentKey string `json:"parent_key,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ImportBackfillSummary counts the cohort honestly for operators and agents.
type ImportBackfillSummary struct {
	Selected             int  `json:"selected"`
	WouldImport          int  `json:"would_import"`
	AlreadyOwned         int  `json:"already_owned"`
	ExpectedFail         int  `json:"expected_fail"`
	Applied              int  `json:"applied,omitempty"`
	Failed               int  `json:"failed,omitempty"`
	IncludeNotRequested  bool `json:"include_not_requested"`
	NotRequestedExcluded int  `json:"not_requested_excluded,omitempty"`
}

// ImportBackfillResult is stable machine output for `papio zotio import-backfill`.
type ImportBackfillResult struct {
	DryRun       bool                  `json:"dry_run"`
	Summary      ImportBackfillSummary `json:"summary"`
	WouldImport  []ImportBackfillItem  `json:"would_import"`
	AlreadyOwned []ImportBackfillItem  `json:"already_owned"`
	ExpectedFail []ImportBackfillItem  `json:"expected_fail"`
	Applied      []ImportBackfillItem  `json:"applied,omitempty"`
	Failed       []ImportBackfillItem  `json:"failed,omitempty"`
	Cursor       string                `json:"cursor,omitempty"`
	Truncated    bool                  `json:"truncated"`
}

// ImportBackfillImporter plans and applies one ready job through the same path
// inline auto-import uses.
type ImportBackfillImporter interface {
	PlanAndApply(context.Context, string) (status, parentKey, attachmentKey string, err error)
}

// ImportBackfill selects stranded ready jobs oldest-first, classifies them, and
// optionally imports them through PlanAndApply. Dry-run is the default: no
// Zotero writes and no durable auto-import events unless Apply is true.
func (s *Service) ImportBackfill(ctx context.Context, req ImportBackfillRequest, importer ImportBackfillImporter) (*ImportBackfillResult, error) {
	if s == nil || s.Store == nil || s.Bundle == nil || s.Bundle.Jobs == nil {
		return nil, errors.New("zotio import-backfill is not configured")
	}
	if req.Apply {
		if s.CLI == nil {
			return nil, errors.New("zotio is not configured")
		}
		if _, err := s.CLI.Preflight(ctx); err != nil {
			return nil, err
		}
		if importer == nil {
			return nil, errors.New("zotio import-backfill apply requires an importer")
		}
	}
	limit := effectiveImportBackfillLimit(req.Limit)
	cursor := strings.TrimSpace(req.Cursor)
	if err := s.ValidateImportBackfillCursor(ctx, cursor); err != nil {
		return nil, err
	}

	excluded, err := s.countImportBackfillExcluded(ctx, req.IncludeNotRequested)
	if err != nil {
		return nil, err
	}

	candidates, truncated, err := s.listImportBackfillCandidates(ctx, req.IncludeNotRequested, cursor, limit)
	if err != nil {
		return nil, err
	}

	result := &ImportBackfillResult{
		DryRun:       !req.Apply,
		Truncated:    truncated,
		WouldImport:  []ImportBackfillItem{},
		AlreadyOwned: []ImportBackfillItem{},
		ExpectedFail: []ImportBackfillItem{},
		Summary: ImportBackfillSummary{
			IncludeNotRequested:  req.IncludeNotRequested,
			NotRequestedExcluded: excluded,
		},
	}
	if req.Apply {
		result.Applied = []ImportBackfillItem{}
		result.Failed = []ImportBackfillItem{}
	}

	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		class, reason, parentKey, classifyErr := s.classifyImportBackfillJob(ctx, candidate.JobID)
		if classifyErr != nil {
			return nil, classifyErr
		}
		item := ImportBackfillItem{
			JobID:     candidate.JobID,
			CreatedAt: candidate.CreatedAt,
			Reason:    reason,
			ParentKey: parentKey,
		}
		switch class {
		case importBackfillWouldImport:
			result.Summary.WouldImport++
			result.WouldImport = append(result.WouldImport, item)
		case importBackfillAlreadyOwned:
			result.Summary.AlreadyOwned++
			result.AlreadyOwned = append(result.AlreadyOwned, item)
		case importBackfillExpectedFail:
			result.Summary.ExpectedFail++
			result.ExpectedFail = append(result.ExpectedFail, item)
		}

		if !req.Apply {
			continue
		}

		switch class {
		case importBackfillExpectedFail:
			item.Error = reason
			result.Summary.Failed++
			result.Failed = append(result.Failed, item)
			continue
		case importBackfillAlreadyOwned:
			status, appliedParent, attachmentKey, applyErr := importer.PlanAndApply(ctx, candidate.JobID)
			s.recordAutoImportEvent(ctx, candidate.JobID, status, appliedParent, attachmentKey, applyErr)
			applied := ImportBackfillItem{
				JobID:     candidate.JobID,
				CreatedAt: candidate.CreatedAt,
				Status:    status,
				ParentKey: appliedParent,
			}
			if applyErr != nil {
				applied.Status = "error"
				applied.Error = SanitizeErrorHint(applyErr.Error())
				if applied.Error == "" {
					applied.Error = ErrorInfoFrom(applyErr).Hint
				}
				result.Summary.Failed++
				result.Failed = append(result.Failed, applied)
				continue
			}
			result.Summary.Applied++
			result.Applied = append(result.Applied, applied)
			continue
		}

		status, appliedParent, attachmentKey, applyErr := importer.PlanAndApply(ctx, candidate.JobID)
		s.recordAutoImportEvent(ctx, candidate.JobID, status, appliedParent, attachmentKey, applyErr)
		applied := ImportBackfillItem{
			JobID:     candidate.JobID,
			CreatedAt: candidate.CreatedAt,
			Status:    status,
			ParentKey: appliedParent,
		}
		if applyErr != nil {
			applied.Status = "error"
			applied.Error = SanitizeErrorHint(applyErr.Error())
			if applied.Error == "" {
				applied.Error = ErrorInfoFrom(applyErr).Hint
			}
			result.Summary.Failed++
			result.Failed = append(result.Failed, applied)
			continue
		}
		result.Summary.Applied++
		result.Applied = append(result.Applied, applied)
	}

	result.Summary.Selected = len(candidates)
	if len(candidates) > 0 {
		result.Cursor = candidates[len(candidates)-1].JobID
	}
	return result, nil
}

type importBackfillClass int

const (
	importBackfillWouldImport importBackfillClass = iota
	importBackfillAlreadyOwned
	importBackfillExpectedFail
)

type importBackfillCandidate struct {
	JobID     string
	CreatedAt string
}

func effectiveImportBackfillLimit(requested int) int {
	if requested <= 0 {
		return ImportBackfillLimitDefault
	}
	if requested > importBackfillLimitMax {
		return importBackfillLimitMax
	}
	return requested
}

func (s *Service) listImportBackfillCandidates(ctx context.Context, includeNotRequested bool, cursor string, limit int) ([]importBackfillCandidate, bool, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `
		SELECT j.id, j.created_at
		FROM jobs j
		WHERE j.state = 'ready'
		  AND EXISTS (
			SELECT 1
			FROM job_artifacts ja
			WHERE ja.job_id = j.id
			  AND ja.identity_result IN ('pass', 'user_confirmed')
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM events e
			WHERE e.job_id = j.id
			  AND e.kind = 'zotio.auto_import'
			  AND json_extract(e.detail_json, '$.status') IN ('applied', 'no_op', 'duplicate')
			  AND e.seq = (
				SELECT MAX(e2.seq)
				FROM events e2
				WHERE e2.job_id = j.id AND e2.kind = 'zotio.auto_import'
			  )
		  )
		  AND (
			json_extract(j.policy_json, '$.auto_import') = 1
			OR ? = 1
		  )
		  AND (
			? = ''
			OR (j.created_at, j.id) > (
				SELECT created_at, id FROM jobs WHERE id = ?
			)
		  )
		ORDER BY j.created_at ASC, j.id ASC
		LIMIT ?`,
		boolInt(includeNotRequested), cursor, cursor, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	candidates := make([]importBackfillCandidate, 0, limit+1)
	for rows.Next() {
		var candidate importBackfillCandidate
		if err := rows.Scan(&candidate.JobID, &candidate.CreatedAt); err != nil {
			return nil, false, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(candidates) > limit
	if truncated {
		candidates = candidates[:limit]
	}
	return candidates, truncated, nil
}

func (s *Service) countImportBackfillExcluded(ctx context.Context, includeNotRequested bool) (int, error) {
	if includeNotRequested {
		return 0, nil
	}
	var count int
	err := s.Store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM jobs j
		WHERE j.state = 'ready'
		  AND EXISTS (
			SELECT 1
			FROM job_artifacts ja
			WHERE ja.job_id = j.id
			  AND ja.identity_result IN ('pass', 'user_confirmed')
		  )
		  AND COALESCE(json_extract(j.policy_json, '$.auto_import'), 0) != 1
		  AND NOT EXISTS (
			SELECT 1
			FROM events e
			WHERE e.job_id = j.id
			  AND e.kind = 'zotio.auto_import'
			  AND json_extract(e.detail_json, '$.status') IN ('applied', 'no_op', 'duplicate')
			  AND e.seq = (
				SELECT MAX(e2.seq)
				FROM events e2
				WHERE e2.job_id = j.id AND e2.kind = 'zotio.auto_import'
			  )
		  )`).Scan(&count)
	return count, err
}

func (s *Service) classifyImportBackfillJob(ctx context.Context, jobID string) (class importBackfillClass, reason, parentKey string, err error) {
	row, err := s.Bundle.Jobs.Get(ctx, jobID)
	if err != nil {
		return importBackfillExpectedFail, "", "", err
	}
	if parentKey, owned := s.peekOwnedReadyImport(ctx, *row); owned {
		return importBackfillAlreadyOwned, "already_in_library", parentKey, nil
	}
	if _, _, exportErr := s.Bundle.Export(ctx, jobID, ""); exportErr != nil {
		info := ErrorInfoFrom(exportErr)
		reason = info.Hint
		if reason == "" {
			reason = SanitizeErrorHint(exportErr.Error())
		}
		return importBackfillExpectedFail, reason, "", nil
	}
	if strings.TrimSpace(row.ZotioItemKey) == "" {
		lookup := LookupWork{DOI: row.Work.DOI, ArXiv: row.Work.ArXiv, PMID: row.Work.PMID}
		if lookup.DOI == "" && lookup.ArXiv == "" && lookup.PMID == "" {
			return importBackfillExpectedFail, "new-item Zotio routing requires a DOI", "", nil
		}
	}
	return importBackfillWouldImport, "", "", nil
}

func (s *Service) peekOwnedReadyImport(ctx context.Context, row job.Row) (parentKey string, owned bool) {
	if s == nil || s.CLI == nil || strings.TrimSpace(row.ZotioItemKey) != "" {
		return "", false
	}
	lookup := LookupWork{DOI: row.Work.DOI, ArXiv: row.Work.ArXiv, PMID: row.Work.PMID}
	if lookup.DOI == "" && lookup.ArXiv == "" && lookup.PMID == "" {
		return "", false
	}
	result, err := s.LookupWorks(ctx, LookupWorksRequest{Works: []LookupWork{lookup}})
	if err != nil || len(result.Works) != 1 || result.Works[0].Status != OwnershipOwnedWithPDF {
		return "", false
	}
	return result.Works[0].ItemKey, true
}

func (s *Service) recordAutoImportEvent(ctx context.Context, jobID, status, parentKey, attachmentKey string, err error) {
	if s == nil || s.Bundle == nil || s.Bundle.Jobs == nil {
		return
	}
	eventCtx := context.WithoutCancel(ctx)
	detail := map[string]any{"parent_key": parentKey, "attachment_key": attachmentKey}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		info := ErrorInfoFrom(err)
		detail["status"] = "error"
		detail["error_type"] = fmt.Sprintf("%T", err)
		detail["error_class"] = info.Class
		if info.Hint != "" {
			detail["error_hint"] = info.Hint
		}
		if info.HTTPStatus != 0 {
			detail["error_http_status"] = info.HTTPStatus
		}
		message := SanitizeErrorHint(err.Error())
		if message != "" {
			detail["error_message"] = message
			if info.Hint == "" {
				detail["error_hint"] = message
			}
		}
	} else {
		detail["status"] = status
		if status == "duplicate" {
			detail["reason"] = "already_in_library"
		}
	}
	_ = s.Bundle.Jobs.RecordEvent(eventCtx, jobID, "zotio.auto_import", detail)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ValidateImportBackfillCursor reports whether a cursor still anchors a job row.
func (s *Service) ValidateImportBackfillCursor(ctx context.Context, cursor string) error {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return nil
	}
	if !strings.HasPrefix(cursor, "job_") {
		return fmt.Errorf("invalid import-backfill cursor %q", cursor)
	}
	var id string
	err := s.Store.DB().QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = ?`, cursor).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("import-backfill cursor %q not found", cursor)
	}
	return err
}
