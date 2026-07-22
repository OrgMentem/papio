// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"papio/internal/protocol"
	"papio/internal/zotio"
	"reflect"
	"strings"
	"testing"
)

const batchJSONLFixture = `{"doi":"10.1000/Bare","title":"Bare work","authors":["Ada"],"year":2024,"desired_version":"published"}
{"work":{"arxiv":"arXiv:2601.12345v2","title":"Discovered work","authors":["Grace"],"year":2025}}
`

const batchRISFixture = "\ufeff\r\nTY  - JOUR\r\nTI  - First work\r\nAU  - Ada Lovelace\r\nPY  - 2024\r\nDO  - https://doi.org/10.1000/First\r\nER  - \r\n\r\nTY  - JOUR\r\nTI  - Second work\r\nAU  - Grace Hopper\r\nPY  - 2025\r\nDO  - doi:10.1000/Second\r\nER  - \r\n\r\n"

func TestParseBatchAcceptsBareAndDiscoveredWork(t *testing.T) {
	requests, err := parseBatch(strings.NewReader(batchJSONLFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("parsed %d requests, want 2", len(requests))
	}
	if requests[0].Identifiers == nil || requests[0].Identifiers.DOI != "10.1000/bare" || requests[0].DesiredVersion != "published" {
		t.Fatalf("bare request = %+v", requests[0])
	}
	if requests[1].Identifiers == nil || requests[1].Identifiers.ArXiv != "2601.12345v2" || requests[1].DesiredVersion != "any" {
		t.Fatalf("enveloped request = %+v", requests[1])
	}
	if !strings.HasPrefix(requests[0].RequestID, "batch-") || requests[0].RequestID != batchRequestID(requests[0].Identifiers, requests[0].Title, requests[0].Authors, requests[0].Year) {
		t.Fatalf("bare request ID = %q", requests[0].RequestID)
	}
}

func TestParseBatchAcceptsSearchJSONArray(t *testing.T) {
	requests, err := parseBatch(strings.NewReader(`[{"work":{"doi":"10.1000/one"}},{"work":{"doi":"10.1000/two"}}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Identifiers.DOI != "10.1000/one" || requests[1].Identifiers.DOI != "10.1000/two" {
		t.Fatalf("requests = %+v", requests)
	}
}

func TestParseBatchReportsBadLine(t *testing.T) {
	_, err := parseBatch(strings.NewReader("{\"doi\":\"10.1000/valid\"}\nnot-json\n"))
	if err == nil || !strings.Contains(err.Error(), "batch line 2") {
		t.Fatalf("error = %v, want line number", err)
	}
}

func TestParseBatchRejectsMoreThanFiftyWorks(t *testing.T) {
	line := "{\"doi\":\"10.1000/repeated\"}\n"
	_, err := parseBatch(strings.NewReader(strings.Repeat(line, 51)))
	if err == nil || !strings.Contains(err.Error(), "maximum of 50") {
		t.Fatalf("error = %v, want batch size rejection", err)
	}
}

func TestParseBatchInputRIS(t *testing.T) {
	requests, err := parseBatchInput("references.ris", strings.NewReader(batchRISFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("parsed %d requests, want 2", len(requests))
	}
	for i, wantDOI := range []string{"10.1000/first", "10.1000/second"} {
		if requests[i].Identifiers == nil || requests[i].Identifiers.DOI != wantDOI {
			t.Fatalf("request %d = %+v, want DOI %q", i, requests[i], wantDOI)
		}
		if wantID := batchRequestID(requests[i].Identifiers, requests[i].Title, requests[i].Authors, requests[i].Year); requests[i].RequestID != wantID {
			t.Fatalf("request %d ID = %q, want %q", i, requests[i].RequestID, wantID)
		}
	}
}

func TestParseBatchInputJSONLMatchesParseBatch(t *testing.T) {
	want, err := parseBatch(strings.NewReader(batchJSONLFixture))
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBatchInput("works.jsonl", strings.NewReader(batchJSONLFixture))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %+v, want %+v", got, want)
	}
}

func TestParseBatchInputSniffsRISFromStandardInput(t *testing.T) {
	want, err := parseBatchInput("references.ris", strings.NewReader(batchRISFixture))
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBatchInput("-", strings.NewReader(batchRISFixture))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %+v, want %+v", got, want)
	}
}

func TestParseBatchInputRejectsMoreThanFiftyRISWorks(t *testing.T) {
	var input strings.Builder
	for i := range 51 {
		fmt.Fprintf(&input, "TY  - JOUR\nDO  - 10.1000/cap-%d\nER  - \n", i)
	}
	_, err := parseBatchInput("references.ris", strings.NewReader(input.String()))
	if err == nil || err.Error() != "batch exceeds maximum of 50 works" {
		t.Fatalf("error = %v, want batch size rejection", err)
	}
}

func TestParseBatchInputRejectsOversizeInput(t *testing.T) {
	_, err := parseBatchInput("works.jsonl", bytes.NewReader(make([]byte, 8<<20+1)))
	if err == nil || err.Error() != "batch input exceeds 8 MiB" {
		t.Fatalf("error = %v, want input size rejection", err)
	}
}

func TestApplyBatchOwnershipSkipsOwnedCopiesAndPinsMissingPDF(t *testing.T) {
	requests := []protocol.WorkRequest{
		{RequestID: "new"},
		{RequestID: "complete"},
		{RequestID: "missing"},
	}
	ownership := zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
		{Status: zotio.OwnershipNotOwned},
		{Status: zotio.OwnershipOwnedWithPDF, ItemKey: "PDF00001"},
		{Status: zotio.OwnershipOwnedMissingPDF, ItemKey: "MISS0001"},
	}}

	pending, skipped, err := applyBatchOwnership(requests, ownership, " Reading ", false)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 || len(pending) != 2 {
		t.Fatalf("pending=%+v skipped=%d", pending, skipped)
	}
	if pending[0].Collection != "Reading" || pending[1].Collection != "Reading" || pending[1].ZotioItemKey != "MISS0001" {
		t.Fatalf("batch routes = %+v", pending)
	}

	included, skipped, err := applyBatchOwnership(requests, ownership, "Reading", true)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(included) != 3 || included[2].ZotioItemKey != "MISS0001" {
		t.Fatalf("include-owned routes=%+v skipped=%d", included, skipped)
	}
}

func TestBatchAcceptsResolverFlag(t *testing.T) {
	command := newAcquireCommand(&options{})
	if err := command.Flags().Set("resolver", "institute"); err != nil {
		t.Fatal(err)
	}
	if err := validateBatchFlags(command, nil, false, false); err != nil {
		t.Fatalf("--batch rejected --resolver: %v", err)
	}
}
