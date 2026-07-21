// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package app implements command-independent acquisition use cases. It is the
// only layer that coordinates resolvers, durable jobs, bounded fetching, PDF
// validation, and immutable artifact promotion.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"papio/internal/artifact"
	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

// FetchFunc downloads one live in-memory candidate into the exact, nonexistent
// quarantine path supplied by the app.
type FetchFunc func(context.Context, resolver.Candidate, string) (fetch.Result, error)

// ValidateFunc validates one quarantined file against the requested work.
type ValidateFunc func(context.Context, string, string, work.Work) (pdf.ValidationReport, error)

// AutoImporter plans and applies one ready job through the Zotio service.
// Implementations must make replays idempotent.
type AutoImporter interface {
	PlanAndApply(context.Context, string) (status, parentKey, attachmentKey string, err error)
}

// MetadataEnricher adds corroborated identifiers to title-only work before
// resolvers use those identifiers to find acquisition candidates.
type MetadataEnricher interface {
	Enrich(context.Context, work.Work) (work.Work, bool, error)
}

// classifiedAutoImportError is implemented by the bootstrap decorator. It
// keeps Zotio-specific taxonomy out of the application service while allowing
// durable events to retain safe, actionable failure detail.
type classifiedAutoImportError interface {
	ErrorClass() string
	ErrorHint() string
	ErrorHTTPStatus() int
}

// NotificationSink receives best-effort daemon UX notifications after durable
// job state transitions.
type NotificationSink interface {
	HumanAction(context.Context)
	Imported(context.Context)
}

// ResolverEntry binds an adapter to its policy and estimated metadata-call cost.
type ResolverEntry struct {
	Adapter       resolver.Resolver
	Policy        config.Source
	EstimatedCost float64
}

// Service is the command-independent acquisition service.
type Service struct {
	Config       config.Config
	Jobs         *job.Store
	Artifacts    *artifact.Store
	Budgets      *budget.Manager
	Resolvers    []ResolverEntry
	Enricher     MetadataEnricher
	Fetch        FetchFunc
	Validate     ValidateFunc
	AutoImporter AutoImporter
	Notifier     NotificationSink
	// ReadyHook, when non-nil with a command, runs the user's on_ready hook
	// once per ready transition. Nil disables it.
	ReadyHook *hook.Runner
	// hookWG tracks launched on_ready hook goroutines so shutdown can drain
	// them before the store closes (DrainHooks).
	hookWG sync.WaitGroup

	RetryDelay time.Duration
	Now        func() time.Time
}

// New constructs a service and applies safe timing defaults.
func New(cfg config.Config, jobs *job.Store, artifacts *artifact.Store, budgets *budget.Manager) *Service {
	return &Service{
		Config: cfg, Jobs: jobs, Artifacts: artifacts, Budgets: budgets,
		RetryDelay: 30 * time.Second, Now: time.Now,
	}
}

// Submit strictly validates and canonicalizes a WorkRequest before creating its
// durable queued job. Config access_mode is always required; an optional
// request override is then snapshotted explicitly.
func (s *Service) Submit(ctx context.Context, wr protocol.WorkRequest) (string, error) {
	return s.SubmitWithAutoImport(ctx, wr, nil)
}

// SubmitWithAutoImport behaves like Submit while applying an optional per-job
// auto-import override. A nil override preserves the config default.
func (s *Service) SubmitWithAutoImport(ctx context.Context, wr protocol.WorkRequest, autoImport *bool) (string, error) {
	if err := wr.Validate(); err != nil {
		return "", err
	}
	mode, err := s.Config.RequireAccessMode()
	if err != nil {
		return "", err
	}
	if wr.AccessModeOverride != "" {
		mode = wr.AccessModeOverride
	}
	w, raw, err := canonicalWork(wr)
	if err != nil {
		return "", err
	}
	resolverName := strings.TrimSpace(wr.Resolver)
	if resolverName != "" {
		if _, ok := s.Config.OpenURLBaseFor(resolverName); !ok {
			names := s.Config.ResolverNames()
			if len(names) == 0 {
				return "", fmt.Errorf("unknown resolver %q (configured profiles: none)", resolverName)
			}
			return "", fmt.Errorf("unknown resolver %q (configured profiles: %s)", resolverName, strings.Join(names, ", "))
		}
	}
	desired := wr.DesiredVersion
	if desired == "" {
		desired = "any"
	}
	auto := s.Config.Zotio.AutoImport
	if autoImport != nil {
		auto = *autoImport
	}
	pol := job.Policy{
		AccessMode: mode, DesiredVersion: desired, Resolver: resolverName, MaxCostUSD: wr.MaxCostUSD,
		SourcesAllow:  append([]string(nil), wr.SourcesAllow...),
		SourcesDeny:   append([]string(nil), wr.SourcesDeny...),
		FetchMaxBytes: s.Config.Fetch.MaxBytes,
		AutoImport:    auto,
		Collection:    strings.TrimSpace(wr.Collection),
	}
	return s.Jobs.CreateRequest(ctx, wr.RequestID, w, wr.ZotioItemKey, wr.Collection, pol, raw)
}

