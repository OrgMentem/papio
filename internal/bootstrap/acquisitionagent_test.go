// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bootstrap

import (
	"context"
	"os"
	"testing"

	"papio/internal/config"
	"papio/internal/store/storetest"
)

// Credential precedence and enrollment belong to runtimecredential and are
// pinned there; this test pins what bootstrap adds on top: the acquisition key
// never reaches child processes, and an unusable optional key never stops
// ordinary acquisition from starting.
func TestAcquisitionKeyIsRemovedAndNeverBlocksOrdinaryBootstrap(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"enabled", "synthetic-key"}, // betterleaks:allow -- synthetic test input, never sent
		{"disabled", ""},
		{"invalid", "invalid\nfixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAPIO_TYPESAFE_API_KEY", tc.key)
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
				t.Fatal("ordinary acquisition was not wired")
			}
			if _, present := os.LookupEnv("PAPIO_TYPESAFE_API_KEY"); present {
				t.Fatal("credential remains in child environment")
			}
		})
	}
}
