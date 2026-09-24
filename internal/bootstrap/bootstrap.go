// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package bootstrap wires the production acquisition core. Domain packages keep
// injected interfaces; only this package chooses concrete network, storage,
// resolver, validation, and scheduler implementations.
package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"papio/internal/acquisitionagent"
	"papio/internal/agentcredential"
	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/batch"
	"papio/internal/browser"
	"papio/internal/budget"
	"papio/internal/bundle"
	"papio/internal/captures"
	"papio/internal/config"
	"papio/internal/credential"
	"papio/internal/daemon"
	"papio/internal/delivery"
	"papio/internal/discovery"
	"papio/internal/doctor"
	"papio/internal/doiregistry"
	"papio/internal/drive"
	"papio/internal/enrich"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/landingmeta"
	"papio/internal/notify"
	"papio/internal/ownership"
	"papio/internal/ownershipsnapshot"
	"papio/internal/pdf"
	"papio/internal/preview"
	"papio/internal/pulse"
	"papio/internal/resolver"
	"papio/internal/resolvers/arxiv"
	coreresolver "papio/internal/resolvers/core"
	"papio/internal/resolvers/crossreftdm"
	"papio/internal/resolvers/europepmc"
	"papio/internal/resolvers/openaire"
	"papio/internal/resolvers/openalex"
	"papio/internal/resolvers/semanticscholar"
	"papio/internal/resolvers/unpaywall"
	"papio/internal/retraction"
	"papio/internal/runtimecredential"
	"papio/internal/sourcegate"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/update"
	"papio/internal/watch"
	"papio/internal/work"
	"papio/internal/zotio"
	"strings"
	"sync"
	"time"
)

// System owns the process-wide concrete services used by the daemon and RPC
// handlers. Closing it closes the single SQLite connection.
type System struct {
	Config        config.Config
	Credentials   *runtimecredential.Runtime
	Store         *store.Store
	Jobs          *job.Store
	Artifacts     *artifact.Store
	Captures      *captures.Store
	Budgets       *budget.Manager
	App           *app.Service
	Notify        *notify.Router
	Pulse         *pulse.Service
	Scheduler     *daemon.Scheduler
	Bundle        *bundle.Exporter
	Browser       *browser.Bridge
	Preview       *preview.Server
	PDFCapability pdf.Capability
	WorkerBinary  string
	Discovery     discovery.Source
	Watches       *watch.Store
	WatchRunner   *watch.Runner
	Zotio         *zotio.Service
	// Holdings answers ownership for users without zotio; empty when zotio owns
	// the answer or no generic source is configured.
	Holdings    *ownership.Registry
	Updates     *update.Checker
	Retractions *retraction.Sentinel
	Triage      *triage.Service
	// Drive is the paced drive; it runs as a maintenance runner and serves
	// drive.status / drive.pause / drive.resume.
	Drive *drive.Pacer
}

const autoImportRetryBackoff = 2 * time.Second

// autoImportMinInterval spaces successive imports so papio does not drive a
// human-facing desktop application at machine speed.
//
// Measured 2026-08-22 on the operator's machine: an import-backfill run of ~78
// consecutive applies through `zotio import apply --via connector` left Zotero
// holding 44+ stacked windows titled "Progress" — one leaked per invocation —
// and blocked its main thread. It sat at 0.1% CPU and could answer neither an
// accessibility query nor a screen capture, which is what distinguishes a
// blocked app from a busy one. Roughly half the windows had been reclaimed by
// the end of the run, so Zotero does shed them; papio was simply issuing work
// faster than it could.
//
// This value is deliberately conservative and is NOT a measured optimum. The
// applies in that run already took ~3.5s each and that spacing was still too
// fast, so the floor has to sit well above it; the dismissal rate itself was
// never measured. The window lifecycle belongs to zotio/Zotero and is reported
// upstream — until it is bounded there, papio's own guard is to stay slower
// than the app can shed surfaces. Lower this only with a measurement of that
// rate, not by timing a run that happens not to wedge.
const autoImportMinInterval = 10 * time.Second

// serialAutoImporter prevents concurrent mutations through a single zotio
// mirror, and paces successive imports. The exports ledger makes the one retry
// safe to replay.
type serialAutoImporter struct {
	importer    app.AutoImporter
	mu          sync.Mutex
	backoff     time.Duration
	minInterval time.Duration
	// lastApplyAt guards the interval; it is read and written only under mu.
	lastApplyAt time.Time
}

func newSerialAutoImporter(importer app.AutoImporter, minInterval time.Duration) *serialAutoImporter {
	return &serialAutoImporter{importer: importer, backoff: autoImportRetryBackoff, minInterval: minInterval}
}

