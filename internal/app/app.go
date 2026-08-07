// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package app implements command-independent acquisition use cases. It is the
// only layer that coordinates resolvers, durable jobs, bounded fetching, PDF
// validation, and immutable artifact promotion.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/artifact"
	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/discovery"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/illiad"
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

// WorkLookup retrieves discovery metadata for a requested DOI.
type WorkLookup interface {
	LookupWork(context.Context, string) (discovery.DiscoveredWork, error)
}

// DOIRegistry reports whether a DOI is registered with the global Handle
// System. The bool is only meaningful when the error is nil: an unreachable
// registry means "unknown", never "unregistered".
type DOIRegistry interface {
	Registered(context.Context, string) (bool, error)
}

// LandingReader fetches a publisher landing page and returns the
// citation_pdf_url it advertises, without the app layer doing any I/O of its
// own. It exists so a permanently failed open-access candidate can fall back
// to the very landing page its own candidate row already carries in
// LandingRedacted (see expandLandingSeeds) instead of an institutional
// handoff the paper never needed.
type LandingReader interface {
	PDFURLFor(ctx context.Context, landingURL string) (string, error)
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
	HumanActionReminder(context.Context, string)
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
	Config        config.Config
	Jobs          *job.Store
	Artifacts     *artifact.Store
	Budgets       *budget.Manager
	Resolvers     []ResolverEntry
	Enricher      MetadataEnricher
	Discovery     WorkLookup
	DOIRegistry   DOIRegistry
	LandingReader LandingReader
	Fetch         FetchFunc
	Validate      ValidateFunc
	AutoImporter  AutoImporter
	Notifier      NotificationSink
	// Delivery is ADR-0017's document-delivery/ILL service (Decisions 1,
	// 3A-3C, 4). Nil disables the feature entirely: exhaustedCandidates
	// falls back to its pre-ADR-0017 OpenURL/no_entitlement behavior
	// exactly, byte for byte — the same "unconfigured" contract an
	// institution profile with no document_delivery block gets even when
	// Delivery is set. Bootstrap wiring (constructing this from the daemon's
	// store/config) lives outside this package.
	Delivery *delivery.Service
	// IlliadHTTPClient is the transport internal/illiad uses for every
	// per-request Client this package constructs (one per institution
	// profile, from that profile's document_delivery.base_url/api_key).
	// Defaults to http.DefaultClient in New; tests inject an httptest
	// server's client instead.
	IlliadHTTPClient illiad.HTTPClient
	// ReadyHook, when non-nil with a command, runs the user's on_ready hook
	// once per ready transition. Nil disables it.
	ReadyHook *hook.Runner
	// hookWG tracks launched on_ready hook goroutines so shutdown can drain
	// them before the store closes (DrainHooks).
	hookWG sync.WaitGroup

	RetryDelay time.Duration
	Now        func() time.Time
}

// SubmitOptions keeps explicit retry intent at the application boundary so
// callers cannot accidentally create parallel live acquisition attempts.
type SubmitOptions struct {
	AutoImport *bool
	Force      bool
	// Consumer names the caller for its own accounting. Empty records no
	// attribution rather than a placeholder one, and a submission that matches
	// an in-flight job does not overwrite the attribution that job was queued
	// with.
	//
	// It is a label the caller supplies for itself. papio does not authenticate
	// it, cannot verify it, and it is NOT a rights input: it must never be read
	// as the entitlement holder or as an acquiring principal, the same refusal
	// ADR-0009 Decision 3 places on the transport principal. papio authenticates
	// nobody and holds no institutional credential.
	Consumer string
}

// SubmitResult lets callers distinguish a newly queued job from an in-flight
// job that already owns the requested work.
type SubmitResult struct {
	JobID    string
	Existing bool
}

// New constructs a service and applies safe timing defaults.
func New(cfg config.Config, jobs *job.Store, artifacts *artifact.Store, budgets *budget.Manager) *Service {
	return &Service{
		Config: cfg, Jobs: jobs, Artifacts: artifacts, Budgets: budgets,
		RetryDelay: 30 * time.Second, Now: time.Now,
		IlliadHTTPClient: http.DefaultClient,
	}
}

// Submit strictly validates and canonicalizes a WorkRequest before creating or
// reusing its durable job. Config access_mode is always required; an optional
// request override is then snapshotted explicitly.
func (s *Service) Submit(ctx context.Context, wr protocol.WorkRequest) (string, error) {
	return s.SubmitAs(ctx, job.PrincipalUnknown, wr)
}

// SubmitAs creates or reuses a durable job while recording the principal whose
// entitlement initiated the acquisition.
func (s *Service) SubmitAs(ctx context.Context, principal job.Principal, wr protocol.WorkRequest) (string, error) {
	result, err := s.SubmitWithOptionsAs(ctx, principal, wr, SubmitOptions{})
	if err != nil {
		return "", err
	}
	return result.JobID, nil
}

// SubmitWithAutoImport behaves like Submit while applying an optional per-job
// auto-import override. A nil override preserves the config default.
func (s *Service) SubmitWithAutoImport(ctx context.Context, wr protocol.WorkRequest, autoImport *bool) (string, error) {
	result, err := s.SubmitWithOptionsAs(ctx, job.PrincipalUnknown, wr, SubmitOptions{AutoImport: autoImport})
	if err != nil {
		return "", err
	}
	return result.JobID, nil
}

// SubmitWithOptions returns Existing when an in-flight job already owns the
// canonical work. Force deliberately creates a fresh request instead.
func (s *Service) SubmitWithOptions(ctx context.Context, wr protocol.WorkRequest, options SubmitOptions) (SubmitResult, error) {
	return s.SubmitWithOptionsAs(ctx, job.PrincipalUnknown, wr, options)
}

// SubmitWithOptionsAs is SubmitWithOptions with explicit durable provenance.
func (s *Service) SubmitWithOptionsAs(ctx context.Context, principal job.Principal, wr protocol.WorkRequest, options SubmitOptions) (SubmitResult, error) {
	if err := wr.Validate(); err != nil {
		return SubmitResult{}, err
	}
	mode, err := s.Config.RequireAccessMode()
	if err != nil {
		return SubmitResult{}, err
	}
	// Narrow-only: see config.NarrowAccessMode. The snapshot below therefore
	// records the mode that will actually govern, so diagnose does not report an
	// override that the daemon declined to honour.
	mode = s.Config.NarrowAccessMode(mode, wr.AccessModeOverride)
	w, raw, err := canonicalWork(wr)
	if err != nil {
		return SubmitResult{}, err
	}
	resolverName := strings.TrimSpace(wr.Resolver)
	if resolverName == "" {
		resolverName = strings.TrimSpace(s.Config.Browser.DefaultResolver)
	}
	if resolverName != "" {
		if _, ok := s.Config.OpenURLBaseFor(resolverName); !ok {
			names := s.Config.ResolverNames()
			if len(names) == 0 {
				return SubmitResult{}, fmt.Errorf("unknown resolver %q (configured profiles: none)", resolverName)
			}
			return SubmitResult{}, fmt.Errorf("unknown resolver %q (configured profiles: %s)", resolverName, strings.Join(names, ", "))
		}
	}
	desired := wr.DesiredVersion
	if desired == "" {
		desired = "any"
	}
	auto := s.Config.Zotio.AutoImport
	if options.AutoImport != nil {
		auto = *options.AutoImport
	}
	pol := job.Policy{
		AccessMode: mode, DesiredVersion: desired, Resolver: resolverName, MaxCostUSD: wr.MaxCostUSD,
		SourcesAllow:  append([]string(nil), wr.SourcesAllow...),
		SourcesDeny:   append([]string(nil), wr.SourcesDeny...),
		FetchMaxBytes: s.Config.Fetch.MaxBytes,
		AutoImport:    auto,
		Collection:    strings.TrimSpace(wr.Collection),
	}
	consumer, err := validConsumer(options.Consumer)
	if err != nil {
		return SubmitResult{}, err
	}
	created, err := s.Jobs.CreateRequestForWork(ctx, wr.RequestID, w, wr.ZotioItemKey, wr.Collection, pol, raw,
		job.Attribution{Principal: principal, Consumer: consumer}, options.Force)
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{JobID: created.JobID, Existing: created.Existing}, nil
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
	case job.StateFetching:
		reused, err := s.reuseAcceptedReview(ctx, row)
		if err != nil {
			return err
		}
		if reused {
			return nil
		}
		fallthrough
	case job.StateValidating:
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
	// Runnable jobs cannot retain a user-review file unless an accepted identity
	// review still binds a verified quarantined PDF for promotion. All other
	// contents belong to a crashed attempt rewound before fresh resolver URLs.
	if err := s.Artifacts.CleanQuarantine(row.ID); err != nil {
		return err
	}

	// Resolver order step 1: verified local content-addressed cache.
	if row.Work.DOI != "" {
		cached, source, err := s.Jobs.FindArtifactByDOI(ctx, row.Work.DOI)
		if err != nil {
			return err
		}
		if cached != nil && s.Artifacts.Verify(cached.SHA256) == nil {
			// Carry the source acquisition's candidate: these bytes are its
			// bytes, so its licence and access basis are the honest provenance
			// of this job's artifact. Recording it also replaces any stale
			// selection this job carried in from a rejected earlier attempt,
			// which a bare WithArtifact transition would preserve (ADR-0007).
			detail := map[string]any{"source": "cache", "sha256": cached.SHA256}
			opts := []job.TransitionOpt{job.WithArtifact(cached.SHA256)}
			if source != nil {
				detail["provenance_candidate_id"] = source.ID
				opts = append(opts, job.WithCandidate(source.ID))
			}
			if err := s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateReady, detail, opts...); err != nil {
				return err
			}
			s.autoImportReady(ctx, row)
			s.runReadyHook(ctx, row, cached.SHA256)
			return nil
		}
	}

	live, plan, err := s.resolve(ctx, row)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		if !plan.IsZero() {
			return s.parkForRetry(ctx, row, job.StateResolving, plan,
				map[string]any{"reason": "resolver_temporarily_unavailable"},
				job.TerminalReasonTemporarySourceFailuresDidNotClear, "")
		}
		return s.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", job.TerminalReasonNoLegalCandidates, "")
	}

	if err := s.Jobs.Transition(ctx, row.ID, job.StateResolving, job.StateFetching,
		map[string]any{"candidates": len(live)}); err != nil {
		return err
	}
	row, err = s.Jobs.Get(ctx, row.ID)
	if err != nil {
		return err
	}
	return s.fetchCandidates(ctx, row, live, plan)
}

