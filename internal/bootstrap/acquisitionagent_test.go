// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/agentcredential"
	"papio/internal/config"
	"papio/internal/store/storetest"
)

func TestAcquisitionKeyIsRemovedBeforeChildrenCanInheritIt(t *testing.T) {
	for _, tc := range []struct {
		name, key       string
		enabled, failed bool
	}{
		{"enabled", "test-key", true, false},
		{"disabled", "", false, false},
		{"invalid", "bad\nkey", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAPIO_TYPESAFE_API_KEY", tc.key)
			backend, err := takeAcquisitionBackend(context.Background(), config.Default())
			if (err != nil) != tc.failed || (backend != nil) != tc.enabled {
				t.Fatalf("backend=%T err=%v", backend, err)
			}
			if _, present := os.LookupEnv("PAPIO_TYPESAFE_API_KEY"); present {
				t.Fatal("credential remains in child environment")
			}
		})
	}
}

func TestAcquisitionCredentialFailurePreservesOrdinaryBootstrap(t *testing.T) {
	t.Setenv("PAPIO_TYPESAFE_API_KEY", "invalid\nfixture")
	cfg := config.Default()
	cfg.DataDir = storetest.DataDir(t)
	cfg.AccessMode = config.ModeConservative
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Error(err)
		}
	})
	if system.App == nil || len(system.App.Resolvers) == 0 || system.Browser == nil {
		t.Fatal("ordinary acquisition was not wired after optional credential failure")
	}
	if _, exists := os.LookupEnv("PAPIO_TYPESAFE_API_KEY"); exists {
		t.Fatal("key left in child environment")
	}
}

func TestAcquisitionCredentialEnrollmentAndOverride(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		enrolled, override             bool
		env, stored                    string
		storeErr                       error
		wantLoad, wantEnabled, wantErr bool
	}{
		{name: "not enrolled"},
		{name: "stored key", enrolled: true, stored: "synthetic-key", wantLoad: true, wantEnabled: true},
		{name: "missing key", enrolled: true, storeErr: agentcredential.ErrNotFound, wantLoad: true, wantErr: true},
		{name: "locked store", enrolled: true, storeErr: errors.New("private store detail"), wantLoad: true, wantErr: true},
		{name: "bad stored key", enrolled: true, stored: "private\nkey", wantLoad: true, wantErr: true},
		{name: "environment priority", enrolled: true, override: true, env: "synthetic-env", wantEnabled: true},
		{name: "empty override disables", enrolled: true, override: true},
		{name: "invalid override never falls back", enrolled: true, override: true, env: "private\nkey", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAPIO_TYPESAFE_API_KEY", tc.env)
			if !tc.override {
				_ = os.Unsetenv("PAPIO_TYPESAFE_API_KEY")
			}
			cfg := config.Default()
			cfg.Path, cfg.DataDir = filepath.Join(t.TempDir(), "config.toml"), t.TempDir()
			if tc.enrolled {
				cfg.Agent = &config.Agent{Backend: "typesafe"}
			}
			calls := 0
			backend, err := takeAcquisitionBackendWithStore(context.Background(), cfg, func(_ context.Context, profile string) (string, error) {
				calls++
				want, e := agentcredential.Profile(cfg.Path, cfg.DataDir)
				if e != nil || profile != want {
					t.Fatal("wrong credential profile")
				}
				if _, present := os.LookupEnv("PAPIO_TYPESAFE_API_KEY"); present {
					t.Fatal("environment not cleared before store subprocess")
				}
				return tc.stored, tc.storeErr
			})
			if (calls == 1) != tc.wantLoad || (backend != nil) != tc.wantEnabled || (err != nil) != tc.wantErr {
				t.Fatalf("calls=%d backend=%T err=%v", calls, backend, err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("private error escaped")
			}
			if _, present := os.LookupEnv("PAPIO_TYPESAFE_API_KEY"); present {
				t.Fatal("key remains in child environment")
			}
		})
	}
}