func canonicalWork(wr protocol.WorkRequest) (work.Work, map[string]string, error) {
	w := work.Work{Title: wr.Title, Authors: append([]string(nil), wr.Authors...), Year: wr.Year}
	raw := make(map[string]string)
	if wr.Identifiers == nil {
		return w, raw, nil
	}
	var err error
	for _, item := range []struct {
		kind string
		raw  string
		dst  *string
		norm func(string) (string, error)
	}{
		{"doi", wr.Identifiers.DOI, &w.DOI, work.NormalizeDOI},
		{"pmid", wr.Identifiers.PMID, &w.PMID, work.NormalizePMID},
		{"arxiv", wr.Identifiers.ArXiv, &w.ArXiv, work.NormalizeArXiv},
		{"isbn", wr.Identifiers.ISBN, &w.ISBN, work.NormalizeISBN},
		{"openalex", wr.Identifiers.OpenAlex, &w.OpenAlex, work.NormalizeOpenAlex},
	} {
		if item.raw == "" {
			continue
		}
		*item.dst, err = item.norm(item.raw)
		if err != nil {
			return work.Work{}, nil, fmt.Errorf("normalizing %s: %w", item.kind, err)
		}
		raw[item.kind] = item.raw
	}
	return w, raw, nil
}

// Process executes one already-leased runnable job until it reaches ready,
// unavailable, a retry wait, or a human-review state. Live URLs and headers
// never escape this call; after a crash the state machine rewinds to resolving.
func (s *Service) Process(ctx context.Context, row *job.Row) error {
	if row == nil {
		return errors.New("nil job")
	}
	if s.Fetch == nil || s.Validate == nil {
		return errors.New("acquisition service is missing fetch/validation dependencies")
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.RetryDelay <= 0 {
		s.RetryDelay = 30 * time.Second
	}

	var err error
	switch row.State {
	case job.StateQueued, job.StateRetryWait:
		err = s.Jobs.Transition(ctx, row.ID, row.State, job.StateResolving,
			map[string]any{"reason": "scheduler_dispatch"})
		if err != nil {
			return err
		}
		row, err = s.Jobs.Get(ctx, row.ID)
		if err != nil {
			return err
		}
	case job.StateResolving:
		// Normal after startup recovery.
	case job.StateFetching, job.StateValidating:
		// A caller that skipped startup recovery cannot safely reuse a bearer URL.
		if err := s.Jobs.Transition(ctx, row.ID, row.State, job.StateResolving,
			map[string]any{"reason": "missing_live_candidate_after_recovery"}); err != nil {
			return err
		}
		row, err = s.Jobs.Get(ctx, row.ID)
		if err != nil {
			return err
		}
	default:
		return nil // terminal and parked human states are not runnable
	}
	// Runnable jobs cannot retain a user-review file: those states are not
	// claimed by the scheduler. Any contents here belong to a crashed attempt
	// that recovery has rewound before issuing fresh resolver URLs.
	if err := s.Artifacts.CleanQuarantine(row.ID); err != nil {
		return err
	}

	// Resolver order step 1: verified local content-addressed cache.
	if row.Work.DOI != "" {
		cached, err := s.Jobs.FindArtifactByDOI(ctx, row.Work.DOI)
		if err != nil {
			return err
		}
		if cached != nil && s.Artifacts.Verify(cached.SHA256) == nil {
			if err := s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateReady,
				map[string]any{"source": "cache", "sha256": cached.SHA256}, job.WithArtifact(cached.SHA256)); err != nil {
				return err
			}
			s.autoImportReady(ctx, row)
			s.runReadyHook(ctx, row, cached.SHA256)
			return nil
		}
	}

	live, retryAt, err := s.resolve(ctx, row)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		if !retryAt.IsZero() {
			if s.retryBudgetExhausted(ctx, row.ID) {
				return s.exhaustedCandidates(ctx, row, job.StateResolving, "retry_budget_exhausted", "temporary source failures did not clear", "")
			}
			return s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateRetryWait,
				map[string]any{"reason": "resolver_temporarily_unavailable"}, job.WithRetryAt(retryAt))
		}
		return s.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", "no legal candidates", "")
	}

	if err := s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateFetching,
		map[string]any{"candidates": len(live)}); err != nil {
		return err
	}
	row, err = s.Jobs.Get(ctx, row.ID)
	if err != nil {
		return err
	}
	return s.fetchCandidates(ctx, row, live, retryAt)
}

