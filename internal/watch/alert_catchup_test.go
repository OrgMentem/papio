// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/discovery"
	"papio/internal/work"
	"papio/internal/zotio"
)

// A stranded digest entry must be retried even when the next scan finds
// nothing new. Before the fix the runner returned early on an empty scan
// without consulting UnalertedDigestEntries, so a failed route was lost
// forever when the following cadence reported no hits.
func TestRunnerAlertRetriesStrandedDigestWhenScanFindsNothing(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "stranded empty", Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	discoveryFake := &fakeDiscovery{works: []discovery.DiscoveredWork{{
		Work: work.Work{DOI: "10.1000/stranded-empty", Title: "Stranded Empty", Authors: []string{"Ada"}, Year: 2026},
	}}}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}}
	broken := &fakeNotifier{err: errors.New("desktop unavailable")}
	runner := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Notifier: broken,
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	if _, err := runner.Run(ctx, watched.ID); err == nil || !strings.Contains(err.Error(), "routing watch alert") {
		t.Fatalf("Run() error = %v; want the alert routing failure", err)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, watched.ID); err != nil || len(unalerted) != 1 {
		t.Fatalf("unalerted entries = %+v, %v; want the stranded discovery", unalerted, err)
	}

	// The next scan finds nothing, yet the stranded entry must still be
	// announced exactly once.
	discoveryFake.works = nil
	fixed := &fakeNotifier{}
	rerun := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Notifier: fixed,
		DataDir: runner.DataDir, Now: func() time.Time { return now },
	}
	recovered, err := rerun.Run(ctx, watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reported != 0 {
		t.Fatalf("recovery run result = %+v; want no new discoveries", recovered)
	}
	if len(fixed.intents) != 1 || fixed.intents[0].Detail.Count != 1 {
		t.Fatalf("notifications = %+v; want exactly one catch-up alert", fixed.messages)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, watched.ID); err != nil || len(unalerted) != 0 {
		t.Fatalf("unalerted entries = %+v, %v; want the receipt recorded", unalerted, err)
	}
	stored, err := watches.Get(ctx, watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConsecutiveFailures != 0 {
		t.Fatalf("watch health = %+v; want the failure cleared after recovery", stored)
	}

	// A further empty scan announces nothing again.
	if _, err := rerun.Run(ctx, watched.ID); err != nil {
		t.Fatal(err)
	}
	if len(fixed.intents) != 1 {
		t.Fatalf("notifications = %+v; want no duplicate alert", fixed.messages)
	}
}

// The same stranding must be retried when the next scan finds only owned
// work. The ownership filter leaves the queue empty, which took the same
// early return as an empty scan and likewise skipped the catch-up alert.
func TestRunnerAlertRetriesStrandedDigestWhenScanFindsOnlyOwned(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "stranded owned", Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	discoveryFake := &fakeDiscovery{works: []discovery.DiscoveredWork{{
		Work: work.Work{DOI: "10.1000/stranded-owned", Title: "Stranded Owned", Authors: []string{"Ada"}, Year: 2026},
	}}}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}}
	broken := &fakeNotifier{err: errors.New("desktop unavailable")}
	runner := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Notifier: broken,
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	if _, err := runner.Run(ctx, watched.ID); err == nil || !strings.Contains(err.Error(), "routing watch alert") {
		t.Fatalf("Run() error = %v; want the alert routing failure", err)
	}

	// The stranded work is now owned, so the queue is empty — but the
	// earlier alert still never arrived and must be retried.
	lookup.result = &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipOwnedWithPDF}}}
	fixed := &fakeNotifier{}
	rerun := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Notifier: fixed,
		DataDir: runner.DataDir, Now: func() time.Time { return now },
	}
	recovered, err := rerun.Run(ctx, watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reported != 0 {
		t.Fatalf("recovery run result = %+v; want no new discoveries", recovered)
	}
	if len(fixed.intents) != 1 || fixed.intents[0].Detail.Count != 1 {
		t.Fatalf("notifications = %+v; want exactly one catch-up alert", fixed.messages)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, watched.ID); err != nil || len(unalerted) != 0 {
		t.Fatalf("unalerted entries = %+v, %v; want the receipt recorded", unalerted, err)
	}
}