// reuseAcceptedReview promotes the exact bytes a human accepted, rather than
// issuing a fresh resolver URL that may no longer identify the same candidate.
func (s *Service) reuseAcceptedReview(ctx context.Context, row *job.Row) (bool, error) {
	binding, err := s.Jobs.AcceptedReviewBinding(ctx, row.ID)
	if err != nil {
		return false, err
	}
	if binding == nil {
		return false, nil
	}
	result, ok := reviewedFetchResult(binding)
	if !ok {
		return false, nil
	}
	stored, err := s.Jobs.GetCandidate(ctx, binding.CandidateID)
	if err != nil {
		return false, err
	}
	// A browser-adopted candidate accepted before papio stopped synthesizing
	// versions still carries that claim: the human confirmed the bytes are the
	// right work, never which version they are (ADR-0007). Resolver candidates
	// keep their version — theirs came from the source, not from a guess.
	if stored.Source == "browser" && stored.Version != resolver.VersionUnknown {
		if err := s.Jobs.MarkCandidateVersionUnobserved(ctx, stored.ID); err != nil {
			return false, err
		}
		stored.Version = resolver.VersionUnknown
	}
	if err := s.Jobs.Transition(ctx, row.ID, job.StateFetching, job.StateValidating,
		map[string]any{"reason": "review_accepted_reuse", "candidate_id": stored.ID, "source": stored.Source},
		job.WithCandidate(stored.ID)); err != nil {
		return false, err
	}
	accepted, parked, err := s.validateCandidate(ctx, row, stored, result)
	if err != nil {
		return false, err
	}
	return accepted || parked, nil
}

// reviewedFetchResult verifies the review binding without loading a PDF into
// memory before it is passed to the normal validation and promotion path.
func reviewedFetchResult(binding *job.HumanActionBinding) (fetch.Result, bool) {
	info, err := os.Stat(binding.QuarantinePath)
	if err != nil || !info.Mode().IsRegular() {
		return fetch.Result{}, false
	}
	file, err := os.Open(binding.QuarantinePath)
	if err != nil {
		return fetch.Result{}, false
	}
	defer func() { _ = file.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return fetch.Result{}, false
	}
	if !strings.EqualFold(hex.EncodeToString(sum.Sum(nil)), binding.QuarantineSHA256) {
		return fetch.Result{}, false
	}
	return fetch.Result{
		TempPath: binding.QuarantinePath, SHA256: binding.QuarantineSHA256, SizeBytes: info.Size(),
		SniffedMIME: "application/pdf", ContentType: "application/pdf",
	}, true
}

func (s *Service) resolve(ctx context.Context, row *job.Row) (map[string]resolver.Candidate, retryPlan, error) {
	var plan retryPlan
	if err := s.enrichDOIWork(ctx, row); err != nil {
		return nil, plan, err
	}
	if err := s.Jobs.ResetCandidates(ctx, row.ID); err != nil {
		return nil, plan, err
	}
	var all []resolver.Candidate
	if err := s.enrich(ctx, row); err != nil {
		return nil, plan, err
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
			return nil, plan, err
		}
		if s.Budgets != nil {
			if err := s.Budgets.Acquire(ctx, name, entry.Policy, entry.EstimatedCost); err != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
				var exceeded *budget.ErrExceeded
				if errors.As(err, &exceeded) {
					continue
				}
				// The source is gated too far out to wait on (typically a daily
				// quota reset). Skip it and let the job park until the gate
				// lifts, instead of holding this worker's claim until then.
				var deferred *budget.ErrDeferred
				if errors.As(err, &deferred) {
					plan.Gate = earlierTime(plan.Gate, deferred.Until)
					plan.ClosedSourceGates++
					continue
				}
				return nil, plan, err
			}
		}
		cands, err := entry.Adapter.Resolve(ctx, row.Work)
		if err != nil {
			if ctx.Err() != nil {
				return nil, plan, ctx.Err()
			}
			if delay, temporary := resolver.Temporary(err); temporary {
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
				plan.TemporaryResolvers++
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, name, entry.Policy, sourceRetry)
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
		siblings, siblingPlan := s.resolveSiblings(ctx, row)
		all = append(all, siblings...)
		plan.merge(siblingPlan)
	}

	ranked, evidence := resolver.Rank(row.Policy.DesiredVersion, all)
	resolved := row.Work
	for _, c := range ranked {
		resolved = fillMissing(resolved, c.ResolvedWork)
	}
	if !sameWork(resolved, row.Work) {
		updated, err := s.Jobs.FillWorkMetadata(ctx, row.ID, resolved)
		if err != nil {
			return nil, plan, err
		}
		row.Work = updated.Work
	}

	persisted, live := candidateRows(row, ranked, evidence)
	if _, err := s.Jobs.InsertCandidates(ctx, row.ID, persisted); err != nil {
		return nil, plan, err
	}
	return live, plan, nil
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

// landingSeed is a landing page worth checking for citation_pdf_url,
// captured in fetchCandidates from a candidate that just failed
// permanently. It carries enough of the parent candidate to build a
// fetchable derivative without re-running source policy or budget lookups:
// the derived candidate is the same observation reached a second way, not a
// new one.
type landingSeed struct {
	landingURL string
	parent     resolver.Candidate
	parentKey  string
}

// landingDerivedEventKind durably records that a landing page produced a
// derived PDF candidate. Like oaBrowserHintEventKind, the detail carries
// only url_keys — never the landing URL or the derived PDF URL.
const landingDerivedEventKind = "job.landing_derived"

// expandLandingSeeds is the acquisition side of the 10.3389/feduc.2018.00095
// incident: Unpaywall and OpenAlex both returned the identical expired
// Azure SAS link, both candidates 403'd and were correctly marked invalid,
// yet their own Landing URL (doi.org, redirecting to the publisher) was
// advertising a working, unauthenticated PDF via citation_pdf_url the whole
// time. One GET per unique landing URL per pass; fetchCandidates calls this
// twice — once at the top to re-derive any landing_derived candidate an
// earlier pass already inserted and that is still pending/retryable (see
// rederivableLandingSeeds), then again at the fetch-exhaustion boundary for
// candidates that just failed permanently this pass. A hit inserts a
// derived candidate that inherits its parent's source, access basis,
// version, reuse license and identity confidence, so ranking, budgets and
// source policy still apply to it exactly as they did to the parent — this
// is the same candidate reached a second way, never a new resolver source.
func (s *Service) expandLandingSeeds(ctx context.Context, row *job.Row, live map[string]resolver.Candidate, seeds []landingSeed) (bool, error) {
	if len(seeds) == 0 {
		return false, nil
	}
	// Only resolver-produced candidates reappear in live every pass — a
	// landing-derived one never does, because no resolver ever produces it
	// (see rederivableLandingSeeds, which re-adds it from the durable
	// landing_derived event instead). So tried is scoped to this pass only:
	// it guards a citation_pdf_url tag that just points back at a URL
	// already live this pass, resolver-supplied or derived earlier in this
	// same loop. What actually stops this job from re-deriving and
	// re-inserting the same dead link on every future pass forever is
	// InsertCandidates' (job_id, url_key) dedupe in the store, not this map.
	tried := make(map[string]bool, len(live))
	for _, c := range live {
		tried[c.URL] = true
	}
	inserted := false
	for _, seed := range seeds {
		pdfURL, err := s.LandingReader.PDFURLFor(ctx, seed.landingURL)
		if err != nil {
			if ctx.Err() != nil {
				return inserted, ctx.Err()
			}
			// landingmeta.ErrConflictingPDFURL (two disagreeing citation_pdf_url
			// tags) and every other read failure both mean "no usable
			// derivation" for this seed: this is a fallback riding on a
			// candidate that already failed permanently, and must never turn a
			// landing-page read failure into a job-ending error.
			continue
		}
		if pdfURL == "" || tried[pdfURL] {
			continue
		}
		derived := seed.parent
		derived.URL = pdfURL
		derived.Landing = ""
		derived.Direct = true
		derived.ExpectedMIME = "application/pdf"
		derived.RequestHeaders = nil
		persisted, derivedLive := candidateRows(row, []resolver.Candidate{derived},
			[]string{"derived=citation_pdf_url url_key=" + seed.parentKey})
		n, err := s.Jobs.InsertCandidates(ctx, row.ID, persisted)
		if err != nil {
			return inserted, err
		}
		// Merge into live even when the row already existed (n == 0): a
		// re-derivation on a later pass reproduces the identical url_key —
		// same parent source, same rediscovered pdfURL — and that row's
		// whole point in being re-derived is to be found here. Skipping the
		// merge on n == 0 is exactly the bug rederivableLandingSeeds exists
		// to fix: the pending-queue loop below would look this url_key up,
		// find nothing, and mark a perfectly revivable candidate skipped.
		for key, c := range derivedLive {
			live[key] = c
			tried[c.URL] = true
		}
		if n == 0 {
			continue
		}
		if err := s.Jobs.RecordEvent(ctx, row.ID, landingDerivedEventKind, map[string]any{
			"derived": "citation_pdf_url", "parent_url_key": seed.parentKey, "url_key": persisted[0].URLKey,
		}); err != nil {
			return inserted, err
		}
		inserted = true
	}
	return inserted, nil
}

