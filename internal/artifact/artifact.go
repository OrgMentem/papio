// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package artifact owns the quarantine area and the immutable content-addressed
// store: validated files are atomically published to artifacts/<sha256>.pdf by
// hard link on supported filesystems (rename where hard links are unsupported)
// and never mutated afterward. The file is flushed before it becomes visible
// and the artifacts directory is flushed before Promote reports success, so a
// caller that commits a database row after Promote never points at a name or
// bytes that a power loss can revert. Quarantine cleanup after publication is
// best-effort and destination-authoritative, so a surviving quarantine name
// does not mean the artifact is unpublished. Re-fetching the same bytes is a
// no-op by construction, which is what makes crash recovery duplicate-free.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"papio/internal/config"
)

// Layout under the data directory.
const (
	quarantineDir = "quarantine"
	artifactsDir  = "artifacts"
)

// linkFile lets tests simulate filesystems that do not support hard links.
// chmodFile and removeFile let tests inject pre- and post-publication failures.
// syncFile and syncDir let tests inject durability failures and observe the
// order of the flushes around publication. config.SyncDir is a no-op on
// Windows, which cannot flush a directory through a handle: the durability of
// a published name there rests on NTFS journaling its link and rename
// metadata, while the file's bytes are still flushed by syncRegularFile.
var (
	linkFile   = os.Link
	chmodFile  = os.Chmod
	removeFile = os.Remove
	syncFile   = syncRegularFile
	syncDir    = config.SyncDir
)

// Store manages quarantine and immutable artifact paths under dataDir.
type Store struct{ dataDir string }

// New creates the layout (0700) and returns the store.
func New(dataDir string) (*Store, error) {
	for _, d := range []string{filepath.Join(dataDir, quarantineDir), filepath.Join(dataDir, artifactsDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("creating %s: %w", d, err)
		}
	}
	return &Store{dataDir: dataDir}, nil
}

// validJobDir rejects every job id that would not name a fresh child directory
// under quarantine/. Separators are the obvious case; the dot segments are the
// dangerous one, because filepath.Join(dataDir, "quarantine", "..") collapses
// to dataDir itself, and CleanQuarantine would then remove the entire data
// directory — every published artifact and the database beside them. No
// caller can reach that today (both pass ids minted by job.NewID), so this is
// a guard against a future caller, not a live bug.
func validJobDir(jobID string) error {
	if jobID == "" || jobID == "." || jobID == ".." || strings.ContainsAny(jobID, "/\\") {
		return fmt.Errorf("invalid job id %q", jobID)
	}
	return nil
}

// QuarantineDir returns (and creates) the per-job quarantine directory.
func (s *Store) QuarantineDir(jobID string) (string, error) {
	if err := validJobDir(jobID); err != nil {
		return "", err
	}
	d := filepath.Join(s.dataDir, quarantineDir, jobID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// CleanQuarantine removes a job's quarantine directory.
func (s *Store) CleanQuarantine(jobID string) error {
	if err := validJobDir(jobID); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.dataDir, quarantineDir, jobID))
}

// ArtifactPath returns the immutable path for a hash (existence not implied).
func (s *Store) ArtifactPath(sha string) (string, error) {
	if len(sha) != 64 || strings.ToLower(sha) != sha {
		return "", fmt.Errorf("invalid sha256 %q", sha)
	}
	return filepath.Join(s.dataDir, artifactsDir, sha+".pdf"), nil
}

