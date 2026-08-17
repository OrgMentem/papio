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
	"log"
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
	"papio/internal/notify"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
	"papio/internal/zotio"
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

// MetadataEnricherEntry binds one metadata source to its policy name. Entries
// are attempted in order, allowing Crossref to remain first while OpenAlex
// rescues title-only works that Crossref does not index.
type MetadataEnricherEntry struct {
	Name     string
	Enricher MetadataEnricher
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

// Notification delivery is best effort and never changes domain state.

// ResolverEntry binds an adapter to its policy and estimated metadata-call cost.
type ResolverEntry struct {
	Adapter       resolver.Resolver
	Policy        config.Source
	EstimatedCost float64
}

// Service is the command-independent acquisition service.
type Service struct {
	Config            config.Config
	Jobs              *job.Store
	Artifacts         *artifact.Store
	Budgets           *budget.Manager
	Resolvers         []ResolverEntry
	Enricher          MetadataEnricher
	MetadataEnrichers []MetadataEnricherEntry
	Discovery         WorkLookup
	DOIRegistry       DOIRegistry
	LandingReader     LandingReader
	Fetch             FetchFunc
	Validate          ValidateFunc
	AutoImporter      AutoImporter
	Notifier          notify.Sink
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
	// Attribute every credit this pass spends to this job. Set once here, at
	// the top of the pass, so resolve, enrichment, discovery and the sibling
	// hop all charge the same fair-share row without any of them carrying a
	// job id in its signature.
	ctx = budget.WithJobID(ctx, row.ID)

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

	anchor, err := s.Jobs.SubmittedIdentity(ctx, row.ID)
	if err != nil {
		return err
	}

	// Resolver order step 1: verified local content-addressed cache.
	// Requires a submitted/verified DOI on the immutable anchor — not merely a
	// DOI row.Work may have picked up from a later resolver pass.
	if row.Work.DOI != "" && anchor.AnchorAllowsDOICache(row.Work.DOI) {
		cached, source, err := s.Jobs.FindArtifactByDOI(ctx, row.Work.DOI)
		if err != nil {
			return err
		}
		if cached != nil && s.Artifacts.Verify(cached.SHA256) == nil {
			// A cache hit skips resolve(), which is where enrichDOIWork normally
			// runs, so a DOI-only work would reach `ready` carrying no citation
			// record and could never be exported to the library. Enrich here
			// rather than before the lookup: on a miss, resolve() performs it
			// with proper retry-budget accounting, and this branch cannot
			// re-charge the lookup on a later pass because the job goes `ready`.
			if _, err := s.enrichDOIWork(ctx, row); err != nil {
				return err
			}
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
	row, err = s.Jobs.Get(ctx, row.ID)
	if err != nil {
		return err
	}
	if row.State != job.StateResolving {
		return nil
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

// acquirePolicies lists the quota identities a source may call under, in
// preference order. Only OpenAlex has a keyless fallback tier worth using
// (help.openalex.org/api/authentication: casual use works with no key, free);
// other keyed sources have no comparable anonymous tier, so the fallback is
// deliberately not generic.
func acquirePolicies(name string, policy config.Source) []config.Source {
	if name == config.SourceOpenAlex && strings.TrimSpace(policy.APIKey) != "" {
		anon := policy
		anon.APIKey = ""
		return []config.Source{policy, anon}
	}
	return []config.Source{policy}
}

// anonymousIfFallback marks the adapter call when admission fell back from the
// configured keyed identity to the keyless one, so the adapter omits its API
// key and the request is metered against the identity that admitted it.
func anonymousIfFallback(ctx context.Context, configured, chosen config.Source) context.Context {
	if chosen.APIKey == "" && configured.APIKey != "" {
		return resolver.WithAnonymousCredentials(ctx)
	}
	return ctx
}

func (s *Service) resolve(ctx context.Context, row *job.Row) (map[string]resolver.Candidate, retryPlan, error) {
	var plan retryPlan
	// One read for the whole pass: the anchor is immutable, and both the
	// enrichment write below and the promotion gate at the end of this
	// function must judge against the same attested identity.
	anchor, err := s.Jobs.SubmittedIdentity(ctx, row.ID)
	if err != nil {
		return nil, plan, err
	}
	enrichDOIPlan, err := s.enrichDOIWork(ctx, row)
	if err != nil {
		return nil, plan, err
	}
	plan.merge(enrichDOIPlan)
	if err := s.Jobs.ResetCandidates(ctx, row.ID); err != nil {
		return nil, plan, err
	}
	var all []resolver.Candidate
	enrichPlan, err := s.enrich(ctx, row, anchor)
	if err != nil {
		return nil, plan, err
	}
	plan.merge(enrichPlan)
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
		chosen := entry.Policy
		if s.Budgets != nil {
			var err error
			chosen, err = s.Budgets.AcquireAny(ctx, name, acquirePolicies(name, entry.Policy), entry.EstimatedCost)
			if err != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(err))
				if absorbBudgetRefusal(&plan, err) {
					continue
				}
				return nil, plan, err
			}
		}
		plan.SourcesCalled++
		cands, err := entry.Adapter.Resolve(anonymousIfFallback(ctx, entry.Policy, chosen), row.Work)
		if err != nil {
			if ctx.Err() != nil {
				return nil, plan, ctx.Err()
			}
			if delay, temporary := resolver.Temporary(err); temporary {
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
				plan.TemporaryResolvers++
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, name, chosen, sourceRetry)
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
		// A pending ordinary TEMPORARY retry means the primary sources get
		// their next attempt before the ten-credit fuzzy search is worth
		// asking. Note what this does and does not check: a pass held back
		// only by a durable source gate has no temporary failure, so it
		// satisfies the first arm and the search may run even though that
		// gated source will get another opportunity later. That is
		// deliberate for now — it matches siblingHop's caller exactly, and
		// the per-basis marker bounds it to one search per question — but
		// "necessary" here means "no ordinary retry is pending", not "nothing
		// else could possibly succeed". The typed relations run either way.
		// Only PROVEN exhaustion is a permit: an unreadable history must not
		// authorize the expensive search, even though the same unknown does
		// settle the job elsewhere. The error is deliberately consumed here
		// rather than propagated — a permit that cannot be established is
		// simply absent, and the pass continues without the search.
		exhausted, exhaustionErr := s.retryBudgetExhaustedProven(ctx, row.ID)
		if exhaustionErr != nil {
			exhausted = false
		}
		atBoundary := plan.Temporary().IsZero() || exhausted || plan.StickyBudgetGate
		siblings, siblingPlan := s.resolveSiblings(ctx, row, atBoundary)
		all = append(all, siblings...)
		plan.merge(siblingPlan)
	}

	ranked, evidence := resolver.Rank(row.Policy.DesiredVersion, all)

	// Promotion gate (after enrich/resolver loops above): budget refusals recorded
	// via absorbBudgetRefusal during those loops park the pass but never bypass
	// this gate — a mixed pass may carry ClosedSourceGates on plan while still
	// ranking candidates, and only AuthorityExactEcho may durably adopt identity
	// fields from them. Sparse-anchor disposition (below) runs after promotion:
	// title-only attested anchors may list candidates, but candidate-derived
	// facts cannot verify canonical identity without independent authority.
	resolved := accumulatePromotedIdentity(row.Work, ranked)
	if !sameWorkIdentity(resolved, row.Work) {
		updated, err := s.Jobs.FillWorkMetadata(ctx, row.ID, resolved)
		if err != nil {
			return nil, plan, err
		}
		row.Work = updated.Work
	}

	if anchor.InsufficientIdentityAuthority() && len(ranked) > 0 {
		if err := s.settleInsufficientIdentity(ctx, row, job.StateResolving); err != nil {
			return nil, plan, err
		}
		return map[string]resolver.Candidate{}, plan, nil
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
	// This hop only ever runs at the fetch-exhaustion boundary — its caller
	// resolves endsHere before calling — so necessity is already established.
	cands, siblingPlan := s.resolveSiblings(ctx, row, true)
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
func (s *Service) resolveSiblings(ctx context.Context, row *job.Row, atBoundary bool) ([]resolver.Candidate, retryPlan) {
	typed, plan := s.typedSiblings(ctx, row)
	if len(typed) > 0 {
		return typed, plan
	}
	// Everything below is the fuzzy title search, and it is the most expensive
	// request papio makes: OpenAlex prices a singleton entity GET at one credit
	// and a search at ten. Measured over one day of real traffic, 304 of these
	// ran, 280 returned nothing, and they accounted for ~90% of the daily
	// spend. So it runs only when it is both NECESSARY and NOVEL.
	//
	// Necessary: the primary candidates have no ordinary retry left to take.
	// This mirrors siblingHop's caller, whose rule is that a pending temporary
	// retry deserves the next attempt before a sibling hop pre-empts it; the
	// resolve() path never applied it and paid ten credits per pass while an
	// unrelated source's 503 was still clearing.
	//
	// Novel: this job has not already completed one successful search for the
	// same bibliographic basis. A search that returned zero candidates is a
	// fact about the provider's index, not a transient failure, and re-asking
	// the identical question cannot produce a different answer until the
	// question changes. A transport failure records nothing, so it stays
	// retryable.
	if !atBoundary {
		return nil, plan
	}
	basis := siblingSearchBasis(row.Work)
	if s.siblingSearchRecorded(ctx, row.ID, basis) {
		return nil, plan
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
		chosen := entry.Policy
		if s.Budgets != nil {
			var acquireErr error
			chosen, acquireErr = s.Budgets.AcquireAny(ctx, name, acquirePolicies(name, entry.Policy), entry.EstimatedCost)
			if acquireErr != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(acquireErr))
				absorbBudgetRefusal(&plan, acquireErr)
				continue
			}
		}
		cands, err := sibling.ResolveSiblings(anonymousIfFallback(ctx, entry.Policy, chosen), row.Work)
		// A sibling lookup can legitimately make no request at all: with no
		// cached canonical record and no caller-supplied title there is
		// nothing to search on. Charging that would spend a retry attempt on
		// a pass where this source cost nothing.
		if errors.Is(err, resolver.ErrNoSearchBasis) {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "no_search_basis")
			continue
		}
		plan.SourcesCalled++
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
					_ = s.Budgets.Defer(ctx, name, chosen, sourceRetry)
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
		// The provider answered, so this basis is now known-searched whatever
		// it returned. Recorded before the candidate check so a zero-result
		// search — the expensive common case — is the one that gets
		// suppressed next pass.
		s.recordSiblingSearch(ctx, row.ID, basis)
		if len(valid) > 0 {
			return valid, plan
		}
	}
	return nil, plan
}

// siblingSearchEventKind durably records that one fuzzy sibling search already
// completed for a given bibliographic basis, so a later pass does not pay for
// the identical question. Like oaBrowserHintEventKind, it carries no URL and
// is read back through the job's own event stream.
const siblingSearchEventKind = "job.sibling_search"

// siblingSearchBasis names the question a fuzzy search asks: the identifiers
// and bibliography it searches on. It uses the acquisition-side,
// version-preserving DOI normalization — v2 is a different work to acquire
// than v1, so it is also a different question to ask — and never the
// ownership-side collapsing form. Enrichment that materially changes the
// title, year, or authors changes the basis, which correctly buys one new
// search.
func siblingSearchBasis(w work.Work) string {
	doi := strings.TrimSpace(w.DOI)
	if normalized, err := work.NormalizeDOI(doi); err == nil {
		doi = normalized
	}
	authors := make([]string, 0, len(w.Authors))
	for _, author := range w.Authors {
		if trimmed := strings.ToLower(strings.TrimSpace(author)); trimmed != "" {
			authors = append(authors, trimmed)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		doi,
		strings.ToLower(strings.Join(strings.Fields(w.Title), " ")),
		strconv.Itoa(w.Year),
		strings.Join(authors, "|"),
	}, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// siblingSearchRecorded reports whether this job already completed a fuzzy
// search for this basis. It fails CLOSED in every direction it can: skipping a
// ten-credit query papio cannot prove it needs is the cheap error, and the free
// typed relations above it still run.
//
// "Every direction" includes an unreadable detail, which is not hypothetical:
// Jobs.Events decodes each detail with `_ = json.Unmarshal(...)` and yields a
// nil map on failure, so a truncated or corrupt row would otherwise leave
// basis "" — matching nothing, buying the search again, and doing it precisely
// when storage is already misbehaving. A marker of the right kind is proof a
// search happened; only a marker whose basis is legible and different is
// evidence that this particular question is new.
func (s *Service) siblingSearchRecorded(ctx context.Context, jobID, basis string) bool {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return true
	}
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != siblingSearchEventKind {
			continue
		}
		detail, ok := event["detail"].(map[string]any)
		if !ok {
			return true
		}
		recorded, ok := detail["basis"].(string)
		if !ok || recorded == "" || recorded == basis {
			return true
		}
	}
	return false
}

// recordSiblingSearch persists the basis just searched. A failure to record is
// logged and tolerated: the search already happened, and the only consequence
// is that a later pass may pay for it again.
func (s *Service) recordSiblingSearch(ctx context.Context, jobID, basis string) {
	if err := s.Jobs.RecordEvent(context.WithoutCancel(ctx), jobID, siblingSearchEventKind,
		map[string]any{"basis": basis}); err != nil {
		log.Printf("papio: record sibling search basis for job %s: %v", jobID, err)
	}
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
	chosen := policy
	if s.Budgets != nil {
		var acquireErr error
		chosen, acquireErr = s.Budgets.AcquireAny(ctx, name, acquirePolicies(name, policy), 0)
		if acquireErr != nil {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(acquireErr))
			absorbBudgetRefusal(&plan, acquireErr)
			return nil, plan
		}
	}
	plan.SourcesCalled++
	sibs, err := relations.VersionSiblings(ctx, row.Work.DOI)
	if err != nil {
		if delay, temporary := resolver.Temporary(err); temporary {
			sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
			plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
			plan.TemporaryResolvers++
			if s.Budgets != nil {
				_ = s.Budgets.Defer(ctx, name, chosen, sourceRetry)
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
			sibChosen := entry.Policy
			if s.Budgets != nil {
				var acquireErr error
				sibChosen, acquireErr = s.Budgets.AcquireAny(ctx, rname, acquirePolicies(rname, entry.Policy), entry.EstimatedCost)
				if acquireErr != nil {
					absorbBudgetRefusal(&plan, acquireErr)
					if valid == 0 {
						outcome, detail = "budget_blocked", safeType(acquireErr)
					}
					break
				}
			}
			plan.SourcesCalled++
			cands, err := entry.Adapter.Resolve(anonymousIfFallback(ctx, entry.Policy, sibChosen), work.Work{DOI: sib})
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
						_ = s.Budgets.Defer(ctx, rname, sibChosen, sourceRetry)
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

// enrichDOIWork fills a DOI-only work's bibliography from the discovery
// backend. It returns a retryPlan because that lookup is a real, budgeted
// provider request: a pass whose only outbound call was this one must still be
// charged against the bounded retry budget, or a job that keeps failing later
// re-runs it every cycle for free. Enrichment never fails the job.
func (s *Service) enrichDOIWork(ctx context.Context, row *job.Row) (retryPlan, error) {
	var plan retryPlan
	if s.Discovery == nil || strings.TrimSpace(row.Work.Title) != "" || strings.TrimSpace(row.Work.DOI) == "" {
		return plan, nil
	}
	if _, normErr := work.NormalizeDOI(row.Work.DOI); normErr != nil {
		return plan, nil // invalid DOI: LookupWork would reject it pre-wire; skip the call entirely
	}
	discovered, err := s.Discovery.LookupWork(ctx, row.Work.DOI)
	var exceeded *budget.ErrExceeded
	var deferred *budget.ErrDeferred
	if errors.As(err, &exceeded) || errors.As(err, &deferred) {
		if exceeded != nil {
			plan.recordExceeded(exceeded)
		}
		if deferred != nil {
			plan.recordDeferral(deferred)
		}
		return plan, nil // admission refused inside sourcegate.Client: no request was made
	}
	plan.SourcesCalled++ // admission passed: the request reached the wire (the rare
	// pre-wire seedURL-construction failure is charged too — bounded over-charge,
	// vs. a post-wire 404/decode failure going uncharged, which is unbounded)
	if err != nil {
		return plan, nil // post-wire failure; enrichment never fails the job
	}
	changed, err := s.Jobs.EnrichWorkRequestMetadata(
		ctx, row.WorkRequestID, discovered.Work.Title, discovered.Work.Authors, discovered.Work.Year,
	)
	if err != nil {
		return plan, err
	}
	if !changed {
		return plan, nil
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
	return plan, nil
}

func (s *Service) metadataEnricherEntries() []MetadataEnricherEntry {
	if len(s.MetadataEnrichers) > 0 {
		return s.MetadataEnrichers
	}
	if s.Enricher == nil {
		return nil
	}
	// Keep the legacy single-enricher field as the Crossref seam used by
	// callers and by typed version-relation traversal.
	return []MetadataEnricherEntry{{
		Name: config.SourceCrossrefMetadata, Enricher: s.Enricher,
	}}
}

// enrich runs the metadata enrichers over a title-only work. It returns a
// retryPlan because each enricher call is a real, budgeted provider request —
// an OpenAlex title search is the most expensive request shape papio makes —
// and a pass that spent one must be charged against the bounded retry budget
// even when it found nothing. Only an identified PRE-WIRE decline
// (resolver.ErrNotApplicable, or admission refused before the adapter ran) is
// exempt: undercharging a request that actually went out lets a mixed pass
// classify source_gate and loop forever, spending real credits every cycle.
func (s *Service) enrich(ctx context.Context, row *job.Row, anchor job.SubmittedIdentity) (retryPlan, error) {
	var plan retryPlan
	if strings.TrimSpace(row.Work.Title) == "" {
		return plan, nil
	}
	for _, entry := range s.metadataEnricherEntries() {
		if entry.Enricher == nil || row.Work.HasFetchableIdentifier() {
			break
		}
		name := entry.Name
		if name == "" {
			name = config.SourceCrossrefMetadata
		}
		policy := s.Config.SourcePolicy(name)
		if !policy.Enabled || !row.Policy.SourceAllowed(name) {
			continue
		}
		attempt, err := s.Jobs.StartAttempt(ctx, row.ID, 0, "resolve", name)
		if err != nil {
			return plan, err
		}
		chosen := policy
		if s.Budgets != nil {
			var acquireErr error
			chosen, acquireErr = s.Budgets.AcquireAny(ctx, name, acquirePolicies(name, policy), 0)
			if acquireErr != nil {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "budget_blocked", 0, safeType(acquireErr))
				// An optional source that is spent or gated must not prevent a
				// later metadata source from attempting this same pass.
				if absorbBudgetRefusal(&plan, acquireErr) {
					continue
				}
				return plan, acquireErr
			}
		}
		enriched, matched, err := entry.Enricher.Enrich(anonymousIfFallback(ctx, policy, chosen), row.Work)
		if errors.Is(err, resolver.ErrNotApplicable) {
			// The enricher declined before making any request: nothing was
			// spent, so nothing is charged. Same silent skip as a no-op.
			_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "not_applicable")
			continue
		}
		plan.SourcesCalled++
		if err != nil {
			if ctx.Err() != nil {
				return plan, ctx.Err()
			}
			if delay, temporary := resolver.Temporary(err); temporary {
				sourceRetry := earlierRetry(time.Time{}, s.Now(), delay, s.RetryDelay)
				plan.ResolverTemporary = earlierTime(plan.ResolverTemporary, sourceRetry)
				plan.TemporaryResolvers++
				if s.Budgets != nil {
					_ = s.Budgets.Defer(ctx, name, chosen, sourceRetry)
				}
				_ = s.Jobs.FinishAttempt(ctx, attempt, "retryable", 0, safeType(err))
			} else {
				_ = s.Jobs.FinishAttempt(ctx, attempt, "failed", 0, safeType(err))
			}
			// Metadata enrichment is optional: one source's failure degrades
			// to the next source instead of failing or short-circuiting resolve.
			continue
		}
		if !matched {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "no_confident_match")
			continue
		}
		// Item 5: an enricher match is SEARCH evidence. It may create
		// candidates, but only evidence verified as describing the submitted
		// canonical work may mutate canonical identity durably before
		// validation. `matchesTitleSearch` accepts a title-only submission on
		// the normalized title alone, so persisting the matched record's own
		// DOI here would let a wrong title match adopt a wrong identifier —
		// and validation then compares the PDF against that adopted identity
		// and agrees with itself. enrichmentPersistWork keeps the durable
		// write to gaps the anchor left open.
		persistable, hasWrite, accepted := enrichmentPersistWork(anchor, enriched)
		if !accepted {
			_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "metadata_conflict_rejected")
			continue
		}
		if hasWrite {
			updated, err := s.Jobs.FillWorkMetadata(ctx, row.ID, persistable)
			if err != nil {
				return plan, err
			}
			row.Work = updated.Work
		}
		// The unpersisted remainder still serves THIS pass: resolvers may use a
		// search-derived identifier to look for candidates, which is what
		// search evidence is licensed to do. It is deliberately not written.
		row.Work = mergeObservedInMemory(row.Work, enriched)
		_ = s.Jobs.FinishAttempt(ctx, attempt, "success", 0, "metadata_enriched")
		// Enrichment has just given this job a strong identifier, which is the
		// first moment papio can know it duplicates a job it is already running:
		// submit-time dedup correctly matched nothing, because a title-only
		// request has no canonical key.
		_, _ = s.Jobs.RecordDuplicateWork(ctx, row.ID, row.Work)
	}
	return plan, nil
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
					if err := s.Jobs.MarkCandidate(ctx, stored.ID, "retryable"); err != nil {
						return err
					}
					plan.recordExceeded(exceeded)
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
					plan.recordDeferral(deferred)
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
			job.Access(manualRequiresAuth, "landing_page"),
			job.WithHumanActionDiagnosis(job.DiagnosisReasonLandingPageOnly)); err != nil {
			return err
		}
		return s.park(ctx, row.ID, job.StateFetching, job.StateAwaitingHuman,
			job.WithCutoverDecision(
				map[string]any{"reason": "landing_page_only"},
				job.InstitutionCutoverDecision{
					Blocker:                job.InstitutionCutoverBlockerLiveSourceRemaining,
					CanaryReadyRouteExists: false,
				},
			))
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
		at, kind = plan.LatestGate, retryKindExhaustedGate
	}
	if at.IsZero() || !at.After(now) {
		// Either the gate elapsed while the rest of the pass ran, or nothing
		// but this process's own throttle refused a source. Persisting a past
		// time makes the scheduler re-claim instantly and spend another
		// attempt on a wait that already happened; persisting the throttle's
		// own sub-second time does the same thing at token-bucket speed.
		at = now.Add(s.RetryDelay)
	}
	detail["retry_kind"] = kind
	decision := retryCutoverDecision(plan)
	if kind == retryKindExhaustedGate {
		// This park is deliberately forced to the gate: the retry budget is
		// spent, and only the one source that never got a request justifies
		// this final wait. Do not let stale/mixed temporary observations from
		// the pass relabel that forced gate decision.
		decision = job.InstitutionCutoverDecision{
			Blocker:                job.InstitutionCutoverBlockerSourceGateOnly,
			CanaryReadyRouteExists: false,
		}
	}
	detail = job.WithCutoverDecision(detail, decision)
	return s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, detail, job.WithRetryAt(at))
}