// rederivableLandingSeeds finds every landing_derived candidate an earlier
// pass inserted that is still pending or retryable — never acquired, never
// permanently failed — and rebuilds the seed that produced it. Without this,
// such a row is a ticking bomb: candidateRows only ever populates live from
// resolver output, and no resolver produces a landing-derived URL, so on the
// very next pass live has no entry for its url_key. A single retryable fetch
// failure flips it back to pending (ResetCandidates), the pending-queue loop
// in fetchCandidates finds no live match, and marks it skipped — which
// ResetCandidates never revives. One 503, and the recovery is gone for good.
//
// The fix is to make the candidate re-derivable rather than try to carry it
// in live across passes — a bearer URL is never persisted and never
// reconstructed from storage, and that invariant has to hold here too. The
// landing_derived event durably records the parent's url_key; resolvers are
// deterministic, so the parent — despite being invalid in the store — still
// reappears in this pass's live under that same key, carrying its real
// Landing URL. That is enough to re-read the landing page and reproduce the
// identical derived URL without ever storing it.
//
// The second return value is the set of derived url_keys recognized here as
// landing-derived and currently pending/retryable, whether or not revival
// below actually lands them back in live. The pending-queue loop needs it to
// tell "a bearer URL that legitimately died" from "a derivation this very
// pass failed to re-read" (a flaky landing-page GET), and must retry the
// latter rather than discard it — the same mistake this function exists to
// fix, just one step earlier.
func (s *Service) rederivableLandingSeeds(ctx context.Context, row *job.Row, live map[string]resolver.Candidate) ([]landingSeed, map[string]bool, error) {
	events, err := s.Jobs.Events(ctx, row.ID)
	if err != nil {
		return nil, nil, err
	}
	pendingDerived := make(map[string]bool)
	var seeds []landingSeed
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != landingDerivedEventKind {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		parentKey, _ := detail["parent_url_key"].(string)
		derivedKey, _ := detail["url_key"].(string)
		if parentKey == "" || derivedKey == "" {
			continue
		}
		id, err := s.candidateIDByKey(ctx, row.ID, derivedKey)
		if err != nil {
			continue // row is gone; nothing to revive
		}
		derived, err := s.Jobs.GetCandidate(ctx, id)
		if err != nil || (derived.Status != "pending" && derived.Status != "retryable") {
			continue // already acquired, invalid, or skipped: settled, not our concern
		}
		pendingDerived[derivedKey] = true
		parent, ok := live[parentKey]
		if !ok {
			continue // this pass's resolvers no longer reproduce the parent; unusable
		}
		seeds = append(seeds, landingSeed{landingURL: parent.Landing, parent: parent, parentKey: parentKey})
	}
	return seeds, pendingDerived, nil
}

// siblingHop is the C3 version hop at the fetch-exhaustion boundary: the
// canonical identifier produced candidates, every one failed, and the job is
// about to park or go unavailable. One OpenAlex sibling lookup runs and its
// candidates enter the normal pending queue. InsertCandidates deduplicates by
// url_key, so a repeated hop (later Process re-entry, or hop candidates
// themselves failing) inserts zero and the exhaustion verdict stands — no
// loop is possible.
func (s *Service) siblingHop(ctx context.Context, row *job.Row, live map[string]resolver.Candidate, plan *retryPlan) bool {
	if strings.TrimSpace(row.Work.DOI) == "" {
		return false
	}
	cands, siblingPlan := s.resolveSiblings(ctx, row)
	plan.merge(siblingPlan)
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

// VersionRelations is the optional enricher capability behind the typed
// version hop: Crossref's registrant-asserted preprint/version edges name
// sibling DOIs of the same work, depth one, before any fuzzy search runs.
type VersionRelations interface {
	VersionSiblings(ctx context.Context, doi string) ([]string, error)
}

// resolveSiblings runs the one-shot version hop, typed edges first: a
// registrant-asserted Crossref relation names a sibling DOI outright, so it
// outranks any fuzzy match — the fuzzy adapters run only when no typed edge
// produced a candidate. Sibling candidates carry deliberately different
// identifiers, so the identifier-conflict filter against row.Work is
// skipped: typed edges were asserted by the registrant, strict title/author
// matching happened in the fuzzy adapter, and PDF semantic-identity
// validation against row.Work remains the acceptance gate either way.
// Errors never fail resolution — the hop must not make an acquisition worse.
func (s *Service) resolveSiblings(ctx context.Context, row *job.Row) ([]resolver.Candidate, retryPlan) {
	typed, plan := s.typedSiblings(ctx, row)
	if len(typed) > 0 {
		return typed, plan
	}
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
			return nil, plan
		}
		if s.Budgets != nil {
			if err := s.Budgets.Acquire(ctx, name, entry.Policy, entry.EstimatedCost); err != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
				var deferred *budget.ErrDeferred
				if errors.As(err, &deferred) {
					plan.Gate = earlierTime(plan.Gate, deferred.Until)
					plan.ClosedSourceGates++
				}
				continue
			}
		}
		cands, err := sibling.ResolveSiblings(ctx, row.Work)
		if err != nil {
			// A rate-limited sibling lookup is not a verdict. Recording it as
			// a plain failure let a 429 here settle the whole job unavailable,
			// because the hop runs at the exhaustion boundary where a missing
			// retry time is the difference between parking and giving up.
			if delay, temporary := resolver.Temporary(err); temporary {
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
				plan.TemporaryResolvers++
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, name, entry.Policy, sourceRetry)
				}
				_ = s.Jobs.FinishAttempt(ctx, attempt, "retryable", 0, safeType(err))
				continue
			}
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
			return valid, plan
		}
	}
	return nil, plan
}

// typedSiblings follows Crossref's typed version relations (has-preprint,
// is-preprint-of, has-version, is-version-of) from the job's DOI, then runs
// each sibling DOI through the enabled resolvers, keeping open-access
// candidates only: routing a *different* DOI to an institutional resolver
// would hand the operator the wrong work's sign-in. Budget, retry, and
// attempt bookkeeping mirror the fuzzy loop exactly — a 429 here is a park,
// never an unavailable verdict.
func (s *Service) typedSiblings(ctx context.Context, row *job.Row) ([]resolver.Candidate, retryPlan) {
	var plan retryPlan
	relations, ok := s.Enricher.(VersionRelations)
	if !ok || strings.TrimSpace(row.Work.DOI) == "" {
		return nil, plan
	}
	name := config.SourceCrossrefMetadata
	policy := s.Config.SourcePolicy(name)
	if !policy.Enabled || !row.Policy.SourceAllowed(name) {
		return nil, plan
	}
	attempt, err := s.Jobs.StartAttempt(ctx, row.ID, 0, "resolve", name)
	if err != nil {
		return nil, plan
	}
	if s.Budgets != nil {
		if err := s.Budgets.Acquire(ctx, name, policy, 0); err != nil {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
			var deferred *budget.ErrDeferred
			if errors.As(err, &deferred) {
				plan.Gate = earlierTime(plan.Gate, deferred.Until)
				plan.ClosedSourceGates++
			}
			return nil, plan
		}
	}
	sibs, err := relations.VersionSiblings(ctx, row.Work.DOI)
	if err != nil {
		if delay, temporary := resolver.Temporary(err); temporary {
			sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
			plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
			plan.TemporaryResolvers++
			if s.Budgets != nil {
				_ = s.Budgets.Defer(ctx, name, policy, sourceRetry)
			}
			_ = s.Jobs.FinishAttempt(ctx, attempt, "retryable", 0, safeType(err))
			return nil, plan
		}
		_ = s.Jobs.FinishAttempt(ctx, attempt, "failed", 0, safeType(err))
		return nil, plan
	}
	_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, fmt.Sprintf("version_siblings=%d", len(sibs)))
	if len(sibs) == 0 {
		return nil, plan
	}

	var all []resolver.Candidate
	for _, entry := range s.Resolvers {
		if entry.Adapter == nil {
			continue
		}
		rname := entry.Adapter.Name()
		if !row.Policy.SourceAllowed(rname) || !entry.Policy.Enabled {
			continue
		}
		attempt, err := s.Jobs.StartAttempt(ctx, row.ID, 0, "resolve", rname)
		if err != nil {
			return all, plan
		}
		outcome, detail, valid := "success", "", 0
		for _, sib := range sibs {
			// One Acquire per Resolve: the budget manager reserves one
			// request and one estimated-cost unit atomically, so acquiring
			// once for up to three sibling lookups would under-throttle the
			// provider and under-charge a paid source's monthly cap.
			if s.Budgets != nil {
				if err := s.Budgets.Acquire(ctx, rname, entry.Policy, entry.EstimatedCost); err != nil {
					var deferred *budget.ErrDeferred
					if errors.As(err, &deferred) {
						plan.Gate = earlierTime(plan.Gate, deferred.Until)
						plan.ClosedSourceGates++
					}
					if valid == 0 {
						outcome, detail = "budget_blocked", safeType(err)
					}
					break
				}
			}
			cands, err := entry.Adapter.Resolve(ctx, work.Work{DOI: sib})
			if err != nil {
				if ctx.Err() != nil {
					if valid > 0 {
						_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, fmt.Sprintf("typed_sibling_candidates=%d", valid))
					} else {
						_ = s.Jobs.FinishAttempt(ctx, attempt, "failed", 0, safeType(ctx.Err()))
					}
					return all, plan
				}
				if delay, temporary := resolver.Temporary(err); temporary {
					sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
					plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
					plan.TemporaryResolvers++
					if s.Budgets != nil {
						_ = s.Budgets.Defer(ctx, rname, entry.Policy, sourceRetry)
					}
					outcome, detail = "retryable", safeType(err)
				} else {
					outcome, detail = "failed", safeType(err)
				}
				break
			}
			for _, c := range cands {
				if c.Source == "" {
					c.Source = rname
				}
				// Open access only: a typed sibling is a different DOI, and
				// routing it institutionally would sign the operator into
				// the wrong work's paywall. Conflicts are checked against
				// the sibling identity, never row.Work — differing from the
				// requested DOI is the entire point of the hop.
				if c.Source != rname || c.AccessBasis != resolver.AccessOpen ||
					resolver.ValidateCandidate(c) != nil || conflicts(work.Work{DOI: sib}, c.ResolvedWork) {
					continue
				}
				// Both DOIs already passed work.NormalizeDOI, but evidence
				// writers strip line breaks by convention regardless.
				edge := strings.NewReplacer("\n", " ", "\r", " ").Replace(row.Work.DOI + " -> " + sib)
				c.Evidence = append(c.Evidence, "version_relation crossref "+edge)
				all = append(all, c)
				valid++
			}
		}
		// A source that produced usable candidates helped, whatever a later
		// sibling's lookup did — the audit row must not report it failed
		// while its candidates drive the job toward ready. The plan already
		// carries any temporary failure for retry pacing.
		if valid > 0 || outcome == "success" {
			outcome, detail = "success", fmt.Sprintf("typed_sibling_candidates=%d", valid)
		}
		_ = s.Jobs.FinishAttempt(ctx, attempt, outcome, 0, detail)
	}
	return all, plan
}

