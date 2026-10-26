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
// All filesystem operations, including Publish and Undo, run outside database
// writer transactions.
type Staged struct {
	Path     string
	Target   string
	Existing os.FileInfo
	linked   bool // Publish created Target; only the worker goroutine touches it
}

func Key(jobID, destination, sha string) string {
	h := sha256.Sum256([]byte(jobID + "\x00" + destination + "\x00" + sha))
	return hex.EncodeToString(h[:])
}

// Stage runs under the package's stall bound: a destination that stops
// responding yields ErrDestinationStalled instead of blocking the caller.
func Stage(ctx context.Context, source, destination, key, sha string) (*Staged, error) {
	return run(ctx, func(ctx context.Context, w *watch) (*Staged, error) {
		return stage(ctx, w, source, destination, key, sha)
	}, func(s *Staged, _ error) {
		// The caller left before this finished; nobody will publish or clean
		// the partial it wrote.
		s.remove()
	})
}

func stage(ctx context.Context, w *watch, source, destination, key, sha string) (*Staged, error) {
	beforeIO("mkdir")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return nil, err
	}
	w.touch()
	s := &Staged{Target: filepath.Join(destination, key+".pdf")}
	if info, err := os.Lstat(s.Target); err == nil {
		if !info.Mode().IsRegular() {
			return nil, ErrDestinationConflict
		}
		f, err := openRegular(s.Target)
		if err != nil {
			if errors.Is(err, ErrArtifact) || errors.Is(err, os.ErrNotExist) {
				return nil, ErrDestinationConflict
			}
			return nil, err
		}
		opened, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if !os.SameFile(info, opened) {
			_ = f.Close()
			return nil, ErrDestinationConflict
		}
		h := sha256.New()
		_, err = io.Copy(h, &contextReader{ctx: ctx, reader: f, w: w})
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
	w.touch()
	input, err := openRegular(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrArtifact
		}
		return nil, err
	}
	defer func() { _ = input.Close() }()
	output, err := os.CreateTemp(destination, ".papio-"+key+"-*.partial")
	if err != nil {
		return nil, err
	}
	w.touch()
	s.Path = output.Name()
	defer func() { _ = output.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, h), &contextReader{ctx: ctx, reader: input, w: w})
	if err != nil {
		s.remove()
		return nil, err
	}
	if n == 0 || hex.EncodeToString(h.Sum(nil)) != sha {
		s.remove()
		return nil, ErrArtifact
	}
	beforeIO("sync")
	if err = output.Sync(); err != nil {
		s.remove()
		return nil, err
	}
	if err = output.Chmod(0o400); err != nil {
		s.remove()
		return nil, err
	}
	if err = output.Close(); err != nil {
		s.remove()
		return nil, err
	}
	return s, nil
}

// Publish does no copy or hash. The caller must not hold a database writer.
// A different target is never overwritten. A replay checks the verified inode.
// Like Stage it fails with ErrDestinationStalled on a wedged destination.
// If an abandoned link completes later, it is undone.
func (s *Staged) Publish(ctx context.Context) error {
	_, err := run(ctx, func(_ context.Context, w *watch) (struct{}, error) {
		return struct{}{}, s.publish(w)
	}, func(_ struct{}, _ error) {
		_ = s.undo()
		s.remove()
	})
	return err
}

func (s *Staged) publish(w *watch) error {
	if s.Existing != nil {
		beforeIO("verify")
		current, err := os.Lstat(s.Target)
		if err != nil {
			if os.IsNotExist(err) {
				return ErrDestinationConflict
			}
			return err
		}
		if !current.Mode().IsRegular() || !os.SameFile(current, s.Existing) || current.Size() != s.Existing.Size() || !current.ModTime().Equal(s.Existing.ModTime()) {
			return ErrDestinationConflict
		}
	} else {
		beforeIO("link")
		if err := os.Link(s.Path, s.Target); err != nil {
			if os.IsExist(err) {
				return ErrDestinationConflict
			}
			return err
		}
		s.linked = true
	}
	w.touch()
	beforeIO("syncdir")
	return syncDirectory(filepath.Dir(s.Target))
}

// Cleanup removes the staged partial. It never waits on a wedged destination:
// a leftover is swept by CleanupStages once the receipt is filed.
func (s *Staged) Cleanup() {
	if s == nil || s.Path == "" {
		return
	}
	_, _ = run(context.Background(), func(context.Context, *watch) (struct{}, error) {
		s.remove()
		return struct{}{}, nil
	}, nil)
}

// remove deletes the partial directly. It is for code already running inside
// the single-flight gate.
func (s *Staged) remove() {
	if s != nil && s.Path != "" {
		_ = os.Remove(s.Path)
	}
}

// CleanupStages removes this receipt's unpublished crash leftovers. The managed
// folder reserves this exact key prefix; no final PDF or other key is touched.
func CleanupStages(ctx context.Context, destination, key string) error {
	_, err := run(ctx, func(context.Context, *watch) (struct{}, error) {
		return struct{}{}, cleanupStages(destination, key)
	}, nil)
	return err
}

func cleanupStages(destination, key string) error {
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

// contextReader stops a copy between reads and reports each one as progress.
// It cannot interrupt a read or the paired write blocked in the kernel; the
// stall bound in run covers that.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
	w      *watch
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	r.w.touch()
	n, err := r.reader.Read(p)
	r.w.touch()
	return n, err
}
