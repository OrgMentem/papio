// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/config"
	"papio/internal/enrich"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/resolvers/arxiv"
	"papio/internal/resolvers/europepmc"
	"papio/internal/resolvers/openaire"
	"papio/internal/resolvers/semanticscholar"
	"papio/internal/resolvers/unpaywall"
	"papio/internal/store"
	"papio/internal/work"
)

// overlaySources are the resolver source names bench wires for every run,
// plus config.SourceCrossrefMetadata for the typed-relations/enrichment
// HTTP seam (Service.Enricher, not a resolver entry). openalex/core/
// crossref_tdm stay unwired in both overlays: config.Default() already
// ships them disabled, and bench's "current" overlay means "today's shipped
// default," not "every resolver that exists."
var overlaySources = []string{
	config.SourceArXiv, config.SourceEuropePMC, config.SourceUnpaywall,
	config.SourceSemanticScholar, config.SourceOpenAIRE, config.SourceCrossrefMetadata,
}

// baselineDisabledSources are turned off for the baseline overlay:
// Semantic Scholar and OpenAIRE are ordinary resolver sources, per
// dev/post-build-followups.md item 4. crossref_metadata is disabled for a
// second, distinct reason: internal/app's typed-version-relations sibling
// hop reads Service.Enricher, which production wiring only constructs when
// crossref_metadata is enabled (internal/bootstrap/bootstrap.go), and there
// is no independent toggle for typed relations. Disabling crossref_metadata
// is therefore the only available way to turn typed relations off for the
// baseline overlay — and it also disables crossref's title-only metadata
// enrichment as a side effect. That collateral effect is deliberate and
// accepted (see the followups doc), not a bug in this runner.
var baselineDisabledSources = map[string]bool{
	config.SourceSemanticScholar:  true,
	config.SourceOpenAIRE:         true,
	config.SourceCrossrefMetadata: true,
}

// informationalActionKind mirrors job.go's unexported constant of the same
// name and value ("openurl_available"): the conservative-mode advisory that
// records "institutional access exists but was not opened" without
// requiring a human gesture (dev/post-build-followups.md item 1). It is
// duplicated, not imported, because job.go keeps it private; run() excludes
// it from the human-episode count for the same reason job.go's own
// dismissal/sweep logic treats it as non-blocking.
const informationalActionKind = "openurl_available"

// overlayConfig builds the hermetic config for one overlay run. AccessMode
// is delegated so an exhausted resolve attempt opens a real (not merely
// advisory) human action when an institutional OpenURL base is configured —
// conservative mode never crosses a human boundary at all, which would make
// ReadyAfterHumanBoundary unreachable. Hooks, notifications, Zotio, and the
// browser bridge stay at their zero-value "off" state: this function never
// sets Notify.Enabled, Zotio.AutoImport, Hooks, or Browser.ExtensionID, and
// newOverlayService never assigns Service.Notifier/ReadyHook/AutoImporter.
func overlayConfig(dataDir string, current bool) config.Config {
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.DataDir = dataDir
	cfg.Email = "bench@example.org"
	// A fixed, never-dialed default institution: exhaustedCandidates only
	// ever uses this to open a human_actions row and stamp its detail text,
	// never to make an HTTP request.
	cfg.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	cfg.Sources = maps.Clone(cfg.Sources)
	if !current {
		for name := range baselineDisabledSources {
			source := cfg.Sources[name]
			source.Enabled = false
			cfg.Sources[name] = source
		}
	}
	return cfg
}

