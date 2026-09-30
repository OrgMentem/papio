// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/artifact"
	"papio/internal/delivery"
	"papio/internal/job"
	"papio/internal/store"
)

// suppliedPDFDirName is the data-directory child that holds main PDFs the
// operator staged with `papio jobs supply-pdf`, one subdirectory per job.
const suppliedPDFDirName = "supplied"

// These sentinels let the RPC layer tell the operator what to change. The
// wrapped text stays daemon-side for the log where it can carry a path.
var (
	// ErrSuppliedPDFName reports a staged name that is not a plain file name,
	// or a staged file that is missing, a symlink, or not a regular file
	// inside the job's staging directory.
	ErrSuppliedPDFName = errors.New("supplied PDF staging name rejected")
	// ErrSuppliedPDFSize reports a staged file larger than fetch.max_bytes.
	ErrSuppliedPDFSize = errors.New("supplied PDF is larger than fetch.max_bytes")
	// ErrSuppliedPDFState reports a live job that cannot take a main PDF
	// now: one already validating or held for review, one a worker holds,
	// or one waiting on a live document-delivery request. It also carries
	// the notice that a failed supply left a job parked awaiting a PDF.
	ErrSuppliedPDFState = errors.New("job cannot take a supplied PDF")
)

// suppliedPDFCompensationTimeout bounds the write that puts a job back when
// a supply fails after parking it. It runs detached from the caller, whose
// context may be the reason the supply failed.
const suppliedPDFCompensationTimeout = 2 * time.Second

// SuppliedPDFJobFinishedError reports a terminal job. A finished job never
// takes new bytes: the operator submits the work again and supplies the PDF
// to the new job.
type SuppliedPDFJobFinishedError struct {
	JobID string
	State string
}

func (e *SuppliedPDFJobFinishedError) Error() string {
	return fmt.Sprintf("job %s is %s, which is final; it cannot take a supplied PDF", e.JobID, e.State)
}

// SupplyResult is what SupplyPDF did with one staged file. Outcome is one of
// AdoptionAccepted, AdoptionNeedsReview, or AdoptionRejected.
type SupplyResult struct {
	CandidateID int64
	SHA256      string
	Outcome     string
}

// SuppliedPDFStagingDir returns <dataDir>/supplied/<jobID>, the only
// directory SupplyPDF reads from. It does not create the directory. The CLI
// copies the operator's file there, so the daemon never opens a path that a
// caller named.
func SuppliedPDFStagingDir(dataDir, jobID string) (string, error) {
	if !plainFileName(jobID) {
		return "", fmt.Errorf("invalid job id %q", jobID)
	}
	return filepath.Join(dataDir, suppliedPDFDirName, jobID), nil
}

