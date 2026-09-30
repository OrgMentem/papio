// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package captures

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Simulate the index-first crash window: .pending.json is durable but the pin
// sidecar never landed. The restart keeps the index because the job is still
// awaiting, so retention must not prune the live evidence and the reconciler
// must restore the pin for the active lease while leaving an inactive lease
// alone.
func TestReconcileRestoresMissingPinForActiveLease(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})

	live, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pinPath(live)); err != nil {
		t.Fatal(err)
	}
	if _, ok := readPin(live); ok {
		t.Fatal("pin still present after crash simulation")
	}
	pending, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "job-live" {
		t.Fatalf("pending jobs = %v, want [job-live]", pending)
	}
	metadata, err := readMetadata(metadataPath(live))
	if err != nil || metadata.PendingJobID != "job-live" {
		t.Fatalf("metadata lease link = %#v, %v; want job-live", metadata, err)
	}

	// Retention pressure before any reconciler runs: an ordinary capture must
	// go, the indexed-but-unpinned live evidence must stay.
	ordinary, err := store.Store(ctx, "provider.example", "drift", "provider", "1", []byte("ordinary"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(listed))
	for _, row := range listed {
		seen[row.Path] = true
	}
	if !seen[live] {
		t.Fatalf("live indexed-but-unpinned capture was pruned before reconcile; listed=%v", seen)
	}
	if seen[ordinary] {
		t.Fatalf("ordinary capture survived while leased evidence needed the slot")
	}

	// A second interrupted write for a job that already left must not gain a
	// pin when only the live job is active.
	dead, err := store.StoreSanitizedPinned(ctx, "job-dead", "other.example", "drift", "provider", "1", pendingLeaseFixture("other.example", "drift", "dead"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pinPath(dead)); err != nil {
		t.Fatal(err)
	}

	reindexed, restored, err := store.ReconcilePendingPins(ctx, []string{"job-live"})
	if err != nil {
		t.Fatal(err)
	}
	if reindexed != 0 {
		t.Fatalf("reindexed = %d, want 0 (both leases already indexed)", reindexed)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1 (live pin only)", restored)
	}
	if pin, ok := readPin(live); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("live pin after reconcile = %#v, %v; want first decisive", pin, ok)
	}
	if _, ok := readPin(dead); ok {
		t.Fatal("dead pin was restored for an inactive lease")
	}
}

// Both halves of a lease lost: no index entry and no pin, only the metadata
// link. When the reconcile cannot publish the index, it must not restore the
// pin either, because a pin without an index entry is invisible to
// PendingJobs. The next reconcile, with the index writable, restores both.
func TestReconcileRestoresNoPinWhenIndexPublishFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a Windows directory's mode bits do not stop file creation inside it")
	}
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})
	live, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pinPath(live)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pendingIndexPath(store.root)); err != nil {
		t.Fatal(err)
	}
	// The index lives in the store root; the pin lives in the host directory,
	// which stays writable.
	if err := os.Chmod(store.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.root, 0o700) })
	if _, _, err := store.ReconcilePendingPins(ctx, []string{"job-live"}); err == nil {
		t.Fatal("ReconcilePendingPins() = nil error, want index publish failure")
	}
	if pin, ok := readPin(live); ok {
		t.Fatalf("pin restored without an index entry: %#v", pin)
	}

	if err := os.Chmod(store.root, 0o700); err != nil {
		t.Fatal(err)
	}
	reindexed, restored, err := store.ReconcilePendingPins(ctx, []string{"job-live"})
	if err != nil || reindexed != 1 || restored != 1 {
		t.Fatalf("retry reconcile = %d reindexed, %d restored, %v; want 1, 1, nil", reindexed, restored, err)
	}
	if pending, err := store.PendingJobs(ctx); err != nil || len(pending) != 1 || pending[0] != "job-live" {
		t.Fatalf("pending jobs after retry = %v, %v; want [job-live]", pending, err)
	}
	if pin, ok := readPin(live); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("live pin after retry = %#v, %v; want first decisive", pin, ok)
	}
}

