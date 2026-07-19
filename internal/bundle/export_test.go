// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	}, "AB12CD34", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
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
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any"}, nil)
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
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any"}, nil)
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
