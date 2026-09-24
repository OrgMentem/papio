// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package captures

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

func TestPinIncidentReplacesLatestAtomically(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 3, MaxAge: 24 * time.Hour})
	first, err := store.Store(ctx, "provider.example.edu", "drift", "adapter", "1", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	latest1, err := store.Store(ctx, "provider.example.edu", "observed", "adapter", "1", []byte("latest-1"))
	if err != nil {
		t.Fatal(err)
	}
	latest2, err := store.Store(ctx, "provider.example.edu", "observed", "adapter", "1", []byte("latest-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PinIncident(ctx, "incident-2", first, latest1); err != nil {
		t.Fatal(err)
	}
	if err := store.PinIncident(ctx, "incident-2", first, latest2); err != nil {
		t.Fatal(err)
	}
	if _, ok := readPin(latest1); ok {
		t.Fatal("previous latest marker survived replacement")
	}
	pin, ok := readPin(latest2)
	if !ok || pin.Fingerprint != "incident-2" || pin.Role != PinLatest {
		t.Fatalf("latest replacement marker = %#v, %v", pin, ok)
	}
	firstPin, ok := readPin(first)
	if !ok || firstPin.Role != PinFirstDecisive {
		t.Fatalf("first decisive marker = %#v, %v", firstPin, ok)
	}
}

func TestStoreSanitizedPinnedRetainsBeforeOutcomePrune(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})
	fixture := func(body string) []byte {
		return []byte("<!-- papio-fixture provider=\"provider\" scenario=\"drift\" origin=\"https://provider.example/\" captured=\"2026-08-10T00:00:00Z\" -->\n" + body)
	}
	first, err := store.StoreSanitizedPinned(ctx, "job-retain", "provider.example", "drift", "provider", "1", fixture("first"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.StoreSanitizedPinned(ctx, "job-retain", "provider.example", "drift", "provider", "1", fixture("latest"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("retained rows = %d, want first/latest despite MaxPerHost=1", len(rows))
	}
	if pin, ok := readPin(first); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("first provisional pin = %#v, %v", pin, ok)
	}
	if pin, ok := readPin(latest); !ok || pin.Role != PinLatest {
		t.Fatalf("latest provisional pin = %#v, %v", pin, ok)
	}
}

const pendingRollbackHost = "provider.example"

func rollbackFixture(body string) []byte {
	return []byte("<!-- papio-fixture provider=\"provider\" scenario=\"drift\" origin=\"https://provider.example/\" captured=\"2026-08-10T00:00:00Z\" -->\n" + body)
}

// pendingRollbackStore stores two successful pinned captures for one job, so a
// later failing call has both a first decisive pin and a prior latest pin that
// it must leave exactly as they are.
func pendingRollbackStore(t *testing.T) (*Store, string, string, time.Time) {
	t.Helper()
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})
	at := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return at }
	first, err := store.StoreSanitizedPinned(ctx, "job-rollback", pendingRollbackHost, "drift", "provider", "1", rollbackFixture("first"))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return at.Add(time.Minute) }
	latest, err := store.StoreSanitizedPinned(ctx, "job-rollback", pendingRollbackHost, "drift", "provider", "1", rollbackFixture("latest"))
	if err != nil {
		t.Fatal(err)
	}
	if pin, ok := readPin(first); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("first pin before failure = %#v, %v", pin, ok)
	}
	if pin, ok := readPin(latest); !ok || pin.Role != PinLatest {
		t.Fatalf("latest pin before failure = %#v, %v", pin, ok)
	}
	return store, first, latest, at
}

// hostSnapshot records every file in one host directory with its bytes, so a
// rollback that leaks, deletes, or rewrites anything is visible.
func hostSnapshot(t *testing.T, hostDir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(hostDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = string(data)
	}
	return out
}

func captureCount(snapshot map[string]string) int {
	count := 0
	for name := range snapshot {
		if strings.HasSuffix(name, htmlExt) {
			count++
		}
	}
	return count
}

