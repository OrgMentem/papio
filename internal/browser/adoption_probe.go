// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// scanAdoptionDirWithProbe is deliberately separate from settledFileIn: grabs
// still deliver every settled file to their own validation/outcome pipeline.
// A job-folder scan has no completion frame, so non-PDF bytes must not consume
// its handoff. Refusal changes no file or job state and is retried from disk.
func (b *Bridge) scanAdoptionDirWithProbe(jobID string, probe func(string, string) bool) (string, bool) {
	for _, root := range b.cfg.AdoptionRoots() {
		dir := filepath.Join(root, jobID)
		entries, err := b.readAdoptionDirWith(dir, func(dir string) ([]os.DirEntry, error) {
			readDir := b.readDir
			if readDir == nil {
				readDir = os.ReadDir
			}
			entries, err := readDir(dir)
			if err != nil {
				return nil, err
			}
			name, ok := settledFileName(entries)
			if !ok || !probe(dir, name) {
				return nil, nil
			}
			for _, entry := range entries {
				if entry.Name() == name {
					return []os.DirEntry{entry}, nil
				}
			}
			return nil, nil
		})
		if err == nil {
			if len(entries) == 1 {
				return entries[0].Name(), true
			}
			// Adoption resolves the first existing job directory again. Do
			// not approve a legacy file but then ingest an effective-root
			// wrapper (or partial/ambiguous file) with the same name.
			return "", false
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false
		}
	}
	return "", false
}

func probeAdoptionPDF(dir, name string) bool {
	return probeAdoptionPDFWithRead(dir, name, io.ReadFull)
}

// This is a five-byte exclusion gate, not PDF validation or proof of download
// completion. The payload validator already requires %PDF- at byte zero. A
// short read, an I/O error, or a changing file defers the scan just like HTML.
// PDF-shaped malformed bytes still go through the ordinary full validator.
// The caller bounds ALL filesystem work, including OpenRoot, Lstat and Close.
func probeAdoptionPDFWithRead(dir, name string, read func(io.Reader, []byte) (int, error)) bool {
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() {
		return false
	}
	// OpenRoot confines a replacement symlink even if the final component
	// changes after Lstat. Descriptor/path identity checks below reject it.
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !sameAdoptionFile(before, opened) {
		return false
	}
	var header [5]byte
	n, err := read(f, header[:])
	if err != nil || n != len(header) || string(header[:]) != "%PDF-" {
		return false
	}
	after, err := f.Stat()
	if err != nil || !sameAdoptionFile(before, after) {
		return false
	}
	current, err := root.Lstat(name)
	return err == nil && sameAdoptionFile(before, current)
}

func sameAdoptionFile(before, after os.FileInfo) bool {
	return after.Mode().IsRegular() && os.SameFile(before, after) &&
		before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}