func (s *Service) enrichDOIWork(ctx context.Context, row *job.Row) error {
	if s.Discovery == nil || strings.TrimSpace(row.Work.Title) != "" || strings.TrimSpace(row.Work.DOI) == "" {
		return nil
	}
	discovered, err := s.Discovery.LookupWork(ctx, row.Work.DOI)
	if err != nil {
		return nil
	}
	changed, err := s.Jobs.EnrichWorkRequestMetadata(
		ctx, row.WorkRequestID, discovered.Work.Title, discovered.Work.Authors, discovered.Work.Year,
	)
	if err != nil || !changed {
		return err
	}
	if strings.TrimSpace(row.Work.Title) == "" {
		row.Work.Title = discovered.Work.Title
	}
	if len(row.Work.Authors) == 0 {
		row.Work.Authors = append([]string(nil), discovered.Work.Authors...)
	}
	if row.Work.Year == 0 {
		row.Work.Year = discovered.Work.Year
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
			// Enrichment is optional metadata, so both a spent budget and a
			// gated source (daily quota reset) just skip it; neither may fail
			// the job or hold the worker until the gate lifts.
			var exceeded *budget.ErrExceeded
			var deferred *budget.ErrDeferred
			if errors.As(err, &exceeded) || errors.As(err, &deferred) {
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
				_ = s.Budgets.Defer(ctx, name, policy, earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay))
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
	// Enrichment has just given this job a strong identifier, which is the
	// first moment papio can know it duplicates a job it is already running:
	// submit-time dedup correctly matched nothing, because a title-only request
	// has no canonical key. The duplication is recorded and nothing is merged —
	// see RecordDuplicateWork for why. Discarded like the attempt bookkeeping
	// above: this is an advisory note, and failing a correct acquisition
	// because a note could not be written would be the worse trade.
	_, _ = s.Jobs.RecordDuplicateWork(ctx, row.ID, row.Work)
	return nil
}

// oaBrowserHintEventKind durably records — without the bearer URL — that some
// earlier pass identified a browser-eligible OA candidate. Candidate rows and
// events retain only redacted URLs (see the oaBrowserURL comment in
// fetchCandidates), so the event carries only the candidate's url_key; the
// live URL is re-derived from a later pass's own live map, never stored.
const oaBrowserHintEventKind = "job.oa_browser_hint"

// recoverOABrowserHint re-derives the OA URL a previous pass discovered from
// the url_key a job.oa_browser_hint event recorded, resolved against THIS
// pass's live map. Candidates only ever fetch on a pass where
// NextPendingCandidate hands one back; once the one OA candidate is marked
// invalid it never will again, so isOABrowserBlocked is never reconsulted and
// a purely in-memory oaBrowserURL forgot it on every later pass. That sent
// 10.3389/feduc.2018.00095 (open access, expired Azure SAS link) to an
// institutional sign-in it never needed: 38 handoff offers over 3 days, both
// extension drive slots pinned, zero terminal outcomes. A hint whose url_key
// this pass's resolve() no longer produced is unusable and must fall through
// to the ordinary institutional path, not reuse a stale URL.
func (s *Service) recoverOABrowserHint(ctx context.Context, jobID string, live map[string]resolver.Candidate) string {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if kind, _ := events[i]["kind"].(string); kind != oaBrowserHintEventKind {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		urlKey, _ := detail["url_key"].(string)
		if urlKey == "" {
			continue
		}
		if candidate, ok := live[urlKey]; ok && candidate.AccessBasis == resolver.AccessOpen && strings.HasPrefix(candidate.URL, "https://") {
			return candidate.URL
		}
	}
	return ""
}

func (s *Service) fetchCandidates(ctx context.Context, row *job.Row, live map[string]resolver.Candidate, plan retryPlan) error {
	manual := false
	manualRequiresAuth := false
	// Candidate rows and events retain only redacted URLs. oaBrowserURL is
	// seeded from any durable hint a prior pass left (recoverOABrowserHint)
	// so a pass whose pending queue is already empty still knows about a
	// browser-eligible OA candidate found earlier, then kept live only
	// through this pass and recorded in the local handoff action if the job
	// exhausts.
	oaBrowserURL := s.recoverOABrowserHint(ctx, row.ID, live)
	hopTried := false
	landingExpansionTried := false
	landingSeeds := make([]landingSeed, 0, 1)
	seenLanding := map[string]bool{}
	// Re-derive, before the pending queue is ever consulted, any
	// landing-derived candidate an earlier pass inserted that is still
	// pending or retryable (see rederivableLandingSeeds for why this can't
	// wait for the exhaustion boundary below: that row is next out of
	// NextPendingCandidate, not last). seenLanding is pre-marked so a
	// same-pass candidate failure sharing that landing URL never issues a
	// second GET for it.
	pendingDerived := map[string]bool{}
	if s.LandingReader != nil {
		reseeds, derived, err := s.rederivableLandingSeeds(ctx, row, live)
		if err != nil {
			return err
		}
		pendingDerived = derived
		for _, seed := range reseeds {
			seenLanding[seed.landingURL] = true
		}
		if _, err := s.expandLandingSeeds(ctx, row, live, reseeds); err != nil {
			return err
		}
	}
	for {
		stored, err := s.Jobs.NextPendingCandidate(ctx, row.ID)
		if err != nil {
			return err
		}
		if stored == nil {
			// A permanently failed open-access candidate's own landing page may
			// still advertise a working PDF via citation_pdf_url (see
			// expandLandingSeeds); 10.3389/feduc.2018.00095 had that route in
			// hand on both dead candidate rows (landing_redacted = the doi.org
			// resolver link) and still parked 38 times over 3 days because an
			// unrelated resolver gate kept declaring the pass "not exhausted
			// yet". This one-shot check runs before the sibling hop and before
			// any retry-park decision below: a deterministic recovery route
			// already in hand must never sit behind either.
			if !landingExpansionTried {
				landingExpansionTried = true
				if s.LandingReader != nil {
					ok, err := s.expandLandingSeeds(ctx, row, live, landingSeeds)
					if err != nil {
						return err
					}
					if ok {
						continue
					}
				}
			}
			// The pending queue drained. Before any terminal or parking
			// verdict, try the OA sibling hop once — but never pre-empt an
			// ordinary retry wait, where the primary candidates deserve
			// their next attempt first. A pure source gate is not an ordinary
			// retry — no request was made — so it must not suppress a hop onto
			// a sibling source that may not be gated at all.
			if !hopTried {
				hopTried = true
				endsHere := manual || plan.Temporary().IsZero() || s.retryBudgetExhausted(ctx, row.ID)
				if endsHere && s.siblingHop(ctx, row, live, &plan) {
					continue
				}
			}
			break
		}
		candidate, ok := live[stored.URLKey]
		if !ok {
			if pendingDerived[stored.URLKey] {
				// This pending row is a landing-derived candidate the reseed
				// step above recognized but could not revive this pass (the
				// landing-page GET itself failed, or its parent no longer
				// resolves) — a flaky read, not a dead bearer URL. Marking it
				// skipped here would be exactly the bug this whole mechanism
				// exists to fix, one step earlier: retryable lets
				// ResetCandidates hand it back to a later pass instead.
				if err := s.Jobs.MarkCandidate(ctx, stored.ID, "retryable"); err != nil {
					return err
				}
				sourceRetry := earlierRetry(time.Time{}, s.Now(), 0, s.RetryDelay)
				plan.CandidateTemporary = earlierTime(plan.CandidateTemporary, sourceRetry)
				plan.RetryableCandidates++
				continue
			}
			// A prior bearer URL survived only in redacted form; never reconstruct it.
			_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
			continue
		}
		if !candidate.Direct {
			manual = true
			// Whether that manual route needs a sign-in is decided here, where
			// the candidate's access basis is still in hand; the park below has
			// only the flag. An open-access landing page can be fetched by a
			// human with no institution behind them, anything else cannot.
			if candidate.AccessBasis != resolver.AccessOpen {
				manualRequiresAuth = true
			}
			// A non-direct candidate is a landing page the daemon cannot fetch
			// as a file. When it is open access (no institutional login), the
			// extension's provider adapters can still resolve the PDF from the
			// page — so keep the first such URL as the browser-eligible OA URL
			// and route it to the OA browser handoff below instead of a
			// dead-end manual_download. Paywalled landing pages keep the manual
			// route (or the institutional handoff exhaustedCandidates builds).
			if oaBrowserURL == "" && candidate.AccessBasis == resolver.AccessOpen && strings.HasPrefix(candidate.URL, "https://") {
				oaBrowserURL = candidate.URL
			}
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
		// One policy binding for the whole iteration: the Defer on a retryable
		// fetch failure below must name the same quota identity the Acquire
		// here reserved against, or the gate lands on a different row.
		policy := s.Config.SourcePolicy(stored.Source)
		if s.Budgets != nil {
			if err := s.Budgets.Acquire(ctx, stored.Source, policy, stored.CostUSD); err != nil {
				if releaseErr := s.Jobs.ReleaseReservedCost(context.WithoutCancel(ctx), row.ID, stored.Source, stored.CostUSD); releaseErr != nil {
					return releaseErr
				}
				var exceeded *budget.ErrExceeded
				if errors.As(err, &exceeded) {
					_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
					continue
				}
				// A gated source (daily quota reset) is temporary, so the
				// candidate stays retryable and the job parks until the gate
				// lifts — never holding this worker's claim for the window.
				var deferred *budget.ErrDeferred
				if errors.As(err, &deferred) {
					// Unlike the branches around it this one reaches `continue`
					// without a network call in between, so a swallowed failure
					// here leaves the row pending and NextPendingCandidate hands
					// back the same candidate on a tight loop — a hot spin
					// holding the lease instead of parking.
					if err := s.Jobs.MarkCandidate(ctx, stored.ID, "retryable"); err != nil {
						return err
					}
					plan.Gate = earlierTime(plan.Gate, deferred.Until)
					plan.ClosedSourceGates++
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
				// Durable so a later pass whose pending queue is already
				// empty still knows this candidate is browser-recoverable OA
				// (see recoverOABrowserHint). Only url_key is stored — never
				// the URL itself, which is a bearer URL.
				if err := s.Jobs.RecordEvent(ctx, row.ID, oaBrowserHintEventKind, map[string]any{"url_key": stored.URLKey}); err != nil {
					return err
				}
			}
			switch class {
			case fetch.ClassRetryable:
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "retryable")
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				plan.CandidateTemporary = earlierTime(plan.CandidateTemporary, sourceRetry)
				plan.RetryableCandidates++
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, stored.Source, policy, sourceRetry)
				}
			case fetch.ClassBlocked:
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
			default:
				_ = s.Jobs.MarkCandidate(ctx, stored.ID, "invalid")
				// The candidate is dead, but its own landing page (Landing on
				// this live candidate, LandingRedacted on the persisted row) may
				// still advertise a working PDF via citation_pdf_url — worth a
				// GET only for open access, where a login-less recovery route
				// actually helps; anything else already has the institutional
				// path this pass tracks separately. Deduplicated by landing URL
				// since Unpaywall and OpenAlex, per the verified incident, can
				// both return the SAME dead link and therefore the same landing
				// page.
				if candidate.AccessBasis == resolver.AccessOpen && candidate.Landing != "" && !seenLanding[candidate.Landing] {
					seenLanding[candidate.Landing] = true
					landingSeeds = append(landingSeeds, landingSeed{
						landingURL: candidate.Landing, parent: candidate, parentKey: stored.URLKey,
					})
				}
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

	// Classification, not gating. Conservative is documented to "emit
	// institutional or document-delivery actions, but not open them"
	// (docs/concepts/access-modes.md), and emitting this park is exactly that —
	// papio tells the operator what it found and drives no browser. So the park
	// stays in every mode; what was wrong was the hardcoded false, which
	// labelled a paywalled landing page as needing no sign-in and so lied to
	// the one field the access-mode safety check reads.
	if manual && oaBrowserURL == "" {
		if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "manual_download",
			"a resolver returned a landing page but no verified direct PDF",
			job.Access(manualRequiresAuth, "landing_page")); err != nil {
			return err
		}
		return s.park(ctx, row.ID, job.StateFetching, job.StateAwaitingHuman,
			map[string]any{"reason": "landing_page_only"})
	}
	if !plan.IsZero() {
		return s.parkForRetry(ctx, row, job.StateFetching, plan,
			map[string]any{
				"reason":               "acquisition_inputs_temporarily_unavailable",
				"retryable_candidates": plan.RetryableCandidates,
				"temporary_resolvers":  plan.TemporaryResolvers,
				"closed_source_gates":  plan.ClosedSourceGates,
			},
			job.TerminalReasonTemporaryCandidateFailuresDidNotClear, oaBrowserURL)
	}
	return s.exhaustedCandidates(ctx, row, job.StateFetching, "candidates_exhausted", job.TerminalReasonCandidatesExhausted, oaBrowserURL)
}

// parkForRetry schedules the next attempt, or gives up. Two rules keep the
// bounded retry budget honest:
//
// A pass that only met closed source gates made no request, so it is recorded
// as retryKindSourceGate and retryBudgetExhausted does not count it. Otherwise
// a day-long provider gate alongside ordinary thirty-second gates spends all
// eight attempts in minutes and settles the job with a "temporary failures did
// not clear" reason naming a source that was never called.
//
// And when the budget really is spent, a pending gate buys the job exactly ONE
// more wait, not an open-ended one. The rule exists so a source that never had
// its one call still gets it — but a temporary failure also defers its own
// source, so a job failing for real keeps manufacturing the very gate that
// excuses it. Observed live at 41 temporary transitions against a bound of 8,
// re-parking every thirty seconds indefinitely. One wait lets the gated source
// answer; a second means the gate is being refreshed by the failures rather
// than waited out, and the job settles.
func (s *Service) parkForRetry(ctx context.Context, row *job.Row, from string, plan retryPlan, detail map[string]any, exhaustedReason job.TerminalReason, oaBrowserURL string) error {
	now := s.Now().UTC()
	at := plan.At()
	kind := plan.Kind()
	if s.retryBudgetExhausted(ctx, row.ID) {
		if !plan.GatePending(now) || s.alreadyWaitedPastExhaustion(ctx, row.ID) {
			return s.exhaustedCandidates(ctx, row, from, "retry_budget_exhausted", exhaustedReason, oaBrowserURL)
		}
		// Only the gate still justifies waiting. Waking at the shorter
		// temporary time would re-claim, find the budget still spent, and
		// park again — a spin at the temporary interval until the gate opens.
		at, kind = plan.Gate, retryKindExhaustedGate
	}
	if !at.After(now) {
		// The gate elapsed while the rest of the pass ran. Persisting a past
		// time makes the scheduler re-claim instantly and spend another
		// attempt on a wait that already happened.
		at = now.Add(s.RetryDelay)
	}
	detail["retry_kind"] = kind
	return s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, detail, job.WithRetryAt(at))
}

// alreadyWaitedPastExhaustion reports whether this job has already spent its
// one post-exhaustion wait on a pending gate.
func (s *Service) alreadyWaitedPastExhaustion(ctx context.Context, jobID string) bool {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return true // fail closed: prefer settling to looping
	}
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if kind, _ := detail["retry_kind"].(string); kind == retryKindExhaustedGate {
			return true
		}
	}
	return false
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

