// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"papio/internal/batch"
	"papio/internal/config"
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

// PagedDiscovery is a Discovery source that resumes each backend's search
// from a stored continuation token. discovery.Multi implements it. The runner
// type-asserts for it, as it does for discovery.PartialSearcher; a source
// without it keeps the single-page scan.
type PagedDiscovery interface {
	SearchPages(context.Context, discovery.SearchParams, map[string]string) (discovery.MultiPage, error)
}

// The daemon's discovery source reaches the paged scan only by
// type-assertion, so a signature drift would silently fall back to one page.
var _ PagedDiscovery = (*discovery.Multi)(nil)

// ScanPagesPerSource bounds the result pages one run reads from each
// discovery source: the first page, where new papers appear, plus up to
// ScanPagesPerSource-1 deeper pages when the first page held nothing new.
// Every page is one budget-gated search request.
const ScanPagesPerSource = 4

// scanPageLimit is the result count every scan page asks for: the first page
// (as min(per-run cap*3, scanPageLimit)) and each deeper page. It stays well
// under discovery.MaxLimit because a full OpenAlex page of 50 works outgrows
// the discovery client's 1 MiB response-body cap (defaultMaxBody in
// internal/discovery/discovery.go), so every such page fails instead of
// answering.
const scanPageLimit = 25

// OwnershipLookup is the Zotio deduplication surface used before submitting
// acquisition work.
type OwnershipLookup interface {
	LookupWorks(context.Context, zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error)
}

// HoldingsLookup is the generic (non-Zotero) ownership surface. When enabled it
// answers ownership for every discovery watch stage (ADR-0008).
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
	// Degraded reports a run whose discovery scan did not finish: one or
	// more discovery backends failed, or the bounded deeper scan stopped
	// with results left unread. The cadence advances and the reasons are
	// named in the watch's persisted last_error; the run is deliberately
	// not a clean success.
	Degraded bool `json:"degraded,omitempty"`
}

