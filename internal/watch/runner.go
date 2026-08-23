// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/notify"
	"papio/internal/ownership"
	"papio/internal/protocol"
	"papio/internal/work"
	"papio/internal/zotio"
)

// Discovery searches the existing bounded discovery surface.
type Discovery interface {
	Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error)
}

// OwnershipLookup is the Zotio deduplication surface used before submitting
// acquisition work.
type OwnershipLookup interface {
	LookupWorks(context.Context, zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error)
}

// HoldingsLookup is the generic (non-Zotero) ownership surface. It is consulted
// for acquisition watches when configured (ADR-0008).
type HoldingsLookup interface {
	Enabled() bool
	Lookup(context.Context, []ownership.Query) ownership.Result
}

// Submitter is the application's normal acquisition submission path. A nil
// auto-import override retains the configured Zotio auto-import policy.
type Submitter interface {
	SubmitWithAutoImport(context.Context, protocol.WorkRequest, *bool) (string, error)
}

// BackfillQueue queues Zotio items missing PDFs through its idempotent path.
type BackfillQueue interface {
	QueueMissingPDF(context.Context, zotio.QueueOptions) (*zotio.QueueResult, error)
}

// Notifier is the daemon's shared typed notification router.

// RunResult is the outcome of one forced or scheduled watch execution.
type RunResult struct {
	WatchID             int64  `json:"watch_id"`
	Queued              int    `json:"queued"`
	Reported            int    `json:"reported,omitempty"`
	Failed              int    `json:"failed"`
	ManifestID          string `json:"manifest_id,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	Disabled            bool   `json:"disabled"`
}

// Runner composes the existing discovery, Zotio ownership, acquisition batch,
// and desktop notification services for a single watch execution at a time.
// Generic holdings apply only to acquisition watches; alert watches retain their
// established Zotio ownership path.
type Runner struct {
	Store     *Store
	Discovery Discovery
	Lookup    OwnershipLookup
	// Holdings answers acquisition ownership when configured; nil or disabled
	// leaves the Zotio path exactly as it was.
	Holdings  HoldingsLookup
	Submitter Submitter
	Backfill  BackfillQueue
	Notifier  notify.Sink
	DataDir   string
	Now       func() time.Time

	mu sync.Mutex
}

// Run force-runs one watch now, including a disabled watch. It is the explicit
// recovery/test lever; disabled watches are never selected by RunDue.
func (r *Runner) Run(ctx context.Context, id int64) (*RunResult, error) {
	if r == nil || r.Store == nil {
		return nil, errors.New("watch runner is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	watch, err := r.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.runWatch(ctx, *watch)
}

func (r *Runner) AcquireDigest(ctx context.Context, watchID int64, keys []string) (queued int, err error) {
	if r == nil || r.Store == nil || r.Lookup == nil || r.Submitter == nil {
		return 0, errors.New("watch runner dependencies are not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acquireDigestLocked(ctx, watchID, keys)
}

func (r *Runner) acquireDigestLocked(ctx context.Context, watchID int64, keys []string) (queued int, err error) {
	watch, err := r.Store.Get(ctx, watchID)
	if err != nil {
		return 0, err
	}
	if watch.Kind != KindDiscovery {
		return 0, fmt.Errorf("watch %d is not a discovery watch", watchID)
	}
	entries, err := r.Store.TakeDigest(ctx, watchID, keys)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	return r.acquireDigestWithEntriesLocked(ctx, watchID, entries)
}

// acquireDigestWithEntriesLocked is the work of AcquireDigest but operating on
// pre-resolved entries. Caller must hold r.mu.
func (r *Runner) acquireDigestWithEntriesLocked(ctx context.Context, watchID int64, entries []DigestEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	watch, err := r.Store.Get(ctx, watchID)
	if err != nil {
		return 0, err
	}
	if watch.Kind != KindDiscovery {
		return 0, fmt.Errorf("watch %d is not a discovery watch", watchID)
	}
	works := make([]protocol.WorkRequest, len(entries))
	lookupRequest := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(entries))}
	for i, entry := range entries {
		works[i], err = workRequestForDigest(entry)
		if err != nil {
			return 0, err
		}
		if works[i].Identifiers != nil {
			lookupRequest.Works[i] = zotio.LookupWork{
				DOI:   works[i].Identifiers.DOI,
				ArXiv: works[i].Identifiers.ArXiv,
			}
		}
		if lookupRequest.Works[i].DOI == "" && lookupRequest.Works[i].ArXiv == "" {
			return 0, fmt.Errorf("watch digest entry %q cannot be authoritatively classified without a DOI or arXiv ID", entry.WorkKey)
		}
	}
	ownership, err := r.Lookup.LookupWorks(ctx, lookupRequest)
	if err != nil {
		return 0, fmt.Errorf("Zotio ownership lookup: %w", err)
	}
	if ownership == nil || len(ownership.Works) != len(entries) {
		return 0, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", ownershipCount(ownership), len(entries))
	}
	if strings.TrimSpace(ownership.StalenessWarning) != "" {
		return 0, fmt.Errorf("Zotio ownership lookup is stale: %s", ownership.StalenessWarning)
	}

	eligibleEntries := make([]DigestEntry, 0, len(entries))
	eligibleWorks := make([]protocol.WorkRequest, 0, len(works))
	consumedEntries := make([]DigestEntry, 0, len(entries))
	for i, classification := range ownership.Works {
		switch classification.Status {
		case zotio.OwnershipNotOwned:
			eligibleEntries = append(eligibleEntries, entries[i])
			eligibleWorks = append(eligibleWorks, works[i])
		case zotio.OwnershipOwnedWithPDF, zotio.OwnershipOwnedMissingPDF:
			consumedEntries = append(consumedEntries, entries[i])
		default:
			return 0, fmt.Errorf("Zotio ownership result %d has unknown status %q", i+1, classification.Status)
		}
	}
	for _, entry := range consumedEntries {
		if err := r.Store.consumeDigestEntry(ctx, watchID, entry.WorkKey); err != nil {
			return 0, err
		}
	}
	if len(eligibleEntries) == 0 {
		return 0, nil
	}

	manifest := batch.NewManifest(eligibleWorks, "watch: "+watch.Label, watch.Collection, r.now())
	for i := range manifest.Works {
		requestID := batch.RequestID(fmt.Sprintf("watch-%d", watch.ID), manifest.Works[i].Work)
		manifest.Works[i].RequestID = requestID
		manifest.Works[i].Work.RequestID = requestID
	}
	// Make the manifest resumable: if a prior attempt left a durable manifest
	// with JobIDs for the same requestIDs, reuse them so a retry does not
	// resubmit already-created jobs. This covers the window where a job was
	// created but its post-submit manifest write failed.
	if existing, loadErr := batch.Load(r.DataDir, manifest.ID); loadErr == nil {
		existingByRequest := make(map[string]string, len(existing.Works))
		for _, w := range existing.Works {
			if w.JobID != "" && w.Status != "submission_failed" {
				existingByRequest[w.RequestID] = w.JobID
			}
		}
		for i := range manifest.Works {
			if jobID, ok := existingByRequest[manifest.Works[i].RequestID]; ok {
				manifest.Works[i].JobID = jobID
			}
		}
	}
	// Persist the manifest before any submission so a later manifest-write
	// failure does not leave already-created jobs without a durable record.
	// Each successful submission is then persisted incrementally, making a
	// retry resumable without resubmitting jobs whose JobIDs are already
	// durable.
	if err := batch.Write(r.DataDir, manifest); err != nil {
		return 0, err
	}
	autoImport := true
	succeeded := make([]DigestEntry, 0, len(eligibleEntries))
	queued := 0
	for i := range manifest.Works {
		if manifest.Works[i].JobID != "" {
			queued++
			succeeded = append(succeeded, eligibleEntries[i])
			continue
		}
		jobID, submitErr := r.Submitter.SubmitWithAutoImport(ctx, manifest.Works[i].Work, &autoImport)
		if submitErr != nil {
			manifest.Works[i].Status = "submission_failed"
			manifest.Works[i].Error = "submit"
			manifest.Works = manifest.Works[:i+1]
			if writeErr := batch.Write(r.DataDir, manifest); writeErr != nil {
				return queued, fmt.Errorf("%w (writing digest manifest: %w)", submitErr, writeErr)
			}
			for _, entry := range succeeded {
				if err := r.Store.consumeDigestEntry(ctx, watchID, entry.WorkKey); err != nil {
					return queued, err
				}
			}
			return queued, submitErr
		}
		manifest.Works[i].JobID = jobID
		queued++
		succeeded = append(succeeded, eligibleEntries[i])
		if err := batch.Write(r.DataDir, manifest); err != nil {
			return queued, err
		}
	}
	for _, entry := range succeeded {
		if err := r.Store.consumeDigestEntry(ctx, watchID, entry.WorkKey); err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// DigestTarget names one watch digest entry by watch and work key.
type DigestTarget struct {
	WatchID int64
	WorkKey string
}

// AcquireDigests acquires one digest entry per target atomically with respect to
// conflict reporting: if any target is missing (ErrDigestEntryNotFound / sql.ErrNoRows)
// no target's entry is consumed. It holds one mutex across the whole operation so
// the API's multi-watch decide either applies everywhere or nowhere.
func (r *Runner) AcquireDigests(ctx context.Context, targets []DigestTarget) error {
	if r == nil || r.Store == nil || r.Lookup == nil || r.Submitter == nil {
		return errors.New("watch runner dependencies are not configured")
	}
	if len(targets) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	validated := make([][]DigestEntry, len(targets))
	for i, t := range targets {
		entries, err := r.Store.TakeDigest(ctx, t.WatchID, []string{t.WorkKey})
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return fmt.Errorf("%w: %q", ErrDigestEntryNotFound, t.WorkKey)
		}
		validated[i] = entries
	}
	for i, t := range targets {
		if _, err := r.acquireDigestWithEntriesLocked(ctx, t.WatchID, validated[i]); err != nil {
			return err
		}
	}
	return nil
}

// ConsumeDigests consumes one digest entry per target atomically: a conflict on
// any target leaves no entry consumed.
func (r *Runner) ConsumeDigests(ctx context.Context, targets []DigestTarget) error {
	if r == nil || r.Store == nil {
		return errors.New("watch runner is not configured")
	}
	if len(targets) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	validated := make([]DigestEntry, len(targets))
	for i, t := range targets {
		entries, err := r.Store.TakeDigest(ctx, t.WatchID, []string{t.WorkKey})
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return fmt.Errorf("%w: %q", ErrDigestEntryNotFound, t.WorkKey)
		}
		validated[i] = entries[0]
	}
	watchIDs := make([]int64, len(targets))
	for i, t := range targets {
		watchIDs[i] = t.WatchID
	}
	return r.Store.consumeDigestEntriesTx(ctx, validated, watchIDs)
}

// ClearDigest consumes all pending alert discoveries while serializing with
// acquisition.
func (r *Runner) ClearDigest(ctx context.Context, watchID int64) (int, error) {
	if r == nil || r.Store == nil {
		return 0, errors.New("watch runner is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Store.ClearDigest(ctx, watchID)
}

// RunDue serially executes all watches due at the current time. Per-watch
// failures are recorded by runWatch and intentionally do not stop later due
// watches or crash the daemon scheduler.
func (r *Runner) RunDue(ctx context.Context) error {
	if r == nil || r.Store == nil {
		return errors.New("watch runner is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	watches, err := r.Store.Due(ctx, r.now())
	if err != nil {
		return err
	}
	for _, watch := range watches {
		_, _ = r.runWatch(ctx, watch)
	}
	return nil
}

func (r *Runner) runWatch(ctx context.Context, watch Watch) (*RunResult, error) {
	runStart := r.now().UTC()
	result, err := r.executeAt(ctx, watch, runStart)
	if err == nil {
		return result, nil
	}
	failure, recordErr := r.Store.RecordFailure(ctx, watch.ID, runStart, err)
	if recordErr != nil {
		return result, fmt.Errorf("%w (recording watch failure: %w)", err, recordErr)
	}
	result.ConsecutiveFailures = failure.ConsecutiveFailures
	result.Disabled = failure.Disabled
	if failure.Disabled && r.Notifier != nil {
		message := fmt.Sprintf("watch %s disabled after %s", watch.Label, countedNoun(failure.ConsecutiveFailures, "consecutive failure", "consecutive failures"))
		r.route(ctx, notify.Intent{
			EventKind: "watch.disabled", Category: notify.CategorySystemDegraded,
			AggregateKey: fmt.Sprintf("watch:%d:failure", watch.ID), Phase: notify.PhaseEpisode,
			WindowStart: runStart, HappenedAt: runStart, Message: message,
			Detail: notify.Event{Kind: "watch.disabled", Message: message, WatchID: watch.ID, WatchLabel: watch.Label, Count: failure.ConsecutiveFailures},
		})
	}
	return result, err
}

func (r *Runner) route(ctx context.Context, intent notify.Intent) {
	if r == nil || r.Notifier == nil {
		return
	}
	if err := r.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
		log.Printf("papio: routing watch notification: %v", err)
	}
}

// countedNoun renders an exact quantity with a noun that agrees with it. Watch
// notices carry real counts, and a single discovery must not read as
// unfinished plural copy.
func countedNoun(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", count, plural)
}

func (r *Runner) executeAt(ctx context.Context, watch Watch, runStart time.Time) (*RunResult, error) {
	result, err := r.executeBody(ctx, watch, runStart)
	return result, err
}
func (r *Runner) watchIntent(watch Watch, runStart time.Time, event notify.Event, category notify.Category) notify.Intent {
	return notify.Intent{
		EventKind: event.Kind, Category: category,
		AggregateKey: fmt.Sprintf("watch:%d:%s", watch.ID, runStart.UTC().Format(time.RFC3339Nano)),
		Phase:        notify.PhaseDigest, WindowStart: runStart.UTC(),
		HappenedAt: runStart.UTC(), Message: event.Message, Detail: event,
		ScanID: fmt.Sprintf("watch:%d:%s", watch.ID, runStart.UTC().Format(time.RFC3339Nano)),
	}
}
func (r *Runner) executeBody(ctx context.Context, watch Watch, runStart time.Time) (*RunResult, error) {
	result := &RunResult{WatchID: watch.ID}
	if watch.Kind == KindBackfill {
		if r.Backfill == nil {
			return result, errors.New("watch runner backfill dependency is not configured")
		}
		queued, err := r.Backfill.QueueMissingPDF(ctx, zotio.QueueOptions{
			Collection: watch.Collection,
			Limit:      watch.PerRunCap,
		})
		if err != nil {
			return result, fmt.Errorf("queueing missing PDFs: %w", err)
		}
		if queued == nil {
			return result, errors.New("queueing missing PDFs returned no result")
		}
		result.Queued = len(queued.Queued)
		if err := r.Store.MarkRun(ctx, watch.ID, runStart); err != nil {
			return result, err
		}
		if result.Queued > 0 {
			r.route(ctx, r.watchIntent(watch, runStart, notify.Event{
				Kind:    "watch.backfill",
				Message: fmt.Sprintf("watch %s: %s queued", watch.Label, countedNoun(result.Queued, "missing PDF", "missing PDFs")),
				WatchID: watch.ID, WatchLabel: watch.Label, Count: result.Queued,
			}, notify.CategoryDiscoveryNew))
		}
		return result, nil
	}
	if watch.Kind != KindDiscovery {
		return result, fmt.Errorf("unknown watch kind %q", watch.Kind)
	}
	if watch.Mode != ModeAcquire && watch.Mode != ModeAlert {
		return result, fmt.Errorf("unknown watch mode %q", watch.Mode)
	}
	// Acquisition may use either ownership authority; alert watches retain their
	// historical Zotio ownership dependency regardless of generic holdings.
	needsZotio := watch.Mode == ModeAlert || !r.holdingsEnabled()
	if r.Discovery == nil || (needsZotio && r.Lookup == nil) || (watch.Mode == ModeAcquire && r.Submitter == nil) {
		return result, errors.New("watch runner dependencies are not configured")
	}
	works, err := r.Discovery.Search(ctx, discovery.SearchParams{
		Query: watch.Query, Limit: min(watch.PerRunCap*3, 25), Slim: true,
		YearFrom: watch.Filters.YearFrom, YearTo: watch.Filters.YearTo, OAOnly: watch.Filters.OAOnly,
		Cites: watch.Filters.Cites, CitedBy: watch.Filters.CitedBy, RelatedTo: watch.Filters.RelatedTo,
	})
	if err != nil {
		return result, fmt.Errorf("discovery search: %w", err)
	}
	requests := requestsForDiscoveredWithWork(works)
	if len(requests) == 0 {
		return result, r.Store.MarkRun(ctx, watch.ID, runStart)
	}
	var queued []discoveredRequest
	if watch.Mode == ModeAcquire && r.holdingsEnabled() {
		queued, err = r.selectUnheldRequests(ctx, requests, watch.PerRunCap)
		if err != nil {
			return result, err
		}
	} else {
		lookupRequest := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(requests))}
		for i, request := range requests {
			if request.Work.Identifiers != nil {
				lookupRequest.Works[i] = zotio.LookupWork{
					DOI: request.Work.Identifiers.DOI, ArXiv: request.Work.Identifiers.ArXiv,
				}
			}
		}
		ownership, err := r.Lookup.LookupWorks(ctx, lookupRequest)
		if err != nil {
			return result, fmt.Errorf("Zotio ownership lookup: %w", err)
		}
		if ownership == nil || len(ownership.Works) != len(requests) {
			return result, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", ownershipCount(ownership), len(requests))
		}
		queued = make([]discoveredRequest, 0, min(watch.PerRunCap, len(requests)))
		for i, classification := range ownership.Works {
			switch classification.Status {
			case zotio.OwnershipNotOwned:
				if len(queued) < watch.PerRunCap {
					queued = append(queued, requests[i])
				}
			case zotio.OwnershipOwnedWithPDF, zotio.OwnershipOwnedMissingPDF:
				// Existing Zotio items are not new watch discoveries, regardless of
				// whether their attachment is currently missing.
			default:
				return result, fmt.Errorf("Zotio ownership result %d has unknown status %q", i+1, classification.Status)
			}
		}
	}
	if len(queued) == 0 {
		return result, r.Store.MarkRun(ctx, watch.ID, runStart)
	}
	if watch.Mode == ModeAlert {
		entries, err := digestEntriesForDiscovered(queued)
		if err != nil {
			return result, err
		}
		reported, err := r.Store.RecordDigest(ctx, watch.ID, runStart, entries)
		if err != nil {
			return result, err
		}
		result.Reported = reported
		if err := r.Store.MarkRun(ctx, watch.ID, runStart); err != nil {
			return result, err
		}
		if reported > 0 {
			r.route(ctx, r.watchIntent(watch, runStart, notify.Event{
				Kind:    "watch.alert",
				Message: fmt.Sprintf("watch %s: %s found — papio watch digest %d", watch.Label, countedNoun(reported, "new work", "new works"), watch.ID),
				WatchID: watch.ID, WatchLabel: watch.Label, Count: reported,
			}, notify.CategoryDiscoveryNew))
		}
		return result, nil
	}

	queuedWorks := make([]protocol.WorkRequest, len(queued))
	for i, request := range queued {
		queuedWorks[i] = request.Work
	}
	manifest := batch.NewManifest(queuedWorks, "watch: "+watch.Label, watch.Collection, runStart)
	result.ManifestID = manifest.ID
	for i := range manifest.Works {
		requestID := batch.RequestID(fmt.Sprintf("watch-%d", watch.ID), manifest.Works[i].Work)
		manifest.Works[i].RequestID = requestID
		manifest.Works[i].Work.RequestID = requestID
	}
	autoImport := true
	for i := range manifest.Works {
		request := manifest.Works[i].Work
		jobID, err := r.Submitter.SubmitWithAutoImport(ctx, request, &autoImport)
		if err != nil {
			manifest.Works[i].Status = "submission_failed"
			manifest.Works[i].Error = "submit"
			result.Failed++
			continue
		}
		manifest.Works[i].JobID = jobID
		result.Queued++
	}
	if err := batch.Write(r.DataDir, manifest); err != nil {
		return result, err
	}
	if result.Failed == len(manifest.Works) {
		return result, fmt.Errorf("all %d watch submissions failed", result.Failed)
	}
	if result.Failed > 0 {
		if err := r.Store.MarkDegradedRun(ctx, watch.ID, runStart, result.Failed, len(manifest.Works)); err != nil {
			return result, err
		}
	} else if err := r.Store.MarkRun(ctx, watch.ID, runStart); err != nil {
		return result, err
	}
	if result.Queued > 0 {
		r.route(ctx, r.watchIntent(watch, runStart, notify.Event{
			Kind:    "watch.acquire",
			Message: fmt.Sprintf("watch %s: %s queued", watch.Label, countedNoun(result.Queued, "new paper", "new papers")),
			WatchID: watch.ID, WatchLabel: watch.Label, Count: result.Queued,
		}, notify.CategoryDiscoveryNew))
	}
	return result, nil
}

type discoveredRequest struct {
	Work       protocol.WorkRequest
	Discovered discovery.DiscoveredWork
}

func requestsForDiscoveredWithWork(works []discovery.DiscoveredWork) []discoveredRequest {
	requests := make([]discoveredRequest, 0, len(works))
	seen := make(map[string]struct{}, len(works))
	for _, discovered := range works {
		doi := strings.TrimSpace(discovered.Work.DOI)
		arXiv, err := work.NormalizeArXiv(discovered.Work.ArXiv)
		if err != nil {
			arXiv = ""
		}
		openAlexID, err := work.NormalizeOpenAlex(discovered.OpenAlexID)
		if err != nil {
			openAlexID = ""
		}
		title := strings.TrimSpace(discovered.Work.Title)
		authors := append([]string(nil), discovered.Work.Authors...)
		if doi == "" && arXiv == "" && (openAlexID == "" || title == "" || len(authors) == 0 || discovered.Work.Year == 0) {
			continue
		}
		keys := make([]string, 0, 3)
		if doi != "" {
			keys = append(keys, "doi:"+doi)
		}
		if arXiv != "" {
			keys = append(keys, "arxiv:"+arXiv)
		}
		if openAlexID != "" {
			keys = append(keys, "openalex:"+openAlexID)
		}
		duplicate := false
		for _, key := range keys {
			if _, found := seen[key]; found {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		for _, key := range keys {
			seen[key] = struct{}{}
		}
		var identifiers *protocol.Identifiers
		if doi != "" || arXiv != "" || openAlexID != "" {
			identifiers = &protocol.Identifiers{DOI: doi, ArXiv: arXiv, OpenAlex: openAlexID}
		}
		requests = append(requests, discoveredRequest{
			Work: protocol.WorkRequest{
				SchemaVersion: protocol.WorkRequestSchemaVersion,
				Identifiers:   identifiers,
				Title:         title,
				Authors:       authors,
				Year:          discovered.Work.Year,
			},
			Discovered: discovered,
		})
	}
	return requests
}

func digestEntriesForDiscovered(requests []discoveredRequest) ([]DigestEntry, error) {
	entries := make([]DigestEntry, 0, len(requests))
	for _, request := range requests {
		title := strings.TrimSpace(request.Discovered.Work.Title)
		titleKey := strings.Join(strings.Fields(strings.ToLower(title)), " ")
		doi, arXiv := "", ""
		if request.Work.Identifiers != nil {
			doi = strings.TrimSpace(request.Work.Identifiers.DOI)
			arXiv = strings.TrimSpace(request.Work.Identifiers.ArXiv)
		}
		workKey := titleKey
		if doi != "" {
			normalized, err := work.NormalizeDOI(doi)
			if err != nil {
				return nil, fmt.Errorf("normalizing watch digest DOI: %w", err)
			}
			doi = normalized
			workKey = doi
		} else if arXiv != "" {
			workKey = "arxiv:" + arXiv
		}
		if workKey == "" {
			return nil, errors.New("watch digest work requires a DOI, arXiv ID, or title")
		}
		authors := append([]string(nil), request.Discovered.Work.Authors...)
		entries = append(entries, DigestEntry{
			WorkKey:     workKey,
			TitleKey:    titleKey,
			Title:       title,
			Authors:     strings.Join(authors, ", "),
			AuthorNames: authors,
			Year:        request.Discovered.Work.Year,
			DOI:         doi,
			Identifiers: request.Work.Identifiers,
			IsOA:        request.Discovered.IsOA,
			Abstract:    request.Discovered.Abstract,
		})
	}
	return entries, nil
}

func workRequestForDigest(entry DigestEntry) (protocol.WorkRequest, error) {
	title := strings.TrimSpace(entry.Title)
	if title == "" {
		return protocol.WorkRequest{}, errors.New("watch digest entry requires title")
	}
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		Title:         title,
		Year:          entry.Year,
	}
	if len(entry.AuthorNames) > 0 {
		request.Authors = append(request.Authors, entry.AuthorNames...)
	} else {
		for _, author := range strings.Split(entry.Authors, ",") {
			if author = strings.TrimSpace(author); author != "" {
				request.Authors = append(request.Authors, author)
			}
		}
	}
	if entry.Identifiers != nil {
		request.Identifiers = entry.Identifiers
		return request, nil
	}
	if doi := strings.TrimSpace(entry.DOI); doi != "" {
		normalized, err := work.NormalizeDOI(doi)
		if err != nil {
			return protocol.WorkRequest{}, fmt.Errorf("normalizing watch digest DOI: %w", err)
		}
		request.Identifiers = &protocol.Identifiers{DOI: normalized}
		return request, nil
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.WorkKey)), "arxiv:") {
		arXiv, err := work.NormalizeArXiv(entry.WorkKey)
		if err != nil {
			return protocol.WorkRequest{}, fmt.Errorf("normalizing watch digest arXiv ID: %w", err)
		}
		request.Identifiers = &protocol.Identifiers{ArXiv: arXiv}
	}
	return request, nil
}

func ownershipCount(result *zotio.LookupWorksResult) int {
	if result == nil {
		return 0
	}
	return len(result.Works)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// selectUnheldRequests keeps the works no configured library already holds.
//
// Automation is strict on purpose: an incomplete lookup fails the run so it
// retries on the next cadence, rather than turning one unreadable export into a
// recurring burst of duplicate acquisitions. This deliberately regularises an
// inconsistency in the older zotio paths, where the digest run treats a
// staleness warning as fatal while the acquire run never inspects it.
func (r *Runner) selectUnheldRequests(ctx context.Context, requests []discoveredRequest, cap int) ([]discoveredRequest, error) {
	queries := make([]ownership.Query, len(requests))
	for i, request := range requests {
		var doi, arxiv, pmid string
		if request.Work.Identifiers != nil {
			doi = request.Work.Identifiers.DOI
			arxiv = request.Work.Identifiers.ArXiv
			pmid = request.Work.Identifiers.PMID
		}
		queries[i] = ownership.QueryFor(doi, arxiv, pmid, request.Work.DesiredVersion, "")
	}
	lookup := r.Holdings.Lookup(ctx, queries)
	if incomplete := lookup.Incomplete(); len(incomplete) != 0 {
		return nil, fmt.Errorf("library sources unavailable (%s); ownership could not be verified, so this run was not acquired", strings.Join(incomplete, ", "))
	}
	if len(lookup.Works) != len(requests) {
		return nil, fmt.Errorf("holdings lookup returned %d results for %d works", len(lookup.Works), len(requests))
	}
	queued := make([]discoveredRequest, 0, min(cap, len(requests)))
	for i := range requests {
		// A known citation without full text is not "already held": acquiring it
		// is the whole point of a watch for someone backfilling a library.
		if ownership.Decide(queries[i], lookup.Works[i]).Suppress {
			continue
		}
		if len(queued) < cap {
			queued = append(queued, requests[i])
		}
	}
	return queued, nil
}

// holdingsEnabled reports whether generic holdings sources are configured and
// therefore own the ownership answer for this daemon.
func (r *Runner) holdingsEnabled() bool {
	return r != nil && r.Holdings != nil && r.Holdings.Enabled()
}
