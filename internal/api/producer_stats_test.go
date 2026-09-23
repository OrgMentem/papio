// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
)

func TestStoreHandlerProducerStatsCountsRecordedPromotions(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	since := time.Now().Add(-time.Minute).UTC()
	for i, record := range []map[string]any{
		{"producer": "adapter", "interventions": []string{"sign_in"}, "opened_by": job.PacerPrincipal},
		{"producer": "unknown", "interventions": []string{"open", "sign_in"}, "opened_by": "cli"},
	} {
		jobID := storeHandlerHandoffJob(t, system, []string{"wr_producer_stats_a", "wr_producer_stats_b"}[i])
		if err := system.Jobs.RecordEvent(ctx, jobID, job.ArtifactProducerEvent, record); err != nil {
			t.Fatal(err)
		}
	}
	var stats job.ProducerStats
	if rpcErr := callMethod(t, Router(system), "stats.producers_v1",
		map[string]any{"since": since.Format(time.RFC3339Nano)}, &stats); rpcErr != nil {
		t.Fatalf("stats.producers_v1 = %+v", rpcErr)
	}
	if stats.Acquired != 2 || stats.SignInOnly != 1 || stats.Intervened != 1 ||
		stats.Producers[job.ProducerAdapter] != 1 || stats.Producers[job.ProducerUnknown] != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestStoreHandlerProducerStatsRejectsBadPeriods(t *testing.T) {
	router := Router(testSystem(t))
	now := time.Now().UTC()
	for _, params := range []map[string]any{
		{},
		{"since": "yesterday"},
		{"since": now.Format(time.RFC3339Nano), "until": now.Add(-time.Hour).Format(time.RFC3339Nano)},
		{"since": now.Format(time.RFC3339Nano), "limit": 1},
	} {
		if rpcErr := callMethod(t, router, "stats.producers_v1", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("stats.producers_v1 %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}