// applyPaced runs one import, waiting out any remaining interval first. The
// caller holds mu, so the wait also holds the serialization slot: a queued
// sibling must not slip in and issue the very work this pause exists to space.
func (s *serialAutoImporter) applyPaced(ctx context.Context, jobID string) (status, parentKey, attachmentKey string, err error) {
	if !s.lastApplyAt.IsZero() && s.minInterval > 0 {
		if remaining := s.minInterval - time.Since(s.lastApplyAt); remaining > 0 {
			if err := waitAutoImportRetry(ctx, remaining); err != nil {
				return "failed", "", "", err
			}
		}
	}
	// Stamped on return, not on entry: the interval is a gap between one apply
	// finishing and the next starting, because the leaked surface appears while
	// the apply runs.
	defer func() { s.lastApplyAt = time.Now() }()
	return s.importer.PlanAndApply(ctx, jobID)
}

func (s *serialAutoImporter) PlanAndApply(ctx context.Context, jobID string) (status, parentKey, attachmentKey string, err error) {
	s.mu.Lock()
	status, parentKey, attachmentKey, err = s.applyPaced(ctx, jobID)
	s.mu.Unlock()
	if err == nil {
		return status, parentKey, attachmentKey, nil
	}
	if err := ctx.Err(); err != nil {
		return status, parentKey, attachmentKey, zotio.WithErrorInfo(err)
	}
	if err := waitAutoImportRetry(ctx, s.backoff); err != nil {
		return "failed", "", "", zotio.WithErrorInfo(err)
	}
	if err := ctx.Err(); err != nil {
		return "failed", "", "", zotio.WithErrorInfo(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "failed", "", "", zotio.WithErrorInfo(err)
	}
	status, parentKey, attachmentKey, err = s.applyPaced(ctx, jobID)
	if err != nil && ctx.Err() != nil {
		return "failed", "", "", zotio.WithErrorInfo(ctx.Err())
	}
	return status, parentKey, attachmentKey, zotio.WithErrorInfo(err)
}

// citationEnrichingImporter fills a job's missing citation identity before the
// import runs. It wraps OUTSIDE the pacing serializer on purpose: the lookup is
// a network request, and holding the one import slot across it would make the
// pacing interval pay for enrichment latency it does not own.
//
// See app.Service.EnsureCitationMetadata for why this sits on the shared
// importer seam rather than inside the retry pass.
type citationEnrichingImporter struct {
	next app.AutoImporter
	svc  *app.Service
}

func (c citationEnrichingImporter) PlanAndApply(ctx context.Context, jobID string) (status, parentKey, attachmentKey string, err error) {
	c.svc.EnsureCitationMetadata(ctx, jobID)
	return c.next.PlanAndApply(ctx, jobID)
}

// EnsureCitationMetadata satisfies zotio's optional citationRepairer, which the
// import backfill uses to close a metadata gap BEFORE it classifies a job as an
// expected failure and skips the importer entirely.
func (c citationEnrichingImporter) EnsureCitationMetadata(ctx context.Context, jobID string) {
	c.svc.EnsureCitationMetadata(ctx, jobID)
}

func waitAutoImportRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// New builds one production system without starting background goroutines.
// New constructs the production system with the development version marker.
// Daemon startup passes its build version through NewWithVersion.
func New(ctx context.Context, cfg config.Config) (*System, error) {
	return NewWithVersion(ctx, cfg, "0.1.0-dev")
}

func NewWithVersion(ctx context.Context, cfg config.Config, version string) (*System, error) {
	env, err := runtimecredential.TakeEnvironment(cfg)
	if err != nil {
		return nil, err
	}
	credentials := runtimecredential.Resolve(ctx, cfg, credential.NewStore(), agentcredential.NewStore().Load, env)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var agentBackend acquisitionagent.Backend
	if key := credentials.AgentKey(); key != "" {
		agentBackend, _ = acquisitionagent.NewTypeSafe(key, nil)
	}
	for _, status := range credentials.Statuses() {
		if status.State == "missing" || status.State == "unavailable" || status.State == "invalid" {
			_, _ = fmt.Fprintf(os.Stderr, "papio: credential for %s is %s; check papio config credentials status and restart after fixing setup\n", status.Target, status.State)
		}
	}
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = db.Close()
		}
	}()
	jobs := &job.Store{S: db}
	if err := jobs.ImportLegacyStartedEpochs(ctx); err != nil {
		return nil, fmt.Errorf("importing legacy browser effects: %w", err)
	}
	artifacts, err := artifact.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	captureStore := captures.New(cfg.DataDir, captures.Retention{
		MaxPerHost: cfg.Captures.MaxPerHost,
		MaxAge:     time.Duration(cfg.Captures.MaxAgeDays) * 24 * time.Hour,
	})
	budgets := budget.New(db,
		budget.WithCreditPolicy(budget.CreditPolicyFromSource(credentials.SourcePolicy)),
		// Without this the per-job credit share never binds: an unspent
		// allowance cannot be carried forward, so deferring a job when
		// nothing else is waiting would cost throughput and buy nothing.
		budget.WithContentionProbe(jobs.OtherWorkWaiting),
	)

	artifactPolicy := fetch.DefaultPolicy()
	artifactPolicy.MaxBytes = cfg.Fetch.MaxBytes
	artifactPolicy.Timeout = cfg.FetchTimeout()
	artifactPolicy.AllowHTTPLoopback = cfg.Fetch.AllowHTTPLoopback
	artifactPolicy.UserAgent = "papio/0.1"
	downloader, err := fetch.New(artifactPolicy, nil, nil)
	if err != nil {
		return nil, err
	}
	metadataPolicy := artifactPolicy
	metadataPolicy.MaxBytes = 8 << 20
	metadataPolicy.MaxRedirects = 3
	metadataClient, err := fetch.NewSecureHTTPClient(metadataPolicy, nil, fetch.MetadataTransport(metadataDisableKeepAlives(cfg, credentials)))
	if err != nil {
		return nil, err
	}

	entries := resolverEntries(cfg, budgets, metadataClient, credentials)
	service := app.New(cfg, jobs, artifacts, budgets)
	service.Credentials = credentials
	// ILLiad is the only POST caller. It uses the same policy as metadata:
	// submission responses need no distinct timeout or byte bound today, while
	// the separate constructor keeps metadata and discovery GET-only.
	illiadClient, err := fetch.NewSecureHTTPClientWithPOST(metadataPolicy, nil, http.DefaultTransport)
	if err != nil {
		return nil, err
	}
	service.Delivery = delivery.New(db, &cfg, nil)
	service.Delivery.Credentials = credentials
	service.IlliadHTTPClient = illiadClient
	discoveryBackends, err := discoverySources(cfg, budgets, metadataClient, credentials)
	if err != nil {
		return nil, err
	}
	discoveryClient := discovery.NewMulti(discoveryBackends...)
	for _, backend := range discoveryBackends {
		if lookup, ok := backend.(app.WorkLookup); ok {
			service.Discovery = lookup
			break
		}
	}
	// Not a configurable source: the Handle System is the DOI system's own
	// existence oracle, carries no quota or credential, and is consulted only
	// at the no-candidates handoff boundary.
	service.DOIRegistry = doiregistry.New(doiregistry.Options{Client: metadataClient, ContactEmail: cfg.Email, Version: version})
	// Not a second HTTP client: landingmeta only ever reads a page the app
	// layer already decided to fetch (an OA candidate's own Landing URL), so
	// it shares metadataClient's SSRF guard, redirect cap and body bound
	service.LandingReader = landingmeta.NewReader(metadataClient, metadataPolicy.MaxBytes)
	policy, err := notify.ResolvePolicy(cfg.Notify)
	if err != nil {
		return nil, err
	}
	var desktop notify.Sender
	if cfg.Notify.Enabled {
		desktop, _ = notify.NewPlatformSender()
	}
	var webhook notify.Sender
	if endpoint, bearer := credentials.Webhook(); endpoint != "" {
		webhook = notify.NewWebhook(endpoint, bearer)
	}
	revalidate := func(ctx context.Context, row notify.Record) (bool, error) {
		dbh := db.DB()
		aggregate := row.Intent.AggregateKey
		if strings.HasPrefix(aggregate, "gate:") {
			var count int
			err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM human_actions WHERE status='open' AND blocked_by=?`, strings.TrimPrefix(aggregate, "gate:")).Scan(&count)
			if err != nil {
				// An unreadable aggregate is not evidence of resolution. Fail
				// toward sending a possibly stale notice rather than dropping a
				// real user turn.
				return true, nil
			}
			return count > 0, nil
		}
		if strings.HasPrefix(aggregate, "action:") {
			var status string
			err := dbh.QueryRowContext(ctx, `SELECT status FROM human_actions WHERE id=?`, strings.TrimPrefix(aggregate, "action:")).Scan(&status)
			if err != nil {
				return true, nil
			}
			return status == "open", nil
		}
		if strings.HasPrefix(aggregate, "drive:paused:") {
			// The paced drive's sign-in notice is stale once the drive has
			// resumed; a notice held through quiet hours must not arrive
			// after the person already signed in.
			paused, err := drive.Paused(ctx, jobs)
			if err != nil {
				return true, nil
			}
			return paused, nil
		}
		if reason, ok := strings.CutPrefix(aggregate, app.ZoteroNoticePrefix); ok {
			// "Open Zotero" held through quiet hours is stale once Zotero
			// accepts papers or no paper waits for this reason any more.
			current, err := service.ZoteroWaitNoticeCurrent(ctx, reason)
			if err != nil {
				return true, nil
			}
			return current, nil
		}
		if strings.HasPrefix(aggregate, "decision:") || strings.HasPrefix(aggregate, "actions:") {
			var count int
			err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM human_actions WHERE status='open'`).Scan(&count)
			if err != nil {
				// See above: lookup failure must not silently suppress work.
				return true, nil
			}
			return count > 0, nil
		}
		if row.Intent.BatchID != "" || strings.HasPrefix(aggregate, "cohort:") {
			key := row.Intent.BatchID
			if key == "" {
				key = strings.TrimPrefix(aggregate, "cohort:")
			}
			var closed sql.NullString
			err := dbh.QueryRowContext(ctx, `SELECT closed_at FROM acquisition_batches WHERE id=? OR cohort_id=? LIMIT 1`, key, key).Scan(&closed)
			if err != nil {
				// Missing/unreadable cohort metadata is conservatively treated as
				// still unsettled, preserving a potentially real milestone.
				return true, nil
			}
			return !closed.Valid || closed.String == "", nil
		}
		return true, nil
	}
	router := notify.NewRouter(notify.RouterOptions{
		Ledger: notify.NewStoreLedger(db), Desktop: desktop, Webhook: webhook,
		Policy: policy, Activity: db, Revalidate: revalidate,
	})
	service.Notifier = router
	service.Resolvers = entries
	if credentials.SourcePolicy(config.SourceCrossrefMetadata).Enabled {
		crossrefEnricher := enrich.NewWithOptions(enrich.Options{
			Client: metadataClient, ContactEmail: cfg.Email,
			BaseURL: cfg.Sources[config.SourceCrossrefMetadata].BaseURLForDev,
		})
		service.Enricher = crossrefEnricher
		service.MetadataEnrichers = append(service.MetadataEnrichers, app.MetadataEnricherEntry{
			Name: config.SourceCrossrefMetadata, Enricher: crossrefEnricher,
		})
	}
	if credentials.SourcePolicy(config.SourceOpenAlex).Enabled {
		openAlexEnricher := enrich.NewOpenAlexWithOptions(enrich.OpenAlexOptions{
			// Same daily budget as the resolver and discovery paths, so the
			// enricher's own responses must feed the header-derived floor too.
			Client:       mustOpenAlexClient(budgets, cfg, metadataClient, credentials),
			ContactEmail: cfg.Email,
			APIKey:       credentials.SourcePolicy(config.SourceOpenAlex).APIKey,
			BaseURL:      cfg.Sources[config.SourceOpenAlex].BaseURLForDev,
		})
		service.MetadataEnrichers = append(service.MetadataEnrichers, app.MetadataEnricherEntry{
			Name: config.SourceOpenAlex, Enricher: openAlexEnricher,
		})
	}
	service.Fetch = func(ctx context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
		return downloader.DownloadWithHeaders(ctx, candidate.URL, candidate.RequestHeaders, path)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	capability := pdf.DetectCapability()
	if !cfg.PDF.OCREnabled {
		capability.PDFToPPM = ""
		capability.Tesseract = ""
	}
	validationOptions := pdf.ValidationOptions{
		Structural:          pdf.DefaultStructuralOptions(),
		Semantic:            pdf.DefaultSemanticOptions(),
		TitleMatchThreshold: cfg.PDF.TitleMatchThreshold,
	}
	validationOptions.Semantic.MinChars = cfg.PDF.MinTextChars
	validationOptions.Semantic.OCRPages = cfg.PDF.MaxOCRPages
	service.Validate = func(ctx context.Context, path, declaredMIME string, target work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidateFile(ctx, pdf.ValidationInput{
			DeclaredMIME: declaredMIME,
			Path:         path,
			WorkerBinary: executable,
			Capability:   capability,
			Target:       target,
		}, validationOptions)
	}
	service.Sanitize = func(ctx context.Context, path, dest string) (pdf.StructuralReport, error) {
		return pdf.SanitizeEmbeddedFiles(ctx, executable, path, dest, validationOptions.Structural)
	}

	bundleExporter := &bundle.Exporter{Jobs: jobs, Artifacts: artifacts, DataDir: cfg.DataDir}
	zotioService := &zotio.Service{
		Submitter: service,
		Bundle:    bundleExporter, Store: db, DataDir: cfg.DataDir,
		AttachmentMode: cfg.Zotio.AttachmentMode, AutoEnrich: cfg.Zotio.AutoEnrich,
		ExceptionTags:      cfg.Zotio.ExceptionTags,
		UnavailableRecheck: time.Duration(cfg.Zotio.UnavailableRecheckDays) * 24 * time.Hour,
	}
	// The budgeted lookup enrichDOIWork uses also gives a new Zotero item the
	// abstract its registry record lacks, before Zotero desktop saves it.
	if lookup := service.Discovery; lookup != nil {
		zotioService.Abstracts = func(ctx context.Context, doi string) (string, error) {
			found, err := lookup.LookupWork(ctx, doi)
			return found.Abstract, err
		}
	}
	holdings := ownership.NewRegistry()
	// browserZotio is the page-bulk status ownership seam (nil when zotio is
	// not configured, mirroring the ADR-0008 exclusivity with holdings below):
	// the bridge must never call an unconfigured zotio service and paint
	// every item ownership_unknown when the generic holdings registry is the
	// intended answer for this daemon.
	var browserZotio *zotio.Service
	if strings.TrimSpace(cfg.Zotio.Executable) != "" {
		// zotio is optional: an empty executable disables the deep Zotero
		// integration (auto-import, plan/apply, queue) while hooks remain the
		// generic hand-off seam.
		zotioClient := zotio.New(cfg.Zotio)
		zotioService.CLI = zotioClient
		browserZotio = zotioService
		service.AutoImporter = citationEnrichingImporter{
			next: newSerialAutoImporter(zotioService, autoImportMinInterval),
			svc:  service,
		}
		// An import that needs the connector waits for Zotero desktop
		// instead of failing while it is closed, when zotio can tell.
		service.ZoteroDesktop = zotioClient
	} else {
		// Generic holdings sources answer ownership only when zotio is absent.
		// Mixing them is deliberately out of scope (ADR-0008): "make this Zotero
		// item complete" and "do I hold a PDF anywhere?" are different questions,
		// and without an explicit lookup purpose there is no correct way to
		// reconcile a zotio parent that lacks a PDF with another library that has
		// one. When zotio is configured, its behaviour is untouched.
		providers := make([]ownership.Provider, 0, len(cfg.Library.Sources))
		for _, source := range cfg.Library.Sources {
			provider, err := ownershipsnapshot.NewProvider(source, time.Now)
			if err != nil {
				// Config validation already rejects unusable sources, so this is a
				// programming error rather than user input; failing startup keeps a
				// half-configured registry from silently answering lookups.
				return nil, fmt.Errorf("library source %q: %w", source.Name, err)
			}
			providers = append(providers, provider)
		}
		holdings = ownership.NewRegistry(providers...)
	}
	watches := watch.NewStore(db)
	service.ReadyHook = &hook.Runner{
		Command: cfg.Hooks.OnReady,
		Timeout: time.Duration(cfg.Hooks.TimeoutSeconds) * time.Second,
	}
	watchRunner := &watch.Runner{
		Store: watches, Discovery: discoveryClient, Lookup: zotioService, Submitter: service,
		Backfill: zotioService, Notifier: router, DataDir: cfg.DataDir,
		Holdings: holdings,
	}
	var retractions *retraction.Sentinel
	if policy := credentials.SourcePolicy(config.SourceRetractionWatch); policy.Enabled {
		retractionHTTPPolicy := metadataPolicy
		retractionHTTPPolicy.MaxBytes = retraction.DefaultMaxResponseBytes
		retractionClient, err := fetch.NewSecureHTTPClientNoRedirect(
			retractionHTTPPolicy, nil, fetch.MetadataTransport(policy.DisableKeepAlives()))
		if err != nil {
			return nil, err
		}
		var retractionZotio *zotio.Client
		if strings.TrimSpace(cfg.Zotio.Executable) != "" {
			retractionZotio = zotio.New(cfg.Zotio)
		}
		libraryDOIs := retraction.NewLibraryCatalog(cfg.Library.Sources, retractionZotio)
		retractions = retraction.New(retraction.Options{
			Store: db, Budgets: budgets, Policy: policy, Client: retractionClient,
			DataDir: cfg.DataDir, BaseURL: policy.BaseURLForDev, ContactEmail: cfg.Email,
			Notifier: router, Scope: cfg.Retraction.Scope, LibraryDOIs: libraryDOIs,
		})
	}
	triageService := triage.New(db, watches, jobs)
	if retractions != nil {
		triageService.RegisterSource(retractions)
	}
	// The paced drive (ADR-0009, amended 2026-09-23). It opens nothing unless
	// [drive] enabled = true; Browser is attached once the bridge exists below.
	pacer := &drive.Pacer{Jobs: jobs, Config: cfg, Notifier: router}
	maintenance := daemon.MaintenanceRunners{watchRunner, service.ImportRetrier(), service.UnavailableRechecker(), service.HandoffRepairer(), service.OfferedDeliveryRecovery(), service.ActionReminder(), pacer, retractions, router}
	if reconciler := zotioService.TagReconciler(); reconciler != nil {
		maintenance = append(maintenance, reconciler)
	}
	// A paused config stops every automatic Zotero write, and the follow-up
	// retries are Zotero writes papio starts on its own.
	if retrier := zotioService.FollowUpRetrier(); retrier != nil && !cfg.Zotio.AutoImportPaused {
		maintenance = append(maintenance, retrier)
	}
	var background []daemon.BackgroundRunner
	if watcher := service.ZoteroDesktopWatcher(); watcher != nil {
		background = append(background, watcher)
	}
	scheduler, err := daemon.NewScheduler(jobs, service, daemon.SchedulerConfig{
		Owner:               job.NewID("daemon"),
		Workers:             3,
		LeaseDuration:       60 * time.Second,
		HeartbeatInterval:   15 * time.Second,
		PollInterval:        250 * time.Millisecond,
		Maintenance:         maintenance,
		MaintenanceInterval: time.Minute,
		Background:          background,
	})
	if err != nil {
		return nil, err
	}
	var updates *update.Checker
	if cfg.Updates.Check {
		updates = update.New(cfg.DataDir)
	}

	previewServer := preview.New(jobs)
	bridge := browser.NewBridge(jobs, service, triageService, watchRunner, previewServer, captureStore, holdings, browserZotio, cfg, version)
	// Only a resolved, explicitly configured backend reaches the browser.
	// Credential values remain in the daemon; they never enter browser IPC.
	bridge.SetAcquisitionBackend(agentBackend)
	router.SetPresence(bridge.PresenceProvider())
	pacer.Browser = bridge

	pulseService := &pulse.Service{
		Jobs: jobs, Cohorts: batch.New(db), EffectLimit: 1, Now: time.Now,
	}
	bridge.SetPulseService(pulseService)
	system := &System{
		Config: cfg, Credentials: credentials, Store: db, Jobs: jobs, Artifacts: artifacts, Captures: captureStore, Budgets: budgets,
		App: service, Notify: router, Pulse: pulseService, Scheduler: scheduler, Watches: watches, WatchRunner: watchRunner,
		Bundle:        bundleExporter,
		Browser:       bridge,
		Drive:         pacer,
		Preview:       previewServer,
		Discovery:     discoveryClient,
		Zotio:         zotioService,
		Holdings:      holdings,
		Updates:       updates,
		Retractions:   retractions,
		Triage:        triageService,
		PDFCapability: capability, WorkerBinary: executable,
	}
	failed = false
	return system, nil
}