// resolverEntries builds the real resolver adapters, each pointed at the
// rig's per-source fixture server instead of the provider's real API. This
// mirrors internal/bootstrap/bootstrap.go's resolverEntries wiring, but with
// a plain http.DefaultClient instead of a *fetch.SecureHTTPClient: the
// fixture servers are loopback-only httptest.Servers with nothing to
// SSRF-guard against, and every resolver's Options.Client is a small
// Do(*http.Request) interface any *http.Client already satisfies (see
// internal/resolvers/*/*_test.go).
func resolverEntries(cfg config.Config, rig *sourceRig) []app.ResolverEntry {
	client := http.DefaultClient
	return []app.ResolverEntry{
		{Adapter: arxiv.NewWithOptions(arxiv.Options{Client: client, BaseURL: rig.baseURL(config.SourceArXiv)}), Policy: cfg.SourcePolicy(config.SourceArXiv)},
		{Adapter: europepmc.NewWithOptions(europepmc.Options{Client: client, BaseURL: rig.baseURL(config.SourceEuropePMC)}), Policy: cfg.SourcePolicy(config.SourceEuropePMC)},
		{Adapter: unpaywall.NewWithOptions(unpaywall.Options{Client: client, ContactEmail: cfg.Email, BaseURL: rig.baseURL(config.SourceUnpaywall)}), Policy: cfg.SourcePolicy(config.SourceUnpaywall)},
		{Adapter: semanticscholar.NewWithOptions(semanticscholar.Options{Client: client, BaseURL: rig.baseURL(config.SourceSemanticScholar)}), Policy: cfg.SourcePolicy(config.SourceSemanticScholar)},
		{Adapter: openaire.NewWithOptions(openaire.Options{Client: client, BaseURL: rig.baseURL(config.SourceOpenAIRE)}), Policy: cfg.SourcePolicy(config.SourceOpenAIRE)},
	}
}

// buildEnricher wires the crossref_metadata-backed enricher exactly when
// that source is enabled, matching bootstrap.go's production gate — and,
// per the baselineDisabledSources doc comment, this is also what turns
// typed-relations traversal on or off.
func buildEnricher(cfg config.Config, rig *sourceRig) app.MetadataEnricher {
	if !cfg.SourcePolicy(config.SourceCrossrefMetadata).Enabled {
		return nil
	}
	return enrich.NewWithOptions(enrich.Options{
		Client: http.DefaultClient, ContactEmail: cfg.Email,
		BaseURL: rig.baseURL(config.SourceCrossrefMetadata),
	})
}

// fixtureFetch always "downloads" successfully: hermetic v1 exercises real
// resolver adapters against injected HTTP fixtures (the routing-relevant
// layer — access basis, direct vs. landing page, human action or not), not
// a real PDF byte pipeline. This mirrors internal/app/app_test.go's
// fakeDownload test double.
func fixtureFetch() app.FetchFunc {
	return func(_ context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		body := []byte("%PDF-1.4\nbench fixture for " + c.URL + "\n%%EOF")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		return fetch.Result{
			TempPath: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
			FinalHost: "bench.fixture",
		}, nil
	}
}

// fixtureValidate always accepts, mirroring app_test.go's passValidation.
func fixtureValidate() app.ValidateFunc {
	return func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Chars: 1000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"bench fixture"}},
		}, nil
	}
}

// overlayService is one overlay's fully wired, ephemeral acquisition
// service: its own temp-dir SQLite store and artifact cache, never the
// caller's real data directory.
type overlayService struct {
	svc  *app.Service
	jobs *job.Store
}

