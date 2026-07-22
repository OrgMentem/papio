// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"papio/internal/bundle"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/work"
)

// CLI is the credential-owning Zotio process boundary used by the service.
type CLI interface {
	Preflight(context.Context) (*PreflightResult, error)
	MissingPDF(context.Context, string, int) ([]MissingPDFItem, error)
	GetItem(context.Context, string) (*Item, error)
	Sync(context.Context) error
	RunJSON(context.Context, ...string) (json.RawMessage, error)
}

// Submitter is papio's command-independent acquisition application service.
type Submitter interface {
	Submit(context.Context, protocol.WorkRequest) (string, error)
}

// Service coordinates Zotio observations with papio acquisition requests.
type Service struct {
	CLI            CLI
	Submitter      Submitter
	Bundle         *bundle.Exporter
	Store          *store.Store
	DataDir        string
	AttachmentMode string
	AutoEnrich     bool
	// ExceptionTags enables the reconciled exception-tag ledger on linked
	// Zotero items (config zotio.exception_tags).
	ExceptionTags bool
	// UnavailableRecheck is how long an unavailable outcome parks an item
	// before QueueMissingPDF re-checks it. Zero disables the cool-down.
	UnavailableRecheck time.Duration
	Now                func() time.Time
	tagMu              sync.Mutex
}

// QueueOptions configures one bounded import of Zotio's missing-PDF queue.
type QueueOptions struct {
	Collection         string   `json:"collection,omitempty"`
	Limit              int      `json:"limit"`
	DesiredVersion     string   `json:"desired_version,omitempty"`
	AccessModeOverride string   `json:"access_mode_override,omitempty"`
	MaxCostUSD         *float64 `json:"max_cost_usd,omitempty"`
	SourcesAllow       []string `json:"sources_allow,omitempty"`
	SourcesDeny        []string `json:"sources_deny,omitempty"`
}

// QueuedJob links one Zotero parent item to its idempotent papio request/job.
type QueuedJob struct {
	ZotioItemKey string `json:"zotio_item_key"`
	RequestID    string `json:"request_id"`
	JobID        string `json:"job_id"`
	Title        string `json:"title,omitempty"`
}

// SkippedItem records a queue row that lacks enough bibliographic identity.
type SkippedItem struct {
	ZotioItemKey string `json:"zotio_item_key"`
	Title        string `json:"title,omitempty"`
	Reason       string `json:"reason"`
}

// QueueResult is stable machine output for `acquire --from-zotio`.
type QueueResult struct {
	Preflight *PreflightResult `json:"preflight"`
	Queued    []QueuedJob      `json:"queued"`
	Skipped   []SkippedItem    `json:"skipped"`
}

// OwnershipStatus describes one work's present local-library state.
const (
	OwnershipNotOwned        = "not_owned"
	OwnershipOwnedWithPDF    = "owned_with_pdf"
	OwnershipOwnedMissingPDF = "owned_missing_pdf"
)

// LookupWork contains the stable identifiers used for library ownership
// classification. A work with both identifiers matches either representation.
type LookupWork struct {
	DOI   string `json:"doi,omitempty"`
	ArXiv string `json:"arxiv,omitempty"`
}

// LookupWorksRequest is the internal RPC input for bounded batch
// pre-acquisition deduplication.
type LookupWorksRequest struct {
	Works []LookupWork `json:"works"`
}

// WorkOwnership is aligned by index with LookupWorksRequest.Works.
type WorkOwnership struct {
	Status  string `json:"status"`
	ItemKey string `json:"item_key,omitempty"`
}

// LookupWorksResult reports ownership while preserving the input ordering.
// StalenessWarning is set when the best-effort mirror refresh could not finish.
type LookupWorksResult struct {
	Works            []WorkOwnership `json:"works"`
	StalenessWarning string          `json:"staleness_warning,omitempty"`
}

