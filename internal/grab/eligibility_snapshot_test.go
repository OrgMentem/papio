// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package grab

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

func TestEligibilitySnapshotByteBound(t *testing.T) {
	entries := make([]EligibilitySnapshotEntry, 0, 2500)
	for i := 0; i < 2500; i++ {
		entries = append(entries, EligibilitySnapshotEntry{
			JobID: "job-" + strings.Repeat("x", 8) + "-" + strings.Repeat("y", i%10),
			Work: EligibilitySnapshotWork{
				Title: strings.Repeat("z", 300),
			},
			BoundDOIs: []string{"10.1234/example." + strings.Repeat("a", 20)},
		})
	}
	snap := EligibilityPoolSnapshot{
		Schema:     EligibilityPoolSnapshotSchema,
		RecordedAt: store.Now(),
		Phase:      SnapshotPhasePreBind,
		PoolSize:   len(entries),
		Entries:    entries,
	}
	_, err := marshalEligibilityPoolSnapshot(snap)
	if !errors.Is(err, ErrEligibilitySnapshotTooLarge) {
		t.Fatalf("marshal = %v, want ErrEligibilitySnapshotTooLarge", err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, nil)
	g, err := svc.Allocate(ctx, "pdf.example.org", "oversize snapshot")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := RecordEligibilitySnapshotTx(ctx, tx, g.ID, SnapshotPhasePreBind, snap); !errors.Is(err, ErrEligibilitySnapshotTooLarge) {
		t.Fatalf("RecordEligibilitySnapshotTx = %v, want ErrEligibilitySnapshotTooLarge", err)
	}
}

// The snapshot insert precedes the state CAS in one transaction. A refused
// transition must roll the snapshot back too: the table is keyed by grab_id,
// so an orphan row would block that grab's later valid settlement.
func TestParkWithEligibilitySnapshotIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, nil)
	snap := EligibilityPoolSnapshot{
		Schema: EligibilityPoolSnapshotSchema, Phase: SnapshotPhasePreBind, PoolSize: 1,
		Entries: []EligibilitySnapshotEntry{{JobID: "job-one", Work: EligibilitySnapshotWork{Title: "One"}}},
	}

	refused, err := svc.Allocate(ctx, "pdf.example.org", "already failed")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkFailedValidation(ctx, refused.ID, "not a pdf"); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkParkedNoIdentifierWithEligibilitySnapshot(ctx, refused.ID, snap); err == nil {
		t.Fatal("parking a terminal grab succeeded")
	}
	if _, err := svc.EligibilitySnapshot(ctx, refused.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("refused park left a snapshot behind: %v", err)
	}
	if g, err := svc.Get(ctx, refused.ID); err != nil || g.State != StateFailedValidation {
		t.Fatalf("refused park changed the grab: %+v %v", g, err)
	}

	parked, err := svc.Allocate(ctx, "pdf.example.org", "parks cleanly")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkParkedNoIdentifierWithEligibilitySnapshot(ctx, parked.ID, snap); err != nil {
		t.Fatalf("park eligible grab: %v", err)
	}
	if g, err := svc.Get(ctx, parked.ID); err != nil || g.State != StateParkedNoIdentifier {
		t.Fatalf("parked grab = %+v %v", g, err)
	}
	got, err := svc.EligibilitySnapshot(ctx, parked.ID)
	if err != nil || got.PoolSize != 1 || len(got.Entries) != 1 || got.Entries[0].JobID != "job-one" {
		t.Fatalf("stored snapshot = %+v %v", got, err)
	}
	var rows int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pdf_grab_eligibility_snapshots`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("snapshot rows = %d %v, want exactly the parked grab's", rows, err)
	}
}
