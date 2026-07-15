// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/protocol"
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

// Submitter is the application's normal acquisition submission path. A nil
// auto-import override retains the configured Zotio auto-import policy.
type Submitter interface {
	SubmitWithAutoImport(context.Context, protocol.WorkRequest, *bool) (string, error)
}

// Notifier sends an optional best-effort local notification.
type Notifier interface {
	Send(context.Context, string)
}

// RunResult is the outcome of one forced or scheduled watch execution.
type RunResult struct {
	WatchID             int64  `json:"watch_id"`
	Queued              int    `json:"queued"`
	ManifestID          string `json:"manifest_id,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	Disabled            bool   `json:"disabled"`
}

// Runner composes the existing discovery, Zotio ownership, acquisition batch,
// and desktop notification services for a single watch execution at a time.
type Runner struct {
	Store     *Store
	Discovery Discovery
	Lookup    OwnershipLookup
	Submitter Submitter
	Notifier  Notifier
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
	result, err := r.execute(ctx, watch)
	if err == nil {
		return result, nil
	}
	failure, recordErr := r.Store.RecordFailure(ctx, watch.ID, r.now(), err)
	if recordErr != nil {
		return result, fmt.Errorf("%w (recording watch failure: %v)", err, recordErr)
	}
	result.ConsecutiveFailures = failure.ConsecutiveFailures
	result.Disabled = failure.Disabled
	if failure.Disabled && r.Notifier != nil {
		r.Notifier.Send(ctx, fmt.Sprintf("watch %s disabled after %d consecutive failures", watch.Label, failure.ConsecutiveFailures))
	}
	return result, err
}

func (r *Runner) execute(ctx context.Context, watch Watch) (*RunResult, error) {
	result := &RunResult{WatchID: watch.ID}
	if r.Discovery == nil || r.Lookup == nil || r.Submitter == nil {
		return result, errors.New("watch runner dependencies are not configured")
	}
	works, err := r.Discovery.Search(ctx, discovery.SearchParams{
		Query: watch.Query, Limit: MaxPerRunCap,
		YearFrom: watch.Filters.YearFrom, YearTo: watch.Filters.YearTo, OAOnly: watch.Filters.OAOnly,
	})
	if err != nil {
		return result, fmt.Errorf("discovery search: %w", err)
	}
	requests := requestsForDiscovered(works)
	if len(requests) == 0 {
		return result, r.Store.MarkRun(ctx, watch.ID, r.now())
	}
	lookupRequest := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(requests))}
	for i, request := range requests {
		lookupRequest.Works[i] = zotio.LookupWork{
			DOI: request.Identifiers.DOI, ArXiv: request.Identifiers.ArXiv,
		}
	}
	ownership, err := r.Lookup.LookupWorks(ctx, lookupRequest)
	if err != nil {
		return result, fmt.Errorf("Zotio ownership lookup: %w", err)
	}
	if ownership == nil || len(ownership.Works) != len(requests) {
		return result, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", ownershipCount(ownership), len(requests))
	}
	queued := make([]protocol.WorkRequest, 0, min(watch.PerRunCap, len(requests)))
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
	if len(queued) == 0 {
		return result, r.Store.MarkRun(ctx, watch.ID, r.now())
	}

	manifest := batch.NewManifest(queued, "watch: "+watch.Label, watch.Collection, r.now())
	result.ManifestID = manifest.ID
	for i := range manifest.Works {
		requestID := batch.RequestID(fmt.Sprintf("watch-%d", watch.ID), manifest.Works[i].Work)
		manifest.Works[i].RequestID = requestID
		manifest.Works[i].Work.RequestID = requestID
	}
	var firstErr error
	for i := range manifest.Works {
		request := manifest.Works[i].Work
		jobID, err := r.Submitter.SubmitWithAutoImport(ctx, request, nil)
		if err != nil {
			manifest.Works[i].Status = "submission_failed"
			manifest.Works[i].Error = "submit"
			if firstErr == nil {
				firstErr = fmt.Errorf("submitting watch work %d: %w", i+1, err)
			}
			continue
		}
		manifest.Works[i].JobID = jobID
		result.Queued++
	}
	if err := batch.Write(r.DataDir, manifest); err != nil {
		return result, err
	}
	if firstErr != nil {
		return result, firstErr
	}
	if r.Notifier != nil && result.Queued > 0 {
		r.Notifier.Send(ctx, fmt.Sprintf("watch %s: %d new papers queued", watch.Label, result.Queued))
	}
	if err := r.Store.MarkRun(ctx, watch.ID, r.now()); err != nil {
		return result, err
	}
	return result, nil
}

func requestsForDiscovered(works []discovery.DiscoveredWork) []protocol.WorkRequest {
	requests := make([]protocol.WorkRequest, 0, len(works))
	seen := make(map[string]struct{}, len(works))
	for _, discovered := range works {
		identifiers := &protocol.Identifiers{
			DOI: discovered.Work.DOI, PMID: discovered.Work.PMID, ArXiv: discovered.Work.ArXiv,
			ISBN: discovered.Work.ISBN, OpenAlex: discovered.Work.OpenAlex,
		}
		if identifiers.OpenAlex == "" {
			identifiers.OpenAlex = strings.TrimSpace(discovered.OpenAlexID)
		}
		if identifiers.DOI == "" && identifiers.PMID == "" && identifiers.ArXiv == "" && identifiers.ISBN == "" && identifiers.OpenAlex == "" {
			continue
		}
		keys := make([]string, 0, 5)
		for _, item := range []struct {
			kind, value string
		}{
			{"doi", identifiers.DOI}, {"pmid", identifiers.PMID}, {"arxiv", identifiers.ArXiv},
			{"isbn", identifiers.ISBN}, {"openalex", identifiers.OpenAlex},
		} {
			if item.value != "" {
				keys = append(keys, item.kind+":"+item.value)
			}
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
		requests = append(requests, protocol.WorkRequest{
			SchemaVersion: protocol.WorkRequestSchemaVersion,
			Identifiers:   identifiers,
			Title:         strings.TrimSpace(discovered.Work.Title),
			Authors:       append([]string(nil), discovered.Work.Authors...),
			Year:          discovered.Work.Year,
		})
	}
	return requests
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
