// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package triage builds the daemon-owned inbox read model.
package triage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/watch"
	"papio/internal/work"
)

const (
	SchemaVersion = 1
	defaultLimit  = 50
	maxLimit      = 100

	retractionRankBase  = 0
	humanActionRankBase = 1_000_000
	watchHitRankBase    = 2_000_000
	pdfGrabRankBase     = 3_000_000
)

const (
	KindWatchHit    = "watch_hit"
	KindHumanAction = "human_action"
	KindRetraction  = "retraction"
	KindPdfGrab     = "pdf_grab"
	PdfGrabIDPrefix = KindPdfGrab + ":"

	// RetractionIDPrefix opens the item ID of every retraction notice; the rest
	// of the ID is the normalized DOI of the work the notice concerns.
	RetractionIDPrefix = KindRetraction + ":"
)

// PdfGrab carries the durable pre-identity grab state. It deliberately has no
// job or action identity: a canonical job is created only after an identifier
// is supplied.
type PdfGrab struct {
	GrabID string `json:"grab_id"`
	State  string `json:"state"`
}

// Fact is bounded display-only metadata for an inbox item.
type Fact struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Link is a daemon-derived canonical destination.
type Link struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

// Work is the immutable identity details of a watch hit.
type Work struct {
	DOI     string `json:"doi"`
	Title   string `json:"title"`
	Authors string `json:"authors"`
	Year    int    `json:"year"`
	IsOA    bool   `json:"is_oa"`
}

// Watch identifies one watch that surfaced a grouped work. WorkKey is internal
// mutation input and deliberately never appears in a snapshot frame.
type Watch struct {
	ID      int64  `json:"id"`
	Label   string `json:"label"`
	WorkKey string `json:"-"`
}

// WatchHit carries the watch-hit-specific portion of an Item.
type WatchHit struct {
	Work        Work    `json:"work"`
	Abstract    string  `json:"abstract"`
	Watches     []Watch `json:"watches"`
	FirstSeenAt string  `json:"first_seen_at"`

	arXiv    string
	openAlex string
}

// HumanAction carries fields needed to display and safely resolve a human
// action. Quarantine paths and candidate IDs never leave the daemon.
type HumanAction struct {
	ActionID        int64  `json:"action_id"`
	JobID           string `json:"job_id"`
	ActionKind      string `json:"action_kind"`
	JobState        string `json:"job_state"`
	Revision        int64  `json:"revision"`
	SHA256          string `json:"sha256"`
	SizeBytes       int64  `json:"size_bytes"`
	RequiresAuth    *bool  `json:"requires_auth,omitempty"`
	BlockedBy       string `json:"blocked_by,omitempty"`
	DiagnosisReason string `json:"-"`
	DeliveryState   string `json:"-"`
}

// Retraction carries the retraction-specific portion of an Item.
type Retraction struct {
	DOI       string    `json:"doi"`
	Nature    string    `json:"nature"`
	NoticedAt time.Time `json:"noticed_at"`
	NoticeDOI string    `json:"notice_doi"`
}

// Item is a schema-v1 inbox item. Exactly one kind-specific field is set for
// each supported Kind. PdfGrab is emitted only when the browser negotiates
// triage-snapshot/4.
type Item struct {
	Kind  string   `json:"kind"`
	ID    string   `json:"id"`
	Rank  int      `json:"rank"`
	Title string   `json:"title"`
	Facts []Fact   `json:"facts"`
	Links []Link   `json:"links"`
	Ops   []string `json:"ops"`

	WatchHit    *WatchHit         `json:"-"`
	HumanAction *HumanAction      `json:"-"`
	Retraction  *Retraction       `json:"-"`
	PdfGrab     *PdfGrab          `json:"-"`
	Family      *FamilyAssignment `json:"-"`
}

// MarshalJSON emits exactly one supported kind-specific object.
func (item Item) MarshalJSON() ([]byte, error) {
	if item.Kind == KindPdfGrab {
		return json.Marshal(struct {
			Kind       string   `json:"kind"`
			Label      string   `json:"label"`
			Grab       *PdfGrab `json:"grab"`
			RouteClass string   `json:"route_class"`
			BlockedBy  string   `json:"blocked_by"`
			Attention  string   `json:"attention"`
			Ops        []string `json:"ops"`
		}{item.Kind, item.Title, item.PdfGrab, "pdf_identifier_needed", "identifier_missing", "required", item.Ops})
	}

	type core struct {
		Kind  string   `json:"kind"`
		ID    string   `json:"id"`
		Rank  int      `json:"rank"`
		Title string   `json:"title"`
		Facts []Fact   `json:"facts"`
		Links []Link   `json:"links"`
		Ops   []string `json:"ops"`
	}
	payload := struct {
		core
		*WatchHit
		*HumanAction
		*Retraction
	}{
		core:     core{Kind: item.Kind, ID: item.ID, Rank: item.Rank, Title: item.Title, Facts: item.Facts, Links: item.Links, Ops: item.Ops},
		WatchHit: item.WatchHit, HumanAction: item.HumanAction, Retraction: item.Retraction,
	}
	return json.Marshal(payload)
}