// RecordDigest rewrites a title-derived work key to its canonical DOI when a
// later sighting supplies the stable identifier. Alert receipts keyed by the
// exact work key lose the earlier receipt and re-alert the same work. The
// unalerted query must match on identity aliases instead.
func TestUnalertedSurvivesTitleToDOICanonicalization(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("digest rename"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "the same work", Title: "The Same Work", Authors: "Ada", Year: 2025,
	}})
	if err != nil || reported != 1 {
		t.Fatalf("title-only RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	pending, err := watches.UnalertedDigestEntries(ctx, created.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unalerted after title record = %+v, %v; want one entry", pending, err)
	}
	if err := watches.MarkDigestAlerted(ctx, created.ID, []string{pending[0].WorkKey}, now); err != nil {
		t.Fatal(err)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID); err != nil || len(unalerted) != 0 {
		t.Fatalf("unalerted after alert = %+v, %v; want none", unalerted, err)
	}

	// The same work sighted again with its DOI merges into the existing row
	// and rewrites the canonical key. It must not become unalerted again.
	reported, err = watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
		WorkKey: "10.1000/the-same-work", Title: "The Same Work", Authors: "Ada", Year: 2025,
		DOI: "10.1000/the-same-work",
	}})
	if err != nil || reported != 0 {
		t.Fatalf("DOI RecordDigest() = %d, %v; want 0, nil", reported, err)
	}
	digest, err := watches.Digest(ctx, created.ID, 100)
	if err != nil || len(digest) != 1 || digest[0].WorkKey != "10.1000/the-same-work" {
		t.Fatalf("Digest() = %+v, %v; want one DOI-keyed entry", digest, err)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID); err != nil || len(unalerted) != 0 {
		t.Fatalf("unalerted after DOI merge = %+v, %v; want none, not a duplicate alert", unalerted, err)
	}

	// A genuinely new work still alerts.
	reported, err = watches.RecordDigest(ctx, created.ID, now.Add(2*time.Hour), []DigestEntry{{
		WorkKey: "10.1000/other-work", Title: "Other Work", Authors: "Bob", Year: 2025,
		DOI: "10.1000/other-work",
	}})
	if err != nil || reported != 1 {
		t.Fatalf("new-work RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID); err != nil || len(unalerted) != 1 {
		t.Fatalf("unalerted after new work = %+v, %v; want the new entry", unalerted, err)
	}
}