// LookupWorks classifies up to one batch of DOI/arXiv works against Zotio's
// synced mirror. A failed sync is deliberately non-fatal: the prior mirror is
// still useful, and the caller receives a bounded staleness warning. An
// unconfigured Zotio (nil CLI) is a supported mode, not an error: every work
// classifies as not-owned and the warning says ownership was not checked, so
// batch submit and watches keep working without a Zotero library.
func (s *Service) LookupWorks(ctx context.Context, request LookupWorksRequest) (*LookupWorksResult, error) {
	if len(request.Works) == 0 || len(request.Works) > 50 {
		return nil, fmt.Errorf("work lookup requires 1..50 works")
	}
	if s == nil || s.CLI == nil {
		result := &LookupWorksResult{
			Works:            make([]WorkOwnership, len(request.Works)),
			StalenessWarning: "Zotio is not configured; ownership was not checked",
		}
		for i := range result.Works {
			result.Works[i].Status = OwnershipNotOwned
		}
		return result, nil
	}
	result := &LookupWorksResult{Works: make([]WorkOwnership, len(request.Works))}
	for i := range result.Works {
		result.Works[i].Status = OwnershipNotOwned
	}
	if err := s.CLI.Sync(ctx); err != nil {
		result.StalenessWarning = "Zotio mirror sync failed; ownership classification may be stale"
	}

	lookupCache := make(map[string][]string)
	workKeys := make([][]string, len(request.Works))
	for i, item := range request.Works {
		identifiers, err := normalizedLookupIdentifiers(item)
		if err != nil {
			return nil, fmt.Errorf("work %d: %w", i+1, err)
		}
		seen := make(map[string]bool)
		for _, identifier := range identifiers {
			keys, ok := lookupCache[identifier.kind+":"+identifier.value]
			if !ok {
				keys, err = s.findParentItemKeys(ctx, identifier)
				if err != nil {
					return nil, err
				}
				lookupCache[identifier.kind+":"+identifier.value] = keys
			}
			for _, key := range keys {
				seen[key] = true
			}
		}
		for key := range seen {
			workKeys[i] = append(workKeys[i], key)
		}
		sort.Strings(workKeys[i])
	}

	owned := make(map[string]bool)
	for _, keys := range workKeys {
		for _, key := range keys {
			owned[key] = true
		}
	}
	if len(owned) == 0 {
		return result, nil
	}
	ownedKeys := make([]string, 0, len(owned))
	for key := range owned {
		ownedKeys = append(ownedKeys, key)
	}
	sort.Strings(ownedKeys)
	var missing []MissingPDFItem
	var missingErr error
	if keyed, ok := s.CLI.(interface {
		MissingPDFKeys(context.Context, []string) ([]MissingPDFItem, error)
	}); ok {
		missing, missingErr = keyed.MissingPDFKeys(ctx, ownedKeys)
	} else {
		missing, missingErr = s.CLI.MissingPDF(ctx, "", 500)
	}
	if missingErr != nil {
		return nil, fmt.Errorf("reading Zotio missing-PDF queue: %w", missingErr)
	}
	missingKeys := make(map[string]bool, len(missing))
	for _, item := range missing {
		if owned[item.Key] {
			missingKeys[item.Key] = true
		}
	}
	for i, keys := range workKeys {
		if len(keys) == 0 {
			continue
		}
		for _, key := range keys {
			if !missingKeys[key] {
				result.Works[i] = WorkOwnership{Status: OwnershipOwnedWithPDF, ItemKey: key}
				break
			}
		}
		if result.Works[i].Status == OwnershipOwnedWithPDF {
			continue
		}
		result.Works[i] = WorkOwnership{Status: OwnershipOwnedMissingPDF, ItemKey: keys[0]}
	}
	return result, nil
}

type lookupIdentifier struct {
	kind  string
	value string
}

func normalizedLookupIdentifiers(item LookupWork) ([]lookupIdentifier, error) {
	identifiers := make([]lookupIdentifier, 0, 2)
	if raw := strings.TrimSpace(item.DOI); raw != "" {
		doi, err := work.NormalizeDOI(raw)
		if err != nil {
			return nil, fmt.Errorf("normalizing DOI: %w", err)
		}
		identifiers = append(identifiers, lookupIdentifier{kind: "doi", value: doi})
	}
	if raw := strings.TrimSpace(item.ArXiv); raw != "" {
		arxiv, err := work.NormalizeArXiv(raw)
		if err != nil {
			return nil, fmt.Errorf("normalizing arXiv ID: %w", err)
		}
		identifiers = append(identifiers, lookupIdentifier{kind: "arxiv", value: arxiv})
	}
	return identifiers, nil
}

