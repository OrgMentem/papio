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
	"papio/internal/store"
	"papio/internal/work"
)

func readyFixture(t *testing.T) (*Exporter, string, string) {
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
		TextChars: 1200, IdentityResult: "pass", Path: path,
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
	exporter, id, sha := readyFixture(t)
	ctx := context.Background()
	art, err := exporter.Jobs.GetArtifact(ctx, sha)
	if err != nil || art == nil {
		t.Fatalf("get artifact: %v", err)
	}
	art.IdentityResult = "user_confirmed"
	if err := exporter.Jobs.UpsertArtifact(ctx, *art); err != nil {
		t.Fatal(err)
	}

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
			exporter, id, sha := readyFixture(t)
			ctx := context.Background()
			art, err := exporter.Jobs.GetArtifact(ctx, sha)
			if err != nil || art == nil {
				t.Fatalf("get artifact: %v", err)
			}
			art.IdentityResult = identity
			if err := exporter.Jobs.UpsertArtifact(ctx, *art); err != nil {
				t.Fatal(err)
			}
			if _, _, err := exporter.Export(ctx, id, ""); err == nil {
				t.Fatalf("exported %s identity artifact", identity)
			}
		})
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
