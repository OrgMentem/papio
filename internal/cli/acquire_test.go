// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

func TestParseBatchAcceptsBareAndDiscoveredWork(t *testing.T) {
	requests, err := parseBatch(strings.NewReader(`{"doi":"10.1000/Bare","title":"Bare work","authors":["Ada"],"year":2024,"desired_version":"published"}
{"work":{"arxiv":"arXiv:2601.12345v2","title":"Discovered work","authors":["Grace"],"year":2025}}
`))
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
