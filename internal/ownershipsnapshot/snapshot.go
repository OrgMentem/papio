// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package ownershipsnapshot loads a user's holdings from a bibliographic export
// and answers ownership lookups from it (ADR-0008, tier 1).
//
// Everything here exists to make one failure impossible: a source that papio
// cannot read must never be mistaken for a source that holds nothing. A bad
// read, a partial write, or a truncated export therefore retains the previous
// index and reports the source as incomplete, so callers keep the right to
// re-acquire rather than silently skipping work the user asked for.
package ownershipsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"papio/internal/bibparse"
	"papio/internal/config"
	"papio/internal/ownership"
)

// defaultMaxBytes caps one snapshot read. A bibliographic export of a large
// personal library is a few megabytes; anything past this is more likely a
// mistargeted path than a library, and reading it would stall every lookup.
const defaultMaxBytes = 32 << 20

// freshnessWindow bounds how long an older in-memory index remains useful as
// annotation after the source becomes uncertain. Its claims are stale on every
// failed refresh and therefore cannot suppress acquisition.
const freshnessWindow = 15 * time.Minute

// collapseFloor is the smallest previous entry count at which a sudden collapse
// is treated as corruption rather than an ordinary edit. Below it, a large
// proportional drop is unremarkable: deleting two of three entries is plausible.
const collapseFloor = 20

// NewProvider builds the holdings provider for one configured source. Only
// kind = "file" exists in v1; a command loader and a PDF-folder scanner are
// planned (ADR-0008) and are rejected here rather than silently ignored.
func NewProvider(source config.LibrarySource, now func() time.Time) (ownership.Provider, error) {
	if now == nil {
		now = time.Now
	}
	name := strings.TrimSpace(source.Name)
	if name == "" {
		return nil, fmt.Errorf("library source name is required")
	}
	if source.Kind != config.LibraryKindFile {
		return nil, fmt.Errorf("library source %q: kind %q is not supported yet (only %q)", name, source.Kind, config.LibraryKindFile)
	}
	path := expandHome(strings.TrimSpace(source.Path))
	if path == "" {
		return nil, fmt.Errorf("library source %q: path is required", name)
	}
	artifact, err := artifactStateForClaim(source.Claim)
	if err != nil {
		return nil, fmt.Errorf("library source %q: %w", name, err)
	}
	empty := &snapshot{index: ownership.BuildIndex(nil)}
	return &fileProvider{
		name:     name,
		path:     path,
		format:   bibparse.Format(source.Format),
		artifact: artifact,
		maxBytes: defaultMaxBytes,
		now:      now,
		current:  empty,
		refresh:  make(chan struct{}, 1),
		read:     readBounded,
		identity: fileIdentity,
	}, nil
}

// expandHome mirrors config's normalization so callers that construct a
// LibrarySource directly receive the same file-source behavior as Config.Load.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// artifactStateForClaim translates the source's declaration into what its
// entries assert. papio never infers this from per-manager attachment fields:
// that is convention knowledge the abstraction must not carry, and a wrong guess
// suppresses an acquisition the source never vouched for.
func artifactStateForClaim(claim string) (string, error) {
	switch claim {
	case config.LibraryClaimPDFPresent:
		return ownership.ArtifactPresent, nil
	case config.LibraryClaimRecordPresent:
		return ownership.ArtifactUnknown, nil
	default:
		return "", fmt.Errorf("claim must be %q or %q", config.LibraryClaimPDFPresent, config.LibraryClaimRecordPresent)
	}
}

// revision fingerprints the opened file well enough to detect replacement and
// in-place edits. Filesystems without identity/change metadata deliberately do
// not reuse a revision; rereading is safer than retaining a false positive.
type revision struct {
	size    int64
	modTime time.Time
	fileID  fileID
}

func (r revision) equal(other revision) bool {
	return r.size == other.size &&
		r.modTime.Equal(other.modTime) &&
		r.fileID.known &&
		other.fileID.known &&
		r.fileID.equal(other.fileID)
}

type fileID struct {
	device  uint64
	inode   uint64
	changeA uint64
	changeB uint64
	known   bool
}

func (id fileID) equal(other fileID) bool {
	return id.device == other.device &&
		id.inode == other.inode &&
		id.changeA == other.changeA &&
		id.changeB == other.changeB
}

// stableSince is stricter when the platform exposes file change metadata. The
// fallback can only establish size and mtime, so it allows this refresh but
// revision.equal will force a bounded reread on every later lookup.
func (r revision) stableSince(other revision) bool {
	if r.size != other.size || !r.modTime.Equal(other.modTime) {
		return false
	}
	if r.fileID.known != other.fileID.known {
		return false
	}
	return !r.fileID.known || r.fileID.equal(other.fileID)
}

// snapshot is one successfully loaded index. It is immutable once published, so
// concurrent readers can never observe a half-built index.
type snapshot struct {
	index       *ownership.Index
	revision    revision
	loadedAt    time.Time
	entryCount  int
	failureCode string
}