func (s *Service) resolve(ctx context.Context, row *job.Row) (map[string]resolver.Candidate, time.Time, error) {
	if err := s.Jobs.ResetCandidates(ctx, row.ID); err != nil {
		return nil, time.Time{}, err
	}
	var all []resolver.Candidate
	var retryAt time.Time
	if err := s.enrich(ctx, row); err != nil {
		return nil, time.Time{}, err
	}
	for _, entry := range s.Resolvers {
		if entry.Adapter == nil {
			continue
		}
		name := entry.Adapter.Name()
		if !row.Policy.SourceAllowed(name) || !entry.Policy.Enabled {
			continue
		}
		attempt, err := s.Jobs.StartAttempt(ctx, row.ID, 0, "resolve", name)
		if err != nil {
			return nil, retryAt, err
		}
		if s.Budgets != nil {
			if err := s.Budgets.Acquire(ctx, name, entry.Policy, entry.EstimatedCost); err != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
				var exceeded *budget.ErrExceeded
				if errors.As(err, &exceeded) {
					continue
				}
				return nil, retryAt, err
			}
		}
		cands, err := entry.Adapter.Resolve(ctx, row.Work)
		if err != nil {
			if ctx.Err() != nil {
				return nil, retryAt, ctx.Err()
			}
			if delay, temporary := resolver.Temporary(err); temporary {
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				retryAt = earlierTime(retryAt, sourceRetry)
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, name, sourceRetry)
				}
				_ = s.Jobs.FinishAttempt(ctx, attempt, "retryable", 0, safeType(err))
			} else {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "failed", 0, safeType(err))
			}
			continue
		}
		valid := 0
		for _, c := range cands {
			if c.Source == "" {
				c.Source = name
			}
			if c.Source != name || resolver.ValidateCandidate(c) != nil || conflicts(row.Work, c.ResolvedWork) {
				continue
			}
			all = append(all, c)
			valid++
		}
		_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, fmt.Sprintf("candidates=%d", valid))
	}

	if len(all) == 0 && strings.TrimSpace(row.Work.DOI) != "" {
		all = append(all, s.resolveSiblings(ctx, row)...)
	}

	ranked, evidence := resolver.Rank(row.Policy.DesiredVersion, all)
	resolved := row.Work
	for _, c := range ranked {
		resolved = fillMissing(resolved, c.ResolvedWork)
	}
	if !sameWork(resolved, row.Work) {
		updated, err := s.Jobs.FillWorkMetadata(ctx, row.ID, resolved)
		if err != nil {
			return nil, retryAt, err
		}
		row.Work = updated.Work
	}

	persisted, live := candidateRows(row, ranked, evidence)
	if _, err := s.Jobs.InsertCandidates(ctx, row.ID, persisted); err != nil {
		return nil, retryAt, err
	}
	return live, retryAt, nil
}

// candidateRows converts ranked candidates into their persisted (redacted)
// rows and the in-memory live map keyed for fetch-time lookup.
func candidateRows(row *job.Row, ranked []resolver.Candidate, evidence []string) ([]job.Candidate, map[string]resolver.Candidate) {
	persisted := make([]job.Candidate, 0, len(ranked))
	live := make(map[string]resolver.Candidate, len(ranked))
	for i, c := range ranked {
		key := c.Key()
		live[key] = c
		persisted = append(persisted, job.Candidate{
			JobID: row.ID, Source: c.Source, URLRedacted: redact.URL(c.URL), URLKey: key,
			LandingRedacted: redact.URL(c.Landing), Version: c.Version, AccessBasis: c.AccessBasis,
			ReuseLicense: c.ReuseLicense, ExpectedMIME: c.ExpectedMIME, CostUSD: c.CostUSD,
			Direct: c.Direct, IdentityConfidence: c.IdentityConfidence, RankEvidence: evidence[i], Rank: i,
		})
	}
	return persisted, live
}

// siblingHop is the C3 version hop at the fetch-exhaustion boundary: the
// canonical identifier produced candidates, every one failed, and the job is
// about to park or go unavailable. One OpenAlex sibling lookup runs and its
// candidates enter the normal pending queue. InsertCandidates deduplicates by
// url_key, so a repeated hop (later Process re-entry, or hop candidates
// themselves failing) inserts zero and the exhaustion verdict stands — no
// loop is possible.
func (s *Service) siblingHop(ctx context.Context, row *job.Row, live map[string]resolver.Candidate) bool {
	if strings.TrimSpace(row.Work.DOI) == "" {
		return false
	}
	cands := s.resolveSiblings(ctx, row)
	if len(cands) == 0 {
		return false
	}
	ranked, evidence := resolver.Rank(row.Policy.DesiredVersion, cands)
	persisted, hopLive := candidateRows(row, ranked, evidence)
	inserted, err := s.Jobs.InsertCandidates(ctx, row.ID, persisted)
	if err != nil || inserted == 0 {
		return false
	}
	for key, c := range hopLive {
		live[key] = c
	}
	return true
}

// SiblingResolver is the optional adapter capability behind the C3 version
// hop: when the canonical identifier yields zero legal candidates — or every
// candidate it yielded has failed (see siblingHop) — an adapter may look up
// open-access sibling versions (preprints, repository copies under a
// different DOI) of the same work.
type SiblingResolver interface {
	ResolveSiblings(ctx context.Context, requested work.Work) ([]resolver.Candidate, error)
}

