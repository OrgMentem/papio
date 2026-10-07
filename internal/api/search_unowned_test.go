// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/discovery"
	"papio/internal/work"
	"papio/internal/zotio"
)

type unidentifiedDiscovery struct{ work work.Work }

func (s unidentifiedDiscovery) Name() string { return "fixture" }
func (s unidentifiedDiscovery) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return []discovery.DiscoveredWork{{Work: s.work}}, nil
}
func (s unidentifiedDiscovery) SearchPage(ctx context.Context, params discovery.SearchParams, _ string) (discovery.Page, error) {
	rows, err := s.Search(ctx, params)
	return discovery.Page{Works: rows, State: discovery.PageExhausted}, err
}

type syncedOwnershipCLI struct{ zotio.CLI }

func (syncedOwnershipCLI) Sync(context.Context) error { return nil }

func TestSearchUnownedRejectsUnsupportedZotioIdentity(t *testing.T) {
	for _, candidate := range []work.Work{
		{Title: "A title is not ownership evidence", Year: 2026},
		{Title: "Invalid ISBN is not ownership evidence", ISBN: "invalid"},
		{Title: "Whitespace is not ownership evidence", DOI: " \t"},
	} {
		t.Run(candidate.Title, func(t *testing.T) {
			system := &bootstrap.System{
				Discovery: unidentifiedDiscovery{work: candidate},
				Zotio:     &zotio.Service{CLI: syncedOwnershipCLI{}},
			}
			raw, err := json.Marshal(discovery.UnownedRequest{Search: discovery.SearchParams{Query: "fixture", Limit: 10}})
			if err != nil {
				t.Fatal(err)
			}
			data, rpcErr := searchUnowned(context.Background(), raw, system)
			if rpcErr == nil || rpcErr.Code != "precondition_failed" || len(data) != 0 {
				t.Fatalf("unchecked work entered new-only results: data=%s error=%+v", data, rpcErr)
			}
		})
	}
}
