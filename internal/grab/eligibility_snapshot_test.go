// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package grab

import (
	"context"
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
