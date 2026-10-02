// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package filing contains destinations whose replay contract is explicit.
package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var ErrArtifact = errors.New("artifact_invalid")
var ErrDestinationConflict = errors.New("destination_conflict")

// Staged holds verified destination-local bytes and a verified existing target.
// Stage does all large copies and hashing before the database writer fence.
type Staged struct {
	Path     string
	Target   string
	Existing os.FileInfo
}

func Key(jobID, destination, sha string) string {
	h := sha256.Sum256([]byte(jobID + "\x00" + destination + "\x00" + sha))
	return hex.EncodeToString(h[:])
}

func Stage(ctx context.Context, source, destination, key, sha string) (*Staged, error) {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return nil, err
	}
	s := &Staged{Target: filepath.Join(destination, key+".pdf")}
	if info, err := os.Lstat(s.Target); err == nil {
		if !info.Mode().IsRegular() {
			return nil, ErrDestinationConflict
		}
		f, err := openRegular(s.Target)
		if err != nil {
			return nil, ErrDestinationConflict
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) {
			_ = f.Close()
			return nil, ErrDestinationConflict
		}
		h := sha256.New()
		_, err = io.Copy(h, &contextReader{ctx: ctx, reader: f})
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if hex.EncodeToString(h.Sum(nil)) != sha {
			return nil, ErrDestinationConflict
		}
		s.Existing = info
		return s, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	input, err := openRegular(source)
	if err != nil {
		return nil, ErrArtifact
	}
	defer func() { _ = input.Close() }()
	output, err := os.CreateTemp(destination, ".papio-"+key+"-*.partial")
	if err != nil {
		return nil, err
	}
	s.Path = output.Name()
	defer func() { _ = output.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, h), &contextReader{ctx: ctx, reader: input})
	if err != nil {
		s.Cleanup()
		return nil, err
	}
	if n == 0 || hex.EncodeToString(h.Sum(nil)) != sha {
		s.Cleanup()
		return nil, ErrArtifact
	}
	if err = output.Sync(); err != nil {
		s.Cleanup()
		return nil, err
	}
	if err = output.Chmod(0o400); err != nil {
		s.Cleanup()
		return nil, err
	}
	if err = output.Close(); err != nil {
		s.Cleanup()
		return nil, err
	}
	return s, nil
}

// Publish does no copy or hash. The caller holds its short database writer fence.
// A different target is never overwritten. A replay checks the verified inode.
func (s *Staged) Publish() error {
	if s.Existing != nil {
		current, err := os.Lstat(s.Target)
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, s.Existing) || current.Size() != s.Existing.Size() || !current.ModTime().Equal(s.Existing.ModTime()) {
			return ErrDestinationConflict
		}
	} else if err := os.Link(s.Path, s.Target); err != nil {
		if os.IsExist(err) {
			return ErrDestinationConflict
		}
		return err
	}
	return syncDirectory(filepath.Dir(s.Target))
}

func (s *Staged) Cleanup() {
	if s != nil && s.Path != "" {
		_ = os.Remove(s.Path)
	}
}

// CleanupStages removes this receipt's unpublished crash leftovers. The managed
// folder reserves this exact key prefix; no final PDF or other key is touched.
func CleanupStages(destination, key string) error {
	paths, err := filepath.Glob(filepath.Join(destination, ".papio-"+key+"-*.partial"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
