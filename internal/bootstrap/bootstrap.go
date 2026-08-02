// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package bootstrap wires the production acquisition core. Domain packages keep
// injected interfaces; only this package chooses concrete network, storage,
// resolver, validation, and scheduler implementations.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/browser"
	"papio/internal/budget"
	"papio/internal/bundle"
	"papio/internal/captures"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/discovery"
	"papio/internal/doctor"
	"papio/internal/enrich"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/ownership"
	"papio/internal/ownershipsnapshot"
	"papio/internal/pdf"
	"papio/internal/preview"
	"papio/internal/resolver"
	"papio/internal/resolvers/arxiv"
	coreresolver "papio/internal/resolvers/core"
	"papio/internal/resolvers/crossreftdm"
	"papio/internal/resolvers/europepmc"
	"papio/internal/resolvers/openalex"
	"papio/internal/resolvers/unpaywall"
	"papio/internal/retraction"
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
	Store         *store.Store
	Jobs          *job.Store
	Artifacts     *artifact.Store
	Captures      *captures.Store
	Budgets       *budget.Manager
	App           *app.Service
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
}

const autoImportRetryBackoff = 2 * time.Second

// serialAutoImporter prevents concurrent mutations through a single zotio
// mirror. The exports ledger makes the one retry safe to replay.
type serialAutoImporter struct {
	importer app.AutoImporter
	mu       sync.Mutex
	backoff  time.Duration
}

func newSerialAutoImporter(importer app.AutoImporter) *serialAutoImporter {
	return &serialAutoImporter{importer: importer, backoff: autoImportRetryBackoff}
}

