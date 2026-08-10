// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package captures

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPinnedCapturesSurviveBurstAndReleaseOnSweep(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 2, MaxAge: 24 * time.Hour})
	at := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return at }
	first, err := store.Store(ctx, "provider.example.edu", "drift", "adapter", "1", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.Store(ctx, "provider.example.edu", "observed", "adapter", "1", []byte("latest"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(ctx, first, "incident-1", PinFirstDecisive); err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(ctx, latest, "incident-1", PinLatest); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		store.now = func() time.Time { return at.Add(time.Duration(i+1) * time.Minute) }
		if _, err := store.Store(ctx, "provider.example.edu", "observed", "adapter", "1", []byte("new")); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("captures after burst = %d, want pinned two", len(listed))
	}
	seen := map[string]bool{}
	for _, capture := range listed {
		seen[capture.Path] = true
	}
	if !seen[first] || !seen[latest] {
		t.Fatalf("pinned captures missing after burst: %#v", seen)
	}
	if err := store.ReleaseIncident(ctx, "incident-1"); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return at.Add(48 * time.Hour) }
	if err := store.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	listed, err = store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("captures after releasing incident and sweep = %d, want 0", len(listed))
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("released first capture still exists: %v", err)
	}
}
