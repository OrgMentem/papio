// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bootstrap

import (
	"os"
	"testing"
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
			backend, err := takeAcquisitionBackend()
			if (err != nil) != tc.failed || (backend != nil) != tc.enabled {
				t.Fatalf("backend=%T err=%v", backend, err)
			}
			if _, present := os.LookupEnv("PAPIO_TYPESAFE_API_KEY"); present {
				t.Fatal("credential remains in child environment")
			}
		})
	}
}