func (s *serialAutoImporter) PlanAndApply(ctx context.Context, jobID string) (status, parentKey, attachmentKey string, err error) {
	s.mu.Lock()
	status, parentKey, attachmentKey, err = s.importer.PlanAndApply(ctx, jobID)
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
	status, parentKey, attachmentKey, err = s.importer.PlanAndApply(ctx, jobID)
	if err != nil && ctx.Err() != nil {
		return "failed", "", "", zotio.WithErrorInfo(ctx.Err())
	}
	return status, parentKey, attachmentKey, zotio.WithErrorInfo(err)
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
	artifacts, err := artifact.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	captureStore := captures.New(cfg.DataDir, captures.Retention{
		MaxPerHost: cfg.Captures.MaxPerHost,
		MaxAge:     time.Duration(cfg.Captures.MaxAgeDays) * 24 * time.Hour,
	})
	budgets := budget.New(db)

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
	metadataClient, err := fetch.NewSecureHTTPClient(metadataPolicy, nil, http.DefaultTransport)
	if err != nil {
		return nil, err
	}

	entries := resolverEntries(cfg, metadataClient)
	service := app.New(cfg, jobs, artifacts, budgets)
	discoveryBackends := discoverySources(cfg, budgets, metadataClient)
	discoveryClient := discovery.NewMulti(discoveryBackends...)
	for _, backend := range discoveryBackends {
		if lookup, ok := backend.(app.WorkLookup); ok {
			service.Discovery = lookup
			break
		}
	}
	var senders []notify.Sender
	if cfg.Notify.Enabled {
		senders = append(senders, notify.NewMacOS())
	}
	if cfg.Notify.WebhookURL != "" {
		senders = append(senders, notify.NewWebhook(cfg.Notify.WebhookURL, cfg.Notify.WebhookSecret))
	}
	var watchNotifier notify.Sender
	if len(senders) > 0 {
		watchNotifier = notify.Fanout(senders...)
		service.Notifier = notify.NewCoalescer(watchNotifier)
	}
	service.Resolvers = entries
	if cfg.SourcePolicy(config.SourceCrossrefMetadata).Enabled {
		service.Enricher = enrich.NewWithOptions(enrich.Options{
			Client: metadataClient, ContactEmail: cfg.Email,
			BaseURL: cfg.Sources[config.SourceCrossrefMetadata].BaseURLForDev,
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

	bundleExporter := &bundle.Exporter{Jobs: jobs, Artifacts: artifacts, DataDir: cfg.DataDir}
	zotioService := &zotio.Service{
		Submitter: service,
		Bundle:    bundleExporter, Store: db, DataDir: cfg.DataDir,
		AttachmentMode: cfg.Zotio.AttachmentMode, AutoEnrich: cfg.Zotio.AutoEnrich,
		ExceptionTags:      cfg.Zotio.ExceptionTags,
		UnavailableRecheck: time.Duration(cfg.Zotio.UnavailableRecheckDays) * 24 * time.Hour,
	}
	holdings := ownership.NewRegistry()
	if strings.TrimSpace(cfg.Zotio.Executable) != "" {
		// zotio is optional: an empty executable disables the deep Zotero
		// integration (auto-import, plan/apply, queue) while hooks remain the
		// generic hand-off seam.
		zotioService.CLI = zotio.New(cfg.Zotio)
		service.AutoImporter = newSerialAutoImporter(zotioService)
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
	service.ReadyHook = &hook.Runner{
		Command: cfg.Hooks.OnReady,
		Timeout: time.Duration(cfg.Hooks.TimeoutSeconds) * time.Second,
	}
	watches := watch.NewStore(db)
	watchRunner := &watch.Runner{
		Store: watches, Discovery: discoveryClient, Lookup: zotioService, Submitter: service,
		Backfill: zotioService, Notifier: watchNotifier, DataDir: cfg.DataDir,
		Holdings: holdings,
	}
	var retractions *retraction.Sentinel
	if policy := cfg.SourcePolicy(config.SourceRetractionWatch); policy.Enabled {
		retractions = retraction.New(retraction.Options{
			Store: db, Budgets: budgets, Policy: policy, Client: metadataClient,
			DataDir: cfg.DataDir, BaseURL: policy.BaseURLForDev, Notifier: watchNotifier,
		})
	}
	triageService := triage.New(db, watches, jobs)
	if retractions != nil {
		triageService.RegisterSource(retractions)
	}
	maintenance := daemon.MaintenanceRunners{watchRunner, service.ImportRetrier(), service.HandoffRepairer(), service.ActionReminder(), retractions}
	if reconciler := zotioService.TagReconciler(); reconciler != nil {
		maintenance = append(maintenance, reconciler)
	}
	scheduler, err := daemon.NewScheduler(jobs, service, daemon.SchedulerConfig{
		Owner:               job.NewID("daemon"),
		Workers:             3,
		LeaseDuration:       60 * time.Second,
		HeartbeatInterval:   15 * time.Second,
		PollInterval:        250 * time.Millisecond,
		Maintenance:         maintenance,
		MaintenanceInterval: time.Minute,
	})
	if err != nil {
		return nil, err
	}
	var updates *update.Checker
	if cfg.Updates.Check {
		updates = update.New(cfg.DataDir)
	}

	previewServer := preview.New()

	system := &System{
		Config: cfg, Store: db, Jobs: jobs, Artifacts: artifacts, Captures: captureStore, Budgets: budgets,
		App: service, Scheduler: scheduler, Watches: watches, WatchRunner: watchRunner,
		Bundle:        bundleExporter,
		Browser:       browser.NewBridge(jobs, service, triageService, watchRunner, previewServer, captureStore, cfg, version),
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

// Close releases the process-wide services and database connection.
func (s *System) Close() error {
	if s == nil {
		return nil
	}
	var previewErr error
	if s.Preview != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		previewErr = s.Preview.Shutdown(ctx)
		cancel()
	}
	if s.App != nil {
		// Launched on_ready hooks record their durable outcome event after the
		// command exits; give them a bounded window before SQLite goes away so
		// a normal daemon stop does not lose the audit record.
		s.App.DrainHooks(5 * time.Second)
	}
	if s.Store == nil {
		return previewErr
	}
	return errors.Join(previewErr, s.Store.Close())
}

// DoctorReport runs readiness checks against this live system without exposing
// credentials or opening a second database connection.
func (s *System) DoctorReport(ctx context.Context) doctor.Report {
	return doctor.Run(ctx, s.Config, s.Store, s.PDFCapability, s.WorkerBinary, s.Discovery)
}

func resolverEntries(cfg config.Config, client *fetch.SecureHTTPClient) []app.ResolverEntry {
	return []app.ResolverEntry{
		{Adapter: arxiv.NewWithOptions(arxiv.Options{Client: client, BaseURL: cfg.Sources[config.SourceArXiv].BaseURLForDev}), Policy: cfg.SourcePolicy(config.SourceArXiv)},
		{Adapter: europepmc.NewWithOptions(europepmc.Options{Client: client, BaseURL: cfg.Sources[config.SourceEuropePMC].BaseURLForDev}), Policy: cfg.SourcePolicy(config.SourceEuropePMC)},
		{Adapter: unpaywall.NewWithOptions(unpaywall.Options{Client: client, ContactEmail: cfg.Email, BaseURL: cfg.Sources[config.SourceUnpaywall].BaseURLForDev}), Policy: cfg.SourcePolicy(config.SourceUnpaywall)},
		{Adapter: openalex.NewWithOptions(openalex.Options{Client: client, ContactEmail: cfg.Email, APIKey: cfg.Sources[config.SourceOpenAlex].APIKey, BaseURL: cfg.Sources[config.SourceOpenAlex].BaseURLForDev}), Policy: cfg.SourcePolicy(config.SourceOpenAlex)},
		{Adapter: coreresolver.NewWithOptions(coreresolver.Options{Client: client, APIKey: cfg.Sources[config.SourceCORE].APIKey, BaseURL: cfg.Sources[config.SourceCORE].BaseURLForDev}), Policy: cfg.SourcePolicy(config.SourceCORE)},
		{Adapter: crossreftdm.NewWithOptions(crossreftdm.Options{Client: client, APIKey: cfg.Sources[config.SourceCrossrefTDM].APIKey, BaseURL: cfg.Sources[config.SourceCrossrefTDM].BaseURLForDev}), Policy: cfg.SourcePolicy(config.SourceCrossrefTDM)},
	}
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
func discoveryPolicy(cfg config.Config, name string) config.Source {
	policy := cfg.SourcePolicy(name)
	policy.Enabled = true
	return policy
}

// discoverySources builds the discovery backends. Each is given the shared
// secure HTTP client wrapped in its source's budget gate, so discovery is
// accounted for, paced and paused exactly like the acquisition resolvers that
// hit the same providers. Left ungated it drew on the same provider quota
// invisibly and ignored a durable gate that had already paused acquisition.
func discoverySources(cfg config.Config, budgets *budget.Manager, client sourcegate.HTTPClient) []discovery.Source {
	names := cfg.Discovery.Sources
	if len(names) == 0 {
		names = []string{config.SourceOpenAlex}
	}
	sources := make([]discovery.Source, 0, len(names))
	for _, name := range names {
		gated := sourcegate.New(budgets, name, discoveryPolicy(cfg, name), 0, client)
		switch name {
		case config.SourceOpenAlex:
			sources = append(sources, discovery.NewWithOptions(discovery.Options{
				Client:       gated,
				ContactEmail: cfg.Email,
				APIKey:       cfg.Sources[config.SourceOpenAlex].APIKey,
				BaseURL:      cfg.Sources[config.SourceOpenAlex].BaseURLForDev,
			}))
		case config.SourceSemanticScholar:
			sources = append(sources, discovery.NewSemanticScholarWithOptions(discovery.SemanticScholarOptions{
				Client:  gated,
				APIKey:  cfg.Sources[config.SourceSemanticScholar].APIKey,
				BaseURL: cfg.Sources[config.SourceSemanticScholar].BaseURLForDev,
			}))
		}
	}
	return sources
}