// retryCutoverDecision classifies only facts observed in the current resolver
// and fetch pass. A mixed temporary failure plus source gate is still a
// transient retry, and so is a pass that reached any source at all:
// source_gate_only is reserved for the strict case where no callable source
// made a request. The SourcesCalled condition keeps this in agreement with
// retryPlan.Kind — otherwise the same transition would persist
// retry_kind: temporary beside a source_gate_only diagnosis.
func retryCutoverDecision(plan retryPlan) job.InstitutionCutoverDecision {
	blocker := job.InstitutionCutoverBlockerNone
	switch {
	case !plan.Temporary().IsZero() || plan.RetryableCandidates > 0 || plan.TemporaryResolvers > 0 || plan.SourcesCalled > 0:
		blocker = job.InstitutionCutoverBlockerTransientRetryRemaining
	case plan.ClosedSourceGates > 0 || !plan.Gate.IsZero():
		blocker = job.InstitutionCutoverBlockerSourceGateOnly
	case plan.AdvisoryOnly():
		// This process throttled itself. No source refused papio and no
		// institutional route was ruled out, so the honest classification is
		// an ordinary transient retry, not a closed source gate.
		blocker = job.InstitutionCutoverBlockerTransientRetryRemaining
	}
	return job.InstitutionCutoverDecision{
		Blocker:                blocker,
		CanaryReadyRouteExists: false,
	}
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
	if s.Notifier == nil || (to != job.StateAwaitingHuman && to != job.StateNeedsReview) {
		return nil
	}
	// The transition and action row are durable before routing. A routing
	// failure is deliberately swallowed so acquisition remains authoritative.
	actions, err := s.Jobs.ListOpenHumanActionsForJobs(context.WithoutCancel(ctx), []string{jobID})
	if err != nil {
		log.Printf("papio: notification action lookup for job %s: %v", jobID, err)
		return nil
	}
	action := job.HumanAction{JobID: jobID}
	if len(actions) > 0 {
		action = actions[len(actions)-1]
	}
	happened := s.Now().UTC()
	if created, parseErr := time.Parse(time.RFC3339Nano, action.CreatedAt); parseErr == nil {
		happened = created.UTC()
	}
	window := 5 * time.Minute
	if policy, policyErr := notify.ResolvePolicy(s.Config.Notify); policyErr == nil {
		if configured := policy.For(notify.CategoryDecisionOpened).Window; configured > 0 {
			window = configured
		}
	}
	windowStart := happened.Truncate(window)
	aggregate := fmt.Sprintf("decision:%s", windowStart.Format(time.RFC3339Nano))
	if action.BlockedBy != "" {
		// One typed gate/claim is one effective turn, regardless of dependent
		// siblings. Its durable identity remains stable across the coalescing
		// window so it cannot create duplicate notices.
		aggregate = "gate:" + action.BlockedBy
	}
	event := notify.Event{
		Message: "papio has work waiting for you — open the papio inbox",
		Count:   1,
	}
	if err := s.Jobs.RecordEvent(context.WithoutCancel(ctx), jobID, "notification.intent", map[string]any{
		"event_kind": "action.opened", "category": string(notify.CategoryDecisionOpened),
		"phase": string(notify.PhaseOpened), "aggregate_key": aggregate,
		"window_start": windowStart.Format(time.RFC3339Nano),
	}); err != nil {
		log.Printf("papio: recording action notification for job %s: %v", jobID, err)
		return nil
	}
	intent := notify.Intent{
		EventKind: "action.opened", Category: notify.CategoryDecisionOpened,
		AggregateKey: aggregate, Phase: notify.PhaseOpened,
		WindowStart: windowStart, JobID: jobID,
		HappenedAt: happened, Message: event.Message, Detail: event,
	}
	if err := s.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
		log.Printf("papio: routing action notification for job %s: %v", jobID, err)
	}
	return nil
}