// UnmarshalJSON accepts the exact schema-v1 item envelope emitted by
// MarshalJSON. It deliberately rejects unknown fields so IPC consumers fail
// closed rather than silently misrender a newer schema.
func (item *Item) UnmarshalJSON(data []byte) error {
	var wire struct {
		Kind       string   `json:"kind"`
		ID         string   `json:"id"`
		Rank       int      `json:"rank"`
		Title      string   `json:"title"`
		Facts      []Fact   `json:"facts"`
		Links      []Link   `json:"links"`
		Ops        []string `json:"ops"`
		Label      string   `json:"label"`
		Grab       *PdfGrab `json:"grab"`
		RouteClass string   `json:"route_class"`
		Attention  string   `json:"attention"`

		Work        *Work   `json:"work"`
		Abstract    string  `json:"abstract"`
		Watches     []Watch `json:"watches"`
		FirstSeenAt string  `json:"first_seen_at"`

		ActionID     int64  `json:"action_id"`
		JobID        string `json:"job_id"`
		ActionKind   string `json:"action_kind"`
		JobState     string `json:"job_state"`
		Revision     int64  `json:"revision"`
		SHA256       string `json:"sha256"`
		SizeBytes    int64  `json:"size_bytes"`
		RequiresAuth *bool  `json:"requires_auth"`
		BlockedBy    string `json:"blocked_by"`

		DOI       string    `json:"doi"`
		Nature    string    `json:"nature"`
		NoticedAt time.Time `json:"noticed_at"`
		NoticeDOI string    `json:"notice_doi"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("triage item has trailing data")
	}
	*item = Item{Kind: wire.Kind, ID: wire.ID, Rank: wire.Rank, Title: wire.Title, Facts: wire.Facts, Links: wire.Links, Ops: wire.Ops}
	if wire.Kind == KindPdfGrab {
		if wire.Label == "" || wire.Grab == nil || wire.Grab.GrabID == "" || wire.Grab.State == "" ||
			wire.RouteClass != "pdf_identifier_needed" || wire.BlockedBy != "identifier_missing" ||
			wire.Attention != "required" || len(wire.Ops) != 2 || wire.Ops[0] != "provide_identifier" || wire.Ops[1] != "dismiss" {
			return errors.New("invalid pdf grab item")
		}
		item.Title = wire.Label
		item.PdfGrab = wire.Grab
		return nil
	}
	switch wire.Kind {
	case KindWatchHit:
		if wire.Work == nil || len(wire.Watches) == 0 || wire.FirstSeenAt == "" {
			return errors.New("invalid watch hit item")
		}
		item.WatchHit = &WatchHit{Work: *wire.Work, Abstract: wire.Abstract, Watches: wire.Watches, FirstSeenAt: wire.FirstSeenAt}
	case KindHumanAction:
		if wire.ActionID <= 0 || wire.JobID == "" || wire.ActionKind == "" || wire.JobState == "" || wire.Revision <= 0 {
			return errors.New("invalid human action item")
		}
		item.HumanAction = &HumanAction{
			ActionID: wire.ActionID, JobID: wire.JobID, ActionKind: wire.ActionKind, JobState: wire.JobState,
			Revision: wire.Revision, SHA256: wire.SHA256, SizeBytes: wire.SizeBytes,
			RequiresAuth: wire.RequiresAuth, BlockedBy: wire.BlockedBy,
		}
	case KindRetraction:
		if wire.DOI == "" || wire.Nature == "" || wire.NoticedAt.IsZero() {
			return errors.New("invalid retraction item")
		}
		item.Retraction = &Retraction{DOI: wire.DOI, Nature: wire.Nature, NoticedAt: wire.NoticedAt, NoticeDOI: wire.NoticeDOI}
	default:
		return errors.New("unsupported triage item kind")
	}
	return nil
}

type FamilyAssignment struct {
	RunKey           string
	NextActor        string
	GuidanceVariant  string
	OperationVariant string
	RouteClass       string
	ActionKind       string
	DependentJobs    int
	GateClaimID      string
}

type FamilyRun struct {
	RunKey, RouteClass, ActionKind, NextActor, GuidanceVariant, OperationVariant string
	FirstRank, Count                                                             int
}

type RequiredTurn struct {
	ItemID, ItemKind, RouteClass, GateClaimID, JobID, GrabID string
	ActionID, DependentJobs                                  int
}

// Counts is complete even when Snapshot.Items is paginated.
type Counts struct {
	PendingTotal            int `json:"pending_total"`
	TurnsRequired           *int
	TurnsWorking            *int
	FamilyBreakdownComplete *bool
	FamilyRuns              []FamilyRun
	RequiredTurnsComplete   *bool
	RequiredTurns           []RequiredTurn
	WatchHits               int `json:"watch_hits"`
	Actions                 int `json:"actions"`
	ActionsRequiresAuth     int `json:"actions_requires_auth"`
	Retractions             int `json:"retractions"`
	JobsWorking             int `json:"jobs_working"`
	JobsNeedsReview         int `json:"jobs_needs_review"`
	FailureGroups7d         int `json:"failure_groups_7d"`
}

// SnapshotRequest controls a bounded view into a complete snapshot ordering.
// Schema is the negotiated browser snapshot schema; legacy schemas exclude
// grab-backed items before pagination so they never consume page slots.
type SnapshotRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Schema int    `json:"schema,omitempty"`
}

// Snapshot is the frozen triage snapshot schema v1 envelope.
type Snapshot struct {
	Schema                int    `json:"schema"`
	GeneratedAt           string `json:"generated_at"`
	Counts                Counts `json:"counts"`
	Items                 []Item `json:"items"`
	Cursor                string `json:"cursor,omitempty"`
	HasMore               bool   `json:"has_more"`
	UnsupportedItemsCount int    `json:"unsupported_items_count"`
}

// ItemSource supplies full pending items in the snapshot's read transaction.
// It lets independently owned domains contribute a schema-v1 kind without
// coupling the aggregation to their persistence package.
type ItemSource interface {
	SnapshotItems(context.Context, *sql.Tx) ([]Item, error)
}

// RetractionAcknowledger is an ItemSource that can clear one of the retraction
// notices it contributes. Retraction notices are recomputed from an external
// metadata source, so unlike a watch hit or a human action they never resolve
// themselves; acknowledging one is the only way the inbox empties.
type RetractionAcknowledger interface {
	AcknowledgeRetraction(ctx context.Context, itemID string) (bool, error)
}

// Service composes the transactionally consistent inbox read model.
type Service struct {
	Store   *store.Store
	Watches *watch.Store
	Jobs    *job.Store

	mu      sync.RWMutex
	sources []ItemSource
	now     func() time.Time
}

// New creates a triage service over the process-wide store.
func New(s *store.Store, watches *watch.Store, jobs *job.Store) *Service {
	return &Service{Store: s, Watches: watches, Jobs: jobs, now: time.Now}
}

// RegisterSource adds one independently owned item producer. Registration is
// intended for bootstrap and a nil source is ignored.
func (s *Service) RegisterSource(source ItemSource) {
	if s == nil || source == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources = append(s.sources, source)
}

// AcknowledgeRetraction clears the retraction notice named by itemID from the
// inbox, reporting whether this call recorded the acknowledgement. No
// registered source owning the item yields sql.ErrNoRows, which callers render
// as the same conflict a vanished watch hit produces.
func (s *Service) AcknowledgeRetraction(ctx context.Context, itemID string) (bool, error) {
	if s == nil {
		return false, errors.New("triage service is not configured")
	}
	s.mu.RLock()
	sources := append([]ItemSource(nil), s.sources...)
	s.mu.RUnlock()
	for _, source := range sources {
		acknowledger, ok := source.(RetractionAcknowledger)
		if !ok {
			continue
		}
		applied, err := acknowledger.AcknowledgeRetraction(ctx, itemID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return applied, err
	}
	return false, sql.ErrNoRows
}

// Snapshot returns one bounded page of a transactionally consistent inbox.
func (s *Service) Snapshot(ctx context.Context, request SnapshotRequest) (Snapshot, error) {
	all, counts, unsupported, generatedAt, err := s.collect(ctx, request.Schema)
	if err != nil {
		return Snapshot{}, err
	}
	limit, offset, err := parsePage(request)
	if err != nil {
		return Snapshot{}, err
	}
	if request.Schema > 0 && request.Schema < 4 {
		legacy := all[:0]
		for _, item := range all {
			if item.Kind != KindPdfGrab {
				legacy = append(legacy, item)
			}
		}
		all = legacy
	}
	if offset > len(all) {
		return Snapshot{}, errors.New("triage cursor is beyond the snapshot")
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	items := append([]Item(nil), all[offset:end]...)
	if items == nil {
		items = []Item{}
	}
	snapshot := Snapshot{
		Schema: SchemaVersion, GeneratedAt: generatedAt, Counts: counts, Items: items,
		HasMore: end < len(all), UnsupportedItemsCount: unsupported,
	}
	if snapshot.HasMore {
		snapshot.Cursor = encodeCursor(end)
	}
	return snapshot, nil
}

// Counts returns a complete count envelope from the same data model as
// Snapshot. It intentionally does not expose a cursor or partial item list.
func (s *Service) Counts(ctx context.Context, schema ...int) (Counts, error) {
	_, counts, _, _, err := s.collect(ctx, schema...)
	return counts, err
}

// statsSeriesWeeks bounds the browser stats weekly time series, well under
// the wire cap of 60 buckets (protocol.validateStatsResponse).
const statsSeriesWeeks = 12

// Stats is the daemon-owned acquisition value read model behind the browser
// extension's stats_request RPC. Acquired jobs are terminal ready/imported
// jobs; failed jobs are terminal failed/unavailable jobs (cancelled jobs
// count toward neither, since the user - not the acquisition pipeline -
// decided the outcome).
type Stats struct {
	AcquiredTotal    int           `json:"acquired_total"`
	FailedTotal      int           `json:"failed_total"`
	HandoffsRequired int           `json:"handoffs_required"`
	Access           StatsAccess   `json:"access"`
	Series           []StatsBucket `json:"series"`
}

// StatsAccess breaks AcquiredTotal down by the access basis of each
// acquired job's accepted candidate. Other captures manual acquisitions and
// acquired jobs with no accepted candidate (stale rows predating candidate
// recording), so the buckets need not sum to AcquiredTotal-exact parity with
// any single source.
type StatsAccess struct {
	OpenAccess    int `json:"open_access"`
	Institutional int `json:"institutional"`
	LicensedAPI   int `json:"licensed_api"`
	Other         int `json:"other"`
}

// StatsBucket is one weekly bucket of the acquisition series: jobs acquired
// in the ISO week (Monday-Sunday, UTC) beginning PeriodStart.
type StatsBucket struct {
	PeriodStart time.Time `json:"period_start"`
	Acquired    int       `json:"acquired"`
}

// Stats returns lifetime acquisition value metrics for the browser
// extension's stats surface. It reads from the same store as Counts but is
// independent of the paginated triage inbox.
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	if s == nil || s.Store == nil {
		return Stats{}, errors.New("triage service is not configured")
	}
	tx, err := s.Store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Stats{}, fmt.Errorf("starting triage stats: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var stats Stats
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN ('failed', 'unavailable')`,
	).Scan(&stats.FailedTotal); err != nil {
		return Stats{}, err
	}

	weeks := seriesWeeks(s.now(), statsSeriesWeeks)
	buckets := make(map[int64]int, len(weeks))
	for _, week := range weeks {
		buckets[week.Unix()] = 0
	}
	seriesStart := weeks[0]
	seriesEnd := weeks[len(weeks)-1].AddDate(0, 0, 7)

	// Acquired jobs are bucketed by the timestamp of their ready transition
	// (recorded in the events table), not jobs.updated_at: reaching ready is
	// the acquisition itself, while updated_at is overwritten again by the
	// separate, manually-triggered ready -> imported transition (`papio
	// zotio apply`, internal/zotio/plan.go's markImported), which can land
	// weeks after the paper was actually acquired and would otherwise move
	// an old acquisition into a recent bucket. Every transition logs exactly
	// one job.transition event carrying its target state in
	// detail_json->>'to' (job.go's transition() unconditionally sets
	// detail["to"] before marshaling, for every route into a state); ORDER
	// BY seq (events_by_job is keyed (job_id, seq), so this is an index seek,
	// not a sort) picks the FIRST such event, so even a hypothetical future
	// edge that re-enters ready still resolves to the original acquisition
	// moment rather than an arbitrary matching row. AGENTS.md documents that
	// a long-running dev papio.db can hold rows that predate a later
	// behavior change (job.WithHumanActionBinding is the known precedent);
	// a ready/imported job whose event log predates this convention and so
	// has no matching row falls back to updated_at, the exact pre-fix
	// signal, rather than failing the whole query over one legacy row.
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(
		         (SELECT e.at FROM events e
		            WHERE e.job_id = j.id AND e.kind = 'job.transition'
		              AND json_extract(e.detail_json, '$.to') = 'ready'
		            ORDER BY e.seq LIMIT 1),
		         j.updated_at
		       ),
		       (SELECT c.access_basis FROM candidates c
		          WHERE c.job_id = j.id AND c.status = 'accepted' LIMIT 1),
		       EXISTS(SELECT 1 FROM human_actions ha WHERE ha.job_id = j.id)
		FROM jobs j
		WHERE j.state IN ('ready', 'imported')`)
	if err != nil {
		return Stats{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var acquiredAtRaw string
		var accessBasis sql.NullString
		var handoff int
		if err := rows.Scan(&acquiredAtRaw, &accessBasis, &handoff); err != nil {
			return Stats{}, err
		}
		stats.AcquiredTotal++
		if handoff != 0 {
			stats.HandoffsRequired++
		}
		switch accessBasis.String {
		case resolver.AccessOpen:
			stats.Access.OpenAccess++
		case resolver.AccessInstitutional:
			stats.Access.Institutional++
		case resolver.AccessLicensedAPI:
			stats.Access.LicensedAPI++
		default:
			stats.Access.Other++
		}
		acquiredAt, err := time.Parse(time.RFC3339Nano, acquiredAtRaw)
		if err != nil {
			return Stats{}, err
		}
		if acquiredAt.Before(seriesStart) || !acquiredAt.Before(seriesEnd) {
			continue
		}
		buckets[weekStart(acquiredAt).Unix()]++
	}
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stats{}, fmt.Errorf("committing triage stats: %w", err)
	}

	stats.Series = make([]StatsBucket, len(weeks))
	for i, week := range weeks {
		stats.Series[i] = StatsBucket{PeriodStart: week, Acquired: buckets[week.Unix()]}
	}
	return stats, nil
}

// weekStart returns the Monday UTC midnight beginning t's ISO week.
func weekStart(t time.Time) time.Time {
	t = t.UTC()
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	daysSinceMonday := (int(midnight.Weekday()) + 6) % 7 // Monday=0 .. Sunday=6
	return midnight.AddDate(0, 0, -daysSinceMonday)
}

// seriesWeeks returns n consecutive weekly bucket starts, oldest first,
// ending with the week containing now.
func seriesWeeks(now time.Time, n int) []time.Time {
	current := weekStart(now)
	weeks := make([]time.Time, n)
	for i := range weeks {
		weeks[i] = current.AddDate(0, 0, -7*(n-1-i))
	}
	return weeks
}

// FindWatchHit resolves an item ID against the full current inbox. The returned
// keys are internal-only inputs for consume/acquire mutations.
func (s *Service) FindWatchHit(ctx context.Context, id string) (*WatchHit, error) {
	all, _, _, _, err := s.collect(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range all {
		if item.ID == id && item.Kind == KindWatchHit {
			return item.WatchHit, nil
		}
	}
	return nil, sql.ErrNoRows
}

type pageCursor struct {
	Version int `json:"v"`
	Offset  int `json:"o"`
}

func parsePage(request SnapshotRequest) (limit, offset int, _ error) {
	limit = request.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return 0, 0, fmt.Errorf("triage limit must be between 1 and %d", maxLimit)
	}
	if request.Cursor == "" {
		return limit, 0, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil || len(encoded) > 64 {
		return 0, 0, errors.New("invalid triage cursor")
	}
	var cursor pageCursor
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != SchemaVersion || cursor.Offset < 0 {
		return 0, 0, errors.New("invalid triage cursor")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return 0, 0, errors.New("invalid triage cursor")
	}
	return limit, cursor.Offset, nil
}

func encodeCursor(offset int) string {
	encoded, _ := json.Marshal(pageCursor{Version: SchemaVersion, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func (s *Service) collect(ctx context.Context, schema ...int) ([]Item, Counts, int, string, error) {
	if s == nil || s.Store == nil || s.Watches == nil || s.Jobs == nil {
		return nil, Counts{}, 0, "", errors.New("triage service is not configured")
	}
	tx, err := s.Store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, Counts{}, 0, "", fmt.Errorf("starting triage snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	watchItems, err := watchHitItems(ctx, tx)
	if err != nil {
		return nil, Counts{}, 0, "", err
	}
	actionItems, err := humanActionItems(ctx, tx)
	if err != nil {
		return nil, Counts{}, 0, "", err
	}
	counts, err := snapshotCounts(ctx, tx, len(watchItems), len(actionItems), s.Jobs)
	if err != nil {
		return nil, Counts{}, 0, "", err
	}

	s.mu.RLock()
	sources := append([]ItemSource(nil), s.sources...)
	s.mu.RUnlock()
	retractionItems := make([]Item, 0)
	pdfGrabItems := make([]Item, 0)
	unsupported := 0
	for _, source := range sources {
		items, err := source.SnapshotItems(ctx, tx)
		if err != nil {
			return nil, Counts{}, 0, "", fmt.Errorf("reading triage item source: %w", err)
		}
		for _, item := range items {
			switch item.Kind {
			case KindRetraction:
				if err := normalizeRetractionItem(&item); err != nil {
					return nil, Counts{}, 0, "", err
				}
				retractionItems = append(retractionItems, item)
			case KindPdfGrab:
				if item.PdfGrab == nil || item.PdfGrab.GrabID == "" || item.PdfGrab.State == "" || item.Title == "" {
					return nil, Counts{}, 0, "", errors.New("invalid pending pdf grab item")
				}
				pdfGrabItems = append(pdfGrabItems, item)
			default:
				unsupported++
			}
		}
	}
	counts.Retractions = len(retractionItems)
	counts.PendingTotal = counts.WatchHits + counts.Actions + counts.Retractions
	if len(schema) > 0 && schema[0] >= 4 {
		counts.PendingTotal += len(pdfGrabItems)
	}

	assignRanks(retractionItems, retractionRankBase)
	assignRanks(pdfGrabItems, pdfGrabRankBase)
	assignRanks(actionItems, humanActionRankBase)
	assignRanks(watchItems, watchHitRankBase)
	items := make([]Item, 0, len(retractionItems)+len(pdfGrabItems)+len(actionItems)+len(watchItems))
	items = append(items, retractionItems...)
	items = append(items, pdfGrabItems...)
	items = append(items, actionItems...)
	items = append(items, watchItems...)
	if err := tx.Commit(); err != nil {
		return nil, Counts{}, 0, "", fmt.Errorf("committing triage snapshot: %w", err)
	}
	if err := s.projectFamilies(ctx, items, &counts); err != nil {
		return nil, Counts{}, 0, "", err
	}
	return items, counts, unsupported, s.now().UTC().Format(time.RFC3339Nano), nil
}

func snapshotCounts(ctx context.Context, tx *sql.Tx, watchHits, actions int, jobs *job.Store) (Counts, error) {
	counts := Counts{WatchHits: watchHits, Actions: actions}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM human_actions
		WHERE status = 'open' AND requires_auth = 1`).Scan(&counts.ActionsRequiresAuth); err != nil {
		return Counts{}, err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE state IN ('queued', 'resolving', 'fetching', 'validating', 'awaiting_human', 'retry_wait')`).Scan(&counts.JobsWorking); err != nil {
		return Counts{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state = 'needs_review'`).Scan(&counts.JobsNeedsReview); err != nil {
		return Counts{}, err
	}
	failureGroups, err := jobs.FailureGroupCount(ctx, tx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		return Counts{}, err
	}
	counts.FailureGroups7d = failureGroups
	return counts, nil
}
func (s *Service) projectFamilies(ctx context.Context, items []Item, counts *Counts) error {
	if counts == nil {
		return errors.New("nil triage counts")
	}
	required, working := 0, 0
	breakdownComplete := true
	requiredTurnsProjectionComplete := true
	var gates []job.HumanGateObservation
	if s.Jobs != nil {
		projection, err := s.Jobs.CurrentHumanAttention(ctx)
		if err != nil {
			// Human-gate authority is required to claim an exact required-turn
			// projection. Keep ordinary row counts, but fail closed for the
			// required-turn list and family totals.
			breakdownComplete = false
			requiredTurnsProjectionComplete = false
		} else {
			gates = projection.Gates
		}
	} else {
		breakdownComplete = false
		requiredTurnsProjectionComplete = false
	}
	gateByJob := make(map[string]job.HumanGateObservation)
	for _, gate := range gates {
		for _, id := range append(append([]string(nil), gate.DependentJobIDs...), gate.ClaimMemberJobIDs...) {
			if _, exists := gateByJob[id]; !exists {
				gateByJob[id] = gate
			}
		}
	}
	ownerByGate := make(map[string]string)
	for _, gate := range gates {
		// The producer identifies one owner by keeping it out of the
		// dependent sibling set. If no such member exists, the authority is
		// unresolvable and the required-turn projection fails closed.
		for _, member := range gate.ClaimMemberJobIDs {
			if !contains(gate.DependentJobIDs, member) {
				ownerByGate[gate.ID] = member
				break
			}
		}
		if _, exists := ownerByGate[gate.ID]; !exists {
			breakdownComplete = false
			requiredTurnsProjectionComplete = false
		}
	}
	rows := make([]familyRow, len(items))
	for i := range items {
		item := &items[i]
		var assignment *FamilyAssignment
		var turn *RequiredTurn
		switch item.Kind {
		case KindHumanAction:
			if item.HumanAction == nil {
				breakdownComplete = false
				continue
			}
			action := item.HumanAction
			attention, mapped := EffectiveAttention(action.ActionKind, action.RequiresAuth, action.DeliveryState)
			if !mapped {
				attention = "required"
			}
			gateID, dependent := "", 0
			if gate, ok := gateByJob[action.JobID]; ok {
				gateID = gate.ID
				dependent = len(gate.DependentJobIDs)
				owner, resolvable := ownerByGate[gate.ID]
				if !resolvable {
					// A typed claim without an owner is not safely
					// actionable; do not silently report zero turns.
					breakdownComplete = false
					requiredTurnsProjectionComplete = false
					continue
				}
				if owner != action.JobID {
					attention = "working"
				}
			}
			route := action.ActionKind
			operation := familyOperation(action.ActionKind, attention, item.Ops)
			actor := "researcher"
			if attention == "working" {
				actor = "papio"
			}
			if attention == "required" {
				required++
			} else {
				working++
			}
			guidance := familyGuidance(action.ActionKind, action.RequiresAuth, action.DiagnosisReason, attention, gateByJob[action.JobID])
			if route == "" || guidance == "" || operation == "" || !contains(protocol.TriageRouteClassesV5(), route) ||
				!contains(protocol.TriageNextActors(), actor) || !contains(protocol.TriageGuidanceVariants(), guidance) ||
				!contains(protocol.TriageOperationVariants(), operation) {
				// Keep the exact attention count, but make family and
				// required-turn detail unavailable rather than dropping this row.
				breakdownComplete = false
				requiredTurnsProjectionComplete = false
				continue
			}
			assignment = &FamilyAssignment{NextActor: actor, GuidanceVariant: guidance, OperationVariant: operation, RouteClass: route, ActionKind: action.ActionKind, GateClaimID: gateID, DependentJobs: dependent}
			if attention == "required" {
				turn = &RequiredTurn{ItemID: item.ID, ItemKind: KindHumanAction, RouteClass: route, GateClaimID: gateID, JobID: action.JobID, ActionID: int(action.ActionID), DependentJobs: dependent}
			}
		case KindPdfGrab:
			if item.PdfGrab == nil {
				breakdownComplete = false
				requiredTurnsProjectionComplete = false
				continue
			}
			assignment = &FamilyAssignment{NextActor: "researcher", GuidanceVariant: "pdf_identifier", OperationVariant: "provide_identifier_or_dismiss", RouteClass: "pdf_identifier_needed", ActionKind: "pdf_identifier_needed"}
			required++
			turn = &RequiredTurn{ItemID: item.ID, ItemKind: KindPdfGrab, RouteClass: "pdf_identifier_needed", GrabID: item.PdfGrab.GrabID}
		}
		if assignment == nil {
			continue
		}
		rows[i] = familyRow{assignment: assignment, tuple: familyTupleOf(item.Kind, assignment), turn: turn}
	}
	orderHumanActionsByFamily(items, rows)
	runCount := buildFamilyRuns(items, rows, counts)
	if required > 1024 || !requiredTurnsProjectionComplete || len(counts.RequiredTurns) != required {
		counts.RequiredTurnsComplete = new(false)
		counts.RequiredTurns = nil
	} else {
		counts.RequiredTurnsComplete = new(true)
	}
	// The wire bound survives the family ordering: grouping shrinks the run
	// count, but the projection must still refuse to overflow the contract.
	if runCount > 128 {
		breakdownComplete = false
	}
	if !breakdownComplete {
		counts.FamilyRuns = nil
	}
	counts.TurnsRequired, counts.TurnsWorking = &required, &working
	counts.FamilyBreakdownComplete = &breakdownComplete
	return nil
}

