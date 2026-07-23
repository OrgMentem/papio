// Copyright 2026 OrgMentem. Licensed under MIT.

package job

import (
	"context"
	"testing"

	"papio/internal/work"
)

func TestEnrichWorkRequestMetadataOnlyFillsEmptyFields(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "wr_enrich_store_01", work.Work{DOI: "10.1002/example"}, "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := js.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := js.EnrichWorkRequestMetadata(ctx, row.WorkRequestID, "Discovered title", []string{"Ada Lovelace"}, 2024)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first enrichment reported unchanged")
	}
	changed, err = js.EnrichWorkRequestMetadata(ctx, row.WorkRequestID, "Replacement title", []string{"Grace Hopper"}, 2025)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("request-supplied metadata was overwritten")
	}
	persisted, err := js.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Work.Title != "Discovered title" || len(persisted.Work.Authors) != 1 ||
		persisted.Work.Authors[0] != "Ada Lovelace" || persisted.Work.Year != 2024 {
		t.Fatalf("persisted work = %+v", persisted.Work)
	}
}
