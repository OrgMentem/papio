// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/batch"
	"papio/internal/config"
	"papio/internal/ownership"
	"papio/internal/ownershipsnapshot"
	"papio/internal/protocol"
	"papio/internal/store"
)

// OnceSubmitter keeps the request-to-job receipt in the same transaction as
// creation or convergence, including when the chosen job later becomes terminal.
type OnceSubmitter interface {
	SubmitOnceWithAutoImport(context.Context, protocol.WorkRequest, *bool) (string, error)
}

// GenericBackfillInput deliberately belongs to a new method, not watch.add.
type GenericBackfillInput struct {
	SourceName   string `json:"source_name"`
	Label        string `json:"label,omitempty"`
	CadenceHours int    `json:"cadence_hours"`
	PerRunCap    int    `json:"per_run_cap"`
}

type GenericBackfillWatch struct {
	Watch      Watch  `json:"watch"`
	SourceName string `json:"source_name"`
}

func (r *Runner) recordSource(name string) (config.LibrarySource, error) {
	for _, source := range r.LibrarySources {
		if source.Name != name {
			continue
		}
		if source.Claim != config.LibraryClaimRecordPresent {
			return config.LibrarySource{}, errors.New("backfill source must explicitly declare record_present")
		}
		return source, nil
	}
	return config.LibrarySource{}, fmt.Errorf("backfill library source %q is not configured", name)
}

