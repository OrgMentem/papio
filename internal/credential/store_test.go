// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

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

const testReference = "keyring:0123456789abcdef0123456789abcdef"

// sameSentinel reports whether err is target with nothing added: errors.Is
// matches and the message is target's own, so no wrapper carries private
// content out.
func sameSentinel(err, target error) bool {
	return errors.Is(err, target) && err.Error() == target.Error()
}

func testStore(b backend) *Store {
	return &Store{service: serviceName, backend: b, busy: new(atomic.Bool), timeout: operationTimeout}
}

func memoryStore(t *testing.T) *Store {
	t.Helper()
	var mu sync.Mutex
	values := map[string]string{}
	return testStore(backend{
		load: func(service, account string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if service != serviceName || !keyringReference(account) {
				return "", errors.New("incorrect store address")
			}
			value, ok := values[account]
			if !ok {
				return "", keyring.ErrNotFound
			}
			return value, nil
		},
		save: func(service, account, value string) error {
			mu.Lock()
			defer mu.Unlock()
			if service != serviceName || !keyringReference(account) {
				return errors.New("incorrect store address")
			}
			values[account] = value
			return nil
		},
		delete: func(service, account string) error {
			mu.Lock()
			defer mu.Unlock()
			if service != serviceName || !keyringReference(account) {
				return errors.New("incorrect store address")
			}
			if _, ok := values[account]; !ok {
				return keyring.ErrNotFound
			}
			delete(values, account)
			return nil
		},
	})
}

func TestStoreRoundTripReplaceAndIsolation(t *testing.T) {
	s := memoryStore(t)
	ctx := context.Background()
	other := "keyring:" + strings.Repeat("b", 32)
	initial := Record{Kind: KindOpenAIREClient, ClientID: "synthetic-id", ClientSecret: "synthetic-secret"}
	replacement := Record{Kind: KindOpenAIREClient, ClientID: "synthetic-new-id", ClientSecret: "synthetic-new-secret"}
	if r, err := s.Load(ctx, testReference); r != (Record{}) || !sameSentinel(err, ErrNotFound) {
		t.Fatal("missing entry misclassified")
	}
	for _, ref := range []string{testReference, other} {
		if err := s.Save(ctx, ref, initial); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(ctx, testReference, replacement); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Load(ctx, testReference); r != replacement || err != nil {
		t.Fatalf("pair replacement failed: %v", err)
	}
	if err := s.Delete(ctx, testReference); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, testReference); !sameSentinel(err, ErrNotFound) {
		t.Fatal("second delete was not missing")
	}
	if r, err := s.Load(ctx, other); r != initial || err != nil {
		t.Fatal("deletion changed another record")
	}
}

func TestStoreSanitizesBackendErrorsAndCorruptRecords(t *testing.T) {
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
		if record, err := s.Load(context.Background(), testReference); record != (Record{}) || !sameSentinel(err, want) {
			t.Fatal("load exposed backend data or misclassified error")
		}
		for _, err := range []error{s.Save(context.Background(), testReference, validRecords()[0]), s.Delete(context.Background(), testReference)} {
			if !sameSentinel(err, want) || errors.Is(err, cause) {
				t.Fatal("mutation exposed backend error or misclassified it")
			}
		}
	}
	for _, invalid := range []string{"", "private-malformed", `{"version":1,"kind":"core","api_key":"private","surprise":true}`, strings.Repeat("a", MaxEncodedBytes+1)} {
		s := testStore(backend{load: func(string, string) (string, error) { return invalid, nil }})
		if record, err := s.Load(context.Background(), testReference); record != (Record{}) || !sameSentinel(err, ErrInvalidRecord) {
			t.Fatal("invalid stored value escaped")
		}
	}
}

func TestStoreRejectsBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	s := testStore(backend{
		load:   func(string, string) (string, error) { calls.Add(1); return "", nil },
		save:   func(string, string, string) error { calls.Add(1); return nil },
		delete: func(string, string) error { calls.Add(1); return nil },
	})
	ctx := context.Background()
	for _, ref := range []string{"", "../private/path", "env:VALID_ENV", "keyring:" + strings.Repeat("A", 32)} {
		if _, err := s.Load(ctx, ref); !sameSentinel(err, ErrInvalidReference) {
			t.Fatal("non-keyring reference accepted by store")
		}
		if err := s.Save(ctx, ref, validRecords()[0]); !sameSentinel(err, ErrInvalidReference) {
			t.Fatal("non-keyring reference accepted by store")
		}
		if err := s.Delete(ctx, ref); !sameSentinel(err, ErrInvalidReference) {
			t.Fatal("non-keyring reference accepted by store")
		}
	}
	for _, record := range []Record{{}, {Kind: KindCORE, APIKey: strings.Repeat("\"", 1024)}, {Kind: KindOpenAIREClient, ClientID: strings.Repeat("a", 1100), ClientSecret: strings.Repeat("b", 1100)}} {
		if err := s.Save(ctx, testReference, record); !sameSentinel(err, ErrInvalidRecord) {
			t.Fatal("invalid/oversized record accepted")
		}
	}
	canceled, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("synthetic-private-cancellation-cause"))
	_, err := s.Load(canceled, testReference)
	for _, err := range []error{err, s.Save(canceled, testReference, validRecords()[0]), s.Delete(canceled, testReference)} {
		if !sameSentinel(err, context.Canceled) || errors.Is(err, ErrUncertain) {
			t.Fatal("pre-dispatch cancellation should be certain and sanitized")
		}
	}
	if calls.Load() != 0 || s.busy.Load() {
		t.Fatal("invalid request dispatched or retained gate")
	}
	var missing *Store
	if _, err := missing.Load(ctx, testReference); !sameSentinel(err, ErrUnavailable) {
		t.Fatal("nil store was not unavailable")
	}
	if err := (&Store{}).Delete(ctx, testReference); !sameSentinel(err, ErrUnavailable) {
		t.Fatal("zero store was not unavailable")
	}
}

