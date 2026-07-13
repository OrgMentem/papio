// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package artifact owns the quarantine area and the immutable content-addressed
// store: validated files are atomically renamed to artifacts/<sha256>.pdf and
// never mutated afterward. Re-fetching the same bytes is a no-op by
// construction, which is what makes crash recovery duplicate-free.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Layout under the data directory.
const (
	quarantineDir = "quarantine"
	artifactsDir  = "artifacts"
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

// QuarantineDir returns (and creates) the per-job quarantine directory.
func (s *Store) QuarantineDir(jobID string) (string, error) {
	if strings.ContainsAny(jobID, "/\\") || jobID == "" {
		return "", fmt.Errorf("invalid job id %q", jobID)
	}
	d := filepath.Join(s.dataDir, quarantineDir, jobID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// CleanQuarantine removes a job's quarantine directory.
func (s *Store) CleanQuarantine(jobID string) error {
	if strings.ContainsAny(jobID, "/\\") || jobID == "" {
		return fmt.Errorf("invalid job id %q", jobID)
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
// returned. The temp file must live on the same filesystem (it does; both are
// under dataDir), making the rename atomic.
func (s *Store) Promote(tempPath, expectedSHA string) (string, error) {
	dest, err := s.ArtifactPath(expectedSHA)
	if err != nil {
		return "", err
	}
	sha, size, err := HashFile(tempPath)
	if err != nil {
		return "", fmt.Errorf("hashing quarantine file: %w", err)
	}
	if size == 0 {
		return "", fmt.Errorf("refusing to promote empty file")
	}
	if sha != expectedSHA {
		return "", fmt.Errorf("quarantine file hash %s does not match expected %s", sha, expectedSHA)
	}
	if _, err := os.Stat(dest); err == nil {
		// Content-addressed: same hash, same bytes. Drop the duplicate.
		_ = os.Remove(tempPath)
		return dest, nil
	}
	if err := os.Rename(tempPath, dest); err != nil {
		return "", fmt.Errorf("promoting artifact: %w", err)
	}
	if err := os.Chmod(dest, 0o400); err != nil { // immutable by convention: read-only
		return "", err
	}
	return dest, nil
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

// HashFile returns the SHA-256 hex digest and size of a file.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
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