func (r *Runner) AddGenericBackfill(ctx context.Context, input GenericBackfillInput) (*GenericBackfillWatch, error) {
	if r == nil || r.Store == nil || !r.holdingsEnabled() {
		return nil, errors.New("generic backfill requires configured library holdings")
	}
	input.SourceName = strings.TrimSpace(input.SourceName)
	if len(input.SourceName) < 1 || len(input.SourceName) > 128 {
		return nil, errors.New("source_name must contain 1-128 bytes")
	}
	if _, err := r.recordSource(input.SourceName); err != nil {
		return nil, err
	}
	creation, err := normalizeCreateInput(CreateInput{Kind: KindBackfill, Mode: ModeAcquire, Label: input.Label, CadenceHours: input.CadenceHours, PerRunCap: input.PerRunCap})
	if err != nil {
		return nil, err
	}
	tx, err := r.Store.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO watches(label,kind,mode,query,filters_json,collection,cadence_hours,per_run_cap,enabled,created_at) VALUES(?,?,?,'','{}','',?,?,1,?)`, creation.Label, creation.Kind, creation.Mode, creation.CadenceHours, creation.PerRunCap, store.Now())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO watch_backfill_sources(watch_id,source_name) VALUES(?,?)`, id, input.SourceName); err != nil {
		return nil, err
	}
	created, err := scanWatch(tx.QueryRowContext(ctx, watchSelect+` WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &GenericBackfillWatch{Watch: *created, SourceName: input.SourceName}, nil
}

func (s *Store) genericBackfillSource(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.S.DB().QueryRowContext(ctx, `SELECT source_name FROM watch_backfill_sources WHERE watch_id=?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

func (r *Runner) executeGenericBackfill(ctx context.Context, watched Watch, sourceName string, at time.Time) (*RunResult, error) {
	result := &RunResult{WatchID: watched.ID}
	if !r.holdingsEnabled() || r.Submitter == nil {
		return result, errors.New("generic backfill dependencies are not configured")
	}
	submitter, ok := r.Submitter.(OnceSubmitter)
	if !ok {
		return result, errors.New("generic backfill requires durable once-only submission")
	}
	source, err := r.recordSource(sourceName)
	if err != nil {
		return result, err
	}
	records, err := ownershipsnapshot.EnumerateBibliographicRecords(ctx, source)
	if err != nil {
		return result, err
	}
	works := make([]protocol.WorkRequest, 0, len(records))
	seen := make(map[string]bool, len(records))
	for i, record := range records {
		// No fetchable exact identifier means no safe automated acquisition.
		if !record.HasIdentifier() {
			continue
		}
		data, err := json.Marshal(map[string]any{"doi": record.DOI, "pmid": record.PMID, "arxiv": record.ArXiv, "title": record.Title, "authors": record.Authors, "year": record.Year})
		if err != nil {
			return result, err
		}
		request, err := batch.ParseWork(data)
		if err != nil {
			return result, fmt.Errorf("backfill record %d: %w", i+1, err)
		}
		request.RequestID = fmt.Sprintf("backfill-%d-%s", watched.ID, strings.TrimPrefix(request.RequestID, "batch-"))
		// Non-Zotero automation never requests Zotio import or a Zotio collection.
		request.ZotioItemKey, request.Collection = "", ""
		if !seen[request.RequestID] {
			seen[request.RequestID] = true
			works = append(works, request)
		}
	}
	queries := make([]ownership.Query, len(works))
	for i, request := range works {
		queries[i] = ownership.QueryFor(request.Identifiers.DOI, request.Identifiers.ArXiv, request.Identifiers.PMID, "any", "")
	}
	lookup := r.Holdings.Lookup(ctx, queries)
	if len(lookup.Sources) == 0 || !lookup.Complete() || len(lookup.Works) != len(works) {
		return result, errors.New("backfill ownership is incomplete; nothing was submitted")
	}
	for _, health := range lookup.Sources {
		if health.Stale {
			return result, errors.New("backfill ownership is stale; nothing was submitted")
		}
	}
	sourceConfirmed := false
	healthNames := make(map[string]bool, len(lookup.Sources))
	for _, health := range lookup.Sources {
		if healthNames[health.Name] || health.Name == "" {
			return result, errors.New("backfill source health is ambiguous; nothing was submitted")
		}
		healthNames[health.Name] = true
		if health.Name == sourceName {
			sourceConfirmed = true
		}
	}
	if !sourceConfirmed {
		return result, errors.New("backfill ownership omits the declared source; nothing was submitted")
	}
	// Validate every result before making a receipt or submitting anything.
	eligible := make([]protocol.WorkRequest, 0, len(works))
	for i, request := range works {
		present, pdf := false, false
		for _, claim := range lookup.Works[i].Claims {
			matched := false
			for _, id := range queries[i].Identifiers {
				if claim.Matched == id {
					matched = true
				}
			}
			if !matched || claim.Stale || !healthNames[claim.Source] || (claim.Artifact != ownership.ArtifactUnknown && claim.Artifact != ownership.ArtifactMissing && claim.Artifact != ownership.ArtifactPresent) {
				return result, errors.New("backfill ownership is ambiguous; nothing was submitted")
			}
			if claim.Source == sourceName && claim.RecordPresent {
				present = true
			}
			if claim.Artifact == ownership.ArtifactPresent {
				pdf = true
			}
		}
		if !present {
			return result, fmt.Errorf("backfill source %q does not confirm record %d; nothing was submitted", sourceName, i+1)
		}
		if !pdf {
			eligible = append(eligible, request)
		}
	}
	for _, request := range eligible {
		var jobID string
		err := r.Store.S.DB().QueryRowContext(ctx, `SELECT COALESCE(job_id,'') FROM watch_backfill_submissions WHERE watch_id=? AND work_key=?`, watched.ID, request.RequestID).Scan(&jobID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		if jobID != "" {
			continue
		}
		if result.Queued >= watched.PerRunCap {
			break
		}
		if errors.Is(err, sql.ErrNoRows) {
			data, err := json.Marshal(request)
			if err != nil {
				return result, err
			}
			if _, err := r.Store.S.DB().ExecContext(ctx, `INSERT INTO watch_backfill_submissions(watch_id,work_key,request_id,work_json,created_at) VALUES(?,?,?,?,?)`, watched.ID, request.RequestID, request.RequestID, string(data), store.FormatTime(at)); err != nil {
				return result, err
			}
		} else {
			var document string
			if err := r.Store.S.DB().QueryRowContext(ctx, `SELECT work_json FROM watch_backfill_submissions WHERE request_id=?`, request.RequestID).Scan(&document); err != nil {
				return result, err
			}
			if err := json.Unmarshal([]byte(document), &request); err != nil {
				return result, err
			}
		}
		// The submission seam atomically owns crash and convergence recovery.
		autoImport := false
		jobID, err = submitter.SubmitOnceWithAutoImport(ctx, request, &autoImport)
		if err != nil {
			return result, err
		}
		if jobID == "" {
			return result, errors.New("backfill submission returned no job ID")
		}
		if _, err := r.Store.S.DB().ExecContext(ctx, `UPDATE watch_backfill_submissions SET job_id=? WHERE request_id=?`, jobID, request.RequestID); err != nil {
			return result, err
		}
		result.Queued++
	}
	return result, r.Store.MarkRun(ctx, watched.ID, at)
}
