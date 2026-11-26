// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/agentcredential"
)

const (
	helperDirEnv = "PAPIO_CREDENTIAL_TEST_HELPER_DIR"
	hungRef      = "keyring:ffffffffffffffffffffffffffffffff"
)

// TestMain lets this test image act as the helper process. Its fake OS store
// never returns for hungRef and reports absence for any other reference.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == agentcredential.HelperArgument {
		raw, _ := io.ReadAll(os.Stdin)
		var req struct{ Account string }
		_ = json.Unmarshal(raw, &req)
		if req.Account == hungRef {
			_ = os.WriteFile(filepath.Join(os.Getenv(helperDirEnv), "hung"), nil, 0o600)
			time.Sleep(time.Hour)
		}
		_, _ = os.Stdout.WriteString(`{"status":"not_found"}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(helperDirEnv, dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := func() { t.Error("helper store called the OS in process") }
	s := testStore(backend{
		load:   func(string, string) (string, error) { calls(); return "", nil },
		save:   func(string, string, string) error { calls(); return nil },
		delete: func(string, string) error { calls(); return nil },
	})
	s.helper = executable
	s.timeout = 500 * time.Millisecond
	return s, dir
}

func TestHelperStoreKillsHungCallAndRecovers(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		name := "load"
		if mutation {
			name = "save"
		}
		t.Run(name, func(t *testing.T) {
			s, dir := helperTestStore(t)
			var err error
			if mutation {
				err = s.Save(context.Background(), hungRef, Record{Kind: KindOpenAlex, APIKey: "synthetic-key"})
			} else {
				_, err = s.Load(context.Background(), hungRef)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "hung")); statErr != nil {
				t.Fatal("the helper never reached the hung call")
			}
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUncertain) != mutation {
				t.Fatalf("hung call = %v, want a deadline, ErrUncertain only for a mutation", err)
			}
			for deadline := time.Now().Add(5 * time.Second); s.busy.Load() && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			if s.busy.Load() {
				t.Fatal("the gate stayed held after the helper was killed")
			}
			s.timeout = 10 * time.Second
			if _, err := s.Load(context.Background(), testReference); !sameSentinel(err, ErrNotFound) {
				t.Fatalf("load after the kill = %v, want the helper reachable (ErrNotFound)", err)
			}
		})
	}
}

func TestHelperStoreUnstartableHelperIsUnavailableWithoutInProcessCall(t *testing.T) {
	var calls atomic.Int32
	s := testStore(backend{
		load:   func(string, string) (string, error) { calls.Add(1); return "", nil },
		save:   func(string, string, string) error { calls.Add(1); return nil },
		delete: func(string, string) error { calls.Add(1); return nil },
	})
	s.helper = filepath.Join(t.TempDir(), "missing")
	ctx := context.Background()
	if _, err := s.Load(ctx, testReference); !sameSentinel(err, ErrUnavailable) {
		t.Fatalf("load = %v, want ErrUnavailable", err)
	}
	if err := s.Save(ctx, testReference, Record{Kind: KindOpenAlex, APIKey: "synthetic-key"}); !sameSentinel(err, ErrUnavailable) {
		t.Fatalf("save = %v, want ErrUnavailable", err)
	}
	if err := s.Delete(ctx, testReference); !sameSentinel(err, ErrUnavailable) {
		t.Fatalf("delete = %v, want ErrUnavailable", err)
	}
	if calls.Load() != 0 || s.busy.Load() {
		t.Fatal("an in-process call ran or the gate stayed held")
	}
}

func TestNewStoreWithoutHelperHasNoOSBackend(t *testing.T) {
	s := NewStore()
	if s.helper != "" {
		t.Skip("a helper is configured in this process")
	}
	ctx := context.Background()
	if _, err := s.Load(ctx, testReference); !sameSentinel(err, ErrUnavailable) {
		t.Fatalf("load = %v, want ErrUnavailable", err)
	}
	if err := s.Delete(ctx, testReference); !sameSentinel(err, ErrUnavailable) {
		t.Fatalf("delete = %v, want ErrUnavailable", err)
	}
}

func TestDispatchKilledMutationIsUncertain(t *testing.T) {
	for _, op := range []string{agentcredential.OpSave, agentcredential.OpDelete, agentcredential.OpLoad} {
		t.Run(op, func(t *testing.T) {
			s, _ := helperTestStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := s.dispatch(ctx, op, hungRef, "value")
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUncertain) != (op != agentcredential.OpLoad) {
				t.Fatalf("%s killed at the deadline = %v", op, err)
			}
		})
	}
}

func TestHelperNamespaceRejectsInvalidRecords(t *testing.T) {
	ns := HelperNamespace()
	good, err := Encode(Record{Kind: KindOpenAlex, APIKey: "synthetic-key"})
	if err != nil {
		t.Fatal(err)
	}
	if !ns.ValidValue(good) || ns.ValidValue("not a record") || ns.ValidValue("") || ns.ValidAccount("keyring:zz") || !ns.ValidAccount(testReference) {
		t.Fatal("helper namespace validation is wrong")
	}
}
