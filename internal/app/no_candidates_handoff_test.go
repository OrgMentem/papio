// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

// A work with ZERO legal candidates (not merely exhausted fetches) must still
// route to the institutional handoff in assisted/maximal: the browser plane is
// exactly for works OA cannot see. Regression for the Tyler-1989 live gap.
func TestNoCandidatesRoutesToInstitutionalHandoff(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = "maximal"
	svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	svc.Resolvers = nil // no resolver returns anything
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()
	id, err := svc.Submit(context.Background(), protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "request_no_cands_01",
		Identifiers:   &protocol.Identifiers{DOI: "10.1037/0022-3514.57.5.830"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", 0)
	if err != nil || row == nil {
		t.Fatalf("claim: %+v %v", row, err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := jobs.Get(context.Background(), id)
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %s, want awaiting_human", got.State)
	}
	actions, _ := jobs.ListHumanActions(context.Background(), true)
	found := false
	for _, a := range actions {
		if a.JobID == id && a.Kind == "openurl_handoff" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing openurl_handoff action: %+v", actions)
	}
}