func TestStoreCanceledMutationRetainsGateUntilCompletion(t *testing.T) {
	for _, operation := range []string{"save", "delete"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			var changed atomic.Bool
			fn := func() error {
				calls.Add(1)
				close(entered)
				<-release
				changed.Store(true)
				return nil
			}
			late, _ := Encode(validRecords()[0])
			s := testStore(backend{
				save:   func(string, string, string) error { return fn() },
				delete: func(string, string) error { return fn() },
				load:   func(string, string) (string, error) { return late, nil },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			finished := make(chan error, 1)
			go func() {
				if operation == "save" {
					finished <- s.Save(ctx, testReference, validRecords()[0])
				} else {
					finished <- s.Delete(ctx, testReference)
				}
			}()
			<-entered
			cancel()
			if err := <-finished; !errors.Is(err, ErrUncertain) || !errors.Is(err, context.Canceled) {
				t.Fatalf("dispatched mutation must be uncertain: %v", err)
			}
			sibling := *s
			var wg sync.WaitGroup
			for range 100 {
				wg.Go(func() {
					if err := sibling.Save(context.Background(), testReference, validRecords()[0]); !sameSentinel(err, ErrBusy) {
						t.Errorf("overlapping save: %v", err)
					}
					if err := sibling.Delete(context.Background(), testReference); !sameSentinel(err, ErrBusy) {
						t.Errorf("overlapping delete: %v", err)
					}
					if record, err := sibling.Load(context.Background(), testReference); record != (Record{}) || !sameSentinel(err, ErrBusy) {
						t.Errorf("overlapping load: %v", err)
					}
				})
			}
			wg.Wait()
			if calls.Load() != 1 || changed.Load() {
				t.Fatal("retry dispatched or mutation falsely settled")
			}
			unblock()
			deadline := time.Now().Add(time.Second)
			for s.busy.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s.busy.Load() || !changed.Load() {
				t.Fatal("completed operation did not release gate")
			}
			if record, err := s.Load(context.Background(), testReference); record != validRecords()[0] || err != nil {
				t.Fatalf("settled gate not reusable: %v", err)
			}
		})
	}
}

func TestStoreDefaultDeadline(t *testing.T) {
	release := make(chan struct{})
	s := testStore(backend{save: func(string, string, string) error { <-release; return nil }})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	err := s.Save(ctx, testReference, validRecords()[0])
	elapsed := time.Since(start)
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.DeadlineExceeded) || elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("default deadline: elapsed=%v err=%v", elapsed, err)
	}
	if !s.busy.Load() {
		t.Fatal("deadline released in-flight gate")
	}
}

func TestStoreLoadTimeoutIsNotAbsenceOrMutation(t *testing.T) {
	release := make(chan struct{})
	s := testStore(backend{load: func(string, string) (string, error) { <-release; return "late-invalid", nil }})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	record, err := s.Load(ctx, testReference)
	if record != (Record{}) || !sameSentinel(err, context.DeadlineExceeded) || errors.Is(err, ErrUncertain) || errors.Is(err, ErrNotFound) || !s.busy.Load() {
		t.Fatal("load deadline misclassified or gate released")
	}
}

func TestNewStoresShareOneGateWithoutOSAccess(t *testing.T) {
	a, b := NewStore(), NewStore()
	if a.busy != b.busy || a.busy != &systemBusy || a.timeout != 5*time.Second || a.service != serviceName {
		t.Fatal("default stores do not share a bounded operation gate")
	}
	b.backend.load = func(string, string) (string, error) {
		return "", errors.New("gate bypassed")
	}
	if !a.busy.CompareAndSwap(false, true) {
		t.Fatal("unexpected real store operation")
	}
	defer a.busy.Store(false)
	if _, err := b.Load(context.Background(), testReference); !sameSentinel(err, ErrBusy) {
		t.Fatal("new store bypassed gate")
	}
}