// retryBudgetExhausted reports whether a job has already spent its bounded
// attempts. It counts durable transition events into retry_wait so the bound
// survives daemon restarts, but skips parks recorded as retryKindSourceGate:
// waiting for a closed source gate consumed no attempt because no request was
// made. Events written before that discriminator existed carry no retry_kind
// and are counted, which preserves the original bound for existing jobs. A
// read error never escalates: best-effort maintenance prefers another retry to
// falsely giving up on a job.
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
		if to, _ := detail["to"].(string); to != job.StateRetryWait {
			continue
		}
		if kind, _ := detail["retry_kind"].(string); kind == retryKindSourceGate {
			continue
		}
		n++
	}
	return n >= maxRetryAttempts
}

// institutionalRouteExhausted reports whether an institutional OpenURL route
// has already conclusively reported no entitlement for this job. The event is
// durable so a rediscovery pass after a daemon restart cannot offer that route
// again. A read error is deliberately non-escalating, like retryBudgetExhausted:
// another institutional offer is safer than falsely hiding available access.
func (s *Service) institutionalRouteExhausted(ctx context.Context, jobID string) bool {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return false
	}
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind == "browser.no_entitlement_requeue" {
			return true
		}
	}
	return false
}

// handoffGate reports whether this work carries an identifier a human handoff
// could actually act on, and — when it does not — the durable classification
// that says why.
//
// The syntactic half is HasFetchableIdentifier: no DOI/PMID/arXiv/OpenAlex id
// means no route a login can open. The second half exists because a DOI that
// merely *parses* is not a DOI that *exists*. A mistyped one survives every
// upstream check — Crossref, OpenAlex, EuropePMC and Unpaywall all report "no
// record" and "no open copy" as the same empty result — and then reaches the
// link resolver, which has nothing to match and bounces the user to doi.org's
// "DOI NOT FOUND" page. The handoff can never be completed, so it re-offers on
// every session-live tick and re-notifies on the reminder schedule forever.
//
// The registry is consulted only when a DOI is the sole fetchable identifier
// (a PMID, arXiv id or OpenAlex id is its own route) and only where a handoff
// is actually about to be created or repaired. A probe failure fails open,
// like institutionalRouteExhausted: during a registry outage another handoff
// is far cheaper than terminating a job that was perfectly fetchable. The
// registry client memoizes, so the once-a-minute repair sweep over every
// parked job does not become a request per job per tick.
func (s *Service) handoffGate(ctx context.Context, w work.Work) (ok bool, reason string, terminal job.TerminalReason) {
	if !w.HasFetchableIdentifier() {
		return false, "no_identifier", job.TerminalReasonNoIdentifier
	}
	if s.DOIRegistry == nil || w.DOI == "" || w.PMID != "" || w.ArXiv != "" || w.OpenAlex != "" {
		return true, "", ""
	}
	registered, err := s.DOIRegistry.Registered(ctx, w.DOI)
	if err != nil || registered {
		return true, "", ""
	}
	return false, "doi_not_registered", job.TerminalReasonDOINotRegistered
}