// resolveSiblings runs the one-shot version hop. Sibling candidates carry
// deliberately different identifiers, so the identifier-conflict filter is
// skipped: strict title/author matching happened in the adapter, and PDF
// semantic-identity validation against row.Work remains the acceptance gate.
// Errors never fail resolution — the hop must not make an acquisition worse.
func (s *Service) resolveSiblings(ctx context.Context, row *job.Row) []resolver.Candidate {
	for _, entry := range s.Resolvers {
		sibling, ok := entry.Adapter.(SiblingResolver)
		if !ok {
			continue
		}
		name := entry.Adapter.Name()
		if !row.Policy.SourceAllowed(name) || !entry.Policy.Enabled {
			continue
		}
		// The attempts.stage CHECK allows only resolve/fetch/validate; the
		// sibling pass is a resolve-stage attempt distinguished by its detail.
		attempt, err := s.Jobs.StartAttempt(ctx, row.ID, 0, "resolve", name)
		if err != nil {
			return nil
		}
		if s.Budgets != nil {
			if err := s.Budgets.Acquire(ctx, name, entry.Policy, entry.EstimatedCost); err != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
				continue
			}
		}
		cands, err := sibling.ResolveSiblings(ctx, row.Work)
		if err != nil {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "failed", 0, safeType(err))
			continue
		}
		valid := make([]resolver.Candidate, 0, len(cands))
		for _, c := range cands {
			if c.Source != name || resolver.ValidateCandidate(c) != nil {
				continue
			}
			valid = append(valid, c)
		}
		_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, fmt.Sprintf("sibling_candidates=%d", len(valid)))
		if len(valid) > 0 {
			return valid
		}
	}
	return nil
}

func (s *Service) enrich(ctx context.Context, row *job.Row) error {
	if s.Enricher == nil || row.Work.DOI != "" || strings.TrimSpace(row.Work.Title) == "" {
		return nil
	}
	name := config.SourceCrossrefMetadata
	policy := s.Config.SourcePolicy(name)
	if !policy.Enabled || !row.Policy.SourceAllowed(name) {
		return nil
	}
	attempt, err := s.Jobs.StartAttempt(ctx, row.ID, 0, "resolve", name)
	if err != nil {
		return err
	}
	if s.Budgets != nil {
		if err := s.Budgets.Acquire(ctx, name, policy, 0); err != nil {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
			var exceeded *budget.ErrExceeded
			if errors.As(err, &exceeded) {
				return nil
			}
			return err
		}
	}
	enriched, matched, err := s.Enricher.Enrich(ctx, row.Work)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if delay, temporary := resolver.Temporary(err); temporary {
			if s.Budgets != nil {
				_ = s.Budgets.Defer(ctx, name, earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay))
			}
			_ = s.Jobs.FinishAttempt(ctx, attempt, "retryable", 0, safeType(err))
		} else {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "failed", 0, safeType(err))
		}
		return nil
	}
	if !matched {
		_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "no_confident_match")
		return nil
	}
	if conflicts(row.Work, enriched) {
		_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "metadata_conflict_rejected")
		return nil
	}
	updated, err := s.Jobs.FillWorkMetadata(ctx, row.ID, enriched)
	if err != nil {
		return err
	}
	row.Work = updated.Work
	_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "metadata_enriched")
	return nil
}

