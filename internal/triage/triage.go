// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package triage builds the daemon-owned inbox read model.
package triage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
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
)

const (
	KindWatchHit    = "watch_hit"
	KindHumanAction = "human_action"
	KindRetraction  = "retraction"
)

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
	ActionID   int64  `json:"action_id"`
	JobID      string `json:"job_id"`
	ActionKind string `json:"action_kind"`
	JobState   string `json:"job_state"`
	Revision   int64  `json:"revision"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
}

// Retraction carries the retraction-specific portion of an Item.
type Retraction struct {
	DOI       string    `json:"doi"`
	Nature    string    `json:"nature"`
	NoticedAt time.Time `json:"noticed_at"`
	NoticeDOI string    `json:"notice_doi"`
}

// Item is a schema-v1 inbox item. Exactly one kind-specific field is set for
// each supported Kind.
type Item struct {
	Kind  string   `json:"kind"`
	ID    string   `json:"id"`
	Rank  int      `json:"rank"`
	Title string   `json:"title"`
	Facts []Fact   `json:"facts"`
	Links []Link   `json:"links"`
	Ops   []string `json:"ops"`

	WatchHit    *WatchHit    `json:"-"`
	HumanAction *HumanAction `json:"-"`
	Retraction  *Retraction  `json:"-"`
}

// MarshalJSON emits exactly one supported kind-specific object.
func (item Item) MarshalJSON() ([]byte, error) {
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
		Kind  string   `json:"kind"`
		ID    string   `json:"id"`
		Rank  int      `json:"rank"`
		Title string   `json:"title"`
		Facts []Fact   `json:"facts"`
		Links []Link   `json:"links"`
		Ops   []string `json:"ops"`

		Work        *Work   `json:"work"`
		Abstract    string  `json:"abstract"`
		Watches     []Watch `json:"watches"`
		FirstSeenAt string  `json:"first_seen_at"`

		ActionID   int64  `json:"action_id"`
		JobID      string `json:"job_id"`
		ActionKind string `json:"action_kind"`
		JobState   string `json:"job_state"`
		Revision   int64  `json:"revision"`
		SHA256     string `json:"sha256"`
		SizeBytes  int64  `json:"size_bytes"`

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

// Counts is complete even when Snapshot.Items is paginated.
type Counts struct {
	PendingTotal    int `json:"pending_total"`
	WatchHits       int `json:"watch_hits"`
	Actions         int `json:"actions"`
	Retractions     int `json:"retractions"`
	JobsWorking     int `json:"jobs_working"`
	JobsNeedsReview int `json:"jobs_needs_review"`
	FailureGroups7d int `json:"failure_groups_7d"`
}

// SnapshotRequest controls a bounded view into a complete snapshot ordering.
type SnapshotRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
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

// Snapshot returns one bounded page of a transactionally consistent inbox.
func (s *Service) Snapshot(ctx context.Context, request SnapshotRequest) (Snapshot, error) {
	all, counts, unsupported, generatedAt, err := s.collect(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	limit, offset, err := parsePage(request)
	if err != nil {
		return Snapshot{}, err
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
func (s *Service) Counts(ctx context.Context) (Counts, error) {
	_, counts, _, _, err := s.collect(ctx)
	return counts, err
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

func (s *Service) collect(ctx context.Context) ([]Item, Counts, int, string, error) {
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
			default:
				unsupported++
			}
		}
	}
	counts.Retractions = len(retractionItems)
	counts.PendingTotal = counts.WatchHits + counts.Actions + counts.Retractions

	assignRanks(retractionItems, retractionRankBase)
	assignRanks(actionItems, humanActionRankBase)
	assignRanks(watchItems, watchHitRankBase)
	items := make([]Item, 0, len(retractionItems)+len(actionItems)+len(watchItems))
	items = append(items, retractionItems...)
	items = append(items, actionItems...)
	items = append(items, watchItems...)
	if err := tx.Commit(); err != nil {
		return nil, Counts{}, 0, "", fmt.Errorf("committing triage snapshot: %w", err)
	}
	return items, counts, unsupported, s.now().UTC().Format(time.RFC3339Nano), nil
}

func snapshotCounts(ctx context.Context, tx *sql.Tx, watchHits, actions int, jobs *job.Store) (Counts, error) {
	counts := Counts{WatchHits: watchHits, Actions: actions}
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

func humanActionItems(ctx context.Context, tx *sql.Tx) ([]Item, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.job_id, a.kind, j.state, COALESCE(a.detail, ''),
			a.revision, a.quarantine_sha256
		FROM human_actions a
		JOIN jobs j ON j.id = a.job_id
		WHERE a.status = 'open'
		ORDER BY a.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Item, 0)
	for rows.Next() {
		var action HumanAction
		var detail string
		if err := rows.Scan(&action.ActionID, &action.JobID, &action.ActionKind, &action.JobState, &detail,
			&action.Revision, &action.SHA256); err != nil {
			return nil, err
		}
		if action.ActionID <= 0 || action.JobID == "" || action.ActionKind == "" || action.Revision <= 0 {
			return nil, errors.New("invalid open human action")
		}
		title := bounded(strings.ReplaceAll(action.ActionKind, "_", " "), 500)
		facts := make([]Fact, 0, 2)
		if detail = bounded(detail, 400); detail != "" {
			facts = append(facts, Fact{Label: "Detail", Text: detail})
		}
		facts = append(facts, Fact{Label: "Job", Text: bounded(action.JobID, 400)})
		ops := []string{"open"}
		if action.ActionKind == "verify_identity" && action.JobState == job.StateNeedsReview && validSHA256(action.SHA256) {
			ops = []string{"accept", "reject", "open"}
		}
		items = append(items, Item{
			Kind: KindHumanAction, ID: fmt.Sprintf("action:%d", action.ActionID), Title: title,
			Facts: facts, Links: []Link{}, Ops: ops, HumanAction: &action,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
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
		item.ID = "retraction:" + doi
	}
	if item.ID != "retraction:"+doi {
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
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
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