// Read once before bootstrap can launch PDF workers, hooks or integrations.
// Those children have no reason to inherit the acquisition inference key.
func takeAcquisitionBackend(ctx context.Context, cfg config.Config) (acquisitionagent.Backend, error) {
	return takeAcquisitionBackendWithStore(ctx, cfg, agentcredential.NewStore().Load)
}

func takeAcquisitionBackendWithStore(ctx context.Context, cfg config.Config, load func(context.Context, string) (string, error)) (acquisitionagent.Backend, error) {
	env, err := runtimecredential.TakeEnvironment(cfg)
	if err != nil {
		return nil, err
	}
	resolved := runtimecredential.Resolve(ctx, cfg, credential.NewStore(), load, env)
	if key := resolved.AgentKey(); key != "" {
		backend, err := acquisitionagent.NewTypeSafe(key, nil)
		if err != nil {
			return nil, errors.New("invalid TypeSafe key for acquisition decisions")
		}
		return backend, nil
	}
	for _, status := range resolved.Statuses() {
		if status.Target == "agent.typesafe" && status.State != "not_configured" {
			return nil, errors.New("TypeSafe credential is " + status.State + "; check papio config credentials status")
		}
	}
	return nil, nil
}

// hookShutdownGraceCap keeps daemon shutdown inside ordinary service-manager
// deadlines. After this grace period, cancellation leaves five seconds for the
// hook process tree to stop and its small outcome event to reach SQLite.
const (
	hookShutdownGraceCap   = 5 * time.Second
	hookShutdownCancelWait = 5 * time.Second
)