// plainFileName reports whether name is one path element that stays where it
// is joined: not empty, not a dot segment, no separator of either platform,
// and no volume or reserved name.
func plainFileName(name string) bool {
	return filepath.IsLocal(name) && !strings.ContainsAny(name, `/\`) && name != "."
}

// SupplyPDF adopts a main PDF the operator staged for jobID with `papio jobs
// supply-pdf`. name is a file name inside SuppliedPDFStagingDir(jobID); any
// other shape is refused before the filesystem is touched, so a caller of the
// RPC can make the daemon read nothing but a file in that directory.
//
// The file then takes the browser adoption path: a live job is parked at
// awaiting_human, and the bytes are copied into quarantine and run through
// the same payload, structure and identity validation. A PDF of a different
// work goes to identity review or is rejected; it never becomes ready because
// it exists. The candidate's source is job.CandidateSourceOperator, so the
// producer record names the bytes manual. The staged file is removed once the
// daemon has read it or refused it; the operator's original file is never
// touched.
//
// Unlike a browser download, a supplied PDF is also accepted for a job in
// retry_wait — where a paywalled job usually sits between attempts — by
// following retry_wait -> resolving first. A job pinned to a live
// document-delivery request is refused in every state: its provider request
// would be left behind, so the operator settles that request first. The
// refusal is checked up front and again inside the park and the
// awaiting_human -> validating transactions, so a request that goes live
// while the file is read still stops the supply.
//
// If the supply fails after it parked a queued or retry_wait job and before
// validation took the job, unparkSuppliedPDFJob puts the job back where the
// state graph allows it.
func (s *Service) SupplyPDF(ctx context.Context, jobID, name string) (SupplyResult, error) {
	if !plainFileName(name) {
		return SupplyResult{}, fmt.Errorf("%w: %q is not a file name", ErrSuppliedPDFName, name)
	}
	stageDir, err := SuppliedPDFStagingDir(s.Config.DataDir, jobID)
	if err != nil {
		return SupplyResult{}, fmt.Errorf("%w: %w", ErrSuppliedPDFName, err)
	}
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return SupplyResult{}, err
	}
	if job.Terminal(row.State) {
		return SupplyResult{}, &SuppliedPDFJobFinishedError{JobID: jobID, State: row.State}
	}
	// Refused before the staged file is touched, so the operator can supply
	// the same file once the request is settled.
	if err := refuseLiveDeliveryRequest(ctx, s.Jobs.S.DB(), jobID); err != nil {
		return SupplyResult{}, err
	}
	staged, info, err := confineSuppliedPDF(stageDir, name)
	if err != nil {
		return SupplyResult{}, err
	}
	defer func() {
		_ = os.Remove(staged)
		_ = os.Remove(stageDir) // only when no other staged file remains
	}()
	limit := s.Config.Fetch.MaxBytes
	if info.Size() > limit {
		return SupplyResult{}, fmt.Errorf("%w: %d bytes, limit %d", ErrSuppliedPDFSize, info.Size(), limit)
	}
	// The job is parked only after the staged file is confined, so a refused
	// request leaves a live job where it was.
	var park suppliedPDFPark
	row, err = s.prepareMainAdoption(ctx, jobID, func(ctx context.Context) error {
		var parkErr error
		park, parkErr = s.parkSuppliedPDFJob(ctx, jobID)
		return parkErr
	})
	if err != nil {
		if park.from != "" {
			return SupplyResult{}, s.unparkSuppliedPDFJob(ctx, jobID, park, err)
		}
		var stateErr *adoptionStateError
		if errors.As(err, &stateErr) {
			// The job can finish between the check above and the park.
			if job.Terminal(stateErr.state) {
				return SupplyResult{}, &SuppliedPDFJobFinishedError{JobID: jobID, State: stateErr.state}
			}
			return SupplyResult{}, fmt.Errorf("%w: %w", ErrSuppliedPDFState, err)
		}
		return SupplyResult{}, err
	}
	adopted, err := s.adoptMainPDF(ctx, row, mainPDFOrigin{
		source:    job.CandidateSourceOperator,
		url:       "operator://supplied-pdf",
		keyPrefix: "operator-supply:sha256:",
		copyInto: func(dst string) (string, int64, error) {
			return copySuppliedPDF(staged, info, dst, limit)
		},
		transition: func(ctx context.Context, jobID string, candidateID int64) error {
			return s.Jobs.TransitionAwaitingToValidatingForSuppliedPDF(ctx, jobID, candidateID,
				func(ctx context.Context, tx *sql.Tx) error { return refuseLiveDeliveryRequest(ctx, tx, jobID) })
		},
		rejected: s.rejectSuppliedPDF,
	})
	if err != nil && !adopted.validating && park.from != "" {
		err = s.unparkSuppliedPDFJob(ctx, jobID, park, err)
	}
	return SupplyResult{CandidateID: adopted.candidateID, SHA256: adopted.sha256, Outcome: adopted.outcome}, err
}

// rowQuerier is a *sql.DB or a *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// refuseLiveDeliveryRequest returns an ErrSuppliedPDFState error when jobID
// is pinned to a document-delivery request the provider may still fulfil
// (submitted or pending). It reads the request jobs.delivery_request_id pins,
// as delivery.Service.GetByJobID does, through q, so a caller can run it
// inside the transaction that moves the job.
func refuseLiveDeliveryRequest(ctx context.Context, q rowQuerier, jobID string) error {
	var live int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM jobs
		JOIN delivery_requests ON delivery_requests.id = jobs.delivery_request_id
		WHERE jobs.id = ? AND delivery_requests.state IN (?, ?)`,
		jobID, string(delivery.StateSubmitted), string(delivery.StatePending)).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the job's document-delivery request: %w", err)
	}
	return fmt.Errorf("%w: job %s is waiting on a document-delivery request; check it with `papio delivery get %s` and settle or cancel it before you supply a PDF", ErrSuppliedPDFState, jobID, jobID)
}

// suppliedPDFPark is what parkSuppliedPDFJob changed. from is empty when it
// changed nothing.
type suppliedPDFPark struct {
	// from is the state the job was parked from, and retryAt its retry_at.
	from    string
	retryAt string
	// actionID is the manual_download action the park opened, or 0 when the
	// park refreshed one that was already open.
	actionID int64
}

// parkSuppliedPDFJob moves a live job to awaiting_human for a supplied PDF
// and opens a manual_download action, so the adoption fence sees a genuine
// awaiting-human action; the operator's command is the human gesture that
// justified the park. queued and retry_wait have no direct edge to
// awaiting_human, so they follow their legal edge to resolving first.
//
// Everything happens in one transaction: the state read, the live
// document-delivery refusal, the transitions and the action. The store has
// one connection, so no scheduler retry or delivery poll can commit between
// the refusal and the park. A resolving or fetching job whose lease is live
// is refused: a worker is in the middle of an attempt that may be about to
// submit a delivery request, and taking the job from it would strand that
// request.
func (s *Service) parkSuppliedPDFJob(ctx context.Context, jobID string) (suppliedPDFPark, error) {
	tx, err := s.Jobs.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return suppliedPDFPark{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := suppliedPDFJobTx(ctx, tx, jobID)
	if err != nil {
		return suppliedPDFPark{}, err
	}
	switch current.State {
	case job.StateAwaitingHuman:
		return suppliedPDFPark{}, nil
	case job.StateQueued, job.StateRetryWait, job.StateResolving, job.StateFetching:
	default:
		return suppliedPDFPark{}, &adoptionStateError{jobID: jobID, state: current.State}
	}
	if err := refuseLiveDeliveryRequest(ctx, tx, jobID); err != nil {
		return suppliedPDFPark{}, err
	}
	if current.LeaseActive(s.Now()) {
		return suppliedPDFPark{}, fmt.Errorf("%w: papio is working on job %s right now (state %s); supply the PDF again once `papio jobs get %s` shows it waiting", ErrSuppliedPDFState, jobID, current.State, jobID)
	}
	now := store.FormatTime(s.Now())
	from := current.State
	if from == job.StateQueued || from == job.StateRetryWait {
		if err := transitionSuppliedPDFTx(ctx, s.Jobs, tx, jobID, from, job.StateResolving, "operator_supplied_pdf", "", now); err != nil {
			return suppliedPDFPark{}, err
		}
		from = job.StateResolving
	}
	if err := transitionSuppliedPDFTx(ctx, s.Jobs, tx, jobID, from, job.StateAwaitingHuman, "operator_supplied_pdf", "", now); err != nil {
		return suppliedPDFPark{}, err
	}
	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM human_actions WHERE job_id = ? AND kind = ? AND status = 'open' LIMIT 1`,
		jobID, job.CandidateEligibleKind).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return suppliedPDFPark{}, err
	}
	actionID, err := s.Jobs.OpenHumanActionTx(ctx, tx, jobID, job.CandidateEligibleKind, "please download the paper", now, job.Access(false, ""))
	if err != nil {
		return suppliedPDFPark{}, err
	}
	if err := tx.Commit(); err != nil {
		return suppliedPDFPark{}, err
	}
	park := suppliedPDFPark{from: current.State, retryAt: current.RetryAt}
	if existing == 0 {
		park.actionID = actionID
	}
	return park, nil
}