// exhaustedCandidates handles the terminal "no direct candidate" boundary —
// either resolving produced zero legal candidates or fetching exhausted them
// all without an artifact. A bot-blocked open-access candidate gets one
// browser-native attempt before the identifier gate because its PDF can still
// be fetched without a DOI; an authentication wall on that offer is settled by
// the bridge as no_identifier rather than becoming an institutional handoff.
// The action detail carries the live OA URL solely for the browser bridge; it
// is never copied into job events or protocol metadata.
func (s *Service) exhaustedCandidates(ctx context.Context, row *job.Row, from, reason string, terminal job.TerminalReason, oaBrowserURL string) error {
	mode := s.Config.EffectiveAccessMode(row.Policy.AccessMode)
	switch mode {
	case config.ModeAssisted, config.ModeDelegated:
		if oaBrowserURL != "" {
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", OABrowserHandoffActionDetail(oaBrowserURL), job.Access(false, "anti_bot")); err != nil {
				return err
			}
			return s.park(ctx, row.ID, from, job.StateAwaitingHuman,
				map[string]any{"reason": "open_access_browser_handoff"})
		}
		institutionalExhausted := s.institutionalRouteExhausted(ctx, row.ID)
		base, hasBase := s.Config.OpenURLBaseFor(row.Policy.Resolver)
		routeable, gateReason, gateTerminal := s.handoffGate(ctx, row.Work)
		switch {
		// A work with no identifier a login could act on must never be routed
		// to an institutional sign-in. The resolver would be handed a bare
		// title or an unregistered DOI, and the destination for a printed
		// monograph, a report, or a typo is a catalogue record or an error
		// page — no login produces a PDF, so a handoff here spends the user's
		// SSO round trip, parks forever, and (since human actions are now
		// re-notified on a schedule) nags them about impossible work.
		case !routeable:
			reason, terminal = gateReason, gateTerminal
		case hasBase && base != "" && !institutionalExhausted:
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail, job.Access(true, "paywall")); err != nil {
				return err
			}
			return s.park(ctx, row.ID, from, job.StateAwaitingHuman,
				map[string]any{"reason": "institutional_handoff"})
		default:
			// institutionalExhausted, or no institutional OpenURL route was
			// ever configured for this profile: the plain-OpenURL handoff has
			// nothing further to offer this pass. ADR-0017 Decision 4
			// supersedes the no_entitlement collapse that follows for the
			// case where a document-delivery route was actually configured
			// and pursued (Consequences: "superseded ... for the case where
			// a delivery route was actually configured and pursued; the
			// terminal reason remains correct for the case where no route
			// exists at all"). deliveryRoute is itself a no-op — Configured
			// stays false — whenever s.Delivery is nil or this profile has no
			// document_delivery block, which keeps both terminal/reason
			// assignments below byte-for-bit identical to pre-ADR-0017
			// behavior.
			if institutionalExhausted {
				terminal = job.TerminalReasonNoEntitlement
			}
			result, err := s.deliveryRoute(ctx, row, from)
			if err != nil {
				return err
			}
			if result.Configured {
				return nil
			}
		}
	case config.ModeConservative:
		// Same gate: an OpenURL built from a bare title or an unregistered DOI
		// is not worth surfacing.
		routeable, gateReason, gateTerminal := s.handoffGate(ctx, row.Work)
		if base, ok := s.Config.OpenURLBaseFor(row.Policy.Resolver); ok && base != "" && routeable {
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_available",
				"no direct candidates; institutional OpenURL available but not opened in conservative mode",
				// An advisory, not a sign-in prompt: it exists precisely to say
				// that institutional access was NOT opened.
				job.Access(false, "")); err != nil {
				return err
			}
		}
		// ADR-0017 Decision 3B condition 1: "under conservative the route is
		// discovered and recorded, never opened or submitted." Unlike the
		// delegated/assisted path, this never runs Branch/EvaluateGate, and
		// deliberately records an EVENT rather than a job.ActionKindDocumentDelivery
		// human action: that kind is reserved for the delegated/assisted
		// path's actionable prefill/reconciliation offers, and — unlike
		// openurl_available — has no exemption from the terminal
		// transition's open-action cleanup a few lines down, which would
		// silently cancel it the instant it opened. An event survives that
		// cleanup untouched and is exactly "recorded": visible in the job's
		// history without masquerading as something actionable.
		if _, dd, ok := s.deliveryConfigured(row); ok && routeable {
			if err := s.Jobs.RecordEvent(ctx, row.ID, "delivery.route_discovered", map[string]any{
				"provider": dd.Kind,
				"reason":   "not opened in conservative mode",
			}); err != nil {
				return err
			}
		}
		if !routeable {
			reason, terminal = gateReason, gateTerminal
		}
	}
	return s.Jobs.Transition(ctx, row.ID, from, job.StateUnavailable,
		map[string]any{"reason": reason}, job.WithTerminalReason(terminal))
}

// DeliveryRouteResult reports what one document-delivery route evaluation
// did for a job (ADR-0017 Decision 3B/4). exhaustedCandidates's automatic
// call only consults Configured; SubmitDelivery returns the rest to its RPC
// caller.
type DeliveryRouteResult struct {
	// Configured is false when the job's institution profile has no
	// document_delivery configured or s.Delivery is nil — the ADR-0017
	// route is off for this job, nothing was evaluated, and the job was
	// left untouched. It is the only field callers need to decide whether
	// to fall back to their own pre-ADR-0017 handling.
	Configured bool
	Branch     delivery.BranchDecision
	Decision   delivery.Decision
	Request    *delivery.Request
}

// SubmitDelivery runs the same Decision 3B/4 idempotency-branch-then-gate
// path exhaustedCandidates takes automatically when ordinary candidates are
// exhausted, for an explicit operator/RPC-triggered call (`papio delivery
// submit <job-id>`). It transitions the job exactly as the automatic call
// does: submit -> retry_wait pending on success, or retry_wait/
// resolver_temporarily_unavailable on a transport failure (never
// awaiting_human for an ordinary outage — see submitToProvider),
// prefill/enrich_then_prefill -> awaiting_human with the document_delivery
// action, join_poll -> retry_wait pending on the existing row,
// adopt_fulfilled/reconcile/resubmission_policy -> awaiting_human with the
// document_delivery action. Like any other Transition call, it fails with
// job.ErrConflict if jobID's current state cannot reach the resulting state
// (e.g. a job already mid-poll for the same live request) — callers should
// check delivery.get's row state first.
func (s *Service) SubmitDelivery(ctx context.Context, jobID string) (DeliveryRouteResult, error) {
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return DeliveryRouteResult{}, err
	}
	return s.deliveryRoute(ctx, row, row.State)
}

// CancelJob cancels a job exactly as job.Store.Cancel does, additionally
// reconciling any live (submitted/pending) delivery_requests row it was
// driving (ADR-0017 Decision 4): cancelling the job stops papio from ever
// polling that row again, so a live row must not be left looking like it is
// still being watched. See delivery.Service.OrphanIfLive. Compensation is
// best-effort — the cancellation itself, which the caller actually asked
// for, is what this must never fail on.
func (s *Service) CancelJob(ctx context.Context, jobID string, reason job.TerminalReason) error {
	if err := s.Jobs.Cancel(ctx, jobID, reason); err != nil {
		return err
	}
	if s.Delivery != nil {
		_ = s.Delivery.OrphanIfLive(ctx, jobID, "job_cancelled")
	}
	return nil
}

// DismissAction dismisses a human action exactly as
// job.Store.DismissHumanAction does, additionally reconciling any live
// delivery_requests row a resulting job cancellation was driving — see
// CancelJob. Dismissing a non-document_delivery action, or one whose job
// isn't cancelled by the dismissal, is untouched: OrphanIfLive only ever
// acts on a row actually in state submitted/pending.
func (s *Service) DismissAction(ctx context.Context, actionID, expectedRevision int64) (string, error) {
	jobID, err := s.Jobs.DismissHumanAction(ctx, actionID, expectedRevision)
	if err != nil {
		return "", err
	}
	if s.Delivery != nil {
		_ = s.Delivery.OrphanIfLive(ctx, jobID, "action_dismissed")
	}
	return jobID, nil
}

// deliveryConfigured reports whether row's institution profile has a
// document_delivery route configured and s.Delivery is wired — the single
// nil-safe gate every ADR-0017 code path in this file checks first.
//
// It deliberately ignores InstitutionFor's own boolean result: for the
// default profile that result tracks OpenURLBase presence alone (see
// InstitutionFor), which would wrongly report "not configured" for a
// default profile that sets document_delivery without ever setting
// openurl_base — a document-delivery-only institution profile is exactly
// what ADR-0017 Decision 2 makes possible. inst.DocumentDelivery == nil is
// already the correct, self-sufficient signal: InstitutionFor never
// populates it on a resolver name it does not recognize either.
func (s *Service) deliveryConfigured(row *job.Row) (config.Institution, *config.DocumentDelivery, bool) {
	if s.Delivery == nil {
		return config.Institution{}, nil, false
	}
	inst, _ := s.Config.InstitutionFor(row.Policy.Resolver)
	if inst.DocumentDelivery == nil {
		return config.Institution{}, nil, false
	}
	return inst, inst.DocumentDelivery, true
}

// deliveryProfileName normalizes row.Policy.Resolver the same way
// config.Config.InstitutionFor does, so the idempotency key and gate profile
// this file computes always key on the same profile name InstitutionFor
// actually resolved.
func deliveryProfileName(resolver string) string {
	if resolver == "" {
		return "default"
	}
	return resolver
}

// deliveryRequestClass returns v1's only supported request class,
// digital_journal_article, when the work carries a strong article
// identifier — DOI or PMID. work.Work has no work-type/container
// discriminator anywhere in this pipeline (enrichDOIWork/enrich only ever
// copy title/authors/year out of a discovery lookup), so DOI-or-PMID is the
// entire "article-shaped" test v1 can make — the same signal
// HasFetchableIdentifier and the ISBN-exclusion precedent already treat as
// papio's article-vs-monograph distinction (see work.Work.HasFetchableIdentifier:
// an ISBN routes to a catalogue record, never a PDF). PMID names only
// journal articles; a DOI with neither is everything else (books, chapters,
// datasets, theses) and correctly returns "", which EvaluateGate's condition
// 3 (SupportedRequestClasses) always fails closed to prefill on.
func deliveryRequestClass(w work.Work) string {
	if w.DOI != "" || w.PMID != "" {
		return "digital_journal_article"
	}
	return ""
}

// deliveryHasRequiredFields reports whether row's Work meets the illiad
// citation minimum for a Borrowing/Article transaction: a DOI or PMID plus
// a title (Decision 3B condition 4).
func deliveryHasRequiredFields(w work.Work) bool {
	return (w.DOI != "" || w.PMID != "") && w.Title != ""
}