type fileProvider struct {
	name     string
	path     string
	format   bibparse.Format
	artifact string
	maxBytes int64
	now      func() time.Time

	// refresh is a cancellable single-flight gate. The mutex only protects the
	// published immutable snapshot and diagnostics; no caller holds it while
	// doing filesystem I/O or parsing.
	refresh chan struct{}
	mu      sync.RWMutex
	current *snapshot
	reads   int
	read    func(context.Context, io.Reader) ([]byte, error)
	// identity is per-provider so tests can exercise the metadata-less fallback
	// on every platform without changing global filesystem behavior.
	identity func(*os.File, os.FileInfo) fileID
}

func (p *fileProvider) Name() string { return p.name }

// Lookup refreshes the index when the file has changed, then answers from it.
// A failed refresh preserves the last immutable index as annotation, but marks
// its claims stale so uncertainty never suppresses an acquisition.
func (p *fileProvider) Lookup(ctx context.Context, queries []ownership.Query) ([][]ownership.Claim, ownership.SourceHealth) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return p.answer(p.currentSnapshot(), false, ownership.FailureTimeout, queries)
	}

	select {
	case p.refresh <- struct{}{}:
		defer func() { <-p.refresh }()
	case <-ctx.Done():
		return p.answer(p.currentSnapshot(), false, ownership.FailureTimeout, queries)
	}
	if ctx.Err() != nil {
		return p.answer(p.currentSnapshot(), false, ownership.FailureTimeout, queries)
	}

	snap, complete, failure := p.refreshSnapshot(ctx)
	return p.answer(snap, complete, failure, queries)
}

func (p *fileProvider) answer(snap *snapshot, complete bool, failure string, queries []ownership.Query) ([][]ownership.Claim, ownership.SourceHealth) {
	if snap == nil {
		snap = &snapshot{index: ownership.BuildIndex(nil)}
	}

	// A failed read is not current positive evidence. Preserve it for display,
	// but never let it decide that requested work can be skipped.
	stale := !complete
	health := ownership.SourceHealth{
		Name:        p.name,
		Complete:    complete,
		Stale:       stale,
		EntryCount:  snap.entryCount,
		LastSuccess: snap.loadedAt,
		FailureCode: failure,
	}

	claims := make([][]ownership.Claim, len(queries))
	for i, query := range queries {
		claims[i] = snap.index.Claims(p.name, query)
		if stale {
			for j := range claims[i] {
				claims[i][j].Stale = true
			}
		}
	}
	return claims, health
}

func (p *fileProvider) currentSnapshot() *snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current
}

// refreshSnapshot returns the snapshot to answer from, whether this lookup
// counted as a successful read, and a bounded failure code. It never returns
// nil.
func (p *fileProvider) refreshSnapshot(ctx context.Context) (*snapshot, bool, string) {
	previous := p.currentSnapshot()
	if err := ctx.Err(); err != nil {
		return previous, false, ownership.FailureTimeout
	}

	file, err := openSource(p.path)
	if err != nil {
		return previous, false, ownership.FailureUnreadable
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return previous, false, ownership.FailureUnreadable
	}
	before := p.revisionFor(file, info)
	if previous != nil && before.equal(previous.revision) {
		currentPath, err := p.pathRevision()
		if err != nil {
			return previous, false, ownership.FailureUnreadable
		}
		// A cache hit is fresh only when the current pathname exposes the same
		// complete identity. A transient metadata failure must not suppress work.
		if !currentPath.fileID.known || !before.fileID.equal(currentPath.fileID) {
			return previous, false, ownership.FailureUnreadable
		}
		return previous, true, ""
	}
	if before.size > p.maxBytes {
		// Refusing is the honest answer: a partial read would index a fraction of
		// the library and look like a successful, much smaller one.
		return previous, false, ownership.FailureTruncated
	}
	if err := ctx.Err(); err != nil {
		return previous, false, ownership.FailureTimeout
	}

	p.mu.Lock()
	p.reads++
	p.mu.Unlock()
	data, err := p.read(ctx, io.LimitReader(file, p.maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return previous, false, ownership.FailureTimeout
		}
		return previous, false, ownership.FailureUnreadable
	}

	// Re-stat this same descriptor, not the path. A writer changing the opened
	// file during the read yields bytes belonging to neither version.
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return previous, false, ownership.FailureUnreadable
	}
	after := p.revisionFor(file, info)
	if int64(len(data)) > p.maxBytes || after.size > p.maxBytes {
		return previous, false, ownership.FailureTruncated
	}
	if int64(len(data)) != after.size {
		return previous, false, ownership.FailureUnreadable
	}
	if !after.stableSince(before) {
		return previous, false, ownership.FailureUnreadable
	}

	if err := ctx.Err(); err != nil {
		return previous, false, ownership.FailureTimeout
	}

	format := p.format
	if strings.TrimSpace(string(format)) == "" {
		format = bibparse.Detect(p.path, data)
	}
	records, err := bibparse.ParseRecords(format, data)
	if err != nil && !errors.Is(err, bibparse.ErrNoEntries) {
		return previous, false, ownership.FailureParse
	}
	// A library with no entries is a real, successfully read state — someone who
	// has just started — and must stay distinguishable from a source papio could
	// not read. Acquisition treats the same input as an error; holdings does not.

	entries := make([]ownership.Entry, 0, len(records))
	for _, record := range records {
		identifiers := make([]ownership.Identifier, 0, 3)
		for kind, value := range map[string]string{
			ownership.KindDOI:   record.DOI,
			ownership.KindArXiv: record.ArXiv,
			ownership.KindPMID:  record.PMID,
		} {
			if strings.TrimSpace(value) != "" {
				identifiers = append(identifiers, ownership.Identifier{Kind: kind, Value: value})
			}
		}
		if len(identifiers) == 0 {
			// Counted as a source record, not an error: a real library holds
			// books, reports, and hand-typed notes with no matchable identifier.
			continue
		}
		entries = append(entries, ownership.Entry{
			Identifiers: identifiers,
			Artifact:    p.artifact,
			// A bibliographic export says nothing about which manifestation its
			// full text is, which is exactly why such a source can never satisfy
			// an explicit --desired-version published request (ownership.Decide).
			ArtifactVersion: ownership.VersionUnknown,
			EntityKind:      ownership.EntityUnknown,
		})
	}
	index := ownership.BuildIndex(entries)

	if p.collapsed(previous, len(records)) {
		// A truncated or half-written export parses cleanly and simply contains
		// less. Accepting it would quietly disable de-duplication.
		return previous, false, ownership.FailureCountCollapse
	}

	// Parsing is deliberately finished before the last pathname check: a
	// replacement while parsing must not make the old, now-unlinked bytes a
	// fresh snapshot either.
	if failure := p.pathStillMatches(ctx, after, data); failure != "" {
		return previous, false, failure
	}

	next := &snapshot{
		index:      index,
		revision:   after,
		loadedAt:   p.now(),
		entryCount: len(records),
	}
	p.mu.Lock()
	p.current = next
	p.mu.Unlock()
	return next, true, ""
}