func newOverlayService(ctx context.Context, current bool, rig *sourceRig) (*overlayService, func(), error) {
	dir, err := os.MkdirTemp("", "papio-bench-*")
	if err != nil {
		return nil, nil, fmt.Errorf("bench: temp data dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	db, err := store.Open(ctx, dir)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("bench: opening ephemeral store: %w", err)
	}
	artifacts, err := artifact.New(dir)
	if err != nil {
		_ = db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("bench: creating ephemeral artifact store: %w", err)
	}
	cfg := overlayConfig(dir, current)
	jobs := &job.Store{S: db}
	svc := app.New(cfg, jobs, artifacts, nil)
	svc.Resolvers = resolverEntries(cfg, rig)
	svc.Enricher = buildEnricher(cfg, rig)
	svc.Fetch = fixtureFetch()
	svc.Validate = fixtureValidate()
	closeFn := func() {
		_ = db.Close()
		cleanup()
	}
	return &overlayService{svc: svc, jobs: jobs}, closeFn, nil
}

// outcome is one work's settled state under one overlay.
type outcome struct {
	class         ReportClass
	source        string
	route         string
	humanEpisodes int
}

// run submits w's request, drives it through Service.Process to a resting
// state, resolves at most one blocking human action via AdoptDownload when
// the fixture scripts it, and classifies the result.
func (o *overlayService) run(ctx context.Context, w Work, wf WorkFixture, overlay string) (outcome, error) {
	jobID, err := o.svc.Submit(ctx, workRequest(w, overlay))
	if err != nil {
		return outcome{}, fmt.Errorf("submit: %w", err)
	}
	row, err := o.jobs.ClaimNext(ctx, "bench", time.Minute)
	if err != nil {
		return outcome{}, fmt.Errorf("claim: %w", err)
	}
	if row == nil {
		return outcome{}, fmt.Errorf("claim: no claimable job for %s", jobID)
	}
	if err := o.svc.Process(ctx, row); err != nil {
		return outcome{}, fmt.Errorf("process: %w", err)
	}
	row, err = o.jobs.Get(ctx, jobID)
	if err != nil {
		return outcome{}, fmt.Errorf("get after process: %w", err)
	}

	if row.State == job.StateAwaitingHuman {
		if !wf.Adopt {
			return outcome{}, fmt.Errorf("job parked awaiting a human action (state %s) and the fixture does not set \"adopt\": true", row.State)
		}
		if err := o.adopt(ctx, jobID); err != nil {
			return outcome{}, fmt.Errorf("adopt: %w", err)
		}
		row, err = o.jobs.Get(ctx, jobID)
		if err != nil {
			return outcome{}, fmt.Errorf("get after adopt: %w", err)
		}
	}

	actions, err := o.jobs.ListHumanActionsForJob(ctx, jobID)
	if err != nil {
		return outcome{}, fmt.Errorf("listing human actions: %w", err)
	}
	episodes, route := 0, "direct"
	for _, a := range actions {
		if a.Action.Kind == informationalActionKind {
			continue
		}
		episodes++
		route = a.Action.Kind
	}

	class, err := Classify(row.State, job.TerminalReason(row.TerminalReason), episodes)
	if err != nil {
		return outcome{}, err
	}

	source := ""
	if row.SelectedCandidateID != 0 {
		if cand, err := o.jobs.GetCandidate(ctx, row.SelectedCandidateID); err == nil && cand != nil {
			source = cand.Source
		}
	}
	return outcome{class: class, source: source, route: route, humanEpisodes: episodes}, nil
}

// adopt simulates a human completing the parked handoff by writing a
// fixture PDF into the job's adoption directory and calling the same
// Service.AdoptDownload entrypoint the browser bridge calls in production.
func (o *overlayService) adopt(ctx context.Context, jobID string) error {
	dir := filepath.Join(o.svc.Config.EffectiveAdoptionRoot(), jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "bench-adopted.pdf")
	body := []byte("%PDF-1.4\nbench adopted download\n%%EOF")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return o.svc.AdoptDownload(ctx, jobID, path)
}

func workRequest(w Work, overlay string) protocol.WorkRequest {
	req := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     requestID(w.Key, overlay),
		Title:         w.Request.Title,
		Authors:       w.Request.Authors,
		Year:          w.Request.Year,
	}
	if w.Request.DOI != "" || w.Request.ArXiv != "" || w.Request.PMID != "" {
		req.Identifiers = &protocol.Identifiers{DOI: w.Request.DOI, ArXiv: w.Request.ArXiv, PMID: w.Request.PMID}
	}
	return req
}

// requestID derives a stable, schema-legal request_id ([A-Za-z0-9_-]{8,128})
// from a cohort work key.
func requestID(key, overlay string) string {
	var b strings.Builder
	b.WriteString("bench-")
	b.WriteString(overlay)
	b.WriteByte('-')
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	id := b.String()
	for len(id) < 8 {
		id += "-"
	}
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}