// familyTuple is the full family identity from plan §14. Two rows belong to
// the same task family only when every component matches, so
// `manual_download` and `manual_download_adapter_missing` stay separate.
type familyTuple struct {
	kind, route, action, actor, guidance, operation string
}

// familyRow is one item's projected family membership. It is computed before
// the rows are reordered so ordering, ranking, and run building all see the
// same assignment.
type familyRow struct {
	assignment *FamilyAssignment
	tuple      familyTuple
	turn       *RequiredTurn
}

func familyTupleOf(kind string, assignment *FamilyAssignment) familyTuple {
	return familyTuple{kind, assignment.RouteClass, assignment.ActionKind, assignment.NextActor, assignment.GuidanceVariant, assignment.OperationVariant}
}

// orderHumanActionsByFamily makes every task family one contiguous block:
// families are ordered by their earliest member and members keep their
// insertion order inside a family. The underlying action order is pure
// insertion order with no priority, severity, or attention term, so grouping
// preserves the only real signal at both levels while letting the inbox hoist
// one instruction per family instead of repeating it per fragment.
//
// A row with no mapped variant cannot join a family (plan §11 rule 4): it is
// keyed by its own position, so it sorts among the families by its own id and
// never merges with anything. Ranks are reassigned afterwards so `Rank` and
// `first_rank` describe the emitted order. Other kinds own separate rank bases
// and never interleave with actions, so they are left untouched.
func orderHumanActionsByFamily(items []Item, rows []familyRow) {
	positions := make([]int, 0, len(items))
	for i := range items {
		if items[i].Kind == KindHumanAction {
			positions = append(positions, i)
		}
	}
	if len(positions) == 0 {
		return
	}
	group := make([]int, len(positions))
	firstOf := make(map[familyTuple]int, len(positions))
	for n, position := range positions {
		if rows[position].assignment == nil {
			group[n] = position
			continue
		}
		first, grouped := firstOf[rows[position].tuple]
		if !grouped {
			first = position
			firstOf[rows[position].tuple] = position
		}
		group[n] = first
	}
	order := make([]int, len(positions))
	for n := range order {
		order[n] = n
	}
	sort.SliceStable(order, func(left, right int) bool { return group[order[left]] < group[order[right]] })
	orderedItems := make([]Item, len(positions))
	orderedRows := make([]familyRow, len(positions))
	for n, source := range order {
		orderedItems[n], orderedRows[n] = items[positions[source]], rows[positions[source]]
	}
	for n, position := range positions {
		items[position], rows[position] = orderedItems[n], orderedRows[n]
		items[position].Rank = humanActionRankBase + n
	}
}