func (s *Service) fetchCandidates(ctx context.Context, row *job.Row, live map[string]resolver.Candidate, retryAt time.Time) error {
	manual := false
	// Candidate rows and events retain only redacted URLs. Keep the one
	// browser-eligible OA URL only through this acquisition pass, then record
	// it in the local handoff action if the job exhausts.
	oaBrowserURL := ""
	hopTried := false
	for {
		stored, err := s.Jobs.NextPendingCandidate(ctx, row.ID)
		if err != nil {
			return err
		}
		if stored == nil {
			// The pending queue drained. Before any terminal or parking
			// verdict, try the OA sibling hop once — but never pre-empt an
			// ordinary retry wait, where the primary candidates deserve
			// their next attempt first.
			if !hopTried {
				hopTried = true
				endsHere := manual || retryAt.IsZero() || s.retryBudgetExhausted(ctx, row.ID)
				if endsHere && s.siblingHop(ctx, row, live) {
					continue
				}
			}
			break
		}
		candidate, ok := live[stored.URLKey]
		if !ok {
			// A prior bearer URL survived only in redacted form; never reconstruct it.
			_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
			continue
		}
		if !candidate.Direct {
			manual = true
			_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
			continue
		}
		if err := s.Jobs.ReserveCost(ctx, row.ID, stored.Source, stored.CostUSD, row.Policy.MaxCostUSD); err != nil {
			var exceeded *job.ErrCostExceeded
			if errors.As(err, &exceeded) {
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
				continue
			}
			return err
		}
		if s.Budgets != nil {
			policy := s.Config.SourcePolicy(stored.Source)
			if err := s.Budgets.Acquire(ctx, stored.Source, policy, stored.CostUSD); err != nil {
				if releaseErr := s.Jobs.ReleaseReservedCost(context.WithoutCancel(ctx), row.ID, stored.Source, stored.CostUSD); releaseErr != nil {
					return releaseErr
				}
				var exceeded *budget.ErrExceeded
				if errors.As(err, &exceeded) {
					_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
					continue
				}
				return err
			}
		}

		if err := s.Jobs.MarkCandidate(ctx, stored.ID, "fetching"); err != nil {
			return err
		}
		attempt, err := s.Jobs.StartAttempt(ctx, row.ID, stored.ID, "fetch", stored.Source)
		if err != nil {
			return err
		}
		qdir, err := s.Artifacts.QuarantineDir(row.ID)
		if err != nil {
			return err
		}
		temp := filepath.Join(qdir, job.NewID("dl")+".tmp")
		result, err := s.Fetch(ctx, candidate, temp)
		if err != nil {
			if ctx.Err() != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "cancelled", 0, "context_cancelled")
				return ctx.Err()
			}
			_ = os.Remove(temp)
			class, status, delay := fetchFailure(err)
			_ = s.Jobs.FinishAttempt(ctx, attempt, class, status, safeType(err))
			if oaBrowserURL == "" && isOABrowserBlocked(candidate, err) {
				oaBrowserURL = candidate.URL
			}
			switch class {
			case fetch.ClassRetryable:
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "retryable")
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				retryAt = earlierTime(retryAt, sourceRetry)
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, stored.Source, sourceRetry)
				}
			case fetch.ClassBlocked:
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
			default:
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "invalid")
			}
			continue
		}
		if result.TempPath == "" {
			result.TempPath = temp
		}
		_ = s.Jobs.FinishAttempt(ctx, attempt, "success", result.HTTPStatus,
			fmt.Sprintf("bytes=%d mime=%s host=%s", result.SizeBytes, result.SniffedMIME, result.FinalHost))
		if err := s.Jobs.Transition(ctx, row.ID, job.StateFetching, job.StateValidating,
			map[string]any{"candidate_id": stored.ID, "source": stored.Source}, job.WithCandidate(stored.ID)); err != nil {
			_ = os.Remove(result.TempPath)
			return err
		}

		accepted, parked, err := s.validateCandidate(ctx, row, stored, result)
		if err != nil {
			return err
		}
		if accepted || parked {
			return nil
		}
		// Rejection returned the job to fetching to try the next candidate.
	}

	if manual {
		if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "manual_download", "a resolver returned a landing page but no verified direct PDF", job.WithAccessClassification(false, "landing_page")); err != nil {
			return err
		}
		return s.park(ctx, row.ID, job.StateFetching, job.StateAwaitingHuman,
			map[string]any{"reason": "landing_page_only"})
	}
	if !retryAt.IsZero() {
		if s.retryBudgetExhausted(ctx, row.ID) {
			return s.exhaustedCandidates(ctx, row, job.StateFetching, "retry_budget_exhausted", "temporary candidate failures did not clear", oaBrowserURL)
		}
		return s.Jobs.Transition(ctx, row.ID, job.StateFetching, job.StateRetryWait,
			map[string]any{"reason": "candidate_temporarily_unavailable"}, job.WithRetryAt(retryAt))
	}
	return s.exhaustedCandidates(ctx, row, job.StateFetching, "candidates_exhausted", "all candidates exhausted", oaBrowserURL)
}

func (s *Service) park(ctx context.Context, jobID, from, to string, detail map[string]any, opts ...job.TransitionOpt) error {
	if err := s.Jobs.Transition(ctx, jobID, from, to, detail, opts...); err != nil {
		return err
	}
	if s.Notifier != nil {
		s.Notifier.HumanAction(context.WithoutCancel(ctx))
	}
	return nil
}

// maxRetryAttempts bounds how many times a job may cycle through retry_wait for
// temporary resolver/fetch failures. A permanently "temporary" source would
// otherwise retry forever; past the cap the job escalates to the ordinary
// exhaustion boundary (institutional handoff, or unavailable) instead of
// re-scheduling another attempt.
const maxRetryAttempts = 8

// retryBudgetExhausted reports whether a job has already cycled through
// retry_wait at least maxRetryAttempts times. It counts durable transition
// events into retry_wait so the bound survives daemon restarts. A read error
// never escalates: best-effort maintenance prefers another retry to falsely
// giving up on a job.
func (s *Service) retryBudgetExhausted(ctx context.Context, jobID string) bool {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return false
	}
	n := 0
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if to, _ := detail["to"].(string); to == job.StateRetryWait {
			n++
		}
	}
	return n >= maxRetryAttempts
}

// exhaustedCandidates handles the terminal "no direct candidate" boundary —
// either resolving produced zero legal candidates or fetching exhausted them
// all without an artifact. A bot-blocked open-access candidate gets one
// browser-native attempt before the ordinary institutional OpenURL handoff.
// The action detail carries the live OA URL solely for the browser bridge; it
// is never copied into job events or protocol metadata.
func (s *Service) exhaustedCandidates(ctx context.Context, row *job.Row, from, reason, terminal, oaBrowserURL string) error {
	mode := s.Config.AccessMode
	switch mode {
	case config.ModeAssisted, config.ModeMaximal:
		if oaBrowserURL != "" {
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", OABrowserHandoffActionDetail(oaBrowserURL), job.WithAccessClassification(false, "anti_bot")); err != nil {
				return err
			}
			return s.park(ctx, row.ID, from, job.StateAwaitingHuman,
				map[string]any{"reason": "open_access_browser_handoff"})
		}
		if base, ok := s.Config.OpenURLBaseFor(row.Policy.Resolver); ok && base != "" {
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail, job.WithAccessClassification(true, "paywall")); err != nil {
				return err
			}
			return s.park(ctx, row.ID, from, job.StateAwaitingHuman,
				map[string]any{"reason": "institutional_handoff"})
		}
	case config.ModeConservative:
		if base, ok := s.Config.OpenURLBaseFor(row.Policy.Resolver); ok && base != "" {
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_available",
				"no direct candidates; institutional OpenURL available but not opened in conservative mode"); err != nil {
				return err
			}
		}
	}
	return s.Jobs.Transition(ctx, row.ID, from, job.StateUnavailable,
		map[string]any{"reason": reason}, job.WithTerminalReason(terminal))
}

