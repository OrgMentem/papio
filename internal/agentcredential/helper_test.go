// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package agentcredential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	helperDirEnv = "PAPIO_AGENTCREDENTIAL_TEST_HELPER_DIR"
	// hungProfile names the account whose OS call never returns in the test
	// helper, as a stuck Keychain call would.
	hungProfile      = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	helperHungMarker = "hung"
)

// TestMain lets this test image serve as the helper process. Its fake OS store
// keeps one file per account in a directory the parent names, so separate helper
// processes share state, and it hangs for hungProfile.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == HelperArgument {
		dir := os.Getenv(helperDirEnv)
		file := func(account string) string { return filepath.Join(dir, account) }
		hang := func(account string) {
			if account == hungProfile {
				// Tell the test the OS call was dispatched, so it ends the call
				// only once the helper is blocked in it.
				_ = os.WriteFile(filepath.Join(dir, helperHungMarker), nil, 0o600)
				time.Sleep(time.Hour)
			}
		}
		fake := backend{
			load: func(_, account string) (string, error) {
				hang(account)
				data, err := os.ReadFile(file(account))
				if errors.Is(err, os.ErrNotExist) {
					return "", keyring.ErrNotFound
				}
				return string(data), err
			},
			save: func(_, account, key string) error {
				hang(account)
				return os.WriteFile(file(account), []byte(key), 0o600)
			},
			delete: func(_, account string) error {
				hang(account)
				if err := os.Remove(file(account)); errors.Is(err, os.ErrNotExist) {
					return keyring.ErrNotFound
				} else {
					return err
				}
			},
		}
		if err := serveHelper(os.Stdin, os.Stdout, fake); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(helperDirEnv, dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Store{service: serviceName, helper: executable, busy: new(atomic.Bool), timeout: operationTimeout}, dir
}

func TestHelperStoreRoundTrip(t *testing.T) {
	s, _ := helperStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, testProfile, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	if key, err := s.Load(ctx, testProfile); key != "synthetic-key" || err != nil {
		t.Fatalf("load = %q, %v", key, err)
	}
	if err := s.Delete(ctx, testProfile); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, testProfile); !sameSentinel(err, ErrNotFound) {
		t.Fatalf("load after delete = %v, want ErrNotFound", err)
	}
}

// TestHelperStoreFallsBackWhenTheHelperCannotStart pins that isolation is
// hardening, not a dependency: after the executable is replaced on disk the
// call still reaches the OS store, in process, and the gate is released.
func TestHelperStoreFallsBackWhenTheHelperCannotStart(t *testing.T) {
	s := testStore(backend{load: func(string, string) (string, error) { return "synthetic-key", nil }})
	s.helper = filepath.Join(t.TempDir(), "missing")
	if key, err := s.Load(context.Background(), testProfile); key != "synthetic-key" || err != nil {
		t.Fatalf("load with a missing helper = %q, %v, want the in-process call", key, err)
	}
	if s.busy.Load() {
		t.Fatal("the gate stayed held")
	}
}

// TestHelperStoreRecoversAfterAHungOSCall pins the capacity guarantee: a call
// that never returns is killed when its caller gives up, so the next call
// reaches the OS store instead of reporting busy until the process restarts.
func TestHelperStoreRecoversAfterAHungOSCall(t *testing.T) {
	for _, mutation := range []bool{true, false} {
		name := "load"
		if mutation {
			name = "save"
		}
		t.Run(name, func(t *testing.T) {
			s, dir := helperStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errs := make(chan error, 1)
			go func() {
				if mutation {
					errs <- s.Save(ctx, hungProfile, "synthetic-key")
					return
				}
				_, err := s.Load(ctx, hungProfile)
				errs <- err
			}()
			marker := filepath.Join(dir, helperHungMarker)
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the helper never reached the hung OS call")
				}
			}
			if !s.busy.Load() {
				t.Fatal("a call in flight did not hold the gate")
			}
			cancel()
			err := <-errs
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUncertain) != mutation {
				t.Fatalf("hung call = %v, want cancellation that is ErrUncertain only for a mutation", err)
			}
			// The gate is released once the worker is gone, which follows the
			// kill; allow for that to land.
			for deadline := time.Now().Add(5 * time.Second); s.busy.Load() && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			if s.busy.Load() {
				t.Fatal("the gate stayed held after the hung worker was killed")
			}
			if mutation {
				// One call through the recovered gate is enough; each helper
				// start costs a process spawn.
				if err := s.Save(context.Background(), testProfile, "synthetic-key"); err != nil {
					t.Fatalf("save after the hung call = %v, want the OS store reachable again", err)
				}
			}
		})
	}
}

func TestServeHelperRejectsRequestsForOtherCredentials(t *testing.T) {
	never := func(string, string) (string, error) { t.Error("backend reached"); return "", nil }
	b := backend{load: never, save: func(string, string, string) error { t.Error("backend reached"); return nil }, delete: func(string, string) error { t.Error("backend reached"); return nil }}
	for name, request := range map[string]string{
		"other service":  `{"op":"load","service":"com.example.other","account":"` + testProfile + `"}`,
		"invalid acount": `{"op":"load","service":"` + serviceName + `","account":"../x"}`,
		"unknown op":     `{"op":"list","service":"` + serviceName + `","account":"` + testProfile + `"}`,
		"invalid key":    `{"op":"save","service":"` + serviceName + `","account":"` + testProfile + `","key":"has space"}`,
		"extra field":    `{"op":"load","service":"` + serviceName + `","account":"` + testProfile + `","x":1}`,
		"two requests":   `{"op":"load","service":"` + serviceName + `","account":"` + testProfile + `"}{}`,
	} {
		var out strings.Builder
		if err := serveHelper(strings.NewReader(request), &out, b); err == nil || out.Len() != 0 {
			t.Errorf("%s: serveHelper = %v, output %q; want a refusal with no response", name, err, out.String())
		}
	}
}