// buildFamilyRuns records one run per maximal contiguous family in the emitted
// order and appends required turns in that same order. Because action rows are
// grouped before ranking, every family yields exactly one run.
func buildFamilyRuns(items []Item, rows []familyRow, counts *Counts) int {
	var previous familyTuple
	// runIndex points at the open run inside counts.FamilyRuns, so a member
	// increments the stored run rather than a detached copy of it.
	runIndex, runCount := -1, 0
	for i := range items {
		row := rows[i]
		if row.assignment == nil {
			runIndex, previous = -1, familyTuple{}
			continue
		}
		if row.turn != nil {
			counts.RequiredTurns = append(counts.RequiredTurns, *row.turn)
		}
		if runIndex < 0 || row.tuple != previous {
			keyInput := []any{5, items[i].ID, items[i].Kind, row.assignment.RouteClass, row.assignment.ActionKind, row.assignment.NextActor, row.assignment.GuidanceVariant, row.assignment.OperationVariant}
			raw, _ := json.Marshal(keyInput)
			sum := sha256.Sum256(raw)
			counts.FamilyRuns = append(counts.FamilyRuns, FamilyRun{
				RunKey: "fr1_" + hex.EncodeToString(sum[:])[:32], FirstRank: items[i].Rank,
				RouteClass: row.assignment.RouteClass, ActionKind: row.assignment.ActionKind,
				NextActor: row.assignment.NextActor, GuidanceVariant: row.assignment.GuidanceVariant,
				OperationVariant: row.assignment.OperationVariant, Count: 1,
			})
			runIndex, previous, runCount = len(counts.FamilyRuns)-1, row.tuple, runCount+1
		} else {
			counts.FamilyRuns[runIndex].Count++
		}
		row.assignment.RunKey = counts.FamilyRuns[runIndex].RunKey
		items[i].Family = row.assignment
	}
	return runCount
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// manualDownloadGuidance maps a manual download's durable diagnosis onto its
// task family. Five genuinely different reasons open this one action kind and
// they need five different instructions — telling someone to "download the
// PDF" when the real problem is that the file they already supplied was
// rejected sends them round the same loop.
//
// A NULL diagnosis (a row predating the column, or a producer with no
// structured reason) is the plain manual download, which is what it has always
// rendered as. An unrecognised one returns "" so the row stays standalone and
// makes the breakdown incomplete; it is never guessed from the prose detail.
func manualDownloadGuidance(diagnosisReason string) string {
	switch diagnosisReason {
	case job.DiagnosisReasonProviderAdapterMissing:
		return "manual_download_adapter_missing"
	case job.DiagnosisReasonProviderAdapterDrift:
		return "manual_download_page_undriveable"
	case job.DiagnosisReasonAdoptedPDFInvalid:
		return "manual_download_rejected_file"
	case job.DiagnosisReasonWrongWork:
		return "manual_download_wrong_work"
	case "", job.DiagnosisReasonLandingPageOnly, job.DiagnosisReasonInstitutionalHandoff:
		return "manual_download"
	default:
		return ""
	}
}

func familyGuidance(kind string, auth *bool, diagnosisReason, attention string, gate job.HumanGateObservation) string {
	if attention == "working" {
		return "papio_continuing"
	}
	switch {
	case kind == "manual_download":
		// Ahead of the gate branch, as the adapter-missing case already was:
		// an open profile gate does not change what this row asks for. papio
		// will not continue on its own once the gate clears — the researcher
		// still has to supply the file — so sign-in copy would be a lie.
		return manualDownloadGuidance(diagnosisReason)
	case gate.ID != "":
		if gate.GateType == job.HumanGateCaptchaOrSecurity {
			return "security_challenge"
		}
		return "institution_sign_in"
	case kind == "human_auth_required":
		return "institution_sign_in"
	case kind == "openurl_handoff":
		if auth == nil {
			// Unknown authentication is not an authoritative actor mapping.
			return ""
		}
		if *auth {
			return "institution_sign_in"
		}
		return "open_page"
	case kind == "openurl_available":
		return "open_page"
	case kind == "verify_identity":
		return "verify_identity"
	case kind == "document_delivery":
		return "document_delivery"
	case kind == "downloads_access_required":
		return "downloads_access"
	case kind == "terms_acceptance_required":
		return "terms_acceptance"
	default:
		return ""
	}
}

// EffectiveAttention applies the shared durable action-attention mapping used
// by triage counts, pulse, and browser presentation. The bool reports whether
// the mapping is authoritative; unknown authentication is deliberately
// unmapped rather than guessed as autonomous work.
func EffectiveAttention(kind string, requiresAuth *bool, deliveryState string) (attention string, mapped bool) {
	if kind == job.ActionKindDocumentDelivery {
		if deliveryState == "fulfilled" {
			return "working", true
		}
		return "required", true
	}
	if kind == "openurl_handoff" && requiresAuth == nil {
		return "", false
	}
	return "required", true
}
func familyOperation(kind, attention string, ops []string) string {
	if attention == "working" {
		return "none"
	}
	switch kind {
	case "verify_identity":
		if contains(ops, "open") {
			return "accept_reject_open"
		}
		return "accept_reject"
	case "document_delivery":
		return "delivery_reconcile"
	default:
		if len(ops) > 1 {
			return "open_and_dismiss"
		}
		return "dismiss_only"
	}
}

type digestRow struct {
	watchID     int64
	watchLabel  string
	workKey     string
	title       string
	authors     string
	year        int
	doi         string
	isOA        bool
	abstract    string
	firstSeenAt string
	identifiers string
}

func watchHitItems(ctx context.Context, tx *sql.Tx) ([]Item, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.watch_id, w.label, d.work_key, d.title, d.authors, d.year, d.doi,
			d.is_oa, d.abstract, d.first_seen_at, d.identifiers_json
		FROM watch_digest_entries d
		JOIN watches w ON w.id = d.watch_id
		WHERE d.consumed = 0
		ORDER BY d.first_seen_at ASC, d.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make(map[string]*WatchHit)
	for rows.Next() {
		var row digestRow
		if err := rows.Scan(&row.watchID, &row.watchLabel, &row.workKey, &row.title, &row.authors, &row.year, &row.doi,
			&row.isOA, &row.abstract, &row.firstSeenAt, &row.identifiers); err != nil {
			return nil, err
		}
		if row.watchID <= 0 || strings.TrimSpace(row.workKey) == "" || strings.TrimSpace(row.title) == "" {
			return nil, errors.New("invalid pending watch digest entry")
		}
		identifiers, err := decodeIdentifiers(row.identifiers)
		if err != nil {
			return nil, err
		}
		doi, arxiv, openalex := canonicalIdentifiers(row.doi, identifiers)
		identity := "key:" + strings.ToLower(strings.TrimSpace(row.workKey))
		switch {
		case doi != "":
			identity = "doi:" + doi
		case arxiv != "":
			identity = "arxiv:" + strings.ToLower(arxiv)
		case openalex != "":
			identity = "openalex:" + strings.ToLower(openalex)
		}
		group := groups[identity]
		if group == nil {
			group = &WatchHit{
				Work:     Work{DOI: doi, Title: bounded(row.title, 500), Authors: bounded(row.authors, 200), Year: row.year, IsOA: row.isOA},
				Abstract: bounded(row.abstract, 2000), FirstSeenAt: row.firstSeenAt, arXiv: arxiv, openAlex: openalex,
			}
			groups[identity] = group
		}
		if group.Work.DOI == "" {
			group.Work.DOI = doi
		}
		if group.arXiv == "" {
			group.arXiv = arxiv
		}
		if group.openAlex == "" {
			group.openAlex = openalex
		}
		if group.FirstSeenAt == "" || row.firstSeenAt < group.FirstSeenAt {
			group.FirstSeenAt = row.firstSeenAt
		}
		group.Watches = append(group.Watches, Watch{ID: row.watchID, Label: bounded(row.watchLabel, 500), WorkKey: row.workKey})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(groups))
	for _, hit := range groups {
		sort.Slice(hit.Watches, func(i, j int) bool { return hit.Watches[i].ID < hit.Watches[j].ID })
		if len(hit.Watches) == 0 {
			continue
		}
		first := hit.Watches[0]
		item := Item{
			Kind: KindWatchHit, ID: fmt.Sprintf("hit:%d:%s", first.ID, first.WorkKey),
			Title: hit.Work.Title, Facts: watchFacts(hit.Work), Links: canonicalLinks(hit.Work.DOI, hit.arXiv, hit.openAlex),
			Ops: []string{"acquire", "dismiss"}, WatchHit: hit,
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].WatchHit, items[j].WatchHit
		if left.FirstSeenAt != right.FirstSeenAt {
			return left.FirstSeenAt > right.FirstSeenAt
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func decodeIdentifiers(value string) (protocol.Identifiers, error) {
	if len(value) > 16<<10 {
		return protocol.Identifiers{}, errors.New("watch digest identifiers exceed limit")
	}
	type payload struct {
		protocol.Identifiers
		TitleAliases []string `json:"title_aliases,omitempty"`
	}
	var decoded payload
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return protocol.Identifiers{}, fmt.Errorf("decoding watch digest identifiers: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return protocol.Identifiers{}, errors.New("watch digest identifiers have trailing data")
	}
	return decoded.Identifiers, nil
}

func canonicalIdentifiers(rowDOI string, identifiers protocol.Identifiers) (doi, arxiv, openalex string) {
	if normalized, err := work.NormalizeDOI(rowDOI); err == nil {
		doi = normalized
	} else if normalized, err := work.NormalizeDOI(identifiers.DOI); err == nil {
		doi = normalized
	}
	if normalized, err := work.NormalizeArXiv(identifiers.ArXiv); err == nil {
		arxiv = normalized
	}
	if normalized, err := work.NormalizeOpenAlex(identifiers.OpenAlex); err == nil {
		openalex = normalized
	}
	return doi, arxiv, openalex
}

func canonicalLinks(doi, arxiv, openalex string) []Link {
	links := make([]Link, 0, 3)
	if doi != "" {
		links = append(links, Link{Rel: "doi", URL: canonicalDOIURL(doi)})
	}
	if arxiv != "" {
		links = append(links, Link{Rel: "arxiv", URL: "https://arxiv.org/abs/" + arxiv})
	}
	if openalex != "" {
		links = append(links, Link{Rel: "openalex", URL: "https://openalex.org/" + openalex})
	}
	return links
}

func canonicalDOIURL(doi string) string {
	return (&url.URL{Scheme: "https", Host: "doi.org", Path: "/" + doi}).String()
}

func watchFacts(work Work) []Fact {
	facts := make([]Fact, 0, 2)
	if work.Authors != "" {
		facts = append(facts, Fact{Label: "Authors", Text: work.Authors})
	}
	if work.Year != 0 {
		facts = append(facts, Fact{Label: "Year", Text: fmt.Sprintf("%d", work.Year)})
	}
	return facts
}

// humanActionItems loads open human actions with their work identity so the
// inbox shows a paper's title/authors/DOI instead of only the daemon's
// internal job id — matching watch_hit's Work/watchFacts treatment. Any kind
// is dismissible (Store.Cancel is idempotent on an already-terminal job);
// only verify_identity additionally offers accept (gated on a valid
// quarantine binding) and reject (never gated — reject needs no SHA match).
func humanActionItems(ctx context.Context, tx *sql.Tx) ([]Item, error) {
	// diagnosis is a durable column on the action, not an event join. The
	// join it replaced read `$.diagnosis` out of browser.provider_outcome,
	// a key no producer ever wrote and that two of the five manual-download
	// producers had no event for at all.
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.job_id, a.kind, j.state, COALESCE(a.detail, ''),
			a.revision, a.quarantine_sha256, a.requires_auth, a.blocked_by, j.work_request_id,
			COALESCE(w.title, ''), COALESCE(w.authors_json, '[]'), COALESCE(w.year, 0),
			COALESCE(a.diagnosis, '')
		FROM human_actions a
		JOIN jobs j ON j.id = a.job_id
		JOIN work_requests w ON w.id = j.work_request_id
		WHERE a.status = 'open'
		ORDER BY a.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		action             HumanAction
		detail             string
		requiresAuth       bool
		blockedBy          string
		workRequestID      string
		title, authorsJSON string
		year               int
		diagnosisReason    string
	}
	loaded := make([]row, 0)
	workRequestIDs := make([]string, 0)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.action.ActionID, &r.action.JobID, &r.action.ActionKind, &r.action.JobState, &r.detail,
			&r.action.Revision, &r.action.SHA256, &r.requiresAuth, &r.blockedBy, &r.workRequestID, &r.title, &r.authorsJSON, &r.year, &r.diagnosisReason); err != nil {
			return nil, err
		}
		r.action.DiagnosisReason = r.diagnosisReason
		if r.action.ActionID <= 0 || r.action.JobID == "" || r.action.ActionKind == "" || r.action.Revision <= 0 {
			return nil, errors.New("invalid open human action")
		}
		if r.blockedBy != "" {
			r.action.RequiresAuth, r.action.BlockedBy = &r.requiresAuth, r.blockedBy
		}
		loaded = append(loaded, r)
		workRequestIDs = append(workRequestIDs, r.workRequestID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Delivery state is an optional enrichment. Older stores may predate the
	// delivery table; absence must not make the whole triage snapshot fail.
	for i := range loaded {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM delivery_requests WHERE job_id=? ORDER BY updated_at DESC LIMIT 1`, loaded[i].action.JobID).Scan(&state); err == nil {
			loaded[i].action.DeliveryState = state
		}
	}
	identifiers, err := identifiersByWorkRequest(ctx, tx, workRequestIDs)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(loaded))
	for _, r := range loaded {
		var authors []string
		_ = json.Unmarshal([]byte(r.authorsJSON), &authors)
		ids := identifiers[r.workRequestID]
		work := Work{DOI: ids["doi"], Title: bounded(r.title, 500), Authors: bounded(strings.Join(authors, ", "), 200), Year: r.year}

		title := work.Title
		if title == "" {
			title = bounded(strings.ReplaceAll(r.action.ActionKind, "_", " "), 500)
		}
		facts := make([]Fact, 0, 5)
		facts = append(facts, Fact{Label: "Action", Text: bounded(strings.ReplaceAll(r.action.ActionKind, "_", " "), 60)})
		facts = append(facts, watchFacts(work)...)
		if r.detail = bounded(r.detail, 400); r.detail != "" {
			facts = append(facts, Fact{Label: "Detail", Text: r.detail})
		}
		facts = append(facts, Fact{Label: "Job", Text: bounded(r.action.JobID, 400)})

		links := canonicalLinks(ids["doi"], ids["arxiv"], ids["openalex"])
		ops := []string{"dismiss"}
		if len(links) > 0 {
			ops = append(ops, "open")
		}
		if r.action.ActionKind == "verify_identity" && r.action.JobState == job.StateNeedsReview {
			ops = []string{"reject"}
			if validSHA256(r.action.SHA256) {
				ops = []string{"accept", "reject"}
			}
			if len(links) > 0 {
				ops = append(ops, "open")
			}
		}
		items = append(items, Item{
			Kind: KindHumanAction, ID: fmt.Sprintf("action:%d", r.action.ActionID), Title: title,
			Facts: facts, Links: links, Ops: ops, HumanAction: &r.action,
		})
	}
	return items, nil
}

// identifiersByWorkRequest batch-loads every identifier for the given work
// requests in one query, avoiding an N+1 lookup per open human action.
func identifiersByWorkRequest(ctx context.Context, tx *sql.Tx, workRequestIDs []string) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(workRequestIDs))
	if len(workRequestIDs) == 0 {
		return out, nil
	}
	seen := make(map[string]bool, len(workRequestIDs))
	placeholders := make([]string, 0, len(workRequestIDs))
	args := make([]any, 0, len(workRequestIDs))
	for _, id := range workRequestIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	//nolint:gosec // only generated "?" placeholders enter the query text; IDs remain bound arguments.
	rows, err := tx.QueryContext(ctx,
		`SELECT work_request_id, kind, value FROM identifiers WHERE work_request_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var workRequestID, kind, value string
		if err := rows.Scan(&workRequestID, &kind, &value); err != nil {
			return nil, err
		}
		if out[workRequestID] == nil {
			out[workRequestID] = make(map[string]string, 3)
		}
		out[workRequestID][kind] = value
	}
	return out, rows.Err()
}

func normalizeRetractionItem(item *Item) error {
	if item.Retraction == nil || item.Kind != KindRetraction {
		return errors.New("invalid retraction triage item")
	}
	doi, err := work.NormalizeDOI(item.Retraction.DOI)
	if err != nil {
		return fmt.Errorf("invalid retraction DOI: %w", err)
	}
	if item.ID == "" {
		item.ID = RetractionIDPrefix + doi
	}
	if item.ID != RetractionIDPrefix+doi {
		return errors.New("invalid retraction item ID")
	}
	if item.Retraction.Nature != "retraction" && item.Retraction.Nature != "correction" && item.Retraction.Nature != "concern" {
		return errors.New("invalid retraction nature")
	}
	item.Retraction.DOI = doi
	item.Retraction.NoticeDOI = normalizeOptionalDOI(item.Retraction.NoticeDOI)
	item.Title = bounded(item.Title, 500)
	if item.Title == "" {
		item.Title = doi
	}
	item.Facts = normalizeFacts(item.Facts)
	item.Links = canonicalLinks(doi, "", "")
	item.Ops = normalizeOps(item.Ops)
	return nil
}

func normalizeOptionalDOI(value string) string {
	doi, err := work.NormalizeDOI(value)
	if err != nil {
		return ""
	}
	return doi
}

func normalizeFacts(facts []Fact) []Fact {
	if len(facts) > 8 {
		facts = facts[:8]
	}
	out := make([]Fact, 0, len(facts))
	for _, fact := range facts {
		fact.Label = bounded(fact.Label, 40)
		fact.Text = bounded(fact.Text, 400)
		if fact.Label != "" && fact.Text != "" {
			out = append(out, fact)
		}
	}
	return out
}

func normalizeOps(ops []string) []string {
	allowed := map[string]bool{"acquire": true, "dismiss": true, "accept": true, "reject": true, "open": true, "retry": true}
	out := make([]string, 0, len(ops))
	seen := make(map[string]bool)
	for _, op := range ops {
		if allowed[op] && !seen[op] {
			seen[op] = true
			out = append(out, op)
		}
	}
	return out
}
func assignRanks(items []Item, base int) {
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].ID, items[j].ID
		const prefix = "action:"
		if strings.HasPrefix(left, prefix) && strings.HasPrefix(right, prefix) {
			li, le := strconv.ParseInt(strings.TrimPrefix(left, prefix), 10, 64)
			ri, re := strconv.ParseInt(strings.TrimPrefix(right, prefix), 10, 64)
			if le == nil && re == nil && li != ri {
				return li < ri
			}
		}
		return left < right
	})
	for index := range items {
		items[index].Rank = base + index
	}
}

func validSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	for _, runeValue := range value {
		if (runeValue < '0' || runeValue > '9') && (runeValue < 'a' || runeValue > 'f') && (runeValue < 'A' || runeValue > 'F') {
			return false
		}
	}
	return true
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return ""
	}
	runes := 0
	for index := range value {
		if runes == limit {
			return value[:index]
		}
		runes++
	}
	return value
}