// suppliedPDFJobTx reads the fields the supply park and its compensation
// decide on, through tx only.
func suppliedPDFJobTx(ctx context.Context, tx *sql.Tx, jobID string) (job.Row, error) {
	row := job.Row{ID: jobID}
	err := tx.QueryRowContext(ctx, `
		SELECT state, COALESCE(retry_at, ''), COALESCE(lease_owner, ''), COALESCE(lease_expires_at, '')
		FROM jobs WHERE id = ?`, jobID).Scan(&row.State, &row.RetryAt, &row.LeaseOwner, &row.LeaseExpiresAt)
	return row, err
}

// transitionSuppliedPDFTx is one job.Store.TransitionTx move with the detail
// shape Store.Transition records.
func transitionSuppliedPDFTx(ctx context.Context, jobs *job.Store, tx *sql.Tx, jobID, from, to, reason, retryAt, now string) error {
	detail, err := json.Marshal(map[string]any{"reason": reason, "from": from, "to": to})
	if err != nil {
		return err
	}
	return jobs.TransitionTx(ctx, tx, jobID, from, to, string(detail), job.TransitionTxConfig{RetryAt: retryAt}, now)
}

// unparkSuppliedPDFJob compensates for a supply that failed after
// parkSuppliedPDFJob parked a queued or retry_wait job and before validation
// took the job; cause is that failure. A park from resolving or fetching is
// kept: a job there without a live lease was stranded, and awaiting_human is
// where a human can reach it.
//
// A job from retry_wait goes back to retry_wait with its retry_at, and the
// action the park opened is cancelled, in one transaction; cause is then
// returned. queued has no legal edge back from awaiting_human, so a job from
// there stays parked with its manual_download action, and so does a
// retry_wait job the restore cannot reach. The operator then gets an
// ErrSuppliedPDFState error that says the job is parked awaiting a PDF. The
// cause itself goes to the daemon log: it can carry a path, and wrapping it
// would let its own sentinel hide the notice from the RPC seam.
func (s *Service) unparkSuppliedPDFJob(ctx context.Context, jobID string, park suppliedPDFPark, cause error) error {
	if park.from != job.StateQueued && park.from != job.StateRetryWait {
		return cause
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), suppliedPDFCompensationTimeout)
	defer cancel()
	parked, restoreErr := s.restoreSuppliedPDFPark(restoreCtx, jobID, park)
	if !parked && restoreErr == nil {
		return cause
	}
	log.Printf("papio: supply-pdf for job %s failed after parking it from %s: %v", jobID, park.from, errors.Join(cause, restoreErr))
	why := "papio cannot return a job to queued"
	if park.from == job.StateRetryWait {
		why = "papio could not return it to retry_wait"
	}
	// Kept under the RPC seam's 500-character message bound: past it the
	// operator would get the bare sentinel, not the parked notice.
	return fmt.Errorf("%w: job %s is parked awaiting a PDF, with a manual_download action open, because %s; the supply failed: %s",
		ErrSuppliedPDFState, jobID, why, suppliedPDFFailureSummary(cause))
}

