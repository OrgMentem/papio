// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package bootstrap wires the production acquisition core. Domain packages keep
// injected interfaces; only this package chooses concrete network, storage,
// resolver, validation, and scheduler implementations.
package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"os"
	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/browser"
	"papio/internal/budget"
	"papio/internal/bundle"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/discovery"
	"papio/internal/doctor"
	"papio/internal/enrich"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/notify"
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
	Updates       *update.Checker
	Retractions   *retraction.Sentinel
	Triage        *triage.Service
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
			Client: metadataClient, BaseURL: cfg.Sources[config.SourceCrossrefMetadata].BaseURLForDev,
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
		return pdf.ValidateBytes(ctx, pdf.ValidationInput{
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
	}
	if strings.TrimSpace(cfg.Zotio.Executable) != "" {
		// zotio is optional: an empty executable disables the deep Zotero
		// integration (auto-import, plan/apply, queue) while ownership lookup
		// degrades to not-owned and hooks remain the generic hand-off seam.
		zotioService.CLI = zotio.New(cfg.Zotio)
		service.AutoImporter = newSerialAutoImporter(zotioService)
	}
	service.ReadyHook = &hook.Runner{
		Command: cfg.Hooks.OnReady,
		Timeout: time.Duration(cfg.Hooks.TimeoutSeconds) * time.Second,
	}
	discoveryClient := discovery.NewMulti(discoverySources(cfg)...)
	watches := watch.NewStore(db)
	watchRunner := &watch.Runner{
		Store: watches, Discovery: discoveryClient, Lookup: zotioService, Submitter: service,
		Backfill: zotioService, Notifier: watchNotifier, DataDir: cfg.DataDir,
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
	maintenance := daemon.MaintenanceRunners{watchRunner, service.ImportRetrier(), retractions}
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
		Config: cfg, Store: db, Jobs: jobs, Artifacts: artifacts, Budgets: budgets,
		App: service, Scheduler: scheduler, Watches: watches, WatchRunner: watchRunner,
		Bundle:        bundleExporter,
		Browser:       browser.NewBridge(jobs, service, triageService, watchRunner, previewServer, cfg, version, nil),
		Preview:       previewServer,
		Discovery:     discoveryClient,
		Zotio:         zotioService,
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
	return doctor.Run(ctx, s.Config, s.Store, s.PDFCapability, s.WorkerBinary)
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

// discoverySources builds the configured discovery backends in merge-preference
// order. An empty selection keeps the historical OpenAlex-only behavior.
func discoverySources(cfg config.Config) []discovery.Source {
	names := cfg.Discovery.Sources
	if len(names) == 0 {
		names = []string{config.SourceOpenAlex}
	}
	sources := make([]discovery.Source, 0, len(names))
	for _, name := range names {
		switch name {
		case config.SourceOpenAlex:
			sources = append(sources, discovery.NewWithOptions(discovery.Options{
				ContactEmail: cfg.Email, BaseURL: cfg.Sources[config.SourceOpenAlex].BaseURLForDev,
			}))
		case config.SourceSemanticScholar:
			sources = append(sources, discovery.NewSemanticScholarWithOptions(discovery.SemanticScholarOptions{
				APIKey:  cfg.Sources[config.SourceSemanticScholar].APIKey,
				BaseURL: cfg.Sources[config.SourceSemanticScholar].BaseURLForDev,
			}))
		}
	}
	return sources
}
