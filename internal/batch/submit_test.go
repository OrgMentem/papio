// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
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
