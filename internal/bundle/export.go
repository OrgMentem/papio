// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package bundle exports a ready job as a self-contained, schema-validated,
// provenance-digested AcquisitionBundle. Export is idempotent and never writes
// a live/signed candidate URL or credential.
package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/artifact"
	"papio/internal/job"
	"papio/internal/protocol"
)

// Exporter materializes bundles from the durable job/artifact stores.
type Exporter struct {
	Jobs      *job.Store
	Artifacts *artifact.Store
	DataDir   string
	Now       func() time.Time
}

// Export creates (or verifies and reuses) bundle.json and its relative
// content-addressed artifact. An empty destination uses DataDir/bundles/<job>.
func (e *Exporter) Export(ctx context.Context, jobID, destination string) (string, *protocol.AcquisitionBundle, error) {
	if e.Jobs == nil || e.Artifacts == nil {
		return "", nil, errors.New("bundle exporter missing stores")
	}
	row, err := e.Jobs.Get(ctx, jobID)
	if err != nil {
		return "", nil, err
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		return "", nil, fmt.Errorf("job %s is %s, not ready", jobID, row.State)
	}
	art, err := e.Jobs.GetArtifact(ctx, row.ArtifactSHA256)
	if err != nil || art == nil {
		if err == nil {
			err = fmt.Errorf("job %s references missing artifact %s", jobID, row.ArtifactSHA256)
		}
		return "", nil, err
	}
	if err := e.Artifacts.Verify(art.SHA256); err != nil {
		return "", nil, err
	}
	candidate, err := e.Jobs.FindCandidateByArtifact(ctx, art.SHA256)
	if err != nil {
		return "", nil, err
	}
	if candidate == nil {
		return "", nil, fmt.Errorf("artifact %s has no accepted candidate provenance", art.SHA256)
	}
	if art.IdentityResult != "pass" && art.IdentityResult != "user_confirmed" {
		return "", nil, fmt.Errorf("artifact identity is %q, not exportable", art.IdentityResult)
	}

	retrieved := art.CreatedAt
	if _, err := time.Parse(time.RFC3339, retrieved); err != nil {
		now := e.Now
		if now == nil {
			now = time.Now
		}
		retrieved = now().UTC().Format(time.RFC3339)
	}
	landing := candidate.LandingRedacted
	if strings.Contains(landing, "<redacted>") {
		landing = "" // do not export a syntactically fake or secret-bearing URI
	}
	b := &protocol.AcquisitionBundle{
		SchemaVersion: protocol.AcquisitionBundleSchemaVersion,
		JobID:         jobID, RequestID: row.WorkRequestID,
		Identity: protocol.BundleIdentity{
			DOI: row.Work.DOI, Title: row.Work.Title, Authors: append([]string(nil), row.Work.Authors...), Year: row.Work.Year,
		},
		Candidate: protocol.BundleCandidate{
			Source: candidate.Source, Version: candidate.Version, AccessBasis: candidate.AccessBasis,
			ReuseLicense: candidate.ReuseLicense, LandingURL: landing,
		},
		RetrievedAt: retrieved,
		Artifact: protocol.BundleArtifact{
			SHA256: art.SHA256, SizeBytes: art.SizeBytes, MIME: art.MIME, PageCount: art.PageCount,
			TextChars: art.TextChars, OCRUsed: art.OCRUsed, Path: filepath.ToSlash(filepath.Join("artifacts", art.SHA256+".pdf")),
		},
		Validation:   protocol.BundleValidation{Structural: "pass", Identity: art.IdentityResult},
		ZotioItemKey: row.ZotioItemKey,
	}
	b.ProvenanceDigest, err = digest(b)
	if err != nil {
		return "", nil, err
	}
	if err := b.Validate(); err != nil {
		return "", nil, fmt.Errorf("bundle validation: %w", err)
	}

	if destination == "" {
		destination = filepath.Join(e.DataDir, "bundles", jobID)
	}
	if err := os.MkdirAll(filepath.Join(destination, "artifacts"), 0o700); err != nil {
		return "", nil, err
	}
	artifactPath := filepath.Join(destination, filepath.FromSlash(b.Artifact.Path))
	if err := materializeArtifact(art.Path, artifactPath, art.SHA256); err != nil {
		return "", nil, err
	}
	bundlePath := filepath.Join(destination, "bundle.json")
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", nil, err
	}
	encoded = append(encoded, '\n')
	if err := atomicWrite(bundlePath, encoded, 0o600); err != nil {
		return "", nil, err
	}
	if err := e.record(ctx, jobID, art.SHA256, bundlePath); err != nil {
		return "", nil, err
	}
	return bundlePath, b, nil
}

func digest(b *protocol.AcquisitionBundle) (string, error) {
	copy := *b
	copy.ProvenanceDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func materializeArtifact(source, target, expectedSHA string) (retErr error) {
	if got, _, err := artifact.HashFile(target); err == nil {
		if got == expectedSHA {
			return nil
		}
		return fmt.Errorf("existing bundle artifact %s has hash %s, want %s", target, got, expectedSHA)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(source, target); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); retErr == nil && closeErr != nil {
			retErr = closeErr
		}
	}()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	got, _, err := artifact.HashFile(target)
	if err != nil {
		return err
	}
	if got != expectedSHA {
		_ = os.Remove(target)
		return fmt.Errorf("copied artifact hash %s, want %s", got, expectedSHA)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bundle-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (e *Exporter) record(ctx context.Context, jobID, sha, path string) error {
	key := "bundle:" + jobID + ":" + sha
	result, _ := json.Marshal(map[string]string{"artifact_sha256": sha})
	_, err := e.Jobs.S.DB().ExecContext(ctx, `
		INSERT INTO exports(job_id, kind, idempotency_key, path, result_json, created_at)
		VALUES(?, 'bundle', ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO UPDATE SET path = excluded.path, result_json = excluded.result_json`,
		jobID, key, path, string(result), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