// collapsed reports a catastrophic drop against the last good source record
// count: not an ordinary edit, but the shape of a truncated write.
func (p *fileProvider) collapsed(previous *snapshot, count int) bool {
	if previous == nil || previous.entryCount < collapseFloor {
		return false
	}
	return count*10 < previous.entryCount
}

func (p *fileProvider) revisionFor(file *os.File, info os.FileInfo) revision {
	id := fileID{}
	if p.identity != nil {
		id = p.identity(file, info)
	}
	return revision{
		size:    info.Size(),
		modTime: info.ModTime(),
		fileID:  id,
	}
}

func (p *fileProvider) pathRevision() (revision, error) {
	file, err := openSource(p.path)
	if err != nil {
		return revision{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return revision{}, err
	}
	if !info.Mode().IsRegular() {
		return revision{}, fmt.Errorf("source is not a regular file")
	}
	return p.revisionFor(file, info), nil
}

// pathStillMatches proves that the pathname describes the bytes read from the
// original descriptor. Files with no usable identity/change metadata require a
// bounded byte comparison rather than treating two unknown identities as equal.
func (p *fileProvider) pathStillMatches(ctx context.Context, expected revision, data []byte) string {
	if err := ctx.Err(); err != nil {
		return ownership.FailureTimeout
	}
	file, err := openSource(p.path)
	if err != nil {
		return ownership.FailureUnreadable
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ownership.FailureUnreadable
	}
	current := p.revisionFor(file, info)
	if current.size > p.maxBytes {
		return ownership.FailureTruncated
	}
	if expected.fileID.known && current.fileID.known {
		if expected.fileID.equal(current.fileID) {
			return ""
		}
		return ownership.FailureUnreadable
	}
	if current.size != int64(len(data)) {
		return ownership.FailureUnreadable
	}

	matches, err := fileBytesMatch(ctx, file, data)
	if err != nil {
		if ctx.Err() != nil {
			return ownership.FailureTimeout
		}
		return ownership.FailureUnreadable
	}
	if !matches {
		return ownership.FailureUnreadable
	}
	return ""
}

// fileBytesMatch streams one already bounded file against expected without
// materializing a second copy. It also checks for a concurrent growth after the
// stat that admitted the comparison.
func fileBytesMatch(ctx context.Context, file *os.File, expected []byte) (bool, error) {
	bufferSize := len(expected)
	if bufferSize > 32<<10 {
		bufferSize = 32 << 10
	}
	buf := make([]byte, bufferSize)
	for offset := 0; offset < len(expected); {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remaining := len(expected) - offset
		chunk := buf
		if remaining < len(chunk) {
			chunk = chunk[:remaining]
		}
		n, err := file.Read(chunk)
		if n > 0 {
			for i := range n {
				if chunk[i] != expected[offset+i] {
					return false, nil
				}
			}
			offset += n
		}
		if err == io.EOF {
			return offset == len(expected), nil
		}
		if err != nil {
			return false, err
		}
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}
	var extra [1]byte
	n, err := file.Read(extra[:])
	if n > 0 {
		return false, nil
	}
	if err == io.EOF {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, io.ErrNoProgress
}

func readBounded(ctx context.Context, reader io.Reader) ([]byte, error) {
	buf := make([]byte, 32<<10)
	var data []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
