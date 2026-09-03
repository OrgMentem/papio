// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"strings"
	"testing"

	"papio/internal/ipc"
	"papio/internal/ownership"
	"papio/internal/protocol"
)

// holdingsCaller answers library.lookup_works with a canned result and records
// which methods the batch path reached.
type holdingsCaller struct {
	baseBatchCaller
	methodRecorder
	result   ownership.Result
	unknown  bool
	submitID string
}

func (c *holdingsCaller) Call(ctx context.Context, method string, params, result any) error {
	c.record(method)
	switch method {
	case "library.lookup_works":
		if c.unknown {
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		}
		request := params.(libraryLookupParams)
		out := result.(*ownership.Result)
		*out = c.result
		if len(out.Works) == 0 {
			out.Works = make([]ownership.WorkResult, len(request.Works))
		}
	case "acquire.submit_v2":
		id := c.submitID
		if id == "" {
			id = "job-holdings"
		}
		result.(*submitResult).JobID = id
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
	return nil
}

func holdingsRequest(doi string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      "batch-" + doi,
		Identifiers:    &protocol.Identifiers{DOI: doi},
		DesiredVersion: "any",
	}
}

func heldClaim(doi string) ownership.WorkResult {
	return ownership.WorkResult{Claims: []ownership.Claim{{
		Source:        "papis",
		Matched:       ownership.Identifier{Kind: ownership.KindDOI, Value: doi},
		RecordPresent: true,
		Artifact:      ownership.ArtifactPresent,
	}}}
}

func citationOnlyClaim(doi string) ownership.WorkResult {
	return ownership.WorkResult{Claims: []ownership.Claim{{
		Source:        "refs",
		Matched:       ownership.Identifier{Kind: ownership.KindDOI, Value: doi},
		RecordPresent: true,
		Artifact:      ownership.ArtifactUnknown,
	}}}
}

func TestSubmitSkipsWorksHeldByAGenericSource(t *testing.T) {
	caller := &holdingsCaller{result: ownership.Result{
		Works:   []ownership.WorkResult{heldClaim("10.1000/held")},
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: true, EntryCount: 1}},
	}}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{holdingsRequest("10.1000/held")},
		SubmitOptions{Holdings: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.SkippedOwned) != 1 {
		t.Fatalf("SkippedOwned = %+v, want the held work skipped", output.SkippedOwned)
	}
	if len(output.Submitted) != 0 {
		t.Fatalf("Submitted = %+v, want nothing acquired", output.Submitted)
	}
	if caller.called("zotio.lookup_works") {
		t.Fatal("the holdings path must not consult zotio")
	}
}

// A citation without full text is exactly what a backfill user wants acquired,
// so record-present alone must never skip.
func TestSubmitAcquiresACitationWithoutFullText(t *testing.T) {
	caller := &holdingsCaller{result: ownership.Result{
		Works:   []ownership.WorkResult{citationOnlyClaim("10.1000/citation")},
		Sources: []ownership.SourceHealth{{Name: "refs", Complete: true, EntryCount: 1}},
	}}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{holdingsRequest("10.1000/citation")},
		SubmitOptions{Holdings: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.SkippedOwned) != 0 {
		t.Fatalf("SkippedOwned = %+v, want nothing skipped", output.SkippedOwned)
	}
	if len(output.Submitted) != 1 {
		t.Fatalf("Submitted = %+v, want the citation acquired", output.Submitted)
	}
}

// The core invariant at the batch seam: an unreadable source must not become a
// batch of duplicate downloads.
func TestSubmitAbortsWhenASourceIsUnreadable(t *testing.T) {
	caller := &holdingsCaller{result: ownership.Result{
		Works:   []ownership.WorkResult{{}},
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: false, FailureCode: ownership.FailureUnreadable}},
	}}
	_, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{holdingsRequest("10.1000/one")},
		SubmitOptions{Holdings: true})
	if err == nil {
		t.Fatal("an incomplete lookup must abort the batch before creating jobs")
	}
	if !strings.Contains(err.Error(), "papis") || !strings.Contains(err.Error(), "--include-owned") {
		t.Fatalf("error must name the source and the override, got %v", err)
	}
	if caller.called("acquire.submit_v2") {
		t.Fatal("no job may be created when ownership could not be verified")
	}
}

// --include-owned is the documented "proceed despite ownership uncertainty"
// override, and it must still create the jobs.
func TestIncludeOwnedProceedsDespiteAnUnreadableSource(t *testing.T) {
	caller := &holdingsCaller{result: ownership.Result{
		Works:   []ownership.WorkResult{{}},
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: false, FailureCode: ownership.FailureUnreadable}},
	}}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{holdingsRequest("10.1000/one")},
		SubmitOptions{Holdings: true, IncludeOwned: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Submitted) != 1 {
		t.Fatalf("Submitted = %+v, want the work acquired under the override", output.Submitted)
	}
	if output.StalenessWarning == "" {
		t.Fatal("proceeding under uncertainty must still warn")
	}
}

// A new CLI against a daemon that predates library.lookup_works must degrade to
// the old method rather than failing the batch.
func TestSubmitFallsBackWhenTheDaemonLacksTheMethod(t *testing.T) {
	caller := &holdingsCaller{unknown: true}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{holdingsRequest("10.1000/one")},
		SubmitOptions{Holdings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !caller.called("zotio.lookup_works") {
		t.Fatal("the fallback must reach the old method")
	}
	if len(output.Submitted) != 1 {
		t.Fatalf("Submitted = %+v, want the work acquired via the fallback", output.Submitted)
	}
}
