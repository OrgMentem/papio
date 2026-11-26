// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package adaptercontrol

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"papio/internal/config"
	"papio/internal/protocol"
)

const (
	// DocumentFileName is the persisted copy of the last verified envelope in
	// the data directory. It is re-verified on every load, so editing it makes
	// the state unavailable rather than forging a suspension.
	DocumentFileName = "adapter-control.json"
	// StatusFileName records the last fetch attempt.
	StatusFileName = "adapter-control-status.json"
)

// State names what the daemon can say about control.
type State string

const (
	// StateNone: nothing has ever been published or accepted here.
	StateNone State = State(protocol.AdapterControlStateNone)
	// StateCurrent: a verified document is held.
	StateCurrent State = State(protocol.AdapterControlStateCurrent)
	// StateUnavailable: control exists on disk but cannot be trusted. Browser
	// sessions fail closed on it.
	StateUnavailable State = State(protocol.AdapterControlStateUnavailable)
)

var (
	// ErrOlderSequence marks a replayed or rolled-back document.
	ErrOlderSequence = errors.New("adapter control document has an older sequence than the one held")
	// ErrSequenceConflict marks a different document at the sequence already held.
	ErrSequenceConflict = errors.New("adapter control document conflicts with the one already held at its sequence")
)

// Snapshot is the canonical verified control. Its slices are never mutated.
type Snapshot struct {
	State     State
	Sequence  int64
	Digest    string
	Revisions []string
	Domains   []string
	KeyID     string
	IssuedAt  time.Time
	// Reason explains StateUnavailable.
	Reason string
}

// RevisionSuspended reports whether an exact revision ID is suspended.
func (s Snapshot) RevisionSuspended(revision string) bool {
	for _, candidate := range s.Revisions {
		if candidate == revision {
			return true
		}
	}
	return false
}

// DomainSuspended reports whether an exact safety-domain ID is suspended.
func (s Snapshot) DomainSuspended(domain string) bool {
	for _, candidate := range s.Domains {
		if candidate == domain {
			return true
		}
	}
	return false
}

// Mandatory reports whether browser sessions must prove they applied this
// control before any automatic effect: once the daemon holds or fails to hold
// restrictive state, a client that has not applied it cannot be trusted.
func (s Snapshot) Mandatory() bool {
	return s.State == StateCurrent || s.State == StateUnavailable
}

// Service holds the canonical control state and persists it.
type Service struct {
	dir string
	key ed25519.PublicKey

	mu       sync.RWMutex
	snapshot Snapshot
	raw      []byte
	fetchURL string
}

// Open loads persisted control from dataDir. It never fails: a missing file is
// StateNone, and an unverifiable file is StateUnavailable with its reason, so
// the daemon keeps running and every browser session fails closed. key is nil
// for a build that embeds no control key.
func Open(dataDir string, key ed25519.PublicKey) *Service {
	service := &Service{dir: dataDir, key: key, snapshot: Snapshot{
		State: StateNone, Digest: protocol.SuspensionsDigest(nil, nil),
		Revisions: []string{}, Domains: []string{},
	}}
	raw, err := os.ReadFile(service.documentPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return service
	case err != nil:
		service.snapshot.State, service.snapshot.Reason = StateUnavailable, "reading "+DocumentFileName+": "+err.Error()
		return service
	}
	verified, err := Verify(raw, key)
	if err != nil {
		service.snapshot.State, service.snapshot.Reason = StateUnavailable, DocumentFileName+" does not verify: "+err.Error()
		return service
	}
	service.snapshot, service.raw = snapshotOf(verified), verified.Envelope
	return service
}

func snapshotOf(verified *Verified) Snapshot {
	return Snapshot{
		State: StateCurrent, Sequence: verified.Document.Sequence, Digest: verified.Digest,
		Revisions: verified.Document.SuspendedRevisions, Domains: verified.Document.SuspendedSafetyDomains,
		KeyID: verified.Document.KeyID, IssuedAt: verified.IssuedAt,
	}
}

