// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/watch"
	"papio/internal/work"
	"papio/internal/zotio"
)

type fakeResolver struct {
	name      string
	cands     []resolver.Candidate
	err       error
	calls     int
	requested []work.Work
}

func (f *fakeResolver) Name() string { return f.name }
func (f *fakeResolver) Resolve(_ context.Context, requested work.Work) ([]resolver.Candidate, error) {
	f.calls++
	f.requested = append(f.requested, requested)
	return append([]resolver.Candidate(nil), f.cands...), f.err
}

type fakeEnricher struct {
	result  work.Work
	matched bool
	err     error
	calls   int
}

func (f *fakeEnricher) Enrich(context.Context, work.Work) (work.Work, bool, error) {
	f.calls++
	return f.result, f.matched, f.err
}

type fakeWorkLookup struct {
	result discovery.DiscoveredWork
	err    error
	calls  int
}

func (f *fakeWorkLookup) LookupWork(context.Context, string) (discovery.DiscoveredWork, error) {
	f.calls++
	return f.result, f.err
}

func newTestService(t *testing.T) (*Service, *job.Store) {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	artifacts, err := artifact.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = data
	// Adoption is a filesystem contract: pin the root to this test's data
	// dir so nothing here ever reaches the real <downloads>/papio default.
	cfg.Browser.AdoptionRoot = filepath.Join(data, "adoptions")
	cfg.Sources["fixture"] = config.Source{Enabled: true}
	svc := New(cfg, &job.Store{S: db}, artifacts, nil)
	return svc, svc.Jobs
}

func submitDedupRequest(requestID string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     requestID,
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/live-dedup"},
	}
}

func TestSubmitReusesLiveCanonicalWork(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	first, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_dedup_0001"), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Existing {
		t.Fatalf("first submission = %+v, want newly created job", first)
	}
	second, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_dedup_0002"), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Existing || second.JobID != first.JobID {
		t.Fatalf("second submission = %+v, want existing job %q", second, first.JobID)
	}
	rows, err := jobs.List(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("jobs = %+v, want one live job", rows)
	}
}