func (s *Service) findParentItemKeys(ctx context.Context, identifier lookupIdentifier) ([]string, error) {
	raw, err := s.CLI.RunJSON(ctx, "--agent", "items", "find", "--"+identifier.kind, identifier.value)
	if err != nil {
		return nil, fmt.Errorf("looking up Zotio %s %q: %w", identifier.kind, identifier.value, err)
	}
	var items []struct {
		Key  string `json:"key"`
		Data struct {
			ParentItem string `json:"parentItem"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decoding Zotio %s lookup: %w", identifier.kind, err)
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		key := strings.TrimSpace(item.Key)
		if item.Data.ParentItem == "" && keyRE.MatchString(key) {
			seen[key] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// MissingPDFCount reports how many library items (optionally within one
// collection) lack a PDF attachment, per Zotio's synced missing-PDF queue.
// It reads the local mirror only — no jobs are created.
func (s *Service) MissingPDFCount(ctx context.Context, collection string) (int, error) {
	if s == nil || s.CLI == nil {
		return 0, fmt.Errorf("Zotio integration is not configured")
	}
	items, err := s.CLI.MissingPDF(ctx, strings.TrimSpace(collection), 0)
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

// QueueMissingPDF preflights Zotio, scans its complete missing-PDF queue, and
// submits at most options.Limit deterministic papio requests. Re-running it
// returns existing live jobs.
func (s *Service) QueueMissingPDF(ctx context.Context, options QueueOptions) (*QueueResult, error) {
	if s == nil || s.CLI == nil || s.Submitter == nil {
		return nil, fmt.Errorf("Zotio integration is not configured")
	}
	if options.Limit == 0 {
		options.Limit = 25
	}
	if options.Limit < 1 || options.Limit > 500 {
		return nil, fmt.Errorf("limit must be in 1..500")
	}
	preflight, err := s.CLI.Preflight(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.CLI.MissingPDF(ctx, strings.TrimSpace(options.Collection), 0)
	if err != nil {
		return nil, err
	}
	if s.Store != nil {
		if err := s.observePersonalItems(ctx, items); err != nil {
			return nil, err
		}
	}
	result := &QueueResult{
		Preflight: preflight,
		Queued:    make([]QueuedJob, 0, options.Limit),
		Skipped:   make([]SkippedItem, 0, options.Limit),
	}
	appendSkipped := func(row MissingPDFItem, reason string) {
		if len(result.Skipped) < options.Limit {
			result.Skipped = append(result.Skipped, SkippedItem{
				ZotioItemKey: row.Key,
				Title:        row.Title,
				Reason:       reason,
			})
		}
	}
	for _, row := range items {
		if len(result.Queued) >= options.Limit {
			break
		}
		request, reason := s.requestForQueueItem(ctx, row, options)
		if reason != "" {
			appendSkipped(row, reason)
			continue
		}
		if s.Store != nil {
			if existing, lerr := s.liveJobForRequest(ctx, request.RequestID); lerr != nil {
				return nil, fmt.Errorf("checking live job for Zotio item %s: %w", row.Key, lerr)
			} else if existing != "" {
				// Deterministic request IDs make resubmission a no-op upstream,
				// but reporting the item as queued every run turns a stuck job
				// into recurring notification noise. Truthful count: skip it.
				appendSkipped(row, "already queued as "+existing)
				continue
			}
			if remaining, lerr := s.unavailableCooldown(ctx, request.RequestID); lerr != nil {
				return nil, fmt.Errorf("checking unavailable cool-down for Zotio item %s: %w", row.Key, lerr)
			} else if remaining > 0 {
				appendSkipped(row, fmt.Sprintf("unavailable as of last attempt; recheck in %s", formatCooldown(remaining)))
				continue
			}
		}
		jobID, err := s.Submitter.Submit(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("submitting Zotio item %s: %w", row.Key, err)
		}
		result.Queued = append(result.Queued, QueuedJob{
			ZotioItemKey: row.Key,
			RequestID:    request.RequestID,
			JobID:        jobID,
			Title:        request.Title,
		})
	}
	return result, nil
}

// observePersonalItems records provenance only after a personal-library Zotio
// scan returns the keys. Existing pre-v13 links remain scope-unknown and are
// deliberately ineligible for personal exception-tag writes until observed.
func (s *Service) observePersonalItems(ctx context.Context, items []MissingPDFItem) error {
	if len(items) == 0 {
		return nil
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO zotio_item_scope(item_key, scope, observed_at) VALUES (?, 'personal', ?)
			ON CONFLICT(item_key) DO UPDATE SET scope = excluded.scope, observed_at = excluded.observed_at`,
			item.Key, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording personal Zotero item scope: %w", err)
		}
		// A key that exists in the current missing-PDF scan is no longer a
		// permanent 404; let the reconciler attempt it again.
		if _, err := tx.ExecContext(ctx, `DELETE FROM zotio_tag_state WHERE item_key = ? AND status = 'missing'`, item.Key); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("resetting missing Zotero tag target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing personal Zotero item scope: %w", err)
	}
	return nil
}