func (s *Service) validateCandidate(ctx context.Context, row *job.Row, stored *job.Candidate, result fetch.Result) (accepted, parked bool, err error) {
	attempt, err := s.Jobs.StartAttempt(ctx, row.ID, stored.ID, "validate", stored.Source)
	if err != nil {
		return false, false, err
	}
	report, validateErr := s.Validate(ctx, result.TempPath, result.ContentType, row.Work)
	if validateErr != nil {
		if ctx.Err() != nil {
			_ = s.Jobs.FinishAttempt(context.WithoutCancel(ctx), attempt, "cancelled", 0, "context_cancelled")
			_ = os.Remove(result.TempPath)
			return false, false, ctx.Err()
		}
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, safeType(validateErr))
		_, _ = s.Jobs.OpenHumanAction(ctx, row.ID, "validation_error", "PDF validation could not complete within configured bounds")
		if err := s.Jobs.MarkCandidate(ctx, stored.ID, "skipped"); err != nil {
			return false, false, err
		}
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "validation_error"})
	}
	active := report.Structural.HasJavaScript || report.Structural.HasEmbeddedFiles
	needsIdentityReview := report.Text.NeedsReview || report.Identity.Result == pdf.IdentityReview
	switch {
	case !report.Payload.OK || !report.Structural.Valid:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "invalid", 0, "payload_or_structure_rejected")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "invalid")
		_ = os.Remove(result.TempPath)
		return false, false, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateFetching,
			map[string]any{"reason": "invalid_pdf"})
	case report.Structural.Encrypted || active:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, "encrypted_or_active_content")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
		_, _ = s.Jobs.OpenHumanAction(ctx, row.ID, "unsafe_pdf", "PDF is encrypted or contains active/embedded content")
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "encrypted_or_active_content"})
	case needsIdentityReview && !stored.ReviewOverride:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, "semantic_or_identity_review")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
		if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "verify_identity",
			fmt.Sprintf("PDF text or identity requires human verification; local quarantine file: %s", result.TempPath),
			job.WithHumanActionBinding(job.HumanActionBinding{
				CandidateID: stored.ID, QuarantinePath: result.TempPath, QuarantineSHA256: result.SHA256,
			}),
		); err != nil {
			return false, false, err
		}
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "semantic_or_identity_review"})
	case report.Identity.Result != pdf.IdentityPass && report.Identity.Result != pdf.IdentityReview:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "invalid", 0, "identity_rejected")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "invalid")
		_ = os.Remove(result.TempPath)
		return false, false, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateFetching,
			map[string]any{"reason": "wrong_work"})
	}

	dest, err := s.Artifacts.ArtifactPath(result.SHA256)
	if err != nil {
		return false, false, err
	}
	identityResult := report.Identity.Result
	if stored.ReviewOverride && needsIdentityReview {
		identityResult = "user_confirmed"
	}
	art := job.Artifact{
		SHA256: result.SHA256, SizeBytes: result.SizeBytes, MIME: result.SniffedMIME,
		PageCount: report.Structural.Pages, TextChars: report.Text.Chars, OCRUsed: report.Text.OCRUsed,
		Encrypted: report.Structural.Encrypted, HasActiveContent: active,
		IdentityResult: identityResult, Path: dest,
	}
	// Persist the metadata before the atomic rename so a database failure
	// cannot leave an immutable file with no durable owner.
	existingArtifact, err := s.Jobs.GetArtifact(ctx, result.SHA256)
	if err != nil {
		return false, false, err
	}
	if err := s.Jobs.UpsertArtifact(ctx, art); err != nil {
		return false, false, err
	}
	if _, err := s.Artifacts.Promote(result.TempPath, result.SHA256); err != nil {
		if existingArtifact == nil {
			if _, cleanupErr := s.Jobs.S.DB().ExecContext(context.WithoutCancel(ctx),
				`DELETE FROM artifacts WHERE sha256 = ?`, result.SHA256); cleanupErr != nil {
				return false, false, errors.Join(err, fmt.Errorf("removing unpromoted artifact metadata: %w", cleanupErr))
			}
		}
		return false, false, err
	}
	if err := s.Jobs.MarkCandidate(ctx, stored.ID, "accepted"); err != nil {
		return false, false, err
	}
	acceptDetail := map[string]any{"candidate_id": stored.ID, "sha256": result.SHA256}
	if stored.ReviewOverride && needsIdentityReview {
		acceptDetail["reason"] = "human_identity_override"
	}
	_ = s.Jobs.FinishAttempt(ctx, attempt, "accepted", 0, fmt.Sprintf("sha256=%s", result.SHA256))
	if err := s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateReady,
		acceptDetail, job.WithCandidate(stored.ID), job.WithArtifact(result.SHA256)); err != nil {
		return false, false, err
	}
	s.autoImportReady(ctx, row)
	s.runReadyHook(ctx, row, result.SHA256)
	return true, false, nil
}

