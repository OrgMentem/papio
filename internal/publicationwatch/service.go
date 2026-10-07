// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package publicationwatch monitors acquired non-published manifestations.
// Its notices never replace or attach to the original artifact.
package publicationwatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"papio/internal/batch"
	"papio/internal/enrich"
	"papio/internal/notify"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/work"
)

const MaxWatches = 50

// Relations preserves both relation type and direction from the source.
type Relations interface {
	PublicationRelations(context.Context, string) ([]enrich.PublicationRelation, error)
}

type Submitter interface {
	SubmitOnceWithAutoImport(context.Context, protocol.WorkRequest, *bool) (string, error)
}

type Service struct {
	Store     *store.Store
	Relations Relations
	Submitter Submitter
	Notifier  notify.Sink
	Now       func() time.Time
	mu        sync.Mutex
}

func NewService(s *store.Store, relations Relations, submitter Submitter, notifier notify.Sink) *Service {
	return &Service{Store: s, Relations: relations, Submitter: submitter, Notifier: notifier}
}

type AddInput struct {
	JobID        string `json:"job_id"`
	CadenceHours int    `json:"cadence_hours"`
}

type Watch struct {
	ID              int64  `json:"id"`
	JobID           string `json:"job_id"`
	SourceDOI       string `json:"source_doi"`
	ArtifactSHA256  string `json:"artifact_sha256"`
	ArtifactVersion string `json:"artifact_version"`
	CadenceHours    int    `json:"cadence_hours"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
}

type Notice struct {
	ID            int64  `json:"id"`
	WatchID       int64  `json:"watch_id"`
	SourceDOI     string `json:"source_doi"`
	TargetDOI     string `json:"target_doi"`
	RelationType  string `json:"relation_type"`
	Provider      string `json:"provider"`
	FirstSeenAt   string `json:"first_seen_at"`
	AcquiredJobID string `json:"acquired_job_id,omitempty"`
}

type RunResult struct {
	WatchID    int64 `json:"watch_id"`
	NewNotices int   `json:"new_notices"`
}

type Acquisition struct {
	NoticeID       int64  `json:"notice_id"`
	JobID          string `json:"job_id"`
	TargetDOI      string `json:"target_doi"`
	OriginalSHA256 string `json:"original_sha256"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) configured() error {
	if s == nil || s.Store == nil {
		return errors.New("publication watches are not configured")
	}
	return nil
}

const watchSelect = `SELECT id,job_id,source_doi,artifact_sha256,artifact_version,cadence_hours,enabled,created_at,COALESCE(last_run_at,''),last_error FROM publication_watches`
const noticeSelect = `SELECT id,watch_id,source_doi,target_doi,relation_type,provider,first_seen_at,COALESCE(acquired_job_id,'') FROM publication_notices`

type scanner interface{ Scan(...any) error }

func scanWatch(row scanner) (*Watch, error) {
	var w Watch
	err := row.Scan(&w.ID, &w.JobID, &w.SourceDOI, &w.ArtifactSHA256, &w.ArtifactVersion, &w.CadenceHours, &w.Enabled, &w.CreatedAt, &w.LastRunAt, &w.LastError)
	return &w, err
}
func scanNotice(row scanner) (*Notice, error) {
	var n Notice
	err := row.Scan(&n.ID, &n.WatchID, &n.SourceDOI, &n.TargetDOI, &n.RelationType, &n.Provider, &n.FirstSeenAt, &n.AcquiredJobID)
	return &n, err
}

