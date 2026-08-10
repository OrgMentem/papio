// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"testing"

	"papio/internal/config"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/work"
)

func TestOpenAlexEnrichmentSourceDisabledSkipsEntirely(t *testing.T) {
	svc, jobs := newTestService(t)
	openAlex := &fakeEnricher{result: work.Work{Title: "A title", OpenAlex: "W123"}, matched: true}
	svc.Config.Sources[config.SourceOpenAlex] = config.Source{Enabled: false}
	svc.MetadataEnrichers = []MetadataEnricherEntry{{Name: config.SourceOpenAlex, Enricher: openAlex}}

	id, err := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_openalex_disabled_01", Title: "A title", Authors: []string{"Author"}, Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.enrich(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if openAlex.calls != 0 {
		t.Fatalf("OpenAlex calls = %d, want 0 while source is disabled", openAlex.calls)
	}
}

func TestEnrichmentAPIErrorDegradesToOpenAlex(t *testing.T) {
	svc, jobs := newTestService(t)
	crossref := &fakeEnricher{err: &resolver.TemporaryError{Err: errors.New("Crossref unavailable")}}
	openAlex := &fakeEnricher{result: work.Work{
		Title: "Evaluating training programs: the four levels", OpenAlex: "W12345", DOI: "10.1234/book",
	}, matched: true}
	adapter := &fakeResolver{name: "fixture"}
	svc.Enricher = crossref
	svc.Config.Sources[config.SourceOpenAlex] = config.Source{Enabled: true}
	svc.MetadataEnrichers = []MetadataEnricherEntry{
		{Name: config.SourceCrossrefMetadata, Enricher: crossref},
		{Name: config.SourceOpenAlex, Enricher: openAlex},
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	id, err := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_openalex_fallback_01",
		Title: "Evaluating training programs: the four levels", Authors: []string{"Kirkpatrick"}, Year: 2006,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolve(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if crossref.calls != 1 || openAlex.calls != 1 {
		t.Fatalf("enricher calls = crossref %d, OpenAlex %d; want both sources attempted", crossref.calls, openAlex.calls)
	}
	if adapter.calls != 1 || len(adapter.requested) != 1 {
		t.Fatalf("resolver calls = %d; want one same-pass resolver call", adapter.calls)
	}
	if adapter.requested[0].OpenAlex != "W12345" || adapter.requested[0].DOI != "10.1234/book" {
		t.Fatalf("resolver received %+v; want OpenAlex rescue identifiers", adapter.requested[0])
	}
}
