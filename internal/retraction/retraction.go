// Copyright 2026 OrgMentem. Licensed under MIT.

// Package retraction monitors Crossref update notices for ready library works.
package retraction

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/config"
	"papio/internal/notify"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/work"
)

const (
	defaultBaseURL = "https://api.crossref.org/works"
	defaultMaxBody = 1 << 20
	cacheFileName  = "retraction-cache.json"
	sweepEvery     = 24 * time.Hour
	maxNotices     = 1000
	cacheVersion   = 1
)

// Nature classifies an update notice recognized by the sentinel.
type Nature string

const (
	NatureRetraction Nature = "retraction"
	NatureCorrection Nature = "correction"
	NatureConcern    Nature = "concern"
)

// Finding is one current Crossref update notice for a ready library work.
type Finding struct {
	DOI       string    `json:"doi"`
	Nature    Nature    `json:"nature"`
	NoticedAt time.Time `json:"noticed_at"`
	NoticeDOI string    `json:"notice_doi,omitempty"`
}

// HTTPClient is the injected dependency used for Crossref metadata requests.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// BudgetAcquirer is the budget surface needed before every metadata request.
type BudgetAcquirer interface {
	Acquire(context.Context, string, config.Source, float64) error
}

type Options struct {
	Store            *store.Store
	Budgets          BudgetAcquirer
	Policy           config.Source
	Client           HTTPClient
	DataDir          string
	BaseURL          string
	MaxResponseBytes int64
	Notifier         notify.Sink
	Now              func() time.Time
}

// Sentinel performs at most one Crossref sweep each day and provides the
// cached current notices to the triage read model.
type Sentinel struct {
	store    *store.Store
	budgets  BudgetAcquirer
	policy   config.Source
	client   HTTPClient
	dataDir  string
	baseURL  string
	maxBody  int64
	notifier notify.Sink
	now      func() time.Time

	mu      sync.Mutex
	sweepMu sync.Mutex
}

// New constructs a sentinel with production defaults.
func New(options Options) *Sentinel {
	client := options.Client
	if client == nil {
		client = http.DefaultClient
	}
	baseURL := options.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	maxBody := options.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Sentinel{
		store: options.Store, budgets: options.Budgets, policy: options.Policy,
		client: client, dataDir: options.DataDir, baseURL: baseURL, maxBody: maxBody,
		notifier: options.Notifier, now: now,
	}
}

// RunDue performs a daily metadata sweep when the configured source policy is
// enabled. Partial sweep results are committed; a failed DOI keeps its last
// known notices. A total sweep failure leaves the last known-good cache intact.
func (s *Sentinel) RunDue(ctx context.Context) error {
	if s == nil || !s.policy.Enabled {
		return nil
	}
	if s.store == nil {
		return errors.New("retraction: store is required")
	}
	if s.budgets == nil {
		return errors.New("retraction: budget manager is required")
	}

	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()

	now := s.now().UTC()
	s.mu.Lock()
	cached, ok := s.readCache()
	fresh := ok && !cached.CheckedAt.IsZero() && now.Sub(cached.CheckedAt) < sweepEvery
	s.mu.Unlock()
	if fresh {
		return nil
	}

	dois, err := s.readyDOIs(ctx)
	if err != nil {
		return err
	}
	previous := validNotices(cached.Notices)
	current := make(map[string]Finding)
	addCurrent := func(finding Finding) {
		if prior, exists := current[finding.DOI]; !exists || prefer(finding, prior) {
			current[finding.DOI] = finding
		}
	}
	var firstLookupErr error
	failedLookups := 0
	for _, doi := range dois {
		if err := s.budgets.Acquire(ctx, config.SourceRetractionWatch, s.policy, 0); err != nil {
			return fmt.Errorf("retraction: acquire Crossref budget: %w", err)
		}
		updates, err := s.lookup(ctx, doi)
		if err != nil {
			failedLookups++
			if firstLookupErr == nil {
				firstLookupErr = err
			}
			for _, finding := range previous {
				if finding.DOI == doi {
					addCurrent(finding)
				}
			}
			continue
		}
		for _, update := range updates {
			finding := Finding{DOI: doi, Nature: update.Nature, NoticeDOI: update.NoticeDOI}
			key := findingKey(finding)
			if old, exists := previous[key]; exists {
				finding.NoticedAt = old.NoticedAt
			} else {
				finding.NoticedAt = now
			}
			addCurrent(finding)
		}
	}
	if len(dois) > 0 && failedLookups == len(dois) {
		return firstLookupErr
	}

	findings := make([]Finding, 0, len(current))
	for _, finding := range current {
		findings = append(findings, finding)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].DOI < findings[j].DOI })
	if len(findings) > maxNotices {
		findings = findings[:maxNotices]
	}
	notices := make(map[string]Finding, len(findings))
	for _, finding := range findings {
		notices[findingKey(finding)] = finding
	}

	s.mu.Lock()
	seenNotices := make(map[string]bool, len(previous))
	newFindings := make([]Finding, 0)
	for _, finding := range previous {
		seenNotices[noticeKey(finding)] = true
	}
	for _, finding := range findings {
		key := noticeKey(finding)
		if seenNotices[key] {
			continue
		}
		seenNotices[key] = true
		newFindings = append(newFindings, finding)
	}
	scanID := cached.ScanID
	if scanID == "" || !ok || !fresh {
		scanID = fmt.Sprintf("scan:%d", now.UnixNano())
	}
	if err := s.writeCache(cache{Version: cacheVersion, CheckedAt: now, ScanID: scanID, Notices: notices}); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("retraction: write cache: %w", err)
	}
	if err := s.pruneAcks(ctx, notices); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if len(newFindings) > 0 && s.notifier != nil {
		message := integrityNoticeMessage(newFindings)
		details := make([]map[string]any, 0, len(newFindings))
		for _, finding := range newFindings {
			details = append(details, map[string]any{
				"doi": finding.DOI, "nature": finding.Nature,
				"noticed_at": finding.NoticedAt.UTC().Format(time.RFC3339Nano),
				"notice_doi": finding.NoticeDOI,
			})
		}
		event := notify.Event{Kind: "library.retraction", Message: message, Count: len(newFindings), Detail: map[string]any{"findings": details, "scan_id": scanID}}
		intent := notify.Intent{
			EventKind: "library.retraction", Category: notify.CategoryIntegrityNotice,
			AggregateKey: scanID, Phase: notify.PhaseScan, WindowStart: now,
			ScanID: scanID, HappenedAt: now, Message: message, Detail: event,
		}
		if err := s.notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
			// Notification failures must not alter the committed scan/cache.
			log.Printf("papio: routing retraction notification: %v", err)
		}
	}
	return nil
}

