// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"papio/internal/ipc"
	"papio/internal/ownership"
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
		result.(*jobDetail).Job = json.RawMessage(`{"id":"job-x","state":"queued"}`)
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
		result.(*jobDetail).Job = json.RawMessage(`{"id":"job-x","state":"queued"}`)
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
	return nil
}

type fingerprintBatchCaller struct {
	mu                  sync.Mutex
	expectedFingerprint string
	lookupErr           error
	methods             []string
}

func (c *fingerprintBatchCaller) Call(_ context.Context, method string, params, result any) error {
	c.mu.Lock()
	c.methods = append(c.methods, method)
	c.mu.Unlock()

	switch method {
	case "library.lookup_works":
		request := params.(libraryLookupParams)
		if request.ExpectedFingerprint != c.expectedFingerprint {
			return fmt.Errorf("expected fingerprint = %q, got %q", c.expectedFingerprint, request.ExpectedFingerprint)
		}
		if c.lookupErr != nil {
			return c.lookupErr
		}
		result.(*ownership.Result).Works = make([]ownership.WorkResult, len(request.Works))
	case "zotio.lookup_works":
		request := params.(zotio.LookupWorksRequest)
		result.(*zotio.LookupWorksResult).Works = make([]zotio.WorkOwnership, len(request.Works))
		for i := range request.Works {
			result.(*zotio.LookupWorksResult).Works[i].Status = zotio.OwnershipNotOwned
		}
	case "acquire.submit":
		result.(*submitResult).JobID = "job-fingerprint"
	case "jobs.get":
		result.(*jobDetail).Job = json.RawMessage(`{"id":"job-fingerprint","state":"queued"}`)
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
	return nil
}

func (c *fingerprintBatchCaller) called(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, called := range c.methods {
		if called == method {
			return true
		}
	}
	return false
}

func TestSubmitBindsHoldingsLookupToFingerprint(t *testing.T) {
	caller := &fingerprintBatchCaller{expectedFingerprint: "library-fingerprint"}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{doiWork("batch-fingerprint", "10.1000/fingerprint")},
		SubmitOptions{Holdings: true, LibraryFingerprint: "library-fingerprint"})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Submitted) != 1 {
		t.Fatalf("Submitted = %+v, want one job", output.Submitted)
	}
	if !caller.called("library.lookup_works") {
		t.Fatal("generic holdings lookup was not called")
	}
}

func TestSubmitDoesNotFallbackOnLibraryPreconditionFailure(t *testing.T) {
	caller := &fingerprintBatchCaller{
		expectedFingerprint: "library-fingerprint",
		lookupErr:           &ipc.RemoteError{Code: "precondition_failed", Message: "library configuration does not match caller"},
	}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{doiWork("batch-fingerprint-mismatch", "10.1000/fingerprint-mismatch")},
		SubmitOptions{Holdings: true, LibraryFingerprint: "library-fingerprint"})
	if err == nil {
		t.Fatal("library precondition failure must stop the batch")
	}
	if output != nil {
		t.Fatalf("output = %+v, want nil after precondition failure", output)
	}
	if caller.called("zotio.lookup_works") {
		t.Fatal("a library precondition failure must not use the unknown-method fallback")
	}
	if caller.called("acquire.submit") {
		t.Fatal("a library precondition failure must not create jobs")
	}
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