// exhaustion boundary (institutional handoff, or unavailable) instead of
// re-scheduling another attempt.
const maxRetryAttempts = 8

// retryBudgetExhausted reports whether a job has already spent its bounded
// attempts. It counts durable transition events into retry_wait but skips
// parks that made no request at all — retryKindSourceGate (a durable gate
// held the source back) and retryKindAdvisory (this process's own token
// bucket did). Charging either would settle a job "temporary source failures
// did not clear" about sources papio never called. Events written before
// those discriminators existed carry no retry_kind and are counted, which
// preserves the original bound for existing jobs.
//
// An unreadable history fails CLOSED. This function is the only bound on
// provider spend for a job whose candidates are all dead, so treating "I
// cannot prove any budget remains" as "budget remains" makes the bound
// unenforceable by construction: a persistently failing Events read authorizes
// unlimited paid passes. Its sibling alreadyWaitedPastExhaustion has always
// failed closed for the same reason, and the cost of being wrong here is one
// job settling early — recoverable with `papio jobs retry` — against a
// provider quota that is not recoverable until the next reset.
func (s *Service) retryBudgetExhausted(ctx context.Context, jobID string) bool {
	exhausted, err := s.retryBudgetExhaustedProven(ctx, jobID)
	if err != nil {
		return true
	}
	return exhausted
}