func TestStoreSanitizedPinnedRollsBackWhenLeaseIndexFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a Windows directory's mode bits do not stop file creation inside it")
	}
	ctx := context.Background()
	store, first, latest, at := pendingRollbackStore(t)
	hostDir := filepath.Join(store.root, hostDirName(pendingRollbackHost))
	// A capture whose lease association is already durable needs no index
	// write, so the lost index below is what forces this call to write one.
	if err := os.Remove(pendingIndexPath(store.root)); err != nil {
		t.Fatal(err)
	}
	before := hostSnapshot(t, hostDir)
	pendingBefore, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The lease index lives in the store root, so a read-only root fails the
	// index write after the new pin sidecar is already durable.
	if err := os.Chmod(store.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.root, 0o700) })
	store.now = func() time.Time { return at.Add(2 * time.Minute) }
	path, err := store.StoreSanitizedPinned(ctx, "job-rollback", pendingRollbackHost, "drift", "provider", "1", rollbackFixture("third"))
	if err == nil {
		t.Fatal("StoreSanitizedPinned() = nil error, want lease index failure")
	}
	if path != "" {
		t.Fatalf("StoreSanitizedPinned() path = %q, want no usable path", path)
	}

	after := hostSnapshot(t, hostDir)
	if !maps.Equal(before, after) {
		t.Fatalf("host directory after rolled-back capture = %v, want %v", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
	pendingAfter, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pendingBefore, pendingAfter) {
		t.Fatalf("pending jobs after rolled-back capture = %v, want %v", pendingAfter, pendingBefore)
	}
	if pin, ok := readPin(latest); !ok || pin.Role != PinLatest {
		t.Fatalf("prior latest pin after rolled-back capture = %#v, %v", pin, ok)
	}
	if pin, ok := readPin(first); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("first decisive pin after rolled-back capture = %#v, %v", pin, ok)
	}
}

func TestStoreSanitizedPinnedRollsBackWhenPinWriteFails(t *testing.T) {
	ctx := context.Background()
	store, first, latest, at := pendingRollbackStore(t)
	hostDir := filepath.Join(store.root, hostDirName(pendingRollbackHost))
	before := hostSnapshot(t, hostDir)
	pendingBefore, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A directory where the next capture's pin sidecar belongs fails the pin
	// publication rename, after the capture bytes and metadata are durable.
	failAt := at.Add(3 * time.Minute)
	store.now = func() time.Time { return failAt }
	blocked := pinPath(filepath.Join(hostDir, failAt.Format(captureTimestampLayout)+"-drift"+htmlExt))
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := store.StoreSanitizedPinned(ctx, "job-rollback", pendingRollbackHost, "drift", "provider", "1", rollbackFixture("third"))
	if err == nil {
		t.Fatal("StoreSanitizedPinned() = nil error, want pin write failure")
	}
	if path != "" {
		t.Fatalf("StoreSanitizedPinned() path = %q, want no usable path", path)
	}

	if _, err := os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatalf("pin sidecar path %s after rolled-back capture = %v, want removed", blocked, err)
	}
	after := hostSnapshot(t, hostDir)
	if !maps.Equal(before, after) {
		t.Fatalf("host directory after rolled-back capture = %v, want %v", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
	if captureCount(after) != captureCount(before) {
		t.Fatalf("retained captures for host = %d, want %d", captureCount(after), captureCount(before))
	}
	pendingAfter, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pendingBefore, pendingAfter) {
		t.Fatalf("pending jobs after rolled-back capture = %v, want %v", pendingAfter, pendingBefore)
	}
	if pin, ok := readPin(latest); !ok || pin.Role != PinLatest {
		t.Fatalf("prior latest pin after rolled-back capture = %#v, %v", pin, ok)
	}
	if pin, ok := readPin(first); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("first decisive pin after rolled-back capture = %#v, %v", pin, ok)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != captureCount(before) {
		t.Fatalf("listed captures = %d, want %d untouched successful captures", len(rows), captureCount(before))
	}
}
