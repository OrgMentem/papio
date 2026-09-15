// Copyright 2026 OrgMentem. Licensed under MIT.

// Package retraction monitors the Crossref Retraction Watch dataset for ready
// works and, when configured, every DOI in the user's library.
//
// The source choice follows two bounded live probes made on 2026-09-15.
// The proposed Crossref works sweep returned these fragments verbatim:
//
//	HTTP/2 400
//	"type":"select-not-available","value":"updated"
//	Select 'updated' specified but there is no such select for this route.
//
// Thus the proposed select shape did not provide a verified incremental
// watermark. The Retraction Watch dataset probe used
// `curl --max-time 20 -r 0-65535` and returned these fragments verbatim:
//
//	HTTP/2 200
//	content-disposition: attachment; filename=retractions.csv
//	Record ID,Title,Subject,Institution,Journal,Publisher,Country,Author,URLS,ArticleType,RetractionDate,RetractionDOI,RetractionPubMedID,OriginalPaperDate,OriginalPaperDOI,OriginalPaperPubMedID,RetractionNature,Reason,Paywalled,Notes,
//
// The server ignored the byte range and sent 66.7 MB. The 256 MiB response
// limit admits that measured dataset with headroom. One daily request replaces
// a request for every corpus DOI, and the last valid CSV remains available when
// a later fetch fails.
package retraction

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
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
	defaultBaseURL = "https://api.labs.crossref.org/data/retractionwatch"
	// DefaultMaxResponseBytes admits the 66.7 MB, 72,526-row dataset measured
	// on 2026-09-15 with enough headroom for sustained growth.
	DefaultMaxResponseBytes int64 = 256 << 20
	maxCacheBody            int64 = 1 << 20
	cacheFileName                 = "retraction-cache.json"
	datasetFileName               = "retraction-watch.csv"
	sweepEvery                    = 24 * time.Hour
	maxNotices                    = 1000
	cacheVersion                  = 1
	maxDatasetFieldBytes          = 64 << 10
	maxDatasetRecordBytes         = 1 << 20
	maxDatasetColumns             = 64
)

// Nature classifies an update notice recognized by the sentinel.
type Nature string

const (
	NatureRetraction Nature = "retraction"
	NatureCorrection Nature = "correction"
	NatureConcern    Nature = "concern"
)

// Finding is one current Crossref update notice for a monitored library work.
type Finding struct {
	DOI       string    `json:"doi"`
	Nature    Nature    `json:"nature"`
	NoticedAt time.Time `json:"noticed_at"`
	NoticeDOI string    `json:"notice_doi,omitempty"`
	Title     string    `json:"title,omitempty"`
}

// HTTPClient is the injected dependency used for Crossref metadata requests.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// BudgetAcquirer is the budget surface needed before every metadata request.
type BudgetAcquirer interface {
	Acquire(context.Context, string, config.Source, float64) error
}

// LibraryDOILister supplies every configured library DOI when library scope is
// enabled.
type LibraryDOILister interface {
	LibraryDOIs(context.Context) ([]string, error)
}

type libraryDOITitleProvider interface {
	libraryDOITitle(string) string
}

type Options struct {
	Store            *store.Store
	Budgets          BudgetAcquirer
	Policy           config.Source
	Client           HTTPClient
	DataDir          string
	BaseURL          string
	ContactEmail     string
	MaxResponseBytes int64
	Notifier         notify.Sink
	Scope            string
	LibraryDOIs      LibraryDOILister
	Now              func() time.Time
}

// Sentinel performs at most one Crossref sweep each day and provides the
// cached current notices to the triage read model.
type Sentinel struct {
	store       *store.Store
	budgets     BudgetAcquirer
	policy      config.Source
	client      HTTPClient
	dataDir     string
	baseURL     string
	email       string
	maxBody     int64
	notifier    notify.Sink
	scope       string
	libraryDOIs LibraryDOILister
	now         func() time.Time

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
		maxBody = DefaultMaxResponseBytes
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Sentinel{
		store: options.Store, budgets: options.Budgets, policy: options.Policy,
		client: client, dataDir: options.DataDir, baseURL: baseURL,
		email: strings.TrimSpace(options.ContactEmail), maxBody: maxBody,
		notifier: options.Notifier, scope: options.Scope,
		libraryDOIs: options.LibraryDOIs, now: now,
	}
}

