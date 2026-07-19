// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"papio/internal/protocol"
	"papio/internal/zotio"
)

type resolverBatchCaller struct {
	t        *testing.T
	resolver string
}

func (c resolverBatchCaller) Call(_ context.Context, method string, params, result any) error {
	switch method {
	case "zotio.lookup_works":
		request := params.(zotio.LookupWorksRequest)
		result.(*zotio.LookupWorksResult).Works = make([]zotio.WorkOwnership, len(request.Works))
		for i := range request.Works {
			result.(*zotio.LookupWorksResult).Works[i].Status = zotio.OwnershipNotOwned
		}
	case "acquire.submit":
		request := params.(protocol.WorkRequest)
		if request.Resolver != c.resolver {
			c.t.Errorf("resolver = %q, want %q", request.Resolver, c.resolver)
		}
		result.(*submitResult).JobID = "job-resolver-profile"
	case "jobs.get":
		result.(*jobDetail).Job = &struct {
			State string `json:"state"`
		}{State: "queued"}
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
	return nil
}

func TestSubmitAppliesResolverProfileToEveryBatchRequest(t *testing.T) {
	request := protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      "batch-resolver-request",
		Identifiers:    &protocol.Identifiers{DOI: "10.1000/resolver"},
		DesiredVersion: "any",
	}
	output, err := Submit(context.Background(), resolverBatchCaller{t: t, resolver: "institute"}, t.TempDir(), []protocol.WorkRequest{request}, SubmitOptions{Resolver: "institute"})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].State != "queued" {
		t.Fatalf("output = %+v", output)
	}
}

type collectionBatchCaller struct {
	mu          sync.Mutex
	collections []string
}

func (c *collectionBatchCaller) Call(_ context.Context, method string, params, result any) error {
	switch method {
	case "zotio.lookup_works":
		request := params.(zotio.LookupWorksRequest)
		result.(*zotio.LookupWorksResult).Works = make([]zotio.WorkOwnership, len(request.Works))
		for i := range request.Works {
			result.(*zotio.LookupWorksResult).Works[i].Status = zotio.OwnershipNotOwned
		}
	case "acquire.submit":
		request := params.(protocol.WorkRequest)
		c.mu.Lock()
		c.collections = append(c.collections, request.Collection)
		c.mu.Unlock()
		result.(*submitResult).JobID = "job-collection-default"
	case "jobs.get":
		result.(*jobDetail).Job = &struct {
			State string `json:"state"`
		}{State: "queued"}
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
	return nil
}

func doiWork(requestID, doi string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      requestID,
		Identifiers:    &protocol.Identifiers{DOI: doi},
		DesiredVersion: "any",
	}
}

func TestSubmitDefaultsCollectionToLabelWhenUnset(t *testing.T) {
	caller := &collectionBatchCaller{}
	work := doiWork("batch-collection-default", "10.1000/collection")
	if _, err := Submit(context.Background(), caller, t.TempDir(), []protocol.WorkRequest{work}, SubmitOptions{Label: "evidence synthesis"}); err != nil {
		t.Fatal(err)
	}
	if len(caller.collections) != 1 || caller.collections[0] != "evidence synthesis" {
		t.Fatalf("submitted collections = %q, want [evidence synthesis]", caller.collections)
	}
}

func TestSubmitKeepsExplicitCollectionOverLabel(t *testing.T) {
	caller := &collectionBatchCaller{}
	work := doiWork("batch-collection-explicit", "10.1000/explicit")
	if _, err := Submit(context.Background(), caller, t.TempDir(), []protocol.WorkRequest{work}, SubmitOptions{Label: "evidence synthesis", Collection: "Reading"}); err != nil {
		t.Fatal(err)
	}
	if len(caller.collections) != 1 || caller.collections[0] != "Reading" {
		t.Fatalf("submitted collections = %q, want [Reading]", caller.collections)
	}
}

func TestParseWorkRejectsUnknownFields(t *testing.T) {
	for _, data := range []string{
		`{"doi":"10.1000/example","DOIs":["10.1000/typo"]}`,
		`{"work":{"doi":"10.1000/example","author":"Ada"}}`,
		`{"work":{"doi":"10.1000/example"},"typo":true}`,
	} {
		if _, err := ParseWork([]byte(data)); err == nil {
			t.Fatalf("ParseWork(%s) accepted an unknown field", data)
		}
	}
}

func TestParseWorkAcceptsDiscoveredWorkEnvelope(t *testing.T) {
	request, err := ParseWork([]byte(`{"work":{"doi":"10.1000/example","container":"Journal"},"openalex_id":"W12345","is_oa":true,"oa_url":"https://example.test/paper","cited_by":1,"abstract":"Summary","owned":false,"owned_item_key":"AB12CD34"}`))
	if err != nil {
		t.Fatalf("ParseWork discovered envelope: %v", err)
	}
	if request.Identifiers == nil || request.Identifiers.DOI != "10.1000/example" {
		t.Fatalf("request identifiers = %#v", request.Identifiers)
	}
}

func TestBatchRequestIDSeparatesLegacyPrefixCollision(t *testing.T) {
	const first = "10.1000/collision-11784"
	const second = "10.1000/collision-77155"

	firstSum := sha256.Sum256([]byte("doi:" + first))
	secondSum := sha256.Sum256([]byte("doi:" + second))
	if string(firstSum[:4]) != string(secondSum[:4]) {
		t.Fatal("test inputs must collide on the legacy four-byte hash prefix")
	}

	firstID := batchRequestID(&protocol.Identifiers{DOI: first}, "", nil, 0)
	secondID := batchRequestID(&protocol.Identifiers{DOI: second}, "", nil, 0)
	if firstID == secondID {
		t.Fatalf("batch request IDs collided: %q", firstID)
	}
	if len(firstID) != len("batch-")+batchIdentityHashBytes*2 {
		t.Fatalf("batch request ID length = %d, want %d", len(firstID), len("batch-")+batchIdentityHashBytes*2)
	}
}
