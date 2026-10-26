// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// Undo compensates a publish whose receipt did not commit. It leaves preexisting
// targets alone and uses the same stall bound and single-flight gate as Publish.
func (s *Staged) Undo() error {
	_, err := run(context.Background(), func(context.Context, *watch) (struct{}, error) {
		return struct{}{}, s.undo()
	}, nil)
	return err
}

func (s *Staged) undo() error {
	if !s.linked {
		return nil
	}
	source, err := os.Lstat(s.Path)
	if err != nil {
		return err
	}
	target, err := os.Lstat(s.Target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(source, target) {
		return ErrDestinationConflict
	}
	if err := os.Remove(s.Target); err != nil {
		return err
	}
	s.linked = false
	return syncDirectory(filepath.Dir(s.Target))
}

// UndoPublished recovers an unreceipted publication after restart. The journal's
// deterministic name and artifact hash identify our bytes; a different file,
// symlink, or changed inode is never removed.
func UndoPublished(ctx context.Context, destination, key, sha string) error {
	_, err := run(ctx, func(ctx context.Context, w *watch) (struct{}, error) {
		return struct{}{}, undoPublished(ctx, w, destination, key, sha)
	}, nil)
	return err
}

func undoPublished(ctx context.Context, w *watch, destination, key, sha string) error {
	target := filepath.Join(destination, key+".pdf")
	beforeIO("undo_verify")
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		// A prior removal may have completed before its directory sync or
		// journal update. Finish that durability step before settling.
		// A missing destination may be an offline mount, not proof that
		// its publication disappeared.
		dir, err := os.Stat(destination)
		if err != nil {
			return err
		}
		if !dir.IsDir() {
			return ErrDestinationConflict
		}
		return syncDirectory(destination)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrDestinationConflict
	}
	f, err := openRegular(target)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return ErrDestinationConflict
	}
	h := sha256.New()
	if _, err := io.Copy(h, &contextReader{ctx: ctx, reader: f, w: w}); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return ErrDestinationConflict
	}
	current, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return ErrDestinationConflict
	}
	if err := f.Close(); err != nil {
		return err
	}
	beforeIO("undo_remove")
	if err := os.Remove(target); err != nil {
		return err
	}
	w.touch()
	return syncDirectory(destination)
}