// Snapshot returns the canonical control.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// HasKey reports whether this build trusts a control key.
func (s *Service) HasKey() bool { return len(s.key) == ed25519.PublicKeySize }

// Applied describes one successful Apply.
type Applied struct {
	// Changed is false when the same document was already held.
	Changed  bool
	Verified *Verified
}

// Apply verifies a published envelope and, when it advances the sequence,
// persists it before it becomes canonical. A lower sequence is a replay, and a
// different document at the held sequence is a conflict; both are refused. A
// service in StateUnavailable accepts any verified document, which is how a
// corrupt copy is repaired. A document that cannot be made durable, including
// one whose rename could not be flushed to its directory, is not published:
// the held control stays as it was, the file on disk goes back to the copy it
// replaced so that a restart cannot load the refused document, and the error
// says why.
func (s *Service) Apply(envelope []byte) (Applied, error) {
	verified, err := Verify(envelope, s.key)
	if err != nil {
		return Applied{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.State == StateCurrent {
		switch held := s.snapshot.Sequence; {
		case verified.Document.Sequence < held:
			return Applied{Verified: verified}, fmt.Errorf("%w: sequence %d, holding %d", ErrOlderSequence, verified.Document.Sequence, held)
		case verified.Document.Sequence == held:
			if bytes.Equal(verified.Payload, s.heldPayload()) {
				return Applied{Verified: verified}, nil
			}
			return Applied{Verified: verified}, fmt.Errorf("%w: sequence %d", ErrSequenceConflict, held)
		}
	}
	path := s.documentPath()
	// The copy being replaced, kept so a replacement that cannot be made
	// durable can be undone. If it exists but cannot be read, nothing could
	// restore it, so the replacement does not start: the file stays as it is.
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return Applied{Verified: verified}, fmt.Errorf("persisting adapter control: the control file being replaced cannot be read, so it could not be restored: %w", readErr)
	}
	hadPrevious := readErr == nil
	if err := writeFileAtomic(path, verified.Envelope); err != nil {
		err = fmt.Errorf("persisting adapter control: %w", err)
		if errors.Is(err, errNotFlushed) {
			if undoErr := undoReplacement(path, previous, hadPrevious); undoErr != nil {
				err = fmt.Errorf("%w; restoring the control held before: %w", err, undoErr)
			}
		}
		return Applied{Verified: verified}, err
	}
	s.snapshot, s.raw = snapshotOf(verified), verified.Envelope
	return Applied{Changed: true, Verified: verified}, nil
}

func (s *Service) heldPayload() []byte {
	verified, err := Verify(s.raw, s.key)
	if err != nil {
		return nil
	}
	return verified.Payload
}

func (s *Service) documentPath() string { return filepath.Join(s.dir, DocumentFileName) }

// syncDir flushes a directory's entries to stable storage. It is a variable so
// a test can inject the failure; config.SyncDir is the repository's one
// implementation, a no-op on Windows where NTFS journals a rename.
var syncDir = config.SyncDir

// errNotFlushed marks a replacement whose new file is already at its path but
// whose directory entry could not be flushed, so a crash may keep either copy.
var errNotFlushed = errors.New("directory entry not flushed")

// writeFileAtomic replaces path with data, mode 0600, through a synced
// temporary file in the same directory, then flushes the directory so the
// rename survives power loss. A directory flush error is returned wrapping
// errNotFlushed: the new file is in place, but it is not known to be durable.
func writeFileAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := file.Name()
	cleanup := func() { _ = file.Close(); _ = os.Remove(name) }
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: syncing %s: %w", errNotFlushed, filepath.Dir(path), err)
	}
	return nil
}

// undoReplacement puts back what a replacement of path overwrote: previous,
// read before the replacement when hadPrevious, or no file at all when there
// was none. Without it the refused document would stay at the path the next
// Open trusts, and a flush that later succeeds would make a document the
// daemon never accepted the one a restart loads.
func undoReplacement(path string, previous []byte, hadPrevious bool) error {
	if hadPrevious {
		return writeFileAtomic(path, previous)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: syncing %s: %w", errNotFlushed, filepath.Dir(path), err)
	}
	return nil
}