func TestSubmitDoesNotMergeTitleOnlyRequests(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		Title:         "Shared title",
		Authors:       []string{"Ada Lovelace"},
		Year:          2024,
	}
	request.RequestID = "request_title_0001"
	first, err := svc.SubmitWithOptions(ctx, request, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request.RequestID = "request_title_0002"
	second, err := svc.SubmitWithOptions(ctx, request, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Existing || second.Existing || first.JobID == second.JobID {
		t.Fatalf("title-only submissions = %+v, %+v; want distinct jobs", first, second)
	}
	rows, err := jobs.List(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("jobs = %+v, want distinct title-only jobs", rows)
	}
}

func TestSubmitAllowsFreshJobAfterEveryTerminalState(t *testing.T) {
	for _, state := range []string{
		job.StateReady,
		job.StateImported,
		job.StateFailed,
		job.StateUnavailable,
		job.StateCancelled,
	} {
		t.Run(state, func(t *testing.T) {
			svc, jobs := newTestService(t)
			ctx := context.Background()
			first, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_terminal_0001"), SubmitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			terminalizeSubmitDedupJob(t, jobs, first.JobID, state)
			second, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_terminal_0001"), SubmitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if second.Existing || second.JobID == first.JobID {
				t.Fatalf("second submission = %+v, want fresh job after %s", second, state)
			}
			rows, err := jobs.List(ctx, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 {
				t.Fatalf("jobs after %s = %+v, want prior terminal and fresh job", state, rows)
			}
		})
	}
}

func TestSubmitAllowsNoIdentifierRetryAfterDOIAdded(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "request_no_identifier_retry",
		Title:         "A printed monograph",
		Authors:       []string{"A. Author"},
		Year:          1999,
	}
	first, err := svc.SubmitWithOptions(ctx, request, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	terminalizeSubmitDedupJob(t, jobs, first.JobID, job.StateUnavailable)
	request.Identifiers = &protocol.Identifiers{DOI: "10.1000/now-identified"}
	second, err := svc.SubmitWithOptions(ctx, request, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Existing || second.JobID == first.JobID {
		t.Fatalf("identified retry = %+v, want fresh job", second)
	}
	row, err := jobs.Get(ctx, second.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Work.DOI != "10.1000/now-identified" {
		t.Fatalf("retry work = %+v, want added DOI", row.Work)
	}
}

func TestSubmitForceCreatesFreshLiveJob(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	first, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_force_0001"), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_force_0001"), SubmitOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Existing || second.JobID == first.JobID {
		t.Fatalf("forced submission = %+v, want fresh job after %q", second, first.JobID)
	}
	rows, err := jobs.List(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("jobs = %+v, want two concurrent jobs after --force", rows)
	}
}

// A force submission is the operator withdrawing a verdict this work already
// received. The conservative advisory on the superseded job must not outlive
// it: left open, every resubmission double-counts the work's institutional
// opportunity against a job that no longer represents it. Observed live —
// four fabricated `unavailable` verdicts each left a permanent advisory, and
// the consumer's advisory count was inflated rather than its failure count,
// so the corruption was invisible from that side.
func TestForceSubmissionWithdrawsTheSupersededVerdict(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	first, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_supersede_0001"), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	terminalizeSubmitDedupJob(t, jobs, first.JobID, job.StateUnavailable)
	if _, err := jobs.OpenHumanAction(ctx, first.JobID, "openurl_available",
		"no direct candidates; institutional OpenURL available but not opened in conservative mode", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	second, err := svc.SubmitWithOptions(ctx, submitDedupRequest("request_supersede_0002"), SubmitOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Existing || second.JobID == first.JobID {
		t.Fatalf("forced submission = %+v, want a fresh job", second)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{first.JobID})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == first.JobID {
			t.Fatalf("advisory %d still open on superseded job %s; it outlives its own remedy and double-counts the work", action.ID, first.JobID)
		}
	}
	events, err := jobs.Events(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "job.superseded" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		found, _ = detail["superseded_by"].(string)
	}
	if found != second.JobID {
		t.Fatalf("superseded_by = %q, want %q: a withdrawn verdict must say what replaced it", found, second.JobID)
	}
}

func terminalizeSubmitDedupJob(t *testing.T, jobs *job.Store, id, state string) {
	t.Helper()
	if state == job.StateCancelled {
		if err := jobs.Transition(context.Background(), id, job.StateQueued, state, nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := jobs.Transition(context.Background(), id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if state == job.StateImported {
		if err := jobs.Transition(context.Background(), id, job.StateResolving, job.StateReady, nil); err != nil {
			t.Fatal(err)
		}
		if err := jobs.Transition(context.Background(), id, job.StateReady, state, nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	if state == job.StateUnavailable {
		if err := jobs.Transition(context.Background(), id, job.StateResolving, state, nil, job.WithTerminalReason("no_identifier")); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := jobs.Transition(context.Background(), id, job.StateResolving, state, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResolveEnrichesTitleOnlyWorkBeforeResolvers(t *testing.T) {
	svc, jobs := newTestService(t)
	enricher := &fakeEnricher{result: work.Work{
		DOI: "10.1234/crossref", Title: "Exact Title", Authors: []string{"Jane Smith"}, Year: 2024,
	}, matched: true}
	adapter := &fakeResolver{name: "fixture"}
	svc.Enricher = enricher
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	id, err := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_enrich_0001",
		Title: "Exact Title", Authors: []string{"Jane Smith"}, Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolve(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	persisted, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if enricher.calls != 1 || adapter.calls != 1 {
		t.Fatalf("enricher/resolver calls = %d/%d, want 1/1", enricher.calls, adapter.calls)
	}
	if persisted.Work.DOI != "10.1234/crossref" {
		t.Fatalf("persisted DOI = %q", persisted.Work.DOI)
	}
	if len(adapter.requested) != 1 || adapter.requested[0].DOI != persisted.Work.DOI {
		t.Fatalf("resolver received %+v, want enriched DOI", adapter.requested)
	}
}

// A temporary enrichment failure is a request that actually went out, so the
// pass it belongs to is chargeable: the plan must carry that observation, or a
// job whose enrichment keeps failing re-runs the whole chain for free forever.
// Resolution itself still continues in the same pass with the un-enriched work.
func TestResolveContinuesAfterTemporaryEnrichmentFailure(t *testing.T) {
	svc, jobs := newTestService(t)
	enricher := &fakeEnricher{err: &resolver.TemporaryError{Err: errors.New("rate limited")}}
	adapter := &fakeResolver{name: "fixture"}
	svc.Enricher = enricher
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	id, err := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_enrich_0002",
		Title: "Exact Title", Authors: []string{"Jane Smith"}, Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	_, plan, err := svc.resolve(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TemporaryResolvers != 1 || plan.Kind() != retryKindTemporary {
		t.Fatalf("plan = %+v, kind = %q; want one temporary observation charged as %q",
			plan, plan.Kind(), retryKindTemporary)
	}
	if enricher.calls != 1 || adapter.calls != 1 {
		t.Fatalf("enricher/resolver calls = %d/%d, want 1/1", enricher.calls, adapter.calls)
	}
	if len(adapter.requested) != 1 || adapter.requested[0].DOI != "" {
		t.Fatalf("resolver received %+v, want original title-only work", adapter.requested)
	}
}

func TestResolveEnrichesDOIOnlyWorkFromDiscovery(t *testing.T) {
	svc, jobs := newTestService(t)
	lookup := &fakeWorkLookup{result: discovery.DiscoveredWork{Work: work.Work{
		DOI: "10.1002/example", Title: "Discovered title", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}}}
	adapter := &fakeResolver{name: "fixture"}
	svc.Discovery = lookup
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	id, err := svc.Submit(context.Background(), doiRequest("wr_lookup_0001"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolve(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if got := row.Work; got.Title != "Discovered title" || strings.Join(got.Authors, ", ") != "Ada Lovelace" || got.Year != 2024 {
		t.Fatalf("in-memory work = %+v", got)
	}
	persisted, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Work; got.Title != "Discovered title" || strings.Join(got.Authors, ", ") != "Ada Lovelace" || got.Year != 2024 {
		t.Fatalf("persisted work = %+v", got)
	}
	var title, authorsJSON string
	var year int
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT title, authors_json, year FROM work_requests WHERE id = ?`, row.WorkRequestID,
	).Scan(&title, &authorsJSON, &year); err != nil {
		t.Fatal(err)
	}
	if title != "Discovered title" || authorsJSON != `["Ada Lovelace"]` || year != 2024 {
		t.Fatalf("work request metadata = %q, %q, %d", title, authorsJSON, year)
	}
	if len(adapter.requested) != 1 || adapter.requested[0].Title != "Discovered title" {
		t.Fatalf("resolver received %+v, want discovered work", adapter.requested)
	}

	if _, _, err := svc.resolve(context.Background(), persisted); err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 1 {
		t.Fatalf("discovery lookups = %d, want 1 after rediscovery", lookup.calls)
	}
}

func TestResolveSkipsDiscoveryLookupForTitledWork(t *testing.T) {
	svc, jobs := newTestService(t)
	lookup := &fakeWorkLookup{}
	adapter := &fakeResolver{name: "fixture"}
	svc.Discovery = lookup
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	request := doiRequest("wr_lookup_0002")
	request.Title = "Request-supplied title"
	id, err := svc.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolve(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 0 {
		t.Fatalf("discovery lookups = %d, want 0", lookup.calls)
	}
}

func TestResolveContinuesAfterDiscoveryLookupFailure(t *testing.T) {
	svc, jobs := newTestService(t)
	lookup := &fakeWorkLookup{err: errors.New("rate limited")}
	adapter := &fakeResolver{name: "fixture"}
	svc.Discovery = lookup
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	id, err := svc.Submit(context.Background(), doiRequest("wr_lookup_0003"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolve(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 1 || len(adapter.requested) != 1 || adapter.requested[0].Title != "" {
		t.Fatalf("lookup/resolver = %d/%+v", lookup.calls, adapter.requested)
	}
}

func doiRequest(id string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      id,
		Identifiers:    &protocol.Identifiers{DOI: "10.1002/example"},
		DesiredVersion: "any",
	}
}

// doiRequestFor derives a DISTINCT DOI per request id, for fixtures that need
// several concurrent jobs. doiRequest deliberately keeps one fixed DOI so tests
// about a single work stay readable, but submission now dedups against a live
// job for the same canonical work, so reusing it for several jobs yields one.
func doiRequestFor(id string) protocol.WorkRequest {
	request := doiRequest(id)
	request.Identifiers = &protocol.Identifiers{DOI: "10.1002/example-" + strings.ToLower(id)}
	return request
}

func pdfBytes(label string) []byte {
	body := []byte("%PDF-1.4\n" + label + "\n")
	body = append(body, make([]byte, pdf.MinimumPayloadBytes+100)...)
	body = append(body, []byte("\n%%EOF")...)
	return body
}

func fakeDownload(counter *int) FetchFunc {
	return func(_ context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		*counter++
		body := pdfBytes(c.URL)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		return fetch.Result{
			TempPath: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
			FinalHost: "example.test",
		}, nil
	}
}

func passValidation() ValidateFunc {
	return func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text:       pdf.TextReport{Chars: 2000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
}

type fakeAutoImporter struct {
	calls         int
	status        string
	parentKey     string
	attachmentKey string
	err           error
}

func (f *fakeAutoImporter) PlanAndApply(context.Context, string) (string, string, string, error) {
	f.calls++
	return f.status, f.parentKey, f.attachmentKey, f.err
}

type watchDiscoveryForApp struct{ works []discovery.DiscoveredWork }

func (d watchDiscoveryForApp) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return append([]discovery.DiscoveredWork(nil), d.works...), nil
}

type watchLookupForApp struct{ result *zotio.LookupWorksResult }

func (l watchLookupForApp) LookupWorks(context.Context, zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error) {
	return l.result, nil
}

func TestProcessReadyEnrichesMetadataAndNeverPersistsSecrets(t *testing.T) {
	svc, jobs := newTestService(t)
	secret := "SENTINEL_DO_NOT_STORE"
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/paper.pdf?token=" + secret,
		Landing:        "https://example.test/article#" + secret,
		RequestHeaders: map[string]string{"Authorization": "Bearer " + secret},
		ResolvedWork:   work.Work{DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024},
		Version:        resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_ready_0001"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatalf("process: %v", err)
	}
	ready, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady || ready.ArtifactSHA256 == "" || ready.SelectedCandidateID == 0 {
		t.Fatalf("ready row = %+v", ready)
	}
	if ready.Work.Title != "Example Paper" || len(ready.Work.Authors) != 1 || ready.Work.Year != 2024 {
		t.Fatalf("resolver metadata not filled: %+v", ready.Work)
	}
	artifact, err := jobs.GetArtifact(context.Background(), ready.ArtifactSHA256)
	if err != nil || artifact == nil || artifact.IdentityResult != pdf.IdentityPass {
		t.Fatalf("pass artifact = %+v, %v", artifact, err)
	}
	if fetches != 1 || adapter.calls != 1 {
		t.Fatalf("fetch/resolver calls = %d/%d", fetches, adapter.calls)
	}
	if err := svc.Artifacts.Verify(ready.ArtifactSHA256); err != nil {
		t.Fatalf("artifact verify: %v", err)
	}
	candidate, err := jobs.GetCandidate(context.Background(), ready.SelectedCandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(candidate.URLRedacted, secret) || strings.Contains(candidate.LandingRedacted, secret) {
		t.Fatalf("candidate leaked secret: %+v", candidate)
	}
	events, _ := jobs.Events(context.Background(), id)
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "Authorization") {
		t.Fatalf("event stream leaked ephemeral headers/query: %s", encoded)
	}
}

func TestLocalCacheCompletesWithoutResolverOrFetch(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/paper.pdf", ResolvedWork: work.Work{DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"A"}, Year: 2024},
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	first, _ := svc.Submit(context.Background(), doiRequest("wr_cache_0001"))
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if got, _ := jobs.Get(context.Background(), first); got.State != job.StateReady {
		t.Fatalf("first state = %s", got.State)
	}

	second, _ := svc.Submit(context.Background(), doiRequest("wr_cache_0002"))
	row, _ = jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	cached, _ := jobs.Get(context.Background(), second)
	if cached.State != job.StateReady || cached.ArtifactSHA256 == "" {
		t.Fatalf("cache state = %+v", cached)
	}
	if fetches != 1 || adapter.calls != 1 {
		t.Fatalf("cache repeated network: fetch=%d resolve=%d", fetches, adapter.calls)
	}
	// The cached job did no acquisition of its own: these are the first job's
	// bytes, so the first job's accepted candidate is the honest provenance and
	// must be recorded rather than reconstructed from the digest later (ADR-0007).
	firstRow, _ := jobs.Get(context.Background(), first)
	if cached.SelectedCandidateID == 0 {
		t.Fatal("cache-completed job recorded no provenance candidate")
	}
	if cached.SelectedCandidateID != firstRow.SelectedCandidateID {
		t.Fatalf("cache provenance candidate = %d, want the source acquisition's %d",
			cached.SelectedCandidateID, firstRow.SelectedCandidateID)
	}
}

func TestWrongPaperFallsThroughToNextCandidate(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{
		{Source: "fixture", URL: "https://example.test/wrong.pdf", Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1},
		{Source: "fixture", URL: "https://example.test/right.pdf", Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: .9},
	}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches, validations := 0, 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		validations++
		decision := pdf.IdentityReject
		if validations == 2 {
			decision = pdf.IdentityPass
		}
		return pdf.ValidationReport{
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text: pdf.TextReport{Chars: 1500}, Identity: pdf.IdentityDecision{Result: decision},
		}, nil
	}
	id, _ := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_fallback_01",
		Identifiers: &protocol.Identifiers{DOI: "10.1002/example"}, Title: "Example", Authors: []string{"A"}, Year: 2024,
	})
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, _ := jobs.Get(context.Background(), id)
	if got.State != job.StateReady || fetches != 2 || validations != 2 {
		t.Fatalf("fallback result=%+v fetch=%d validate=%d", got, fetches, validations)
	}
}

func TestRetryableFetchParksJobAndPersistsNoURL(t *testing.T) {
	svc, jobs := newTestService(t)
	secret := "RETRY_SECRET"
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/p.pdf?sig=" + secret,
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassRetryable, HTTPStatus: 503, RetryAfter: time.Minute, Msg: "service unavailable"}
	}
	svc.Validate = passValidation()
	id, _ := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_retry_0001",
		Identifiers: &protocol.Identifiers{DOI: "10.1002/example"}, Title: "Example", Authors: []string{"A"}, Year: 2024,
	})
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, _ := jobs.Get(context.Background(), id)
	if got.State != job.StateRetryWait || got.RetryAt == "" {
		t.Fatalf("retry state = %+v", got)
	}
	events, _ := jobs.Events(context.Background(), id)
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("retry event leaked signed URL: %s", encoded)
	}
}

// A source gated by a daily quota reset must park the job, not the worker.
// OpenAlex answers an exhausted daily quota with a Retry-After pointing at the
// next UTC midnight; budget.Acquire used to sleep that out inside Process,
// which holds the scheduler's job claim while its heartbeat keeps renewing the
// lease — three workers on three such rows froze a 309-job cohort for a day.
// Process must return promptly with the job parked at the gate instead.
func TestDailyQuotaGateParksTheJobNotTheWorker(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.Budgets = budget.New(jobs.S)
	gate := time.Now().UTC().Add(18 * time.Hour)
	if err := svc.Budgets.Defer(ctx, "fixture", config.Source{Enabled: true}, gate); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/p.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, errors.New("fetch must not run: the only source is gated")
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	svc.Validate = passValidation()
	id, _ := svc.Submit(ctx, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_quota_0001",
		Identifiers: &protocol.Identifiers{DOI: "10.1002/example"}, Title: "Example", Authors: []string{"A"}, Year: 2024,
	})
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	start := time.Now()
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Process held the worker %v on a gated source; it must hand the wait to the scheduler", elapsed)
	}
	if adapter.calls != 0 {
		t.Fatalf("gated adapter was called %d times, want 0", adapter.calls)
	}
	got, _ := jobs.Get(ctx, id)
	if got.State != job.StateRetryWait || got.RetryAt == "" {
		t.Fatalf("job state = %+v, want retry_wait parked at the gate", got)
	}
	parked, err := time.Parse(time.RFC3339Nano, got.RetryAt)
	if err != nil {
		t.Fatalf("retry_at %q: %v", got.RetryAt, err)
	}
	if parked.Sub(gate).Abs() > time.Minute {
		t.Fatalf("retry_at = %s, want the source gate %s", parked, gate)
	}
}

func TestOALandingRoutesToBrowserHandoff(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
	const landing = "https://example.test/landing"
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: landing, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: false, IdentityConfidence: 1,
	}}}, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()
	id, _ := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_oa_landing_001",
		Identifiers: &protocol.Identifiers{DOI: "10.1002/example"}, Title: "Example", Authors: []string{"A"}, Year: 2024,
	})
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, _ := jobs.Get(context.Background(), id)
	// The daemon never fetches a landing page server-side; the extension's
	// provider adapters resolve the file from the open-access page.
	if got.State != job.StateAwaitingHuman || fetches != 0 {
		t.Fatalf("oa landing result = %+v fetches=%d", got, fetches)
	}
	actions, _ := jobs.ListHumanActions(context.Background(), true)
	if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != "openurl_handoff" {
		t.Fatalf("actions = %+v, want one openurl_handoff", actions)
	}
	// The handoff must carry the OA browser URL (no login) so the bridge opens
	// the page itself rather than constructing an institutional resolver link.
	if actions[0].Detail != OABrowserHandoffActionDetail(landing) {
		t.Fatalf("handoff detail = %q, want OA browser handoff for %q", actions[0].Detail, landing)
	}
	if actions[0].RequiresAuth {
		t.Fatalf("OA browser handoff must not require auth: %+v", actions[0])
	}
}

func TestPaywalledLandingRequiresManualDownload(t *testing.T) {
	svc, jobs := newTestService(t)
	// No institutional OpenURL base: a non-OA landing page cannot be browser-
	// fetched (no adapter path) nor handed to a resolver, so it stays a manual
	// download rather than opening a tab that cannot resolve to a file.
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://paywall.test/landing", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessInstitutional, ReuseLicense: "unknown", Direct: false, IdentityConfidence: 1,
	}}}, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()
	id, _ := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_manual_001",
		Identifiers: &protocol.Identifiers{DOI: "10.1002/example"}, Title: "Example", Authors: []string{"A"}, Year: 2024,
	})
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, _ := jobs.Get(context.Background(), id)
	if got.State != job.StateAwaitingHuman || fetches != 0 {
		t.Fatalf("manual result = %+v fetches=%d", got, fetches)
	}
	actions, _ := jobs.ListHumanActions(context.Background(), true)
	if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != "manual_download" {
		t.Fatalf("actions = %+v", actions)
	}
	if got := actionDiagnosis(t, jobs, actions[0].ID); got != job.DiagnosisReasonLandingPageOnly {
		t.Fatalf("diagnosis = %q, want %q", got, job.DiagnosisReasonLandingPageOnly)
	}
}

func TestCrashRecoveryRefetchesMidflightWithoutDuplicateDurableRecords(t *testing.T) {
	for _, crashedState := range []string{job.StateFetching, job.StateValidating} {
		t.Run(crashedState, func(t *testing.T) {
			svc, jobs := newTestService(t)
			adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
				Source: "fixture", URL: "https://example.test/recovered.pdf",
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
				ReuseLicense: "cc-by", ExpectedMIME: "application/pdf", Direct: true,
				IdentityConfidence: 1,
			}}}
			svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
			fetches := 0
			svc.Fetch = fakeDownload(&fetches)
			svc.Validate = passValidation()
			id, err := svc.Submit(context.Background(), doiRequest("wr_recovery_"+crashedState))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.ClaimNext(context.Background(), "crashed", -time.Second); err != nil {
				t.Fatal(err)
			}
			if err := jobs.Transition(context.Background(), id, job.StateQueued, job.StateResolving, nil); err != nil {
				t.Fatal(err)
			}
			if err := jobs.Transition(context.Background(), id, job.StateResolving, job.StateFetching, nil); err != nil {
				t.Fatal(err)
			}
			if crashedState == job.StateValidating {
				if err := jobs.Transition(context.Background(), id, job.StateFetching, job.StateValidating, nil); err != nil {
					t.Fatal(err)
				}
			}
			qdir, err := svc.Artifacts.QuarantineDir(id)
			if err != nil {
				t.Fatal(err)
			}
			stalePaths := []string{
				filepath.Join(qdir, "stale-fetch.tmp"),
				filepath.Join(qdir, "stale-validate.tmp"),
			}
			for _, stalePath := range stalePaths {
				if err := os.WriteFile(stalePath, pdfBytes("stale"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := jobs.RecoverStale(context.Background())
			if err != nil || len(recovered) != 1 || recovered[0] != id {
				t.Fatalf("recovered = %v, %v", recovered, err)
			}
			row, err := jobs.ClaimNext(context.Background(), "replacement", time.Minute)
			if err != nil || row == nil {
				t.Fatalf("reclaim = %+v, %v", row, err)
			}
			if err := svc.Process(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			for _, stalePath := range stalePaths {
				if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
					t.Fatalf("recovered quarantine file %q exists: %v", stalePath, err)
				}
			}
			ready, err := jobs.Get(context.Background(), id)
			if err != nil || ready.State != job.StateReady || fetches != 1 {
				t.Fatalf("recovered job = %+v, fetches=%d, err=%v", ready, fetches, err)
			}
			var artifacts, candidates int
			if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifacts); err != nil {
				t.Fatal(err)
			}
			if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM candidates WHERE job_id = ?`, id).Scan(&candidates); err != nil {
				t.Fatal(err)
			}
			if artifacts != 1 || candidates != 1 {
				t.Fatalf("durable duplicates: artifacts=%d candidates=%d", artifacts, candidates)
			}
		})
	}
}

// seedValidatingCandidate drives a fresh job through the three-edge walk
// queued→resolving→fetching→validating and stages a quarantined candidate file.
// The three edges are queued→resolving, resolving→fetching, fetching→validating.
// Validation is the starting point because these tests call validateCandidate
// directly — that function requires the job already be in StateValidating, where
// artifact-metadata persistence is attempted before promotion. The helper returns
// the validating row, the pending candidate, the PDF body, the quarantine
// temp path, and the SHA-256 hex so callers can focus on the Validate stub
// and the post-validation assertions.
func seedValidatingCandidate(t *testing.T, svc *Service, jobs *job.Store, wrName, urlKey, seed string) (*job.Row, *job.Candidate, []byte, string, string) {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, wrName, work.Work{DOI: "10.1002/example"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/" + urlKey + ".pdf", URLKey: urlKey,
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.NextPendingCandidate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateValidating},
	} {
		if err := jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	qdir, err := svc.Artifacts.QuarantineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	body := pdfBytes(seed)
	tempPath := filepath.Join(qdir, "candidate.tmp")
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	return row, candidate, body, tempPath, sha
}

func TestValidationPersistsArtifactMetadataBeforePromotion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, jobs := newTestService(t)
	row, candidate, body, tempPath, sha := seedValidatingCandidate(t, svc, jobs, "wr_artifact_metadata_first", "paper", "promotion-order")
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		cancel()
		return passValidation()(context.Background(), "", "", work.Work{})
	}

	_, _, err := svc.validateCandidate(ctx, row, candidate, fetch.Result{
		TempPath: tempPath, SHA256: sha, SizeBytes: int64(len(body)), SniffedMIME: "application/pdf",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("validation = %v, want context cancellation", err)
	}
	dest, err := svc.Artifacts.ArtifactPath(sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("artifact was promoted after metadata persistence failed: %v", err)
	}
	art, err := jobs.GetArtifact(context.Background(), sha)
	if err != nil || art != nil {
		t.Fatalf("artifact metadata after failed persistence = %+v, %v", art, err)
	}
}

func TestValidationRemovesMetadataWhenPromotionFails(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	row, candidate, body, tempPath, sha := seedValidatingCandidate(t, svc, jobs, "wr_promotion_rollback", "promotion-rollback", "promotion-rollback")
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		if err := os.Remove(tempPath); err != nil {
			t.Fatal(err)
		}
		return passValidation()(context.Background(), "", "", work.Work{})
	}

	if _, _, err := svc.validateCandidate(ctx, row, candidate, fetch.Result{
		TempPath: tempPath, SHA256: sha, SizeBytes: int64(len(body)), SniffedMIME: "application/pdf",
	}); err == nil {
		t.Fatal("validation succeeded despite promotion failure")
	}
	dest, err := svc.Artifacts.ArtifactPath(sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("artifact file remains after promotion failure: %v", err)
	}
	art, err := jobs.GetArtifact(ctx, sha)
	if err != nil || art != nil {
		t.Fatalf("artifact metadata remains after promotion failure = %+v, %v", art, err)
	}
}

func TestSubmitRequiresExplicitAccessMode(t *testing.T) {
	svc, _ := newTestService(t)
	svc.Config.AccessMode = ""
	_, err := svc.Submit(context.Background(), doiRequest("wr_no_mode_01"))
	var unset *config.ErrAccessModeUnset
	if !errors.As(err, &unset) {
		t.Fatalf("submit err = %v, want ErrAccessModeUnset", err)
	}
}

func TestAcceptedIdentityReviewResumesAndRecordsOverride(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/review.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text: pdf.TextReport{Chars: 2000}, Identity: pdf.IdentityDecision{Result: pdf.IdentityReview},
		}, nil
	}
	id, err := svc.Submit(context.Background(), doiRequest("wr_review_resume"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "first-worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("first claim = %+v, %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	parked, _ := jobs.Get(context.Background(), id)
	if parked.State != job.StateNeedsReview {
		t.Fatalf("initial review state = %+v", parked)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil || len(actions) != 1 || actions[0].Kind != "verify_identity" || !strings.Contains(actions[0].Detail, "local quarantine file:") ||
		actions[0].CandidateID <= 0 || actions[0].QuarantinePath == "" || len(actions[0].QuarantineSHA256) != 64 || actions[0].Revision != 1 {
		t.Fatalf("review action = %+v, %v", actions, err)
	}
	resolution, err := jobs.ResolveReviewCAS(context.Background(), job.ResolveReviewInput{
		ActionID: actions[0].ID, Verdict: "accept", ExpectedRevision: actions[0].Revision,
		ExpectedSHA256: actions[0].QuarantineSHA256,
	})
	if err != nil || resolution.Outcome != job.ReviewApplied || resolution.State != job.StateFetching {
		t.Fatalf("accept review = %+v, %v", resolution, err)
	}
	row, err = jobs.ClaimNext(context.Background(), "second-worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("resumed claim = %+v, %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	ready, _ := jobs.Get(context.Background(), id)
	if ready.State != job.StateReady || ready.ArtifactSHA256 != actions[0].QuarantineSHA256 || fetches != 1 {
		t.Fatalf("review-resumed result = %+v; fetches=%d", ready, fetches)
	}
	artifact, err := jobs.GetArtifact(context.Background(), ready.ArtifactSHA256)
	if err != nil || artifact == nil || artifact.IdentityResult != "user_confirmed" {
		t.Fatalf("accepted review artifact = %+v, %v", artifact, err)
	}
	events, _ := jobs.Events(context.Background(), id)
	foundOverride := false
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if detail["reason"] == "human_identity_override" {
			foundOverride = true
		}
	}
	if !foundOverride {
		t.Fatalf("events missing human_identity_override: %+v", events)
	}
}

func TestAcceptedIdentityReviewRedownloadsWhenQuarantineIsMissing(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/review-missing.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text: pdf.TextReport{Chars: 2000}, Identity: pdf.IdentityDecision{Result: pdf.IdentityReview},
		}, nil
	}
	id, err := svc.Submit(context.Background(), doiRequest("wr_review_missing"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "first-worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("first claim = %+v, %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil || len(actions) != 1 {
		t.Fatalf("review action = %+v, %v", actions, err)
	}
	if err := os.Remove(actions[0].QuarantinePath); err != nil {
		t.Fatal(err)
	}
	resolution, err := jobs.ResolveReviewCAS(context.Background(), job.ResolveReviewInput{
		ActionID: actions[0].ID, Verdict: "accept", ExpectedRevision: actions[0].Revision,
		ExpectedSHA256: actions[0].QuarantineSHA256,
	})
	if err != nil || resolution.Outcome != job.ReviewApplied || resolution.State != job.StateFetching {
		t.Fatalf("accept review = %+v, %v", resolution, err)
	}
	row, err = jobs.ClaimNext(context.Background(), "second-worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("resumed claim = %+v, %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	ready, err := jobs.Get(context.Background(), id)
	if err != nil || ready.State != job.StateReady || fetches != 2 {
		t.Fatalf("missing-review-file result = %+v; fetches=%d, err=%v", ready, fetches, err)
	}
}

func TestReviewOverrideDoesNotBypassRejectOrUnsafePDF(t *testing.T) {
	for name, report := range map[string]pdf.ValidationReport{
		"identity_reject": {
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true},
			Text: pdf.TextReport{Chars: 2000}, Identity: pdf.IdentityDecision{Result: pdf.IdentityReject},
		},
		"unsafe_pdf": {
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true, Encrypted: true},
			Text: pdf.TextReport{Chars: 2000}, Identity: pdf.IdentityDecision{Result: pdf.IdentityReview},
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, jobs := newTestService(t)
			svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				return report, nil
			}
			id, err := jobs.CreateRequest(context.Background(), "wr_override_"+name, work.Work{DOI: "10.1002/example"}, "", "", job.Policy{
				AccessMode: config.ModeConservative, DesiredVersion: "any",
			}, nil, job.PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.InsertCandidates(context.Background(), id, []job.Candidate{{
				JobID: id, Source: "fixture", URLRedacted: "https://example.test/" + name + ".pdf", URLKey: name,
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
				ReviewOverride: true,
			}}); err != nil {
				t.Fatal(err)
			}
			candidate, _ := jobs.NextPendingCandidate(context.Background(), id)
			for _, edge := range [][2]string{
				{job.StateQueued, job.StateResolving},
				{job.StateResolving, job.StateFetching},
				{job.StateFetching, job.StateValidating},
			} {
				if err := jobs.Transition(context.Background(), id, edge[0], edge[1], nil); err != nil {
					t.Fatal(err)
				}
			}
			row, _ := jobs.Get(context.Background(), id)
			temp := t.TempDir() + "/candidate.pdf"
			if err := os.WriteFile(temp, pdfBytes(name), 0o600); err != nil {
				t.Fatal(err)
			}
			accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{
				TempPath: temp, SHA256: strings.Repeat("a", 64), SniffedMIME: "application/pdf",
			})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := jobs.Get(context.Background(), id)
			switch name {
			case "identity_reject":
				if accepted || parked || got.State != job.StateFetching {
					t.Fatalf("identity reject bypassed by override: accepted=%t parked=%t job=%+v", accepted, parked, got)
				}
			case "unsafe_pdf":
				if accepted || !parked || got.State != job.StateNeedsReview {
					t.Fatalf("unsafe PDF bypassed by override: accepted=%t parked=%t job=%+v", accepted, parked, got)
				}
			}
		})
	}
}
func TestValidateCandidateHoldsEmbeddedAndEncryptedPDFsForReview(t *testing.T) {
	// Embedded/active/encrypted PDFs must park needs_review with an unsafe_pdf
	// action holding the quarantine file — not return to fetching with a
	// manual_download "please supply a different file" ask. SAGE/T&F publisher
	// PDFs legitimately bundle supplementary files while returning Valid=false
	// (plus HasEmbeddedFiles=true), so the encrypted/active branch must precede
	// the generic invalid check.
	cases := []struct {
		name   string
		report pdf.ValidationReport
	}{
		{
			name: "embedded_files_with_invalid_structure",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: false, HasEmbeddedFiles: true, Reason: "PDF contains embedded files"},
				Text:       pdf.TextReport{Chars: 2000},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
		},
		{
			name: "embedded_files_with_valid_structure",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true, HasEmbeddedFiles: true},
				Text:       pdf.TextReport{Chars: 2000},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
		},
		{
			name: "javascript_with_invalid_structure",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: false, HasJavaScript: true, Reason: "PDF contains JavaScript"},
				Text:       pdf.TextReport{Chars: 2000},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
		},
		{
			name: "encrypted_with_invalid_structure",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: false, Encrypted: true, Reason: "encrypted PDF"},
				Text:       pdf.TextReport{Chars: 2000},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, jobs := newTestService(t)
			svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				return tc.report, nil
			}
			id, err := jobs.CreateRequest(context.Background(), "wr_hold_"+tc.name, work.Work{DOI: "10.1002/example"}, "", "", job.Policy{
				AccessMode: config.ModeConservative, DesiredVersion: "any",
			}, nil, job.PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.InsertCandidates(context.Background(), id, []job.Candidate{{
				JobID: id, Source: "fixture", URLRedacted: "https://example.test/" + tc.name + ".pdf", URLKey: tc.name,
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
			}}); err != nil {
				t.Fatal(err)
			}
			candidate, _ := jobs.NextPendingCandidate(context.Background(), id)
			for _, edge := range [][2]string{
				{job.StateQueued, job.StateResolving},
				{job.StateResolving, job.StateFetching},
				{job.StateFetching, job.StateValidating},
			} {
				if err := jobs.Transition(context.Background(), id, edge[0], edge[1], nil); err != nil {
					t.Fatal(err)
				}
			}
			row, _ := jobs.Get(context.Background(), id)
			temp := t.TempDir() + "/candidate.pdf"
			if err := os.WriteFile(temp, pdfBytes(tc.name), 0o600); err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("b", 64)
			accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{
				TempPath: temp, SHA256: sha, SniffedMIME: "application/pdf",
			})
			if err != nil {
				t.Fatal(err)
			}
			if accepted || !parked {
				t.Fatalf("embedded/active/encrypted must park unsafe_pdf: accepted=%t parked=%t", accepted, parked)
			}
			got, _ := jobs.Get(context.Background(), id)
			if got.State != job.StateNeedsReview {
				t.Fatalf("state = %s, want needs_review", got.State)
			}
			actions, err := jobs.ListHumanActions(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			var unsafe *job.HumanAction
			for i := range actions {
				if actions[i].JobID == id && actions[i].Kind == "unsafe_pdf" {
					unsafe = &actions[i]
					break
				}
			}
			if unsafe == nil || unsafe.Status != "open" {
				t.Fatalf("unsafe_pdf action = %+v, want open", unsafe)
			}
			if unsafe.CandidateID != candidate.ID || unsafe.QuarantineSHA256 != sha || unsafe.QuarantinePath != temp {
				t.Fatalf("unsafe_pdf binding = %+v, want candidate %d sha %s path %s", unsafe, candidate.ID, sha, temp)
			}
			if _, err := os.Stat(temp); err != nil {
				t.Fatalf("quarantined file missing at %s: %v", temp, err)
			}
			// Must not also have opened a manual_download replacement ask.
			for i := range actions {
				if actions[i].JobID == id && actions[i].Kind == "manual_download" && actions[i].Status == "open" {
					t.Fatalf("unexpected manual_download action alongside unsafe_pdf: %+v", &actions[i])
				}
			}
		})
	}
}

func TestValidateCandidateTrulyInvalidPayloadStillFetchesAgain(t *testing.T) {
	// A genuinely invalid payload with no embedded/JS/encrypted signal must still
	// return to fetching (and remove the temp file) rather than parking.
	svc, jobs := newTestService(t)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: false, Reason: "too small"},
			Structural: pdf.StructuralReport{Valid: false},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}
	id, err := jobs.CreateRequest(context.Background(), "wr_invalid_payload", work.Work{DOI: "10.1002/example"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.InsertCandidates(context.Background(), id, []job.Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/invalid_payload.pdf", URLKey: "invalid_payload",
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, _ := jobs.NextPendingCandidate(context.Background(), id)
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateValidating},
	} {
		if err := jobs.Transition(context.Background(), id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	row, _ := jobs.Get(context.Background(), id)
	temp := t.TempDir() + "/candidate.pdf"
	if err := os.WriteFile(temp, pdfBytes("invalid payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{
		TempPath: temp, SHA256: strings.Repeat("c", 64), SniffedMIME: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted || parked {
		t.Fatalf("invalid payload must not accept or park: accepted=%t parked=%t", accepted, parked)
	}
	got, _ := jobs.Get(context.Background(), id)
	if got.State != job.StateFetching {
		t.Fatalf("state = %s, want fetching", got.State)
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file should have been removed at %s: stat err=%v", temp, err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.JobID == id && a.Status == "open" {
			t.Fatalf("invalid payload should not open a human action, got %+v", &a)
		}
	}
}

func readyPipeline(svc *Service) {
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/auto-import.pdf",
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}}}, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()
}

func TestWatchSubmissionForcesAutoImportThroughReady(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = false
	importer := &fakeAutoImporter{status: "applied", parentKey: "PARENT01", attachmentKey: "ATTACH01"}
	svc.AutoImporter = importer
	readyPipeline(svc)

	watches := watch.NewStore(jobs.S)
	created, err := watches.Create(ctx, watch.CreateInput{
		Query: "auto import", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &watch.Runner{
		Store: watches,
		Discovery: watchDiscoveryForApp{works: []discovery.DiscoveredWork{{
			Work:       work.Work{DOI: "10.1002/example", Title: "Watch paper", Authors: []string{"Ada"}, Year: 2026},
			OpenAlexID: "https://openalex.org/W2741809807",
		}}},
		Lookup:    watchLookupForApp{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		Submitter: svc,
		DataDir:   svc.Config.DataDir,
	}
	result, err := runner.Run(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 1 || result.Failed != 0 {
		t.Fatalf("watch run = %+v", result)
	}
	row, err := jobs.ClaimNext(ctx, "watch-test", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim watch job = %+v, %v", row, err)
	}
	submitted, err := jobs.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !submitted.Policy.AutoImport || submitted.Policy.Collection != "Reading" {
		t.Fatalf("watch job policy = %+v", submitted.Policy)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	if importer.calls != 1 {
		t.Fatalf("PlanAndApply calls = %d, want 1", importer.calls)
	}
}

func autoImportEvent(t *testing.T, jobs *job.Store, id string) map[string]any {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "zotio.auto_import" {
			detail, ok := event["detail"].(map[string]any)
			if !ok {
				t.Fatalf("auto-import event detail = %#v", event["detail"])
			}
			return detail
		}
	}
	t.Fatalf("no zotio.auto_import event in %#v", events)
	return nil
}

func TestProcessReadyAutoImportsOnce(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	importer := &fakeAutoImporter{status: "applied", parentKey: "PARENT01", attachmentKey: "ATTACH01"}
	svc.AutoImporter = importer
	notifier := &fakeNotificationSink{}
	svc.Notifier = notifier
	readyPipeline(svc)

	id, err := svc.Submit(context.Background(), doiRequest("wr_auto_import_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	ready, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady {
		t.Fatalf("job state = %s, want ready", ready.State)
	}
	detail := autoImportEvent(t, jobs, id)
	if detail["status"] != "applied" || detail["parent_key"] != "PARENT01" || detail["attachment_key"] != "ATTACH01" {
		t.Fatalf("auto-import detail = %#v", detail)
	}
	if err := svc.Process(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if importer.calls != 1 {
		t.Fatalf("auto-import calls = %d, want 1", importer.calls)
	}
	if notifier.imported != 0 {
		t.Fatalf("per-paper import notifications = %d, want none", notifier.imported)
	}
}
func TestAutoImportCancellationDoesNotRecordFailure(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	importer := &fakeAutoImporter{err: context.Canceled}
	svc.AutoImporter = importer
	id, err := svc.Submit(context.Background(), doiRequest("wr_auto_import_cancelled"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.autoImportReady(ctx, row)
	if importer.calls != 1 {
		t.Fatalf("PlanAndApply calls = %d, want 1", importer.calls)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "zotio.auto_import" {
			t.Fatalf("cancelled auto-import recorded a durable outcome: %#v", event)
		}
	}
	if !importNeedsRetry(events) {
		t.Fatal("cancelled auto-import must remain eligible for a later retry")
	}
}

func TestProcessReadyAutoImportFailureLeavesJobReady(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	importer := &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("zotio stderr: unknown item field at https://zotero.example.test/users/42 /Users/reader/private.db"))}
	svc.AutoImporter = importer
	readyPipeline(svc)

	id, err := svc.Submit(context.Background(), doiRequest("wr_auto_import_fail"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatalf("auto-import failure should be non-fatal: %v", err)
	}
	ready, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady {
		t.Fatalf("job state = %s, want ready", ready.State)
	}
	detail := autoImportEvent(t, jobs, id)
	if detail["status"] != "error" || detail["error_type"] == "" || detail["error_class"] != zotio.ErrorClassZoteroFieldValidation || detail["error_hint"] != "unknown item field" {
		t.Fatalf("auto-import failure detail = %#v", detail)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "zotero.example.test") || strings.Contains(string(encoded), "/Users/reader") {
		t.Fatalf("auto-import event leaked private detail: %s", encoded)
	}
	if importer.calls != 1 {
		t.Fatalf("auto-import calls = %d, want 1", importer.calls)
	}
}

func TestProcessReadyAutoImportWithoutServiceRecordsSkip(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)

	id, err := svc.Submit(context.Background(), doiRequest("wr_auto_import_skip"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	detail := autoImportEvent(t, jobs, id)
	if detail["status"] != "skipped" || detail["reason"] != "zotio_not_configured" {
		t.Fatalf("auto-import skip detail = %#v", detail)
	}
}

func TestSubmitWithAutoImportOverrideBeatsConfigDefault(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	disabled := false
	// Two DISTINCT works. Each half asserts a property of NEW-job creation, and
	// re-submitting one work would dedup into the first job and read back its
	// policy rather than the one under test.
	id, err := svc.SubmitWithAutoImport(context.Background(), doiRequestFor("wr_auto_import_off"), &disabled)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Policy.AutoImport {
		t.Fatal("explicit --auto-import=false did not override config")
	}
	id, err = svc.SubmitWithAutoImport(context.Background(), doiRequestFor("wr_auto_import_cfg"), nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err = jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Policy.AutoImport {
		t.Fatal("config zotio.auto_import did not become new-job default")
	}
}

func TestSubmitCarriesCollectionIntoJobPolicy(t *testing.T) {
	svc, jobs := newTestService(t)
	request := doiRequest("wr_collection_policy")
	request.Collection = "  Reading list  "
	id, err := svc.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Policy.Collection != "Reading list" {
		t.Fatalf("policy collection = %q", row.Policy.Collection)
	}
}

func TestSubmitResolverProfileAndUnknownValidation(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	svc.Config.Browser.Resolvers = map[string]config.Institution{
		"example":   {OpenURLBase: "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE"},
		"institute": {OpenURLBase: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
	}
	request := doiRequest("wr_resolver_profile")
	request.Resolver = "institute"
	id, err := svc.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Policy.Resolver != "institute" {
		t.Fatalf("resolver policy = %q", row.Policy.Resolver)
	}
	svc.Config.Browser.DefaultResolver = "institute"
	request.RequestID = "wr_default_resolver"
	request.Resolver = ""
	defaultID, err := svc.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	defaultRow, err := jobs.Get(context.Background(), defaultID)
	if err != nil {
		t.Fatal(err)
	}
	if defaultRow.Policy.Resolver != "institute" {
		t.Fatalf("default resolver policy = %q, want institute", defaultRow.Policy.Resolver)
	}
	request.RequestID = "wr_unknown_resolver"
	request.Resolver = "missing"
	_, err = svc.Submit(context.Background(), request)
	if err == nil {
		t.Fatal("unknown resolver accepted")
	}
	for _, profile := range []string{"default", "example", "institute"} {
		if !strings.Contains(err.Error(), profile) {
			t.Fatalf("unknown resolver error %q does not list %q", err, profile)
		}
	}
}

type fakeNotificationSink struct {
	human, imported int
	reminders       []string
	intents         []notify.Intent
}

func (f *fakeNotificationSink) Route(_ context.Context, intent notify.Intent) error {
	f.intents = append(f.intents, intent)
	switch intent.Category {
	case notify.CategoryDecisionOpened:
		f.human++
	case notify.CategoryDecisionPending:
		f.reminders = append(f.reminders, intent.Message)
	}
	return nil
}

func TestParkNotifiesAfterSuccessfulTransition(t *testing.T) {
	svc, jobs := newTestService(t)
	notifier := &fakeNotificationSink{}
	svc.Notifier = notifier
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequest("wr_park_notification"))
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, map[string]any{"reason": "test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.park(ctx, id, job.StateResolving, job.StateAwaitingHuman, map[string]any{"reason": "test"}); err != nil {
		t.Fatal(err)
	}
	if notifier.human != 1 {
		t.Fatalf("human notifications = %d, want 1", notifier.human)
	}
}

func TestImportNeedsRetry(t *testing.T) {
	ev := func(status string) map[string]any {
		return map[string]any{"kind": "zotio.auto_import", "detail": map[string]any{"status": status}}
	}
	errN := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = ev("error")
		}
		return out
	}
	cases := []struct {
		name   string
		events []map[string]any
		want   bool
	}{
		{"missing event", nil, true},
		{"applied", []map[string]any{ev("applied")}, false},
		{"no_op", []map[string]any{ev("no_op")}, false},
		{"duplicate", []map[string]any{ev("duplicate")}, false},
		{"skipped retries", []map[string]any{ev("skipped")}, true},
		{"error then applied wins", []map[string]any{ev("error"), ev("applied")}, false},
		{"under cap", errN(maxImportAttempts - 1), true},
		{"at cap gives up", errN(maxImportAttempts), false},
	}
	for _, c := range cases {
		if got := importNeedsRetry(c.events); got != c.want {
			t.Errorf("%s: importNeedsRetry = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetryPendingImportsIsNoOpWithoutImporter(t *testing.T) {
	svc, _ := newTestService(t)
	svc.AutoImporter = nil
	if err := svc.retryPendingImports(context.Background()); err != nil {
		t.Fatalf("retry with no importer: %v", err)
	}
}

func TestRetryPendingImportsRedrivesFailedImportUntilCap(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	importer := &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("zotio stderr: transient outage"))}
	svc.AutoImporter = importer
	readyPipeline(svc)

	id, err := svc.Submit(ctx, doiRequest("wr_retry_cap"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	if importer.calls != 1 {
		t.Fatalf("inline import calls = %d, want 1", importer.calls)
	}

	// Ready is terminal, so only the retry pass re-drives the failing import.
	// It must re-drive up to the attempt cap, then give up.
	for range 10 {
		if err := svc.retryPendingImports(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if importer.calls != maxImportAttempts {
		t.Fatalf("PlanAndApply calls = %d, want cap %d", importer.calls, maxImportAttempts)
	}
	ready, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady {
		t.Fatalf("job state after retries = %s, want ready (PDF stays a validated artifact)", ready.State)
	}
}

func TestRetryPendingImportsStopsAfterSuccess(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	importer := &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("zotio stderr: transient outage"))}
	svc.AutoImporter = importer
	readyPipeline(svc)

	id, err := svc.Submit(ctx, doiRequest("wr_retry_ok"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}

	// Zotio recovers: the next retry imports, and no further retry re-drives it.
	importer.err = nil
	importer.status = "applied"
	importer.parentKey = "PARENT01"
	importer.attachmentKey = "ATTACH01"
	if err := svc.retryPendingImports(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.retryPendingImports(ctx); err != nil {
		t.Fatal(err)
	}
	if importer.calls != 2 {
		t.Fatalf("PlanAndApply calls = %d, want 2 (one inline failure + one successful retry)", importer.calls)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if importNeedsRetry(events) {
		t.Fatal("a successfully imported job must not need another retry")
	}
}

func TestResolverRetryBudgetEscalatesAfterCap(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t) // conservative mode: exhaustion escalates to unavailable
	svc.RetryDelay = time.Millisecond
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/p.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}, Policy: config.Source{Enabled: true}}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassRetryable, HTTPStatus: 503, RetryAfter: time.Millisecond, Msg: "service unavailable"}
	}
	svc.Validate = passValidation()

	id, err := svc.Submit(ctx, doiRequest("wr_retry_budget"))
	if err != nil {
		t.Fatal(err)
	}
	// Drive the job repeatedly through its temporary-failure retry cycle. It
	// must retry exactly maxRetryAttempts times, then escalate instead of
	// cycling retry_wait forever.
	retryWaits := 0
	for range maxRetryAttempts + 4 {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateQueued && row.State != job.StateRetryWait {
			break
		}
		if err := svc.Process(ctx, row); err != nil {
			t.Fatal(err)
		}
		if after, _ := jobs.Get(ctx, id); after.State == job.StateRetryWait {
			retryWaits++
		}
	}
	if retryWaits != maxRetryAttempts {
		t.Fatalf("retry_wait cycles = %d, want cap %d", retryWaits, maxRetryAttempts)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateUnavailable {
		t.Fatalf("state after exhausting retry budget = %s, want unavailable (escalated, not retry_wait forever)", row.State)
	}
}

// A pass that only met closed source gates made no request, so it must not
// spend the bounded retry budget. Before this, a day-long OpenAlex quota gate
// alongside ordinary thirty-second gates burned all eight attempts within
// minutes and settled real jobs "temporary source failures did not clear" —
// naming a source that had never been called. Observed live on 8 jobs of a
// 309-job cohort.
func TestSourceGateParksDoNotSpendTheRetryBudget(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Budgets = budget.New(jobs.S)
	gate := time.Now().UTC().Add(18 * time.Hour)
	if err := svc.Budgets.Defer(ctx, "fixture", config.Source{Enabled: true}, gate); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeResolver{name: "fixture"}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, errors.New("fetch must not run: the only source is gated")
	}
	svc.Validate = passValidation()
	id, err := svc.Submit(ctx, doiRequest("wr_gate_budget"))
	if err != nil {
		t.Fatal(err)
	}
	for range maxRetryAttempts + 4 {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateQueued && row.State != job.StateRetryWait {
			t.Fatalf("job left the retry cycle in %s; a closed gate is not a verdict", row.State)
		}
		if err := svc.Process(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateRetryWait {
		t.Fatalf("state after %d gate parks = %s, want retry_wait", maxRetryAttempts+4, row.State)
	}
	if adapter.calls != 0 {
		t.Fatalf("gated adapter was called %d times, want 0", adapter.calls)
	}
	parked, err := time.Parse(time.RFC3339Nano, row.RetryAt)
	if err != nil {
		t.Fatalf("retry_at %q: %v", row.RetryAt, err)
	}
	if parked.Sub(gate).Abs() > time.Minute {
		t.Fatalf("retry_at = %s, want the source gate %s", parked, gate)
	}
}

// The budget may legitimately run out on real temporary failures while a source
// gate is still closed. The gated source has never had its one call, so it gets
// exactly ONE post-exhaustion wait — and no more. An unlimited wait is not a
// safe generalisation: a temporary failure defers its own source, so a job
// failing for real keeps refreshing the gate that excuses it. Seen live at 41
// temporary transitions against a bound of 8, re-parking every thirty seconds
// forever, on a job that should have settled 33 cycles earlier.
func TestPendingGateOutranksAnExhaustedRetryBudget(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Budgets = budget.New(jobs.S)
	gate := time.Now().UTC().Add(18 * time.Hour)
	if err := svc.Budgets.Defer(ctx, "gated", config.Source{Enabled: true}, gate); err != nil {
		t.Fatal(err)
	}
	svc.RetryDelay = time.Millisecond
	flaky := &fakeResolver{name: "fixture", err: &resolver.TemporaryError{Err: errors.New("upstream blip")}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: flaky, Policy: config.Source{Enabled: true}},
		{Adapter: &fakeResolver{name: "gated"}, Policy: config.Source{Enabled: true}},
	}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, errors.New("no candidate should reach fetch")
	}
	svc.Validate = passValidation()
	svc.Config.Sources["gated"] = config.Source{Enabled: true}
	id, err := svc.Submit(ctx, doiRequest("wr_gate_outranks"))
	if err != nil {
		t.Fatal(err)
	}
	// Drive to exhaustion; the one allowed post-exhaustion wait parks at the gate.
	var parkedAt string
	for range maxRetryAttempts + 4 {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateQueued && row.State != job.StateRetryWait {
			break
		}
		if err := svc.Process(ctx, row); err != nil {
			t.Fatal(err)
		}
		if after, _ := jobs.Get(ctx, id); after != nil && after.State == job.StateRetryWait {
			parkedAt = after.RetryAt
		}
	}
	parked, err := time.Parse(time.RFC3339Nano, parkedAt)
	if err != nil {
		t.Fatalf("retry_at %q: %v", parkedAt, err)
	}
	if parked.Sub(gate).Abs() > time.Minute {
		t.Fatalf("retry_at = %s, want the pending gate %s once the temporary budget is spent", parked, gate)
	}
	// A second post-exhaustion wait would mean the gate is being refreshed by
	// the failures rather than waited out, so the job must settle instead.
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !job.Terminal(row.State) {
		t.Fatalf("state = %s; a gate the job's own failures keep refreshing must not defer the verdict forever", row.State)
	}
}

func hookEvent(t *testing.T, jobs *job.Store, id string) map[string]any {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "hook.on_ready" {
			detail, ok := event["detail"].(map[string]any)
			if !ok {
				t.Fatalf("hook event detail = %#v", event["detail"])
			}
			return detail
		}
	}
	return nil
}

func waitForHookEvent(t *testing.T, jobs *job.Store, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if detail := hookEvent(t, jobs, id); detail != nil {
			return detail
		}
		if time.Now().After(deadline) {
			t.Fatal("no hook.on_ready event recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessReadyFiresOnReadyHookOnce(t *testing.T) {
	svc, jobs := newTestService(t)
	readyPipeline(svc)
	var mu sync.Mutex
	var envs [][]string
	svc.ReadyHook = &hook.Runner{
		Command: "configured",
		Exec: func(_ context.Context, _ string, env []string) hook.Result {
			mu.Lock()
			envs = append(envs, env)
			mu.Unlock()
			return hook.Result{Ran: true, ExitCode: 0}
		},
	}

	id, err := svc.Submit(context.Background(), doiRequest("wr_hook_ok_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	ready, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady {
		t.Fatalf("job state = %s, want ready", ready.State)
	}
	detail := waitForHookEvent(t, jobs, id)
	if detail["status"] != "ok" {
		t.Fatalf("hook detail = %#v, want status ok", detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(envs) != 1 {
		t.Fatalf("hook invocations = %d, want 1", len(envs))
	}
	byKey := map[string]string{}
	for _, kv := range envs[0] {
		key, value, _ := strings.Cut(kv, "=")
		byKey[key] = value
	}
	if byKey["PAPIO_DOI"] != "10.1002/example" || byKey["PAPIO_JOB_ID"] != id ||
		byKey["PAPIO_STATE"] != "ready" || byKey["PAPIO_SHA256"] != ready.ArtifactSHA256 ||
		!strings.HasSuffix(byKey["PAPIO_PDF"], ready.ArtifactSHA256+".pdf") {
		t.Fatalf("hook env = %#v", byKey)
	}
}

func TestProcessReadyHookExposesPMIDOnlyWork(t *testing.T) {
	svc, jobs := newTestService(t)
	readyPipeline(svc)
	envs := make(chan []string, 1)
	svc.ReadyHook = &hook.Runner{
		Command: "configured",
		Exec: func(_ context.Context, _ string, env []string) hook.Result {
			envs <- env
			return hook.Result{Ran: true, ExitCode: 0}
		},
	}

	id, err := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "wr_hook_pmid_01",
		Identifiers:   &protocol.Identifiers{PMID: "12345678"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if detail := waitForHookEvent(t, jobs, id); detail["status"] != "ok" {
		t.Fatalf("hook detail = %#v, want status ok", detail)
	}
	byKey := map[string]string{}
	for _, kv := range <-envs {
		key, value, _ := strings.Cut(kv, "=")
		byKey[key] = value
	}
	if byKey["PAPIO_DOI"] != "" || byKey["PAPIO_ARXIV"] != "" || byKey["PAPIO_PMID"] != "12345678" {
		t.Fatalf("hook identifiers = DOI %q, arXiv %q, PMID %q", byKey["PAPIO_DOI"], byKey["PAPIO_ARXIV"], byKey["PAPIO_PMID"])
	}
}

func TestOnReadyHookFailureLeavesJobReady(t *testing.T) {
	svc, jobs := newTestService(t)
	readyPipeline(svc)
	svc.ReadyHook = &hook.Runner{
		Command: "configured",
		Exec: func(_ context.Context, _ string, _ []string) hook.Result {
			return hook.Result{Ran: true, ExitCode: 1, StderrTail: "boom"}
		},
	}

	id, err := svc.Submit(context.Background(), doiRequest("wr_hook_fail_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatalf("hook failure must be non-fatal: %v", err)
	}
	detail := waitForHookEvent(t, jobs, id)
	if detail["status"] != "error" {
		t.Fatalf("hook failure detail = %#v", detail)
	}
	if _, leaked := detail["stderr_tail"]; leaked {
		t.Fatalf("raw hook stderr persisted to a durable event: %#v", detail)
	}
	ready, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady {
		t.Fatalf("job state = %s, want ready despite hook failure", ready.State)
	}
}

// retryWaitDetail returns the detail of the job's most recent transition
// into retry_wait, so tests can inspect what a park actually reported.
func retryWaitDetail(t *testing.T, jobs *job.Store, id string) map[string]any {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "job.transition" {
			continue
		}
		detail, ok := events[i]["detail"].(map[string]any)
		if !ok {
			continue
		}
		if to, _ := detail["to"].(string); to == job.StateRetryWait {
			return detail
		}
	}
	t.Fatal("no retry_wait transition event recorded")
	return nil
}

// Regression coverage for the DOI 10.3389/feduc.2018.00095 incident: both
// candidates failed permanently (403, classified invalid) while an unrelated
// resolver was the thing actually temporary, yet every park asserted
// "candidate_temporarily_unavailable" — a cause the pass never observed. The
// retry_wait detail must now name what actually happened instead.
func TestRetryPlanReportsWhatItObserved(t *testing.T) {
	t.Run("permanent_candidate_and_temporary_resolver", func(t *testing.T) {
		svc, jobs := newTestService(t)
		ctx := context.Background()
		svc.Config.Sources["flaky"] = config.Source{Enabled: true}
		working := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
			Source: "fixture", URL: "https://example.test/expired-sas.pdf", Version: resolver.VersionPublished,
			AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
		}}}
		flaky := &fakeResolver{name: "flaky", err: &resolver.TemporaryError{Err: errors.New("upstream rate limited")}}
		svc.Resolvers = []ResolverEntry{
			{Adapter: working, Policy: config.Source{Enabled: true}},
			{Adapter: flaky, Policy: config.Source{Enabled: true}},
		}
		// The publisher link's signature is expired: every fetch is a
		// permanent 403, never a retryable one — the candidate is dead, not
		// waiting.
		svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
			return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "sas token expired"}
		}
		svc.Validate = passValidation()
		id, err := svc.Submit(ctx, doiRequest("wr_observed_mixed"))
		if err != nil {
			t.Fatal(err)
		}
		row, err := jobs.ClaimNext(ctx, "w", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Process(ctx, row); err != nil {
			t.Fatal(err)
		}
		got, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != job.StateRetryWait {
			t.Fatalf("state = %s, want retry_wait", got.State)
		}
		detail := retryWaitDetail(t, jobs, id)
		if detail["reason"] != "acquisition_inputs_temporarily_unavailable" {
			t.Fatalf("reason = %v, want acquisition_inputs_temporarily_unavailable", detail["reason"])
		}
		if n, _ := detail["retryable_candidates"].(float64); n != 0 {
			t.Fatalf("retryable_candidates = %v, want 0 (the candidate failed permanently)", detail["retryable_candidates"])
		}
		if n, _ := detail["temporary_resolvers"].(float64); n != 1 {
			t.Fatalf("temporary_resolvers = %v, want 1", detail["temporary_resolvers"])
		}
		if n, _ := detail["closed_source_gates"].(float64); n != 0 {
			t.Fatalf("closed_source_gates = %v, want 0", detail["closed_source_gates"])
		}
	})

	t.Run("pure_gate_pass_not_counted_against_budget", func(t *testing.T) {
		svc, jobs := newTestService(t)
		ctx := context.Background()
		svc.RetryDelay = time.Millisecond
		svc.Budgets = budget.New(jobs.S)
		gate := time.Now().UTC().Add(18 * time.Hour)
		if err := svc.Budgets.Defer(ctx, "fixture", config.Source{Enabled: true}, gate); err != nil {
			t.Fatal(err)
		}
		adapter := &fakeResolver{name: "fixture"}
		svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
		svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
			return fetch.Result{}, errors.New("fetch must not run: the only source is gated")
		}
		svc.Validate = passValidation()
		id, err := svc.Submit(ctx, doiRequest("wr_observed_gate"))
		if err != nil {
			t.Fatal(err)
		}
		// Drive it well past the retry budget cap; a pass that made no
		// request must never be counted against it.
		for range maxRetryAttempts + 4 {
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateQueued && row.State != job.StateRetryWait {
				t.Fatalf("job left the retry cycle in %s; a closed gate is not a verdict", row.State)
			}
			if err := svc.Process(ctx, row); err != nil {
				t.Fatal(err)
			}
		}
		got, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != job.StateRetryWait {
			t.Fatalf("state after %d gate parks = %s, want retry_wait (never exhausted)", maxRetryAttempts+4, got.State)
		}
		detail := retryWaitDetail(t, jobs, id)
		if detail["retry_kind"] != retryKindSourceGate {
			t.Fatalf("retry_kind = %v, want %q", detail["retry_kind"], retryKindSourceGate)
		}
	})

	t.Run("wake_time_is_earliest_of_all_three_observations", func(t *testing.T) {
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		plan := retryPlan{
			CandidateTemporary: base.Add(3 * time.Minute),
			ResolverTemporary:  base.Add(1 * time.Minute), // earliest
			Gate:               base.Add(2 * time.Minute),
		}
		want := base.Add(1 * time.Minute)
		if got := plan.At(); !got.Equal(want) {
			t.Fatalf("At() = %v, want earliest observation %v", got, want)
		}
		// Splitting Temporary into two fields must not change what a caller
		// that only cares "when" sees.
		if got := plan.Temporary(); !got.Equal(want) {
			t.Fatalf("Temporary() = %v, want %v", got, want)
		}
	})

	// Verified in production 2026-08-12: openaire's process-local token bucket
	// refused a request every few seconds while openalex sat behind a real
	// 24-hour quota gate. Both surfaced as budget.ErrDeferred, the plan kept
	// only the earliest, and every job woke on the token bucket, re-ran every
	// source, learned nothing and re-parked — 10,437 durable transitions in 97
	// minutes across 82 jobs, uncharged because a source_gate park spends no
	// attempt. The durable gate is the only honest wake time here.
	t.Run("advisory_throttle_never_outranks_a_durable_gate", func(t *testing.T) {
		base := time.Date(2026, 8, 12, 1, 37, 0, 0, time.UTC)
		plan := retryPlan{}
		plan.recordDeferral(&budget.ErrDeferred{
			Source: "openalex", Until: base.Add(23 * time.Hour),
		})
		plan.recordDeferral(&budget.ErrDeferred{
			Source: "openaire", Until: base.Add(5 * time.Second), Advisory: true,
		})
		if got, want := plan.At(), base.Add(23*time.Hour); !got.Equal(want) {
			t.Fatalf("At() = %v, want the durable gate %v", got, want)
		}
		if plan.ClosedSourceGates != 1 || plan.AdvisoryBackoffs != 1 {
			t.Fatalf("counters = %d gates / %d advisory, want 1/1",
				plan.ClosedSourceGates, plan.AdvisoryBackoffs)
		}
		if plan.AdvisoryOnly() {
			t.Fatal("a pass holding a durable gate is not advisory-only")
		}
		if plan.Kind() != retryKindSourceGate {
			t.Fatalf("Kind() = %q, want %q", plan.Kind(), retryKindSourceGate)
		}
	})

	// With no durable gate the throttle is the only observation. It is not a
	// wake time — sub-second parks are the spin itself — so the job falls back
	// to the ordinary retry cadence. It is also not an attempt: no request was
	// made, so charging it would let papio's own throttle settle a job
	// "temporary source failures did not clear" about sources it never called.
	// Liveness comes from the cadence floor, not from the budget.
	// The exemption must be narrow. A pass where sources WERE reached and simply
	// had nothing is a real answer: it stays chargeable, so the retry budget can
	// still bound it and the job can eventually settle. Exempting it turned an
	// answered-but-empty pass into an uncharged 30-second re-park forever.
	t.Run("a_pass_that_reached_sources_is_still_charged", func(t *testing.T) {
		now := time.Date(2026, 8, 12, 1, 37, 0, 0, time.UTC)
		plan := retryPlan{SourcesCalled: 2}
		plan.recordDeferral(&budget.ErrDeferred{
			Source: "openaire", Until: now.Add(3 * time.Second), Advisory: true,
		})
		if plan.AdvisoryOnly() {
			t.Fatal("a pass that called sources is not advisory-only")
		}
		if plan.Kind() != retryKindTemporary {
			t.Fatalf("Kind() = %q, want %q so the retry budget bounds it", plan.Kind(), retryKindTemporary)
		}
		if plan.IsZero() {
			t.Fatal("a throttled source papio never asked must not settle the job")
		}
	})

	t.Run("advisory_only_pass_uses_retry_cadence_and_is_not_charged", func(t *testing.T) {
		svc, jobs := newTestService(t)
		ctx := context.Background()
		now := time.Date(2026, 8, 12, 1, 37, 0, 0, time.UTC)
		svc.Now = func() time.Time { return now }
		svc.RetryDelay = 30 * time.Second
		id, err := svc.Submit(ctx, doiRequest("wr_advisory_only"))
		if err != nil {
			t.Fatal(err)
		}
		if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving,
			map[string]any{"reason": "scheduler_dispatch"}); err != nil {
			t.Fatal(err)
		}
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		plan := retryPlan{}
		plan.recordDeferral(&budget.ErrDeferred{
			Source: "openaire", Until: now.Add(3 * time.Second), Advisory: true,
		})
		if !plan.AdvisoryOnly() || plan.IsZero() {
			t.Fatalf("advisory-only plan = %+v, want advisory-only and non-zero", plan)
		}
		if plan.Kind() != retryKindAdvisory {
			t.Fatalf("Kind() = %q, want %q", plan.Kind(), retryKindAdvisory)
		}
		if err := svc.parkForRetry(ctx, row, job.StateResolving, plan,
			map[string]any{"reason": "resolver_temporarily_unavailable"},
			job.TerminalReasonTemporarySourceFailuresDidNotClear, ""); err != nil {
			t.Fatal(err)
		}
		got, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		retryAt, err := time.Parse(time.RFC3339Nano, got.RetryAt)
		if err != nil {
			t.Fatalf("parsing retry_at %q: %v", got.RetryAt, err)
		}
		if wake := retryAt.Sub(now); wake < svc.RetryDelay {
			t.Fatalf("retry_at is %v out, want at least the %v retry cadence", wake, svc.RetryDelay)
		}
		// The defect this guards: charging advisory parks let eight of them —
		// four minutes at a 30s cadence — exhaust the budget and settle the job
		// as though its sources had failed.
		for range maxRetryAttempts + 2 {
			cur, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if cur.State != job.StateRetryWait {
				t.Fatalf("job left the retry cycle in %s; a self-inflicted throttle is not a verdict", cur.State)
			}
			if err := jobs.Transition(ctx, id, job.StateRetryWait, job.StateResolving,
				map[string]any{"reason": "scheduler_dispatch"}); err != nil {
				t.Fatal(err)
			}
			again, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.parkForRetry(ctx, again, job.StateResolving, plan,
				map[string]any{"reason": "resolver_temporarily_unavailable"},
				job.TerminalReasonTemporarySourceFailuresDidNotClear, ""); err != nil {
				t.Fatal(err)
			}
		}
		if svc.retryBudgetExhausted(ctx, id) {
			t.Fatal("advisory parks exhausted the retry budget; no request was ever made")
		}
	})
}

// onceTemporaryResolver stands in for the "unrelated temporary source/gate"
// that populated the pass-wide retry plan in the verified production
// incident: it fails once (a transient outage), then clears, contributing no
// candidates either way in both calls.
type onceTemporaryResolver struct {
	name  string
	calls int
}

func (r *onceTemporaryResolver) Name() string { return r.name }
func (r *onceTemporaryResolver) Resolve(context.Context, work.Work) ([]resolver.Candidate, error) {
	r.calls++
	if r.calls == 1 {
		return nil, &resolver.TemporaryError{Err: errors.New("gate down"), RetryAfter: time.Millisecond}
	}
	return nil, nil
}

// runOABrowserHintFixture reproduces the verified incident
// (10.3389/feduc.2018.00095: fully open access, expired Azure SAS link) over
// two passes. Pass 1's only candidate 403s -- classified permanently, so
// MarkCandidate leaves it "invalid" -- while an unrelated resolver gate fails
// temporarily, so the pass parks instead of exhausting immediately, exactly
// like the mislabelled "candidate_temporarily_unavailable" parks observed
// live. Pass 2's gate has cleared and the OA candidate is already invalid, so
// NextPendingCandidate returns nothing and the fetch loop -- and
// isOABrowserBlocked with it -- never runs again.
func runOABrowserHintFixture(t *testing.T, requestID, oaURL string) (*Service, *job.Store, string) {
	t.Helper()
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
	svc.RetryDelay = time.Millisecond
	oa := &fakeResolver{name: "oa", cands: []resolver.Candidate{{
		Source: "oa", URL: oaURL, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	gate := &onceTemporaryResolver{name: "gate"}
	svc.Resolvers = []ResolverEntry{
		{Adapter: oa, Policy: config.Source{Enabled: true}},
		{Adapter: gate, Policy: config.Source{Enabled: true}},
	}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "forbidden"}
	}
	svc.Validate = passValidation()

	id, err := svc.Submit(ctx, doiRequest(requestID))
	if err != nil {
		t.Fatal(err)
	}

	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	afterPass1, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if afterPass1.State != job.StateRetryWait {
		t.Fatalf("pass 1 state = %s, want retry_wait (unrelated gate should have parked, not exhausted, the job)", afterPass1.State)
	}

	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	return svc, jobs, id
}

// This is the incident itself: without the durable hint, pass 2's empty
// pending queue never re-runs isOABrowserBlocked, oaBrowserURL resets to ""
// in memory, and exhaustedCandidates falls through to an institutional
// OpenURL handoff for a paper that needs no institution.
func TestOABrowserHintSurvivesEmptyPendingQueueOnLaterPass(t *testing.T) {
	const oaURL = "https://example.test/oa-survives.pdf"
	_, jobs, id := runOABrowserHintFixture(t, "wr_oa_hint_survives", oaURL)
	ctx := context.Background()

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("pass 2 state = %s, want awaiting_human", got.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != "openurl_handoff" {
		t.Fatalf("actions = %+v, want one openurl_handoff", actions)
	}
	// The routing decision on pass 2 must match pass 1's: open-access browser
	// handoff, never the institutional OpenURL sign-in this paper never needed.
	if actions[0].Detail != OABrowserHandoffActionDetail(oaURL) {
		t.Fatalf("handoff detail = %q, want OA browser handoff for %q (institutional handoff would mean the hint was lost)", actions[0].Detail, oaURL)
	}
	if actions[0].RequiresAuth {
		t.Fatalf("OA browser handoff must not require auth: %+v", actions[0])
	}
}

// safeType, redacted candidate rows, and now the oa_browser_hint event all
// exist so upstream bearer URLs never reach durable storage. This asserts on
// the actual event detail contents, not just the event kind, so a future
// change that widens the hint payload to "just store the URL" is caught here
// rather than in production.
func TestOABrowserHintEventNeverStoresBearerURL(t *testing.T) {
	const oaURL = "https://example.test/oa-secret.pdf?sig=SHOULD_NEVER_PERSIST"
	_, jobs, id := runOABrowserHintFixture(t, "wr_oa_hint_no_leak", oaURL)
	ctx := context.Background()

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), oaURL) || strings.Contains(string(encoded), "SHOULD_NEVER_PERSIST") {
		t.Fatalf("a job event leaked the bearer OA URL: %s", encoded)
	}

	found := false
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != oaBrowserHintEventKind {
			continue
		}
		found = true
		detail, _ := event["detail"].(map[string]any)
		if _, hasURL := detail["url"]; hasURL {
			t.Fatalf("oa_browser_hint event detail carries a url field: %+v", detail)
		}
		urlKey, _ := detail["url_key"].(string)
		if urlKey == "" {
			t.Fatalf("oa_browser_hint event detail missing url_key: %+v", detail)
		}
	}
	if !found {
		t.Fatalf("expected an %s event, events = %+v", oaBrowserHintEventKind, events)
	}
}

// fakeLandingReader stands in for internal/landingmeta.Reader: it maps a
// landing URL to the citation_pdf_url the fixture page would advertise,
// without any real network I/O.
type fakeLandingReader struct {
	pdfURLFor map[string]string
	err       error
	calls     int
}

func (f *fakeLandingReader) PDFURLFor(_ context.Context, landingURL string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.pdfURLFor[landingURL], nil
}

// landingExpansionFetch classifies every candidate URL other than pdfURL as
// the incident's dead, permanently-403ing publisher link, and only succeeds
// for the derived citation_pdf_url candidate — so a passing test proves the
// job actually walked through expansion rather than acquiring by luck.
func landingExpansionFetch(pdfURL string) FetchFunc {
	return func(_ context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		if c.URL != pdfURL {
			return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "sas token expired"}
		}
		body := pdfBytes(c.URL)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		return fetch.Result{
			TempPath: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
			FinalHost: "frontiersin.example",
		}, nil
	}
}

// TestLandingExpansionRecoversDeadOpenAccessPDF reproduces the verified
// incident (DOI 10.3389/feduc.2018.00095): Unpaywall and OpenAlex both
// return the SAME dead, SAS-expired publisher URL as separate open-access
// candidates, both carrying the same doi.org Landing URL. Both 403
// permanently, but that landing page advertises a working citation_pdf_url,
// and the job must acquire it in the same Process pass instead of parking
// or falling through to an institutional handoff. Two dead candidates
// sharing one landing page must also cost exactly one landing GET, not one
// per candidate.
func TestLandingExpansionRecoversDeadOpenAccessPDF(t *testing.T) {
	const deadURL = "https://blob.example/expired-sas.pdf?se=2021-02-16"
	const landingURL = "https://doi.org/10.3389/feduc.2018.00095"
	const pdfURL = "https://frontiersin.example/articles/pdf/valid.pdf"

	svc, jobs := newTestService(t)
	ctx := context.Background()
	unpaywall := &fakeResolver{name: "unpaywall", cands: []resolver.Candidate{{
		Source: "unpaywall", URL: deadURL, Landing: landingURL, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0", Direct: true, IdentityConfidence: 1,
	}}}
	openalex := &fakeResolver{name: "openalex", cands: []resolver.Candidate{{
		Source: "openalex", URL: deadURL, Landing: landingURL, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: unpaywall, Policy: config.Source{Enabled: true}},
		{Adapter: openalex, Policy: config.Source{Enabled: true}},
	}
	svc.Fetch = landingExpansionFetch(pdfURL)
	svc.Validate = passValidation()
	reader := &fakeLandingReader{pdfURLFor: map[string]string{landingURL: pdfURL}}
	svc.LandingReader = reader

	id, err := svc.Submit(ctx, doiRequest("wr_landing_expansion_recovers"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("state = %s, want ready (the job had a working recovery route and must not park)", got.State)
	}
	if reader.calls != 1 {
		t.Fatalf("landing reader called %d times, want 1 (both dead candidates share one landing page)", reader.calls)
	}
}

// TestLandingExpansionNilReaderLeavesParkingUnchanged asserts the nil-safety
// contract: without a wired LandingReader, a job with the exact shape of the
// verified incident (permanent open-access 403 plus an unrelated temporary
// resolver) must park exactly as it did before this feature existed, byte
// for byte — same state, same reason, same observation counts.
func TestLandingExpansionNilReaderLeavesParkingUnchanged(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.Config.Sources["flaky"] = config.Source{Enabled: true}
	const landingURL = "https://doi.org/10.3389/feduc.2018.00095"
	working := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/expired-sas.pdf", Landing: landingURL, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	flaky := &fakeResolver{name: "flaky", err: &resolver.TemporaryError{Err: errors.New("upstream rate limited")}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: working, Policy: config.Source{Enabled: true}},
		{Adapter: flaky, Policy: config.Source{Enabled: true}},
	}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "sas token expired"}
	}
	svc.Validate = passValidation()
	// svc.LandingReader is left nil: expansion must be a strict no-op.

	id, err := svc.Submit(ctx, doiRequest("wr_landing_expansion_nil_reader"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait (nil reader must change nothing)", got.State)
	}
	detail := retryWaitDetail(t, jobs, id)
	if detail["reason"] != "acquisition_inputs_temporarily_unavailable" {
		t.Fatalf("reason = %v, want acquisition_inputs_temporarily_unavailable", detail["reason"])
	}
	if n, _ := detail["retryable_candidates"].(float64); n != 0 {
		t.Fatalf("retryable_candidates = %v, want 0", detail["retryable_candidates"])
	}
	if n, _ := detail["temporary_resolvers"].(float64); n != 1 {
		t.Fatalf("temporary_resolvers = %v, want 1", detail["temporary_resolvers"])
	}
	if n, _ := detail["closed_source_gates"].(float64); n != 0 {
		t.Fatalf("closed_source_gates = %v, want 0", detail["closed_source_gates"])
	}
}

// TestLandingExpansionEventNeverStoresBearerURLs asserts the same
// redacted-URL discipline as TestOABrowserHintEventNeverStoresBearerURL: the
// landing_derived provenance event, and every other event this Process call
// records, must carry the parent's url_key only — never the dead candidate
// URL, the landing URL, or the derived PDF URL.
func TestLandingExpansionEventNeverStoresBearerURLs(t *testing.T) {
	const deadURL = "https://blob.example/expired.pdf?sig=DEAD_SHOULD_NEVER_PERSIST"
	const landingURL = "https://doi.org/10.3389/feduc.2018.00095?ref=LANDING_SHOULD_NEVER_PERSIST"
	const pdfURL = "https://frontiersin.example/valid.pdf?tok=PDF_SHOULD_NEVER_PERSIST"

	svc, jobs := newTestService(t)
	ctx := context.Background()
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: deadURL, Landing: landingURL, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	svc.Fetch = landingExpansionFetch(pdfURL)
	svc.Validate = passValidation()
	svc.LandingReader = &fakeLandingReader{pdfURLFor: map[string]string{landingURL: pdfURL}}

	id, err := svc.Submit(ctx, doiRequest("wr_landing_expansion_no_leak"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("state = %s, want ready", got.State)
	}

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{deadURL, landingURL, pdfURL, "SHOULD_NEVER_PERSIST"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("a job event leaked a bearer URL (%q): %s", leaked, encoded)
		}
	}

	found := false
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != landingDerivedEventKind {
			continue
		}
		found = true
		detail, _ := event["detail"].(map[string]any)
		if detail["derived"] != "citation_pdf_url" {
			t.Fatalf("landing_derived event detail = %+v, want derived=citation_pdf_url", detail)
		}
		if _, hasKey := detail["parent_url_key"]; !hasKey {
			t.Fatalf("landing_derived event detail missing parent_url_key: %+v", detail)
		}
	}
	if !found {
		t.Fatalf("expected a %s event, events = %+v", landingDerivedEventKind, events)
	}
}

// TestLandingExpansionSurvivesRetryableDerivedFetchFailure is the fix for
// the defect expandLandingSeeds' original tried-map comment falsified: a
// landing-derived candidate is never reproduced by any resolver, so once
// live is rebuilt fresh on the next pass it has no entry for the derived
// candidate's url_key. A 503 on pass 1 flips it retryable, ResetCandidates
// flips it back to pending on pass 2, and — without the fix —
// NextPendingCandidate hands back a url_key live can't find, so it is
// marked skipped, which ResetCandidates never revives: the recovery this
// whole mechanism exists to provide is permanently erased by one transient
// failure, and the job falls back to an institutional OpenURL handoff for a
// paper that needs no institution. This asserts the opposite: pass 2 must
// re-read the same landing page (rederivableLandingSeeds), rediscover the
// identical PDF URL, and let the pending-queue loop fetch it again — at
// most once per pass, and without ever opening that handoff.
func TestLandingExpansionSurvivesRetryableDerivedFetchFailure(t *testing.T) {
	const deadURL = "https://blob.example/expired-sas.pdf?se=2021-02-16"
	const landingURL = "https://doi.org/10.3389/feduc.2018.00095"
	const pdfURL = "https://frontiersin.example/articles/pdf/valid.pdf"

	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.RetryDelay = time.Millisecond
	unpaywall := &fakeResolver{name: "unpaywall", cands: []resolver.Candidate{{
		Source: "unpaywall", URL: deadURL, Landing: landingURL, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: unpaywall, Policy: config.Source{Enabled: true}}}

	pdfFetchAttempts := 0
	svc.Fetch = func(_ context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		if c.URL != pdfURL {
			return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "sas token expired"}
		}
		pdfFetchAttempts++
		if pdfFetchAttempts == 1 {
			return fetch.Result{}, &fetch.Error{Class: fetch.ClassRetryable, HTTPStatus: 503, Msg: "upstream unavailable"}
		}
		body := pdfBytes(c.URL)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		return fetch.Result{
			TempPath: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
			FinalHost: "frontiersin.example",
		}, nil
	}
	svc.Validate = passValidation()
	reader := &fakeLandingReader{pdfURLFor: map[string]string{landingURL: pdfURL}}
	svc.LandingReader = reader

	id, err := svc.Submit(ctx, doiRequest("wr_landing_expansion_retryable_derived"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	afterPass1, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if afterPass1.State != job.StateRetryWait {
		t.Fatalf("pass 1 state = %s, want retry_wait (a 503 on the derived candidate must park for retry, not exhaust or park as manual)", afterPass1.State)
	}
	if reader.calls != 1 {
		t.Fatalf("landing reader called %d times after pass 1, want 1", reader.calls)
	}

	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("state = %s, want ready (a transient 503 on the recovered candidate must not discard the recovery)", got.State)
	}
	if pdfFetchAttempts != 2 {
		t.Fatalf("pdf fetch attempts = %d, want 2 (503 then success on the SAME re-derived url_key)", pdfFetchAttempts)
	}
	if reader.calls != 2 {
		t.Fatalf("landing reader called %d times total, want 2 (at most once per pass, across both passes)", reader.calls)
	}

	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Kind == "openurl_handoff" {
			t.Fatalf("job opened an institutional handoff despite a fully open-access recovery route: %+v", a)
		}
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none (the job must acquire cleanly)", actions)
	}
}

// Programmer-error guard: a partially wired Service must fail before any state
// change when fetch or validation dependencies are missing.
func TestProcessRejectsMissingFetchOrValidateDependencies(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()

	id, err := svc.Submit(ctx, doiRequest("wr_nil_fetch_validate"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	before, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	const wantErr = "acquisition service is missing fetch/validation dependencies"
	fetches := 0
	for _, tc := range []struct {
		name string
		wire func(*Service)
	}{
		{
			name: "nil Fetch",
			wire: func(s *Service) {
				s.Fetch = nil
				s.Validate = passValidation()
			},
		},
		{
			name: "nil Validate",
			wire: func(s *Service) {
				s.Fetch = fakeDownload(&fetches)
				s.Validate = nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.wire(svc)
			err := svc.Process(ctx, row)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != wantErr {
				t.Fatalf("error = %q, want %q", err, wantErr)
			}
			after, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || after.UpdatedAt != before.UpdatedAt {
				t.Fatalf("job mutated: before=%+v after=%+v", before, after)
			}
		})
	}
}