// Simulate the legacy pin-first crash window: the pin sidecar is durable but
// the index write never landed. The reconciler must re-index the pin for an
// active lease before orphan cleanup runs; a genuinely orphan pin for a job
// that already left must still be collected.
func TestReconcileReindexesMissingIndexForActiveLease(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})

	live, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live"))
	if err != nil {
		t.Fatal(err)
	}
	dead, err := store.StoreSanitizedPinned(ctx, "job-dead", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "dead"))
	if err != nil {
		t.Fatal(err)
	}
	livePin, ok := readPin(live)
	if !ok {
		t.Fatal("live capture has no pin")
	}

	// Drop both index entries: pins on disk, no lease pointing at them.
	if err := os.WriteFile(pendingIndexPath(store.root), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingJobs(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending jobs = %v, %v; want empty after crash simulation", pending, err)
	}

	reindexed, restored, err := store.ReconcilePendingPins(ctx, []string{"job-live"})
	if err != nil {
		t.Fatal(err)
	}
	if reindexed != 1 {
		t.Fatalf("reindexed = %d, want 1 (live lease only)", reindexed)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (both pins already present)", restored)
	}
	pending, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "job-live" {
		t.Fatalf("pending jobs after reconcile = %v, want [job-live]", pending)
	}
	if pin, ok := readPin(live); !ok || pin.Fingerprint != livePin.Fingerprint {
		t.Fatalf("live pin after reconcile = %#v, %v; want it preserved", pin, ok)
	}

	// The remaining indexless pin belongs to no active lease, so orphan
	// cleanup still collects exactly it and leaves the re-indexed lease alone.
	removed, err := store.ReleaseOrphanPendingPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 genuine orphan", removed)
	}
	if _, ok := readPin(dead); ok {
		t.Fatalf("genuine orphan pin survives on %s", dead)
	}
	if pin, ok := readPin(live); !ok || pin.Fingerprint != livePin.Fingerprint {
		t.Fatalf("live lease pin = %#v, %v; want it preserved", pin, ok)
	}

	// The released capture is ordinary again, so the sweep enforces the
	// per-host bound instead of keeping both forever.
	if err := store.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Path != live {
		paths := make([]string, 0, len(listed))
		for _, row := range listed {
			paths = append(paths, row.Path)
		}
		t.Fatalf("listed after sweep = %v, want only live", paths)
	}
}

// A second capture for one job that loses its pin must come back as the new
// latest while the displaced prior latest loses both its pin and its lease
// link, so retention can evict the intermediate instead of keeping every
// capture for an active job forever.
func TestReconcileRestoresLatestAndClearsDisplacedLink(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 24 * time.Hour})
	at := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return at }

	first, err := store.StoreSanitizedPinned(ctx, "job-multi", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "first"))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return at.Add(time.Minute) }
	second, err := store.StoreSanitizedPinned(ctx, "job-multi", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "second"))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return at.Add(2 * time.Minute) }
	third, err := store.StoreSanitizedPinned(ctx, "job-multi", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "third"))
	if err != nil {
		t.Fatal(err)
	}
	// The third write displaced the second as latest; the second is ordinary
	// evidence again with no lease link.
	if _, ok := readPin(second); ok {
		t.Fatal("second pin survives displacement")
	}
	if metadata, err := readMetadata(metadataPath(second)); err != nil || metadata.PendingJobID != "" {
		t.Fatalf("displaced metadata link = %#v, %v; want it cleared", metadata, err)
	}

	// Crash the third pin write: index still names the lease, the bytes still
	// link to it, but no pin protects the newest evidence.
	if err := os.Remove(pinPath(third)); err != nil {
		t.Fatal(err)
	}

	reindexed, restored, err := store.ReconcilePendingPins(ctx, []string{"job-multi"})
	if err != nil {
		t.Fatal(err)
	}
	if reindexed != 0 || restored != 1 {
		t.Fatalf("reindexed, restored = %d, %d; want 0, 1", reindexed, restored)
	}
	if pin, ok := readPin(first); !ok || pin.Role != PinFirstDecisive {
		t.Fatalf("first pin = %#v, %v; want first decisive", pin, ok)
	}
	if pin, ok := readPin(third); !ok || pin.Role != PinLatest {
		t.Fatalf("third pin = %#v, %v; want latest", pin, ok)
	}
	if _, ok := readPin(second); ok {
		t.Fatal("displaced second pin came back")
	}
	_ = first
}

// A capture whose bytes no longer match its recorded hash must not gain a pin:
// restoring it would protect tampered evidence as first/latest.
func TestReconcileSkipsModifiedCapture(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 24 * time.Hour})

	live, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pinPath(live)); err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, append(html, []byte("tampered")...), 0o600); err != nil {
		t.Fatal(err)
	}

	_, restored, err := store.ReconcilePendingPins(ctx, []string{"job-live"})
	if err != nil {
		t.Fatal(err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 for modified bytes", restored)
	}
	if _, ok := readPin(live); ok {
		t.Fatal("pin was restored for modified bytes")
	}
}

// Reconciliation with no active jobs is a no-op and never resurrects leases;
// genuine orphans stay collectible.
func TestReconcileWithNoActiveJobsLeavesOrphans(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})

	live, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live"))
	if err != nil {
		t.Fatal(err)
	}
	livePin, ok := readPin(live)
	if !ok {
		t.Fatal("live capture has no pin")
	}
	// Simulate the pin-without-index window and reconcile with nobody active.
	if err := os.WriteFile(pendingIndexPath(store.root), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reindexed, restored, err := store.ReconcilePendingPins(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reindexed != 0 || restored != 0 {
		t.Fatalf("reindexed, restored = %d, %d; want 0, 0 with no active jobs", reindexed, restored)
	}
	removed, err := store.ReleaseOrphanPendingPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 genuine orphan", removed)
	}
	if _, ok := readPin(live); ok {
		t.Fatalf("orphan pin survives: %#v", livePin)
	}
}

