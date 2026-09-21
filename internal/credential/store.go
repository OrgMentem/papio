// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	serviceName      = "com.orgmentem.papio.credentials"
	operationTimeout = 5 * time.Second
)

// Reader loads a record by its explicit reference. Store accepts only keyring
// references; Resolve handles environment references without calling Reader.
type Reader interface {
	Load(context.Context, string) (Record, error)
}

type backend struct {
	load   func(service, account string) (string, error)
	save   func(service, account, value string) error
	delete func(service, account string) error
}

// The library's API cannot cancel an OS call. Retain this process-wide gate
// until the call actually completes, even after its caller stops waiting.
// The legacy agentcredential package keeps its own compatibility-store gate.
var systemBusy atomic.Bool

// Store bounds OS waiting without queuing concurrent operations. Construct it
// with NewStore. Separate default Store instances share the in-flight gate.
type Store struct {
	service string
	backend backend
	busy    *atomic.Bool
	timeout time.Duration
}

// NewStore selects the current user's macOS Keychain, Windows Credential
// Manager or Linux Secret Service. Construction performs no OS store access.
func NewStore() *Store {
	return &Store{
		service: serviceName,
		backend: backend{load: keyring.Get, save: keyring.Set, delete: keyring.Delete},
		busy:    &systemBusy,
		timeout: operationTimeout,
	}
}

// Load returns a validated record. An unavailable, busy or invalid store entry
// must not be treated as absence or cause selection of a different credential.
func (s *Store) Load(ctx context.Context, ref string) (Record, error) {
	encoded, err := s.run(ctx, ref, false, func() (string, error) {
		if s.backend.load == nil {
			return "", ErrUnavailable
		}
		value, err := s.backend.load(s.service, ref)
		if err != nil {
			return "", sanitize(err)
		}
		return value, nil
	})
	if err != nil {
		return Record{}, err
	}
	return Decode(encoded)
}

// Save replaces exactly the referenced record. Encoding and size validation
// precede OS access. Cancellation after dispatch reports ErrUncertain.
func (s *Store) Save(ctx context.Context, ref string, record Record) error {
	encoded, err := Encode(record)
	if err != nil {
		return err
	}
	_, err = s.run(ctx, ref, true, func() (string, error) {
		if s.backend.save == nil {
			return "", ErrUnavailable
		}
		return "", sanitize(s.backend.save(s.service, ref, encoded))
	})
	return err
}

// Delete removes exactly the referenced record; a missing record is ErrNotFound.
// A dispatched deletion may still finish after a caller receives ErrUncertain.
func (s *Store) Delete(ctx context.Context, ref string) error {
	_, err := s.run(ctx, ref, true, func() (string, error) {
		if s.backend.delete == nil {
			return "", ErrUnavailable
		}
		return "", sanitize(s.backend.delete(s.service, ref))
	})
	return err
}

type result struct {
	value string
	err   error
}

func (s *Store) run(ctx context.Context, ref string, mutation bool, call func() (string, error)) (string, error) {
	if !keyringReference(ref) {
		return "", ErrInvalidReference
	}
	if s == nil || s.busy == nil || s.timeout <= 0 || s.service == "" {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !s.busy.CompareAndSwap(false, true) {
		return "", ErrBusy
	}
	if err := ctx.Err(); err != nil {
		s.busy.Store(false)
		return "", err
	}
	done := make(chan result, 1)
	go func() {
		value, err := call()
		s.busy.Store(false)
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		if mutation {
			return "", errors.Join(ErrUncertain, ctx.Err())
		}
		return "", ctx.Err()
	}
}

func sanitize(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	return ErrUnavailable
}