// WorkResult is one cohort work's outcome under both overlays.
type WorkResult struct {
	Key                    string        `json:"key"`
	ExpectedClass          ExpectedClass `json:"expected_class"`
	BaselineClass          ReportClass   `json:"baseline_class"`
	CurrentClass           ReportClass   `json:"current_class"`
	BaselineAcceptedSource string        `json:"baseline_accepted_source,omitempty"`
	CurrentAcceptedSource  string        `json:"current_accepted_source,omitempty"`
	BaselineRouteClass     string        `json:"baseline_route_class,omitempty"`
	CurrentRouteClass      string        `json:"current_route_class,omitempty"`
	BaselineHumanEpisodes  int           `json:"baseline_human_episodes"`
	CurrentHumanEpisodes   int           `json:"current_human_episodes"`
	// Error is set instead of the fields above when the hermetic run itself
	// could not settle this work (a fixture bug, an unscripted human
	// action, an unrecognized terminal reason). It is always a bench-level
	// or fixture-authoring defect, never a silent substitute for a report
	// class.
	Error string `json:"error,omitempty"`
}

// Report is one hermetic comparative run over one cohort.
type Report struct {
	CohortID string       `json:"cohort_id"`
	Works    []WorkResult `json:"works"`
}

// IncrementalAutonomousReady is the headline number: how many more (or
// fewer) works reached ready with zero human actions under the current
// resolver set than under the baseline. A work whose row carries an Error,
// or whose classes are both ClassFixtureMissing, contributes zero to both
// sides by construction.
func (r Report) IncrementalAutonomousReady() int {
	var baseline, current int
	for _, w := range r.Works {
		if w.BaselineClass == ClassAutonomousReady {
			baseline++
		}
		if w.CurrentClass == ClassAutonomousReady {
			current++
		}
	}
	return current - baseline
}

// Headline renders the field-report-style "+2 / 9 works" summary.
func (r Report) Headline() string {
	return fmt.Sprintf("%+d / %d works", r.IncrementalAutonomousReady(), len(r.Works))
}

// Run executes cohort twice — once under the baseline resolver overlay,
// once under the current one — against fixtures, and returns one row per
// cohort work. Run itself never fails for an individual work's fixture gap
// or scripting defect; those are reported per-row via WorkResult.Error. It
// returns a top-level error only when the hermetic infrastructure itself
// (the ephemeral stores, the fixture transport) could not be built.
func Run(ctx context.Context, cohort Cohort, fixtures FixtureSet) (Report, error) {
	rig := newSourceRig(overlaySources)
	defer rig.close()

	baseline, closeBaseline, err := newOverlayService(ctx, false, rig)
	if err != nil {
		return Report{}, fmt.Errorf("bench: baseline overlay: %w", err)
	}
	defer closeBaseline()

	current, closeCurrent, err := newOverlayService(ctx, true, rig)
	if err != nil {
		return Report{}, fmt.Errorf("bench: current overlay: %w", err)
	}
	defer closeCurrent()

	report := Report{CohortID: cohort.ID, Works: make([]WorkResult, 0, len(cohort.Works))}
	for _, w := range cohort.Works {
		result := WorkResult{Key: w.Key, ExpectedClass: w.ExpectedClass}
		wf, ok, err := fixtures.Lookup(w.Key)
		switch {
		case err != nil:
			result.Error = fmt.Sprintf("loading fixture: %v", err)
		case !ok:
			result.BaselineClass, result.CurrentClass = ClassFixtureMissing, ClassFixtureMissing
		default:
			rig.setWork(wf)
			base, baseErr := baseline.run(ctx, w, wf, "baseline")
			cur, curErr := current.run(ctx, w, wf, "current")
			switch {
			case baseErr != nil:
				result.Error = fmt.Sprintf("baseline: %v", baseErr)
			case curErr != nil:
				result.Error = fmt.Sprintf("current: %v", curErr)
			default:
				result.BaselineClass, result.CurrentClass = base.class, cur.class
				result.BaselineAcceptedSource, result.CurrentAcceptedSource = base.source, cur.source
				result.BaselineRouteClass, result.CurrentRouteClass = base.route, cur.route
				result.BaselineHumanEpisodes, result.CurrentHumanEpisodes = base.humanEpisodes, cur.humanEpisodes
			}
		}
		report.Works = append(report.Works, result)
	}
	return report, nil
}
