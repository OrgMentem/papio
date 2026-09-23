// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package agentcredential

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

const testProfile = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// sameSentinel reports whether err is target with nothing added: errors.Is
// matches and the message is target's own, so no wrapper carries private
// content out.
func sameSentinel(err, target error) bool {
	return errors.Is(err, target) && err.Error() == target.Error()
}

func testStore(b backend) *Store {
	return &Store{service: serviceName, backend: b, busy: new(atomic.Bool), timeout: operationTimeout}
}

func TestValidateKey(t *testing.T) {
	for _, key := range []string{"", " ", " leading", "trailing ", "embedded space", "line\nbreak", "\x00", "\t", "\r", "\x1f", "\x7f", "é", "\xff", strings.Repeat("a", 1025)} {
		if err := ValidateKey(key); !sameSentinel(err, ErrInvalidKey) {
			t.Fatal("invalid key was accepted or exposed in an error")
		}
	}
	for _, key := range []string{"synthetic-fixture", "a!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", strings.Repeat("a", 1024)} {
		if err := ValidateKey(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreRoundTripAndProfileIsolation(t *testing.T) {
	values := map[string]string{}
	var mu sync.Mutex
	b := backend{
		load: func(service, account string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if service != serviceName {
				return "", errors.New("wrong service")
			}
			v, ok := values[account]
			if !ok {
				return "", keyring.ErrNotFound
			}
			return v, nil
		},
		save: func(service, account, key string) error {
			mu.Lock()
			defer mu.Unlock()
			if service != serviceName {
				return errors.New("wrong service")
			}
			values[account] = key
			return nil
		},
		delete: func(service, account string) error {
			mu.Lock()
			defer mu.Unlock()
			if service != serviceName {
				return errors.New("wrong service")
			}
			if _, ok := values[account]; !ok {
				return keyring.ErrNotFound
			}
			delete(values, account)
			return nil
		},
	}
	s := testStore(b)
	ctx := context.Background()
	other := strings.Repeat("b", 64)
	if key, err := s.Load(ctx, testProfile); key != "" || !sameSentinel(err, ErrNotFound) {
		t.Fatalf("missing load: empty=%v err=%v", key == "", err)
	}
	for _, profile := range []string{testProfile, other} {
		if err := s.Save(ctx, profile, "synthetic-initial"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(ctx, testProfile, "synthetic-replacement"); err != nil {
		t.Fatal(err)
	}
	if key, err := s.Load(ctx, testProfile); key != "synthetic-replacement" || err != nil {
		t.Fatalf("replacement not loaded: %v", err)
	}
	if err := s.Delete(ctx, testProfile); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, testProfile); !sameSentinel(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if key, err := s.Load(ctx, other); key != "synthetic-initial" || err != nil {
		t.Fatalf("other profile changed: %v", err)
	}
}

func TestStoreSanitizesBackendErrorsAndStoredKeys(t *testing.T) {
	private := "synthetic-private-value/private/account/VendorDiagnostic"
	for _, cause := range []error{errors.New(private), fmt.Errorf("%s: %w", private, keyring.ErrNotFound)} {
		want := ErrUnavailable
		if errors.Is(cause, keyring.ErrNotFound) {
			want = ErrNotFound
		}
		s := testStore(backend{
			load:   func(string, string) (string, error) { return private, cause },
			save:   func(string, string, string) error { return cause },
			delete: func(string, string) error { return cause },
		})
		key, err := s.Load(context.Background(), testProfile)
		if key != "" || !sameSentinel(err, want) {
			t.Fatal("load exposed backend data or misclassified error")
		}
		for _, err := range []error{s.Save(context.Background(), testProfile, private), s.Delete(context.Background(), testProfile)} {
			if !sameSentinel(err, want) || errors.Is(err, cause) {
				t.Fatal("mutation exposed backend error or misclassified it")
			}
		}
	}
	for _, invalid := range []string{"", " ", "synthetic invalid", "synthetic\ninvalid", strings.Repeat("a", 1025)} {
		s := testStore(backend{load: func(string, string) (string, error) { return invalid, nil }})
		if key, err := s.Load(context.Background(), testProfile); key != "" || !sameSentinel(err, ErrInvalidKey) {
			t.Fatal("invalid stored value escaped")
		}
	}
}

func TestStoreRejectsBeforeDispatch(t *testing.T) {
	// Nil functions panic if any invalid/pre-canceled request reaches the backend.
	s := testStore(backend{})
	ctx := context.Background()
	for _, profile := range []string{"", "../private/path", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, err := s.Load(ctx, profile); !sameSentinel(err, ErrInvalidProfile) {
			t.Fatal("invalid profile accepted")
		}
		if err := s.Save(ctx, profile, "synthetic-valid"); !sameSentinel(err, ErrInvalidProfile) {
			t.Fatal("invalid profile accepted")
		}
		if err := s.Delete(ctx, profile); !sameSentinel(err, ErrInvalidProfile) {
			t.Fatal("invalid profile accepted")
		}
	}
	if err := s.Save(ctx, testProfile, ""); !sameSentinel(err, ErrInvalidKey) {
		t.Fatal("invalid key accepted")
	}
	canceled, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("synthetic-private-cancellation-cause"))
	_, err := s.Load(canceled, testProfile)
	for _, err := range []error{err, s.Save(canceled, testProfile, "synthetic-valid"), s.Delete(canceled, testProfile)} {
		if !sameSentinel(err, context.Canceled) || errors.Is(err, ErrUncertain) {
			t.Fatal("pre-dispatch cancellation should be certain and sanitized")
		}
	}
	if s.busy.Load() {
		t.Fatal("invalid request retained the gate")
	}
}

func TestStoreTimedOutMutationRetainsGateUntilCompletion(t *testing.T) {
	for _, operation := range []string{"save", "delete"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			var changed atomic.Bool
			fn := func() error {
				calls.Add(1)
				close(entered)
				<-release
				changed.Store(true)
				return nil
			}
			s := testStore(backend{
				save:   func(string, string, string) error { return fn() },
				delete: func(string, string) error { return fn() },
				load:   func(string, string) (string, error) { return "synthetic-late-value", nil },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			finished := make(chan error, 1)
			go func() {
				if operation == "save" {
					finished <- s.Save(ctx, testProfile, "synthetic-value")
				} else {
					finished <- s.Delete(ctx, testProfile)
				}
			}()
			<-entered
			cancel()
			err := <-finished
			if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.Canceled) {
				t.Fatalf("dispatched mutation must be uncertain: %v", err)
			}
			// Even a new Store sharing the default-style gate cannot spawn another
			// worker while the original operation may still mutate the OS store.
			sibling := *s
			var wg sync.WaitGroup
			for range 100 {
				wg.Go(func() {
					if err := sibling.Save(context.Background(), testProfile, "synthetic-retry"); !sameSentinel(err, ErrBusy) {
						t.Errorf("overlapping save: %v", err)
					}
					if err := sibling.Delete(context.Background(), testProfile); !sameSentinel(err, ErrBusy) {
						t.Errorf("overlapping delete: %v", err)
					}
					if key, err := sibling.Load(context.Background(), testProfile); key != "" || !sameSentinel(err, ErrBusy) {
						t.Errorf("overlapping load: %v", err)
					}
				})
			}
			wg.Wait()
			if calls.Load() != 1 || changed.Load() {
				t.Fatal("retry dispatched, or timed-out mutation falsely settled")
			}
			unblock()
			deadline := time.Now().Add(time.Second)
			for s.busy.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s.busy.Load() || !changed.Load() {
				t.Fatal("completed operation did not release gate")
			}
			if key, err := s.Load(context.Background(), testProfile); key != "synthetic-late-value" || err != nil {
				t.Fatalf("settled gate not reusable: %v", err)
			}
		})
	}
}

func TestStoreDefaultDeadline(t *testing.T) {
	release := make(chan struct{})
	s := testStore(backend{save: func(string, string, string) error { <-release; return nil }})
	defer close(release)
	// The outer watchdog is longer than the internal deadline; caller cancellation
	// alone cannot make this oracle pass.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	err := s.Save(ctx, testProfile, "synthetic-value")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.DeadlineExceeded) || elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("default deadline: elapsed=%v err=%v", elapsed, err)
	}
	if !s.busy.Load() {
		t.Fatal("deadline released the in-flight gate")
	}
}

func TestStoreLoadTimeoutIsNotAbsenceOrMutation(t *testing.T) {
	release := make(chan struct{})
	s := testStore(backend{load: func(string, string) (string, error) { <-release; return "synthetic-late-key", nil }})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	key, err := s.Load(ctx, testProfile)
	if key != "" || !sameSentinel(err, context.DeadlineExceeded) || errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotFound) {
		t.Fatalf("load deadline misclassified: %v", err)
	}
	if !s.busy.Load() {
		t.Fatal("timed-out load must still exclude new workers")
	}
}

func TestNewStoresShareOneGateWithoutOSAccess(t *testing.T) {
	a, b := NewStore(), NewStore()
	if a.busy != b.busy || a.busy != &systemBusy || a.timeout != 5*time.Second || a.service != serviceName {
		t.Fatal("default stores do not share a bounded operation gate")
	}
	// Inject a fake before any operation, even when testing constructor wiring.
	b.backend.load = func(string, string) (string, error) {
		return "", errors.New("gate bypassed")
	}
	if !a.busy.CompareAndSwap(false, true) {
		t.Fatal("unexpected real store operation")
	}
	defer a.busy.Store(false)
	if _, err := b.Load(context.Background(), testProfile); !sameSentinel(err, ErrBusy) {
		t.Fatalf("new store bypassed gate: %v", err)
	}
}