func (s *Service) autoImportReady(ctx context.Context, row *job.Row) {
	if !row.Policy.AutoImport {
		return
	}
	eventCtx := context.WithoutCancel(ctx)
	detail := map[string]any{"parent_key": "", "attachment_key": ""}
	if s.AutoImporter == nil {
		detail["status"] = "skipped"
		detail["reason"] = "zotio_not_configured"
		_ = s.Jobs.RecordEvent(eventCtx, row.ID, "zotio.auto_import", detail)
		return
	}
	status, parentKey, attachmentKey, err := s.AutoImporter.PlanAndApply(ctx, row.ID)
	detail["parent_key"] = parentKey
	detail["attachment_key"] = attachmentKey
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		class, hint, httpStatus := autoImportErrorInfo(err)
		detail["status"] = "error"
		detail["error_type"] = safeType(err)
		detail["error_class"] = class
		if hint != "" {
			detail["error_hint"] = hint
		}
		if httpStatus != 0 {
			detail["error_http_status"] = httpStatus
		}
		_ = s.Jobs.RecordEvent(eventCtx, row.ID, "zotio.auto_import", detail)
		return
	}
	detail["status"] = status
	_ = s.Jobs.RecordEvent(eventCtx, row.ID, "zotio.auto_import", detail)
	if status == "applied" && s.Notifier != nil {
		s.Notifier.Imported(eventCtx)
	}
}

// runReadyHook fires the user's on_ready hook exactly once per ready
// transition, detached from the caller: hook latency or failure never blocks
// or fails acquisition. Import retries never re-fire it. Raw hook stderr is
// deliberately NEVER persisted: the hook inherits the daemon environment, so
// its output can carry credentials, and durable events must stay
// secret-free. The durable audit trail is status, exit code, and duration.
func (s *Service) runReadyHook(ctx context.Context, row *job.Row, sha string) {
	if s.ReadyHook == nil || strings.TrimSpace(s.ReadyHook.Command) == "" {
		return
	}
	eventCtx := context.WithoutCancel(ctx)
	pdfPath, err := s.Artifacts.ArtifactPath(sha)
	if err == nil && !filepath.IsAbs(pdfPath) {
		// The env contract promises an absolute path; a relative data_dir
		// must not leak a cwd-dependent PAPIO_PDF to the hook.
		pdfPath, err = filepath.Abs(pdfPath)
	}
	if err != nil {
		_ = s.Jobs.RecordEvent(eventCtx, row.ID, "hook.on_ready",
			map[string]any{"status": "error", "reason": "artifact_path"})
		return
	}
	env := map[string]string{
		"PAPIO_JOB_ID":     row.ID,
		"PAPIO_REQUEST_ID": row.WorkRequestID,
		"PAPIO_DOI":        row.Work.DOI,
		"PAPIO_ARXIV":      row.Work.ArXiv,
		"PAPIO_TITLE":      row.Work.Title,
		"PAPIO_SHA256":     sha,
		"PAPIO_PDF":        pdfPath,
		"PAPIO_STATE":      "ready",
	}
	jobID := row.ID
	s.hookWG.Add(1)
	go func() {
		defer s.hookWG.Done()
		result := s.ReadyHook.Run(eventCtx, env)
		detail := map[string]any{
			"exit_code":   result.ExitCode,
			"duration_ms": result.Duration.Milliseconds(),
		}
		if result.Err == nil && result.ExitCode == 0 {
			detail["status"] = "ok"
		} else {
			detail["status"] = "error"
		}
		_ = s.Jobs.RecordEvent(eventCtx, jobID, "hook.on_ready", detail)
	}()
}