// SnapshotItems supplies the current retraction notices for one consistent
// triage snapshot, minus the ones the user has already acknowledged. The
// notices themselves are an external metadata snapshot rather than SQLite
// state, but the acknowledgements are daemon state, so they are read inside
// the caller's snapshot transaction when one is supplied.
func (s *Sentinel) SnapshotItems(ctx context.Context, tx *sql.Tx) ([]triage.Item, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	cached, ok := s.readCache()
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	acked, err := s.acknowledged(ctx, tx)
	if err != nil {
		return nil, err
	}
	findings := make([]Finding, 0, len(cached.Notices))
	for _, finding := range cached.Notices {
		if acked[findingKey(finding)] {
			continue
		}
		findings = append(findings, finding)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].DOI < findings[j].DOI })
	items := make([]triage.Item, 0, len(findings))
	seenNotices := make(map[string]bool, len(findings))
	for _, finding := range findings {
		if seenNotices[noticeKey(finding)] {
			continue
		}
		seenNotices[noticeKey(finding)] = true
		items = append(items, triage.Item{
			Kind:  triage.KindRetraction,
			ID:    triage.RetractionIDPrefix + finding.DOI,
			Title: "Library update notice",
			Facts: []triage.Fact{{Label: "Nature", Text: string(finding.Nature)}},
			Links: []triage.Link{{Rel: "doi", URL: "https://doi.org/" + finding.DOI}},
			Ops:   []string{"dismiss", "open"},
			Retraction: &triage.Retraction{
				DOI: finding.DOI, Nature: string(finding.Nature), NoticedAt: finding.NoticedAt,
				NoticeDOI: finding.NoticeDOI,
			},
		})
	}
	return items, nil
}

// AcknowledgeRetraction clears one current notice from the inbox. It reports
// whether this call was the one that recorded the acknowledgement; a repeat is
// not an error. An unknown item, or a work with no current notice, reports
// sql.ErrNoRows so callers can render the same conflict they render for a
// vanished watch hit.
func (s *Sentinel) AcknowledgeRetraction(ctx context.Context, itemID string) (bool, error) {
	if s == nil || s.store == nil {
		return false, sql.ErrNoRows
	}
	doi, ok := strings.CutPrefix(itemID, triage.RetractionIDPrefix)
	if !ok || doi == "" {
		return false, sql.ErrNoRows
	}
	s.mu.Lock()
	cached, cacheOK := s.readCache()
	s.mu.Unlock()
	if !cacheOK {
		return false, sql.ErrNoRows
	}
	var target Finding
	found := false
	for _, finding := range cached.Notices {
		if finding.DOI != doi {
			continue
		}
		if !found || prefer(finding, target) {
			target, found = finding, true
		}
	}
	if !found {
		return false, sql.ErrNoRows
	}
	result, err := s.store.DB().ExecContext(ctx, `
		INSERT OR IGNORE INTO retraction_acks (doi, nature, notice_doi, acked_at)
		VALUES (?, ?, ?, ?)`,
		target.DOI, string(target.Nature), target.NoticeDOI, s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("retraction: acknowledge notice: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retraction: acknowledge notice: %w", err)
	}
	return affected > 0, nil
}

