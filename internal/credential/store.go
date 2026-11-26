// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/zalando/go-keyring"

	"papio/internal/agentcredential"
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
// Through the helper process the deadline kills the call, which frees the gate.
// The legacy agentcredential package keeps its own compatibility-store gate.
var systemBusy atomic.Bool

// Store bounds OS waiting without queuing concurrent operations. Construct it
// with NewStore. Separate default Store instances share the in-flight gate.
type Store struct {
	service string
	backend backend
	// helper, when set, is the executable that runs each OS call in a process
	// the deadline can kill. Empty means backend is called in this process; only
	// tests and a process that never called agentcredential.UseHelperProcess do so.
	helper  string
	busy    *atomic.Bool
	timeout time.Duration
}

// HelperNamespace describes this package's OS credential service for the shared
// helper process. Pass it to agentcredential.RunHelper in helper mode.
func HelperNamespace() agentcredential.Namespace {
	return agentcredential.Namespace{
		Service:      serviceName,
		ValidAccount: keyringReference,
		ValidValue:   func(v string) bool { _, err := Decode(v); return err == nil },
	}
}

// NewStore selects the current user's macOS Keychain, Windows Credential
// Manager or Linux Secret Service. Construction performs no OS store access.
// After agentcredential.UseHelperProcess, each call runs in the shared helper
// process that a timeout kills; a helper that cannot start yields ErrUnavailable.
// With no helper configured the store has no backend and every call returns
// ErrUnavailable: production never calls the OS in process.
func NewStore() *Store {
	s := &Store{
		service: serviceName,
		busy:    &systemBusy,
		timeout: operationTimeout,
	}
	if binary, ok := agentcredential.HelperBinary(); ok {
		s.helper = binary
	}
	return s
}

// dispatch runs one OS call. Through a helper, ctx kills it; in process it
// cannot be cancelled. A helper that cannot start dispatched nothing and never
// falls back to an in-process call.
func (s *Store) dispatch(ctx context.Context, op, ref, value string) (string, error) {
	if s.helper != "" {
		out, err := agentcredential.CallHelper(ctx, s.helper, s.service, op, ref, value)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, agentcredential.ErrNotFound):
			return "", ErrNotFound
		case errors.Is(err, agentcredential.ErrHelperNotStarted):
			if cerr := ctx.Err(); cerr != nil {
				return "", cerr
			}
			return "", ErrUnavailable
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
			// The helper started, so a mutation may have reached the OS.
			if op != agentcredential.OpLoad {
				return "", errors.Join(ErrUncertain, err)
			}
			return "", err
		default:
			return "", ErrUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch op {
	case agentcredential.OpLoad:
		if s.backend.load == nil {
			return "", ErrUnavailable
		}
		v, err := s.backend.load(s.service, ref)
		return v, sanitize(err)
	case agentcredential.OpSave:
		if s.backend.save == nil {
			return "", ErrUnavailable
		}
		return "", sanitize(s.backend.save(s.service, ref, value))
	default:
		if s.backend.delete == nil {
			return "", ErrUnavailable
		}
		return "", sanitize(s.backend.delete(s.service, ref))
	}
}

// Load returns a validated record. An unavailable, busy or invalid store entry
// must not be treated as absence or cause selection of a different credential.
func (s *Store) Load(ctx context.Context, ref string) (Record, error) {
	encoded, err := s.run(ctx, ref, false, func(ctx context.Context) (string, error) {
		return s.dispatch(ctx, agentcredential.OpLoad, ref, "")
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
	_, err = s.run(ctx, ref, true, func(ctx context.Context) (string, error) {
		return s.dispatch(ctx, agentcredential.OpSave, ref, encoded)
	})
	return err
}

// Delete removes exactly the referenced record; a missing record is ErrNotFound.
// A dispatched deletion may still finish after a caller receives ErrUncertain.
func (s *Store) Delete(ctx context.Context, ref string) error {
	_, err := s.run(ctx, ref, true, func(ctx context.Context) (string, error) {
		return s.dispatch(ctx, agentcredential.OpDelete, ref, "")
	})
	return err
}

type result struct {
	value string
	err   error
}

func (s *Store) run(ctx context.Context, ref string, mutation bool, call func(context.Context) (string, error)) (string, error) {
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
		value, err := call(ctx)
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
