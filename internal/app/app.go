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
	"time"

	"papio/internal/artifact"
	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/fetch"
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

// ResolverEntry binds an adapter to its policy and estimated metadata-call cost.
type ResolverEntry struct {
	Adapter       resolver.Resolver
	Policy        config.Source
	EstimatedCost float64
}

// Service is the command-independent acquisition service.
type Service struct {
	Config    config.Config
	Jobs      *job.Store
	Artifacts *artifact.Store
	Budgets   *budget.Manager
	Resolvers []ResolverEntry
	Fetch     FetchFunc
	Validate  ValidateFunc

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
	desired := wr.DesiredVersion
	if desired == "" {
		desired = "any"
	}
	pol := job.Policy{
		AccessMode: mode, DesiredVersion: desired, MaxCostUSD: wr.MaxCostUSD,
		SourcesAllow:  append([]string(nil), wr.SourcesAllow...),
		SourcesDeny:   append([]string(nil), wr.SourcesDeny...),
		FetchMaxBytes: s.Config.Fetch.MaxBytes,
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

	// Resolver order step 1: verified local content-addressed cache.
	if row.Work.DOI != "" {
		cached, err := s.Jobs.FindArtifactByDOI(ctx, row.Work.DOI)
		if err != nil {
			return err
		}
		if cached != nil && s.Artifacts.Verify(cached.SHA256) == nil {
			return s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateReady,
				map[string]any{"source": "cache", "sha256": cached.SHA256}, job.WithArtifact(cached.SHA256))
		}
	}

	live, retryAt, err := s.resolve(ctx, row)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		if !retryAt.IsZero() {
			return s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateRetryWait,
				map[string]any{"reason": "resolver_temporarily_unavailable"}, job.WithRetryAt(retryAt))
		}
		return s.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", "no legal candidates")
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
	if _, err := s.Jobs.InsertCandidates(ctx, row.ID, persisted); err != nil {
		return nil, retryAt, err
	}
	return live, retryAt, nil
}

func (s *Service) fetchCandidates(ctx context.Context, row *job.Row, live map[string]resolver.Candidate, retryAt time.Time) error {
	manual := false
	for {
		stored, err := s.Jobs.NextPendingCandidate(ctx, row.ID)
		if err != nil {
			return err
		}
		if stored == nil {
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
		_, _ = s.Jobs.OpenHumanAction(ctx, row.ID, "manual_download", "a resolver returned a landing page but no verified direct PDF")
		return s.Jobs.Transition(ctx, row.ID, job.StateFetching, job.StateAwaitingHuman,
			map[string]any{"reason": "landing_page_only"})
	}
	if !retryAt.IsZero() {
		return s.Jobs.Transition(ctx, row.ID, job.StateFetching, job.StateRetryWait,
			map[string]any{"reason": "candidate_temporarily_unavailable"}, job.WithRetryAt(retryAt))
	}
	return s.exhaustedCandidates(ctx, row, job.StateFetching, "candidates_exhausted", "all candidates exhausted")
}

// exhaustedCandidates handles the terminal "no direct candidate" boundary —
// either resolving produced zero legal candidates or fetching exhausted them
// all without an artifact. With an OpenURL base configured, assisted/maximal
// jobs route to the visible institutional handoff (parked in awaiting_human
// with an open openurl_handoff action) instead of failing; conservative mode
// records that the institutional option exists but deliberately does not open
// it, then ends the job unavailable as before. The handoff detail is a static,
// redacted note: no signed query ever appears here.
func (s *Service) exhaustedCandidates(ctx context.Context, row *job.Row, from, reason, terminal string) error {
	mode := s.Config.AccessMode
	if s.Config.Browser.OpenURLBase != "" {
		switch mode {
		case config.ModeAssisted, config.ModeMaximal:
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff",
				"open-access candidates exhausted; institutional OpenURL handoff available in your browser"); err != nil {
				return err
			}
			return s.Jobs.Transition(ctx, row.ID, from, job.StateAwaitingHuman,
				map[string]any{"reason": "institutional_handoff"})
		case config.ModeConservative:
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
		return false, true, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "validation_error"})
	}
	active := report.Structural.HasJavaScript || report.Structural.HasEmbeddedFiles
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
		return false, true, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "encrypted_or_active_content"})
	case report.Text.NeedsReview || report.Identity.Result == pdf.IdentityReview:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, "semantic_or_identity_review")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
		_, _ = s.Jobs.OpenHumanAction(ctx, row.ID, "verify_identity", "PDF text or identity requires human verification")
		return false, true, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "semantic_or_identity_review"})
	case report.Identity.Result != pdf.IdentityPass:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "invalid", 0, "identity_rejected")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "invalid")
		_ = os.Remove(result.TempPath)
		return false, false, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateFetching,
			map[string]any{"reason": "wrong_work"})
	}

	dest, err := s.Artifacts.Promote(result.TempPath, result.SHA256)
	if err != nil {
		return false, false, err
	}
	art := job.Artifact{
		SHA256: result.SHA256, SizeBytes: result.SizeBytes, MIME: result.SniffedMIME,
		PageCount: report.Structural.Pages, TextChars: report.Text.Chars, OCRUsed: report.Text.OCRUsed,
		Encrypted: report.Structural.Encrypted, HasActiveContent: active,
		IdentityResult: report.Identity.Result, Path: dest,
	}
	if err := s.Jobs.UpsertArtifact(ctx, art); err != nil {
		return false, false, err
	}
	if err := s.Jobs.MarkCandidate(ctx, stored.ID, "accepted"); err != nil {
		return false, false, err
	}
	_ = s.Jobs.FinishAttempt(ctx, attempt, "accepted", 0, fmt.Sprintf("sha256=%s", result.SHA256))
	if err := s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateReady,
		map[string]any{"candidate_id": stored.ID, "sha256": result.SHA256},
		job.WithCandidate(stored.ID), job.WithArtifact(result.SHA256)); err != nil {
		return false, false, err
	}
	return true, false, nil
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