// liveJobForRequest reports a non-terminal job already carrying requestID, so
// repeat backfill runs count only genuinely new submissions (mirrors the
// browser bridge's page_acquire duplicate check).
func (s *Service) liveJobForRequest(ctx context.Context, requestID string) (string, error) {
	var jobID string
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE work_request_id = ? AND state NOT IN ('failed','cancelled','unavailable') ORDER BY created_at DESC LIMIT 1`,
		requestID,
	).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// unavailableCooldown returns how long an item's unavailable outcome still
// parks it. Unavailability decays — green-OA copies appear months after
// publication, holdings change, adapters gain providers — so backfill
// re-checks after the configured window instead of retrying every cadence or
// never. Only the newest job counts: a newer failed/cancelled attempt means
// someone already chose to retry, and the cool-down must not resurrect the
// older verdict.
func (s *Service) unavailableCooldown(ctx context.Context, requestID string) (time.Duration, error) {
	if s.UnavailableRecheck <= 0 {
		return 0, nil
	}
	var state, updatedAt string
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT state, updated_at FROM jobs WHERE work_request_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		requestID,
	).Scan(&state, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if state != job.StateUnavailable {
		return 0, nil
	}
	decided, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		// An unparseable timestamp must not permanently wall off the item.
		return 0, nil
	}
	remaining := s.UnavailableRecheck - s.now().UTC().Sub(decided)
	if remaining < 0 {
		return 0, nil
	}
	return remaining, nil
}

// formatCooldown renders a human skip reason without sub-hour noise.
func formatCooldown(d time.Duration) string {
	const day = 24 * time.Hour
	if d >= day {
		return fmt.Sprintf("%dd", int((d+day-1)/day))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int((d+time.Hour-1)/time.Hour))
	}
	return "under an hour"
}

func (s *Service) requestForQueueItem(ctx context.Context, row MissingPDFItem, options QueueOptions) (protocol.WorkRequest, string) {
	request := protocol.WorkRequest{
		SchemaVersion:      protocol.WorkRequestSchemaVersion,
		RequestID:          "request_zotio_" + row.Key,
		Title:              strings.TrimSpace(row.Title),
		ZotioItemKey:       row.Key,
		Collection:         strings.TrimSpace(options.Collection),
		DesiredVersion:     defaultVersion(options.DesiredVersion),
		AccessModeOverride: strings.TrimSpace(options.AccessModeOverride),
		MaxCostUSD:         options.MaxCostUSD,
		SourcesAllow:       trimValues(options.SourcesAllow),
		SourcesDeny:        trimValues(options.SourcesDeny),
	}
	if row.DOI != "" {
		doi, err := work.NormalizeDOI(row.DOI)
		if err != nil {
			return protocol.WorkRequest{}, "invalid DOI from Zotio: " + err.Error()
		}
		request.Identifiers = &protocol.Identifiers{DOI: doi}
		// Best-effort: enrich the DOI-anchored request with the item's
		// creators/year, which the missing-PDF list row does not carry. A DOI is
		// not a reason to drop authors — downstream bundle export and new-item
		// routing want them. A lookup miss leaves the request DOI-only.
		if item, ierr := s.CLI.GetItem(ctx, row.Key); ierr == nil {
			if item.Title != "" {
				request.Title = item.Title
			}
			request.Authors = append([]string(nil), item.Authors...)
			request.Year = item.Year
		}
	} else {
		item, err := s.CLI.GetItem(ctx, row.Key)
		if err != nil {
			return protocol.WorkRequest{}, err.Error()
		}
		request.Title = item.Title
		request.Authors = append([]string(nil), item.Authors...)
		request.Year = item.Year
		if item.DOI != "" {
			doi, err := work.NormalizeDOI(item.DOI)
			if err != nil {
				return protocol.WorkRequest{}, "invalid DOI from Zotio item: " + err.Error()
			}
			request.Identifiers = &protocol.Identifiers{DOI: doi}
		}
	}
	if err := request.Validate(); err != nil {
		return protocol.WorkRequest{}, "insufficient Zotio identity: " + err.Error()
	}
	return request, ""
}

func defaultVersion(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "any"
}

func trimValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
