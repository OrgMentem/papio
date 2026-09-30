// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"papio/internal/artifact"
	"papio/internal/job"
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
	// ErrSuppliedPDFState reports a job that cannot take a main PDF: a
	// terminal job, or one already validating, held for review, or waiting
	// to retry.
	ErrSuppliedPDFState = errors.New("job cannot take a supplied PDF")
)

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
// awaiting_human through the same legal edges, and the bytes are copied into
// quarantine and run through the same payload, structure and identity
// validation. A PDF of a different work goes to identity review or is
// rejected; it never becomes ready because it exists. The candidate's source
// is job.CandidateSourceOperator, so the producer record names the bytes
// manual. The staged file is removed once the daemon has read it or refused
// it; the operator's original file is never touched.
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
		return SupplyResult{}, fmt.Errorf("%w: job %s is %s, which is final", ErrSuppliedPDFState, jobID, row.State)
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
	row, err = s.prepareMainAdoption(ctx, jobID, "operator_supplied_pdf")
	if err != nil {
		var stateErr *adoptionStateError
		if errors.As(err, &stateErr) {
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
		transition: s.Jobs.TransitionAwaitingToValidatingForSuppliedPDF,
		rejected:   s.rejectSuppliedPDF,
	})
	return SupplyResult{CandidateID: adopted.candidateID, SHA256: adopted.sha256, Outcome: adopted.outcome}, err
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
