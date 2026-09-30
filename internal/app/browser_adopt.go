// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"papio/internal/artifact"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
)

// adoptionStateError reports a job whose state has no legal edge onto the
// human-adoption boundary: a terminal job, or one already validating, held
// for review, or (for a browser download) waiting to retry.
type adoptionStateError struct {
	jobID string
	state string
}

func (e *adoptionStateError) Error() string {
	return fmt.Sprintf("job %s is not adoptable while in state %s", e.jobID, e.state)
}

// parkForAdoption moves a live job onto the existing human-adoption boundary
// for a browser download. queued has no direct edge to awaiting_human, so it
// first follows the legal queued -> resolving edge; a scheduler race is
// retried from the durable state it won. Parking from resolving/fetching
// opens a manual_download action so the any-open-action adoption fence sees a
// genuine awaiting-human action — the browser download is itself the human
// gesture that justified the park. reason names that gesture in the
// transition detail. A job waiting to retry has no browser handoff open, so a
// download that names it is refused; the operator's supply-pdf command parks
// through parkSuppliedPDFJob instead.
func (s *Service) parkForAdoption(ctx context.Context, jobID, reason string) error {
	for range 4 {
		row, err := s.Jobs.Get(ctx, jobID)
		if err != nil {
			return err
		}
		switch row.State {
		case job.StateAwaitingHuman:
			return nil
		case job.StateQueued:
			err = s.Jobs.Transition(ctx, jobID, row.State, job.StateResolving,
				map[string]any{"reason": reason})
		case job.StateResolving, job.StateFetching:
			prev := row.State
			err = s.Jobs.Transition(ctx, jobID, prev, job.StateAwaitingHuman,
				map[string]any{"reason": reason})
			if err == nil {
				if _, aErr := s.Jobs.OpenHumanAction(ctx, jobID, job.CandidateEligibleKind, "please download the paper", job.Access(false, "")); aErr != nil {
					return nil
				}
				return nil
			}
		default:
			return &adoptionStateError{jobID: jobID, state: row.State}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, job.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("job %s changed while preparing adoption", jobID)
}

// resolveAdoptionRoots returns the symlink-resolved landing directories for
// jobID across config.AdoptionRoots — the effective root first, then the
// drain-only legacy <data_dir>/adoptions root when it is distinct. Both are
// returned when both exist, because the caller confines against all of them:
// a file that landed in the legacy root before the default moved under the
// browser's download directory must still be adoptable, and its job may well
// have an (empty) directory under the effective root too.
//
// When no root holds a directory for the job it returns the effective root's
// resolution error, unwrapped enough for callers to test it with
// errors.Is(fs.ErrNotExist), unless that error is a plain absence and a later
// root failed operationally. The drain-only legacy root is normally absent;
// reporting its lstat hid both where the landing directory went and an
// EACCES/EIO on the effective root.
func (s *Service) resolveAdoptionRoots(jobID string) ([]string, error) {
	bases := s.Config.AdoptionRoots()
	roots := make([]string, 0, len(bases))
	var rootErr error
	for _, base := range bases {
		real, err := filepath.EvalSymlinks(filepath.Join(base, jobID))
		if err != nil {
			if rootErr == nil || errors.Is(rootErr, fs.ErrNotExist) && !errors.Is(err, fs.ErrNotExist) {
				rootErr = err
			}
			continue
		}
		roots = append(roots, real)
	}
	if len(roots) == 0 {
		if rootErr == nil {
			rootErr = fs.ErrNotExist
		}
		return nil, rootErr
	}
	return roots, nil
}

// confineToAdoptionRoots accepts resolved when it is a regular file inside any
// of roots, and otherwise returns the last confinement failure so the caller
// can report a real reason rather than a generic refusal.
func confineToAdoptionRoots(roots []string, resolved string) error {
	var last error
	for _, root := range roots {
		err := artifact.ConfineRegularFile(root, resolved)
		if err == nil {
			return nil
		}
		last = err
	}
	return last
}

// AdoptDownload ingests a browser-supplied download for a job parked in
// awaiting_human, or for a live queued/resolving/fetching job that the
// browser has steered into its adoption directory. Live jobs are parked at
// awaiting_human through existing legal edges before the same validation path
// runs; no new acquisition state is needed. The reported path must be a
// confined regular file under the job's adoption directory (Chrome bytes
// never cross native messaging; only the path does). The file is copied into
// quarantine and run through the exact same payload/structure/identity
// validation pipeline that fetched candidates use, so an adopted PDF is held
// to the same bar as an OA download before it can become a content-addressed
// artifact.
//
// The live-job park is deliberately action-free: the browser's download
// completion is already the human gesture, and the bridge's directory sweep
// scans every awaiting_human adoption directory. If validation races the
// browser rename, the parked job and landing file remain eligible for the
// next poll.
//
// The job is briefly leased for the duration so neither the scheduler nor
// RecoverStale can claim or rewind it while it sits in validating. Outcomes:
// ready on acceptance, needs_review when validation parks it (including
// unsafe_pdf — encrypted or active/embedded PDFs held for review with their
// quarantine file — which does NOT open a manual_download replacement and does
// NOT move the file to rejected/), and back to awaiting_human (with a fresh
// manual_download action) when the file is rejected so the human can supply a
// different one.
func (s *Service) AdoptDownload(ctx context.Context, jobID, path string) error {
	row, err := s.prepareMainAdoption(ctx, jobID, func(ctx context.Context) error {
		return s.parkForAdoption(ctx, jobID, "browser_download_adoption")
	})
	if err != nil {
		return err
	}
	// Defense in depth: the bridge already confined the path, but re-confine
	// under the job's adoption root and reject symlinks/irregular files here too.
	// Ancestor symlinks are resolved (so /var -> /private/var and mounts work),
	// but the final component is checked with Lstat so a symlinked file is
	// rejected rather than followed.
	roots, err := s.resolveAdoptionRoots(jobID)
	if err != nil {
		return fmt.Errorf("adoption root unavailable: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("adoption path rejected: %w", err)
	}
	resolved := filepath.Join(realDir, filepath.Base(path))
	if err := confineToAdoptionRoots(roots, resolved); err != nil {
		return fmt.Errorf("adoption path rejected: %w", err)
	}
	_, err = s.adoptMainPDF(ctx, row, mainPDFOrigin{
		source:     "browser",
		url:        "browser://adopted-download",
		keyPrefix:  "browser-adopt:sha256:",
		copyInto:   func(dst string) (string, int64, error) { return copyHashed(resolved, dst) },
		transition: s.Jobs.TransitionAwaitingToValidatingIfAdoptEligible,
		rejected: func(ctx context.Context, jobID string, access job.AccessClassification) error {
			return s.rejectAdoptedDownload(ctx, jobID, resolved, access)
		},
	})
	return err
}

// prepareMainAdoption brings jobID onto the awaiting_human adoption boundary
// for a main PDF and returns its row. park moves a job that is not yet
// awaiting_human there; each origin owns which states it admits.
func (s *Service) prepareMainAdoption(ctx context.Context, jobID string, park func(context.Context) error) (*job.Row, error) {
	if s.Validate == nil {
		return nil, fmt.Errorf("acquisition service is missing its validation dependency")
	}
	if err := s.reconcilePreparedPublications(ctx, jobID); err != nil {
		return nil, err
	}
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if row.State != job.StateAwaitingHuman {
		if err := park(ctx); err != nil {
			return nil, err
		}
		row, err = s.Jobs.Get(ctx, jobID)
		if err != nil {
			return nil, err
		}
	}
	if row.State != job.StateAwaitingHuman {
		return nil, fmt.Errorf("job %s is not awaiting a human handoff (state %s)", jobID, row.State)
	}
	return row, nil
}

// mainPDFOrigin is what differs between the two ways a person hands papio a
// job's main PDF: a browser download and a local file the operator supplied.
// Everything else — the lease, quarantine, the candidate keyed by content,
// validation, the review gate and the outcomes — is one path.
type mainPDFOrigin struct {
	// source, url and keyPrefix shape the synthetic candidate; the content
	// hash follows keyPrefix in its url_key.
	source    string
	url       string
	keyPrefix string
	// copyInto copies the supplied bytes into the quarantine file dst while
	// hashing them.
	copyInto func(dst string) (sha string, size int64, err error)
	// transition is the guarded awaiting_human -> validating move. After it
	// succeeds the validation path owns the job's outcome.
	transition func(ctx context.Context, jobID string, candidateID int64) error
	// rejected handles a file validation rejected. The job is in fetching;
	// the handler must park it again and ask for a different file.
	rejected func(ctx context.Context, jobID string, access job.AccessClassification) error
}

// Main-PDF adoption outcomes, as SupplyPDF reports them.
const (
	// AdoptionAccepted: the file passed validation and the job is ready.
	AdoptionAccepted = "accepted"
	// AdoptionNeedsReview: validation held the file for a human decision.
	AdoptionNeedsReview = "needs_review"
	// AdoptionRejected: validation refused the file and the job waits for a
	// different one.
	AdoptionRejected = "rejected"
)

// adoptedMainPDF is what one main-PDF adoption did. validating reports that
// the job left awaiting_human for validation: an error returned before that
// left the job parked where the caller put it.
type adoptedMainPDF struct {
	candidateID int64
	sha256      string
	outcome     string
	validating  bool
}

// adoptMainPDF runs one human-supplied main PDF for a job parked at
// awaiting_human through the validation pipeline fetched candidates use. See
// AdoptDownload for the lease and the outcomes.
func (s *Service) adoptMainPDF(ctx context.Context, row *job.Row, origin mainPDFOrigin) (adoptedMainPDF, error) {
	jobID := row.ID
	var adopted adoptedMainPDF
	owner := job.NewID("adopt")
	held, err := s.Jobs.LeaseAwaitingHuman(ctx, jobID, owner, 5*time.Minute)
	if err != nil {
		return adopted, err
	}
	if !held {
		return adopted, fmt.Errorf("job %s is not adoptable right now", jobID)
	}
	defer func() { _ = s.Jobs.Release(context.WithoutCancel(ctx), jobID, owner) }()

	// Copy into the job's quarantine (same filesystem as the artifact store, so a
	// validated file promotes with an atomic rename) while hashing it.
	qdir, err := s.Artifacts.QuarantineDir(jobID)
	if err != nil {
		return adopted, err
	}
	temp := filepath.Join(qdir, job.NewID("adopt")+".tmp")
	sha, size, err := origin.copyInto(temp)
	if err != nil {
		return adopted, err
	}
	adopted.sha256 = sha

	// Synthetic provenance: a human-supplied file of unknown reuse license.
	// Key the candidate by content so accepting an identity review applies
	// only to those exact bytes. The scheduler deliberately re-resolves after
	// review acceptance; a repeated adoption of the unchanged file must
	// therefore recover the candidate's durable review_override instead of
	// creating a fresh candidate and parking the same PDF forever.
	//
	// The version is `unknown` and must stay that way: adoption observes bytes
	// arriving from a human, never which version that human chose.
	// `Policy.DesiredVersion` is a ranking *preference* for resolver candidates —
	// echoing it back here would report the request as an obtained fact, and a
	// consumer that gates an adverse finding on the version would act on papio's
	// own guess (ADR-0007).
	version := resolver.VersionUnknown
	key := origin.keyPrefix + sha
	if result, ok := ctx.Value(adoptionCandidateResultKey{}).(*adoptionCandidateResult); ok {
		result.Key = key
	}
	if _, err := s.Jobs.InsertCandidates(ctx, jobID, []job.Candidate{{
		JobID: jobID, Source: origin.source, URLRedacted: origin.url,
		URLKey: key, Version: version, AccessBasis: resolver.AccessManual, ReuseLicense: "unknown",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.5, Rank: 0,
	}}); err != nil {
		_ = os.Remove(temp)
		return adopted, err
	}
	id, err := s.Jobs.CandidateIDByKey(ctx, jobID, key)
	if err != nil {
		_ = os.Remove(temp)
		return adopted, err
	}
	// InsertCandidates is INSERT OR IGNORE on (job_id, url_key), so re-adopting
	// the same bytes keeps whatever row already exists — including one written
	// before papio stopped synthesizing the version. Normalize before reading it
	// back, or the old `published` claim outlives the fix.
	if err := s.Jobs.MarkCandidateVersionUnobserved(ctx, id); err != nil {
		_ = os.Remove(temp)
		return adopted, err
	}
	stored, err := s.Jobs.GetCandidate(ctx, id)
	if err != nil {
		_ = os.Remove(temp)
		return adopted, err
	}
	adopted.candidateID = stored.ID
	if result, ok := ctx.Value(adoptionCandidateResultKey{}).(*adoptionCandidateResult); ok {
		result.ID = stored.ID
	}

	result := fetch.Result{
		TempPath: temp, SHA256: sha, SizeBytes: size,
		SniffedMIME: "application/pdf", ContentType: "application/pdf", FinalHost: origin.source,
	}
	if err := origin.transition(ctx, jobID, stored.ID); err != nil {
		_ = os.Remove(temp)
		return adopted, err
	}
	adopted.validating = true
	replacementAccess := s.adoptionReplacementAccess(ctx, jobID)
	s.resolveAdoptedHandoffActions(ctx, jobID)

	accepted, parked, err := s.validateCandidate(ctx, row, stored, result, &owner)
	if err != nil {
		// A prepared publication owns the quarantine bytes and records a
		// validating -> ready transition. Re-parking this job would make that
		// transition impossible to finalize after a crash or an ambiguous SQL
		// reply. Leave it validating for the journal recovery path instead.
		prepared, preparedErr := s.Jobs.PreparedPublications(context.WithoutCancel(ctx), jobID)
		if preparedErr != nil {
			return adopted, errors.Join(err, fmt.Errorf("checking prepared adoption publication: %w", preparedErr))
		}
		for _, publication := range prepared {
			if publication.Role == job.PublicationRoleMain && publication.CandidateID != nil && *publication.CandidateID == stored.ID {
				return adopted, err
			}
		}
		// validateCandidate returns before completing a transition on an
		// infrastructure error (start-attempt / preparation failure),
		// leaving the job in validating. Left there, the scheduler's
		// RecoverStale rewinds it to resolving and re-fetches, discarding the
		// human's supplied file for whatever OA resolution finds. Re-park in
		// awaiting_human (best-effort) so the file can be supplied again: a
		// browser download stays in the adoption directory and the directory
		// sweep re-drives it; a transient store error clears on a later tick.
		// The handoff actions were already resolved above, so the park
		// reopens a manual_download in the same transaction: an awaiting_human
		// job with no open action is an orphaned park that handoff repair
		// sends back to resolving, dropping the supplied file just the same.
		// The original error is still returned so the caller reports it (the
		// bridge records it as browser.adoption_deferred).
		parkCtx := context.WithoutCancel(ctx)
		if parkErr := s.Jobs.ParkWithHumanAction(parkCtx, jobID, job.StateValidating, job.StateAwaitingHuman,
			"manual_download", "the supplied PDF could not be validated because of an internal error; supply it again",
			map[string]any{"reason": "adoption_validation_error"}, replacementAccess); parkErr == nil {
			s.notifyParked(parkCtx, jobID, job.StateAwaitingHuman)
		}
		return adopted, err
	}
	switch {
	case accepted:
		adopted.outcome = AdoptionAccepted
		return adopted, nil
	case parked:
		adopted.outcome = AdoptionNeedsReview
		return adopted, nil
	}
	// Rejected: validateCandidate returned the job to fetching. There is no next
	// candidate to fetch for a supplied file, so the origin re-parks the job and
	// asks the human for a different file.
	adopted.outcome = AdoptionRejected
	return adopted, origin.rejected(ctx, jobID, replacementAccess)
}

// rejectAdoptedDownload re-parks a job whose browser download failed
// validation in awaiting_human and asks the human for a different file. It
// moves the rejected file out of the adoption directory (into a sibling
// rejected/<job_id>/ dir, preserving it for the user) so the daemon's
// directory sweep does not re-adopt and re-reject the same file forever.
func (s *Service) rejectAdoptedDownload(ctx context.Context, jobID, path string, replacementAccess job.AccessClassification) error {
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
		// remove the file and redrive, which reopens the park only once no
		// file remains, so the loop cannot spin.
		if _, err := s.Jobs.OpenHumanAction(ctx, jobID, "manual_download",
			fmt.Sprintf("the adopted download failed validation and could not be quarantined; remove the file from the adoption directory, then run 'papio jobs redrive %s --revision 1'", jobID),
			replacementAccess, job.WithHumanActionDiagnosis(job.DiagnosisReasonAdoptedPDFInvalid)); err != nil {
			return err
		}
		return s.park(ctx, jobID, job.StateFetching, job.StateNeedsReview,
			map[string]any{"reason": "adopted_download_rejected_unquarantined"})
	}
	if _, err := s.Jobs.OpenHumanAction(ctx, jobID, "manual_download",
		"the adopted download failed validation; please supply a different file",
		replacementAccess, job.WithHumanActionDiagnosis(job.DiagnosisReasonAdoptedPDFInvalid)); err != nil {
		return err
	}
	return s.park(ctx, jobID, job.StateFetching, job.StateAwaitingHuman,
		map[string]any{"reason": "adopted_download_rejected"})
}

type adoptionCandidateResultKey struct{}

type adoptionCandidateResult struct {
	ID  int64
	Key string
}

// AdoptDownloadCandidate adopts a browser file and returns the durable
// candidate created or reused for that file. The existing adoption API keeps
// its error-only shape for callers that do not need provenance correlation.
func (s *Service) AdoptDownloadCandidate(ctx context.Context, jobID, path string) (int64, error) {
	result := new(adoptionCandidateResult)
	adoptErr := s.AdoptDownload(context.WithValue(ctx, adoptionCandidateResultKey{}, result), jobID, path)
	if adoptErr != nil {
		if result.ID == 0 && result.Key != "" {
			result.ID, _ = s.Jobs.CandidateIDByKey(ctx, jobID, result.Key)
		}
		return result.ID, adoptErr
	}
	if result.ID == 0 {
		return 0, fmt.Errorf("browser adoption created no candidate for job %s", jobID)
	}
	return result.ID, nil
}

// BrowserDeliveryContext is the bounded, non-secret provenance observed by
// the extension for one browser download. It is intentionally separate from
// the download bytes and from the synthetic adopted URL.
type BrowserDeliveryContext struct {
	Route           string
	PageHost        string
	SessionEvidence string
}

// AdoptDownloadWithContext preserves the legacy adoption path while attaching
// a context that arrived with the browser delivery. The context is applied
// after validation to the candidate created by this file's content hash.
func (s *Service) AdoptDownloadWithContext(ctx context.Context, jobID, path string, context *BrowserDeliveryContext) error {
	_, err := s.AdoptDownloadWithContextCandidate(ctx, jobID, path, context)
	return err
}

// AdoptDownloadWithContextCandidate adopts one browser file and applies its
// validated delivery context only to the candidate bound to that file.
func (s *Service) AdoptDownloadWithContextCandidate(ctx context.Context, jobID, path string, context *BrowserDeliveryContext) (int64, error) {
	if context == nil {
		return s.AdoptDownloadCandidate(ctx, jobID, path)
	}
	if _, err := job.BrowserAccessBasis(context.Route, context.SessionEvidence); err != nil {
		return 0, err
	}
	candidateID, err := s.AdoptDownloadCandidate(ctx, jobID, path)
	if err != nil {
		return candidateID, err
	}
	landing := ""
	if context.PageHost != "" {
		landing = "https://" + context.PageHost
	}
	applied, err := s.Jobs.ApplyBrowserDeliveryContextToCandidate(ctx, jobID, candidateID, context.Route, context.SessionEvidence, landing)
	if err != nil {
		return candidateID, err
	}
	if !applied {
		return candidateID, fmt.Errorf("browser delivery candidate %d is unavailable for job %s", candidateID, jobID)
	}
	return candidateID, nil
}

// resolveAdoptedHandoffActions closes the handoff actions satisfied by a
// browser download. This is best-effort cleanup: validation must continue even
// if an already-stale action cannot be resolved.
func (s *Service) resolveAdoptedHandoffActions(ctx context.Context, jobID string) {
	actions, err := s.Jobs.ListOpenHumanActionsForJobs(context.WithoutCancel(ctx), []string{jobID})
	if err != nil {
		log.Printf("papio: listing browser handoff actions for adopted job %s: %v", jobID, err)
		return
	}
	for _, action := range actions {
		if action.Kind != "openurl_handoff" && action.Kind != "manual_download" {
			continue
		}
		if err := s.Jobs.ResolveHumanAction(context.WithoutCancel(ctx), action.ID, "resolved"); err != nil {
			log.Printf("papio: resolving browser handoff action %d for adopted job %s: %v", action.ID, jobID, err)
		}
	}
}

// adoptionReplacementAccess snapshots the access classification of the handoff
// that supplied the browser download before resolveAdoptedHandoffActions closes
// it. A missing action fails closed through the job-level inheritance helper.
func (s *Service) adoptionReplacementAccess(ctx context.Context, jobID string) job.AccessClassification {
	actions, err := s.Jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil {
		return job.AccessInheritedFromResolvedHandoff("")
	}
	for _, kind := range []string{"openurl_handoff", "manual_download"} {
		for _, action := range actions {
			if action.Kind == kind {
				return job.Access(action.RequiresAuth, action.BlockedBy)
			}
		}
	}
	return job.AccessInheritedFromResolvedHandoff("")
}

// copyHashed streams src into dst (created 0600) while computing its SHA-256 and
// size. The download's own bytes never enter events or the database.
//
// Callers confine src with an Lstat-based check before calling, but that check
// and this open are separate path lookups: a final component swapped for a
// symlink in between would be followed by a plain open. copyHashed therefore
// re-checks the name without following it and requires the opened descriptor
// to be that same regular file, failing closed on any change.
func copyHashed(src, dst string) (sha string, size int64, err error) {
	checked, err := os.Lstat(src)
	if err != nil {
		return "", 0, err
	}
	if !checked.Mode().IsRegular() {
		return "", 0, fmt.Errorf("adoption source %s is no longer a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()
	opened, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(checked, opened) {
		return "", 0, fmt.Errorf("adoption source %s changed before it was read", src)
	}
	return writeHashed(in, dst)
}

// writeHashed streams r into dst (created 0600, never an existing file) while
// computing its SHA-256 and size. dst is removed if the copy fails.
func writeHashed(r io.Reader, dst string) (sha string, size int64, err error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), r)
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
