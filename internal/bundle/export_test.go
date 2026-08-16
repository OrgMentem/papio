// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"papio/internal/artifact"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

func readyFixture(t *testing.T) (*Exporter, string, string) {
	return readyFixtureWithIdentity(t, "pass")
}

// readyFixtureWithIdentity builds a ready job whose acquisition recorded the
// given identity finding. The identity must be on the artifact BEFORE the ready
// transition, because that transition is what captures the acquisition edge —
// which is the whole point: mutating the shared artifact row afterwards must not
// change what this job's acquisition found.
func readyFixtureWithIdentity(t *testing.T, identity string) (*Exporter, string, string) {
	t.Helper()
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	jobs := &job.Store{S: db}
	arts, err := artifact.New(data)
	if err != nil {
		t.Fatal(err)
	}
	id, err := jobs.CreateRequest(ctx, "wr_bundle_001", work.Work{
		DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, "AB12CD34", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	live := "https://example.test/paper.pdf?signature=SECRET"
	_, err = jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "unpaywall", URLRedacted: redact.URL(live), URLKey: "url-key",
		LandingRedacted: "https://example.test/article", Version: "published", AccessBasis: "open_access",
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1, Rank: 0,
	}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := jobs.NextPendingCandidate(ctx, id)
	if candidate == nil {
		t.Fatal("candidate missing")
	}
	_ = jobs.MarkCandidate(ctx, candidate.ID, "accepted")

	q, _ := arts.QuarantineDir(id)
	temp := filepath.Join(q, "fixture.tmp")
	body := []byte("%PDF-1.4\nfixture\n%%EOF")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, _ := artifact.HashFile(temp)
	path, err := arts.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: int64(len(body)), MIME: "application/pdf", PageCount: 1,
		TextChars: 1200, IdentityResult: identity, Path: path,
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.Transition(ctx, id, job.StateValidating, job.StateReady, nil,
		job.WithCandidate(candidate.ID), job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	return &Exporter{Jobs: jobs, Artifacts: arts, DataDir: data}, id, sha
}

func TestExportIsSchemaValidPrivateAndIdempotent(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	path, b, err := exporter.Export(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Artifact.SHA256 != sha || b.Candidate.Source != "unpaywall" || b.ZotioItemKey != "AB12CD34" {
		t.Fatalf("bundle = %+v", b)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") || strings.Contains(string(data), "signature=") {
		t.Fatalf("bundle leaked signed URL: %s", data)
	}
	decoded, err := protocol.DecodeAcquisitionBundle(data)
	if err != nil {
		t.Fatalf("decode exported bundle: %v", err)
	}
	if decoded.ProvenanceDigest != b.ProvenanceDigest || !strings.HasPrefix(b.ProvenanceDigest, "sha256:") {
		t.Fatalf("digest mismatch: %q / %q", decoded.ProvenanceDigest, b.ProvenanceDigest)
	}
	got, _, err := artifact.HashFile(filepath.Join(filepath.Dir(path), filepath.FromSlash(b.Artifact.Path)))
	if err != nil || got != sha {
		t.Fatalf("exported artifact hash = %q, %v", got, err)
	}

	path2, b2, err := exporter.Export(ctx, id, "")
	if err != nil || path2 != path || b2.ProvenanceDigest != b.ProvenanceDigest {
		t.Fatalf("repeat export = %q %+v %v", path2, b2, err)
	}
	var count int
	if err := exporter.Jobs.S.DB().QueryRowContext(ctx, `SELECT count(*) FROM exports WHERE job_id = ?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("export ledger count = %d, %v", count, err)
	}
}

func TestExportCopiesArtifactWithoutMutatingStore(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	bundlePath, bundle, err := exporter.Export(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	exportedArtifact := filepath.Join(filepath.Dir(bundlePath), filepath.FromSlash(bundle.Artifact.Path))
	if err := os.Chmod(exportedArtifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportedArtifact, []byte("reader annotation"), 0o600); err != nil {
		t.Fatal(err)
	}
	storedArtifact, err := exporter.Jobs.GetArtifact(ctx, sha)
	if err != nil || storedArtifact == nil {
		t.Fatalf("get stored artifact: %v", err)
	}
	got, _, err := artifact.HashFile(storedArtifact.Path)
	if err != nil || got != sha {
		t.Fatalf("stored artifact hash = %q, %v; want %q", got, err, sha)
	}
}

func TestExportCleansFilesWhenLedgerRecordingFails(t *testing.T) {
	exporter, id, _ := readyFixture(t)
	ctx := context.Background()
	if _, err := exporter.Jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_bundle_export
		BEFORE INSERT ON exports
		WHEN NEW.kind = 'bundle'
		BEGIN
			SELECT RAISE(ABORT, 'injected ledger failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "export")
	if _, _, err := exporter.Export(ctx, id, destination); err == nil {
		t.Fatal("export succeeded despite ledger failure")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed export directory remains: %v", err)
	}
	var count int
	if err := exporter.Jobs.S.DB().QueryRowContext(ctx, `SELECT count(*) FROM exports WHERE job_id = ?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("export ledger count = %d, %v; want zero", count, err)
	}
}

func TestExportCleansArtifactWhenBundleWriteFails(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(destination, "bundle.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := exporter.Export(context.Background(), id, destination); err == nil {
		t.Fatal("export succeeded despite bundle write failure")
	}
	if _, err := os.Stat(filepath.Join(destination, "artifacts", sha+".pdf")); !os.IsNotExist(err) {
		t.Fatalf("artifact remains after bundle write failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("artifact directory remains after bundle write failure: %v", err)
	}
}

func TestConcurrentFailedExportPreservesWinnerFiles(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	if _, err := exporter.Jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_bundle_export_update
		BEFORE UPDATE ON exports
		WHEN NEW.kind = 'bundle'
		BEGIN
			SELECT RAISE(ABORT, 'injected ledger update failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "export")
	unlock := lockExportDestination(destination)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := exporter.Export(ctx, id, destination)
			results <- err
		}()
	}
	close(start)
	unlock()
	wg.Wait()
	close(results)

	var successes, failures int
	for err := range results {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent exports: successes=%d failures=%d", successes, failures)
	}
	if _, err := os.Stat(filepath.Join(destination, "bundle.json")); err != nil {
		t.Fatalf("winner bundle.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "artifacts", sha+".pdf")); err != nil {
		t.Fatalf("winner artifact missing: %v", err)
	}
}

func TestExportPreservesUserConfirmedIdentity(t *testing.T) {
	exporter, id, _ := readyFixtureWithIdentity(t, "user_confirmed")
	ctx := context.Background()

	path, b, err := exporter.Export(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Validation.Identity != "user_confirmed" {
		t.Fatalf("bundle validation identity = %q", b.Validation.Identity)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeAcquisitionBundle(data)
	if err != nil {
		t.Fatalf("decode exported bundle: %v", err)
	}
	if decoded.Validation.Identity != "user_confirmed" {
		t.Fatalf("bundle.json validation identity = %q", decoded.Validation.Identity)
	}
}

func TestExportRefusesUnconfirmedIdentity(t *testing.T) {
	for _, identity := range []string{"review", "reject"} {
		t.Run(identity, func(t *testing.T) {
			exporter, id, _ := readyFixtureWithIdentity(t, identity)
			if _, _, err := exporter.Export(context.Background(), id, ""); err == nil {
				t.Fatalf("exported %s identity artifact", identity)
			}
		})
	}
}

// Identity is computed against a per-job target, so on the shared artifacts row
// it is last-writer-wins: a later acquisition of the same bytes used to rewrite
// an earlier job's recorded finding, and with it that job's exported validation
// block. The acquisition edge makes each job's finding its own (ADR-0007).
func TestExportIdentityIsNotRewrittenByALaterAcquisition(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()

	// A second acquisition of the identical bytes decides the work is wrong.
	art, err := exporter.Jobs.GetArtifact(ctx, sha)
	if err != nil || art == nil {
		t.Fatalf("get artifact: %v", err)
	}
	art.IdentityResult = "reject"
	if err := exporter.Jobs.UpsertArtifact(ctx, *art); err != nil {
		t.Fatal(err)
	}

	_, b, err := exporter.Export(ctx, id, "")
	if err != nil {
		t.Fatalf("export after another job rewrote the shared artifact identity: %v", err)
	}
	if b.Validation.Identity != "pass" {
		t.Fatalf("bundle validation identity = %q, want the pass THIS acquisition recorded", b.Validation.Identity)
	}
}

func TestCacheReadyJobReusesOriginalCandidateProvenance(t *testing.T) {
	exporter, _, sha := readyFixture(t)
	ctx := context.Background()
	id, err := exporter.Jobs.CreateRequest(ctx, "wr_bundle_cache", work.Work{
		DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any"}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	_, b, err := exporter.Export(ctx, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b.Candidate.Source != "unpaywall" || b.Candidate.AccessBasis != "open_access" {
		t.Fatalf("cache lost provenance: %+v", b.Candidate)
	}
}

func TestExportRejectsNonReadyAndCorruptExistingTarget(t *testing.T) {
	exporter, id, _ := readyFixture(t)
	ctx := context.Background()
	queued, _ := exporter.Jobs.CreateRequest(ctx, "wr_notready_01", work.Work{
		DOI: "10.1002/other", Title: "Other Paper", Authors: []string{"A"}, Year: 2020,
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any"}, nil, job.PrincipalUnknown)
	if _, _, err := exporter.Export(ctx, queued, t.TempDir()); err == nil {
		t.Fatal("exported a queued job")
	}

	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	row, _ := exporter.Jobs.Get(ctx, id)
	bad := filepath.Join(dest, "artifacts", row.ArtifactSHA256+".pdf")
	if err := os.WriteFile(bad, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exporter.Export(ctx, id, dest); err == nil {
		t.Fatal("reused a corrupt destination artifact")
	}
}

// readyJobSharingArtifact adds a second acquisition that reached the same bytes
// under different terms. Content addressing makes the artifact row shared, so the
// bundle must still report THIS job's licence and access basis.
func readyJobSharingArtifact(t *testing.T, jobs *job.Store, reqID, sha string, withOwnCandidate bool) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, work.Work{
		DOI: "10.1002/example-b", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, "", "", job.Policy{AccessMode: "delegated", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	opts := []job.TransitionOpt{job.WithArtifact(sha)}
	if withOwnCandidate {
		if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
			JobID: id, Source: "browser", URLRedacted: "browser://adopted-download", URLKey: "own-key",
			Version: "unknown", AccessBasis: "institutional", ReuseLicense: "unknown",
			ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.5, Rank: 0,
		}}); err != nil {
			t.Fatal(err)
		}
		own, _ := jobs.NextPendingCandidate(ctx, id)
		if own == nil {
			t.Fatal("own candidate missing")
		}
		_ = jobs.MarkCandidate(ctx, own.ID, "accepted")
		opts = append(opts, job.WithCandidate(own.ID))
	}
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.Transition(ctx, id, job.StateValidating, job.StateReady, nil, opts...); err != nil {
		t.Fatal(err)
	}
	return id
}

// Two acquisitions can hold identical bytes under different terms. Resolving
// provenance by content hash alone reported the EARLIEST job's licence for every
// later one — first-writer-wins rights attribution on a digest (ADR-0007).
func TestExportReportsThisJobsProvenanceNotAnotherJobsSharingTheArtifact(t *testing.T) {
	exporter, first, sha := readyFixture(t)
	ctx := context.Background()
	second := readyJobSharingArtifact(t, exporter.Jobs, "wr_bundle_shared", sha, true)

	_, firstBundle, err := exporter.Export(ctx, first, "")
	if err != nil {
		t.Fatal(err)
	}
	if firstBundle.Candidate.AccessBasis != "open_access" || firstBundle.Candidate.ReuseLicense != "cc-by-4.0" {
		t.Fatalf("first bundle candidate = %+v, want its own open_access/cc-by-4.0", firstBundle.Candidate)
	}

	_, secondBundle, err := exporter.Export(ctx, second, "")
	if err != nil {
		t.Fatal(err)
	}
	if secondBundle.Candidate.AccessBasis != "institutional" {
		t.Fatalf("second bundle access_basis = %q, want institutional (borrowed the earlier job's provenance)",
			secondBundle.Candidate.AccessBasis)
	}
	if secondBundle.Candidate.ReuseLicense != "unknown" {
		t.Fatalf("second bundle reuse_license = %q, want unknown: a licence must never be inherited from another acquisition",
			secondBundle.Candidate.ReuseLicense)
	}
	if secondBundle.Candidate.Version != "unknown" || secondBundle.Candidate.Source != "browser" {
		t.Fatalf("second bundle candidate = %+v, want its own browser/unknown", secondBundle.Candidate)
	}
}

// The content-hash fallback stays, and is load-bearing: a job completed from the
// local cache reaches ready with an artifact but no candidate of its own, so its
// provenance legitimately comes from the acquisition that first fetched the bytes.
func TestExportFallsBackToOriginalAcquisitionForCacheCompletedJob(t *testing.T) {
	exporter, _, sha := readyFixture(t)
	ctx := context.Background()
	cached := readyJobSharingArtifact(t, exporter.Jobs, "wr_bundle_cached", sha, false)

	row, err := exporter.Jobs.Get(ctx, cached)
	if err != nil {
		t.Fatal(err)
	}
	if row.SelectedCandidateID != 0 {
		t.Fatalf("cache-completed fixture has its own candidate %d; the fallback is not being exercised", row.SelectedCandidateID)
	}
	_, b, err := exporter.Export(ctx, cached, "")
	if err != nil {
		t.Fatalf("export cache-completed job: %v", err)
	}
	if b.Candidate.Source != "unpaywall" || b.Candidate.ReuseLicense != "cc-by-4.0" {
		t.Fatalf("cache-completed bundle candidate = %+v, want the original unpaywall acquisition", b.Candidate)
	}
}

// A job can carry a REJECTED selected_candidate_id forward: it is written when a
// fetch starts, before validation, and the transition SQL COALESCEs it through
// crash recovery and scheduler retries. If that job then completes from the local
// cache, provenance must not be read from the file papio threw away.
func TestExportIgnoresARejectedSelectionAndUsesTheAcceptedAcquisition(t *testing.T) {
	exporter, _, sha := readyFixture(t)
	ctx := context.Background()
	jobs := exporter.Jobs

	id, err := jobs.CreateRequest(ctx, "wr_bundle_rejected", work.Work{
		DOI: "10.1002/example-rejected", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "sci-hub-lookalike", URLRedacted: "https://rejected.example/paper.pdf", URLKey: "rejected-key",
		Version: "published", AccessBasis: "manual", ReuseLicense: "all-rights-reserved",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1, Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	rejected, _ := jobs.NextPendingCandidate(ctx, id)
	if rejected == nil {
		t.Fatal("rejected candidate missing")
	}
	if err := jobs.MarkCandidate(ctx, rejected.ID, job.CandidateInvalid); err != nil {
		t.Fatal(err)
	}
	// Reach ready with the rejected candidate still selected, exactly as the
	// fetch-then-reject-then-cache-hit sequence leaves it.
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}} {
		if err := jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.Transition(ctx, id, job.StateFetching, job.StateValidating, nil, job.WithCandidate(rejected.ID)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateValidating, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.SelectedCandidateID != rejected.ID {
		t.Fatalf("fixture did not preserve the rejected selection (%d); this test would prove nothing", row.SelectedCandidateID)
	}

	_, b, err := exporter.Export(ctx, id, "")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if b.Candidate.ReuseLicense == "all-rights-reserved" || b.Candidate.Source == "sci-hub-lookalike" {
		t.Fatalf("bundle published the REJECTED candidate's provenance: %+v", b.Candidate)
	}
	if b.Candidate.Source != "unpaywall" || b.Candidate.ReuseLicense != "cc-by-4.0" {
		t.Fatalf("bundle candidate = %+v, want the accepted unpaywall acquisition", b.Candidate)
	}
}

// TestEntitlementIsDerivedNeverInferred covers the acquisition-bundle/2
// entitlement object. The consumer's gate accepts only a bare https origin, so
// every case here is really asking one question: does papio emit a value the
// consumer must reject? It must never do so — the sanitised-reference rule is
// papio's obligation, enforced at emission and fail-closed.
func TestEntitlementIsDerivedNeverInferred(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate job.Candidate
		want      *protocol.BundleEntitlement
	}{
		{
			name: "open access keeps the origin and drops the signed query",
			candidate: job.Candidate{
				Source:      "unpaywall",
				AccessBasis: resolver.AccessOpen,
				URLRedacted: redact.URL("https://example.test/paper.pdf?signature=SECRET"),
			},
			want: &protocol.BundleEntitlement{Route: "https://example.test", AcquisitionMode: "open_access"},
		},
		{
			name: "licensed api names the daemon credential in cleartext",
			candidate: job.Candidate{
				Source:      "crossref_tdm",
				AccessBasis: resolver.AccessLicensedAPI,
				URLRedacted: "https://api.crossref.org/works/10.1000/x",
			},
			want: &protocol.BundleEntitlement{
				Route:           "https://api.crossref.org",
				EntitlementRef:  "entitlement:source:crossref_tdm",
				AcquisitionMode: "daemon_held_credential",
			},
		},
		{
			// The qualifying shape: adoption recorded an institutional route
			// AND recent positive evidence the session was authenticated, so
			// the mode has a producer. The route is the page host the extension
			// reported, not the synthetic adopted URL, and not anything
			// reconstructed from current OpenURL config.
			name: "a freshly evidenced adoption names the observed origin",
			candidate: job.Candidate{
				Source:          "browser",
				AccessBasis:     resolver.AccessInstitutional,
				URLRedacted:     "browser://adopted-download",
				LandingRedacted: "https://journals.sagepub.com",
				BrowserRoute:    "direct",
				SessionEvidence: "fresh_auth",
			},
			want: &protocol.BundleEntitlement{
				Route:           "https://journals.sagepub.com",
				AcquisitionMode: "operator_browser_session",
			},
		},
		{
			// A warm session is a real institutional basis but an inherited
			// one: papio found it already authenticated and never observed the
			// login this mode would claim. ADR-0018 chose the honest floor.
			name: "a warm session is an institutional basis but no witnessed login",
			candidate: job.Candidate{
				Source:          "browser",
				AccessBasis:     resolver.AccessInstitutional,
				URLRedacted:     "browser://adopted-download",
				LandingRedacted: "https://journals.sagepub.com",
				BrowserRoute:    "resolver",
				SessionEvidence: "warm",
			},
			want: nil,
		},
		{
			// An adoption with no recorded context. Migration 0019 normalized
			// the pre-0.17.0 rows of this shape to `manual`, so a row that
			// still reads institutional-without-context was written AFTER the
			// migration by a path that carried no context: a directory-scan
			// adoption (bridge.go's scanAdoptionDir) always does, and a
			// delivery context can be pruned by its TTL before the completion
			// frame lands. This is an ongoing shape, not a historical one.
			//
			// Note what actually stops this row: entitlementRoute rejects the
			// synthetic URL, and the empty route fails the lattice inside
			// BrowserSessionFreshlyEvidenced. The evidence literal alone is not
			// load-bearing here — the `warm` case above is where it is.
			name: "an adoption with no recorded delivery context stays entitlement-less",
			candidate: job.Candidate{
				Source:      "browser",
				AccessBasis: resolver.AccessInstitutional,
				URLRedacted: "browser://adopted-download",
			},
			want: nil,
		},
		{
			// The basis is not the gate, and this is the shape that proves it:
			// a real https URL, so route resolution SUCCEEDS and the gate is
			// the only thing left to refuse the mode. A resolver reached this
			// basis from its own paywall metadata with no browser session
			// behind it (internal/app/app_test.go's paywall.test/landing
			// fixture), so emitting here would claim a session that never
			// existed.
			name: "a resolver-produced institutional candidate has no session evidence",
			candidate: job.Candidate{
				Source:      "fixture",
				AccessBasis: resolver.AccessInstitutional,
				URLRedacted: "https://paywall.test/landing",
			},
			want: nil,
		},
		{
			// An oa-route adoption: papio classified the work open access
			// before the handoff, so BrowserAccessBasis forces evidence "none"
			// and derives open_access. It still gets an entitlement, because
			// open_access needs no witnessed session — and preferring the
			// recorded landing origin is what gives it a route at all. Before
			// the landing preference existed this emitted NOTHING, since the
			// synthetic URL never yielded a route and entitlementFor is
			// all-or-nothing.
			name: "an oa-route adoption names the observed origin without any session claim",
			candidate: job.Candidate{
				Source:          "browser",
				AccessBasis:     resolver.AccessOpen,
				URLRedacted:     "browser://adopted-download",
				LandingRedacted: "https://arxiv.example.test",
				BrowserRoute:    "oa",
				SessionEvidence: "none",
			},
			want: &protocol.BundleEntitlement{
				Route:           "https://arxiv.example.test",
				AcquisitionMode: "open_access",
			},
		},
		{
			name: "manual has no observed route and is never guessed",
			candidate: job.Candidate{
				Source:      "manual",
				AccessBasis: "manual",
				URLRedacted: "https://example.test/manual.pdf",
			},
			want: nil,
		},
		{
			name: "an unparseable candidate URL omits the object rather than exporting a placeholder",
			candidate: job.Candidate{
				Source:      "unpaywall",
				AccessBasis: resolver.AccessOpen,
				URLRedacted: "not-a-url",
			},
			want: nil,
		},
		{
			name: "a non-https candidate URL omits the object",
			candidate: job.Candidate{
				Source:      "unpaywall",
				AccessBasis: resolver.AccessOpen,
				URLRedacted: "http://insecure.example.test/paper.pdf",
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := entitlementFor(&tc.candidate)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("entitlement = %+v, want omitted", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("entitlement omitted, want %+v", tc.want)
			}
			if *got != *tc.want {
				t.Fatalf("entitlement = %+v, want %+v", *got, *tc.want)
			}
			// Whatever the derivation produced must survive the same gate the
			// consumer applies; a route that fails here would be rejected there.
			bundle := protocol.AcquisitionBundle{
				SchemaVersion: protocol.AcquisitionBundleSchemaVersionV2,
				Candidate:     protocol.BundleCandidate{Entitlement: got},
			}
			if err := bundle.Validate(); err != nil && strings.Contains(err.Error(), "entitlement") {
				t.Fatalf("emitted entitlement fails papio's own gate: %v", err)
			}
		})
	}
}

// TestExportedBundleIsV2AndCarriesTheEntitlement is the end-to-end half: the
// fixture's candidate URL is bearer-signed, so this also proves the signed query
// never reaches the route.
func TestExportedBundleIsV2AndCarriesTheEntitlement(t *testing.T) {
	exporter, id, _ := readyFixture(t)
	_, b, err := exporter.Export(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != protocol.AcquisitionBundleSchemaVersionV2 {
		t.Fatalf("schema_version = %q, want %q", b.SchemaVersion, protocol.AcquisitionBundleSchemaVersionV2)
	}
	entitlement := b.Candidate.Entitlement
	if entitlement == nil {
		t.Fatal("exported bundle omitted the entitlement for an open-access acquisition")
	}
	want := protocol.BundleEntitlement{Route: "https://example.test", AcquisitionMode: "open_access"}
	if *entitlement != want {
		t.Fatalf("entitlement = %+v, want %+v", *entitlement, want)
	}
	if strings.Contains(entitlement.Route, "SECRET") || strings.ContainsAny(entitlement.Route, "?#") {
		t.Fatalf("route %q leaked query data", entitlement.Route)
	}
}
func TestExportReplacesSymlinkArtifact(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	art, err := exporter.Jobs.GetArtifact(ctx, sha)
	if err != nil || art == nil {
		t.Fatalf("get artifact: %v", err)
	}
	body, err := os.ReadFile(art.Path)
	if err != nil {
		t.Fatalf("read source artifact: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "export")
	artifactsDir := filepath.Join(destination, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o700); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	external := filepath.Join(t.TempDir(), "external.pdf")
	if err := os.WriteFile(external, body, 0o600); err != nil {
		t.Fatalf("write external: %v", err)
	}
	linkPath := filepath.Join(artifactsDir, sha+".pdf")
	if err := os.Symlink(external, linkPath); err != nil {
		t.Fatalf("symlink artifact: %v", err)
	}
	// HashFile follows the symlink, so without Lstat the export would
	// consider this already materialized.
	if got, _, err := artifact.HashFile(linkPath); err != nil || got != sha {
		t.Fatalf("symlink hash = %q, %v; want %q", got, err, sha)
	}
	if info, err := os.Lstat(linkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("precondition: %s is not a symlink: %v %v", linkPath, info, err)
	}
	bundlePath, _, err := exporter.Export(ctx, id, destination)
	if err != nil {
		// materializeArtifact's Lstat branch is specified to either error
		// or replace the symlink; both pin the fix over HashFile-follows-
		// symlink. This implementation replaces, so an error would still be
		// a correct pin, but we note it.
		if info, lerr := os.Lstat(linkPath); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("export errored but left symlink in place: %v", err)
		}
		return
	}
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stat exported artifact: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("exported artifact mode = %s, want regular file; symlink was trusted as materialized", info.Mode())
	}
	if got, _, err := artifact.HashFile(linkPath); err != nil || got != sha {
		t.Fatalf("exported artifact hash = %q, %v; want %q", got, err, sha)
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("bundle.json missing after symlink replacement: %v", err)
	}
}

func TestExportRollbackRemovesFreshDestinationWhenMaterializeFails(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	art, err := exporter.Jobs.GetArtifact(ctx, sha)
	if err != nil || art == nil {
		t.Fatalf("get artifact: %v", err)
	}
	source := art.Path
	destination := filepath.Join(t.TempDir(), "fresh-dest")
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("precondition: fresh destination already exists: %v", err)
	}
	exportPreMaterializeHook = func() {
		_ = os.Remove(source)
	}
	t.Cleanup(func() { exportPreMaterializeHook = nil })
	_, _, err = exporter.Export(ctx, id, destination)
	if err == nil {
		t.Fatal("export succeeded; want failure after source removed in pre-materialize hook")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("fresh destination not cleaned after materialize failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("fresh artifacts dir not cleaned after materialize failure: %v", err)
	}
}

func TestExportRollbackPreservesExistingDestinationWhenMaterializeFails(t *testing.T) {
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	art, err := exporter.Jobs.GetArtifact(ctx, sha)
	if err != nil || art == nil {
		t.Fatalf("get artifact: %v", err)
	}
	source := art.Path
	destination := filepath.Join(t.TempDir(), "existing")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatalf("mkdir existing destination: %v", err)
	}
	marker := filepath.Join(destination, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	exportPreMaterializeHook = func() {
		_ = os.Remove(source)
	}
	t.Cleanup(func() { exportPreMaterializeHook = nil })
	_, _, err = exporter.Export(ctx, id, destination)
	if err == nil {
		t.Fatal("export succeeded; want failure after source removed in pre-materialize hook")
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("pre-existing destination should survive rollback: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pre-existing file should survive rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("newly-created artifacts dir not cleaned after materialize failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "bundle.json")); !os.IsNotExist(err) {
		t.Fatalf("bundle.json should not exist after materialize failure: %v", err)
	}
}