// A title-only receipt must not suppress a different identified work that
// happens to share the title. RecordDigest retains both rows when their
// stable IDs conflict, so matching on a bare title overlap would hide the
// second work forever. The receipt follows its own work to the DOI key on
// merge; the distinct work still alerts on its own stable identity.
func TestUnalertedDoesNotSuppressDistinctSameTitleDOIWorks(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("digest shared title"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "shared title", Title: "Shared Title", Authors: "Ada", Year: 2025,
	}})
	if err != nil || reported != 1 {
		t.Fatalf("title-only RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	pending, err := watches.UnalertedDigestEntries(ctx, created.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unalerted after title record = %+v, %v; want one entry", pending, err)
	}
	if err := watches.MarkDigestAlerted(ctx, created.ID, []string{pending[0].WorkKey}, now); err != nil {
		t.Fatal(err)
	}

	// The same work sighted with its DOI keeps its suppression: the title
	// receipt moves to the DOI key in the same transaction.
	reported, err = watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
		WorkKey: "10.1000/shared-a", Title: "Shared Title", Authors: "Ada", Year: 2025,
		DOI: "10.1000/shared-a",
	}})
	if err != nil || reported != 0 {
		t.Fatalf("DOI RecordDigest() = %d, %v; want 0, nil", reported, err)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID); err != nil || len(unalerted) != 0 {
		t.Fatalf("unalerted after DOI merge = %+v, %v; want none", unalerted, err)
	}

	// A different work with the same title but a conflicting DOI is a
	// separate row and must alert on its own.
	reported, err = watches.RecordDigest(ctx, created.ID, now.Add(2*time.Hour), []DigestEntry{{
		WorkKey: "10.1000/shared-b", Title: "Shared Title", Authors: "Bob", Year: 2025,
		DOI: "10.1000/shared-b",
	}})
	if err != nil || reported != 1 {
		t.Fatalf("distinct DOI RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID)
	if err != nil || len(unalerted) != 1 || unalerted[0].WorkKey != "10.1000/shared-b" {
		t.Fatalf("unalerted after distinct work = %+v, %v; want only 10.1000/shared-b", unalerted, err)
	}
}

// All receipts for one routed alert must commit atomically. Writing each row
// in its own statement leaves a crash between two inserts with a partial
// receipt set, and the next run re-alerts the entries still missing
// receipts. A validation failure partway through the batch must therefore
// leave no receipts behind.
func TestMarkDigestAlertedCommitsAtomically(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("digest atomic"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{
		{WorkKey: "10.1000/atomic-a", Title: "Atomic A", Authors: "Ada", Year: 2025, DOI: "10.1000/atomic-a"},
		{WorkKey: "10.1000/atomic-b", Title: "Atomic B", Authors: "Bob", Year: 2025, DOI: "10.1000/atomic-b"},
	})
	if err != nil || reported != 2 {
		t.Fatalf("RecordDigest() = %d, %v; want 2, nil", reported, err)
	}

	// One empty key in the middle must fail the whole batch, not record a
	// partial receipt for the first key.
	if err := watches.MarkDigestAlerted(ctx, created.ID, []string{"10.1000/atomic-a", "", "10.1000/atomic-b"}, now); err == nil {
		t.Fatal("MarkDigestAlerted() succeeded with an empty key; want an error")
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID); err != nil || len(unalerted) != 2 {
		t.Fatalf("unalerted after failed Mark = %+v, %v; want both entries, not a partial receipt", unalerted, err)
	}

	if err := watches.MarkDigestAlerted(ctx, created.ID, []string{"10.1000/atomic-a", "10.1000/atomic-b"}, now); err != nil {
		t.Fatal(err)
	}
	if unalerted, err := watches.UnalertedDigestEntries(ctx, created.ID); err != nil || len(unalerted) != 0 {
		t.Fatalf("unalerted after Mark = %+v, %v; want none", unalerted, err)
	}
}

// Consuming a renamed entry must clear the older receipt stored under its
// previous key as well; otherwise the orphan lingers and could suppress a
// different work that happens to share the old title.
func TestConsumeDigestClearsRenamedReceipt(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("digest consume rename"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "renamed work", Title: "Renamed Work", Authors: "Ada", Year: 2025,
	}}); err != nil {
		t.Fatal(err)
	}
	pending, err := watches.UnalertedDigestEntries(ctx, created.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unalerted = %+v, %v; want one entry", pending, err)
	}
	if err := watches.MarkDigestAlerted(ctx, created.ID, []string{pending[0].WorkKey}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
		WorkKey: "10.1000/renamed-work", Title: "Renamed Work", Authors: "Ada", Year: 2025,
		DOI: "10.1000/renamed-work",
	}}); err != nil {
		t.Fatal(err)
	}

	// Consume via the current canonical key.
	if consumed, err := watches.ConsumeDigest(ctx, created.ID, []string{"10.1000/renamed-work"}); err != nil || consumed != 1 {
		t.Fatalf("ConsumeDigest() = %d, %v; want 1, nil", consumed, err)
	}
	var receipts int
	if err := watches.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM watch_digest_alerts WHERE watch_id = ?`, created.ID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("alert receipts after consume = %d; want none, including the renamed title receipt", receipts)
	}
}

// A successful Route followed by a crash before MarkDigestAlerted leaves no
// receipt; when the next scan upgrades the title-derived work key to a DOI,
// the retry must still present the SAME notification identity so the ledger
// coalesces it instead of posting a duplicate. Identity hashes durable row
// IDs, which survive key rewrites, rather than the mutable work keys.
func TestRunnerAlertIdentityStableAcrossKeyUpgradeAfterLostReceipt(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "key upgrade", Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	discoveryFake := &fakeDiscovery{works: []discovery.DiscoveredWork{{
		Work:       work.Work{Title: "Shared Title", Authors: []string{"Ada"}, Year: 2025},
		OpenAlexID: "https://openalex.org/W9999991",
	}}}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}}
	notifier := &fakeNotifier{}
	runner := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Notifier: notifier,
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	if _, err := runner.Run(ctx, watched.ID); err != nil {
		t.Fatal(err)
	}
	if len(notifier.offered) != 1 {
		t.Fatalf("offered intents = %d; want the first alert", len(notifier.offered))
	}
	first := notifier.offered[0]

	// Simulate a crash after a delivered Route but before the receipt commit.
	if _, err := watches.S.DB().ExecContext(ctx,
		`DELETE FROM watch_digest_alerts WHERE watch_id = ?`, watched.ID); err != nil {
		t.Fatal(err)
	}

	// The next scan sees the same work with its DOI; RecordDigest merges it
	// into the same row under a new canonical key.
	discoveryFake.works = []discovery.DiscoveredWork{{
		Work:       work.Work{DOI: "10.1000/shared-title", Title: "Shared Title", Authors: []string{"Ada"}, Year: 2025},
		OpenAlexID: "https://openalex.org/W9999991",
	}}
	runStart := now.Add(24 * time.Hour)
	rerun := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Notifier: notifier,
		DataDir: runner.DataDir, Now: func() time.Time { return runStart },
	}
	recovered, err := rerun.Run(ctx, watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reported != 0 {
		t.Fatalf("recovery run result = %+v; want the upgrade merged, not reported anew", recovered)
	}
	if len(notifier.offered) != 2 {
		t.Fatalf("offered intents = %d; want the first alert and its retry", len(notifier.offered))
	}
	retry := notifier.offered[1]
	if retry.AggregateKey != first.AggregateKey || retry.ScanID != first.ScanID {
		t.Fatalf("retry identity = %q/%q, want the lost alert's %q/%q", retry.AggregateKey, retry.ScanID, first.AggregateKey, first.ScanID)
	}
	if !retry.WindowStart.Equal(first.WindowStart) || !retry.HappenedAt.Equal(first.HappenedAt) {
		t.Fatalf("retry window = %s/%s, want the lost alert's %s/%s", retry.WindowStart, retry.HappenedAt, first.WindowStart, first.HappenedAt)
	}
	if retry.Detail.Count != 1 {
		t.Fatalf("retry count = %d; want the single upgraded work", retry.Detail.Count)
	}

	// A distinct work sharing the title but carrying a conflicting DOI and
	// OpenAlex ID is a different row and must alert under its own identity.
	discoveryFake.works = []discovery.DiscoveredWork{{
		Work:       work.Work{DOI: "10.1000/shared-other", Title: "Shared Title", Authors: []string{"Bob"}, Year: 2025},
		OpenAlexID: "https://openalex.org/W9999992",
	}}
	lookup.result = &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}
	runStart = now.Add(48 * time.Hour)
	if _, err := rerun.Run(ctx, watched.ID); err != nil {
		t.Fatal(err)
	}
	if len(notifier.offered) != 3 {
		t.Fatalf("offered intents = %d; want a third alert for the distinct work", len(notifier.offered))
	}
	fresh := notifier.offered[2]
	if fresh.AggregateKey == retry.AggregateKey {
		t.Fatalf("distinct work reused the delivered alert identity %q", fresh.AggregateKey)
	}
	if fresh.Detail.Count != 1 {
		t.Fatalf("new alert count = %d; want only the distinct work", fresh.Detail.Count)
	}
}