// retryBudgetExhaustedProven reports exhaustion only from history it actually
// read, and hands the read error back instead of folding it into the verdict.
//
// The distinction is load-bearing because exhaustion is consumed in two
// opposite senses. For liveness (parkForRetry, siblingHop) "unknown" must mean
// "stop", so retryBudgetExhausted above fails closed. But resolve() also reads
// exhaustion as a positive PERMIT — it is one arm of atBoundary, which is what
// authorizes the ten-credit fuzzy search — and there "unknown" must not mean
// "go". A single transient Events failure would otherwise buy a search that no
// proven fact justified: the read fails once, exhaustion reads true, the
// separate marker read then succeeds and finds no marker, and the expensive
// query runs while an ordinary temporary retry was still pending.
func (s *Service) retryBudgetExhaustedProven(ctx context.Context, jobID string) (bool, error) {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return false, err
	}
	n := 0
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "job.transition" {
			continue
		}
		detail, ok := event["detail"].(map[string]any)
		if !ok {
			// Jobs.Events decodes each detail with `_ = json.Unmarshal(...)`, so
			// an illegible row arrives as a nil map rather than an error.
			// Skipping it would silently shrink the retry budget's evidence and
			// let a job keep spending; a transition to some unknown state is
			// still a transition this job made, so surface the doubt to the
			// caller and let each read decide what unknown means.
			return false, fmt.Errorf("job %s: unreadable transition detail", jobID)
		}
		if to, _ := detail["to"].(string); to != job.StateRetryWait {
			continue
		}
		switch kind, _ := detail["retry_kind"].(string); kind {
		case retryKindSourceGate, retryKindAdvisory:
			continue
		}
		n++
	}
	return n >= maxRetryAttempts, nil
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
// means no automatic fetch route a login can open; ISBN is a deliberate
// human-assisted catalogue/ebook exception handled by exhaustedCandidates.
// The second half exists because a DOI that merely *parses* is not a DOI that
// exists. A mistyped one survives every upstream check — Crossref, OpenAlex,
// EuropePMC and Unpaywall all report "no record" and "no open copy" as the
// same empty result — and then reaches the link resolver, which has nothing to
// match and bounces the user to doi.org's "DOI NOT FOUND" page. The handoff
// can never be completed, so it re-offers on every session-live tick and
// re-notifies on the reminder schedule forever.
//
// The registry is consulted only when a DOI is the sole fetchable identifier
// (a PMID, arXiv id or OpenAlex id is its own route) and only where a handoff
// is actually about to be created or repaired. A probe failure fails open,
// like institutionalRouteExhausted: during a registry outage another handoff
// is far cheaper than terminating a job that was perfectly fetchable. The
// registry client memoizes, so the once-a-minute repair sweep over every
// parked job does not become a request per job per tick.
func (s *Service) handoffGate(ctx context.Context, w work.Work, resolverName string) (ok bool, reason string, terminal job.TerminalReason) {
	if !w.HasFetchableIdentifier() {
		// ISBN is deliberately not a fetchable identifier: the resolver can
		// locate a catalogue or ebook record, but papio cannot automatically
		// fetch and validate a book PDF. It is nevertheless actionable in the
		// human-assisted institutional flow when that profile has a usable
		// OpenURL destination.
		if w.ISBN != "" {
			if base, configured := s.Config.OpenURLBaseFor(resolverName); configured && base != "" {
				return true, "", ""
			}
		}
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
	decision := job.InstitutionCutoverDecision{
		Blocker:                job.InstitutionCutoverBlockerNone,
		CanaryReadyRouteExists: false,
	}
	switch mode {
	case config.ModeAssisted, config.ModeDelegated:
		if oaBrowserURL != "" {
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", OABrowserHandoffActionDetail(oaBrowserURL), job.Access(false, "anti_bot")); err != nil {
				return err
			}
			return s.park(ctx, row.ID, from, job.StateAwaitingHuman,
				job.WithCutoverDecision(
					map[string]any{"reason": "open_access_browser_handoff"},
					job.InstitutionCutoverDecision{
						Blocker:                job.InstitutionCutoverBlockerLiveSourceRemaining,
						CanaryReadyRouteExists: false,
					},
				))
		}
		institutionalExhausted := s.institutionalRouteExhausted(ctx, row.ID)
		base, hasBase := s.Config.OpenURLBaseFor(row.Policy.Resolver)
		routeable, gateReason, gateTerminal := s.handoffGate(ctx, row.Work, row.Policy.Resolver)
		switch {
		// A work with no identifier a login could act on must never be routed
		// to an institutional sign-in. An ISBN is the one assisted exception:
		// the resolver can locate a catalogue or ebook record from its book
		// metadata, while papio leaves the human to obtain any readable file.
		// A bare title or an unregistered DOI still lands on a catalogue/error
		// page — no login produces a PDF, so those handoffs spend the user's SSO
		// round trip and park forever.
		case !routeable:
			reason, terminal = gateReason, gateTerminal
			if gateReason == "no_identifier" || gateReason == "doi_not_registered" {
				decision.Blocker = job.InstitutionCutoverBlockerIdentifierGate
			} else {
				decision.Blocker = job.InstitutionCutoverBlockerNoLegalRoute
			}
		case hasBase && base != "" && !institutionalExhausted:
			detail := InstitutionalOpenURLHandoffDetail
			if !row.Work.HasFetchableIdentifier() && row.Work.ISBN != "" {
				// ISBN is an assisted-only rescue: persist the narrower
				// ceiling before opening the action so a restarted bridge
				// cannot advertise delegated automation for a catalogue/ebook
				// handoff. NarrowPolicyAccessMode is monotone, and the row
				// copy is updated for this pass's offer construction.
				if err := s.Jobs.NarrowPolicyAccessMode(ctx, row.ID, config.ModeAssisted); err != nil {
					return err
				}
				row.Policy.AccessMode = config.ModeAssisted
				detail = InstitutionalBookOpenURLHandoffDetail
			}
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", detail, job.Access(true, "paywall")); err != nil {
				return err
			}
			return s.park(ctx, row.ID, from, job.StateAwaitingHuman,
				job.WithCutoverDecision(
					map[string]any{"reason": "institutional_handoff"},
					decision,
				))
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
			_, _, configured := s.deliveryConfigured(row)
			if configured {
				decision.Blocker = job.InstitutionCutoverBlockerNone
			} else {
				decision.Blocker = job.InstitutionCutoverBlockerNoLegalRoute
			}
			result, err := s.deliveryRoute(withCutoverDecision(ctx, decision), row, from)
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
		routeable, gateReason, gateTerminal := s.handoffGate(ctx, row.Work, row.Policy.Resolver)
		base, hasBase := s.Config.OpenURLBaseFor(row.Policy.Resolver)
		if hasBase && base != "" && routeable {
			decision.Blocker = job.InstitutionCutoverBlockerPolicyGate
			if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_available",
				"no direct candidates; institutional OpenURL available but not opened in conservative mode",
				// An advisory, not a sign-in prompt: it exists precisely to say
				// that institutional access was NOT opened.
				job.Access(false, "")); err != nil {
				return err
			}
		}
		// ADR-0017 Decision 3B condition 1: conservative mode discovers a
		// configured delivery route but never opens or submits it. Preserve
		// that observation independently of the cutover blocker payload.
		if _, dd, ok := s.deliveryConfigured(row); ok && routeable {
			if err := s.Jobs.RecordEvent(ctx, row.ID, "delivery.route_discovered", map[string]any{
				"provider": dd.Kind,
				"reason":   "not opened in conservative mode",
			}); err != nil {
				return err
			}
		}
		// A syntactically handoff-capable work is not itself a legal route:
		// without an OpenURL destination, and without a configured delivery
		// service, conservative mode has nothing institutional to discover or
		// hold back. Keep the identifier gate above distinct, but classify this
		// exhausted boundary as no_legal_route rather than the initial none.
		if !routeable {
			reason, terminal = gateReason, gateTerminal
			if gateReason == "no_identifier" || gateReason == "doi_not_registered" {
				decision.Blocker = job.InstitutionCutoverBlockerIdentifierGate
			} else {
				decision.Blocker = job.InstitutionCutoverBlockerNoLegalRoute
			}
		} else if hasBase && base != "" {
			decision.Blocker = job.InstitutionCutoverBlockerPolicyGate
		} else if _, _, configured := s.deliveryConfigured(row); configured {
			decision.Blocker = job.InstitutionCutoverBlockerPolicyGate
		} else {
			decision.Blocker = job.InstitutionCutoverBlockerNoLegalRoute
		}
	}
	err := s.Jobs.Transition(ctx, row.ID, from, job.StateUnavailable,
		job.WithCutoverDecision(
			map[string]any{"reason": reason},
			decision,
		), job.WithTerminalReason(terminal))
	if err == nil {
		s.recordStandaloneOutcome(ctx, row)
	}
	return err
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
type cutoverDecisionContextKey struct{}

