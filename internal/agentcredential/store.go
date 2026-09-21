// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package agentcredential stores acquisition credentials in the current user's
// OS credential store. It never falls back to a plaintext file.
package agentcredential

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	serviceName      = "com.orgmentem.papio.acquisition"
	operationTimeout = 5 * time.Second
)

var (
	ErrNotFound       = errors.New("agent credential not found")
	ErrInvalidKey     = errors.New("agent credential must contain 1 to 1024 printable non-space ASCII bytes")
	ErrInvalidProfile = errors.New("agent credential profile is invalid")
	ErrUnavailable    = errors.New("OS credential store is unavailable")
	ErrBusy           = errors.New("OS credential store operation is still in progress")
	// ErrUncertain means a dispatched mutation has not returned. It may still
	// complete; callers must not claim that the credential was left unchanged.
	ErrUncertain = errors.New("OS credential store operation outcome is uncertain")
)

type backend struct {
	load   func(service, account string) (string, error)
	save   func(service, account, key string) error
	delete func(service, account string) error
}

// The library has no context-aware API. All default stores share this gate so a
// blocked OS call leaves at most one worker, even when callers create new stores.
// Keep it held until that call actually returns, not just until our caller times
// out. This is process-local; it cannot serialize separate Papio processes.
var systemBusy atomic.Bool

// Store bounds waiting for OS calls. Construct it with NewStore. Methods are safe
// for concurrent use; an overlapping operation returns ErrBusy without queuing.
type Store struct {
	service string
	backend backend
	busy    *atomic.Bool
	timeout time.Duration
}

// NewStore uses macOS Keychain, Windows Credential Manager, or Linux Secret
// Service through go-keyring. Construction does not access the OS store.
func NewStore() *Store {
	return &Store{
		service: serviceName,
		backend: backend{load: keyring.Get, save: keyring.Set, delete: keyring.Delete},
		busy:    &systemBusy,
		timeout: operationTimeout,
	}
}

// ValidateKey accepts portable printable non-space ASCII, without trimming or
// changing the key. The bound fits all supported stores, including Windows.
func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > 1024 {
		return ErrInvalidKey
	}
	for i := range len(key) {
		if key[i] < 0x21 || key[i] > 0x7e {
			return ErrInvalidKey
		}
	}
	return nil
}

// Load returns only a valid key. ErrNotFound means absence; cancellation, busy,
// invalid stored content, and unavailable stores must not be treated as absence.
func (s *Store) Load(ctx context.Context, profile string) (string, error) {
	return s.run(ctx, profile, false, func() (string, error) {
		key, err := s.backend.load(s.service, profile)
		if err != nil {
			return "", sanitize(err)
		}
		if err := ValidateKey(key); err != nil {
			return "", err
		}
		return key, nil
	})
}

// Save replaces this profile's key. A timeout or cancellation after dispatch
// includes ErrUncertain and the stable context error, never the vendor's error.
func (s *Store) Save(ctx context.Context, profile, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	_, err := s.run(ctx, profile, true, func() (string, error) {
		return "", sanitize(s.backend.save(s.service, profile, key))
	})
	return err
}

// Delete removes only this profile's key. A missing key returns ErrNotFound.
// A timeout or cancellation after dispatch includes ErrUncertain.
func (s *Store) Delete(ctx context.Context, profile string) error {
	_, err := s.run(ctx, profile, true, func() (string, error) {
		return "", sanitize(s.backend.delete(s.service, profile))
	})
	return err
}

type result struct {
	key string
	err error
}

func (s *Store) run(ctx context.Context, profile string, mutation bool, call func() (string, error)) (string, error) {
	if !validProfile(profile) {
		return "", ErrInvalidProfile
	}
	if s == nil || s.busy == nil {
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
	// Cancellation while acquiring the gate must not dispatch a mutation.
	if err := ctx.Err(); err != nil {
		s.busy.Store(false)
		return "", err
	}
	done := make(chan result, 1)
	go func() {
		key, err := call()
		s.busy.Store(false)
		done <- result{key, err}
	}()
	select {
	case r := <-done:
		return r.key, r.err
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