// Close releases the process-wide services and database connection.
func (s *System) Close() error {
	if s == nil {
		return nil
	}
	if s.Browser != nil {
		s.Browser.CloseAcquisitionBackend()
	}
	var previewErr error
	if s.Preview != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		previewErr = s.Preview.Shutdown(ctx)
		cancel()
	}
	var hookErr error
	if s.App != nil {
		grace := time.Duration(s.Config.Hooks.TimeoutSeconds) * time.Second
		if grace <= 0 || grace > hookShutdownGraceCap {
			grace = hookShutdownGraceCap
		}
		drained := s.App.DrainHooks(grace)
		s.App.CancelHooks()
		if !drained && !s.App.DrainHooks(hookShutdownCancelWait) {
			hookErr = errors.New("on_ready hooks did not stop after cancellation; database left open so their outcome records are not lost")
		}
	}
	if hookErr != nil {
		return errors.Join(previewErr, hookErr)
	}
	if s.Store == nil {
		return previewErr
	}
	return errors.Join(previewErr, s.Store.Close())
}

// DoctorReport runs readiness checks against this live system without exposing
// credentials or opening a second database connection.
func (s *System) DoctorReport(ctx context.Context) doctor.Report {
	if s.Credentials == nil {
		return doctor.Run(ctx, s.Config, s.Store, s.PDFCapability, s.WorkerBinary, s.Discovery)
	}
	return doctor.Run(ctx, s.Config, s.Store, s.PDFCapability, s.WorkerBinary, s.Discovery, s.Credentials)
}