func withCutoverDecision(ctx context.Context, decision job.InstitutionCutoverDecision) context.Context {
	return context.WithValue(ctx, cutoverDecisionContextKey{}, decision)
}
func deliveryRetryCutoverDetail(ctx context.Context, detail map[string]any) map[string]any {
	decision, ok := ctx.Value(cutoverDecisionContextKey{}).(job.InstitutionCutoverDecision)
	if !ok {
		return detail
	}
	decision.Blocker = job.InstitutionCutoverBlockerTransientRetryRemaining
	decision.CanaryReadyRouteExists = false
	return job.WithCutoverDecision(detail, decision)
}

func deliveryCutoverDetail(ctx context.Context, detail map[string]any) map[string]any {
	decision, ok := ctx.Value(cutoverDecisionContextKey{}).(job.InstitutionCutoverDecision)
	if !ok {
		return detail
	}
	return job.WithCutoverDecision(detail, decision)
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
			s.joinDeliveryPoll(ctx, row, from, dd, existing)
	case delivery.BranchAdoptFulfilled:
		// 2026-08-07 amendment: routeFulfilledDelivery is the sole
		// consumer of a row that landed StateFulfilled — whether Branch
		// observes it here (a later re-evaluation) or a poller settles it
		// directly and calls routeFulfilledDelivery itself. It falls back
		// to the same reconciliation action as BranchReconcile/
		// BranchResubmissionPolicy below whenever no automatic retrieval
		// channel is configured.
		return DeliveryRouteResult{Configured: true, Branch: branch, Request: existing},
			s.routeFulfilledDelivery(ctx, row, from, existing)
	case delivery.BranchReconcile, delivery.BranchResubmissionPolicy:
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
			ProfileClass:       profile.Class,
			ProfileDigest:      profile.Digest(),
			Decision:           decision,
			FulfillmentChannel: profile.FulfillmentChannel,
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
	duplicate := false
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
		duplicate = true
		if created == nil || created.State != delivery.StateOffered || created.ProviderReference != "" {
			// A live or otherwise-resolved request already occupies this
			// idempotency key. Never open a second live request.
			return created, s.openDeliveryReconciliationAction(ctx, row, from, created)
		}
		// Fall through: nothing was ever lodged for this row: retry
		// submission against it instead of reconciling a request that was
		// never actually created.
	}
	if created == nil || created.GateProfileDigest != profile.Digest() {
		if created == nil {
			return created, fmt.Errorf("delivery: missing offered request before submission")
		}
		return created, s.openDeliveryReconciliationAction(ctx, row, from, created)
	}
	if duplicate {
		// Transfer ownership when the existing offered row is owned by a
		// different (potentially cancelled) job. Without this RecordSubmission
		// would check the original owner's job state and leave a provider
		// transaction orphaned with no durable local reference.
		if created.JobID != row.ID {
			ok, err := s.Delivery.ReassignOfferedRequest(ctx, created.ID, row.ID, created.JobID)
			if err != nil {
				return created, err
			}
			if !ok {
				return created, s.openDeliveryReconciliationAction(ctx, row, from, created)
			}
			refreshed, err := s.Delivery.Get(ctx, created.ID)
			if err != nil {
				return created, err
			}
			if refreshed != nil {
				created = refreshed
			}
			// Successfully re-owned an offered row that never reached the
			// provider: allow the retry without requiring a prior failure
			// classification for this new job (the row itself proves no live
			// request exists). This is the ea626f43 fix — bypass the gate
			// that would otherwise route an unclassified duplicate to
			// reconciliation.
			return s.submitToProvider(ctx, row, from, dd, key, profile, created)
		}
		class, classified, err := s.submissionFailureClass(ctx, row.ID, created.ID)
		if err != nil {
			return created, err
		}
		if !classified || class != illiad.FailurePreSend {
			if classified && class == illiad.FailureAmbiguous {
				return created, s.reconcileAmbiguousSubmission(ctx, row, from, dd, profile, created)
			}
			return created, s.openDeliveryReconciliationAction(ctx, row, from, created)
		}
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
func (s *Service) submitToProvider(ctx context.Context, row *job.Row, from string, dd *config.DocumentDelivery, key string, profile delivery.GateProfile, created *delivery.Request) (*delivery.Request, error) {
	traced := newTracedSubmissionClient(s.illiadHTTPClient())
	client := illiad.New(traced, dd.BaseURL, dd.APIKey)
	// Commit an ambiguous classification before the irreversible provider
	// request. Only a proven pre-send failure below can narrow it.
	if err := s.Jobs.RecordEvent(ctx, row.ID, "delivery.submission_failure_classified", map[string]any{
		"delivery_request_id": created.ID,
		"class":               string(illiad.FailureAmbiguous),
	}); err != nil {
		return created, err
	}
	txn, err := client.CreateTransaction(ctx, illiadTransactionRequest(row.Work, dd.PatronRef, key))
	if err != nil {
		class := traced.class
		if trusted, ok := illiad.FailureClassOf(err); ok {
			class = trusted
		}
		if recordErr := s.Jobs.RecordEvent(ctx, row.ID, "delivery.submission_failure_classified", map[string]any{
			"delivery_request_id": created.ID,
			"class":               string(class),
		}); recordErr != nil {
			return created, recordErr
		}
		if class != illiad.FailurePreSend {
			return created, s.reconcileAmbiguousSubmission(ctx, row, from, dd, profile, created)
		}
		return created, s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, deliveryRetryCutoverDetail(ctx, map[string]any{
			"reason":              "resolver_temporarily_unavailable",
			"delivery_request_id": created.ID,
		}), job.WithRetryAt(s.Now().Add(s.RetryDelay)))
	}
	providerRef := strconv.Itoa(txn.TransactionNumber)
	nextCheck := delivery.NextCheck(s.Now(), 0, profile.StatusPollMinutes)
	won, err := s.Delivery.RecordSubmission(ctx, created.ID, providerRef, nextCheck)
	if err != nil {
		return created, s.reconcileAmbiguousSubmission(ctx, row, from, dd, profile, created)
	}
	// RecordSubmission atomically made the provider reference durable before
	// this separate job transition can fail. Re-fetch so callers — the
	// SubmitDelivery RPC/CLI caller in particular — see the row's real,
	// post-submission state rather than a stale Create snapshot.
	submitted, err := s.Delivery.Get(ctx, created.ID)
	if err != nil {
		return created, err
	}
	if submitted == nil {
		return created, fmt.Errorf("delivery: submission row %d disappeared after provider success", created.ID)
	}
	if !won && submitted.ProviderReference != providerRef {
		if err := s.openProviderSubmissionConflict(ctx, row.ID, created.ID, providerRef, submitted.ProviderReference); err != nil {
			return submitted, err
		}
		return submitted, nil
	}
	// Use the durable reference, not our provider response, when a concurrent
	// submitter won the CAS. A differing reference took the reconciliation
	// path above; this is the benign same-reference loser case.
	durableProviderRef := submitted.ProviderReference
	// Plain job.Store.Transition, not s.park: a delivery poll is not a human
	// action, exactly like parkForRetry's ordinary retry_wait never notifies
	// through s.park either.
	return submitted, s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, deliveryCutoverDetail(ctx, map[string]any{
		"reason":              string(job.RetryReasonDocumentDeliveryPending),
		"delivery_request_id": created.ID,
		"provider_reference":  durableProviderRef,
	}), job.WithRetryAt(nextCheck))
}