func (s *Service) Add(ctx context.Context, input AddInput) (*Watch, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if s.Relations == nil {
		return nil, errors.New("typed publication relation source is not configured")
	}
	input.JobID = strings.TrimSpace(input.JobID)
	if input.JobID == "" || input.CadenceHours < 1 || input.CadenceHours > 87600 {
		return nil, errors.New("job_id and cadence_hours (1-87600) are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var doi, sha, version string
	// Evidence belongs to the acquisition edge, not the shared artifacts row.
	// Never treat a requested version, a title, or an untyped sibling as proof.
	err = tx.QueryRowContext(ctx, `SELECT i.value,j.artifact_sha256,c.version
	 FROM jobs j JOIN identifiers i ON i.work_request_id=j.work_request_id AND i.kind='doi'
	 JOIN candidates c ON c.id=j.selected_candidate_id AND c.job_id=j.id
	 JOIN job_artifacts ja ON ja.job_id=j.id AND ja.artifact_sha256=j.artifact_sha256 AND ja.role='main' AND ja.candidate_id=c.id
	 JOIN artifacts a ON a.sha256=ja.artifact_sha256
	 JOIN validation_reports vr ON vr.job_id=j.id AND vr.candidate_id=c.id AND vr.sha256=ja.artifact_sha256
	 WHERE j.id=? AND j.state IN ('ready','imported') AND c.status='accepted' AND c.version IN ('preprint','accepted')
	 AND ja.identity_result='pass' AND vr.outcome='pass' AND a.mime='application/pdf' AND a.size_bytes>0`, input.JobID).Scan(&doi, &sha, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("publication opt-in requires a DOI-bearing acquired PDF with passed validation and selected preprint/accepted evidence")
	}
	if err != nil {
		return nil, err
	}
	doi, err = work.NormalizeDOI(doi)
	if err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM publication_watches WHERE job_id<>?`, input.JobID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= MaxWatches {
		return nil, fmt.Errorf("publication watches are capped at %d", MaxWatches)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO publication_watches(job_id,source_doi,artifact_sha256,artifact_version,cadence_hours,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(job_id) DO NOTHING`, input.JobID, doi, sha, version, input.CadenceHours, store.FormatTime(s.now())); err != nil {
		return nil, err
	}
	w, err := scanWatch(tx.QueryRowContext(ctx, watchSelect+` WHERE job_id=?`, input.JobID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return w, nil
}

func (s *Service) List(ctx context.Context) ([]Watch, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.Store.DB().QueryContext(ctx, watchSelect+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Watch, 0)
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *Service) Notices(ctx context.Context, watchID int64) ([]Notice, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if watchID <= 0 {
		return nil, errors.New("publication watch id must be positive")
	}
	rows, err := s.Store.DB().QueryContext(ctx, noticeSelect+` WHERE watch_id=? ORDER BY id`, watchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Notice, 0)
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *Service) Pause(ctx context.Context, id int64) error {
	if err := s.configured(); err != nil {
		return err
	}
	if id <= 0 {
		return errors.New("publication watch id must be positive")
	}
	result, err := s.Store.DB().ExecContext(ctx, `UPDATE publication_watches SET enabled=0 WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Service) Run(ctx context.Context, id int64) (*RunResult, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errors.New("publication watch id must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, err := scanWatch(s.Store.DB().QueryRowContext(ctx, watchSelect+` WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	result, err := s.run(ctx, *w)
	if err != nil {
		recorded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, recordErr := s.Store.DB().ExecContext(recorded, `UPDATE publication_watches SET last_run_at=?,last_error=? WHERE id=?`, store.FormatTime(s.now()), boundedError(err), id)
		if recordErr != nil {
			err = errors.Join(err, recordErr)
		}
	}
	return result, err
}

func (s *Service) RunDue(ctx context.Context) error {
	watches, err := s.List(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, w := range watches {
		if !w.Enabled {
			continue
		}
		if w.LastRunAt != "" {
			last, err := time.Parse(time.RFC3339Nano, w.LastRunAt)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if s.now().Sub(last) < time.Duration(w.CadenceHours)*time.Hour {
				continue
			}
		}
		if _, err := s.Run(ctx, w.ID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) run(ctx context.Context, w Watch) (*RunResult, error) {
	result := &RunResult{WatchID: w.ID}
	if s.Relations == nil {
		return result, errors.New("typed publication relation source is not configured")
	}
	relations, err := s.Relations.PublicationRelations(ctx, w.SourceDOI)
	if err != nil {
		return result, err
	}
	if len(relations) > 10 {
		return result, errors.New("publication relation result exceeds the bounded limit")
	}
	verified := make([]enrich.PublicationRelation, 0, len(relations))
	for _, r := range relations {
		if r.Type != "is-preprint-of" {
			continue
		}
		source, sourceErr := work.NormalizeDOI(r.SourceDOI)
		target, targetErr := work.NormalizeDOI(r.TargetDOI)
		if sourceErr != nil || targetErr != nil || source != w.SourceDOI || target == source || strings.TrimSpace(r.Provider) == "" {
			return result, errors.New("publication relation has invalid directional evidence")
		}
		r.SourceDOI, r.TargetDOI = source, target
		verified = append(verified, r)
	}
	at := store.FormatTime(s.now())
	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range verified {
		// A target creates one durable notice, even across overlapping opt-ins.
		insert, err := tx.ExecContext(ctx, `INSERT INTO publication_notices(watch_id,source_doi,target_doi,relation_type,provider,first_seen_at) SELECT ?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM publication_notices WHERE target_doi=?)`, w.ID, r.SourceDOI, r.TargetDOI, r.Type, r.Provider, at, r.TargetDOI)
		if err != nil {
			return result, err
		}
		count, err := insert.RowsAffected()
		if err != nil {
			return result, err
		}
		result.NewNotices += int(count)
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	if err := s.deliver(ctx, w.ID); err != nil {
		return result, err
	}
	_, err = s.Store.DB().ExecContext(ctx, `UPDATE publication_watches SET last_run_at=?,last_error='' WHERE id=?`, at, w.ID)
	return result, err
}

func (s *Service) deliver(ctx context.Context, id int64) error {
	if s.Notifier == nil {
		return nil
	} // Durable notices remain available through the API and CLI.
	notices, err := s.Notices(ctx, id)
	if err != nil {
		return err
	}
	for _, n := range notices {
		var delivered string
		if err := s.Store.DB().QueryRowContext(ctx, `SELECT COALESCE(notified_at,'') FROM publication_notices WHERE id=?`, n.ID).Scan(&delivered); err != nil {
			return err
		}
		if delivered != "" {
			continue
		}
		first, err := time.Parse(time.RFC3339Nano, n.FirstSeenAt)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("publication-notice:%d", n.ID)
		var routed int
		// The durable router row closes the crash window before notified_at.
		if err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_intents WHERE event_kind='publication.available' AND aggregate_key=?`, key).Scan(&routed); err != nil {
			return err
		}
		if routed == 0 {
			message := fmt.Sprintf("Published version available: %s (original %s remains unchanged)", n.TargetDOI, n.SourceDOI)
			if err := s.Notifier.Route(ctx, notify.Intent{EventKind: "publication.available", Category: notify.CategoryIntegrityNotice, AggregateKey: key, Phase: notify.PhaseScan, WindowStart: first, HappenedAt: first, Message: message, Detail: notify.Event{Kind: "publication.available", Message: message, Count: 1, Detail: map[string]any{"notice_id": n.ID, "source_doi": n.SourceDOI, "target_doi": n.TargetDOI, "relation_type": n.RelationType, "provider": n.Provider}}}); err != nil {
				return err
			}
		}
		if _, err := s.Store.DB().ExecContext(ctx, `UPDATE publication_notices SET notified_at=? WHERE id=?`, store.FormatTime(s.now()), n.ID); err != nil {
			return err
		}
	}
	return nil
}

// Acquire is explicit, target-specific, and never files over the source copy.
func (s *Service) Acquire(ctx context.Context, noticeID int64) (*Acquisition, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if noticeID <= 0 || s.Submitter == nil {
		return nil, errors.New("notice id and acquisition service are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := scanNotice(s.Store.DB().QueryRowContext(ctx, noticeSelect+` WHERE id=?`, noticeID))
	if err != nil {
		return nil, err
	}
	var original string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT artifact_sha256 FROM publication_watches WHERE id=?`, n.WatchID).Scan(&original); err != nil {
		return nil, err
	}
	result := &Acquisition{NoticeID: n.ID, JobID: n.AcquiredJobID, TargetDOI: n.TargetDOI, OriginalSHA256: original}
	if result.JobID != "" {
		return result, nil
	}
	request := protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, Identifiers: &protocol.Identifiers{DOI: n.TargetDOI}, DesiredVersion: "published"}
	request.RequestID = batch.RequestID(fmt.Sprintf("publication-%d", n.ID), request)
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.Store.DB().ExecContext(ctx, `UPDATE publication_notices SET request_id=? WHERE id=?`, request.RequestID, n.ID); err != nil {
		return nil, err
	}
	autoImport := false
	result.JobID, err = s.Submitter.SubmitOnceWithAutoImport(ctx, request, &autoImport)
	if err != nil {
		return nil, err
	}
	if result.JobID == "" {
		return nil, errors.New("publication acquisition returned no job id")
	}
	if _, err := s.Store.DB().ExecContext(ctx, `UPDATE publication_notices SET acquired_job_id=? WHERE id=?`, result.JobID, n.ID); err != nil {
		return nil, err
	}
	return result, nil
}

func boundedError(err error) string {
	value := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\n", " "), "\r", " ")
	if len(value) > 500 {
		return value[:500]
	}
	return value
}
