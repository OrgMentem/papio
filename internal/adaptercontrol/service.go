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
// corrupt copy is repaired.
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
	if err := writeFileAtomic(s.documentPath(), verified.Envelope); err != nil {
		return Applied{Verified: verified}, fmt.Errorf("persisting adapter control: %w", err)
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

// writeFileAtomic replaces path with data, mode 0600, through a synced
// temporary file in the same directory.
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
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