// metadataDisableKeepAlives aggregates every configured source's keep-alive
// policy for the one shared metadata HTTP client. Any source that opts into
// reuse disables keep-alive suppression for the shared transport.
func metadataDisableKeepAlives(cfg config.Config, views ...sourcePolicies) bool {
	source := selectSourcePolicies(cfg, views)
	for name := range cfg.Sources {
		if !source.SourcePolicy(name).DisableKeepAlives() {
			return false
		}
	}
	return true
}

// mustOpenAlexClient is the OpenAlex metadata HTTP stack: replay-bounded
// transport, no in-client redirect following, and quota-header observation.
// Admission still happens at app.go AcquireAny sites — not in sourcegate.Client.
func mustOpenAlexClient(budgets *budget.Manager, cfg config.Config, _ *fetch.SecureHTTPClient, views ...sourcePolicies) sourcegate.HTTPClient {
	source := selectSourcePolicies(cfg, views)
	policy := source.SourcePolicy(config.SourceOpenAlex)
	transport := fetch.MetadataTransport(policy.DisableKeepAlives())
	metaPolicy := fetch.DefaultPolicy()
	metaPolicy.MaxBytes = 8 << 20
	inner, err := fetch.NewSecureHTTPClientNoRedirect(metaPolicy, nil, transport)
	if err != nil {
		panic(err)
	}
	client, err := sourcegate.WrapOpenAlex(budgets, budgets, config.SourceOpenAlex, policy, inner)
	if err != nil {
		panic(err)
	}
	return client
}