// reconcileAmbiguousSubmission is the only application call site for the
// shared read-only reconciliation unit. A missing token match is persisted as
// a bounded recheck event and parks the job; it never reaches submission.
func (s *Service) reconcileAmbiguousSubmission(ctx context.Context, row *job.Row, from string, dd *config.DocumentDelivery, profile delivery.GateProfile, req *delivery.Request) error {
	if req == nil || dd == nil {
		return fmt.Errorf("delivery: reconciliation requires request and configuration")
	}
	identity := delivery.ReconciliationIdentity{
		DOI:          row.Work.DOI,
		PMID:         row.Work.PMID,
		RequestClass: req.RequestClass,
		Title:        row.Work.Title,
		Author:       strings.Join(row.Work.Authors, "; "),
	}
	result, err := s.Delivery.Reconcile(ctx, req, delivery.ReconciliationDeps{
		Client:         illiad.New(s.illiadHTTPClient(), dd.BaseURL, dd.APIKey),
		PatronRef:      dd.PatronRef,
		ReferenceField: illiadIdempotencyReferenceField,
		Identity:       identity,
		GateAction:     delivery.ActionSubmit,
		CurrentBinding: profile.Digest(),
	})
	if err != nil {
		// A provider call already happened. Never turn a local persistence
		// failure into a second POST; route to the same reconciliation action.
		return s.openDeliveryReconciliationAction(ctx, row, from, req)
	}
	if err := s.Jobs.RecordEvent(ctx, row.ID, "delivery.reconciliation_outcome", map[string]any{
		"delivery_request_id": req.ID,
		"disposition":         string(result.Disposition),
		"reason":              result.Reason,
		"provider_reference":  result.ProviderReference,
	}); err != nil {
		return err
	}
	switch result.Disposition {
	case delivery.ReconciliationAdopted:
		adopted, err := s.Delivery.Get(ctx, req.ID)
		if err != nil {
			return err
		}
		if adopted == nil {
			return fmt.Errorf("delivery: adopted request %d disappeared", req.ID)
		}
		next := delivery.NextCheck(s.Now(), 0, profile.StatusPollMinutes)
		return s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, deliveryCutoverDetail(ctx, map[string]any{
			"reason":              string(job.RetryReasonDocumentDeliveryPending),
			"delivery_request_id": req.ID,
			"provider_reference":  adopted.ProviderReference,
		}), job.WithRetryAt(next))
	case delivery.ReconciliationNotFoundYet:
		attempt := reconciliationAttemptCount(ctx, s.Jobs, row.ID, req.ID) + 1
		if err := s.Jobs.RecordEvent(ctx, row.ID, "delivery.reconciliation_attempt", map[string]any{
			"delivery_request_id": req.ID,
			"attempt":             attempt,
			"attempted_at":        s.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			return err
		}
		if attempt >= offeredRecoveryMaxAttempts {
			return s.openDeliveryReconciliationAction(ctx, row, from, req)
		}
		delay := offeredRecoveryInitialBackoff << (attempt - 1)
		return s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, deliveryRetryCutoverDetail(ctx, map[string]any{
			"reason":                 string(job.RetryReasonDocumentDeliveryPending),
			"delivery_request_id":    req.ID,
			"reconciliation_attempt": attempt,
		}), job.WithRetryAt(s.Now().Add(delay)))
	default:
		if result.Reason == delivery.ReconciliationReasonCommitConflict && result.ProviderReference != "" {
			current, getErr := s.Delivery.Get(ctx, req.ID)
			if getErr != nil {
				return getErr
			}
			durable := ""
			if current != nil {
				durable = current.ProviderReference
			}
			return s.openProviderSubmissionConflict(ctx, row.ID, req.ID, result.ProviderReference, durable)
		}
		return s.openDeliveryReconciliationAction(ctx, row, from, req)
	}
}

func reconciliationAttemptCount(ctx context.Context, jobs *job.Store, jobID string, requestID int64) int {
	events, err := jobs.Events(ctx, jobID)
	if err != nil {
		return 0
	}
	count := 0
	for _, event := range events {
		if event["kind"] != "delivery.reconciliation_attempt" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		id, _ := detail["delivery_request_id"].(float64)
		if int64(id) == requestID {
			count++
		}
	}
	return count
}

// openProviderSubmissionConflict records the provider reference received by a
// CAS loser and parks the owning job for human reconciliation. The received
// reference is intentionally kept in the event even when another writer's
// reference already occupies the delivery row: both provider-side mutations
// must remain visible to an operator.
func (s *Service) openProviderSubmissionConflict(ctx context.Context, jobID string, requestID int64, receivedReference, durableReference string) error {
	if err := s.Jobs.RecordEvent(ctx, jobID, "delivery.submission_provider_conflict", map[string]any{
		"delivery_request_id":         requestID,
		"received_provider_reference": receivedReference,
		"durable_provider_reference":  durableReference,
	}); err != nil {
		return err
	}
	request, err := s.Delivery.Get(ctx, requestID)
	if err != nil {
		return err
	}
	if request == nil {
		return fmt.Errorf("delivery: submission conflict row %d disappeared", requestID)
	}
	actions, err := s.Jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil {
		return err
	}
	hasDeliveryAction := false
	for _, action := range actions {
		if action.Kind == job.ActionKindDocumentDelivery {
			hasDeliveryAction = true
			break
		}
	}
	if !hasDeliveryAction {
		if _, err := s.Jobs.OpenHumanAction(ctx, jobID, job.ActionKindDocumentDelivery,
			DeliveryReconciliationActionDetail(request), job.Access(false, "")); err != nil {
			return err
		}
	}
	current, err := s.Jobs.Get(ctx, jobID)
	if err != nil || current == nil {
		return err
	}
	switch current.State {
	case job.StateAwaitingHuman, job.StateReady, job.StateImported, job.StateUnavailable, job.StateFailed, job.StateCancelled:
		return nil
	case job.StateQueued, job.StateResolving, job.StateFetching, job.StateValidating, job.StateRetryWait, job.StateNeedsReview:
		return s.park(ctx, jobID, current.State, job.StateAwaitingHuman,
			deliveryCutoverDetail(ctx, map[string]any{"reason": "document_delivery_submission_conflict"}))
	default:
		return nil
	}
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
	return s.park(ctx, row.ID, from, job.StateAwaitingHuman, deliveryCutoverDetail(ctx, map[string]any{"reason": "document_delivery_prefill"}))
}

// joinDeliveryPoll is Decision 3B's `join_poll` branch: this job attaches to
// an already-submitted/pending request rather than evaluating the gate
// again. It is also ADR-0017 Decision 4's poll executor's wake hook: a
// retry_wait job re-entering StateResolving (Process's
// StateQueued/StateRetryWait case) and exhausting candidates again is
// exactly "wake" for a parked delivery request, so this is where the
// actual provider status check happens — internal/delivery.Service.Poll
// itself no-ops when existing's next_check_at is not yet due, so a wake
// forced early (e.g. `papio jobs retry`) still just re-parks on the
// existing schedule without hitting the provider. The existing row's own
// job_id (whichever job first created it) is untouched — this only
// records the join on THIS job's event stream and retry schedule.
func (s *Service) joinDeliveryPoll(ctx context.Context, row *job.Row, from string, dd *config.DocumentDelivery, existing *delivery.Request) error {
	result, err := s.Delivery.Poll(ctx, existing, delivery.PollDeps{
		Client:            illiad.New(s.illiadHTTPClient(), dd.BaseURL, dd.APIKey),
		PatronRef:         dd.PatronRef,
		ReferenceField:    illiadIdempotencyReferenceField,
		StatusPollMinutes: dd.StatusPollMinutes,
	})
	if err != nil {
		return err
	}
	if result.Settled {
		if result.State == delivery.StateFulfilled {
			return s.routeFulfilledDelivery(ctx, row, from, existing)
		}
		return s.openDeliveryReconciliationAction(ctx, row, from, existing)
	}
	next := result.NextCheckAt
	if next.IsZero() {
		next = deliveryJoinPollAt(s.Now(), existing)
	}
	return s.Jobs.Transition(ctx, row.ID, from, job.StateRetryWait, deliveryCutoverDetail(ctx, map[string]any{
		"reason":              string(job.RetryReasonDocumentDeliveryPending),
		"delivery_request_id": existing.ID,
	}), job.WithRetryAt(next))
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
	return s.park(ctx, row.ID, from, job.StateAwaitingHuman, deliveryCutoverDetail(ctx, map[string]any{"reason": "document_delivery_reconciliation"}))
}

// routeFulfilledDelivery is the 2026-08-07 ADR-0017 amendment's sole
// consumer of a delivery_requests row that reached StateFulfilled — the
// existing delivery.BranchAdoptFulfilled branch above, or a poller settling
// a request directly, both call this with the same (row, from, existing)
// shape openDeliveryReconciliationAction already used. Fulfilled means the
// provider supplied the document, never that papio holds trusted bytes: the
// file still has to be retrieved and pass the ordinary quarantine/
// structural/identity pipeline before a job can go ready.
//
// With patron_web_base_url configured (kind=illiad only —
// deliveryConfigured/CompileGateProfile's FulfillmentChannel gates that),
// this builds the ILLiad "View PDF" form-75 URL and routes it through the
// EXISTING openurl_handoff browser-handoff machinery — reusing its access-
// mode dispatch rather than inventing a parallel one:
//   - conservative never opens or submits (Decision 3B condition 1); no
//     action is opened, only an advisory event is recorded, exactly like
//     exhaustedCandidates's conservative branch records delivery.route_discovered
//     instead of an actionable action.
//   - assisted/delegated open the action; internal/browser's
//     offerableAccessMode/offer() — not this function — decide whether the
//     extension opens the tab passively or drives it immediately, exactly
//     as they already do for the institutional OpenURL and OA-candidate
//     handoffs.
//
// Whatever the browser drives to next is out of scope here: a direct PDF
// download lands through the ordinary browser-managed adoption/quarantine
// path (the same path any other openurl_handoff capture uses) with zero
// new code. A custom, non-inline-PDF landing page is deliberately NOT
// scanned for "PDF-looking" links — that heuristic is exactly what a
// provider-aware ILLiad adapter would replace with a real parser, and
// building a fixture-backed one is future work; today it is a human
// action, same as the Firefox no-download-steering limitation already is
// by design.
//
// Without patron_web_base_url, papio cannot construct a retrieval URL at
// all: this falls back to the pre-existing reconciliation action rather
// than dropping a fulfilled request on the floor.
func (s *Service) routeFulfilledDelivery(ctx context.Context, row *job.Row, from string, existing *delivery.Request) error {
	_, dd, ok := s.deliveryConfigured(row)
	if !ok || dd.PatronWebBaseURL == "" {
		return s.openDeliveryReconciliationAction(ctx, row, from, existing)
	}
	retrievalURL := delivery.FulfillmentRetrievalURL(dd.PatronWebBaseURL, existing.ProviderReference)
	if retrievalURL == "" {
		// No provider reference recorded (should not happen for a row that
		// reached fulfilled through the ordinary submit/poll path, but a
		// human-confirmed confirm_request_exists row could) — the same
		// "cannot build a route" fallback as an absent base URL.
		return s.openDeliveryReconciliationAction(ctx, row, from, existing)
	}
	eventDetail := map[string]any{
		"route_class":         "document_delivery",
		"provider_reference":  existing.ProviderReference,
		"delivery_request_id": existing.ID,
	}
	if s.Config.EffectiveAccessMode(row.Policy.AccessMode) == config.ModeConservative {
		return s.Jobs.RecordEvent(ctx, row.ID, "delivery.retrieval_discovered", eventDetail)
	}
	if err := s.Jobs.RecordEvent(ctx, row.ID, "delivery.retrieval_enqueued", eventDetail); err != nil {
		return err
	}
	if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff",
		DocumentDeliveryRetrievalHandoffDetail+"\n"+retrievalURL, job.Access(false, "")); err != nil {
		return err
	}
	return s.park(ctx, row.ID, from, job.StateAwaitingHuman, deliveryCutoverDetail(ctx, map[string]any{"reason": "document_delivery_retrieval"}))
}

