// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build agentcredential_integration

package agentcredential

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

// TestOSStoreRoundTrip requires two explicit opt-ins:
//
//	PAPIO_AGENTCREDENTIAL_INTEGRATION=1 go test -race -tags agentcredential_integration ./internal/agentcredential -run '^TestOSStoreRoundTrip$' -count=1
//
// It uses a synthetic key in a unique service/account, never a configured profile.
// Cleanup is registered before the first write, runs after failed assertions too,
// and verifies absence. If the OS store cannot finish cleanup, the test fails and
// reports only this synthetic service/account for manual removal.
func TestOSStoreRoundTrip(t *testing.T) {
	if os.Getenv("PAPIO_AGENTCREDENTIAL_INTEGRATION") != "1" {
		t.Skip("requires explicit OS credential store integration opt-in")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("cannot generate isolated test account")
	}
	account := hex.EncodeToString(random[:])
	s := NewStore()
	s.service = serviceName + ".test." + account
	ctx := context.Background()
	if _, err := s.Load(ctx, account); err != ErrNotFound {
		t.Fatalf("isolated account is not known to be absent: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for {
			err := s.Delete(cleanupCtx, account)
			if (errors.Is(err, ErrBusy) || errors.Is(err, ErrUncertain)) && cleanupCtx.Err() == nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil && err != ErrNotFound {
				t.Errorf("synthetic cleanup incomplete: service=%s account=%s: %v", s.service, account, err)
				return
			}
			if _, err := s.Load(cleanupCtx, account); err != ErrNotFound {
				t.Errorf("synthetic cleanup not verified: service=%s account=%s: %v", s.service, account, err)
			}
			return
		}
	})
	for _, key := range []string{"papio-synthetic-initial-" + account, "papio-synthetic-replacement-" + account} {
		if err := s.Save(ctx, account, key); err != nil {
			t.Fatal(err)
		}
		if loaded, err := s.Load(ctx, account); loaded != key || err != nil {
			t.Fatalf("synthetic key did not round-trip: %v", err)
		}
	}
	if err := s.Delete(ctx, account); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, account); err != ErrNotFound {
		t.Fatalf("synthetic key was not removed: %v", err)
	}
}
