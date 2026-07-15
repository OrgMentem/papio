// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"papio/internal/protocol"
)

type fakeCLI struct {
	collection string
	limit      int
	items      []MissingPDFItem
	details    map[string]*Item
	find       map[string]json.RawMessage
	syncErr    error
	syncCalls  int
}

func (f *fakeCLI) Preflight(context.Context) (*PreflightResult, error) {
	return &PreflightResult{Executable: "zotio", Version: "1.0.0"}, nil
}
func (f *fakeCLI) MissingPDF(_ context.Context, collection string, limit int) ([]MissingPDFItem, error) {
	f.collection, f.limit = collection, limit
	return append([]MissingPDFItem(nil), f.items...), nil
}
func (f *fakeCLI) GetItem(_ context.Context, key string) (*Item, error) {
	item, ok := f.details[key]
	if !ok {
		return nil, fmt.Errorf("missing item %s", key)
	}
	copy := *item
	return &copy, nil
}
func (f *fakeCLI) Sync(context.Context) error {
	f.syncCalls++
	return f.syncErr
}
func (f *fakeCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	if len(args) >= 5 && strings.Join(args[:3], " ") == "--agent items find" {
		key := strings.TrimPrefix(args[3], "--") + ":" + args[4]
		if raw := f.find[key]; raw != nil {
			return raw, nil
		}
		return json.RawMessage("[]"), nil
	}
	return nil, fmt.Errorf("unexpected RunJSON %q", args)
}

type fakeSubmitter struct {
	requests []protocol.WorkRequest
}

func (f *fakeSubmitter) Submit(_ context.Context, request protocol.WorkRequest) (string, error) {
	f.requests = append(f.requests, request)
	return "job_" + request.ZotioItemKey, nil
}

func TestQueueMissingPDFSubmitsDeterministicRequestsAndSkipsUnidentified(t *testing.T) {
	cli := &fakeCLI{
		items: []MissingPDFItem{
			{Key: "AB12CD34", Title: "DOI paper", DOI: "https://doi.org/10.1000/One"},
			{Key: "EF56GH78", Title: "Tuple paper"},
			{Key: "JK90LM12", Title: "Incomplete"},
		},
		details: map[string]*Item{
			"EF56GH78": {Key: "EF56GH78", Title: "Tuple paper", Authors: []string{"Ada Lovelace"}, Year: 2024},
			"JK90LM12": {Key: "JK90LM12", Title: "Incomplete"},
		},
	}
	submitter := &fakeSubmitter{}
	service := &Service{CLI: cli, Submitter: submitter}
	maxCost := 2.5
	result, err := service.QueueMissingPDF(context.Background(), QueueOptions{
		Collection:     "ZX98YU76",
		Limit:          12,
		DesiredVersion: "published",
		MaxCostUSD:     &maxCost,
		SourcesDeny:    []string{" unpaywall "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cli.collection != "ZX98YU76" || cli.limit != 12 {
		t.Fatalf("queue args = (%q,%d)", cli.collection, cli.limit)
	}
	if len(result.Queued) != 2 || len(result.Skipped) != 1 || len(submitter.requests) != 2 {
		t.Fatalf("result = %+v requests=%+v", result, submitter.requests)
	}
	first := submitter.requests[0]
	if first.RequestID != "request_zotio_AB12CD34" || first.ZotioItemKey != "AB12CD34" || first.Identifiers == nil || first.Identifiers.DOI != "10.1000/one" {
		t.Fatalf("DOI request = %+v", first)
	}
	second := submitter.requests[1]
	if second.RequestID != "request_zotio_EF56GH78" || second.Title != "Tuple paper" || second.Year != 2024 || len(second.Authors) != 1 {
		t.Fatalf("tuple request = %+v", second)
	}
	if second.DesiredVersion != "published" || second.MaxCostUSD == nil || *second.MaxCostUSD != 2.5 || len(second.SourcesDeny) != 1 || second.SourcesDeny[0] != "unpaywall" {
		t.Fatalf("policy propagation = %+v", second)
	}
	if result.Skipped[0].ZotioItemKey != "JK90LM12" || result.Skipped[0].Reason == "" {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

func TestQueueMissingPDFDefaultsLimitAndDesiredVersion(t *testing.T) {
	cli := &fakeCLI{}
	service := &Service{CLI: cli, Submitter: &fakeSubmitter{}}
	result, err := service.QueueMissingPDF(context.Background(), QueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cli.limit != 25 || result.Preflight.Version != "1.0.0" {
		t.Fatalf("limit=%d preflight=%+v", cli.limit, result.Preflight)
	}
}

func TestLookupWorksClassifiesOwnedPDFMissingPDFAndNewWork(t *testing.T) {
	cli := &fakeCLI{
		items: []MissingPDFItem{{Key: "MISS0001"}},
		find: map[string]json.RawMessage{
			"doi:10.1000/with-pdf": json.RawMessage(`[{"key":"PDF00001","data":{}}]`),
			"doi:10.1000/missing":  json.RawMessage(`[{"key":"MISS0001","data":{}}]`),
			"arxiv:2601.12345v2":   json.RawMessage(`[]`),
		},
		syncErr: errors.New("offline"),
	}
	result, err := (&Service{CLI: cli}).LookupWorks(context.Background(), LookupWorksRequest{Works: []LookupWork{
		{DOI: "10.1000/with-pdf"},
		{DOI: "10.1000/missing"},
		{ArXiv: "arXiv:2601.12345v2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cli.syncCalls != 1 || cli.collection != "" || cli.limit != 500 {
		t.Fatalf("mirror calls = sync:%d collection:%q limit:%d", cli.syncCalls, cli.collection, cli.limit)
	}
	if result.StalenessWarning == "" {
		t.Fatal("missing staleness warning after failed sync")
	}
	want := []WorkOwnership{
		{Status: OwnershipOwnedWithPDF, ItemKey: "PDF00001"},
		{Status: OwnershipOwnedMissingPDF, ItemKey: "MISS0001"},
		{Status: OwnershipNotOwned},
	}
	if len(result.Works) != len(want) {
		t.Fatalf("works = %+v", result.Works)
	}
	for i := range want {
		if result.Works[i] != want[i] {
			t.Fatalf("work %d = %+v, want %+v", i, result.Works[i], want[i])
		}
	}
}
