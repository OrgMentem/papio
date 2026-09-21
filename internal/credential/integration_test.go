// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build credential_integration

package credential

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestOSStoreRoundTrip requires both explicit opt-ins:
//
//	PAPIO_CREDENTIAL_INTEGRATION=1 go test -race -tags credential_integration ./internal/credential -run '^TestOSStoreRoundTrip$' -count=1
//
// Only synthetic records in a unique test namespace are used. On Windows run
// in the signed-in user's interactive context; a network logon may lack a vault.
func TestOSStoreRoundTrip(t *testing.T) {
	if os.Getenv("PAPIO_CREDENTIAL_INTEGRATION") != "1" {
		t.Skip("requires explicit OS credential store integration opt-in")
	}
	ref, err := NewReference()
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	s.service = serviceName + ".test." + strings.TrimPrefix(ref, "keyring:")
	ctx := context.Background()
	if _, err := s.Load(ctx, ref); err != ErrNotFound {
		t.Fatalf("isolated record is not known absent: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for {
			err := s.Delete(cleanupCtx, ref)
			if (errors.Is(err, ErrBusy) || errors.Is(err, ErrUncertain)) && cleanupCtx.Err() == nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil && err != ErrNotFound {
				t.Errorf("synthetic cleanup incomplete: service=%s reference=%s: %v", s.service, ref, err)
				return
			}
			if _, err := s.Load(cleanupCtx, ref); err != ErrNotFound {
				t.Errorf("synthetic cleanup unverified: service=%s reference=%s: %v", s.service, ref, err)
			}
			return
		}
	})
	// All kinds, Unicode, the complete URL and both halves of a client pair
	// must survive actual store encoding and repeated atomic replacement.
	records := append(validRecords(), Record{Kind: KindCORE, APIKey: "synthetic-密钥"})
	max := Record{Kind: KindCORE, APIKey: "a"}
	encoded, _ := Encode(max)
	max.APIKey = strings.Repeat("a", MaxEncodedBytes-len(encoded)+1)
	records = append(records, max)
	for _, record := range records {
		if err := s.Save(ctx, ref, record); err != nil {
			t.Fatal(err)
		}
		if loaded, err := s.Load(ctx, ref); loaded != record || err != nil {
			t.Fatalf("synthetic record did not round trip: %v", err)
		}
	}
	if err := s.Save(ctx, ref, Record{Kind: KindCORE, APIKey: strings.Repeat("x", MaxEncodedBytes)}); err != ErrInvalidRecord {
		t.Fatal("oversized record reached native storage")
	}
	if loaded, err := s.Load(ctx, ref); loaded != max || err != nil {
		t.Fatal("rejected save changed native record")
	}
	r := Resolve(ctx, []Binding{{Target: "wrong-kind", Reference: ref, Kinds: []Kind{KindTypeSafe}}}, s, nil)
	if _, ok := r.Record("wrong-kind"); ok || r.Statuses()[0].State != "invalid" {
		t.Fatal("actual stored record bypassed consumer kind validation")
	}
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, ref); err != ErrNotFound {
		t.Fatalf("synthetic record was not removed: %v", err)
	}
}