// DrainHooks waits up to timeout for launched on_ready hooks to finish and
// record their events. Shutdown calls it before the store closes so a hook
// outcome insert does not race the SQLite connection teardown; hooks that
// outlive the timeout are abandoned (their own deadline still bounds them).
func (s *Service) DrainHooks(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.hookWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// InstitutionalOpenURLHandoffDetail describes the ordinary resolver handoff.
// The browser bridge uses this same durable detail when a one-time OA offer
// fails, so a re-park cannot alternate back to the OA candidate.
const InstitutionalOpenURLHandoffDetail = "institutional OpenURL handoff: sign in to your institution first, then run 'papio actions open' — a fresh link is generated on each open; if the provider reports a stale or expired session, re-run 'papio actions open'"

// OABrowserHandoffDetail identifies a handoff that must open the public OA
// candidate itself rather than constructing an institutional OpenURL. The
// second line is consumed only by the local browser bridge; protocol frames
// continue to use their frozen openurl field.
const OABrowserHandoffDetail = "open-access fetch via browser"

func OABrowserHandoffActionDetail(url string) string {
	return OABrowserHandoffDetail + "\n" + url
}

// OABrowserHandoffURL returns the live OA offer URL stored in an OA browser
// handoff detail. The strict two-line shape avoids accepting an arbitrary
// human-action message as a browser URL.
func OABrowserHandoffURL(detail string) (string, bool) {
	const prefix = OABrowserHandoffDetail + "\n"
	url, ok := strings.CutPrefix(detail, prefix)
	if !ok || url == "" || strings.ContainsAny(url, "\r\n") || !strings.HasPrefix(url, "https://") {
		return "", false
	}
	return url, true
}

func isOABrowserBlocked(candidate resolver.Candidate, err error) bool {
	if candidate.AccessBasis != resolver.AccessOpen || !strings.HasPrefix(candidate.URL, "https://") {
		return false
	}
	var fe *fetch.Error
	if !errors.As(err, &fe) {
		return false
	}
	if fe.HTTPStatus == http.StatusForbidden {
		return true
	}
	// Fetch keeps the classification message redacted. Challenge/captcha
	// payloads are meaningful here: the ordinary browser can clear a public
	// anti-bot gate without presenting an institutional credential.
	msg := strings.ToLower(fe.Msg)
	return strings.Contains(msg, "challenge") || strings.Contains(msg, "anti-bot") || strings.Contains(msg, "captcha")
}

func fetchFailure(err error) (class string, status int, delay time.Duration) {
	var fe *fetch.Error
	if errors.As(err, &fe) {
		return fe.Class, fe.HTTPStatus, fe.RetryAfter
	}
	return fetch.ClassRetryable, 0, 0
}

func safeType(err error) string {
	if err == nil {
		return ""
	}
	// Persist only the type/category, never arbitrary upstream text that may
	// contain a bearer URL, query, body, token, or credential.
	return fmt.Sprintf("%T", err)
}

func autoImportErrorInfo(err error) (class, hint string, httpStatus int) {
	class = "unknown"
	var classified classifiedAutoImportError
	if errors.As(err, &classified) {
		if value := strings.TrimSpace(classified.ErrorClass()); value != "" {
			class = value
		}
		hint = strings.TrimSpace(classified.ErrorHint())
		httpStatus = classified.ErrorHTTPStatus()
	}
	return class, hint, httpStatus
}

func earlierTime(current, candidate time.Time) time.Time {
	if current.IsZero() || (!candidate.IsZero() && candidate.Before(current)) {
		return candidate
	}
	return current
}

func earlierRetry(current time.Time, now time.Time, delay, fallback time.Duration) time.Time {
	if delay <= 0 {
		delay = fallback
	}
	candidate := now.UTC().Add(delay)
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func conflicts(base, observed work.Work) bool {
	for _, pair := range [][2]string{
		{base.DOI, observed.DOI}, {base.PMID, observed.PMID}, {base.ArXiv, observed.ArXiv},
		{base.ISBN, observed.ISBN}, {base.OpenAlex, observed.OpenAlex},
	} {
		if pair[0] != "" && pair[1] != "" && !strings.EqualFold(pair[0], pair[1]) {
			return true
		}
	}
	return false
}

func sameWork(a, b work.Work) bool {
	if a.DOI != b.DOI || a.PMID != b.PMID || a.ArXiv != b.ArXiv || a.ISBN != b.ISBN ||
		a.OpenAlex != b.OpenAlex || a.Title != b.Title || a.Year != b.Year || len(a.Authors) != len(b.Authors) {
		return false
	}
	for i := range a.Authors {
		if a.Authors[i] != b.Authors[i] {
			return false
		}
	}
	return true
}

func fillMissing(base, observed work.Work) work.Work {
	if conflicts(base, observed) {
		return base
	}
	for _, pair := range []struct {
		dst   *string
		value string
	}{
		{&base.DOI, observed.DOI}, {&base.PMID, observed.PMID}, {&base.ArXiv, observed.ArXiv},
		{&base.ISBN, observed.ISBN}, {&base.OpenAlex, observed.OpenAlex}, {&base.Title, observed.Title},
	} {
		if *pair.dst == "" {
			*pair.dst = pair.value
		}
	}
	if len(base.Authors) == 0 && len(observed.Authors) > 0 {
		base.Authors = append([]string(nil), observed.Authors...)
	}
	if base.Year == 0 {
		base.Year = observed.Year
	}
	return base
}

// RequestForCandidate constructs the ephemeral retrieval request. It exists in
// app rather than resolver/job so neither durable layer needs net/http types.
func RequestForCandidate(ctx context.Context, c resolver.Candidate) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range c.RequestHeaders {
		req.Header.Set(name, value)
	}
	return req, nil
}

// StableResolverNames returns the configured adapter names for doctor output.
func (s *Service) StableResolverNames() []string {
	names := make([]string, 0, len(s.Resolvers))
	for _, r := range s.Resolvers {
		if r.Adapter != nil {
			names = append(names, r.Adapter.Name())
		}
	}
	sort.Strings(names)
	return names
}
