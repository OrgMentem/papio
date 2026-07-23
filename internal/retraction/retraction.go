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

// Options configures a Sentinel. DataDir, Store, and Budgets are production
// dependencies; the remaining fields make the bounded network operation
// deterministic in tests.
type Options struct {
	Store            *store.Store
	Budgets          BudgetAcquirer
	Policy           config.Source
	Client           HTTPClient
	DataDir          string
	BaseURL          string
	MaxResponseBytes int64
	Notifier         notify.Sender
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
	notifier notify.Sender
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
	if err := s.writeCache(cache{Version: cacheVersion, CheckedAt: now, Notices: notices}); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("retraction: write cache: %w", err)
	}
	seenNotices := make(map[string]bool, len(previous))
	for _, finding := range previous {
		seenNotices[noticeKey(finding)] = true
	}
	for _, finding := range findings {
		key := noticeKey(finding)
		if seenNotices[key] {
			continue
		}
		seenNotices[key] = true
		notify.Emit(ctx, s.notifier, notify.Event{
			Kind:    "library.retraction",
			Message: noticeMessage(finding),
			Count:   1,
		})
	}
	s.mu.Unlock()
	return nil
}

// SnapshotItems supplies the current retraction notices for one consistent
// triage snapshot. The transaction is intentionally unused: this cache is an
// external metadata snapshot, not SQLite state, and it is validated before
// conversion to the triage model.
func (s *Sentinel) SnapshotItems(_ context.Context, _ *sql.Tx) ([]triage.Item, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cached, ok := s.readCache()
	if !ok {
		return nil, nil
	}
	findings := make([]Finding, 0, len(cached.Notices))
	for _, finding := range cached.Notices {
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
			ID:    "retraction:" + finding.DOI,
			Title: "Library update notice",
			Facts: []triage.Fact{{Label: "Nature", Text: string(finding.Nature)}},
			Links: []triage.Link{{Rel: "doi", URL: "https://doi.org/" + finding.DOI}},
			Retraction: &triage.Retraction{
				DOI: finding.DOI, Nature: string(finding.Nature), NoticedAt: finding.NoticedAt,
				NoticeDOI: finding.NoticeDOI,
			},
		})
	}
	return items, nil
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
	if f.NoticeDOI != "" {
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

func noticeMessage(f Finding) string {
	if f.NoticeDOI != "" {
		return fmt.Sprintf("Library %s notice for DOI %s (notice DOI %s)", f.Nature, f.DOI, f.NoticeDOI)
	}
	return fmt.Sprintf("Library %s notice for DOI %s", f.Nature, f.DOI)
}

type cache struct {
	Version   int                `json:"version"`
	CheckedAt time.Time          `json:"checked_at"`
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