// restoreSuppliedPDFPark puts a job parkSuppliedPDFJob parked from
// retry_wait back there. It reports whether the job is still parked by the
// supply: false when it restored the job, or when something else already
// moved it off awaiting_human. It leaves a job another adoption holds alone.
func (s *Service) restoreSuppliedPDFPark(ctx context.Context, jobID string, park suppliedPDFPark) (bool, error) {
	tx, err := s.Jobs.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := suppliedPDFJobTx(ctx, tx, jobID)
	if err != nil {
		return true, err
	}
	switch {
	case current.State != job.StateAwaitingHuman:
		return false, nil
	case park.from != job.StateRetryWait || current.LeaseActive(s.Now()):
		return true, nil
	}
	now := store.FormatTime(s.Now())
	retryAt := park.retryAt
	if retryAt == "" {
		retryAt = now
	}
	if err := transitionSuppliedPDFTx(ctx, s.Jobs, tx, jobID, job.StateAwaitingHuman, job.StateRetryWait,
		"operator_supplied_pdf_failed", retryAt, now); err != nil {
		return true, err
	}
	if park.actionID != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE human_actions SET status = 'cancelled', resolved_at = ? WHERE id = ? AND status = 'open'`,
			now, park.actionID); err != nil {
			return true, err
		}
	}
	if err := tx.Commit(); err != nil {
		return true, err
	}
	return false, nil
}

// suppliedPDFFailureSummary names why a supply failed in words that carry no
// path: the sentinel's own text for a staging failure, the message of a state
// refusal (built in this package, path-free), and a pointer to the daemon log
// otherwise.
func suppliedPDFFailureSummary(cause error) string {
	switch {
	case errors.Is(cause, ErrSuppliedPDFName):
		return ErrSuppliedPDFName.Error()
	case errors.Is(cause, ErrSuppliedPDFSize):
		return ErrSuppliedPDFSize.Error()
	case errors.Is(cause, ErrSuppliedPDFState):
		return strings.TrimPrefix(cause.Error(), ErrSuppliedPDFState.Error()+": ")
	default:
		return "see the daemon log"
	}
}

// confineSuppliedPDF resolves name inside the job's staging directory. The
// staging root and the job directory must be real directories: a symlink
// planted in their place could otherwise point the daemon at a file elsewhere
// on disk. The file itself must be a regular file, not a symlink.
func confineSuppliedPDF(stageDir, name string) (string, os.FileInfo, error) {
	for _, dir := range []string{filepath.Dir(stageDir), stageDir} {
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("%w: nothing is staged for this job", ErrSuppliedPDFName)
		}
		if err != nil {
			return "", nil, fmt.Errorf("reading the supplied PDF staging directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", nil, fmt.Errorf("%w: %s is not a directory", ErrSuppliedPDFName, dir)
		}
	}
	// Ancestors of the data directory may be symlinks (/var -> /private/var);
	// the two directories above were checked themselves.
	realDir, err := filepath.EvalSymlinks(stageDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolving the supplied PDF staging directory: %w", err)
	}
	path := filepath.Join(realDir, name)
	if err := artifact.ConfineRegularFile(realDir, path); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrSuppliedPDFName, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrSuppliedPDFName, err)
	}
	return path, info, nil
}

// copySuppliedPDF copies the confined staged file into dst while hashing it.
// It refuses a file that is no longer the one confinement checked (a swap for
// a symlink or another file in between), and it stops past limit, so a file
// that grows after the size check cannot exceed the bound either.
func copySuppliedPDF(src string, checked os.FileInfo, dst string, limit int64) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrSuppliedPDFName, err)
	}
	defer func() { _ = in.Close() }()
	opened, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(checked, opened) {
		return "", 0, fmt.Errorf("%w: the staged file changed before it was read", ErrSuppliedPDFName)
	}
	sha, size, err := writeHashed(io.LimitReader(in, limit+1), dst)
	if err != nil {
		return "", 0, err
	}
	if size > limit {
		_ = os.Remove(dst)
		return "", 0, fmt.Errorf("%w: more than %d bytes", ErrSuppliedPDFSize, limit)
	}
	return sha, size, nil
}

// rejectSuppliedPDF re-parks a job whose supplied PDF failed validation in
// awaiting_human and asks for a different file. Unlike a browser download
// there is nothing to move aside: no sweep reads the staging directory, and
// the staged copy is removed when SupplyPDF returns.
func (s *Service) rejectSuppliedPDF(ctx context.Context, jobID string, replacementAccess job.AccessClassification) error {
	if _, err := s.Jobs.OpenHumanAction(ctx, jobID, "manual_download",
		"the supplied PDF failed validation; please supply a different file",
		replacementAccess, job.WithHumanActionDiagnosis(job.DiagnosisReasonAdoptedPDFInvalid)); err != nil {
		return err
	}
	return s.park(ctx, jobID, job.StateFetching, job.StateAwaitingHuman,
		map[string]any{"reason": "supplied_pdf_rejected"})
}