// DocumentDeliveryRetrievalHandoffDetail identifies an openurl_handoff
// action that must retrieve a fulfilled document-delivery request's
// patron-web page rather than following the institution's ordinary
// resolver or a one-time OA candidate. The browser bridge reads this
// exactly as it reads OABrowserHandoffDetail: a fixed marker line, then
// the retrieval URL on the line that follows.
const DocumentDeliveryRetrievalHandoffDetail = "document delivery: retrieve the fulfilled request from your institution's request-management portal"

// DocumentDeliveryRetrievalHandoffURL returns the retrieval URL stored in a
// document-delivery retrieval handoff detail. The strict two-line shape
// mirrors OABrowserHandoffURL and avoids accepting an arbitrary human-action
// message as a browser URL.
func DocumentDeliveryRetrievalHandoffURL(detail string) (string, bool) {
	const prefix = DocumentDeliveryRetrievalHandoffDetail + "\n"
	url, ok := strings.CutPrefix(detail, prefix)
	if !ok || url == "" || strings.ContainsAny(url, "\r\n") || !strings.HasPrefix(url, "https://") {
		return "", false
	}
	return url, true
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
	// Validate against what the requester attested, not against row.Work, which
	// earlier passes may have enriched from search evidence. Comparing a PDF to
	// an identity that a resolver or enricher supplied lets a wrong match
	// confirm itself: the adopted DOI is the one the document then "agrees"
	// with. validationTarget keeps legacy rows (no attested anchor) unchanged.
	anchor, err := s.Jobs.SubmittedIdentity(ctx, row.ID)
	if err != nil {
		return false, false, err
	}
	report, validateErr := s.Validate(ctx, result.TempPath, result.ContentType, validationTarget(anchor, row))
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
	// The conclusive-identity veto: MatchIdentityWithThreshold's foreign-DOI
	// rejection is gated on wantDOI != "" (identity.go:148-162), so a job
	// with NO DOI can never be contradicted by the document's own
	// conclusive DOI — an exact printed title plus matching authors returns
	// IdentityPass and the bytes get promoted under the wrong citation.
	// That silent wrong-accept is what this arm closes. Computed once
	// alongside needsIdentityReview so switch and verdict share one value.
	conclusiveVeto := pdf.CheckConclusiveIdentity(report.Text.Excerpt, job.BoundDOIs(anchor, row.Work))
	// Recorded before the branch, not inside each arm: the verdict is a function
	// of the report alone, so one call site cannot drift from the decision below,
	// and evidence survives even for the candidates papio throws away — which is
	// exactly the set a consumer asks "why not this one?" about.
	s.recordValidation(ctx, row.ID, stored.ID, result.SHA256,
		validationVerdict(report, active, needsIdentityReview, conclusiveVeto.Blocks()), report)
	switch {
	case report.Structural.Encrypted || active:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, "encrypted_or_active_content")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
		if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "unsafe_pdf", "PDF is encrypted or contains active/embedded content", job.Access(false, ""),
			job.WithHumanActionBinding(job.HumanActionBinding{
				CandidateID: stored.ID, QuarantinePath: result.TempPath, QuarantineSHA256: result.SHA256,
			}),
		); err != nil {
			return false, false, err
		}
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "encrypted_or_active_content"})
	case !report.Payload.OK || !report.Structural.Valid:
		_ = s.Jobs.FinishAttempt(ctx, attempt, "invalid", 0, "payload_or_structure_rejected")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "invalid")
		_ = os.Remove(result.TempPath)
		return false, false, s.Jobs.Transition(ctx, row.ID, job.StateValidating, job.StateFetching,
			map[string]any{"reason": "invalid_pdf"})
	case conclusiveVeto.Blocks() && !stored.ReviewOverride:
		// ReviewOverride still wins: it is set only by an explicit human
		// review of the quarantined preview (ADR-0002), and without that
		// escape a legitimately mismatched document (a chapter DOI against a
		// book job) could never be accepted at all. A job selection never
		// sets ReviewOverride, so picks stay gated.
		_ = s.Jobs.FinishAttempt(ctx, attempt, "needs_review", 0, "conclusive_doi_mismatch")
		_ = s.Jobs.MarkCandidate(ctx, stored.ID, "skipped")
		detail := fmt.Sprintf("Document front matter DOI %s does not match this job; local quarantine file: %s — %s",
			strings.Join(conclusiveVeto.DOIs, ", "), result.TempPath, strings.Join(conclusiveVeto.Evidence, "; "))
		if _, err := s.Jobs.OpenHumanAction(ctx, row.ID, "verify_identity",
			detail,
			job.Access(false, ""),
			job.WithHumanActionBinding(job.HumanActionBinding{
				CandidateID: stored.ID, QuarantinePath: result.TempPath, QuarantineSHA256: result.SHA256,
			}),
		); err != nil {
			return false, false, err
		}
		return false, true, s.park(ctx, row.ID, job.StateValidating, job.StateNeedsReview,
			map[string]any{"reason": "conclusive_doi_mismatch"})
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
	s.recordStandaloneOutcome(ctx, row)
	s.autoImportReady(ctx, row)
	s.runReadyHook(ctx, row, result.SHA256)
	return true, false, nil
}