// Runner composes the existing discovery, ownership, acquisition batch, and
// desktop notification services for a single watch execution at a time.
// Ownership comes from generic holdings when they are enabled (the daemon only
// enables them without zotio) and from Zotio otherwise, for acquisition runs,
// alert runs, and digest acquisition alike.
type Runner struct {
	Store     *Store
	Discovery Discovery
	Lookup    OwnershipLookup
	// Holdings answers ownership when configured; nil or disabled leaves the
	// Zotio path exactly as it was.
	Holdings  HoldingsLookup
	Submitter Submitter
	Backfill  BackfillQueue
	// LibrarySources declares the explicit feeds available for generic backfill.
	LibrarySources []config.LibrarySource
	Notifier       notify.Sink
	DataDir        string
	Now            func() time.Time

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
	if r == nil || r.Store == nil || (r.Lookup == nil && !r.holdingsEnabled()) || r.Submitter == nil {
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
	for i, entry := range entries {
		works[i], err = workRequestForDigest(entry)
		if err != nil {
			return 0, err
		}
		if works[i].Identifiers == nil || (works[i].Identifiers.DOI == "" && works[i].Identifiers.ArXiv == "") {
			return 0, fmt.Errorf("watch digest entry %q cannot be authoritatively classified without a DOI or arXiv ID", entry.WorkKey)
		}
	}
	unheld, err := r.classifyUnheld(ctx, works)
	if err != nil {
		return 0, err
	}

	eligibleEntries := make([]DigestEntry, 0, len(entries))
	eligibleWorks := make([]protocol.WorkRequest, 0, len(works))
	consumedEntries := make([]DigestEntry, 0, len(entries))
	for i := range entries {
		if unheld[i] {
			eligibleEntries = append(eligibleEntries, entries[i])
			eligibleWorks = append(eligibleWorks, works[i])
		} else {
			consumedEntries = append(consumedEntries, entries[i])
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
	if r == nil || r.Store == nil || (r.Lookup == nil && !r.holdingsEnabled()) || r.Submitter == nil {
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

// routeChecked delivers one watch notification and reports its failure. The
// alert path uses it instead of route so a lost alert fails the run (and is
// retried from durable digest state) rather than passing silently.
func (r *Runner) routeChecked(ctx context.Context, intent notify.Intent) error {
	if r == nil || r.Notifier == nil {
		return nil
	}
	return r.Notifier.Route(context.WithoutCancel(ctx), intent)
}

// deliverPendingDigestAlerts routes one alert for every pending digest entry
// without a durable alert receipt and records the receipts only after the
// route succeeds. It is the shared catch-up path for both a scan that just
// recorded new entries and a scan that found nothing new: stranded entries
// from a failed route must be retried even when later scans report no hits
// or only owned work, which otherwise return early without routing.
func (r *Runner) deliverPendingDigestAlerts(ctx context.Context, watch Watch, runStart time.Time) error {
	pending, err := r.Store.UnalertedDigestEntries(ctx, watch.ID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if err := r.routeChecked(ctx, r.alertIntent(watch, pending, runStart)); err != nil {
		return fmt.Errorf("routing watch alert: %w", err)
	}
	keys := make([]string, 0, len(pending))
	for _, entry := range pending {
		keys = append(keys, entry.WorkKey)
	}
	if err := r.Store.MarkDigestAlerted(ctx, watch.ID, keys, runStart); err != nil {
		return err
	}
	return nil
}

// searchDiscovery runs the configured discovery source, preferring partial
// results when the source can report per-backend failures. A plain Search
// source keeps its old behavior: usable results or a hard error.
func (r *Runner) searchDiscovery(ctx context.Context, params discovery.SearchParams) ([]discovery.DiscoveredWork, []discovery.BackendFailure, error) {
	if partial, ok := r.Discovery.(discovery.PartialSearcher); ok {
		return partial.SearchPartial(ctx, params)
	}
	works, err := r.Discovery.Search(ctx, params)
	return works, nil, err
}

// markDiscoveryRun advances the watch cadence after a scan that produced no
// hard error. A scan that finished is a clean success. One that did not —
// failed backends, or a deeper scan stopped with results left unread — keeps
// its healthy results but is recorded as partial with every reason named, so
// an incomplete scan, including a zero-result one, is never reported as
// complete.
func (r *Runner) markDiscoveryRun(ctx context.Context, watch Watch, runStart time.Time, detail string, result *RunResult) error {
	if detail == "" {
		return r.Store.MarkRun(ctx, watch.ID, runStart)
	}
	result.Degraded = true
	return r.Store.MarkPartialRun(ctx, watch.ID, runStart, detail)
}

// discoveryScan is what one run's discovery scan found and what it left
// unfinished.
type discoveryScan struct {
	// fresh holds new works in scan order — unheld and, for an alert watch,
	// never recorded in its digest — capped at the watch's per-run cap.
	fresh []discoveredRequest
	// known holds scanned unheld works that the alert digest already has.
	// Recording them again reports nothing; it only refreshes their stored
	// identity (a title-only entry gaining its DOI).
	known []discoveredRequest
	// coverage is each scanned source's next position, persisted only
	// after fresh has been handled so a failed run re-reads its pages.
	coverage []SourceCoverage
	// rereads holds, per source, the position that fetched a deeper page
	// holding fresh works. It replaces that source's coverage when any
	// submission fails, so the next run re-reads the page.
	rereads  []SourceCoverage
	failures []discovery.BackendFailure
	// limits names each source whose deeper scan stopped with results left
	// unread.
	limits []string
}

// take appends new works up to the per-run cap and reports how many it kept.
func (scan *discoveryScan) take(fresh []discoveredRequest, perRunCap int) int {
	kept := min(max(perRunCap-len(scan.fresh), 0), len(fresh))
	scan.fresh = append(scan.fresh, fresh[:kept]...)
	return kept
}

// storedCoverage is the coverage to persist: with a failed submission, each
// source whose deeper page supplied fresh works keeps the position that
// fetched that page.
func (scan *discoveryScan) storedCoverage(submissionFailed bool) []SourceCoverage {
	if !submissionFailed || len(scan.rereads) == 0 {
		return scan.coverage
	}
	stored := make([]SourceCoverage, 0, len(scan.coverage))
	for _, entry := range scan.coverage {
		for _, reread := range scan.rereads {
			if reread.Source == entry.Source {
				entry = reread
				break
			}
		}
		stored = append(stored, entry)
	}
	return stored
}

// detail names every reason the scan did not finish, or is empty when it did.
func (scan *discoveryScan) detail() string {
	parts := make([]string, 0, 1+len(scan.limits))
	if summary := discovery.SummarizeFailures(scan.failures); summary != "" {
		parts = append(parts, summary)
	}
	parts = append(parts, scan.limits...)
	return strings.Join(parts, "; ")
}

// scanFirstPage is the scan for a discovery source that cannot page: one
// search, classified once.
func (r *Runner) scanFirstPage(ctx context.Context, watch Watch, params discovery.SearchParams) (*discoveryScan, error) {
	works, failures, err := r.searchDiscovery(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("discovery search: %w", err)
	}
	scan := &discoveryScan{failures: failures}
	fresh, known, err := r.newWork(ctx, watch, works, make(map[string]struct{}))
	if err != nil {
		return nil, err
	}
	scan.take(fresh, watch.PerRunCap)
	scan.known = known
	return scan, nil
}

// scanPages reads every source's first page, where new papers appear. Only
// when that page holds nothing new does it walk deeper, one source at a time
// in preference order, resuming each source's stored position, until a page
// yields new work or the source has spent ScanPagesPerSource pages.
//
// Result order is not stable. Relevance-ranked backends (OpenAlex text
// search, Semantic Scholar search) reorder as their corpus changes, so a
// resumed walk can repeat or skip a work; arXiv's newest-first order only
// repeats. The walk is a rolling sweep, not exact coverage: only a source
// that reports exhausted counts as scanned to the end, and the next run then
// restarts its walk after the first page.
func (r *Runner) scanPages(ctx context.Context, pager PagedDiscovery, watch Watch, params discovery.SearchParams) (*discoveryScan, error) {
	first, err := pager.SearchPages(ctx, params, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery search: %w", err)
	}
	scan := &discoveryScan{}
	seen := make(map[string]struct{})
	fresh, known, err := r.newWork(ctx, watch, first.Works, seen)
	if err != nil {
		return nil, err
	}
	scan.take(fresh, watch.PerRunCap)
	scan.known = known
	deeper := len(fresh) == 0
	walks := make([]discovery.SourcePage, 0, len(first.Sources))
	for _, page := range first.Sources {
		switch page.State {
		case discovery.PageFailed:
			// The stored position stays for the next run.
			scan.failures = append(scan.failures, pageFailure(page))
		case discovery.PageMore:
			if deeper {
				walks = append(walks, page)
			}
		default:
			// The first page holds everything this source will return, so
			// any stored deeper position is obsolete.
			scan.coverage = append(scan.coverage, SourceCoverage{Source: page.Source, State: string(page.State)})
			if note := stopNote(page.Source, page.State); deeper && note != "" {
				scan.limits = append(scan.limits, note)
			}
		}
	}
	if len(walks) == 0 {
		return scan, nil
	}
	stored, err := r.Store.ScanCoverage(ctx, watch.ID)
	if err != nil {
		return nil, err
	}
	for _, page := range walks {
		if len(scan.fresh) > 0 {
			// New work found: later sources keep their stored positions.
			break
		}
		if err := r.walkSource(ctx, pager, watch, params, page, stored[page.Source].NextToken, seen, scan); err != nil {
			return nil, err
		}
	}
	return scan, nil
}

// walkSource reads up to ScanPagesPerSource-1 deeper pages from one source,
// starting at its stored position or, without one, right after its first
// page, and records where the next run resumes.
func (r *Runner) walkSource(ctx context.Context, pager PagedDiscovery, watch Watch, params discovery.SearchParams, first discovery.SourcePage, stored string, seen map[string]struct{}, scan *discoveryScan) error {
	source := first.Source
	params.Source = source
	params.Limit = scanPageLimit
	token, resumed := first.Next, false
	if stored != "" {
		token, resumed = stored, true
	}
	for pages := 1; pages < ScanPagesPerSource; {
		// With one source selected, a hard error means that source failed,
		// and its failed page is still listed.
		result, err := pager.SearchPages(ctx, params, map[string]string{source: token})
		page, found := sourcePage(result, source)
		if !found {
			message := "search returned no page"
			if err != nil {
				message = discovery.SanitizeError(err)
			}
			scan.failures = append(scan.failures, discovery.BackendFailure{Source: source, Message: message, At: r.now()})
			return nil
		}
		if page.State == discovery.PageFailed {
			if resumed && errors.Is(page.Err, discovery.ErrInvalidPageToken) {
				// The stored position belongs to another search (the watch's
				// query or filters changed) or is unusable. Discard it and
				// restart after the first page. It is not a source failure,
				// and the rejected token cost no request.
				log.Printf("papio: watch %d: discarding stored %s scan position (%s); restarting after the first page", watch.ID, source, pageFailure(page).Message)
				token, resumed = first.Next, false
				continue
			}
			// A failure or a budget refusal is not exhaustion: page.Next
			// echoes the token, so the next run retries this position.
			scan.failures = append(scan.failures, pageFailure(page))
			scan.coverage = append(scan.coverage, SourceCoverage{Source: source, NextToken: page.Next, State: string(page.State)})
			return nil
		}
		pages++
		resumed = false
		fresh, known, err := r.newWork(ctx, watch, result.Works, seen)
		if err != nil {
			return err
		}
		scan.known = append(scan.known, known...)
		if len(fresh) > 0 {
			scan.rereads = append(scan.rereads, SourceCoverage{Source: source, NextToken: token, State: string(discovery.PageMore)})
		}
		if kept := scan.take(fresh, watch.PerRunCap); kept < len(fresh) {
			// New works beyond the per-run cap: the next run re-reads this
			// page rather than skip past them.
			scan.coverage = append(scan.coverage, SourceCoverage{Source: source, NextToken: token, State: string(discovery.PageMore)})
			return nil
		}
		if page.State != discovery.PageMore {
			scan.coverage = append(scan.coverage, SourceCoverage{Source: source, State: string(page.State)})
			if note := stopNote(source, page.State); len(fresh) == 0 && note != "" {
				scan.limits = append(scan.limits, note)
			}
			return nil
		}
		token = page.Next
		if len(fresh) > 0 {
			scan.coverage = append(scan.coverage, SourceCoverage{Source: source, NextToken: token, State: string(page.State)})
			return nil
		}
	}
	scan.coverage = append(scan.coverage, SourceCoverage{Source: source, NextToken: token, State: string(discovery.PageMore)})
	scan.limits = append(scan.limits, fmt.Sprintf("%s: scan limit of %d pages reached with more results left; the next run continues", source, ScanPagesPerSource))
	return nil
}

// sourcePage finds one source's page in a paged search result.
func sourcePage(result discovery.MultiPage, source string) (discovery.SourcePage, bool) {
	for _, page := range result.Sources {
		if page.Source == source {
			return page, true
		}
	}
	return discovery.SourcePage{}, false
}

// pageFailure is the sanitized failure of a failed page.
func pageFailure(page discovery.SourcePage) discovery.BackendFailure {
	if page.Failure != nil {
		return *page.Failure
	}
	return discovery.BackendFailure{Source: page.Source, Message: "search failed"}
}

// stopNote names a source whose results end before the scan could look at
// all of them, or is empty for a source that reached its real end.
func stopNote(source string, state discovery.PageState) string {
	switch state {
	case discovery.PageTruncated:
		return source + ": later results lie past the source's result window and cannot be reached"
	case discovery.PageUnsupported:
		return source + ": cannot page past its first results"
	}
	return ""
}

// newWork classifies one page's works and returns the ones this watch has not
// handled yet: fresh works are unheld by the ownership authority and, for an
// alert watch, absent from its digest — including entries already cleared or
// acquired, so a work met again on a later page or run is never reported
// twice. known holds the unheld works an alert digest already has. seen
// drops works an earlier page of the same run returned.
func (r *Runner) newWork(ctx context.Context, watch Watch, works []discovery.DiscoveredWork, seen map[string]struct{}) (fresh, known []discoveredRequest, err error) {
	requests := appendDiscoveredRequests(nil, seen, works)
	if len(requests) == 0 {
		return nil, nil, nil
	}
	requestWorks := make([]protocol.WorkRequest, len(requests))
	for i, request := range requests {
		requestWorks[i] = request.Work
	}
	// Classification fails the whole run before anything is recorded or
	// submitted, so an unverifiable answer is retried on the next cadence
	// instead of turning an unknown work into a new discovery.
	unheld, err := r.classifyUnheld(ctx, requestWorks)
	if err != nil {
		return nil, nil, err
	}
	for i, request := range requests {
		if unheld[i] {
			fresh = append(fresh, request)
		}
	}
	if watch.Mode != ModeAlert || len(fresh) == 0 {
		return fresh, nil, nil
	}
	// A work the digest cannot record (a malformed DOI) is left unknown, so
	// it fails the run only if the per-run cap actually selects it, exactly
	// as when every run looked at one page.
	entries := make([]DigestEntry, 0, len(fresh))
	checked := make([]int, 0, len(fresh))
	for i, request := range fresh {
		built, err := digestEntriesForDiscovered([]discoveredRequest{request})
		if err != nil {
			continue
		}
		entries = append(entries, built[0])
		checked = append(checked, i)
	}
	digested, err := r.Store.KnownDigestEntries(ctx, watch.ID, entries)
	if err != nil {
		return nil, nil, err
	}
	inDigest := make([]bool, len(fresh))
	for j, i := range checked {
		inDigest[i] = digested[j]
	}
	unreported := make([]discoveredRequest, 0, len(fresh))
	for i, request := range fresh {
		if inDigest[i] {
			known = append(known, request)
		} else {
			unreported = append(unreported, request)
		}
	}
	return unreported, known, nil
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

// alertIntent builds the new-work alert for one pending digest generation.
//
// Its identity must not move between attempts. A retry after a successful
// Route but a lost receipt (crash between the two) re-routes the same alert,
// and a run-start-derived identity would make that a fresh ledger row and a
// second webhook POST. The identity is therefore derived from the pending set
// itself: the aggregate key hashes the sorted pending digest row IDs, and the
// window is the earliest first sighting among them. Row IDs survive
// RecordDigest key rewrites (title key to DOI), where work keys move: a retry
// after a key upgrade hashes the same row and coalesces into the same
// notification row, whose at-most-once webhook claim rejects the second
// dispatch. Distinct rows keep distinct IDs even when they share a title, so
// a genuinely different pending set is a different generation and alerts on
// its own. Entries without a row ID (synthetic, never routed) fall back to
// their work keys.
func (r *Runner) alertIntent(watch Watch, pending []DigestEntry, runStart time.Time) notify.Intent {
	keys := make([]string, 0, len(pending))
	windowStart := time.Time{}
	for _, entry := range pending {
		if entry.rowID != 0 {
			keys = append(keys, fmt.Sprintf("row:%d", entry.rowID))
		} else {
			keys = append(keys, entry.WorkKey)
		}
		seen, err := time.Parse(time.RFC3339Nano, entry.FirstSeenAt)
		if err != nil {
			continue
		}
		if windowStart.IsZero() || seen.Before(windowStart) {
			windowStart = seen.UTC()
		}
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	generation := hex.EncodeToString(sum[:8])
	if windowStart.IsZero() {
		windowStart = runStart.UTC()
	}
	message := fmt.Sprintf("watch %s: %s found — papio watch digest %d",
		watch.Label, countedNoun(len(pending), "new work", "new works"), watch.ID)
	event := notify.Event{
		Kind: "watch.alert", Message: message,
		WatchID: watch.ID, WatchLabel: watch.Label, Count: len(pending),
	}
	identity := fmt.Sprintf("watch:%d:digest:%s", watch.ID, generation)
	return notify.Intent{
		EventKind: event.Kind, Category: notify.CategoryDiscoveryNew,
		AggregateKey: identity, Phase: notify.PhaseDigest, WindowStart: windowStart,
		HappenedAt: windowStart, Message: message, Detail: event, ScanID: identity,
	}
}
func (r *Runner) executeBody(ctx context.Context, watch Watch, runStart time.Time) (*RunResult, error) {
	result := &RunResult{WatchID: watch.ID}
	if watch.Kind == KindBackfill {
		sourceName, err := r.Store.genericBackfillSource(ctx, watch.ID)
		if err != nil {
			return result, err
		}
		if sourceName != "" {
			return r.executeGenericBackfill(ctx, watch, sourceName, runStart)
		}
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
	// Both modes use the same ownership authority: generic holdings when they
	// are enabled, Zotio otherwise.
	if r.Discovery == nil || (r.Lookup == nil && !r.holdingsEnabled()) || (watch.Mode == ModeAcquire && r.Submitter == nil) {
		return result, errors.New("watch runner dependencies are not configured")
	}
	params := discovery.SearchParams{
		Query: watch.Query, Limit: min(watch.PerRunCap*3, scanPageLimit), Slim: true,
		YearFrom: watch.Filters.YearFrom, YearTo: watch.Filters.YearTo, OAOnly: watch.Filters.OAOnly,
		Cites: watch.Filters.Cites, CitedBy: watch.Filters.CitedBy, RelatedTo: watch.Filters.RelatedTo,
	}
	var scan *discoveryScan
	var err error
	if pager, ok := r.Discovery.(PagedDiscovery); ok {
		scan, err = r.scanPages(ctx, pager, watch, params)
	} else {
		scan, err = r.scanFirstPage(ctx, watch, params)
	}
	if err != nil {
		return result, err
	}
	if watch.Mode == ModeAlert {
		if records := append(append([]discoveredRequest(nil), scan.fresh...), scan.known...); len(records) > 0 {
			entries, err := digestEntriesForDiscovered(records)
			if err != nil {
				return result, err
			}
			reported, err := r.Store.RecordDigest(ctx, watch.ID, runStart, entries)
			if err != nil {
				return result, err
			}
			result.Reported = reported
		}
		// The new works are durable digest entries now, so the scan
		// position may move past them.
		if err := r.Store.SaveScanCoverage(ctx, watch.ID, runStart, scan.coverage); err != nil {
			return result, err
		}
		// Alert delivery is at-least-once: the digest rows above are durable,
		// but a crash (or a notifier failure) between RecordDigest and the
		// route below used to leave them permanently unannounced, because the
		// next run deduplicates them to reported=0 and never routes. Re-read
		// every pending entry without a durable alert receipt — including ones
		// stranded by an earlier run — route one alert for them, and record
		// the receipt only after the route succeeds. A failed route returns
		// before MarkRun so the run is recorded as a failure and the stranded
		// entries are retried without rediscovery on the next cadence. The
		// same catch-up runs when this scan finds nothing new, so a failed
		// route is retried even when later scans report no hits or only
		// owned work.
		if err := r.deliverPendingDigestAlerts(ctx, watch, runStart); err != nil {
			return result, err
		}
		return result, r.markDiscoveryRun(ctx, watch, runStart, scan.detail(), result)
	}
	if len(scan.fresh) == 0 {
		if err := r.Store.SaveScanCoverage(ctx, watch.ID, runStart, scan.coverage); err != nil {
			return result, err
		}
		return result, r.markDiscoveryRun(ctx, watch, runStart, scan.detail(), result)
	}

	queuedWorks := make([]protocol.WorkRequest, len(scan.fresh))
	for i, request := range scan.fresh {
		queuedWorks[i] = request.Work
	}
	manifest := batch.NewManifest(queuedWorks, "watch: "+watch.Label, watch.Collection, runStart)
	for i := range manifest.Works {
		requestID := batch.RequestID(fmt.Sprintf("watch-%d", watch.ID), manifest.Works[i].Work)
		manifest.Works[i].RequestID = requestID
		manifest.Works[i].Work.RequestID = requestID
	}
	// Carry forward JobIDs a previous attempt made durable for this batch, so
	// a manifest rewritten after a failure never drops a job that exists.
	if existing, loadErr := batch.Load(r.DataDir, manifest.ID); loadErr == nil {
		existingByRequest := make(map[string]string, len(existing.Works))
		for _, entry := range existing.Works {
			if entry.JobID != "" {
				existingByRequest[entry.RequestID] = entry.JobID
			}
		}
		for i := range manifest.Works {
			manifest.Works[i].JobID = existingByRequest[manifest.Works[i].RequestID]
		}
	} else if !errors.Is(loadErr, batch.ErrManifestNotFound) {
		return result, loadErr
	}
	// Persist the manifest before the first submission: a job created without
	// a durable record is one the watch can neither report nor recover.
	if err := batch.Write(r.DataDir, manifest); err != nil {
		return result, err
	}
	result.ManifestID = manifest.ID
	autoImport := true
	for i := range manifest.Works {
		// A recorded JobID is never a reason to skip submission. The request
		// ID reconciles a still-live job to itself, while a job that reached
		// a terminal state must be replaced — which only submission can do.
		// A JobID carried from an earlier manifest is a record, not evidence
		// that the job is still live, so a failed submission stays a failure.
		request := manifest.Works[i].Work
		jobID, err := r.Submitter.SubmitWithAutoImport(ctx, request, &autoImport)
		if err != nil {
			manifest.Works[i].Status = "submission_failed"
			manifest.Works[i].Error = "submit"
			result.Failed++
		} else {
			manifest.Works[i].JobID = jobID
			result.Queued++
		}
		// A later submission or write failure must not erase earlier JobIDs.
		if err := batch.Write(r.DataDir, manifest); err != nil {
			return result, err
		}
	}
	if result.Failed == len(manifest.Works) {
		// The scan position stays put, so the next run re-reads these pages.
		return result, fmt.Errorf("all %d watch submissions failed", result.Failed)
	}
	if err := r.Store.SaveScanCoverage(ctx, watch.ID, runStart, scan.storedCoverage(result.Failed > 0)); err != nil {
		return result, err
	}
	detail := scan.detail()
	switch {
	case result.Failed > 0 && detail != "":
		result.Degraded = true
		if err := r.Store.MarkPartialRun(ctx, watch.ID, runStart, fmt.Sprintf("%d of %d watch submissions failed; %s", result.Failed, len(manifest.Works), detail)); err != nil {
			return result, err
		}
	case result.Failed > 0:
		if err := r.Store.MarkDegradedRun(ctx, watch.ID, runStart, result.Failed, len(manifest.Works)); err != nil {
			return result, err
		}
	default:
		if err := r.markDiscoveryRun(ctx, watch, runStart, detail, result); err != nil {
			return result, err
		}
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

// appendDiscoveredRequests turns discovered works into work requests,
// dropping works without a usable identity and any work whose DOI, arXiv ID,
// or OpenAlex ID is already in seen, which it updates. Passing one seen set
// across pages drops a work an earlier page returned.
func appendDiscoveredRequests(requests []discoveredRequest, seen map[string]struct{}, works []discovery.DiscoveredWork) []discoveredRequest {
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

// classifyUnheld reports, aligned by index, which works the configured
// ownership authority does not already hold. It is the single classification
// used by acquisition runs, alert runs, and digest acquisition.
func (r *Runner) classifyUnheld(ctx context.Context, works []protocol.WorkRequest) ([]bool, error) {
	if r.holdingsEnabled() {
		return r.unheldByHoldings(ctx, works)
	}
	return r.unownedByZotio(ctx, works)
}

// unownedByZotio classifies works against Zotio. Any existing Zotio item, with
// or without its attachment, counts as held: it is neither a new discovery nor
// a digest entry to acquire. A stale mirror (or an unconfigured Zotio)
// classifies from old or no data, so it fails closed rather than persisting
// false not-owned verdicts past recovery.
func (r *Runner) unownedByZotio(ctx context.Context, works []protocol.WorkRequest) ([]bool, error) {
	lookupRequest := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(works))}
	for i, request := range works {
		if request.Identifiers != nil {
			lookupRequest.Works[i] = zotio.LookupWork{
				DOI: request.Identifiers.DOI, ArXiv: request.Identifiers.ArXiv,
			}
		}
	}
	owned, err := r.Lookup.LookupWorks(ctx, lookupRequest)
	if err != nil {
		return nil, fmt.Errorf("Zotio ownership lookup: %w", err)
	}
	if owned == nil || len(owned.Works) != len(works) {
		return nil, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", ownershipCount(owned), len(works))
	}
	if strings.TrimSpace(owned.StalenessWarning) != "" {
		return nil, fmt.Errorf("Zotio ownership lookup is stale: %s", owned.StalenessWarning)
	}
	unowned := make([]bool, len(works))
	for i, classification := range owned.Works {
		switch classification.Status {
		case zotio.OwnershipNotOwned:
			unowned[i] = true
		case zotio.OwnershipOwnedWithPDF, zotio.OwnershipOwnedMissingPDF:
			// Held, whether or not its attachment is currently missing.
		default:
			return nil, fmt.Errorf("Zotio ownership result %d has unknown status %q", i+1, classification.Status)
		}
	}
	return unowned, nil
}

// unheldByHoldings classifies works against the generic holdings sources.
//
// Automation is strict on purpose: an incomplete lookup fails the caller so a
// run retries on the next cadence, rather than turning one unreadable export
// into a recurring burst of duplicate acquisitions or false new-work alerts.
// Only a fresh pdf_present claim counts as held; a record_present citation
// without full text stays unheld, because acquiring it is the point of a watch
// for someone backfilling a library.
func (r *Runner) unheldByHoldings(ctx context.Context, works []protocol.WorkRequest) ([]bool, error) {
	queries := make([]ownership.Query, len(works))
	for i, request := range works {
		var doi, arxiv, pmid string
		if request.Identifiers != nil {
			doi = request.Identifiers.DOI
			arxiv = request.Identifiers.ArXiv
			pmid = request.Identifiers.PMID
		}
		queries[i] = ownership.QueryFor(doi, arxiv, pmid, request.DesiredVersion, "")
	}
	lookup := r.Holdings.Lookup(ctx, queries)
	if incomplete := lookup.Incomplete(); len(incomplete) != 0 {
		return nil, fmt.Errorf("library sources unavailable (%s); ownership could not be verified, so nothing was recorded or acquired", strings.Join(incomplete, ", "))
	}
	if len(lookup.Works) != len(works) {
		return nil, fmt.Errorf("holdings lookup returned %d results for %d works", len(lookup.Works), len(works))
	}
	unheld := make([]bool, len(works))
	for i := range works {
		unheld[i] = !ownership.Decide(queries[i], lookup.Works[i]).Suppress
	}
	return unheld, nil
}

// holdingsEnabled reports whether generic holdings sources are configured and
// therefore own the ownership answer for this daemon.
func (r *Runner) holdingsEnabled() bool {
	return r != nil && r.Holdings != nil && r.Holdings.Enabled()
}
