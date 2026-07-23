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
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
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
	cfg.Sources["fixture"] = config.Source{Enabled: true}
	svc := New(cfg, &job.Store{S: db}, artifacts, nil)
	return svc, svc.Jobs
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
	if _, retryAt, err := svc.resolve(context.Background(), row); err != nil || !retryAt.IsZero() {
		t.Fatalf("resolve = retry %v, error %v", retryAt, err)
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

func TestLandingOnlyRequiresHumanAction(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/landing", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: false, IdentityConfidence: 1,
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
func TestValidationPersistsArtifactMetadataBeforePromotion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, jobs := newTestService(t)
	id, err := jobs.CreateRequest(ctx, "wr_artifact_metadata_first", work.Work{DOI: "10.1002/example"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/paper.pdf", URLKey: "paper",
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
	body := pdfBytes("promotion-order")
	tempPath := filepath.Join(qdir, "candidate.tmp")
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		cancel()
		return passValidation()(context.Background(), "", "", work.Work{})
	}

	_, _, err = svc.validateCandidate(ctx, row, candidate, fetch.Result{
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
	id, err := jobs.CreateRequest(ctx, "wr_promotion_rollback", work.Work{DOI: "10.1002/example"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/paper.pdf", URLKey: "promotion-rollback",
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
	body := pdfBytes("promotion-rollback")
	tempPath := filepath.Join(qdir, "candidate.tmp")
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
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
			}, nil)
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
	if notifier.imported != 1 {
		t.Fatalf("import notifications = %d, want 1", notifier.imported)
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
	id, err := svc.SubmitWithAutoImport(context.Background(), doiRequest("wr_auto_import_off"), &disabled)
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
	id, err = svc.SubmitWithAutoImport(context.Background(), doiRequest("wr_auto_import_cfg"), nil)
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
}

func (f *fakeNotificationSink) HumanAction(context.Context) { f.human++ }
func (f *fakeNotificationSink) Imported(context.Context)    { f.imported++ }

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
