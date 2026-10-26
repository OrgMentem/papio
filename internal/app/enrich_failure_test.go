// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/enrich"
	"papio/internal/job"
	"papio/internal/protocol"
)

func TestTitleLookupFailureParksForRetry(t *testing.T) {
	for _, source := range []string{config.SourceCrossrefMetadata, config.SourceOpenAlex} {
		for _, failure := range []string{"truncated_body", "forbidden"} {
			t.Run(source+"/"+failure, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if failure == "forbidden" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Length", "1000")
					_, _ = io.WriteString(w, `{"message":`)
				}))
				defer server.Close()
				svc, jobs := newTestService(t)
				readyPipeline(svc)
				svc.Resolvers = nil // Only title lookup can supply an acquisition candidate.
				svc.Config.Sources[source] = config.Source{Enabled: true}
				entry := MetadataEnricherEntry{Name: source, Enricher: enrich.NewWithOptions(enrich.Options{BaseURL: server.URL})}
				if source == config.SourceOpenAlex {
					entry.Enricher = enrich.NewOpenAlexWithOptions(enrich.OpenAlexOptions{BaseURL: server.URL, ContactEmail: "test@example.org"})
				}
				svc.MetadataEnrichers = []MetadataEnricherEntry{entry, {Name: "fixture", Enricher: &fakeEnricher{}}}
				ctx := context.Background()
				id, err := svc.Submit(ctx, protocol.WorkRequest{
					SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_title_lookup_failure",
					Title: "A paper", Authors: []string{"Smith"}, Year: 2024,
				})
				if err != nil {
					t.Fatal(err)
				}
				row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
				if err != nil || row == nil {
					t.Fatalf("claim = %+v, %v", row, err)
				}
				if err := svc.Process(ctx, row); err != nil {
					t.Fatal(err)
				}
				after, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != job.StateRetryWait {
					t.Fatalf("state = %s, want retry_wait after failed title lookup", after.State)
				}
				if reason := lastTransitionReason(t, jobs, id); reason != "resolver_temporarily_unavailable" {
					t.Fatalf("retry reason = %q", reason)
				}
			})
		}
	}
}