// deliveryRoute implements ADR-0017 Decisions 3B and 4: the idempotency
// branch, then — for a fresh or merely-offered key — the seven-point gate,
// then acts on the verdict. Configured stays false, with row left entirely
// untouched, whenever s.Delivery is nil or row's institution profile has no
// document_delivery block; every other code path in this file relies on
// that to stay a no-op.
func (s *Service) deliveryRoute(ctx context.Context, row *job.Row, from string) (DeliveryRouteResult, error) {
	inst, dd, ok := s.deliveryConfigured(row)
	if !ok {
		return DeliveryRouteResult{}, nil
	}
	profileName := deliveryProfileName(row.Policy.Resolver)
	requestClass := deliveryRequestClass(row.Work)
	workIdentity := row.Work.Describe()
	key := delivery.IdempotencyKey(profileName, workIdentity, dd.Kind, requestClass)

	branch, existing, err := s.Delivery.Branch(ctx, key)
	if err != nil {
		return DeliveryRouteResult{}, err
	}
	switch branch {
	case delivery.BranchJoinPoll:
		return DeliveryRouteResult{Configured: true, Branch: branch, Request: existing},
			s.joinDeliveryPoll(ctx, row, from, existing)
	case delivery.BranchAdoptFulfilled, delivery.BranchReconcile, delivery.BranchResubmissionPolicy:
		// v1: route every non-live-pending outcome to the document_delivery
		// reconciliation action rather than building fetch/adopt/resubmission
		// policy here — Decision 4: "the CLI (papio actions list/papio jobs
		// get) is the only faithful surface" until reconciliation ships.
		return DeliveryRouteResult{Configured: true, Branch: branch, Request: existing},
			s.openDeliveryReconciliationAction(ctx, row, from, existing)
	case delivery.BranchEvaluateGate:
		profile, err := s.Delivery.ResolveGateProfile(ctx, profileName, inst)
		if err != nil {
			return DeliveryRouteResult{Configured: true, Branch: branch}, err
		}
		submittedThisMonth, err := s.Delivery.SubmittedThisMonth(ctx, profileName, dd.Kind)
		if err != nil {
			return DeliveryRouteResult{Configured: true, Branch: branch}, err
		}
		decision := delivery.EvaluateGate(profile, delivery.GateRequest{
			EffectiveAccessMode: s.Config.EffectiveAccessMode(row.Policy.AccessMode),
			RequestClass:        requestClass,
			HasRequiredFields:   deliveryHasRequiredFields(row.Work),
			SubmittedThisMonth:  submittedThisMonth,
		})
		if err := s.Delivery.AppendGateEvent(ctx, row.ID, delivery.GateEvaluated{
			ProfileClass:  profile.Class,
			ProfileDigest: profile.Digest(),
			Decision:      decision,
		}); err != nil {
			return DeliveryRouteResult{Configured: true, Branch: branch, Decision: decision}, err
		}
		var (
			request *delivery.Request
			actErr  error
		)
		if decision.Action == delivery.ActionSubmit {
			request, actErr = s.submitDeliveryRequest(ctx, row, from, profileName, dd, requestClass, workIdentity, key, profile)
		} else {
			// ActionPrefill and ActionEnrichThenPrefill both open the same
			// prefill action: v1 has no separate enrichment step here — the
			// job already ran normal metadata enrichment before ever
			// reaching candidate exhaustion — so "enrich, then prefill if
			// still incomplete" (Decision 3B) resolves to prefill directly.
			request, actErr = s.openDeliveryPrefillAction(ctx, row, from, profileName, dd, requestClass, workIdentity, profile)
		}
		return DeliveryRouteResult{Configured: true, Branch: branch, Decision: decision, Request: request}, actErr
	default:
		return DeliveryRouteResult{Configured: true, Branch: branch, Request: existing},
			fmt.Errorf("delivery: unrecognized branch decision %q", branch)
	}
}

// illiadIdempotencyReferenceField is the ILLiad transaction field papio's
// idempotency key is written to (illiad.TransactionRequest.ReferenceField).
// v1 config has no per-institution mapping for this; ItemInfo4 is one of
// ILLiad's five general-purpose fields and carries no meaning ILLiad itself
// assigns, so every institution can read papio's token back from it via
// illiad.Transaction.ReferenceValue.
const illiadIdempotencyReferenceField = "ItemInfo4"

// illiadHTTPClient returns the transport used to construct every institution
// profile's illiad.Client, defaulting to http.DefaultClient the same way
// New's zero-value Service would if a caller built one by hand.
func (s *Service) illiadHTTPClient() illiad.HTTPClient {
	if s.IlliadHTTPClient != nil {
		return s.IlliadHTTPClient
	}
	return http.DefaultClient
}

// illiadTransactionRequest builds the citation and routing payload for a new
// ILLiad Borrowing/Article transaction, carrying papio's idempotency key in
// the fixed reference field (Decision 1/3A).
func illiadTransactionRequest(w work.Work, patronRef, idempotencyKey string) illiad.TransactionRequest {
	req := illiad.TransactionRequest{
		ExternalUserID:     patronRef,
		ProcessType:        "Borrowing",
		RequestType:        "Article",
		PhotoJournalTitle:  w.Container,
		PhotoArticleTitle:  w.Title,
		PhotoArticleAuthor: strings.Join(w.Authors, "; "),
		DOI:                w.DOI,
		PMID:               w.PMID,
		ReferenceField:     illiadIdempotencyReferenceField,
		ReferenceValue:     idempotencyKey,
	}
	if w.Year > 0 {
		req.PhotoJournalYear = strconv.Itoa(w.Year)
	}
	return req
}

// submitDeliveryRequest is Decision 3B's `submit` verdict: it durably
// occupies the idempotency key first (Create, state offered), then calls
// internal/illiad only for a profile compiled auto_capable. Creating the row
// before the transport call means a crash or transport failure between the
// two never leaves a live ILLiad transaction with no local record.
//
// Create's ErrDuplicateRequest means an idempotency key already has a row —
// but "already has a row" is not "already resubmitted". An existing row
// still in state offered with no provider_reference never reached the
// provider (Branch already treats offered exactly like a fresh key for the
// same reason, see its default case): retrying the illiad call against THAT
// row is the retry a prior transport failure earned, not a second live
// request. Only a row that actually reached the provider (any other state)
// makes this a genuine duplicate — Decision 1 forbids a second attempt
// there, so it routes to reconciliation instead.
func (s *Service) submitDeliveryRequest(ctx context.Context, row *job.Row, from, profileName string, dd *config.DocumentDelivery, requestClass, workIdentity, key string, profile delivery.GateProfile) (*delivery.Request, error) {
	created, err := s.Delivery.Create(ctx, delivery.CreateRequest{
		JobID:              row.ID,
		InstitutionProfile: profileName,
		Provider:           dd.Kind,
		RequestClass:       requestClass,
		WorkIdentity:       workIdentity,
		GateProfileDigest:  profile.Digest(),
	})
	if err != nil {
		if !errors.Is(err, delivery.ErrDuplicateRequest) {
			return nil, err
		}
		if created == nil || created.State != delivery.StateOffered || created.ProviderReference != "" {
			// A live or otherwise-resolved request already occupies this
			// idempotency key (a concurrent evaluation, or a settled
			// outcome). Never open a second live request (Decision 1) —
			// route to reconciliation so a human resolves whatever it is,
			// rather than guessing at its state from here.
			return created, s.openDeliveryReconciliationAction(ctx, row, from, created)
		}
		// Fall through: nothing was ever lodged for this row: retry
		// submission against it instead of reconciling a request that was
		// never actually created.
	}
	return s.submitToProvider(ctx, row, from, dd, key, profile, created)
}

// submitToProvider calls internal/illiad for a durably-offered row that has
// never reached the provider — a fresh Create, or one submitDeliveryRequest
// recovered after an earlier pass's transport failure. On success it
// advances the row to submitted and parks the job
// retry_wait/document_delivery_pending on the live-poll cadence.
//
// On transport failure it parks retry_wait/resolver_temporarily_unavailable
// on the ordinary short retry cadence instead of
// parkDeliveryPrefill's document_delivery human action: the row already
// durably occupies the idempotency key (Decision 1), so nothing was lost,
// and a transient ILLiad outage is exactly the class of condition
// resolver_temporarily_unavailable already tells the operator to expect —
// "papio will retry automatically" (see errcat.Explain) — so escalating it
// to a human action would be an overreaction. Reusing
// RetryReasonDocumentDeliveryPending instead would misreport a request that
// never actually reached the provider as one papio is now polling.
func (s *Service) submitToProvider(ctx context.Context, row *job.Row, from string, dd *config.DocumentDelivery, key string, profile delivery.GateProfile, created *delivery.Request) (*delivery.Request, error) {
	client := illiad.New(s.illiadHTTPClient(), dd.BaseURL, dd.APIKey)
	txn, err := client.CreateTransaction(ctx, illiadTransactionRequest(row.Work, dd.PatronRef, key))
	if err != nil {
		return created, s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, map[string]any{
			"reason":              "resolver_temporarily_unavailable",
			"delivery_request_id": created.ID,
		}, job.WithRetryAt(s.Now().Add(s.RetryDelay)))
	}
	if err := s.Delivery.UpdateState(ctx, created.ID, delivery.StateSubmitted); err != nil {
		return created, err
	}
	nextCheck := delivery.NextCheck(s.Now(), 0, profile.StatusPollMinutes)
	providerRef := strconv.Itoa(txn.TransactionNumber)
	if err := s.Delivery.RecordPoll(ctx, created.ID, providerRef, nextCheck); err != nil {
		return created, err
	}
	// UpdateState/RecordPoll only mutated the database; created is still the
	// pre-submission snapshot Create returned (state offered, no reference).
	// Re-fetch so callers — SubmitDelivery's RPC/CLI caller in particular —
	// see the row's real, post-submission state rather than a stale one.
	submitted, err := s.Delivery.Get(ctx, created.ID)
	if err != nil {
		return created, err
	}
	// Plain job.Store.Transition, not s.park: a delivery poll is not a human
	// action, exactly like parkForRetry's ordinary retry_wait never notifies
	// through s.park either.
	return submitted, s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, map[string]any{
		"reason":              string(job.RetryReasonDocumentDeliveryPending),
		"delivery_request_id": created.ID,
		"provider_reference":  providerRef,
	}, job.WithRetryAt(nextCheck))
}