// recordStandaloneOutcome emits a terminal request milestone only when durable
// cohort attribution proves the job is not a member of a browser/CLI batch.
func (s *Service) recordStandaloneOutcome(ctx context.Context, row *job.Row) {
	if s.Notifier == nil || row == nil {
		return
	}
	if current, err := s.Jobs.Get(ctx, row.ID); err == nil {
		row = current
	}
	var members int
	if err := s.Jobs.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM acquisition_batch_members WHERE job_id=?`, row.ID).Scan(&members); err != nil || members != 0 {
		return
	}
	happened := s.Now().UTC()
	event := notify.Event{Kind: "request.outcome", Count: 1}
	switch row.State {
	case job.StateReady:
		event.Message = "Request ready — open the papio inbox"
	case job.StateFailed, job.StateUnavailable, job.StateCancelled:
		event.Message = "Request failed — open the papio inbox"
	default:
		return
	}
	intent := notify.Intent{
		EventKind: "request.outcome", Category: notify.CategoryRequestOutcome,
		AggregateKey: "job:" + row.ID, Phase: notify.PhaseTerminal,
		WindowStart: happened, HappenedAt: happened, JobID: row.ID,
		Message: event.Message, Detail: event,
	}
	if err := s.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
		log.Printf("papio: routing request outcome for job %s: %v", row.ID, err)
	}
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
		message := zotio.SanitizeErrorHint(err.Error())
		if message != "" {
			detail["error_message"] = message
			if hint == "" {
				detail["error_hint"] = message
			}
		}
		logged := message
		if logged == "" {
			logged = hint
		}
		log.Printf("papio: auto-import for job %s failed [%s]: %s", row.ID, class, logged)
		_ = s.Jobs.RecordEvent(eventCtx, row.ID, "zotio.auto_import", detail)
		return
	}
	detail["status"] = status
	if status == "duplicate" {
		detail["reason"] = "already_in_library"
	}
	_ = s.Jobs.RecordEvent(eventCtx, row.ID, "zotio.auto_import", detail)
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

// InstitutionalBookOpenURLHandoffDetail explains the deliberately human-
// assisted ISBN route. Before this action is opened, the job policy is
// durably narrowed to assisted; the detail itself remains the existing
// HumanAction wire payload and is not used as an access-mode signal.
const InstitutionalBookOpenURLHandoffDetail = "institutional OpenURL handoff: sign in to your institution first, then run 'papio actions open' — this ISBN route can locate a catalogue or ebook record, but papio cannot automatically fetch or validate a book PDF; if you obtain a file, papio can adopt it; if the provider reports a stale or expired session, re-run 'papio actions open'"

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
	// A typed local-budget refusal carries WHY in a closed vocabulary papio
	// defines, and the type alone cannot express it: a job parked because it
	// took its share of the day and a job parked because the whole day is
	// spent are both *budget.ErrExceeded, and an operator reading the attempt
	// row has no way to tell a fair-share wait from an exhausted allowance.
	// Kind and Window are enums with fixed strings, so recording them cannot
	// leak upstream text.
	var exceeded *budget.ErrExceeded
	if errors.As(err, &exceeded) {
		return fmt.Sprintf("%T(%s/%s)", exceeded, exceeded.Kind, exceeded.Window)
	}
	// The same problem, one budget layer up, and it is the more expensive one
	// to lack: 87,471 of 241,093 attempt rows on the operator's own store are
	// `*budget.ErrDeferred` with nothing to say whether papio's own token
	// bucket declined to make the request or the provider gated us. Those are
	// opposite diagnoses — one is a local pacing choice the operator can
	// change, the other is the provider's answer — and telling them apart
	// required knowing out of band that OpenAIRE has never once returned 429.
	var deferred *budget.ErrDeferred
	if errors.As(err, &deferred) {
		if deferred.Advisory {
			return fmt.Sprintf("%T(self_paced)", deferred)
		}
		return fmt.Sprintf("%T(source_gate)", deferred)
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
	// retryKindAdvisory marks a pass that made no request because this
	// process's own token bucket turned every callable source away. Like a
	// closed source gate it consumes no attempt — charging it would settle a
	// job "temporary source failures did not clear" about sources papio never
	// called — but it stays distinct so the two are legible apart in the log.
	// Liveness comes from the RetryDelay floor in parkForRetry, not from the
	// retry budget: a self-inflicted throttle has always refilled by the next
	// ordinary retry, so the following pass makes real requests.
	retryKindAdvisory = "advisory"
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
	Gate               time.Time // a durable source gate was closed; no request was made
	// LatestGate is the LATEST durable source gate observed this pass (Gate is
	// the earliest, used for scheduling At()). When the retry budget is spent,
	// the job gets exactly one more wait (parkForRetry, retryKindExhaustedGate)
	// — that wait, and the decision to grant it at all, must be driven by
	// whichever gated source has the longest reset, not the shortest.
	LatestGate time.Time
	// Advisory is this process's own token bucket turning a request away.
	// It is deliberately NOT a wake time: it is at most budget.MaxInlineWait
	// out, so scheduling on it wakes the job seconds later to re-run every
	// source and learn nothing. Under cohort load one rate-limited source
	// produced a two-event-per-cycle spin at ~5s — 10,437 durable transitions
	// in 97 minutes — while the source that actually blocked those jobs sat
	// behind a real 24-hour quota gate the whole time.
	Advisory time.Time

	RetryableCandidates int // candidate fetches that failed retryably this pass
	TemporaryResolvers  int // resolver/sibling calls that failed retryably this pass
	ClosedSourceGates   int // durable source gates closed before any request this pass
	AdvisoryBackoffs    int // token-bucket refusals before any request this pass
	// StickyBudgetGate marks a local budget refusal with no timed reopen
	// (pricing-drift / prepaid closure). Gate and LatestGate stay zero so the
	// job never wakes at a UTC boundary while egress remains closed; parkForRetry
	// still schedules RetryDelay so the scheduler has a wake time, and
	// StickyBudgetGate blocks sibling search like a durable gate.
	StickyBudgetGate bool
	// SourcesCalled counts sources this pass actually reached. A pass that
	// called something and came back empty is a real answer and must stay
	// chargeable; only a pass where this process's own throttle turned away
	// every callable source made no request at all.
	SourcesCalled int
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
	p.LatestGate = laterTime(p.LatestGate, other.LatestGate)
	p.Advisory = earlierTime(p.Advisory, other.Advisory)
	p.RetryableCandidates += other.RetryableCandidates
	p.TemporaryResolvers += other.TemporaryResolvers
	p.ClosedSourceGates += other.ClosedSourceGates
	p.AdvisoryBackoffs += other.AdvisoryBackoffs
	p.StickyBudgetGate = p.StickyBudgetGate || other.StickyBudgetGate
	p.SourcesCalled += other.SourcesCalled
}

// recordDeferral folds one refused source into the plan, keeping a durable
// gate (a real next_allowed_at this process cannot shorten) apart from this
// process's own token-bucket backoff.
func (p *retryPlan) recordDeferral(deferred *budget.ErrDeferred) {
	if deferred == nil {
		return
	}
	if deferred.Advisory {
		p.Advisory = earlierTime(p.Advisory, deferred.Until)
		p.AdvisoryBackoffs++
		return
	}
	p.Gate = earlierTime(p.Gate, deferred.Until)
	p.LatestGate = laterTime(p.LatestGate, deferred.Until)
	p.ClosedSourceGates++
}

// recordExceeded folds a typed local-budget refusal into the plan as a durable
// park. Timed windows (UTC day, calendar month) carry the reset from the
// refusal; WindowSticky has no reopen instant — Gate stays zero so UTC rollover
// cannot revive the job.
func (p *retryPlan) recordExceeded(exceeded *budget.ErrExceeded) {
	if exceeded == nil {
		return
	}
	if exceeded.Window == budget.WindowSticky {
		p.StickyBudgetGate = true
		p.ClosedSourceGates++
		return
	}
	until := exceeded.Until.UTC()
	if until.IsZero() {
		return
	}
	p.Gate = earlierTime(p.Gate, until)
	p.LatestGate = laterTime(p.LatestGate, until)
	p.ClosedSourceGates++
}

// absorbBudgetRefusal records a budget admission refusal on the plan. Returns
// true when err was a typed local-budget refusal the pass must park on.
func absorbBudgetRefusal(plan *retryPlan, err error) bool {
	var exceeded *budget.ErrExceeded
	if errors.As(err, &exceeded) {
		plan.recordExceeded(exceeded)
		return true
	}
	var deferred *budget.ErrDeferred
	if errors.As(err, &deferred) {
		plan.recordDeferral(deferred)
		return true
	}
	return false
}

// At is when the job should wake: the earliest real opportunity, because a
// source that frees up sooner deserves its attempt sooner. An advisory
// token-bucket backoff is never that opportunity — it says only that this
// process throttled itself, so it cannot outrank a durable gate or a real
// retryable failure, and on its own it yields the caller's ordinary retry
// cadence rather than a sub-second wake.
func (p retryPlan) At() time.Time { return earlierTime(p.Temporary(), p.Gate) }

// AdvisoryOnly reports a pass that made no request and observed nothing but
// this process's own throttle. There is no honest wake time to schedule, so
// the caller supplies its ordinary retry cadence.
func (p retryPlan) AdvisoryOnly() bool {
	return p.At().IsZero() && p.AdvisoryBackoffs > 0 && p.SourcesCalled == 0
}

// IsZero means the pass observed nothing at all and the job can be settled.
// Any throttle refusal keeps it non-zero — a source papio never asked cannot
// justify a terminal verdict — but only an advisory-ONLY pass is uncharged.
func (p retryPlan) IsZero() bool {
	return p.At().IsZero() && !p.StickyBudgetGate && p.AdvisoryBackoffs == 0
}

// Kind names why the pass ended with no verdict. source_gate means a durable
// gate held every callable source back and NOTHING was called this pass;
// advisory means only this process's own throttle did. Neither made a request,
// so neither is charged against the retry budget; they stay distinct so the
// durable log says which one it was. A pass that reached at least one source is
// chargeable even if another source was also gated: a job whose candidates are
// all permanently dead otherwise re-runs the whole resolver chain forever,
// spending real provider credits on every uncharged cycle, because some
// unrelated source happened to be gated in the same pass.
func (p retryPlan) Kind() string {
	if p.SourcesCalled > 0 {
		return retryKindTemporary
	}
	if !p.Temporary().IsZero() {
		return retryKindTemporary
	}
	if !p.Gate.IsZero() || p.ClosedSourceGates > 0 {
		return retryKindSourceGate
	}
	if p.AdvisoryOnly() {
		return retryKindAdvisory
	}
	return retryKindTemporary
}

// GatePending reports a durable source gate that has not yet elapsed. At the
// retry exhaustion boundary this outranks a terminal verdict: the bounded
// attempts are spent, but the gated source still deserves the one call it
// never got. It reads LatestGate, not Gate: when several sources are gated for
// different durations, the one wait past exhaustion must be long enough for
// the slowest of them, or that source still never gets its call. An advisory
// throttle is deliberately excluded — it is this process's own backoff, not a
// source withholding access.
func (p retryPlan) GatePending(now time.Time) bool {
	return !p.LatestGate.IsZero() && p.LatestGate.After(now)
}

func earlierTime(current, candidate time.Time) time.Time {
	if current.IsZero() || (!candidate.IsZero() && candidate.Before(current)) {
		return candidate
	}
	return current
}

func laterTime(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.After(current) {
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