// RunDue performs one daily Retraction Watch dataset request when the source
// policy is enabled. The corpus is matched locally, so request count does not
// grow with library size. A failed fetch uses the last valid dataset; without
// one, the current notice cache remains intact.
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
	latestAttempt := cached.CheckedAt
	if cached.LastFetchErrorAt.After(latestAttempt) {
		latestAttempt = cached.LastFetchErrorAt
	}
	fresh := ok && !latestAttempt.IsZero() && now.Sub(latestAttempt) < sweepEvery
	s.mu.Unlock()
	if fresh {
		return nil
	}

	corpus, err := s.corpusWorks(ctx)
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
	var lastFetchErr error
	if len(corpus) != 0 {
		if err := s.budgets.Acquire(ctx, config.SourceRetractionWatch, s.policy, 0); err != nil {
			return fmt.Errorf("retraction: acquire Retraction Watch budget: %w", err)
		}
		updates, sourceErr, lookupErr := s.lookup(ctx, corpus)
		if lookupErr != nil {
			s.recordFetchFailure(cached, ok, now, lookupErr)
			return lookupErr
		}
		lastFetchErr = sourceErr
		for doi, records := range updates {
			local := corpus[doi]
			for _, update := range records {
				title := local.Title
				if title == "" {
					title = update.Title
				}
				finding := Finding{DOI: doi, Nature: update.Nature, NoticeDOI: update.NoticeDOI, Title: title}
				key := findingKey(finding)
				if old, exists := previous[key]; exists {
					finding.NoticedAt = old.NoticedAt
					if finding.Title == "" {
						finding.Title = old.Title
					}
				} else {
					finding.NoticedAt = now
				}
				addCurrent(finding)
			}
		}
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
	nextCache := cache{Version: cacheVersion, CheckedAt: now, ScanID: scanID, Notices: notices}
	if lastFetchErr != nil {
		nextCache.LastFetchError = boundedFetchError(lastFetchErr)
		nextCache.LastFetchErrorAt = now
	}
	if err := s.writeCache(nextCache); err != nil {
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
		title := strings.TrimSpace(finding.Title)
		if title == "" {
			title = "Library update notice"
		}
		items = append(items, triage.Item{
			Kind:  triage.KindRetraction,
			ID:    triage.RetractionIDPrefix + finding.DOI,
			Title: title,
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

type readyWork struct {
	DOI   string
	Title string
}

func (s *Sentinel) readyWorks(ctx context.Context) ([]readyWork, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT i.value, COALESCE(w.title, '')
		  FROM jobs j
		  JOIN work_requests w ON w.id = j.work_request_id
		  JOIN identifiers i ON i.work_request_id = j.work_request_id
		  JOIN artifacts a ON a.sha256 = j.artifact_sha256
		 WHERE j.state IN ('ready','imported') AND i.kind = 'doi'
		 ORDER BY i.value, w.title`)
	if err != nil {
		return nil, fmt.Errorf("retraction: query ready library DOIs: %w", err)
	}
	defer rows.Close()
	byDOI := make(map[string]readyWork)
	for rows.Next() {
		var raw, title string
		if err := rows.Scan(&raw, &title); err != nil {
			return nil, err
		}
		doi, err := work.NormalizeDOI(raw)
		if err != nil {
			continue
		}
		title = strings.TrimSpace(title)
		ready, exists := byDOI[doi]
		if !exists || (ready.Title == "" && title != "") {
			byDOI[doi] = readyWork{DOI: doi, Title: title}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	works := make([]readyWork, 0, len(byDOI))
	for _, ready := range byDOI {
		works = append(works, ready)
	}
	sort.Slice(works, func(i, j int) bool { return works[i].DOI < works[j].DOI })
	return works, nil
}
func (s *Sentinel) corpusWorks(ctx context.Context) (map[string]readyWork, error) {
	ready, err := s.readyWorks(ctx)
	if err != nil {
		return nil, err
	}
	corpus := make(map[string]readyWork, len(ready))
	for _, item := range ready {
		corpus[item.DOI] = item
	}
	if s.scope != config.RetractionScopeLibrary || s.libraryDOIs == nil {
		return corpus, nil
	}
	dois, err := s.libraryDOIs.LibraryDOIs(ctx)
	if err != nil {
		return nil, fmt.Errorf("retraction: enumerate library DOIs: %w", err)
	}
	titles, _ := s.libraryDOIs.(libraryDOITitleProvider)
	for _, raw := range dois {
		doi, err := work.NormalizeDOI(raw)
		if err != nil {
			continue
		}
		title := ""
		if titles != nil {
			title = strings.TrimSpace(titles.libraryDOITitle(doi))
		}
		if current, exists := corpus[doi]; !exists || (current.Title == "" && title != "") {
			corpus[doi] = readyWork{DOI: doi, Title: title}
		}
	}
	return corpus, nil
}

type update struct {
	Nature    Nature
	NoticeDOI string
	Title     string
}

func (s *Sentinel) lookup(ctx context.Context, corpus map[string]readyWork) (map[string][]update, error, error) {
	downloaded, sourceErr := s.downloadDataset(ctx)
	if sourceErr == nil {
		updates, rowCount, parseErr := parseDatasetFile(downloaded, corpus)
		if parseErr != nil || rowCount == 0 {
			sourceErr = fmt.Errorf("retraction: invalid Retraction Watch dataset: %w", datasetValidationError(parseErr, rowCount))
		} else if err := os.Rename(downloaded, s.datasetPath()); err != nil {
			sourceErr = fmt.Errorf("retraction: cache Retraction Watch dataset: %w", err)
		} else {
			return updates, nil, nil
		}
		_ = os.Remove(downloaded)
	}
	var updates map[string][]update
	var rowCount int
	info, cacheErr := os.Stat(s.datasetPath())
	if cacheErr == nil && info.Size() > s.maxBody {
		cacheErr = fmt.Errorf("dataset exceeds %d-byte limit", s.maxBody)
	}
	if cacheErr == nil {
		updates, rowCount, cacheErr = parseDatasetFile(s.datasetPath(), corpus)
	}
	if cacheErr != nil || rowCount == 0 {
		return nil, sourceErr, fmt.Errorf("%w; cached Retraction Watch dataset is invalid: %v", sourceErr, datasetValidationError(cacheErr, rowCount))
	}
	log.Printf("papio: Retraction Watch fetch failed; using last known-good dataset: %v", sourceErr)
	return updates, sourceErr, nil
}

func (s *Sentinel) downloadDataset(ctx context.Context) (string, error) {
	endpoint, err := url.Parse(s.baseURL)
	if err != nil {
		return "", errors.New("retraction: invalid configured Retraction Watch endpoint")
	}
	if s.email != "" {
		query := endpoint.Query()
		query.Set("mailto", s.email)
		endpoint.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("retraction: build Retraction Watch request: %w", err)
	}
	req.Header.Set("Accept", "text/csv")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &resolver.TemporaryError{Err: errors.New("retraction: Retraction Watch request failed")}
	}
	if resp == nil {
		return "", &resolver.TemporaryError{Err: errors.New("retraction: empty Retraction Watch response")}
	}
	if resp.Body == nil {
		return "", errors.New("retraction: Retraction Watch response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return "", temporaryStatus(resp)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return "", fmt.Errorf("retraction: Retraction Watch returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > s.maxBody {
		return "", fmt.Errorf("retraction: Retraction Watch response exceeds %d-byte limit", s.maxBody)
	}
	if contentType := strings.TrimSpace(resp.Header.Get("Content-Type")); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || (mediaType != "text/csv" && mediaType != "application/csv" && mediaType != "application/octet-stream") {
			return "", fmt.Errorf("retraction: unexpected Retraction Watch content type %q", contentType)
		}
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(s.dataDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.dataDir, ".retraction-watch-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, s.maxBody+1))
	if err != nil {
		return "", fmt.Errorf("retraction: read Retraction Watch response: %w", err)
	}
	if written > s.maxBody {
		return "", fmt.Errorf("retraction: Retraction Watch response exceeds %d-byte limit", s.maxBody)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}

func parseDatasetFile(path string, corpus map[string]readyWork) (map[string][]update, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = file.Close() }()
	if err := validateDatasetShape(file); err != nil {
		return nil, 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return nil, 0, err
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[strings.TrimSpace(name)] = i
	}
	required := []string{"Title", "RetractionDOI", "OriginalPaperDOI", "RetractionNature"}
	for _, name := range required {
		if _, ok := columns[name]; !ok {
			return nil, 0, fmt.Errorf("missing %q column", name)
		}
	}
	value := func(row []string, name string) string {
		index := columns[name]
		if index >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[index])
	}
	updates := make(map[string][]update)
	seen := make(map[string]bool)
	rowCount := 0
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, rowCount, err
		}
		nonempty := false
		for _, field := range row {
			if strings.TrimSpace(field) != "" {
				nonempty = true
				break
			}
		}
		if !nonempty {
			continue
		}
		rowCount++
		doi, err := work.NormalizeDOI(value(row, "OriginalPaperDOI"))
		if err != nil {
			continue
		}
		if _, wanted := corpus[doi]; !wanted {
			continue
		}
		nature, ok := parseNature(value(row, "RetractionNature"))
		if !ok {
			continue
		}
		noticeDOI, err := work.NormalizeDOI(value(row, "RetractionDOI"))
		if err != nil {
			noticeDOI = ""
		}
		key := doi + "\x00" + string(nature) + "\x00" + noticeDOI
		if seen[key] {
			continue
		}
		seen[key] = true
		updates[doi] = append(updates[doi], update{
			Nature: nature, NoticeDOI: noticeDOI, Title: value(row, "Title"),
		})
	}
	return updates, rowCount, nil
}

func validateDatasetShape(body io.Reader) error {
	reader := bufio.NewReaderSize(body, 64<<10)
	fieldBytes := 0
	recordBytes := 0
	columns := 1
	inQuotes := false
	atFieldStart := true
	addFieldByte := func() error {
		fieldBytes++
		if fieldBytes > maxDatasetFieldBytes {
			return fmt.Errorf("CSV field exceeds %d-byte limit", maxDatasetFieldBytes)
		}
		return nil
	}
	for {
		char, err := reader.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		recordBytes++
		if recordBytes > maxDatasetRecordBytes {
			return fmt.Errorf("CSV record exceeds %d-byte limit", maxDatasetRecordBytes)
		}
		if inQuotes {
			if char == '"' {
				next, peekErr := reader.Peek(1)
				if peekErr == nil && next[0] == '"' {
					if _, err := reader.ReadByte(); err != nil {
						return err
					}
					recordBytes++
					if recordBytes > maxDatasetRecordBytes {
						return fmt.Errorf("CSV record exceeds %d-byte limit", maxDatasetRecordBytes)
					}
					if err := addFieldByte(); err != nil {
						return err
					}
				} else {
					if peekErr != nil && peekErr != io.EOF {
						return peekErr
					}
					inQuotes = false
				}
				continue
			}
			if err := addFieldByte(); err != nil {
				return err
			}
			continue
		}
		switch char {
		case '"':
			if atFieldStart {
				inQuotes = true
			} else if err := addFieldByte(); err != nil {
				return err
			}
			atFieldStart = false
		case ',':
			columns++
			if columns > maxDatasetColumns {
				return fmt.Errorf("CSV record exceeds %d-column limit", maxDatasetColumns)
			}
			fieldBytes = 0
			atFieldStart = true
		case '\n':
			fieldBytes = 0
			recordBytes = 0
			columns = 1
			atFieldStart = true
		case '\r':
			next, peekErr := reader.Peek(1)
			if peekErr != nil && peekErr != io.EOF {
				return peekErr
			}
			if len(next) == 0 || next[0] != '\n' {
				if err := addFieldByte(); err != nil {
					return err
				}
				atFieldStart = false
			}
		default:
			if err := addFieldByte(); err != nil {
				return err
			}
			atFieldStart = false
		}
	}
	if inQuotes {
		return errors.New("CSV record has an unterminated quoted field")
	}
	return nil
}

func datasetValidationError(parseErr error, rowCount int) error {
	if parseErr != nil {
		return parseErr
	}
	if rowCount == 0 {
		return errors.New("dataset contains no data rows")
	}
	return nil
}

func (s *Sentinel) datasetPath() string {
	return filepath.Join(s.dataDir, datasetFileName)
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
	Version          int                `json:"version"`
	CheckedAt        time.Time          `json:"checked_at"`
	ScanID           string             `json:"scan_id,omitempty"`
	Notices          map[string]Finding `json:"notices"`
	LastFetchError   string             `json:"last_fetch_error,omitempty"`
	LastFetchErrorAt time.Time          `json:"last_fetch_error_at,omitempty"`
}

func (s *Sentinel) cachePath() string {
	return filepath.Join(s.dataDir, cacheFileName)
}
func (s *Sentinel) recordFetchFailure(cached cache, valid bool, at time.Time, fetchErr error) {
	if !valid {
		cached = cache{Version: cacheVersion, Notices: map[string]Finding{}}
	}
	cached.LastFetchError = boundedFetchError(fetchErr)
	cached.LastFetchErrorAt = at
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeCache(cached); err != nil {
		log.Printf("papio: recording Retraction Watch fetch failure: %v", err)
	}
}

func boundedFetchError(err error) string {
	if err == nil {
		return ""
	}
	const maximumRunes = 512
	message := []rune(err.Error())
	if len(message) > maximumRunes {
		message = message[:maximumRunes]
	}
	return string(message)
}

// readCache requires the caller to hold s.mu so readers cannot observe a cache
// replacement in progress.
func (s *Sentinel) readCache() (cache, bool) {
	data, err := os.ReadFile(s.cachePath())
	if err != nil || int64(len(data)) > maxCacheBody {
		return cache{}, false
	}
	var cached cache
	if err := decodeBoundedJSON(bytes.NewReader(data), maxCacheBody, &cached); err != nil ||
		cached.Version != cacheVersion ||
		(cached.CheckedAt.IsZero() && cached.LastFetchErrorAt.IsZero()) ||
		len(cached.Notices) > maxNotices {
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