// Promote moves a validated quarantine file into the immutable store,
// verifying its hash on the way. Idempotent: if the artifact already exists
// with matching content, the temp file is discarded and the existing path is
// returned with created false. The call that atomically makes the artifact
// visible returns created true. The temp file must live on the same filesystem
// (it does; both are under dataDir), allowing link to atomically claim the
// destination. On filesystems without hard-link support, Promote checks that
// the destination is absent before renaming instead; that fallback is weaker
// against cross-process publication races.
//
// Promote flushes the quarantine file before publication and the artifacts
// directory after it, on every success path including the reuse of an existing
// destination, so a concurrent publisher's name is durable too. A flush
// failure fails the call: the caller must not record the artifact as
// published. After a directory-flush failure the destination may already be
// visible and the quarantine file still exists, so a retry converges through
// the existing-destination path.
func (s *Store) Promote(tempPath, expectedSHA string) (path string, created bool, err error) {
	dest, err := s.ArtifactPath(expectedSHA)
	if err != nil {
		return "", false, err
	}
	sha, size, err := HashFile(tempPath)
	if err != nil {
		return "", false, fmt.Errorf("hashing quarantine file: %w", err)
	}
	if size == 0 {
		return "", false, fmt.Errorf("refusing to promote empty file")
	}
	if sha != expectedSHA {
		return "", false, fmt.Errorf("quarantine file hash %s does not match expected %s", sha, expectedSHA)
	}

	// The bytes must be on stable storage before any name can expose them: a
	// link or rename that survives a power loss while the data does not would
	// publish a truncated artifact under its full-content hash name.
	if err := syncFile(tempPath); err != nil {
		return "", false, fmt.Errorf("syncing quarantine file: %w", err)
	}

	// Apply read-only mode before the artifact becomes visible, so it is never
	// published writable. This is the last fallible step before the atomic
	// publication (link or rename). Only Windows needs the mode again after
	// cleanup (reassertReadOnly).
	if err := chmodFile(tempPath, 0o400); err != nil {
		return "", false, err
	}

	renamed := false
	if err := linkFile(tempPath, dest); err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			if err := validateExisting(dest, expectedSHA); err != nil {
				return "", false, err
			}
		case linkUnsupported(err):
			if _, statErr := os.Lstat(dest); statErr == nil {
				if err := validateExisting(dest, expectedSHA); err != nil {
					return "", false, err
				}
			} else if !os.IsNotExist(statErr) {
				return "", false, fmt.Errorf("checking existing artifact: %w", statErr)
			} else if err := os.Rename(tempPath, dest); err != nil {
				if !errors.Is(err, fs.ErrExist) {
					return "", false, fmt.Errorf("promoting artifact without hard links: %w", err)
				}
				if err := validateExisting(dest, expectedSHA); err != nil {
					return "", false, err
				}
			} else {
				renamed = true
				created = true
			}
		default:
			return "", false, fmt.Errorf("promoting artifact: %w", err)
		}
	} else {
		created = true
	}
	// The publication (or the reuse of another publisher's destination) is
	// visible; make its directory entry durable before reporting success.
	// Failure leaves the destination and the quarantine file in place, which
	// is the state a retry or crash recovery expects.
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return "", false, fmt.Errorf("syncing artifact directory: %w", err)
	}
	// Temp cleanup is best-effort: a failure to remove the (now hard-linked or
	// duplicate) quarantine file must not be reported as a promotion failure
	// because the artifact IS published. Callers that roll back artifact
	// metadata on Promote failure would incorrectly delete a valid artifact if
	// cleanup were treated as fatal.
	if !renamed {
		_ = removeFile(tempPath)
		if created {
			// The removed name was a hard link to the artifact; see
			// reassertReadOnly for why that can cost the read-only mode.
			reassertReadOnly(dest)
		}
	}
	return dest, created, nil
}

func validateExisting(dest, expectedSHA string) error {
	got, existingSize, err := HashFile(dest)
	if err != nil {
		return fmt.Errorf("hashing existing artifact: %w", err)
	}
	if existingSize == 0 || got != expectedSHA {
		return fmt.Errorf("existing artifact %s has hash %s, want %s", dest, got, expectedSHA)
	}
	return nil
}

func linkUnsupported(err error) bool {
	var linkErr *os.LinkError
	return errors.As(err, &linkErr) &&
		(errors.Is(linkErr.Err, syscall.EPERM) ||
			errors.Is(linkErr.Err, syscall.ENOTSUP) ||
			errors.Is(linkErr.Err, syscall.EOPNOTSUPP))
}

// Verify re-hashes a stored artifact against its name.
func (s *Store) Verify(sha string) error {
	path, err := s.ArtifactPath(sha)
	if err != nil {
		return err
	}
	got, size, err := HashFile(path)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("artifact %s is corrupt: content hash %s (size %d)", sha, got, size)
	}
	return nil
}

// SyncPublished flushes an already-published artifact's bytes and its
// directory entry. Crash recovery calls it before committing a journal row
// whose destination already exists: the interrupted Promote may have exposed
// the name before its flushes ran, so the name's durability is unproven until
// this succeeds. A failure means the artifact must not yet be recorded as
// published.
func (s *Store) SyncPublished(sha string) error {
	dest, err := s.ArtifactPath(sha)
	if err != nil {
		return err
	}
	if err := syncFile(dest); err != nil {
		return fmt.Errorf("syncing published artifact: %w", err)
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("syncing artifact directory: %w", err)
	}
	return nil
}

// HashFile returns the SHA-256 hex digest and size of a file.
func HashFile(path string) (digest string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			digest, size, err = "", 0, closeErr
		}
	}()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ConfineRegularFile rejects paths that are not plain files inside root
// (symlinks, devices, directories, or traversal outside root). Used for
// browser-download adoption in Phase 2 and quarantine hygiene now.
func ConfineRegularFile(root, path string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %s escapes root %s", abs, absRoot)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %s is a symlink", abs)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path %s is not a regular file", abs)
	}
	return nil
}
