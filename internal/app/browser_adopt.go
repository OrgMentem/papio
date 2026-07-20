// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"papio/internal/artifact"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
)

// AdoptDownload ingests a browser-supplied download for a job parked in
// awaiting_human. The reported path must be a confined regular file under the
// job's adoption directory (Chrome bytes never cross native messaging; only the
// path does). The file is copied into quarantine and run through the exact same
// payload/structure/identity validation pipeline that fetched candidates use, so
// an adopted PDF is held to the same bar as an OA download before it can become
// a content-addressed artifact.
//
// The job is briefly leased for the duration so neither the scheduler nor
// RecoverStale can claim or rewind it while it sits in validating. Outcomes:
// ready on acceptance, needs_review when validation parks it, and back to
// awaiting_human (with a fresh manual_download action) when the file is rejected
// so the human can supply a different one.
func (s *Service) AdoptDownload(ctx context.Context, jobID, path string) error {
	if s.Validate == nil {
		return fmt.Errorf("acquisition service is missing its validation dependency")
	}
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if row.State != job.StateAwaitingHuman {
		return fmt.Errorf("job %s is not awaiting a human handoff (state %s)", jobID, row.State)
	}
	// Defense in depth: the bridge already confined the path, but re-confine
	// under the job's adoption root and reject symlinks/irregular files here too.
	// Ancestor symlinks are resolved (so /var -> /private/var and mounts work),
	// but the final component is checked with Lstat so a symlinked file is
	// rejected rather than followed.
	realRoot, err := filepath.EvalSymlinks(filepath.Join(s.Config.EffectiveAdoptionRoot(), jobID))
	if err != nil {
		return fmt.Errorf("adoption root unavailable: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("adoption path rejected: %w", err)
	}
	resolved := filepath.Join(realDir, filepath.Base(path))
	if err := artifact.ConfineRegularFile(realRoot, resolved); err != nil {
		return fmt.Errorf("adoption path rejected: %w", err)
	}
	path = resolved

	owner := job.NewID("adopt")
	held, err := s.leaseAwaitingHuman(ctx, jobID, owner, 5*time.Minute)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("job %s is not adoptable right now", jobID)
	}
	defer func() { _ = s.Jobs.Release(context.WithoutCancel(ctx), jobID, owner) }()

	// Copy into the job's quarantine (same filesystem as the artifact store, so a
	// validated file promotes with an atomic rename) while hashing it.
	qdir, err := s.Artifacts.QuarantineDir(jobID)
	if err != nil {
		return err
	}
	temp := filepath.Join(qdir, job.NewID("adopt")+".tmp")
	sha, size, err := copyHashed(path, temp)
	if err != nil {
		return err
	}

	// Synthetic provenance: a browser-adopted institutional download of unknown
	// reuse license. Key the candidate by content so accepting an identity review
	// applies only to those exact bytes. The scheduler deliberately re-resolves
	// after review acceptance; a repeated adoption of the unchanged file must
	// therefore recover the candidate's durable review_override instead of
	// creating a fresh candidate and parking the same PDF forever.
	version := resolver.VersionPublished
	if v := row.Policy.DesiredVersion; v != "" && v != "any" {
		version = v
	}
	key := "browser-adopt:sha256:" + sha
	if _, err := s.Jobs.InsertCandidates(ctx, jobID, []job.Candidate{{
		JobID: jobID, Source: "browser", URLRedacted: "browser://adopted-download",
		URLKey: key, Version: version, AccessBasis: resolver.AccessInstitutional, ReuseLicense: "unknown",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.5, Rank: 0,
	}}); err != nil {
		_ = os.Remove(temp)
		return err
	}
	id, err := s.candidateIDByKey(ctx, jobID, key)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	stored, err := s.Jobs.GetCandidate(ctx, id)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}

	result := fetch.Result{
		TempPath: temp, SHA256: sha, SizeBytes: size,
		SniffedMIME: "application/pdf", ContentType: "application/pdf", FinalHost: "browser",
	}
	if err := s.Jobs.Transition(ctx, jobID, job.StateAwaitingHuman, job.StateValidating,
		map[string]any{"reason": "adopt_browser_download", "source": "browser"},
		job.WithCandidate(stored.ID)); err != nil {
		_ = os.Remove(temp)
		return err
	}

	accepted, parked, err := s.validateCandidate(ctx, row, stored, result)
	if err != nil {
		// validateCandidate returns before completing a transition on an
		// infrastructure error (start-attempt / promote / store failure),
		// leaving the job in validating. Left there, the scheduler's
		// RecoverStale rewinds it to resolving and re-fetches, discarding the
		// user's supplied download for whatever OA resolution finds. Re-park in
		// awaiting_human (best-effort) so the file — still in the adoption
		// directory under the job's still-open handoff action — is preserved and
		// re-driven by the directory sweep; a transient store error clears on a
		// later tick. The original error is still returned so the bridge records
		// it as browser.adoption_deferred.
		_ = s.park(context.WithoutCancel(ctx), jobID, job.StateValidating, job.StateAwaitingHuman,
			map[string]any{"reason": "adoption_validation_error"})
		return err
	}
	if accepted || parked {
		return nil
	}
	// Rejected: validateCandidate returned the job to fetching. There is no next
	// candidate to fetch for an adopted download, so re-park in awaiting_human
	// and ask the human for a different file. Move the rejected file out of the
	// adoption directory (into a sibling rejected/<job_id>/ dir, preserving it
	// for the user) so the daemon's directory sweep does not re-adopt and
	// re-reject the same file forever.
	rejectDir := filepath.Join(s.Config.EffectiveAdoptionRoot(), "rejected", jobID)
	moved := false
	if mkErr := os.MkdirAll(rejectDir, 0o700); mkErr == nil {
		if renErr := os.Rename(path, filepath.Join(rejectDir, filepath.Base(path))); renErr == nil {
			moved = true
		}
	}
	if !moved {
		// The rejected file could not be moved out of the adoption directory, so
		// re-parking in awaiting_human would let the directory sweep re-adopt and
		// re-reject the same file every tick. Park in needs_review instead — the
		// adoption sweep never scans it — with an action telling the user to
		// remove or replace the file so the loop cannot spin.
		if _, err := s.Jobs.OpenHumanAction(ctx, jobID, "manual_download",
			"the adopted download failed validation and could not be quarantined; remove or replace the file in the adoption directory"); err != nil {
			return err
		}
		return s.park(ctx, jobID, job.StateFetching, job.StateNeedsReview,
			map[string]any{"reason": "adopted_download_rejected_unquarantined"})
	}
	if _, err := s.Jobs.OpenHumanAction(ctx, jobID, "manual_download",
		"the adopted download failed validation; please supply a different file"); err != nil {
		return err
	}
	return s.park(ctx, jobID, job.StateFetching, job.StateAwaitingHuman,
		map[string]any{"reason": "adopted_download_rejected"})
}

// leaseAwaitingHuman CAS-acquires a lease on a job that is parked in
// awaiting_human. It mirrors ClaimNext's ownership guard but targets a specific
// parked job (which ClaimNext never selects), so adoption can hold the job
// across the validating window.
func (s *Service) leaseAwaitingHuman(ctx context.Context, jobID, owner string, lease time.Duration) (bool, error) {
	now := time.Now().UTC()
	expires := now.Add(lease).Format(time.RFC3339Nano)
	res, err := s.Jobs.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_owner = ?, lease_expires_at = ?
		 WHERE id = ? AND state = ? AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		owner, expires, jobID, job.StateAwaitingHuman, now.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// candidateIDByKey resolves the durable id of a job candidate by its url_key.
func (s *Service) candidateIDByKey(ctx context.Context, jobID, key string) (int64, error) {
	var id int64
	err := s.Jobs.S.DB().QueryRowContext(ctx,
		`SELECT id FROM candidates WHERE job_id = ? AND url_key = ?`, jobID, key).Scan(&id)
	return id, err
}

// copyHashed streams src into dst (created 0600) while computing its SHA-256 and
// size. The download's own bytes never enter events or the database.
func copyHashed(src, dst string) (sha string, size int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return "", 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return "", 0, closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