// resolverEntries builds the acquisition resolver chain. Every constructor is
// explicit here — the one place that knows how to build one — and the chain's
// order is config's catalog order for RoleAcquisitionResolver, which
// TestResolverChainMatchesCatalogAcquisitionRole pins in both directions: a
// catalog source with no entry here fails, and an entry here with no catalog
// acquisition role fails. Adding a source is therefore one catalog row and one
// constructor, with no name list to keep in step.
//
// The OpenAlex adapter is the one client that reads its own daily-budget
// headers: budgets is needed for the OpenAlex egress stack (GuardedClient →
// Observer). The entry is deliberately NOT wrapped in sourcegate.Client —
// admission already happens at the app.go AcquireAny sites, and a second
// wrapper would reserve twice.
func resolverEntries(cfg config.Config, budgets *budget.Manager, client *fetch.SecureHTTPClient, views ...sourcePolicies) []app.ResolverEntry {
	source := selectSourcePolicies(cfg, views)
	return []app.ResolverEntry{
		{Adapter: arxiv.NewWithOptions(arxiv.Options{Client: client, BaseURL: cfg.Sources[config.SourceArXiv].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceArXiv)},
		{Adapter: europepmc.NewWithOptions(europepmc.Options{Client: client, BaseURL: cfg.Sources[config.SourceEuropePMC].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceEuropePMC)},
		{Adapter: unpaywall.NewWithOptions(unpaywall.Options{Client: client, ContactEmail: cfg.Email, BaseURL: cfg.Sources[config.SourceUnpaywall].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceUnpaywall)},
		{Adapter: openalex.NewWithOptions(openalex.Options{Client: mustOpenAlexClient(budgets, cfg, client, source), ContactEmail: cfg.Email, APIKey: source.SourcePolicy(config.SourceOpenAlex).APIKey, BaseURL: cfg.Sources[config.SourceOpenAlex].BaseURLForDev, SiblingTitleSearch: cfg.Sources[config.SourceOpenAlex].SiblingTitleSearch}), Policy: source.SourcePolicy(config.SourceOpenAlex)},
		{Adapter: semanticscholar.NewWithOptions(semanticscholar.Options{Client: client, APIKey: source.SourcePolicy(config.SourceSemanticScholar).APIKey, BaseURL: cfg.Sources[config.SourceSemanticScholar].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceSemanticScholar)},
		{Adapter: coreresolver.NewWithOptions(coreresolver.Options{Client: client, APIKey: source.SourcePolicy(config.SourceCORE).APIKey, BaseURL: cfg.Sources[config.SourceCORE].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceCORE)},
		{Adapter: crossreftdm.NewWithOptions(crossreftdm.Options{Client: client, APIKey: source.SourcePolicy(config.SourceCrossrefTDM).APIKey, BaseURL: cfg.Sources[config.SourceCrossrefTDM].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceCrossrefTDM)},
		{Adapter: openaire.NewWithOptions(openaire.Options{Client: client, Tokens: openAIRETokens(cfg, client, source), APIKey: source.SourcePolicy(config.SourceOpenAIRE).APIKey, BaseURL: cfg.Sources[config.SourceOpenAIRE].BaseURLForDev}), Policy: source.SourcePolicy(config.SourceOpenAIRE)},
	}
}

// openAIRETokens selects OpenAIRE's credential path, preferring the only one
// that survives unattended: a registered service's client id and secret, which
// do not expire, exchanged for short-lived access tokens as needed. Returning
// nil leaves the adapter on its api_key fallback (or keyless).
//
// The exchange runs against OpenAIRE's AAI host rather than the Graph API, so
// it is neither metered by the Graph rate ceiling papio paces itself to nor
// admitted through the source's budget gate — one request per token lifetime
// against a different service.
func openAIRETokens(cfg config.Config, client *fetch.SecureHTTPClient, views ...sourcePolicies) openaire.TokenSource {
	source := selectSourcePolicies(cfg, views).SourcePolicy(config.SourceOpenAIRE)
	if !source.HasClientCredentials() {
		return nil
	}
	return openaire.NewClientCredentials(openaire.ClientCredentialsOptions{
		Client:       client,
		ClientID:     source.ClientID,
		ClientSecret: source.ClientSecret,
	})
}

// discoveryPolicy is the budget policy for a discovery backend: the source's
// pacing and spend ceiling, but always enabled.
//
// Selection in discovery.sources IS discovery's enablement. sources.<n>.enabled
// governs the acquisition resolver chain, and the two are independent — the
// shipped default has discovery falling back to OpenAlex while the OpenAlex
// ACQUISITION source is disabled. Feeding that flag to Acquire vetoed every
// discovery request on a default config, which is a backend built and then
// silently refused. Pacing and cost still come from the source because they
// describe the provider, which both callers share.
func discoveryPolicy(cfg config.Config, name string, views ...sourcePolicies) config.Source {
	source := selectSourcePolicies(cfg, views)
	policy := source.SourcePolicy(name)
	policy.Enabled = true
	return policy
}

// discoverySources builds the discovery backends. The switch below stays
// explicit — one constructor per backend — and config's catalog declares which
// sources may appear here: config.validate() refuses a discovery.sources entry
// without RoleDiscoveryBackend, and
// TestDiscoveryBackendsMatchCatalogDiscoveryRole pins the switch against that
// role in both directions.
//
// Each backend is given the shared secure HTTP client wrapped in its source's
// budget gate, so discovery is accounted for, paced and paused exactly like the
// acquisition resolvers that hit the same providers. Left ungated it drew on
// the same provider quota invisibly and ignored a durable gate that had already
// paused acquisition.
func discoverySources(cfg config.Config, budgets *budget.Manager, client sourcegate.HTTPClient, views ...sourcePolicies) ([]discovery.Source, error) {
	source := selectSourcePolicies(cfg, views)
	names := cfg.Discovery.Sources
	if len(names) == 0 {
		names = []string{config.SourceOpenAlex}
	}
	sources := make([]discovery.Source, 0, len(names))
	for _, name := range names {
		// OpenAlex shares its keyed daily budget with the resolver and
		// enrichment paths, so discovery must refuse admission at the same
		// header-derived floor and observe the headers itself. It gains no
		// fallback identity: that stays out of scope for discovery.
		var gated sourcegate.HTTPClient
		var err error
		if name == config.SourceOpenAlex {
			// OpenAlex discovery: wire-derived identity authority at GuardedClient;
			// sourcegate.Client's construction-time policy is removed so it cannot
			// pre-empt the keyed identity CommitEgress derives from the request.
			// Identity-agnostic pacing sits outside the guarded stack.
			stack := mustOpenAlexClient(budgets, cfg, nil, source)
			gated, err = sourcegate.NewPacingOnly(budgets, name, discoveryPolicy(cfg, name, source), 0, stack)
		} else {
			gated, err = sourcegate.New(budgets, name, discoveryPolicy(cfg, name, source), 0, client)
		}
		if err != nil {
			return nil, err
		}
		switch name {
		case config.SourceArXiv:
			sources = append(sources, discovery.NewArxivWithOptions(discovery.ArxivOptions{
				Client:  gated,
				BaseURL: cfg.Sources[config.SourceArXiv].BaseURLForDev,
			}))
		case config.SourceOpenAlex:
			sources = append(sources, discovery.NewWithOptions(discovery.Options{
				Client:       gated,
				ContactEmail: cfg.Email,
				APIKey:       source.SourcePolicy(config.SourceOpenAlex).APIKey,
				BaseURL:      cfg.Sources[config.SourceOpenAlex].BaseURLForDev,
			}))
		case config.SourceSemanticScholar:
			sources = append(sources, discovery.NewSemanticScholarWithOptions(discovery.SemanticScholarOptions{
				Client:  gated,
				APIKey:  source.SourcePolicy(config.SourceSemanticScholar).APIKey,
				BaseURL: cfg.Sources[config.SourceSemanticScholar].BaseURLForDev,
			}))
		}
	}
	return sources, nil
}

// sourcePolicies is shared by raw legacy config and the resolved runtime view.
type sourcePolicies interface{ SourcePolicy(string) config.Source }

func selectSourcePolicies(cfg config.Config, views []sourcePolicies) sourcePolicies {
	if len(views) != 0 && views[0] != nil {
		return views[0]
	}
	return &cfg
}
