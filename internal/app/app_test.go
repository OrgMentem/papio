// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/work"
)

type fakeResolver struct {
	name  string
	cands []resolver.Candidate
	err   error
	calls int
}

func (f *fakeResolver) Name() string { return f.name }
func (f *fakeResolver) Resolve(context.Context, work.Work) ([]resolver.Candidate, error) {
	f.calls++
	return append([]resolver.Candidate(nil), f.cands...), f.err
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
	if err != nil || len(actions) != 1 || actions[0].Kind != "verify_identity" || !strings.Contains(actions[0].Detail, "local quarantine file:") {
		t.Fatalf("review action = %+v, %v", actions, err)
	}
	if _, state, err := jobs.ResolveReview(context.Background(), actions[0].ID, "accept"); err != nil || state != job.StateFetching {
		t.Fatalf("accept review = %q, %v", state, err)
	}
	row, err = jobs.ClaimNext(context.Background(), "second-worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("resumed claim = %+v, %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	ready, _ := jobs.Get(context.Background(), id)
	if ready.State != job.StateReady || ready.ArtifactSHA256 == "" || fetches != 2 {
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
				JobID: id, Source: "fixture", URLRedacted: "https://example.test/"+name+".pdf", URLKey: name,
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