// The pending index is local daemon state that must stay valid JSON: a corrupt
// index fails reconciliation instead of guessing which leases are active.
func TestReconcileRejectsCorruptIndex(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})
	if _, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingIndexPath(store.root), []byte(`{not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReconcilePendingPins(ctx, []string{"job-live"}); err == nil {
		t.Fatal("ReconcilePendingPins() = nil error, want corrupt index failure")
	}
}

// Re-indexing an active lease must preserve the opaque fingerprint mapping the
// rest of the store uses; spot-check the rewritten index file.
func TestReconcileIndexFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := New(dir, Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})
	if _, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pendingIndexPath(store.root)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReconcilePendingPins(ctx, []string{"  job-live "}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "captures", ".pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]string
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index) != 1 || index[pendingFingerprint("job-live")] != "job-live" {
		t.Fatalf("rewritten index = %v, want one trimmed job-live entry", index)
	}
}

// Releasing a lease must drop its metadata links as well as its pins and
// index entry. Otherwise a retry with the same job ID resurrects stale
// captures through ReconcilePendingPins instead of starting fresh.
func TestReleaseJobClearsLeaseLinks(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 24 * time.Hour})

	first, err := store.StoreSanitizedPinned(ctx, "job-retry", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "first"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.StoreSanitizedPinned(ctx, "job-retry", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "latest"))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.ReleaseJob(ctx, "job-retry"); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingJobs(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending jobs after release = %v, %v; want empty", pending, err)
	}
	for _, path := range []string{first, latest} {
		if _, ok := readPin(path); ok {
			t.Fatalf("pin survives release on %s", path)
		}
		if metadata, err := readMetadata(metadataPath(path)); err != nil || metadata.PendingJobID != "" {
			t.Fatalf("lease link survives release on %s: %#v, %v", path, metadata, err)
		}
	}

	// A retry with the same job ID must not restore the released captures.
	reindexed, restored, err := store.ReconcilePendingPins(ctx, []string{"job-retry"})
	if err != nil {
		t.Fatal(err)
	}
	if reindexed != 0 || restored != 0 {
		t.Fatalf("reindexed, restored = %d, %d; want 0, 0 after release", reindexed, restored)
	}
	for _, path := range []string{first, latest} {
		if _, ok := readPin(path); ok {
			t.Fatalf("released capture resurrected on %s", path)
		}
	}
	if pending, err := store.PendingJobs(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending jobs after retry reconcile = %v, %v; want empty", pending, err)
	}

	// The released captures are ordinary evidence again, so retention evicts
	// them once the host exceeds its bound.
	for _, body := range []string{"fresh-a", "fresh-b"} {
		if _, err := store.Store(ctx, "provider.example", "drift", "provider", "1", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	store.retention.MaxPerHost = 1
	if err := store.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, latest} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("released capture %s survives sweep with no lease", path)
		}
	}
}

// Collecting a genuine orphan must drop its lease link too, so a later retry
// with the same job ID cannot bring the collected file back as evidence.
func TestOrphanCollectionClearsLeaseLink(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 24 * time.Hour})

	orphan, err := store.StoreSanitizedPinned(ctx, "job-gone", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "orphan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingIndexPath(store.root), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := store.ReleaseOrphanPendingPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if metadata, err := readMetadata(metadataPath(orphan)); err != nil || metadata.PendingJobID != "" {
		t.Fatalf("lease link survives orphan collection: %#v, %v", metadata, err)
	}

	reindexed, restored, err := store.ReconcilePendingPins(ctx, []string{"job-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if reindexed != 0 || restored != 0 {
		t.Fatalf("reindexed, restored = %d, %d; want 0, 0 for a collected orphan", reindexed, restored)
	}
	if _, ok := readPin(orphan); ok {
		t.Fatal("collected orphan pin came back on retry")
	}
}

// PendingLeaseCandidates must surface jobs referenced only by on-disk traces
// (a pin-first crash leaves no index entry), so the poll can complete its
// active set past the awaiting-page cap.
func TestPendingLeaseCandidatesCoversIndexlessPins(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 24 * time.Hour})

	if _, err := store.StoreSanitizedPinned(ctx, "job-indexed", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreSanitizedPinned(ctx, "job-indexless", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "b")); err != nil {
		t.Fatal(err)
	}
	livePin, ok := readPinForJob(t, store, "job-indexless")
	if !ok {
		t.Fatal("indexless capture has no pin")
	}
	_ = livePin
	// Drop the whole index: both jobs are now referenced only by pins and
	// metadata links.
	if err := os.WriteFile(pendingIndexPath(store.root), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	candidates, err := store.PendingLeaseCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0] != "job-indexed" || candidates[1] != "job-indexless" {
		t.Fatalf("candidates = %v, want [job-indexed job-indexless]", candidates)
	}
}

func readPinForJob(t *testing.T, store *Store, jobID string) (capturePin, bool) {
	t.Helper()
	entries, err := os.ReadDir(store.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			continue
		}
		files, err := scanHost(context.Background(), filepath.Join(store.root, entry.Name()), entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if pin, ok := readPin(file.Path); ok && pin.Fingerprint == pendingFingerprint(jobID) {
				return pin, true
			}
		}
	}
	return capturePin{}, false
}
