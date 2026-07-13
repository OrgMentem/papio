// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package bootstrap wires the production acquisition core. Domain packages keep
// injected interfaces; only this package chooses concrete network, storage,
// resolver, validation, and scheduler implementations.
package bootstrap

import (
	"context"
	"net/http"
	"os"
	"time"

	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/browser"
	"papio/internal/budget"
	"papio/internal/bundle"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/doctor"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/resolver"
	"papio/internal/resolvers/arxiv"
	coreresolver "papio/internal/resolvers/core"
	"papio/internal/resolvers/crossreftdm"
	"papio/internal/resolvers/europepmc"
	"papio/internal/resolvers/openalex"
	"papio/internal/resolvers/unpaywall"
	"papio/internal/store"
	"papio/internal/work"
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
	PDFCapability pdf.Capability
	WorkerBinary  string
}

// New builds one production system without starting background goroutines.
func New(ctx context.Context, cfg config.Config) (*System, error) {
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
	service.Resolvers = entries
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

	scheduler, err := daemon.NewScheduler(jobs, service, daemon.SchedulerConfig{
		Owner:             job.NewID("daemon"),
		Workers:           3,
		LeaseDuration:     60 * time.Second,
		HeartbeatInterval: 15 * time.Second,
		PollInterval:      250 * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	system := &System{
		Config: cfg, Store: db, Jobs: jobs, Artifacts: artifacts, Budgets: budgets,
		App: service, Scheduler: scheduler,
		Bundle:        &bundle.Exporter{Jobs: jobs, Artifacts: artifacts},
		Browser:       browser.NewBridge(jobs, service, cfg),
		PDFCapability: capability, WorkerBinary: executable,
	}
	failed = false
	return system, nil
}

// Close releases the process-wide database connection.
func (s *System) Close() error {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.Close()
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