// openDeliveryPrefillAction is Decision 3B's `prefill`/`enrich_then_prefill`
// verdict: it ensures a durable offered row occupies this idempotency key
// (Decision 1: "the row a compiled-prefill route occupies before any live
// submission exists"), then opens the document_delivery action and parks
// awaiting_human.
func (s *Service) openDeliveryPrefillAction(ctx context.Context, row *job.Row, from, profileName string, dd *config.DocumentDelivery, requestClass, workIdentity string, profile delivery.GateProfile) (*delivery.Request, error) {
	created, err := s.Delivery.Create(ctx, delivery.CreateRequest{
		JobID:              row.ID,
		InstitutionProfile: profileName,
		Provider:           dd.Kind,
		RequestClass:       requestClass,
		WorkIdentity:       workIdentity,
		GateProfileDigest:  profile.Digest(),
	})
	if err != nil && !errors.Is(err, delivery.ErrDuplicateRequest) {
		return nil, err
	}
	return created, s.parkDeliveryPrefill(ctx, row, from, dd)
}

// parkDeliveryPrefill opens the document_delivery prefill action and parks
// the job awaiting_human. Split out of openDeliveryPrefillAction so
// submitDeliveryRequest can fall back to it after an already-created row's
// illiad transport call fails, without a second Create attempt.
func (s *Service) parkDeliveryPrefill(ctx context.Context, row *job.Row, from string, dd *config.DocumentDelivery) error {
	if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, job.ActionKindDocumentDelivery,
		DeliveryPrefillActionDetail(dd.BaseURL), job.Access(false, "")); err != nil {
		return err
	}
	return s.park(ctx, row.ID, from, job.StateAwaitingHuman, map[string]any{"reason": "document_delivery_prefill"})
}

// joinDeliveryPoll is Decision 3B's `join_poll` branch: this job attaches to
// an already-submitted/pending request rather than evaluating the gate
// again, and parks on that same pending-delivery retry_wait reason. The
// existing row's own job_id (whichever job first created it) is untouched —
// this only records the join on THIS job's event stream and retry schedule.
func (s *Service) joinDeliveryPoll(ctx context.Context, row *job.Row, from string, existing *delivery.Request) error {
	return s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, map[string]any{
		"reason":              string(job.RetryReasonDocumentDeliveryPending),
		"delivery_request_id": existing.ID,
	}, job.WithRetryAt(deliveryJoinPollAt(s.Now(), existing)))
}

// deliveryJoinPollAt reuses an already-scheduled next_check_at when it is
// still in the future, and only falls back to a fresh default-cadence
// NextCheck when the existing row carries none (or an already-past one).
func deliveryJoinPollAt(now time.Time, existing *delivery.Request) time.Time {
	if existing != nil && existing.NextCheckAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, existing.NextCheckAt); err == nil && t.After(now) {
			return t
		}
	}
	return delivery.NextCheck(now, 0, 0)
}

// openDeliveryReconciliationAction is Decision 4's exhausted-reconciliation
// path: fulfilled, unknown_outcome, declined, and cancelled rows all open
// the same document_delivery action and park awaiting_human. It never
// offers retry_submission — Decision 4: "papio must not submit a second
// request while an earlier one's outcome is unknown."
func (s *Service) openDeliveryReconciliationAction(ctx context.Context, row *job.Row, from string, existing *delivery.Request) error {
	if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, job.ActionKindDocumentDelivery,
		DeliveryReconciliationActionDetail(existing), job.Access(false, "")); err != nil {
		return err
	}
	return s.park(ctx, row.ID, from, job.StateAwaitingHuman, map[string]any{"reason": "document_delivery_reconciliation"})
}

// DeliveryPrefillActionDetail describes the document-delivery/ILL request
// form opened for a configured, but not auto-submitted, delivery route
// (ADR-0017 Decision 3B: prefill_only or enrich_then_prefill).
func DeliveryPrefillActionDetail(baseURL string) string {
	if baseURL == "" {
		return "document delivery / ILL request available; the institution's request form has no configured base_url — run 'papio delivery get' for the compiled gate profile that explains why"
	}
	return "document delivery / ILL request available; open the institution's request form and submit it yourself:\n" + baseURL
}

// DeliveryReconciliationActionDetail describes the human action Decision 4
// opens only once deterministic reconciliation is exhausted, or a request
// landed declined/cancelled — never an offer to resubmit. It names only the
// one CLI surface this package's own contract guarantees (`papio delivery
// get <job-id>`, ADR-0017 Decision 1); the specific reconciliation
// operations (open_request_history/confirm_request_exists/confirm_request_absent,
// Decision 4) are the CLI's own naming, not restated here.
func DeliveryReconciliationActionDetail(existing *delivery.Request) string {
	ref := existing.ProviderReference
	if ref == "" {
		ref = "(no provider reference recorded)"
	}
	return fmt.Sprintf("a document-delivery request (provider %s, reference %s, state %s) needs reconciliation; run 'papio delivery get %s' for its history and resolve it by hand — papio never resubmits automatically",
		existing.Provider, ref, existing.State, existing.JobID)
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
		// A partial report is the most useful thing anyone will ever have about
		// a validation that could not finish: it names the stage that stopped.
		s.recordValidation(ctx, row.ID, stored.ID, result.SHA256, validationIncomplete, report)
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, safeType(validateErr))
		_, _ = s.Jobs.OpenHumanAction(ctx, row.ID, "validation_error", "PDF validation could not complete within configured bounds", job.Access(false, ""))
		if err := s.Jobs.MarkCandidate(ctx, stored.ID, "skipped"); err != nil {
			return false, false, err
		}
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "validation_error"})
	}
	active := report.Structural.HasJavaScript || report.Structural.HasEmbeddedFiles
	needsIdentityReview := report.Text.NeedsReview || report.Identity.Result == pdf.IdentityReview
	// Recorded before the branch, not inside each arm: the verdict is a function
	// of the report alone, so one call site cannot drift from the decision below,
	// and evidence survives even for the candidates papio throws away — which is
	// exactly the set a consumer asks "why not this one?" about.
	s.recordValidation(ctx, row.ID, stored.ID, result.SHA256,
		validationVerdict(report, active, needsIdentityReview), report)
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
		_, _ = s.Jobs.OpenHumanAction(ctx, row.ID, "unsafe_pdf", "PDF is encrypted or contains active/embedded content", job.Access(false, ""))
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "encrypted_or_active_content"})
	case needsIdentityReview && !stored.ReviewOverride:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, "semantic_or_identity_review")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
		if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "verify_identity",
			fmt.Sprintf("PDF text or identity requires human verification; local quarantine file: %s", result.TempPath),
			job.Access(false, ""),
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
		"PAPIO_PMID":       row.Work.PMID,
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

// retryKind labels why a pass ended without a verdict. It is recorded on the
// retry_wait transition so retryBudgetExhausted can tell the two apart.
const (
	retryKindTemporary  = "temporary"
	retryKindSourceGate = "source_gate"
	// retryKindExhaustedGate marks the single wait a job is allowed after its
	// retry budget is spent, so a second one can be refused. Not counted by
	// retryBudgetExhausted: the budget is already spent by definition here.
	retryKindExhaustedGate = "exhausted_gate"
)

// retryPlan separates the reasons an acquisition pass can end with no
// verdict. CandidateTemporary and ResolverTemporary both mean a request went
// out and failed, so retrying costs the job one of its bounded attempts —
// kept apart only so a park can report what it actually saw instead of
// asserting a cause it never observed. 10.3389/feduc.2018.00095 parked 11
// times as "candidate_temporarily_unavailable" while both candidates had
// failed permanently (403) and an unrelated resolver was the thing actually
// temporary; nobody could tell what was really holding the job. Gate means a
// source was closed before any request was made (budget.ErrDeferred), so the
// job learned nothing and must not be charged for waiting: a day-long
// provider gate alongside ordinary thirty-second gates would otherwise burn
// the whole retry budget within minutes and settle the job "temporary source
// failures did not clear" — a claim about a source that was never called.
type retryPlan struct {
	CandidateTemporary time.Time // a candidate fetch failed retryably
	ResolverTemporary  time.Time // a resolver/sibling source failed retryably
	Gate               time.Time // a source gate was closed; no request was made

	RetryableCandidates int // candidate fetches that failed retryably this pass
	TemporaryResolvers  int // resolver/sibling calls that failed retryably this pass
	ClosedSourceGates   int // source gates closed before any request this pass
}

// Temporary is the earliest retryable-request observation, candidate or
// resolver side. Scheduling has never distinguished the two — only the park
// detail does — so At, IsZero and Kind keep using it.
func (p retryPlan) Temporary() time.Time {
	return earlierTime(p.CandidateTemporary, p.ResolverTemporary)
}

// merge folds another pass's plan in, keeping the earliest of each kind and
// summing what each pass actually observed.
func (p *retryPlan) merge(other retryPlan) {
	p.CandidateTemporary = earlierTime(p.CandidateTemporary, other.CandidateTemporary)
	p.ResolverTemporary = earlierTime(p.ResolverTemporary, other.ResolverTemporary)
	p.Gate = earlierTime(p.Gate, other.Gate)
	p.RetryableCandidates += other.RetryableCandidates
	p.TemporaryResolvers += other.TemporaryResolvers
	p.ClosedSourceGates += other.ClosedSourceGates
}

// At is when the job should wake: the earliest opportunity of either kind,
// because a source that frees up sooner deserves its attempt sooner.
func (p retryPlan) At() time.Time { return earlierTime(p.Temporary(), p.Gate) }

func (p retryPlan) IsZero() bool { return p.At().IsZero() }

// Kind is source_gate only when no request was actually attempted this pass.
func (p retryPlan) Kind() string {
	if p.Temporary().IsZero() {
		return retryKindSourceGate
	}
	return retryKindTemporary
}

// GatePending reports a source gate that has not yet elapsed. At the retry
// exhaustion boundary this outranks a terminal verdict: the bounded attempts
// are spent, but the gated source still deserves the one call it never got.
func (p retryPlan) GatePending(now time.Time) bool {
	return !p.Gate.IsZero() && p.Gate.After(now)
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