// acknowledged reads the acknowledged notice keys. The snapshot transaction is
// preferred so one inbox page cannot mix pre- and post-acknowledgement state;
// callers outside a snapshot pass nil and read the writer connection.
func (s *Sentinel) acknowledged(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	var query func(context.Context, string, ...any) (*sql.Rows, error)
	switch {
	case tx != nil:
		query = tx.QueryContext
	case s.store != nil:
		query = s.store.DB().QueryContext
	default:
		return nil, nil
	}
	rows, err := query(ctx, `SELECT doi, nature, notice_doi FROM retraction_acks`)
	if err != nil {
		return nil, fmt.Errorf("retraction: read acknowledged notices: %w", err)
	}
	defer rows.Close()
	acked := make(map[string]bool)
	for rows.Next() {
		var finding Finding
		var nature string
		if err := rows.Scan(&finding.DOI, &nature, &finding.NoticeDOI); err != nil {
			return nil, err
		}
		finding.Nature = Nature(nature)
		acked[findingKey(finding)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return acked, nil
}

// pruneAcks drops acknowledgements whose notice is no longer current, so the
// table stays bounded by the live notice set and a reissued notice is shown
// again rather than staying silently acknowledged forever.
func (s *Sentinel) pruneAcks(ctx context.Context, current map[string]Finding) error {
	acked, err := s.acknowledged(ctx, nil)
	if err != nil {
		return err
	}
	for key := range acked {
		if _, live := current[key]; live {
			continue
		}
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		if _, err := s.store.DB().ExecContext(ctx, `
			DELETE FROM retraction_acks WHERE doi = ? AND nature = ? AND notice_doi = ?`,
			parts[0], parts[1], parts[2]); err != nil {
			return fmt.Errorf("retraction: prune acknowledged notices: %w", err)
		}
	}
	return nil
}

func (s *Sentinel) readyDOIs(ctx context.Context) ([]string, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT DISTINCT i.value
		  FROM jobs j
		  JOIN identifiers i ON i.work_request_id = j.work_request_id
		  JOIN artifacts a ON a.sha256 = j.artifact_sha256
		 WHERE j.state IN ('ready','imported') AND i.kind = 'doi'
		 ORDER BY i.value`)
	if err != nil {
		return nil, fmt.Errorf("retraction: query ready library DOIs: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	var dois []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		doi, err := work.NormalizeDOI(raw)
		if err == nil && !seen[doi] {
			seen[doi] = true
			dois = append(dois, doi)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(dois)
	return dois, nil
}

type update struct {
	Nature    Nature
	NoticeDOI string
}

type response struct {
	Message struct {
		UpdateTo []struct {
			DOI     string `json:"DOI"`
			Updated string `json:"updated"`
			Label   string `json:"label"`
		} `json:"update-to"`
	} `json:"message"`
}

func (s *Sentinel) lookup(ctx context.Context, doi string) ([]update, error) {
	endpoint, err := url.Parse(s.baseURL)
	if err != nil {
		return nil, errors.New("retraction: invalid configured Crossref endpoint")
	}
	escapedPrefix := strings.TrimRight(endpoint.EscapedPath(), "/")
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + doi
	endpoint.RawPath = escapedPrefix + "/" + url.PathEscape(doi)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("retraction: build Crossref request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &resolver.TemporaryError{Err: errors.New("retraction: Crossref request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("retraction: empty Crossref response")}
	}
	if resp.Body == nil {
		return nil, errors.New("retraction: Crossref response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, temporaryStatus(resp)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return nil, fmt.Errorf("retraction: Crossref returned HTTP %d", resp.StatusCode)
	}
	var payload response
	if err := decodeBoundedJSON(resp.Body, s.maxBody, &payload); err != nil {
		return nil, fmt.Errorf("retraction: invalid Crossref response: %w", err)
	}
	updates := make([]update, 0, len(payload.Message.UpdateTo))
	seen := make(map[string]bool, len(payload.Message.UpdateTo))
	for _, record := range payload.Message.UpdateTo {
		nature, ok := parseNature(record.Updated)
		if !ok {
			nature, ok = parseNature(record.Label)
		}
		if !ok {
			continue
		}
		noticeDOI, err := work.NormalizeDOI(record.DOI)
		if err != nil {
			noticeDOI = ""
		}
		key := string(nature) + "\x00" + noticeDOI
		if seen[key] {
			continue
		}
		seen[key] = true
		updates = append(updates, update{Nature: nature, NoticeDOI: noticeDOI})
	}
	return updates, nil
}

func parseNature(value string) (Nature, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "retraction", "retracted":
		return NatureRetraction, true
	case "correction", "corrigendum", "erratum":
		return NatureCorrection, true
	case "concern", "expression of concern", "expression-of-concern":
		return NatureConcern, true
	default:
		return "", false
	}
}

func prefer(candidate, current Finding) bool {
	if candidate.Nature != current.Nature {
		return candidate.Nature == NatureRetraction || (candidate.Nature == NatureConcern && current.Nature == NatureCorrection)
	}
	return candidate.NoticeDOI < current.NoticeDOI
}

func findingKey(f Finding) string {
	return f.DOI + "\x00" + string(f.Nature) + "\x00" + f.NoticeDOI
}

func validNotices(notices map[string]Finding) map[string]Finding {
	out := make(map[string]Finding, len(notices))
	for key, finding := range notices {
		if key == findingKey(finding) && validFinding(finding) {
			out[key] = finding
		}
	}
	return out
}

func noticeKey(f Finding) string {
	if f.DOI == "" && f.NoticeDOI != "" {
		return f.NoticeDOI
	}
	return findingKey(f)
}

func validFinding(f Finding) bool {
	if _, err := work.NormalizeDOI(f.DOI); err != nil || f.DOI != strings.ToLower(f.DOI) {
		return false
	}
	if f.NoticeDOI != "" {
		if _, err := work.NormalizeDOI(f.NoticeDOI); err != nil || f.NoticeDOI != strings.ToLower(f.NoticeDOI) {
			return false
		}
	}
	if f.NoticedAt.IsZero() {
		return false
	}
	switch f.Nature {
	case NatureRetraction, NatureCorrection, NatureConcern:
		return true
	default:
		return false
	}
}

// integrityNoticeMessage names what one retraction scan found. A single
// finding keeps its DOI-specific identity; a whole scan reuses the shared
// integrity vocabulary in internal/notify rather than maintaining a second
// plural template. Either way the copy names the inbox, where the durable
// notices are recoverable.
func integrityNoticeMessage(findings []Finding) string {
	if len(findings) != 1 {
		return notify.ComposeMessage(notify.CategoryIntegrityNotice, len(findings), notify.Event{}, "")
	}
	return noticeMessage(findings[0])
}

func noticeMessage(f Finding) string {
	if f.NoticeDOI != "" {
		return fmt.Sprintf("Library %s notice for DOI %s (notice DOI %s) — open the papio inbox", f.Nature, f.DOI, f.NoticeDOI)
	}
	return fmt.Sprintf("Library %s notice for DOI %s — open the papio inbox", f.Nature, f.DOI)
}

type cache struct {
	Version   int                `json:"version"`
	CheckedAt time.Time          `json:"checked_at"`
	ScanID    string             `json:"scan_id,omitempty"`
	Notices   map[string]Finding `json:"notices"`
}

func (s *Sentinel) cachePath() string {
	return filepath.Join(s.dataDir, cacheFileName)
}

// readCache requires the caller to hold s.mu so readers cannot observe a cache
// replacement in progress.
func (s *Sentinel) readCache() (cache, bool) {
	data, err := os.ReadFile(s.cachePath())
	if err != nil || int64(len(data)) > defaultMaxBody {
		return cache{}, false
	}
	var cached cache
	if err := decodeBoundedJSON(bytes.NewReader(data), defaultMaxBody, &cached); err != nil ||
		cached.Version != cacheVersion || cached.CheckedAt.IsZero() || len(cached.Notices) > maxNotices {
		return cache{}, false
	}
	cached.Notices = validNotices(cached.Notices)
	return cached, true
}

// writeCache requires the caller to hold s.mu so replacement is atomic to
// SnapshotItems readers.
func (s *Sentinel) writeCache(cached cache) error {
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dataDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, ".retraction-cache-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.cachePath())
}

func decodeBoundedJSON(body io.Reader, maximum int64, destination any) error {
	if maximum <= 0 {
		return errors.New("invalid response limit")
	}
	data, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maximum {
		return fmt.Errorf("response exceeds %d-byte limit", maximum)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func temporaryStatus(resp *http.Response) error {
	return &resolver.TemporaryError{Err: fmt.Errorf("retraction: Crossref returned HTTP %d", resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		const maxDuration = time.Duration(1<<63 - 1)
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return time.Until(when)
	}
	return 0
}
