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
// out. In process, a call that never returns holds it until the process exits.
// Through a helper process (UseHelperProcess) the deadline kills the worker, the
// call returns, and the gate is released. The gate is process-local; it cannot
// serialize separate Papio processes.
var systemBusy atomic.Bool

// Store bounds waiting for OS calls. Construct it with NewStore. Methods are safe
// for concurrent use; an overlapping operation returns ErrBusy without queuing.
type Store struct {
	service string
	backend backend
	// helper, when set, is the executable that runs each OS call in a process
	// the deadline can kill. Empty means backend is called in this process.
	helper  string
	busy    *atomic.Bool
	timeout time.Duration
}

// NewStore uses macOS Keychain, Windows Credential Manager, or Linux Secret
// Service through go-keyring. Construction does not access the OS store. After
// UseHelperProcess, each call runs in a helper process that a timeout kills.
func NewStore() *Store {
	s := &Store{
		service: serviceName,
		backend: backend{load: keyring.Get, save: keyring.Set, delete: keyring.Delete},
		busy:    &systemBusy,
		timeout: operationTimeout,
	}
	if binary := helperBinary.Load(); binary != nil {
		s.helper = *binary
	}
	return s
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
	return s.run(ctx, profile, false, func(ctx context.Context) (string, error) {
		key, err := s.dispatch(ctx, opLoad, profile, "")
		if err != nil {
			return "", err
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
	_, err := s.run(ctx, profile, true, func(ctx context.Context) (string, error) {
		return s.dispatch(ctx, opSave, profile, key)
	})
	return err
}

// Delete removes only this profile's key. A missing key returns ErrNotFound.
// A timeout or cancellation after dispatch includes ErrUncertain.
func (s *Store) Delete(ctx context.Context, profile string) error {
	_, err := s.run(ctx, profile, true, func(ctx context.Context) (string, error) {
		return s.dispatch(ctx, opDelete, profile, "")
	})
	return err
}

// dispatch performs one OS call and returns only sentinel errors. In-process,
// the call ignores ctx and cannot be cancelled; through a helper, ctx kills it.
func (s *Store) dispatch(ctx context.Context, op, profile, key string) (string, error) {
	if s.helper != "" {
		value, err := runHelperProcess(ctx, s.helper, helperRequest{Op: op, Service: s.service, Account: profile, Key: key})
		// Isolation hardens the call; it is not a dependency. A helper that
		// cannot start has dispatched nothing, so the call may run in process.
		if !errors.Is(err, errHelperNotStarted) {
			return value, err
		}
	}
	switch op {
	case opLoad:
		value, err := s.backend.load(s.service, profile)
		return value, sanitize(err)
	case opSave:
		return "", sanitize(s.backend.save(s.service, profile, key))
	default:
		return "", sanitize(s.backend.delete(s.service, profile))
	}
}

type result struct {
	key string
	err error
}

func (s *Store) run(ctx context.Context, profile string, mutation bool, call func(context.Context) (string, error)) (string, error) {
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
		key, err := call(ctx)
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
